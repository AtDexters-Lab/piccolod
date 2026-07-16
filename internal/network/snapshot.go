package network

import "time"

// DeviceStatus enumerates the per-device user-facing health classification.
// Computed mechanically from (DeviceObservation, ledger, tick.SystemBusy).
type DeviceStatus int

const (
	StatusHealthy                    DeviceStatus = iota
	StatusRecovering                              // bounce/reboot just initiated, in quiet period
	StatusWedgedAwaitingBudget                    // Faulted, budget exhausted
	StatusWedgedAwaitingPrecondition              // Faulted, blocked on SystemBusy()
	StatusEnvSuspected                            // HW Healthy + L3 dead persistent
	StatusInactive                                // no profile / no cable / rfkill / AP impossible
)

func (s DeviceStatus) String() string {
	switch s {
	case StatusHealthy:
		return "healthy"
	case StatusRecovering:
		return "recovering"
	case StatusWedgedAwaitingBudget:
		return "wedged_awaiting_budget"
	case StatusWedgedAwaitingPrecondition:
		return "wedged_awaiting_precondition"
	case StatusEnvSuspected:
		return "env_suspected"
	case StatusInactive:
		return "inactive"
	default:
		return "unknown"
	}
}

// DeviceStatusInfo bundles the status enum with its anchor timestamp and
// reason string for one device.
type DeviceStatusInfo struct {
	Status     DeviceStatus
	LastAction time.Time
	Reason     string // diagnostic; aligned with the action that produced this status
}

// APModeInfo describes the AP-mode portion of a Snapshot.
type APModeInfo struct {
	Active bool
	SSID   string
	Reason string // "recovery" | "user-forced" | "setup" | ""
}

// Snapshot is the supervisor's externally-published network status. Three
// on-demand readers consume it: journald (as it happens), captive portal
// HTML (when AP is active), mDNS TXT (passive LAN discovery). Atomic-swapped
// once per tick — readers see whole-snapshot consistency.
type Snapshot struct {
	Devices      map[DeviceKind]DeviceStatusInfo
	ActiveUplink UplinkType
	APMode       APModeInfo
	Connectivity Connectivity
	Hint         string
	At           time.Time
}

// buildSnapshot constructs a Snapshot from the latest Tick + LastBounceAt
// (the only ledger projection needed) + AP runtime state. Mechanical (no
// judgment); see RFC §"Snapshot construction".
func buildSnapshot(tick Tick, lastBounceAt map[DeviceKind]time.Time, apActive bool, apSSID, apReason string) Snapshot {
	devices := make(map[DeviceKind]DeviceStatusInfo, len(tick.Devices))
	for k, obs := range tick.Devices {
		devices[k] = DeviceStatusInfo{
			Status:     classifyDeviceStatus(k, obs, lastBounceAt, tick),
			LastAction: lastBounceAt[k],
		}
	}

	connectivity := tick.Connectivity
	switch {
	case !tick.InterfacesObserved || !tick.DefaultRouteObserved:
		// Device enumeration or route ownership is incomplete. NM/L3 may still
		// indicate working connectivity, but there is no concrete interface to
		// which that fact can safely be attributed.
		connectivity = ConnectivityUnknown
	case !tick.DefaultRouteKnown:
		// A completed observation proved there is no default route.
		connectivity = ConnectivityNone
	}

	return Snapshot{
		Devices:      devices,
		ActiveUplink: tick.ActiveUplink,
		APMode:       APModeInfo{Active: apActive, SSID: apSSID, Reason: apReason},
		Connectivity: connectivity,
		Hint:         deriveHint(tick),
		At:           tick.At,
	}
}

// classifyDeviceStatus implements the priority ladder from the RFC's
// "DeviceStatus resolution priority" section.
func classifyDeviceStatus(dev DeviceKind, obs DeviceObservation, lastBounceAt map[DeviceKind]time.Time, tick Tick) DeviceStatus {
	// Quiet period wins regardless of current probe — prevents
	// Healthy→Recovering→Healthy flicker.
	if last, ok := lastBounceAt[dev]; ok && tick.At.Sub(last) < QuietPeriod {
		return StatusRecovering
	}

	// HW Faulted ladder.
	if obs.HWHealth == TriFaulted {
		if tick.SystemBusy {
			return StatusWedgedAwaitingPrecondition
		}
		return StatusWedgedAwaitingBudget
	}

	// HW Healthy but L3 dead — env class.
	if obs.HWHealth == TriHealthy && obs.GwReachable == TriFaulted && tick.L3Probe == L3ProbeDown {
		return StatusEnvSuspected
	}

	// HW Healthy but Config Faulted (WiFi only — pre-AP-handover narrative).
	if obs.HWHealth == TriHealthy && obs.ConfigHealth == TriFaulted && dev == DeviceWiFi {
		return StatusWedgedAwaitingBudget
	}

	// Inactive: no profile / no cable / rfkill / AP-impossible.
	if obs.HWHealth == TriInactive || obs.RfkillHard {
		return StatusInactive
	}

	return StatusHealthy
}

// deriveHint picks the most specific owner-actionable hint. At most one
// applies at a time — picks the most specific.
func deriveHint(tick Tick) string {
	if w, ok := tick.Devices[DeviceWiFi]; ok && w.RfkillHard {
		return "physical wifi switch off"
	}
	return ""
}
