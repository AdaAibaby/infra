package redis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox/sandboxtypes"
)

// A restored pause tells its waiters which refusal it was: a joining pause is
// answered with the refusal the starter got, and a plain waiter still reads a
// restore. Without the class every joiner read a busy node.
func TestStartRemoving_JoinerIsAnsweredWithTheRestoredRefusalsClass(t *testing.T) {
	t.Parallel()

	storage, _ := setupTestStorage(t)
	for name, refusal := range map[string]error{
		"checkpoint": sandboxtypes.PauseRefusedCheckpointInProgressError{},
		"busy":       sandboxtypes.PauseQueueExhaustedError{},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sbx := createTestSandbox("joiner-class-" + name)
			require.NoError(t, storage.Add(t.Context(), sbx))
			transition, alreadyDone, finish, err := storage.StartRemoving(t.Context(), sbx.TeamID, sbx.SandboxID, sandboxtypes.RemoveOpts{Action: sandboxtypes.StateActionPause})
			require.NoError(t, err)
			require.False(t, alreadyDone)

			joiner := make(chan error, 1)
			waiter := make(chan error, 1)
			go func() {
				_, _, _, err := storage.StartRemoving(t.Context(), sbx.TeamID, sbx.SandboxID, sandboxtypes.RemoveOpts{Action: sandboxtypes.StateActionPause})
				joiner <- err
			}()
			go func() { waiter <- storage.WaitForStateChange(t.Context(), sbx.TeamID, sbx.SandboxID) }()
			routingKey := getTransitionRoutingKey(sbx.TeamID.String(), sbx.SandboxID, transitionID(t, storage, sbx))
			require.Eventually(t, func() bool { return waitersOn(storage, routingKey) == 2 },
				5*time.Second, 5*time.Millisecond, "both waiters must be on the transition before it is finished")

			_, err = storage.RestoreRunning(t.Context(), transition, sandboxtypes.RestoreOpts{RetryAfter: 10 * time.Second})
			require.NoError(t, err)
			finish(t.Context(), &sandboxtypes.RestoredTransitionError{Refusal: refusal})

			select {
			case err := <-joiner:
				require.ErrorIs(t, err, refusal, "the joiner must get the starter's refusal")
				require.ErrorIs(t, err, sandboxtypes.PauseQueueExhaustedError{})
				if name == "busy" {
					require.NotErrorIs(t, err, sandboxtypes.PauseRefusedCheckpointInProgressError{})
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the joiner never returned")
			}
			select {
			case err := <-waiter:
				require.ErrorIs(t, err, sandboxtypes.ErrTransitionRestored)
				require.ErrorIs(t, err, refusal)
			case <-time.After(5 * time.Second):
				t.Fatal("the waiter never returned")
			}
		})
	}
}

// waitersOn counts the waiters subscribed to a transition's routing key.
func waitersOn(storage *Storage, routingKey string) int {
	storage.subManager.mu.RLock()
	defer storage.subManager.mu.RUnlock()

	return len(storage.subManager.waiters[routingKey])
}

// transitionID reads the id of the transition in flight on the sandbox.
func transitionID(t *testing.T, storage *Storage, sbx sandboxtypes.Sandbox) string {
	t.Helper()

	id, err := storage.redisClient.Get(t.Context(), getTransitionKey(sbx.TeamID.String(), sbx.SandboxID)).Result()
	require.NoError(t, err)

	return id
}

// A hold-only restore sets the retry window and nothing else: no refusal
// episode opens, and a live one ends, so the next refusal's budget counts
// from that refusal. A charged refusal keeps the live episode.
func TestRestoreRunning_HoldOnlyEndsTheEpisode(t *testing.T) {
	t.Parallel()

	storage, _ := setupTestStorage(t)
	for name, holdOnly := range map[string]bool{"hold only": true, "charged": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			episodeStart := time.Now().Add(-90 * time.Second)
			sbx := createTestSandbox("restore-hold-" + name)
			sbx.RefusedSince = episodeStart
			sbx.RefusedUntil = time.Now().Add(-time.Second)
			require.NoError(t, storage.Add(t.Context(), sbx))
			transition, _, finish, err := storage.StartRemoving(t.Context(), sbx.TeamID, sbx.SandboxID, sandboxtypes.RemoveOpts{Action: sandboxtypes.StateActionPause})
			require.NoError(t, err)
			defer finish(context.WithoutCancel(t.Context()), sandboxtypes.ErrTransitionRestored)

			restored, err := storage.RestoreRunning(t.Context(), transition, sandboxtypes.RestoreOpts{RetryAfter: 10 * time.Second, HoldOnly: holdOnly})
			require.NoError(t, err)
			stored, err := storage.Get(t.Context(), sbx.TeamID, sbx.SandboxID)
			require.NoError(t, err)
			for _, got := range []sandboxtypes.Sandbox{restored, stored} {
				assert.WithinDuration(t, time.Now().Add(10*time.Second), got.RefusedUntil, time.Second, "the sweep is held either way")
				if holdOnly {
					assert.True(t, got.RefusedSince.IsZero(), "the hold ends the episode it found")
				} else {
					assert.WithinDuration(t, episodeStart, got.RefusedSince, time.Millisecond, "the live episode goes on")
				}
			}
		})
	}
}
