package runner

import (
	"context"
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

func TestExecRunner_RunWithStdin(t *testing.T) {
	r := ExecRunner{}
	err := r.RunWithStdin(context.Background(), []byte("data"), "cat")
	if err != nil {
		t.Fatalf("RunWithStdin: %v", err)
	}
}
