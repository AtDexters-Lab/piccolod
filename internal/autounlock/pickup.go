package autounlock

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"piccolod/internal/crypt"
	"piccolod/internal/cryptoutil"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// pickupIdentityWaitTimeout bounds how long pickup will wait for the identity
// service (TPM enrollment + namek client init) to come up before giving up.
// Warm boots typically settle in < 5s; first-boot or post-network-outage
// boots can lag closer to 30s. 60s leaves headroom while still keeping the
// locked-screen UI responsive — after that the operator can password-unlock.
const pickupIdentityWaitTimeout = 60 * time.Second
const pickupIdentityWaitInterval = 1 * time.Second
const minimumRecoveryAttemptLifetime = 30 * time.Second

// PickupOutcome reports what happened in the post-boot pickup attempt. The
// caller (gin_server.Start integration) uses this to drive UI status (the
// "Auto-unlocking" transient state and the locked-screen banner).
type PickupOutcome int

const (
	// PickupOutcomeNoBlob: auto-unlock is disabled OR no blob present. Either way,
	// nothing to do — caller proceeds to normal locked-state startup.
	PickupOutcomeNoBlob PickupOutcome = iota
	// PickupOutcomeUnlocked: blob retrieved + unwrapped successfully; the
	// post-decrypt chain has run; caller transitions to Login Required.
	PickupOutcomeUnlocked
	// PickupOutcomeManualUnlockFirst: pickup retrieved a valid F but the
	// password handler had already unlocked the manager. Cleanup ran
	// (local blob + metadata only); caller treats as success.
	PickupOutcomeManualUnlockFirst
	// PickupOutcomeFailed: pickup couldn't unlock the device. Caller leaves
	// the device locked + surfaces the failure banner. State file's
	// last_failure_* fields carry the reason.
	PickupOutcomeFailed
)

// CompleteUnlockChain is the post-decrypt chain handler the orchestrator
// invokes after a successful UnwrapSDEKWithEscrow. Provided by GinServer
// (gin_server.completeUnlockChain) — same helper handleCryptoUnlock uses on
// the password path, so storage / persistence / PCV / reload are kicked off
// uniformly regardless of which Path won.
type CompleteUnlockChain func(ctx context.Context) error

// RunPickup is invoked from gin_server.Start as a goroutine. Reads the
// state, retrieves F from namek if a blob exists, unwraps the SDEK,
// and triggers the post-decrypt chain. Falls through cleanly to manual unlock
// on any failure — the locked HTTP server stays up the whole time so the
// password path is always available in parallel.
//
// The shared operation gate serializes this path with prepare, Test, settings,
// and local cleanup. Gate acquisition itself is context-aware so an emergency
// owner can abandon continuity at its hard deadline.
func (o *Orchestrator) RunPickup(ctx context.Context, completeChain CompleteUnlockChain) PickupOutcome {
	result, err := o.Recover(ctx, completeChain)
	if err != nil {
		log.Printf("WARN: autounlock: recovery failed: %v", err)
	}
	switch result.Disposition {
	case RecoverDispositionUnlocked:
		if result.Reason == ReasonManualUnlockFirst {
			return PickupOutcomeManualUnlockFirst
		}
		return PickupOutcomeUnlocked
	case RecoverDispositionNoHandoff:
		return PickupOutcomeNoBlob
	default:
		return PickupOutcomeFailed
	}
}

// Recover consumes a compatible local handoff, asks its recorded provider for
// the random factor, unwraps the SDEK with the released v1 AAD, and runs the
// complete unlock chain before consuming local authority.
func (o *Orchestrator) Recover(ctx context.Context, completeChain CompleteUnlockChain) (RecoverResult, error) {
	if err := o.acquire(ctx); err != nil {
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonServiceUnreachable}, err
	}
	defer o.release()

	state, stateErr := LoadState()
	if stateErr != nil && !errors.Is(stateErr, ErrInvalidStateFile) {
		log.Printf("WARN: autounlock: pickup load state: %v", stateErr)
		return RecoverResult{Disposition: RecoverDispositionNoHandoff}, stateErr
	}
	if !state.Enabled {
		return RecoverResult{Disposition: RecoverDispositionNoHandoff}, nil
	}

	// Marker for handleCryptoStatus's UI surface. Set after the disabled-
	// or-state-unreadable bail so the "Auto-unlocking…" indicator never
	// shows on devices that never opted in.
	o.inFlight.Store(true)
	defer o.inFlight.Store(false)

	handoff, err := o.reconcileHandoffLocked(&state, stateErr)
	if err != nil {
		if errors.Is(err, ErrBlobMissing) {
			// Auto-unlock is enabled but ceremony didn't deposit (crash,
			// SIGKILL, hardware-watchdog reset). Audit the silent gap so
			// operators can see "we expected to auto-unlock and didn't."
			o.recordFailure(&state, AuditCycleFailedPickup, ReasonNoBlob)
			return RecoverResult{Disposition: RecoverDispositionNoHandoff, Reason: ReasonNoBlob}, nil
		}
		if errors.Is(err, ErrEffectiveExpiryTooShort) {
			o.recordFailure(&state, AuditCycleFailedPickup, ReasonEscrowNotFound)
			return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonEscrowNotFound}, err
		}
		log.Printf("ERROR: autounlock: reconcile handoff: %v", err)
		if errors.Is(stateErr, ErrInvalidStateFile) {
			// Preserve the unreadable state file. Rewriting defaults here would
			// erase evidence of possibly matching future metadata and downgrade
			// the same raw blob to legacy Namek on the next attempt.
			o.emitAudit(AuditCycleFailedPickup, map[string]any{"reason": ReasonBlobCorrupt})
		} else {
			o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
		}
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
	}
	if !handoff.expiry.IsZero() && handoff.expiry.Sub(o.deps.Now()) < minimumRecoveryAttemptLifetime {
		// A recurrent unlock-chain backoff may outlive most of a recorded
		// provider lease. Do not start a pickup that cannot retain the RFC's
		// final retry margin; clear only local authority and let the remote
		// factor expire naturally.
		if clearErr := o.clearLocalHandoffLocked(&state); clearErr != nil {
			return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, clearErr
		}
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonEscrowNotFound)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonEscrowNotFound}, ErrEffectiveExpiryTooShort
	}

	// Wait briefly for identity to come up — TPM enrollment + namek client
	// init can race the pickup goroutine on a fast boot. Without this wait,
	// a first-boot pickup that runs ~2s before identity finishes would
	// record service_not_ready and require manual unlock for that boot. The
	// wait is bounded by deps.PickupIdentityWaitTimeout AND by ctx (Stop
	// Phase 0 cancels promptly).
	if !o.waitForProvider(ctx, o.deps.PickupIdentityWaitTimeout) {
		log.Printf("WARN: autounlock: pickup — recovery provider not ready after %s", o.deps.PickupIdentityWaitTimeout)
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonServiceNotReady)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonServiceNotReady}, nil
	}
	provider, ok := o.providerForID(handoff.providerID)
	if !ok {
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonServiceNotReady)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonServiceNotReady}, nil
	}

	factor, err := o.pickupWithRetry(ctx, provider)
	if err != nil {
		reason := ReasonServiceUnreachable
		if errors.Is(err, namekclient.ErrEscrowNotFound) {
			// F is gone at namek — blob is permanently inert. Delete it so
			// the next boot doesn't show stale state.
			_ = o.clearLocalHandoffLocked(&state)
			reason = ReasonEscrowNotFound
		} else if errors.Is(err, ErrRecoveryFactorInvalid) {
			reason = ReasonBlobCorrupt
		} else {
			var apiErr *namekclient.APIError
			if errors.As(err, &apiErr) {
				if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
					reason = ReasonAuthFailed
				}
			}
		}
		log.Printf("WARN: autounlock: pickup failed (%s): %v", reason, err)
		o.recordFailure(&state, AuditCycleFailedPickup, reason)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: reason}, err
	}
	if len(factor) != fSize {
		cryptoutil.SecureZero(factor)
		err := fmt.Errorf("%w: factor length %d", ErrRecoveryFactorInvalid, len(factor))
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
	}
	defer cryptoutil.SecureZero(factor)

	aad, aadErr := o.aad()
	if aadErr != nil {
		log.Printf("ERROR: autounlock: aad: %v", aadErr)
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonServiceNotReady)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonServiceNotReady}, aadErr
	}
	unwrapErr := o.deps.Manager.UnwrapSDEKWithEscrow(handoff.blob, factor, aad)
	if errors.Is(unwrapErr, crypt.ErrAutoUnlockAlreadyUnlocked) {
		// Manual-unlock-first means the password path installed the SDEK, not
		// necessarily that its joinable post-decrypt chain has reached Ready.
		// Join the same chain and retain the handoff on failure/timeout so the
		// next bounded recovery attempt still has valid authority.
		if completeChain == nil {
			err := errors.New("autounlock: complete unlock chain unavailable")
			o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
			return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
		}
		if err := completeChain(ctx); err != nil {
			log.Printf("ERROR: autounlock: join manual-first unlock chain: %v", err)
			o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
			return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
		}
		if err := o.clearLocalHandoffLocked(&state); err != nil {
			return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
		}
		o.emitAudit(AuditCycleRevoked, map[string]any{"reason": ReasonManualUnlockFirst})
		return RecoverResult{Disposition: RecoverDispositionUnlocked, Reason: ReasonManualUnlockFirst}, nil
	}
	if errors.Is(unwrapErr, crypt.ErrAutoUnlockBlobCorrupt) {
		_ = o.clearLocalHandoffLocked(&state)
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, unwrapErr
	}
	if unwrapErr != nil {
		log.Printf("ERROR: autounlock: unwrap SDEK: %v", unwrapErr)
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, unwrapErr
	}

	if completeChain == nil {
		err := errors.New("autounlock: complete unlock chain unavailable")
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
	}
	if err := completeChain(ctx); err != nil {
		// Post-decrypt chain failure (storage volume, persistence notify, PCV,
		// reload). Externally indistinguishable from a corrupt blob — the
		// device is locked at the same downstream level. Folded into
		// ReasonBlobCorrupt to keep the failure-token vocabulary minimal.
		log.Printf("ERROR: autounlock: post-unlock chain: %v", err)
		o.recordFailure(&state, AuditCycleFailedPickup, ReasonBlobCorrupt)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
	}

	if err := DeleteBlob(); err != nil {
		log.Printf("WARN: autounlock: delete blob post-pickup: %v", err)
		return RecoverResult{Disposition: RecoverDispositionManualUnlockRequired, Reason: ReasonBlobCorrupt}, err
	}
	o.clearHandoffClaimsLocked()
	state.Handoff = nil
	now := o.deps.Now()
	state.LastPickupAt = &now
	state.LastFailureAt = nil
	state.LastFailureReason = ""
	if err := SaveState(state); err != nil {
		log.Printf("WARN: autounlock: persist post-pickup state: %v", err)
	}
	o.emitAudit(AuditCyclePickedUp, nil)
	return RecoverResult{Disposition: RecoverDispositionUnlocked}, nil
}

// pickupRetryAttempts bounds the number of namek PickupUnlockEscrow tries.
// Boot timing is racy: WaitForIdentityReady fires when IsIdentityReady()
// flips true (namek client constructed + identity enrolled), but the
// underlying namek client may still be mid-DNS-lookup / mid-TLS-handshake
// for several seconds after construction. Without retry, the first pickup
// attempt fails with service_unreachable and the device sits at the
// password prompt despite a healthy blob + healthy escrow row.
const pickupRetryAttempts = 4
const pickupRetryBackoff = 2 * time.Second

// pickupWithRetry calls PickupUnlockEscrow with bounded retry on transient
// errors. Mirrors ceremony.deposit's retry pattern but with more attempts
// since boot-time network ramp-up can take several seconds. Non-transient
// errors (auth_failed, escrow_not_found) bail immediately so the caller
// records the right failure reason on the first cycle.
func (o *Orchestrator) pickupWithRetry(ctx context.Context, provider RecoveryFactorProvider) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < pickupRetryAttempts; attempt++ {
		factor, err := provider.Pickup(ctx)
		if err == nil {
			return factor, nil
		}
		lastErr = err
		if !isTransientNamekErr(err) {
			return nil, err
		}
		if attempt+1 < pickupRetryAttempts {
			log.Printf("WARN: autounlock: pickup transient error, retrying in %s: %v", pickupRetryBackoff, err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(pickupRetryBackoff):
			}
		}
	}
	return nil, lastErr
}

// waitForIdentity blocks until identity is ready, the timeout elapses, or
// ctx is cancelled. When deps.WaitForIdentityReady is wired (production),
// uses event-bus subscription for sub-1s wake on enrollment completion;
// falls back to 1s polling when nil (tests, hand-wired callers).
func (o *Orchestrator) waitForIdentity(ctx context.Context, timeout time.Duration) bool {
	if o.deps.IsIdentityReady == nil {
		return false
	}
	if o.deps.IsIdentityReady() {
		return true
	}
	if o.deps.WaitForIdentityReady != nil {
		return o.deps.WaitForIdentityReady(ctx, timeout)
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pickupIdentityWaitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.Done():
			return o.deps.IsIdentityReady()
		case <-ticker.C:
			if o.deps.IsIdentityReady() {
				return true
			}
		}
	}
}
