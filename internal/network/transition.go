package network

import (
	"net/netip"
	"reflect"
	"sort"
	"sync"
	"time"
)

const retainedNetworkTransitionHistory = 64

// NetworkInterfaceRole classifies how piccolod should treat an interface for
// reconciliation. Roles are per interface; active uplink is only the chosen
// egress path, not the complete LAN publication surface.
type NetworkInterfaceRole string

const (
	InterfaceRoleWANLAN       NetworkInterfaceRole = "wan_lan"
	InterfaceRoleWAN          NetworkInterfaceRole = "wan"
	InterfaceRoleLAN          NetworkInterfaceRole = "lan"
	InterfaceRoleNotConnected NetworkInterfaceRole = "not_connected"
	InterfaceRoleUnknown      NetworkInterfaceRole = "unknown"
	InterfaceRoleFiltered     NetworkInterfaceRole = "filtered"
)

// NetworkTransitionReason identifies the observed network transition axis.
type NetworkTransitionReason string

const (
	ReasonActiveUplinkChanged        NetworkTransitionReason = "active_uplink_changed"
	ReasonDefaultRouteChanged        NetworkTransitionReason = "default_route_changed"
	ReasonDNSDefaultChanged          NetworkTransitionReason = "dns_default_changed"
	ReasonRouteDNSObservationChanged NetworkTransitionReason = "route_dns_observation_changed"
	ReasonInterfaceRolesChanged      NetworkTransitionReason = "interface_roles_changed"
	ReasonInterfaceAddressesChanged  NetworkTransitionReason = "interface_addresses_changed"
	ReasonConnectivityChanged        NetworkTransitionReason = "connectivity_changed"
	ReasonAPModeChanged              NetworkTransitionReason = "ap_mode_changed"
	ReasonHistoryOverflow            NetworkTransitionReason = "history_overflow"
)

// NetworkInterfaceState is the stable per-interface projection consumed by
// transition owners. (Kind, Iface) is the identity; multiple interfaces with
// the same Kind are first-class.
type NetworkInterfaceState struct {
	Kind   DeviceKind           `json:"kind"`
	Iface  string               `json:"iface"`
	Role   NetworkInterfaceRole `json:"role"`
	LinkUp bool                 `json:"link_up"`
	HasIP  bool                 `json:"has_ip"`
	IPv4   []netip.Addr         `json:"ipv4,omitempty"`
	IPv6   []netip.Addr         `json:"ipv6,omitempty"`
}

// NetworkTransitionState is the current network reconciliation substrate.
type NetworkTransitionState struct {
	ActiveUplink         UplinkType              `json:"active_uplink"`
	ActiveUplinkIface    string                  `json:"active_uplink_iface,omitempty"`
	Connectivity         Connectivity            `json:"connectivity"`
	APActive             bool                    `json:"ap_active"`
	DefaultRouteIface    string                  `json:"default_route_iface,omitempty"`
	DefaultRouteObserved bool                    `json:"default_route_observed"`
	DefaultRouteKnown    bool                    `json:"default_route_known"`
	DNSDefaultIface      string                  `json:"dns_default_iface,omitempty"`
	DNSDefaultObserved   bool                    `json:"dns_default_observed"`
	DNSDefaultKnown      bool                    `json:"dns_default_known"`
	Interfaces           []NetworkInterfaceState `json:"interfaces,omitempty"`
	InterfacesObserved   bool                    `json:"interfaces_observed"`
	At                   time.Time               `json:"at,omitempty"`
}

// NetworkTransitionEvent is published to the event bus as a diagnostic wake.
// Consumers that require reliable delivery should call TransitionDeltaSince.
type NetworkTransitionEvent struct {
	Generation        uint64                    `json:"generation"`
	Reasons           []NetworkTransitionReason `json:"reasons"`
	Previous          NetworkTransitionState    `json:"previous"`
	Current           NetworkTransitionState    `json:"current"`
	TouchedInterfaces []string                  `json:"touched_interfaces,omitempty"`
}

// NetworkTransitionDelta coalesces retained events after a consumer's last
// ingested generation.
type NetworkTransitionDelta struct {
	FromGeneration    uint64                    `json:"from_generation"`
	ToGeneration      uint64                    `json:"to_generation"`
	Reasons           []NetworkTransitionReason `json:"reasons"`
	Previous          NetworkTransitionState    `json:"previous"`
	Current           NetworkTransitionState    `json:"current"`
	Coalesced         bool                      `json:"coalesced"`
	TouchedInterfaces []string                  `json:"touched_interfaces,omitempty"`
}

// NetworkTransitionSource is the owner-facing retained transition API.
type NetworkTransitionSource interface {
	TransitionDeltaSince(lastIngested uint64) (NetworkTransitionDelta, bool)
	SubscribeNetworkTransitionWake(func()) func()
}

type networkTransitionStore struct {
	mu         sync.Mutex
	generation uint64
	latest     *NetworkTransitionState
	history    []NetworkTransitionEvent
	watches    map[uint64]func()
	nextWatch  uint64
}

func newNetworkTransitionStore() *networkTransitionStore {
	return &networkTransitionStore{
		watches: make(map[uint64]func()),
	}
}

func (s *networkTransitionStore) record(state NetworkTransitionState) (NetworkTransitionEvent, bool, []func()) {
	state = canonicalNetworkTransitionState(state)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.latest == nil {
		s.generation = 1
		s.latest = cloneNetworkTransitionStatePtr(state)
		return NetworkTransitionEvent{}, false, nil
	}

	state = preserveLastKnownRouteDNS(*s.latest, state)
	reasons := transitionReasons(*s.latest, state)
	if len(reasons) == 0 {
		if routeDNSKnowledgeGained(*s.latest, state) {
			reasons = []NetworkTransitionReason{ReasonRouteDNSObservationChanged}
		} else {
			return NetworkTransitionEvent{}, false, nil
		}
	}

	s.generation++
	event := NetworkTransitionEvent{
		Generation:        s.generation,
		Reasons:           reasons,
		Previous:          cloneNetworkTransitionState(*s.latest),
		Current:           cloneNetworkTransitionState(state),
		TouchedInterfaces: transitionTouchedInterfaces(*s.latest, state),
	}
	s.latest = cloneNetworkTransitionStatePtr(state)
	s.history = append(s.history, event)
	if len(s.history) > retainedNetworkTransitionHistory {
		copy(s.history, s.history[len(s.history)-retainedNetworkTransitionHistory:])
		s.history = s.history[:retainedNetworkTransitionHistory]
	}

	watches := make([]func(), 0, len(s.watches))
	for _, cb := range s.watches {
		watches = append(watches, cb)
	}
	return event, true, watches
}

func (s *networkTransitionStore) deltaSince(lastIngested uint64) (NetworkTransitionDelta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.latest == nil || len(s.history) == 0 || lastIngested >= s.generation {
		return NetworkTransitionDelta{}, false
	}

	first := -1
	for i, evt := range s.history {
		if evt.Generation > lastIngested {
			first = i
			break
		}
	}
	if first < 0 {
		return NetworkTransitionDelta{}, false
	}

	events := s.history[first:]
	reasons := make([]NetworkTransitionReason, 0, len(events)+1)
	overflow := (lastIngested > 0 && events[0].Generation > lastIngested+1) ||
		(lastIngested == 0 && events[0].Generation > 2)
	if overflow {
		reasons = append(reasons, ReasonHistoryOverflow)
	}
	var touched []string
	for _, evt := range events {
		reasons = append(reasons, evt.Reasons...)
		touched = append(touched, evt.TouchedInterfaces...)
	}
	reasons = canonicalTransitionReasons(reasons)
	touched = canonicalStringSet(touched)

	from := events[0].Generation - 1
	if overflow {
		from = lastIngested
	}

	return NetworkTransitionDelta{
		FromGeneration:    from,
		ToGeneration:      s.generation,
		Reasons:           reasons,
		Previous:          cloneNetworkTransitionState(events[0].Previous),
		Current:           cloneNetworkTransitionState(*s.latest),
		Coalesced:         overflow || len(events) > 1,
		TouchedInterfaces: touched,
	}, true
}

func (s *networkTransitionStore) subscribeWake(cb func()) func() {
	if cb == nil {
		return func() {}
	}
	s.mu.Lock()
	id := s.nextWatch
	s.nextWatch++
	s.watches[id] = cb
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.watches, id)
			s.mu.Unlock()
		})
	}
}

func transitionReasons(prev, cur NetworkTransitionState) []NetworkTransitionReason {
	reasons := make([]NetworkTransitionReason, 0, 7)
	if prev.ActiveUplink != cur.ActiveUplink || prev.ActiveUplinkIface != cur.ActiveUplinkIface {
		reasons = append(reasons, ReasonActiveUplinkChanged)
	}
	if prev.defaultRouteObserved() && cur.defaultRouteObserved() &&
		(prev.DefaultRouteKnown != cur.DefaultRouteKnown ||
			prev.DefaultRouteIface != cur.DefaultRouteIface) {
		reasons = append(reasons, ReasonDefaultRouteChanged)
	}
	if prev.dnsDefaultObserved() && cur.dnsDefaultObserved() &&
		(prev.DNSDefaultKnown != cur.DNSDefaultKnown ||
			prev.DNSDefaultIface != cur.DNSDefaultIface) {
		reasons = append(reasons, ReasonDNSDefaultChanged)
	}
	if !interfaceRolesEqual(prev.Interfaces, cur.Interfaces) {
		reasons = append(reasons, ReasonInterfaceRolesChanged)
	}
	if !interfaceAddressesEqual(prev.Interfaces, cur.Interfaces) {
		reasons = append(reasons, ReasonInterfaceAddressesChanged)
	}
	if prev.Connectivity != cur.Connectivity {
		reasons = append(reasons, ReasonConnectivityChanged)
	}
	if prev.APActive != cur.APActive {
		reasons = append(reasons, ReasonAPModeChanged)
	}
	return canonicalTransitionReasons(reasons)
}

func preserveLastKnownRouteDNS(prev, cur NetworkTransitionState) NetworkTransitionState {
	if !cur.defaultRouteObserved() && prev.DefaultRouteKnown {
		cur.DefaultRouteObserved = prev.defaultRouteObserved()
		cur.DefaultRouteKnown = true
		cur.DefaultRouteIface = prev.DefaultRouteIface
	}
	if !cur.dnsDefaultObserved() && prev.DNSDefaultKnown {
		cur.DNSDefaultObserved = prev.dnsDefaultObserved()
		cur.DNSDefaultKnown = true
		cur.DNSDefaultIface = prev.DNSDefaultIface
	}
	return cur
}

func routeDNSKnowledgeGained(prev, cur NetworkTransitionState) bool {
	return (!prev.defaultRouteObserved() && cur.defaultRouteObserved()) ||
		(!prev.DefaultRouteKnown && cur.DefaultRouteKnown) ||
		(!prev.dnsDefaultObserved() && cur.dnsDefaultObserved()) ||
		(!prev.DNSDefaultKnown && cur.DNSDefaultKnown)
}

func canonicalNetworkTransitionState(state NetworkTransitionState) NetworkTransitionState {
	if state.DefaultRouteKnown {
		state.DefaultRouteObserved = true
	}
	if state.DNSDefaultKnown {
		state.DNSDefaultObserved = true
	}
	if !state.DefaultRouteKnown {
		state.DefaultRouteIface = ""
	}
	if !state.DNSDefaultKnown {
		state.DNSDefaultIface = ""
	}
	state.Interfaces = cloneNetworkInterfaceStates(state.Interfaces)
	sort.Slice(state.Interfaces, func(i, j int) bool {
		a := state.Interfaces[i]
		b := state.Interfaces[j]
		if a.Kind != b.Kind {
			return a.Kind.String() < b.Kind.String()
		}
		return a.Iface < b.Iface
	})
	for i := range state.Interfaces {
		state.Interfaces[i].IPv4 = canonicalAddrs(state.Interfaces[i].IPv4)
		state.Interfaces[i].IPv6 = canonicalAddrs(state.Interfaces[i].IPv6)
	}
	return state
}

func (state NetworkTransitionState) defaultRouteObserved() bool {
	return state.DefaultRouteObserved || state.DefaultRouteKnown
}

func (state NetworkTransitionState) dnsDefaultObserved() bool {
	return state.DNSDefaultObserved || state.DNSDefaultKnown
}

func cloneNetworkTransitionStatePtr(state NetworkTransitionState) *NetworkTransitionState {
	cloned := cloneNetworkTransitionState(state)
	return &cloned
}

func cloneNetworkTransitionState(state NetworkTransitionState) NetworkTransitionState {
	state.Interfaces = cloneNetworkInterfaceStates(state.Interfaces)
	return state
}

func cloneNetworkInterfaceStates(in []NetworkInterfaceState) []NetworkInterfaceState {
	if len(in) == 0 {
		return nil
	}
	out := make([]NetworkInterfaceState, len(in))
	for i, iface := range in {
		out[i] = iface
		out[i].IPv4 = append([]netip.Addr(nil), iface.IPv4...)
		out[i].IPv6 = append([]netip.Addr(nil), iface.IPv6...)
	}
	return out
}

func canonicalAddrs(in []netip.Addr) []netip.Addr {
	if len(in) == 0 {
		return nil
	}
	out := append([]netip.Addr(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Compare(out[j]) < 0
	})
	n := 0
	for _, addr := range out {
		if n == 0 || out[n-1] != addr {
			out[n] = addr
			n++
		}
	}
	return out[:n]
}

func interfaceRolesEqual(a, b []NetworkInterfaceState) bool {
	ra := make(map[interfaceKey]NetworkInterfaceRole, len(a))
	rb := make(map[interfaceKey]NetworkInterfaceRole, len(b))
	for _, iface := range a {
		ra[interfaceKey{kind: iface.Kind, iface: iface.Iface}] = iface.Role
	}
	for _, iface := range b {
		rb[interfaceKey{kind: iface.Kind, iface: iface.Iface}] = iface.Role
	}
	return reflect.DeepEqual(ra, rb)
}

func interfaceAddressesEqual(a, b []NetworkInterfaceState) bool {
	aa := make(map[interfaceKey]interfaceAddressSet, len(a))
	ab := make(map[interfaceKey]interfaceAddressSet, len(b))
	for _, iface := range a {
		aa[interfaceKey{kind: iface.Kind, iface: iface.Iface}] = interfaceAddressSet{
			// IPv6 is kept in NetworkInterfaceState for diagnostics, but excluded
			// from restart-producing equality until temporary-address flags are
			// available from the probe layer.
			ipv4: canonicalAddrs(iface.IPv4),
		}
	}
	for _, iface := range b {
		ab[interfaceKey{kind: iface.Kind, iface: iface.Iface}] = interfaceAddressSet{
			ipv4: canonicalAddrs(iface.IPv4),
		}
	}
	return reflect.DeepEqual(aa, ab)
}

func transitionTouchedInterfaces(prev, cur NetworkTransitionState) []string {
	prevRoles := interfaceRoleMap(prev.Interfaces)
	curRoles := interfaceRoleMap(cur.Interfaces)
	prevAddrs := interfaceAddressMap(prev.Interfaces)
	curAddrs := interfaceAddressMap(cur.Interfaces)

	keys := make(map[interfaceKey]bool, len(prevRoles)+len(curRoles)+len(prevAddrs)+len(curAddrs))
	for key := range prevRoles {
		keys[key] = true
	}
	for key := range curRoles {
		keys[key] = true
	}
	for key := range prevAddrs {
		keys[key] = true
	}
	for key := range curAddrs {
		keys[key] = true
	}

	touched := make(map[string]bool)
	for key := range keys {
		if key.iface == "" {
			continue
		}
		if prevRoles[key] != curRoles[key] || !reflect.DeepEqual(prevAddrs[key], curAddrs[key]) {
			touched[key.iface] = true
		}
	}
	return sortedStringSet(touched)
}

func interfaceRoleMap(in []NetworkInterfaceState) map[interfaceKey]NetworkInterfaceRole {
	out := make(map[interfaceKey]NetworkInterfaceRole, len(in))
	for _, iface := range in {
		out[interfaceKey{kind: iface.Kind, iface: iface.Iface}] = iface.Role
	}
	return out
}

func interfaceAddressMap(in []NetworkInterfaceState) map[interfaceKey]interfaceAddressSet {
	out := make(map[interfaceKey]interfaceAddressSet, len(in))
	for _, iface := range in {
		out[interfaceKey{kind: iface.Kind, iface: iface.Iface}] = interfaceAddressSet{
			ipv4: canonicalAddrs(iface.IPv4),
		}
	}
	return out
}

type interfaceKey struct {
	kind  DeviceKind
	iface string
}

type interfaceAddressSet struct {
	ipv4 []netip.Addr
}

func canonicalTransitionReasons(in []NetworkTransitionReason) []NetworkTransitionReason {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[NetworkTransitionReason]bool, len(in))
	for _, reason := range in {
		seen[reason] = true
	}
	order := []NetworkTransitionReason{
		ReasonActiveUplinkChanged,
		ReasonDefaultRouteChanged,
		ReasonDNSDefaultChanged,
		ReasonRouteDNSObservationChanged,
		ReasonInterfaceRolesChanged,
		ReasonInterfaceAddressesChanged,
		ReasonConnectivityChanged,
		ReasonAPModeChanged,
		ReasonHistoryOverflow,
	}
	out := make([]NetworkTransitionReason, 0, len(seen))
	for _, reason := range order {
		if seen[reason] {
			out = append(out, reason)
		}
	}
	return out
}

func canonicalStringSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := make(map[string]bool, len(in))
	for _, item := range in {
		if item != "" {
			set[item] = true
		}
	}
	return sortedStringSet(set)
}

func sortedStringSet(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func buildNetworkTransitionState(tick Tick, snap Snapshot) NetworkTransitionState {
	state := NetworkTransitionState{
		ActiveUplink:         tick.ActiveUplink,
		ActiveUplinkIface:    tick.ActiveUplinkIface,
		Connectivity:         snap.Connectivity,
		APActive:             snap.APMode.Active,
		DefaultRouteIface:    tick.DefaultRouteIface,
		DefaultRouteObserved: tick.DefaultRouteObserved,
		DefaultRouteKnown:    tick.DefaultRouteKnown,
		DNSDefaultIface:      tick.DNSDefaultIface,
		DNSDefaultObserved:   tick.DNSDefaultObserved,
		DNSDefaultKnown:      tick.DNSDefaultKnown,
		Interfaces:           tick.Interfaces,
		InterfacesObserved:   tick.InterfacesObserved || len(tick.Interfaces) > 0,
		At:                   tick.At,
	}
	if len(state.Interfaces) == 0 && !state.InterfacesObserved {
		state.Interfaces = legacyInterfacesFromDevices(tick)
	}
	if state.ActiveUplinkIface == "" {
		state.ActiveUplinkIface = activeUplinkIfaceFromDevices(tick.Devices, tick.ActiveUplink)
	}
	return canonicalNetworkTransitionState(state)
}

func legacyInterfacesFromDevices(tick Tick) []NetworkInterfaceState {
	out := make([]NetworkInterfaceState, 0, len(tick.Devices))
	for kind, obs := range tick.Devices {
		if !obs.Present || obs.Iface == "" {
			continue
		}
		out = append(out, NetworkInterfaceState{
			Kind:   kind,
			Iface:  obs.Iface,
			Role:   roleFromObservation(obs, tick.ActiveUplinkIface, tick.DefaultRouteIface, tick.DefaultRouteKnown, tick.NMConn),
			LinkUp: obs.LinkUp,
			HasIP:  obs.HasIP,
		})
	}
	return out
}

func activeUplinkIfaceFromDevices(devices map[DeviceKind]DeviceObservation, uplink UplinkType) string {
	switch uplink {
	case UplinkEthernet:
		if obs, ok := devices[DeviceEthernet]; ok {
			return obs.Iface
		}
	case UplinkWiFi:
		if obs, ok := devices[DeviceWiFi]; ok {
			return obs.Iface
		}
	}
	return ""
}
