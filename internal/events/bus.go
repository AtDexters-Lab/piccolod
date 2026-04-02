package events

import (
	"log"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
)

// Topic enumerates bus channels shared across piccolod subsystems.
type Topic string

const (
	TopicLockStateChanged        Topic = "lock_state_changed"
	TopicLeadershipRoleChanged   Topic = "leadership_role_changed"
	TopicDeviceEvent   Topic = "device_event"
	TopicControlHealth Topic = "control_health"
	TopicControlStoreCommit      Topic = "control_store_commit"
	TopicRemoteConfigChanged     Topic = "remote_config_changed"
	TopicVolumeStateChanged      Topic = "volume_state_changed"
	TopicAudit                   Topic = "audit"
	TopicServiceEndpointsChanged Topic = "service_endpoints_changed"

	// Certificate and listener health events (RFC 20260125)
	TopicCertificateChanged    Topic = "certificate_changed"     // Emitted by remote manager on cert status change
	TopicListenerHealthChanged Topic = "listener_health_changed" // Emitted by health aggregator on health change

	// Hostname events
	TopicHostnamesChanged Topic = "hostnames_changed" // Emitted by mDNS manager when advertised hostname set changes

	// App status events
	TopicAppStatusChanged Topic = "app_status_changed" // Emitted by app manager on status change

	// Storage events
	TopicStoragePhase1Complete  Topic = "storage_phase1_complete"  // Phase 1 disk prep finished
	TopicStorageEmergency       Topic = "storage_emergency"        // Storage entered emergency mode
	TopicStoragePoolInitialized Topic = "storage_pool_initialized" // LVM data pool initialized
	TopicStoragePoolActivated   Topic = "storage_pool_activated"   // LVM data pool activated
	TopicStorageLocked          Topic = "storage_locked"            // Storage locked (LVM deactivated)

	// PCV export events
	TopicPCVExportPublished Topic = "pcv_export_published" // PCV archive published successfully
	TopicPCVExportFailed    Topic = "pcv_export_failed"    // PCV archive publish failed

	// Network peer events
	TopicNetworkPeersChanged Topic = "network_peers_changed" // Emitted by mDNS when peer list changes

	// Onboarding events
	TopicOnboardingStateChanged Topic = "onboarding_state_changed" // Emitted when onboarding state changes

	// Identity events (RFC 20260312: namek-managed remote access)
	TopicIdentityReady   Topic = "identity.ready"   // Emitted when identity service finishes initialization
	TopicIdentityChanged Topic = "identity.changed" // Emitted on enrollment, enable/disable, hostname change

	// Network state events (WiFi uplink + watchdog)
	TopicNetworkStateChanged Topic = "network_state_changed" // Emitted on connectivity state transitions, signal tier changes
)

// Event represents a message broadcast on the event bus.
type Event struct {
	Topic   Topic
	Payload any
}

type VolumeStateChanged struct {
	ID          string
	Desired     string
	Observed    string
	Role        string
	Generation  int
	NeedsRepair bool
	LastError   string
}

// AuditEvent captures operator-visible security events.
type AuditEvent struct {
	Kind     string
	Time     time.Time
	Source   string
	Metadata map[string]any
}

// LeadershipChanged describes a leadership role update for a resource.
type LeadershipChanged struct {
	Resource string
	Role     cluster.Role
}
type LockStateChanged struct {
	Locked bool
}

// ControlStoreCommit announces that the control store has advanced to a new revision.
type ControlStoreCommit struct {
	Revision uint64
	Checksum string
	Role     cluster.Role
}

// ServiceEndpointInfo is a lightweight representation of a service endpoint for events.
type ServiceEndpointInfo struct {
	App              string
	Name             string
	DerivedHostLabel string
	Flow             api.ListenerFlow
}

// ServiceEndpointsChanged announces endpoint lifecycle transitions for an app.
//   - Added: new endpoints now routable (install, start, restore)
//   - Deactivated: endpoints temporarily offline (app stop, reconcile rebuild);
//     downstream should stop routing but preserve associated resources (certs, DNS)
//   - Removed: endpoints permanently gone (uninstall, failed install, listener config
//     change); downstream should clean up all associated resources
type ServiceEndpointsChanged struct {
	App         string
	Added       []ServiceEndpointInfo
	Deactivated []ServiceEndpointInfo
	Removed     []ServiceEndpointInfo
}

// CertificateChangedEvent is emitted when a certificate's status changes.
// The health aggregator subscribes to this and recomputes affected listener health.
type CertificateChangedEvent struct {
	CertID       string    `json:"cert_id"`
	Status       string    `json:"status"`        // "ok", "pending", "error"
	FailureClass string    `json:"failure_class,omitempty"`
	FailureCode  string    `json:"failure_code,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}

// ListenerHealthEvent is emitted when a listener's health status changes.
// UI/WebSocket subscribers receive this for real-time health updates.
type ListenerHealthEvent struct {
	App       string         `json:"app"`
	Listener  string         `json:"listener"`
	Health    ListenerHealth `json:"health"`
	Timestamp time.Time      `json:"timestamp"`
}

// ListenerHealth represents the health status of an app listener.
// This is a summary type for events; the full type is in services/health.go.
type ListenerHealth struct {
	Status         string                     `json:"status"`                    // ok, degraded, recovering, error
	ReasonCode     string                     `json:"reason_code"`               // Machine-readable code
	Reason         string                     `json:"reason"`                    // Human-readable explanation
	Details        *string                    `json:"details,omitempty"`         // Technical details
	RecoveryETA    *time.Time                 `json:"recovery_eta,omitempty"`    // When next retry/check will occur
	Recoverable    bool                       `json:"recoverable"`               // Can system self-heal?
	ActionRequired bool                       `json:"action_required"`           // Does user need to do something?
	CertStatuses   map[string]CertHealthStatus `json:"cert_statuses,omitempty"` // Per-cert health (certID → status)
	LastChecked    time.Time                  `json:"last_checked"`
	LastOK         *time.Time                 `json:"last_ok,omitempty"` // When backend was last healthy
}

// CertHealthStatus tracks individual certificate health for events.
type CertHealthStatus struct {
	Status      string     `json:"status"`                 // ok, recovering, error
	ReasonCode  string     `json:"reason_code"`            // e.g., "cert_pending", "cert_dns_error"
	RecoveryETA *time.Time `json:"recovery_eta,omitempty"`
}

// StoragePhase1Complete is emitted when Phase 1 disk preparation finishes.
type StoragePhase1Complete struct {
	Success bool
	Error   string // non-empty on failure
}

// StorageEmergencyEvent is emitted when storage enters emergency mode.
type StorageEmergencyEvent struct {
	Level string // "hard" or "soft"
	Error string
}

// AppStatusChangedEvent is emitted when an app's status changes.
// Status values: installed, uninstalled, running, stopped, error, starting
type AppStatusChangedEvent struct {
	App        string    `json:"app"`
	Status     string    `json:"status"`
	PrevStatus string    `json:"prev_status,omitempty"`
	Message    string    `json:"message,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// PCVExportPublished is emitted after a PCV archive is published successfully.
type PCVExportPublished struct {
	Generation string // generation ID (e.g. "20260202T180405Z-000001")
	SHA256     string // hex-encoded SHA-256 of the archive
	SizeBytes  int64
}

// PCVExportFailed is emitted when a PCV publish fails.
type PCVExportFailed struct {
	Error string
}

// NetworkPeer represents a discovered peer for event payloads.
type NetworkPeer struct {
	Hostname  string `json:"hostname"`
	MachineID string `json:"machine_id"`
	IPv4      string `json:"ipv4,omitempty"`
	IPv6      string `json:"ipv6,omitempty"`
	Model     string `json:"model,omitempty"`
	Version   string `json:"version,omitempty"`
	Online    bool   `json:"online"`
}

// NetworkPeersChangedEvent is emitted when the network peer list changes.
// Contains a full snapshot of all known peers.
type NetworkPeersChangedEvent struct {
	Peers     []NetworkPeer `json:"peers"`
	Timestamp time.Time     `json:"timestamp"`
}

// OnboardingStateChangedEvent is emitted when the onboarding state transitions.
type OnboardingStateChangedEvent struct {
	State         string `json:"state"`
	PreviousState string `json:"previous_state"`
	BootMode      string `json:"boot_mode"`
}

// Bus is a simple pub/sub dispatcher for intra-process events.
type Bus struct {
	mu       sync.RWMutex
	subs     map[Topic][]chan Event
	closed   bool
	lastDrop sync.Map // Topic → time.Time; rate-limits drop warnings
}

// NewBus constructs an empty event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[Topic][]chan Event)}
}

// Subscribe registers a buffered channel for a topic.
func (b *Bus) Subscribe(topic Topic, buffer int) <-chan Event {
	ch := make(chan Event, buffer)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch
	}
	b.subs[topic] = append(b.subs[topic], ch)
	return ch
}

// SubscribeWithCancel registers a buffered channel for a topic and returns a cancel
// function that removes and closes the channel.
func (b *Bus) SubscribeWithCancel(topic Topic, buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.closed {
				return
			}
			chans := b.subs[topic]
			for i, existing := range chans {
				if existing != ch {
					continue
				}
				// Remove without preserving order.
				chans[i] = chans[len(chans)-1]
				chans[len(chans)-1] = nil
				chans = chans[:len(chans)-1]
				break
			}
			if len(chans) == 0 {
				delete(b.subs, topic)
			} else {
				b.subs[topic] = chans
			}
			close(ch)
		})
	}
	return ch, cancel
}

// Publish broadcasts an event to all subscribers.
func (b *Bus) Publish(evt Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	subs := b.subs[evt.Topic]
	for i, ch := range subs {
		select {
		case ch <- evt:
		default:
			// Rate-limited drop warning: log at most once per 10s per topic.
			now := time.Now()
			if last, ok := b.lastDrop.Load(evt.Topic); !ok || now.Sub(last.(time.Time)) >= 10*time.Second {
				b.lastDrop.Store(evt.Topic, now)
				log.Printf("WARN: events: dropped %s event (subscriber %d/%d buffer full)", evt.Topic, i+1, len(subs))
			}
		}
	}
}

// Close shuts down the bus and all subscriber channels.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, chans := range b.subs {
		for _, ch := range chans {
			close(ch)
		}
	}
	b.subs = nil
}
