package network

import "time"

// Tri is a three-state classification used for HW and Config health per device.
//
//   - TriHealthy:  proved working
//   - TriFaulted:  preconditions agree it should work, but it isn't
//   - TriInactive: preconditions correctly say "nothing to do here" (no cable,
//     rfkill-by-intent, no profile)
type Tri int

const (
	TriHealthy Tri = iota
	TriFaulted
	TriInactive
)

func (t Tri) String() string {
	switch t {
	case TriHealthy:
		return "healthy"
	case TriFaulted:
		return "faulted"
	case TriInactive:
		return "inactive"
	default:
		return "unknown"
	}
}

// DeviceKind enumerates the hardware classes managed by recovery actuators.
// Connectivity truth is per-interface in Tick.Interfaces; this map remains a
// class-level recovery observation and must not be used to discard another
// working interface of the same kind.
type DeviceKind int

const (
	DeviceWiFi DeviceKind = iota
	DeviceEthernet
)

func (d DeviceKind) String() string {
	switch d {
	case DeviceWiFi:
		return "wifi"
	case DeviceEthernet:
		return "ethernet"
	default:
		return "unknown"
	}
}

// DeviceObservation is a per-device snapshot from one probe pass.
type DeviceObservation struct {
	Kind         DeviceKind
	Present      bool
	HWHealth     Tri
	ConfigHealth Tri // Healthy | Faulted | Inactive — DHCP-in-flight maps to Healthy
	LinkUp       bool
	HasIP        bool
	GwReachable  Tri
	RfkillHard   bool   // physical kill switch — unblock impossible
	NMState      string // diagnostic only ("activated", "failed", etc.)
	NMReason     string // diagnostic only ("no_secrets", etc.)
	Iface        string // e.g. "wlan0", "eth0" (empty if Present=false)
}

// Connectivity is the supervisor's classification of upstream connectivity.
// Sourced from L3Probe + GwReachable + NMConn (advisory).
type Connectivity int

const (
	ConnectivityUnknown Connectivity = iota
	ConnectivityNone
	ConnectivityPortal
	ConnectivityLimited
	ConnectivityFull
)

func (c Connectivity) String() string {
	switch c {
	case ConnectivityNone:
		return "none"
	case ConnectivityPortal:
		return "portal"
	case ConnectivityLimited:
		return "limited"
	case ConnectivityFull:
		return "full"
	default:
		return "unknown"
	}
}

// L3ProbeResult is the result of the TCP-connect L3 probe to a small set of
// well-known IPs. Up if any target succeeds within the timeout; Down otherwise.
type L3ProbeResult int

const (
	L3ProbeUnknown L3ProbeResult = iota
	L3ProbeUp
	L3ProbeDown
)

func (r L3ProbeResult) String() string {
	switch r {
	case L3ProbeUp:
		return "up"
	case L3ProbeDown:
		return "down"
	default:
		return "unknown"
	}
}

// Tick is the input to deciders for one supervisor tick. Pure data — every
// field is set by the probe layer before deciders run.
type Tick struct {
	Devices map[DeviceKind]DeviceObservation
	// Interfaces is the authoritative connectivity projection of all
	// NM-managed physical interfaces. Devices remains a class-level input for
	// hardware recovery decisions.
	Interfaces []NetworkInterfaceState
	// InterfacesObserved says the all-interface projection completed. This
	// distinguishes a proven empty interface set from a partial projection.
	InterfacesObserved bool

	// NMConn is advisory only — read from NM's Connectivity property as it
	// stands. Deciders consume L3Probe (the TCP-connect truth) instead.
	NMConn Connectivity

	// L3Probe is the primary L3-truth: TCP-connect to 8.8.8.8:53 / 1.1.1.1:53.
	L3Probe L3ProbeResult

	// ActiveUplink is the kind of the concrete default-route interface when
	// route projection succeeds. It remains none when that projection is
	// incomplete; class-level recovery observations never populate it.
	ActiveUplink UplinkType
	// ActiveUplinkIface is the concrete interface that owns ActiveUplink
	// when known. Same-kind multi-NIC owners must consume this rather than
	// inferring from ActiveUplink alone.
	ActiveUplinkIface string

	// DefaultRouteObserved says the NetworkManager route projection completed.
	// DefaultRouteKnown says that completed projection found a default route.
	// This distinguishes a proven no-default-route state from projection
	// unavailability.
	DefaultRouteIface    string
	DefaultRouteObserved bool
	DefaultRouteKnown    bool
	DNSDefaultIface      string
	DNSDefaultObserved   bool
	DNSDefaultKnown      bool

	// SystemBusy is read once per tick from SystemState, then passed as data
	// to deciders. Closes mid-tick TOCTOU between deciders.
	SystemBusy       bool
	SystemBusyReason string

	// SystemUptime is sourced from /proc/uptime (NOT process uptime —
	// piccolod restart doesn't reset grace if the system has been up).
	SystemUptime time.Duration

	At time.Time
}
