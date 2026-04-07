package network

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/testutil"
)

func newTestStateMachine() *stateMachine {
	return newStateMachine(nil)
}

func TestStateMachine_InitialState(t *testing.T) {
	sm := newTestStateMachine()
	if sm.Current() != StateDisconnected {
		t.Fatalf("initial state = %s, want %s", sm.Current(), StateDisconnected)
	}
	if sm.Uplink() != UplinkNone {
		t.Fatalf("initial uplink = %s, want %s", sm.Uplink(), UplinkNone)
	}
}

func TestStateMachine_EthernetUp(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()

	if sm.Current() != StateEthernet {
		t.Fatalf("state = %s, want %s", sm.Current(), StateEthernet)
	}
	if sm.Uplink() != UplinkEthernet {
		t.Fatalf("uplink = %s, want %s", sm.Uplink(), UplinkEthernet)
	}
}

func TestStateMachine_EthernetDown_WithSavedWifi(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()

	sm.handleEthernetDown(true)
	if sm.Current() != StateReconnecting {
		t.Fatalf("state = %s, want %s", sm.Current(), StateReconnecting)
	}
}

func TestStateMachine_EthernetDown_NoWifi(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()

	sm.handleEthernetDown(false)
	if sm.Current() != StateAPMode {
		t.Fatalf("state = %s, want %s", sm.Current(), StateAPMode)
	}
}

func TestStateMachine_EthernetDown_WiFiAlreadyConnected(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()
	sm.wifiConnected = func() bool { return true }

	sm.handleEthernetDown(true)
	if sm.Current() != StateWiFiSTA {
		t.Fatalf("state = %s, want %s", sm.Current(), StateWiFiSTA)
	}
	if sm.Uplink() != UplinkWiFi {
		t.Fatalf("uplink = %s, want %s", sm.Uplink(), UplinkWiFi)
	}
}

func TestStateMachine_EthernetDown_WiFiNotConnected(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()
	sm.wifiConnected = func() bool { return false }

	sm.handleEthernetDown(true)
	if sm.Current() != StateReconnecting {
		t.Fatalf("state = %s, want %s", sm.Current(), StateReconnecting)
	}
}

func TestStateMachine_WiFiConnected(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()

	if sm.Current() != StateWiFiSTA {
		t.Fatalf("state = %s, want %s", sm.Current(), StateWiFiSTA)
	}
	if sm.Uplink() != UplinkWiFi {
		t.Fatalf("uplink = %s, want %s", sm.Uplink(), UplinkWiFi)
	}
}

func TestStateMachine_WiFiConnected_DoesNotOverrideEthernet(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()
	sm.handleWiFiConnected()

	if sm.Current() != StateEthernet {
		t.Fatalf("state = %s, want %s (Ethernet should take priority)", sm.Current(), StateEthernet)
	}
}

func TestStateMachine_WiFiDisconnected(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true)

	if sm.Current() != StateReconnecting {
		t.Fatalf("state = %s, want %s", sm.Current(), StateReconnecting)
	}
}

func TestStateMachine_WiFiDisconnected_NoSavedProfiles(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(false)

	if sm.Current() != StateAPMode {
		t.Fatalf("state = %s, want %s (profile deleted → AP mode)", sm.Current(), StateAPMode)
	}
}

func TestStateMachine_WiFiDisconnected_IgnoredWhenNotWifi(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp()
	sm.handleWiFiDisconnected(true)

	if sm.Current() != StateEthernet {
		t.Fatalf("state = %s, want %s (should ignore WiFi disconnect when on Ethernet)", sm.Current(), StateEthernet)
	}
}

func TestStateMachine_WiFiDisconnected_DuringLongTransition(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()

	// Simulate Connect() in progress (long transition active)
	_, ok := sm.beginLongTransition()
	if !ok {
		t.Fatal("beginLongTransition should succeed")
	}

	// WiFi disconnect during Connect — should go to Reconnecting (not AP mode)
	// even when hasSavedWifi=false (profile may have been deleted by NM)
	sm.handleWiFiDisconnected(false)

	if sm.Current() != StateReconnecting {
		t.Fatalf("state = %s, want %s (during long transition, should reconnect not AP)",
			sm.Current(), StateReconnecting)
	}

	sm.endLongTransition()
}

func TestStateMachine_SignalClearedOnLeavingWiFi(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.updateSignal(-55) // good

	sm.mu.Lock()
	if sm.lastSignalDBm == 0 {
		sm.mu.Unlock()
		t.Fatal("signal should be set while in WiFi STA")
	}
	sm.mu.Unlock()

	// Leave WiFi → signal should be cleared
	sm.handleWiFiDisconnected(true) // → Reconnecting

	sm.mu.Lock()
	dbm := sm.lastSignalDBm
	tier := sm.lastSignalTier
	sm.mu.Unlock()

	if dbm != 0 || tier != "" {
		t.Fatalf("signal not cleared on leaving WiFi STA: dbm=%d, tier=%s", dbm, tier)
	}
}

func TestStateMachine_ReconnectSuccess(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true)

	sm.handleReconnectResult(true)
	if sm.Current() != StateWiFiSTA {
		t.Fatalf("state = %s, want %s", sm.Current(), StateWiFiSTA)
	}
}

func TestStateMachine_ReconnectFailure_Backoff(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true)

	// First failure — still reconnecting, backoff advances
	sm.handleReconnectResult(false)
	if sm.Current() != StateReconnecting {
		t.Fatalf("after 1 failure: state = %s, want %s", sm.Current(), StateReconnecting)
	}

	// Advance to ceiling
	for i := 0; i < len(backoffSchedule)-2; i++ {
		sm.handleReconnectResult(false)
	}

	// At ceiling — should transition to Disconnected
	sm.handleReconnectResult(false)
	if sm.Current() != StateDisconnected {
		t.Fatalf("at ceiling: state = %s, want %s", sm.Current(), StateDisconnected)
	}
}

func TestStateMachine_ReconnectFailure_EscalatesToAPMode(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true)

	// Advance to ceiling
	for i := 0; i < len(backoffSchedule); i++ {
		sm.handleReconnectResult(false)
	}

	// 5 failures at ceiling → AP mode
	for i := 0; i < ceilingFailuresForAP-1; i++ {
		sm.handleReconnectResult(false)
	}

	if sm.Current() != StateAPMode {
		t.Fatalf("after %d ceiling failures: state = %s, want %s", ceilingFailuresForAP, sm.Current(), StateAPMode)
	}
}

func TestStateMachine_ReconnectResult_IgnoredOutsideReconnecting(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp() // → StateEthernet

	sm.handleReconnectResult(false)

	if sm.Current() != StateEthernet {
		t.Fatalf("state = %s, want %s (reconnect result should be ignored outside reconnecting)",
			sm.Current(), StateEthernet)
	}
}

func TestStateMachine_ReconnectResult_IgnoredDuringLongTransition(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true) // → StateReconnecting

	_, ok := sm.beginLongTransition()
	if !ok {
		t.Fatal("beginLongTransition should succeed")
	}

	sm.handleReconnectResult(false)

	// Backoff should NOT have advanced
	sm.mu.Lock()
	idx := sm.backoffIdx
	sm.mu.Unlock()
	if idx != 0 {
		t.Fatalf("backoffIdx = %d, want 0 (should be ignored during long transition)", idx)
	}

	sm.endLongTransition()
}

func TestStateMachine_AuthFailure_ImmediateAPMode(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true) // → StateReconnecting

	sm.handleAuthFailure()

	if sm.Current() != StateAPMode {
		t.Fatalf("state = %s, want %s (auth failure should skip backoff → AP mode)",
			sm.Current(), StateAPMode)
	}
}

func TestStateMachine_AuthFailure_IgnoredDuringLongTransition(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true) // → StateReconnecting

	_, ok := sm.beginLongTransition()
	if !ok {
		t.Fatal("beginLongTransition should succeed")
	}

	sm.handleAuthFailure()

	if sm.Current() != StateReconnecting {
		t.Fatalf("state = %s, want %s (auth failure should be ignored during long transition)",
			sm.Current(), StateReconnecting)
	}

	sm.endLongTransition()
}

func TestStateMachine_AuthFailure_IgnoredOutsideReconnecting(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleEthernetUp() // → StateEthernet

	sm.handleAuthFailure()

	if sm.Current() != StateEthernet {
		t.Fatalf("state = %s, want %s (auth failure should be ignored outside reconnecting)",
			sm.Current(), StateEthernet)
	}
}

func TestStateMachine_ReconnectSuccess_ResetsBackoff(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.handleWiFiDisconnected(true)

	// Advance backoff partway
	for i := 0; i < 3; i++ {
		sm.handleReconnectResult(false)
	}

	// Success resets
	sm.handleReconnectResult(true)

	sm.mu.Lock()
	idx := sm.backoffIdx
	sm.mu.Unlock()

	if idx != 0 {
		t.Fatalf("backoffIdx = %d after success, want 0", idx)
	}
}

func TestStateMachine_EthernetPreemptsLongTransition(t *testing.T) {
	sm := newTestStateMachine()

	// Start a long transition
	ctx, ok := sm.beginLongTransition()
	if !ok {
		t.Fatal("beginLongTransition should succeed")
	}

	// Ethernet arrives — should cancel the transition
	sm.handleEthernetUp()

	// The STA context should be cancelled
	select {
	case <-ctx.Done():
		// expected
	default:
		t.Fatal("staCtx should be cancelled after Ethernet promotion")
	}

	if sm.Current() != StateEthernet {
		t.Fatalf("state = %s, want %s", sm.Current(), StateEthernet)
	}

	sm.endLongTransition()
}

func TestStateMachine_ConcurrentLongTransitions(t *testing.T) {
	sm := newTestStateMachine()

	_, ok1 := sm.beginLongTransition()
	if !ok1 {
		t.Fatal("first beginLongTransition should succeed")
	}

	_, ok2 := sm.beginLongTransition()
	if ok2 {
		t.Fatal("second beginLongTransition should fail (transition in progress)")
	}

	sm.endLongTransition()

	// Now it should work again
	_, ok3 := sm.beginLongTransition()
	if !ok3 {
		t.Fatal("third beginLongTransition should succeed after end")
	}
	sm.endLongTransition()
}

func TestStateMachine_SignalUpdate_PublishesOnTierChange(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()

	// First reading — sets the tier
	sm.updateSignal(-55) // good

	sm.mu.Lock()
	tier1 := sm.lastSignalTier
	sm.mu.Unlock()

	if tier1 != SignalGood {
		t.Fatalf("tier = %s, want %s", tier1, SignalGood)
	}

	// Same tier — no event expected (just updates dBm)
	sm.updateSignal(-58) // still good

	// Different tier
	sm.updateSignal(-75) // weak

	sm.mu.Lock()
	tier2 := sm.lastSignalTier
	sm.mu.Unlock()

	if tier2 != SignalWeak {
		t.Fatalf("tier = %s, want %s", tier2, SignalWeak)
	}
}

func TestStateMachine_Snapshot(t *testing.T) {
	sm := newTestStateMachine()
	sm.handleWiFiConnected()
	sm.updateSignal(-65) // fair

	state, uplink, dbm, tier := sm.snapshot()
	if state != StateWiFiSTA {
		t.Fatalf("snapshot state = %s, want %s", state, StateWiFiSTA)
	}
	if uplink != UplinkWiFi {
		t.Fatalf("snapshot uplink = %s, want %s", uplink, UplinkWiFi)
	}
	if dbm != -65 {
		t.Fatalf("snapshot dbm = %d, want -65", dbm)
	}
	if tier != SignalFair {
		t.Fatalf("snapshot tier = %s, want %s", tier, SignalFair)
	}
}

func TestStateMachine_TransitionCallback(t *testing.T) {
	sm := newTestStateMachine()

	var mu sync.Mutex
	var transitions []ConnState
	sm.onTransition = func(s ConnState) {
		mu.Lock()
		transitions = append(transitions, s)
		mu.Unlock()
	}

	sm.handleEthernetUp()
	sm.handleEthernetDown(false) // → AP mode (no saved wifi)

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 2 {
		t.Fatalf("got %d transitions, want 2", len(transitions))
	}
	if transitions[0] != StateEthernet {
		t.Fatalf("transition[0] = %s, want %s", transitions[0], StateEthernet)
	}
	if transitions[1] != StateAPMode {
		t.Fatalf("transition[1] = %s, want %s", transitions[1], StateAPMode)
	}
}

func TestStateMachine_NextBackoff(t *testing.T) {
	sm := newTestStateMachine()

	tests := []struct {
		idx      int
		expected time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{6, 60 * time.Second}, // ceiling
		{99, 60 * time.Second}, // beyond ceiling
	}

	for _, tt := range tests {
		sm.mu.Lock()
		sm.backoffIdx = tt.idx
		sm.mu.Unlock()

		got := sm.nextBackoff()
		if got != tt.expected {
			t.Errorf("nextBackoff(idx=%d) = %v, want %v", tt.idx, got, tt.expected)
		}
	}
}

// --- determineInitialState tests (exercises Manager method) ---

func newTestManager(stub *nmclient.StubClient) *Manager {
	return NewManager(stub, &testutil.FakeRunner{}, nil)
}

func TestDetermineInitialState_NoProfiles_WithWiFiDevice(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.SavedConnections = nil // no saved WiFi profiles
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      dbus.ObjectPath("/dev/wifi0"),
		Interface: "wlan0",
		HWAddress: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		State:     nmclient.NMDeviceStateDisconnected,
	}}
	m := newTestManager(stub)
	_ = m.discoverDevices()
	m.determineInitialState()

	if m.state.Current() != StateAPMode {
		t.Fatalf("state = %s, want %s (no profiles + WiFi device → AP mode)", m.state.Current(), StateAPMode)
	}
}

func TestDetermineInitialState_NoProfiles_NoWiFiDevice(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.SavedConnections = nil
	stub.WiFiDevicesResult = nil // no WiFi hardware
	m := newTestManager(stub)
	_ = m.discoverDevices()
	m.determineInitialState()

	if m.state.Current() != StateDisconnected {
		t.Fatalf("state = %s, want %s (no profiles + no WiFi → disconnected)", m.state.Current(), StateDisconnected)
	}
}

func TestDetermineInitialState_HasProfiles_NotConnected(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.SavedConnections = []nmclient.ConnectionProfile{
		{SSID: "HomeWiFi", Path: dbus.ObjectPath("/conn/1")},
	}
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      dbus.ObjectPath("/dev/wifi0"),
		Interface: "wlan0",
		State:     nmclient.NMDeviceStateDisconnected,
	}}
	m := newTestManager(stub)
	_ = m.discoverDevices()
	m.determineInitialState()

	if m.state.Current() != StateReconnecting {
		t.Fatalf("state = %s, want %s (saved profile exists → NM will auto-connect)", m.state.Current(), StateReconnecting)
	}
}

func TestDetermineInitialState_NoProfiles_WiFiUnavailable(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.SavedConnections = nil
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      dbus.ObjectPath("/dev/wifi0"),
		Interface: "wlan0",
		State:     nmclient.NMDeviceStateUnavailable, // rfkill or driver issue
	}}
	m := newTestManager(stub)
	_ = m.discoverDevices()
	m.determineInitialState()

	if m.state.Current() != StateDisconnected {
		t.Fatalf("state = %s, want %s (WiFi unavailable → cannot AP)", m.state.Current(), StateDisconnected)
	}
}

func TestDetermineInitialState_NoProfiles_APSuppressed(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.SavedConnections = nil
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      dbus.ObjectPath("/dev/wifi0"),
		Interface: "wlan0",
		HWAddress: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		State:     nmclient.NMDeviceStateDisconnected,
	}}
	m := newTestManager(stub)
	_ = m.discoverDevices()
	m.mu.Lock()
	m.apSuppressed = true
	m.mu.Unlock()
	m.determineInitialState()

	if m.state.Current() != StateDisconnected {
		t.Fatalf("state = %s, want %s (AP suppressed → no AP mode)", m.state.Current(), StateDisconnected)
	}
}

func TestDetermineInitialState_HasProfiles_WiFiUnavailable(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.SavedConnections = []nmclient.ConnectionProfile{
		{SSID: "HomeWiFi", Path: dbus.ObjectPath("/conn/1")},
	}
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      dbus.ObjectPath("/dev/wifi0"),
		Interface: "wlan0",
		State:     nmclient.NMDeviceStateUnavailable,
	}}
	m := newTestManager(stub)
	_ = m.discoverDevices()
	m.determineInitialState()

	// WiFi unavailable — cannot reconnect, fall through to Disconnected
	if m.state.Current() != StateDisconnected {
		t.Fatalf("state = %s, want %s (saved profiles but WiFi unavailable)", m.state.Current(), StateDisconnected)
	}
}
