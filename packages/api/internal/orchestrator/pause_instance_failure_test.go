package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// pauseFailingSandboxClient rejects every Pause with a fixed error.
type pauseFailingSandboxClient struct {
	orchestrator.SandboxServiceClient

	err error
}

func (c *pauseFailingSandboxClient) Pause(_ context.Context, _ *orchestrator.SandboxPauseRequest, _ ...grpc.CallOption) (*orchestrator.SandboxPauseResponse, error) {
	return nil, c.err
}

// pauseCapturingSandboxClient accepts every Pause and keeps the last request.
type pauseCapturingSandboxClient struct {
	orchestrator.SandboxServiceClient

	last *orchestrator.SandboxPauseRequest
}

func (c *pauseCapturingSandboxClient) Pause(_ context.Context, in *orchestrator.SandboxPauseRequest, _ ...grpc.CallOption) (*orchestrator.SandboxPauseResponse, error) {
	c.last = in

	return &orchestrator.SandboxPauseResponse{}, nil
}

// newPauseFixture returns an orchestrator on a test database, a node whose Pause
// always fails with pauseErr, and the sandbox to pause.
func newPauseFixture(t *testing.T, pauseErr error) (*Orchestrator, *testutils.Database, *nodemanager.Node, sandbox.Sandbox) {
	t.Helper()

	db := testutils.SetupDatabase(t)

	sem, err := utils.NewAdjustableSemaphore(1)
	require.NoError(t, err)

	o := &Orchestrator{sqlcDB: db.SqlcClient, snapshotUpsertSem: sem}

	teamID := testutils.CreateTestTeam(t, db)
	baseTemplateID := testutils.CreateTestTemplate(t, db, teamID)
	sourceBuildID := testutils.CreateTestBuild(t, t.Context(), db, baseTemplateID, string(types.BuildStatusUploaded))

	node := nodemanager.NewTestNode("node-1", api.NodeStatusReady, 0, 8)
	node.SetSandboxClient(&pauseFailingSandboxClient{err: pauseErr})

	sbx := sandbox.Sandbox{
		SandboxID:      "sbx-" + uuid.NewString()[:8],
		TeamID:         teamID,
		BaseTemplateID: baseTemplateID,
		BuildID:        sourceBuildID,
		ClusterID:      consts.LocalClusterID,
		StartTime:      time.Now(),
		VCpu:           2,
		RamMB:          2048,
	}

	return o, db, node, sbx
}

// snapshotBuildStatus reads back the build UpsertSnapshot created for the
// sandbox, not the source build the fixture seeded.
func snapshotBuildStatus(t *testing.T, db *testutils.Database, sandboxID string) (string, bool) {
	t.Helper()

	var (
		buildStatus string
		finishedAt  *time.Time
	)

	err := db.SqlcClient.TestsRawSQLQuery(t.Context(),
		`SELECT b.status, b.finished_at
		 FROM public.snapshots s
		 JOIN public.env_build_assignments eba ON eba.env_id = s.env_id
		 JOIN public.env_builds b ON b.id = eba.build_id
		 WHERE s.sandbox_id = $1
		 ORDER BY b.created_at DESC
		 LIMIT 1`,
		func(rows pgx.Rows) error {
			if !rows.Next() {
				return errors.New("no snapshot build found")
			}

			return rows.Scan(&buildStatus, &finishedAt)
		},
		sandboxID,
	)
	require.NoError(t, err)

	return buildStatus, finishedAt != nil
}

// UpsertSnapshot has committed the snapshot row and its build before the node
// RPC runs, so a rejected pause must still close the build out.
func TestPauseSandbox_FailsBuildWhenPauseRPCFails(t *testing.T) {
	t.Parallel()

	o, db, node, sbx := newPauseFixture(t, errors.New("node exploded"))

	err := o.pauseSandbox(t.Context(), node, sbx, false, false)
	require.Error(t, err)

	buildStatus, hasFinishedAt := snapshotBuildStatus(t, db, sbx.SandboxID)
	assert.Equal(t, string(types.BuildStatusFailed), buildStatus)
	assert.True(t, hasFinishedAt, "a terminal build must record finished_at")
}

// A pause the node refuses for a checkpoint in flight keeps its own type
// under the retryable refusal, and closes the build out like any refusal.
func TestPauseSandbox_FailsBuildWhenCheckpointInProgress(t *testing.T) {
	t.Parallel()

	refusal, err := status.New(codes.ResourceExhausted, "sandbox is being checkpointed, please retry").WithDetails(
		&orchestrator.UserError{Code: orchestrator.UserErrorCode_CHECKPOINT_IN_PROGRESS, HttpStatus: 503},
	)
	require.NoError(t, err)
	o, db, node, sbx := newPauseFixture(t, refusal.Err())

	err = o.pauseSandbox(t.Context(), node, sbx, false, false)
	require.ErrorIs(t, err, PauseRefusedCheckpointInProgressError{})
	require.ErrorIs(t, err, PauseQueueExhaustedError{}, "a checkpoint in flight is one of the retryable refusals")

	buildStatus, hasFinishedAt := snapshotBuildStatus(t, db, sbx.SandboxID)
	assert.Equal(t, string(types.BuildStatusFailed), buildStatus)
	assert.True(t, hasFinishedAt, "a terminal build must record finished_at")
}

// A pause the node refuses because the guest agent did not answer its probe
// keeps its own type under the retryable refusal, and closes the build out
// like any refusal.
func TestPauseSandbox_FailsBuildWhenEnvdUnresponsive(t *testing.T) {
	t.Parallel()

	refusal, err := status.New(codes.ResourceExhausted, "sandbox 'x' guest agent is not responding, please retry").WithDetails(
		&orchestrator.UserError{Code: orchestrator.UserErrorCode_ENVD_UNRESPONSIVE, HttpStatus: 503},
	)
	require.NoError(t, err)
	o, db, node, sbx := newPauseFixture(t, refusal.Err())

	err = o.pauseSandbox(t.Context(), node, sbx, false, false)
	require.ErrorIs(t, err, PauseRefusedEnvdUnresponsiveError{})
	require.ErrorIs(t, err, PauseQueueExhaustedError{}, "an unresponsive guest agent is one of the retryable refusals")
	require.NotErrorIs(t, err, PauseRefusedCheckpointInProgressError{})

	buildStatus, hasFinishedAt := snapshotBuildStatus(t, db, sbx.SandboxID)
	assert.Equal(t, string(types.BuildStatusFailed), buildStatus)
	assert.True(t, hasFinishedAt, "a terminal build must record finished_at")
}

// A full pause queue returns a distinct error, and closes the build out too.
func TestPauseSandbox_FailsBuildWhenPauseQueueExhausted(t *testing.T) {
	t.Parallel()

	o, db, node, sbx := newPauseFixture(t, status.Error(codes.ResourceExhausted, "queue full"))

	err := o.pauseSandbox(t.Context(), node, sbx, false, false)
	require.ErrorIs(t, err, PauseQueueExhaustedError{})

	buildStatus, hasFinishedAt := snapshotBuildStatus(t, db, sbx.SandboxID)
	assert.Equal(t, string(types.BuildStatusFailed), buildStatus)
	assert.True(t, hasFinishedAt, "a terminal build must record finished_at")
}

// The evictor's probe bypass travels on the pause request as the snapshot
// kind does; a plain pause leaves it unset.
func TestPauseSandbox_SkipEnvdHealthProbeReachesTheNode(t *testing.T) {
	t.Parallel()

	o, _, node, sbx := newPauseFixture(t, nil)
	client := &pauseCapturingSandboxClient{}
	node.SetSandboxClient(client)
	o.snapshotCache = noopSnapshotCache{}

	require.NoError(t, o.pauseSandbox(t.Context(), node, sbx, false, true))
	require.NotNil(t, client.last)
	assert.True(t, client.last.GetSkipEnvdHealthProbe(), "the bypass must reach the node")
	assert.False(t, client.last.GetFilesystemOnly(), "the bypass is a memory pause")
	assert.Equal(t, sbx.SandboxID, client.last.GetSandboxId())

	require.NoError(t, o.pauseSandbox(t.Context(), node, sbx, false, false))
	assert.False(t, client.last.GetSkipEnvdHealthProbe(), "a probed pause sends nothing")
}
