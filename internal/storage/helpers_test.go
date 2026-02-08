package storage

import (
	"context"
	"fmt"
	"strings"
)

// fakeRunner is a test double for runner.CommandRunner that returns
// pre-configured output keyed by "name arg1 arg2 ..." command strings.
type fakeRunner struct {
	outputs map[string]string // key = "name arg1 arg2 ...", value = stdout
	errs    map[string]error  // key = same, value = error to return
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	key := buildKey(name, args)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := buildKey(name, args)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return nil, err
		}
	}
	if f.outputs != nil {
		if out, ok := f.outputs[key]; ok {
			return []byte(out), nil
		}
	}
	return nil, fmt.Errorf("fakeRunner: no output configured for %q", key)
}

func (f *fakeRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	key := buildKey(name, args)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func buildKey(name string, args []string) string {
	parts := append([]string{name}, args...)
	return strings.Join(parts, " ")
}
