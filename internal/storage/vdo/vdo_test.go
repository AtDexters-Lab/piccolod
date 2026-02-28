package vdo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records all Run/RunWithOutput calls.
type fakeRunner struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func buildKey(name string, args []string) string {
	parts := append([]string{name}, args...)
	return strings.Join(parts, " ")
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	key := buildKey(name, args)
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := buildKey(name, args)
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
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
	return nil, nil
}

func (f *fakeRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	key := buildKey(name, args)
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func TestManager_Format(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewManager(run)

	if err := mgr.Format(context.Background(), "/dev/mapper/piccolo-vol-golden-abc"); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.calls))
	}
	want := "vdoformat --force /dev/mapper/piccolo-vol-golden-abc"
	if run.calls[0] != want {
		t.Errorf("got %q, want %q", run.calls[0], want)
	}
}

func TestManager_Format_Error(t *testing.T) {
	run := &fakeRunner{
		errs: map[string]error{
			"vdoformat --force /dev/mapper/test": fmt.Errorf("device busy"),
		},
	}
	mgr := NewManager(run)
	err := mgr.Format(context.Background(), "/dev/mapper/test")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "vdoformat") {
		t.Errorf("error should mention vdoformat: %v", err)
	}
}

func TestManager_Create(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			"blockdev --getsize64 /dev/mapper/piccolo-vol-golden-abc": "10737418240\n",
		},
	}
	mgr := NewManager(run)

	params := DefaultTargetParams(10737418240) // 10 GiB
	err := mgr.Create(context.Background(), "piccolo-vdo-golden-abc", "/dev/mapper/piccolo-vol-golden-abc", params)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(run.calls) != 2 {
		t.Fatalf("expected 2 calls (blockdev + dmsetup), got %d: %v", len(run.calls), run.calls)
	}

	// Verify blockdev was called first.
	if !strings.HasPrefix(run.calls[0], "blockdev") {
		t.Errorf("first call should be blockdev, got %q", run.calls[0])
	}

	// Verify dmsetup create with table.
	dmCall := run.calls[1]
	if !strings.HasPrefix(dmCall, "dmsetup create piccolo-vdo-golden-abc") {
		t.Errorf("expected dmsetup create, got %q", dmCall)
	}
	if !strings.Contains(dmCall, "vdo V4") {
		t.Errorf("table should contain 'vdo V4': %q", dmCall)
	}
	if !strings.Contains(dmCall, "compression on") {
		t.Errorf("table should contain 'compression on': %q", dmCall)
	}
	if !strings.Contains(dmCall, "deduplication off") {
		t.Errorf("table should contain 'deduplication off': %q", dmCall)
	}

	// Verify logical sectors = 10GiB / 512
	wantSectors := "20971520" // 10737418240 / 512
	if !strings.Contains(dmCall, "0 "+wantSectors+" vdo") {
		t.Errorf("expected logical sectors %s in table: %q", wantSectors, dmCall)
	}
}

func TestManager_Remove(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewManager(run)

	if err := mgr.Remove(context.Background(), "piccolo-vdo-test"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := "dmsetup remove piccolo-vdo-test"
	if run.calls[0] != want {
		t.Errorf("got %q, want %q", run.calls[0], want)
	}
}

func TestManager_Exists(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewManager(run)
		if !mgr.Exists(context.Background(), "piccolo-vdo-test") {
			t.Error("expected true when dmsetup info succeeds")
		}
	})

	t.Run("not_exists", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				"dmsetup info piccolo-vdo-test": fmt.Errorf("no such device"),
			},
		}
		mgr := NewManager(run)
		if mgr.Exists(context.Background(), "piccolo-vdo-test") {
			t.Error("expected false when dmsetup info fails")
		}
	})
}

func TestDevicePath(t *testing.T) {
	got := DevicePath("piccolo-vdo-golden-abc")
	want := "/dev/mapper/piccolo-vdo-golden-abc"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDefaultTargetParams(t *testing.T) {
	p := DefaultTargetParams(10 << 30)
	if p.LogicalSizeBytes != 10<<30 {
		t.Errorf("LogicalSizeBytes: %d", p.LogicalSizeBytes)
	}
	if p.MinIOSize != 4096 {
		t.Errorf("MinIOSize: %d", p.MinIOSize)
	}
	if p.BlockMapCacheKB != 131072 {
		t.Errorf("BlockMapCacheKB: %d", p.BlockMapCacheKB)
	}
	if p.BlockMapEraLen != 16380 {
		t.Errorf("BlockMapEraLen: %d", p.BlockMapEraLen)
	}
}

func TestManager_CheckAvailability(t *testing.T) {
	t.Run("available", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				"dmsetup targets": "striped          v1.6.0\nvdo              v8.3.0\nlinear           v1.4.0\n",
			},
		}
		mgr := NewManager(run)
		if err := mgr.CheckAvailability(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("no_vdoformat", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				"which vdoformat": fmt.Errorf("not found"),
			},
		}
		mgr := NewManager(run)
		err := mgr.CheckAvailability(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "vdoformat not found") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no_kernel_target", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				"dmsetup targets": "striped          v1.6.0\nlinear           v1.4.0\n",
			},
		}
		mgr := NewManager(run)
		err := mgr.CheckAvailability(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "dm-vdo target not loaded") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
