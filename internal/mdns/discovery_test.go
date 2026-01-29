package mdns

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestPeerRegistry_UpdateAndGet(t *testing.T) {
	registry := newPeerRegistry()

	// Add a new peer
	machineID := "test123"
	isNew := registry.UpdatePeer(machineID, func(peer *DiscoveredPeer) {
		peer.Hostname = "test-device.local"
		peer.Model = "Test Model"
		peer.Version = "1.0.0"
		peer.IPv4 = net.ParseIP("192.168.1.100")
	})

	if !isNew {
		t.Error("Expected first update to report peer as new")
	}

	// Verify we can get the peer
	peer, exists := registry.GetPeer(machineID)
	if !exists {
		t.Fatal("Expected peer to exist")
	}

	if peer.Hostname != "test-device.local" {
		t.Errorf("Expected hostname 'test-device.local', got '%s'", peer.Hostname)
	}
	if peer.Model != "Test Model" {
		t.Errorf("Expected model 'Test Model', got '%s'", peer.Model)
	}
	if peer.Version != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got '%s'", peer.Version)
	}
	if !peer.IPv4.Equal(net.ParseIP("192.168.1.100")) {
		t.Errorf("Expected IPv4 192.168.1.100, got %v", peer.IPv4)
	}
	if peer.FirstSeen.IsZero() {
		t.Error("Expected FirstSeen to be set")
	}
	if peer.LastSeen.IsZero() {
		t.Error("Expected LastSeen to be set")
	}
}

func TestPeerRegistry_UpdateExisting(t *testing.T) {
	registry := newPeerRegistry()
	machineID := "test456"

	// Add initial peer
	registry.UpdatePeer(machineID, func(peer *DiscoveredPeer) {
		peer.Hostname = "original.local"
		peer.Version = "1.0.0"
	})

	// Get first seen time
	peer1, _ := registry.GetPeer(machineID)
	firstSeen := peer1.FirstSeen

	// Wait a bit and update
	time.Sleep(10 * time.Millisecond)

	isNew := registry.UpdatePeer(machineID, func(peer *DiscoveredPeer) {
		peer.Version = "2.0.0"
	})

	if isNew {
		t.Error("Expected second update to report peer as existing")
	}

	peer2, _ := registry.GetPeer(machineID)
	if peer2.Version != "2.0.0" {
		t.Errorf("Expected version '2.0.0', got '%s'", peer2.Version)
	}
	if !peer2.FirstSeen.Equal(firstSeen) {
		t.Error("FirstSeen should not change on update")
	}
	if !peer2.LastSeen.After(peer1.LastSeen) {
		t.Error("LastSeen should be updated")
	}
}

func TestPeerRegistry_List(t *testing.T) {
	registry := newPeerRegistry()

	// Add multiple peers
	registry.UpdatePeer("peer1", func(peer *DiscoveredPeer) {
		peer.Hostname = "zeta.local"
	})
	registry.UpdatePeer("peer2", func(peer *DiscoveredPeer) {
		peer.Hostname = "alpha.local"
	})
	registry.UpdatePeer("peer3", func(peer *DiscoveredPeer) {
		peer.Hostname = "beta.local"
	})

	peers := registry.List()
	if len(peers) != 3 {
		t.Fatalf("Expected 3 peers, got %d", len(peers))
	}

	// Should be sorted by hostname
	if peers[0].Hostname != "alpha.local" {
		t.Errorf("Expected first peer 'alpha.local', got '%s'", peers[0].Hostname)
	}
	if peers[1].Hostname != "beta.local" {
		t.Errorf("Expected second peer 'beta.local', got '%s'", peers[1].Hostname)
	}
	if peers[2].Hostname != "zeta.local" {
		t.Errorf("Expected third peer 'zeta.local', got '%s'", peers[2].Hostname)
	}
}

func TestPeerRegistry_Count(t *testing.T) {
	registry := newPeerRegistry()

	if registry.Count() != 0 {
		t.Errorf("Expected count 0, got %d", registry.Count())
	}

	registry.UpdatePeer("peer1", func(peer *DiscoveredPeer) {})
	if registry.Count() != 1 {
		t.Errorf("Expected count 1, got %d", registry.Count())
	}

	registry.UpdatePeer("peer2", func(peer *DiscoveredPeer) {})
	if registry.Count() != 2 {
		t.Errorf("Expected count 2, got %d", registry.Count())
	}

	// Update existing should not change count
	registry.UpdatePeer("peer1", func(peer *DiscoveredPeer) {})
	if registry.Count() != 2 {
		t.Errorf("Expected count 2 after update, got %d", registry.Count())
	}
}

func TestPeerRegistry_RemoveStale(t *testing.T) {
	registry := newPeerRegistry()

	// Add peers with different last seen times
	registry.UpdatePeer("old", func(peer *DiscoveredPeer) {
		peer.LastSeen = time.Now().Add(-5 * time.Minute)
	})
	registry.UpdatePeer("new", func(peer *DiscoveredPeer) {
		peer.LastSeen = time.Now()
	})

	// Remove peers older than 3 minutes
	cutoff := time.Now().Add(-3 * time.Minute)
	removed := registry.RemoveStale(cutoff)

	if removed != 1 {
		t.Errorf("Expected 1 peer removed, got %d", removed)
	}

	if registry.Count() != 1 {
		t.Errorf("Expected 1 peer remaining, got %d", registry.Count())
	}

	_, exists := registry.GetPeer("old")
	if exists {
		t.Error("Old peer should have been removed")
	}

	_, exists = registry.GetPeer("new")
	if !exists {
		t.Error("New peer should still exist")
	}
}

func TestPeerRegistry_GetNonexistent(t *testing.T) {
	registry := newPeerRegistry()

	_, exists := registry.GetPeer("nonexistent")
	if exists {
		t.Error("Expected nonexistent peer to not exist")
	}
}

func TestParseTXTRecord(t *testing.T) {
	tests := []struct {
		name     string
		txt      []string
		expected ServiceMetadata
	}{
		{
			name: "complete record with valid hex ID",
			txt: []string{
				"txtvers=1",
				"id=abc123",
				"model=Raspberry Pi 4",
				"name=piccolo",
				"version=0.1.0",
				"boot=1705334400",
			},
			expected: ServiceMetadata{
				TxtVersion:    1,
				MachineID:     "abc123",
				Model:         "Raspberry Pi 4",
				PreferredName: "piccolo",
				Version:       "0.1.0",
				BootTime:      time.Unix(1705334400, 0),
			},
		},
		{
			name: "partial record with valid hex ID",
			txt: []string{
				"id=def456",
				"version=2.0.0",
			},
			expected: ServiceMetadata{
				MachineID: "def456",
				Version:   "2.0.0",
			},
		},
		{
			name:     "empty record",
			txt:      []string{},
			expected: ServiceMetadata{},
		},
		{
			name: "malformed entries ignored",
			txt: []string{
				"id=a1b2c3",
				"noequals",
				"version=1.0",
			},
			expected: ServiceMetadata{
				MachineID: "a1b2c3",
				Version:   "1.0",
			},
		},
		{
			name: "case insensitive keys with valid hex ID",
			txt: []string{
				"ID=AABBCC",
				"VERSION=3.0.0",
				"Model=Test Device",
			},
			expected: ServiceMetadata{
				MachineID: "AABBCC",
				Version:   "3.0.0",
				Model:     "Test Device",
			},
		},
		{
			name: "invalid machine ID rejected - too short",
			txt: []string{
				"id=abc",
				"version=1.0.0",
			},
			expected: ServiceMetadata{
				MachineID: "", // rejected
				Version:   "1.0.0",
			},
		},
		{
			name: "invalid machine ID rejected - too long",
			txt: []string{
				"id=abc1234567",
				"version=1.0.0",
			},
			expected: ServiceMetadata{
				MachineID: "", // rejected
				Version:   "1.0.0",
			},
		},
		{
			name: "invalid machine ID rejected - non-hex chars",
			txt: []string{
				"id=abcxyz",
				"version=1.0.0",
			},
			expected: ServiceMetadata{
				MachineID: "", // rejected
				Version:   "1.0.0",
			},
		},
		{
			name: "invalid machine ID rejected - injection attempt",
			txt: []string{
				"id=abc123; DROP TABLE",
				"version=1.0.0",
			},
			expected: ServiceMetadata{
				MachineID: "", // rejected
				Version:   "1.0.0",
			},
		},
		{
			name: "long string fields truncated",
			txt: []string{
				"id=abc123",
				"model=" + strings.Repeat("x", 300),
				"name=" + strings.Repeat("y", 300),
				"version=" + strings.Repeat("z", 300),
			},
			expected: ServiceMetadata{
				MachineID:     "abc123",
				Model:         strings.Repeat("x", 256), // truncated to maxTXTFieldLen
				PreferredName: strings.Repeat("y", 256),
				Version:       strings.Repeat("z", 256),
			},
		},
		{
			name: "boot timestamp rejected - too old (pre-2000)",
			txt: []string{
				"id=abc123",
				"boot=946684700", // Jan 1, 2000 - slightly before minBootTime
			},
			expected: ServiceMetadata{
				MachineID: "abc123",
				// BootTime is zero (rejected)
			},
		},
		{
			name: "boot timestamp rejected - in future",
			txt: []string{
				"id=abc123",
				"boot=4102444800", // Jan 1, 2100
			},
			expected: ServiceMetadata{
				MachineID: "abc123",
				// BootTime is zero (rejected)
			},
		},
		{
			name: "boot timestamp rejected - negative",
			txt: []string{
				"id=abc123",
				"boot=-1000",
			},
			expected: ServiceMetadata{
				MachineID: "abc123",
				// BootTime is zero (rejected)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseTXTRecord(tc.txt)

			if result.TxtVersion != tc.expected.TxtVersion {
				t.Errorf("TxtVersion: expected %d, got %d", tc.expected.TxtVersion, result.TxtVersion)
			}
			if result.MachineID != tc.expected.MachineID {
				t.Errorf("MachineID: expected '%s', got '%s'", tc.expected.MachineID, result.MachineID)
			}
			if result.Model != tc.expected.Model {
				t.Errorf("Model: expected '%s', got '%s'", tc.expected.Model, result.Model)
			}
			if result.PreferredName != tc.expected.PreferredName {
				t.Errorf("PreferredName: expected '%s', got '%s'", tc.expected.PreferredName, result.PreferredName)
			}
			if result.Version != tc.expected.Version {
				t.Errorf("Version: expected '%s', got '%s'", tc.expected.Version, result.Version)
			}
			if !result.BootTime.Equal(tc.expected.BootTime) {
				t.Errorf("BootTime: expected %v, got %v", tc.expected.BootTime, result.BootTime)
			}
		})
	}
}

func TestPeerRegistry_MaxPeersLimit(t *testing.T) {
	registry := newPeerRegistry()

	// Fill registry to capacity
	for i := 0; i < maxPeers; i++ {
		// Generate unique machine IDs like "peer00", "peer01", etc.
		machineID := "peer" + padInt(i, 2)
		isNew := registry.UpdatePeer(machineID, func(peer *DiscoveredPeer) {
			peer.Hostname = "peer.local"
		})
		if !isNew {
			t.Errorf("Peer %d should be new", i)
		}
	}

	if registry.Count() != maxPeers {
		t.Errorf("Expected %d peers, got %d", maxPeers, registry.Count())
	}

	// Try to add one more - should be rejected
	isNew := registry.UpdatePeer("newpeer", func(peer *DiscoveredPeer) {
		peer.Hostname = "new.local"
	})

	if isNew {
		t.Error("New peer should be rejected when at capacity")
	}

	// Count should not increase
	if registry.Count() != maxPeers {
		t.Errorf("Expected %d peers after rejection, got %d", maxPeers, registry.Count())
	}

	// Updating existing peer should still work
	existingID := "peer00"
	isNew = registry.UpdatePeer(existingID, func(peer *DiscoveredPeer) {
		peer.Hostname = "updated.local"
	})

	if isNew {
		t.Error("Updating existing peer should return false")
	}

	peer, exists := registry.GetPeer(existingID)
	if !exists {
		t.Error("Existing peer should still exist")
	}
	if peer.Hostname != "updated.local" {
		t.Errorf("Expected hostname 'updated.local', got '%s'", peer.Hostname)
	}
}

// padInt returns an integer as a zero-padded string
func padInt(n, width int) string {
	s := ""
	for i := 0; i < width; i++ {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestServiceFQDN(t *testing.T) {
	manager := NewManager()

	serviceFQDN := manager.ServiceFQDN()
	expected := "_piccolo._tcp.local."

	if serviceFQDN != expected {
		t.Errorf("Expected ServiceFQDN '%s', got '%s'", expected, serviceFQDN)
	}
}

func TestInstanceFQDN(t *testing.T) {
	manager := NewManager()

	instanceFQDN := manager.InstanceFQDN()

	// Should contain service name and service type
	if !strings.Contains(instanceFQDN, "_piccolo._tcp.local.") {
		t.Errorf("InstanceFQDN '%s' should contain '_piccolo._tcp.local.'", instanceFQDN)
	}

	// Should start with the hostname
	serviceName := manager.currentServiceName()
	expected := serviceName + "._piccolo._tcp.local."
	if instanceFQDN != expected {
		t.Errorf("Expected InstanceFQDN '%s', got '%s'", expected, instanceFQDN)
	}
}

func TestBuildTXTStrings(t *testing.T) {
	manager := NewManager()
	manager.SetVersion("1.2.3")

	txtStrings := manager.buildTXTStrings()

	if len(txtStrings) == 0 {
		t.Fatal("Expected non-empty TXT strings")
	}

	// Check that required fields are present
	hasVersion := false
	hasID := false
	hasTxtvers := false

	for _, s := range txtStrings {
		if strings.Contains(s, "txtvers=") {
			hasTxtvers = true
		}
		if strings.Contains(s, "id=") {
			hasID = true
		}
		if strings.Contains(s, "version=1.2.3") {
			hasVersion = true
		}
	}

	if !hasTxtvers {
		t.Error("TXT strings missing txtvers")
	}
	if !hasID {
		t.Error("TXT strings missing id")
	}
	if !hasVersion {
		t.Error("TXT strings missing version")
	}
}

func TestManagerPeers(t *testing.T) {
	manager := NewManager()

	// Initially no peers
	peers := manager.Peers()
	if len(peers) != 0 {
		t.Errorf("Expected 0 peers initially, got %d", len(peers))
	}

	if manager.PeerCount() != 0 {
		t.Errorf("Expected PeerCount 0, got %d", manager.PeerCount())
	}

	// Add a peer through the registry
	manager.peerRegistry.UpdatePeer("test123", func(peer *DiscoveredPeer) {
		peer.Hostname = "test.local"
		peer.Model = "Test"
	})

	peers = manager.Peers()
	if len(peers) != 1 {
		t.Errorf("Expected 1 peer, got %d", len(peers))
	}

	if manager.PeerCount() != 1 {
		t.Errorf("Expected PeerCount 1, got %d", manager.PeerCount())
	}

	peer, exists := manager.GetPeer("test123")
	if !exists {
		t.Fatal("Expected peer to exist")
	}
	if peer.Hostname != "test.local" {
		t.Errorf("Expected hostname 'test.local', got '%s'", peer.Hostname)
	}
}

func TestManagerMachineID(t *testing.T) {
	manager := NewManager()

	machineID := manager.MachineID()
	if machineID == "" {
		t.Error("Expected non-empty machine ID")
	}

	// Machine ID should be consistent
	if manager.MachineID() != machineID {
		t.Error("Machine ID should be consistent")
	}
}

func TestHandlePeerDiscoveryResponse_ValidPeer(t *testing.T) {
	manager := NewManager()

	// Create a mock DNS response with PTR, SRV, TXT records
	msg := &dns.Msg{}
	msg.Response = true

	serviceFQDN := "_piccolo._tcp.local."
	instanceFQDN := "otherpeer._piccolo._tcp.local."
	hostFQDN := "otherpeer.local."

	// PTR record
	msg.Answer = append(msg.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: serviceFQDN, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 4500},
		Ptr: instanceFQDN,
	})

	// SRV record
	msg.Answer = append(msg.Answer, &dns.SRV{
		Hdr:    dns.RR_Header{Name: instanceFQDN, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 4500},
		Target: hostFQDN,
		Port:   80,
	})

	// TXT record with valid machine ID
	msg.Answer = append(msg.Answer, &dns.TXT{
		Hdr: dns.RR_Header{Name: instanceFQDN, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 4500},
		Txt: []string{"txtvers=1", "id=def456", "model=Test Device", "name=otherpeer", "version=1.0.0"},
	})

	// A record in Extra
	msg.Extra = append(msg.Extra, &dns.A{
		Hdr: dns.RR_Header{Name: hostFQDN, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120},
		A:   net.ParseIP("192.168.1.200"),
	})

	// Process the response
	clientAddr := &net.UDPAddr{IP: net.ParseIP("192.168.1.200"), Port: 5353}
	manager.handlePeerDiscoveryResponse(msg, clientAddr)

	// Verify peer was discovered
	if manager.PeerCount() != 1 {
		t.Errorf("Expected 1 peer, got %d", manager.PeerCount())
	}

	peer, exists := manager.GetPeer("def456")
	if !exists {
		t.Fatal("Expected peer to exist")
	}

	if peer.Model != "Test Device" {
		t.Errorf("Expected model 'Test Device', got '%s'", peer.Model)
	}
	if peer.Version != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got '%s'", peer.Version)
	}
	if peer.PreferredName != "otherpeer" {
		t.Errorf("Expected name 'otherpeer', got '%s'", peer.PreferredName)
	}
}

func TestHandlePeerDiscoveryResponse_InvalidMachineID(t *testing.T) {
	manager := NewManager()

	// Create a mock DNS response with invalid machine ID
	msg := &dns.Msg{}
	msg.Response = true

	serviceFQDN := "_piccolo._tcp.local."
	instanceFQDN := "badpeer._piccolo._tcp.local."

	// PTR record
	msg.Answer = append(msg.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: serviceFQDN, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 4500},
		Ptr: instanceFQDN,
	})

	// TXT record with invalid machine ID (injection attempt)
	msg.Answer = append(msg.Answer, &dns.TXT{
		Hdr: dns.RR_Header{Name: instanceFQDN, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 4500},
		Txt: []string{"id=malicious; DROP", "version=1.0.0"},
	})

	// Process the response
	clientAddr := &net.UDPAddr{IP: net.ParseIP("192.168.1.201"), Port: 5353}
	manager.handlePeerDiscoveryResponse(msg, clientAddr)

	// Verify peer was NOT discovered due to invalid machine ID
	if manager.PeerCount() != 0 {
		t.Errorf("Expected 0 peers (invalid ID rejected), got %d", manager.PeerCount())
	}
}

func TestHandlePeerDiscoveryResponse_IgnoresSelf(t *testing.T) {
	manager := NewManager()

	// Create a mock DNS response from ourselves
	msg := &dns.Msg{}
	msg.Response = true

	serviceFQDN := "_piccolo._tcp.local."
	instanceFQDN := manager.InstanceFQDN()

	// PTR record
	msg.Answer = append(msg.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: serviceFQDN, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 4500},
		Ptr: instanceFQDN,
	})

	// TXT record with our own machine ID
	msg.Answer = append(msg.Answer, &dns.TXT{
		Hdr: dns.RR_Header{Name: instanceFQDN, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 4500},
		Txt: []string{"id=" + manager.MachineID(), "version=1.0.0"},
	})

	// Process the response (simulating receiving our own multicast)
	clientAddr := &net.UDPAddr{IP: net.ParseIP("192.168.1.100"), Port: 5353}
	manager.handlePeerDiscoveryResponse(msg, clientAddr)

	// Verify we didn't add ourselves as a peer
	if manager.PeerCount() != 0 {
		t.Errorf("Expected 0 peers (should ignore self), got %d", manager.PeerCount())
	}
}
