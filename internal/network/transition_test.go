package network

import (
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
)

func TestCanonicalNetworkTransitionStateSortsSameKindInterfacesAndAddresses(t *testing.T) {
	state := canonicalNetworkTransitionState(NetworkTransitionState{
		Interfaces: []NetworkInterfaceState{
			{
				Kind:  DeviceEthernet,
				Iface: "usb0",
				Role:  InterfaceRoleLAN,
				IPv4: []netip.Addr{
					netip.MustParseAddr("10.0.0.20"),
					netip.MustParseAddr("10.0.0.2"),
					netip.MustParseAddr("10.0.0.2"),
				},
			},
			{Kind: DeviceWiFi, Iface: "wlan0", Role: InterfaceRoleWANLAN},
			{Kind: DeviceEthernet, Iface: "eth0", Role: InterfaceRoleLAN},
		},
	})

	gotOrder := []string{
		state.Interfaces[0].Iface,
		state.Interfaces[1].Iface,
		state.Interfaces[2].Iface,
	}
	wantOrder := []string{"eth0", "usb0", "wlan0"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("interface order = %v, want %v", gotOrder, wantOrder)
	}
	gotAddrs := state.Interfaces[1].IPv4
	wantAddrs := []netip.Addr{
		netip.MustParseAddr("10.0.0.2"),
		netip.MustParseAddr("10.0.0.20"),
	}
	if !reflect.DeepEqual(gotAddrs, wantAddrs) {
		t.Fatalf("canonical IPv4 = %v, want %v", gotAddrs, wantAddrs)
	}
}

func TestTransitionReasonsClassifyIndependentAxes(t *testing.T) {
	base := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityFull,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
		ifaceState(DeviceEthernet, "eth0", InterfaceRoleLAN, "10.0.0.20"),
	)
	base.DefaultRouteKnown = true
	base.DefaultRouteIface = "wlan0"
	base.DNSDefaultKnown = true
	base.DNSDefaultIface = "wlan0"

	t.Run("role only", func(t *testing.T) {
		cur := base
		cur.Interfaces = cloneNetworkInterfaceStates(base.Interfaces)
		cur.Interfaces[1].Role = InterfaceRoleNotConnected
		assertReasons(t, transitionReasons(base, cur), ReasonInterfaceRolesChanged)
	})

	t.Run("address only", func(t *testing.T) {
		cur := base
		cur.Interfaces = cloneNetworkInterfaceStates(base.Interfaces)
		cur.Interfaces[1].IPv4 = []netip.Addr{netip.MustParseAddr("10.0.0.99")}
		assertReasons(t, transitionReasons(base, cur), ReasonInterfaceAddressesChanged)
	})

	t.Run("route and dns", func(t *testing.T) {
		cur := base
		cur.DefaultRouteIface = "eth0"
		cur.DNSDefaultIface = "eth0"
		assertReasons(t, transitionReasons(base, cur), ReasonDefaultRouteChanged, ReasonDNSDefaultChanged)
	})

	t.Run("partial route and dns observation", func(t *testing.T) {
		cur := base
		cur.DefaultRouteObserved = false
		cur.DefaultRouteKnown = false
		cur.DefaultRouteIface = ""
		cur.DNSDefaultObserved = false
		cur.DNSDefaultKnown = false
		cur.DNSDefaultIface = ""
		assertReasons(t, transitionReasons(base, cur))
	})

	t.Run("known route and dns loss", func(t *testing.T) {
		cur := base
		cur.DefaultRouteObserved = true
		cur.DefaultRouteKnown = false
		cur.DefaultRouteIface = ""
		cur.DNSDefaultObserved = true
		cur.DNSDefaultKnown = false
		cur.DNSDefaultIface = ""
		assertReasons(t, transitionReasons(base, cur), ReasonDefaultRouteChanged, ReasonDNSDefaultChanged)
	})

	t.Run("active uplink and ap", func(t *testing.T) {
		cur := base
		cur.ActiveUplink = UplinkEthernet
		cur.ActiveUplinkIface = "eth0"
		cur.APActive = true
		assertReasons(t, transitionReasons(base, cur), ReasonActiveUplinkChanged, ReasonAPModeChanged)
	})
}

func TestTransitionStoreCoalescesReasonsAfterLastIngested(t *testing.T) {
	store := newNetworkTransitionStore()
	base := transitionStateWithInterfaces(
		UplinkEthernet,
		"eth0",
		ConnectivityFull,
		ifaceState(DeviceEthernet, "eth0", InterfaceRoleWANLAN, "10.0.0.10"),
	)
	if _, changed, _ := store.record(base); changed {
		t.Fatal("initial baseline should not publish a transition")
	}

	down := base
	down.ActiveUplink = UplinkNone
	down.ActiveUplinkIface = ""
	down.Connectivity = ConnectivityNone
	down.Interfaces = cloneNetworkInterfaceStates(base.Interfaces)
	down.Interfaces[0].Role = InterfaceRoleLAN
	if _, changed, _ := store.record(down); !changed {
		t.Fatal("down transition not recorded")
	}

	recovered := down
	recovered.Connectivity = ConnectivityLimited
	if _, changed, _ := store.record(recovered); !changed {
		t.Fatal("connectivity transition not recorded")
	}

	delta, ok := store.deltaSince(1)
	if !ok {
		t.Fatal("deltaSince(1) returned false")
	}
	if !delta.Coalesced {
		t.Fatal("delta should be coalesced")
	}
	assertReasons(t, delta.Reasons,
		ReasonActiveUplinkChanged,
		ReasonInterfaceRolesChanged,
		ReasonConnectivityChanged,
	)
	if delta.ToGeneration != 3 {
		t.Fatalf("ToGeneration = %d, want 3", delta.ToGeneration)
	}
	if delta.Current.Connectivity != ConnectivityLimited {
		t.Fatalf("Current connectivity = %s, want limited", delta.Current.Connectivity)
	}
}

func TestTransitionStorePreservesRouteDNSBaselineAcrossUnknownObservation(t *testing.T) {
	store := newNetworkTransitionStore()
	base := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityFull,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
	)
	base.DefaultRouteKnown = true
	base.DefaultRouteIface = "wlan0"
	base.DNSDefaultKnown = true
	base.DNSDefaultIface = "wlan0"
	store.record(base)

	unknown := base
	unknown.DefaultRouteObserved = false
	unknown.DefaultRouteKnown = false
	unknown.DefaultRouteIface = ""
	unknown.DNSDefaultObserved = false
	unknown.DNSDefaultKnown = false
	unknown.DNSDefaultIface = ""
	unknown.Connectivity = ConnectivityLimited
	if _, changed, _ := store.record(unknown); !changed {
		t.Fatal("connectivity transition not recorded")
	}

	ethernet := base
	ethernet.ActiveUplink = UplinkEthernet
	ethernet.ActiveUplinkIface = "eth0"
	ethernet.DefaultRouteIface = "eth0"
	ethernet.DNSDefaultIface = "eth0"
	ethernet.Interfaces = []NetworkInterfaceState{
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleLAN, "192.168.1.20"),
		ifaceState(DeviceEthernet, "eth0", InterfaceRoleWANLAN, "10.0.0.20"),
	}
	event, changed, _ := store.record(ethernet)
	if !changed {
		t.Fatal("ethernet transition not recorded")
	}
	assertReasons(t, event.Reasons,
		ReasonActiveUplinkChanged,
		ReasonDefaultRouteChanged,
		ReasonDNSDefaultChanged,
		ReasonInterfaceRolesChanged,
		ReasonInterfaceAddressesChanged,
		ReasonConnectivityChanged,
	)
}

func TestTransitionStoreRecordsKnownDefaultRouteLoss(t *testing.T) {
	store := newNetworkTransitionStore()
	base := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityLimited,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
	)
	base.DefaultRouteObserved = true
	base.DefaultRouteKnown = true
	base.DefaultRouteIface = "wlan0"
	base.DNSDefaultObserved = true
	base.DNSDefaultKnown = true
	base.DNSDefaultIface = "wlan0"
	store.record(base)

	noDefault := base
	noDefault.DefaultRouteObserved = true
	noDefault.DefaultRouteKnown = false
	noDefault.DefaultRouteIface = ""
	noDefault.DNSDefaultObserved = true
	noDefault.DNSDefaultKnown = false
	noDefault.DNSDefaultIface = ""
	event, changed, _ := store.record(noDefault)
	if !changed {
		t.Fatal("known default-route loss should be recorded")
	}
	assertReasons(t, event.Reasons, ReasonDefaultRouteChanged, ReasonDNSDefaultChanged)
	if event.Current.DefaultRouteKnown || event.Current.DefaultRouteIface != "" {
		t.Fatalf("current default route = (%v,%q), want known absent", event.Current.DefaultRouteKnown, event.Current.DefaultRouteIface)
	}
}

func TestTransitionStoreWakesWhenRouteDNSFactsBecomeKnown(t *testing.T) {
	store := newNetworkTransitionStore()
	unknown := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityFull,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
		ifaceState(DeviceEthernet, "eth0", InterfaceRoleLAN, "10.0.0.20"),
	)
	unknown.DefaultRouteKnown = false
	unknown.DNSDefaultKnown = false
	store.record(unknown)
	wakes := 0
	cancel := store.subscribeWake(func() { wakes++ })
	defer cancel()

	known := unknown
	known.DefaultRouteKnown = true
	known.DefaultRouteIface = "wlan0"
	known.DNSDefaultKnown = true
	known.DNSDefaultIface = "wlan0"
	event, changed, callbacks := store.record(known)
	if !changed {
		t.Fatal("gaining route/DNS facts should publish a wakeable transition")
	}
	assertReasons(t, event.Reasons, ReasonRouteDNSObservationChanged)
	for _, wake := range callbacks {
		wake()
	}
	if wakes != 1 {
		t.Fatalf("wake callbacks = %d, want 1", wakes)
	}
	delta, ok := store.deltaSince(1)
	if !ok {
		t.Fatal("deltaSince(1) returned false after route/DNS knowledge gain")
	}
	assertReasons(t, delta.Reasons, ReasonRouteDNSObservationChanged)
	if !delta.Current.DefaultRouteKnown || delta.Current.DefaultRouteIface != "wlan0" {
		t.Fatalf("delta current default route = (%v,%q), want wlan0 known", delta.Current.DefaultRouteKnown, delta.Current.DefaultRouteIface)
	}

	changedDNS := known
	changedDNS.DNSDefaultIface = "eth0"
	event, changed, _ = store.record(changedDNS)
	if !changed {
		t.Fatal("DNS default change after silent baseline was not recorded")
	}
	assertReasons(t, event.Reasons, ReasonDNSDefaultChanged)
}

func TestTransitionStoreReportsHistoryOverflow(t *testing.T) {
	store := newNetworkTransitionStore()
	base := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityFull,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
	)
	store.record(base)

	for i := 0; i < retainedNetworkTransitionHistory+3; i++ {
		next := base
		next.At = base.At.Add(time.Duration(i+1) * time.Second)
		next.Interfaces = []NetworkInterfaceState{
			ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
		}
		next.Interfaces[0].IPv4 = []netip.Addr{netip.MustParseAddr("192.168.1." + strconv.Itoa(20+i%100))}
		store.record(next)
	}

	delta, ok := store.deltaSince(1)
	if !ok {
		t.Fatal("deltaSince(1) returned false")
	}
	if !containsReason(delta.Reasons, ReasonHistoryOverflow) {
		t.Fatalf("Reasons = %v, want history_overflow", delta.Reasons)
	}
	if !delta.Coalesced {
		t.Fatal("overflow delta should be coalesced")
	}
}

func TestTransitionStoreReportsHistoryOverflowForDelayedFirstIngestion(t *testing.T) {
	store := newNetworkTransitionStore()
	base := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityFull,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
	)
	store.record(base)

	for i := 0; i < retainedNetworkTransitionHistory+3; i++ {
		next := base
		next.Interfaces = []NetworkInterfaceState{
			ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
		}
		next.Interfaces[0].IPv4 = []netip.Addr{netip.MustParseAddr("192.168.1." + strconv.Itoa(20+i%100))}
		store.record(next)
	}

	delta, ok := store.deltaSince(0)
	if !ok {
		t.Fatal("deltaSince(0) returned false")
	}
	if !containsReason(delta.Reasons, ReasonHistoryOverflow) {
		t.Fatalf("Reasons = %v, want history_overflow", delta.Reasons)
	}
}

func TestRoleForInterfaceKeepsLANOnlySeparateFromActiveUplink(t *testing.T) {
	wan := transitionInterfaceProbe{kind: DeviceWiFi, iface: "wlan0", linkUp: true, hasIP: true, ipv4: []netip.Addr{netip.MustParseAddr("192.168.1.20")}}
	publicWAN := transitionInterfaceProbe{kind: DeviceWiFi, iface: "wwan0", linkUp: true, hasIP: true, ipv4: []netip.Addr{netip.MustParseAddr("203.0.113.20")}}
	ipv6LinkLocalWAN := transitionInterfaceProbe{
		kind:   DeviceWiFi,
		iface:  "wwan0",
		linkUp: true,
		hasIP:  true,
		ipv6: []netip.Addr{
			netip.MustParseAddr("2001:4860:4860::8888"),
			netip.MustParseAddr("fe80::1"),
		},
		info: &nmclient.ActiveConnectionInfo{IP6HasDefaultRoute: true},
	}
	ipv6ULAWAN := transitionInterfaceProbe{
		kind:   DeviceWiFi,
		iface:  "wwan0",
		linkUp: true,
		hasIP:  true,
		ipv6:   []netip.Addr{netip.MustParseAddr("fd00::20")},
		info:   &nmclient.ActiveConnectionInfo{IP6HasDefaultRoute: true},
	}
	lan := transitionInterfaceProbe{kind: DeviceEthernet, iface: "eth0", linkUp: true, hasIP: true, ipv4: []netip.Addr{netip.MustParseAddr("10.0.0.20")}}
	secondaryWAN := transitionInterfaceProbe{kind: DeviceEthernet, iface: "usb0", linkUp: true, hasIP: true, info: &nmclient.ActiveConnectionInfo{IP4HasDefaultRoute: true}}
	secondaryWANLAN := transitionInterfaceProbe{
		kind:   DeviceEthernet,
		iface:  "usb0",
		linkUp: true,
		hasIP:  true,
		ipv4:   []netip.Addr{netip.MustParseAddr("10.0.0.20")},
		info:   &nmclient.ActiveConnectionInfo{IP4HasDefaultRoute: true},
	}
	unknown := transitionInterfaceProbe{kind: DeviceEthernet, iface: "dock0", linkUp: true, unknown: true}
	down := transitionInterfaceProbe{kind: DeviceEthernet, iface: "usb0", linkUp: false, hasIP: false}

	if got := roleForInterface(wan, "wlan0", "wlan0", true, ConnectivityFull); got != InterfaceRoleWANLAN {
		t.Fatalf("wan role = %s, want %s", got, InterfaceRoleWANLAN)
	}
	if got := roleForInterface(publicWAN, "wwan0", "wwan0", true, ConnectivityFull); got != InterfaceRoleWAN {
		t.Fatalf("public wan role = %s, want %s", got, InterfaceRoleWAN)
	}
	if got := roleForInterface(publicWAN, "wwan0", "wwan0", true, ConnectivityNone); got != InterfaceRoleWAN {
		t.Fatalf("public wan outage role = %s, want %s", got, InterfaceRoleWAN)
	}
	if got := roleForInterface(publicWAN, "wwan0", "wwan0", true, ConnectivityPortal); got != InterfaceRoleWAN {
		t.Fatalf("public wan portal role = %s, want %s", got, InterfaceRoleWAN)
	}
	if got := roleForInterface(publicWAN, "", "", false, ConnectivityFull); got != InterfaceRoleUnknown {
		t.Fatalf("partial route role = %s, want %s", got, InterfaceRoleUnknown)
	}
	privateNoDefault := transitionInterfaceProbe{
		kind:        DeviceEthernet,
		iface:       "eth0",
		linkUp:      true,
		hasIP:       true,
		ipv4:        []netip.Addr{netip.MustParseAddr("10.0.0.20")},
		info:        &nmclient.ActiveConnectionInfo{IP4RoutesKnown: true},
		routesKnown: true,
	}
	if got := roleForInterface(privateNoDefault, "", "", false, ConnectivityFull); got != InterfaceRoleLAN {
		t.Fatalf("private no-default role = %s, want %s", got, InterfaceRoleLAN)
	}
	publicNoDefault := privateNoDefault
	publicNoDefault.ipv4 = []netip.Addr{netip.MustParseAddr("203.0.113.20")}
	if got := roleForInterface(publicNoDefault, "", "", false, ConnectivityFull); got != InterfaceRoleUnknown {
		t.Fatalf("public no-default role = %s, want %s", got, InterfaceRoleUnknown)
	}
	if got := roleForInterface(ipv6LinkLocalWAN, "wwan0", "wwan0", true, ConnectivityFull); got != InterfaceRoleWAN {
		t.Fatalf("ipv6 link-local wan role = %s, want %s", got, InterfaceRoleWAN)
	}
	if got := roleForInterface(ipv6ULAWAN, "wwan0", "wwan0", true, ConnectivityFull); got != InterfaceRoleWANLAN {
		t.Fatalf("ipv6 ula wan role = %s, want %s", got, InterfaceRoleWANLAN)
	}
	if got := roleForInterface(lan, "wlan0", "wlan0", true, ConnectivityFull); got != InterfaceRoleLAN {
		t.Fatalf("lan role = %s, want %s", got, InterfaceRoleLAN)
	}
	if got := roleForInterface(secondaryWAN, "wlan0", "wlan0", true, ConnectivityFull); got != InterfaceRoleWAN {
		t.Fatalf("secondary wan role = %s, want %s", got, InterfaceRoleWAN)
	}
	if got := roleForInterface(secondaryWANLAN, "wlan0", "wlan0", true, ConnectivityFull); got != InterfaceRoleWANLAN {
		t.Fatalf("secondary wan+lan role = %s, want %s", got, InterfaceRoleWANLAN)
	}
	if got := roleForInterface(unknown, "wlan0", "wlan0", true, ConnectivityFull); got != InterfaceRoleUnknown {
		t.Fatalf("unknown role = %s, want %s", got, InterfaceRoleUnknown)
	}
	if got := roleForInterface(down, "wlan0", "wlan0", true, ConnectivityFull); got != InterfaceRoleNotConnected {
		t.Fatalf("down role = %s, want %s", got, InterfaceRoleNotConnected)
	}
}

func TestInterfaceAddressReasonsIgnoreIPv6Churn(t *testing.T) {
	base := transitionStateWithInterfaces(
		UplinkWiFi,
		"wlan0",
		ConnectivityFull,
		ifaceState(DeviceWiFi, "wlan0", InterfaceRoleWANLAN, "192.168.1.20"),
	)
	base.Interfaces[0].IPv6 = []netip.Addr{netip.MustParseAddr("2001:db8::1")}
	cur := base
	cur.Interfaces = cloneNetworkInterfaceStates(base.Interfaces)
	cur.Interfaces[0].IPv6 = []netip.Addr{netip.MustParseAddr("2001:db8::2")}
	assertReasons(t, transitionReasons(base, cur))
}

func TestProbeTransitionProjectionIgnoresLANOnlyNameserversForDNSDefault(t *testing.T) {
	wifiPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/1")
	lanPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/2")
	stub := nmclient.NewStubClient()
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      wifiPath,
		Interface: "wlan0",
		State:     nmclient.NMDeviceStateActivated,
	}}
	stub.EthernetDevicesResult = []nmclient.EthernetDevice{{
		Path:      lanPath,
		Interface: "eth0",
		State:     nmclient.NMDeviceStateActivated,
		Carrier:   true,
	}}
	stub.ActiveConnByDevice = map[dbus.ObjectPath]*nmclient.ActiveConnectionInfo{
		wifiPath: {
			IP4Addresses:       []string{"192.168.1.20"},
			IP4Nameservers:     []string{"8.8.8.8"},
			IP4HasDefaultRoute: true,
			RouteMetric:        100,
		},
		lanPath: {
			IP4Addresses:   []string{"10.0.0.20"},
			IP4Nameservers: []string{"10.0.0.1"},
			IP4RoutesKnown: true,
			RouteMetric:    0,
		},
	}
	proj := (&Prober{nm: stub}).probeTransitionProjection(ConnectivityFull)
	if !proj.dnsDefaultKnown || proj.dnsDefaultIface != "wlan0" {
		t.Fatalf("dns default = (%v,%q), want known wlan0", proj.dnsDefaultKnown, proj.dnsDefaultIface)
	}
}

func TestProbeTransitionProjectionFallsBackOnPartialDeviceEnumerationFailure(t *testing.T) {
	wifiPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/1")
	stub := nmclient.NewStubClient()
	stub.WiFiDevicesResult = []nmclient.WiFiDevice{{
		Path:      wifiPath,
		Interface: "wlan0",
		State:     nmclient.NMDeviceStateActivated,
	}}
	stub.EthernetDevicesErr = errors.New("dbus unavailable")
	stub.ActiveConnByDevice = map[dbus.ObjectPath]*nmclient.ActiveConnectionInfo{
		wifiPath: {
			IP4Addresses:       []string{"192.168.1.20"},
			IP4HasDefaultRoute: true,
		},
	}

	proj := (&Prober{nm: stub}).probeTransitionProjection(ConnectivityFull)
	if len(proj.interfaces) != 0 {
		t.Fatalf("projection interfaces = %v, want fallback-empty on partial enumeration failure", proj.interfaces)
	}
	if proj.interfacesObserved {
		t.Fatal("interfacesObserved = true, want false on partial enumeration failure")
	}
	if proj.defaultRouteKnown || proj.activeUplinkIface != "" {
		t.Fatalf("projection route/uplink = (%v,%q), want unknown fallback", proj.defaultRouteKnown, proj.activeUplinkIface)
	}
}

func TestProbeTransitionProjectionMarksConnectedInterfaceUnknownWithoutRouteProof(t *testing.T) {
	usbPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/2")
	stub := nmclient.NewStubClient()
	stub.EthernetDevicesResult = []nmclient.EthernetDevice{{
		Path:      usbPath,
		Interface: "usb0",
		State:     nmclient.NMDeviceStateActivated,
		Carrier:   true,
	}}
	stub.ActiveConnByDevice = map[dbus.ObjectPath]*nmclient.ActiveConnectionInfo{
		usbPath: {
			IP4Addresses: []string{"203.0.113.20"},
			IP4Gateway:   "203.0.113.1",
		},
	}

	proj := (&Prober{nm: stub}).probeTransitionProjection(ConnectivityFull)
	if proj.defaultRouteKnown {
		t.Fatalf("defaultRouteKnown = true, want false without route proof")
	}
	if proj.defaultRouteObserved {
		t.Fatalf("defaultRouteObserved = true, want false without route proof")
	}
	if len(proj.interfaces) != 1 {
		t.Fatalf("interfaces = %v, want one interface", proj.interfaces)
	}
	if got := proj.interfaces[0].Role; got != InterfaceRoleUnknown {
		t.Fatalf("role = %s, want %s", got, InterfaceRoleUnknown)
	}
}

func TestProbeTransitionProjectionClassifiesObservedPrivateNoDefaultAsLAN(t *testing.T) {
	lanPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/2")
	stub := nmclient.NewStubClient()
	stub.EthernetDevicesResult = []nmclient.EthernetDevice{{
		Path:      lanPath,
		Interface: "eth0",
		State:     nmclient.NMDeviceStateActivated,
		Carrier:   true,
	}}
	stub.ActiveConnByDevice = map[dbus.ObjectPath]*nmclient.ActiveConnectionInfo{
		lanPath: {
			IP4Addresses:   []string{"10.0.0.20"},
			IP4Gateway:     "10.0.0.1",
			IP4RoutesKnown: true,
		},
	}

	proj := (&Prober{nm: stub}).probeTransitionProjection(ConnectivityNone)
	if !proj.interfacesObserved {
		t.Fatal("interfacesObserved = false, want true for complete projection")
	}
	if proj.defaultRouteKnown {
		t.Fatalf("defaultRouteKnown = true, want false for never-default LAN")
	}
	if !proj.defaultRouteObserved {
		t.Fatalf("defaultRouteObserved = false, want true for observed never-default LAN")
	}
	if len(proj.interfaces) != 1 {
		t.Fatalf("interfaces = %v, want one interface", proj.interfaces)
	}
	if got := proj.interfaces[0].Role; got != InterfaceRoleLAN {
		t.Fatalf("role = %s, want %s", got, InterfaceRoleLAN)
	}
}

func TestBuildNetworkTransitionStateMarksLegacyConnectedInterfacesUnknown(t *testing.T) {
	tick := Tick{
		Devices: map[DeviceKind]DeviceObservation{
			DeviceWiFi: {
				Kind:    DeviceWiFi,
				Present: true,
				Iface:   "wlan0",
				LinkUp:  true,
				HasIP:   true,
			},
			DeviceEthernet: {
				Kind:    DeviceEthernet,
				Present: true,
				Iface:   "eth0",
				LinkUp:  false,
				HasIP:   false,
			},
		},
		ActiveUplink:      UplinkWiFi,
		ActiveUplinkIface: "wlan0",
		DefaultRouteKnown: true,
		DefaultRouteIface: "wlan0",
		NMConn:            ConnectivityFull,
	}

	state := buildNetworkTransitionState(tick, Snapshot{Connectivity: ConnectivityFull})
	if state.InterfacesObserved {
		t.Fatal("interfacesObserved = true, want false for legacy fallback")
	}
	roles := map[string]NetworkInterfaceRole{}
	for _, iface := range state.Interfaces {
		roles[iface.Iface] = iface.Role
	}
	if roles["wlan0"] != InterfaceRoleUnknown {
		t.Fatalf("wlan0 role = %s, want unknown", roles["wlan0"])
	}
	if roles["eth0"] != InterfaceRoleNotConnected {
		t.Fatalf("eth0 role = %s, want not_connected", roles["eth0"])
	}
}

func TestBuildNetworkTransitionStatePreservesObservedEmptyInterfaceProjection(t *testing.T) {
	tick := Tick{
		Devices: map[DeviceKind]DeviceObservation{
			DeviceWiFi: {
				Kind:    DeviceWiFi,
				Present: true,
				Iface:   "wlan0",
				LinkUp:  true,
				HasIP:   true,
			},
		},
		InterfacesObserved: true,
		ActiveUplink:       UplinkWiFi,
		ActiveUplinkIface:  "wlan0",
		NMConn:             ConnectivityFull,
	}

	state := buildNetworkTransitionState(tick, Snapshot{Connectivity: ConnectivityFull})
	if !state.InterfacesObserved {
		t.Fatal("interfacesObserved = false, want true for completed empty projection")
	}
	if len(state.Interfaces) != 0 {
		t.Fatalf("interfaces = %v, want no legacy fallback when projection completed empty", state.Interfaces)
	}
}

func TestTransitionProbeForDeviceMarksActiveConnectionFailureUnknown(t *testing.T) {
	path := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/1")
	stub := nmclient.NewStubClient()
	stub.ActiveConnErrByDevice = map[dbus.ObjectPath]error{path: errors.New("dbus unavailable")}
	probe := (&Prober{nm: stub}).transitionProbeForDevice(DeviceEthernet, path, "eth0", true)
	if !probe.unknown {
		t.Fatal("probe should be marked unknown")
	}
	if got := roleForInterface(probe, "eth0", "eth0", true, ConnectivityFull); got != InterfaceRoleUnknown {
		t.Fatalf("role = %s, want %s", got, InterfaceRoleUnknown)
	}
}

func transitionStateWithInterfaces(uplink UplinkType, iface string, conn Connectivity, ifaces ...NetworkInterfaceState) NetworkTransitionState {
	return canonicalNetworkTransitionState(NetworkTransitionState{
		ActiveUplink:       uplink,
		ActiveUplinkIface:  iface,
		Connectivity:       conn,
		Interfaces:         ifaces,
		InterfacesObserved: true,
		At:                 time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
	})
}

func ifaceState(kind DeviceKind, iface string, role NetworkInterfaceRole, ipv4 string) NetworkInterfaceState {
	state := NetworkInterfaceState{
		Kind:   kind,
		Iface:  iface,
		Role:   role,
		LinkUp: true,
		HasIP:  true,
	}
	if ipv4 != "" {
		state.IPv4 = []netip.Addr{netip.MustParseAddr(ipv4)}
	}
	return state
}

func assertReasons(t *testing.T, got []NetworkTransitionReason, want ...NetworkTransitionReason) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons = %v, want %v", got, want)
	}
}

func containsReason(reasons []NetworkTransitionReason, want NetworkTransitionReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
