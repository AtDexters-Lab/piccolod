package mdns

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

func TestValidatePacket_LargePacket(t *testing.T) {
	manager := createMockManager()

	// Create a packet larger than the max size
	largePacket := make([]byte, manager.securityConfig.MaxPacketSize+100)

	err := manager.validatePacket(largePacket)
	if err == nil {
		t.Error("Expected error for large packet")
	}

	// Verify metrics were updated
	if manager.securityMetrics.LargePackets != 1 {
		t.Errorf("LargePackets = %d, want 1", manager.securityMetrics.LargePackets)
	}
}

func TestValidatePacket_SmallPacket(t *testing.T) {
	manager := createMockManager()

	// Create a packet smaller than the minimum DNS header
	smallPacket := make([]byte, 10)

	err := manager.validatePacket(smallPacket)
	if err == nil {
		t.Error("Expected error for small packet")
	}

	// Verify metrics were updated
	if manager.securityMetrics.MalformedPackets != 1 {
		t.Errorf("MalformedPackets = %d, want 1", manager.securityMetrics.MalformedPackets)
	}
}

func TestValidatePacket_ValidPacket(t *testing.T) {
	manager := createMockManager()

	// Create a valid-sized packet (minimum DNS header size is 12)
	validPacket := make([]byte, 100)

	err := manager.validatePacket(validPacket)
	if err != nil {
		t.Errorf("Unexpected error for valid packet: %v", err)
	}
}

func TestValidateDNSMessage_TooManyQuestions(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	for i := 0; i < 15; i++ {
		msg.Question = append(msg.Question, dns.Question{
			Name:   "test.local.",
			Qtype:  dns.TypeA,
			Qclass: dns.ClassINET,
		})
	}

	err := manager.validateDNSMessage(msg)
	if err == nil {
		t.Error("Expected error for too many questions")
	}
}

func TestValidateDNSMessage_TooManyAnswers(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "test.local.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	})
	for i := 0; i < 15; i++ {
		msg.Answer = append(msg.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: "test.local.", Rrtype: dns.TypeA, Class: dns.ClassINET},
			A:   net.ParseIP("192.168.1.1"),
		})
	}

	err := manager.validateDNSMessage(msg)
	if err == nil {
		t.Error("Expected error for too many answers")
	}
}

func TestValidateDNSMessage_QUBit(t *testing.T) {
	manager := createMockManager()

	// RFC 6762 Section 5.4: The QU bit (0x8000) requests unicast response
	// Qclass 0x8001 = ClassINET (1) | QU bit (0x8000)
	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeA,
		Qclass: 0x8001, // ClassINET with QU bit set
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("QU bit (class 0x8001) should be accepted: %v", err)
	}
}

func TestValidateDNSMessage_PTRQuery(t *testing.T) {
	manager := createMockManager()

	// PTR queries are essential for mDNS service discovery
	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "_services._dns-sd._udp.local.",
		Qtype:  dns.TypePTR,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("PTR queries should be accepted: %v", err)
	}
}

func TestValidateDNSMessage_SRVQuery(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "_http._tcp.local.",
		Qtype:  dns.TypeSRV,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("SRV queries should be accepted: %v", err)
	}
}

func TestValidateDNSMessage_TXTQuery(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeTXT,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("TXT queries should be accepted: %v", err)
	}
}

func TestValidateDNSMessage_UnsupportedQueryType(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeMX, // MX is not a valid mDNS query type
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err == nil {
		t.Error("Unsupported query type (MX) should be rejected")
	}
}

func TestValidateDNSMessage_NonLocalQuery(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "google.com.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err == nil {
		t.Error("Non-.local queries should be rejected")
	}
}

func TestValidateDNSMessage_UnsupportedClass(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassCHAOS, // Not ClassINET
	})

	err := manager.validateDNSMessage(msg)
	if err == nil {
		t.Error("Non-INET class should be rejected")
	}
}

func TestValidateDNSMessage_ValidAQuery(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("Valid A query should be accepted: %v", err)
	}
}

func TestValidateDNSMessage_ValidAAAAQuery(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeAAAA,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("Valid AAAA query should be accepted: %v", err)
	}
}

func TestValidateDNSMessage_ValidANYQuery(t *testing.T) {
	manager := createMockManager()

	msg := &dns.Msg{}
	msg.Question = append(msg.Question, dns.Question{
		Name:   "piccolo.local.",
		Qtype:  dns.TypeANY,
		Qclass: dns.ClassINET,
	})

	err := manager.validateDNSMessage(msg)
	if err != nil {
		t.Errorf("Valid ANY query should be accepted: %v", err)
	}
}

func TestHandleDualStackQueryCountsQueryOnce(t *testing.T) {
	manager := NewManager()
	state := createMockInterfaceState("eth0", true, false)
	manager.interfaces["eth0"] = state

	msg := dns.Msg{}
	msg.SetQuestion(manager.finalName+".local.", dns.TypeA)

	data, err := msg.Pack()
	if err != nil {
		t.Fatalf("failed to pack DNS query: %v", err)
	}

	clientAddr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50)}

	manager.handleDualStackQuery(data, clientAddr, state, "IPv4")

	if got := manager.securityMetrics.TotalQueries; got != 1 {
		t.Fatalf("TotalQueries = %d, want 1", got)
	}
}

func TestQueryProcessorSemaphore(t *testing.T) {
	manager := createMockManager()
	processor := manager.queryProcessor

	// Test semaphore capacity
	maxConcurrent := manager.securityConfig.MaxConcurrentQueries
	if cap(processor.semaphore) != maxConcurrent {
		t.Errorf("Semaphore capacity = %v, want %v", cap(processor.semaphore), maxConcurrent)
	}

	// Test acquiring semaphore slots
	for i := 0; i < maxConcurrent; i++ {
		select {
		case processor.semaphore <- struct{}{}:
			// Successfully acquired slot
		default:
			t.Fatalf("Failed to acquire semaphore slot %d", i)
		}
	}

	// Test that we can't acquire more than max
	select {
	case processor.semaphore <- struct{}{}:
		t.Error("Should not be able to acquire more than max concurrent slots")
	default:
		// Expected behavior
	}

	// Test releasing slots
	for i := 0; i < maxConcurrent; i++ {
		select {
		case <-processor.semaphore:
			// Successfully released slot
		default:
			t.Fatalf("Failed to release semaphore slot %d", i)
		}
	}
}

func TestSecurityMetricsIncrement(t *testing.T) {
	manager := createMockManager()
	metrics := manager.securityMetrics

	// Test initial state
	if metrics.TotalQueries != 0 {
		t.Error("Initial TotalQueries should be 0")
	}

	// Test incrementing metrics (simulating what would happen in real usage)
	metrics.TotalQueries++
	metrics.MalformedPackets++
	metrics.LargePackets++

	// Verify increments
	tests := []struct {
		name     string
		actual   uint64
		expected uint64
	}{
		{"TotalQueries", metrics.TotalQueries, 1},
		{"MalformedPackets", metrics.MalformedPackets, 1},
		{"LargePackets", metrics.LargePackets, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.actual != tt.expected {
				t.Errorf("%s = %v, want %v", tt.name, tt.actual, tt.expected)
			}
		})
	}
}

func TestSecurityConfigValidation(t *testing.T) {
	manager := createMockManager()
	config := manager.securityConfig

	// Test configuration values are positive
	tests := []struct {
		name  string
		value int
	}{
		{"MaxPacketSize", config.MaxPacketSize},
		{"MaxResponseSize", config.MaxResponseSize},
		{"MaxConcurrentQueries", config.MaxConcurrentQueries},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value <= 0 {
				t.Errorf("%s should be positive, got %v", tt.name, tt.value)
			}
		})
	}

	// Test duration values are positive
	if config.QueryTimeout <= 0 {
		t.Error("QueryTimeout should be positive")
	}

	// Test reasonable limits
	if config.MaxPacketSize > 65535 {
		t.Error("MaxPacketSize should not exceed maximum UDP packet size")
	}

	if config.MaxResponseSize > config.MaxPacketSize {
		t.Error("MaxResponseSize should not exceed MaxPacketSize")
	}
}

func TestAcquireReleaseQuerySlot(t *testing.T) {
	manager := createMockManager()

	// Acquire a slot
	if !manager.acquireQuerySlot() {
		t.Error("Should be able to acquire query slot")
	}

	if manager.queryProcessor.activeCount != 1 {
		t.Errorf("activeCount = %d, want 1", manager.queryProcessor.activeCount)
	}

	// Release the slot
	manager.releaseQuerySlot()

	if manager.queryProcessor.activeCount != 0 {
		t.Errorf("activeCount = %d, want 0", manager.queryProcessor.activeCount)
	}
}

func TestAcquireQuerySlotConcurrent(t *testing.T) {
	manager := createMockManager()
	maxConcurrent := manager.securityConfig.MaxConcurrentQueries

	// Fill up all slots
	for i := 0; i < maxConcurrent; i++ {
		if !manager.acquireQuerySlot() {
			t.Fatalf("Failed to acquire slot %d", i)
		}
	}

	// Verify no more slots available
	if manager.acquireQuerySlot() {
		t.Error("Should not be able to acquire more slots than maxConcurrent")
		manager.releaseQuerySlot()
	}

	// Release all slots
	for i := 0; i < maxConcurrent; i++ {
		manager.releaseQuerySlot()
	}

	// Verify all slots are available again
	if !manager.acquireQuerySlot() {
		t.Error("Should be able to acquire slot after releasing all")
	}
	manager.releaseQuerySlot()

	// Verify active count is 0
	if manager.queryProcessor.activeCount != 0 {
		t.Errorf("activeCount should be 0, got %d", manager.queryProcessor.activeCount)
	}
}
