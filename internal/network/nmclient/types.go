// Package nmclient wraps the NetworkManager D-Bus API for WiFi and Ethernet
// management. It provides a testable Client interface with a production D-Bus
// implementation and a stub for unit tests.
package nmclient

import (
	"net"
	"strings"

	"github.com/godbus/dbus/v5"
)

// D-Bus well-known names and object paths for NetworkManager.
const (
	nmBusName    = "org.freedesktop.NetworkManager"
	nmObjectPath = "/org/freedesktop/NetworkManager"

	nmInterface           = "org.freedesktop.NetworkManager"
	nmDeviceInterface     = "org.freedesktop.NetworkManager.Device"
	nmWirelessInterface   = "org.freedesktop.NetworkManager.Device.Wireless"
	nmAPInterface         = "org.freedesktop.NetworkManager.AccessPoint"
	nmActiveConnInterface = "org.freedesktop.NetworkManager.Connection.Active"
	nmSettingsInterface   = "org.freedesktop.NetworkManager.Settings"
	nmSettingsConnIface   = "org.freedesktop.NetworkManager.Settings.Connection"
	nmIP4ConfigInterface  = "org.freedesktop.NetworkManager.IP4Config"

	dbusPropertiesInterface = "org.freedesktop.DBus.Properties"
)

// NMState represents the overall NetworkManager daemon state.
type NMState uint32

const (
	NMStateUnknown      NMState = 0
	NMStateAsleep       NMState = 10
	NMStateDisconnected NMState = 20
	NMStateDisconnecting NMState = 30
	NMStateConnecting   NMState = 40
	NMStateConnectedLocal NMState = 50
	NMStateConnectedSite  NMState = 60
	NMStateConnectedGlobal NMState = 70
)

// NMDeviceState represents the state of a network device.
type NMDeviceState uint32

const (
	NMDeviceStateUnknown      NMDeviceState = 0
	NMDeviceStateUnmanaged    NMDeviceState = 10
	NMDeviceStateUnavailable  NMDeviceState = 20
	NMDeviceStateDisconnected NMDeviceState = 30
	NMDeviceStatePrepare      NMDeviceState = 40
	NMDeviceStateConfig       NMDeviceState = 50
	NMDeviceStateNeedAuth     NMDeviceState = 60
	NMDeviceStateIPConfig     NMDeviceState = 70
	NMDeviceStateIPCheck      NMDeviceState = 80
	NMDeviceStateSecondaries  NMDeviceState = 90
	NMDeviceStateActivated    NMDeviceState = 100
	NMDeviceStateDeactivating NMDeviceState = 110
	NMDeviceStateFailed       NMDeviceState = 120
)

// IsConnected returns true when the device is fully activated with an IP.
func (s NMDeviceState) IsConnected() bool {
	return s == NMDeviceStateActivated
}

// String renders the device state as a short lowercase identifier suitable
// for structured logs and diagnostic output.
func (s NMDeviceState) String() string {
	switch s {
	case NMDeviceStateUnknown:
		return "unknown"
	case NMDeviceStateUnmanaged:
		return "unmanaged"
	case NMDeviceStateUnavailable:
		return "unavailable"
	case NMDeviceStateDisconnected:
		return "disconnected"
	case NMDeviceStatePrepare:
		return "prepare"
	case NMDeviceStateConfig:
		return "config"
	case NMDeviceStateNeedAuth:
		return "need_auth"
	case NMDeviceStateIPConfig:
		return "ip_config"
	case NMDeviceStateIPCheck:
		return "ip_check"
	case NMDeviceStateSecondaries:
		return "secondaries"
	case NMDeviceStateActivated:
		return "activated"
	case NMDeviceStateDeactivating:
		return "deactivating"
	case NMDeviceStateFailed:
		return "failed"
	default:
		return "?"
	}
}

// String renders the most operationally-meaningful reason codes; less-common
// reasons collapse to "other" so callers don't have to enumerate the whole
// enum just to get readable output.
func (r NMDeviceStateReason) String() string {
	switch r {
	case NMDeviceStateReasonNone:
		return "none"
	case NMDeviceStateReasonNoSecrets:
		return "no_secrets"
	case NMDeviceStateReasonSupplicantConfigFailed:
		return "supplicant_config_failed"
	case NMDeviceStateReasonSupplicantFailed:
		return "supplicant_failed"
	case NMDeviceStateReasonSupplicantTimeout:
		return "supplicant_timeout"
	case NMDeviceStateReasonSupplicantDisconnect:
		return "supplicant_disconnect"
	case NMDeviceStateReasonDHCPFailed:
		return "dhcp_failed"
	case NMDeviceStateReasonDHCPError:
		return "dhcp_error"
	case NMDeviceStateReasonCarrier:
		return "carrier"
	case NMDeviceStateReasonUserRequested:
		return "user_requested"
	default:
		return "other"
	}
}

// NMDeviceStateReason provides additional context for device state changes.
type NMDeviceStateReason uint32

const (
	NMDeviceStateReasonNone            NMDeviceStateReason = 0
	NMDeviceStateReasonUnknown         NMDeviceStateReason = 1
	NMDeviceStateReasonNowManaged      NMDeviceStateReason = 2
	NMDeviceStateReasonNowUnmanaged    NMDeviceStateReason = 3
	NMDeviceStateReasonConfigFailed    NMDeviceStateReason = 4
	NMDeviceStateReasonIPConfigUnavail NMDeviceStateReason = 5
	NMDeviceStateReasonIPConfigExpired NMDeviceStateReason = 6
	NMDeviceStateReasonNoSecrets       NMDeviceStateReason = 7
	NMDeviceStateReasonSupplicantDisconnect NMDeviceStateReason = 8
	NMDeviceStateReasonSupplicantConfigFailed NMDeviceStateReason = 9
	NMDeviceStateReasonSupplicantFailed NMDeviceStateReason = 10
	NMDeviceStateReasonSupplicantTimeout NMDeviceStateReason = 11
	NMDeviceStateReasonPPPStartFailed  NMDeviceStateReason = 12
	NMDeviceStateReasonPPPDisconnect   NMDeviceStateReason = 13
	NMDeviceStateReasonPPPFailed       NMDeviceStateReason = 14
	NMDeviceStateReasonDHCPStartFailed NMDeviceStateReason = 15
	NMDeviceStateReasonDHCPError       NMDeviceStateReason = 16
	NMDeviceStateReasonDHCPFailed      NMDeviceStateReason = 17
	NMDeviceStateReasonSharedStartFailed NMDeviceStateReason = 18
	NMDeviceStateReasonSharedFailed    NMDeviceStateReason = 19
	NMDeviceStateReasonAutoIPStartFailed NMDeviceStateReason = 20
	NMDeviceStateReasonAutoIPError     NMDeviceStateReason = 21
	NMDeviceStateReasonAutoIPFailed    NMDeviceStateReason = 22
	NMDeviceStateReasonModemBusy       NMDeviceStateReason = 23
	NMDeviceStateReasonModemNoDialTone NMDeviceStateReason = 24
	NMDeviceStateReasonModemNoCarrier  NMDeviceStateReason = 25
	NMDeviceStateReasonModemDialTimeout NMDeviceStateReason = 26
	NMDeviceStateReasonModemDialFailed NMDeviceStateReason = 27
	NMDeviceStateReasonModemInitFailed NMDeviceStateReason = 28
	NMDeviceStateReasonGSMAPNFailed    NMDeviceStateReason = 29
	NMDeviceStateReasonGSMRegistrationNotSearching NMDeviceStateReason = 30
	NMDeviceStateReasonGSMRegistrationDenied       NMDeviceStateReason = 31
	NMDeviceStateReasonGSMRegistrationTimeout      NMDeviceStateReason = 32
	NMDeviceStateReasonGSMRegistrationFailed       NMDeviceStateReason = 33
	NMDeviceStateReasonGSMPINCheckFailed           NMDeviceStateReason = 34
	NMDeviceStateReasonFirmwareMissing NMDeviceStateReason = 35
	NMDeviceStateReasonRemoved         NMDeviceStateReason = 36
	NMDeviceStateReasonSleeping        NMDeviceStateReason = 37
	NMDeviceStateReasonConnectionRemoved NMDeviceStateReason = 38
	NMDeviceStateReasonUserRequested   NMDeviceStateReason = 39
	NMDeviceStateReasonCarrier         NMDeviceStateReason = 40
	NMDeviceStateReasonConnectionAssumed NMDeviceStateReason = 41
	NMDeviceStateReasonSupplicantAvail NMDeviceStateReason = 42
	NMDeviceStateReasonModemNotFound   NMDeviceStateReason = 43
	NMDeviceStateReasonBTFailed        NMDeviceStateReason = 44
	NMDeviceStateReasonNewActivation   NMDeviceStateReason = 50
	NMDeviceStateReasonParentChanged   NMDeviceStateReason = 51
	NMDeviceStateReasonParentManagedChanged NMDeviceStateReason = 52
)

// NMDeviceType identifies the type of a network device.
type NMDeviceType uint32

const (
	NMDeviceTypeUnknown  NMDeviceType = 0
	NMDeviceTypeEthernet NMDeviceType = 1
	NMDeviceTypeWiFi     NMDeviceType = 2
	NMDeviceTypeBT       NMDeviceType = 5
	NMDeviceTypeBridge   NMDeviceType = 13
	NMDeviceTypeVLAN     NMDeviceType = 11
)

// NMConnectivityState represents NM's assessment of internet connectivity.
type NMConnectivityState uint32

const (
	NMConnectivityUnknown NMConnectivityState = 0
	NMConnectivityNone    NMConnectivityState = 1
	NMConnectivityPortal  NMConnectivityState = 2
	NMConnectivityLimited NMConnectivityState = 3
	NMConnectivityFull    NMConnectivityState = 4
)

// NM80211ApSecurityFlags represents the security capabilities of an access point.
type NM80211ApSecurityFlags uint32

const (
	NM80211ApSecNone         NM80211ApSecurityFlags = 0x0
	NM80211ApSecPairWEP40    NM80211ApSecurityFlags = 0x1
	NM80211ApSecPairWEP104   NM80211ApSecurityFlags = 0x2
	NM80211ApSecPairTKIP     NM80211ApSecurityFlags = 0x4
	NM80211ApSecPairCCMP     NM80211ApSecurityFlags = 0x8
	NM80211ApSecGroupWEP40   NM80211ApSecurityFlags = 0x10
	NM80211ApSecGroupWEP104  NM80211ApSecurityFlags = 0x20
	NM80211ApSecGroupTKIP    NM80211ApSecurityFlags = 0x40
	NM80211ApSecGroupCCMP    NM80211ApSecurityFlags = 0x80
	NM80211ApSecKeyMgmtPSK   NM80211ApSecurityFlags = 0x100
	NM80211ApSecKeyMgmt8021X NM80211ApSecurityFlags = 0x200
	NM80211ApSecKeyMgmtSAE   NM80211ApSecurityFlags = 0x400
)

// WiFiDevice represents a discovered WiFi network interface.
type WiFiDevice struct {
	Path       dbus.ObjectPath
	Interface  string          // e.g., "wlan0"
	HWAddress  net.HardwareAddr
	State      NMDeviceState
	Driver     string
}

// EthernetDevice represents a discovered Ethernet network interface.
type EthernetDevice struct {
	Path      dbus.ObjectPath
	Interface string          // e.g., "eth0", "enp0s3"
	HWAddress net.HardwareAddr
	State     NMDeviceState
	Carrier   bool           // physical link detected
}

// AccessPoint represents a WiFi network seen during a scan.
type AccessPoint struct {
	Path      dbus.ObjectPath
	SSID      string
	HWAddress string         // BSSID
	Frequency uint32         // MHz
	Strength  uint8          // 0–100 (NM's normalized signal percentage)
	RSNFlags  NM80211ApSecurityFlags
	WPAFlags  NM80211ApSecurityFlags
	Flags     uint32
}

// SecurityType returns a human-readable security classification.
func (ap AccessPoint) SecurityType() string {
	if ap.RSNFlags&NM80211ApSecKeyMgmtSAE != 0 {
		return "wpa3"
	}
	if ap.RSNFlags&NM80211ApSecKeyMgmtPSK != 0 {
		return "wpa2"
	}
	if ap.WPAFlags&NM80211ApSecKeyMgmtPSK != 0 {
		return "wpa"
	}
	if ap.RSNFlags&NM80211ApSecKeyMgmt8021X != 0 || ap.WPAFlags&NM80211ApSecKeyMgmt8021X != 0 {
		return "enterprise"
	}
	if ap.Flags&0x1 != 0 { // NM_802_11_AP_FLAGS_PRIVACY
		return "wep"
	}
	return "open"
}

// Band returns "2.4GHz" or "5GHz" based on the frequency.
func (ap AccessPoint) Band() string {
	if ap.Frequency >= 5000 {
		return "5GHz"
	}
	return "2.4GHz"
}

// SignalDBm converts NM's 0–100 strength percentage to approximate dBm.
// NM maps roughly: 100% ≈ -30 dBm, 0% ≈ -90 dBm.
func (ap AccessPoint) SignalDBm() int {
	return int(ap.Strength)*60/100 - 90
}

// ConnectionProfile represents a saved NM connection profile.
type ConnectionProfile struct {
	Path     dbus.ObjectPath
	ID       string   // connection name
	UUID     string
	Type     string   // "802-11-wireless", "802-3-ethernet", etc.
	SSID     string   // for WiFi connections
}

// ConnectionSnapshot holds the full settings of a connection profile for rollback.
type ConnectionSnapshot struct {
	Path     dbus.ObjectPath
	Device   dbus.ObjectPath // device to activate on when restoring
	Settings map[string]map[string]dbus.Variant
}

// SSID extracts the SSID from the snapshot settings.
func (s *ConnectionSnapshot) SSID() string {
	if s == nil {
		return ""
	}
	return SSIDFromSettings(s.Settings)
}

// SSIDFromSettings reads the 802-11-wireless.ssid byte field from a
// connection-settings dict and returns it as a string. Returns "" on any
// missing section, missing key, or wrong-typed value. Shared between
// (*ConnectionSnapshot).SSID and the NM SecretAgent's GetSecrets handler
// (nmagent), which receives the same dict shape from NM but does not wrap
// it in a ConnectionSnapshot.
func SSIDFromSettings(settings map[string]map[string]dbus.Variant) string {
	ws, ok := settings["802-11-wireless"]
	if !ok {
		return ""
	}
	v, ok := ws["ssid"]
	if !ok {
		return ""
	}
	b, ok := v.Value().([]byte)
	if !ok {
		return ""
	}
	return string(b)
}

// PSK extracts the WPA-PSK passphrase from the snapshot settings.
// Returns "" for open profiles, profiles whose secrets weren't recoverable
// from getConnectionSecrets, or any non-string PSK value. Mirrors SSID()'s
// nil-tolerant accessor pattern. Used by Manager.Connect's rollback path
// to populate the secret-agent cache and by RestoreConnection itself.
func (s *ConnectionSnapshot) PSK() string {
	if s == nil {
		return ""
	}
	if sec, ok := s.Settings["802-11-wireless-security"]; ok {
		if pskV, ok := sec["psk"]; ok {
			if psk, ok := pskV.Value().(string); ok {
				return psk
			}
		}
	}
	return ""
}

// ActiveConnectionInfo holds details about a currently active connection.
type ActiveConnectionInfo struct {
	Path        dbus.ObjectPath
	UUID        string
	ID          string
	State       uint32 // NM_ACTIVE_CONNECTION_STATE_*
	Type        string
	IP4Address  string
	IP4Gateway  string
	Devices     []dbus.ObjectPath
}

// IsHotspot returns true if this connection is a piccolo AP hotspot.
func (i *ActiveConnectionInfo) IsHotspot() bool {
	return i != nil && strings.HasPrefix(i.ID, HotspotIDPrefix)
}

// HotspotOpts configures AP mode hotspot activation.
type HotspotOpts struct {
	Channel   int    // 0 = auto-select
	Band      string // "a" (5GHz) or "bg" (2.4GHz), "" = auto
	Zone      string // firewalld zone to assign (e.g., "piccolo-ap")
}

// StateChange is emitted when NM's overall daemon state changes.
type StateChange struct {
	NewState NMState
}

// DeviceStateChange is emitted when a specific device's state changes.
type DeviceStateChange struct {
	Device   dbus.ObjectPath
	NewState NMDeviceState
	OldState NMDeviceState
	Reason   NMDeviceStateReason
}

// DeviceEventType identifies whether a device was added or removed.
type DeviceEventType int

const (
	DeviceAdded   DeviceEventType = 1
	DeviceRemoved DeviceEventType = 2
)

// DeviceEvent is emitted when a device is added to or removed from NM.
type DeviceEvent struct {
	Type   DeviceEventType
	Device dbus.ObjectPath
}

// NMActiveConnectionState represents the state of a single active-connection
// D-Bus object (org.freedesktop.NetworkManager.Connection.Active.State).
// Distinct from NMDeviceState (which is per-device, shared across every
// connection's lifecycle on that device). Path-scoped subscriptions on
// ActiveConnection paths return this enum's values.
type NMActiveConnectionState uint32

const (
	NMActiveConnectionStateUnknown      NMActiveConnectionState = 0
	NMActiveConnectionStateActivating   NMActiveConnectionState = 1
	NMActiveConnectionStateActivated    NMActiveConnectionState = 2
	NMActiveConnectionStateDeactivating NMActiveConnectionState = 3
	NMActiveConnectionStateDeactivated  NMActiveConnectionState = 4
)

func (s NMActiveConnectionState) String() string {
	switch s {
	case NMActiveConnectionStateUnknown:
		return "unknown"
	case NMActiveConnectionStateActivating:
		return "activating"
	case NMActiveConnectionStateActivated:
		return "activated"
	case NMActiveConnectionStateDeactivating:
		return "deactivating"
	case NMActiveConnectionStateDeactivated:
		return "deactivated"
	default:
		return "?"
	}
}

// NMActiveConnectionStateReason provides additional context for active-conn
// state changes. Distinct enum from NMDeviceStateReason; values DO NOT match
// numerically. Translated to NMDeviceStateReason at the WaitForActivation
// boundary via translateActiveConnReason so callers see a single reason
// vocabulary.
type NMActiveConnectionStateReason uint32

const (
	NMActiveConnectionStateReasonUnknown          NMActiveConnectionStateReason = 0
	NMActiveConnectionStateReasonNone             NMActiveConnectionStateReason = 1
	NMActiveConnectionStateReasonUserDisconnected NMActiveConnectionStateReason = 2
	NMActiveConnectionStateReasonDeviceDisconnect NMActiveConnectionStateReason = 3
	NMActiveConnectionStateReasonServiceStopped   NMActiveConnectionStateReason = 4
	NMActiveConnectionStateReasonIPConfigInvalid  NMActiveConnectionStateReason = 5
	NMActiveConnectionStateReasonConnectTimeout   NMActiveConnectionStateReason = 6
	NMActiveConnectionStateReasonServiceStartTimeout NMActiveConnectionStateReason = 7
	NMActiveConnectionStateReasonServiceStartFailed  NMActiveConnectionStateReason = 8
	NMActiveConnectionStateReasonNoSecrets        NMActiveConnectionStateReason = 9
	NMActiveConnectionStateReasonLoginFailed      NMActiveConnectionStateReason = 10
	NMActiveConnectionStateReasonConnectionRemoved NMActiveConnectionStateReason = 11
	NMActiveConnectionStateReasonDependencyFailed NMActiveConnectionStateReason = 12
	NMActiveConnectionStateReasonDeviceRealizeFailed NMActiveConnectionStateReason = 13
	NMActiveConnectionStateReasonDeviceRemoved    NMActiveConnectionStateReason = 14
)

// ActiveConnectionStateChange is emitted when an active-connection D-Bus
// object's State property changes. Path-scoped (vs DeviceStateChange which
// is device-scoped); the wait function for a specific activation watches
// only its own path's stream, immune to OLD-connection contamination from
// other paths on the same device.
type ActiveConnectionStateChange struct {
	Path     dbus.ObjectPath
	NewState NMActiveConnectionState
	Reason   NMActiveConnectionStateReason
}
