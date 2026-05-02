package autounlock

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"

	"piccolod/internal/crypt"
	"piccolod/internal/cryptoutil"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

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
	// (revoke + delete blob); caller treats as success.
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
// Takes o.mu to serialize against ceremony. If a fast Stop catches pickup
// mid-namek-call, the wiring layer cancels pickup's context first → pickup's
// HTTP call returns ctx.Canceled → pickup releases the mutex → ceremony
// proceeds. Without this lock, pickup's tail (state-file save, blob delete,
// revoke) could race ceremony's wrap+deposit on the same files.
func (o *Orchestrator) RunPickup(ctx context.Context, completeChain CompleteUnlockChain) PickupOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()

	state, err := LoadState()
	if err != nil && err != ErrInvalidStateFile {
		log.Printf("WARN: autounlock: pickup load state: %v", err)
		return PickupOutcomeNoBlob
	}
	if !state.Enabled {
		return PickupOutcomeNoBlob
	}

	blob, err := ReadBlob()
	if err != nil {
		if errors.Is(err, ErrBlobMissing) {
			// Auto-unlock is enabled but ceremony didn't deposit (crash,
			// SIGKILL, hardware-watchdog reset). Audit the silent gap so
			// operators can see "we expected to auto-unlock and didn't."
			o.recordFailure(&state, ReasonNoBlob)
			return PickupOutcomeNoBlob
		}
		log.Printf("ERROR: autounlock: pickup read blob: %v", err)
		o.recordFailure(&state, ReasonNoBlob)
		return PickupOutcomeFailed
	}

	if !o.deps.IsIdentityReady() {
		log.Printf("WARN: autounlock: pickup — identity not ready")
		o.recordFailure(&state, ReasonServiceNotReady)
		return PickupOutcomeFailed
	}
	client := o.deps.NamekClient()
	if client == nil {
		o.recordFailure(&state, ReasonServiceNotReady)
		return PickupOutcomeFailed
	}

	resp, err := client.PickupUnlockEscrow(ctx)
	if err != nil {
		reason := ReasonServiceUnreachable
		if errors.Is(err, namekclient.ErrEscrowNotFound) {
			// F is gone at namek — blob is permanently inert. Delete it so
			// the next boot doesn't show stale state.
			_ = DeleteBlob()
			reason = ReasonEscrowNotFound
		} else {
			var apiErr *namekclient.APIError
			if errors.As(err, &apiErr) {
				if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
					reason = ReasonAuthFailed
				}
			}
		}
		log.Printf("WARN: autounlock: pickup failed (%s): %v", reason, err)
		o.recordFailure(&state, reason)
		return PickupOutcomeFailed
	}

	F, err := base64.RawURLEncoding.DecodeString(resp.Secret)
	if err != nil {
		log.Printf("ERROR: autounlock: decode F: %v", err)
		o.recordFailure(&state, ReasonBlobCorrupt)
		return PickupOutcomeFailed
	}
	defer cryptoutil.SecureZero(F)

	unwrapErr := o.deps.Manager.UnwrapSDEKWithEscrow(blob, F, o.aad())
	if errors.Is(unwrapErr, crypt.ErrAutoUnlockAlreadyUnlocked) {
		// Manual-unlock-first race: revoke server-side state and delete the
		// blob. From the user's perspective this is a success — the device
		// is unlocked, just via the password path, not auto.
		o.bestEffortRevoke(ctx, client)
		_ = DeleteBlob()
		o.emitAudit(AuditCycleRevoked, map[string]any{"reason": ReasonManualUnlockFirst})
		return PickupOutcomeManualUnlockFirst
	}
	if errors.Is(unwrapErr, crypt.ErrAutoUnlockBlobCorrupt) {
		_ = DeleteBlob()
		o.recordFailure(&state, ReasonBlobCorrupt)
		return PickupOutcomeFailed
	}
	if unwrapErr != nil {
		log.Printf("ERROR: autounlock: unwrap SDEK: %v", unwrapErr)
		o.recordFailure(&state, ReasonBlobCorrupt)
		return PickupOutcomeFailed
	}

	if err := completeChain(ctx); err != nil {
		log.Printf("ERROR: autounlock: post-unlock chain: %v", err)
		o.recordFailure(&state, ReasonChainFailed)
		return PickupOutcomeFailed
	}

	o.bestEffortRevoke(ctx, client)
	if err := DeleteBlob(); err != nil {
		log.Printf("WARN: autounlock: delete blob post-pickup: %v", err)
	}
	now := o.deps.Now()
	state.LastPickupAt = &now
	state.LastFailureAt = nil
	state.LastFailureReason = ""
	if err := SaveState(state); err != nil {
		log.Printf("WARN: autounlock: persist post-pickup state: %v", err)
	}
	o.emitAudit(AuditCyclePickedUp, nil)
	return PickupOutcomeUnlocked
}

// bestEffortRevoke calls RevokeUnlockEscrow with errors swallowed. namek's
// DELETE is idempotent and 204s even when no row exists — the call is purely
// cleanup and a transient failure here doesn't compromise correctness (the
// row will expire naturally).
func (o *Orchestrator) bestEffortRevoke(ctx context.Context, client NamekEscrowClient) {
	if err := client.RevokeUnlockEscrow(ctx); err != nil {
		log.Printf("WARN: autounlock: revoke: %v", err)
	}
}
