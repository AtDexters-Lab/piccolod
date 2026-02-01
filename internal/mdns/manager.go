package mdns

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"piccolod/internal/events"
)

const defaultBaseName = "piccolo"

// NewManager creates a new mDNS manager
func NewManager() *Manager {
	machineID := getMachineID()
	// Use specific name (piccolo-<machineId>) as base to avoid piccolo.local conflicts.
	// Gateway leader will add piccolo.local separately via AddGatewayHostname().
	specificName := "piccolo-" + machineID
	baseName := sanitizeBaseName(specificName)

	// Initialize security configuration with safe defaults
	securityConfig := &SecurityConfig{
		MaxPacketSize:        1500, // Standard MTU limit
		MaxResponseSize:      512,  // DNS standard response limit
		MaxConcurrentQueries: 50,   // Max concurrent query processing
		QueryTimeout:         time.Second * 2,
	}

	// Initialize resilience configuration with recovery defaults
	resilienceConfig := &ResilienceConfig{
		MaxRetries:            3,
		InitialBackoff:        time.Second * 5,
		MaxBackoff:            time.Minute * 5,
		BackoffMultiplier:     2.0,
		HealthCheckInterval:   time.Second * 30,
		RecoveryCheckInterval: time.Second * 15,
		MaxFailureRate:        0.3, // 30% failure rate threshold
		MinHealthScore:        0.7, // Minimum health score to be considered healthy
		RecoveryTimeout:       time.Minute * 2,
	}

	// Capture device info at startup
	deviceModel := GetDeviceModel()
	bootTime := GetBootTime()

	manager := &Manager{
		interfaces: make(map[string]*InterfaceState),
		hostname:   baseName,
		port:       80,
		stopCh:     make(chan struct{}),
		baseName:   baseName,
		machineID:  machineID,
		finalName:  baseName, // Will be updated if conflicts detected
		names:      newNameRegistry(baseName, specificName),

		// Security components
		securityConfig:  securityConfig,
		securityMetrics: &SecurityMetrics{},
		queryProcessor: &QueryProcessor{
			semaphore: make(chan struct{}, securityConfig.MaxConcurrentQueries),
		},

		// Resilience components
		resilienceConfig: resilienceConfig,
		healthMonitor: &HealthMonitor{
			OverallHealth:   1.0,
			InterfaceHealth: make(map[string]float64),
			LastHealthCheck: time.Now(),
		},

		// Conflict detection
		conflictDetector: &ConflictDetector{
			ConflictingSources: make(map[string]ConflictingHost),
			LastConflictCheck:  time.Now(),
		},

		// Peer discovery
		peerRegistry: newPeerRegistry(),
		deviceModel:  deviceModel,
		bootTime:     bootTime,

		// Service endpoint observation
		appHostLabels: make(map[string][]string),

		// Gateway leadership
		specificHostname: specificName + ".local",
		gatewayLeader:    NewGatewayLeader(machineID),
	}

	// Initialize service metadata
	manager.rebuildServiceMetadata()

	// Wire gateway leader to peer registry
	manager.gatewayLeader.SetPeersFunc(func() []DiscoveredPeer {
		if manager.peerRegistry == nil {
			return nil
		}
		return manager.peerRegistry.List()
	})

	// Wire leadership change callback to update advertised hostnames
	manager.gatewayLeader.SetLeadershipChangeCallback(func(isLeader bool) {
		if isLeader {
			manager.names.AddGatewayHostname()
		} else {
			manager.names.RemoveGatewayHostname()
		}
		// Re-announce with updated hostnames
		if manager.started.Load() {
			manager.sendMultiInterfaceAnnouncements()
		}
	})

	manager.ipv4SocketFactory = manager.createIPv4Socket
	manager.ipv6SocketFactory = manager.createIPv6Socket

	return manager
}

// Start begins advertising the service via mDNS
func (m *Manager) Start() error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	if m.started.Load() {
		return fmt.Errorf("mdns manager already started")
	}
	if m.stopped.Load() {
		return fmt.Errorf("mdns manager already stopped")
	}

	log.Printf("INFO: Starting multi-interface mDNS manager (machine ID: %s)", m.machineID)

	// Discover and setup all network interfaces
	if err := m.discoverInterfaces(); err != nil {
		return fmt.Errorf("failed to discover network interfaces: %w", err)
	}

	if m.stopped.Load() {
		return fmt.Errorf("mdns manager stopped during startup")
	}

	m.started.Store(true)

	// Start gateway leader election
	if m.gatewayLeader != nil {
		m.gatewayLeader.Start()
	}

	// Start network monitor for interface changes
	m.wg.Add(1)
	go m.networkMonitor()

	// Start announcement routine
	m.wg.Add(1)
	go m.announcer()

	// Start health monitoring routine
	m.wg.Add(1)
	go m.healthMonitorLoop()

	// Start DNS-SD service announcer
	m.wg.Add(1)
	go m.serviceAnnouncer()

	// Start peer discovery loop
	m.wg.Add(1)
	go m.peerDiscoveryLoop()

	// Launch probing and conflict monitoring in background to avoid blocking startup
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		// Perform initial conflict detection
		if err := m.probeNameAvailability(); err != nil {
			log.Printf("ERROR: conflict detection failed: %v", err)
			return
		}

		// Start conflict monitoring routine
		m.wg.Add(1)
		go m.conflictMonitor()

		m.mutex.RLock()
		interfaceCount := len(m.interfaces)
		m.mutex.RUnlock()

		serviceName := m.currentServiceName()

		log.Printf("INFO: Secured dual-stack mDNS server started - advertising %s.local on %d interfaces",
			serviceName, interfaceCount)
		log.Printf("INFO: Security limits - %d concurrent queries, %d max packet size",
			m.securityConfig.MaxConcurrentQueries, m.securityConfig.MaxPacketSize)

		// Log service metadata
		m.logServiceMetadata()
	}()

	return nil
}

// Stop shuts down the mDNS server
func (m *Manager) Stop() error {
	m.stopOnce.Do(func() {
		// Stop gateway leader first
		if m.gatewayLeader != nil {
			m.gatewayLeader.Stop()
		}

		// Stop service endpoint observer
		m.StopServiceEndpointsObserver()

		// Mark as stopped BEFORE sending goodbyes so that any interface failures
		// during goodbye sending don't trigger resilience tracking (backoff, retries).
		// This is important because container interfaces (veth) may already be gone.
		m.stopped.Store(true)
		close(m.stopCh)

		if m.started.Load() {
			// Send goodbye for host records
			m.sendMultiInterfaceAnnouncementsWithTTL(0)
			// Send goodbye for service records
			m.sendServiceAnnouncementWithTTL(0)
		}
	})

	// Close all interface connections
	m.mutex.Lock()
	for name, state := range m.interfaces {
		state.Active = false
		if state.IPv4Conn != nil {
			state.IPv4Conn.Close()
			log.Printf("INFO: Closed IPv4 connection for interface %s", name)
		}
		if state.IPv6Conn != nil {
			state.IPv6Conn.Close()
			log.Printf("INFO: Closed IPv6 connection for interface %s", name)
		}
	}
	m.mutex.Unlock()

	// Wait for all goroutines to finish
	m.wg.Wait()
	m.started.Store(false)

	log.Printf("INFO: Multi-interface mDNS manager stopped")
	return nil
}

// getMachineID generates a deterministic machine identifier
func getMachineID() string {
	// Try multiple sources for machine ID
	sources := []func() string{
		getMachineIDFromFile,
		getMachineIDFromMAC,
		getMachineIDFromHostname,
	}

	for _, source := range sources {
		if id := source(); id != "" {
			// Generate a short, deterministic suffix from the full ID
			hash := sha256.Sum256([]byte(id))
			return fmt.Sprintf("%x", hash[:3]) // 6 character hex
		}
	}

	// Fallback to timestamp-based (not ideal but deterministic per boot)
	return fmt.Sprintf("%06d", time.Now().Unix()%1000000)
}

// getMachineIDFromFile tries to read system machine ID
func getMachineIDFromFile() string {
	paths := []string{
		"/etc/machine-id",
		"/var/lib/dbus/machine-id",
		"/etc/hostid",
	}

	for _, path := range paths {
		if data, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// getMachineIDFromMAC generates ID from MAC addresses
func getMachineIDFromMAC() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	var macs []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 && len(iface.HardwareAddr) > 0 {
			macs = append(macs, iface.HardwareAddr.String())
		}
	}

	if len(macs) > 0 {
		// Use first non-loopback MAC as base
		return strings.ReplaceAll(macs[0], ":", "")
	}
	return ""
}

// getMachineIDFromHostname uses hostname as fallback
func getMachineIDFromHostname() string {
	if hostname, err := os.Hostname(); err == nil {
		return hostname
	}
	return ""
}

// currentServiceName returns the currently advertised service name.
func (m *Manager) currentServiceName() string {
	if m.names == nil {
		m.mutex.RLock()
		defer m.mutex.RUnlock()
		return m.finalName
	}
	return m.names.BaseName()
}

// Hostname returns the full mDNS hostname (e.g., "piccolo.local" or "piccolo-abc123.local").
func (m *Manager) Hostname() string {
	if m.names == nil {
		m.mutex.RLock()
		defer m.mutex.RUnlock()
		return m.finalName + ".local"
	}
	return m.names.Hostname()
}

// SpecificHostname returns the device's unique hostname (always includes machine ID).
// This hostname is always advertised, regardless of gateway leadership status.
// Example: "piccolo-abc123.local"
func (m *Manager) SpecificHostname() string {
	return m.specificHostname
}

// IsGatewayLeader returns true if this device is currently serving piccolo.local.
func (m *Manager) IsGatewayLeader() bool {
	if m.gatewayLeader == nil {
		return false
	}
	return m.gatewayLeader.IsLeader()
}

// DeviceModel returns the device model string (e.g., "Raspberry Pi 4 Model B").
func (m *Manager) DeviceModel() string {
	return m.deviceModel
}

// Version returns the service version string.
func (m *Manager) Version() string {
	return m.version
}

// AdvertisedNames returns the list of currently advertised FQDNs (with trailing dot).
func (m *Manager) AdvertisedNames() []string {
	if m.names == nil {
		return []string{m.currentServiceName() + ".local."}
	}
	return m.names.Names()
}

// HostAliases returns the current alias labels (without the base domain).
func (m *Manager) HostAliases() []string {
	if m.names == nil {
		return nil
	}
	return m.names.AliasLabels()
}

func (m *Manager) matchesAdvertisedName(name string) bool {
	if m.names == nil {
		expected := normalizeFQDN(m.currentServiceName() + ".local.")
		return normalizeFQDN(name) == expected
	}
	return m.names.MatchName(name)
}

// SetHostAliases replaces the set of alias labels to advertise (e.g., "immich", "metrics-immich").
// Invalid labels are ignored; if any are present, an error is returned.
func (m *Manager) SetHostAliases(labels []string) error {
	if m.names == nil {
		return fmt.Errorf("name registry not initialized")
	}

	valid, invalid := normalizeLabels(labels)
	m.names.SetAliases(valid)

	if len(invalid) > 0 {
		log.Printf("WARN: Ignored invalid mDNS alias labels: %s", strings.Join(invalid, ", "))
		if m.started.Load() {
			m.sendMultiInterfaceAnnouncements()
		}
		return fmt.Errorf("invalid mDNS alias labels: %s", strings.Join(invalid, ", "))
	}

	if m.started.Load() {
		m.sendMultiInterfaceAnnouncements()
	}
	return nil
}

// GoodbyeAliases sends TTL=0 goodbye announcements for the specified alias labels.
// This is used to notify peers that these aliases are no longer valid.
func (m *Manager) GoodbyeAliases(labels []string) {
	if m.names == nil || !m.started.Load() || len(labels) == 0 {
		return
	}

	baseName := m.names.BaseName()
	fqdns := make([]string, 0, len(labels))
	for _, label := range labels {
		normalized, err := normalizeLabel(label)
		if err != nil {
			continue
		}
		fqdn := normalized + "." + baseName + "." + localTLD + "."
		fqdns = append(fqdns, fqdn)
	}

	if len(fqdns) > 0 {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.sendAnnouncementsForNames(fqdns, 0)
		}()
	}
}

// ObserveServiceEndpoints subscribes to service endpoint changes and updates mDNS aliases.
func (m *Manager) ObserveServiceEndpoints(bus *events.Bus) {
	if bus == nil {
		return
	}

	m.endpointsMu.Lock()
	if m.endpointsCancel != nil {
		m.endpointsCancel()
	}
	if m.endpointsUnsubscribe != nil {
		m.endpointsUnsubscribe()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.endpointsCancel = cancel

	ch, unsubscribe := bus.SubscribeWithCancel(events.TopicServiceEndpointsChanged, 64)
	m.endpointsUnsubscribe = unsubscribe
	m.endpointsMu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case evt, ok := <-ch:
				if !ok {
					return
				}
				payload, ok := evt.Payload.(events.ServiceEndpointsChanged)
				if !ok {
					log.Printf("WARN: mDNS received unexpected service endpoints payload: %#v", evt.Payload)
					continue
				}
				m.handleServiceEndpointsChanged(payload)
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			}
		}
	}()
}

// StopServiceEndpointsObserver cancels the service endpoint subscription.
func (m *Manager) StopServiceEndpointsObserver() {
	m.endpointsMu.Lock()
	if m.endpointsCancel != nil {
		m.endpointsCancel()
		m.endpointsCancel = nil
	}
	if m.endpointsUnsubscribe != nil {
		m.endpointsUnsubscribe()
		m.endpointsUnsubscribe = nil
	}
	m.endpointsMu.Unlock()

	// Cancel any pending debounced announcement
	m.announceDebounceMu.Lock()
	if m.announceDebounceTimer != nil {
		m.announceDebounceTimer.Stop()
		m.announceDebounceTimer = nil
		m.announcePending = false
	}
	m.announceDebounceMu.Unlock()
}

func (m *Manager) handleServiceEndpointsChanged(payload events.ServiceEndpointsChanged) {
	// Collect labels to goodbye (from removed endpoints)
	var labelsToGoodbye []string
	for _, ep := range payload.Removed {
		if ep.DerivedHostLabel != "" {
			labelsToGoodbye = append(labelsToGoodbye, ep.DerivedHostLabel)
		}
	}

	// Send goodbye for removed aliases first
	if len(labelsToGoodbye) > 0 {
		m.GoodbyeAliases(labelsToGoodbye)
	}

	// Update internal tracking by merging delta with existing state
	m.appHostLabelsMu.Lock()
	if payload.App == "" {
		m.appHostLabelsMu.Unlock()
		return
	}

	// Build set of labels to remove
	removeSet := make(map[string]struct{}, len(payload.Removed))
	for _, ep := range payload.Removed {
		if ep.DerivedHostLabel != "" {
			removeSet[ep.DerivedHostLabel] = struct{}{}
		}
	}

	// Start with existing labels, filtering out removed ones
	existing := m.appHostLabels[payload.App]
	merged := make([]string, 0, len(existing)+len(payload.Added))
	for _, label := range existing {
		if _, shouldRemove := removeSet[label]; !shouldRemove {
			merged = append(merged, label)
		}
	}

	// Add new labels
	for _, ep := range payload.Added {
		if ep.DerivedHostLabel != "" {
			merged = append(merged, ep.DerivedHostLabel)
		}
	}

	// Update or delete the app entry
	if len(merged) > 0 {
		m.appHostLabels[payload.App] = merged
	} else {
		delete(m.appHostLabels, payload.App)
	}

	m.appHostLabelsMu.Unlock()

	// Schedule debounced announcement
	m.scheduleDebouncedAnnouncement()
}

// announcementDebounceDelay is the delay before announcing mDNS changes.
// This allows batching multiple rapid changes into a single announcement.
const announcementDebounceDelay = 500 * time.Millisecond

// scheduleDebouncedAnnouncement schedules an mDNS announcement after a delay.
// If called multiple times within the delay, only one announcement is sent.
func (m *Manager) scheduleDebouncedAnnouncement() {
	m.announceDebounceMu.Lock()
	defer m.announceDebounceMu.Unlock()

	// If there's already a pending timer, reset it
	if m.announceDebounceTimer != nil {
		m.announceDebounceTimer.Stop()
	}

	m.announcePending = true
	m.announceDebounceTimer = time.AfterFunc(announcementDebounceDelay, func() {
		m.announceDebounceMu.Lock()
		m.announcePending = false
		m.announceDebounceTimer = nil
		m.announceDebounceMu.Unlock()

		m.flushDebouncedAnnouncement()
	})
}

// flushDebouncedAnnouncement rebuilds the alias set and announces.
func (m *Manager) flushDebouncedAnnouncement() {
	m.appHostLabelsMu.RLock()
	allLabels := make([]string, 0)
	for _, appLabels := range m.appHostLabels {
		allLabels = append(allLabels, appLabels...)
	}
	m.appHostLabelsMu.RUnlock()

	if err := m.SetHostAliases(allLabels); err != nil {
		log.Printf("WARN: mDNS failed to update aliases: %v", err)
	}
}

// SetPort sets the port number advertised in SRV records.
// This should be called before Start() or will trigger re-announcement.
func (m *Manager) SetPort(port int) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if port < 1 || port > 65535 {
		log.Printf("WARN: Invalid port %d, keeping current port %d", port, m.port)
		return
	}

	m.port = port
	log.Printf("INFO: mDNS service port set to %d", port)

	// Re-announce if already started
	if m.started.Load() {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.sendServiceAnnouncement()
		}()
	}
}

// Port returns the currently configured port.
func (m *Manager) Port() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.port
}

func sanitizeBaseName(name string) string {
	normalized, err := normalizeLabel(name)
	if err != nil {
		return defaultBaseName
	}
	return normalized
}

func (m *Manager) setFinalNameLocked(name string) {
	m.finalName = name
	if m.names != nil {
		m.names.SetBaseName(name)
	}
}
