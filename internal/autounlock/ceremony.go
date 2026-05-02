package autounlock

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"time"

	"piccolod/internal/cryptoutil"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// ceremonyTimeoutBudget bounds the total wall-clock time the ceremony can
// consume during shutdown. Well within systemd TimeoutStopSec=120s, leaving
// headroom for the rest of the Stop() phases.
const ceremonyTimeoutBudget = 25 * time.Second

// configuredWindowSeconds is the requested window passed to namek. Server
// clamps to its own ceiling (default 600s, matches namek-server's
// autoUnlock.maxWindowSeconds).
const configuredWindowSeconds = 600

// fSize is the AES-256-GCM key size used as the per-cycle escrow secret.
const fSize = 32

// RunCeremony is invoked from gin_server.Stop()'s Phase 0. Generates a fresh
// per-cycle F, wraps the in-memory SDEK into an on-disk blob, and deposits F
// at namek. Bails (no-op) when auto-unlock is disabled or identity is not in a state
// to deposit. Failure does NOT block shutdown — caller logs and proceeds.
func (o *Orchestrator) RunCeremony(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	state, err := LoadState()
	if err != nil && !errors.Is(err, ErrInvalidStateFile) {
		log.Printf("WARN: autounlock: ceremony load state: %v", err)
		return err
	}
	if !state.Enabled {
		return nil
	}
	if !o.deps.IsIdentityReady() {
		log.Printf("WARN: autounlock: ceremony skipped — identity not ready")
		return nil
	}
	if o.deps.Manager.IsLocked() {
		// Defense-in-depth: shutdown without an unlocked manager has no SDEK
		// to wrap. Should not happen in normal operation.
		log.Printf("WARN: autounlock: ceremony skipped — manager locked")
		return nil
	}
	client := o.deps.NamekClient()
	if client == nil {
		log.Printf("WARN: autounlock: ceremony skipped — namek client unavailable")
		return nil
	}

	cctx, cancel := context.WithTimeout(ctx, ceremonyTimeoutBudget)
	defer cancel()

	F := make([]byte, fSize)
	defer cryptoutil.SecureZero(F)
	if _, err := io.ReadFull(rand.Reader, F); err != nil {
		log.Printf("ERROR: autounlock: rand F: %v", err)
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return err
	}

	aad, err := o.aad()
	if err != nil {
		log.Printf("ERROR: autounlock: aad: %v", err)
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return err
	}
	blob, err := o.deps.Manager.WrapSDEKForEscrow(F, aad)
	if err != nil {
		log.Printf("ERROR: autounlock: wrap SDEK: %v", err)
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return err
	}
	if err := WriteBlob(blob); err != nil {
		log.Printf("ERROR: autounlock: write blob: %v", err)
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonBlobWriteFailed)
		return err
	}

	resp, err := o.deposit(cctx, client, F)
	if err != nil {
		// Clean up: blob wrapped to F that's not at namek is unrecoverable.
		// Deleting prevents a stale-blob pickup on next boot.
		_ = DeleteBlob()
		log.Printf("ERROR: autounlock: deposit failed: %v", err)
		// All deposit-side failures (including the 25s ceremony-budget
		// timeout) collapse to deposit_failed. The pickup goroutine's
		// service_unreachable is for the *next boot's* read — they're not
		// the same failure class.
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return err
	}

	now := o.deps.Now()
	state.LastDepositAt = &now
	if err := SaveState(state); err != nil {
		log.Printf("WARN: autounlock: persist post-deposit state: %v", err)
	}
	o.emitAudit(AuditCycleDeposited, map[string]any{
		"effective_window":  resp.EffectiveWindowSeconds,
		"requested_clamped": resp.RequestedClamped,
		"expires_at":        resp.ExpiresAt,
	})
	return nil
}

// deposit attempts the deposit RPC with a single retry on transient errors.
// F is captured once at ceremony start — the retry reuses the same bytes,
// never regenerates. Regenerating mid-retry would race the wrap/blob already
// committed to the same F, leaving an unrecoverable blob on disk.
func (o *Orchestrator) deposit(ctx context.Context, client NamekEscrowClient, F []byte) (*namekclient.DepositUnlockEscrowResponse, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := client.DepositUnlockEscrow(ctx, F, configuredWindowSeconds)
		if err == nil {
			return resp, nil
		}
		if attempt == 0 && isTransientNamekErr(err) {
			log.Printf("WARN: autounlock: deposit transient error, retrying: %v", err)
			continue
		}
		return nil, err
	}
	return nil, errors.New("deposit retries exhausted")
}

// recordFailure persists last_failure_* and emits the supplied audit kind.
// Ceremony-side failures pass AuditCycleFailedDeposit; pickup-side failures
// pass AuditCycleFailedPickup. Distinguishing the two lets the Activity
// sub-page tell the operator whether the previous cycle failed at deposit
// (last shutdown couldn't reach namek) or at pickup (this boot couldn't
// retrieve from namek) — different remediation flows.
func (o *Orchestrator) recordFailure(state *State, auditKind, reason string) {
	now := o.deps.Now()
	state.LastFailureAt = &now
	state.LastFailureReason = reason
	if err := SaveState(*state); err != nil {
		log.Printf("WARN: autounlock: persist failure state: %v", err)
	}
	o.emitAudit(auditKind, map[string]any{"reason": reason})
}

// isTransientNamekErr classifies an error as worth retrying. Transport-level
// errors (network, timeout) and 5xx responses are transient; 4xx (auth, bad
// request) are not. Sentinel errors that indicate permanent server-side
// state (ErrEscrowNotFound, ErrEnrollmentRequired) are also non-transient.
func isTransientNamekErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// Permanent sentinels — retrying just burns cycles.
	if errors.Is(err, namekclient.ErrEscrowNotFound) {
		return false
	}
	var apiErr *namekclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	// Non-APIError → transport-level failure (DNS, dial, TLS handshake, etc.).
	return true
}
