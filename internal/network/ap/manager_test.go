package ap

import (
	"context"
	"testing"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/testutil"
)

func TestActiveOnNM_HotspotActive(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.ActiveConnResult = &nmclient.ActiveConnectionInfo{
		ID: nmclient.HotspotIDPrefix + "Piccolo-Setup-AABB",
	}
	mgr := NewManager(stub, &testutil.FakeRunner{})

	if !mgr.ActiveOnNM(dbus.ObjectPath("/dev/wifi0")) {
		t.Fatal("ActiveOnNM should return true when hotspot is active on NM")
	}
}

func TestActiveOnNM_STAActive(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.ActiveConnResult = &nmclient.ActiveConnectionInfo{
		ID: "MyHomeWiFi",
	}
	mgr := NewManager(stub, &testutil.FakeRunner{})

	if mgr.ActiveOnNM(dbus.ObjectPath("/dev/wifi0")) {
		t.Fatal("ActiveOnNM should return false when STA connection is active")
	}
}

func TestActiveOnNM_NoConnection(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.ActiveConnResult = nil
	mgr := NewManager(stub, &testutil.FakeRunner{})

	if mgr.ActiveOnNM(dbus.ObjectPath("/dev/wifi0")) {
		t.Fatal("ActiveOnNM should return false when no connection is active")
	}
}

func TestActiveOnNM_Error(t *testing.T) {
	stub := nmclient.NewStubClient()
	stub.ActiveConnErr = context.DeadlineExceeded
	mgr := NewManager(stub, &testutil.FakeRunner{})

	if mgr.ActiveOnNM(dbus.ObjectPath("/dev/wifi0")) {
		t.Fatal("ActiveOnNM should return false on D-Bus error")
	}
}

func TestForceStop_ActiveTrue(t *testing.T) {
	stub := nmclient.NewStubClient()
	mgr := NewManager(stub, &testutil.FakeRunner{})

	// Simulate active AP
	mgr.mu.Lock()
	mgr.active = true
	cleanupRan := false
	mgr.cleanups = []func(){func() { cleanupRan = true }}
	mgr.ssidVal.Store("test-ssid")
	mgr.mu.Unlock()

	mgr.ForceStop(context.Background(), dbus.ObjectPath("/dev/wifi0"))

	mgr.mu.Lock()
	active := mgr.active
	mgr.mu.Unlock()

	if active {
		t.Fatal("ForceStop should set active=false")
	}
	if !cleanupRan {
		t.Fatal("ForceStop should run cleanups when active=true")
	}
	if mgr.SSID() != "" {
		t.Fatalf("ForceStop should clear SSID, got %q", mgr.SSID())
	}
}

func TestForceStop_ActiveFalse_WithOrphanedHotspot(t *testing.T) {
	stub := nmclient.NewStubClient()
	// Simulate orphaned hotspot: local active=false but NM has a hotspot running
	stub.ActiveConnResult = &nmclient.ActiveConnectionInfo{
		ID: nmclient.HotspotIDPrefix + "Piccolo-Setup-AABB",
	}
	mgr := NewManager(stub, &testutil.FakeRunner{})

	mgr.ForceStop(context.Background(), dbus.ObjectPath("/dev/wifi0"))

	if stub.CallCount("DeactivateHotspot") == 0 {
		t.Fatal("ForceStop should call DeactivateHotspot when active=false but NM has orphaned hotspot")
	}
}

func TestForceStop_ActiveFalse_NoOrphan(t *testing.T) {
	stub := nmclient.NewStubClient()
	// No hotspot on NM — normal non-AP transition
	stub.ActiveConnResult = nil
	mgr := NewManager(stub, &testutil.FakeRunner{})

	mgr.ForceStop(context.Background(), dbus.ObjectPath("/dev/wifi0"))

	if stub.CallCount("DeactivateHotspot") != 0 {
		t.Fatal("ForceStop should not call DeactivateHotspot when no orphaned hotspot exists")
	}
}
