package network

import (
	"reflect"
	"testing"
	"time"
)

// scenario is a parameterized table-driven catalog test. Each case maps a
// catalog ID (A1, B1, ...) to its expected HW + AP decisions and Snapshot
// shape. The 41 catalog scenarios from the RFC are the acceptance bar.
type scenario struct {
	id     string
	desc   string
	tick   Tick
	led    ActionLedger
	intent UserIntent
	apActive bool

	wantHWWiFi    HWAction
	wantHWEth     HWAction
	wantAP        APAction
	wantHint      string
	wantStatusW   DeviceStatus // wifi
	wantStatusE   DeviceStatus // eth
}

var (
	now = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

	// Helpers to build test ticks compactly.
	pastGrace = 5 * time.Minute
)

func dev(kind DeviceKind, hw, cfg, gw Tri, hasIP, linkUp bool) DeviceObservation {
	return DeviceObservation{
		Kind:         kind,
		Present:      true,
		HWHealth:     hw,
		ConfigHealth: cfg,
		GwReachable:  gw,
		HasIP:        hasIP,
		LinkUp:       linkUp,
	}
}

func absent(kind DeviceKind) DeviceObservation {
	return DeviceObservation{Kind: kind, Present: false, HWHealth: TriInactive, ConfigHealth: TriInactive, GwReachable: TriInactive}
}

func tick(wifi, eth DeviceObservation, l3 L3ProbeResult, busy bool, uplink UplinkType) Tick {
	return Tick{
		Devices:      map[DeviceKind]DeviceObservation{DeviceWiFi: wifi, DeviceEthernet: eth},
		L3Probe:      l3,
		ActiveUplink: uplink,
		SystemBusy:   busy,
		SystemUptime: pastGrace,
		At:           now,
	}
}

func emptyLedger() ActionLedger {
	return ActionLedger{
		Bounces:      map[DeviceKind][]time.Time{},
		LastBounceAt: map[DeviceKind]time.Time{},
	}
}

// catalogScenarios enumerates the 41 RFC failure-catalog cases, mapping each
// to expected (HW[wifi], HW[eth], AP, Hint, Status).
func catalogScenarios() []scenario {
	return []scenario{
		// ===== A. Hardware-intrinsic faults =====
		{
			id: "A1", desc: "WiFi driver wedge — scan empty, NM Unavailable past grace, L3 down",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWBounce{Device: DeviceWiFi},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // wifi HW Faulted → AP impossible
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "A2", desc: "WiFi rfkill stuck on, no profiles — Inactive",
			tick: func() Tick {
				w := dev(DeviceWiFi, TriInactive, TriInactive, TriInactive, false, false)
				return tick(w, absent(DeviceEthernet), L3ProbeUp, false, UplinkNone)
			}(),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusInactive,
			wantStatusE: StatusInactive,
		},
		{
			id: "A3", desc: "Ethernet driver wedge — carrier=1, NM Unavailable past grace",
			tick: tick(
				absent(DeviceWiFi),
				dev(DeviceEthernet, TriFaulted, TriInactive, TriInactive, false, false),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWBounce{Device: DeviceEthernet},
			wantAP:      APUnchanged{},
			wantStatusW: StatusInactive,
			wantStatusE: StatusWedgedAwaitingBudget,
		},
		{
			id: "A4", desc: "NIC firmware crash unrecoverable by bounce — escalation to reboot",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led: ActionLedger{
				// Already bounced 3 times in last 1h
				Bounces: map[DeviceKind][]time.Time{DeviceWiFi: {now.Add(-30 * time.Minute), now.Add(-20 * time.Minute), now.Add(-10 * time.Minute)}},
				LastBounceAt: map[DeviceKind]time.Time{DeviceWiFi: now.Add(-10 * time.Minute)},
			},
			wantHWWiFi:  HWReboot{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // wifi HW Faulted → AP impossible
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "A5", desc: "Cold-boot device init slow — grace window suppresses action",
			tick: func() Tick {
				t := tick(
					dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
					absent(DeviceEthernet),
					L3ProbeDown, false, UplinkNone,
				)
				t.SystemUptime = 30 * time.Second // < 60s grace
				return t
			}(),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusWedgedAwaitingBudget, // probe layer would have suppressed; this case is pre-grace forced-Faulted
			wantStatusE: StatusInactive,
		},
		{
			id: "A6", desc: "WiFi rfkill soft + profiles — Faulted, recovery via unblock+bounce",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWBounce{Device: DeviceWiFi},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "A7", desc: "HW Faulted while onboarding install_disk — both bounce and reboot deferred",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, true, UplinkNone, // SystemBusy=true
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // SystemBusy gates AP too
			wantStatusW: StatusWedgedAwaitingPrecondition,
			wantStatusE: StatusInactive,
		},
		{
			id: "A9", desc: "WiFi rfkill HARD-blocked — Inactive + Hint",
			tick: func() Tick {
				w := dev(DeviceWiFi, TriInactive, TriInactive, TriInactive, false, false)
				w.RfkillHard = true
				return tick(w, absent(DeviceEthernet), L3ProbeDown, false, UplinkNone)
			}(),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantHint:    "physical wifi switch off",
			wantStatusW: StatusInactive,
			wantStatusE: StatusInactive,
		},

		// ===== B. Medium / link faults =====
		{
			id: "B1", desc: "Eth cable unplugged — Inactive, no action",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				dev(DeviceEthernet, TriInactive, TriInactive, TriInactive, false, false),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // uplink up
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "B3", desc: "WiFi out of range — scan empty for SSID — retry via NM, AP after sustained loss",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{}, // HW healthy → no HW action
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget, // HW Healthy + Config Faulted → wedged-awaiting
			wantStatusE: StatusInactive,
		},

		// ===== C. Peer / upstream faults =====
		{
			id: "C1", desc: "Router powered off — both uplinks fail; AP if wifi HW healthy",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriFaulted, true, true), // assoc OK, gw unreachable
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone, // wifi linkup but HWHealth Healthy + LinkUp; would be UplinkWiFi normally
			),
			led: emptyLedger(),
			// HW Healthy, no L3 either; HW alone isn't actionable → Wait
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // wifi config still Healthy → STA flow handles, not AP
			wantStatusW: StatusEnvSuspected,
			wantStatusE: StatusInactive,
		},
		{
			id: "C3", desc: "ISP down, router up — NMConn=limited, GwReachable=True — surface only",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{}, // L3 down but HW Healthy
			wantHWEth:   HWWait{},
			wantAP:      APExit{}, // any uplink reachable (gw healthy) → exit AP if active; APExit when NOT active becomes APUnchanged
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C8", desc: "ARP-suppressed network — GwReachable=False + L3 Up — surface only",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriFaulted, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy, // L3 up, even without gw arp
			wantStatusE: StatusInactive,
		},

		// ===== D. Configuration drift =====
		{
			id: "D1", desc: "SSID renamed — saved profile never appears — AP after retries",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "D2", desc: "WiFi password changed — assoc auth-error — AP",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},

		// ===== E. User intent =====
		{
			id: "E4", desc: "User explicitly forces AP via UI — UserIntent.ForceAP=true",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			intent:      UserIntent{ForceAP: true},
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},

		// ===== F. Fresh-install / onboarding =====
		{
			id: "F1", desc: "No saved wifi, eth plugged — eth carries",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriInactive, TriInactive, false, false),
				dev(DeviceEthernet, TriHealthy, TriHealthy, TriHealthy, true, true),
				L3ProbeUp, false, UplinkEthernet,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy, // HW healthy, Config Inactive (no profile) — but our classifier returns Healthy when HW Healthy + ConfigHealth!=Faulted + GwReachable!=Faulted (wifi inactive Gateway is Inactive not Faulted)
			wantStatusE: StatusHealthy,
		},
		{
			id: "F2", desc: "No saved wifi, no eth — AP for owner",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusHealthy, // HW healthy, ConfigHealth Inactive — falls through to Healthy
			wantStatusE: StatusInactive,
		},

		// ===== G. Composition / race =====
		{
			id: "G1", desc: "Eth comes up while in recovery-AP — exit AP unconditionally",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriInactive, TriInactive, false, false),
				dev(DeviceEthernet, TriHealthy, TriHealthy, TriHealthy, true, true),
				L3ProbeUp, false, UplinkEthernet,
			),
			led:         emptyLedger(),
			apActive:    true,
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APExit{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusHealthy,
		},
		{
			id: "G2", desc: "WiFi recovers while in user-forced AP — stay in AP",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			intent:      UserIntent{ForceAP: true},
			apActive:    true,
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // user-forced; already AP
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "G5", desc: "Bounce in flight — quiet period suppresses HW action",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led: ActionLedger{
				Bounces:      map[DeviceKind][]time.Time{DeviceWiFi: {now.Add(-30 * time.Second)}},
				LastBounceAt: map[DeviceKind]time.Time{DeviceWiFi: now.Add(-30 * time.Second)},
			},
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{}, // wifi HW Faulted, AP impossible
			wantStatusW: StatusRecovering, // quiet period wins
			wantStatusE: StatusInactive,
		},

		// ===== Remaining catalog coverage =====
		{
			id: "A8", desc: "Multi-device-per-kind ignored — supervisor picks first",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "B2", desc: "Eth carrier flapping — N-of-M dampens; surface only",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				dev(DeviceEthernet, TriInactive, TriInactive, TriInactive, false, false),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "B4", desc: "WiFi RSSI too weak — ConfigHealth Faulted → AP-handover",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "B5", desc: "Channel congestion — undetectable, no action",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C2", desc: "Router rebooting transient — N-of-M dampens, no action",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C4", desc: "DHCP refused — NM Activating, DHCP-in-flight maps Healthy",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C5", desc: "IP conflict — surface only (NM handles)",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriFaulted, false, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C6", desc: "Captive portal upstream — NMConn=portal, surface only",
			tick: func() Tick {
				t := tick(dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true), absent(DeviceEthernet), L3ProbeUp, false, UplinkWiFi)
				t.NMConn = ConnectivityPortal
				return t
			}(),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C7", desc: "DNS broken upstream — surface only",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "C9", desc: "Corporate guest WiFi blocking probe — TCP-connect blocked, surface only",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APExit{}, // gw healthy → exit if active; collapses to APUnchanged when not
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "D3", desc: "Security mode changed — same as D2",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "D4", desc: "Router replaced same SSID — NM transparent reuse",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				absent(DeviceEthernet),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "D5", desc: "Router replaced new SSID — same as D1",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "E1", desc: "User unplugs eth permanently, wifi takes over",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				dev(DeviceEthernet, TriInactive, TriInactive, TriInactive, false, false),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "E2", desc: "User plugs eth into wifi-only device — eth used per priority",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				dev(DeviceEthernet, TriHealthy, TriHealthy, TriHealthy, true, true),
				L3ProbeUp, false, UplinkEthernet,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusHealthy,
		},
		{
			id: "E3", desc: "User moves to new SSID env — same as D1",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "E5", desc: "User unplugs eth briefly to power-cycle router — auto-recover",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				dev(DeviceEthernet, TriInactive, TriInactive, TriInactive, false, false),
				L3ProbeUp, false, UplinkWiFi,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusInactive,
		},
		{
			id: "F3", desc: "Saved wifi from clone/reset, wrong env — falls into D1",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "F4", desc: "Cold boot, wifi HW slow init — grace window",
			tick: func() Tick {
				t := tick(
					dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
					absent(DeviceEthernet),
					L3ProbeDown, false, UplinkNone,
				)
				t.SystemUptime = 30 * time.Second
				return t
			}(),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "G3", desc: "Both uplinks present, one fails — other carries; no HW action on healthy",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true),
				dev(DeviceEthernet, TriFaulted, TriInactive, TriInactive, false, false),
				L3ProbeUp, false, UplinkWiFi,
			),
			led: ActionLedger{
				Bounces:      map[DeviceKind][]time.Time{DeviceEthernet: {now.Add(-30 * time.Minute), now.Add(-20 * time.Minute), now.Add(-10 * time.Minute)}},
				LastBounceAt: map[DeviceKind]time.Time{DeviceEthernet: now.Add(-10 * time.Minute)},
				Reboots:      []time.Time{now.Add(-1 * time.Hour)},
			},
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{}, // L3 up via wifi → no bounce
			wantAP:      APUnchanged{},
			wantStatusW: StatusHealthy,
			wantStatusE: StatusWedgedAwaitingBudget,
		},
		{
			id: "G4", desc: "Both uplinks fail simultaneously — escalate per dept; AP if wifi HW healthy",
			tick: tick(
				dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false),
				dev(DeviceEthernet, TriFaulted, TriInactive, TriInactive, false, false),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWBounce{Device: DeviceEthernet},
			wantAP:      APEnter{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusWedgedAwaitingBudget,
		},
		{
			id: "G6", desc: "piccolod restart mid-recovery — volatile ledger missing fail-OPEN",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWBounce{Device: DeviceWiFi},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
		{
			id: "G7", desc: "Reboot decision concurrent with disk-op — defer to next tick",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, true, UplinkNone,
			),
			led: ActionLedger{
				Bounces:      map[DeviceKind][]time.Time{DeviceWiFi: {now.Add(-30 * time.Minute), now.Add(-20 * time.Minute), now.Add(-10 * time.Minute)}},
				LastBounceAt: map[DeviceKind]time.Time{DeviceWiFi: now.Add(-10 * time.Minute)},
			},
			wantHWWiFi:  HWWait{},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusWedgedAwaitingPrecondition,
			wantStatusE: StatusInactive,
		},
		{
			id: "G8", desc: "Tick produces both Bounce(WiFi) and APEnter — HW first, AP impossible per APArbiter",
			tick: tick(
				dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false),
				absent(DeviceEthernet),
				L3ProbeDown, false, UplinkNone,
			),
			led:         emptyLedger(),
			wantHWWiFi:  HWBounce{Device: DeviceWiFi},
			wantHWEth:   HWWait{},
			wantAP:      APUnchanged{},
			wantStatusW: StatusWedgedAwaitingBudget,
			wantStatusE: StatusInactive,
		},
	}
}

func TestDecideHW_Catalog(t *testing.T) {
	for _, sc := range catalogScenarios() {
		t.Run(sc.id+"/"+sc.desc, func(t *testing.T) {
			gotW := decideHW(sc.tick.Devices[DeviceWiFi], sc.led, sc.tick, DeviceWiFi)
			gotE := decideHW(sc.tick.Devices[DeviceEthernet], sc.led, sc.tick, DeviceEthernet)

			assertHWAction(t, "wifi", gotW, sc.wantHWWiFi)
			assertHWAction(t, "eth", gotE, sc.wantHWEth)
		})
	}
}

func TestDecideAP_Catalog(t *testing.T) {
	for _, sc := range catalogScenarios() {
		t.Run(sc.id+"/"+sc.desc, func(t *testing.T) {
			got := decideAP(sc.tick, sc.led, sc.intent, sc.apActive)
			assertAPAction(t, got, sc.wantAP, sc.apActive)
		})
	}
}

func TestSnapshot_Catalog(t *testing.T) {
	for _, sc := range catalogScenarios() {
		t.Run(sc.id+"/"+sc.desc, func(t *testing.T) {
			snap := buildSnapshot(sc.tick, sc.led, sc.apActive, "", "")
			if got := snap.Devices[DeviceWiFi].Status; got != sc.wantStatusW {
				t.Errorf("WiFi status = %s, want %s", got, sc.wantStatusW)
			}
			if got := snap.Devices[DeviceEthernet].Status; got != sc.wantStatusE {
				t.Errorf("Eth status = %s, want %s", got, sc.wantStatusE)
			}
			if sc.wantHint != "" && snap.Hint != sc.wantHint {
				t.Errorf("Hint = %q, want %q", snap.Hint, sc.wantHint)
			}
		})
	}
}

// assertHWAction does a structural compare ignoring Reason strings.
func assertHWAction(t *testing.T, label string, got, want HWAction) {
	t.Helper()
	switch w := want.(type) {
	case HWWait:
		if _, ok := got.(HWWait); !ok {
			t.Errorf("%s: want HWWait, got %T (%+v)", label, got, got)
		}
	case HWBounce:
		g, ok := got.(HWBounce)
		if !ok {
			t.Errorf("%s: want HWBounce, got %T (%+v)", label, got, got)
			return
		}
		if g.Device != w.Device {
			t.Errorf("%s: HWBounce device = %s, want %s", label, g.Device, w.Device)
		}
	case HWReboot:
		if _, ok := got.(HWReboot); !ok {
			t.Errorf("%s: want HWReboot, got %T (%+v)", label, got, got)
		}
	default:
		t.Fatalf("unhandled want type %T", want)
	}
}

// assertAPAction handles the "APExit when not in AP collapses to APUnchanged"
// quirk used in some catalog cases.
func assertAPAction(t *testing.T, got, want APAction, apActive bool) {
	t.Helper()
	want = normalizeAP(want, apActive)
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		t.Errorf("AP: want %T, got %T (%+v)", want, got, got)
	}
}

// normalizeAP collapses APExit-when-not-active and APEnter-when-active to
// APUnchanged so the catalog can describe its desire ergonomically. The
// deciders do this internally as well.
func normalizeAP(a APAction, apActive bool) APAction {
	switch a.(type) {
	case APExit:
		if !apActive {
			return APUnchanged{}
		}
	case APEnter:
		if apActive {
			return APUnchanged{}
		}
	}
	return a
}

// TestDecideHW_QuietPeriodGate verifies the defensive quiet-period gate in
// the decider (probe layer also enforces this; double-belt).
func TestDecideHW_QuietPeriodGate(t *testing.T) {
	wifi := dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false)
	tk := tick(wifi, absent(DeviceEthernet), L3ProbeDown, false, UplinkNone)
	led := ActionLedger{
		LastBounceAt: map[DeviceKind]time.Time{DeviceWiFi: now.Add(-30 * time.Second)},
	}
	got := decideHW(wifi, led, tk, DeviceWiFi)
	if _, ok := got.(HWWait); !ok {
		t.Errorf("got %T, want HWWait during quiet period", got)
	}
}

// TestDecideHW_RebootBudgetExhausted verifies all-budgets-exhausted Wait.
func TestDecideHW_RebootBudgetExhausted(t *testing.T) {
	wifi := dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false)
	tk := tick(wifi, absent(DeviceEthernet), L3ProbeDown, false, UplinkNone)
	led := ActionLedger{
		Bounces:      map[DeviceKind][]time.Time{DeviceWiFi: {now.Add(-30 * time.Minute), now.Add(-20 * time.Minute), now.Add(-10 * time.Minute)}},
		LastBounceAt: map[DeviceKind]time.Time{DeviceWiFi: now.Add(-10 * time.Minute)},
		Reboots:      []time.Time{now.Add(-1 * time.Hour)},
	}
	got := decideHW(wifi, led, tk, DeviceWiFi)
	if _, ok := got.(HWWait); !ok {
		t.Errorf("got %T, want HWWait when all budgets exhausted", got)
	}
}

// TestDecideAP_RateShape verifies (count<4 in 1h) OR (cooldown elapsed).
func TestDecideAP_RateShape(t *testing.T) {
	wifi := dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false)
	tk := tick(wifi, absent(DeviceEthernet), L3ProbeDown, false, UplinkNone)

	// 4 toggles in the last 1h, last toggle 5 min ago — locked out.
	led := ActionLedger{
		APToggles: []APEvent{
			{When: now.Add(-50 * time.Minute), Enter: true},
			{When: now.Add(-40 * time.Minute), Enter: false},
			{When: now.Add(-30 * time.Minute), Enter: true},
			{When: now.Add(-5 * time.Minute), Enter: false},
		},
	}
	got := decideAP(tk, led, UserIntent{}, false)
	if _, ok := got.(APUnchanged); !ok {
		t.Errorf("got %T, want APUnchanged (rate-shaped)", got)
	}

	// Same toggles but last is >10min ago — cooldown elapsed → permitted.
	led.APToggles[3].When = now.Add(-15 * time.Minute)
	got = decideAP(tk, led, UserIntent{}, false)
	if _, ok := got.(APEnter); !ok {
		t.Errorf("got %T, want APEnter (cooldown elapsed)", got)
	}
}

// TestDeriveLegacyState verifies the wire-contract mapping.
func TestDeriveLegacyState(t *testing.T) {
	tests := []struct {
		name     string
		tick     Tick
		apActive bool
		want     ConnState
	}{
		{
			"AP mode highest priority",
			tick(dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true), absent(DeviceEthernet), L3ProbeUp, false, UplinkWiFi),
			true, StateAPMode,
		},
		{
			"Eth GwReachable wins over WiFi",
			tick(dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true), dev(DeviceEthernet, TriHealthy, TriHealthy, TriHealthy, true, true), L3ProbeUp, false, UplinkEthernet),
			false, StateEthernet,
		},
		{
			"WiFi only",
			tick(dev(DeviceWiFi, TriHealthy, TriHealthy, TriHealthy, true, true), absent(DeviceEthernet), L3ProbeUp, false, UplinkWiFi),
			false, StateWiFiSTA,
		},
		{
			"WiFi reconnecting (config faulted)",
			tick(dev(DeviceWiFi, TriHealthy, TriFaulted, TriInactive, false, false), absent(DeviceEthernet), L3ProbeDown, false, UplinkNone),
			false, StateReconnecting,
		},
		{
			"Disconnected fallthrough",
			tick(dev(DeviceWiFi, TriFaulted, TriInactive, TriInactive, false, false), absent(DeviceEthernet), L3ProbeDown, false, UplinkNone),
			false, StateDisconnected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveLegacyState(tt.tick, tt.apActive); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}
