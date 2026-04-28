package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainMonitor_StartDrain(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()
	nodeID := "test-node-1"

	dm.StartDrain(ctx, nodeID, 5)

	state := dm.GetDrainState(nodeID)
	require.NotNil(t, state)
	assert.Equal(t, nodeID, state.NodeID)
	assert.Equal(t, DrainStatusInProgress, state.Status)
	assert.Equal(t, 5, state.InitialSandboxCount)
	assert.Equal(t, 5, state.CurrentSandboxCount)
	assert.Nil(t, state.CompletionTime)
	assert.Nil(t, state.Error)
}

func TestDrainMonitor_UpdateDrainProgress(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()
	nodeID := "test-node-1"

	dm.StartDrain(ctx, nodeID, 5)

	// Update progress
	dm.UpdateDrainProgress(ctx, nodeID, 3)
	state := dm.GetDrainState(nodeID)
	assert.Equal(t, 3, state.CurrentSandboxCount)
	assert.Equal(t, DrainStatusInProgress, state.Status)

	// Update to completion
	dm.UpdateDrainProgress(ctx, nodeID, 0)
	state = dm.GetDrainState(nodeID)
	assert.Equal(t, 0, state.CurrentSandboxCount)
	assert.Equal(t, DrainStatusCompleted, state.Status)
	assert.NotNil(t, state.CompletionTime)
}

func TestDrainMonitor_MarkDrainFailed(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()
	nodeID := "test-node-1"

	dm.StartDrain(ctx, nodeID, 5)

	testErr := errors.New("test error")
	dm.MarkDrainFailed(ctx, nodeID, testErr)

	state := dm.GetDrainState(nodeID)
	assert.Equal(t, DrainStatusFailed, state.Status)
	assert.Equal(t, testErr, state.Error)
}

func TestDrainMonitor_IsDrainCompleted(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()
	nodeID := "test-node-1"

	// Not started
	assert.False(t, dm.IsDrainCompleted(nodeID))

	// Started but not completed
	dm.StartDrain(ctx, nodeID, 5)
	assert.False(t, dm.IsDrainCompleted(nodeID))

	// Completed
	dm.UpdateDrainProgress(ctx, nodeID, 0)
	assert.True(t, dm.IsDrainCompleted(nodeID))
}

func TestDrainMonitor_ClearDrainState(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()
	nodeID := "test-node-1"

	dm.StartDrain(ctx, nodeID, 5)
	assert.NotNil(t, dm.GetDrainState(nodeID))

	dm.ClearDrainState(ctx, nodeID)
	assert.Nil(t, dm.GetDrainState(nodeID))
}

func TestDrainMonitor_MultipleNodes(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()

	// Start drain on multiple nodes
	dm.StartDrain(ctx, "node-1", 5)
	dm.StartDrain(ctx, "node-2", 3)
	dm.StartDrain(ctx, "node-3", 7)

	// Update progress on different nodes
	dm.UpdateDrainProgress(ctx, "node-1", 2)
	dm.UpdateDrainProgress(ctx, "node-2", 0) // Complete
	dm.UpdateDrainProgress(ctx, "node-3", 5)

	// Verify states
	assert.False(t, dm.IsDrainCompleted("node-1"))
	assert.True(t, dm.IsDrainCompleted("node-2"))
	assert.False(t, dm.IsDrainCompleted("node-3"))

	// Verify individual states
	state1 := dm.GetDrainState("node-1")
	assert.Equal(t, 2, state1.CurrentSandboxCount)

	state2 := dm.GetDrainState("node-2")
	assert.Equal(t, DrainStatusCompleted, state2.Status)

	state3 := dm.GetDrainState("node-3")
	assert.Equal(t, 5, state3.CurrentSandboxCount)
}

func TestDrainMonitor_GetDrainState_ReturnsNilForNonexistent(t *testing.T) {
	dm := NewDrainMonitor()

	state := dm.GetDrainState("nonexistent-node")
	assert.Nil(t, state)
}

func TestDrainMonitor_UpdateNonexistentNode(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()

	// Should not panic
	dm.UpdateDrainProgress(ctx, "nonexistent-node", 0)

	// State should still not exist
	assert.Nil(t, dm.GetDrainState("nonexistent-node"))
}

func TestDrainMonitor_DrainDurationCalculation(t *testing.T) {
	dm := NewDrainMonitor()
	ctx := context.Background()
	nodeID := "test-node-1"

	dm.StartDrain(ctx, nodeID, 5)
	startTime := dm.GetDrainState(nodeID).StartTime

	// Simulate some time passing
	time.Sleep(100 * time.Millisecond)

	dm.UpdateDrainProgress(ctx, nodeID, 0)
	state := dm.GetDrainState(nodeID)

	duration := state.CompletionTime.Sub(startTime)
	assert.GreaterOrEqual(t, duration, 100*time.Millisecond)
}
