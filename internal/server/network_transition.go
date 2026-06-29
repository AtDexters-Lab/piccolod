package server

import (
	"log"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"piccolod/internal/network"
	"piccolod/internal/remote"
)

const (
	networkTransitionRecoveryWindow   = 2 * time.Minute
	networkTransitionRecoveryInterval = 10 * time.Second
	selfHostedRelayName               = "piccolo-portal"
	namekRelayName                    = "piccolo-namek"
)

type networkTransitionRetry struct {
	pending   []network.NetworkTransitionReason
	deadline  time.Time
	attempted bool
}

type networkPublicationMode int

const (
	networkPublicationNone networkPublicationMode = iota
	networkPublicationOpen
	networkPublicationClose
)

func (r *networkTransitionRetry) add(reasons []network.NetworkTransitionReason, now time.Time) {
	if len(reasons) == 0 {
		return
	}
	r.pending = mergeNetworkTransitionReasons(r.pending, reasons)
	r.deadline = now.Add(networkTransitionRecoveryWindow)
	r.attempted = false
}

func (r *networkTransitionRetry) clear() {
	r.pending = nil
	r.deadline = time.Time{}
	r.attempted = false
}

func (r *networkTransitionRetry) hasPending() bool {
	return len(r.pending) > 0
}

func (r *networkTransitionRetry) reasons() []network.NetworkTransitionReason {
	return append([]network.NetworkTransitionReason(nil), r.pending...)
}

func (r *networkTransitionRetry) expired(now time.Time) bool {
	return !r.deadline.IsZero() && !now.Before(r.deadline)
}

func (r *networkTransitionRetry) markAttempted() {
	r.attempted = true
}

func (r *networkTransitionRetry) hasAttempted() bool {
	return r.attempted
}

func (s *GinServer) subscribeSelfHostedNetworkTransitions(src network.NetworkTransitionSource, rm *remote.Manager) {
	if s == nil || src == nil || rm == nil {
		return
	}
	wakeCh, doneCh, cancel := s.networkTransitionWakeChannel(src)
	s.busUnsubs = append(s.busUnsubs, cancel)

	go func() {
		var lastIngested uint64
		var lastState network.NetworkTransitionState
		var retry networkTransitionRetry
		retryTimer := time.NewTimer(time.Hour)
		if !retryTimer.Stop() {
			<-retryTimer.C
		}
		var retryC <-chan time.Time
		defer retryTimer.Stop()
		scheduleRetry := func() {
			if retryC != nil && !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			retryTimer.Reset(networkTransitionRecoveryInterval)
			retryC = retryTimer.C
		}
		stopRetry := func() {
			if retryC != nil && !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			retryC = nil
		}
		attempt := func(force bool) {
			if !s.attemptSelfHostedNetworkTransitionRetry(rm, &retry, lastState, force) {
				stopRetry()
				return
			}
			scheduleRetry()
		}
		for {
			select {
			case <-doneCh:
				return
			case <-wakeCh:
				delta, ok := src.TransitionDeltaSince(lastIngested)
				if !ok {
					continue
				}
				lastIngested = delta.ToGeneration
				lastState = delta.Current
				force := false
				if reasons := remoteRelevantNetworkTransitionReasons(delta); len(reasons) > 0 {
					retry.add(reasons, time.Now())
					force = true
				}
				if retry.hasPending() {
					attempt(force)
				}
			case <-retryC:
				attempt(false)
			}
		}
	}()
}

func (s *GinServer) attemptSelfHostedNetworkTransitionRetry(rm *remote.Manager, retry *networkTransitionRetry, state network.NetworkTransitionState, force bool) bool {
	if rm == nil || retry == nil || !retry.hasPending() {
		return false
	}
	if networkTransitionRetrySatisfiedByConnectedRelay(force, retry, rm.RelayConnectedByName(selfHostedRelayName)) {
		retry.clear()
		return false
	}
	now := time.Now()
	if retry.expired(now) {
		log.Printf("WARN: remote: network transition recovery expired (reasons=%s)", networkTransitionReasonString(retry.reasons()))
		retry.clear()
		return false
	}
	if !networkTransitionRemoteUsable(state) {
		if !retry.hasAttempted() {
			log.Printf("INFO: remote: network transition recovery pending; uplink not usable yet (reasons=%s)", networkTransitionReasonString(retry.reasons()))
			return false
		}
		log.Printf("INFO: remote: dropping network transition recovery; uplink no longer usable (reasons=%s)", networkTransitionReasonString(retry.reasons()))
		retry.clear()
		return false
	}
	if !rm.RestartAdapterForNetworkTransition(retry.reasons()) {
		retry.clear()
		return false
	}
	retry.markAttempted()
	return true
}

func networkTransitionRetrySatisfiedByConnectedRelay(force bool, retry *networkTransitionRetry, connected bool) bool {
	return !force && connected && retry != nil && retry.hasAttempted()
}

func (s *GinServer) subscribeLANNetworkTransitions(src network.NetworkTransitionSource) {
	if s == nil || src == nil {
		return
	}
	wakeCh, doneCh, cancel := s.networkTransitionWakeChannel(src)
	s.busUnsubs = append(s.busUnsubs, cancel)

	go func() {
		var lastIngested uint64
		for {
			select {
			case <-doneCh:
				return
			case <-wakeCh:
				delta, ok := src.TransitionDeltaSince(lastIngested)
				if !ok {
					continue
				}
				lastIngested = delta.ToGeneration
				publicationMode := servicePublicationMode(delta)
				mdnsRelevant := mdnsRelevantNetworkTransition(delta)
				if publicationMode == networkPublicationNone && !mdnsRelevant {
					continue
				}
				if mdnsRelevant && s.mdnsManager != nil {
					publishIfaces, preserveIfaces, ok := mdnsPublishPolicy(delta.Current)
					if ok {
						s.mdnsManager.ReconcileNetworkTransition(publishIfaces, preserveIfaces)
					}
				}
				if publicationMode == networkPublicationOpen && s.serviceManager != nil {
					count := s.serviceManager.ReconcileNetworkPublications()
					log.Printf("INFO: network-transition: publication-reapplied generation=%d endpoints=%d", delta.ToGeneration, count)
				}
				if publicationMode == networkPublicationClose && s.serviceManager != nil {
					count := s.serviceManager.CloseNetworkPublications()
					log.Printf("INFO: network-transition: publication-closed generation=%d endpoints=%d", delta.ToGeneration, count)
				}
			}
		}
	}()
}

func (s *GinServer) networkTransitionWakeChannel(src network.NetworkTransitionSource) (<-chan struct{}, <-chan struct{}, func()) {
	wakeCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	cancelSrc := src.SubscribeNetworkTransitionWake(func() {
		select {
		case <-doneCh:
			return
		case wakeCh <- struct{}{}:
		default:
		}
	})
	var once sync.Once
	return wakeCh, doneCh, func() {
		once.Do(func() {
			cancelSrc()
			close(doneCh)
		})
	}
}

func remoteRelevantNetworkTransitionReasons(delta network.NetworkTransitionDelta) []network.NetworkTransitionReason {
	var out []network.NetworkTransitionReason
	for _, reason := range delta.Reasons {
		switch reason {
		case network.ReasonActiveUplinkChanged,
			network.ReasonDefaultRouteChanged,
			network.ReasonDNSDefaultChanged,
			network.ReasonHistoryOverflow:
			out = append(out, reason)
		case network.ReasonInterfaceAddressesChanged,
			network.ReasonInterfaceRolesChanged:
			if networkTransitionTouchesRemoteIface(delta) {
				out = append(out, reason)
			}
		}
	}
	return mergeNetworkTransitionReasons(nil, out)
}

func networkTransitionRemoteUsable(state network.NetworkTransitionState) bool {
	if state.Connectivity != network.ConnectivityFull && state.Connectivity != network.ConnectivityLimited {
		return false
	}
	if state.DefaultRouteObserved || state.DefaultRouteKnown {
		return state.DefaultRouteKnown && state.DefaultRouteIface != ""
	}
	return state.ActiveUplink != network.UplinkNone
}

func servicePublicationRelevantNetworkTransition(delta network.NetworkTransitionDelta) bool {
	return servicePublicationMode(delta) == networkPublicationOpen
}

func servicePublicationMode(delta network.NetworkTransitionDelta) networkPublicationMode {
	if !localNetworkTransitionReasonRelevant(delta.Reasons) {
		return networkPublicationNone
	}
	if networkTransitionPublicationSafe(delta.Current) {
		return networkPublicationOpen
	}
	return networkPublicationClose
}

func networkTransitionPublicationSafe(state network.NetworkTransitionState) bool {
	hasLAN := false
	for _, iface := range state.Interfaces {
		switch iface.Role {
		case network.InterfaceRoleLAN, network.InterfaceRoleWANLAN:
			hasLAN = true
		case network.InterfaceRoleWAN, network.InterfaceRoleUnknown:
			return false
		}
	}
	return hasLAN
}

func mdnsRelevantNetworkTransition(delta network.NetworkTransitionDelta) bool {
	if hasNetworkTransitionReason(delta.Reasons, network.ReasonHistoryOverflow) {
		return true
	}
	if !networkTransitionHasPreservedLANSurface(delta.Previous) && !networkTransitionHasPreservedLANSurface(delta.Current) {
		return false
	}
	return localNetworkTransitionReasonRelevant(delta.Reasons)
}

func hasNetworkTransitionReason(reasons []network.NetworkTransitionReason, want network.NetworkTransitionReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func mdnsPublishPolicy(state network.NetworkTransitionState) ([]string, []string, bool) {
	if !state.InterfacesObserved {
		return nil, nil, false
	}
	publish := make(map[string]bool, len(state.Interfaces))
	preserve := make(map[string]bool)
	for _, iface := range state.Interfaces {
		if iface.Iface == "" {
			continue
		}
		switch iface.Role {
		case network.InterfaceRoleLAN, network.InterfaceRoleWANLAN:
			publish[iface.Iface] = true
		case network.InterfaceRoleUnknown:
			preserve[iface.Iface] = true
		}
	}
	return sortedIfaceSet(publish), sortedIfaceSet(preserve), true
}

func sortedIfaceSet(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for iface := range set {
		out = append(out, iface)
	}
	sort.Strings(out)
	return out
}

func localNetworkTransitionReasonRelevant(reasons []network.NetworkTransitionReason) bool {
	for _, reason := range reasons {
		switch reason {
		case network.ReasonActiveUplinkChanged,
			network.ReasonDefaultRouteChanged,
			network.ReasonDNSDefaultChanged,
			network.ReasonInterfaceRolesChanged,
			network.ReasonInterfaceAddressesChanged,
			network.ReasonAPModeChanged,
			network.ReasonHistoryOverflow:
			return true
		}
	}
	return false
}

func networkTransitionHasProvenLANSurface(state network.NetworkTransitionState) bool {
	for _, iface := range state.Interfaces {
		switch iface.Role {
		case network.InterfaceRoleLAN, network.InterfaceRoleWANLAN:
			return true
		}
	}
	return false
}

func networkTransitionHasPreservedLANSurface(state network.NetworkTransitionState) bool {
	for _, iface := range state.Interfaces {
		switch iface.Role {
		case network.InterfaceRoleLAN, network.InterfaceRoleWANLAN, network.InterfaceRoleUnknown:
			return true
		}
	}
	return false
}

func networkTransitionTouchesRemoteIface(delta network.NetworkTransitionDelta) bool {
	ifaces := map[string]bool{}
	addRemoteIface(ifaces, delta.Previous.ActiveUplinkIface)
	addRemoteIface(ifaces, delta.Current.ActiveUplinkIface)
	if delta.Previous.DefaultRouteKnown {
		addRemoteIface(ifaces, delta.Previous.DefaultRouteIface)
	}
	if delta.Current.DefaultRouteKnown {
		addRemoteIface(ifaces, delta.Current.DefaultRouteIface)
	}
	if delta.Previous.DNSDefaultKnown {
		addRemoteIface(ifaces, delta.Previous.DNSDefaultIface)
	}
	if delta.Current.DNSDefaultKnown {
		addRemoteIface(ifaces, delta.Current.DNSDefaultIface)
	}
	if len(ifaces) == 0 {
		return false
	}

	prev := networkInterfaceMap(delta.Previous.Interfaces)
	cur := networkInterfaceMap(delta.Current.Interfaces)
	for key := range ifaces {
		prevIface := prev[key]
		curIface := cur[key]
		if prevIface.role == network.InterfaceRoleUnknown || curIface.role == network.InterfaceRoleUnknown {
			continue
		}
		if !reflect.DeepEqual(prevIface, curIface) {
			return true
		}
	}
	if !delta.Coalesced || len(delta.TouchedInterfaces) == 0 {
		return false
	}
	touched := make(map[string]bool, len(delta.TouchedInterfaces))
	for _, iface := range delta.TouchedInterfaces {
		touched[iface] = true
	}
	for key := range ifaces {
		if !touched[key] {
			continue
		}
		prevIface := prev[key]
		curIface := cur[key]
		if prevIface.role == network.InterfaceRoleUnknown || curIface.role == network.InterfaceRoleUnknown {
			continue
		}
		return true
	}
	return false
}

func addRemoteIface(ifaces map[string]bool, iface string) {
	if iface != "" {
		ifaces[iface] = true
	}
}

type comparableNetworkIface struct {
	role network.NetworkInterfaceRole
	ipv4 []string
	ipv6 []string
}

func networkInterfaceMap(in []network.NetworkInterfaceState) map[string]comparableNetworkIface {
	out := make(map[string]comparableNetworkIface, len(in))
	for _, iface := range in {
		out[iface.Iface] = comparableNetworkIface{
			role: iface.Role,
			ipv4: addrStrings(iface.IPv4),
			ipv6: addrStrings(iface.IPv6),
		}
	}
	return out
}

func addrStrings(in []netip.Addr) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, addr := range in {
		out[i] = addr.String()
	}
	return out
}

func mergeNetworkTransitionReasons(existing []network.NetworkTransitionReason, next []network.NetworkTransitionReason) []network.NetworkTransitionReason {
	if len(existing) == 0 && len(next) == 0 {
		return nil
	}
	seen := make(map[network.NetworkTransitionReason]bool, len(existing)+len(next))
	for _, reason := range existing {
		seen[reason] = true
	}
	for _, reason := range next {
		seen[reason] = true
	}
	order := []network.NetworkTransitionReason{
		network.ReasonActiveUplinkChanged,
		network.ReasonDefaultRouteChanged,
		network.ReasonDNSDefaultChanged,
		network.ReasonInterfaceRolesChanged,
		network.ReasonInterfaceAddressesChanged,
		network.ReasonConnectivityChanged,
		network.ReasonAPModeChanged,
		network.ReasonHistoryOverflow,
	}
	out := make([]network.NetworkTransitionReason, 0, len(seen))
	for _, reason := range order {
		if seen[reason] {
			out = append(out, reason)
		}
	}
	return out
}

func networkTransitionReasonString(reasons []network.NetworkTransitionReason) string {
	if len(reasons) == 0 {
		return "unknown"
	}
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, string(reason))
	}
	return strings.Join(parts, ",")
}
