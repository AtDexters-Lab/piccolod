package nmclient

import (
	"context"
	"sync"

	"github.com/godbus/dbus/v5"
)

// StubClient is a test double for Client. It returns configurable canned
// responses and records method calls for assertions. All fields are public
// for test configuration.
type StubClient struct {
	mu sync.Mutex

	// Configurable return values
	WaitForActivationState      NMDeviceState
	WaitForActivationReason     NMDeviceStateReason
	WaitForActivationErr        error
	ActivateHotspotActivePath   dbus.ObjectPath
	ActivateHotspotSettingsPath dbus.ObjectPath
	ActiveConnStateCh           chan ActiveConnectionStateChange
	WiFiDevicesResult           []WiFiDevice
	WiFiDevicesErr              error
	EthernetDevicesResult       []EthernetDevice
	EthernetDevicesErr          error
	ScanResult                  []AccessPoint
	ScanErr                     error
	ConnectErr                  error
	DisconnectErr               error
	ConnectPath                 dbus.ObjectPath
	ConnectActivePath           dbus.ObjectPath
	SavedConnections            []ConnectionProfile
	SavedConnectionsErr         error
	DeleteConnectionErr         error
	SnapshotResult              *ConnectionSnapshot
	SnapshotErr                 error
	RestoreErr                  error
	ActivateHotspotErr          error
	DeactivateHotspotErr        error
	ConnectivityResult          NMConnectivityState
	ConnectivityErr             error
	DeviceStateResult           NMDeviceState
	DeviceStateErr              error
	DeviceStateReasonResult     NMDeviceStateReason
	DeviceStateReasonErr        error
	ActiveConnResult            *ActiveConnectionInfo
	ActiveConnByDevice          map[dbus.ObjectPath]*ActiveConnectionInfo
	ActiveConnErr               error
	ActiveConnErrByDevice       map[dbus.ObjectPath]error
	ActiveAPResult              *AccessPoint
	ActiveAPByDevice            map[dbus.ObjectPath]*AccessPoint
	ActiveAPErr                 error
	ActiveAPErrByDevice         map[dbus.ObjectPath]error
	SignalStrengthResult        uint8
	SignalStrengthErr           error
	WirelessEnabledResult       bool
	WirelessEnabledErr          error
	SetWirelessErr              error
	Connected                   bool

	// Call recording
	Calls []StubCall

	// State change injection channels (tests push events into these)
	stateChangeCh       chan StateChange
	deviceStateChangeCh chan DeviceStateChange
	deviceEventCh       chan DeviceEvent
}

// StubCall records a method invocation on the stub.
type StubCall struct {
	Method string
	Args   []interface{}
}

// NewStubClient creates a StubClient with sensible defaults.
func NewStubClient() *StubClient {
	return &StubClient{
		Connected:                   true,
		ConnectivityResult:          NMConnectivityFull,
		DeviceStateResult:           NMDeviceStateActivated,
		WaitForActivationState:      NMDeviceStateActivated,
		WaitForActivationReason:     NMDeviceStateReasonNone,
		ActivateHotspotActivePath:   "/org/freedesktop/NetworkManager/ActiveConnection/stub-hotspot",
		ActivateHotspotSettingsPath: "/org/freedesktop/NetworkManager/Settings/stub-hotspot",
		WirelessEnabledResult:       true,
		stateChangeCh:               make(chan StateChange, 16),
		deviceStateChangeCh:         make(chan DeviceStateChange, 16),
		deviceEventCh:               make(chan DeviceEvent, 16),
		ActiveConnStateCh:           make(chan ActiveConnectionStateChange, 64),
	}
}

func (s *StubClient) record(method string, args ...interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, StubCall{Method: method, Args: args})
}

// CallCount returns the number of times a method was called.
func (s *StubClient) CallCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// LastCall returns the most recent call to the given method, or nil.
func (s *StubClient) LastCall(method string) *StubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Calls) - 1; i >= 0; i-- {
		if s.Calls[i].Method == method {
			return &s.Calls[i]
		}
	}
	return nil
}

// InjectStateChange sends a state change event to any subscriber.
func (s *StubClient) InjectStateChange(sc StateChange) {
	select {
	case s.stateChangeCh <- sc:
	default:
	}
}

// InjectDeviceStateChange sends a device state change to any subscriber.
func (s *StubClient) InjectDeviceStateChange(dsc DeviceStateChange) {
	select {
	case s.deviceStateChangeCh <- dsc:
	default:
	}
}

// InjectDeviceEvent sends a device add/remove event to any subscriber.
func (s *StubClient) InjectDeviceEvent(de DeviceEvent) {
	select {
	case s.deviceEventCh <- de:
	default:
	}
}

// --- Client interface implementation ---

func (s *StubClient) WiFiDevices() ([]WiFiDevice, error) {
	s.record("WiFiDevices")
	return s.WiFiDevicesResult, s.WiFiDevicesErr
}

func (s *StubClient) EthernetDevices() ([]EthernetDevice, error) {
	s.record("EthernetDevices")
	return s.EthernetDevicesResult, s.EthernetDevicesErr
}

func (s *StubClient) Scan(device dbus.ObjectPath) ([]AccessPoint, error) {
	s.record("Scan", device)
	return s.ScanResult, s.ScanErr
}

func (s *StubClient) Connect(device dbus.ObjectPath, ssid, passphrase string) (dbus.ObjectPath, dbus.ObjectPath, error) {
	s.record("Connect", device, ssid, passphrase)
	return s.ConnectPath, s.ConnectActivePath, s.ConnectErr
}

func (s *StubClient) Disconnect(device dbus.ObjectPath) error {
	s.record("Disconnect", device)
	return s.DisconnectErr
}

func (s *StubClient) SavedWiFiConnections() ([]ConnectionProfile, error) {
	s.record("SavedWiFiConnections")
	return s.SavedConnections, s.SavedConnectionsErr
}

func (s *StubClient) DeleteConnection(path dbus.ObjectPath) error {
	s.record("DeleteConnection", path)
	return s.DeleteConnectionErr
}

func (s *StubClient) SnapshotConnection(path dbus.ObjectPath) (*ConnectionSnapshot, error) {
	s.record("SnapshotConnection", path)
	return s.SnapshotResult, s.SnapshotErr
}

func (s *StubClient) RestoreConnection(snapshot *ConnectionSnapshot) error {
	s.record("RestoreConnection", snapshot)
	return s.RestoreErr
}

func (s *StubClient) ActivateHotspot(device dbus.ObjectPath, ssid string, opts HotspotOpts) (dbus.ObjectPath, dbus.ObjectPath, error) {
	s.record("ActivateHotspot", device, ssid, opts)
	return s.ActivateHotspotActivePath, s.ActivateHotspotSettingsPath, s.ActivateHotspotErr
}

func (s *StubClient) DeactivateHotspot(device dbus.ObjectPath) error {
	s.record("DeactivateHotspot", device)
	return s.DeactivateHotspotErr
}

func (s *StubClient) Connectivity() (NMConnectivityState, error) {
	s.record("Connectivity")
	return s.ConnectivityResult, s.ConnectivityErr
}

func (s *StubClient) DeviceState(device dbus.ObjectPath) (NMDeviceState, error) {
	s.record("DeviceState", device)
	return s.DeviceStateResult, s.DeviceStateErr
}

func (s *StubClient) DeviceStateReason(device dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error) {
	s.record("DeviceStateReason", device)
	return s.DeviceStateResult, s.DeviceStateReasonResult, s.DeviceStateReasonErr
}

func (s *StubClient) ActiveConnectionInfo(device dbus.ObjectPath) (*ActiveConnectionInfo, error) {
	s.record("ActiveConnectionInfo", device)
	if s.ActiveConnErrByDevice != nil {
		if err, ok := s.ActiveConnErrByDevice[device]; ok {
			return nil, err
		}
	}
	if s.ActiveConnByDevice != nil {
		if info, ok := s.ActiveConnByDevice[device]; ok {
			return info, nil
		}
	}
	return s.ActiveConnResult, s.ActiveConnErr
}

func (s *StubClient) ActiveAccessPoint(device dbus.ObjectPath) (*AccessPoint, error) {
	s.record("ActiveAccessPoint", device)
	if s.ActiveAPErrByDevice != nil {
		if err, ok := s.ActiveAPErrByDevice[device]; ok {
			return nil, err
		}
	}
	if s.ActiveAPByDevice != nil {
		if ap, ok := s.ActiveAPByDevice[device]; ok {
			return ap, nil
		}
	}
	return s.ActiveAPResult, s.ActiveAPErr
}

func (s *StubClient) SignalStrength(device dbus.ObjectPath) (uint8, error) {
	s.record("SignalStrength", device)
	return s.SignalStrengthResult, s.SignalStrengthErr
}

func (s *StubClient) WirelessEnabled() (bool, error) {
	s.record("WirelessEnabled")
	return s.WirelessEnabledResult, s.WirelessEnabledErr
}

func (s *StubClient) SetWirelessEnabled(enabled bool) error {
	s.record("SetWirelessEnabled", enabled)
	return s.SetWirelessErr
}

func (s *StubClient) SubscribeStateChanges(ctx context.Context) (<-chan StateChange, error) {
	s.record("SubscribeStateChanges")
	ch := make(chan StateChange, 16)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case sc, ok := <-s.stateChangeCh:
				if !ok {
					return
				}
				select {
				case ch <- sc:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

func (s *StubClient) SubscribeDeviceStateChanges(ctx context.Context, device dbus.ObjectPath) (<-chan DeviceStateChange, error) {
	s.record("SubscribeDeviceStateChanges", device)
	ch := make(chan DeviceStateChange, 16)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case dsc, ok := <-s.deviceStateChangeCh:
				if !ok {
					return
				}
				// Filter to the requested device (or pass all if device is empty)
				if device != "" && dsc.Device != device {
					continue
				}
				select {
				case ch <- dsc:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

func (s *StubClient) SubscribeDeviceAddedRemoved(ctx context.Context) (<-chan DeviceEvent, error) {
	s.record("SubscribeDeviceAddedRemoved")
	ch := make(chan DeviceEvent, 8)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case de, ok := <-s.deviceEventCh:
				if !ok {
					return
				}
				select {
				case ch <- de:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

func (s *StubClient) WaitForActivation(_ context.Context, path dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error) {
	s.record("WaitForActivation", path)
	if path == "" {
		return NMDeviceStateUnknown, NMDeviceStateReasonNone, errEmptyActiveConnPath
	}
	return s.WaitForActivationState, s.WaitForActivationReason, s.WaitForActivationErr
}

func (s *StubClient) SubscribeActiveConnectionState(ctx context.Context, path dbus.ObjectPath) (<-chan ActiveConnectionStateChange, error) {
	s.record("SubscribeActiveConnectionState", path)
	ch := make(chan ActiveConnectionStateChange, 64)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-s.ActiveConnStateCh:
				if !ok {
					return
				}
				if path != "" && evt.Path != path {
					continue
				}
				select {
				case ch <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

// InjectActiveConnStateChange sends an active-conn state change to any subscriber.
func (s *StubClient) InjectActiveConnStateChange(evt ActiveConnectionStateChange) {
	select {
	case s.ActiveConnStateCh <- evt:
	default:
	}
}

func (s *StubClient) IsConnected() bool {
	return s.Connected
}

func (s *StubClient) Close() error {
	s.record("Close")
	return nil
}

// compile-time interface checks
var (
	_ Client = (*StubClient)(nil)
	_ Client = (*DBusClient)(nil)
)
