package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage"
)

func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	paths.SetCoreRootForTest(t, dir)
	// Create the network-bootstrap subdirectory.
	if err := os.MkdirAll(filepath.Join(dir, "network-bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNewManager_DefaultPending(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	if m.State() != StatePending {
		t.Errorf("expected pending, got %s", m.State())
	}
	if m.BootMode() != storage.BootModeUSB {
		t.Errorf("expected usb, got %s", m.BootMode())
	}
}

func TestNewManager_LoadExistingState(t *testing.T) {
	dir := setupTestDir(t)
	cfg := OnboardingConfig{State: StateTryPiccolo, BootMode: "usb"}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "network-bootstrap", "onboarding.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(storage.BootModeUSB)
	if m.State() != StateTryPiccolo {
		t.Errorf("expected try_piccolo, got %s", m.State())
	}
}

func TestNewManager_BootRecovery(t *testing.T) {
	dir := setupTestDir(t)
	// install_disk with InstallDone=false should reset to pending.
	cfg := OnboardingConfig{State: StateInstallDisk, InstallDone: false}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "network-bootstrap", "onboarding.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(storage.BootModeUSB)
	if m.State() != StatePending {
		t.Errorf("expected pending after boot recovery, got %s", m.State())
	}
}

func TestNewManager_InstallDonePreserved(t *testing.T) {
	dir := setupTestDir(t)
	// install_disk with InstallDone=true should NOT reset.
	cfg := OnboardingConfig{State: StateInstallDisk, InstallDone: true}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "network-bootstrap", "onboarding.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(storage.BootModeUSB)
	if m.State() != StateInstallDisk {
		t.Errorf("expected install_disk, got %s", m.State())
	}
	if !m.InstallDone() {
		t.Error("expected InstallDone to be true")
	}
}

func TestIsRequired(t *testing.T) {
	tests := []struct {
		name     string
		bootMode storage.BootMode
		state    OnboardingState
		want     bool
	}{
		{"usb_pending", storage.BootModeUSB, StatePending, true},
		{"usb_try_piccolo", storage.BootModeUSB, StateTryPiccolo, false},
		{"usb_complete", storage.BootModeUSB, StateComplete, false},
		{"internal_pending", storage.BootModeInternal, StatePending, false},
		{"unknown_pending", storage.BootModeUnknown, StatePending, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDir(t)
			m := NewManager(tt.bootMode)
			if tt.state != StatePending {
				// Transition to the desired state.
				if tt.state == StateTryPiccolo {
					m.Choose(StateTryPiccolo)
				} else if tt.state == StateComplete {
					m.Choose(StateTryPiccolo)
					m.Complete()
				}
			}
			if got := m.IsRequired(); got != tt.want {
				t.Errorf("IsRequired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsUSBBoot(t *testing.T) {
	tests := []struct {
		mode storage.BootMode
		want bool
	}{
		{storage.BootModeUSB, true},
		{storage.BootModeUnknown, true},
		{storage.BootModeInternal, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			setupTestDir(t)
			m := NewManager(tt.mode)
			if got := m.IsUSBBoot(); got != tt.want {
				t.Errorf("IsUSBBoot() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChoose_ValidTransitions(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*Manager)
		choice OnboardingState
	}{
		{"pending_to_try_piccolo", func(m *Manager) {}, StateTryPiccolo},
		{"pending_to_install_disk", func(m *Manager) {}, StateInstallDisk},
		{"try_piccolo_to_install_disk", func(m *Manager) { m.Choose(StateTryPiccolo) }, StateInstallDisk},
		{"complete_to_install_disk", func(m *Manager) { m.Choose(StateTryPiccolo); m.Complete() }, StateInstallDisk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDir(t)
			m := NewManager(storage.BootModeUSB)
			tt.setup(m)
			if err := m.Choose(tt.choice); err != nil {
				t.Fatalf("Choose(%s) unexpected error: %v", tt.choice, err)
			}
			if m.State() != tt.choice {
				t.Errorf("state = %s, want %s", m.State(), tt.choice)
			}
		})
	}
}

func TestChoose_InvalidTransitions(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*Manager)
		choice OnboardingState
	}{
		{"pending_to_complete", func(m *Manager) {}, StateComplete},
		{"complete_to_pending", func(m *Manager) { m.Choose(StateTryPiccolo); m.Complete() }, StatePending},
		{"install_disk_to_try_piccolo", func(m *Manager) { m.Choose(StateInstallDisk) }, StateTryPiccolo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDir(t)
			m := NewManager(storage.BootModeUSB)
			tt.setup(m)
			if err := m.Choose(tt.choice); err == nil {
				t.Errorf("Choose(%s) expected error, got nil", tt.choice)
			}
		})
	}
}

func TestComplete(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	if err := m.Choose(StateTryPiccolo); err != nil {
		t.Fatal(err)
	}
	if err := m.Complete(); err != nil {
		t.Fatalf("Complete() unexpected error: %v", err)
	}
	if m.State() != StateComplete {
		t.Errorf("state = %s, want complete", m.State())
	}
}

func TestComplete_WrongState(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	if err := m.Complete(); err == nil {
		t.Error("Complete() from pending expected error, got nil")
	}
}

func TestMarkInstallDone(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	if err := m.Choose(StateInstallDisk); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkInstallDone(); err != nil {
		t.Fatalf("MarkInstallDone() unexpected error: %v", err)
	}
	if !m.InstallDone() {
		t.Error("expected InstallDone to be true")
	}
}

func TestMarkInstallDone_WrongState(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	if err := m.MarkInstallDone(); err == nil {
		t.Error("MarkInstallDone() from pending expected error, got nil")
	}
}

func TestPersistence_RoundTrip(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	if err := m.Choose(StateTryPiccolo); err != nil {
		t.Fatal(err)
	}

	// Load again from disk.
	m2 := NewManager(storage.BootModeUSB)
	if m2.State() != StateTryPiccolo {
		t.Errorf("reloaded state = %s, want try_piccolo", m2.State())
	}
}

func TestStatusResponse(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeUSB)
	resp := m.StatusResponse()

	if resp["state"] != "pending" {
		t.Errorf("state = %v, want pending", resp["state"])
	}
	if resp["boot_mode"] != "usb" {
		t.Errorf("boot_mode = %v, want usb", resp["boot_mode"])
	}
	if resp["required"] != true {
		t.Errorf("required = %v, want true", resp["required"])
	}
}

func TestStatusResponse_InternalBoot(t *testing.T) {
	setupTestDir(t)
	m := NewManager(storage.BootModeInternal)
	resp := m.StatusResponse()
	if resp["required"] != false {
		t.Errorf("required = %v, want false for internal boot", resp["required"])
	}
}
