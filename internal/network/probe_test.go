package network

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
)

// stubRunner is a runner.CommandRunner for tests. Records invocations and
// returns configurable per-command errors keyed by the command name.
type stubRunner struct {
	mu     sync.Mutex
	errs   map[string]error
	calls  []string
	output []byte
}

func newStubRunner() *stubRunner {
	return &stubRunner{errs: make(map[string]error)}
}
func (s *stubRunner) Run(_ context.Context, name string, _ ...string) error {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	err := s.errs[name]
	s.mu.Unlock()
	return err
}
func (s *stubRunner) RunWithOutput(_ context.Context, name string, _ ...string) ([]byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	err := s.errs[name]
	out := s.output
	s.mu.Unlock()
	return out, err
}
func (s *stubRunner) RunWithStdin(_ context.Context, _ []byte, name string, _ ...string) error {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	err := s.errs[name]
	s.mu.Unlock()
	return err
}

var _ runner.CommandRunner = (*stubRunner)(nil)

// scenarioFixture configures all the test seams + stub client for one
// scenario. Tests instantiate one, configure it, and call probe().
type scenarioFixture struct {
	t            *testing.T
	nm           *nmclient.StubClient
	run          *stubRunner
	prober       *Prober
	rfkillSoft   bool
	rfkillHard   bool
	tcpL3Up      bool
	systemUptime time.Duration
	now          time.Time
}

func newFixture(t *testing.T) *scenarioFixture {
	t.Helper()
	nm := nmclient.NewStubClient()
	r := newStubRunner()
	sys := NewStaticSystemState(false, "")
	prober := NewProber(nm, r, sys)

	f := &scenarioFixture{
		t:            t,
		nm:           nm,
		run:          r,
		prober:       prober,
		systemUptime: 5 * time.Minute, // past cold-boot grace
		now:          time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC),
		tcpL3Up:      true,
	}

	// Override package-level seams. Restored via t.Cleanup.
	prevUptime := readSystemUptime
	prevRfkill := readWiFiRfkill
	prevTCP := tcpConnectAny

	readSystemUptime = func() time.Duration { return f.systemUptime }
	readWiFiRfkill = func() (bool, bool) { return f.rfkillSoft, f.rfkillHard }
	tcpConnectAny = func(_ context.Context, _ []string, _ time.Duration) bool { return f.tcpL3Up }

	t.Cleanup(func() {
		readSystemUptime = prevUptime
		readWiFiRfkill = prevRfkill
		tcpConnectAny = prevTCP
	})

	return f
}

// probeN runs the probe N consecutive times to advance the dampener
// counters. Returns the final Tick.
func (f *scenarioFixture) probeN(n int) Tick {
	var tick Tick
	led := ActionLedger{
		Bounces:      map[DeviceKind][]time.Time{},
		LastBounceAt: map[DeviceKind]time.Time{},
	}
	for i := 0; i < n; i++ {
		tick = f.prober.Probe(context.Background(), led, f.now.Add(time.Duration(i)*TickInterval))
	}
	return tick
}

// helpers to populate stub responses.

func (f *scenarioFixture) wifiAt(state nmclient.NMDeviceState, hasIP bool, gateway string) dbus.ObjectPath {
	path := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/0")
	f.nm.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      path,
		Interface: "wlan0",
		State:     state,
	}}
	// Single device-state stub, sufficient for these scenarios. Real DBus
	// would dispatch by path; the stub's DeviceStateResult applies to all
	// queries.
	f.nm.DeviceStateResult = state
	if hasIP || gateway != "" {
		f.nm.ActiveConnResult = &nmclient.ActiveConnectionInfo{
			Path:       "/org/freedesktop/NetworkManager/ActiveConnection/0",
			IP4Address: "192.168.1.42",
			IP4Gateway: gateway,
		}
	}
	return path
}

func (f *scenarioFixture) ethAt(carrier bool, state nmclient.NMDeviceState, hasIP bool, gateway string) dbus.ObjectPath {
	path := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/1")
	f.nm.EthernetDevicesResult = []nmclient.EthernetDevice{{
		Path:      path,
		Interface: "eth0",
		Carrier:   carrier,
		State:     state,
	}}
	// DeviceStateResult is shared; the scenarios that use Ethernet do not
	// share-conflict with WiFi (probes call DeviceState individually but
	// the stub returns the same value).
	if hasIP || gateway != "" {
		f.nm.ActiveConnResult = &nmclient.ActiveConnectionInfo{
			Path:       "/org/freedesktop/NetworkManager/ActiveConnection/1",
			IP4Address: "10.0.0.42",
			IP4Gateway: gateway,
		}
	}
	return path
}

// ----- Catalog scenarios -----

// A1: WiFi driver wedge — NM Unavailable past grace.
//
// Expected after 3 ticks: HWHealth=Faulted, ActiveUplink=none.
func TestProbe_A1_WiFiDriverWedge(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateUnavailable, false, "")

	// Tick 1: candidate=Faulted, counter=1 → reported as Healthy
	t1 := f.prober.Probe(context.Background(), ActionLedger{Bounces: map[DeviceKind][]time.Time{}, LastBounceAt: map[DeviceKind]time.Time{}}, f.now)
	if got := t1.Devices[DeviceWiFi].HWHealth; got != TriHealthy {
		t.Fatalf("tick 1: HWHealth = %s, want healthy (dampener at 1/3)", got)
	}

	// Tick 3: counter=3 → Faulted surfaces
	tick := f.probeN(3)
	if got := tick.Devices[DeviceWiFi].HWHealth; got != TriFaulted {
		t.Errorf("after 3 ticks: HWHealth = %s, want faulted", got)
	}
	if got := tick.ActiveUplink; got != UplinkNone {
		t.Errorf("ActiveUplink = %s, want none", got)
	}
}

// B1: Eth cable unplugged — Inactive (no dampening for Inactive).
func TestProbe_B1_EthCableUnplugged(t *testing.T) {
	f := newFixture(t)
	f.ethAt(false, nmclient.NMDeviceStateUnavailable, false, "")
	f.wifiAt(nmclient.NMDeviceStateActivated, true, "192.168.1.1")

	tick := f.probeN(1)
	if got := tick.Devices[DeviceEthernet].HWHealth; got != TriInactive {
		t.Errorf("HWHealth = %s, want inactive", got)
	}
}

// C3: ISP down, gateway up — L3 down, GwReachable=Healthy.
//
// HW + Config Healthy, but L3 probe reports Down after 3-of-3 dampening.
// Surface only — no action expected from deciders (Stage 2 verifies that).
func TestProbe_C3_ISPDown(t *testing.T) {
	f := newFixture(t)
	f.tcpL3Up = false
	f.wifiAt(nmclient.NMDeviceStateActivated, true, "192.168.1.1")
	f.run.errs["arping"] = nil // arping succeeds → GwReachable=Healthy

	tick := f.probeN(3)
	if got := tick.L3Probe; got != L3ProbeDown {
		t.Errorf("L3Probe = %s, want down (3-of-3)", got)
	}
	if got := tick.Devices[DeviceWiFi].GwReachable; got != TriHealthy {
		t.Errorf("WiFi GwReachable = %s, want healthy", got)
	}
	if got := tick.Devices[DeviceWiFi].HWHealth; got != TriHealthy {
		t.Errorf("WiFi HWHealth = %s, want healthy", got)
	}
	if got := tick.ActiveUplink; got != UplinkWiFi {
		t.Errorf("ActiveUplink = %s, want wifi", got)
	}
}

// D2: WiFi password changed — auth-error from signal.
//
// HW Healthy (NM Disconnected, not Unavailable), ConfigHealth=Faulted via
// the auth-failure signal cache. AP-handover candidate.
func TestProbe_D2_WiFiPasswordChanged(t *testing.T) {
	f := newFixture(t)
	wifiPath := f.wifiAt(nmclient.NMDeviceStateDisconnected, false, "")
	f.nm.SavedConnections = []nmclient.ConnectionProfile{{Path: "/c0", SSID: "home"}}

	// Inject the cached reason directly (bypass signal goroutine for unit-
	// test determinism).
	f.prober.lastReasonByDev[wifiPath] = nmclient.NMDeviceStateReasonNoSecrets

	tick := f.probeN(1)
	wifi := tick.Devices[DeviceWiFi]
	if wifi.HWHealth != TriHealthy {
		t.Errorf("HWHealth = %s, want healthy", wifi.HWHealth)
	}
	if wifi.ConfigHealth != TriFaulted {
		t.Errorf("ConfigHealth = %s, want faulted (auth-error)", wifi.ConfigHealth)
	}
}

// F2: No saved wifi, no eth — wifi HW Healthy, Config Inactive.
//
// AP arbiter (Stage 2) will return APEnter. Stage 1 only verifies the
// observation shape.
func TestProbe_F2_NoSavedWiFiNoEth(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateDisconnected, false, "")
	f.nm.SavedConnections = nil
	// No ethernet device — empty result.

	tick := f.probeN(1)
	wifi := tick.Devices[DeviceWiFi]
	if wifi.HWHealth != TriHealthy {
		t.Errorf("WiFi HWHealth = %s, want healthy", wifi.HWHealth)
	}
	if wifi.ConfigHealth != TriInactive {
		t.Errorf("WiFi ConfigHealth = %s, want inactive (no profile)", wifi.ConfigHealth)
	}
	if got := tick.Devices[DeviceEthernet].HWHealth; got != TriInactive {
		t.Errorf("Eth HWHealth = %s, want inactive (no device)", got)
	}
	if got := tick.ActiveUplink; got != UplinkNone {
		t.Errorf("ActiveUplink = %s, want none", got)
	}
}

// G1: Eth comes up while in recovery-AP — ActiveUplink switches to Eth.
//
// Stage 1 only verifies the observation: when eth probes Healthy + LinkUp,
// ActiveUplink prioritizes ethernet over wifi. Stage 2's APArbiter then
// returns APExit unconditionally.
func TestProbe_G1_EthComesUpDuringAP(t *testing.T) {
	f := newFixture(t)
	f.ethAt(true, nmclient.NMDeviceStateActivated, true, "10.0.0.1")
	f.wifiAt(nmclient.NMDeviceStateDisconnected, false, "")

	tick := f.probeN(1)
	if got := tick.Devices[DeviceEthernet].HWHealth; got != TriHealthy {
		t.Errorf("Eth HWHealth = %s, want healthy", got)
	}
	if got := tick.Devices[DeviceEthernet].LinkUp; !got {
		t.Errorf("Eth LinkUp = false, want true")
	}
	if got := tick.ActiveUplink; got != UplinkEthernet {
		t.Errorf("ActiveUplink = %s, want ethernet (priority)", got)
	}
}

// ----- Dampener-specific tests -----

// QuietPeriodSuppresses verifies that observations within the post-bounce
// quiet period do not advance the dampener counter.
func TestProbe_QuietPeriodSuppresses(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateUnavailable, false, "")

	// Ledger says we just bounced wifi 30s ago — quiet period (90s) active.
	led := ActionLedger{
		Bounces:      map[DeviceKind][]time.Time{DeviceWiFi: {f.now.Add(-30 * time.Second)}},
		LastBounceAt: map[DeviceKind]time.Time{DeviceWiFi: f.now.Add(-30 * time.Second)},
	}

	for i := 0; i < 5; i++ {
		tick := f.prober.Probe(context.Background(), led, f.now.Add(time.Duration(i)*time.Second))
		if got := tick.Devices[DeviceWiFi].HWHealth; got != TriHealthy {
			t.Fatalf("tick %d during quiet period: HWHealth = %s, want healthy (suppressed)", i, got)
		}
	}
}

// ColdBootGraceSuppresses verifies that NM Unavailable during cold-boot
// grace does not flip HWHealth to Faulted, even after 3 ticks.
func TestProbe_ColdBootGraceSuppresses(t *testing.T) {
	f := newFixture(t)
	f.systemUptime = 30 * time.Second // < 60s cold-boot grace
	f.wifiAt(nmclient.NMDeviceStateUnavailable, false, "")

	tick := f.probeN(5)
	if got := tick.Devices[DeviceWiFi].HWHealth; got != TriHealthy {
		t.Errorf("HWHealth = %s during cold-boot grace, want healthy", got)
	}
}

// RfkillHard maps to Inactive (catalog A9).
func TestProbe_A9_RfkillHard(t *testing.T) {
	f := newFixture(t)
	f.rfkillHard = true
	f.wifiAt(nmclient.NMDeviceStateUnavailable, false, "")

	tick := f.probeN(1)
	wifi := tick.Devices[DeviceWiFi]
	if wifi.HWHealth != TriInactive {
		t.Errorf("HWHealth = %s, want inactive (hard rfkill)", wifi.HWHealth)
	}
	if !wifi.RfkillHard {
		t.Errorf("RfkillHard = false, want true")
	}
}

// RfkillSoft + profile maps to Faulted (catalog A6) after dampening.
func TestProbe_A6_RfkillSoftWithProfile(t *testing.T) {
	f := newFixture(t)
	f.rfkillSoft = true
	f.wifiAt(nmclient.NMDeviceStateUnavailable, false, "")
	f.nm.SavedConnections = []nmclient.ConnectionProfile{{Path: "/c0", SSID: "home"}}

	tick := f.probeN(3)
	if got := tick.Devices[DeviceWiFi].HWHealth; got != TriFaulted {
		t.Errorf("HWHealth = %s, want faulted (soft rfkill + profile)", got)
	}
}

// RfkillSoft + no profile maps to Inactive (catalog A2).
func TestProbe_A2_RfkillSoftNoProfile(t *testing.T) {
	f := newFixture(t)
	f.rfkillSoft = true
	f.wifiAt(nmclient.NMDeviceStateUnavailable, false, "")

	tick := f.probeN(3)
	if got := tick.Devices[DeviceWiFi].HWHealth; got != TriInactive {
		t.Errorf("HWHealth = %s, want inactive (soft rfkill, no profile)", got)
	}
}

// LedgerLoad on a fresh sandbox returns empty slices (no fail-closed since
// the volatile file is missing — F-B4 fail-open path).
func TestLedger_FreshLoadFailsOpen(t *testing.T) {
	dir := t.TempDir()
	store := NewLedgerStoreWithPaths(dir+"/persistent.json", dir+"/volatile.json", dir+"/legacy")
	led := store.Load()
	if len(led.Reboots) != 0 {
		t.Errorf("Reboots = %d, want 0", len(led.Reboots))
	}
	if len(led.Bounces) != 0 {
		t.Errorf("Bounces = %d, want 0 (volatile fail-open)", len(led.Bounces))
	}
}

// LedgerLoad on a corrupt persistent file fails closed (Reboots=[now]).
func TestLedger_CorruptPersistentFailsClosed(t *testing.T) {
	dir := t.TempDir()
	persistent := dir + "/persistent.json"
	if err := writeFile(t, persistent, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	store := NewLedgerStoreWithPaths(persistent, dir+"/volatile.json", dir+"/legacy")
	led := store.Load()
	if len(led.Reboots) != 1 {
		t.Errorf("Reboots = %d, want 1 (fail-closed budget exhaustion)", len(led.Reboots))
	}
}

// LedgerLoad on a corrupt volatile file pre-populates synthetic bounces.
func TestLedger_CorruptVolatileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	volatile := dir + "/volatile.json"
	if err := writeFile(t, volatile, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	store := NewLedgerStoreWithPaths(dir+"/persistent.json", volatile, dir+"/legacy")
	led := store.Load()
	if got := len(led.Bounces[DeviceWiFi]); got != 3 {
		t.Errorf("Bounces[wifi] = %d, want 3 (fail-closed synthesized)", got)
	}
	if got := len(led.Bounces[DeviceEthernet]); got != 3 {
		t.Errorf("Bounces[eth] = %d, want 3", got)
	}
}

// LegacyMigration: legacy net-watchdog-reboots is parsed into Reboots,
// persisted, and then deleted. A subsequent Load() must read the merged
// reboots from the persistent file (covers the bug_010 regression where
// the merge survived only in-memory until the next RecordReboot fired).
func TestLedger_LegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := dir + "/legacy"
	persistent := dir + "/persistent.json"
	contents := []byte("1714867200\n1714870800\n")
	if err := writeFile(t, legacy, contents); err != nil {
		t.Fatal(err)
	}
	store := NewLedgerStoreWithPaths(persistent, dir+"/volatile.json", legacy)
	led := store.Load()
	if got := len(led.Reboots); got != 2 {
		t.Errorf("Reboots = %d, want 2 (migrated from legacy)", got)
	}
	// Legacy file must be deleted post-migrate.
	if _, err := readFile(legacy); err == nil {
		t.Errorf("legacy file still exists after migration")
	}
	// Migration must have been persisted: a fresh store on the same
	// paths must observe the merged reboots from the persistent file.
	store2 := NewLedgerStoreWithPaths(persistent, dir+"/volatile.json", legacy)
	led2 := store2.Load()
	if got := len(led2.Reboots); got != 2 {
		t.Errorf("Reboots after restart = %d, want 2 (migration must have been persisted)", got)
	}
}

// L3 dampener: 3-of-3 Down ticks before reporting L3ProbeDown.
func TestProbe_L3DampenerThreeOfThree(t *testing.T) {
	f := newFixture(t)
	f.tcpL3Up = false
	f.wifiAt(nmclient.NMDeviceStateActivated, true, "192.168.1.1")

	for i := 1; i <= 3; i++ {
		tick := f.prober.Probe(context.Background(), ActionLedger{Bounces: map[DeviceKind][]time.Time{}, LastBounceAt: map[DeviceKind]time.Time{}}, f.now.Add(time.Duration(i)*TickInterval))
		want := L3ProbeUp
		if i == 3 {
			want = L3ProbeDown
		}
		if got := tick.L3Probe; got != want {
			t.Errorf("tick %d: L3Probe = %s, want %s", i, got, want)
		}
	}
}

// ConnectivityClassification: L3 up + GwHealthy → Full; L3 up only → Limited.
func TestClassifyConnectivity(t *testing.T) {
	devs := func(gw Tri) map[DeviceKind]DeviceObservation {
		return map[DeviceKind]DeviceObservation{
			DeviceWiFi: {Kind: DeviceWiFi, GwReachable: gw},
		}
	}
	tests := []struct {
		name string
		l3   L3ProbeResult
		gw   Tri
		nm   Connectivity
		want Connectivity
	}{
		{"l3 up + gw healthy", L3ProbeUp, TriHealthy, ConnectivityFull, ConnectivityFull},
		{"l3 up + gw faulted (limited)", L3ProbeUp, TriFaulted, ConnectivityFull, ConnectivityLimited},
		{"l3 down + gw healthy", L3ProbeDown, TriHealthy, ConnectivityNone, ConnectivityLimited},
		{"l3 down + gw faulted", L3ProbeDown, TriFaulted, ConnectivityNone, ConnectivityNone},
		{"l3 up + portal advisory", L3ProbeUp, TriHealthy, ConnectivityPortal, ConnectivityPortal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyConnectivity(tt.l3, devs(tt.gw), tt.nm)
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// TestProbeWiFi_TerminalConfigFailureDampened verifies the 3-of-3
// sustained-failure detection for saved-profile-but-not-connecting cases
// (SSID renamed, out of range, BSSID gone). Pre-fix, NMState=Disconnected
// + HasProfile=true fell through to "DHCP-in-flight Healthy" forever and
// the supervisor never entered the setup AP — the owner had no way to
// fix credentials.
func TestProbeWiFi_TerminalConfigFailureDampened(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateDisconnected, false, "")
	f.nm.SavedConnections = []nmclient.ConnectionProfile{{Path: "/c0", SSID: "home"}}

	// Tick 1+2: candidate=Faulted, counter < 3 → reported as Healthy
	for i := 1; i <= 2; i++ {
		tick := f.prober.Probe(context.Background(), ActionLedger{Bounces: map[DeviceKind][]time.Time{}, LastBounceAt: map[DeviceKind]time.Time{}}, f.now.Add(time.Duration(i)*TickInterval))
		if got := tick.Devices[DeviceWiFi].ConfigHealth; got != TriHealthy {
			t.Errorf("tick %d: ConfigHealth = %s, want healthy (dampener at %d/3)", i, got, i)
		}
	}

	// Tick 3: counter=3 → ConfigHealth flips to Faulted.
	tick := f.prober.Probe(context.Background(), ActionLedger{Bounces: map[DeviceKind][]time.Time{}, LastBounceAt: map[DeviceKind]time.Time{}}, f.now.Add(3*TickInterval))
	if got := tick.Devices[DeviceWiFi].ConfigHealth; got != TriFaulted {
		t.Errorf("after 3 ticks: ConfigHealth = %s, want faulted", got)
	}
}

// TestProbeWiFi_IgnoresHotspotActiveConnection verifies that during AP
// mode the supervisor does NOT treat the hotspot's local 10.42.x IP as a
// STA uplink. Pre-fix, info.IP4Address from the hotspot ActiveConnection
// flipped HasIP=true → ConfigHealth=Healthy → APArbiter exits AP →
// recovery oscillates with no real STA uplink.
func TestProbeWiFi_IgnoresHotspotActiveConnection(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateActivated, true, "10.42.0.1") // hotspot-shaped IP
	// Override ActiveConnectionInfo to return a hotspot (ID prefix matches
	// nmclient.HotspotIDPrefix → IsHotspot() returns true).
	f.nm.ActiveConnResult = &nmclient.ActiveConnectionInfo{
		Path:       "/org/freedesktop/NetworkManager/ActiveConnection/0",
		ID:         nmclient.HotspotIDPrefix + "ABCD",
		IP4Address: "10.42.0.1",
		IP4Gateway: "",
	}

	tick := f.probeN(1)
	wifi := tick.Devices[DeviceWiFi]
	if wifi.HasIP {
		t.Errorf("HasIP = true on hotspot active connection; want false (hotspot is not a STA uplink)")
	}
}

// TestProbeWiFi_ColdStartReadsStateReason verifies that on piccolod
// restart with NM in a persistent failure state (e.g. NoSecrets after a
// wrong-password attempt), the prober falls back to NM's cached
// StateReason property when the signal cache is empty. Without this,
// catalog D2 regresses across restart — ConfigHealth misclassifies as
// Healthy and the supervisor never triggers AP entry.
func TestProbeWiFi_ColdStartReadsStateReason(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateDisconnected, false, "")
	f.nm.SavedConnections = []nmclient.ConnectionProfile{{Path: "/c0", SSID: "home"}}
	// NM's cached StateReason returns NoSecrets; signal cache is empty
	// (lastReasonByDev not populated — fresh restart).
	f.nm.DeviceStateReasonResult = nmclient.NMDeviceStateReasonNoSecrets

	tick := f.probeN(1)
	wifi := tick.Devices[DeviceWiFi]
	if wifi.HWHealth != TriHealthy {
		t.Errorf("HWHealth = %s, want healthy", wifi.HWHealth)
	}
	if wifi.ConfigHealth != TriFaulted {
		t.Errorf("ConfigHealth = %s, want faulted (cold-start StateReason fallback)", wifi.ConfigHealth)
	}
}

// TestSystemState_SelfSeedsFromDisk verifies the post-restart guarantee:
// when piccolod starts mid-onboarding, NewBusSystemState reads the
// on-disk onboarding.json and reports SystemBusy=true from the very first
// call — before any bus event arrives. Without this, the supervisor would
// bounce wifi during install_disk on a restart (defeats catalog A7/G7).
func TestSystemState_SelfSeedsFromDisk(t *testing.T) {
	dir := t.TempDir()
	pathsSetCoreRoot(t, dir)

	// Simulate mid-install state.
	mustWriteJSON(t, dir+"/network-bootstrap/onboarding.json", map[string]any{
		"state":        "install_disk",
		"install_done": false,
	})

	sys := NewBusSystemState(context.Background(), nil)
	busy, reason := sys.SystemBusy()
	if !busy {
		t.Fatalf("SystemBusy = false on first call after restart mid-install_disk; want true")
	}
	if reason == "" {
		t.Errorf("reason = empty; want a non-empty reason")
	}
}

// TestSystemState_NoFileNotBusy verifies that a missing onboarding.json
// resolves to SystemBusy=false (devices past first-run lack the marker).
func TestSystemState_NoFileNotBusy(t *testing.T) {
	dir := t.TempDir()
	pathsSetCoreRoot(t, dir)

	sys := NewBusSystemState(context.Background(), nil)
	busy, _ := sys.SystemBusy()
	if busy {
		t.Errorf("SystemBusy = true on first call with no onboarding.json; want false")
	}
}

// pathsSetCoreRoot is a thin wrapper for paths.SetCoreRootForTest so the
// test stays readable. Imported lazily to keep the network package's test
// surface small.
func pathsSetCoreRoot(t *testing.T, dir string) {
	t.Helper()
	pathsSetCoreRootImpl(t, dir)
}

// TestProbeL3_ParentBudgetCoversSumOfAttempts verifies that probeL3's
// parent context budget covers sequential dials to ALL targets, not just
// one. A silently-blackholed first target must not strangle the second
// target's per-attempt timeout.
//
// Pre-fix: parent timeout was 2.25s while two sequential 2s attempts
// need 4s — the second target had only ~250ms instead of 2s, producing
// false L3 Down on real RFC catalog C9 scenarios.
func TestProbeL3_ParentBudgetCoversSumOfAttempts(t *testing.T) {
	f := newFixture(t)
	f.wifiAt(nmclient.NMDeviceStateActivated, true, "192.168.1.1")

	prevTCP := tcpConnectAny
	t.Cleanup(func() { tcpConnectAny = prevTCP })

	// Stub: target[0] consumes its full perAttempt budget then fails;
	// target[1] succeeds 150ms in. Total wall-clock ≈ perAttempt + 150ms.
	tcpConnectAny = func(ctx context.Context, targets []string, perAttempt time.Duration) bool {
		select {
		case <-time.After(perAttempt):
		case <-ctx.Done():
			return false
		}
		select {
		case <-time.After(150 * time.Millisecond):
			return ctx.Err() == nil
		case <-ctx.Done():
			return false
		}
	}

	tick := f.probeN(1)
	if got := tick.L3Probe; got != L3ProbeUp {
		t.Errorf("L3Probe = %s, want up (parent budget should cover both targets after blackholed target[0])", got)
	}
}

// ----- helpers -----

func writeFile(t *testing.T, path string, data []byte) error {
	t.Helper()
	return os.WriteFile(path, data, 0o644)
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func pathsSetCoreRootImpl(t *testing.T, dir string) {
	t.Helper()
	paths.SetCoreRootForTest(t, dir)
}

func mustWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
