package workspacedisk

import "errors"

var (
	// ErrNotInitialized indicates the workspace disk has not been initialized.
	ErrNotInitialized = errors.New("workspace disk: not initialized")

	// ErrAlreadyInitialized indicates the workspace disk was already initialized.
	ErrAlreadyInitialized = errors.New("workspace disk: already initialized")

	// ErrAlreadyMounted indicates the overlay is already mounted.
	ErrAlreadyMounted = errors.New("workspace disk: already mounted")

	// ErrNotMounted indicates the overlay is not mounted.
	ErrNotMounted = errors.New("workspace disk: not mounted")

	// ErrBaseImageMissing indicates the base image is not available locally.
	ErrBaseImageMissing = errors.New("workspace disk: base image not available")

	// ErrMountFailed indicates the overlay mount operation failed.
	ErrMountFailed = errors.New("workspace disk: mount failed")

	// ErrUnmountFailed indicates the overlay unmount operation failed.
	ErrUnmountFailed = errors.New("workspace disk: unmount failed")

	// ErrStaleMount indicates a stale mount was detected and cleanup is needed.
	ErrStaleMount = errors.New("workspace disk: stale mount detected")
)

// WorkspaceDiskError wraps workspace disk errors with context.
type WorkspaceDiskError struct {
	InstanceID string
	Operation  string
	Err        error
}

func (e *WorkspaceDiskError) Error() string {
	return "workspace disk [" + e.InstanceID + "] " + e.Operation + ": " + e.Err.Error()
}

func (e *WorkspaceDiskError) Unwrap() error {
	return e.Err
}

// WrapError wraps an error with instance and operation context.
func WrapError(instanceID, operation string, err error) error {
	if err == nil {
		return nil
	}
	return &WorkspaceDiskError{
		InstanceID: instanceID,
		Operation:  operation,
		Err:        err,
	}
}
