package mdns

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/miekg/dns"
)

// validatePacket performs security validation on incoming packets
func (m *Manager) validatePacket(data []byte) error {
	// Check packet size
	if len(data) > m.securityConfig.MaxPacketSize {
		atomic.AddUint64(&m.securityMetrics.TotalQueries, 1)
		atomic.AddUint64(&m.securityMetrics.LargePackets, 1)
		return fmt.Errorf("packet too large: %d bytes", len(data))
	}

	if len(data) < 12 { // Minimum DNS header size
		atomic.AddUint64(&m.securityMetrics.TotalQueries, 1)
		atomic.AddUint64(&m.securityMetrics.MalformedPackets, 1)
		return fmt.Errorf("packet too small: %d bytes", len(data))
	}

	return nil
}

// validateDNSMessage performs DNS-specific validation for mDNS queries
// This should only be called for queries (not responses)
func (m *Manager) validateDNSMessage(msg *dns.Msg) error {
	// Check for DNS query bombs
	if len(msg.Question) > 10 {
		return fmt.Errorf("too many questions: %d", len(msg.Question))
	}

	// Allow reasonable number of answers in probing queries (RFC 6762 Section 8.1)
	if len(msg.Answer) > 10 {
		return fmt.Errorf("too many answer records in query: %d", len(msg.Answer))
	}

	if len(msg.Extra) > 100 {
		return fmt.Errorf("too many extra records: %d", len(msg.Extra))
	}

	// Validate question types
	for _, q := range msg.Question {
		// RFC 6762 Section 5.4: Handle QU bit (unicast-response) in class field
		// The QU bit is the most significant bit of the class field (0x8000)
		// Extract class without QU bit for validation
		qclass := q.Qclass & 0x7FFF
		if qclass != dns.ClassINET {
			return fmt.Errorf("unsupported query class: %d", qclass)
		}

		// Support standard mDNS query types per RFC 6762
		switch q.Qtype {
		case dns.TypeA, dns.TypeAAAA, dns.TypePTR, dns.TypeSRV, dns.TypeTXT, dns.TypeANY:
			// Valid mDNS query types
		default:
			return fmt.Errorf("unsupported query type: %d", q.Qtype)
		}

		// Validate hostname - must be in .local domain for mDNS
		if !strings.HasSuffix(q.Name, ".local.") {
			return fmt.Errorf("non-local query: %s", q.Name)
		}

		if len(q.Name) > 253 { // DNS name length limit
			return fmt.Errorf("hostname too long: %d", len(q.Name))
		}
	}

	return nil
}

// acquireQuerySlot tries to acquire a processing slot for concurrent queries
func (m *Manager) acquireQuerySlot() bool {
	select {
	case m.queryProcessor.semaphore <- struct{}{}:
		atomic.AddInt64(&m.queryProcessor.activeCount, 1)
		return true
	default:
		// No slot available
		return false
	}
}

// releaseQuerySlot releases a processing slot
func (m *Manager) releaseQuerySlot() {
	<-m.queryProcessor.semaphore
	atomic.AddInt64(&m.queryProcessor.activeCount, -1)
}
