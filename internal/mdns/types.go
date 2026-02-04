package mdns

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// InterfaceState tracks the state of a network interface for mDNS
type InterfaceState struct {
	Interface *net.Interface

	// Dual-stack IP support
	IPv4     net.IP
	IPv6     net.IP
	IPv4Conn *net.UDPConn
	IPv6Conn *net.UDPConn

	Active   bool
	LastSeen time.Time

	// Stack capabilities
	HasIPv4 bool
	HasIPv6 bool

	// Security metrics
	QueryCount uint64
	LastQuery  time.Time
	ErrorCount uint64

	// Resilience tracking
	FailureCount     uint64
	LastFailure      time.Time
	RecoveryAttempts uint64
	BackoffUntil     time.Time
	HealthScore      float64 // 0.0 (unhealthy) to 1.0 (healthy)
	resilienceMu     sync.RWMutex
}

// SecurityConfig defines security limits and thresholds
type SecurityConfig struct {
	MaxPacketSize        int
	MaxResponseSize      int
	MaxConcurrentQueries int
	QueryTimeout         time.Duration
}

// SecurityMetrics tracks overall security statistics
type SecurityMetrics struct {
	TotalQueries     uint64
	MalformedPackets uint64
	LargePackets     uint64
}

// ResilienceConfig defines error recovery and resilience parameters
type ResilienceConfig struct {
	MaxRetries            int
	InitialBackoff        time.Duration
	MaxBackoff            time.Duration
	BackoffMultiplier     float64
	HealthCheckInterval   time.Duration
	RecoveryCheckInterval time.Duration
	MaxFailureRate        float64
	MinHealthScore        float64
	RecoveryTimeout       time.Duration
}

// HealthMonitor tracks system health and triggers recovery
type HealthMonitor struct {
	OverallHealth    float64
	InterfaceHealth  map[string]float64
	LastHealthCheck  time.Time
	RecoveryActive   bool
	SystemErrors     uint64
	RecoveryAttempts uint64
	mutex            sync.RWMutex
}

// ConflictDetector manages DNS name conflicts and resolution
type ConflictDetector struct {
	ConflictDetected   bool
	ConflictingSources map[string]ConflictingHost
	LastConflictCheck  time.Time
	ResolutionAttempts uint64
	CurrentSuffix      string
	mutex              sync.RWMutex
}

// ConflictingHost represents a host that conflicts with our name
type ConflictingHost struct {
	IP         net.IP
	FirstSeen  time.Time
	LastSeen   time.Time
	QueryCount uint64
	MachineID  string // Derived from responses if available
}

// QueryProcessor manages concurrent query processing with limits
type QueryProcessor struct {
	semaphore   chan struct{}
	activeCount int64
}

// Manager handles mDNS advertising for the Piccolo service
type Manager struct {
	// Multi-interface support
	interfaces map[string]*InterfaceState
	mutex      sync.RWMutex

	// Original fields
	hostname string
	port     int
	stopCh   chan struct{}
	wg       sync.WaitGroup
	startMu  sync.Mutex
	stopOnce sync.Once
	started  atomic.Bool
	stopped  atomic.Bool

	// Deterministic naming support
	baseName  string
	machineID string
	finalName string
	names     *NameRegistry

	// Security components
	securityConfig  *SecurityConfig
	securityMetrics *SecurityMetrics
	queryProcessor  *QueryProcessor

	// Resilience components
	resilienceConfig *ResilienceConfig
	healthMonitor    *HealthMonitor

	// Conflict detection and resolution
	conflictDetector *ConflictDetector

	// Peer discovery
	peerRegistry    *PeerRegistry
	serviceMetadata *ServiceMetadata
	version         string
	deviceModel     string
	bootTime        time.Time

	// Gateway leadership
	gatewayLeader    *GatewayLeader
	specificHostname string // Always "piccolo-<machineId>.local"

	// Socket factories (overrideable for tests)
	ipv4SocketFactory func(*net.Interface) (*net.UDPConn, error)
	ipv6SocketFactory func(*net.Interface) (*net.UDPConn, error)

	// Service endpoint observation
	endpointsMu          sync.Mutex
	endpointsCancel      func()
	endpointsUnsubscribe func() // Unsubscribe from event bus
	appHostLabels        map[string][]string // app -> host labels for that app
	appHostLabelsMu      sync.RWMutex

	// Announcement debouncing
	announceDebounceMu    sync.Mutex
	announceDebounceTimer *time.Timer
	announcePending       bool

}
