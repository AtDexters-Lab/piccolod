package network

import (
	"context"
	"log"
	"sync"
	"time"

	"piccolod/internal/events"
)

// stateMachine manages the three-tier connectivity priority stack:
// Ethernet > WiFi STA > AP Mode.
//
// Transitions are serialized through mu. Long-running transitions (STA
// validation, periodic AP reconnection) use a cancellable staCtx so that
// Ethernet can preempt them immediately.
type stateMachine struct {
	mu      sync.Mutex
	current ConnState
	uplink  UplinkType

	// Preemption: long transitions set cancelSTA; Ethernet calls it.
	staCtx    context.Context
	cancelSTA context.CancelFunc

	// WiFi reconnection backoff
	backoffIdx      int // index into backoffSchedule
	ceilingFailures int // consecutive failures at backoff ceiling

	// Signal quality
	lastSignalDBm  int
	lastSignalTier SignalTier

	// Callbacks (set before Start, read under sm.mu)
	//
	// Lock ordering: sm.mu → Manager.mu (RLock). The wifiConnected callback
	// acquires Manager.mu.RLock while sm.mu is held. No code path acquires
	// these in reverse order.
	onTransition  func(newState ConnState) // called after state change (for health tracker)
	wifiConnected func() bool             // queries whether WiFi STA is currently active

	// Dependencies
	events *events.Bus
}

var backoffSchedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	60 * time.Second, // ceiling
}

const (
	ceilingFailuresForAP = 5
	ethDebounceDuration  = 5 * time.Second
)

func newStateMachine(bus *events.Bus) *stateMachine {
	return &stateMachine{
		current: StateDisconnected,
		uplink:  UplinkNone,
		events:  bus,
	}
}

// Current returns the current connectivity state (thread-safe).
func (sm *stateMachine) Current() ConnState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.current
}

// Uplink returns the current active uplink type (thread-safe).
func (sm *stateMachine) Uplink() UplinkType {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.uplink
}

// transition updates the state and publishes an event. Caller must hold mu.
func (sm *stateMachine) transition(newState ConnState, newUplink UplinkType) {
	old := sm.current
	if old == newState {
		return
	}
	sm.current = newState
	sm.uplink = newUplink

	// Clear stale signal readings when leaving WiFi STA.
	if old == StateWiFiSTA && newState != StateWiFiSTA {
		sm.lastSignalDBm = 0
		sm.lastSignalTier = ""
	}

	log.Printf("INFO: network: state %s → %s (uplink=%s)", old, newState, newUplink)

	if sm.onTransition != nil {
		sm.onTransition(newState)
	}

	if sm.events != nil {
		sm.events.Publish(events.Event{
			Topic: events.TopicNetworkStateChanged,
			Payload: NetworkStateChangedEvent{
				State:        string(newState),
				ActiveUplink: string(newUplink),
			},
		})
	}
}

// handleEthernetUp is called when Ethernet link-up + DHCP lease is detected.
// It preempts any in-progress STA transition by cancelling staCtx.
func (sm *stateMachine) handleEthernetUp() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Cancel any in-progress STA validation
	if sm.cancelSTA != nil {
		sm.cancelSTA()
		sm.cancelSTA = nil
	}

	sm.transition(StateEthernet, UplinkEthernet)
	sm.resetBackoff()
}

// handleEthernetDown is called when Ethernet is lost. hasSavedWifi is
// queried from NM by the caller — not cached in the state machine.
func (sm *stateMachine) handleEthernetDown(hasSavedWifi bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current != StateEthernet {
		return // not relevant
	}

	// If WiFi STA is already active (NM auto-connected a saved profile while
	// Ethernet was primary), promote it directly — no reconnection needed.
	if hasSavedWifi && sm.wifiConnected != nil && sm.wifiConnected() {
		sm.transition(StateWiFiSTA, UplinkWiFi)
		return
	}

	if hasSavedWifi {
		// NM will auto-connect the saved profile, but association + DHCP
		// takes seconds. Report Reconnecting until the D-Bus DeviceStateChange
		// signal confirms WiFi is actually activated.
		sm.transition(StateReconnecting, UplinkNone)
	} else {
		sm.transition(StateAPMode, UplinkNone)
	}
}

// handleWiFiConnected is called when WiFi STA successfully connects.
func (sm *stateMachine) handleWiFiConnected() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == StateEthernet {
		return // Ethernet takes priority
	}

	sm.transition(StateWiFiSTA, UplinkWiFi)
	sm.resetBackoff()
}

// handleWiFiDisconnected is called when WiFi STA loses association.
// hasSavedWifi is queried from NM by the caller.
func (sm *stateMachine) handleWiFiDisconnected(hasSavedWifi bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current != StateWiFiSTA {
		return
	}

	// Guard: if a long transition (Connect or watchdog reconnection) is in
	// progress, the disconnect is expected — NM is switching from the old
	// network to the new one. Don't escalate to AP mode regardless of
	// hasSavedWifi; Connect() will set the final state on success or
	// restore the old profile on failure.
	if sm.cancelSTA != nil {
		sm.transition(StateReconnecting, UplinkNone)
		return
	}

	if hasSavedWifi {
		sm.transition(StateReconnecting, UplinkNone)
	} else {
		// Profile was deleted externally (nmcli, NM auto-cleanup) — no
		// point reconnecting. AP mode lets the user reconfigure.
		sm.transition(StateAPMode, UplinkNone)
	}
}

// handleReconnectResult is called after a transient reconnection failure
// (DHCP timeout, signal loss, etc.). Advances backoff and eventually
// escalates to AP mode after repeated failures at the ceiling.
func (sm *stateMachine) handleReconnectResult(success bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Only process during active reconnection
	if sm.current != StateReconnecting && sm.current != StateDisconnected {
		return
	}
	// Skip during Connect() — it manages its own success/failure lifecycle
	if sm.cancelSTA != nil {
		return
	}

	if success {
		sm.transition(StateWiFiSTA, UplinkWiFi)
		sm.resetBackoff()
		return
	}

	// Advance backoff
	if sm.backoffIdx < len(backoffSchedule)-1 {
		sm.backoffIdx++
	}

	atCeiling := sm.backoffIdx == len(backoffSchedule)-1
	if atCeiling {
		sm.ceilingFailures++
		if sm.current == StateReconnecting {
			sm.transition(StateDisconnected, UplinkNone)
		}
		if sm.ceilingFailures >= ceilingFailuresForAP {
			sm.transition(StateAPMode, UplinkNone)
		}
	}
}

// handleAuthFailure is called when WiFi reconnection fails with a
// deterministic auth error (wrong password, no secrets). Retrying with
// the same saved credentials will always fail, so skip backoff and
// enter AP mode immediately for user reconfiguration.
func (sm *stateMachine) handleAuthFailure() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current != StateReconnecting && sm.current != StateDisconnected {
		return
	}
	if sm.cancelSTA != nil {
		return
	}

	log.Printf("INFO: network: WiFi auth failure — credentials invalid, entering AP mode")
	sm.transition(StateAPMode, UplinkNone)
	sm.resetBackoff()
}

// nextBackoff returns the current backoff duration.
func (sm *stateMachine) nextBackoff() time.Duration {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.backoffIdx >= len(backoffSchedule) {
		return backoffSchedule[len(backoffSchedule)-1]
	}
	return backoffSchedule[sm.backoffIdx]
}

// resetBackoff resets the reconnection backoff. Caller must hold mu.
func (sm *stateMachine) resetBackoff() {
	sm.backoffIdx = 0
	sm.ceilingFailures = 0
}

// beginLongTransition sets up a cancellable context for a long-running
// transition (STA validation, periodic reconnection). Returns false if
// another long transition is already in progress.
func (sm *stateMachine) beginLongTransition() (context.Context, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.cancelSTA != nil {
		return nil, false // another transition is in progress
	}

	ctx, cancel := context.WithCancel(context.Background())
	sm.staCtx = ctx
	sm.cancelSTA = cancel
	return ctx, true
}

// endLongTransition clears the cancellation context. Safe to call even if
// the transition was preempted (cancelSTA already called by Ethernet).
func (sm *stateMachine) endLongTransition() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.cancelSTA != nil {
		sm.cancelSTA()
	}
	sm.staCtx = nil
	sm.cancelSTA = nil
}

// updateSignal stores the latest signal quality reading.
func (sm *stateMachine) updateSignal(dbm int) {
	tier := ClassifySignal(dbm)

	sm.mu.Lock()
	oldTier := sm.lastSignalTier
	sm.lastSignalDBm = dbm
	sm.lastSignalTier = tier
	state := sm.current
	uplink := sm.uplink
	sm.mu.Unlock()

	if tier != oldTier && sm.events != nil {
		sm.events.Publish(events.Event{
			Topic: events.TopicNetworkStateChanged,
			Payload: NetworkStateChangedEvent{
				State:        string(state),
				ActiveUplink: string(uplink),
				SignalDBm:    &dbm,
				SignalTier:   string(tier),
			},
		})
	}
}

// snapshot returns a consistent copy of the state for API responses.
func (sm *stateMachine) snapshot() (ConnState, UplinkType, int, SignalTier) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.current, sm.uplink, sm.lastSignalDBm, sm.lastSignalTier
}

// NetworkStateChangedEvent is the payload for TopicNetworkStateChanged.
type NetworkStateChangedEvent struct {
	State        string `json:"state"`
	ActiveUplink string `json:"active_uplink"`
	SSID         string `json:"ssid,omitempty"`
	SignalDBm    *int   `json:"signal_dbm,omitempty"`
	SignalTier   string `json:"signal_tier,omitempty"`
	APActive     bool   `json:"ap_active"`
	APSSID       string `json:"ap_ssid,omitempty"`
	Error        string `json:"error,omitempty"`
}
