package luks

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"piccolod/internal/state/paths"
)

// fakeRunner records all Run/RunWithOutput calls and returns preconfigured outputs/errors.
type fakeRunner struct {
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	key := buildKey(name, args)
	f.calls = append(f.calls, key)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := buildKey(name, args)
	f.calls = append(f.calls, key)
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
	return nil, fmt.Errorf("fakeRunner: no output for %q", key)
}

func (f *fakeRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	key := buildKey(name, args)
	f.calls = append(f.calls, key)
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

func TestPoolManager_Lock(t *testing.T) {
	paths.SetRootsForTest(t)

	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil)

	if err := mgr.Lock(context.Background()); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Verify commands.
	if len(run.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d: %v", len(run.calls), run.calls)
	}
	if !strings.HasPrefix(run.calls[0], "umount ") {
		t.Errorf("first call should be umount, got %q", run.calls[0])
	}
	if !strings.Contains(run.calls[1], "cryptsetup close piccolo_data_pool_0") {
		t.Errorf("second call should be cryptsetup close, got %q", run.calls[1])
	}
}

func TestPoolManager_isMapperActive(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil)

	// Default: cryptsetup status succeeds → active.
	if !mgr.isMapperActive(context.Background(), "piccolo_data_pool_0") {
		t.Error("expected mapper to be active when status succeeds")
	}

	// Status fails → inactive.
	run.errs = map[string]error{
		"cryptsetup status piccolo_data_pool_0": fmt.Errorf("not active"),
	}
	if mgr.isMapperActive(context.Background(), "piccolo_data_pool_0") {
		t.Error("expected mapper to be inactive when status fails")
	}
}

func TestPoolManager_DetectOrphanedLUKSHeader(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	_ = core

	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil)

	// No pool keyfile → orphaned.
	if !mgr.DetectOrphanedLUKSHeader("/dev/sda3") {
		t.Error("expected orphaned header when no pool keyfile")
	}
}

func TestContains(t *testing.T) {
	if !contains([]string{"a", "b", "c"}, "b") {
		t.Error("expected true for existing element")
	}
	if contains([]string{"a", "b", "c"}, "d") {
		t.Error("expected false for missing element")
	}
}
