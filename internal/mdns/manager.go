package mdns

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

const defaultBaseName = "piccolo"

// NewManager creates a new mDNS manager
func NewManager() *Manager {
	machineID := getMachineID()
	baseName := sanitizeBaseName(defaultBaseName)

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
		names:      newNameRegistry(baseName),

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
	}

	// Initialize service metadata
	manager.rebuildServiceMetadata()

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
		if m.started.Load() {
			// Send goodbye for host records
			m.sendMultiInterfaceAnnouncementsWithTTL(0)
			// Send goodbye for service records
			m.sendServiceAnnouncementWithTTL(0)
		}
		close(m.stopCh)
		m.stopped.Store(true)
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
