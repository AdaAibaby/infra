package nodemanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
)

// MockNode is a mock implementation of Node for testing
type MockNode struct {
	mock.Mock
	*Node
}

func (m *MockNode) GetSandboxes(ctx context.Context) ([]sandbox.Sandbox, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]sandbox.Sandbox), args.Error(1)
}

func TestWaitForDrain_AlreadyDrained(t *testing.T) {
	// Create a mock node that returns no sandboxes
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}
	mockNode.On("GetSandboxes", mock.Anything).Return([]sandbox.Sandbox{}, nil)

	ctx := context.Background()
	config := DrainWaitConfig{
		PollInterval: 100 * time.Millisecond,
		MaxWaitTime:  5 * time.Second,
	}

	duration, err := mockNode.WaitForDrainWithContext(ctx, config)

	assert.NoError(t, err)
	assert.Greater(t, duration, time.Duration(0))
	assert.Less(t, duration, 1*time.Second)
}

func TestWaitForDrain_Timeout(t *testing.T) {
	// Create a mock node that always returns sandboxes
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	sandboxes := []sandbox.Sandbox{
		{SandboxID: "sandbox-1"},
	}
	mockNode.On("GetSandboxes", mock.Anything).Return(sandboxes, nil)

	ctx := context.Background()
	config := DrainWaitConfig{
		PollInterval: 100 * time.Millisecond,
		MaxWaitTime:  200 * time.Millisecond,
	}

	duration, err := mockNode.WaitForDrainWithContext(ctx, config)

	assert.Error(t, err)
	assert.GreaterOrEqual(t, duration, 200*time.Millisecond)
}

func TestWaitForDrain_ContextCancellation(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	sandboxes := []sandbox.Sandbox{
		{SandboxID: "sandbox-1"},
	}
	mockNode.On("GetSandboxes", mock.Anything).Return(sandboxes, nil)

	ctx, cancel := context.WithCancel(context.Background())
	config := DrainWaitConfig{
		PollInterval: 100 * time.Millisecond,
		MaxWaitTime:  5 * time.Second,
	}

	// Cancel context after a short delay
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	duration, err := mockNode.WaitForDrainWithContext(ctx, config)

	assert.Error(t, err)
	assert.Less(t, duration, 1*time.Second)
}

func TestWaitForDrain_GetSandboxesError(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	testErr := errors.New("failed to get sandboxes")
	mockNode.On("GetSandboxes", mock.Anything).Return(nil, testErr)

	ctx := context.Background()
	config := DrainWaitConfig{
		PollInterval: 100 * time.Millisecond,
		MaxWaitTime:  500 * time.Millisecond,
	}

	duration, err := mockNode.WaitForDrainWithContext(ctx, config)

	assert.Error(t, err)
	assert.GreaterOrEqual(t, duration, 500*time.Millisecond)
}

func TestGetSandboxCount(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	sandboxes := []sandbox.Sandbox{
		{SandboxID: "sandbox-1"},
		{SandboxID: "sandbox-2"},
		{SandboxID: "sandbox-3"},
	}
	mockNode.On("GetSandboxes", mock.Anything).Return(sandboxes, nil)

	ctx := context.Background()
	count, err := mockNode.GetSandboxCount(ctx)

	assert.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestGetSandboxCount_Error(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	testErr := errors.New("failed to get sandboxes")
	mockNode.On("GetSandboxes", mock.Anything).Return(nil, testErr)

	ctx := context.Background()
	count, err := mockNode.GetSandboxCount(ctx)

	assert.Error(t, err)
	assert.Equal(t, 0, count)
}

func TestIsDrained_True(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	mockNode.On("GetSandboxes", mock.Anything).Return([]sandbox.Sandbox{}, nil)

	ctx := context.Background()
	isDrained, err := mockNode.IsDrained(ctx)

	assert.NoError(t, err)
	assert.True(t, isDrained)
}

func TestIsDrained_False(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	sandboxes := []sandbox.Sandbox{
		{SandboxID: "sandbox-1"},
	}
	mockNode.On("GetSandboxes", mock.Anything).Return(sandboxes, nil)

	ctx := context.Background()
	isDrained, err := mockNode.IsDrained(ctx)

	assert.NoError(t, err)
	assert.False(t, isDrained)
}

func TestDefaultDrainWaitConfig(t *testing.T) {
	config := DefaultDrainWaitConfig()

	assert.Equal(t, 2*time.Second, config.PollInterval)
	assert.Equal(t, 5*time.Minute, config.MaxWaitTime)
}

func TestWaitForDrain_GradualDrain(t *testing.T) {
	mockNode := new(MockNode)
	mockNode.Node = &Node{ID: "test-node"}

	// Simulate gradual drain: 3 -> 2 -> 1 -> 0
	callCount := 0
	mockNode.On("GetSandboxes", mock.Anything).Run(func(args mock.Arguments) {
		callCount++
	}).Return(func(ctx context.Context) []sandbox.Sandbox {
		switch callCount {
		case 1:
			return []sandbox.Sandbox{
				{SandboxID: "sandbox-1"},
				{SandboxID: "sandbox-2"},
				{SandboxID: "sandbox-3"},
			}
		case 2:
			return []sandbox.Sandbox{
				{SandboxID: "sandbox-1"},
				{SandboxID: "sandbox-2"},
			}
		case 3:
			return []sandbox.Sandbox{
				{SandboxID: "sandbox-1"},
			}
		default:
			return []sandbox.Sandbox{}
		}
	}, nil)

	ctx := context.Background()
	config := DrainWaitConfig{
		PollInterval: 50 * time.Millisecond,
		MaxWaitTime:  5 * time.Second,
	}

	duration, err := mockNode.WaitForDrainWithContext(ctx, config)

	assert.NoError(t, err)
	assert.Greater(t, duration, 100*time.Millisecond)
	assert.Less(t, duration, 1*time.Second)
}
