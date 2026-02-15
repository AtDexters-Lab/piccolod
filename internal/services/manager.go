package services

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
	"piccolod/internal/events"
	"piccolod/internal/hostname"
)

// ServiceManager coordinates listener allocation, registry, and proxy startup
type ServiceManager struct {
	allocator      *PortAllocator
	registry       map[string]map[string]ServiceEndpoint // app -> name -> endpoint
	proxyManager   *ProxyManager
	mu             sync.RWMutex
	stopCh         chan struct{}
	wg             sync.WaitGroup
	containerIDs   map[string]string // app -> containerID (optional)
	eventsMu       sync.Mutex
	eventCancel    context.CancelFunc
	eventBus       *events.Bus
	statusMu       sync.RWMutex
	leadership     map[string]cluster.Role
	unpublisher    PortUnpublisher
	publisher      PortPublisher
	lockReader     LockStateReader
	lockOverrideMu sync.RWMutex
	lockOverride   *bool

	// Backend health debouncing (RFC 20260125)
	backendHealth *BackendHealthState

	// Remote status provider for health computation (RFC 20260125)
	remoteProvider RemoteStatusProvider

	// Health aggregator cancellation
	healthAggregatorCancel func()

	// App status tracking for health check suppression
	appTransient    map[string]time.Time // app ID → time entered transient state
	appTransientMu  sync.RWMutex
	appStatusCancel func() // unsubscribe from app status events
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
		allocator:     allocator,
		registry:      make(map[string]map[string]ServiceEndpoint),
		proxyManager:  NewProxyManager(),
		stopCh:        make(chan struct{}),
		containerIDs:  make(map[string]string),
		leadership:    make(map[string]cluster.Role),
		backendHealth: NewBackendHealthState(),
		appTransient:  make(map[string]time.Time),
	}
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

func (m *ServiceManager) publishEndpointsChanged(app string, added, removed []ServiceEndpoint) {
	m.eventsMu.Lock()
	bus := m.eventBus
	m.eventsMu.Unlock()
	if bus == nil {
		return
	}
	addedInfo := make([]events.ServiceEndpointInfo, len(added))
	for i, ep := range added {
		addedInfo[i] = events.ServiceEndpointInfo{
			App:              ep.App,
			Name:             ep.Name,
			DerivedHostLabel: ep.DerivedHostLabel,
		}
	}
	removedInfo := make([]events.ServiceEndpointInfo, len(removed))
	for i, ep := range removed {
		removedInfo[i] = events.ServiceEndpointInfo{
			App:              ep.App,
			Name:             ep.Name,
			DerivedHostLabel: ep.DerivedHostLabel,
		}
	}
	bus.Publish(events.Event{
		Topic: events.TopicServiceEndpointsChanged,
		Payload: events.ServiceEndpointsChanged{
			App:     app,
			Added:   addedInfo,
			Removed: removedInfo,
		},
	})
}

const ACMEHTTPFallbackPort = 5002

func normalizeRemotePort(port int) int {
	if port == ACMEHTTPFallbackPort {
		return 80
	}
	return port
}

func defaultRemotePorts(listener api.AppListener) []int {
	if len(listener.RemotePorts) == 0 {
		return []int{80, 443}
	}
	ports := make([]int, len(listener.RemotePorts))
	copy(ports, listener.RemotePorts)
	return ports
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

	leaders := bus.Subscribe(events.TopicLeadershipRoleChanged, 16)
	locks := bus.Subscribe(events.TopicLockStateChanged, 8)

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
	m.eventsMu.Unlock()
}

// StopRuntimeEvents cancels leadership/lock subscriptions and waits for handlers to exit.
func (m *ServiceManager) StopRuntimeEvents() {
	m.stopEventObservers()
	m.wg.Wait()
}

// ProxyManager returns the underlying ProxyManager.
func (m *ServiceManager) ProxyManager() *ProxyManager { return m.proxyManager }

func (m *ServiceManager) RegisterProxyHint(listenerPort, sourcePort, remotePort int, isTLS bool) {
	if listenerPort <= 0 || sourcePort <= 0 || m.proxyManager == nil {
		return
	}
	m.proxyManager.registerHint(listenerPort, sourcePort, connectionHint{isTLS: isTLS, remotePort: remotePort})
}

func (m *ServiceManager) consumeProxyHint(listenerPort, sourcePort int) (connectionHint, bool) {
	if listenerPort <= 0 || sourcePort <= 0 || m.proxyManager == nil {
		return connectionHint{}, false
	}
	return m.proxyManager.consumeHint(listenerPort, sourcePort)
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
func (m *ServiceManager) RestoreFromPodman(appName string, listeners []api.AppListener, hostByGuest map[int]int) ([]ServiceEndpoint, error) {
	// Stop any existing proxies first
	m.RemoveApp(appName)

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
		host, ok := hostByGuest[l.GuestPort]
		if !ok {
			continue
		}
		if err := m.allocator.ReserveHost(host); err != nil {
			continue
		}
		public, err := m.allocator.AllocatePublic()
		if err != nil {
			m.allocator.freeHost(host)
			return endpoints, err
		}
		remotePorts := defaultRemotePorts(l)
		isPrimary := l.Name == primaryName
		hostLabel := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l.Protocol, l.Flow))
		ep := ServiceEndpoint{
			App:              appName,
			Name:             l.Name,
			GuestPort:        l.GuestPort,
			HostBind:         host,
			PublicPort:       public,
			Flow:             l.Flow,
			Protocol:         l.Protocol,
			Primary:          isPrimary,
			DerivedHostLabel: hostLabel,
			Middleware:       l.Middleware,
			RemotePorts:      remotePorts,
			Auth:             l.Auth,
		}
		registry[l.Name] = ep
		endpoints = append(endpoints, ep)
		m.proxyManager.StartListener(ep)
		m.notifyPublish(ep.PublicPort)
	}

	if len(registry) > 0 {
		m.registry[appName] = registry
	}

	// Publish endpoint changes (non-blocking)
	if len(endpoints) > 0 {
		m.publishEndpointsChanged(appName, endpoints, nil)
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

	for _, l := range listeners {
		hb, pp, err := m.allocator.AllocatePair()
		if err != nil {
			return nil, err
		}
		remotePorts := defaultRemotePorts(l)
		isPrimary := l.Name == primaryName
		hostLabel := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l.Protocol, l.Flow))
		ep := ServiceEndpoint{
			App:              appName,
			Name:             l.Name,
			GuestPort:        l.GuestPort,
			HostBind:         hb,
			PublicPort:       pp,
			Flow:             l.Flow,
			Protocol:         l.Protocol,
			Primary:          isPrimary,
			DerivedHostLabel: hostLabel,
			Middleware:       l.Middleware,
			RemotePorts:      remotePorts,
			Auth:             l.Auth,
		}
		endpoints = append(endpoints, ep)
		if _, ok := m.registry[appName]; !ok {
			m.registry[appName] = make(map[string]ServiceEndpoint)
		}
		m.registry[appName][l.Name] = ep
	}

	// Start proxies after registration
	for _, ep := range endpoints {
		m.proxyManager.StartListener(ep)
		m.notifyPublish(ep.PublicPort)
	}

	// Publish endpoint changes (non-blocking)
	m.publishEndpointsChanged(appName, endpoints, nil)

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
	if len(ep.RemotePorts) == 0 {
		return remotePort == 80 || remotePort == 443
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
		label := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l.Protocol, l.Flow))
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
// 1. ServiceManager doesn't have access to certificate data (avoiding import cycle)
// 2. Full health is computed on-demand via API (handleGinListenerHealth) and on WebSocket connect
// 3. This event serves as a notification that health may have changed, prompting clients to
//    refresh via API if needed
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
	Removed          []ServiceEndpoint
	GuestPortChanged []struct{ Old, New ServiceEndpoint }
	ProxyOnlyChanged []ServiceEndpoint
}

// Reconcile synchronizes listeners for an app in-place. Returns final endpoints and whether container changes are required.
func (m *ServiceManager) Reconcile(appName string, listeners []api.AppListener) (ReconcileResult, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.registry[appName]
	if existing == nil {
		existing = make(map[string]ServiceEndpoint)
	}

	// Determine the primary listener for host-based routing
	primaryName, _ := hostname.ResolvePrimaryListener(listeners)

	// Check for host label collisions before making changes
	if err := m.checkHostLabelCollisions(appName, listeners, primaryName); err != nil {
		return ReconcileResult{}, false, err
	}

	newMap := make(map[string]ServiceEndpoint)
	containerChange := false
	result := ReconcileResult{}

	// Index new by name
	for _, l := range listeners {
		isPrimary := l.Name == primaryName
		hostLabel := hostname.DeriveHostLabel(appName, l.Name, isPrimary, IsEligibleForHostRouting(l.Protocol, l.Flow))

		if old, ok := existing[l.Name]; ok {
			// Reuse ports; update config
			ep := old
			// Detect guest port change
			if ep.GuestPort != l.GuestPort {
				containerChange = true
				result.GuestPortChanged = append(result.GuestPortChanged, struct{ Old, New ServiceEndpoint }{
					Old: ep,
					New: ServiceEndpoint{
						App:              appName,
						Name:             l.Name,
						GuestPort:        l.GuestPort,
						HostBind:         ep.HostBind,
						PublicPort:       ep.PublicPort,
						Flow:             l.Flow,
						Protocol:         l.Protocol,
						Primary:          isPrimary,
						DerivedHostLabel: hostLabel,
						Middleware:       l.Middleware,
						RemotePorts:      defaultRemotePorts(l),
					},
				})
			}
			ep.GuestPort = l.GuestPort
			// Only restart proxy if proxy-related fields changed
			proxyChanged := ep.Flow != l.Flow || ep.Protocol != l.Protocol || !middlewareEqual(ep.Middleware, l.Middleware)
			ep.Flow = l.Flow
			ep.Protocol = l.Protocol
			ep.Primary = isPrimary
			oldHostLabel := ep.DerivedHostLabel
			ep.DerivedHostLabel = hostLabel
			ep.Middleware = l.Middleware
			ep.RemotePorts = defaultRemotePorts(l)
			newMap[l.Name] = ep

			// Detect host label changes for mDNS updates
			if oldHostLabel != hostLabel {
				oldEp := old
				oldEp.DerivedHostLabel = oldHostLabel
				result.Removed = append(result.Removed, oldEp)
				result.Added = append(result.Added, ep)
			}

			if proxyChanged {
				m.proxyManager.StopPort(ep.PublicPort)
				m.proxyManager.StartListener(ep)
				result.ProxyOnlyChanged = append(result.ProxyOnlyChanged, ep)
				m.notifyPublish(ep.PublicPort)
			}
		} else {
			// New listener: allocate ports, start proxy, mark container change
			hb, pp, err := m.allocator.AllocatePair()
			if err != nil {
				return ReconcileResult{}, false, err
			}
			ep := ServiceEndpoint{
				App:              appName,
				Name:             l.Name,
				GuestPort:        l.GuestPort,
				HostBind:         hb,
				PublicPort:       pp,
				Flow:             l.Flow,
				Protocol:         l.Protocol,
				Primary:          isPrimary,
				DerivedHostLabel: hostLabel,
				Middleware:       l.Middleware,
				RemotePorts:      defaultRemotePorts(l),
			}
			newMap[l.Name] = ep
			m.proxyManager.StartListener(ep)
			containerChange = true
			result.Added = append(result.Added, ep)
			m.notifyPublish(ep.PublicPort)
		}
	}

	// Removed listeners
	for name, ep := range existing {
		if _, ok := newMap[name]; !ok {
			m.proxyManager.StopPort(ep.PublicPort)
			containerChange = true
			result.Removed = append(result.Removed, ep)
			m.notifyUnpublish(ep.PublicPort)
			if m.backendHealth != nil {
				m.backendHealth.RemoveEndpoint(ep.endpointKey())
			}
		}
	}

	// Save
	m.registry[appName] = newMap

	// Return endpoints slice
	var eps []ServiceEndpoint
	for _, ep := range newMap {
		eps = append(eps, ep)
	}
	result.Endpoints = eps

	// Publish endpoint changes (non-blocking)
	if len(result.Added) > 0 || len(result.Removed) > 0 {
		m.publishEndpointsChanged(appName, result.Added, result.Removed)
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
		// Params equality elided for v1
	}
	return true
}

// RemoveApp stops and removes all listeners for an app
func (m *ServiceManager) RemoveApp(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var removed []ServiceEndpoint
	if mapp, ok := m.registry[appName]; ok {
		removed = make([]ServiceEndpoint, 0, len(mapp))
		for _, ep := range mapp {
			removed = append(removed, ep)
			m.proxyManager.StopPort(ep.PublicPort)
			m.allocator.Release(ep.HostBind, ep.PublicPort)
			m.notifyUnpublish(ep.PublicPort)
			// Clean up backend health state for removed endpoint
			if m.backendHealth != nil {
				m.backendHealth.RemoveEndpoint(ep.endpointKey())
			}
		}
		delete(m.registry, appName)
	}
	delete(m.containerIDs, appName)

	// Clean up transient state for removed app
	m.appTransientMu.Lock()
	delete(m.appTransient, appName)
	m.appTransientMu.Unlock()

	// Publish endpoint changes (non-blocking)
	if len(removed) > 0 {
		m.publishEndpointsChanged(appName, nil, removed)
	}
}
