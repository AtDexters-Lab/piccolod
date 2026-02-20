package mdns

import (
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxPeers limits the number of peers in the registry to prevent resource exhaustion.
	maxPeers = 100
)

// machineIDPattern validates machine ID format (6 hex characters).
var machineIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

// ServiceMetadata contains parsed TXT record data from a Piccolo service.
type ServiceMetadata struct {
	TxtVersion    int
	MachineID     string
	Model         string
	PreferredName string
	Version       string
	BootTime      time.Time
}

// DiscoveredPeer represents a discovered Piccolo device on the network.
type DiscoveredPeer struct {
	// Hostname is the mDNS hostname without the .local suffix (e.g., "piccolo-abc123").
	// This differs from Manager.Hostname() which returns the full hostname with suffix.
	Hostname      string
	IPv4          net.IP    // IPv4 address if available
	IPv6          net.IP    // IPv6 address if available
	MachineID     string    // abc123 (6 hex chars)
	Model         string    // "Raspberry Pi 4 Model B"
	PreferredName string    // "piccolo" (before conflict resolution)
	Version       string    // "0.1.0"
	BootTime      time.Time // Current uptime start
	FirstSeen     time.Time
	LastSeen      time.Time
}

// PeerRegistry manages discovered peers in memory.
type PeerRegistry struct {
	peers map[string]*DiscoveredPeer // keyed by MachineID
	mu    sync.RWMutex
}

// newPeerRegistry creates a new peer registry.
func newPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		peers: make(map[string]*DiscoveredPeer),
	}
}

// UpdatePeer adds or updates a peer in the registry.
// Returns true if this is a new peer, false if updated or rejected.
// New peers are rejected if the registry is at capacity (maxPeers).
func (r *PeerRegistry) UpdatePeer(machineID string, update func(*DiscoveredPeer)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	peer, exists := r.peers[machineID]
	if !exists {
		// Reject new peers if at capacity to prevent resource exhaustion
		if len(r.peers) >= maxPeers {
			return false
		}
		peer = &DiscoveredPeer{
			MachineID: machineID,
			FirstSeen: time.Now(),
		}
		r.peers[machineID] = peer
	}

	peer.LastSeen = time.Now()
	update(peer)

	return !exists
}

// GetPeer returns a copy of a peer by machine ID.
func (r *PeerRegistry) GetPeer(machineID string) (DiscoveredPeer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peer, exists := r.peers[machineID]
	if !exists {
		return DiscoveredPeer{}, false
	}

	// Return a copy to avoid race conditions
	return *peer, true
}

// GetPeerByHostname returns a copy of a peer by hostname (case-insensitive).
// Used for PTR goodbye handling where only hostname is available.
func (r *PeerRegistry) GetPeerByHostname(hostname string) (DiscoveredPeer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	for _, peer := range r.peers {
		peerHost := strings.ToLower(strings.TrimSuffix(peer.Hostname, "."))
		if peerHost == hostname {
			return *peer, true
		}
	}

	return DiscoveredPeer{}, false
}

// List returns a copy of all peers sorted by hostname.
func (r *PeerRegistry) List() []DiscoveredPeer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peers := make([]DiscoveredPeer, 0, len(r.peers))
	for _, peer := range r.peers {
		peers = append(peers, *peer)
	}

	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Hostname < peers[j].Hostname
	})

	return peers
}

// Count returns the number of peers.
func (r *PeerRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// RemovePeer removes a specific peer by machine ID.
// Returns true if the peer existed and was removed.
func (r *PeerRegistry) RemovePeer(machineID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, exists := r.peers[machineID]
	if exists {
		delete(r.peers, machineID)
	}
	return exists
}

// RemoveStale removes peers not seen since the given cutoff time.
// Returns the number of removed peers.
func (r *PeerRegistry) RemoveStale(cutoff time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	removed := 0
	for id, peer := range r.peers {
		if peer.LastSeen.Before(cutoff) {
			delete(r.peers, id)
			removed++
		}
	}

	return removed
}

// Peers returns a copy of all discovered peers (public API).
func (m *Manager) Peers() []DiscoveredPeer {
	if m.peerRegistry == nil {
		return nil
	}
	return m.peerRegistry.List()
}

// PeerCount returns the number of discovered peers.
func (m *Manager) PeerCount() int {
	if m.peerRegistry == nil {
		return 0
	}
	return m.peerRegistry.Count()
}

// GetPeer returns a specific peer by machine ID.
func (m *Manager) GetPeer(machineID string) (DiscoveredPeer, bool) {
	if m.peerRegistry == nil {
		return DiscoveredPeer{}, false
	}
	return m.peerRegistry.GetPeer(machineID)
}

// GetPeerByHostname returns a specific peer by hostname.
func (m *Manager) GetPeerByHostname(hostname string) (DiscoveredPeer, bool) {
	if m.peerRegistry == nil {
		return DiscoveredPeer{}, false
	}
	return m.peerRegistry.GetPeerByHostname(hostname)
}

// MachineID returns the local machine's ID.
func (m *Manager) MachineID() string {
	return m.machineID
}
