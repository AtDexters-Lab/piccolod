package autounlock

import (
	"context"
	"log"
)

// Update applies a partial update to the on-disk state. Handles the cleanup
// transition (delete the local blob + metadata only) when going from enabled
// to disabled. The unkeyed Namek v1 revoke is intentionally not used because
// a late revoke could erase a newer singleton-slot deposit. Initializes
// auto_reboot defaults on the first transition from
// disabled to enabled.
//
// Validates the window-hour bounds (0..23, start != end) when either is
// supplied; returns ErrInvalidWindow on malformed input.
//
// Caller is the HTTP PUT handler; emits AuditEnabledChanged on enabled
// transitions and AuditAutoRebootChanged when the auto_reboot block changes.
func (o *Orchestrator) Update(ctx context.Context, in UpdateInput) error {
	if err := o.acquire(ctx); err != nil {
		return err
	}
	defer o.release()

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

	// Disable transition: clean up the local handoff so the next reboot cycle
	// doesn't try to pickup against stale state. The remote factor expires. Also
	// reset the auto_reboot block (window + last_fired/failed timestamps)
	// so a future re-enable starts from defaults — without this, an operator
	// who toggles off-then-on retains stale fire timestamps that could
	// suppress the next legitimate fire via rehydrate.
	if prev.Enabled && !state.Enabled {
		if err := DeleteBlob(); err != nil {
			log.Printf("WARN: autounlock: disable-cleanup blob: %v", err)
			// The first save above already committed enabled=false while
			// retaining metadata. Do not clear metadata when the raw blob could
			// not be removed: that would make a later re-enable reinterpret the
			// stale bytes as a legacy handoff.
			return err
		}
		o.clearHandoffClaimsLocked()
		state.Handoff = nil
		state.AutoReboot = AutoReboot{}
		if err := SaveState(state); err != nil {
			log.Printf("WARN: autounlock: persist post-disable reset: %v", err)
		}
		o.emitAudit(AuditCycleRevoked, map[string]any{"reason": "disabled"})
	}

	if in.Enabled != nil && *in.Enabled != prev.Enabled {
		o.emitAudit(AuditEnabledChanged, map[string]any{
			"from": prev.Enabled,
			"to":   *in.Enabled,
		})
	}
	// Audit operator-initiated auto_reboot changes — drives the Activity
	// sub-page entry that lets the operator audit "did someone change my
	// maintenance window?" Gate on `in.AutoReboot != nil` (operator
	// explicitly sent the block) rather than struct-diff: the
	// first-enable seed-defaults branch and the disable-cleanup zero both
	// modify state.AutoReboot internally, and neither is user-initiated.
	if in.AutoReboot != nil && state.AutoReboot != prev.AutoReboot {
		o.emitAudit(AuditAutoRebootChanged, map[string]any{
			"from_enabled": prev.AutoReboot.Enabled,
			"to_enabled":   state.AutoReboot.Enabled,
			"from_window":  [2]int{prev.AutoReboot.WindowStartHour, prev.AutoReboot.WindowEndHour},
			"to_window":    [2]int{state.AutoReboot.WindowStartHour, state.AutoReboot.WindowEndHour},
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
