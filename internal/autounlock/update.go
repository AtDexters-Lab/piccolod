package autounlock

import (
	"context"
	"log"
)

// Update applies a partial update to the on-disk state. Handles the cleanup
// transition (revoke at namek + delete on-disk blob) when going from enabled
// to disabled. Initializes auto_reboot defaults on the first transition from
// disabled to enabled.
//
// Validates the window-hour bounds (0..23, start != end) when either is
// supplied; returns ErrInvalidWindow on malformed input.
//
// Caller is the HTTP PUT handler; emits AuditEnabledChanged on enabled
// transitions. State changes other than enabled toggles are not audited.
func (o *Orchestrator) Update(ctx context.Context, in UpdateInput) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	state, _ := LoadState() // ErrInvalidStateFile → fall through to defaults
	prev := state

	if in.Enabled != nil {
		state.Enabled = *in.Enabled
		// First transition disabled→enabled and auto_reboot has zero-value
		// window bounds → seed with defaults so the user has a meaningful
		// window if they toggle auto_reboot on later.
		if *in.Enabled && !prev.Enabled &&
			state.AutoReboot.WindowStartHour == 0 && state.AutoReboot.WindowEndHour == 0 {
			state.AutoReboot = DefaultAutoReboot()
		}
	}

	if in.AutoReboot != nil {
		ar := state.AutoReboot
		if in.AutoReboot.Enabled != nil {
			ar.Enabled = *in.AutoReboot.Enabled
		}
		if in.AutoReboot.WindowStartHour != nil {
			ar.WindowStartHour = *in.AutoReboot.WindowStartHour
		}
		if in.AutoReboot.WindowEndHour != nil {
			ar.WindowEndHour = *in.AutoReboot.WindowEndHour
		}
		if !validWindow(ar.WindowStartHour, ar.WindowEndHour) {
			return ErrInvalidWindow
		}
		state.AutoReboot = ar
	}

	if err := SaveState(state); err != nil {
		return err
	}

	// Disable transition: clean up server-side escrow and on-disk blob so
	// the next reboot cycle doesn't try to pickup against stale state.
	if prev.Enabled && !state.Enabled {
		if client := o.deps.NamekClient(); client != nil {
			if err := client.RevokeUnlockEscrow(ctx); err != nil {
				log.Printf("WARN: autounlock: disable-cleanup revoke: %v", err)
			}
		}
		if err := DeleteBlob(); err != nil {
			log.Printf("WARN: autounlock: disable-cleanup blob: %v", err)
		}
		o.emitAudit(AuditCycleRevoked, map[string]any{"reason": "disabled"})
	}

	if in.Enabled != nil && *in.Enabled != prev.Enabled {
		o.emitAudit(AuditEnabledChanged, map[string]any{
			"from": prev.Enabled,
			"to":   *in.Enabled,
		})
	}
	return nil
}

func validWindow(start, end int) bool {
	if start < 0 || start > 23 || end < 0 || end > 23 {
		return false
	}
	return start != end
}
