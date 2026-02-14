package mdns

import (
	"log"
	"sync"
	"time"
)

const (
	// LeadershipClaimDelay is the time to wait for peer discovery before claiming leadership.
	// This balances peer discovery time vs startup latency.
	LeadershipClaimDelay = 2 * time.Second

	// PeerOnlineThreshold is the time after which a peer is considered offline.
	// Matches the existing stale threshold (3x 60s discovery interval).
	PeerOnlineThreshold = 180 * time.Second

	// LeaderHeartbeatInterval is the service announcement frequency when this device is leader.
	// Faster than the default 60s to allow followers to detect leader absence quickly.
	LeaderHeartbeatInterval = 10 * time.Second

	// LeaderProbeTimeout is the time a deferred peer waits before considering the leader gone.
	// 3 missed heartbeats at LeaderHeartbeatInterval.
	LeaderProbeTimeout = 3 * LeaderHeartbeatInterval

	// ReEvalInterval is how often a deferred peer re-evaluates leadership.
	// Matches the leader heartbeat so we check once per expected heartbeat.
	ReEvalInterval = LeaderHeartbeatInterval

	// GatewayHostname is the gateway domain that the leader serves.
	GatewayHostname = "piccolo.local"
)

// LeadershipState represents the current state of gateway leadership.
type LeadershipState int

const (
	// LeadershipUnknown is the startup state, waiting for discovery.
	LeadershipUnknown LeadershipState = iota
	// LeadershipClaimed means we are serving piccolo.local.
	LeadershipClaimed
	// LeadershipDeferred means another device with lower ID is the leader.
	LeadershipDeferred
)

// String returns a human-readable representation of the leadership state.
func (s LeadershipState) String() string {
	switch s {
	case LeadershipUnknown:
		return "unknown"
	case LeadershipClaimed:
		return "claimed"
	case LeadershipDeferred:
		return "deferred"
	default:
		return "invalid"
	}
}

// GatewayLeader manages the state machine for gateway leadership.
// Leadership is determined by the lowest machine ID among online peers.
type GatewayLeader struct {
	mu            sync.RWMutex
	selfMachineID string
	state         LeadershipState
	claimTimer    *time.Timer
	stopCh        chan struct{}
	stopped       bool
	wg            sync.WaitGroup

	// Re-evaluation timer for deferred peers
	reEvalActive bool
	reEvalStopCh chan struct{}

	// getPeers returns a copy of online peers for leadership evaluation.
	// This is set by the Manager during initialization.
	// LOCK ORDERING: This function may acquire PeerRegistry.mu. Callers
	// holding GatewayLeader.mu must ensure no reverse ordering exists.
	getPeers func() []DiscoveredPeer

	// onLeadershipChange is called when leadership state changes.
	// The callback receives true if we became the leader, false if we yielded.
	onLeadershipChange func(isLeader bool)
}

// NewGatewayLeader creates a new GatewayLeader with the given machine ID.
func NewGatewayLeader(selfMachineID string) *GatewayLeader {
	return &GatewayLeader{
		selfMachineID: selfMachineID,
		state:         LeadershipUnknown,
		stopCh:        make(chan struct{}),
	}
}

// SetPeersFunc sets the function used to get online peers.
func (g *GatewayLeader) SetPeersFunc(f func() []DiscoveredPeer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.getPeers = f
}

// SetLeadershipChangeCallback sets the callback for leadership changes.
// IMPORTANT: This must be called before Start() to ensure the callback is
// properly registered before any leadership transitions occur. The callback
// is invoked asynchronously (in a goroutine) to avoid deadlocks when the
// callback needs to acquire other locks.
func (g *GatewayLeader) SetLeadershipChangeCallback(f func(isLeader bool)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onLeadershipChange = f
}

// Start begins the leadership claim process.
// It schedules a leadership evaluation after LeadershipClaimDelay.
func (g *GatewayLeader) Start() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopped {
		return
	}

	g.state = LeadershipUnknown
	log.Printf("INFO: Gateway leader starting, will evaluate leadership in %v", LeadershipClaimDelay)

	// Schedule leadership claim attempt after delay
	g.claimTimer = time.AfterFunc(LeadershipClaimDelay, func() {
		g.evaluateLeadership()
	})
}

// Stop cancels any pending claim and stops the leader.
func (g *GatewayLeader) Stop() {
	g.mu.Lock()

	if g.stopped {
		g.mu.Unlock()
		return
	}
	g.stopped = true

	g.stopReEvalLocked()
	close(g.stopCh)

	if g.claimTimer != nil {
		g.claimTimer.Stop()
		g.claimTimer = nil
	}

	// If we were leader, yield
	if g.state == LeadershipClaimed {
		g.state = LeadershipUnknown
		log.Printf("INFO: Gateway leader stopped, yielding leadership")
	}

	g.mu.Unlock()

	// Wait for reEvalLoop goroutine to exit (outside lock since it acquires g.mu)
	g.wg.Wait()
}

// OnPeerDiscovered is called when a new peer is discovered.
// It re-evaluates leadership if the new peer has a lower machine ID.
func (g *GatewayLeader) OnPeerDiscovered(peer DiscoveredPeer) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopped {
		return
	}

	// If we discover a peer with lower ID before claiming, cancel our claim
	if g.state == LeadershipUnknown && peer.MachineID < g.selfMachineID {
		if g.claimTimer != nil {
			g.claimTimer.Stop()
			g.claimTimer = nil
		}
		g.state = LeadershipDeferred
		g.startReEvalLocked()
		log.Printf("INFO: Gateway leader deferring to peer %s (lower ID than %s)", peer.MachineID, g.selfMachineID)
		return
	}

	// If we're leader and discover lower ID peer, yield
	if g.state == LeadershipClaimed && peer.MachineID < g.selfMachineID {
		g.yieldLeadershipLocked()
	}
}

// OnPeerGoodbye is called when a peer sends a goodbye announcement.
// If the leader departed, trigger immediate re-evaluation.
func (g *GatewayLeader) OnPeerGoodbye(machineID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopped {
		return
	}

	// If we're deferred and lost a peer, re-evaluate immediately
	if g.state == LeadershipDeferred {
		log.Printf("INFO: Gateway leader re-evaluating after peer %s goodbye", machineID)
		// Schedule immediate evaluation (async to avoid lock contention).
		// Tracked by WaitGroup so Stop() waits for completion.
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			g.evaluateLeadership()
		}()
	}
}

// OnPeerTimeout is called when a peer is considered offline (stale).
// Similar to goodbye but triggered by timeout rather than explicit goodbye.
func (g *GatewayLeader) OnPeerTimeout(machineID string) {
	g.OnPeerGoodbye(machineID) // Same behavior
}

// evaluateLeadership determines if we should be the leader.
// Called after the claim delay or when peer state changes.
func (g *GatewayLeader) evaluateLeadership() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopped {
		return
	}

	shouldLead := g.shouldBeLeaderLocked()

	if shouldLead && g.state != LeadershipClaimed {
		g.claimLeadershipLocked()
	} else if !shouldLead && g.state == LeadershipClaimed {
		g.yieldLeadershipLocked()
	} else if !shouldLead && g.state == LeadershipUnknown {
		g.state = LeadershipDeferred
		g.startReEvalLocked()
		log.Printf("INFO: Gateway leader deferred (found peer with lower ID)")
	}
	// !shouldLead && state == LeadershipDeferred: no change needed, re-eval continues
}

// shouldBeLeaderLocked returns true if we should be the gateway leader.
// We are leader if we have the lowest machine ID among all online peers.
// Must be called with lock held.
func (g *GatewayLeader) shouldBeLeaderLocked() bool {
	if g.getPeers == nil {
		// No peer function set, assume we're the only device
		return true
	}

	peers := g.getPeers()
	now := time.Now()

	// When deferred (actively monitoring a known leader), use the aggressive
	// probe timeout. Otherwise use the conservative online threshold.
	threshold := PeerOnlineThreshold
	if g.state == LeadershipDeferred {
		threshold = LeaderProbeTimeout
	}

	for _, p := range peers {
		// Only consider online peers
		if now.Sub(p.LastSeen) >= threshold {
			continue
		}
		if p.MachineID < g.selfMachineID {
			return false // Someone else should lead
		}
	}

	return true // We have lowest ID (or no online peers)
}

// claimLeadershipLocked claims gateway leadership.
// Must be called with lock held.
func (g *GatewayLeader) claimLeadershipLocked() {
	g.stopReEvalLocked()
	g.state = LeadershipClaimed
	log.Printf("INFO: Gateway leader claimed by %s", g.selfMachineID)

	if g.onLeadershipChange != nil {
		// Call callback outside lock to avoid deadlock
		callback := g.onLeadershipChange
		go callback(true)
	}
}

// yieldLeadershipLocked yields gateway leadership.
// Must be called with lock held.
func (g *GatewayLeader) yieldLeadershipLocked() {
	g.state = LeadershipDeferred
	g.startReEvalLocked()
	log.Printf("INFO: Gateway leader yielded by %s", g.selfMachineID)

	if g.onLeadershipChange != nil {
		// Call callback outside lock to avoid deadlock
		callback := g.onLeadershipChange
		go callback(false)
	}
}

// startReEvalLocked starts the periodic re-evaluation timer for deferred peers.
// Must be called with lock held.
func (g *GatewayLeader) startReEvalLocked() {
	if g.reEvalActive {
		return
	}
	g.reEvalActive = true
	g.reEvalStopCh = make(chan struct{})
	log.Printf("INFO: Starting leader re-evaluation timer (self: %s)", g.selfMachineID)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.reEvalLoop(g.reEvalStopCh)
	}()
}

// stopReEvalLocked stops the periodic re-evaluation timer.
// Must be called with lock held.
func (g *GatewayLeader) stopReEvalLocked() {
	if !g.reEvalActive || g.reEvalStopCh == nil {
		return
	}
	close(g.reEvalStopCh)
	g.reEvalStopCh = nil
	g.reEvalActive = false
	log.Printf("INFO: Stopped leader re-evaluation timer (self: %s)", g.selfMachineID)
}

// reEvalLoop periodically calls evaluateLeadership until stopped.
func (g *GatewayLeader) reEvalLoop(stopCh chan struct{}) {
	ticker := time.NewTicker(ReEvalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-stopCh:
			return
		case <-ticker.C:
			g.evaluateLeadership()
		}
	}
}

// IsLeader returns true if this device is the gateway leader.
func (g *GatewayLeader) IsLeader() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.state == LeadershipClaimed
}

// State returns the current leadership state.
func (g *GatewayLeader) State() LeadershipState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.state
}

// SelfMachineID returns the machine ID of this device.
func (g *GatewayLeader) SelfMachineID() string {
	return g.selfMachineID // Immutable, no lock needed
}
