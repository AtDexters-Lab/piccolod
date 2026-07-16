package server

import (
	"net/netip"
	"sync"
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

func TestNetworkTransitionRemoteUsableRequiresAuthoritativeActiveUplink(t *testing.T) {
	state := network.NetworkTransitionState{
		ActiveUplink:         network.UplinkEthernet,
		ActiveUplinkIface:    "usb0",
		Connectivity:         network.ConnectivityLimited,
		DefaultRouteObserved: true,
		DefaultRouteKnown:    true,
		DefaultRouteIface:    "usb0",
		InterfacesObserved:   true,
		Interfaces: []network.NetworkInterfaceState{
			{Kind: network.DeviceEthernet, Iface: "usb0", LinkUp: true, HasIP: true},
		},
	}
	if !networkTransitionRemoteUsable(state) {
		t.Fatal("matching active uplink and default route should be usable")
	}
	state.ActiveUplink = network.UplinkNone
	state.ActiveUplinkIface = ""
	if networkTransitionRemoteUsable(state) {
		t.Fatal("state without an authoritative active uplink should not be usable")
	}
	state.DefaultRouteObserved = false
	state.DefaultRouteKnown = false
	state.DefaultRouteIface = ""
	state.ActiveUplink = network.UplinkWiFi
	state.ActiveUplinkIface = "wlan0"
	if networkTransitionRemoteUsable(state) {
		t.Fatal("unavailable route projection must not claim a usable uplink")
	}
}

func TestSubscribeNetworkAvailabilityInitializesAfterGenerationOne(t *testing.T) {
	source := &testNetworkStateSource{
		state: network.NetworkTransitionState{
			ActiveUplink:         network.UplinkEthernet,
			ActiveUplinkIface:    "enp2s0",
			Connectivity:         network.ConnectivityFull,
			DefaultRouteObserved: true,
			DefaultRouteKnown:    true,
			DefaultRouteIface:    "enp2s0",
			InterfacesObserved:   true,
			Interfaces: []network.NetworkInterfaceState{
				{Kind: network.DeviceEthernet, Iface: "enp2s0", LinkUp: true, HasIP: true},
			},
		},
		generation: 1,
		ok:         true,
	}
	notified := make(chan struct{}, 1)
	srv := &GinServer{}
	srv.subscribeNetworkAvailability(source, func() { notified <- struct{}{} })
	defer srv.busUnsubs[0]()

	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("late availability subscriber did not consume current generation")
	}
}

func TestSubscribeNetworkAvailabilityNotifiesOnlyOnProvenGeneration(t *testing.T) {
	source := &testNetworkStateSource{
		state: network.NetworkTransitionState{
			ActiveUplink: network.UplinkNone,
			Connectivity: network.ConnectivityUnknown,
		},
		generation: 1,
		ok:         true,
	}
	notified := make(chan struct{}, 1)
	srv := &GinServer{}
	srv.subscribeNetworkAvailability(source, func() { notified <- struct{}{} })
	defer srv.busUnsubs[0]()

	select {
	case <-notified:
		t.Fatal("uncertain initial generation triggered availability")
	case <-time.After(50 * time.Millisecond):
	}

	source.setState(network.NetworkTransitionState{
		ActiveUplink:         network.UplinkEthernet,
		ActiveUplinkIface:    "enp2s0",
		Connectivity:         network.ConnectivityLimited,
		DefaultRouteObserved: true,
		DefaultRouteKnown:    true,
		DefaultRouteIface:    "enp2s0",
		InterfacesObserved:   true,
		Interfaces: []network.NetworkInterfaceState{
			{Kind: network.DeviceEthernet, Iface: "enp2s0", LinkUp: true, HasIP: true},
		},
	}, 2)
	source.wake()
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("later proven generation did not trigger availability")
	}

	// A duplicate wake without a new topology generation models signal-only
	// activity and must not retrigger identity or STUN availability work.
	source.wake()
	select {
	case <-notified:
		t.Fatal("duplicate generation retriggered availability")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeNetworkAvailabilityCancellationDropsQueuedWake(t *testing.T) {
	source := &testNetworkStateSource{
		state: network.NetworkTransitionState{
			ActiveUplink:         network.UplinkEthernet,
			ActiveUplinkIface:    "enp2s0",
			Connectivity:         network.ConnectivityFull,
			DefaultRouteObserved: true,
			DefaultRouteKnown:    true,
			DefaultRouteIface:    "enp2s0",
			InterfacesObserved:   true,
			Interfaces: []network.NetworkInterfaceState{
				{Kind: network.DeviceEthernet, Iface: "enp2s0", LinkUp: true, HasIP: true},
			},
		},
		generation: 1,
		ok:         true,
	}
	notified := make(chan struct{}, 2)
	srv := &GinServer{}
	srv.subscribeNetworkAvailability(source, func() { notified <- struct{}{} })

	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("initial availability notification missing")
	}
	entered, release := source.blockNextCurrent()
	source.setGeneration(2)
	source.wake()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("availability consumer did not begin reading queued generation")
	}
	cancelled := make(chan struct{})
	go func() {
		srv.busUnsubs[0]()
		close(cancelled)
	}()
	select {
	case <-source.unsubscribed():
	case <-time.After(time.Second):
		t.Fatal("availability cancellation did not unregister its wake")
	}
	close(release)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("availability cancellation did not wait for consumer exit")
	}

	select {
	case <-notified:
		t.Fatal("cancelled availability subscriber received a queued wake")
	case <-time.After(50 * time.Millisecond):
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

type testNetworkStateSource struct {
	mu         sync.Mutex
	state      network.NetworkTransitionState
	generation uint64
	ok         bool
	wakeFn     func()
	blockEnter chan struct{}
	blockExit  chan struct{}
	unsubCh    chan struct{}
}

func (s *testNetworkStateSource) CurrentNetworkState() (network.NetworkTransitionState, uint64, bool) {
	s.mu.Lock()
	state, generation, ok := s.state, s.generation, s.ok
	enter, exit := s.blockEnter, s.blockExit
	s.blockEnter = nil
	s.blockExit = nil
	s.mu.Unlock()
	if enter != nil {
		close(enter)
		<-exit
	}
	return state, generation, ok
}

func (s *testNetworkStateSource) SubscribeNetworkTransitionWake(wake func()) func() {
	s.mu.Lock()
	s.wakeFn = wake
	s.unsubCh = make(chan struct{})
	unsubCh := s.unsubCh
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.wakeFn = nil
		s.mu.Unlock()
		close(unsubCh)
	}
}

func (s *testNetworkStateSource) setGeneration(generation uint64) {
	s.mu.Lock()
	s.generation = generation
	s.mu.Unlock()
}

func (s *testNetworkStateSource) setState(state network.NetworkTransitionState, generation uint64) {
	s.mu.Lock()
	s.state = state
	s.generation = generation
	s.ok = true
	s.mu.Unlock()
}

func (s *testNetworkStateSource) wake() {
	s.mu.Lock()
	wake := s.wakeFn
	s.mu.Unlock()
	if wake != nil {
		wake()
	}
}

func (s *testNetworkStateSource) blockNextCurrent() (<-chan struct{}, chan struct{}) {
	enter := make(chan struct{})
	exit := make(chan struct{})
	s.mu.Lock()
	s.blockEnter = enter
	s.blockExit = exit
	s.mu.Unlock()
	return enter, exit
}

func (s *testNetworkStateSource) unsubscribed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unsubCh
}
