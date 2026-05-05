package network

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piccolod/internal/network/nmclient"
)

// rawWiFiObs is the unsmoothed per-tick observation. Dampening is applied by
// the Prober before producing a DeviceObservation.
type rawWiFiObs struct {
	Present       bool
	Iface         string
	NMState       nmclient.NMDeviceState
	NMReason      nmclient.NMDeviceStateReason
	RfkillSoft    bool
	RfkillHard    bool
	HasProfile    bool
	HasIP         bool
	GwReachable   Tri
	ScanFoundBSS  bool
	IsAuthFailure bool // NMReason ∈ deterministic auth-failure set
}

// probeWiFi gathers raw observations for the WiFi device. Returns
// (raw, devicePath, present). When present=false, devicePath is empty and
// raw is zeroed.
func (p *Prober) probeWiFi() rawWiFiObs {
	raw := rawWiFiObs{}

	// rfkill state — read regardless of whether NM has the device, so we
	// can correctly classify A9 (hard kill, NM may have removed device).
	soft, hard := readWiFiRfkill()
	raw.RfkillSoft = soft
	raw.RfkillHard = hard

	devs, err := p.nm.WiFiDevices()
	if err != nil {
		log.Printf("WARN: probe_wifi: list devices: %v", err)
		return raw
	}
	if len(devs) == 0 {
		return raw
	}
	dev := devs[0] // first NM-managed wifi device per A8 single-device rule
	raw.Present = true
	raw.Iface = dev.Interface
	raw.NMState = dev.State

	// Reason resolution — preference order:
	//   1. Signal-cached reason from p.lastReasonByDev (most recent
	//      transition observed at runtime).
	//   2. NM's cached StateReason property — covers the cold-start path
	//      where piccolod restarted into a persistent failure state (e.g.
	//      NoSecrets after a wrong-password attempt) and no signal has
	//      fired since the prober subscribed. Without this, catalog D2
	//      regresses across piccolod restart.
	if cached, ok := p.lastReason(dev.Path); ok {
		raw.NMReason = cached
		raw.IsAuthFailure = isAuthFailure(cached)
	} else if _, reason, err := p.nm.DeviceStateReason(dev.Path); err == nil && reason != nmclient.NMDeviceStateReasonUnknown {
		raw.NMReason = reason
		raw.IsAuthFailure = isAuthFailure(reason)
	}

	// Saved profiles — drives ConfigHealth Inactive vs Faulted. Filter
	// out our own AP hotspot profiles (NM exposes the temporary
	// piccolo-ap-* connection as a saved 802-11-wireless profile while
	// AP mode is active). Without this filter, a first-run device with
	// no real STA profile reports HasProfile=true → ConfigHealth defaults
	// to Healthy → APArbiter exits AP and oscillates instead of keeping
	// the captive portal up.
	profiles, err := p.nm.SavedWiFiConnections()
	if err == nil {
		raw.HasProfile = hasNonHotspotProfile(profiles)
	}

	// Active connection info → IP presence. Skip if the active connection
	// is OUR hotspot — NM exposes the AP-mode hotspot as the WiFi device's
	// active connection with a 10.42.x address, and counting that as a
	// STA IP makes ConfigHealth flip to Healthy → APArbiter exits AP →
	// recovery oscillates with no real STA uplink.
	if info, err := p.nm.ActiveConnectionInfo(dev.Path); err == nil && info != nil && !info.IsHotspot() {
		raw.HasIP = info.IP4Address != ""
	}

	// Scan only when we'd otherwise be ambiguous about HW (e.g. Unavailable
	// for >grace). Scans are 5-10s — too costly to run unconditionally per
	// 30s tick. The Activated path doesn't need a scan; we already know HW
	// works. Stage 1 keeps this conservative — orchestrator can call scan
	// directly when needed.

	p.setDevicePath(DeviceWiFi, dev.Path)
	return raw
}

// resolveWiFi smooths the raw observation through the dampener and quiet
// period, producing the final DeviceObservation. Caller passes ledger so the
// probe can suppress N-of-M during quiet period.
func (p *Prober) resolveWiFi(raw rawWiFiObs, led ActionLedger, sysUptime time.Duration, now time.Time) DeviceObservation {
	obs := DeviceObservation{
		Kind:       DeviceWiFi,
		Present:    raw.Present,
		Iface:      raw.Iface,
		NMState:    raw.NMState.String(),
		NMReason:   raw.NMReason.String(),
		RfkillHard: raw.RfkillHard,
		HasIP:      raw.HasIP,
	}
	if !raw.Present {
		obs.HWHealth = TriInactive
		obs.ConfigHealth = TriInactive
		obs.GwReachable = TriInactive
		return obs
	}
	obs.LinkUp = raw.NMState >= nmclient.NMDeviceStateIPConfig

	// HWHealth classification. Cold-boot grace suppresses all fault
	// classification — the driver may still be loading.
	switch {
	case raw.RfkillHard:
		// Physical kill switch — Inactive; recovery impossible.
		obs.HWHealth = TriInactive
	case raw.RfkillSoft && raw.HasProfile:
		// Soft-block + intent contradicts → Faulted (recoverable via unblock+bounce).
		obs.HWHealth = p.dampenHW(DeviceWiFi, TriFaulted, led, now)
	case raw.RfkillSoft && !raw.HasProfile:
		// Soft-block + no profile → Inactive (no intent to be on).
		obs.HWHealth = TriInactive
	case sysUptime < ColdBootGrace:
		// Cold-boot grace.
		obs.HWHealth = TriHealthy
	case raw.NMState <= nmclient.NMDeviceStateUnavailable:
		obs.HWHealth = p.dampenHW(DeviceWiFi, TriFaulted, led, now)
	default:
		obs.HWHealth = p.dampenHW(DeviceWiFi, TriHealthy, led, now)
	}

	// ConfigHealth classification.
	switch {
	case obs.HWHealth != TriHealthy:
		// HW must be healthy before Config classification is meaningful.
		obs.ConfigHealth = TriInactive
	case raw.NMState == nmclient.NMDeviceStateActivated && raw.HasIP:
		obs.ConfigHealth = TriHealthy
		p.resetConfigFault(DeviceWiFi)
	case raw.IsAuthFailure:
		// Deterministic auth failure — Faulted immediately, no dampening
		// (NM has explicitly told us credentials are wrong; retrying is
		// pointless).
		obs.ConfigHealth = TriFaulted
	case !raw.HasProfile:
		obs.ConfigHealth = TriInactive
		p.resetConfigFault(DeviceWiFi)
	case isTerminalConfigFailure(raw.NMState):
		// Saved profile + NM in Failed/Disconnected past grace = sustained
		// failure (SSID renamed, out of range, BSSID gone). Dampen 3-of-3
		// before flipping to Faulted so a transient drop doesn't
		// prematurely escalate to AP entry. Without this branch, the
		// owner of an SSID-rename device would never see the setup AP
		// because ConfigHealth would stay "DHCP-in-flight Healthy" forever.
		obs.ConfigHealth = p.dampenConfigFault(DeviceWiFi, TriFaulted, led, now)
	default:
		// Activating (Prepare/Config/IPConfig/etc.) — transient DHCP-in-
		// flight maps to Healthy per RFC.
		obs.ConfigHealth = TriHealthy
		p.resetConfigFault(DeviceWiFi)
	}

	return obs
}

// resetConfigFault clears the consecConfigFault counter for kind. Called
// from any branch that classifies Config as Healthy/Inactive so the
// sustained-failure dampener requires fresh consecutive faults before
// promoting again.
func (p *Prober) resetConfigFault(kind DeviceKind) {
	p.mu.Lock()
	p.consecConfigFault[kind] = 0
	p.mu.Unlock()
}

// isTerminalConfigFailure reports whether the NM state represents a
// non-progressing failure (NM has stopped trying or hit a hard wall). NM
// Disconnected (30) and Failed (120) are terminal; Prepare/Config/
// NeedAuth/IPConfig/IPCheck/Secondaries (40-90) are in-flight.
func isTerminalConfigFailure(s nmclient.NMDeviceState) bool {
	return s == nmclient.NMDeviceStateDisconnected || s == nmclient.NMDeviceStateFailed
}

// hasNonHotspotProfile returns true iff at least one of the saved WiFi
// profiles is NOT one of our AP hotspot profiles (HotspotIDPrefix).
func hasNonHotspotProfile(profiles []nmclient.ConnectionProfile) bool {
	for _, p := range profiles {
		if !strings.HasPrefix(p.ID, nmclient.HotspotIDPrefix) {
			return true
		}
	}
	return false
}

// readWiFiRfkill scans /sys/class/rfkill for type=wlan entries and reports
// (anyOnSoft, anyOnHard). Multiple wlan rfkill nodes are OR'd.
//
// Test seam: tests override this to simulate rfkill states.
var readWiFiRfkill = func() (soft, hard bool) {
	entries, err := os.ReadDir("/sys/class/rfkill")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("WARN: probe_wifi: rfkill dir: %v", err)
		}
		return false, false
	}
	for _, e := range entries {
		dir := filepath.Join("/sys/class/rfkill", e.Name())
		t, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(t)) != "wlan" {
			continue
		}
		if v, err := os.ReadFile(filepath.Join(dir, "soft")); err == nil {
			if strings.TrimSpace(string(v)) == "1" {
				soft = true
			}
		}
		if v, err := os.ReadFile(filepath.Join(dir, "hard")); err == nil {
			if strings.TrimSpace(string(v)) == "1" {
				hard = true
			}
		}
	}
	return soft, hard
}

// isAuthFailure returns true for the deterministic auth-failure reason set
// (relocated from manager.go::handleAuthFailure).
//
// SupplicantDisconnect(8) is intentionally omitted — ambiguous between
// wrong-password and transient signal loss.
func isAuthFailure(r nmclient.NMDeviceStateReason) bool {
	switch r {
	case nmclient.NMDeviceStateReasonNoSecrets,
		nmclient.NMDeviceStateReasonSupplicantConfigFailed,
		nmclient.NMDeviceStateReasonSupplicantFailed,
		nmclient.NMDeviceStateReasonSupplicantTimeout:
		return true
	}
	return false
}

