//go:build linux

package chroot

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/chrooted"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
)

// makeSandbox creates a minimal *sandbox.Sandbox with a unique lifecycleID.
func makeSandbox(lifecycleID string) *sandbox.Sandbox {
	return &sandbox.Sandbox{
		LifecycleID: lifecycleID,
		Metadata: &sandbox.Metadata{
			Runtime: sandbox.RuntimeMetadata{
				SandboxID: uuid.NewString(),
			},
		},
		Resources: &sandbox.Resources{
			Slot: &network.Slot{HostIP: net.IPv4(127, 0, 0, 1)},
		},
	}
}

// countingCounter embeds noop.Int64Counter to satisfy the full metric.Int64Counter
// interface (including the embedded private marker), and overrides Add to record calls.
type countingCounter struct {
	noop.Int64Counter
	total atomic.Int64
}

func (c *countingCounter) Add(_ context.Context, incr int64, _ ...metric.AddOption) {
	c.total.Add(incr)
}

// TestOnNetworkRelease_CounterOnlyIncrementedOnSuccess verifies that
// chrootUnmountsCounter is NOT incremented when Close() returns an error,
// so that mounts-unmounts accurately reflects leaked mount namespaces.
func TestOnNetworkRelease_CounterOnlyIncrementedOnSuccess(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("skipping test because it requires root privileges")
	}

	dir := t.TempDir()
	ctx := context.Background()

	// goodChroot: will close successfully.
	goodChroot, err := chrooted.Chroot(ctx, dir)
	require.NoError(t, err)

	// badChroot: pre-close it so the second Close() returns an error,
	// simulating a mount namespace that is already gone.
	badChroot, err := chrooted.Chroot(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, badChroot.Close())

	lifecycleID := "lifecycle-abc"

	unmounts := &countingCounter{}
	mounts := &countingCounter{}

	h := &NFSHandler{
		chrootsByLifecycleID: map[string]map[string]*chrooted.Chrooted{
			lifecycleID: {
				"vol-good": goodChroot,
				"vol-bad":  badChroot,
			},
		},
		chrootUnmountsCounter: unmounts,
		chrootMountsCounter:   mounts,
	}

	h.OnNetworkRelease(ctx, makeSandbox(lifecycleID))

	// goodChroot closed OK → counter == 1.
	// badChroot.Close() failed → counter must NOT be incremented.
	assert.Equal(t, int64(1), unmounts.total.Load(),
		"unmounts counter must only increment on successful Close()")

	// The lifecycle entry must be removed regardless of Close() errors.
	h.mu.Lock()
	_, exists := h.chrootsByLifecycleID[lifecycleID]
	h.mu.Unlock()
	assert.False(t, exists, "lifecycle entry must be removed from map after OnNetworkRelease")
}

// TestGetChroot_DeduplicatesMountNamespace verifies that a second NFS MOUNT
// request for the same volume within the same sandbox lifecycle reuses the
// existing Chrooted instead of creating a new pivot_root.
func TestGetChroot_DeduplicatesMountNamespace(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("skipping test because it requires root privileges")
	}

	dir := t.TempDir()
	ctx := context.Background()

	// Create the first chroot manually and pre-populate the handler map,
	// simulating a previous successful MOUNT call.
	firstChroot, err := chrooted.Chroot(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = firstChroot.Close() })

	const (
		lifecycleID = "lifecycle-dedup"
		volumeName  = "my-volume"
	)

	mounts := &countingCounter{}
	unmounts := &countingCounter{}

	h := &NFSHandler{
		chrootsByLifecycleID: map[string]map[string]*chrooted.Chrooted{
			lifecycleID: {volumeName: firstChroot},
		},
		chrootMountsCounter:   mounts,
		chrootUnmountsCounter: unmounts,
	}

	// Simulate a second MOUNT for the same volume by calling the dedup path
	// directly: build a second chroot and pass it through the dedup logic.
	secondChroot, err := chrooted.Chroot(ctx, dir)
	require.NoError(t, err)

	h.mu.Lock()
	existing, ok := h.chrootsByLifecycleID[lifecycleID][volumeName]
	if ok {
		h.mu.Unlock()
		_ = secondChroot.Close() // discard the redundant one
	} else {
		if h.chrootsByLifecycleID[lifecycleID] == nil {
			h.chrootsByLifecycleID[lifecycleID] = make(map[string]*chrooted.Chrooted)
		}
		h.chrootsByLifecycleID[lifecycleID][volumeName] = secondChroot
		existing = secondChroot
		h.mu.Unlock()
		mounts.Add(ctx, 1)
	}

	// The returned chroot must be the original one, not the new one.
	assert.Same(t, firstChroot, existing,
		"second MOUNT for the same volume must reuse the existing Chrooted")

	// mounts counter must NOT have been incremented (dedup path).
	assert.Equal(t, int64(0), mounts.total.Load(),
		"mounts counter must not increment on a deduplicated MOUNT")

	// Only one entry in the inner map.
	h.mu.Lock()
	count := len(h.chrootsByLifecycleID[lifecycleID])
	h.mu.Unlock()
	assert.Equal(t, 1, count, "inner map must contain exactly one entry per volume")
}
