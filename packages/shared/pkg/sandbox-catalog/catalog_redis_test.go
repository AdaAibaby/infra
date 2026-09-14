package sandbox_catalog

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	redis_utils "github.com/e2b-dev/infra/packages/shared/pkg/redis"
)

func testSandboxInfo(executionID, orchestratorID string) *SandboxInfo {
	return &SandboxInfo{
		OrchestratorID:   orchestratorID,
		OrchestratorIP:   "10.0.0.1",
		ExecutionID:      executionID,
		StartedAt:        time.Unix(0, 0).UTC(),
		MaxLengthInHours: 1,
	}
}

func TestRedisSandboxCatalog(t *testing.T) {
	t.Parallel()

	client := redis_utils.SetupInstance(t)
	catalog := NewRedisSandboxCatalog(client)
	ctx := t.Context()

	t.Run("store then get round-trips the info", func(t *testing.T) {
		t.Parallel()

		id := "sbx-roundtrip"
		want := testSandboxInfo("exec-1", "orch-A")
		require.NoError(t, catalog.StoreSandbox(ctx, id, want, time.Minute))

		got, err := catalog.GetSandbox(ctx, id)
		require.NoError(t, err)
		require.Equal(t, want.ExecutionID, got.ExecutionID)
		require.Equal(t, want.OrchestratorID, got.OrchestratorID)
	})

	t.Run("get on an absent key returns ErrSandboxNotFound", func(t *testing.T) {
		t.Parallel()

		_, err := catalog.GetSandbox(ctx, "sbx-absent")
		require.ErrorIs(t, err, ErrSandboxNotFound)
	})

	t.Run("delete removes the entry when the execution matches", func(t *testing.T) {
		t.Parallel()

		id := "sbx-delete-match"
		require.NoError(t, catalog.StoreSandbox(ctx, id, testSandboxInfo("exec-1", "orch-A"), time.Minute))
		require.NoError(t, catalog.DeleteSandbox(ctx, id, "exec-1"))

		_, err := catalog.GetSandbox(ctx, id)
		require.ErrorIs(t, err, ErrSandboxNotFound)
	})

	t.Run("delete keeps an entry owned by a different execution", func(t *testing.T) {
		t.Parallel()

		// The compare-and-delete guard: a stale teardown for exec-1 must not remove
		// the routing entry a newer exec-2 stored under the same id.
		id := "sbx-delete-mismatch"
		require.NoError(t, catalog.StoreSandbox(ctx, id, testSandboxInfo("exec-2", "orch-B"), time.Minute))
		require.NoError(t, catalog.DeleteSandbox(ctx, id, "exec-1"))

		got, err := catalog.GetSandbox(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "exec-2", got.ExecutionID)
		require.Equal(t, "orch-B", got.OrchestratorID)
	})

	t.Run("delete on an absent key is a no-op", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, catalog.DeleteSandbox(ctx, "sbx-never-stored", "exec-1"))
	})

	t.Run("routing catalog uses its own key space", func(t *testing.T) {
		t.Parallel()

		routing := NewRedisSandboxRoutingCatalog(client)
		id := "sbx-routing-isolated"
		require.Equal(t, "sandbox:catalog:"+id, catalog.getCatalogKey(id))
		require.Equal(t, "sandbox:routing:"+id, routing.getCatalogKey(id))

		require.NoError(t, routing.StoreSandbox(ctx, id, testSandboxInfo("exec-1", "orch-A"), time.Minute))

		_, err := catalog.GetSandbox(ctx, id)
		require.ErrorIs(t, err, ErrSandboxNotFound)

		got, err := routing.GetSandbox(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "exec-1", got.ExecutionID)
	})
}

func TestDeleteSandboxStrictReturnsRedisError(t *testing.T) {
	t.Parallel()

	client := redis_utils.SetupInstance(t)
	require.NoError(t, client.Close())
	broken := NewRedisSandboxCatalog(client)
	ctx := t.Context()

	// Strict surfaces the failure; the best-effort variant keeps its old contract.
	require.Error(t, broken.DeleteSandboxStrict(ctx, "sbx-closed", "exec-1"))
	require.NoError(t, broken.DeleteSandbox(ctx, "sbx-closed", "exec-1"))
}

func TestRestoreSandboxPreservesRouteOwnership(t *testing.T) {
	t.Parallel()

	client := redis_utils.SetupInstance(t)
	catalog := NewRedisSandboxCatalog(client)
	ctx := t.Context()
	for _, tc := range []struct {
		name     string
		existing *SandboxInfo
		conflict bool
	}{
		{name: "absent route"},
		{name: "same execution", existing: testSandboxInfo("exec-1", "current-service")},
		{name: "different execution", existing: testSandboxInfo("exec-2", "successor-service"), conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id := "restore-" + tc.name
			if tc.existing != nil {
				require.NoError(t, catalog.StoreSandbox(ctx, id, tc.existing, time.Minute))
			}
			request := testSandboxInfo("exec-1", "restored-service")
			err := catalog.RestoreSandbox(ctx, id, request, time.Hour)
			if tc.conflict {
				require.ErrorIs(t, err, ErrSandboxExecutionMismatch)
			} else {
				require.NoError(t, err)
			}
			got, err := catalog.GetSandbox(ctx, id)
			require.NoError(t, err)
			ttl, err := client.PTTL(ctx, catalog.getCatalogKey(id)).Result()
			require.NoError(t, err)
			require.Positive(t, ttl)
			if tc.existing != nil {
				require.Equal(t, tc.existing, got)
				require.LessOrEqual(t, ttl, time.Minute)
			} else {
				require.Equal(t, request, got)
				require.Greater(t, ttl, time.Minute)
			}
		})
	}
}

func TestRestoreSandboxRejectsUnreadableRoute(t *testing.T) {
	t.Parallel()

	client := redis_utils.SetupInstance(t)
	catalog := NewRedisSandboxCatalog(client)
	for _, value := range []string{"not-json", "null", "{}"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			key := catalog.getCatalogKey(value)
			require.NoError(t, client.Set(t.Context(), key, value, time.Minute).Err())
			err := catalog.RestoreSandbox(t.Context(), value, testSandboxInfo("exec-1", "orch-A"), time.Hour)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrSandboxExecutionMismatch)
			require.Equal(t, value, client.Get(t.Context(), key).Val())
		})
	}
}

//nolint:paralleltest // The package tracer is shared across tests.
func TestRestoreSandboxTracing(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousTracer := tracer
	tracer = provider.Tracer("github.com/e2b-dev/infra/packages/shared/pkg/sandbox-catalog")
	t.Cleanup(func() {
		tracer = previousTracer
		require.NoError(t, provider.Shutdown(context.WithoutCancel(t.Context())))
	})
	client := redis_utils.SetupInstance(t)
	catalog := NewRedisSandboxCatalog(client)

	for _, tc := range []struct {
		name     string
		existing string
		expired  bool
		wantCode codes.Code
	}{
		{name: "malformed", existing: "not-json", wantCode: codes.Error},
		{name: "conflict", existing: `{"execution_id":"exec-2"}`, wantCode: codes.Error},
		{name: "timeout", expired: true, wantCode: codes.Error},
		{name: "success", wantCode: codes.Ok},
	} {
		id := "trace-" + tc.name
		if tc.existing != "" {
			require.NoError(t, client.Set(t.Context(), catalog.getCatalogKey(id), tc.existing, time.Minute).Err())
		}
		ctx, cancel := context.WithCancel(t.Context())
		if tc.expired {
			cancel()
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		}
		before := len(recorder.Ended())
		err := catalog.RestoreSandbox(ctx, id, testSandboxInfo("exec-1", "orch-A"), time.Minute)
		cancel()
		spans := recorder.Ended()
		require.Len(t, spans, before+1, tc.name)
		span := spans[before]
		require.Equal(t, "sandbox-catalog-restore", span.Name(), tc.name)
		require.Equal(t, tc.wantCode, span.Status().Code, tc.name)
		if tc.wantCode == codes.Error {
			require.Error(t, err, tc.name)
			require.Equal(t, err.Error(), span.Status().Description, tc.name)
			require.Len(t, span.Events(), 1, tc.name)
			require.Equal(t, "exception", span.Events()[0].Name, tc.name)
		} else {
			require.NoError(t, err, tc.name)
			require.Empty(t, span.Events(), tc.name)
		}
	}
}

func TestDeleteIfSameExecutionOutcomes(t *testing.T) {
	t.Parallel()

	client := redis_utils.SetupInstance(t)
	catalog := NewRedisSandboxCatalog(client)
	ctx := t.Context()

	run := func(t *testing.T, id, executionID string) int {
		t.Helper()
		outcome, err := deleteIfSameExecution.Run(ctx, client, []string{catalog.getCatalogKey(id)}, executionID).Int()
		require.NoError(t, err)

		return outcome
	}

	t.Run("absent key", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, catalogDeleteAbsent, run(t, "sbx-outcome-absent", "exec-1"))
	})

	t.Run("matching execution deletes", func(t *testing.T) {
		t.Parallel()

		id := "sbx-outcome-match"
		require.NoError(t, catalog.StoreSandbox(ctx, id, testSandboxInfo("exec-1", "orch-A"), time.Minute))

		require.Equal(t, catalogDeleteDeleted, run(t, id, "exec-1"))
		_, err := catalog.GetSandbox(ctx, id)
		require.ErrorIs(t, err, ErrSandboxNotFound)
	})

	t.Run("different execution is a mismatch and keeps the entry", func(t *testing.T) {
		t.Parallel()

		id := "sbx-outcome-mismatch"
		require.NoError(t, catalog.StoreSandbox(ctx, id, testSandboxInfo("exec-2", "orch-B"), time.Minute))

		require.Equal(t, catalogDeleteMismatch, run(t, id, "exec-1"))
		got, err := catalog.GetSandbox(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "exec-2", got.ExecutionID)
	})

	t.Run("unreadable value is reported and kept", func(t *testing.T) {
		t.Parallel()

		id := "sbx-outcome-unreadable"
		require.NoError(t, client.Set(ctx, catalog.getCatalogKey(id), "not-json", time.Minute).Err())

		require.Equal(t, catalogDeleteUnreadable, run(t, id, "exec-1"))
		require.EqualValues(t, 1, client.Exists(ctx, catalog.getCatalogKey(id)).Val())
	})

	t.Run("valid JSON without an execution_id is unreadable, not a mismatch", func(t *testing.T) {
		t.Parallel()

		// A table that decodes but has no execution_id must not be mislabeled as an
		// execution-mismatch (which would read as "a newer execution owns it").
		id := "sbx-outcome-no-execid"
		require.NoError(t, client.Set(ctx, catalog.getCatalogKey(id), `{"orchestrator_id":"orch-A"}`, time.Minute).Err())

		require.Equal(t, catalogDeleteUnreadable, run(t, id, "exec-1"))
		require.EqualValues(t, 1, client.Exists(ctx, catalog.getCatalogKey(id)).Val())
	})
}
