// Package network implements WiFi LAN uplink management, AP mode with captive
// portal, and network health monitoring for piccolod. It replaces the external
// piccolo-net-watchdog bash script with an integrated, WiFi-aware network
// manager that runs as a supervisor component.
package network

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

// NetworkStatusInterface is the API projection of one managed physical
// interface. The API uses string kinds and addresses instead of exposing the
// supervisor's internal enum and netip representations.
type NetworkStatusInterface struct {
	Kind   string               `json:"kind"`
	Iface  string               `json:"iface"`
	Role   NetworkInterfaceRole `json:"role"`
	LinkUp bool                 `json:"link_up"`
	HasIP  bool                 `json:"has_ip"`
	IPv4   []string             `json:"ipv4,omitempty"`
	IPv6   []string             `json:"ipv6,omitempty"`
}

// NetworkStatus is the public, factual network snapshot returned by
// Manager.Status. Connectivity and interface ownership come from the typed
// multi-interface supervisor state; WiFi-specific fields support management
// actions and presentation without creating a second connection-state model.
type NetworkStatus struct {
	ActiveUplink      UplinkType               `json:"active_uplink"`
	ActiveUplinkIface string                   `json:"active_uplink_iface,omitempty"`
	Connectivity      string                   `json:"connectivity"`
	Interfaces        []NetworkStatusInterface `json:"interfaces"`
	APActive          bool                     `json:"ap_active"`
	WiFiAvailable     bool                     `json:"wifi_available"`
	SSID              string                   `json:"ssid,omitempty"`
	SignalDBM         *int                     `json:"signal_dbm,omitempty"`
	SignalTier        SignalTier               `json:"signal_tier,omitempty"`
	FrequencyMHz      *uint32                  `json:"frequency_mhz,omitempty"`
	Band              string                   `json:"band,omitempty"`
	IPAddress         string                   `json:"ip_address,omitempty"`
	HasSavedNetwork   bool                     `json:"has_saved_network"`
	SavedSSID         string                   `json:"saved_ssid,omitempty"`
}

// ScanResult represents a WiFi network found during a scan.
type ScanResult struct {
	SSID         string     `json:"ssid"`
	Security     string     `json:"security"` // "open", "wpa", "wpa2", "wpa3", "wep", "enterprise"
	SignalDBm    int        `json:"signal_dbm"`
	SignalTier   SignalTier `json:"signal_tier"`
	FrequencyMHz uint32     `json:"frequency_mhz"`
	Band         string     `json:"band"` // "2.4GHz", "5GHz"
}

// APStatus is the public snapshot of AP mode state.
type APStatus struct {
	Active     bool   `json:"active"`
	SSID       string `json:"ssid,omitempty"`
	Suppressed bool   `json:"suppressed"`
	Clients    int    `json:"clients"`
}

// WiFiSignalChangedEvent is independent of topology transitions. Consumers
// that need a full status snapshot should read Manager.Status after this wake.
type WiFiSignalChangedEvent struct {
	SignalDBM  int        `json:"signal_dbm"`
	SignalTier SignalTier `json:"signal_tier"`
}
