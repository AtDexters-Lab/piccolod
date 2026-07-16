package network

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
)

func TestManagerStatusExposesTypedMultiInterfaceState(t *testing.T) {
	store := newNetworkTransitionStore()
	store.record(NetworkTransitionState{
		ActiveUplink:         UplinkEthernet,
		ActiveUplinkIface:    "enp2s0",
		Connectivity:         ConnectivityFull,
		DefaultRouteIface:    "enp2s0",
		DefaultRouteObserved: true,
		DefaultRouteKnown:    true,
		InterfacesObserved:   true,
		Interfaces: []NetworkInterfaceState{
			{Kind: DeviceEthernet, Iface: "enp1s0", Role: InterfaceRoleNotConnected},
			{Kind: DeviceEthernet, Iface: "enp2s0", Role: InterfaceRoleWANLAN, LinkUp: true, HasIP: true},
		},
	})
	mgr := &Manager{
		nm:         nmclient.NewStubClient(),
		supervisor: &Supervisor{transition: store},
	}

	status := mgr.Status()
	if status.ActiveUplink != UplinkEthernet || status.ActiveUplinkIface != "enp2s0" {
		t.Fatalf("active uplink = %s/%s, want ethernet/enp2s0", status.ActiveUplink, status.ActiveUplinkIface)
	}
	if len(status.Interfaces) != 2 || status.Interfaces[1].Iface != "enp2s0" {
		t.Fatalf("interfaces = %+v, want both Ethernet interfaces", status.Interfaces)
	}

	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(encoded), `"state"`) {
		t.Fatalf("status still exposes removed compatibility state: %s", encoded)
	}
}

func TestManagerStatusReadsDetailsFromConcreteActiveWiFiInterface(t *testing.T) {
	primaryPath := dbus.ObjectPath("/devices/wlan0")
	activePath := dbus.ObjectPath("/devices/wlan1")
	stub := nmclient.NewStubClient()
	stub.ActiveConnByDevice = map[dbus.ObjectPath]*nmclient.ActiveConnectionInfo{
		activePath: {ID: "Second-WiFi", IP4Address: "10.42.0.38"},
	}
	stub.ActiveAPByDevice = map[dbus.ObjectPath]*nmclient.AccessPoint{
		activePath: {Frequency: 5180},
	}
	devices := []nmclient.WiFiDevice{
		{Path: primaryPath, Interface: "wlan0"},
		{Path: activePath, Interface: "wlan1"},
	}
	store := newNetworkTransitionStore()
	store.record(NetworkTransitionState{
		ActiveUplink:      UplinkWiFi,
		ActiveUplinkIface: "wlan1",
		Connectivity:      ConnectivityFull,
	})
	mgr := &Manager{
		nm:            stub,
		supervisor:    &Supervisor{transition: store},
		wifiDevice:    &devices[0],
		wifiDevices:   devices,
		wifiAvailable: true,
		sigMon:        newSignalMonitor(stub, nil),
	}

	status := mgr.Status()
	if status.SSID != "Second-WiFi" || status.IPAddress != "10.42.0.38" {
		t.Fatalf("WiFi details = %q/%q, want active wlan1 details", status.SSID, status.IPAddress)
	}
	if status.FrequencyMHz == nil || *status.FrequencyMHz != 5180 || status.Band != "5GHz" {
		t.Fatalf("WiFi radio details = %v/%q, want 5180/5GHz", status.FrequencyMHz, status.Band)
	}
	call := stub.LastCall("ActiveConnectionInfo")
	if call == nil || len(call.Args) != 1 || call.Args[0] != activePath {
		t.Fatalf("ActiveConnectionInfo call = %+v, want device %s", call, activePath)
	}
	call = stub.LastCall("ActiveAccessPoint")
	if call == nil || len(call.Args) != 1 || call.Args[0] != activePath {
		t.Fatalf("ActiveAccessPoint call = %+v, want device %s", call, activePath)
	}
}

func TestManagerStatusExposesIncompleteProjectionAsUnknown(t *testing.T) {
	store := newNetworkTransitionStore()
	store.record(NetworkTransitionState{
		ActiveUplink:       UplinkNone,
		Connectivity:       ConnectivityUnknown,
		InterfacesObserved: false,
		Interfaces: []NetworkInterfaceState{
			{Kind: DeviceEthernet, Iface: "enp2s0", Role: InterfaceRoleUnknown, LinkUp: true},
		},
	})
	mgr := &Manager{
		nm:         nmclient.NewStubClient(),
		supervisor: &Supervisor{transition: store},
	}

	status := mgr.Status()
	if status.ActiveUplink != UplinkNone || status.ActiveUplinkIface != "" {
		t.Fatalf("active uplink = %s/%q, want none", status.ActiveUplink, status.ActiveUplinkIface)
	}
	if status.Connectivity != ConnectivityUnknown.String() {
		t.Fatalf("connectivity = %q, want unknown", status.Connectivity)
	}
	if len(status.Interfaces) != 1 || status.Interfaces[0].Role != InterfaceRoleUnknown {
		t.Fatalf("interfaces = %+v, want retained uncertain observation", status.Interfaces)
	}
}

func TestSignalMonitorReadsConcreteActiveWiFiInterface(t *testing.T) {
	primaryPath := dbus.ObjectPath("/devices/wlan0")
	activePath := dbus.ObjectPath("/devices/wlan1")
	stub := nmclient.NewStubClient()
	stub.SignalStrengthResult = 80
	devices := []nmclient.WiFiDevice{
		{Path: primaryPath, Interface: "wlan0"},
		{Path: activePath, Interface: "wlan1"},
	}
	store := newNetworkTransitionStore()
	store.record(NetworkTransitionState{
		ActiveUplink:      UplinkWiFi,
		ActiveUplinkIface: "wlan1",
		Connectivity:      ConnectivityFull,
	})
	mgr := &Manager{
		nm:          stub,
		supervisor:  &Supervisor{transition: store},
		wifiDevice:  &devices[0],
		wifiDevices: devices,
	}
	mgr.sigMon = newSignalMonitor(stub, mgr)

	mgr.sigMon.poll()

	call := stub.LastCall("SignalStrength")
	if call == nil || len(call.Args) != 1 || call.Args[0] != activePath {
		t.Fatalf("SignalStrength call = %+v, want device %s", call, activePath)
	}
}
