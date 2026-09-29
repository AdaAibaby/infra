package orchestrator

import "errors"

var (
	ErrSandboxNotFound        = errors.New("sandbox not found")
	ErrAccessForbidden        = errors.New("access forbidden")
	ErrSandboxOperationFailed = errors.New("sandbox operation failed")
	// ErrRefusedRouteLost: the node refused the pause retryably but the edge
	// could not put the sandbox's route back, so the sandbox cannot be kept.
	ErrRefusedRouteLost = errors.New("pause refused by the node and the edge could not restore its route")
	// ErrPausePreservedSandbox: the node's snapshot failed but it resumed the
	// sandbox in place instead of destroying it (e2b-dev/infra#3658). Handled
	// like a retryable refusal: the store record and route are restored so the
	// still-healthy sandbox stays usable, rather than removed.
	ErrPausePreservedSandbox = errors.New("pause snapshot failed but the node preserved the sandbox")
)
