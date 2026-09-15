package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

type pauseContextKey struct{}

type pauseContextCatalog struct {
	restorableCatalog

	beforeDelete func(context.Context)
}

func (c *pauseContextCatalog) DeleteSandbox(ctx context.Context, sandboxID, executionID string) error {
	c.beforeDelete(ctx)

	return c.restorableCatalog.DeleteSandbox(ctx, sandboxID, executionID)
}

type pauseContextClient struct {
	orchestrator.SandboxServiceClient

	pause func(context.Context, *orchestrator.SandboxPauseRequest) error
}

func (c *pauseContextClient) Pause(ctx context.Context, in *orchestrator.SandboxPauseRequest, _ ...grpc.CallOption) (*orchestrator.SandboxPauseResponse, error) {
	if err := c.pause(ctx, in); err != nil {
		return nil, err
	}

	return &orchestrator.SandboxPauseResponse{}, nil
}

func TestRemoveSandbox_PausePersistsSnapshotAfterCallerCancellation(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name          string
		beforeUpsert  bool
		callerTimeout time.Duration
	}{
		{name: "before_snapshot_upsert", beforeUpsert: true, callerTimeout: 5 * time.Second},
		{name: "during_rpc", callerTimeout: 5 * time.Second},
		{name: "long_caller_deadline", callerTimeout: 10 * time.Minute},
		{name: "no_caller_deadline"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newRefusalFixture(t, true, consts.LocalClusterID, nil)
			ctx := context.WithValue(t.Context(), pauseContextKey{}, "request-value")
			var cancel context.CancelFunc
			if tt.callerTimeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, tt.callerTimeout)
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()

			var pauseDeadline time.Time
			f.o.routingCatalog = &pauseContextCatalog{
				restorableCatalog: f.o.routingCatalog,
				beforeDelete: func(pauseCtx context.Context) {
					stored, err := f.o.sandboxStore.Get(pauseCtx, f.sbx.TeamID, f.sbx.SandboxID)
					require.NoError(t, err)
					require.Equal(t, sandbox.StatePausing, stored.State)

					var ok bool
					pauseDeadline, ok = pauseCtx.Deadline()
					require.True(t, ok)
					assert.WithinDuration(t, time.Now().Add(80*time.Second), pauseDeadline, time.Second)
					if tt.beforeUpsert {
						cancel()
					}
				},
			}

			var snapshotBuildID uuid.UUID
			node := f.o.GetNode(f.sbx.ClusterID, f.sbx.NodeID)
			require.NotNil(t, node)
			node.SetSandboxClient(&pauseContextClient{pause: func(pauseCtx context.Context, in *orchestrator.SandboxPauseRequest) error {
				cancel()
				assert.Equal(t, "request-value", pauseCtx.Value(pauseContextKey{}))
				deadline, ok := pauseCtx.Deadline()
				require.True(t, ok)
				assert.Equal(t, pauseDeadline, deadline, "snapshot upsert and RPC must share the pause budget")

				var err error
				snapshotBuildID, err = uuid.Parse(in.GetBuildId())
				require.NoError(t, err)

				return pauseCtx.Err()
			}})

			require.NoError(t, f.o.RemoveSandbox(ctx, f.sbx.TeamID, f.sbx.SandboxID, sandbox.RemoveOpts{Action: sandbox.StateActionPause}))
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.NotEqual(t, uuid.Nil, snapshotBuildID)

			snapshot, err := f.o.sqlcDB.GetLastSnapshot(t.Context(), f.sbx.SandboxID)
			require.NoError(t, err, "the completed snapshot must remain available for resume")
			assert.Equal(t, snapshotBuildID, snapshot.EnvBuild.ID)
			assert.Equal(t, types.BuildStatusSuccess, snapshot.EnvBuild.Status)
			assert.NotNil(t, snapshot.EnvBuild.FinishedAt)
			assert.Equal(t, f.sbx.TeamID, snapshot.Snapshot.TeamID)

			_, err = f.o.sandboxStore.Get(t.Context(), f.sbx.TeamID, f.sbx.SandboxID)
			require.ErrorIs(t, err, sandbox.ErrNotFound)
		})
	}
}

func TestRemoveSandbox_CanceledPauseDoesNotAcquireTransition(t *testing.T) {
	t.Parallel()

	f := newRefusalFixture(t, true, consts.LocalClusterID, nil)

	err := f.o.RemoveSandbox(cancelledContext(t), f.sbx.TeamID, f.sbx.SandboxID, sandbox.RemoveOpts{Action: sandbox.StateActionPause})
	require.Error(t, err)

	stored, err := f.o.sandboxStore.Get(t.Context(), f.sbx.TeamID, f.sbx.SandboxID)
	require.NoError(t, err)
	assert.Equal(t, sandbox.StateRunning, stored.State)
}
