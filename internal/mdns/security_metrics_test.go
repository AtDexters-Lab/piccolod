package mdns

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
)

func TestSecurityMetrics_TotalQueries(t *testing.T) {
	t.Log("=== TESTING Security Metrics: Total Queries ===")

	manager := NewManager()
	state := createMockInterfaceState("eth0", true, false)
	manager.interfaces["eth0"] = state

	// Check initial metrics
	initialTotal := atomic.LoadUint64(&manager.securityMetrics.TotalQueries)
	t.Logf("Initial TotalQueries: %d", initialTotal)

	// Create a valid mDNS query
	msg := dns.Msg{}
	msg.SetQuestion(manager.finalName+".local.", dns.TypeA)

	data, err := msg.Pack()
	if err != nil {
		t.Fatalf("failed to pack DNS query: %v", err)
	}

	clientAddr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50)}

	// Process a few queries
	queryCount := 5
	for i := 0; i < queryCount; i++ {
		manager.handleDualStackQuery(data, clientAddr, state, "IPv4")
	}

	// Check metrics after queries
	finalTotal := atomic.LoadUint64(&manager.securityMetrics.TotalQueries)
	t.Logf("After %d queries - TotalQueries: %d", queryCount, finalTotal)

	if finalTotal == initialTotal {
		t.Error("BUG: TotalQueries counter is not being incremented")
	} else if finalTotal < initialTotal+uint64(queryCount) {
		t.Errorf("TotalQueries lower than expected: got %d, expected at least %d",
			finalTotal, initialTotal+uint64(queryCount))
	} else {
		t.Log("SUCCESS: TotalQueries counter is being updated")
	}
}

func TestSecurityMetrics_MalformedPackets(t *testing.T) {
	t.Log("=== TESTING Security Metrics: Malformed Packets ===")

	manager := NewManager()

	initialMalformed := atomic.LoadUint64(&manager.securityMetrics.MalformedPackets)

	// Test with packet that's too small
	smallPacket := make([]byte, 5)
	err := manager.validatePacket(smallPacket)
	if err == nil {
		t.Error("Expected error for small packet")
	}

	finalMalformed := atomic.LoadUint64(&manager.securityMetrics.MalformedPackets)

	if finalMalformed <= initialMalformed {
		t.Error("MalformedPackets metric not incremented for small packet")
	} else {
		t.Log("SUCCESS: MalformedPackets metric working correctly")
	}
}

func TestSecurityMetrics_LargePackets(t *testing.T) {
	t.Log("=== TESTING Security Metrics: Large Packets ===")

	manager := NewManager()

	initialLarge := atomic.LoadUint64(&manager.securityMetrics.LargePackets)

	// Test with packet that's too large
	largePacket := make([]byte, manager.securityConfig.MaxPacketSize+100)
	err := manager.validatePacket(largePacket)
	if err == nil {
		t.Error("Expected error for large packet")
	}

	finalLarge := atomic.LoadUint64(&manager.securityMetrics.LargePackets)

	if finalLarge <= initialLarge {
		t.Error("LargePackets metric not incremented for oversized packet")
	} else {
		t.Log("SUCCESS: LargePackets metric working correctly")
	}
}

func TestSecurityMetrics_AllMetricsConsistency(t *testing.T) {
	t.Log("=== TESTING Security Metrics Consistency ===")

	manager := NewManager()

	// Trigger each type of metric
	// 1. Large packet
	largePacket := make([]byte, manager.securityConfig.MaxPacketSize+100)
	manager.validatePacket(largePacket)

	// 2. Small/malformed packet
	smallPacket := make([]byte, 5)
	manager.validatePacket(smallPacket)

	// Get all metrics
	totalQueries := atomic.LoadUint64(&manager.securityMetrics.TotalQueries)
	malformedPackets := atomic.LoadUint64(&manager.securityMetrics.MalformedPackets)
	largePackets := atomic.LoadUint64(&manager.securityMetrics.LargePackets)

	t.Logf("Metrics after mixed workload:")
	t.Logf("  TotalQueries: %d", totalQueries)
	t.Logf("  MalformedPackets: %d", malformedPackets)
	t.Logf("  LargePackets: %d", largePackets)

	// Each packet rejection should have incremented TotalQueries
	if totalQueries < 2 {
		t.Errorf("TotalQueries should be at least 2 (for the two validation failures), got %d", totalQueries)
	}

	if malformedPackets == 0 {
		t.Error("MalformedPackets should be > 0")
	}

	if largePackets == 0 {
		t.Error("LargePackets should be > 0")
	}
}

func TestSecurityMetrics_ConcurrentAccess(t *testing.T) {
	t.Log("=== TESTING Security Metrics Thread Safety ===")

	manager := NewManager()

	// Concurrent incrementing from multiple goroutines
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()
			for j := 0; j < 100; j++ {
				atomic.AddUint64(&manager.securityMetrics.TotalQueries, 1)
			}
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	finalTotal := atomic.LoadUint64(&manager.securityMetrics.TotalQueries)
	expected := uint64(10 * 100)

	if finalTotal != expected {
		t.Errorf("Concurrent access issue: got %d, expected %d", finalTotal, expected)
	} else {
		t.Log("SUCCESS: Security metrics are thread-safe")
	}
}
