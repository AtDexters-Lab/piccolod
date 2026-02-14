package mdns

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const (
	serviceName = "_piccolo._tcp"
	serviceTTL  = 4500 // 75 minutes per RFC 6762
	hostTTL     = 120  // 2 minutes for A/AAAA records
)

// Note: localTLD ("local") is defined in names.go and reused here

// ServiceFQDN returns the service type FQDN (e.g., "_piccolo._tcp.local.").
func (m *Manager) ServiceFQDN() string {
	return serviceName + "." + localTLD + "."
}

// InstanceFQDN returns the instance FQDN (e.g., "piccolo._piccolo._tcp.local.").
func (m *Manager) InstanceFQDN() string {
	return m.currentServiceName() + "." + serviceName + "." + localTLD + "."
}

// buildTXTStrings returns the TXT record strings for service metadata.
// Thread-safe: acquires mutex to read serviceMetadata.
func (m *Manager) buildTXTStrings() []string {
	m.mutex.RLock()
	meta := m.serviceMetadata
	m.mutex.RUnlock()

	if meta == nil {
		return nil
	}

	return []string{
		"txtvers=1",
		"id=" + meta.MachineID,
		"model=" + meta.Model,
		"name=" + meta.PreferredName,
		"version=" + meta.Version,
		"boot=" + strconv.FormatInt(meta.BootTime.Unix(), 10),
	}
}

// buildServiceAnnouncement creates a DNS-SD announcement message.
// The message contains PTR, SRV, TXT, and A/AAAA records.
// Thread-safe: uses Port() to read port under mutex.
func (m *Manager) buildServiceAnnouncement(state *InterfaceState, ttl uint32) *dns.Msg {
	msg := &dns.Msg{}
	msg.Response = true
	msg.Authoritative = true
	msg.Opcode = dns.OpcodeQuery

	serviceFQDN := m.ServiceFQDN()
	instanceFQDN := m.InstanceFQDN()
	hostFQDN := m.currentServiceName() + "." + localTLD + "."
	port := m.Port() // Thread-safe read

	// PTR record: _piccolo._tcp.local. -> piccolo._piccolo._tcp.local.
	ptrRR := &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   serviceFQDN,
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ptr: instanceFQDN,
	}
	msg.Answer = append(msg.Answer, ptrRR)

	// SRV record: piccolo._piccolo._tcp.local. -> piccolo.local.:port
	srvRR := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   instanceFQDN,
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Priority: 0,
		Weight:   0,
		Port:     uint16(port),
		Target:   hostFQDN,
	}
	msg.Answer = append(msg.Answer, srvRR)

	// TXT record: piccolo._piccolo._tcp.local. -> metadata
	txtStrings := m.buildTXTStrings()
	if len(txtStrings) > 0 {
		txtRR := &dns.TXT{
			Hdr: dns.RR_Header{
				Name:   instanceFQDN,
				Rrtype: dns.TypeTXT,
				Class:  dns.ClassINET,
				Ttl:    ttl,
			},
			Txt: txtStrings,
		}
		msg.Answer = append(msg.Answer, txtRR)
	}

	// Add A/AAAA records in Extra section with shorter TTL
	hostTTLValue := uint32(hostTTL)
	if ttl == 0 {
		hostTTLValue = 0 // For goodbye messages
	}

	if state.HasIPv4 && state.IPv4 != nil {
		aRR := &dns.A{
			Hdr: dns.RR_Header{
				Name:   hostFQDN,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    hostTTLValue,
			},
			A: state.IPv4,
		}
		msg.Extra = append(msg.Extra, aRR)
	}

	if state.HasIPv6 && state.IPv6 != nil {
		aaaaRR := &dns.AAAA{
			Hdr: dns.RR_Header{
				Name:   hostFQDN,
				Rrtype: dns.TypeAAAA,
				Class:  dns.ClassINET,
				Ttl:    hostTTLValue,
			},
			AAAA: state.IPv6,
		}
		msg.Extra = append(msg.Extra, aaaaRR)
	}

	return msg
}

// sendServiceAnnouncement sends DNS-SD announcements on all interfaces.
func (m *Manager) sendServiceAnnouncement() {
	m.sendServiceAnnouncementWithTTL(serviceTTL)
}

// sendServiceAnnouncementWithTTL sends DNS-SD announcements with a specific TTL.
func (m *Manager) sendServiceAnnouncementWithTTL(ttl uint32) {
	type ifaceSnapshot struct {
		name     string
		state    *InterfaceState
		active   bool
		ipv4Conn *net.UDPConn
		ipv6Conn *net.UDPConn
	}

	m.mutex.RLock()
	snapshots := make([]ifaceSnapshot, 0, len(m.interfaces))
	for name, state := range m.interfaces {
		if state == nil {
			continue
		}
		snapshots = append(snapshots, ifaceSnapshot{
			name:     name,
			state:    state,
			active:   state.Active,
			ipv4Conn: state.IPv4Conn,
			ipv6Conn: state.IPv6Conn,
		})
	}
	m.mutex.RUnlock()

	for _, snap := range snapshots {
		if !snap.active {
			continue
		}

		msg := m.buildServiceAnnouncement(snap.state, ttl)
		data, err := msg.Pack()
		if err != nil {
			log.Printf("WARN: Failed to pack service announcement: %v", err)
			continue
		}

		// Send on IPv4
		if snap.ipv4Conn != nil {
			multicastAddr := &net.UDPAddr{
				IP:   net.IPv4(224, 0, 0, 251),
				Port: 5353,
			}
			if _, err := snap.ipv4Conn.WriteToUDP(data, multicastAddr); err == nil {
				if ttl > 0 {
					log.Printf("DEBUG: [%s-IPv4] Announced service %s", snap.name, m.InstanceFQDN())
				} else {
					log.Printf("DEBUG: [%s-IPv4] Sent service goodbye for %s", snap.name, m.InstanceFQDN())
				}
			} else {
				log.Printf("WARN: [%s-IPv4] Failed to send service announcement: %v", snap.name, err)
				if isClosedConnError(err) {
					m.recoverClosedConnection(snap.name, snap.state)
				}
			}
		}

		// Send on IPv6
		if snap.ipv6Conn != nil {
			multicastAddr := &net.UDPAddr{
				IP:   net.ParseIP("ff02::fb"),
				Port: 5353,
			}
			if _, err := snap.ipv6Conn.WriteToUDP(data, multicastAddr); err == nil {
				if ttl > 0 {
					log.Printf("DEBUG: [%s-IPv6] Announced service %s", snap.name, m.InstanceFQDN())
				} else {
					log.Printf("DEBUG: [%s-IPv6] Sent service goodbye for %s", snap.name, m.InstanceFQDN())
				}
			} else {
				log.Printf("WARN: [%s-IPv6] Failed to send service announcement: %v", snap.name, err)
				if isClosedConnError(err) {
					m.recoverClosedConnection(snap.name, snap.state)
				}
			}
		}
	}
}

// serviceAnnouncer periodically sends DNS-SD service announcements.
func (m *Manager) serviceAnnouncer() {
	defer m.wg.Done()

	// Send initial announcements (RFC 6762 recommends 2-3 at startup)
	delays := []time.Duration{0, 1 * time.Second, 2 * time.Second}
	for _, delay := range delays {
		select {
		case <-m.stopCh:
			return
		case <-time.After(delay):
			m.sendServiceAnnouncement()
		}
	}

	// Periodic announcements at 60s by default; leader increases to 10s for fast failover.
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case interval := <-m.serviceIntervalCh:
			ticker.Reset(interval)
		case <-ticker.C:
			m.sendServiceAnnouncement()
		}
	}
}

// Maximum length for TXT record string fields to prevent memory abuse.
const maxTXTFieldLen = 256

// Minimum valid boot timestamp (Jan 1, 2000 00:00:00 UTC).
var minBootTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// parseTXTRecord extracts ServiceMetadata from TXT record strings.
// Invalid machine IDs are rejected for security.
// String fields are truncated to maxTXTFieldLen to prevent memory abuse.
// Boot timestamps must be between year 2000 and now.
func parseTXTRecord(txt []string) *ServiceMetadata {
	meta := &ServiceMetadata{}

	for _, s := range txt {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToLower(parts[0])
		value := parts[1]

		switch key {
		case "txtvers":
			if v, err := strconv.Atoi(value); err == nil {
				meta.TxtVersion = v
			}
		case "id":
			// Validate machine ID format (6 hex chars) to prevent injection
			if machineIDPattern.MatchString(value) {
				meta.MachineID = value
			}
		case "model":
			meta.Model = truncateString(value, maxTXTFieldLen)
		case "name":
			meta.PreferredName = truncateString(value, maxTXTFieldLen)
		case "version":
			meta.Version = truncateString(value, maxTXTFieldLen)
		case "boot":
			if ts, err := strconv.ParseInt(value, 10, 64); err == nil {
				bootTime := time.Unix(ts, 0)
				// Validate timestamp is reasonable (after 2000, not in future)
				if bootTime.After(minBootTime) && bootTime.Before(time.Now().Add(time.Hour)) {
					meta.BootTime = bootTime
				}
			}
		}
	}

	return meta
}

// truncateString returns s truncated to maxLen characters.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// SetVersion sets the version string for service metadata.
func (m *Manager) SetVersion(version string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.version = version
	m.rebuildServiceMetadata()
}

// rebuildServiceMetadata reconstructs the service metadata with current values.
// Must be called with m.mutex held.
func (m *Manager) rebuildServiceMetadata() {
	m.serviceMetadata = &ServiceMetadata{
		TxtVersion:    1,
		MachineID:     m.machineID,
		Model:         m.deviceModel,
		PreferredName: m.baseName,
		Version:       m.version,
		BootTime:      m.bootTime,
	}
}

// handlePTRQuery handles PTR queries for _piccolo._tcp.local.
// Returns true if the query was handled.
// Per RFC 6763 Section 12.1, the response includes SRV, TXT, and A/AAAA in the Additional section.
func (m *Manager) handlePTRQuery(q dns.Question, response *dns.Msg, state *InterfaceState) bool {
	if q.Qtype != dns.TypePTR {
		return false
	}

	if !strings.EqualFold(q.Name, m.ServiceFQDN()) {
		return false
	}

	instanceFQDN := m.InstanceFQDN()
	hostFQDN := m.currentServiceName() + "." + localTLD + "."
	port := m.Port()

	// PTR record in Answer section
	ptrRR := &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    serviceTTL,
		},
		Ptr: instanceFQDN,
	}
	response.Answer = append(response.Answer, ptrRR)

	// RFC 6763 Section 12.1: Include SRV and TXT in Additional section
	srvRR := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   instanceFQDN,
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    serviceTTL,
		},
		Priority: 0,
		Weight:   0,
		Port:     uint16(port),
		Target:   hostFQDN,
	}
	response.Extra = append(response.Extra, srvRR)

	txtStrings := m.buildTXTStrings()
	if len(txtStrings) > 0 {
		txtRR := &dns.TXT{
			Hdr: dns.RR_Header{
				Name:   instanceFQDN,
				Rrtype: dns.TypeTXT,
				Class:  dns.ClassINET,
				Ttl:    serviceTTL,
			},
			Txt: txtStrings,
		}
		response.Extra = append(response.Extra, txtRR)
	}

	// Include A/AAAA records for the target hostname
	if state.HasIPv4 && state.IPv4 != nil {
		aRR := &dns.A{
			Hdr: dns.RR_Header{
				Name:   hostFQDN,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    hostTTL,
			},
			A: state.IPv4,
		}
		response.Extra = append(response.Extra, aRR)
	}

	if state.HasIPv6 && state.IPv6 != nil {
		aaaaRR := &dns.AAAA{
			Hdr: dns.RR_Header{
				Name:   hostFQDN,
				Rrtype: dns.TypeAAAA,
				Class:  dns.ClassINET,
				Ttl:    hostTTL,
			},
			AAAA: state.IPv6,
		}
		response.Extra = append(response.Extra, aaaaRR)
	}

	log.Printf("DEBUG: [%s] Adding PTR record: %s -> %s (with SRV/TXT/A/AAAA)",
		state.Interface.Name, strings.TrimSuffix(q.Name, "."), instanceFQDN)

	return true
}

// handleSRVQuery handles SRV queries for our instance FQDN.
// Returns true if the query was handled.
// Thread-safe: uses Port() to read port under mutex.
func (m *Manager) handleSRVQuery(q dns.Question, response *dns.Msg, state *InterfaceState) bool {
	if q.Qtype != dns.TypeSRV {
		return false
	}

	if !strings.EqualFold(q.Name, m.InstanceFQDN()) {
		return false
	}

	hostFQDN := m.currentServiceName() + "." + localTLD + "."
	port := m.Port() // Thread-safe read

	srvRR := &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    serviceTTL,
		},
		Priority: 0,
		Weight:   0,
		Port:     uint16(port),
		Target:   hostFQDN,
	}
	response.Answer = append(response.Answer, srvRR)

	// Add A/AAAA in Extra section
	if state.HasIPv4 && state.IPv4 != nil {
		aRR := &dns.A{
			Hdr: dns.RR_Header{
				Name:   hostFQDN,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    hostTTL,
			},
			A: state.IPv4,
		}
		response.Extra = append(response.Extra, aRR)
	}

	if state.HasIPv6 && state.IPv6 != nil {
		aaaaRR := &dns.AAAA{
			Hdr: dns.RR_Header{
				Name:   hostFQDN,
				Rrtype: dns.TypeAAAA,
				Class:  dns.ClassINET,
				Ttl:    hostTTL,
			},
			AAAA: state.IPv6,
		}
		response.Extra = append(response.Extra, aaaaRR)
	}

	log.Printf("DEBUG: [%s] Adding SRV record: %s -> %s:%d",
		state.Interface.Name, strings.TrimSuffix(q.Name, "."), hostFQDN, port)

	return true
}

// handleTXTQuery handles TXT queries for our instance FQDN.
// Returns true if the query was handled.
func (m *Manager) handleTXTQuery(q dns.Question, response *dns.Msg, state *InterfaceState) bool {
	if q.Qtype != dns.TypeTXT {
		return false
	}

	if !strings.EqualFold(q.Name, m.InstanceFQDN()) {
		return false
	}

	txtStrings := m.buildTXTStrings()
	if len(txtStrings) == 0 {
		return false
	}

	txtRR := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    serviceTTL,
		},
		Txt: txtStrings,
	}
	response.Answer = append(response.Answer, txtRR)

	log.Printf("DEBUG: [%s] Adding TXT record for %s",
		state.Interface.Name, strings.TrimSuffix(q.Name, "."))

	return true
}

// handleANYQueryForService handles ANY queries for service-related FQDNs.
// Returns true if any records were added.
func (m *Manager) handleANYQueryForService(q dns.Question, response *dns.Msg, state *InterfaceState) bool {
	if q.Qtype != dns.TypeANY {
		return false
	}

	added := false

	// Check if it's for the service type
	if strings.EqualFold(q.Name, m.ServiceFQDN()) {
		ptrQ := dns.Question{Name: q.Name, Qtype: dns.TypePTR, Qclass: q.Qclass}
		added = m.handlePTRQuery(ptrQ, response, state) || added
	}

	// Check if it's for our instance
	if strings.EqualFold(q.Name, m.InstanceFQDN()) {
		srvQ := dns.Question{Name: q.Name, Qtype: dns.TypeSRV, Qclass: q.Qclass}
		txtQ := dns.Question{Name: q.Name, Qtype: dns.TypeTXT, Qclass: q.Qclass}
		added = m.handleSRVQuery(srvQ, response, state) || added
		added = m.handleTXTQuery(txtQ, response, state) || added
	}

	return added
}

// logServiceMetadata logs the current service metadata configuration.
func (m *Manager) logServiceMetadata() {
	if m.serviceMetadata == nil {
		return
	}

	log.Printf("INFO: Service metadata - ID: %s, Model: %s, Name: %s, Version: %s, Boot: %s",
		m.serviceMetadata.MachineID,
		m.serviceMetadata.Model,
		m.serviceMetadata.PreferredName,
		m.serviceMetadata.Version,
		m.serviceMetadata.BootTime.Format(time.RFC3339))
}

// formatServiceInfo returns a formatted string of service information.
// Thread-safe: uses Port() to read port under mutex.
func (m *Manager) formatServiceInfo() string {
	return fmt.Sprintf("service=%s instance=%s hostname=%s port=%d",
		m.ServiceFQDN(),
		m.InstanceFQDN(),
		m.Hostname(),
		m.Port())
}
