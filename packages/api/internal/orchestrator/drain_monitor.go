package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// DrainMonitor monitors the drain status of nodes
type DrainMonitor struct {
	mu sync.RWMutex
	// drainStates maps nodeID to drain state
	drainStates map[string]*DrainState
}

// DrainState represents the state of a node during drain
type DrainState struct {
	NodeID              string
	Status              DrainStatus
	StartTime           time.Time
	CompletionTime      *time.Time
	InitialSandboxCount int
	CurrentSandboxCount int
	LastUpdated         time.Time
	Error               error
}

// DrainStatus represents the status of drain operation
type DrainStatus string

const (
	DrainStatusNotStarted DrainStatus = "not_started"
	DrainStatusInProgress DrainStatus = "in_progress"
	DrainStatusCompleted  DrainStatus = "completed"
	DrainStatusFailed     DrainStatus = "failed"
)

// NewDrainMonitor creates a new drain monitor
func NewDrainMonitor() *DrainMonitor {
	return &DrainMonitor{
		drainStates: make(map[string]*DrainState),
	}
}

// StartDrain marks a node as starting drain
func (dm *DrainMonitor) StartDrain(ctx context.Context, nodeID string, initialSandboxCount int) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	now := time.Now()
	dm.drainStates[nodeID] = &DrainState{
		NodeID:              nodeID,
		Status:              DrainStatusInProgress,
		StartTime:           now,
		InitialSandboxCount: initialSandboxCount,
		CurrentSandboxCount: initialSandboxCount,
		LastUpdated:         now,
	}

	logger.L().Info(ctx, "Drain started for node",
		zap.String("node_id", nodeID),
		zap.Int("initial_sandbox_count", initialSandboxCount),
	)
}

// UpdateDrainProgress updates the current sandbox count for a draining node
func (dm *DrainMonitor) UpdateDrainProgress(ctx context.Context, nodeID string, currentSandboxCount int) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	state, exists := dm.drainStates[nodeID]
	if !exists {
		logger.L().Warn(ctx, "Drain state not found for node", zap.String("node_id", nodeID))
		return
	}

	state.CurrentSandboxCount = currentSandboxCount
	state.LastUpdated = time.Now()

	if currentSandboxCount == 0 && state.Status == DrainStatusInProgress {
		completionTime := time.Now()
		state.Status = DrainStatusCompleted
		state.CompletionTime = &completionTime

		logger.L().Info(ctx, "Drain completed for node",
			zap.String("node_id", nodeID),
			zap.Duration("drain_duration", completionTime.Sub(state.StartTime)),
		)
	}
}

// MarkDrainFailed marks a drain operation as failed
func (dm *DrainMonitor) MarkDrainFailed(ctx context.Context, nodeID string, err error) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	state, exists := dm.drainStates[nodeID]
	if !exists {
		logger.L().Warn(ctx, "Drain state not found for node", zap.String("node_id", nodeID))
		return
	}

	state.Status = DrainStatusFailed
	state.Error = err
	state.LastUpdated = time.Now()

	logger.L().Error(ctx, "Drain failed for node",
		zap.String("node_id", nodeID),
		zap.Error(err),
	)
}

// GetDrainState returns the current drain state for a node
func (dm *DrainMonitor) GetDrainState(nodeID string) *DrainState {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	state, exists := dm.drainStates[nodeID]
	if !exists {
		return nil
	}

	// Return a copy to avoid external modifications
	stateCopy := *state
	return &stateCopy
}

// IsDrainCompleted checks if drain is completed for a node
func (dm *DrainMonitor) IsDrainCompleted(nodeID string) bool {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	state, exists := dm.drainStates[nodeID]
	if !exists {
		return false
	}

	return state.Status == DrainStatusCompleted
}

// ClearDrainState removes the drain state for a node
func (dm *DrainMonitor) ClearDrainState(ctx context.Context, nodeID string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	delete(dm.drainStates, nodeID)
	logger.L().Info(ctx, "Drain state cleared for node", zap.String("node_id", nodeID))
}

// MonitorNodeDrain continuously monitors a node's drain progress
// It polls the node for sandbox count and updates the drain state
func (dm *DrainMonitor) MonitorNodeDrain(
	ctx context.Context,
	node *nodemanager.Node,
	pollInterval time.Duration,
) error {
	nodeID := node.ID

	// Get initial sandbox count
	sandboxes, err := node.GetSandboxes(ctx)
	if err != nil {
		dm.MarkDrainFailed(ctx, nodeID, fmt.Errorf("failed to get initial sandbox count: %w", err))
		return err
	}

	initialCount := len(sandboxes)
	dm.StartDrain(ctx, nodeID, initialCount)

	// If no sandboxes, drain is already complete
	if initialCount == 0 {
		dm.UpdateDrainProgress(ctx, nodeID, 0)
		return nil
	}

	// Poll for sandbox count changes
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			dm.MarkDrainFailed(ctx, nodeID, ctx.Err())
			return ctx.Err()

		case <-ticker.C:
			sandboxes, err := node.GetSandboxes(ctx)
			if err != nil {
				logger.L().Error(ctx, "Failed to get sandbox count during drain",
					zap.String("node_id", nodeID),
					zap.Error(err),
				)
				continue
			}

			currentCount := len(sandboxes)
			dm.UpdateDrainProgress(ctx, nodeID, currentCount)

			// Check if drain is completed
			if currentCount == 0 {
				return nil
			}

			logger.L().Debug(ctx, "Drain in progress",
				zap.String("node_id", nodeID),
				zap.Int("remaining_sandboxes", currentCount),
				zap.Int("initial_sandboxes", initialCount),
			)
		}
	}
}
