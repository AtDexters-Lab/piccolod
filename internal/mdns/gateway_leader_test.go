package mdns

import (
	"sync"
	"testing"
	"time"
)

func TestGatewayLeader_InitialState(t *testing.T) {
	leader := NewGatewayLeader("abc123")
	if leader.State() != LeadershipUnknown {
		t.Errorf("expected initial state LeadershipUnknown, got %v", leader.State())
	}
	if leader.IsLeader() {
		t.Error("expected IsLeader() to be false initially")
	}
}

func TestGatewayLeader_ClaimWithoutPeers(t *testing.T) {
	leader := NewGatewayLeader("abc123")
	leader.SetPeersFunc(func() []DiscoveredPeer { return nil })

	var callbackCalled bool
	var callbackIsLeader bool
	var wg sync.WaitGroup
	wg.Add(1)
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		callbackCalled = true
		callbackIsLeader = isLeader
		wg.Done()
	})

	leader.Start()

	// Wait for the claim delay plus some buffer
	wg.Wait()

	if !leader.IsLeader() {
		t.Error("expected to become leader with no peers")
	}
	if leader.State() != LeadershipClaimed {
		t.Errorf("expected state LeadershipClaimed, got %v", leader.State())
	}
	if !callbackCalled {
		t.Error("expected callback to be called")
	}
	if !callbackIsLeader {
		t.Error("expected callback isLeader to be true")
	}

	leader.Stop()
}

func TestGatewayLeader_DeferToLowerID(t *testing.T) {
	leader := NewGatewayLeader("def456")

	// Peer with lower ID
	peers := []DiscoveredPeer{
		{MachineID: "abc123", LastSeen: time.Now()},
	}
	leader.SetPeersFunc(func() []DiscoveredPeer { return peers })

	var callbackCalled bool
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		callbackCalled = true
	})

	leader.Start()

	// Wait for the claim delay
	time.Sleep(LeadershipClaimDelay + 100*time.Millisecond)

	if leader.IsLeader() {
		t.Error("expected not to be leader when peer has lower ID")
	}
	if leader.State() != LeadershipDeferred {
		t.Errorf("expected state LeadershipDeferred, got %v", leader.State())
	}
	if callbackCalled {
		t.Error("expected no callback when deferring")
	}

	leader.Stop()
}

func TestGatewayLeader_OnPeerDiscovered_CancelClaim(t *testing.T) {
	leader := NewGatewayLeader("def456")
	leader.SetPeersFunc(func() []DiscoveredPeer { return nil })

	leader.Start()

	// Discover peer with lower ID before claim delay expires
	time.Sleep(500 * time.Millisecond)
	leader.OnPeerDiscovered(DiscoveredPeer{MachineID: "abc123", LastSeen: time.Now()})

	// Wait past the original claim delay
	time.Sleep(LeadershipClaimDelay)

	if leader.IsLeader() {
		t.Error("expected claim to be cancelled when lower ID peer discovered")
	}
	if leader.State() != LeadershipDeferred {
		t.Errorf("expected state LeadershipDeferred, got %v", leader.State())
	}

	leader.Stop()
}

func TestGatewayLeader_OnPeerDiscovered_YieldLeadership(t *testing.T) {
	leader := NewGatewayLeader("def456")
	leader.SetPeersFunc(func() []DiscoveredPeer { return nil })

	var yieldCallbackCalled bool
	var wg sync.WaitGroup
	wg.Add(1)
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		if !isLeader {
			yieldCallbackCalled = true
		}
		wg.Done()
	})

	leader.Start()

	// Wait to become leader
	wg.Wait()

	if !leader.IsLeader() {
		t.Error("expected to be leader initially")
	}

	// Add another wait for yield callback
	wg.Add(1)

	// Discover peer with lower ID after becoming leader
	leader.OnPeerDiscovered(DiscoveredPeer{MachineID: "abc123", LastSeen: time.Now()})

	// Wait for yield callback
	wg.Wait()

	if leader.IsLeader() {
		t.Error("expected to yield leadership to lower ID peer")
	}
	if !yieldCallbackCalled {
		t.Error("expected yield callback to be called")
	}

	leader.Stop()
}

func TestGatewayLeader_OnPeerGoodbye_Reevaluate(t *testing.T) {
	leader := NewGatewayLeader("def456")

	// Initially peer with lower ID exists
	peers := []DiscoveredPeer{
		{MachineID: "abc123", LastSeen: time.Now()},
	}
	var peersLock sync.Mutex
	leader.SetPeersFunc(func() []DiscoveredPeer {
		peersLock.Lock()
		defer peersLock.Unlock()
		return peers
	})

	var wg sync.WaitGroup
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		wg.Done()
	})

	leader.Start()

	// Wait for claim delay - should be deferred
	time.Sleep(LeadershipClaimDelay + 100*time.Millisecond)

	if leader.IsLeader() {
		t.Error("expected to defer initially")
	}

	// Peer says goodbye (simulate by clearing peers list)
	peersLock.Lock()
	peers = nil
	peersLock.Unlock()

	wg.Add(1)
	leader.OnPeerGoodbye("abc123")

	// Wait for re-evaluation
	wg.Wait()

	if !leader.IsLeader() {
		t.Error("expected to claim leadership after peer goodbye")
	}

	leader.Stop()
}

func TestGatewayLeader_IgnoreOfflinePeers(t *testing.T) {
	leader := NewGatewayLeader("def456")

	// Peer with lower ID but stale (offline)
	peers := []DiscoveredPeer{
		{MachineID: "abc123", LastSeen: time.Now().Add(-PeerOnlineThreshold - time.Second)},
	}
	leader.SetPeersFunc(func() []DiscoveredPeer { return peers })

	var wg sync.WaitGroup
	wg.Add(1)
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		wg.Done()
	})

	leader.Start()
	wg.Wait()

	if !leader.IsLeader() {
		t.Error("expected to become leader when peer is offline/stale")
	}

	leader.Stop()
}

func TestGatewayLeader_HigherIDPeerIgnored(t *testing.T) {
	leader := NewGatewayLeader("abc123")
	leader.SetPeersFunc(func() []DiscoveredPeer { return nil })

	var wg sync.WaitGroup
	wg.Add(1)
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		wg.Done()
	})

	leader.Start()
	wg.Wait()

	if !leader.IsLeader() {
		t.Error("expected to be leader initially")
	}

	// Discover peer with higher ID - should remain leader
	leader.OnPeerDiscovered(DiscoveredPeer{MachineID: "xyz789", LastSeen: time.Now()})

	// Give time for any potential state change
	time.Sleep(100 * time.Millisecond)

	if !leader.IsLeader() {
		t.Error("expected to remain leader when higher ID peer discovered")
	}

	leader.Stop()
}

func TestGatewayLeader_Stop(t *testing.T) {
	leader := NewGatewayLeader("abc123")
	leader.SetPeersFunc(func() []DiscoveredPeer { return nil })

	leader.Start()
	time.Sleep(100 * time.Millisecond)

	leader.Stop()

	// Shouldn't claim leadership after stop
	time.Sleep(LeadershipClaimDelay + 100*time.Millisecond)

	if leader.IsLeader() {
		t.Error("expected not to claim leadership after stop")
	}

	// Safe to call Stop again
	leader.Stop()
}

func TestLeadershipState_String(t *testing.T) {
	tests := []struct {
		state    LeadershipState
		expected string
	}{
		{LeadershipUnknown, "unknown"},
		{LeadershipClaimed, "claimed"},
		{LeadershipDeferred, "deferred"},
		{LeadershipState(99), "invalid"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("LeadershipState(%d).String() = %v, want %v", tt.state, got, tt.expected)
		}
	}
}

func TestGatewayLeader_ConcurrentPeerDiscovery(t *testing.T) {
	// Test that concurrent OnPeerDiscovered calls with different machine IDs
	// are handled correctly under mutex contention
	leader := NewGatewayLeader("mmm000") // Middle of the alphabet

	var peers []DiscoveredPeer
	var peersLock sync.Mutex
	leader.SetPeersFunc(func() []DiscoveredPeer {
		peersLock.Lock()
		defer peersLock.Unlock()
		result := make([]DiscoveredPeer, len(peers))
		copy(result, peers)
		return result
	})

	var transitionCount int
	var transitionLock sync.Mutex
	leader.SetLeadershipChangeCallback(func(isLeader bool) {
		transitionLock.Lock()
		transitionCount++
		transitionLock.Unlock()
	})

	leader.Start()

	// Wait for initial claim attempt
	time.Sleep(LeadershipClaimDelay + 100*time.Millisecond)

	// Now concurrently discover multiple peers with different IDs
	var wg sync.WaitGroup
	peerIDs := []string{"aaa111", "bbb222", "zzz999", "ccc333", "ddd444"}

	for _, id := range peerIDs {
		wg.Add(1)
		go func(machineID string) {
			defer wg.Done()
			peer := DiscoveredPeer{MachineID: machineID, LastSeen: time.Now()}

			// Add to peers list
			peersLock.Lock()
			peers = append(peers, peer)
			peersLock.Unlock()

			// Notify leader
			leader.OnPeerDiscovered(peer)
		}(id)
	}

	wg.Wait()

	// Give time for all state transitions to complete
	time.Sleep(100 * time.Millisecond)

	// Leader should have deferred since "aaa111" < "mmm000"
	if leader.IsLeader() {
		t.Error("expected to defer to peer with lower ID (aaa111)")
	}
	if leader.State() != LeadershipDeferred {
		t.Errorf("expected state LeadershipDeferred, got %v", leader.State())
	}

	leader.Stop()
}
