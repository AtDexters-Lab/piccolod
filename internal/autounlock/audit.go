package autounlock

// Audit event kind tokens, namespaced under auto_unlock.* and system.timezone.*.
// Plan §audit-event-kinds enumerates the full set including scheduler events
// (deferred to the scheduler commit).
const (
	AuditEnabledChanged    = "auto_unlock.enabled.changed"
	AuditCycleDeposited    = "auto_unlock.cycle.deposited"
	AuditCyclePickedUp     = "auto_unlock.cycle.picked_up"
	AuditCycleRevoked      = "auto_unlock.cycle.revoked"
	AuditCycleFailedPickup = "auto_unlock.cycle.failed_pickup"
	AuditTestRun           = "auto_unlock.test.run"
)

// Failure-reason tokens passed in the `reason` field of `failed_pickup` audit
// events and rendered (via a UI-side map) into user-facing strings on the
// locked-screen banner and Activity sub-page. Brand-neutral names so future
// providers (passkey-PRF push, YubiKey, cluster-quorum) can reuse them.
const (
	ReasonServiceUnreachable = "service_unreachable"
	ReasonServiceNotReady    = "service_not_ready"
	ReasonAuthFailed         = "auth_failed"
	ReasonEscrowNotFound     = "escrow_not_found"
	ReasonBlobCorrupt        = "blob_corrupt"
	ReasonBlobWriteFailed    = "blob_write_failed"
	ReasonNoBlob             = "no_blob"
	ReasonDepositFailed      = "deposit_failed"
	ReasonManualUnlockFirst  = "manual_unlock_first"
	// ReasonChainFailed: pickup retrieved F and unwrapped SDEK successfully,
	// but the post-decrypt chain (UnlockDataVolume / persistence notify /
	// PCV / reload) failed. Distinct from blob_corrupt — the blob was good;
	// the downstream subsystem broke.
	ReasonChainFailed        = "chain_failed"
)

// emitAudit is a thin nil-safe wrapper so callers don't need to check the
// callback before every emit. Best-effort — drops the event on a nil emitter.
func (o *Orchestrator) emitAudit(kind string, details map[string]any) {
	if o.deps.PublishAudit == nil {
		return
	}
	o.deps.PublishAudit(kind, details)
}
