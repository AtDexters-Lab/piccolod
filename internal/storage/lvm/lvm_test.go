package lvm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/testutil"
)

type fakeRunner = testutil.FakeRunner

var buildKey = testutil.BuildKey

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
	t.Run("fresh_device", func(t *testing.T) {
		// pvs returns error (not a PV) — no deactivation needed.
		run := &fakeRunner{
			Errs: map[string]error{
				buildKey("pvs", []string{"--noheadings", "-o", "vg_name", "/dev/sda3"}): fmt.Errorf("not a PV"),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		if err := mgr.CreatePool(context.Background(), "/dev/sda3"); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		// Verify: pvs → wipefs → pvcreate → vgcreate → lvcreate → lvchange
		calls := run.GetCalls()
		if len(calls) != 6 {
			t.Fatalf("expected 6 commands, got %d: %v", len(calls), calls)
		}
		if !strings.HasPrefix(calls[0], "pvs") {
			t.Errorf("first call should be pvs, got %q", calls[0])
		}
		if !strings.HasPrefix(calls[1], "wipefs") {
			t.Errorf("second call should be wipefs, got %q", calls[1])
		}
		if !strings.HasPrefix(calls[2], "pvcreate") {
			t.Errorf("third call should be pvcreate, got %q", calls[2])
		}
		if !strings.HasPrefix(calls[3], "vgcreate") {
			t.Errorf("fourth call should be vgcreate, got %q", calls[3])
		}
		if !strings.Contains(calls[4], "lvcreate") {
			t.Errorf("fifth call should be lvcreate, got %q", calls[4])
		}
		if !strings.Contains(calls[5], "lvchange") || !strings.Contains(calls[5], "--errorwhenfull") {
			t.Errorf("sixth call should be lvchange --errorwhenfull, got %q", calls[5])
		}
	})

	t.Run("leftover_VG_deactivated", func(t *testing.T) {
		// pvs returns our VG name — deactivate + wipefs + create.
		run := &fakeRunner{
			Outputs: map[string]string{
				buildKey("pvs", []string{"--noheadings", "-o", "vg_name", "/dev/sda3"}): "  piccolo-data-vg\n",
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		if err := mgr.CreatePool(context.Background(), "/dev/sda3"); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		// Verify: pvs → vgchange -an → wipefs → pvcreate → vgcreate → lvcreate → lvchange
		calls := run.GetCalls()
		if len(calls) != 7 {
			t.Fatalf("expected 7 commands, got %d: %v", len(calls), calls)
		}
		if !strings.HasPrefix(calls[0], "pvs") {
			t.Errorf("first call should be pvs, got %q", calls[0])
		}
		if !strings.Contains(calls[1], "vgchange -an") {
			t.Errorf("second call should be vgchange -an, got %q", calls[1])
		}
		if !strings.HasPrefix(calls[2], "wipefs") {
			t.Errorf("third call should be wipefs, got %q", calls[2])
		}
	})

	t.Run("leftover_VG_deactivate_fails", func(t *testing.T) {
		// pvs returns our VG, but deactivation fails (LV open) — must abort.
		run := &fakeRunner{
			Outputs: map[string]string{
				buildKey("pvs", []string{"--noheadings", "-o", "vg_name", "/dev/sda3"}): "  piccolo-data-vg\n",
			},
			Errs: map[string]error{
				buildKey("vgchange", []string{"-an", "piccolo-data-vg"}): fmt.Errorf("LV in use"),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		err := mgr.CreatePool(context.Background(), "/dev/sda3")
		if err == nil {
			t.Fatal("expected error when VG deactivation fails")
		}
		if !strings.Contains(err.Error(), "deactivate leftover VG") {
			t.Errorf("error should mention deactivation, got: %v", err)
		}
	})

	t.Run("foreign_VG_not_deactivated", func(t *testing.T) {
		// pvs returns a different VG name — skip deactivation, just wipefs.
		run := &fakeRunner{
			Outputs: map[string]string{
				buildKey("pvs", []string{"--noheadings", "-o", "vg_name", "/dev/sda3"}): "  ubuntu-vg\n",
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		if err := mgr.CreatePool(context.Background(), "/dev/sda3"); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}

		// No vgchange -an — foreign VG left alone.
		calls := run.GetCalls()
		if len(calls) != 6 {
			t.Fatalf("expected 6 commands, got %d: %v", len(calls), calls)
		}
		if !strings.HasPrefix(calls[0], "pvs") {
			t.Errorf("first call should be pvs, got %q", calls[0])
		}
		if !strings.HasPrefix(calls[1], "wipefs") {
			t.Errorf("second call should be wipefs (no vgchange), got %q", calls[1])
		}
	})
}

func TestPoolManager_CreatePool_PvcreateError(t *testing.T) {
	run := &fakeRunner{
		Errs: map[string]error{
			buildKey("pvs", []string{"--noheadings", "-o", "vg_name", "/dev/sda3"}): fmt.Errorf("not a PV"),
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

func TestPoolManager_CreatePool_WipefsError(t *testing.T) {
	run := &fakeRunner{
		Errs: map[string]error{
			buildKey("pvs", []string{"--noheadings", "-o", "vg_name", "/dev/sda3"}): fmt.Errorf("not a PV"),
			"wipefs -a /dev/sda3": fmt.Errorf("permission denied"),
		},
	}
	mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
	err := mgr.CreatePool(context.Background(), "/dev/sda3")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "wipefs") {
		t.Errorf("error should mention wipefs, got: %v", err)
	}
}

func TestPoolManager_ActivatePool(t *testing.T) {
	t.Run("clean_activation", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
		if err := mgr.ActivatePool(context.Background()); err != nil {
			t.Fatalf("ActivatePool: %v", err)
		}
		calls := run.GetCalls()
		if len(calls) != 1 || !strings.Contains(calls[0], "vgchange -ay") {
			t.Errorf("expected single vgchange -ay call, got: %v", calls)
		}
		if strings.Contains(calls[0], "--partial") {
			t.Errorf("clean activation should not use --partial, got: %v", calls[0])
		}
	})

	t.Run("degraded_fallback", func(t *testing.T) {
		cfg := DefaultThinPoolConfig()
		cleanKey := buildKey("vgchange", []string{"-ay", cfg.VGName})
		run := &fakeRunner{
			Errs: map[string]error{
				cleanKey: fmt.Errorf("VG not found"),
			},
		}
		mgr := NewPoolManager(run, nil, cfg)
		if err := mgr.ActivatePool(context.Background()); err != nil {
			t.Fatalf("ActivatePool should succeed via fallback: %v", err)
		}
		calls := run.GetCalls()
		if len(calls) != 2 {
			t.Fatalf("expected 2 calls (clean + fallback), got %d: %v", len(calls), calls)
		}
		if strings.Contains(calls[0], "--partial") {
			t.Errorf("first call should not use --partial, got: %v", calls[0])
		}
		if !strings.Contains(calls[1], "--partial") {
			t.Errorf("second call should use --partial, got: %v", calls[1])
		}
	})

	t.Run("both_fail", func(t *testing.T) {
		cfg := DefaultThinPoolConfig()
		cleanKey := buildKey("vgchange", []string{"-ay", cfg.VGName})
		partialKey := buildKey("vgchange", []string{"-ay", "--partial", cfg.VGName})
		run := &fakeRunner{
			Errs: map[string]error{
				cleanKey:   fmt.Errorf("VG not found"),
				partialKey: fmt.Errorf("PV missing"),
			},
		}
		mgr := NewPoolManager(run, nil, cfg)
		if err := mgr.ActivatePool(context.Background()); err == nil {
			t.Fatal("ActivatePool should fail when both attempts fail")
		}
	})
}

func TestPoolManager_DeactivatePool(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())
	if err := mgr.DeactivatePool(context.Background()); err != nil {
		t.Fatalf("DeactivatePool: %v", err)
	}
	calls := run.GetCalls()
	if len(calls) != 1 || !strings.Contains(calls[0], "vgchange -an") {
		t.Errorf("expected vgchange -an call, got: %v", calls)
	}
}

func TestPoolManager_PoolStatus(t *testing.T) {
	t.Run("valid_output", func(t *testing.T) {
		run := &fakeRunner{
			Outputs: map[string]string{
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
			Outputs: map[string]string{
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
			Outputs: map[string]string{
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
			Errs: map[string]error{
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
			Errs: map[string]error{
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
		Outputs: map[string]string{
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
	for _, call := range run.GetCalls() {
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
		Outputs: map[string]string{
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

func TestPoolManager_ExpandPool(t *testing.T) {
	missingPVKey := buildKey("vgs", []string{"--noheadings", "-o", "vg_missing_pv_count", "piccolo-data-vg"})
	pvresizeKey := buildKey("pvresize", []string{"/dev/sda4"})
	lvextendKey := buildKey("lvextend", []string{"-l", "95%VG", "piccolo-data-vg/thinpool"})

	t.Run("success", func(t *testing.T) {
		run := &fakeRunner{
			Outputs: map[string]string{
				missingPVKey: "  0\n",
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		if err := mgr.ExpandPool(context.Background(), "/dev/sda4"); err != nil {
			t.Fatalf("ExpandPool: %v", err)
		}

		calls := run.GetCalls()
		if len(calls) != 3 {
			t.Fatalf("expected 3 commands, got %d: %v", len(calls), calls)
		}
		if calls[0] != missingPVKey {
			t.Errorf("first call should be vgs missing PV check, got %q", calls[0])
		}
		if calls[1] != pvresizeKey {
			t.Errorf("second call should be pvresize, got %q", calls[1])
		}
		if calls[2] != lvextendKey {
			t.Errorf("third call should be lvextend, got %q", calls[2])
		}
	})

	t.Run("already_at_target_size", func(t *testing.T) {
		// Steady-state boot: pool already at 95%VG. lvextend reports
		// "matches existing size" — should be treated as success.
		run := &fakeRunner{
			Outputs: map[string]string{
				missingPVKey: "  0\n",
				lvextendKey:  "New size matches existing size",
			},
			Errs: map[string]error{
				lvextendKey: fmt.Errorf("exit status 5: New size matches existing size"),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		if err := mgr.ExpandPool(context.Background(), "/dev/sda4"); err != nil {
			t.Fatalf("expected 'matches existing size' to be treated as success, got: %v", err)
		}
	})

	t.Run("degraded_VG_skips_expansion", func(t *testing.T) {
		run := &fakeRunner{
			Outputs: map[string]string{
				missingPVKey: "  1\n", // 1 missing PV
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		if err := mgr.ExpandPool(context.Background(), "/dev/sda4"); err != nil {
			t.Fatalf("ExpandPool should succeed (skip), got: %v", err)
		}

		// Only the missing PV check should be called.
		calls := run.GetCalls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 command (VG check only), got %d: %v", len(calls), calls)
		}
	})

	t.Run("pvresize_error", func(t *testing.T) {
		run := &fakeRunner{
			Outputs: map[string]string{
				missingPVKey: "  0\n",
			},
			Errs: map[string]error{
				pvresizeKey: fmt.Errorf("device busy"),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		err := mgr.ExpandPool(context.Background(), "/dev/sda4")
		if err == nil {
			t.Fatal("expected error from pvresize failure")
		}
		if !strings.Contains(err.Error(), "pvresize") {
			t.Errorf("error should mention pvresize, got: %v", err)
		}
	})

	t.Run("lvextend_error", func(t *testing.T) {
		run := &fakeRunner{
			Outputs: map[string]string{
				missingPVKey: "  0\n",
			},
			Errs: map[string]error{
				lvextendKey: fmt.Errorf("insufficient extents"),
			},
		}
		mgr := NewPoolManager(run, nil, DefaultThinPoolConfig())

		err := mgr.ExpandPool(context.Background(), "/dev/sda4")
		if err == nil {
			t.Fatal("expected error from lvextend failure")
		}
		if !strings.Contains(err.Error(), "lvextend") {
			t.Errorf("error should mention lvextend, got: %v", err)
		}
	})
}

// --- LVManager tests ---

func TestLVManager_CreateThinLV(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	if err := mgr.CreateThinLV(context.Background(), "vol-test", 10*1024*1024*1024); err != nil {
		t.Fatalf("CreateThinLV: %v", err)
	}
	calls := run.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(calls))
	}
	if !strings.Contains(calls[0], "lvcreate") {
		t.Errorf("expected lvcreate, got %q", calls[0])
	}
	if !strings.Contains(calls[0], "vol-test") {
		t.Errorf("expected vol-test in args, got %q", calls[0])
	}
}

func TestLVManager_CreateThinLV_aligns_to_sector(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	// 1319306057B is the size that triggered the original bug — not a multiple of 512.
	if err := mgr.CreateThinLV(context.Background(), "golden-test", 1319306057); err != nil {
		t.Fatalf("CreateThinLV: %v", err)
	}
	call := run.GetCalls()[0]
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
		{10 << 30, 10 << 30},     // already aligned (10 GiB)
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
	if calls := run.GetCalls(); !strings.Contains(calls[0], "lvremove -f") {
		t.Errorf("expected lvremove -f, got %q", calls[0])
	}
}

func TestLVManager_ActivateDeactivate(t *testing.T) {
	run := &fakeRunner{}
	mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)

	if err := mgr.ActivateLV(context.Background(), "vol-test"); err != nil {
		t.Fatalf("ActivateLV: %v", err)
	}
	calls := run.GetCalls()
	if !strings.Contains(calls[0], "lvchange -ay") {
		t.Errorf("expected lvchange -ay, got %q", calls[0])
	}
	if !strings.Contains(calls[1], "udevadm settle") {
		t.Errorf("expected udevadm settle, got %q", calls[1])
	}

	if err := mgr.DeactivateLV(context.Background(), "vol-test"); err != nil {
		t.Fatalf("DeactivateLV: %v", err)
	}
	calls = run.GetCalls()
	if !strings.Contains(calls[2], "lvchange -an") {
		t.Errorf("expected lvchange -an, got %q", calls[2])
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
			Errs: map[string]error{
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

func TestLVManager_LVExistsExact(t *testing.T) {
	const exactKey = "lvs --noheadings -o lv_name " + DefaultVGName

	t.Run("finds target without thin-pool filtering", func(t *testing.T) {
		run := &testutil.FakeRunner{
			Outputs: map[string]string{
				exactKey: " thin-pool\n golden-model\n foreign-lv\n",
			},
		}
		manager := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		exists, err := manager.LVExistsExact(context.Background(), "golden-model")
		if err != nil {
			t.Fatalf("LVExistsExact: %v", err)
		}
		if !exists {
			t.Fatal("LVExistsExact did not find exact LV")
		}
	})

	t.Run("successful empty inventory proves absence", func(t *testing.T) {
		run := &testutil.FakeRunner{}
		manager := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		exists, err := manager.LVExistsExact(context.Background(), "golden-model")
		if err != nil {
			t.Fatalf("LVExistsExact: %v", err)
		}
		if exists {
			t.Fatal("LVExistsExact reported absent LV present")
		}
	})

	t.Run("malformed inventory is ambiguous", func(t *testing.T) {
		run := &testutil.FakeRunner{
			Outputs: map[string]string{
				exactKey: "golden-model unexpected-column\n",
			},
		}
		manager := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if _, err := manager.LVExistsExact(context.Background(), "golden-model"); err == nil {
			t.Fatal("LVExistsExact accepted malformed inventory")
		}
	})

	t.Run("command failure is ambiguous", func(t *testing.T) {
		run := &testutil.FakeRunner{
			Errs: map[string]error{
				exactKey: errors.New("lvs failed"),
			},
		}
		manager := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if _, err := manager.LVExistsExact(context.Background(), "golden-model"); err == nil {
			t.Fatal("LVExistsExact accepted failed inventory")
		}
	})
}

func TestLVManager_ListLVs(t *testing.T) {
	t.Run("mixed_pools", func(t *testing.T) {
		run := &fakeRunner{
			Outputs: map[string]string{
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
			Outputs: map[string]string{
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
		calls := run.GetCalls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d: %v", len(calls), calls)
		}
		call := calls[0]
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
			Errs: map[string]error{
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
	if calls := run.GetCalls(); !strings.Contains(calls[0], "lvresize") {
		t.Errorf("expected lvresize, got %q", calls[0])
	}
}

func TestLVManager_RenameLV(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewLVManager(run, DefaultVGName, DefaultThinPoolName)
		if err := mgr.RenameLV(context.Background(), "vol-old", "vol-new"); err != nil {
			t.Fatalf("RenameLV: %v", err)
		}
		calls := run.GetCalls()
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d: %v", len(calls), calls)
		}
		call := calls[0]
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
			Errs: map[string]error{
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
