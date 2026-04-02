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
