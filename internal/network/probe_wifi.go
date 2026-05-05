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

	// Last-known state-change reason from the signal cache (populated by the
	// signal goroutine started in Prober.Run).
	if cached, ok := p.lastReason(dev.Path); ok {
		raw.NMReason = cached
		raw.IsAuthFailure = isAuthFailure(cached)
	}

	// Saved profiles — drives ConfigHealth Inactive vs Faulted.
	profiles, err := p.nm.SavedWiFiConnections()
	if err == nil {
		raw.HasProfile = len(profiles) > 0
	}

	// Active connection info → IP presence.
	if info, err := p.nm.ActiveConnectionInfo(dev.Path); err == nil && info != nil {
		raw.HasIP = info.IP4Address != ""
	}

	// Scan only when we'd otherwise be ambiguous about HW (e.g. Unavailable
	// for >grace). Scans are 5-10s — too costly to run unconditionally per
	// 30s tick. The Activated path doesn't need a scan; we already know HW
	// works. Stage 1 keeps this conservative — orchestrator can call scan
	// directly when needed.

	p.devicePath[DeviceWiFi] = dev.Path
	return raw
}

// resolveWiFi smooths the raw observation through the dampener and quiet
// period, producing the final DeviceObservation. Caller passes ledger so the
// probe can suppress N-of-M during quiet period.
func (p *Prober) resolveWiFi(raw rawWiFiObs, led ActionLedger, sysUptime, quietPeriod, grace time.Duration, now time.Time) DeviceObservation {
	obs := DeviceObservation{
		Kind:       DeviceWiFi,
		Present:    raw.Present,
		Iface:      raw.Iface,
		NMState:    nmStateString(raw.NMState),
		NMReason:   nmReasonString(raw.NMReason),
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
		obs.HWHealth = p.dampenHW(DeviceWiFi, TriFaulted, led, quietPeriod, now)
	case raw.RfkillSoft && !raw.HasProfile:
		// Soft-block + no profile → Inactive (no intent to be on).
		obs.HWHealth = TriInactive
	case sysUptime < grace:
		// Cold-boot grace.
		obs.HWHealth = TriHealthy
	case raw.NMState <= nmclient.NMDeviceStateUnavailable:
		obs.HWHealth = p.dampenHW(DeviceWiFi, TriFaulted, led, quietPeriod, now)
	default:
		obs.HWHealth = p.dampenHW(DeviceWiFi, TriHealthy, led, quietPeriod, now)
	}

	// ConfigHealth classification.
	switch {
	case obs.HWHealth != TriHealthy:
		// HW must be healthy before Config classification is meaningful.
		obs.ConfigHealth = TriInactive
	case raw.NMState == nmclient.NMDeviceStateActivated && raw.HasIP:
		obs.ConfigHealth = TriHealthy
	case raw.IsAuthFailure:
		// Deterministic auth failure — Faulted, AP-handover candidate.
		obs.ConfigHealth = TriFaulted
	case !raw.HasProfile:
		obs.ConfigHealth = TriInactive
	default:
		// Activating, transient — DHCP-in-flight maps to Healthy per RFC.
		obs.ConfigHealth = TriHealthy
	}

	return obs
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

func nmStateString(s nmclient.NMDeviceState) string {
	switch s {
	case nmclient.NMDeviceStateUnknown:
		return "unknown"
	case nmclient.NMDeviceStateUnmanaged:
		return "unmanaged"
	case nmclient.NMDeviceStateUnavailable:
		return "unavailable"
	case nmclient.NMDeviceStateDisconnected:
		return "disconnected"
	case nmclient.NMDeviceStatePrepare:
		return "prepare"
	case nmclient.NMDeviceStateConfig:
		return "config"
	case nmclient.NMDeviceStateNeedAuth:
		return "need_auth"
	case nmclient.NMDeviceStateIPConfig:
		return "ip_config"
	case nmclient.NMDeviceStateIPCheck:
		return "ip_check"
	case nmclient.NMDeviceStateSecondaries:
		return "secondaries"
	case nmclient.NMDeviceStateActivated:
		return "activated"
	case nmclient.NMDeviceStateDeactivating:
		return "deactivating"
	case nmclient.NMDeviceStateFailed:
		return "failed"
	default:
		return "?"
	}
}

func nmReasonString(r nmclient.NMDeviceStateReason) string {
	switch r {
	case nmclient.NMDeviceStateReasonNone:
		return "none"
	case nmclient.NMDeviceStateReasonNoSecrets:
		return "no_secrets"
	case nmclient.NMDeviceStateReasonSupplicantConfigFailed:
		return "supplicant_config_failed"
	case nmclient.NMDeviceStateReasonSupplicantFailed:
		return "supplicant_failed"
	case nmclient.NMDeviceStateReasonSupplicantTimeout:
		return "supplicant_timeout"
	case nmclient.NMDeviceStateReasonSupplicantDisconnect:
		return "supplicant_disconnect"
	case nmclient.NMDeviceStateReasonDHCPFailed:
		return "dhcp_failed"
	case nmclient.NMDeviceStateReasonDHCPError:
		return "dhcp_error"
	case nmclient.NMDeviceStateReasonCarrier:
		return "carrier"
	case nmclient.NMDeviceStateReasonUserRequested:
		return "user_requested"
	default:
		return "other"
	}
}
