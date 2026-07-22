package autounlock

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"piccolod/internal/cryptoutil"
)

// RestartUnlockContinuity is the capability consumed by graceful shutdown and
// bounded emergency restart owners. It deliberately exposes no provider or
// Namek transport types.
type RestartUnlockContinuity interface {
	Prepare(ctx context.Context, trigger PrepareTrigger, ttl time.Duration) (PrepareResult, error)
	Recover(ctx context.Context, completeUnlockChain CompleteUnlockChain) (RecoverResult, error)
	Cancel(ctx context.Context) error
}

type PrepareTrigger string

const (
	PrepareTriggerGracefulShutdown PrepareTrigger = "graceful_shutdown"
	PrepareTriggerTaskWarning      PrepareTrigger = "task_warning"
	PrepareTriggerTaskCritical     PrepareTrigger = "task_critical"
	PrepareTriggerRecoveryFatal    PrepareTrigger = "recovery_fatal"
)

type PrepareDisposition string

const (
	PrepareDispositionPrepared    PrepareDisposition = "prepared"
	PrepareDispositionReused      PrepareDisposition = "reused"
	PrepareDispositionNotNeeded   PrepareDisposition = "not_needed"
	PrepareDispositionUnavailable PrepareDisposition = "unavailable"
)

type PrepareResult struct {
	Disposition PrepareDisposition
	ExpiresAt   time.Time
}

type RecoverDisposition string

const (
	RecoverDispositionUnlocked             RecoverDisposition = "unlocked"
	RecoverDispositionManualUnlockRequired RecoverDisposition = "manual_unlock_required"
	RecoverDispositionNoHandoff            RecoverDisposition = "no_handoff"
)

type RecoverResult struct {
	Disposition RecoverDisposition
	Reason      string
}

const minimumEffectiveLifetime = 120 * time.Second

func (o *Orchestrator) Prepare(ctx context.Context, trigger PrepareTrigger, ttl time.Duration) (PrepareResult, error) {
	if err := o.acquire(ctx); err != nil {
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	defer o.release()
	return o.prepareLocked(ctx, trigger, ttl)
}

func (o *Orchestrator) prepareLocked(ctx context.Context, trigger PrepareTrigger, ttl time.Duration) (PrepareResult, error) {
	if ttl <= 0 {
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, errors.New("autounlock: prepare TTL must be positive")
	}

	state, stateErr := LoadState()
	if stateErr != nil && !errors.Is(stateErr, ErrInvalidStateFile) {
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, stateErr
	}
	if !state.Enabled || o.deps.Manager == nil || !o.deps.Manager.SDEKLoaded() {
		return PrepareResult{Disposition: PrepareDispositionNotNeeded}, nil
	}

	existing, err := o.reconcileHandoffLocked(&state, stateErr)
	switch {
	case err == nil && !existing.expiry.IsZero() && existing.expiry.Sub(o.deps.Now()) >= minimumEffectiveLifetime:
		o.claimRestartHandoffLocked(trigger, existing.blob)
		return PrepareResult{Disposition: PrepareDispositionReused, ExpiresAt: existing.expiry}, nil
	case err == nil:
		// A rolling-upgrade raw blob has no trustworthy effective expiry, and
		// a recognized lease below the recovery minimum cannot carry a bounded
		// restart. Preserve that existing recovery chance: v1's unkeyed singleton
		// provider cannot replace it without a window where a failed deposit or
		// process exit destroys the only usable handoff. Explicit cancellation or
		// successful pickup owns retirement; a future refresh protocol needs
		// keyed generations or dual-candidate provider support.
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, nil
	case errors.Is(err, ErrBlobMissing):
		// No outstanding handoff. Repair a wholly invalid state file before
		// staging provider dispatch metadata; a present blob must never inherit
		// unreadable or stale dispatch authority.
		if stateErr != nil {
			state.Handoff = nil
			if saveErr := SaveState(state); saveErr != nil {
				return PrepareResult{Disposition: PrepareDispositionUnavailable}, saveErr
			}
		}
	case errors.Is(err, ErrEffectiveExpiryTooShort):
		// Reconciliation already removed an expired recognized handoff. A new
		// one may now take ownership of the singleton slot.
	case err != nil:
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}

	if !o.providerReady() {
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, nil
	}
	binding, ok := o.configuredProvider()
	if !ok {
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, nil
	}

	factor := make([]byte, fSize)
	defer cryptoutil.SecureZero(factor)
	if _, err := io.ReadFull(rand.Reader, factor); err != nil {
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}

	aad, err := o.aad()
	if err != nil {
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	blob, err := o.deps.Manager.WrapSDEKForEscrow(factor, aad)
	if err != nil {
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	// Persist non-secret provider dispatch authority before publishing the raw
	// blob. A crash after WriteBlob must dispatch this exact blob to the selected
	// provider, never downgrade a custom-provider handoff to legacy Namek. A
	// crash before WriteBlob leaves orphan metadata that reconciliation clears.
	if err := setPreparingHandoffMetadata(&state, binding.id, blob); err != nil {
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonBlobWriteFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	if err := SaveState(state); err != nil {
		state.Handoff = nil
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonBlobWriteFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	if err := WriteBlob(blob); err != nil {
		state.Handoff = nil
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonBlobWriteFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	// The singleton handoff has changed. No claim on the previous raw blob is
	// authoritative, even if the provider call below fails.
	o.clearHandoffClaimsLocked()
	o.claimTaskWarningHandoffLocked(trigger, blob)

	expiry, err := o.depositWithRetry(ctx, binding.provider, factor, ttl)
	if err != nil {
		if DeleteBlob() == nil {
			o.clearHandoffClaimsLocked()
			state.Handoff = nil
		}
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	if expiry.Sub(o.deps.Now()) < minimumEffectiveLifetime {
		if DeleteBlob() == nil {
			o.clearHandoffClaimsLocked()
			state.Handoff = nil
		}
		o.recordFailure(&state, AuditCycleFailedDeposit, ReasonDepositFailed)
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, ErrEffectiveExpiryTooShort
	}

	now := o.deps.Now()
	state.LastDepositAt = &now
	if err := setHandoffMetadata(&state, binding.id, expiry, blob); err != nil {
		if DeleteBlob() == nil {
			o.clearHandoffClaimsLocked()
		}
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	if err := SaveState(state); err != nil {
		// The previously committed preparing record still binds this exact raw
		// blob to its provider. Keep both so a restart can recover even though
		// the final effective-expiry update was lost.
		return PrepareResult{Disposition: PrepareDispositionUnavailable}, err
	}
	o.emitAudit(AuditCycleDeposited, map[string]any{
		"trigger":          trigger,
		"effective_window": int(expiry.Sub(now).Seconds()),
		"expires_at":       expiry.UTC().Format(time.RFC3339Nano),
	})
	o.claimRestartHandoffLocked(trigger, blob)
	return PrepareResult{Disposition: PrepareDispositionPrepared, ExpiresAt: expiry}, nil
}

func (o *Orchestrator) claimRestartHandoffLocked(trigger PrepareTrigger, blob []byte) {
	switch trigger {
	case PrepareTriggerGracefulShutdown, PrepareTriggerTaskCritical, PrepareTriggerRecoveryFatal:
		o.restartHandoffClaimDigest = rawBlobDigest(blob)
	}
}

func (o *Orchestrator) claimTaskWarningHandoffLocked(trigger PrepareTrigger, blob []byte) {
	if trigger == PrepareTriggerTaskWarning {
		o.taskWarningHandoffClaimDigest = rawBlobDigest(blob)
	}
}

func (o *Orchestrator) depositWithRetry(ctx context.Context, provider RecoveryFactorProvider, factor []byte, ttl time.Duration) (time.Time, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		expiry, err := provider.Deposit(ctx, factor, ttl)
		if err == nil {
			return expiry, nil
		}
		lastErr = err
		if attempt == 0 && isTransientNamekErr(err) {
			log.Printf("WARN: autounlock: provider deposit transient error, retrying: %v", err)
			continue
		}
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("autounlock: provider deposit retries exhausted: %w", lastErr)
}

// Cancel removes only local handoff authority. The remote singleton factor is
// intentionally left to expire; no unkeyed revoke is safe under overlap.
func (o *Orchestrator) Cancel(ctx context.Context) error {
	if err := o.acquire(ctx); err != nil {
		return err
	}
	defer o.release()
	state, _ := LoadState()
	return o.clearLocalHandoffLocked(&state)
}
