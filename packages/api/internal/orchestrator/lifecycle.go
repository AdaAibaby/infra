package orchestrator

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	e2bcatalog "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-catalog"
)

type restorableCatalog interface {
	e2bcatalog.SandboxesCatalog
	RestoreSandbox(ctx context.Context, sandboxID string, sandboxInfo *e2bcatalog.SandboxInfo, expiration time.Duration) error
}

func (o *Orchestrator) addSandboxToRoutingTable(ctx context.Context, sandbox sandbox.Sandbox) error {
	return o.writeSandboxToRoutingTable(ctx, sandbox, o.routingCatalog.StoreSandbox)
}

func (o *Orchestrator) writeSandboxToRoutingTable(ctx context.Context, sandbox sandbox.Sandbox, write func(context.Context, string, *e2bcatalog.SandboxInfo, time.Duration) error) error {
	node := o.GetNode(sandbox.ClusterID, sandbox.NodeID)
	if node == nil {
		return fmt.Errorf("node '%s' not found", sandbox.NodeID)
	}

	// For remote cluster nodes we are using gPRC metadata for routing registration instead
	if node.IsClusterNode() {
		return nil
	}

	nodeIP := routeNodeIPAddress(node, env.IsLocal())

	info := e2bcatalog.SandboxInfo{
		OrchestratorID: node.Metadata().ServiceInstanceID,
		OrchestratorIP: nodeIP,

		ExecutionID:      sandbox.ExecutionID,
		StartedAt:        sandbox.StartTime,
		MaxLengthInHours: int64(sandbox.MaxInstanceLength / time.Hour),
	}

	lifetime := time.Duration(info.MaxLengthInHours) * time.Hour

	return write(ctx, sandbox.SandboxID, &info, lifetime)
}

func (o *Orchestrator) addSandboxToRoutingTableOrLog(ctx context.Context, sandbox sandbox.Sandbox) {
	if err := o.addSandboxToRoutingTable(ctx, sandbox); err != nil {
		logger.L().Error(ctx, "error adding routing record to catalog", zap.Error(err), logger.WithSandboxID(sandbox.SandboxID))
	}
}
