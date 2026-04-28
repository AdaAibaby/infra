package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/clusters"
	"github.com/e2b-dev/infra/packages/shared/pkg/ginutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

func (a *APIStore) GetNodes(c *gin.Context) {
	result, err := a.orchestrator.AdminNodes()
	if err != nil {
		telemetry.ReportCriticalError(c.Request.Context(), "error when getting nodes", err)
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when getting nodes")

		return
	}

	c.JSON(http.StatusOK, result)
}

func (a *APIStore) GetNodesNodeID(c *gin.Context, nodeID api.NodeID, params api.GetNodesNodeIDParams) {
	clusterID := clusters.WithClusterFallback(params.ClusterID)
	result, err := a.orchestrator.AdminNodeDetail(clusterID, nodeID)
	if err != nil {
		if errors.Is(err, orchestrator.ErrNodeNotFound) {
			c.Status(http.StatusNotFound)

			return
		}

		telemetry.ReportCriticalError(c.Request.Context(), "error when getting node details", err)
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error when getting node details")

		return
	}

	c.JSON(http.StatusOK, result)
}

func (a *APIStore) PostNodesNodeID(c *gin.Context, nodeId api.NodeID) {
	ctx := c.Request.Context()

	body, err := ginutils.ParseBody[api.PostNodesNodeIDJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		telemetry.ReportCriticalError(ctx, "error when parsing request", err)

		return
	}

	clusterID := clusters.WithClusterFallback(body.ClusterID)
	node := a.orchestrator.GetNode(clusterID, nodeId)
	if node == nil {
		c.Status(http.StatusNotFound)

		return
	}

	err = node.SendStatusChange(ctx, body.Status)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, fmt.Sprintf("Error when sending status change: %s", err))

		telemetry.ReportCriticalError(ctx, "error when sending status change", err)

		return
	}

	c.Status(http.StatusNoContent)
}

// PostNodesNodeIDDrain initiates drain on a node
func (a *APIStore) PostNodesNodeIDDrain(c *gin.Context, nodeID api.NodeID) {
	ctx := c.Request.Context()

	clusterID := clusters.WithClusterFallback(nil)
	node := a.orchestrator.GetNode(clusterID, nodeID)
	if node == nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Set node status to draining
	err := node.SendStatusChange(ctx, api.NodeStatusDraining)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, fmt.Sprintf("Error when setting node to draining: %s", err))
		telemetry.ReportCriticalError(ctx, "error when setting node to draining", err)
		return
	}

	logger.L().Info(ctx, "Node drain initiated",
		logger.WithNodeID(nodeID),
	)

	c.JSON(http.StatusOK, gin.H{
		"node_id": nodeID,
		"status":  "draining",
		"message": "Node drain initiated. Use GET /admin/nodes/{nodeID}/drain-status to check progress.",
	})
}

// GetNodesNodeIDDrainStatus returns the current drain status of a node
func (a *APIStore) GetNodesNodeIDDrainStatus(c *gin.Context, nodeID api.NodeID) {
	ctx := c.Request.Context()

	clusterID := clusters.WithClusterFallback(nil)
	node := a.orchestrator.GetNode(clusterID, nodeID)
	if node == nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Get current sandbox count
	sandboxCount, err := node.GetSandboxCount(ctx)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusInternalServerError, fmt.Sprintf("Error when getting sandbox count: %s", err))
		telemetry.ReportCriticalError(ctx, "error when getting sandbox count", err)
		return
	}

	// Get node status
	nodeStatus := node.Status()

	isDrained := sandboxCount == 0

	c.JSON(http.StatusOK, gin.H{
		"node_id":          nodeID,
		"status":           nodeStatus,
		"sandbox_count":    sandboxCount,
		"is_drained":       isDrained,
		"last_updated":     time.Now().UTC(),
	})
}

// PostNodesNodeIDDrainWait waits for a node to complete draining
func (a *APIStore) PostNodesNodeIDDrainWait(c *gin.Context, nodeID api.NodeID) {
	ctx := c.Request.Context()

	// Parse optional timeout from query params (default 5 minutes)
	timeoutStr := c.DefaultQuery("timeout", "300")
	timeout := time.Duration(0)
	if _, err := fmt.Sscanf(timeoutStr, "%d", &timeout); err == nil {
		timeout = timeout * time.Second
	} else {
		timeout = 5 * time.Minute
	}

	clusterID := clusters.WithClusterFallback(nil)
	node := a.orchestrator.GetNode(clusterID, nodeID)
	if node == nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Create context with timeout
	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger.L().Info(ctx, "Waiting for node drain",
		logger.WithNodeID(nodeID),
		zap.Duration("timeout", timeout),
	)

	// Wait for drain to complete
	config := nodemanager.DrainWaitConfig{
		PollInterval: 2 * time.Second,
		MaxWaitTime:  timeout,
	}

	drainDuration, err := node.WaitForDrainWithContext(drainCtx, config)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusRequestTimeout, fmt.Sprintf("Drain wait failed: %s", err))
		telemetry.ReportCriticalError(ctx, "drain wait failed", err)
		return
	}

	logger.L().Info(ctx, "Node drain completed",
		logger.WithNodeID(nodeID),
		zap.Duration("drain_duration", drainDuration),
	)

	c.JSON(http.StatusOK, gin.H{
		"node_id":         nodeID,
		"status":          "drained",
		"drain_duration":  drainDuration.String(),
		"message":         "Node has been successfully drained. All sandboxes have completed.",
	})
}
