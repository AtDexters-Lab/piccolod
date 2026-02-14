package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"time"
)

// CommandRunner abstracts external command execution for testability.
type CommandRunner interface {
	// Run executes a command, connecting stdout/stderr to os.Stdout/os.Stderr.
	Run(ctx context.Context, name string, args ...string) error

	// RunWithOutput executes a command and returns its combined stdout/stderr.
	RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error)

	// RunWithStdin executes a command with the provided bytes piped to stdin.
	RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error
}

// ExecRunner is the production CommandRunner that shells out via os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (ExecRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}

func (ExecRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
