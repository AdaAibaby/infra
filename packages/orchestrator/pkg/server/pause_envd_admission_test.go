//go:build linux

package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	"github.com/launchdarkly/go-server-sdk/v7/testhelpers/ldtestdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// envdProbeMs is the probe timeout the tests enable. Each test serves envd for
// its sandbox from its own test server (registerEnvd), so nothing is shared.
const envdProbeMs = 100

// envdAnswer is how a test's envd answers /health: a status, a status after a
// delay, or no listener (the connection is dropped).
type envdAnswer struct {
	status      int
	delay       time.Duration
	unreachable bool
	// begun, when set, is closed once the first probe reaches the server.
	begun chan struct{}
	once  sync.Once
}

func (answer *envdAnswer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if answer.begun != nil {
		answer.once.Do(func() { close(answer.begun) })
	}
	if answer.unreachable {
		panic(http.ErrAbortHandler)
	}
	if answer.delay > 0 {
		select {
		case <-time.After(answer.delay):
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(answer.status)
}

// registerEnvd serves envd for sbx from a test server that answers as given,
// for the test's lifetime.
func registerEnvd(t *testing.T, sbx *sandbox.Sandbox, answer *envdAnswer) {
	t.Helper()

	srv := httptest.NewServer(answer)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(srv.Close)
	sbx.SetEnvdServerURLForTest(srv.URL)
}

// envdProbeFlagClient enables the envd probe with the given timeout, or
// leaves it at its default when envdMs is nil; the dedup pre-flight stays off.
func envdProbeFlagClient(t *testing.T, envdMs *int) *featureflags.Client {
	t.Helper()

	td := ldtestdata.DataSource()
	if envdMs != nil {
		td.Update(td.Flag(featureflags.PauseEnvdHealthTimeoutMs.Key()).ValueForAll(ldvalue.Int(*envdMs)))
	}

	ff, err := featureflags.NewClientWithDatasource(td)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ff.Close(context.WithoutCancel(t.Context())) })

	return ff
}

// envdProbeSandbox is an admission test sandbox with a live health checker,
// as every published sandbox has; the probe reads it under the sandbox lock.
func envdProbeSandbox(t *testing.T, sandboxID string, slotIdx int) *sandbox.Sandbox {
	t.Helper()

	sbx := admissionTestSandbox(t, sandboxID, slotIdx, utils.NewSetOnce[*header.Header]())
	sbx.SetChecks(sandbox.NewChecks(sbx))

	return sbx
}

func requireEnvdUnresponsive(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.ResourceExhausted, st.Code(), "the refusal keeps the retryable code the API restores on")
	assert.Contains(t, st.Message(), "guest agent is not responding, please retry")
	assert.NotNil(t, envdUnresponsiveDetail(t, err), "the refusal must carry the ENVD_UNRESPONSIVE detail")
}

// awaitPauseCrossedMarkStopping waits for a pause that was admitted: it has
// left the live map and set the pause stop reason, then parks forever in the
// fake template's Metadata.
func awaitPauseCrossedMarkStopping(t *testing.T, s *Server, sbx *sandbox.Sandbox, msg string) {
	t.Helper()

	require.Eventually(t, func() bool {
		_, live := s.sandboxFactory.Sandboxes.Get(sbx.Runtime.SandboxID)

		return !live
	}, 2*time.Second, time.Millisecond, msg)
	require.Eventually(t, func() bool {
		return sbx.GetStopReason() == sandbox.StopReasonPaused
	}, 2*time.Second, time.Millisecond)
}

// An unanswered probe refuses retryably before MarkStopping, with the detail
// that lets the API name the cause; the decision is counted as envd_unhealthy
// and its duration lands in the admission wait histogram under that outcome.
func TestPause_EnvdUnresponsiveRefusesBeforeMarkStopping(t *testing.T) {
	t.Parallel()

	s, reader := admissionMetricsServer(t, -1)
	s.featureFlags = envdProbeFlagClient(t, new(envdProbeMs))
	sbx := envdProbeSandbox(t, "sbx-envd-pause", 31)
	registerEnvd(t, sbx, &envdAnswer{unreachable: true})
	s.sandboxFactory.Sandboxes.MarkRunning(t.Context(), sbx)

	_, pauseErr := s.Pause(t.Context(), &orchestrator.SandboxPauseRequest{SandboxId: "sbx-envd-pause"})
	requireEnvdUnresponsive(t, pauseErr)

	_, live := s.sandboxFactory.Sandboxes.Get("sbx-envd-pause")
	assert.True(t, live, "a refused pause must leave the sandbox live")
	assert.Equal(t, sandbox.StopReasonCrashed, sbx.GetStopReason(), "no stop reason may be set by a refusal")
	assert.True(t, s.sandboxFactory.Sandboxes.MarkStopping(t.Context(), "sbx-envd-pause", "lifecycle-1"),
		"a refused pause must leave the sandbox unmarked")

	points := admissionCounterPoints(t, reader)
	require.Len(t, points, 1)
	attrs := attrsAsMap(t, points[0].Attributes)
	assert.Equal(t, string(sandbox.SnapshotAdmissionEnvdUnhealthy), attrs["outcome"])
	assert.Equal(t, "pause", attrs["rpc"])
	waits := admissionWaitPoints(t, reader)
	require.Len(t, waits, 1)
	assert.Equal(t, string(sandbox.SnapshotAdmissionEnvdUnhealthy), attrsAsMap(t, waits[0].Attributes)["outcome"])
}

// An envd that answers admits the pause: it proceeds into the destructive
// path and the decision is counted as envd_healthy.
func TestPause_EnvdHealthyAdmits(t *testing.T) {
	t.Parallel()

	s, reader := admissionMetricsServer(t, -1)
	s.featureFlags = envdProbeFlagClient(t, new(envdProbeMs))
	sbx := envdProbeSandbox(t, "sbx-envd-pause-healthy", 32)
	registerEnvd(t, sbx, &envdAnswer{status: http.StatusNoContent})
	s.sandboxFactory.Sandboxes.MarkRunning(t.Context(), sbx)

	// Pause parks forever in the fake template's Metadata once admitted.
	go func() {
		_, _ = s.Pause(context.WithoutCancel(t.Context()), &orchestrator.SandboxPauseRequest{SandboxId: "sbx-envd-pause-healthy"})
	}()

	awaitPauseCrossedMarkStopping(t, s, sbx, "a pause whose envd answers must proceed to MarkStopping")
	points := admissionCounterPoints(t, reader)
	require.Len(t, points, 1)
	assert.Equal(t, string(sandbox.SnapshotAdmissionEnvdHealthy), attrsAsMap(t, points[0].Attributes)["outcome"])
}

// A filesystem-only snapshot boots a fresh envd on resume, so it is never
// probed: with envd unreachable the pause proceeds and records no decision.
func TestPause_FilesystemOnlySkipsEnvdProbe(t *testing.T) {
	t.Parallel()

	s, reader := admissionMetricsServer(t, -1)
	s.featureFlags = envdProbeFlagClient(t, new(envdProbeMs))
	sbx := envdProbeSandbox(t, "sbx-envd-pause-fs-only", 33)
	registerEnvd(t, sbx, &envdAnswer{unreachable: true})
	s.sandboxFactory.Sandboxes.MarkRunning(t.Context(), sbx)

	go func() {
		_, _ = s.Pause(context.WithoutCancel(t.Context()), &orchestrator.SandboxPauseRequest{SandboxId: "sbx-envd-pause-fs-only", FilesystemOnly: true})
	}()

	awaitPauseCrossedMarkStopping(t, s, sbx, "a filesystem-only pause must not be refused by the envd probe")
	assert.Empty(t, admissionCounterPoints(t, reader), "a skipped probe records no decision")
}

// Flag absent, negative or zero means no probe: with envd unreachable the
// memory pause proceeds straight into today's order and records no decision.
func TestPause_EnvdProbeFlagOffRunsTodaysOrder(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		flag *int
		slot int
	}{
		"absent":   {flag: nil, slot: 34},
		"negative": {flag: new(-1), slot: 35},
		"zero":     {flag: new(0), slot: 36},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, reader := admissionMetricsServer(t, -1)
			s.featureFlags = envdProbeFlagClient(t, c.flag)
			sandboxID := "sbx-envd-pause-off-" + name
			sbx := envdProbeSandbox(t, sandboxID, c.slot)
			registerEnvd(t, sbx, &envdAnswer{unreachable: true})
			s.sandboxFactory.Sandboxes.MarkRunning(t.Context(), sbx)

			go func() {
				_, _ = s.Pause(context.WithoutCancel(t.Context()), &orchestrator.SandboxPauseRequest{SandboxId: sandboxID})
			}()

			awaitPauseCrossedMarkStopping(t, s, sbx, "flag off must reach MarkStopping without probing")
			assert.Empty(t, admissionCounterPoints(t, reader), "no probe, no decision")
		})
	}
}

// The caller's context ending mid-probe is not evidence about envd: the pause
// answers the context status, never a refusal, records no decision, and the
// sandbox is untouched.
func TestPause_EnvdProbeCallerCancelIsNotARefusal(t *testing.T) {
	t.Parallel()

	s, reader := admissionMetricsServer(t, -1)
	s.featureFlags = envdProbeFlagClient(t, new(5000))
	sbx := envdProbeSandbox(t, "sbx-envd-pause-cancel", 37)
	answer := &envdAnswer{status: http.StatusNoContent, delay: time.Hour, begun: make(chan struct{})}
	registerEnvd(t, sbx, answer)
	s.sandboxFactory.Sandboxes.MarkRunning(t.Context(), sbx)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-answer.begun
		cancel()
	}()

	_, pauseErr := s.Pause(ctx, &orchestrator.SandboxPauseRequest{SandboxId: "sbx-envd-pause-cancel"})
	require.Error(t, pauseErr)
	assert.Equal(t, codes.Canceled, status.Code(pauseErr))
	assert.Nil(t, envdUnresponsiveDetail(t, pauseErr), "a cancelled probe is not a refusal")

	_, live := s.sandboxFactory.Sandboxes.Get("sbx-envd-pause-cancel")
	assert.True(t, live)
	assert.Equal(t, sandbox.StopReasonCrashed, sbx.GetStopReason())
	assert.True(t, s.sandboxFactory.Sandboxes.MarkStopping(t.Context(), "sbx-envd-pause-cancel", "lifecycle-1"),
		"a cancelled pause must leave the sandbox unmarked")
	assert.Empty(t, admissionCounterPoints(t, reader), "nothing was decided")
}

// A checkpoint can claim the sandbox while the probe waits on its guest, and
// the claim freezes the guest, so the probe fails for that reason. The entry
// is gone from the live map by then, but the answer is the checkpoint hold's
// retryable refusal, which the API restores on, never NotFound, which would
// drop the record and kill the resumed sandbox as an orphan.
func TestPause_EnvdProbeMissWhileACheckpointClaimsTheSandboxIsRetryable(t *testing.T) {
	t.Parallel()

	s, sbx, steps, _ := slowResumeServer(t)
	defer steps.releaseAll()
	s.featureFlags = envdProbeFlagClient(t, new(300))
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())).Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/server")
	s.pauseAdmissionCounter = utils.Must(telemetry.GetCounter(meter, telemetry.OrchestratorSandboxPauseAdmissionCounterName))
	s.pauseAdmissionWaitDuration = utils.Must(telemetry.GetHistogram(meter, telemetry.OrchestratorSandboxPauseAdmissionWaitDurationName))
	sbx.SetChecks(sandbox.NewChecks(sbx))
	answer := &envdAnswer{status: http.StatusNoContent, delay: time.Hour, begun: make(chan struct{})}
	registerEnvd(t, sbx, answer)

	pauseErr := make(chan error, 1)
	go func() {
		_, err := s.Pause(t.Context(), &orchestrator.SandboxPauseRequest{SandboxId: sbx.Runtime.SandboxID})
		pauseErr <- err
	}()
	select {
	case <-answer.begun:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never reached the guest")
	}

	done := startSlowCheckpoint(t.Context(), s, steps, sbx)
	awaitStep(t, steps.resumeStarted, "resume")

	var err error
	select {
	case err = <-pauseErr:
	case <-time.After(5 * time.Second):
		t.Fatal("the pause never answered")
	}
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.NotNil(t, checkpointInProgressDetail(t, err), "the hold, not the guest, is the reason the API acts on")
	assert.Nil(t, envdUnresponsiveDetail(t, err))

	steps.releaseResume()
	awaitCheckpoint(t, done)
}
