// Package testutil provides shared test doubles for cross-package use.
package testutil

import (
	"context"
	"strings"
	"sync"
)

// FakeRunner is a configurable test double for runner.CommandRunner.
// It records all calls and returns pre-configured outputs and errors
// keyed by "name arg1 arg2 ..." command strings.
//
// Error lookup order: exact key match first, then bare command name fallback.
// Output lookup order: exact key match first, then bare command name fallback.
type FakeRunner struct {
	mu      sync.Mutex
	calls   []string
	Outputs map[string]string // key → stdout string
	Errs    map[string]error  // key → error to return
}

// BuildKey constructs the lookup key for a command invocation.
func BuildKey(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}

func (f *FakeRunner) Run(_ context.Context, name string, args ...string) error {
	key := BuildKey(name, args)
	f.mu.Lock()
	f.calls = append(f.calls, key)
	err := f.lookupErr(key, name)
	f.mu.Unlock()
	return err
}

func (f *FakeRunner) RunWithOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	key := BuildKey(name, args)
	f.mu.Lock()
	f.calls = append(f.calls, key)
	out := f.lookupOutput(key, name)
	err := f.lookupErr(key, name)
	f.mu.Unlock()
	return []byte(out), err
}

func (f *FakeRunner) RunWithStdin(_ context.Context, _ []byte, name string, args ...string) error {
	return f.Run(context.Background(), name, args...)
}

// GetCalls returns a copy of the recorded call keys.
func (f *FakeRunner) GetCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// lookupErr checks exact key first, then falls back to bare command name.
// Must be called with f.mu held.
func (f *FakeRunner) lookupErr(key, name string) error {
	if f.Errs == nil {
		return nil
	}
	if err, ok := f.Errs[key]; ok {
		return err
	}
	return f.Errs[name]
}

// lookupOutput checks exact key first, then falls back to bare command name.
// Must be called with f.mu held.
func (f *FakeRunner) lookupOutput(key, name string) string {
	if f.Outputs == nil {
		return ""
	}
	if out, ok := f.Outputs[key]; ok {
		return out
	}
	return f.Outputs[name]
}
