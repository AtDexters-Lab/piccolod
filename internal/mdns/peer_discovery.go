package mdns

import (
	"log"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const (
	peerDiscoveryInterval = 60 * time.Second  // Time between PTR queries
	peerTimeout           = 180 * time.Second // 3 missed intervals
)

// peerDiscoveryLoop periodically sends PTR queries and cleans up stale peers.
func (m *Manager) peerDiscoveryLoop() {
	defer m.wg.Done()

	// Send initial discovery query after a short delay
	select {
	case <-m.stopCh:
		return
	case <-time.After(2 * time.Second):
		m.sendPeerDiscoveryQuery()
	}

	ticker := time.NewTicker(peerDiscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sendPeerDiscoveryQuery()
			m.cleanupStalePeers()
		}
	}
}

// sendPeerDiscoveryQuery sends PTR queries for _piccolo._tcp.local on all interfaces.
func (m *Manager) sendPeerDiscoveryQuery() {
	m.mutex.RLock()
	snapshots := make([]struct {
		name     string
		ipv4Conn *net.UDPConn
		ipv6Conn *net.UDPConn
		active   bool
	}, 0, len(m.interfaces))

	for name, state := range m.interfaces {
		if state == nil {
			continue
		}
		snapshots = append(snapshots, struct {
			name     string
			ipv4Conn *net.UDPConn
			ipv6Conn *net.UDPConn
			active   bool
		}{
			name:     name,
			ipv4Conn: state.IPv4Conn,
			ipv6Conn: state.IPv6Conn,
			active:   state.Active,
		})
	}
	m.mutex.RUnlock()

	serviceFQDN := m.ServiceFQDN()

	// Build PTR query message
	msg := &dns.Msg{}
	msg.SetQuestion(serviceFQDN, dns.TypePTR)
	msg.RecursionDesired = false

	data, err := msg.Pack()
	if err != nil {
		log.Printf("DISCOVERY: Failed to pack PTR query: %v", err)
		return
	}

	for _, snap := range snapshots {
		if !snap.active {
			continue
		}

		// Send on IPv4
		if snap.ipv4Conn != nil {
			multicastAddr := &net.UDPAddr{
				IP:   net.IPv4(224, 0, 0, 251),
				Port: 5353,
			}
			if _, err := snap.ipv4Conn.WriteToUDP(data, multicastAddr); err == nil {
				log.Printf("DEBUG: [%s-IPv4] Sent peer discovery query for %s", snap.name, serviceFQDN)
			}
		}

		// Send on IPv6
		if snap.ipv6Conn != nil {
			multicastAddr := &net.UDPAddr{
				IP:   net.ParseIP("ff02::fb"),
				Port: 5353,
			}
			if _, err := snap.ipv6Conn.WriteToUDP(data, multicastAddr); err == nil {
				log.Printf("DEBUG: [%s-IPv6] Sent peer discovery query for %s", snap.name, serviceFQDN)
			}
		}
	}
}

// handlePeerDiscoveryResponse processes mDNS responses for peer discovery.
// This looks for PTR, SRV, and TXT records advertising _piccolo._tcp services.
func (m *Manager) handlePeerDiscoveryResponse(msg *dns.Msg, clientAddr *net.UDPAddr) {
	if m.peerRegistry == nil {
		return
	}

	// Skip our own responses
	if m.isSelfResponse(clientAddr.IP) {
		return
	}

	serviceFQDN := m.ServiceFQDN()

	// Track instances found in this response
	// Map from instance FQDN to partial peer info
	instances := make(map[string]*peerInfo)

	// First pass: collect PTR records pointing to instances
	for _, rr := range msg.Answer {
		if ptr, ok := rr.(*dns.PTR); ok {
			if strings.EqualFold(ptr.Hdr.Name, serviceFQDN) {
				instanceFQDN := normalizeFQDN(ptr.Ptr)
				if instanceFQDN != "" {
					instances[instanceFQDN] = &peerInfo{
						instanceFQDN: instanceFQDN,
					}
				}
			}
		}
	}

	// If no PTR records, check if we got direct SRV/TXT records
	// (could be a response to our direct query)
	if len(instances) == 0 {
		for _, rr := range msg.Answer {
			switch r := rr.(type) {
			case *dns.SRV, *dns.TXT:
				name := normalizeFQDN(rr.Header().Name)
				if strings.HasSuffix(name, normalizeFQDN(serviceFQDN)) && name != normalizeFQDN(serviceFQDN) {
					if _, exists := instances[name]; !exists {
						instances[name] = &peerInfo{instanceFQDN: name}
					}
				}
			default:
				_ = r // unused
			}
		}
	}

	// Second pass: collect SRV, TXT, and address records
	allRecords := append(append([]dns.RR{}, msg.Answer...), msg.Extra...)

	for _, rr := range allRecords {
		name := normalizeFQDN(rr.Header().Name)

		switch r := rr.(type) {
		case *dns.SRV:
			if info, exists := instances[name]; exists {
				info.hostname = strings.TrimSuffix(r.Target, ".")
				info.port = int(r.Port)
			}
		case *dns.TXT:
			if info, exists := instances[name]; exists {
				info.metadata = parseTXTRecord(r.Txt)
			}
		case *dns.A:
			// Match against hostnames in our instances
			for _, info := range instances {
				if info.hostname != "" && strings.EqualFold(name, info.hostname+".") {
					info.ipv4 = r.A
				}
			}
		case *dns.AAAA:
			// Match against hostnames in our instances
			for _, info := range instances {
				if info.hostname != "" && strings.EqualFold(name, info.hostname+".") {
					info.ipv6 = r.AAAA
				}
			}
		}
	}

	// Also check clientAddr as fallback for IP
	clientIP := clientAddr.IP

	// Process discovered instances
	for _, info := range instances {
		m.processDiscoveredInstance(info, clientIP)
	}
}

// peerInfo holds temporary information about a discovered peer.
type peerInfo struct {
	instanceFQDN string
	hostname     string
	port         int
	metadata     *ServiceMetadata
	ipv4         net.IP
	ipv6         net.IP
}

// processDiscoveredInstance updates the peer registry with discovered peer info.
func (m *Manager) processDiscoveredInstance(info *peerInfo, fallbackIP net.IP) {
	// Skip if no metadata (need at least machine ID)
	if info.metadata == nil || info.metadata.MachineID == "" {
		log.Printf("DEBUG: Skipping peer without metadata or machine ID: %s", info.instanceFQDN)
		return
	}

	// Skip self
	if info.metadata.MachineID == m.machineID {
		return
	}

	machineID := info.metadata.MachineID

	isNew := m.peerRegistry.UpdatePeer(machineID, func(peer *DiscoveredPeer) {
		if info.hostname != "" {
			peer.Hostname = info.hostname
		}
		if info.ipv4 != nil {
			peer.IPv4 = info.ipv4
		} else if fallbackIP.To4() != nil && peer.IPv4 == nil {
			peer.IPv4 = fallbackIP
		}
		if info.ipv6 != nil {
			peer.IPv6 = info.ipv6
		} else if fallbackIP.To4() == nil && peer.IPv6 == nil {
			peer.IPv6 = fallbackIP
		}
		if info.metadata.Model != "" {
			peer.Model = info.metadata.Model
		}
		if info.metadata.PreferredName != "" {
			peer.PreferredName = info.metadata.PreferredName
		}
		if info.metadata.Version != "" {
			peer.Version = info.metadata.Version
		}
		if !info.metadata.BootTime.IsZero() {
			peer.BootTime = info.metadata.BootTime
		}
	})

	if isNew {
		peer, _ := m.peerRegistry.GetPeer(machineID)
		log.Printf("DISCOVERY: New peer discovered - ID: %s, Hostname: %s, Model: %s, Version: %s",
			peer.MachineID, peer.Hostname, peer.Model, peer.Version)
	}
}

// cleanupStalePeers removes peers that haven't been seen recently.
func (m *Manager) cleanupStalePeers() {
	if m.peerRegistry == nil {
		return
	}

	cutoff := time.Now().Add(-peerTimeout)
	removed := m.peerRegistry.RemoveStale(cutoff)

	if removed > 0 {
		log.Printf("DISCOVERY: Removed %d stale peer(s)", removed)
	}
}
