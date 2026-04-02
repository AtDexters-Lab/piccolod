package ap

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// Manager controls the AP mode lifecycle: hotspot activation/deactivation,
// dnsmasq DNS redirect, firewalld zone rules, and captive portal server.
type Manager struct {
	nm     nmclient.Client
	runner runner.CommandRunner
	fw     *firewallManager

	mu      sync.Mutex
	active  bool
	ssid    string
	ssidVal atomic.Value // non-blocking SSID for status queries (set in Start, cleared in Stop)
	apIP    string       // dynamically queried from NM after hotspot activation

	// Cleanup functions for atomic rollback on partial activation failure.
	cleanups []func()

	// Captive portal server lifecycle (owned by captive package, wired externally)
	onStartPortal func(ctx context.Context, listenAddr string) error
	onStopPortal  func()

	// Last HTTP activity timestamp for periodic reconnection suppression
	lastHTTPActivity time.Time
	lastHTTPMu       sync.Mutex
}

// NewManager creates an AP mode manager.
func NewManager(nm nmclient.Client, r runner.CommandRunner) *Manager {
	return &Manager{
		nm:     nm,
		runner: r,
		fw:     newFirewallManager(r),
	}
}

// SetPortalCallbacks wires the captive portal start/stop functions.
func (m *Manager) SetPortalCallbacks(start func(ctx context.Context, listenAddr string) error, stop func()) {
	m.onStartPortal = start
	m.onStopPortal = stop
}

// Start activates AP mode. Steps are executed in order; if any step fails,
// previously completed steps are rolled back.
func (m *Manager) Start(ctx context.Context, device dbus.ObjectPath, macAddr net.HardwareAddr) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active {
		return nil // already active
	}

	m.cleanups = nil
	m.ssid = generateSSID(macAddr)
	m.ssidVal.Store(m.ssid)

	// Step 1: Write DNS redirect config BEFORE activating the hotspot.
	// NM's shared mode starts its own dnsmasq — we configure it to redirect
	// all DNS queries to the AP IP for captive portal detection.
	if err := writeDNSRedirectConfig("10.42.0.1"); err != nil {
		log.Printf("WARN: ap: DNS redirect config failed, captive portal detection may not work: %v", err)
	}
	m.cleanups = append(m.cleanups, func() {
		removeDNSRedirectConfig()
	})

	// Step 2: Ensure firewalld zone exists
	if err := m.fw.ensureZone(ctx); err != nil {
		return m.rollback(fmt.Errorf("ensure AP zone: %w", err))
	}

	// Step 3: Activate NM hotspot with zone assignment
	if err := m.nm.ActivateHotspot(device, m.ssid, nmclient.HotspotOpts{
		Zone: apZone,
	}); err != nil {
		return m.rollback(fmt.Errorf("activate hotspot: %w", err))
	}
	m.cleanups = append(m.cleanups, func() {
		_ = m.nm.DeactivateHotspot(device)
	})

	// Brief pause for NM to bring up the interface and assign IP
	time.Sleep(2 * time.Second)

	// Step 4: Verify zone assignment (belt-and-suspenders)
	iface := deviceInterface(m.nm, device)
	if iface != "" && !m.fw.verifyZoneAssignment(ctx, iface) {
		log.Printf("WARN: ap: interface %s not in zone %s, forcing assignment", iface, apZone)
		if err := m.fw.assignInterface(ctx, iface); err != nil {
			return m.rollback(fmt.Errorf("assign interface to AP zone: %w", err))
		}
	}

	// Step 5: Query actual AP IP from the interface directly.
	// NM's ActiveConnectionInfo can return stale data from the previous
	// WiFi connection — reading the interface address is reliable.
	apIP := ""
	if iface != "" {
		for i := 0; i < 15; i++ {
			select {
			case <-ctx.Done():
				return m.rollback(ctx.Err())
			default:
			}
			apIP = getInterfaceIPv4(iface)
			if apIP != "" && len(apIP) >= 5 && apIP[:5] == "10.42" {
				break
			}
			apIP = ""
			time.Sleep(time.Second)
		}
	}
	if apIP == "" {
		apIP = "10.42.0.1" // fallback
		log.Printf("WARN: ap: could not detect AP IP on %s, using fallback: %s", iface, apIP)
	}
	m.apIP = apIP
	log.Printf("INFO: ap: AP interface IP: %s", apIP)

	// Step 6: Apply firewalld rules
	if err := m.fw.applyRules(ctx); err != nil {
		return m.rollback(fmt.Errorf("apply firewall rules: %w", err))
	}
	m.cleanups = append(m.cleanups, func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.fw.removeRules(cleanupCtx)
	})

	// Step 7: Start captive portal on <AP_IP>:80.
	// On Linux, binding to a specific IP:80 takes precedence over the
	// GinServer's 0.0.0.0:80 binding. No NAT redirect needed.
	if m.onStartPortal != nil {
		listenAddr := fmt.Sprintf("%s:80", apIP)
		if err := m.onStartPortal(ctx, listenAddr); err != nil {
			return m.rollback(fmt.Errorf("start captive portal: %w", err))
		}
		m.cleanups = append(m.cleanups, func() {
			if m.onStopPortal != nil {
				m.onStopPortal()
			}
		})
	}

	m.active = true
	log.Printf("INFO: ap: AP mode activated (SSID=%s, IP=%s)", m.ssid, m.apIP)
	return nil
}

// Stop tears down AP mode: captive portal, dnsmasq, firewall, hotspot.
func (m *Manager) Stop(ctx context.Context, device dbus.ObjectPath) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.active {
		return nil
	}

	// Run cleanup in reverse order
	for i := len(m.cleanups) - 1; i >= 0; i-- {
		m.cleanups[i]()
	}
	m.cleanups = nil

	m.active = false
	m.ssidVal.Store("")
	log.Printf("INFO: ap: AP mode deactivated")
	return nil
}

// Active returns true if AP mode is currently running.
func (m *Manager) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// SSID returns the current AP SSID (empty if not active).
// Non-blocking: reads from an atomic value, safe to call from API handlers
// while Start/Stop hold mu.
func (m *Manager) SSID() string {
	if v, ok := m.ssidVal.Load().(string); ok {
		return v
	}
	return ""
}

// APIP returns the AP interface IP (empty if not active).
func (m *Manager) APIP() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apIP
}

// HasActiveClients returns true if any client is connected to the AP or
// has been active via the captive portal recently.
func (m *Manager) HasActiveClients() bool {
	m.lastHTTPMu.Lock()
	lastActivity := m.lastHTTPActivity
	m.lastHTTPMu.Unlock()

	return time.Since(lastActivity) < 2*time.Minute
}

// RecordHTTPActivity updates the last HTTP activity timestamp.
func (m *Manager) RecordHTTPActivity() {
	m.lastHTTPMu.Lock()
	m.lastHTTPActivity = time.Now()
	m.lastHTTPMu.Unlock()
}

// rollback executes cleanup functions in reverse and returns the original error.
func (m *Manager) rollback(err error) error {
	log.Printf("WARN: ap: rolling back AP activation: %v", err)
	for i := len(m.cleanups) - 1; i >= 0; i-- {
		m.cleanups[i]()
	}
	m.cleanups = nil
	m.ssidVal.Store("")
	return err
}

// generateSSID creates the AP SSID from the WiFi MAC address.
// Format: "Piccolo-Setup-XXXX" where XXXX is the last 4 hex digits of the MAC.
func generateSSID(mac net.HardwareAddr) string {
	if len(mac) < 6 {
		return "Piccolo-Setup"
	}
	return fmt.Sprintf("Piccolo-Setup-%02X%02X", mac[4], mac[5])
}

// getInterfaceIPv4 returns the first IPv4 address on a network interface.
func getInterfaceIPv4(ifaceName string) string {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
			return ipnet.IP.String()
		}
	}
	return ""
}

// deviceInterface extracts the interface name for a device.
func deviceInterface(nm nmclient.Client, device dbus.ObjectPath) string {
	devs, err := nm.WiFiDevices()
	if err != nil {
		return ""
	}
	for _, d := range devs {
		if d.Path == device {
			return d.Interface
		}
	}
	return ""
}
