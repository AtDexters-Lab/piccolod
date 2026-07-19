package container

import (
	"context"
	"os/exec"
)

// SystemExecutor abstracts OS command execution for testability.
// The default implementation delegates to exec.Command().CombinedOutput().
// Tests can replace defaultExecutor to mock system commands (useradd, loginctl, etc.)
// without requiring root privileges.
type SystemExecutor interface {
	Run(name string, args ...string) ([]byte, error)
	RunContext(ctx context.Context, name string, args ...string) ([]byte, error)
}

type osExecutor struct{}

func (osExecutor) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func (osExecutor) RunContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// defaultExecutor is the package-level executor used by appuser provisioning functions.
// Tests in this package can replace it with a mock but must not use t.Parallel()
// when doing so, since the variable is shared mutable state.
var defaultExecutor SystemExecutor = osExecutor{}
