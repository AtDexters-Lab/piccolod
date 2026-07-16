package network

import "time"

// UserIntent captures user-visible AP toggles. Boot-volatile (N4-S4): both
// fields default to false on every piccolod start. Rationale:
//   - ForceAP=true is typically transient (user fixing something now); a
//     stale ForceAP surviving reboot would silently lock the device into
//     AP mode after upgrade.
//   - SuppressAP=true would similarly survive reboot and silently disable
//     AP recovery long after the user forgot they set it.
type UserIntent struct {
	ForceAP    bool
	SuppressAP bool
}

// APAction is the union of AP-mode arbiter decisions.
type APAction interface {
	apActionMarker()
}

type APEnter struct {
	Reason string
}

func (APEnter) apActionMarker() {}

type APExit struct {
	Reason string
}

func (APExit) apActionMarker() {}

type APUnchanged struct {
	Reason string
}

func (APUnchanged) apActionMarker() {}

// AP entry rate-shaping (S2-rev / F-S14):
//   - At most 4 toggles in the last 1h, OR
//   - Cooldown elapsed (>10min since last toggle).
//
// Either condition permits entry. Worst-case lockout window is 10 min.
const apToggleCooldown = 10 * time.Minute

// decideAP is the pure AP arbiter. Cross-device.
//
// Exit is unconditional whenever any uplink works — never suppressed by
// the toggle budget (S2-rev). Entry is rate-shaped.
func decideAP(tick Tick, led ActionLedger, intent UserIntent, apActive bool) APAction {
	// SystemBusy gate (B4-rev): don't toggle AP mid-install.
	if tick.SystemBusy {
		return APUnchanged{Reason: "system busy: " + tick.SystemBusyReason}
	}

	if intent.ForceAP {
		if !apActive {
			return APEnter{Reason: "user-forced"}
		}
		return APUnchanged{Reason: "user-forced; already AP"}
	}

	// SuppressAP: exit if we're in AP, otherwise stay out.
	if intent.SuppressAP {
		if apActive {
			return APExit{Reason: "user-suppressed"}
		}
		return APUnchanged{Reason: "user-suppressed; not AP"}
	}

	// Exit unconditionally when any uplink is reachable.
	if anyDeviceWorking(tick) {
		if apActive {
			return APExit{Reason: "uplink restored"}
		}
		return APUnchanged{Reason: "uplink active; not AP"}
	}

	wifi, hasWiFi := tick.Devices[DeviceWiFi]
	if !hasWiFi || wifi.HWHealth != TriHealthy {
		// Without healthy WiFi HW, AP is impossible. Snapshot.Hint surfaces
		// the cause to the operator.
		if apActive {
			// Edge case: WiFi HW just went Faulted while we were in AP.
			// Tear down — staying in AP with no radio is meaningless.
			return APExit{Reason: "wifi HW unhealthy"}
		}
		return APUnchanged{Reason: "wifi HW not healthy; AP impossible"}
	}

	configBroken := wifi.ConfigHealth == TriFaulted || wifi.ConfigHealth == TriInactive

	if !configBroken {
		// Config is healthy or recovering — STA flow handles it.
		if apActive {
			// Should not happen (anyDeviceWorking would have exited),
			// but be defensive.
			return APExit{Reason: "wifi config healthy"}
		}
		return APUnchanged{Reason: "wifi config healthy"}
	}

	if apActive {
		// Already in AP and config still broken — stay.
		return APUnchanged{Reason: "AP active; config still broken"}
	}

	if !apEntryPermitted(led, tick.At) {
		return APUnchanged{Reason: "AP entry rate-shaped"}
	}

	return APEnter{Reason: "no uplink; wifi HW healthy + config broken"}
}

// apEntryPermitted returns true iff (toggle count in last 1h < 4) OR
// (last toggle was > apToggleCooldown ago / never happened).
func apEntryPermitted(led ActionLedger, now time.Time) bool {
	if recentToggles(led.APToggles, now, apToggleWindow) < 4 {
		return true
	}
	last := lastToggleAt(led.APToggles)
	if last.IsZero() || now.Sub(last) > apToggleCooldown {
		return true
	}
	return false
}

// anyDeviceWorking uses the authoritative per-interface/default-route
// projection only when both interface enumeration and route ownership were
// observed. Class-level recovery observations remain the fallback for an
// incomplete projection, without becoming public connectivity truth.
func anyDeviceWorking(tick Tick) bool {
	if tick.InterfacesObserved && tick.DefaultRouteObserved {
		if tick.ActiveUplink == UplinkNone || tick.ActiveUplinkIface == "" {
			return false
		}
		for _, iface := range tick.Interfaces {
			if iface.Iface == tick.ActiveUplinkIface {
				return iface.LinkUp && iface.HasIP
			}
		}
		return false
	}
	for _, obs := range tick.Devices {
		if obs.HWHealth != TriHealthy || obs.ConfigHealth != TriHealthy || !obs.LinkUp || !obs.HasIP {
			continue
		}
		if obs.GwReachable == TriHealthy || tick.L3Probe == L3ProbeUp {
			return true
		}
	}
	return false
}

func recentToggles(toggles []APEvent, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, e := range toggles {
		if e.When.After(cutoff) {
			n++
		}
	}
	return n
}

func lastToggleAt(toggles []APEvent) time.Time {
	var last time.Time
	for _, e := range toggles {
		if e.When.After(last) {
			last = e.When
		}
	}
	return last
}
