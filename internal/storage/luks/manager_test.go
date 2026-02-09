package luks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"piccolod/internal/state/paths"
)

// exitError returns an *exec.ExitError with the given exit code.
func exitError(code int) *exec.ExitError {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr
	}
	panic(fmt.Sprintf("expected *exec.ExitError for exit code %d, got %T", code, err))
}

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

func TestPoolManager_HasLUKSHeader(t *testing.T) {
	t.Run("header_present", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewPoolManager(run, nil)
		has, err := mgr.HasLUKSHeader(context.Background(), "/dev/sda4")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !has {
			t.Error("expected true when cryptsetup isLuks succeeds")
		}
	})

	t.Run("no_header", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				"cryptsetup isLuks /dev/sda4": exitError(1),
			},
		}
		mgr := NewPoolManager(run, nil)
		has, err := mgr.HasLUKSHeader(context.Background(), "/dev/sda4")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if has {
			t.Error("expected false when cryptsetup isLuks exits 1")
		}
	})

	t.Run("transient_error_propagated", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				"cryptsetup isLuks /dev/sda4": fmt.Errorf("I/O error"),
			},
		}
		mgr := NewPoolManager(run, nil)
		has, err := mgr.HasLUKSHeader(context.Background(), "/dev/sda4")
		if err == nil {
			t.Fatal("expected error for non-ExitError failure")
		}
		if has {
			t.Error("expected false on error")
		}
		if !strings.Contains(err.Error(), "I/O error") {
			t.Errorf("expected wrapped I/O error, got: %v", err)
		}
	})
}

func TestPoolManager_DetectOrphanedLUKSHeader(t *testing.T) {
	t.Run("LUKS_header_no_keyfile", func(t *testing.T) {
		paths.SetRootsForTest(t)
		// cryptsetup isLuks succeeds (device has LUKS header), no pool keyfile → orphaned.
		run := &fakeRunner{}
		mgr := NewPoolManager(run, nil)
		if !mgr.DetectOrphanedLUKSHeader(context.Background(), "/dev/sda4") {
			t.Error("expected orphaned when LUKS header exists but no pool keyfile")
		}
	})

	t.Run("no_LUKS_header", func(t *testing.T) {
		paths.SetRootsForTest(t)
		// cryptsetup isLuks exits 1 (fresh partition, no LUKS) → NOT orphaned.
		run := &fakeRunner{
			errs: map[string]error{
				"cryptsetup isLuks /dev/sda4": exitError(1),
			},
		}
		mgr := NewPoolManager(run, nil)
		if mgr.DetectOrphanedLUKSHeader(context.Background(), "/dev/sda4") {
			t.Error("expected not orphaned when no LUKS header on device")
		}
	})

	t.Run("LUKS_header_with_keyfile", func(t *testing.T) {
		core, _ := paths.SetRootsForTest(t)
		// Create pool keyfile → healthy state, NOT orphaned.
		cryptoDir := core + "/crypto"
		_ = os.MkdirAll(cryptoDir, 0o700)
		_ = os.WriteFile(cryptoDir+"/piccolo_data_pool_key.enc", []byte("key"), 0o600)
		run := &fakeRunner{} // isLuks succeeds (has LUKS header)
		mgr := NewPoolManager(run, nil)
		if mgr.DetectOrphanedLUKSHeader(context.Background(), "/dev/sda4") {
			t.Error("expected not orphaned when LUKS header and keyfile both exist")
		}
	})
}

func TestContains(t *testing.T) {
	if !contains([]string{"a", "b", "c"}, "b") {
		t.Error("expected true for existing element")
	}
	if contains([]string{"a", "b", "c"}, "d") {
		t.Error("expected false for missing element")
	}
}
