package nmclient

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/runner"
)

// DBusClient is the production Client backed by the system D-Bus connection
// to NetworkManager. It reconnects automatically if the D-Bus connection drops,
// falling back to nmcli via runner.CommandRunner during outages.
type DBusClient struct {
	conn   *dbus.Conn
	runner runner.CommandRunner

	mu        sync.RWMutex
	connected atomic.Bool

	// Signal dispatch
	sigCancel context.CancelFunc
	sigDone   chan struct{}

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewDBusClient connects to the system D-Bus and verifies that NetworkManager
// is reachable. The runner is used as fallback for nmcli commands when D-Bus
// is unavailable, and for subprocess execution (arping, firewall-cmd, etc.).
func NewDBusClient(r runner.CommandRunner) (*DBusClient, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("nmclient: system bus connect: %w", err)
	}

	// Verify NM is reachable by reading its version.
	obj := conn.Object(nmBusName, dbus.ObjectPath(nmObjectPath))
	var version dbus.Variant
	if err := obj.Call(dbusPropertiesInterface+".Get", 0, nmInterface, "Version").Store(&version); err != nil {
		conn.Close()
		return nil, fmt.Errorf("nmclient: NetworkManager not reachable: %w", err)
	}
	log.Printf("INFO: nmclient: connected to NetworkManager %s", version.Value())

	c := &DBusClient{
		conn:   conn,
		runner: r,
		stopCh: make(chan struct{}),
	}
	c.connected.Store(true)

	return c, nil
}

// Close releases the D-Bus connection and stops all signal subscriptions.
func (c *DBusClient) Close() error {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		if c.sigCancel != nil {
			c.sigCancel()
		}
		if c.sigDone != nil {
			<-c.sigDone
		}
		c.conn.Close()
		c.connected.Store(false)
	})
	return nil
}

// IsConnected returns true if the D-Bus connection to NM is alive.
func (c *DBusClient) IsConnected() bool {
	return c.connected.Load()
}

// nm returns the NM root D-Bus object.
func (c *DBusClient) nm() dbus.BusObject {
	return c.conn.Object(nmBusName, dbus.ObjectPath(nmObjectPath))
}

// obj returns a D-Bus object at the given path on the NM bus.
func (c *DBusClient) obj(path dbus.ObjectPath) dbus.BusObject {
	return c.conn.Object(nmBusName, path)
}

// prop reads a D-Bus property from the given interface on the object.
func (c *DBusClient) prop(path dbus.ObjectPath, iface, property string) (dbus.Variant, error) {
	var v dbus.Variant
	err := c.obj(path).Call(dbusPropertiesInterface+".Get", 0, iface, property).Store(&v)
	if err != nil {
		return v, fmt.Errorf("nmclient: get %s.%s on %s: %w", iface, property, path, err)
	}
	return v, nil
}

// setProp sets a D-Bus property.
func (c *DBusClient) setProp(path dbus.ObjectPath, iface, property string, value interface{}) error {
	return c.obj(path).Call(dbusPropertiesInterface+".Set", 0, iface, property, dbus.MakeVariant(value)).Err
}

// Connectivity returns NM's assessment of internet connectivity.
func (c *DBusClient) Connectivity() (NMConnectivityState, error) {
	var state uint32
	err := c.nm().Call(nmInterface+".CheckConnectivity", 0).Store(&state)
	if err != nil {
		return NMConnectivityUnknown, fmt.Errorf("nmclient: check connectivity: %w", err)
	}
	return NMConnectivityState(state), nil
}

// DeviceState returns the current state of a specific device.
func (c *DBusClient) DeviceState(device dbus.ObjectPath) (NMDeviceState, error) {
	v, err := c.prop(device, nmDeviceInterface, "State")
	if err != nil {
		return NMDeviceStateUnknown, err
	}
	return NMDeviceState(v.Value().(uint32)), nil
}

// SignalStrength returns the WiFi signal strength (0–100) for the active AP.
func (c *DBusClient) SignalStrength(device dbus.ObjectPath) (uint8, error) {
	v, err := c.prop(device, nmWirelessInterface, "ActiveAccessPoint")
	if err != nil {
		return 0, err
	}
	apPath, ok := v.Value().(dbus.ObjectPath)
	if !ok || !apPath.IsValid() || apPath == "/" {
		return 0, nil // no active AP
	}
	sv, err := c.prop(apPath, nmAPInterface, "Strength")
	if err != nil {
		return 0, err
	}
	return sv.Value().(uint8), nil
}

// WirelessEnabled returns true if the WiFi radio is enabled.
func (c *DBusClient) WirelessEnabled() (bool, error) {
	v, err := c.prop(dbus.ObjectPath(nmObjectPath), nmInterface, "WirelessEnabled")
	if err != nil {
		return false, err
	}
	return v.Value().(bool), nil
}

// SetWirelessEnabled enables or disables the WiFi radio.
func (c *DBusClient) SetWirelessEnabled(enabled bool) error {
	return c.setProp(dbus.ObjectPath(nmObjectPath), nmInterface, "WirelessEnabled", enabled)
}

// ActiveConnectionInfo returns details about the active connection on a device.
func (c *DBusClient) ActiveConnectionInfo(device dbus.ObjectPath) (*ActiveConnectionInfo, error) {
	v, err := c.prop(device, nmDeviceInterface, "ActiveConnection")
	if err != nil {
		return nil, err
	}
	acPath, ok := v.Value().(dbus.ObjectPath)
	if !ok || !acPath.IsValid() || acPath == "/" {
		return nil, nil // no active connection
	}

	info := &ActiveConnectionInfo{Path: acPath}

	if uuidV, err := c.prop(acPath, nmActiveConnInterface, "Uuid"); err == nil {
		info.UUID, _ = uuidV.Value().(string)
	}
	if idV, err := c.prop(acPath, nmActiveConnInterface, "Id"); err == nil {
		info.ID, _ = idV.Value().(string)
	}
	if stateV, err := c.prop(acPath, nmActiveConnInterface, "State"); err == nil {
		info.State, _ = stateV.Value().(uint32)
	}
	if typeV, err := c.prop(acPath, nmActiveConnInterface, "Type"); err == nil {
		info.Type, _ = typeV.Value().(string)
	}

	// Get IP4 config
	if ip4V, err := c.prop(acPath, nmActiveConnInterface, "Ip4Config"); err == nil {
		ip4Path, _ := ip4V.Value().(dbus.ObjectPath)
		if ip4Path.IsValid() && ip4Path != "/" {
			info.IP4Address, info.IP4Gateway = c.readIP4Config(ip4Path)
		}
	}

	return info, nil
}

// readIP4Config extracts the first address and gateway from an IP4Config object.
func (c *DBusClient) readIP4Config(path dbus.ObjectPath) (addr, gw string) {
	// Gateway
	if gwV, err := c.prop(path, nmIP4ConfigInterface, "Gateway"); err == nil {
		gw, _ = gwV.Value().(string)
	}

	// AddressData is an array of dicts [{address: "x.x.x.x", prefix: N}, ...]
	if addrV, err := c.prop(path, nmIP4ConfigInterface, "AddressData"); err == nil {
		if addrs, ok := addrV.Value().([]map[string]dbus.Variant); ok && len(addrs) > 0 {
			if a, ok := addrs[0]["address"]; ok {
				addr, _ = a.Value().(string)
			}
		}
	}
	return
}

// WaitForActivation blocks until the device reaches Activated or a terminal
// failure state. A second D-Bus subscription is safe: match rules are
// reference-counted, and godbus fans out signals to all registered channels.
func (c *DBusClient) WaitForActivation(ctx context.Context, device dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error) {
	// Create a child context so we cancel the D-Bus subscription immediately
	// on return, rather than leaking it until the caller's context expires.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	ch, err := c.SubscribeDeviceStateChanges(subCtx, device)
	if err != nil {
		return NMDeviceStateUnknown, NMDeviceStateReasonNone, fmt.Errorf("nmclient: subscribe for activation wait: %w", err)
	}

	// TOCTOU guard: check current state before entering the wait loop.
	// If NM already activated, failed, or raced through failure back to
	// disconnected before we polled, return now to avoid a long stall.
	curState, err := c.DeviceState(device)
	if err == nil {
		switch {
		case curState == NMDeviceStateActivated:
			return NMDeviceStateActivated, NMDeviceStateReasonNone, nil
		case curState == NMDeviceStateFailed:
			return NMDeviceStateFailed, NMDeviceStateReasonUnknown, nil
		case curState <= NMDeviceStateDisconnected:
			// Device is at Disconnected/Unavailable/Unmanaged — activation
			// either hasn't started or already failed and was torn down.
			// Check for an active connection to distinguish: if none exists,
			// the device raced through failure before we got here.
			if info, infoErr := c.ActiveConnectionInfo(device); infoErr != nil || info == nil {
				return curState, NMDeviceStateReasonUnknown, nil
			}
		}
	}

	// Wait for state transitions. Terminal conditions:
	//   Activated (100)           → success
	//   Failed (120)              → activation failed (reason in event)
	//   Disconnected (30) or below, after seeing a progressing state (>=Prepare/40)
	//     → activation was torn down
	seenProgressing := curState >= NMDeviceStatePrepare
	for {
		select {
		case <-ctx.Done():
			return NMDeviceStateUnknown, NMDeviceStateReasonNone, ctx.Err()
		case evt, ok := <-ch:
			if !ok {
				return NMDeviceStateUnknown, NMDeviceStateReasonNone, fmt.Errorf("nmclient: device state channel closed")
			}
			if evt.NewState >= NMDeviceStatePrepare {
				seenProgressing = true
			}
			switch {
			case evt.NewState == NMDeviceStateActivated:
				return NMDeviceStateActivated, evt.Reason, nil
			case evt.NewState == NMDeviceStateFailed:
				return NMDeviceStateFailed, evt.Reason, nil
			case seenProgressing && evt.NewState <= NMDeviceStateDisconnected:
				// Device regressed to disconnected/unmanaged/unavailable after
				// progressing — activation was torn down.
				return evt.NewState, evt.Reason, nil
			}
		}
	}
}

