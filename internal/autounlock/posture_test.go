package autounlock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"piccolod/internal/state/paths"
)

func TestIsEligible(t *testing.T) {
	cases := []struct {
		posture       Posture
		identityClass string
		want          bool
	}{
		{PostureOff, IdentityClassHardwareTPM, true},
		{PostureOff, IdentityClassSoftwareTPM, true},
		{PostureOff, "", true},
		{PostureTPMAuto, IdentityClassHardwareTPM, true},
		{PostureTPMAuto, IdentityClassSoftwareTPM, false},
		{PostureTPMAuto, "", false},
		{PostureSoftAuto, IdentityClassSoftwareTPM, true},
		{PostureSoftAuto, IdentityClassHardwareTPM, false},
		{PostureSoftAuto, "", false},
		{Posture("garbage"), IdentityClassHardwareTPM, false},
	}
	for _, tc := range cases {
		got := IsEligible(tc.posture, tc.identityClass)
		if got != tc.want {
			t.Errorf("IsEligible(%q, %q) = %v; want %v", tc.posture, tc.identityClass, got, tc.want)
		}
	}
}

func TestAllowedPostures(t *testing.T) {
	cases := []struct {
		identityClass string
		want          []Posture
	}{
		{IdentityClassHardwareTPM, []Posture{PostureOff, PostureTPMAuto}},
		{IdentityClassSoftwareTPM, []Posture{PostureOff, PostureSoftAuto}},
		{"unknown", []Posture{PostureOff}},
		{"", []Posture{PostureOff}},
	}
	for _, tc := range cases {
		got := AllowedPostures(tc.identityClass)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("AllowedPostures(%q) = %v; want %v", tc.identityClass, got, tc.want)
		}
	}
}

func TestLoadState_MissingFile(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	s, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState (missing): %v", err)
	}
	if s.Posture != PostureOff {
		t.Errorf("missing-file posture = %q; want off", s.Posture)
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
	if s.Posture != PostureOff {
		t.Errorf("fall-back posture = %q; want off", s.Posture)
	}
}

func TestLoadState_InvalidPosture(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{Posture: Posture("nope")}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	_, err := LoadState()
	if err != ErrInvalidStateFile {
		t.Fatalf("expected ErrInvalidStateFile; got %v", err)
	}
}

func TestSaveLoadState_RoundTrip(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	original := State{
		Posture: PostureTPMAuto,
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
	if got.Posture != original.Posture {
		t.Errorf("posture mismatch: got %q, want %q", got.Posture, original.Posture)
	}
	if !reflect.DeepEqual(got.AutoReboot, original.AutoReboot) {
		t.Errorf("auto_reboot mismatch:\ngot  %+v\nwant %+v", got.AutoReboot, original.AutoReboot)
	}
}

func TestSaveState_AtomicWrite(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{Posture: PostureSoftAuto}); err != nil {
		t.Fatalf("first SaveState: %v", err)
	}
	if err := SaveState(State{Posture: PostureOff}); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Posture != PostureOff {
		t.Errorf("expected last-writer posture off; got %q", got.Posture)
	}
}

func TestSaveState_FilePermissions(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{Posture: PostureOff}); err != nil {
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
	// Sanity check that the on-disk JSON matches the documented shape.
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := SaveState(State{
		Posture:    PostureTPMAuto,
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
	if raw["posture"] != "tpm_auto" {
		t.Errorf("posture key = %v; want tpm_auto", raw["posture"])
	}
	ar, ok := raw["auto_reboot"].(map[string]any)
	if !ok {
		t.Fatalf("auto_reboot not an object: %T", raw["auto_reboot"])
	}
	if ar["window_start_hour"] != float64(3) {
		t.Errorf("window_start_hour = %v; want 3", ar["window_start_hour"])
	}
}
