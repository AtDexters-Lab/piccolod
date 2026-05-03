package server

import (
	"context"
	"errors"
	"time"

	"piccolod/internal/persistence"
)

// computeRecoveryKeyPending decides whether the operator should be re-prompted
// to view/save the recovery key. Shared by /system/boot and /auth/login —
// login must re-evaluate post-unlock because a locked boot suppresses the
// gate (see below) and the unlock-then-login chain doesn't re-call /boot.
//
// The two-step decision:
//
//  1. File-presence: keyset.json absent → pending (no key to ack).
//  2. Ack-presence: file exists but RecoveryAckAt is zero → pending (key
//     written but operator may not have seen the words).
//
// Skip step (2) while crypto is locked: the staleness store lives behind the
// unlock barrier, so the read is *guaranteed* to fail every locked boot.
// Treating that as "pending" would force the auth controller to call
// /recovery-key/generate on unlock, which ROTATES the existing key and
// invalidates the operator's saved words on every routine reboot. Treat
// persistence.ErrLocked from the staleness read identically (TOCTOU vs a
// concurrent /crypto/lock between the IsLocked check and the Staleness call).
// Other staleness read errors fail-closed (re-prompt) — the rotation hazard
// does not apply because /generate is only auto-invoked behind unlock and a
// transient unlocked-state DB error self-heals.
func (s *GinServer) computeRecoveryKeyPending(ctx context.Context) bool {
	if s == nil || s.cryptoManager == nil || !s.cryptoManager.IsInitialized() {
		return false
	}
	if !s.cryptoManager.HasRecoveryKey() {
		return true
	}
	// Composite readiness via lifecycle, not the narrow SDEK check. While
	// the system isn't Ready, the staleness store may not be queryable —
	// emitting "pending" would force the auth controller to call
	// /recovery-key/generate on unlock, which rotates the key and
	// destroys the operator's saved words on every routine reboot.
	if s.lifecycle == nil || !s.lifecycle.IsReady() {
		return false
	}
	st, err := s.readAuthStaleness(ctx)
	switch {
	case errors.Is(err, persistence.ErrLocked):
		// Defensive: TOCTOU vs concurrent /crypto/lock between the lifecycle
		// check above and the Staleness call. Same treatment — don't claim
		// pending and risk rotation hazard.
		return false
	case err != nil, st.RecoveryAckAt.IsZero():
		return true
	}
	return false
}

func (s *GinServer) authStalenessRepo() persistence.AuthRepo {
	if s == nil {
		return nil
	}
	if s.authRepo != nil {
		return s.authRepo
	}
	if s.persistence == nil {
		return nil
	}
	ctrl := s.persistence.Control()
	if ctrl == nil {
		return nil
	}
	return ctrl.Auth()
}

func (s *GinServer) readAuthStaleness(ctx context.Context) (persistence.AuthStaleness, error) {
	repo := s.authStalenessRepo()
	if repo == nil {
		return persistence.AuthStaleness{}, errors.New("auth repo unavailable")
	}
	return repo.Staleness(ctx)
}

func (s *GinServer) applyStalenessUpdate(ctx context.Context, update persistence.AuthStalenessUpdate) error {
	repo := s.authStalenessRepo()
	if repo == nil {
		return errors.New("auth repo unavailable")
	}
	return repo.UpdateStaleness(ctx, update)
}

func boolPtr(v bool) *bool {
	return &v
}

func timePtr(t time.Time) *time.Time {
	return &t
}
