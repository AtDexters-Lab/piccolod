package mdns

import (
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// interfaceResponder handles dual-stack mDNS queries on a specific interface
func (m *Manager) interfaceResponder(state *InterfaceState) {
	defer m.wg.Done()

	interfaceName := state.Interface.Name
	var localWg sync.WaitGroup

	// Start IPv4 responder if available
	if state.IPv4Conn != nil {
		m.wg.Add(1)
		localWg.Add(1)
		go func() {
			defer localWg.Done()
			m.ipv4Responder(state, interfaceName)
		}()
	}

	// Start IPv6 responder if available
	if state.IPv6Conn != nil {
		m.wg.Add(1)
		localWg.Add(1)
		go func() {
			defer localWg.Done()
			m.ipv6Responder(state, interfaceName)
		}()
	}

	// Wait for child responders to finish
	localWg.Wait()
}

// ipv4Responder handles IPv4 mDNS queries
func (m *Manager) ipv4Responder(state *InterfaceState, interfaceName string) {
	defer m.wg.Done()

	buffer := make([]byte, 1500)

	for {
		select {
		case <-m.stopCh:
			return
		default:
			if state.IPv4Conn == nil {
				return
			}

			state.IPv4Conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			n, clientAddr, err := state.IPv4Conn.ReadFromUDP(buffer)
			if err != nil {
				// Check if we should stop before continuing
				select {
				case <-m.stopCh:
					return
				default:
				}

				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if isClosedConnError(err) {
					m.recoverClosedConnection(interfaceName, state)
					return
				}
				log.Printf("WARN: IPv4 mDNS read error on %s: %v", interfaceName, err)
				m.markInterfaceFailure(state, err)
				continue
			}

			// Handle query in separate goroutine to avoid blocking UDP reader
			m.wg.Add(1)
			go func(data []byte, addr *net.UDPAddr) {
				defer m.wg.Done()
				m.handleDualStackQuery(data, addr, state, "IPv4")
			}(append([]byte(nil), buffer[:n]...), clientAddr)
		}
	}
}

// ipv6Responder handles IPv6 mDNS queries
func (m *Manager) ipv6Responder(state *InterfaceState, interfaceName string) {
	defer m.wg.Done()

	buffer := make([]byte, 1500)

	for {
		select {
		case <-m.stopCh:
			return
		default:
			if state.IPv6Conn == nil {
				return
			}

			state.IPv6Conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			n, clientAddr, err := state.IPv6Conn.ReadFromUDP(buffer)
			if err != nil {
				// Check if we should stop before continuing
				select {
				case <-m.stopCh:
					return
				default:
				}

				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if isClosedConnError(err) {
					m.recoverClosedConnection(interfaceName, state)
					return
				}
				log.Printf("WARN: IPv6 mDNS read error on %s: %v", interfaceName, err)
				m.markInterfaceFailure(state, err)
				continue
			}

			// Handle query in separate goroutine to avoid blocking UDP reader
			m.wg.Add(1)
			go func(data []byte, addr *net.UDPAddr) {
				defer m.wg.Done()
				m.handleDualStackQuery(data, addr, state, "IPv6")
			}(append([]byte(nil), buffer[:n]...), clientAddr)
		}
	}
}

// handleDualStackQuery processes mDNS queries with dual-stack support and security
func (m *Manager) handleDualStackQuery(data []byte, clientAddr *net.UDPAddr, state *InterfaceState, stack string) {
	// Try to acquire a processing slot
	if !m.acquireQuerySlot() {
		// Too many concurrent queries, drop this one
		return
	}
	defer m.releaseQuerySlot()

	// Set query timeout
	startTime := time.Now()
	defer func() {
		if time.Since(startTime) > m.securityConfig.QueryTimeout {
			if !m.isSelfResponse(clientAddr.IP) {
				log.Printf("SECURITY: Query timeout from %s", clientAddr.IP)
			}
		}
	}()

	// Update interface metrics
	atomic.AddUint64(&state.QueryCount, 1)
	state.resilienceMu.Lock()
	state.LastQuery = time.Now()
	state.resilienceMu.Unlock()

	// Validate packet security (size checks)
	if err := m.validatePacket(data); err != nil {
		atomic.AddUint64(&state.ErrorCount, 1)
		log.Printf("SECURITY: [%s] Rejected packet from %s: %v", state.Interface.Name, clientAddr.IP, err)
		return
	}

	// Parse DNS message with error handling
	var msg dns.Msg
	if err := msg.Unpack(data); err != nil {
		atomic.AddUint64(&m.securityMetrics.MalformedPackets, 1)
		atomic.AddUint64(&state.ErrorCount, 1)
		log.Printf("SECURITY: [%s] Malformed packet from %s: %v", state.Interface.Name, clientAddr.IP, err)
		return
	}

	// Track total queries (after successful parse)
	atomic.AddUint64(&m.securityMetrics.TotalQueries, 1)

	// Handle responses (for peer discovery) BEFORE query validation
	// Responses legitimately have answers without questions (RFC 6762)
	if msg.Response {
		m.handlePeerDiscoveryResponse(&msg, clientAddr)
		return
	}

	// Only validate queries - skip validation for responses
	if err := m.validateDNSMessage(&msg); err != nil {
		if errors.Is(err, errNonLocalQuery) {
			log.Printf("DEBUG: [%s] Ignored non-local query from %s: %v", state.Interface.Name, clientAddr.IP, err)
		} else {
			atomic.AddUint64(&state.ErrorCount, 1)
			log.Printf("SECURITY: [%s] Invalid DNS query from %s: %v", state.Interface.Name, clientAddr.IP, err)
		}
		return
	}

	// Only handle queries
	if msg.Opcode != dns.OpcodeQuery {
		return
	}

	// Build response
	response := &dns.Msg{}
	response.SetReply(&msg)
	response.Authoritative = true
	response.RecursionAvailable = false

	// Process each question with dual-stack support
	for _, q := range msg.Question {
		// RFC 6762 Section 5.4: Mask out the QU bit when checking class
		qclass := q.Qclass & 0x7FFF
		if qclass != dns.ClassINET {
			continue
		}

		// Handle A record requests (IPv4) - for hostname queries
		if m.matchesAdvertisedName(q.Name) {
			if (q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY) && state.HasIPv4 && state.IPv4 != nil {
				rr := &dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    120,
					},
					A: state.IPv4,
				}
				response.Answer = append(response.Answer, rr)
				log.Printf("DEBUG: [%s-%s] Adding A record: %s -> %s",
					state.Interface.Name, stack, strings.TrimSuffix(q.Name, "."), state.IPv4.String())
			}

			// Handle AAAA record requests (IPv6) - for hostname queries
			if (q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY) && state.HasIPv6 && state.IPv6 != nil {
				rr := &dns.AAAA{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeAAAA,
						Class:  dns.ClassINET,
						Ttl:    120,
					},
					AAAA: state.IPv6,
				}
				response.Answer = append(response.Answer, rr)
				log.Printf("DEBUG: [%s-%s] Adding AAAA record: %s -> %s",
					state.Interface.Name, stack, strings.TrimSuffix(q.Name, "."), state.IPv6.String())
			}
		}

		// Handle DNS-SD service discovery queries (PTR/SRV/TXT)
		m.handlePTRQuery(q, response, state)
		m.handleSRVQuery(q, response, state)
		m.handleTXTQuery(q, response, state)
		m.handleANYQueryForService(q, response, state)
	}

	// Send response if we have answers
	if len(response.Answer) > 0 {
		// Verify host record names are still current before transmitting.
		// Only check A/AAAA records - service records (PTR/SRV/TXT) use different
		// naming patterns and are already derived from currentServiceName().
		for _, rr := range response.Answer {
			switch rr.Header().Rrtype {
			case dns.TypeA, dns.TypeAAAA:
				if !m.matchesAdvertisedName(rr.Header().Name) {
					// Host name changed while we built the response; drop it.
					return
				}
			}
		}

		if responseData, err := response.Pack(); err == nil {
			// Check response size limit
			if len(responseData) > m.securityConfig.MaxResponseSize {
				log.Printf("SECURITY: [%s] Response too large for %s: %d bytes",
					state.Interface.Name, clientAddr.IP, len(responseData))
				return
			}

			// Choose the appropriate connection based on stack
			var conn *net.UDPConn
			if stack == "IPv4" {
				conn = state.IPv4Conn
			} else {
				conn = state.IPv6Conn
			}

			if conn != nil {
				if _, err := conn.WriteToUDP(responseData, clientAddr); err != nil {
					atomic.AddUint64(&state.ErrorCount, 1)
					log.Printf("WARN: [%s-%s] Failed to send response to %s: %v",
						state.Interface.Name, stack, clientAddr.IP, err)
				} else {
					log.Printf("DEBUG: [%s-%s] Responded to query from %s",
						state.Interface.Name, stack, clientAddr.IP)
				}
			}
		}
	}
}

// announcer sends periodic mDNS announcements on all interfaces
func (m *Manager) announcer() {
	defer m.wg.Done()

	// Send initial announcements
	announcements := []time.Duration{0, 1 * time.Second, 2 * time.Second}
	for _, delay := range announcements {
		select {
		case <-m.stopCh:
			return
		case <-time.After(delay):
			m.sendMultiInterfaceAnnouncements()
		}
	}

	// Periodic announcements
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sendMultiInterfaceAnnouncements()
		}
	}
}

// sendMultiInterfaceAnnouncements sends dual-stack mDNS announcements on all active interfaces
func (m *Manager) sendMultiInterfaceAnnouncements() {
	m.sendMultiInterfaceAnnouncementsWithTTL(120)
}

func (m *Manager) sendMultiInterfaceAnnouncementsWithTTL(ttl uint32) {
	names := m.AdvertisedNames()
	if len(names) == 0 {
		return
	}

	type ifaceSnapshot struct {
		name     string
		state    *InterfaceState
		active   bool
		hasIPv4  bool
		hasIPv6  bool
		ipv4     net.IP
		ipv6     net.IP
		ipv4Conn *net.UDPConn
		ipv6Conn *net.UDPConn
	}

	m.mutex.RLock()
	snapshots := make([]ifaceSnapshot, 0, len(m.interfaces))
	for name, state := range m.interfaces {
		if state == nil {
			continue
		}
		snap := ifaceSnapshot{
			name:     name,
			state:    state,
			active:   state.Active,
			hasIPv4:  state.HasIPv4,
			hasIPv6:  state.HasIPv6,
			ipv4Conn: state.IPv4Conn,
			ipv6Conn: state.IPv6Conn,
		}
		if state.IPv4 != nil {
			snap.ipv4 = append(net.IP(nil), state.IPv4...)
		}
		if state.IPv6 != nil {
			snap.ipv6 = append(net.IP(nil), state.IPv6...)
		}
		snapshots = append(snapshots, snap)
	}
	m.mutex.RUnlock()

	for _, snap := range snapshots {
		if !snap.active {
			continue
		}
		for _, fqdn := range names {
			// Send IPv4 announcements
			if snap.hasIPv4 && snap.ipv4Conn != nil && snap.ipv4 != nil {
				m.sendIPv4Announcement(snap.name, snap.state, snap.ipv4Conn, snap.ipv4, fqdn, ttl)
			}

			// Send IPv6 announcements
			if snap.hasIPv6 && snap.ipv6Conn != nil && snap.ipv6 != nil {
				m.sendIPv6Announcement(snap.name, snap.state, snap.ipv6Conn, snap.ipv6, fqdn, ttl)
			}
		}
	}
}

// sendAnnouncementsForNames sends announcements for specific FQDNs with the given TTL.
// Used primarily for sending goodbye (TTL=0) announcements for removed aliases.
func (m *Manager) sendAnnouncementsForNames(fqdns []string, ttl uint32) {
	if len(fqdns) == 0 {
		return
	}

	type ifaceSnapshot struct {
		name     string
		state    *InterfaceState
		active   bool
		hasIPv4  bool
		hasIPv6  bool
		ipv4     net.IP
		ipv6     net.IP
		ipv4Conn *net.UDPConn
		ipv6Conn *net.UDPConn
	}

	m.mutex.RLock()
	snapshots := make([]ifaceSnapshot, 0, len(m.interfaces))
	for name, state := range m.interfaces {
		if state == nil {
			continue
		}
		snap := ifaceSnapshot{
			name:     name,
			state:    state,
			active:   state.Active,
			hasIPv4:  state.HasIPv4,
			hasIPv6:  state.HasIPv6,
			ipv4Conn: state.IPv4Conn,
			ipv6Conn: state.IPv6Conn,
		}
		if state.IPv4 != nil {
			snap.ipv4 = append(net.IP(nil), state.IPv4...)
		}
		if state.IPv6 != nil {
			snap.ipv6 = append(net.IP(nil), state.IPv6...)
		}
		snapshots = append(snapshots, snap)
	}
	m.mutex.RUnlock()

	for _, snap := range snapshots {
		if !snap.active {
			continue
		}
		for _, fqdn := range fqdns {
			if snap.hasIPv4 && snap.ipv4Conn != nil && snap.ipv4 != nil {
				m.sendIPv4Announcement(snap.name, snap.state, snap.ipv4Conn, snap.ipv4, fqdn, ttl)
			}
			if snap.hasIPv6 && snap.ipv6Conn != nil && snap.ipv6 != nil {
				m.sendIPv6Announcement(snap.name, snap.state, snap.ipv6Conn, snap.ipv6, fqdn, ttl)
			}
		}
	}
}

// sendIPv4Announcement sends IPv4 mDNS announcement
func (m *Manager) sendIPv4Announcement(name string, state *InterfaceState, conn *net.UDPConn, ip net.IP, fqdn string, ttl uint32) {
	msg := &dns.Msg{}
	msg.Response = true
	msg.Authoritative = true
	msg.Opcode = dns.OpcodeQuery

	rr := &dns.A{
		Hdr: dns.RR_Header{
			Name:   fqdn,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		A: ip,
	}
	msg.Answer = append(msg.Answer, rr)

	if data, err := msg.Pack(); err == nil {
		multicastAddr := &net.UDPAddr{
			IP:   net.IPv4(224, 0, 0, 251),
			Port: 5353,
		}

		if _, err := conn.WriteToUDP(data, multicastAddr); err == nil {
			log.Printf("DEBUG: [%s-IPv4] Announced %s -> %s",
				name, strings.TrimSuffix(fqdn, "."), ip.String())
		} else {
			log.Printf("WARN: Failed to send IPv4 announcement on %s: %v", name, err)
			if isClosedConnError(err) {
				m.recoverClosedConnection(name, state)
			} else {
				m.markInterfaceFailureSnapshot(name, state, err)
			}
		}
	}
}

// sendIPv6Announcement sends IPv6 mDNS announcement
func (m *Manager) sendIPv6Announcement(name string, state *InterfaceState, conn *net.UDPConn, ip net.IP, fqdn string, ttl uint32) {
	msg := &dns.Msg{}
	msg.Response = true
	msg.Authoritative = true
	msg.Opcode = dns.OpcodeQuery

	rr := &dns.AAAA{
		Hdr: dns.RR_Header{
			Name:   fqdn,
			Rrtype: dns.TypeAAAA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		AAAA: ip,
	}
	msg.Answer = append(msg.Answer, rr)

	if data, err := msg.Pack(); err == nil {
		multicastAddr := &net.UDPAddr{
			IP:   net.ParseIP("ff02::fb"),
			Port: 5353,
		}

		if _, err := conn.WriteToUDP(data, multicastAddr); err == nil {
			log.Printf("DEBUG: [%s-IPv6] Announced %s -> %s",
				name, strings.TrimSuffix(fqdn, "."), ip.String())
		} else {
			log.Printf("WARN: Failed to send IPv6 announcement on %s: %v", name, err)
			if isClosedConnError(err) {
				m.recoverClosedConnection(name, state)
			} else {
				m.markInterfaceFailureSnapshot(name, state, err)
			}
		}
	}
}

func (m *Manager) markInterfaceFailureSnapshot(name string, state *InterfaceState, err error) {
	if state == nil || err == nil {
		return
	}
	if isClosedConnError(err) {
		return
	}
	if m.stopped.Load() {
		return
	}
	m.mutex.RLock()
	current := m.interfaces[name]
	m.mutex.RUnlock()
	if current != state {
		return
	}
	m.markInterfaceFailure(state, err)
}
