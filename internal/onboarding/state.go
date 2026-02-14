package onboarding

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage"
)

// OnboardingState represents the current state of USB onboarding.
type OnboardingState string

const (
	StatePending     OnboardingState = "pending"
	StateTryPiccolo  OnboardingState = "try_piccolo"
	StateInstallDisk OnboardingState = "install_disk"
	StateComplete    OnboardingState = "complete"
)

// OnboardingConfig is the persisted onboarding state.
type OnboardingConfig struct {
	State                OnboardingState `json:"state"`
	BootMode             string          `json:"boot_mode,omitempty"`
	InstallDone          bool            `json:"install_done,omitempty"`
	BootOrderConfigured  bool            `json:"boot_order_configured,omitempty"`
	UpdatedAt            string          `json:"updated_at,omitempty"`
}

// Manager manages the onboarding state machine.
type Manager struct {
	mu       sync.RWMutex
	config   OnboardingConfig
	bootMode storage.BootMode
	filePath string
}

// NewManager creates a new onboarding manager. It loads existing state from
// disk or initialises to pending. Boot recovery: if state is install_disk and
// InstallDone is false, reset to pending (install was interrupted).
func NewManager(bootMode storage.BootMode) *Manager {
	fp := paths.CoreJoin("network-bootstrap", "onboarding.json")
	m := &Manager{
		bootMode: bootMode,
		filePath: fp,
	}

	data, err := os.ReadFile(fp)
	if err != nil {
		m.config = OnboardingConfig{
			State:    StatePending,
			BootMode: string(bootMode),
		}
		return m
	}

	var cfg OnboardingConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("WARN: failed to parse onboarding.json, resetting to pending: %v", err)
		m.config = OnboardingConfig{
			State:    StatePending,
			BootMode: string(bootMode),
		}
		return m
	}

	// Boot recovery: install_disk with InstallDone false means the install
	// was interrupted. Reset to pending so the user can retry.
	if cfg.State == StateInstallDisk && !cfg.InstallDone {
		log.Printf("INFO: onboarding state is install_disk with InstallDone=false; resetting to pending (boot recovery)")
		cfg.State = StatePending
		cfg.InstallDone = false
		cfg.BootOrderConfigured = false
	}

	cfg.BootMode = string(bootMode)
	m.config = cfg
	return m
}

// State returns the current onboarding state.
func (m *Manager) State() OnboardingState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.State
}

// BootMode returns the detected boot mode.
func (m *Manager) BootMode() storage.BootMode {
	return m.bootMode
}

// IsRequired returns true when USB/Unknown boot and state is pending.
func (m *Manager) IsRequired() bool {
	if m.bootMode == storage.BootModeInternal {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.State == StatePending
}

// IsUSBBoot returns true when running from USB (for Settings visibility).
func (m *Manager) IsUSBBoot() bool {
	return m.bootMode == storage.BootModeUSB || m.bootMode == storage.BootModeUnknown
}

// InstallDone returns true when the install has completed successfully.
func (m *Manager) InstallDone() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.InstallDone
}

// BootOrderConfigured returns true when efibootmgr successfully set the boot order.
func (m *Manager) BootOrderConfigured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.BootOrderConfigured
}

// Choose validates a state transition and persists it.
func (m *Manager) Choose(choice OnboardingState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.validateTransition(m.config.State, choice); err != nil {
		return err
	}

	m.config.State = choice
	// Reset install-specific flags when starting a new install so stale
	// values from a previous attempt don't affect the new one.
	if choice == StateInstallDisk {
		m.config.InstallDone = false
		m.config.BootOrderConfigured = false
	}
	m.config.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return m.persistLocked()
}

// ResetInstallFlags clears install-specific flags and persists. Called when
// retrying an install that is already in install_disk state (where Choose()
// would be a no-op since the state transition is a self-loop).
func (m *Manager) ResetInstallFlags() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.State != StateInstallDisk {
		return fmt.Errorf("cannot reset install flags: state is %q, expected %q", m.config.State, StateInstallDisk)
	}

	m.config.InstallDone = false
	m.config.BootOrderConfigured = false
	m.config.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return m.persistLocked()
}

// MarkInstallDone sets InstallDone to true and persists. This gates the reboot endpoint.
func (m *Manager) MarkInstallDone() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.State != StateInstallDisk {
		return fmt.Errorf("cannot mark install done: state is %q, expected %q", m.config.State, StateInstallDisk)
	}

	m.config.InstallDone = true
	m.config.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return m.persistLocked()
}

// MarkBootOrderConfigured records that efibootmgr successfully set the
// internal disk as the first boot entry. Persisted so the UI can show
// the appropriate reboot instructions even after a page reload.
func (m *Manager) MarkBootOrderConfigured() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config.BootOrderConfigured = true
	m.config.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := m.persistLocked(); err != nil {
		log.Printf("WARN: failed to persist boot_order_configured: %v", err)
	}
}

// Complete transitions try_piccolo → complete.
func (m *Manager) Complete() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.config.State != StateTryPiccolo {
		return fmt.Errorf("cannot complete: state is %q, expected %q", m.config.State, StateTryPiccolo)
	}

	m.config.State = StateComplete
	m.config.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return m.persistLocked()
}

// StatusResponse returns the data for the GET /api/v1/system/onboarding endpoint.
func (m *Manager) StatusResponse() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]any{
		"state":                  string(m.config.State),
		"boot_mode":              m.config.BootMode,
		"required":               m.isRequiredLocked(),
		"install_done":           m.config.InstallDone,
		"boot_order_configured":  m.config.BootOrderConfigured,
	}
}

// isRequiredLocked is the internal version that assumes mu is held.
func (m *Manager) isRequiredLocked() bool {
	if m.bootMode == storage.BootModeInternal {
		return false
	}
	return m.config.State == StatePending
}

// validateTransition checks if a state transition is valid.
func (m *Manager) validateTransition(from, to OnboardingState) error {
	switch {
	case from == StatePending && to == StateTryPiccolo:
		return nil
	case from == StatePending && to == StateInstallDisk:
		return nil
	case from == StateTryPiccolo && to == StateComplete:
		return nil
	case from == StateTryPiccolo && to == StateInstallDisk:
		return nil // Settings path: user chose Try Piccolo, now wants to install
	case from == StateComplete && to == StateInstallDisk:
		return nil // Settings path: fully set up, now wants to install
	default:
		return fmt.Errorf("invalid onboarding transition: %s → %s", from, to)
	}
}

// persistLocked writes the config to disk atomically. Caller must hold mu.
func (m *Manager) persistLocked() error {
	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal onboarding config: %w", err)
	}

	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create onboarding dir: %w", err)
	}

	// Atomic write: write to temp file, then rename.
	tmp := m.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write onboarding temp: %w", err)
	}
	if err := os.Rename(tmp, m.filePath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename onboarding config: %w", err)
	}
	return nil
}
