package server

import (
	"net/netip"
	"testing"
	"time"

	"piccolod/internal/network"
	"piccolod/internal/remote"
)

func TestRemoteRelevantNetworkTransitionReasonsIgnoreLANOnlyAddressChurn(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.20"),
	)
	cur := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.99"),
	)

	reasons := remoteRelevantNetworkTransitionReasons(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceAddressesChanged},
		Previous: prev,
		Current:  cur,
	})
	if len(reasons) != 0 {
		t.Fatalf("reasons = %v, want none for LAN-only address churn", reasons)
	}
}

func TestRemoteRelevantNetworkTransitionReasonsIncludeActiveAddressChurn(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.20"),
	)
	cur := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.99"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.20"),
	)

	reasons := remoteRelevantNetworkTransitionReasons(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceAddressesChanged},
		Previous: prev,
		Current:  cur,
	})
	if len(reasons) != 1 || reasons[0] != network.ReasonInterfaceAddressesChanged {
		t.Fatalf("reasons = %v, want active address change", reasons)
	}
}

func TestRemoteRelevantNetworkTransitionReasonsIncludeCoalescedActiveTouch(t *testing.T) {
	state := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.20"),
	)

	reasons := remoteRelevantNetworkTransitionReasons(network.NetworkTransitionDelta{
		Reasons:           []network.NetworkTransitionReason{network.ReasonInterfaceAddressesChanged},
		Previous:          state,
		Current:           state,
		Coalesced:         true,
		TouchedInterfaces: []string{"wlan0"},
	})
	if len(reasons) != 1 || reasons[0] != network.ReasonInterfaceAddressesChanged {
		t.Fatalf("reasons = %v, want coalesced active touch", reasons)
	}
}

func TestRemoteRelevantNetworkTransitionReasonsIgnoreCoalescedLANOnlyTouch(t *testing.T) {
	state := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.20"),
	)

	reasons := remoteRelevantNetworkTransitionReasons(network.NetworkTransitionDelta{
		Reasons:           []network.NetworkTransitionReason{network.ReasonInterfaceAddressesChanged},
		Previous:          state,
		Current:           state,
		Coalesced:         true,
		TouchedInterfaces: []string{"eth0"},
	})
	if len(reasons) != 0 {
		t.Fatalf("reasons = %v, want none for coalesced LAN-only touch", reasons)
	}
}

func TestRemoteRelevantNetworkTransitionReasonsIgnoreUnknownActiveObservationChurn(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
	)
	cur := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		network.NetworkInterfaceState{
			Kind:   network.DeviceWiFi,
			Iface:  "wlan0",
			Role:   network.InterfaceRoleUnknown,
			LinkUp: true,
			HasIP:  false,
		},
	)

	reasons := remoteRelevantNetworkTransitionReasons(network.NetworkTransitionDelta{
		Reasons: []network.NetworkTransitionReason{
			network.ReasonInterfaceRolesChanged,
			network.ReasonInterfaceAddressesChanged,
		},
		Previous: prev,
		Current:  cur,
	})
	if len(reasons) != 0 {
		t.Fatalf("reasons = %v, want none for unknown active observation churn", reasons)
	}
}

func TestNetworkTransitionRemoteUsableUsesKnownDefaultRouteWhenLegacyActiveIsNone(t *testing.T) {
	state := network.NetworkTransitionState{
		ActiveUplink:         network.UplinkNone,
		Connectivity:         network.ConnectivityLimited,
		DefaultRouteObserved: true,
		DefaultRouteKnown:    true,
		DefaultRouteIface:    "usb0",
	}
	if !networkTransitionRemoteUsable(state) {
		t.Fatal("known default route should be usable even when legacy active uplink is none")
	}
	state.DefaultRouteKnown = false
	state.DefaultRouteIface = ""
	if networkTransitionRemoteUsable(state) {
		t.Fatal("observed no-default-route state should not be usable")
	}
	state.DefaultRouteObserved = false
	state.ActiveUplink = network.UplinkWiFi
	if !networkTransitionRemoteUsable(state) {
		t.Fatal("unavailable route projection should fall back to legacy active uplink")
	}
}

func TestServicePublicationRelevantNetworkTransitionRequiresProvenLANSurfaceAndLocalReason(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
	)
	cur := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleLAN, "192.168.1.20"),
	)
	if !servicePublicationRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  cur,
	}) {
		t.Fatal("LAN role transition should be publication-relevant")
	}
	if servicePublicationRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonConnectivityChanged},
		Previous: prev,
		Current:  cur,
	}) {
		t.Fatal("connectivity-only transition should not be publication-relevant")
	}
	noLAN := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWAN, "203.0.113.10"),
	)
	if servicePublicationRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  noLAN,
	}) {
		t.Fatal("WAN-only current state should not be publication-relevant")
	}
	unknownOnly := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleUnknown, "192.168.1.20"),
	)
	if servicePublicationRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  unknownOnly,
	}) {
		t.Fatal("unknown-only current state should not reopen firewall publication")
	}
}

func TestServicePublicationModeClosesWhenIngressApplicabilityIsUnsafe(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
	)
	mixedWAN := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "usb0", network.InterfaceRoleWAN, "203.0.113.10"),
	)
	if got := servicePublicationMode(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  mixedWAN,
	}); got != networkPublicationClose {
		t.Fatalf("publication mode = %v, want close for mixed LAN/WAN", got)
	}
	mixedUnknown := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		network.NetworkInterfaceState{
			Kind:   network.DeviceEthernet,
			Iface:  "usb0",
			Role:   network.InterfaceRoleUnknown,
			LinkUp: true,
		},
	)
	if got := servicePublicationMode(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  mixedUnknown,
	}); got != networkPublicationClose {
		t.Fatalf("publication mode = %v, want close for unknown ingress", got)
	}
}

func TestMDNSRelevantNetworkTransitionWakesOnUnknownSurface(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
	)
	cur := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleUnknown, "192.168.1.20"),
	)
	if !mdnsRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  cur,
	}) {
		t.Fatal("unknown role should remain mDNS-relevant for owner cleanup")
	}
}

func TestMDNSRelevantNetworkTransitionWakesOnLANLoss(t *testing.T) {
	prev := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
	)
	cur := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWAN, "203.0.113.10"),
	)
	if !mdnsRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  cur,
	}) {
		t.Fatal("mDNS should reconcile when the last LAN-capable interface is lost")
	}
	if servicePublicationRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons:  []network.NetworkTransitionReason{network.ReasonInterfaceRolesChanged},
		Previous: prev,
		Current:  cur,
	}) {
		t.Fatal("firewall publication should not reapply after LAN surface is lost")
	}
}

func TestMDNSRelevantNetworkTransitionWakesOnHistoryOverflowWithoutRetainedLANSurface(t *testing.T) {
	if !mdnsRelevantNetworkTransition(network.NetworkTransitionDelta{
		Reasons: []network.NetworkTransitionReason{network.ReasonHistoryOverflow},
		Previous: testTransitionState(
			"wlan0",
			network.ConnectivityFull,
			testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWAN, "203.0.113.10"),
		),
		Current: testTransitionState(
			"wlan0",
			network.ConnectivityFull,
			testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWAN, "203.0.113.10"),
		),
	}) {
		t.Fatal("history overflow should force mDNS reconciliation even without retained LAN surface")
	}
}

func TestMDNSPublishPolicySeparatesProvenLANAndUnknownSurfaces(t *testing.T) {
	state := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
		testIface(network.DeviceEthernet, "eth0", network.InterfaceRoleLAN, "10.0.0.20"),
		testIface(network.DeviceEthernet, "usb0", network.InterfaceRoleWAN, "203.0.113.10"),
		network.NetworkInterfaceState{
			Kind:   network.DeviceWiFi,
			Iface:  "wlan1",
			Role:   network.InterfaceRoleUnknown,
			LinkUp: true,
			HasIP:  false,
		},
	)

	gotPublish, gotPreserve, ok := mdnsPublishPolicy(state)
	if !ok {
		t.Fatal("mdnsPublishPolicy returned ok=false for observed interface projection")
	}
	wantPublish := []string{"eth0", "wlan0"}
	wantPreserve := []string{"wlan1"}
	if len(gotPublish) != len(wantPublish) {
		t.Fatalf("publish interfaces = %v, want %v", gotPublish, wantPublish)
	}
	for i := range wantPublish {
		if gotPublish[i] != wantPublish[i] {
			t.Fatalf("publish interfaces = %v, want %v", gotPublish, wantPublish)
		}
	}
	if len(gotPreserve) != len(wantPreserve) {
		t.Fatalf("preserve interfaces = %v, want %v", gotPreserve, wantPreserve)
	}
	for i := range wantPreserve {
		if gotPreserve[i] != wantPreserve[i] {
			t.Fatalf("preserve interfaces = %v, want %v", gotPreserve, wantPreserve)
		}
	}
}

func TestMDNSPublishPolicySkipsIncompleteProjection(t *testing.T) {
	state := testTransitionState(
		"wlan0",
		network.ConnectivityFull,
		testIface(network.DeviceWiFi, "wlan0", network.InterfaceRoleWANLAN, "192.168.1.20"),
	)
	state.InterfacesObserved = false

	gotPublish, gotPreserve, ok := mdnsPublishPolicy(state)
	if ok {
		t.Fatal("mdnsPublishPolicy ok=true, want false for incomplete projection")
	}
	if gotPublish != nil || gotPreserve != nil {
		t.Fatalf("policy = (%v, %v), want nil policies for incomplete projection", gotPublish, gotPreserve)
	}
}

func TestNetworkTransitionRetryMergesReasonsAndExpires(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	var retry networkTransitionRetry
	retry.add([]network.NetworkTransitionReason{network.ReasonDefaultRouteChanged}, now)
	retry.add([]network.NetworkTransitionReason{network.ReasonDNSDefaultChanged}, now.Add(time.Second))
	if !retry.hasPending() {
		t.Fatal("retry should have pending reasons")
	}
	got := retry.reasons()
	want := []network.NetworkTransitionReason{
		network.ReasonDefaultRouteChanged,
		network.ReasonDNSDefaultChanged,
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("reasons = %v, want %v", got, want)
	}
	got[0] = network.ReasonHistoryOverflow
	if retry.reasons()[0] != network.ReasonDefaultRouteChanged {
		t.Fatal("reasons should return a defensive copy")
	}
	if retry.expired(now.Add(networkTransitionRecoveryWindow)) {
		t.Fatal("deadline should have refreshed on second add")
	}
	if !retry.expired(now.Add(networkTransitionRecoveryWindow + time.Second)) {
		t.Fatal("retry should expire after refreshed deadline")
	}
	if retry.hasAttempted() {
		t.Fatal("retry should not be marked attempted before attempt")
	}
	retry.markAttempted()
	if !retry.hasAttempted() {
		t.Fatal("retry should track attempted state")
	}
	retry.clear()
	if retry.hasPending() {
		t.Fatal("retry should be cleared")
	}
	if retry.hasAttempted() {
		t.Fatal("clear should reset attempted state")
	}
}

func TestNetworkTransitionConnectedRelayDoesNotSatisfyUnattemptedRetry(t *testing.T) {
	var retry networkTransitionRetry
	retry.add([]network.NetworkTransitionReason{network.ReasonDefaultRouteChanged}, time.Now())
	if networkTransitionRetrySatisfiedByConnectedRelay(false, &retry, true) {
		t.Fatal("connected relay should not satisfy pending work before a transition restart attempt")
	}
	if !retry.hasPending() {
		t.Fatal("pending reasons should remain before a transition restart attempt")
	}
	retry.markAttempted()
	if !networkTransitionRetrySatisfiedByConnectedRelay(false, &retry, true) {
		t.Fatal("connected relay should satisfy pending work after a transition restart attempt")
	}
	if networkTransitionRetrySatisfiedByConnectedRelay(true, &retry, true) {
		t.Fatal("forced network transition should not be satisfied by existing connected relay")
	}
}

func TestSelfHostedNetworkTransitionRetryKeepsConnectedPendingUntilAttempted(t *testing.T) {
	rm, err := remote.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("remote manager: %v", err)
	}
	defer rm.Close()
	rm.RelayEventHandler()(selfHostedRelayName, true, "")

	var retry networkTransitionRetry
	retry.add([]network.NetworkTransitionReason{network.ReasonDefaultRouteChanged}, time.Now())
	state := network.NetworkTransitionState{Connectivity: network.ConnectivityNone}

	if (&GinServer{}).attemptSelfHostedNetworkTransitionRetry(rm, &retry, state, false) {
		t.Fatal("unusable uplink should not attempt restart")
	}
	if !retry.hasPending() {
		t.Fatal("connected stale relay should not clear unattempted pending restart")
	}

	retry.markAttempted()
	if (&GinServer{}).attemptSelfHostedNetworkTransitionRetry(rm, &retry, state, false) {
		t.Fatal("connected relay satisfaction should not schedule another restart")
	}
	if retry.hasPending() {
		t.Fatal("connected relay should clear pending restart after an attempt")
	}
}

func testTransitionState(activeIface string, connectivity network.Connectivity, ifaces ...network.NetworkInterfaceState) network.NetworkTransitionState {
	return network.NetworkTransitionState{
		ActiveUplink:         network.UplinkWiFi,
		ActiveUplinkIface:    activeIface,
		Connectivity:         connectivity,
		DefaultRouteObserved: true,
		DefaultRouteKnown:    true,
		DefaultRouteIface:    activeIface,
		DNSDefaultObserved:   true,
		DNSDefaultKnown:      true,
		DNSDefaultIface:      activeIface,
		Interfaces:           ifaces,
		InterfacesObserved:   true,
	}
}

func testIface(kind network.DeviceKind, iface string, role network.NetworkInterfaceRole, ipv4 string) network.NetworkInterfaceState {
	return network.NetworkInterfaceState{
		Kind:   kind,
		Iface:  iface,
		Role:   role,
		LinkUp: true,
		HasIP:  true,
		IPv4:   []netip.Addr{netip.MustParseAddr(ipv4)},
	}
}
