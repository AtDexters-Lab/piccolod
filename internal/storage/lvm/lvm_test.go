package lvm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/events"
)

// fakeRunner records all Run/RunWithOutput calls and returns preconfigured outputs/errors.
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

// getCalls returns a copy of the calls slice (thread-safe).
func (f *fakeRunner) getCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(f.calls))
	copy(cp, f.calls)
	return cp
}

// exitError returns an *exec.ExitError with the given exit code.
func exitError(code int) *exec.ExitError {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr
	}
	panic(fmt.Sprintf("expected *exec.ExitError for exit code %d, got %T", code, err))
}

// --- PoolManager tests ---

func TestNewPoolManager_DefaultConfig(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil, ThinPoolConfig{})
	// Should fill defaults.
	if mgr.cfg.VGName != DefaultVGName {
		t.Errorf("VGName: got %q, want %q", mgr.cfg.VGName, DefaultVGName)
	}
	if mgr.cfg.PoolName != DefaultThinPoolName {
		t.Errorf("PoolName: got %q, want %q", mgr.cfg.PoolName, DefaultThinPoolName)
	}
	if mgr.cfg.ExtentPct != ThinPoolExtentPercent {
		t.Errorf("ExtentPct: got %d, want %d", mgr.cfg.ExtentPct, ThinPoolExtentPercent)
	}
}

func TestNewPoolManager_PartialConfig(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil, ThinPoolConfig{
		ErrorOnFull: true,
	})
	// Should fill missing fields but preserve ErrorOnFull.
	if mgr.cfg.VGName != DefaultVGName {
		t.Errorf("VGName: got %q, want %q", mgr.cfg.VGName, DefaultVGName)
	}
	if !mgr.cfg.ErrorOnFull {
		t.Error("ErrorOnFull should be preserved")
	}
}

func TestPoolManager_CreatePool(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

	if err := mgr.CreatePool(context.Background(), "/dev/sda3"); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	// Verify command sequence: pvcreate → vgcreate → lvcreate → lvchange
	if len(run.calls) != 4 {
		t.Fatalf("expected 4 commands, got %d: %v", len(run.calls), run.calls)
	}
	if !strings.HasPrefix(run.calls[0], "pvcreate") {
		t.Errorf("first call should be pvcreate, got %q", run.calls[0])
	}
	if !strings.HasPrefix(run.calls[1], "vgcreate") {
		t.Errorf("second call should be vgcreate, got %q", run.calls[1])
	}
	if !strings.Contains(run.calls[2], "lvcreate") {
		t.Errorf("third call should be lvcreate, got %q", run.calls[2])
	}
	if !strings.Contains(run.calls[3], "lvchange") || !strings.Contains(run.calls[3], "--errorwhenfull") {
		t.Errorf("fourth call should be lvchange --errorwhenfull, got %q", run.calls[3])
	}
}

func TestPoolManager_CreatePool_PvcreateError(t *testing.T) {
	run := &fakeRunner{
		errs: map[string]error{
			"pvcreate -f /dev/sda3": fmt.Errorf("device busy"),
		},
	}
	mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
	err := mgr.CreatePool(context.Background(), "/dev/sda3")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "pvcreate") {
		t.Errorf("error should mention pvcreate, got: %v", err)
	}
}

func TestPoolManager_ActivatePool(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
	if err := mgr.ActivatePool(context.Background()); err != nil {
		t.Fatalf("ActivatePool: %v", err)
	}
	if len(run.calls) != 1 || !strings.Contains(run.calls[0], "vgchange -ay") {
		t.Errorf("expected vgchange -ay call, got: %v", run.calls)
	}
}

func TestPoolManager_DeactivatePool(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
	if err := mgr.DeactivatePool(context.Background()); err != nil {
		t.Fatalf("DeactivatePool: %v", err)
	}
	if len(run.calls) != 1 || !strings.Contains(run.calls[0], "vgchange -an") {
		t.Errorf("expected vgchange -an call, got: %v", run.calls)
	}
}

func TestPoolManager_PoolStatus(t *testing.T) {
	t.Run("valid_output", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
					"-o", "data_percent,metadata_percent,lv_size",
					"piccolo-data-vg/thinpool"}): "  42.5   10.2   107374182400\n",
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		stats, err := mgr.PoolStatus(context.Background())
		if err != nil {
			t.Fatalf("PoolStatus: %v", err)
		}
		if stats.DataPercent != 42.5 {
			t.Errorf("DataPercent: got %.1f, want 42.5", stats.DataPercent)
		}
		if stats.MetadataPercent != 10.2 {
			t.Errorf("MetadataPercent: got %.1f, want 10.2", stats.MetadataPercent)
		}
		if stats.TotalDataBytes != 107374182400 {
			t.Errorf("TotalDataBytes: got %d, want 107374182400", stats.TotalDataBytes)
		}
	})

	t.Run("parse_error", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
					"-o", "data_percent,metadata_percent,lv_size",
					"piccolo-data-vg/thinpool"}): "  notanumber   10.2   107374182400\n",
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		_, err := mgr.PoolStatus(context.Background())
		if err == nil {
			t.Fatal("expected parse error")
		}
		if !strings.Contains(err.Error(), "parse data_percent") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("too_few_fields", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
					"-o", "data_percent,metadata_percent,lv_size",
					"piccolo-data-vg/thinpool"}): "  42.5\n",
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		_, err := mgr.PoolStatus(context.Background())
		if err == nil {
			t.Fatal("expected error for insufficient fields")
		}
	})
}

func TestPoolManager_VGExists(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		exists, err := mgr.VGExists(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exists {
			t.Error("expected true when vgs succeeds")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				buildKey("vgs", []string{"--noheadings", "piccolo-data-vg"}): exitError(5),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		exists, err := mgr.VGExists(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exists {
			t.Error("expected false when vgs exits 5")
		}
	})

	t.Run("transient_error", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				buildKey("vgs", []string{"--noheadings", "piccolo-data-vg"}): fmt.Errorf("I/O timeout"),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		exists, err := mgr.VGExists(context.Background())
		if err == nil {
			t.Fatal("expected error for transient failure")
		}
		if exists {
			t.Error("expected false on error")
		}
	})
}

func TestPoolManager_HealthMonitor_StartStop(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
				"-o", "data_percent,metadata_percent,lv_size",
				"piccolo-data-vg/thinpool"}): "  50.0   5.0   107374182400\n",
		},
	}
	bus := events.NewBus()
	defer bus.Close()

	mgr := NewPoolManager(run, bus, DefaultThinPoolConfig())
	mgr.StartHealthMonitor(10 * time.Millisecond)

	// Second call is a no-op (idempotent).
	mgr.StartHealthMonitor(10 * time.Millisecond)

	// Wait for at least one health check.
	time.Sleep(50 * time.Millisecond)

	mgr.StopHealthMonitor()

	// Verify lvs was called at least once.
	found := false
	for _, call := range run.getCalls() {
		if strings.Contains(call, "lvs") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one lvs call from health monitor")
	}
}

func TestPoolManager_HealthMonitor_ThresholdEvent(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
				"-o", "data_percent,metadata_percent,lv_size",
				"piccolo-data-vg/thinpool"}): "  85.0   5.0   107374182400\n",
		},
	}
	bus := events.NewBus()
	defer bus.Close()
	ch := bus.Subscribe(events.TopicStorageEmergency, 1)

	mgr := NewPoolManager(run, bus, DefaultThinPoolConfig())
	mgr.StartHealthMonitor(10 * time.Millisecond)
	defer mgr.StopHealthMonitor()

	// Wait for threshold event (85% > 80% warning threshold).
	select {
	case evt := <-ch:
		payload := evt.Payload.(events.StorageEmergencyEvent)
		if payload.Level != "soft" {
			t.Errorf("expected soft level, got %q", payload.Level)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for threshold event")
	}
}

// --- LVManager tests ---

func TestLVManager_CreateThinLV(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	if err := mgr.CreateThinLV(context.Background(), "vol-test", 10*1024*1024*1024); err != nil {
		t.Fatalf("CreateThinLV: %v", err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(run.calls))
	}
	if !strings.Contains(run.calls[0], "lvcreate") {
		t.Errorf("expected lvcreate, got %q", run.calls[0])
	}
	if !strings.Contains(run.calls[0], "vol-test") {
		t.Errorf("expected vol-test in args, got %q", run.calls[0])
	}
}

func TestLVManager_CreateThinLV_aligns_to_sector(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	// 1319306057B is the size that triggered the original bug — not a multiple of 512.
	if err := mgr.CreateThinLV(context.Background(), "golden-test", 1319306057); err != nil {
		t.Fatalf("CreateThinLV: %v", err)
	}
	call := run.calls[0]
	// Must be aligned: 1319306057 → ceil to 512 → 1319306240
	if !strings.Contains(call, "1319306240B") {
		t.Errorf("expected aligned size 1319306240B in lvcreate args, got %q", call)
	}
}

func TestAlignToSector(t *testing.T) {
	tests := []struct {
		input int64
		want  int64
	}{
		{0, 0},
		{1, 512},
		{511, 512},
		{512, 512},
		{513, 1024},
		{1024, 1024},
		{1319306057, 1319306240}, // the real-world failing case
		{10 << 30, 10 << 30},    // already aligned (10 GiB)
	}
	for _, tt := range tests {
		got := alignToSector(tt.input)
		if got != tt.want {
			t.Errorf("alignToSector(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestLVManager_RemoveThinLV(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	if err := mgr.RemoveThinLV(context.Background(), "vol-test"); err != nil {
		t.Fatalf("RemoveThinLV: %v", err)
	}
	if !strings.Contains(run.calls[0], "lvremove -f") {
		t.Errorf("expected lvremove -f, got %q", run.calls[0])
	}
}

func TestLVManager_ActivateDeactivate(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	if err := mgr.ActivateLV(context.Background(), "vol-test"); err != nil {
		t.Fatalf("ActivateLV: %v", err)
	}
	if !strings.Contains(run.calls[0], "lvchange -ay") {
		t.Errorf("expected lvchange -ay, got %q", run.calls[0])
	}

	if err := mgr.DeactivateLV(context.Background(), "vol-test"); err != nil {
		t.Fatalf("DeactivateLV: %v", err)
	}
	if !strings.Contains(run.calls[1], "lvchange -an") {
		t.Errorf("expected lvchange -an, got %q", run.calls[1])
	}
}

func TestLVManager_LVExists(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if !mgr.LVExists(context.Background(), "vol-test") {
			t.Error("expected true when lvs succeeds")
		}
	})

	t.Run("not_exists", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				buildKey("lvs", []string{"--noheadings", "piccolo-data-vg/vol-test"}): fmt.Errorf("not found"),
			},
		}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if mgr.LVExists(context.Background(), "vol-test") {
			t.Error("expected false when lvs fails")
		}
	})
}

func TestLVManager_LVPath(t *testing.T) {
	mgr := NewLVManager(nil, DefaultVGName, DefaultThinPoolName)
	got := mgr.LVPath("vol-test")
	want := "/dev/piccolo-data-vg/vol-test"
	if got != want {
		t.Errorf("LVPath: got %q, want %q", got, want)
	}
}

func TestLVManager_ListLVs(t *testing.T) {
	t.Run("mixed_pools", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
					"-o", "lv_name,lv_size,lv_attr,pool_lv",
					"piccolo-data-vg"}): strings.Join([]string{
					"  vol-app1   10737418240   Vwi-a-tz--   thinpool",
					"  vol-app2   5368709120    Vwi---tz--   thinpool",
					"  thinpool   107374182400  twi-a-t---   ",
					"  other-lv   1073741824    Vwi-a-tz--   otherpool",
				}, "\n"),
			},
		}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		lvs, err := mgr.ListLVs(context.Background())
		if err != nil {
			t.Fatalf("ListLVs: %v", err)
		}
		// Should only include vol-app1 and vol-app2 (thinpool), not thinpool itself or other-lv.
		if len(lvs) != 2 {
			t.Fatalf("expected 2 LVs, got %d: %+v", len(lvs), lvs)
		}
		if lvs[0].Name != "vol-app1" {
			t.Errorf("first LV: got %q, want vol-app1", lvs[0].Name)
		}
		if !lvs[0].Active {
			t.Error("vol-app1 should be active (attr has 'a' at index 4)")
		}
		if lvs[1].Active {
			t.Error("vol-app2 should be inactive (attr has '-' at index 4)")
		}
	})

	t.Run("empty_output", func(t *testing.T) {
		run := &fakeRunner{
			outputs: map[string]string{
				buildKey("lvs", []string{"--noheadings", "--nosuffix", "--units", "b",
					"-o", "lv_name,lv_size,lv_attr,pool_lv",
					"piccolo-data-vg"}): "",
			},
		}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		lvs, err := mgr.ListLVs(context.Background())
		if err != nil {
			t.Fatalf("ListLVs: %v", err)
		}
		if len(lvs) != 0 {
			t.Errorf("expected 0 LVs, got %d", len(lvs))
		}
	})
}

func TestLVManager_CreateSnapshot(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if err := mgr.CreateSnapshot(context.Background(), "golden-abc123", "ws-instance1"); err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}
		if len(run.calls) != 1 {
			t.Fatalf("expected 1 call, got %d: %v", len(run.calls), run.calls)
		}
		call := run.calls[0]
		if !strings.Contains(call, "lvcreate") {
			t.Errorf("expected lvcreate, got %q", call)
		}
		if !strings.Contains(call, "--snapshot") {
			t.Errorf("expected --snapshot flag: %q", call)
		}
		if !strings.Contains(call, "--name ws-instance1") {
			t.Errorf("expected --name ws-instance1: %q", call)
		}
		if !strings.Contains(call, "--setactivationskip n") {
			t.Errorf("expected --setactivationskip n: %q", call)
		}
		if !strings.Contains(call, "piccolo-data-vg/golden-abc123") {
			t.Errorf("expected origin path: %q", call)
		}
	})

	t.Run("error", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				buildKey("lvcreate", []string{"--snapshot", "--name", "ws-fail",
					"--setactivationskip", "n", "piccolo-data-vg/golden-abc"}): fmt.Errorf("insufficient space"),
			},
		}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		err := mgr.CreateSnapshot(context.Background(), "golden-abc", "ws-fail")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "lvcreate snapshot") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestLVManager_ResizeLV(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
	if err := mgr.ResizeLV(context.Background(), "vol-test", 20*1024*1024*1024); err != nil {
		t.Fatalf("ResizeLV: %v", err)
	}
	if !strings.Contains(run.calls[0], "lvresize") {
		t.Errorf("expected lvresize, got %q", run.calls[0])
	}
}

func TestLVManager_RenameLV(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if err := mgr.RenameLV(context.Background(), "vol-old", "vol-new"); err != nil {
			t.Fatalf("RenameLV: %v", err)
		}
		if len(run.calls) != 1 {
			t.Fatalf("expected 1 call, got %d: %v", len(run.calls), run.calls)
		}
		call := run.calls[0]
		if !strings.Contains(call, "lvrename") {
			t.Errorf("expected lvrename, got %q", call)
		}
		if !strings.Contains(call, DefaultVGName) {
			t.Errorf("expected VG name in args: %q", call)
		}
		if !strings.Contains(call, "vol-old") || !strings.Contains(call, "vol-new") {
			t.Errorf("expected old and new names in args: %q", call)
		}
	})

	t.Run("error", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				buildKey("lvrename", []string{DefaultVGName, "vol-old", "vol-new"}): fmt.Errorf("LV locked"),
			},
		}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		err := mgr.RenameLV(context.Background(), "vol-old", "vol-new")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "lvrename") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// --- Types tests ---

func TestThresholdLevel(t *testing.T) {
	tests := []struct {
		pct  float64
		want int
	}{
		{0.0, 0},
		{50.0, 0},
		{79.9, 0},
		{80.0, ThresholdWarning},
		{89.9, ThresholdWarning},
		{90.0, ThresholdCritical},
		{94.9, ThresholdCritical},
		{95.0, ThresholdUrgent},
		{100.0, ThresholdUrgent},
	}
	for _, tt := range tests {
		stats := PoolStats{DataPercent: tt.pct}
		if got := stats.ThresholdLevel(); got != tt.want {
			t.Errorf("ThresholdLevel(%.1f%%): got %d, want %d", tt.pct, got, tt.want)
		}
	}
}

func TestDefaultThinPoolConfig(t *testing.T) {
	cfg := DefaultThinPoolConfig()
	if cfg.VGName != DefaultVGName {
		t.Errorf("VGName: %q", cfg.VGName)
	}
	if cfg.PoolName != DefaultThinPoolName {
		t.Errorf("PoolName: %q", cfg.PoolName)
	}
	if cfg.ExtentPct != ThinPoolExtentPercent {
		t.Errorf("ExtentPct: %d", cfg.ExtentPct)
	}
	if !cfg.ErrorOnFull {
		t.Error("ErrorOnFull should default to true")
	}
}

func TestThinPoolConfig_PoolPath(t *testing.T) {
	cfg := DefaultThinPoolConfig()
	got := cfg.PoolPath()
	want := "/dev/piccolo-data-vg/thinpool"
	if got != want {
		t.Errorf("PoolPath: got %q, want %q", got, want)
	}
}
