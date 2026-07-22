// Package autounlock implements the device-side framework for opt-in
// post-reboot disk unlock via a remote escrow provider (namek in v1).
//
// Architecture:
//   - Path A unlock at the SDEK layer is provided by the existing
//     crypt.Manager.Wrap/Unwrap escrow methods (sibling to Path P/R).
//   - This package owns enabled state, the on-disk blob lifecycle, and the
//     pre-shutdown ceremony / post-boot pickup orchestrators.
package autounlock

import "errors"

var (
	// ErrBlobMissing: pickup invoked but no on-disk blob exists. Typical
	// cause: prior shutdown bypassed the SIGTERM handler (kernel panic,
	// SIGKILL, hardware-watchdog reset). Audit signal only — caller falls
	// through to manual unlock.
	ErrBlobMissing = errors.New("autounlock: blob missing on disk")

	// ErrInvalidStateFile: the on-disk auto_unlock.json failed to parse.
	// Treated as missing — caller falls back to disabled defaults.
	ErrInvalidStateFile = errors.New("autounlock: state file invalid")

	// ErrHandoffBusy means a local wrapped-SDEK handoff already depends on
	// the provider's singleton slot. Test and any new preparation must not
	// overwrite it.
	ErrHandoffBusy = errors.New("autounlock: restart handoff already outstanding")

	// ErrHandoffMetadataInvalid is fail-closed: matching metadata could name a
	// future format/provider and therefore must never be downgraded to legacy
	// Namek pickup.
	ErrHandoffMetadataInvalid = errors.New("autounlock: handoff metadata invalid or unsupported")

	// ErrEffectiveExpiryTooShort means the provider accepted a deposit but did
	// not grant enough remaining lifetime for bounded restart recovery.
	ErrEffectiveExpiryTooShort = errors.New("autounlock: provider expiry leaves insufficient recovery time")

	// ErrRecoveryFactorInvalid is a permanent provider-response failure, not a
	// transport error worth retrying.
	ErrRecoveryFactorInvalid = errors.New("autounlock: recovery factor invalid")

	// ErrInvalidRecoveryProviderID rejects custom providers that cannot be
	// distinguished from the built-in Namek v1 wire protocol.
	ErrInvalidRecoveryProviderID = errors.New("autounlock: custom recovery provider ID must be non-empty and non-reserved")
)
