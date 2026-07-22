package autounlock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

// AutoReboot is the in-window scheduler config nested inside State.
//
// Enabled=false keeps auto-unlock active but disables the scheduler — the user
// reboots manually and the device returns unlocked. Enabled=true lets the
// scheduler trigger reboots when a staged update exists during the window.
type AutoReboot struct {
	Enabled         bool       `json:"enabled"`
	WindowStartHour int        `json:"window_start_hour"`
	WindowEndHour   int        `json:"window_end_hour"`
	LastFiredAt     *time.Time `json:"last_fired_at,omitempty"`
	LastFailedAt    *time.Time `json:"last_failed_at,omitempty"`
}

// State is the on-disk shape of {coreRoot}/network-bootstrap/security/auto_unlock.json.
//
// Single boolean controls whether ceremony fires on shutdown and pickup runs
// on boot. The protocol is uniform across all devices — swtpm transparently
// abstracts whether a hardware TPM is present, so autounlock has no use for
// identity_class or any TPM-attestation tier.
type State struct {
	Enabled    bool       `json:"enabled"`
	AutoReboot AutoReboot `json:"auto_reboot"`
	// Handoff is optional, non-secret dispatch metadata for the raw
	// auto_unlock_blob. Keep it as RawMessage so a future or malformed nested
	// schema cannot make the rest of auto_unlock.json unreadable. The
	// continuity reconciler validates this field only after comparing its
	// recorded blob digest.
	Handoff           json.RawMessage `json:"handoff,omitempty"`
	LastDepositAt     *time.Time      `json:"last_deposit_at,omitempty"`
	LastPickupAt      *time.Time      `json:"last_pickup_at,omitempty"`
	LastFailureAt     *time.Time      `json:"last_failure_at,omitempty"`
	LastFailureReason string          `json:"last_failure_reason,omitempty"`
}

// DefaultState is the implicit state when the file is missing — i.e. on a
// fresh device or before the operator has interacted with Settings →
// Security. Defaults are ON: the appliance value-prop is "it just works"
// post-reboot. Operators with a physical-security threat model that
// rules out auto-unlock can flip it off via Settings; the trust
// disclosure surfaces the trade-off before they leave the default.
//
// Enrolment-gated in practice: ceremony's IsIdentityReady() check
// excludes devices that aren't enrolled to namek, so default-on doesn't
// activate the scheduler / ceremony / pickup on unsupported devices.
func DefaultState() State {
	return State{
		Enabled:    true,
		AutoReboot: DefaultAutoReboot(),
	}
}

// DefaultAutoReboot is the auto-reboot block carried in DefaultState and
// re-seeded when an operator re-enables after a prior disable.
func DefaultAutoReboot() AutoReboot {
	return AutoReboot{
		Enabled:         true,
		WindowStartHour: 3,
		WindowEndHour:   5,
	}
}

// stateDir is {coreRoot}/network-bootstrap/security. Lives outside the
// encrypted control volume so the post-boot pickup goroutine can read state
// pre-unlock.
func stateDir() string {
	return paths.CoreJoin("network-bootstrap", "security")
}

func statePath() string {
	return filepath.Join(stateDir(), "auto_unlock.json")
}

// EnsureStateDir creates the security/ subdir with mode 0o700 if missing.
// Called once on package init (via the orchestrator constructor) so first
// state save / first ceremony don't fail with ENOENT.
func EnsureStateDir() error {
	return os.MkdirAll(stateDir(), 0o700)
}

// LoadState reads the state file. Returns DefaultState (no error) when the
// file is absent. Parse failures return DefaultState + ErrInvalidStateFile
// so the caller can log and continue.
func LoadState() (State, error) {
	b, err := os.ReadFile(statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultState(), nil
		}
		return DefaultState(), err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return DefaultState(), ErrInvalidStateFile
	}
	return s, nil
}

// SaveState writes the state file atomically (write-temp-fsync-rename via
// fsutil.AtomicWriteFile). Mode 0o600.
func SaveState(s State) error {
	if err := EnsureStateDir(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(statePath(), b, 0o600)
}
