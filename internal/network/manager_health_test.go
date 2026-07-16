package network

import (
	"strings"
	"testing"

	"piccolod/internal/health"
)

func TestUpdateNetworkHealthUsesAuthoritativeActiveInterface(t *testing.T) {
	tracker := health.NewTracker()
	mgr := &Manager{healthTracker: tracker}
	mgr.updateNetworkHealth(NetworkTransitionState{
		ActiveUplink:      UplinkEthernet,
		ActiveUplinkIface: "enp2s0",
		Connectivity:      ConnectivityFull,
	})

	status := tracker.Snapshot()["network"]
	if status.Level != health.LevelOK {
		t.Fatalf("network level = %s, want ok", status.Level)
	}
	if !strings.Contains(status.Message, "enp2s0") {
		t.Fatalf("network message = %q, want concrete active interface", status.Message)
	}
}

func TestUpdateNetworkHealthKeepsOfflineStateDiagnostic(t *testing.T) {
	tracker := health.NewTracker()
	mgr := &Manager{healthTracker: tracker}
	mgr.updateNetworkHealth(NetworkTransitionState{
		ActiveUplink: UplinkNone,
		Connectivity: ConnectivityNone,
	})

	if got := tracker.Snapshot()["network"].Level; got != health.LevelError {
		t.Fatalf("network level = %s, want error", got)
	}
}

func TestUpdateNetworkHealthTreatsIncompleteProjectionAsWarning(t *testing.T) {
	tracker := health.NewTracker()
	mgr := &Manager{healthTracker: tracker}
	mgr.updateNetworkHealth(NetworkTransitionState{
		ActiveUplink: UplinkNone,
		Connectivity: ConnectivityUnknown,
	})

	status := tracker.Snapshot()["network"]
	if status.Level != health.LevelWarn {
		t.Fatalf("network level = %s, want warn", status.Level)
	}
	if !strings.Contains(status.Message, "unknown") {
		t.Fatalf("network message = %q, want uncertainty diagnostic", status.Message)
	}
}

func TestUpdateNetworkHealthReportsLimitedConnectivityAsWarning(t *testing.T) {
	tracker := health.NewTracker()
	mgr := &Manager{healthTracker: tracker}
	mgr.updateNetworkHealth(NetworkTransitionState{
		ActiveUplink:      UplinkWiFi,
		ActiveUplinkIface: "wlan0",
		Connectivity:      ConnectivityLimited,
	})

	status := tracker.Snapshot()["network"]
	if status.Level != health.LevelWarn {
		t.Fatalf("network level = %s, want warn", status.Level)
	}
	if !strings.Contains(status.Message, "limited") || !strings.Contains(status.Message, "wlan0") {
		t.Fatalf("network message = %q, want limited concrete-interface diagnostic", status.Message)
	}
}
