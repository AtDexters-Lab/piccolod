package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"slices"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
	"piccolod/internal/events"
	"piccolod/internal/firewall"
	"piccolod/internal/hostname"
)

// ServiceManager coordinates listener allocation, registry, and proxy startup
type ServiceManager struct {
	allocator       *PortAllocator
	registry        map[string]map[string]ServiceEndpoint // app -> name -> endpoint
	proxyManager    *ProxyManager
	mu              sync.RWMutex
	stopCh          chan struct{}
	wg              sync.WaitGroup
	containerIDs    map[string]string // app -> containerID (optional)
	eventsMu        sync.Mutex
	eventCancel     context.CancelFunc
	eventSubCancels []func() // SubscribeWithCancel cleanup
	eventBus        *events.Bus
	statusMu        sync.RWMutex
	leadership      map[string]cluster.Role
	unpublisher     PortUnpublisher
	publisher       PortPublisher
	firewallMgr     firewall.Manager
	lockReader      LockStateReader
	lockOverrideMu  sync.RWMutex
	lockOverride    *bool

	// Backend health debouncing (RFC 20260125)
	backendHealth *BackendHealthState

	// Remote status provider for health computation (RFC 20260125)
	remoteProvider RemoteStatusProvider

	// Health aggregator cancellation
	healthAggregatorCancel func()

	// deactivated tracks endpoint metadata cleared by DeactivateApp so that a
	// subsequent RemoveApp can still emit a permanent removal event for cert cleanup.
	deactivated map[string][]events.ServiceEndpointInfo

	// preparedReservations tracks committed prepared endpoints that own
	// allocator reservations while access repair is pending, even if proxy
	// publication failed before the registry could be swapped.
	preparedReservations map[string]map[string]ServiceEndpoint

	// App status tracking for health check suppression
	appTransient    map[string]time.Time // app ID → time entered transient state
	appTransientMu  sync.RWMutex
	appStatusCancel func() // unsubscribe from app status events

	// portClaimCache must be rebuilt via rebuildPortClaimCache() after every
	// registry or publication-state mutation.
	portClaimCache []api.PortClaimInfo
}

// LockStateReader exposes the control lock state for services.
type LockStateReader interface {
	ControlLocked() bool
}

func NewServiceManager() *ServiceManager {
	allocator := NewPortAllocator(
		PortRange{Start: 15000, End: 25000},
		PortRange{Start: 35000, End: 45000},
	)
	return &ServiceManager{
		allocator:            allocator,
		registry:             make(map[string]map[string]ServiceEndpoint),
		proxyManager:         NewProxyManager(),
		stopCh:               make(chan struct{}),
		containerIDs:         make(map[string]string),
		leadership:           make(map[string]cluster.Role),
		backendHealth:        NewBackendHealthState(),
		deactivated:          make(map[string][]events.ServiceEndpointInfo),
		preparedReservations: make(map[string]map[string]ServiceEndpoint),
		appTransient:         make(map[string]time.Time),
	}
}

// UseInMemoryNetworkForTest disables OS socket probes/listens for unit tests.
func (m *ServiceManager) UseInMemoryNetworkForTest() {
	m.allocator.portAvailable = func(host string, port int, network string) bool {
		return true
	}
	m.proxyManager.listenTCP = func(network, address string) (net.Listener, error) {
		return inMemoryTestListener{}, nil
	}
	m.proxyManager.listenUDP = func(network string, laddr *net.UDPAddr) (udpPacketConn, error) {
		return inMemoryTestUDPConn{}, nil
	}
}

// SetTCPListenForTest overrides TCP listener creation for cross-package tests.
func (m *ServiceManager) SetTCPListenForTest(fn func(network, address string) (net.Listener, error)) {
	m.proxyManager.listenTCP = fn
}

type inMemoryTestListener struct{}

func (inMemoryTestListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (inMemoryTestListener) Close() error {
	return nil
}

func (inMemoryTestListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

type inMemoryTestUDPConn struct{}

func (inMemoryTestUDPConn) ReadFromUDP([]byte) (int, *net.UDPAddr, udpPacketInfo, error) {
	return 0, nil, udpPacketInfo{}, net.ErrClosed
}

func (inMemoryTestUDPConn) WriteToUDP(payload []byte, addr *net.UDPAddr, _ udpPacketInfo) (int, error) {
	return len(payload), nil
}

func (inMemoryTestUDPConn) Close() error {
	return nil
}

// PortUnpublisher abstracts remote unpublish notifications (e.g., Nexus).
// Implementations should be best-effort and non-blocking.
type PortUnpublisher interface{ Unpublish(port int) }

// SetRemoteUnpublisher wires a remote unpublisher for proxy lifecycle hooks.
func (m *ServiceManager) SetRemoteUnpublisher(u PortUnpublisher) {
	m.statusMu.Lock()
	m.unpublisher = u
	m.statusMu.Unlock()
}

func (m *ServiceManager) notifyUnpublish(port int) {
	m.statusMu.RLock()
	u := m.unpublisher
	m.statusMu.RUnlock()
	if u != nil && port > 0 {
		// best-effort; avoid panics
		defer func() { _ = recover() }()
		u.Unpublish(port)
	}
}

// PortPublisher abstracts remote publish notifications (e.g., Nexus re-enable).
type PortPublisher interface{ Publish(port int) }

// SetRemotePublisher wires a remote publisher for proxy lifecycle hooks.
func (m *ServiceManager) SetRemotePublisher(p PortPublisher) {
	m.statusMu.Lock()
	m.publisher = p
	m.statusMu.Unlock()
}

func (m *ServiceManager) notifyPublish(port int) {
	m.statusMu.RLock()
	p := m.publisher
	m.statusMu.RUnlock()
	if p != nil && port > 0 {
		defer func() { _ = recover() }()
		p.Publish(port)
	}
}

// SetFirewallManager wires a firewall manager for opening/closing port claim rules.
func (m *ServiceManager) SetFirewallManager(fw firewall.Manager) {
	m.firewallMgr = fw
}

func (m *ServiceManager) openFirewallClaim(ep ServiceEndpoint) {
	if ep.PortClaim != nil && m.firewallMgr != nil {
		if err := m.firewallMgr.OpenPort(firewall.Rule{Port: *ep.PortClaim, Protocol: ep.Flow.TransportProtocol()}); err != nil {
			log.Printf("ERROR: firewall open port %d: %v", *ep.PortClaim, err)
		}
	}
}

type publicationEndpointRef struct {
	app      string
	listener string
}

// ReconcileNetworkPublications reapplies LAN-facing firewall publication for
// currently registered port claims after a network transition. Proxy state and
// the endpoint registry remain authoritative; this only repairs network-local
// exposure that may have drifted when interface/zone state changed.
func (m *ServiceManager) ReconcileNetworkPublications() int {
	return m.applyNetworkPublicationFirewall(true)
}

// CloseNetworkPublications removes LAN-facing firewall publication for
// currently registered port claims when network role/zone applicability is not
// proven safe. Proxy state and the endpoint registry remain authoritative.
func (m *ServiceManager) CloseNetworkPublications() int {
	return m.applyNetworkPublicationFirewall(false)
}

func (m *ServiceManager) applyNetworkPublicationFirewall(open bool) int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	var refs []publicationEndpointRef
	for app, byListener := range m.registry {
		if len(m.deactivated[app]) > 0 {
			continue
		}
		for listener, ep := range byListener {
			if ep.PortClaim != nil {
				refs = append(refs, publicationEndpointRef{app: app, listener: listener})
			}
		}
	}
	m.mu.RUnlock()

	applied := 0
	for _, ref := range refs {
		if m.applyCurrentFirewallClaim(ref, open) {
			applied++
		}
	}
	return applied
}

func (m *ServiceManager) applyCurrentFirewallClaim(ref publicationEndpointRef, open bool) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.deactivated[ref.app]) > 0 {
		return false
	}
	byListener, ok := m.registry[ref.app]
	if !ok {
		return false
	}
	ep, ok := byListener[ref.listener]
	if !ok || ep.PortClaim == nil {
		return false
	}
	if open {
		m.openFirewallClaim(ep)
	} else {
		m.closeFirewallClaim(ep)
	}
	return true
}

// releaseEndpointPorts releases allocated ports for an endpoint.
// For port claims, uses protocol-aware release to avoid freeing the sibling protocol.
func (m *ServiceManager) releaseEndpointPorts(ep ServiceEndpoint) {
	m.allocator.ReleaseHost(ep.HostBind)
	if ep.PortClaim != nil {
		m.allocator.FreePublicProto(ep.PublicPort, ep.Flow.TransportProtocol())
	} else {
		m.allocator.ReleasePublic(ep.PublicPort)
	}
}

func endpointPublicAllocationKey(ep ServiceEndpoint) string {
	if ep.PortClaim != nil {
		return publicKey(ep.PublicPort, ep.Flow.TransportProtocol())
	}
	return publicKey(ep.PublicPort, "tcp")
}

func (m *ServiceManager) releaseEndpointPublicAllocation(ep ServiceEndpoint) {
	if ep.PortClaim != nil {
		m.allocator.FreePublicProto(ep.PublicPort, ep.Flow.TransportProtocol())
	} else {
		m.allocator.ReleasePublic(ep.PublicPort)
	}
}

func (m *ServiceManager) endpointHostOwnerLocked(host int) (string, string, bool) {
	for appName, mapp := range m.registry {
		for name, ep := range mapp {
			if ep.HostBind == host {
				return appName, name, true
			}
		}
	}
	for appName, mapp := range m.preparedReservations {
		for name, ep := range mapp {
			if ep.HostBind == host {
				return appName, name, true
			}
		}
	}
	return "", "", false
}

func (m *ServiceManager) endpointPublicOwnerLocked(key string) (string, string, bool) {
	for appName, mapp := range m.registry {
		for name, ep := range mapp {
			if endpointPublicAllocationKey(ep) == key {
				return appName, name, true
			}
		}
	}
	for appName, mapp := range m.preparedReservations {
		for name, ep := range mapp {
			if endpointPublicAllocationKey(ep) == key {
				return appName, name, true
			}
		}
	}
	return "", "", false
}

func (m *ServiceManager) endpointTransportPublicOwnerLocked(port int, protocol string) (string, string, bool) {
	for appName, mapp := range m.registry {
		for name, ep := range mapp {
			if ep.PublicPort == port && ep.Flow.TransportProtocol() == protocol {
				return appName, name, true
			}
		}
	}
	for appName, mapp := range m.preparedReservations {
		for name, ep := range mapp {
			if ep.PublicPort == port && ep.Flow.TransportProtocol() == protocol {
				return appName, name, true
			}
		}
	}
	return "", "", false
}

func (m *ServiceManager) ensurePortClaimAvailableLocked(appName, listenerName string, port int, flow api.ListenerFlow, allowSameOwner bool) error {
	protocol := flow.TransportProtocol()
	ownerApp, ownerName, exists := m.endpointTransportPublicOwnerLocked(port, protocol)
	if !exists {
		return nil
	}
	if allowSameOwner && ownerApp == appName && ownerName == listenerName {
		return nil
	}
	return fmt.Errorf("%s port %d already owned by %s/%s", protocol, port, ownerApp, ownerName)
}

func (m *ServiceManager) allocateForListenerLocked(appName string, l api.AppListener, allowSameOwner bool) (int, int, error) {
	if l.PortClaim != nil {
		if err := m.ensurePortClaimAvailableLocked(appName, l.Name, *l.PortClaim, l.Flow, allowSameOwner); err != nil {
			return 0, 0, err
		}
	}
	return m.allocator.AllocateForClaim(l.PortClaim, l.Flow == api.FlowUDP)
}

func (m *ServiceManager) endpointPublicationRunningLocked(ep ServiceEndpoint) bool {
	if m.proxyManager == nil {
		return false
	}
	m.proxyManager.mu.Lock()
	defer m.proxyManager.mu.Unlock()
	if ep.Flow == api.FlowUDP {
		_, ok := m.proxyManager.udpListeners[ep.PublicPort]
		return ok
	}
	_, ok := m.proxyManager.listeners[ep.PublicPort]
	return ok
}

func (m *ServiceManager) releaseStalePreparedReservationsLocked(appName string, keepHosts map[int]struct{}, keepPublic map[string]struct{}) {
	reserved := m.preparedReservations[appName]
	if len(reserved) == 0 {
		return
	}
	for _, ep := range reserved {
		if _, keep := keepHosts[ep.HostBind]; !keep {
			m.allocator.ReleaseHost(ep.HostBind)
		}
		if _, keep := keepPublic[endpointPublicAllocationKey(ep)]; !keep {
			m.releaseEndpointPublicAllocation(ep)
		}
	}
	delete(m.preparedReservations, appName)
}

func cloneEndpointMap(endpoints map[string]ServiceEndpoint) map[string]ServiceEndpoint {
	if len(endpoints) == 0 {
		return nil
	}
	out := make(map[string]ServiceEndpoint, len(endpoints))
	for name, ep := range endpoints {
		out[name] = ep
	}
	return out
}

func (m *ServiceManager) retainPreparedReservationsLocked(appName string, endpoints map[string]ServiceEndpoint) {
	newHosts := make(map[int]struct{}, len(endpoints))
	newPublic := make(map[string]struct{}, len(endpoints))
	for _, ep := range endpoints {
		m.allocator.usedHost[ep.HostBind] = struct{}{}
		publicKey := endpointPublicAllocationKey(ep)
		m.allocator.usedPublic[publicKey] = struct{}{}
		newHosts[ep.HostBind] = struct{}{}
		newPublic[publicKey] = struct{}{}
	}
	m.releaseStalePreparedReservationsLocked(appName, newHosts, newPublic)
	if len(endpoints) > 0 {
		m.preparedReservations[appName] = cloneEndpointMap(endpoints)
	}
}

func (m *ServiceManager) reservePreparedPublicationLocked(appName string, endpoints map[string]ServiceEndpoint) error {
	for name, ep := range endpoints {
		if ownerApp, ownerName, ok := m.endpointHostOwnerLocked(ep.HostBind); ok && ownerApp != appName {
			return fmt.Errorf("restore prepared publication: host bind %d for %s/%s already owned by %s/%s", ep.HostBind, appName, name, ownerApp, ownerName)
		}
		if _, used := m.allocator.usedHost[ep.HostBind]; used {
			if ownerApp, _, ok := m.endpointHostOwnerLocked(ep.HostBind); !ok || ownerApp != appName {
				return fmt.Errorf("restore prepared publication: host bind %d already reserved", ep.HostBind)
			}
		}
		publicKey := endpointPublicAllocationKey(ep)
		if ownerApp, ownerName, ok := m.endpointPublicOwnerLocked(publicKey); ok && ownerApp != appName {
			return fmt.Errorf("restore prepared publication: public port %s for %s/%s already owned by %s/%s", publicKey, appName, name, ownerApp, ownerName)
		}
		if _, used := m.allocator.usedPublic[publicKey]; used {
			if ownerApp, _, ok := m.endpointPublicOwnerLocked(publicKey); !ok || ownerApp != appName {
				return fmt.Errorf("restore prepared publication: public port %s already reserved", publicKey)
			}
		}
		if m.endpointPublicationRunningLocked(ep) {
			if ownerApp, _, ok := m.endpointPublicOwnerLocked(publicKey); !ok || ownerApp != appName {
				return fmt.Errorf("restore prepared publication: public listener %s already running", publicKey)
			}
		}
	}
	m.retainPreparedReservationsLocked(appName, endpoints)
	return nil
}

func (m *ServiceManager) closeFirewallClaim(ep ServiceEndpoint) {
	if ep.PortClaim != nil && m.firewallMgr != nil {
		if err := m.firewallMgr.ClosePort(firewall.Rule{Port: *ep.PortClaim, Protocol: ep.Flow.TransportProtocol()}); err != nil {
			log.Printf("ERROR: firewall close port %d: %v", *ep.PortClaim, err)
		}
	}
}

// ActivePortClaims returns all active port claims from the service registry.
// Implements remote.PortClaimProvider.
func (m *ServiceManager) ActivePortClaims() []api.PortClaimInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]api.PortClaimInfo, len(m.portClaimCache))
	copy(out, m.portClaimCache)
	return out
}

// rebuildPortClaimCache rebuilds the cached port claims from the registry.
// Caller must hold m.mu write lock.
func (m *ServiceManager) rebuildPortClaimCache() {
	var claims []api.PortClaimInfo
	for app, mapp := range m.registry {
		if len(m.deactivated[app]) > 0 {
			continue
		}
		for _, ep := range mapp {
			if ep.PortClaim != nil {
				claims = append(claims, api.PortClaimInfo{
					Port:     *ep.PortClaim,
					HostBind: ep.HostBind,
					Protocol: ep.Flow.TransportProtocol(),
				})
			}
		}
	}
	m.portClaimCache = claims
}

// SetEventBus wires an event bus for publishing endpoint changes and starts the health aggregator.
func (m *ServiceManager) SetEventBus(bus *events.Bus) {
	m.eventsMu.Lock()
	m.eventBus = bus
	m.eventsMu.Unlock()

	// Start health aggregator and app status tracker if event bus is available
	if bus != nil {
		m.startHealthAggregator()
		m.startAppStatusTracker()
	}
}

// SetRemoteStatusProvider wires the remote status provider for health computation.
func (m *ServiceManager) SetRemoteStatusProvider(provider RemoteStatusProvider) {
	m.statusMu.Lock()
	m.remoteProvider = provider
	m.statusMu.Unlock()
}

func (m *ServiceManager) publishEndpointsEvent(evt events.ServiceEndpointsChanged) {
	m.eventsMu.Lock()
	bus := m.eventBus
	m.eventsMu.Unlock()
	if bus == nil {
		return
	}
	bus.Publish(events.Event{
		Topic:   events.TopicServiceEndpointsChanged,
		Payload: evt,
	})
}

func endpointInfoSlice(eps []ServiceEndpoint) []events.ServiceEndpointInfo {
	info := make([]events.ServiceEndpointInfo, len(eps))
	for i, ep := range eps {
		info[i] = events.ServiceEndpointInfo{
			App:                ep.App,
			Name:               ep.Name,
			DerivedHostLabel:   ep.DerivedHostLabel,
			Flow:               ep.Flow,
			Protocol:           ep.Protocol,
			RequiresTLSMuxAuth: ep.RequiresTLSMuxAuth,
		}
	}
	return info
}

const ACMEHTTPFallbackPort = 5002

func normalizeRemotePort(port int) int {
	if port == ACMEHTTPFallbackPort {
		return 80
	}
	return port
}

// deriveRemotePorts computes a listener's effective RemotePorts per
// RFC 20260519 §D2: union of manifest RemotePorts + port_claim + (443 when
// tls_wrap on raw); falls back to [80, 443] when the union is empty AND
// the listener is host-routing-eligible. The eligibility predicate is the
// single source of truth: a listener that gets a DerivedHostLabel also
// gets the [80, 443] default — and conversely, a raw-without-wrap listener
// (not host-routable) stays empty, preserving the byte-leak closure.
func deriveRemotePorts(listener api.AppListener) []int {
	ports := make([]int, 0, len(listener.RemotePorts)+2)
	seen := make(map[int]struct{}, len(listener.RemotePorts)+2)
	add := func(p int) {
		if p <= 0 {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		ports = append(ports, p)
	}
	for _, p := range listener.RemotePorts {
		add(p)
	}
	if listener.PortClaim != nil {
		add(*listener.PortClaim)
	}
	if listener.IsRawTCP() && listener.TLSWrap {
		add(443)
	}
	if len(ports) == 0 && IsEligibleForHostRouting(listener) {
		return []int{80, 443}
	}
	return ports
}

// buildEndpoint is the single source of truth for mapping an AppListener +
// allocated ports into a ServiceEndpoint. All construction sites must use this
// to avoid field-omission bugs when new fields are added.
func buildEndpoint(appName string, l api.AppListener, hostBind, publicPort int, isPrimary bool, hostLabel string) ServiceEndpoint {
	return ServiceEndpoint{
		App:                appName,
		Name:               l.Name,
		GuestPort:          l.GuestPort,
		HostBind:           hostBind,
		PublicPort:         publicPort,
		Flow:               l.Flow,
		Protocol:           l.Protocol,
		Primary:            isPrimary,
		DerivedHostLabel:   hostLabel,
		Middleware:         l.Middleware,
		RemotePorts:        deriveRemotePorts(l),
		Auth:               l.Auth,
		ConnectionAuth:     l.ConnectionAuth,
		RequiresTLSMuxAuth: l.ConnectionAuth.RequiresMTLS(),
		PortClaim:          l.PortClaim,
	}
}

// ObserveRuntimeEvents subscribes to leadership and lock-state events for logging.
func (m *ServiceManager) ObserveRuntimeEvents(bus *events.Bus) {
	if bus == nil {
		return
	}
	m.eventsMu.Lock()
	if m.eventCancel != nil {
		m.eventCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.eventCancel = cancel
	m.eventsMu.Unlock()

	leaders, cancelLeaders := bus.SubscribeWithCancel(events.TopicLeadershipRoleChanged, 16)
	locks, cancelLocks := bus.SubscribeWithCancel(events.TopicLockStateChanged, 8)
	m.eventsMu.Lock()
	m.eventSubCancels = []func(){cancelLeaders, cancelLocks}
	m.eventsMu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case evt, ok := <-leaders:
				if !ok {
					leaders = nil
					if leaders == nil && locks == nil {
						return
					}
					continue
				}
				payload, ok := evt.Payload.(events.LeadershipChanged)
				if !ok {
					log.Printf("WARN: service-manager received unexpected leadership payload: %#v", evt.Payload)
					continue
				}
				m.statusMu.Lock()
				m.leadership[payload.Resource] = payload.Role
				m.statusMu.Unlock()
				log.Printf("INFO: service-manager observed leadership change resource=%s role=%s", payload.Resource, payload.Role)
			case evt, ok := <-locks:
				if !ok {
					locks = nil
					if leaders == nil && locks == nil {
						return
					}
					continue
				}
				payload, ok := evt.Payload.(events.LockStateChanged)
				if !ok {
					log.Printf("WARN: service-manager received unexpected lock payload: %#v", evt.Payload)
					continue
				}
				state := "unlocked"
				if payload.Locked {
					state = "locked"
				}
				log.Printf("INFO: service-manager observed control lock state=%s", state)
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			}
		}
	}()
}

func (m *ServiceManager) stopEventObservers() {
	m.eventsMu.Lock()
	if m.eventCancel != nil {
		m.eventCancel()
		m.eventCancel = nil
	}
	if m.appStatusCancel != nil {
		m.appStatusCancel()
		m.appStatusCancel = nil
	}
	for _, cancel := range m.eventSubCancels {
		cancel()
	}
	m.eventSubCancels = nil
	m.eventsMu.Unlock()
}

// StopRuntimeEvents cancels leadership/lock subscriptions and waits for handlers to exit.
func (m *ServiceManager) StopRuntimeEvents() {
	m.stopEventObservers()
	m.wg.Wait()
}

// ProxyManager returns the underlying ProxyManager.
func (m *ServiceManager) ProxyManager() *ProxyManager { return m.proxyManager }

func (m *ServiceManager) RegisterProxyHint(listenerPort, sourcePort, remotePort int, isTLS bool, clientIP string) {
	if listenerPort <= 0 || sourcePort <= 0 || m.proxyManager == nil {
		return
	}
	m.proxyManager.registerHint(listenerPort, sourcePort, connectionHint{
		clientIP:   clientIP,
		isTLS:      isTLS,
		remotePort: remotePort,
	})
}

func (m *ServiceManager) consumeProxyHint(listenerPort, sourcePort int) (connectionHint, bool) {
	if listenerPort <= 0 || sourcePort <= 0 || m.proxyManager == nil {
		return connectionHint{}, false
	}
	return m.proxyManager.consumeHint(listenerPort, sourcePort)
}

func (m *ServiceManager) peekProxyHint(listenerPort, sourcePort int) (connectionHint, bool) {
	if listenerPort <= 0 || sourcePort <= 0 || m.proxyManager == nil {
		return connectionHint{}, false
	}
	return m.proxyManager.peekHint(listenerPort, sourcePort)
}

// ConsumePortalHint extracts the Nexus-relayed client IP from a consumed portal
// connection hint. Used by the secure loopback's ConnContext to make the real
// client IP available to GinServer middleware.
func (m *ServiceManager) ConsumePortalHint(listenerPort, sourcePort int) (string, bool) {
	hint, ok := m.consumeProxyHint(listenerPort, sourcePort)
	if !ok {
		return "", false
	}
	return hint.clientIP, true
}

// LastObservedRole reports the most recent leadership role seen for a resource.
func (m *ServiceManager) LastObservedRole(resource string) cluster.Role {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	if role, ok := m.leadership[resource]; ok {
		return role
	}
	return cluster.RoleUnknown
}

// Locked reports the current control lock state.
func (m *ServiceManager) Locked() bool {
	m.lockOverrideMu.RLock()
	if m.lockOverride != nil {
		locked := *m.lockOverride
		m.lockOverrideMu.RUnlock()
		return locked
	}
	reader := m.lockReader
	m.lockOverrideMu.RUnlock()
	if reader != nil {
		return reader.ControlLocked()
	}
	return false
}

// SetLockReader wires a shared lock reader for authoritative lock checks.
func (m *ServiceManager) SetLockReader(reader LockStateReader) {
	m.lockOverrideMu.Lock()
	m.lockReader = reader
	m.lockOverrideMu.Unlock()
}

// ForceLockState allows tests to override the observed lock state.
func (m *ServiceManager) ForceLockState(lock bool) {
	m.lockOverrideMu.Lock()
	val := lock
	m.lockOverride = &val
	m.lockOverrideMu.Unlock()
}

// ClearLockOverride clears any forced lock state override.
func (m *ServiceManager) ClearLockOverride() {
	m.lockOverrideMu.Lock()
	m.lockOverride = nil
	m.lockOverrideMu.Unlock()
}

// RestoreFromPodman rebuilds proxies for an app using existing host-bind ports.
func (m *ServiceManager) RestoreFromPodman(appName string, listeners []api.AppListener, hostByGuest map[string]int) ([]ServiceEndpoint, error) {
	// Stop any existing proxies first
	m.DeactivateApp(appName)

	m.mu.Lock()
	defer m.mu.Unlock()

	endpoints := make([]ServiceEndpoint, 0, len(listeners))
	if len(listeners) == 0 {
		return endpoints, nil
	}

	// Determine the primary listener for host-based routing
	primaryName, _ := hostname.ResolvePrimaryListener(listeners)

	registry := make(map[string]ServiceEndpoint)
	for _, l := range listeners {
		gpKey := fmt.Sprintf("%d/%s", l.GuestPort, l.Flow.TransportProtocol())
		host, ok := hostByGuest[gpKey]
		if !ok {
			continue
		}
		if err := m.allocator.ReserveHost(host); err != nil {
			continue
		}
		var public int
		var err error
		if l.PortClaim != nil {
			err = m.ensurePortClaimAvailableLocked(appName, l.Name, *l.PortClaim, l.Flow, false)
			if err == nil {
				err = m.allocator.ClaimPublicPort(*l.PortClaim, l.Flow == api.FlowUDP)
			}
			if err == nil {
				public = *l.PortClaim
			}
		} else {
			public, err = m.allocator.allocatePublicForFlow(l.Flow == api.FlowUDP)
		}
		if err != nil {
			m.allocator.freeHost(host)
			for _, allocated := range endpoints {
				m.releaseEndpointPorts(allocated)
			}
			m.rebuildPortClaimCache()
			return nil, err
		}
		isPrimary := l.Name == primaryName
		hostLabel := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l))
		ep := buildEndpoint(appName, l, host, public, isPrimary, hostLabel)
		registry[l.Name] = ep
		endpoints = append(endpoints, ep)
	}

	started := 0
	for _, ep := range endpoints {
		if err := m.startEndpointPublicationLocked(ep); err != nil {
			for _, startedEp := range endpoints[:started] {
				m.stopEndpointPublicationLocked(startedEp)
			}
			for _, allocated := range endpoints {
				m.releaseEndpointPorts(allocated)
			}
			m.rebuildPortClaimCache()
			return nil, err
		}
		started++
	}

	if len(registry) > 0 {
		m.registry[appName] = registry
	}
	// App is back — clear any stashed deactivated state.
	delete(m.deactivated, appName)
	m.rebuildPortClaimCache()

	// Publish endpoint changes (non-blocking)
	if len(endpoints) > 0 {
		m.publishEndpointsEvent(events.ServiceEndpointsChanged{
			App:   appName,
			Added: endpointInfoSlice(endpoints),
		})
	}

	return endpoints, nil
}

// AllocateForApp allocates ports for all listeners of an app and starts proxies.
func (m *ServiceManager) AllocateForApp(appName string, listeners []api.AppListener) ([]ServiceEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Determine the primary listener for host-based routing
	primaryName, _ := hostname.ResolvePrimaryListener(listeners)

	// Check for host label collisions before allocating
	if err := m.checkHostLabelCollisions(appName, listeners, primaryName); err != nil {
		return nil, err
	}

	endpoints := make([]ServiceEndpoint, 0, len(listeners))
	registry := make(map[string]ServiceEndpoint, len(listeners))

	for _, l := range listeners {
		hb, pp, err := m.allocateForListenerLocked(appName, l, false)
		if err != nil {
			// Roll back ports allocated by previous iterations.
			for _, ep := range endpoints {
				m.releaseEndpointPorts(ep)
			}
			delete(m.registry, appName)
			m.rebuildPortClaimCache()
			return nil, err
		}
		isPrimary := l.Name == primaryName
		hostLabel := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l))
		ep := buildEndpoint(appName, l, hb, pp, isPrimary, hostLabel)
		endpoints = append(endpoints, ep)
		registry[l.Name] = ep
	}

	// Start proxies and open firewall for port claims
	started := 0
	for _, ep := range endpoints {
		if err := m.startEndpointPublicationLocked(ep); err != nil {
			for _, startedEp := range endpoints[:started] {
				m.stopEndpointPublicationLocked(startedEp)
			}
			for _, allocated := range endpoints {
				m.releaseEndpointPorts(allocated)
			}
			delete(m.registry, appName)
			m.rebuildPortClaimCache()
			return nil, err
		}
		started++
	}
	if len(registry) > 0 {
		m.registry[appName] = registry
	} else {
		delete(m.registry, appName)
	}
	// Clear any stashed deactivated state from prior stop.
	delete(m.deactivated, appName)

	m.rebuildPortClaimCache()

	// Publish endpoint changes (non-blocking)
	m.publishEndpointsEvent(events.ServiceEndpointsChanged{
		App:   appName,
		Added: endpointInfoSlice(endpoints),
	})

	return endpoints, nil
}

// ReserveHostPort permanently reserves a host-bind port to avoid future allocation.
func (m *ServiceManager) ReserveHostPort(port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allocator.ReserveHost(port)
}

// GetAll returns all service endpoints across all apps
func (m *ServiceManager) GetAll() []ServiceEndpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ServiceEndpoint
	for _, mapp := range m.registry {
		for _, ep := range mapp {
			out = append(out, ep)
		}
	}
	return out
}

// GetByApp returns endpoints for a single app
func (m *ServiceManager) GetByApp(appName string) ([]ServiceEndpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mapp, ok := m.registry[appName]
	if !ok {
		return nil, fmt.Errorf("app not found: %s", appName)
	}
	var out []ServiceEndpoint
	for _, ep := range mapp {
		out = append(out, ep)
	}
	return out, nil
}

// GetAppListener returns a specific listener endpoint
func (m *ServiceManager) GetAppListener(appName, listener string) (ServiceEndpoint, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mapp, ok := m.registry[appName]
	if !ok {
		return ServiceEndpoint{}, false
	}
	ep, ok := mapp[listener]
	return ep, ok
}

// ResolveByRemotePort locates a service endpoint matching the remote port hint.
func (m *ServiceManager) ResolveByRemotePort(port int) (ServiceEndpoint, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mapp := range m.registry {
		for _, ep := range mapp {
			if matchesRemotePort(ep, port) {
				return ep, true
			}
		}
	}
	return ServiceEndpoint{}, false
}

// ResolveListener finds a listener by name and optional remote port.
func (m *ServiceManager) ResolveListener(listener string, remotePort int) (ServiceEndpoint, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mapp := range m.registry {
		if ep, ok := mapp[listener]; ok && matchesRemotePort(ep, remotePort) {
			return ep, true
		}
	}
	return ServiceEndpoint{}, false
}

func matchesRemotePort(ep ServiceEndpoint, remotePort int) bool {
	original := remotePort
	remotePort = normalizeRemotePort(remotePort)
	if remotePort <= 0 {
		return true
	}
	// Per RFC 20260519 §D2: deriveRemotePorts is the single source of truth
	// for an endpoint's accepted remote ports. The HTTP-flavor default
	// [80, 443] is now applied at derivation, not here. An endpoint that
	// reaches this function with an empty RemotePorts list is structurally
	// unreachable from outside (raw listener without port_claim, without
	// tls_wrap) — return false rather than the legacy [80, 443] fallback,
	// which would silently re-open the byte-leak if a future refactor
	// allowed such an endpoint to acquire a DerivedHostLabel.
	if len(ep.RemotePorts) == 0 {
		return false
	}
	for _, rp := range ep.RemotePorts {
		if rp == remotePort || rp == original {
			return true
		}
	}
	return false
}

// ResolveByHostLabel finds an endpoint by its derived host label (app or listener-app).
// This is the primary method for host-based routing.
func (m *ServiceManager) ResolveByHostLabel(label string, remotePort int) (ServiceEndpoint, bool) {
	if label == "" {
		return ServiceEndpoint{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mapp := range m.registry {
		for _, ep := range mapp {
			if ep.DerivedHostLabel == label && matchesRemotePort(ep, remotePort) {
				return ep, true
			}
		}
	}
	return ServiceEndpoint{}, false
}

// ResolveByHostLabelAnyPort finds an endpoint by derived host label without
// applying remote-port filtering. Use this only to distinguish "host matched
// but the requested port is denied" from "host did not match".
func (m *ServiceManager) ResolveByHostLabelAnyPort(label string) (ServiceEndpoint, bool) {
	if label == "" {
		return ServiceEndpoint{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mapp := range m.registry {
		for _, ep := range mapp {
			if ep.DerivedHostLabel == label {
				return ep, true
			}
		}
	}
	return ServiceEndpoint{}, false
}

// ResolveAppPrimary finds the primary listener for an app.
func (m *ServiceManager) ResolveAppPrimary(appName string) (ServiceEndpoint, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mapp, ok := m.registry[appName]
	if !ok {
		return ServiceEndpoint{}, false
	}
	for _, ep := range mapp {
		if ep.Primary {
			return ep, true
		}
	}
	return ServiceEndpoint{}, false
}

// checkHostLabelCollisions validates that new host labels don't collide with existing ones.
// Must be called while holding m.mu lock.
func (m *ServiceManager) checkHostLabelCollisions(appName string, listeners []api.AppListener, primaryName string) error {
	// Compute new host labels
	newLabels := make([]string, 0, len(listeners))
	for _, l := range listeners {
		isPrimary := l.Name == primaryName
		label := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l))
		if label != "" {
			newLabels = append(newLabels, label)
		}
	}

	if len(newLabels) == 0 {
		return nil // No host-based labels to check
	}

	// Build existing labels map (excluding the app being allocated/reconciled)
	existingLabels := make(map[string]string)
	for app, mapp := range m.registry {
		if app == appName {
			continue // Skip the app being modified
		}
		for _, ep := range mapp {
			if ep.DerivedHostLabel != "" {
				existingLabels[ep.DerivedHostLabel] = fmt.Sprintf("app:%s/listener:%s", ep.App, ep.Name)
			}
		}
	}

	return hostname.CheckCollisions(newLabels, existingLabels)
}

// StopAll stops all proxy listeners
func (m *ServiceManager) StopAll() {
	m.proxyManager.StopAll()
}

// StartBackground starts health checks for backends (connectivity to hostBind)
func (m *ServiceManager) StartBackground() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.checkBackends()
			}
		}
	}()
}

// Stop stops background tasks and proxies
func (m *ServiceManager) Stop() {
	m.stopEventObservers()
	close(m.stopCh)
	m.wg.Wait()
	m.StopAll()
}

// startAppStatusTracker subscribes to TopicAppStatusChanged events and tracks
// which apps are in transient states (starting). Health checks are suppressed
// for these apps to avoid false "unhealthy" reports.
func (m *ServiceManager) startAppStatusTracker() {
	m.eventsMu.Lock()
	bus := m.eventBus
	m.eventsMu.Unlock()
	if bus == nil {
		return
	}

	ch, cancel := bus.SubscribeWithCancel(events.TopicAppStatusChanged, 64)
	m.eventsMu.Lock()
	if m.appStatusCancel != nil {
		m.appStatusCancel()
	}
	m.appStatusCancel = cancel
	m.eventsMu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for evt := range ch {
			payload, ok := evt.Payload.(events.AppStatusChangedEvent)
			if !ok {
				continue
			}
			m.appTransientMu.Lock()
			switch payload.Status {
			case "starting":
				m.appTransient[payload.App] = time.Now()
			default:
				delete(m.appTransient, payload.App)
			}
			m.appTransientMu.Unlock()
		}
	}()
}

func (m *ServiceManager) checkBackends() {
	// Snapshot under lock
	snap := m.snapshotRegistry()

	// TCP connectivity check per endpoint with debouncing
	now := time.Now()
	for appName, mapp := range snap {
		// Skip health checks for apps in transient states (starting).
		// Safety valve: auto-expire entries older than 5 minutes to handle
		// dropped events from the lossy event bus.
		m.appTransientMu.Lock()
		if since, ok := m.appTransient[appName]; ok {
			if now.Sub(since) > 5*time.Minute {
				delete(m.appTransient, appName)
			} else {
				m.appTransientMu.Unlock()
				continue
			}
		}
		m.appTransientMu.Unlock()
		for _, ep := range mapp {
			// Skip health checks for UDP endpoints — TCP dial always fails against
			// a UDP port, which would report permanently unhealthy.
			if ep.Flow == api.FlowUDP {
				continue
			}
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.HostBind))
			conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			checkOK := err == nil
			if checkOK {
				_ = conn.Close()
			}

			// Record check with debouncing
			endpointKey := ep.endpointKey()
			result := m.backendHealth.RecordCheck(endpointKey, checkOK)

			// Log only on state change or first failure in a series
			if !checkOK && result.StateChanged {
				log.Printf("WARN: Backend unhealthy for %s/%s at %s (after %d consecutive failures)",
					ep.App, ep.Name, addr, backendFailureThreshold)
			} else if checkOK && result.StateChanged {
				log.Printf("INFO: Backend recovered for %s/%s at %s", ep.App, ep.Name, addr)
			}

			// Emit event on state change if event bus is configured
			if result.StateChanged && m.eventBus != nil {
				m.emitBackendHealthEvent(ep, result)
			}
		}
	}
}

// emitBackendHealthEvent emits a listener health changed event for backend state changes.
//
// Design note: This emits a simplified health based only on backend status, not including
// certificate health. This is intentional because:
//  1. ServiceManager doesn't have access to certificate data (avoiding import cycle)
//  2. Full health is computed on-demand via API (handleGinListenerHealth) and on WebSocket connect
//  3. This event serves as a notification that health may have changed, prompting clients to
//     refresh via API if needed
//
// The WebSocket stream provides initial full health on connect, and clients should treat
// subsequent events as hints to refresh the complete health state via the API endpoint.
func (m *ServiceManager) emitBackendHealthEvent(ep ServiceEndpoint, result RecordCheckResult) {
	reasonCode := "ok"
	reason := "Operational"
	status := "ok"
	if !result.IsHealthy {
		reasonCode = "backend_unreachable"
		reason = "Backend not responding"
		status = "error"
	}

	m.eventBus.Publish(events.Event{
		Topic: events.TopicListenerHealthChanged,
		Payload: events.ListenerHealthEvent{
			App:      ep.App,
			Listener: ep.Name,
			Health: events.ListenerHealth{
				Status:         status,
				ReasonCode:     reasonCode,
				Reason:         reason,
				Recoverable:    true, // Backend issues are always auto-recoverable
				ActionRequired: false,
				LastChecked:    time.Now(),
				LastOK:         result.LastOK,
			},
			Timestamp: time.Now(),
		},
	})
}

// SnapshotRegistry returns a snapshot of the full endpoint registry.
func (m *ServiceManager) SnapshotRegistry() map[string]map[string]ServiceEndpoint {
	return m.snapshotRegistry()
}

func (m *ServiceManager) snapshotRegistry() map[string]map[string]ServiceEndpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	clone := make(map[string]map[string]ServiceEndpoint, len(m.registry))
	for app, mapp := range m.registry {
		mm := make(map[string]ServiceEndpoint, len(mapp))
		for name, ep := range mapp {
			mm[name] = ep
		}
		clone[app] = mm
	}
	return clone
}

// SetAppContainerID records the container ID for an app (used by watcher reconciliation)
func (m *ServiceManager) SetAppContainerID(appName, containerID string) {
	m.mu.Lock()
	m.containerIDs[appName] = containerID
	m.mu.Unlock()
}

// GetAppContainerID returns the container ID for an app if known
func (m *ServiceManager) GetAppContainerID(appName string) (string, bool) {
	m.mu.RLock()
	id, ok := m.containerIDs[appName]
	m.mu.RUnlock()
	return id, ok
}

// GetBackendHealth returns the debounced backend health state for an endpoint.
// Returns (isHealthy, lastOK). If endpoint is unknown, returns (true, nil) (optimistic).
func (m *ServiceManager) GetBackendHealth(endpointKey string) (bool, *time.Time) {
	if m.backendHealth == nil {
		return true, nil // Optimistic if health state not initialized
	}
	return m.backendHealth.GetHealthState(endpointKey)
}

// GetListenerHealth computes the health status for a listener by aggregating
// certificate status (from remote manager) and backend connectivity (from backend health state).
func (m *ServiceManager) GetListenerHealth(ep ServiceEndpoint) ListenerHealth {
	// 1. Get remote status and certificates
	var remoteEnabled bool
	var solver string
	var portalHostname string
	var certs map[string]*CertificateInfo

	m.statusMu.RLock()
	provider := m.remoteProvider
	m.statusMu.RUnlock()

	if provider != nil {
		status := provider.GetRemoteStatus()
		remoteEnabled = status.Enabled
		solver = status.Solver
		portalHostname = status.PortalHostname

		// Build certificate lookup map
		remoteCerts := provider.GetCertificates()
		certs = make(map[string]*CertificateInfo, len(remoteCerts))
		for _, rc := range remoteCerts {
			certID := resolveCertID(rc.ID, rc.Domains)
			certs[certID] = &CertificateInfo{
				Status:        rc.Status,
				FailureClass:  rc.FailureClass,
				FailureCode:   rc.FailureCode,
				FailureReason: rc.FailureReason,
				RetryAt:       rc.RetryAt,
				ExpiresAt:     rc.ExpiresAt,
			}
		}
	}

	// 2. Get aliases for this app's listener
	var aliases []RemoteAlias
	if provider != nil {
		aliases = provider.GetAliases()
	}

	// 3. Resolve which certificates this listener depends on
	certIDs, _ := ResolveCertificatesForListener(ep, remoteEnabled, solver, portalHostname, aliases)

	// 4. Get backend health state
	endpointKey := ep.App + "/" + ep.Name
	backendOK, lastOK := m.GetBackendHealth(endpointKey)

	// 5. Derive health from all signals
	return DeriveListenerHealth(certIDs, certs, backendOK, lastOK)
}

// startHealthAggregator subscribes to TopicCertificateChanged events and emits
// TopicListenerHealthChanged events for affected listeners. This bridges certificate
// status changes from the remote manager to listener health updates for the UI.
func (m *ServiceManager) startHealthAggregator() {
	m.eventsMu.Lock()
	bus := m.eventBus
	m.eventsMu.Unlock()

	if bus == nil {
		return
	}

	// Cancel any existing aggregator
	if m.healthAggregatorCancel != nil {
		m.healthAggregatorCancel()
	}

	ch, unsubscribe := bus.SubscribeWithCancel(events.TopicCertificateChanged, 64)
	m.healthAggregatorCancel = unsubscribe

	go func() {
		for evt := range ch {
			certEvt, ok := evt.Payload.(events.CertificateChangedEvent)
			if !ok {
				continue
			}
			m.recomputeAffectedListenerHealth(certEvt.CertID)
		}
	}()
}

// recomputeAffectedListenerHealth finds all listeners that depend on the given certificate
// and emits TopicListenerHealthChanged events for each.
func (m *ServiceManager) recomputeAffectedListenerHealth(certID string) {
	m.statusMu.RLock()
	provider := m.remoteProvider
	m.statusMu.RUnlock()

	if provider == nil {
		return
	}

	status := provider.GetRemoteStatus()
	aliases := provider.GetAliases()

	// Iterate all endpoints to find affected listeners
	endpoints := m.GetAll()
	for _, ep := range endpoints {
		certIDs, needsCert := ResolveCertificatesForListener(
			ep, status.Enabled, status.Solver, status.PortalHostname, aliases,
		)
		if !needsCert {
			continue
		}

		// Check if this listener depends on the changed certificate
		for _, cid := range certIDs {
			if cid == certID {
				// Recompute and emit listener health
				health := m.GetListenerHealth(ep)
				m.emitListenerHealthChanged(ep, health)
				break
			}
		}
	}
}

// emitListenerHealthChanged publishes a listener health changed event.
func (m *ServiceManager) emitListenerHealthChanged(ep ServiceEndpoint, health ListenerHealth) {
	m.eventsMu.Lock()
	bus := m.eventBus
	m.eventsMu.Unlock()

	if bus == nil {
		return
	}

	// Convert CertStatuses to events format
	var certStatuses map[string]events.CertHealthStatus
	if len(health.CertStatuses) > 0 {
		certStatuses = make(map[string]events.CertHealthStatus, len(health.CertStatuses))
		for certID, cs := range health.CertStatuses {
			certStatuses[certID] = events.CertHealthStatus{
				Status:      string(cs.Status),
				ReasonCode:  cs.ReasonCode,
				RecoveryETA: cs.RecoveryETA,
			}
		}
	}

	bus.Publish(events.Event{
		Topic: events.TopicListenerHealthChanged,
		Payload: events.ListenerHealthEvent{
			App:      ep.App,
			Listener: ep.Name,
			Health: events.ListenerHealth{
				Status:         string(health.Status),
				ReasonCode:     health.ReasonCode,
				Reason:         health.Reason,
				Details:        health.Details,
				RecoveryETA:    health.RecoveryETA,
				Recoverable:    health.Recoverable,
				ActionRequired: health.ActionRequired,
				CertStatuses:   certStatuses,
				LastChecked:    health.LastChecked,
				LastOK:         health.LastOK,
			},
			Timestamp: time.Now(),
		},
	})
}

// ReconcileResult contains details of changes detected
type ReconcileResult struct {
	Endpoints        []ServiceEndpoint
	Added            []ServiceEndpoint
	Updated          []ServiceEndpoint
	Removed          []ServiceEndpoint
	GuestPortChanged []struct{ Old, New ServiceEndpoint }
	ProxyOnlyChanged []ServiceEndpoint
}

type endpointReplacement struct {
	Old ServiceEndpoint
	New ServiceEndpoint
}

// PreparedReconcile holds allocated candidate listener endpoints that have not
// yet been published to proxy/firewall/registry state.
type PreparedReconcile struct {
	manager         *ServiceManager
	appName         string
	result          ReconcileResult
	containerChange bool
	newMap          map[string]ServiceEndpoint
	allocated       []ServiceEndpoint
	claimReserved   []ServiceEndpoint
	start           []ServiceEndpoint
	stopRelease     []ServiceEndpoint
	proxyRestart    []endpointReplacement
	claimUpdate     []endpointReplacement
	healthRemove    []ServiceEndpoint
	published       bool
	released        bool
	retainedRepair  bool
}

func (p *PreparedReconcile) Result() ReconcileResult {
	if p == nil {
		return ReconcileResult{}
	}
	return p.result
}

func (p *PreparedReconcile) Endpoints() []ServiceEndpoint {
	if p == nil {
		return nil
	}
	out := make([]ServiceEndpoint, len(p.result.Endpoints))
	copy(out, p.result.Endpoints)
	return out
}

func (p *PreparedReconcile) ContainerChange() bool {
	if p == nil {
		return false
	}
	return p.containerChange
}

// Release abandons prepared endpoint allocations without publishing them.
func (p *PreparedReconcile) Release() {
	if p == nil || p.manager == nil {
		return
	}
	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()
	if p.published || p.released {
		return
	}
	if p.retainedRepair {
		p.released = true
		return
	}
	for _, ep := range p.allocated {
		p.manager.releaseEndpointPorts(ep)
	}
	for _, ep := range p.claimReserved {
		p.manager.releaseEndpointPublicAllocation(ep)
	}
	p.manager.rebuildPortClaimCache()
	p.released = true
}

// RetainReservationsForRepair records prepared endpoint ownership after a
// durable commit crossed but publication failed. This keeps same-process repair
// from treating the retained allocator reservations as ownerless conflicts.
func (p *PreparedReconcile) RetainReservationsForRepair() {
	if p == nil || p.manager == nil {
		return
	}
	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()
	if p.published || p.released {
		return
	}
	p.manager.retainPreparedReservationsLocked(p.appName, p.newMap)
	p.retainedRepair = true
	p.released = true
}

// Publish makes the prepared endpoint set authoritative and starts/stops public
// proxy/firewall/remote publication state.
func (p *PreparedReconcile) Publish() (ReconcileResult, bool, error) {
	if p == nil || p.manager == nil {
		return ReconcileResult{}, false, fmt.Errorf("prepared reconcile is nil")
	}
	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()
	if p.released {
		return ReconcileResult{}, false, fmt.Errorf("prepared reconcile already released")
	}
	if p.published {
		return p.result, p.containerChange, nil
	}
	if err := p.manager.publishPreparedReconcileLocked(p); err != nil {
		return ReconcileResult{}, false, err
	}
	p.published = true
	return p.result, p.containerChange, nil
}

// PrepareReconcile allocates candidate listener endpoints without publishing
// them to proxy/firewall/registry state. Call Publish after the durable commit
// boundary, or Release on rollback.
func (m *ServiceManager) PrepareReconcile(appName string, listeners []api.AppListener) (*PreparedReconcile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prepareReconcileLocked(appName, listeners)
}

func (m *ServiceManager) prepareReconcileLocked(appName string, listeners []api.AppListener) (*PreparedReconcile, error) {
	existing := m.registry[appName]
	if existing == nil {
		existing = make(map[string]ServiceEndpoint)
	}

	// Determine the primary listener for host-based routing
	primaryName, _ := hostname.ResolvePrimaryListener(listeners)

	// Check for host label collisions before making changes
	if err := m.checkHostLabelCollisions(appName, listeners, primaryName); err != nil {
		return nil, err
	}

	newMap := make(map[string]ServiceEndpoint)
	containerChange := false
	result := ReconcileResult{}
	incomingNames := make(map[string]struct{}, len(listeners))
	for _, l := range listeners {
		incomingNames[l.Name] = struct{}{}
	}
	removedByClaim := make(map[string]ServiceEndpoint)
	for name, ep := range existing {
		if _, keep := incomingNames[name]; keep || ep.PortClaim == nil {
			continue
		}
		removedByClaim[publicKey(*ep.PortClaim, ep.Flow.TransportProtocol())] = ep
	}
	reusedRemoved := make(map[string]struct{})
	prepared := &PreparedReconcile{
		manager: m,
		appName: appName,
		newMap:  newMap,
	}
	releasePrepared := func() {
		for _, ep := range prepared.allocated {
			m.releaseEndpointPorts(ep)
		}
		for _, ep := range prepared.claimReserved {
			m.releaseEndpointPublicAllocation(ep)
		}
	}

	// Index new by name
	for _, l := range listeners {
		isPrimary := l.Name == primaryName
		hostLabel := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l))

		if old, ok := existing[l.Name]; ok {
			// If the public claim or transport changes, treat as remove + add
			// because allocator/firewall ownership is protocol-specific.
			reuseCurrentPublicClaim := portClaimChanged(old.PortClaim, l.PortClaim) &&
				l.PortClaim != nil &&
				*l.PortClaim == old.PublicPort &&
				old.Flow.TransportProtocol() == l.Flow.TransportProtocol()
			if !reuseCurrentPublicClaim && (portClaimChanged(old.PortClaim, l.PortClaim) || old.Flow.TransportProtocol() != l.Flow.TransportProtocol()) {
				result.Removed = append(result.Removed, old)
				prepared.stopRelease = append(prepared.stopRelease, old)

				// Allocate new
				hb, pp, err := m.allocateForListenerLocked(appName, l, false)
				if err != nil {
					releasePrepared()
					return nil, err
				}
				newEp := buildEndpoint(appName, l, hb, pp, isPrimary, hostLabel)
				newMap[l.Name] = newEp
				prepared.allocated = append(prepared.allocated, newEp)
				prepared.start = append(prepared.start, newEp)
				containerChange = true
				result.Added = append(result.Added, newEp)
				continue
			}

			newEp := buildEndpoint(appName, l, old.HostBind, old.PublicPort, isPrimary, hostLabel)

			if old.GuestPort != newEp.GuestPort {
				containerChange = true
				result.GuestPortChanged = append(result.GuestPortChanged, struct{ Old, New ServiceEndpoint }{Old: old, New: newEp})
			}

			if old.DerivedHostLabel != hostLabel {
				result.Removed = append(result.Removed, old)
				result.Added = append(result.Added, newEp)
			}
			if endpointMDNSVisibilityChanged(old, newEp) {
				result.Updated = append(result.Updated, newEp)
			}
			if reuseCurrentPublicClaim {
				oldKey := endpointPublicAllocationKey(old)
				newKey := endpointPublicAllocationKey(newEp)
				if oldKey != newKey {
					if err := m.ensurePortClaimAvailableLocked(appName, l.Name, newEp.PublicPort, newEp.Flow, true); err != nil {
						releasePrepared()
						return nil, err
					}
					if _, exists := m.allocator.usedPublic[newKey]; exists {
						releasePrepared()
						return nil, fmt.Errorf("%s port %d already in use", newEp.Flow.TransportProtocol(), newEp.PublicPort)
					}
					m.allocator.usedPublic[newKey] = struct{}{}
					prepared.claimReserved = append(prepared.claimReserved, newEp)
				}
				prepared.claimUpdate = append(prepared.claimUpdate, endpointReplacement{Old: old, New: newEp})
				result.Updated = append(result.Updated, newEp)
			}

			if proxyConfigChanged(old, newEp) {
				prepared.proxyRestart = append(prepared.proxyRestart, endpointReplacement{Old: old, New: newEp})
				result.ProxyOnlyChanged = append(result.ProxyOnlyChanged, newEp)
			}

			newMap[l.Name] = newEp
		} else {
			if l.PortClaim != nil {
				claimKey := publicKey(*l.PortClaim, l.Flow.TransportProtocol())
				if old, ok := removedByClaim[claimKey]; ok {
					if _, used := reusedRemoved[old.Name]; !used {
						newEp := buildEndpoint(appName, l, old.HostBind, old.PublicPort, isPrimary, hostLabel)
						newMap[l.Name] = newEp
						prepared.proxyRestart = append(prepared.proxyRestart, endpointReplacement{Old: old, New: newEp})
						prepared.healthRemove = append(prepared.healthRemove, old)
						reusedRemoved[old.Name] = struct{}{}
						containerChange = true
						result.Removed = append(result.Removed, old)
						result.Added = append(result.Added, newEp)
						continue
					}
				}
			}
			// New listener: allocate ports, start proxy, mark container change
			hb, pp, err := m.allocateForListenerLocked(appName, l, false)
			if err != nil {
				releasePrepared()
				return nil, err
			}
			ep := buildEndpoint(appName, l, hb, pp, isPrimary, hostLabel)
			newMap[l.Name] = ep
			prepared.allocated = append(prepared.allocated, ep)
			prepared.start = append(prepared.start, ep)
			containerChange = true
			result.Added = append(result.Added, ep)
		}
	}

	// Removed listeners
	for name, ep := range existing {
		if _, ok := newMap[name]; !ok {
			if _, reused := reusedRemoved[name]; reused {
				continue
			}
			containerChange = true
			result.Removed = append(result.Removed, ep)
			prepared.stopRelease = append(prepared.stopRelease, ep)
			prepared.healthRemove = append(prepared.healthRemove, ep)
		}
	}

	// Return endpoints slice
	var eps []ServiceEndpoint
	for _, ep := range newMap {
		eps = append(eps, ep)
	}
	result.Endpoints = eps
	prepared.result = result
	prepared.containerChange = containerChange
	return prepared, nil
}

func (m *ServiceManager) startEndpointPublicationLocked(ep ServiceEndpoint) error {
	m.openFirewallClaim(ep)
	if err := m.proxyManager.StartListenerChecked(ep); err != nil {
		m.closeFirewallClaim(ep)
		return fmt.Errorf("start listener %s/%s public %d: %w", ep.App, ep.Name, ep.PublicPort, err)
	}
	m.notifyPublish(ep.PublicPort)
	return nil
}

func (m *ServiceManager) stopEndpointPublicationLocked(ep ServiceEndpoint) {
	m.proxyManager.StopEndpoint(ep.PublicPort, ep.Flow)
	m.closeFirewallClaim(ep)
	m.notifyUnpublish(ep.PublicPort)
}

func (m *ServiceManager) publishPreparedReconcileLocked(prepared *PreparedReconcile) error {
	wasDeactivated := len(m.deactivated[prepared.appName]) > 0
	started := []ServiceEndpoint{}
	restarted := []endpointReplacement{}
	startedKeys := make(map[string]struct{})
	rollbackStarted := func(cause error) error {
		errs := []error{cause}
		for i := len(started) - 1; i >= 0; i-- {
			m.stopEndpointPublicationLocked(started[i])
		}
		for i := len(restarted) - 1; i >= 0; i-- {
			replacement := restarted[i]
			if wasDeactivated {
				m.stopEndpointPublicationLocked(replacement.New)
				continue
			}
			m.proxyManager.StopEndpoint(replacement.New.PublicPort, replacement.New.Flow)
			if err := m.proxyManager.StartListenerChecked(replacement.Old); err != nil {
				errs = append(errs, fmt.Errorf("restore listener %s/%s public %d: %w", replacement.Old.App, replacement.Old.Name, replacement.Old.PublicPort, err))
			}
		}
		return errors.Join(errs...)
	}

	for _, replacement := range prepared.proxyRestart {
		m.proxyManager.StopEndpoint(replacement.Old.PublicPort, replacement.Old.Flow)
		var err error
		if wasDeactivated {
			err = m.startEndpointPublicationLocked(replacement.New)
		} else if startErr := m.proxyManager.StartListenerChecked(replacement.New); startErr != nil {
			err = fmt.Errorf("restart listener %s/%s public %d: %w", replacement.New.App, replacement.New.Name, replacement.New.PublicPort, startErr)
		}
		if err != nil {
			if !wasDeactivated {
				if restoreErr := m.proxyManager.StartListenerChecked(replacement.Old); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("restore listener %s/%s public %d: %w", replacement.Old.App, replacement.Old.Name, replacement.Old.PublicPort, restoreErr))
				}
			}
			return rollbackStarted(err)
		}
		restarted = append(restarted, replacement)
		startedKeys[replacement.New.endpointKey()] = struct{}{}
	}
	for _, ep := range prepared.start {
		if err := m.startEndpointPublicationLocked(ep); err != nil {
			return rollbackStarted(err)
		}
		started = append(started, ep)
		startedKeys[ep.endpointKey()] = struct{}{}
	}
	for _, replacement := range prepared.claimUpdate {
		if wasDeactivated {
			if _, alreadyStarted := startedKeys[replacement.New.endpointKey()]; alreadyStarted {
				continue
			}
			if err := m.startEndpointPublicationLocked(replacement.New); err != nil {
				return rollbackStarted(err)
			}
			started = append(started, replacement.New)
			startedKeys[replacement.New.endpointKey()] = struct{}{}
		} else {
			m.closeFirewallClaim(replacement.Old)
			m.openFirewallClaim(replacement.New)
		}
	}
	if wasDeactivated {
		for _, ep := range prepared.newMap {
			if _, alreadyStarted := startedKeys[ep.endpointKey()]; alreadyStarted {
				continue
			}
			if err := m.startEndpointPublicationLocked(ep); err != nil {
				return rollbackStarted(err)
			}
			started = append(started, ep)
			startedKeys[ep.endpointKey()] = struct{}{}
		}
	}
	for _, replacement := range prepared.claimUpdate {
		oldKey := endpointPublicAllocationKey(replacement.Old)
		newKey := endpointPublicAllocationKey(replacement.New)
		if oldKey != newKey {
			m.allocator.usedPublic[newKey] = struct{}{}
			delete(m.allocator.usedPublic, oldKey)
		}
	}
	for _, ep := range prepared.stopRelease {
		if !wasDeactivated {
			m.stopEndpointPublicationLocked(ep)
		}
		m.releaseEndpointPorts(ep)
	}
	for _, ep := range prepared.healthRemove {
		if m.backendHealth != nil {
			m.backendHealth.RemoveEndpoint(ep.endpointKey())
		}
	}

	m.registry[prepared.appName] = prepared.newMap
	delete(m.deactivated, prepared.appName)
	m.rebuildPortClaimCache()

	// Publish endpoint changes (non-blocking). Listener config changes are
	// permanent — removed endpoints will not come back.
	if len(prepared.result.Added) > 0 || len(prepared.result.Updated) > 0 || len(prepared.result.Removed) > 0 {
		m.publishEndpointsEvent(events.ServiceEndpointsChanged{
			App:     prepared.appName,
			Added:   endpointInfoSlice(prepared.result.Added),
			Updated: endpointInfoSlice(prepared.result.Updated),
			Removed: endpointInfoSlice(prepared.result.Removed),
		})
	}
	return nil
}

// Reconcile synchronizes listeners for an app in-place. Returns final endpoints and whether container changes are required.
func (m *ServiceManager) Reconcile(appName string, listeners []api.AppListener) (ReconcileResult, bool, error) {
	prepared, err := m.PrepareReconcile(appName, listeners)
	if err != nil {
		return ReconcileResult{}, false, err
	}
	result, containerChange, err := prepared.Publish()
	if err != nil {
		prepared.Release()
		return ReconcileResult{}, false, err
	}
	return result, containerChange, nil
}

func middlewareEqual(a, b []api.AppProtocolMiddleware) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
		if !reflect.DeepEqual(a[i].Params, b[i].Params) {
			return false
		}
	}
	return true
}

// portClaimChanged returns true if two port claim values differ.
func portClaimChanged(a, b *int) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

// proxyConfigChanged returns true when proxy-affecting fields differ between
// two endpoints. Centralises the restart-decision so new fields are checked
// in one place.
func proxyConfigChanged(old, cur ServiceEndpoint) bool {
	return old.Flow != cur.Flow ||
		old.Protocol != cur.Protocol ||
		!middlewareEqual(old.Middleware, cur.Middleware) ||
		!authEqual(old.Auth, cur.Auth) ||
		!connectionAuthEqual(old.ConnectionAuth, cur.ConnectionAuth)
}

func endpointMDNSVisibilityChanged(old, cur ServiceEndpoint) bool {
	if old.DerivedHostLabel != cur.DerivedHostLabel {
		return false
	}
	return endpointMDNSAdvertisable(old) != endpointMDNSAdvertisable(cur)
}

func endpointMDNSAdvertisable(ep ServiceEndpoint) bool {
	return ep.DerivedHostLabel != "" &&
		!ep.RequiresTLSMuxAuth &&
		api.LanHostBasedEligible(ep.Flow, ep.Protocol)
}

// connectionAuthEqual compares two ConnectionAuth pointers for equality.
// Per plan §D17 upgrade-churn fix (F10): nil and an explicit
// {Default:"allow", Rules:nil} compare equal — both encode the same
// "implicit allow, no rules" semantics. Default="" normalizes to "allow".
// Rules:nil and Rules:[] also compare equal (sibling-shape consistency
// per F-It3-D, mirrors middlewareEqual's `len(nil) == len([])`).
func connectionAuthEqual(a, b *api.ConnectionAuth) bool {
	return connectionAuthDefault(a) == connectionAuthDefault(b) &&
		slices.Equal(connectionAuthRules(a), connectionAuthRules(b)) &&
		connectionAuthMTLSEqual(a, b)
}

func connectionAuthDefault(a *api.ConnectionAuth) string {
	if a == nil || a.Default == "" {
		return "allow"
	}
	return a.Default
}

func connectionAuthRules(a *api.ConnectionAuth) []api.ConnectionAuthRule {
	if a == nil {
		return nil
	}
	return a.Rules
}

func connectionAuthMTLSEqual(a, b *api.ConnectionAuth) bool {
	var at, bt string
	if a != nil && a.MTLS != nil {
		at = a.MTLS.Verifier.Type
	}
	if b != nil && b.MTLS != nil {
		bt = b.MTLS.Verifier.Type
	}
	return at == bt
}

// authEqual compares two ListenerAuth pointers for equality.
// nil and empty-rules are treated as equivalent (both resolve to "protected"
// for all paths in listenerStrategyForPath).
// Order-sensitive: rule ordering matters because listenerStrategyForPath uses
// first-match-wins semantics.
// ListenerAuthRule is a flat struct of 3 string fields — != works for element
// comparison. If fields are added to ListenerAuthRule, this must be revisited.
func authEqual(a, b *api.ListenerAuth) bool {
	return slices.Equal(authRules(a), authRules(b))
}

func authRules(a *api.ListenerAuth) []api.ListenerAuthRule {
	if a == nil {
		return nil
	}
	return a.Rules
}

// DeactivateApp tears down routing for a temporarily stopped app.
// The app still exists and endpoints will be restored on start.
func (m *ServiceManager) DeactivateApp(appName string) {
	m.removeAppEndpoints(appName, false)
}

// SuspendAppPublication temporarily closes an app's public/remote publication
// while preserving registry endpoints and their allocated ports. Container
// recreation can still reuse the existing HostBind mappings, and
// ResumeAppPublication can restore the same public surface after commit.
func (m *ServiceManager) SuspendAppPublication(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapp, ok := m.registry[appName]
	if !ok {
		return
	}
	endpoints := make([]ServiceEndpoint, 0, len(mapp))
	for _, ep := range mapp {
		endpoints = append(endpoints, ep)
		m.proxyManager.StopEndpoint(ep.PublicPort, ep.Flow)
		m.closeFirewallClaim(ep)
		m.notifyUnpublish(ep.PublicPort)
		if m.backendHealth != nil {
			m.backendHealth.RemoveEndpoint(ep.endpointKey())
		}
	}
	if len(endpoints) > 0 {
		info := endpointInfoSlice(endpoints)
		m.deactivated[appName] = info
		m.rebuildPortClaimCache()
		m.publishEndpointsEvent(events.ServiceEndpointsChanged{
			App:         appName,
			Deactivated: info,
		})
	}
}

// ResumeAppPublication restarts proxy/firewall/remote publication for an app
// whose registry endpoints were preserved by SuspendAppPublication.
func (m *ServiceManager) ResumeAppPublication(appName string) {
	if err := m.ResumeAppPublicationChecked(appName); err != nil {
		log.Printf("WARN: resume app publication %s: %v", appName, err)
	}
}

// ResumeAppPublicationChecked restarts publication and reports listener bind
// failures to callers that need transaction recovery to remain open.
func (m *ServiceManager) ResumeAppPublicationChecked(appName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapp, ok := m.registry[appName]
	if !ok {
		return nil
	}
	endpoints := make([]ServiceEndpoint, 0, len(mapp))
	started := []ServiceEndpoint{}
	for _, ep := range mapp {
		endpoints = append(endpoints, ep)
		if err := m.startEndpointPublicationLocked(ep); err != nil {
			for i := len(started) - 1; i >= 0; i-- {
				m.stopEndpointPublicationLocked(started[i])
			}
			return err
		}
		started = append(started, ep)
	}
	delete(m.deactivated, appName)
	m.rebuildPortClaimCache()
	if len(endpoints) > 0 {
		m.publishEndpointsEvent(events.ServiceEndpointsChanged{
			App:   appName,
			Added: endpointInfoSlice(endpoints),
		})
	}
	return nil
}

// RestorePreparedPublication publishes a prepared endpoint set that was
// durably recorded before a manifest update commit. It is used during recovery
// when the in-memory PreparedReconcile was lost after the ledger crossed.
func (m *ServiceManager) RestorePreparedPublication(appName string, endpoints []ServiceEndpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	newMap := make(map[string]ServiceEndpoint, len(endpoints))
	newHosts := make(map[int]struct{}, len(endpoints))
	newPublic := make(map[string]struct{}, len(endpoints))
	added := make([]ServiceEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if ep.Name == "" {
			return fmt.Errorf("restore prepared publication: endpoint name is required")
		}
		if ep.App == "" {
			ep.App = appName
		}
		newMap[ep.Name] = ep
		newHosts[ep.HostBind] = struct{}{}
		newPublic[endpointPublicAllocationKey(ep)] = struct{}{}
		added = append(added, ep)
	}
	if err := m.reservePreparedPublicationLocked(appName, newMap); err != nil {
		return err
	}

	var removed []ServiceEndpoint
	wasDeactivated := len(m.deactivated[appName]) > 0
	stoppedExisting := []ServiceEndpoint{}
	if existing, ok := m.registry[appName]; ok {
		for _, ep := range existing {
			removed = append(removed, ep)
			if !wasDeactivated {
				m.stopEndpointPublicationLocked(ep)
				stoppedExisting = append(stoppedExisting, ep)
			}
		}
	}
	started := []ServiceEndpoint{}
	rollbackRestore := func(cause error) error {
		errs := []error{cause}
		for i := len(started) - 1; i >= 0; i-- {
			m.stopEndpointPublicationLocked(started[i])
		}
		if !wasDeactivated {
			for i := len(stoppedExisting) - 1; i >= 0; i-- {
				ep := stoppedExisting[i]
				if err := m.startEndpointPublicationLocked(ep); err != nil {
					errs = append(errs, fmt.Errorf("restore listener %s/%s public %d: %w", ep.App, ep.Name, ep.PublicPort, err))
				}
			}
		}
		return errors.Join(errs...)
	}
	for _, ep := range added {
		if err := m.startEndpointPublicationLocked(ep); err != nil {
			return rollbackRestore(err)
		}
		started = append(started, ep)
	}
	for _, ep := range removed {
		if m.backendHealth != nil {
			m.backendHealth.RemoveEndpoint(ep.endpointKey())
		}
		if _, keep := newHosts[ep.HostBind]; !keep {
			m.allocator.ReleaseHost(ep.HostBind)
		}
		if _, keep := newPublic[endpointPublicAllocationKey(ep)]; !keep {
			if ep.PortClaim != nil {
				m.allocator.FreePublicProto(ep.PublicPort, ep.Flow.TransportProtocol())
			} else {
				m.allocator.ReleasePublic(ep.PublicPort)
			}
		}
	}
	if len(newMap) > 0 {
		m.registry[appName] = newMap
	} else {
		delete(m.registry, appName)
	}
	delete(m.deactivated, appName)
	m.rebuildPortClaimCache()
	delete(m.preparedReservations, appName)
	if len(added) > 0 || len(removed) > 0 {
		m.publishEndpointsEvent(events.ServiceEndpointsChanged{
			App:     appName,
			Added:   endpointInfoSlice(added),
			Removed: endpointInfoSlice(removed),
		})
	}
	return nil
}

// RemoveApp permanently removes all endpoints for an app (uninstall, failed install).
// Downstream listeners (e.g., cert cleanup) act on the permanent signal.
func (m *ServiceManager) RemoveApp(appName string) {
	m.removeAppEndpoints(appName, true)
	m.proxyManager.ClearOIDCAuthorizePaths(appName)
}

// SetAppOIDCConfig stores OIDC authorize_paths for an app on the proxy manager.
// These paths scope Layer 2 body rewriting for OIDC authorization URLs on WAN.
func (m *ServiceManager) SetAppOIDCConfig(appName string, authorizePaths []string) {
	m.proxyManager.SetOIDCAuthorizePaths(appName, authorizePaths)
}

func (m *ServiceManager) removeAppEndpoints(appName string, permanent bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var removed []ServiceEndpoint
	if mapp, ok := m.registry[appName]; ok {
		removed = make([]ServiceEndpoint, 0, len(mapp))
		for _, ep := range mapp {
			removed = append(removed, ep)
			m.proxyManager.StopEndpoint(ep.PublicPort, ep.Flow)
			m.closeFirewallClaim(ep)
			m.releaseEndpointPorts(ep)
			m.notifyUnpublish(ep.PublicPort)
			if m.backendHealth != nil {
				m.backendHealth.RemoveEndpoint(ep.endpointKey())
			}
		}
		delete(m.registry, appName)
	}
	if permanent {
		m.releaseStalePreparedReservationsLocked(appName, nil, nil)
	}
	m.rebuildPortClaimCache()
	delete(m.containerIDs, appName)

	m.appTransientMu.Lock()
	delete(m.appTransient, appName)
	m.appTransientMu.Unlock()

	info := endpointInfoSlice(removed)

	if !permanent {
		// Stash endpoint metadata so a subsequent RemoveApp can emit a
		// permanent Removed event even though the endpoints are already gone.
		if len(info) > 0 {
			m.deactivated[appName] = info
			m.publishEndpointsEvent(events.ServiceEndpointsChanged{
				App:         appName,
				Deactivated: info,
			})
		}
	} else {
		// Include any previously deactivated endpoints that are no longer
		// in the registry (app was stopped before uninstall).
		if prev, ok := m.deactivated[appName]; ok {
			info = append(info, prev...)
			delete(m.deactivated, appName)
		}
		if len(info) > 0 {
			m.publishEndpointsEvent(events.ServiceEndpointsChanged{
				App:     appName,
				Removed: info,
			})
		}
	}
}
