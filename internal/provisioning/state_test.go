package provisioning

import (
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/onboarding"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage"
)

// withTempCoreRoot points the global paths.CoreRoot at a fresh temp dir for
// the test's lifetime and creates the network-bootstrap subdir that
// onboarding.NewManager writes into.
func withTempCoreRoot(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	paths.SetCoreRootForTest(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "network-bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestState_NilSafe(t *testing.T) {
	var s *State
	if s.IsProvisioned() {
		t.Error("nil State should report not provisioned")
	}
	if err := s.MarkProvisioned(); err != nil {
		t.Errorf("nil State MarkProvisioned should be no-op, got %v", err)
	}
	if err := s.ReconcileFromPersistence(true); err != nil {
		t.Errorf("nil State ReconcileFromPersistence should be no-op, got %v", err)
	}
}

func TestState_FreshDeviceNotProvisioned(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeUSB)
	s := New(mgr)
	if s.IsProvisioned() {
		t.Error("fresh device should not be provisioned")
	}
}

func TestState_MarkProvisionedFromPending(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal) // pending → complete via Complete()
	s := New(mgr)

	if err := s.MarkProvisioned(); err != nil {
		t.Fatalf("MarkProvisioned: %v", err)
	}
	if !s.IsProvisioned() {
		t.Error("expected provisioned after MarkProvisioned")
	}

	// Survives a fresh manager load (durable).
	mgr2 := onboarding.NewManager(storage.BootModeInternal)
	s2 := New(mgr2)
	if !s2.IsProvisioned() {
		t.Error("provisioned state did not survive manager reload")
	}
}

func TestState_ReconcileBackfillsAfterFailedMark(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal)
	s := New(mgr)

	// Simulate the buggy state: handleCryptoSetup's MarkProvisioned write
	// failed best-effort, so the device finished setup but the durable
	// signal is still pending.
	if s.IsProvisioned() {
		t.Fatal("precondition: expected not provisioned")
	}

	// Unlock handler reports authoritative complete → reconcile backfills.
	if err := s.ReconcileFromPersistence(true); err != nil {
		t.Fatalf("ReconcileFromPersistence: %v", err)
	}
	if !s.IsProvisioned() {
		t.Error("reconcile did not backfill provisioning state")
	}
}

func TestState_ReconcileNoOpWhenIncomplete(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal)
	s := New(mgr)

	if err := s.ReconcileFromPersistence(false); err != nil {
		t.Fatalf("ReconcileFromPersistence(false): %v", err)
	}
	if s.IsProvisioned() {
		t.Error("reconcile(false) should not mark provisioned")
	}
}

func TestState_ReconcileIdempotentWhenAlreadyComplete(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal)
	s := New(mgr)

	if err := s.MarkProvisioned(); err != nil {
		t.Fatalf("MarkProvisioned: %v", err)
	}
	if err := s.ReconcileFromPersistence(true); err != nil {
		t.Errorf("idempotent reconcile errored: %v", err)
	}
	if !s.IsProvisioned() {
		t.Error("idempotent reconcile dropped provisioned state")
	}
}

// Codex iter #4 case: existing fleet device that finished setup before the
// marker file existed has onboarding.json in state=complete. The first call
// to IsProvisioned should backfill the marker file from that state.
func TestState_BackfillFromOnboardingComplete(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal)
	if err := mgr.Complete(); err != nil {
		t.Fatal(err)
	}
	// Confirm the marker file does NOT yet exist.
	if _, err := os.Stat(markerPath()); err == nil {
		t.Fatal("precondition: marker should not exist yet")
	}

	s := New(mgr)
	if !s.IsProvisioned() {
		t.Error("backfill did not promote onboarding=complete to marker")
	}
	if _, err := os.Stat(markerPath()); err != nil {
		t.Errorf("marker file was not written by backfill: %v", err)
	}
}

// Codex iter #9 case: a fully-provisioned device that re-enters install_disk
// via the Settings re-install path has onboarding state != complete, but the
// marker file (written during the original setup) makes IsProvisioned return
// true regardless. The state-machine transition does not affect the bit.
func TestState_StickyAcrossInstallDiskTransition(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal)
	s := New(mgr)

	// Original first-run setup completes.
	if err := s.MarkProvisioned(); err != nil {
		t.Fatal(err)
	}
	if !s.IsProvisioned() {
		t.Fatal("precondition: should be provisioned after MarkProvisioned")
	}

	// Settings re-install path: onboarding transitions complete → install_disk.
	if err := mgr.Choose(onboarding.StateInstallDisk); err != nil {
		t.Fatal(err)
	}
	if mgr.State() == onboarding.StateComplete {
		t.Fatal("precondition: state should have moved off complete")
	}

	// Marker file is sticky — IsProvisioned still true.
	if !s.IsProvisioned() {
		t.Error("marker should survive complete → install_disk transition")
	}
}

// Negative case for the backfill: a fresh device with onboarding state =
// pending should NOT have the marker backfilled.
func TestState_BackfillIgnoresPending(t *testing.T) {
	withTempCoreRoot(t)
	mgr := onboarding.NewManager(storage.BootModeInternal)
	s := New(mgr)

	if s.IsProvisioned() {
		t.Error("pending state should not backfill")
	}
	if _, err := os.Stat(markerPath()); err == nil {
		t.Error("marker file unexpectedly created on pending state")
	}
}
