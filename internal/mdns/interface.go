package mdns

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	listNetworkInterfaces = net.Interfaces
	interfaceAddrs        = func(iface *net.Interface) ([]net.Addr, error) {
		return iface.Addrs()
	}
	// interfaceFuncsMu protects listNetworkInterfaces and interfaceAddrs from concurrent access.
	// This is primarily needed for tests that replace these functions.
	interfaceFuncsMu sync.RWMutex
)

// discoverInterfaces finds and sets up all suitable network interfaces
func (m *Manager) discoverInterfaces() error {
	interfaceFuncsMu.RLock()
	listFn := listNetworkInterfaces
	interfaceFuncsMu.RUnlock()

	interfaces, err := listFn()
	if err != nil {
		return err
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	activeCount := 0
	for _, iface := range interfaces {
		ifaceCopy := iface
		if err := m.setupInterface(&ifaceCopy); err != nil {
			// Use DEBUG for intentionally skipped interfaces, WARN for unexpected failures
			if isVirtualInterface(iface.Name) || iface.Flags&net.FlagLoopback != 0 ||
				iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
				log.Printf("DEBUG: Skipping interface %s: %v", iface.Name, err)
			} else {
				log.Printf("WARN: Failed to setup interface %s: %v", iface.Name, err)
				// Track interface setup failure for resilience only for unexpected failures
				if state, exists := m.interfaces[iface.Name]; exists {
					m.markInterfaceFailure(state, err)
				}
			}
			continue
		}
		activeCount++
	}

	if activeCount == 0 {
		log.Printf("WARN: No active network interfaces found during initial discovery")
	} else {
		log.Printf("INFO: Successfully configured %d network interfaces for mDNS", activeCount)
	}

	return nil
}

// virtualInterfacePrefixes lists name prefixes for virtual/container interfaces
// that should be skipped for mDNS. These interfaces cannot send multicast
// traffic to the local network and would cause repeated failures.
var virtualInterfacePrefixes = []string{
	// Container runtimes
	"podman", "docker", "cni",
	// Virtual ethernet pairs
	"veth", "vnet",
	// Virtual bridges
	"br-", "virbr",
	// Tunnel interfaces
	"tap", "tun",
	// Dummy/test interfaces
	"dummy",
	// macOS/BSD specific
	"utun", "awdl", "llw", "gif", "stf",
	// Kubernetes CNI plugins
	"flannel", "cali", "weave",
	// LXC/LXD containers
	"lxc", "lxd",
	// Hypervisors
	"vbox", "vmnet", "hyperv",
}

// isVirtualInterface checks if an interface name matches known virtual interface patterns.
func isVirtualInterface(name string) bool {
	nameLower := strings.ToLower(name)
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(nameLower, prefix) {
			return true
		}
	}
	return false
}

// setupInterface configures dual-stack mDNS for a specific network interface
func (m *Manager) setupInterface(iface *net.Interface) error {
	// Skip loopback and down interfaces
	if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("interface %s not suitable (loopback or down)", iface.Name)
	}

	// Skip interfaces without multicast capability
	if iface.Flags&net.FlagMulticast == 0 {
		return fmt.Errorf("interface %s has no multicast capability", iface.Name)
	}

	// Skip virtual/container interfaces that can't reach the local network
	if isVirtualInterface(iface.Name) {
		return fmt.Errorf("interface %s is a virtual interface", iface.Name)
	}

	// Get all addresses for this interface
	interfaceFuncsMu.RLock()
	addrsFn := interfaceAddrs
	interfaceFuncsMu.RUnlock()

	addrs, err := addrsFn(iface)
	if err != nil {
		return err
	}

	var ipv4Addr, ipv6Addr net.IP

	// Find IPv4 and IPv6 addresses
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ipv4 := ipnet.IP.To4(); ipv4 != nil {
				// IPv4 address - skip link-local
				if !ipnet.IP.IsLinkLocalUnicast() {
					ipv4Addr = ipv4
				}
			} else if ipv6 := ipnet.IP.To16(); ipv6 != nil {
				// IPv6 address - accept link-local (required by RFC 6762), skip only loopback
				// RFC 6762 Section 15: "Multicast DNS operates over link-local scope"
				if !ipnet.IP.IsLoopback() {
					ipv6Addr = ipv6
				}
			}
		}
	}

	// Need at least one IP stack
	if ipv4Addr == nil && ipv6Addr == nil {
		return fmt.Errorf("no suitable IP addresses on interface %s", iface.Name)
	}

	// Create interface state
	state := &InterfaceState{
		Interface:   iface,
		IPv4:        ipv4Addr,
		IPv6:        ipv6Addr,
		HasIPv4:     ipv4Addr != nil,
		HasIPv6:     ipv6Addr != nil,
		Active:      true,
		LastSeen:    time.Now(),
		HealthScore: 1.0, // Start with perfect health
	}

	// Setup IPv4 socket if available
	if state.HasIPv4 {
		factory := m.ipv4SocketFactory
		if factory == nil {
			factory = m.createIPv4Socket
		}
		ipv4Conn, err := factory(iface)
		if err != nil {
			log.Printf("WARN: Failed to create IPv4 socket for %s: %v", iface.Name, err)
		} else {
			state.IPv4Conn = ipv4Conn
		}
	}

	// Setup IPv6 socket if available
	if state.HasIPv6 {
		factory := m.ipv6SocketFactory
		if factory == nil {
			factory = m.createIPv6Socket
		}
		ipv6Conn, err := factory(iface)
		if err != nil {
			log.Printf("WARN: Failed to create IPv6 socket for %s: %v", iface.Name, err)
		} else {
			state.IPv6Conn = ipv6Conn
		}
	}

	// Need at least one working socket
	if state.IPv4Conn == nil && state.IPv6Conn == nil {
		return fmt.Errorf("failed to create any sockets for interface %s", iface.Name)
	}

	// Store in manager
	m.interfaces[iface.Name] = state

	// Start responder for this interface
	m.wg.Add(1)
	go m.interfaceResponder(state)

	var addrInfo []string
	if state.HasIPv4 {
		addrInfo = append(addrInfo, fmt.Sprintf("IPv4:%s", ipv4Addr.String()))
	}
	if state.HasIPv6 {
		addrInfo = append(addrInfo, fmt.Sprintf("IPv6:%s", ipv6Addr.String()))
	}

	log.Printf("INFO: Interface %s ready - %s", iface.Name, strings.Join(addrInfo, ", "))
	return nil
}

// createIPv4Socket creates an IPv4 UDP socket bound to a specific interface
func (m *Manager) createIPv4Socket(iface *net.Interface) (*net.UDPConn, error) {
	// Create raw socket with SO_REUSEPORT
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPv4 socket: %w", err)
	}

	// Set socket options
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("failed to set SO_REUSEADDR: %w", err)
	}

	// SO_REUSEADDR is sufficient for single-daemon mDNS (no need for SO_REUSEPORT)
	// SO_REUSEADDR already set above - no additional port sharing needed

	// Bind to specific interface using SO_BINDTODEVICE
	if err := syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface.Name); err != nil {
		log.Printf("WARN: Failed to bind IPv4 to device %s: %v", iface.Name, err)
	}

	// Bind to mDNS port
	addr := &syscall.SockaddrInet4{Port: 5353}
	copy(addr.Addr[:], net.IPv4zero.To4())

	if err := syscall.Bind(fd, addr); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("failed to bind IPv4 to :5353: %w", err)
	}

	// Convert to net.UDPConn
	file := os.NewFile(uintptr(fd), fmt.Sprintf("mdns4-%s", iface.Name))
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("failed to create file from IPv4 socket")
	}
	defer file.Close()

	fileConn, err := net.FileConn(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPv4 connection: %w", err)
	}

	conn, ok := fileConn.(*net.UDPConn)
	if !ok {
		fileConn.Close()
		return nil, fmt.Errorf("failed to convert to IPv4 UDPConn")
	}

	// Join IPv4 multicast group on this interface
	pc := ipv4.NewPacketConn(conn)
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)}
	if err := pc.JoinGroup(iface, group); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to join IPv4 multicast group on %s: %w", iface.Name, err)
	}

	// Set multicast interface
	if err := pc.SetMulticastInterface(iface); err != nil {
		log.Printf("WARN: Failed to set IPv4 multicast interface %s: %v", iface.Name, err)
	}

	return conn, nil
}

// createIPv6Socket creates an IPv6 UDP socket bound to a specific interface
func (m *Manager) createIPv6Socket(iface *net.Interface) (*net.UDPConn, error) {
	// Create raw socket with SO_REUSEPORT
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPv6 socket: %w", err)
	}

	// Set socket options
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("failed to set SO_REUSEADDR on IPv6: %w", err)
	}

	// SO_REUSEADDR is sufficient for single-daemon mDNS (no need for SO_REUSEPORT)
	// SO_REUSEADDR already set above - no additional port sharing needed

	// Disable IPv6 only to allow dual-stack
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
		log.Printf("WARN: Failed to disable IPv6-only mode on %s: %v", iface.Name, err)
	}

	// Bind to specific interface using SO_BINDTODEVICE
	if err := syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface.Name); err != nil {
		log.Printf("WARN: Failed to bind IPv6 to device %s: %v", iface.Name, err)
	}

	// Bind to mDNS port on IPv6
	addr := &syscall.SockaddrInet6{Port: 5353}
	copy(addr.Addr[:], net.IPv6zero.To16())

	if err := syscall.Bind(fd, addr); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("failed to bind IPv6 to :5353: %w", err)
	}

	// Convert to net.UDPConn
	file := os.NewFile(uintptr(fd), fmt.Sprintf("mdns6-%s", iface.Name))
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("failed to create file from IPv6 socket")
	}
	defer file.Close()

	fileConn, err := net.FileConn(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPv6 connection: %w", err)
	}

	conn, ok := fileConn.(*net.UDPConn)
	if !ok {
		fileConn.Close()
		return nil, fmt.Errorf("failed to convert to IPv6 UDPConn")
	}

	// Join IPv6 multicast group on this interface
	pc := ipv6.NewPacketConn(conn)
	group := &net.UDPAddr{IP: net.ParseIP("ff02::fb")}
	if err := pc.JoinGroup(iface, group); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to join IPv6 multicast group on %s: %w", iface.Name, err)
	}

	// Set multicast interface
	if err := pc.SetMulticastInterface(iface); err != nil {
		log.Printf("WARN: Failed to set IPv6 multicast interface %s: %v", iface.Name, err)
	}

	return conn, nil
}

// failedSetup backoff constants for retrying failed interface setups.
// Uses exponential backoff: 30s, 60s, 120s, 240s, capped at 5min.
const (
	failedSetupInitialCooldown = 30 * time.Second
	failedSetupMaxCooldown     = 5 * time.Minute
)

// failedSetupBackoff computes the cooldown duration for a given attempt count.
func failedSetupBackoff(attempts int) time.Duration {
	if attempts >= 4 {
		return failedSetupMaxCooldown // overflow guard
	}
	d := failedSetupInitialCooldown << uint(attempts) // 30s, 60s, 120s, 240s
	if d > failedSetupMaxCooldown {
		d = failedSetupMaxCooldown
	}
	return d
}

// networkMonitor continuously monitors network interface changes
func (m *Manager) networkMonitor() {
	defer m.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkInterfaceChanges()
		}
	}
}

// checkInterfaceChanges detects and handles interface changes
func (m *Manager) checkInterfaceChanges() {
	interfaceFuncsMu.RLock()
	listFn := listNetworkInterfaces
	interfaceFuncsMu.RUnlock()

	interfaces, err := listFn()
	if err != nil {
		log.Printf("WARN: Failed to check interfaces: %v", err)
		return
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	seenInterfaces := make(map[string]bool)
	needsAnnounce := false

	// Check each interface
	for _, iface := range interfaces {
		ifaceCopy := iface
		seenInterfaces[ifaceCopy.Name] = true
		if existing, exists := m.interfaces[ifaceCopy.Name]; exists {
			// Interface still exists. Two cases:
			//   1. Interface flag is Down (link-down or admin-down): legitimate
			//      change — reconfigure on next FlagUp tick.
			//   2. Interface flag is Up + IP changed: dampen for 3-of-3 ticks
			//      before reconfiguring. Eliminates the 10s reconfig storm
			//      seen during transient DHCP renewal / brief carrier drops.
			ifaceUp := ifaceCopy.Flags&net.FlagUp != 0
			ipChanged := m.hasIPChanged(&ifaceCopy, existing)
			ipLost := ipChanged && m.isIPLost(&ifaceCopy)

			switch {
			case !ifaceUp:
				// Interface down — kernel says the link is gone.
				log.Printf("INFO: Interface %s down, closing connections", ifaceCopy.Name)
				if existing.IPv4Conn != nil {
					existing.IPv4Conn.Close()
				}
				if existing.IPv6Conn != nil {
					existing.IPv6Conn.Close()
				}
				existing.IPLossTicks = 0
				m.setupInterface(&ifaceCopy)
			case ipLost:
				// IP lost while interface still Up — likely transient (DHCP
				// renewal, brief carrier drop). Require 3-of-3 ticks before
				// reconfiguring.
				existing.IPLossTicks++
				if existing.IPLossTicks >= 3 {
					log.Printf("INFO: Sustained IP loss on %s (%d ticks), reconfiguring", ifaceCopy.Name, existing.IPLossTicks)
					if existing.IPv4Conn != nil {
						existing.IPv4Conn.Close()
					}
					if existing.IPv6Conn != nil {
						existing.IPv6Conn.Close()
					}
					existing.IPLossTicks = 0
					m.setupInterface(&ifaceCopy)
				} else {
					existing.Active = true
					existing.LastSeen = time.Now()
				}
			case ipChanged:
				// IP changed to a new value (network move) — legitimate change.
				log.Printf("INFO: IP changed on interface %s, reconfiguring", ifaceCopy.Name)
				if existing.IPv4Conn != nil {
					existing.IPv4Conn.Close()
				}
				if existing.IPv6Conn != nil {
					existing.IPv6Conn.Close()
				}
				existing.IPLossTicks = 0
				m.setupInterface(&ifaceCopy)
			default:
				existing.IPLossTicks = 0
				existing.Active = true
				existing.LastSeen = time.Now()
			}
		} else {
			// New interface detected - only log and setup if it's suitable
			// Skip loopback, down, non-multicast, and virtual interfaces to avoid noise
			if ifaceCopy.Flags&net.FlagLoopback != 0 || ifaceCopy.Flags&net.FlagUp == 0 {
				continue
			}
			if ifaceCopy.Flags&net.FlagMulticast == 0 || isVirtualInterface(ifaceCopy.Name) {
				continue
			}

			// Check if recently failed — avoid log spam from repeated failures
			if info, failed := m.failedSetups[ifaceCopy.Name]; failed {
				cooldown := failedSetupBackoff(info.Attempts)
				if time.Since(info.LastAttempt) < cooldown {
					continue
				}
				log.Printf("DEBUG: Retrying previously failed interface %s (cooldown %v expired, attempt %d)", ifaceCopy.Name, cooldown, info.Attempts+1)
			}

			log.Printf("INFO: New interface detected: %s", ifaceCopy.Name)
			if err := m.setupInterface(&ifaceCopy); err != nil {
				log.Printf("WARN: Failed to setup new interface %s: %v", ifaceCopy.Name, err)
				if info, exists := m.failedSetups[ifaceCopy.Name]; exists {
					info.LastAttempt = time.Now()
					info.Attempts++
				} else {
					m.failedSetups[ifaceCopy.Name] = &failedSetupInfo{
						LastAttempt: time.Now(),
						Attempts:    0,
					}
				}
			} else {
				delete(m.failedSetups, ifaceCopy.Name)
				needsAnnounce = true
			}
		}
	}

	// Remove interfaces that no longer exist
	for name, state := range m.interfaces {
		if !seenInterfaces[name] {
			log.Printf("INFO: Interface %s no longer available, removing", name)
			if state.IPv4Conn != nil {
				state.IPv4Conn.Close()
			}
			if state.IPv6Conn != nil {
				state.IPv6Conn.Close()
			}
			delete(m.interfaces, name)
		}
	}

	// Clean up failedSetups and recoveryCooldowns for interfaces that disappeared
	for name := range m.failedSetups {
		if !seenInterfaces[name] {
			delete(m.failedSetups, name)
		}
	}
	for name := range m.recoveryCooldowns {
		if !seenInterfaces[name] {
			delete(m.recoveryCooldowns, name)
		}
	}

	// Announce on new interface join so device is immediately visible
	if needsAnnounce {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.sendServiceAnnouncement()
			m.sendMultiInterfaceAnnouncements()
			m.sendPeerDiscoveryQuery()
		}()
	}
}

// isIPLost returns true if the interface currently has no usable IPv4 or
// IPv6 address (the previous state had one). Used by checkInterfaceChanges
// to dampen transient IP losses (DHCP renewal, brief carrier drops) for
// 3-of-3 ticks before reconfiguring — closes the 10s reconfig storm.
func (m *Manager) isIPLost(iface *net.Interface) bool {
	interfaceFuncsMu.RLock()
	addrsFn := interfaceAddrs
	interfaceFuncsMu.RUnlock()

	addrs, err := addrsFn(iface)
	if err != nil {
		// Treat as loss if we can't enumerate.
		return true
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ipv4 := ipnet.IP.To4(); ipv4 != nil && !ipnet.IP.IsLinkLocalUnicast() {
			return false
		}
		if ipnet.IP.To4() == nil && ipnet.IP.To16() != nil && !ipnet.IP.IsLoopback() {
			return false
		}
	}
	return true
}

// hasIPChanged checks if an interface's IPv4 or IPv6 addresses have changed
func (m *Manager) hasIPChanged(iface *net.Interface, state *InterfaceState) bool {
	interfaceFuncsMu.RLock()
	addrsFn := interfaceAddrs
	interfaceFuncsMu.RUnlock()

	addrs, err := addrsFn(iface)
	if err != nil {
		return true // Assume changed if we can't check
	}

	var newIPv4, newIPv6 net.IP

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ipv4 := ipnet.IP.To4(); ipv4 != nil {
				if !ipnet.IP.IsLinkLocalUnicast() {
					newIPv4 = ipv4
				}
			} else if ipv6 := ipnet.IP.To16(); ipv6 != nil {
				if !ipnet.IP.IsLoopback() {
					newIPv6 = ipv6
				}
			}
		}
	}

	ipv4Changed := !state.IPv4.Equal(newIPv4)
	ipv6Changed := !state.IPv6.Equal(newIPv6)

	return ipv4Changed || ipv6Changed
}
