package autounlock

// Audit event kind tokens, namespaced under auto_unlock.* and system.timezone.*.
const (
	AuditEnabledChanged     = "auto_unlock.enabled.changed"
	AuditAutoRebootChanged  = "auto_unlock.auto_reboot.changed"
	AuditCycleDeposited     = "auto_unlock.cycle.deposited"
	AuditCyclePickedUp      = "auto_unlock.cycle.picked_up"
	AuditCycleRevoked       = "auto_unlock.cycle.revoked"
	AuditCycleFailedDeposit = "auto_unlock.cycle.failed_deposit"
	AuditCycleFailedPickup  = "auto_unlock.cycle.failed_pickup"
	AuditTestRun            = "auto_unlock.test.run"
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
)

// emitAudit is a thin nil-safe wrapper so callers don't need to check the
// callback before every emit. Best-effort — drops the event on a nil emitter.
func (o *Orchestrator) emitAudit(kind string, details map[string]any) {
	if o.deps.PublishAudit == nil {
		return
	}
	o.deps.PublishAudit(kind, details)
}
