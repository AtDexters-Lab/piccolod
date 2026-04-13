// Package provisioning owns the single answer to "has this device finished
// first-run setup?". The question was previously fragmented across at least
// five ad-hoc signals — onboarding.json, control-plane.luks, keyset.json,
// the users table, and cfg.DeviceID — none of which alone covered the four
// state-tuple cases (fresh, mid-setup, locked-provisioned, unlocked-
// provisioned). Each ad-hoc check served its own narrow context and broke
// when the setup-heartbeat callback became the first caller that needed
// the answer pre-crypto-unlock, across upgrade, and across crash recovery.
//
// The canonical signal is onboarding.json with state == complete. It is:
//   - written at the end of /crypto/setup, after admin-user creation
//   - written atomically (rename(2))
//   - readable before crypto unlock (lives on the unencrypted core root)
//   - already present on every fleet device that finished setup before
//     this package existed — no migration needed
//
// The remaining failure mode is that handleCryptoSetup's call to
// onboardingMgr.Complete() is best-effort; if the write fails on a brand-
// new device, the marker is missing on the next boot and the heartbeat
// callback would misclassify the device as still in first-run setup.
// ReconcileFromPersistence closes that hole by backfilling the marker
// from the unlock path once persistence is available and reports
// users > 0.
package provisioning

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"piccolod/internal/onboarding"
	"piccolod/internal/state/paths"
)

// markerFileName is the dedicated sticky bit that records "first-run setup
// has finished at least once on this device". Lives on the unencrypted core
// root so the heartbeat callback can read it before crypto is unlocked.
// Once written it is never deleted: subsequent state-machine transitions
// (e.g. onboarding state moving from complete back to install_disk via the
// Settings re-install path) do not affect it.
const markerFileName = "first_run_complete"

func markerPath() string {
	return filepath.Join(paths.CoreJoin("network-bootstrap"), markerFileName)
}

// State answers "has this device finished first-run setup?" with a dedicated
// sticky bit, deliberately independent from any other state machine in the
// codebase.
//
// Background: every other "device looks set up" signal here means something
// subtly different and breaks for at least one case the heartbeat needs:
//
//   - onboarding.json state == complete: not sticky — Settings re-install
//     transitions complete → install_disk, dropping the bit
//   - LVM VG exists: false positive — created mid-setup before admin user
//   - keyset.json / control-plane.luks: false positive — same
//   - users table count > 0: requires crypto unlock
//   - cfg.DeviceID != "": namek enrollment, independent of crypto setup
//
// So provisioning owns its own bit. It is written exactly when first-run
// setup finishes (MarkProvisioned), reconciled after unlock if a previous
// best-effort write was lost (ReconcileFromPersistence), and read by the
// setup-heartbeat callback (IsProvisioned). A one-time read-time backfill
// from onboarding.json == complete handles upgraded fleet devices that
// finished setup before this package existed.
type State struct {
	onboarding   *onboarding.Manager
	backfillOnce sync.Once
}

// New constructs a State backed by the given onboarding manager. The manager
// is consulted only for the one-time read-time backfill of upgraded devices.
func New(o *onboarding.Manager) *State {
	return &State{onboarding: o}
}

// IsProvisioned reports whether first-run setup has finished at any point in
// this device's history. Safe to call before crypto is unlocked. Returns
// false on a nil receiver.
func (s *State) IsProvisioned() bool {
	if s == nil {
		return false
	}
	if markerExists() {
		return true
	}
	// One-time read-time backfill for upgraded fleet: the marker file
	// didn't exist in older builds, but onboarding.json may already be
	// in the complete state. Migrate the existing signal to the new one.
	s.backfillOnce.Do(s.backfillFromOnboarding)
	return markerExists()
}

func (s *State) backfillFromOnboarding() {
	if s.onboarding == nil {
		return
	}
	if s.onboarding.State() != onboarding.StateComplete {
		return
	}
	if err := writeMarker(); err != nil {
		// Best-effort migration; will be retried on next boot.
		_ = err
	}
}

// MarkProvisioned writes the durable provisioning marker. Called from the
// crypto-setup handler immediately after admin-user creation. Best-effort:
// callers log failures but don't fail the setup, because a failure here is
// recoverable on the next /crypto/unlock via ReconcileFromPersistence — and
// failing the entire setup at this point would leave the user with a
// half-provisioned device and no in-flow retry path (the admin user, portal
// session, and setup nonce are all already committed).
func (s *State) MarkProvisioned() error {
	if s == nil {
		return nil
	}
	return writeMarker()
}

// ReconcileFromPersistence is called from the unlock handler with the
// authoritative isSetupComplete result post-unlock. If persistence reports
// the device is fully provisioned but the durable marker is missing (because
// MarkProvisioned previously failed best-effort), write it. Idempotent.
func (s *State) ReconcileFromPersistence(complete bool) error {
	if s == nil || !complete || markerExists() {
		return nil
	}
	return writeMarker()
}

func markerExists() bool {
	_, err := os.Stat(markerPath())
	return err == nil
}

func writeMarker() error {
	path := markerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
