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
)
