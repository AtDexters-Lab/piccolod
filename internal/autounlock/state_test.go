package autounlock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"piccolod/internal/state/paths"
)

func TestLoadState_MissingFile(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	s, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState (missing): %v", err)
	}
	// Default-on: the appliance value-prop is "it just works" post-reboot.
	if !s.Enabled {
		t.Errorf("missing-file enabled = false; want true (default-on)")
	}
	if !s.AutoReboot.Enabled {
		t.Errorf("missing-file auto_reboot.enabled = false; want true (default-on)")
	}
	if s.AutoReboot.WindowStartHour != 3 || s.AutoReboot.WindowEndHour != 5 {
		t.Errorf("missing-file window = %d-%d; want 3-5",
			s.AutoReboot.WindowStartHour, s.AutoReboot.WindowEndHour)
	}
}

func TestLoadState_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	paths.SetCoreRootForTest(t, dir)
	if err := EnsureStateDir(); err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network-bootstrap", "security", "auto_unlock.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := LoadState()
	if err != ErrInvalidStateFile {
		t.Fatalf("expected ErrInvalidStateFile; got %v", err)
	}
	// Fall-back to DefaultState (default-on) — corrupt file does not down-
	// grade to disabled, since the implicit semantic is "this device's
	// auto-unlock state is unknowable, fall back to the appliance default."
	if !s.Enabled {
		t.Errorf("fall-back enabled = false; want true (DefaultState)")
	}
}

func TestSaveLoadState_RoundTrip(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	original := State{
		Enabled: true,
		AutoReboot: AutoReboot{
			Enabled:         true,
			WindowStartHour: 3,
			WindowEndHour:   5,
		},
	}
	if err := SaveState(original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Enabled != original.Enabled {
		t.Errorf("enabled mismatch: got %v, want %v", got.Enabled, original.Enabled)
	}
	if !reflect.DeepEqual(got.AutoReboot, original.AutoReboot) {
		t.Errorf("auto_reboot mismatch:\ngot  %+v\nwant %+v", got.AutoReboot, original.AutoReboot)
	}
}

func TestSaveState_AtomicWrite(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{Enabled: true}); err != nil {
		t.Fatalf("first SaveState: %v", err)
	}
	if err := SaveState(State{Enabled: false}); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Enabled {
		t.Errorf("expected last-writer enabled=false; got true")
	}
}

func TestSaveState_FilePermissions(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{Enabled: false}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	info, err := os.Stat(filepath.Join(stateDir(), "auto_unlock.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o; want 0600", mode)
	}
}

func TestState_JSONShape(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{
		Enabled:    true,
		AutoReboot: DefaultAutoReboot(),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(stateDir(), "auto_unlock.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if raw["enabled"] != true {
		t.Errorf("enabled key = %v; want true", raw["enabled"])
	}
	ar, ok := raw["auto_reboot"].(map[string]any)
	if !ok {
		t.Fatalf("auto_reboot not an object: %T", raw["auto_reboot"])
	}
	if ar["window_start_hour"] != float64(3) {
		t.Errorf("window_start_hour = %v; want 3", ar["window_start_hour"])
	}
}
