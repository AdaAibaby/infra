package nodemanager

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// DrainWaitConfig holds configuration for waiting on drain
type DrainWaitConfig struct {
	// PollInterval is how often to check sandbox count
	PollInterval time.Duration
	// MaxWaitTime is the maximum time to wait for drain to complete
	MaxWaitTime time.Duration
}

// DefaultDrainWaitConfig returns default configuration for drain wait
func DefaultDrainWaitConfig() DrainWaitConfig {
	return DrainWaitConfig{
		PollInterval: 2 * time.Second,
		MaxWaitTime:  5 * time.Minute,
	}
}

// WaitForDrain waits for a node to complete draining (all sandboxes to finish)
// It returns the time taken to drain or an error if timeout is exceeded
func (n *Node) WaitForDrain(ctx context.Context, config DrainWaitConfig) (time.Duration, error) {
	return n.WaitForDrainWithContext(ctx, config)
}

// WaitForDrainWithContext waits for a node to complete draining with a custom context
func (n *Node) WaitForDrainWithContext(ctx context.Context, config DrainWaitConfig) (time.Duration, error) {
	startTime := time.Now()

	// Create a context with timeout
	drainCtx, cancel := context.WithTimeout(ctx, config.MaxWaitTime)
	defer cancel()

	logger.L().Info(ctx, "Starting drain wait",
		logger.WithNodeID(n.ID),
		zap.Duration("max_wait_time", config.MaxWaitTime),
		zap.Duration("poll_interval", config.PollInterval),
	)

	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-drainCtx.Done():
			elapsed := time.Since(startTime)
			logger.L().Error(ctx, "Drain wait timeout",
				logger.WithNodeID(n.ID),
				zap.Duration("elapsed", elapsed),
				zap.Error(drainCtx.Err()),
			)
			return elapsed, fmt.Errorf("drain wait timeout after %v: %w", elapsed, drainCtx.Err())

		case <-ticker.C:
			sandboxes, err := n.GetSandboxes(drainCtx)
			if err != nil {
				logger.L().Warn(ctx, "Failed to get sandbox count during drain wait",
					logger.WithNodeID(n.ID),
					zap.Error(err),
				)
				continue
			}

			sandboxCount := len(sandboxes)
			elapsed := time.Since(startTime)

			if sandboxCount == 0 {
				logger.L().Info(ctx, "Drain completed",
					logger.WithNodeID(n.ID),
					zap.Duration("drain_duration", elapsed),
				)
				return elapsed, nil
			}

			logger.L().Debug(ctx, "Waiting for sandboxes to drain",
				logger.WithNodeID(n.ID),
				zap.Int("remaining_sandboxes", sandboxCount),
				zap.Duration("elapsed", elapsed),
			)
		}
	}
}

// GetSandboxCount returns the current number of sandboxes on the node
func (n *Node) GetSandboxCount(ctx context.Context) (int, error) {
	sandboxes, err := n.GetSandboxes(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get sandbox count: %w", err)
	}
	return len(sandboxes), nil
}

// IsDrained checks if the node has no sandboxes
func (n *Node) IsDrained(ctx context.Context) (bool, error) {
	count, err := n.GetSandboxCount(ctx)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}
