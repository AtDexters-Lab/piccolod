package nmclient

import (
	"context"
	"log"

	"github.com/godbus/dbus/v5"
)

// SubscribeStateChanges returns a channel that receives NM daemon state
// changes. The channel is closed when the context is cancelled.
func (c *DBusClient) SubscribeStateChanges(ctx context.Context) (<-chan StateChange, error) {
	rule := "type='signal',interface='" + nmInterface + "',member='StateChanged',path='" + nmObjectPath + "'"
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return nil, err
	}

	ch := make(chan StateChange, 16)
	sigCh := make(chan *dbus.Signal, 32)
	c.conn.Signal(sigCh)

	go func() {
		defer close(ch)
		defer c.conn.RemoveSignal(sigCh)
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				if sig.Name != nmInterface+".StateChanged" {
					continue
				}
				if len(sig.Body) < 1 {
					continue
				}
				newState, ok := sig.Body[0].(uint32)
				if !ok {
					continue
				}
				select {
				case ch <- StateChange{NewState: NMState(newState)}:
				default:
					log.Printf("WARN: nmclient: state change channel full, dropping event")
				}
			}
		}
	}()

	return ch, nil
}

// SubscribeDeviceStateChanges returns a channel that receives state changes
// for a specific device. The channel is closed when the context is cancelled.
func (c *DBusClient) SubscribeDeviceStateChanges(ctx context.Context, device dbus.ObjectPath) (<-chan DeviceStateChange, error) {
	rule := "type='signal',interface='" + nmDeviceInterface + "',member='StateChanged',path='" + string(device) + "'"
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return nil, err
	}

	ch := make(chan DeviceStateChange, 16)
	sigCh := make(chan *dbus.Signal, 32)
	c.conn.Signal(sigCh)

	go func() {
		defer close(ch)
		defer c.conn.RemoveSignal(sigCh)
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				if sig.Name != nmDeviceInterface+".StateChanged" {
					continue
				}
				if sig.Path != device {
					continue
				}
				if len(sig.Body) < 3 {
					continue
				}
				newState, ok1 := sig.Body[0].(uint32)
				oldState, ok2 := sig.Body[1].(uint32)
				reason, ok3 := sig.Body[2].(uint32)
				if !ok1 || !ok2 || !ok3 {
					continue
				}
				evt := DeviceStateChange{
					Device:   device,
					NewState: NMDeviceState(newState),
					OldState: NMDeviceState(oldState),
					Reason:   NMDeviceStateReason(reason),
				}
				select {
				case ch <- evt:
				default:
					log.Printf("WARN: nmclient: device state change channel full, dropping event")
				}
			}
		}
	}()

	return ch, nil
}

// SubscribeActiveConnectionState returns a channel that receives State
// property changes for a specific active-connection D-Bus object. Path-scoped
// (vs SubscribeDeviceStateChanges which is device-scoped); a wait function
// for one activation watches only its own path's stream, immune to OLD-
// connection contamination from other paths on the same device.
//
// Buffer size 64 (vs 16 for device-scoped subs): a dropped terminal State
// event is correctness-affecting for path-scoped consumers (the wait loop
// would fall through to ctx-timeout instead of returning the right answer).
// 64 is generous for a single-path subscription's expected emission rate
// (a full Activating→Activated lifecycle emits ~5 events).
//
// Filter chain: AddMatch by path + interface + member at the broker; in-
// process re-checks path, signal name, and body[0]==Connection.Active
// interface as defense-in-depth against godbus's connection-wide signal
// fan-out (multiple subscriptions on the shared connection deliver every
// signal to every registered channel).
//
// PropertiesChanged payload: (interface, changed dict, invalidated array).
// Three signal cases:
//   - changed["State"] present       → emit ActiveConnectionStateChange
//   - changed["State"] absent        → ignore (non-State property changed)
//   - "State" in invalidated[]       → emit with NewState=Unknown(0); the
//     consumer (WaitForActivation) is expected to issue Properties.Get to
//     resolve the actual State (NM emits invalidated[] when the property
//     value cannot be efficiently conveyed, typically during object-
//     lifecycle teardown)
//
// "Reason" property is read alongside "State" when present in the same
// PropertiesChanged dict; if absent, defaults to Unknown(0). The consumer
// re-translates via translateActiveConnReason at the WaitForActivation
// boundary.
func (c *DBusClient) SubscribeActiveConnectionState(ctx context.Context, path dbus.ObjectPath) (<-chan ActiveConnectionStateChange, error) {
	rule := "type='signal',interface='" + dbusPropertiesInterface + "',member='PropertiesChanged',path='" + string(path) + "'"
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return nil, err
	}

	ch := make(chan ActiveConnectionStateChange, 64)
	sigCh := make(chan *dbus.Signal, 32)
	c.conn.Signal(sigCh)

	go func() {
		defer close(ch)
		defer c.conn.RemoveSignal(sigCh)
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				evt, ok := decodeActiveConnPropertiesChanged(sig, path)
				if !ok {
					continue
				}
				select {
				case ch <- evt:
				default:
					log.Printf("WARN: nmclient: active-conn state change channel full, dropping event path=%s", path)
				}
			}
		}
	}()

	return ch, nil
}

// decodeActiveConnPropertiesChanged is the pure D6 filter chain + payload
// decoder, exposed for unit testing without a live D-Bus. Returns (evt,
// true) for signals that should be emitted to the subscriber, (_, false)
// for signals that should be dropped.
//
// The three filter steps match the contract in SubscribeActiveConnectionState's
// docstring: path-exact, signal-name PropertiesChanged, body[0] interface
// equals Connection.Active. Defense-in-depth against godbus's connection-
// wide signal fan-out: even though the AddMatch broker rule already filters,
// shared subscriptions on the same connection deliver every signal to every
// channel, so foreign signals can reach this goroutine.
//
// State decoding handles all three RFC D1 cases: changed[State] present
// (case A), changed[State] absent (case B → drop), invalidated[State]
// present (case C → emit with NewState=Unknown(0) sentinel; consumer
// resolves via Properties.Get).
func decodeActiveConnPropertiesChanged(sig *dbus.Signal, expectedPath dbus.ObjectPath) (ActiveConnectionStateChange, bool) {
	var zero ActiveConnectionStateChange
	if sig.Path != expectedPath {
		return zero, false
	}
	if sig.Name != dbusPropertiesInterface+".PropertiesChanged" {
		return zero, false
	}
	if len(sig.Body) < 3 {
		return zero, false
	}
	iface, ok := sig.Body[0].(string)
	if !ok || iface != nmActiveConnInterface {
		return zero, false
	}
	changed, _ := sig.Body[1].(map[string]dbus.Variant)
	invalidated, _ := sig.Body[2].([]string)

	var (
		newState     NMActiveConnectionState
		reason       NMActiveConnectionStateReason
		stateUpdated bool
	)
	if v, ok := changed["State"]; ok {
		if u, ok := v.Value().(uint32); ok {
			newState = NMActiveConnectionState(u)
			stateUpdated = true
		}
	}
	if !stateUpdated {
		for _, name := range invalidated {
			if name == "State" {
				newState = NMActiveConnectionStateUnknown
				stateUpdated = true
				break
			}
		}
	}
	if !stateUpdated {
		return zero, false
	}
	if v, ok := changed["Reason"]; ok {
		if u, ok := v.Value().(uint32); ok {
			reason = NMActiveConnectionStateReason(u)
		}
	}
	return ActiveConnectionStateChange{Path: expectedPath, NewState: newState, Reason: reason}, true
}

// SubscribeDeviceAddedRemoved returns a channel that receives events when
// devices are added to or removed from NetworkManager (USB hotplug, etc.).
func (c *DBusClient) SubscribeDeviceAddedRemoved(ctx context.Context) (<-chan DeviceEvent, error) {
	addRule := "type='signal',interface='" + nmInterface + "',member='DeviceAdded',path='" + nmObjectPath + "'"
	remRule := "type='signal',interface='" + nmInterface + "',member='DeviceRemoved',path='" + nmObjectPath + "'"

	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, addRule).Err; err != nil {
		return nil, err
	}
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, remRule).Err; err != nil {
		return nil, err
	}

	ch := make(chan DeviceEvent, 8)
	sigCh := make(chan *dbus.Signal, 16)
	c.conn.Signal(sigCh)

	go func() {
		defer close(ch)
		defer c.conn.RemoveSignal(sigCh)
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, addRule)
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, remRule)
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				var evtType DeviceEventType
				switch sig.Name {
				case nmInterface + ".DeviceAdded":
					evtType = DeviceAdded
				case nmInterface + ".DeviceRemoved":
					evtType = DeviceRemoved
				default:
					continue
				}
				if len(sig.Body) < 1 {
					continue
				}
				devPath, ok := sig.Body[0].(dbus.ObjectPath)
				if !ok {
					continue
				}
				select {
				case ch <- DeviceEvent{Type: evtType, Device: devPath}:
				default:
					log.Printf("WARN: nmclient: device event channel full, dropping")
				}
			}
		}
	}()

	return ch, nil
}
