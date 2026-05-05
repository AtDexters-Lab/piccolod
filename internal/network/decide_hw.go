package network

import "time"

// HWAction is the union of HW-recovery decisions emitted by decideHW.
//
// Concrete types (sealed):
//   - HWBounce: bounce a specific device
//   - HWReboot: escalate to system reboot
//   - HWWait:   no action this tick (still trying, rate-limited)
type HWAction interface {
	hwActionMarker()
}

type HWBounce struct {
	Device DeviceKind
	Reason string
}

func (HWBounce) hwActionMarker() {}

type HWReboot struct {
	Reason string
}

func (HWReboot) hwActionMarker() {}

type HWWait struct {
	Reason string // diagnostic only
}

func (HWWait) hwActionMarker() {}

// decideHW is the pure HW-recovery decider for one device. Inputs:
//   - obs:       per-device observation from the current Tick
//   - led:       action ledger (read-only here; rate-shaping budgets)
//   - tick:      full Tick (for SystemBusy gate, L3Probe truth, SystemUptime)
//   - dev:       the device kind being decided about (must match obs.Kind)
//
// Output: exactly one HWAction. Wait is the default; Bounce or Reboot
// only when the L3 probe AND HW classification both say things are
// genuinely broken AND a budget slot is available.
func decideHW(obs DeviceObservation, led ActionLedger, tick Tick, dev DeviceKind) HWAction {
	// Cold-boot grace: don't act for the first 60s of system uptime.
	// Already enforced in probe layer (HWHealth stays Healthy during grace),
	// but defensively return Wait if for any reason we're called pre-grace.
	if tick.SystemUptime < ColdBootGrace {
		return HWWait{Reason: "cold-boot grace"}
	}

	// SystemBusy gate (B4-rev): bouncing or rebooting during install_disk
	// could disrupt the in-flight install.
	if tick.SystemBusy {
		return HWWait{Reason: "system busy: " + tick.SystemBusyReason}
	}

	// Quiet period: defensive — probe layer already suppresses observations
	// for `QuietPeriod` after a bounce, but we re-check here as a guard
	// against the probe layer's dampener being bypassed.
	if last, ok := led.LastBounceAt[dev]; ok && tick.At.Sub(last) < QuietPeriod {
		return HWWait{Reason: "post-action quiet period"}
	}

	// Hard rfkill: bounce can't help.
	if obs.RfkillHard {
		return HWWait{Reason: "rfkill hard-blocked"}
	}

	// Only Faulted HW triggers escalation.
	if obs.HWHealth != TriFaulted {
		return HWWait{}
	}

	// Don't bounce HW just because L3 is suspect — that's an env problem,
	// not an HW problem (catalog C8/C9). Both must agree.
	if tick.L3Probe != L3ProbeDown {
		return HWWait{Reason: "L3 reachable; HW alone not actionable"}
	}

	// Bounce budget: 3 per 1h per device.
	if recentTimes(led.Bounces[dev], tick.At, bounceWindowDuration) < 3 {
		return HWBounce{Device: dev, Reason: "HW Faulted + L3 Down"}
	}

	// Reboot budget: 1 per 2h system-wide. If exhausted, Wait — budget
	// refills naturally.
	if recentTimes(led.Reboots, tick.At, rebootWindowDuration) < 1 {
		return HWReboot{Reason: "bounce budget exhausted; HW still Faulted"}
	}

	return HWWait{Reason: "all budgets exhausted"}
}

// recentTimes counts entries in ts that fall within `window` of `now`.
// Caller passes the appropriate window (bounceWindowDuration / rebootWindowDuration).
func recentTimes(ts []time.Time, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, t := range ts {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}
