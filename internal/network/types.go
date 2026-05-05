// Package network implements WiFi LAN uplink management, AP mode with captive
// portal, and network health monitoring for piccolod. It replaces the external
// piccolo-net-watchdog bash script with an integrated, WiFi-aware network
// manager that runs as a supervisor component.
package network

// ConnState represents the current connectivity state in the three-tier
// priority stack: Ethernet > WiFi STA > AP Mode.
type ConnState string

const (
	StateEthernet     ConnState = "ethernet"       // Ethernet up (preferred)
	StateWiFiSTA      ConnState = "wifi_connected"  // WiFi STA connected
	StateReconnecting ConnState = "reconnecting"    // WiFi lost, auto-retrying
	StateDisconnected ConnState = "disconnected"    // Retries at backoff ceiling
	StateAPMode       ConnState = "ap_mode"         // Broadcasting setup AP
)

// UplinkType identifies which interface carries traffic.
type UplinkType string

const (
	UplinkEthernet UplinkType = "ethernet"
	UplinkWiFi     UplinkType = "wifi"
	UplinkNone     UplinkType = "none"
)

// SignalTier maps dBm ranges to human-readable quality labels.
type SignalTier string

const (
	SignalGood SignalTier = "good" // > -60 dBm
	SignalFair SignalTier = "fair" // -60 to -70 dBm
	SignalWeak SignalTier = "weak" // -70 to -80 dBm
	SignalPoor SignalTier = "poor" // < -80 dBm
)

// ClassifySignal maps a dBm value to a SignalTier.
func ClassifySignal(dbm int) SignalTier {
	switch {
	case dbm > -60:
		return SignalGood
	case dbm > -70:
		return SignalFair
	case dbm > -80:
		return SignalWeak
	default:
		return SignalPoor
	}
}

// Status is the public snapshot of the network manager's state, returned
// by Manager.Status() and used by API handlers.
type Status struct {
	WifiAvailable   bool       `json:"wifi_available"`
	State           ConnState  `json:"state"`
	ActiveUplink    UplinkType `json:"active_uplink"`
	SSID            string     `json:"ssid,omitempty"`
	SignalDBm       *int       `json:"signal_dbm,omitempty"`
	SignalTier      SignalTier `json:"signal_tier,omitempty"`
	FrequencyMHz    *uint32    `json:"frequency_mhz,omitempty"`
	Band            string     `json:"band,omitempty"`
	IPAddress       string     `json:"ip_address,omitempty"`
	HasSavedNetwork bool       `json:"has_saved_network"`
	SavedSSID       string     `json:"saved_ssid,omitempty"`
}

// ScanResult represents a WiFi network found during a scan.
type ScanResult struct {
	SSID         string     `json:"ssid"`
	Security     string     `json:"security"`       // "open", "wpa", "wpa2", "wpa3", "wep", "enterprise"
	SignalDBm    int        `json:"signal_dbm"`
	SignalTier   SignalTier `json:"signal_tier"`
	FrequencyMHz uint32    `json:"frequency_mhz"`
	Band         string     `json:"band"`            // "2.4GHz", "5GHz"
}

// APStatus is the public snapshot of AP mode state.
type APStatus struct {
	Active     bool   `json:"active"`
	SSID       string `json:"ssid,omitempty"`
	Suppressed bool   `json:"suppressed"`
	Clients    int    `json:"clients"`
}

// NetworkStateChangedEvent is the payload for events.TopicNetworkStateChanged.
// Preserved as a wire contract — existing subscribers (identity, stun, ui)
// pattern-match on (ActiveUplink, SignalDBm) to react to uplink-up.
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
