package autounlock

import (
	"context"
	"errors"
	"log"
	"time"

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
	cctx, cancel := context.WithTimeout(ctx, ceremonyTimeoutBudget)
	defer cancel()
	result, err := o.Prepare(cctx, PrepareTriggerGracefulShutdown, configuredWindowSeconds*time.Second)
	if err != nil {
		log.Printf("ERROR: autounlock: ceremony prepare failed: %v", err)
		return err
	}
	if result.Disposition == PrepareDispositionUnavailable {
		log.Printf("WARN: autounlock: ceremony skipped — recovery provider unavailable")
	}
	return nil
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
	if errors.Is(err, ErrRecoveryFactorInvalid) {
		return false
	}
	var apiErr *namekclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	// Non-APIError → transport-level failure (DNS, dial, TLS handshake, etc.).
	return true
}
