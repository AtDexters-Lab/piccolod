package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"piccolod/internal/resources/pressure"
)

// CommandRunner abstracts external command execution for testability.
type CommandRunner interface {
	// Run executes a command, connecting stdout/stderr to os.Stdout/os.Stderr.
	Run(ctx context.Context, name string, args ...string) error

	// RunWithOutput executes a command and returns its stdout.
	// Stderr is captured separately; on failure it is included in the error.
	RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error)

	// RunWithStdin executes a command with the provided bytes piped to stdin.
	RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error
}

// ExecRunner is the production CommandRunner that shells out via os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkClassFromContext(ctx, pressure.WorkNetworkProbe)); err != nil {
		return err
	}
	defer beginLifecycleOwner(ctx)()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (ExecRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkClassFromContext(ctx, pressure.WorkNetworkProbe)); err != nil {
		return nil, err
	}
	defer beginLifecycleOwner(ctx)()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		return out, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, err
}

func (ExecRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkClassFromContext(ctx, pressure.WorkNetworkProbe)); err != nil {
		return err
	}
	defer beginLifecycleOwner(ctx)()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func beginLifecycleOwner(ctx context.Context) func() {
	class := pressure.WorkClassFromContext(ctx, "")
	owner := string(class)
	if class == pressure.WorkNetworkProbe {
		owner = "network"
	}
	return pressure.BeginLifecycleOwner(owner)
}
