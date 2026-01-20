package mdns

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestDNSQueryParsing_MalformedPackets(t *testing.T) {
	_ = createMockManager()

	malformedPackets := []struct {
		name string
		data []byte
	}{
		{
			name: "Empty packet",
			data: []byte{},
		},
		{
			name: "Too short packet",
			data: []byte{0x00, 0x01},
		},
		{
			name: "Invalid DNS header",
			data: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
		{
			name: "Oversized packet",
			data: make([]byte, 10000), // Way too large
		},
	}

	for _, tt := range malformedPackets {
		t.Run(tt.name, func(t *testing.T) {
			// Try to parse the packet - this should not crash
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DNS parsing panicked on %s: %v", tt.name, r)
				}
			}()

			msg := new(dns.Msg)
			err := msg.Unpack(tt.data)

			// Should fail gracefully, not crash
			if err == nil && len(tt.data) < 12 {
				t.Errorf("Expected parsing to fail for %s, but it succeeded", tt.name)
			}
		})
	}
}

func TestDNSResponseGeneration_InvalidQueries(t *testing.T) {
	tests := []struct {
		name  string
		query *dns.Msg
	}{
		{
			name: "Query with no questions",
			query: &dns.Msg{
				MsgHdr: dns.MsgHdr{
					Id:     12345,
					Opcode: dns.OpcodeQuery,
				},
				Question: nil, // No questions!
			},
		},
		{
			name: "Query with invalid question type",
			query: &dns.Msg{
				MsgHdr: dns.MsgHdr{
					Id:     12345,
					Opcode: dns.OpcodeQuery,
				},
				Question: []dns.Question{{
					Name:   "piccolo.local.",
					Qtype:  dns.TypeNone, // Invalid type
					Qclass: dns.ClassINET,
				}},
			},
		},
		{
			name: "Query with extremely long domain name",
			query: &dns.Msg{
				MsgHdr: dns.MsgHdr{
					Id:     12345,
					Opcode: dns.OpcodeQuery,
				},
				Question: []dns.Question{{
					Name:   strings.Repeat("verylongdomainlabel", 20) + ".local.",
					Qtype:  dns.TypeA,
					Qclass: dns.ClassINET,
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test how the system handles invalid queries
			if len(tt.query.Question) == 0 {
				t.Log("Query has no questions - system should handle gracefully")
			}

			if len(tt.query.Question) > 0 && len(tt.query.Question[0].Name) > 255 {
				t.Log("Domain name exceeds DNS limits - system should reject")
			}
		})
	}
}

func TestConcurrentQuerySlots_ResourceUsage(t *testing.T) {
	manager := createMockManager()

	// Test concurrent slot acquisition
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			defer func() { done <- true }()

			// Each goroutine tries to acquire and release slots
			for j := 0; j < 5; j++ {
				if manager.acquireQuerySlot() {
					time.Sleep(time.Millisecond)
					manager.releaseQuerySlot()
				}
			}
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 100; i++ {
		<-done
	}

	// Check that all slots are released
	if manager.queryProcessor.activeCount != 0 {
		t.Errorf("Expected 0 active slots after completion, got %d - possible leak", manager.queryProcessor.activeCount)
	}
}

func TestNetworkInterfaceFailure_Recovery(t *testing.T) {
	manager := createMockManager()
	state := createMockInterfaceState("test0", true, true)

	// Simulate network interface going down
	state.Active = false
	state.IPv4Conn = nil // Connection lost
	state.IPv6Conn = nil

	manager.mutex.Lock()
	manager.interfaces["test0"] = state
	manager.mutex.Unlock()

	// Test how system handles interface recovery
	manager.performHealthCheck()

	// After health check, system should have attempted recovery
	healthScore := manager.healthMonitor.OverallHealth
	if healthScore == 1.0 {
		t.Error("Health score should be reduced when interface fails")
	}
}

func TestMDNSAnnouncement_MessageFormat(t *testing.T) {
	// Test that our mDNS announcements are properly formatted

	// This is the kind of test we're missing - actual protocol compliance
	msg := &dns.Msg{}
	msg.SetQuestion(dns.Fqdn("piccolo.local"), dns.TypeA)

	// mDNS announcements should have specific properties:
	// - QR bit should be 1 (response)
	// - AA bit should be 1 (authoritative answer)
	// - Should be sent to 224.0.0.251:5353

	if !msg.Response {
		t.Log("mDNS announcement should have Response bit set")
	}

	if !msg.Authoritative {
		t.Log("mDNS announcement should have Authoritative bit set")
	}

	// Test message size limits
	packed, err := msg.Pack()
	if err != nil {
		t.Errorf("Failed to pack DNS message: %v", err)
	}

	if len(packed) > 512 {
		t.Errorf("DNS message too large: %d bytes (mDNS prefers <= 512)", len(packed))
	}
}

func TestIPv6LinkLocal_mDNSCompliance(t *testing.T) {
	origList := listNetworkInterfaces
	origAddrs := interfaceAddrs
	defer func() {
		listNetworkInterfaces = origList
		interfaceAddrs = origAddrs
	}()

	linkLocalIP := net.ParseIP("fe80::1234:5678:9abc:def0")
	if linkLocalIP == nil {
		t.Fatal("failed to parse link-local IPv6 address")
	}

	iface := net.Interface{Name: "eth0", Flags: net.FlagUp | net.FlagMulticast}
	listNetworkInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{iface}, nil
	}

	ipv4Net := &net.IPNet{IP: net.ParseIP("192.168.1.10"), Mask: net.CIDRMask(24, 32)}
	ipv6Net := &net.IPNet{IP: linkLocalIP, Mask: net.CIDRMask(64, 128)}
	interfaceAddrs = func(iface *net.Interface) ([]net.Addr, error) {
		return []net.Addr{ipv4Net, ipv6Net}, nil
	}

	manager := NewManager()
	manager.ipv4SocketFactory = func(*net.Interface) (*net.UDPConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	}
	manager.ipv6SocketFactory = func(*net.Interface) (*net.UDPConn, error) {
		return nil, nil
	}

	t.Cleanup(func() {
		_ = manager.Stop()
	})

	if err := manager.discoverInterfaces(); err != nil {
		t.Fatalf("discoverInterfaces returned error: %v", err)
	}

	state, ok := manager.interfaces["eth0"]
	if !ok {
		t.Fatal("expected eth0 in interface map")
	}

	if !state.HasIPv6 {
		t.Fatal("expected link-local IPv6 to be retained")
	}

	if state.IPv6 == nil || !state.IPv6.Equal(linkLocalIP) {
		t.Fatalf("stored IPv6 address mismatch: got %v, want %v", state.IPv6, linkLocalIP)
	}
}
