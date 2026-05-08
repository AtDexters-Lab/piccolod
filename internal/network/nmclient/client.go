package nmclient

import (
	"context"

	"github.com/godbus/dbus/v5"
)

// Client abstracts NetworkManager D-Bus interaction for testability.
// The production implementation connects to the system bus and calls NM's
// D-Bus methods. A StubClient is provided for unit tests.
type Client interface {
	// WiFiDevices returns all WiFi-capable network devices.
	WiFiDevices() ([]WiFiDevice, error)

	// EthernetDevices returns all Ethernet network devices.
	EthernetDevices() ([]EthernetDevice, error)

	// Scan triggers a WiFi scan on the given device and returns visible APs.
	// Blocks until the scan completes (typically 5–10 seconds).
	Scan(device dbus.ObjectPath) ([]AccessPoint, error)

	// Connect creates or updates an NM connection profile for the given SSID
	// and activates it on the device. Returns the D-Bus paths of the new
	// connection profile (settings) AND the new active connection — the
	// latter is required by WaitForActivation to disambiguate this attempt
	// from a still-Activated previous connection (TOCTOU).
	//
	// The connection attempt is fire-and-forget at this layer; success is
	// determined via WaitForActivation or DeviceStateChange.
	Connect(device dbus.ObjectPath, ssid, passphrase string) (dbus.ObjectPath, dbus.ObjectPath, error)

	// Disconnect deactivates the active connection on the device.
	Disconnect(device dbus.ObjectPath) error

	// SavedWiFiConnections returns all persisted WiFi connection profiles.
	SavedWiFiConnections() ([]ConnectionProfile, error)

	// DeleteConnection removes a saved connection profile.
	DeleteConnection(path dbus.ObjectPath) error

	// SnapshotConnection captures the full settings of a connection profile
	// for later rollback via RestoreConnection.
	SnapshotConnection(path dbus.ObjectPath) (*ConnectionSnapshot, error)

	// RestoreConnection replaces a connection profile's settings with a
	// previously captured snapshot.
	RestoreConnection(snapshot *ConnectionSnapshot) error

	// ActivateHotspot configures and activates an AP-mode hotspot on the
	// given WiFi device with the specified SSID. The hotspot is open (no
	// passphrase). NM handles hostapd internally. Returns both the active-
	// connection path (for path-scoped WaitForActivation) and the settings
	// path (for path-scoped DeleteConnection on rollback). When err is non-
	// nil, paths are returned on a best-effort basis — callers MUST issue
	// DeleteConnection(settingsPath) on rollback if non-empty regardless of
	// err, because NM may have created the profile before failing the
	// activation step (brcmfmac firmware mid-crash, broker mid-restart).
	ActivateHotspot(device dbus.ObjectPath, ssid string, opts HotspotOpts) (dbus.ObjectPath, dbus.ObjectPath, error)

	// DeactivateHotspot tears down the active hotspot on the device.
	DeactivateHotspot(device dbus.ObjectPath) error

	// Connectivity returns NM's current assessment of internet connectivity.
	Connectivity() (NMConnectivityState, error)

	// DeviceState returns the current state of a specific device.
	DeviceState(device dbus.ObjectPath) (NMDeviceState, error)

	// DeviceStateReason returns the (state, reason) pair NM is tracking
	// for a device. Used at startup before any StateChanged signal has
	// been observed — without this, the prober's signal cache is empty
	// and persistent NoSecrets / SupplicantFailed states misclassify as
	// "no reason known".
	DeviceStateReason(device dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error)

	// ActiveConnectionInfo returns details about the active connection on a
	// device, or nil if no connection is active.
	ActiveConnectionInfo(device dbus.ObjectPath) (*ActiveConnectionInfo, error)

	// SignalStrength returns the current WiFi signal strength in NM's 0–100
	// percentage scale for the active AP on the given WiFi device.
	// Returns 0 if no AP is associated.
	SignalStrength(device dbus.ObjectPath) (uint8, error)

	// WirelessEnabled returns true if the WiFi radio is enabled.
	WirelessEnabled() (bool, error)

	// SetWirelessEnabled enables or disables the WiFi radio.
	SetWirelessEnabled(enabled bool) error

	// SubscribeStateChanges returns a channel that receives NM daemon state
	// changes. The channel is closed when the context is cancelled or the
	// D-Bus connection is lost.
	SubscribeStateChanges(ctx context.Context) (<-chan StateChange, error)

	// SubscribeDeviceStateChanges returns a channel that receives state
	// changes for a specific device.
	SubscribeDeviceStateChanges(ctx context.Context, device dbus.ObjectPath) (<-chan DeviceStateChange, error)

	// SubscribeDeviceAddedRemoved returns a channel that receives events
	// when devices are added to or removed from NetworkManager (hotplug).
	SubscribeDeviceAddedRemoved(ctx context.Context) (<-chan DeviceEvent, error)

	// SubscribeActiveConnectionState returns a channel that receives State
	// property changes for a specific active-connection path. Path-scoped;
	// used by WaitForActivation to disambiguate this activation's lifecycle
	// from any concurrent OLD-connection teardown on the same device.
	SubscribeActiveConnectionState(ctx context.Context, path dbus.ObjectPath) (<-chan ActiveConnectionStateChange, error)

	// WaitForActivation blocks until the active-connection path reaches
	// Activated or a terminal failure state. Returns the final device-
	// equivalent state and reason after translation. Use a context with
	// timeout to bound the wait.
	//
	// path is the active-connection D-Bus object returned from
	// AddAndActivateConnection (Connect for STA, ActivateHotspot for AP).
	// The wait is path-scoped: it subscribes to PropertiesChanged on that
	// specific object, immune to OLD-connection contamination from any
	// concurrent teardown on the same device. path must be non-empty;
	// passing "" returns errEmptyActiveConnPath.
	WaitForActivation(ctx context.Context, path dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error)

	// IsConnected returns true if the D-Bus connection to NM is alive.
	// When false, the client is operating in degraded polling-fallback mode.
	IsConnected() bool

	// Close releases the D-Bus connection and all signal subscriptions.
	Close() error
}
