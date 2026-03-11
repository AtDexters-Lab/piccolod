package runner

import (
	"context"
	"strings"
	"testing"
)

func TestExecRunner_Run(t *testing.T) {
	r := ExecRunner{}
	if err := r.Run(context.Background(), "true"); err != nil {
		t.Fatalf("Run true: %v", err)
	}
}

func TestExecRunner_RunWithOutput(t *testing.T) {
	r := ExecRunner{}
	out, err := r.RunWithOutput(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("RunWithOutput: %v", err)
	}
	if got := string(out); got != "hello\n" {
		t.Fatalf("expected 'hello\\n', got %q", got)
	}
}

func TestExecRunner_RunWithOutput_stderr_not_in_stdout(t *testing.T) {
	r := ExecRunner{}
	// sh -c writes "warn" to stderr and "data" to stdout.
	out, err := r.RunWithOutput(context.Background(), "sh", "-c", "echo warn >&2; echo data")
	if err != nil {
		t.Fatalf("RunWithOutput: %v", err)
	}
	if got := string(out); got != "data\n" {
		t.Fatalf("expected stdout only, got %q", got)
	}
}

func TestExecRunner_RunWithOutput_error_includes_stderr(t *testing.T) {
	r := ExecRunner{}
	_, err := r.RunWithOutput(context.Background(), "sh", "-c", "echo oops >&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "oops") {
		t.Fatalf("expected stderr in error, got %q", got)
	}
}

func TestExecRunner_RunWithStdin(t *testing.T) {
	r := ExecRunner{}
	err := r.RunWithStdin(context.Background(), []byte("data"), "cat")
	if err != nil {
		t.Fatalf("RunWithStdin: %v", err)
	}
}
