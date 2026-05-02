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

// Posture is the per-device opt-in setting that selects whether and how
// post-reboot auto-unlock is attempted.
type Posture string

const (
	// PostureOff is today's behavior: every reboot lands at the lock screen
	// and requires a password / recovery key. Default for new and existing
	// devices.
	PostureOff Posture = "off"

	// PostureTPMAuto uses the namek escrow with the TPM-sealed AK as the
	// authenticator. Eligible only on hardware_tpm devices. No theft
	// regression vs PostureOff for cold-theft.
	PostureTPMAuto Posture = "tpm_auto"

	// PostureSoftAuto uses the namek escrow with a software-sealed AK.
	// Eligible only on software_tpm devices. Modest cold-theft regression
	// vs PostureOff (window-W exposure if the device is stolen and the
	// owner doesn't notice).
	PostureSoftAuto Posture = "soft_auto"
)

// IdentityClass values mirror the strings namek-server returns in
// EnrollResult.IdentityClass — see namek-server/docs/piccolod-integration-spec.md.
const (
	IdentityClassHardwareTPM = "hardware_tpm"
	IdentityClassSoftwareTPM = "software_tpm"
)

// IsEligible reports whether the supplied posture is consistent with the
// device's identity_class. Off is always eligible. TPMAuto requires
// hardware_tpm; SoftAuto requires software_tpm. Any unknown identity_class
// is treated as ineligible for either auto posture — degrades to Off-only.
func IsEligible(posture Posture, identityClass string) bool {
	switch posture {
	case PostureOff:
		return true
	case PostureTPMAuto:
		return identityClass == IdentityClassHardwareTPM
	case PostureSoftAuto:
		return identityClass == IdentityClassSoftwareTPM
	default:
		return false
	}
}

// AllowedPostures returns the set of postures the UI should offer for a
// given identity_class. Off is always present.
func AllowedPostures(identityClass string) []Posture {
	out := []Posture{PostureOff}
	switch identityClass {
	case IdentityClassHardwareTPM:
		out = append(out, PostureTPMAuto)
	case IdentityClassSoftwareTPM:
		out = append(out, PostureSoftAuto)
	}
	return out
}

// AutoReboot is the in-window scheduler config nested inside State.
//
// `enabled=false` keeps auto-unlock active but disables the scheduler — the
// user reboots manually and the device returns unlocked. `enabled=true` lets
// the scheduler trigger reboots when a staged update exists during the window.
type AutoReboot struct {
	Enabled         bool       `json:"enabled"`
	WindowStartHour int        `json:"window_start_hour"`
	WindowEndHour   int        `json:"window_end_hour"`
	LastFiredAt     *time.Time `json:"last_fired_at,omitempty"`
	LastFailedAt    *time.Time `json:"last_failed_at,omitempty"`
}

// State is the on-disk shape of {coreRoot}/network-bootstrap/security/auto_unlock.json.
type State struct {
	Posture           Posture    `json:"posture"`
	AutoReboot        AutoReboot `json:"auto_reboot"`
	LastDepositAt     *time.Time `json:"last_deposit_at,omitempty"`
	LastPickupAt      *time.Time `json:"last_pickup_at,omitempty"`
	LastFailureAt     *time.Time `json:"last_failure_at,omitempty"`
	LastFailureReason string     `json:"last_failure_reason,omitempty"`
}

// DefaultState is the implicit state when the file is missing.
func DefaultState() State {
	return State{Posture: PostureOff}
}

// DefaultAutoReboot is initialised on the first A→B/C transition.
func DefaultAutoReboot() AutoReboot {
	return AutoReboot{
		Enabled:         false, // user opts in to overnight reboots explicitly
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
// posture-set / first ceremony don't fail with ENOENT.
func EnsureStateDir() error {
	return os.MkdirAll(stateDir(), 0o700)
}

// LoadState reads the state file. Returns DefaultState (no error) when the
// file is absent. Parse failures and invalid postures return DefaultState
// + ErrInvalidStateFile so the caller can log and continue.
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
	if !isValidPosture(s.Posture) {
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

func isValidPosture(p Posture) bool {
	switch p {
	case PostureOff, PostureTPMAuto, PostureSoftAuto:
		return true
	}
	return false
}
