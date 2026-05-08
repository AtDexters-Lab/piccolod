package nmclient

import (
	"context"
	"errors"
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

// NewDBusClient connects to the shared system D-Bus and verifies that
// NetworkManager is reachable. The runner is used as fallback for nmcli
// commands when D-Bus is unavailable, and for subprocess execution (arping,
// firewall-cmd, etc.).
//
// NOTE: the shared system bus singleton dispatches signals connection-wide
// to all subscribers. If two subsystems register signal handlers on the
// same connection, signals fan out to both. Use NewPrivateDBusClient when
// you need an isolated signal dispatch — e.g., the network supervisor.
func NewDBusClient(r runner.CommandRunner) (*DBusClient, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("nmclient: system bus connect: %w", err)
	}
	return finishNewDBusClient(conn, r)
}

// OpenPrivateSystemBus opens a NEW (private) connection to the system bus —
// NOT the shared singleton — completing the SystemBusPrivate + Auth + Hello
// handshake. Returns the raw connection without any NM-reachability check;
// callers needing the full DBusClient should use NewPrivateDBusClient.
//
// A private connection is required when the caller registers signal
// handlers (godbus's `conn.Signal(sigCh)` is connection-wide and would
// fan out signals to every subscriber on a shared connection) or exports
// D-Bus services. The caller owns the connection and must Close() it.
func OpenPrivateSystemBus() (*dbus.Conn, error) {
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("nmclient: private system bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("nmclient: private bus auth: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("nmclient: private bus hello: %w", err)
	}
	return conn, nil
}

// NewPrivateDBusClient establishes a private connection to the system bus
// and verifies NetworkManager is reachable. This is the constructor the
// network supervisor's probe layer must use — see OpenPrivateSystemBus
// for the rationale on private vs shared connections.
//
// The caller is responsible for calling Close() when done.
func NewPrivateDBusClient(r runner.CommandRunner) (*DBusClient, error) {
	conn, err := OpenPrivateSystemBus()
	if err != nil {
		return nil, err
	}
	return finishNewDBusClient(conn, r)
}

// finishNewDBusClient verifies NM reachability and constructs the client.
// Used by both the shared and private constructors.
func finishNewDBusClient(conn *dbus.Conn, r runner.CommandRunner) (*DBusClient, error) {
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

// Connectivity returns NM's cached connectivity classification by reading
// the Connectivity *property*. This is a passive read of NM's last-known
// state — it does NOT dispatch a fresh check. Calling NM's CheckConnectivity
// method instead would force NM to issue an HTTP GET to its configured
// connectivity-check-uri (default https://nmcheck.gnome.org/...), turning
// every probe tick into unsolicited external traffic — explicitly forbidden
// by the supervisor design (RFC 20260505 §"Probes" / "Risks": "no NM
// external-probe externality"). The supervisor uses TCP-connect probes
// for L3 truth; this method is advisory diagnostic only.
func (c *DBusClient) Connectivity() (NMConnectivityState, error) {
	v, err := c.prop(dbus.ObjectPath(nmObjectPath), nmInterface, "Connectivity")
	if err != nil {
		return NMConnectivityUnknown, fmt.Errorf("nmclient: read connectivity property: %w", err)
	}
	s, ok := v.Value().(uint32)
	if !ok {
		return NMConnectivityUnknown, fmt.Errorf("nmclient: unexpected Connectivity type %T", v.Value())
	}
	return NMConnectivityState(s), nil
}

// DeviceState returns the current state of a specific device.
func (c *DBusClient) DeviceState(device dbus.ObjectPath) (NMDeviceState, error) {
	v, err := c.prop(device, nmDeviceInterface, "State")
	if err != nil {
		return NMDeviceStateUnknown, err
	}
	return NMDeviceState(v.Value().(uint32)), nil
}

// DeviceStateReason reads the cached (state, reason) pair for a device via
// the StateReason property. Useful at startup when the supervisor has not
// yet observed any StateChanged signals — without this, callers fall back
// to "no reason known" and may misclassify a persistent NoSecrets state.
//
// Returns the most recent state-reason pair NM is tracking; on older NM
// builds that lack the StateReason property, falls back to (DeviceState,
// NMDeviceStateReasonUnknown).
func (c *DBusClient) DeviceStateReason(device dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error) {
	v, err := c.prop(device, nmDeviceInterface, "StateReason")
	if err != nil {
		// Fallback to State alone — best effort.
		st, sErr := c.DeviceState(device)
		if sErr != nil {
			return NMDeviceStateUnknown, NMDeviceStateReasonNone, err
		}
		return st, NMDeviceStateReasonUnknown, nil
	}
	// StateReason is exposed as a struct of (uint32 state, uint32 reason).
	if pair, ok := v.Value().([]interface{}); ok && len(pair) == 2 {
		state, _ := pair[0].(uint32)
		reason, _ := pair[1].(uint32)
		return NMDeviceState(state), NMDeviceStateReason(reason), nil
	}
	// Unexpected encoding — fall back to State alone.
	st, _ := c.DeviceState(device)
	return st, NMDeviceStateReasonUnknown, nil
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

// errEmptyActiveConnPath is returned by WaitForActivation when called with
// an empty expectedActiveConn. Path-scoped wait requires a path; both
// production callers always pass one post-RFC.
var errEmptyActiveConnPath = errors.New("nmclient: WaitForActivation requires non-empty expectedActiveConn")

// WaitForActivation blocks until the active-connection path reaches
// Activated or a terminal failure state. Path-scoped: subscribes to
// PropertiesChanged on the specific D-Bus object returned by
// AddAndActivateConnection. Immune to OLD-connection contamination from
// concurrent teardowns on the same device.
//
// See RFC 20260508 for the full design.
func (c *DBusClient) WaitForActivation(ctx context.Context, path dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error) {
	if path == "" {
		return NMDeviceStateUnknown, NMDeviceStateReasonNone, errEmptyActiveConnPath
	}

	// Cancel the subscription on return rather than leaking until ctx expires.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	ch, err := c.SubscribeActiveConnectionState(subCtx, path)
	if err != nil {
		return NMDeviceStateUnknown, NMDeviceStateReasonNone, fmt.Errorf("nmclient: subscribe for activation wait: %w", err)
	}

	// TOCTOU guard: read State after subscribe (subscribe-before-read order
	// ensures we don't miss transitions in the gap; double-observation is
	// idempotent on terminal values).
	state, reason, terminal, err := c.readActiveConnTerminalState(path)
	if err != nil {
		return NMDeviceStateUnknown, NMDeviceStateReasonNone, err
	}
	if terminal {
		return translateActiveConnTerminalPure(path, state, reason)
	}

	return runActivationWaitByPath(ctx, ch, c.readActiveConnTerminalState, path)
}

// terminalReader resolves State for a path. Used by runActivationWaitByPath
// to dispatch D1 case-C (invalidated[State] → Get) and as the test seam
// for that path.
type terminalReader func(path dbus.ObjectPath) (NMActiveConnectionState, NMActiveConnectionStateReason, bool, error)

// readActiveConnTerminalState performs the D4 TOCTOU Properties.Get and
// classifies the outcome per D4 sub-cases (i)–(v). Returns:
//   - (state, reason, true, nil) if Get returned a terminal state
//   - (Activating, Unknown, false, nil) for non-terminal or transient errors
//   - (_, _, true, nil) with state=Deactivated for UnknownObject (D4-iv)
//   - (_, _, _, err) only on programmer-error class issues
func (c *DBusClient) readActiveConnTerminalState(path dbus.ObjectPath) (NMActiveConnectionState, NMActiveConnectionStateReason, bool, error) {
	v, err := c.prop(path, nmActiveConnInterface, "State")
	if err != nil {
		// D4-iv: path-removed-class errors synthesize terminal Deactivated
		// with the path_removed token (logged below). Other errors fall
		// through to the loop, which is floored by ctx-timeout.
		if isUnknownObjectErr(err) {
			log.Printf("WARN: nmclient: WaitForActivation path-removed-on-toctou path=%s synth=path_removed", path)
			return NMActiveConnectionStateDeactivated, NMActiveConnectionStateReasonUnknown, true, nil
		}
		log.Printf("WARN: nmclient: WaitForActivation TOCTOU State read failed path=%s err=%v (proceeding to signal loop)", path, err)
		return NMActiveConnectionStateActivating, NMActiveConnectionStateReasonUnknown, false, nil
	}
	u, ok := v.Value().(uint32)
	if !ok {
		return NMActiveConnectionStateActivating, NMActiveConnectionStateReasonUnknown, false, nil
	}
	state := NMActiveConnectionState(u)
	switch state {
	case NMActiveConnectionStateActivated, NMActiveConnectionStateDeactivating, NMActiveConnectionStateDeactivated:
		// D4-i: terminal — read Reason for additional context (best-effort).
		reason := readActiveConnReason(c, path)
		return state, reason, true, nil
	default:
		// D4-ii (Activating) and D4-iii (Unknown) — proceed to signal loop.
		return state, NMActiveConnectionStateReasonUnknown, false, nil
	}
}

// readActiveConnReason reads the Reason property for additional failure
// context. Best-effort: returns Unknown(0) on any read error.
func readActiveConnReason(c *DBusClient, path dbus.ObjectPath) NMActiveConnectionStateReason {
	v, err := c.prop(path, nmActiveConnInterface, "Reason")
	if err != nil {
		return NMActiveConnectionStateReasonUnknown
	}
	if u, ok := v.Value().(uint32); ok {
		return NMActiveConnectionStateReason(u)
	}
	return NMActiveConnectionStateReasonUnknown
}

// runActivationWaitByPath drives the post-TOCTOU signal loop. Exposed as a
// package-private helper for unit tests; production callers use
// WaitForActivation. `read` is the function-typed seam tests substitute to
// inject the D1 case-C (invalidated[State] → Get) outcome without a live
// D-Bus.
func runActivationWaitByPath(ctx context.Context, ch <-chan ActiveConnectionStateChange, read terminalReader, path dbus.ObjectPath) (NMDeviceState, NMDeviceStateReason, error) {
	for {
		select {
		case <-ctx.Done():
			return NMDeviceStateUnknown, NMDeviceStateReasonNone, ctx.Err()
		case evt, ok := <-ch:
			if !ok {
				return NMDeviceStateUnknown, NMDeviceStateReasonNone, fmt.Errorf("nmclient: active-conn state channel closed")
			}
			// D1 case-C: NewState=Unknown signals invalidated[State] dispatch.
			// Issue Properties.Get via the injected reader to resolve State.
			if evt.NewState == NMActiveConnectionStateUnknown {
				state, reason, terminal, err := read(path)
				if err != nil {
					return NMDeviceStateUnknown, NMDeviceStateReasonNone, err
				}
				if terminal {
					return translateActiveConnTerminalPure(path, state, reason)
				}
				continue
			}
			// Terminal states: Activated (success), Deactivating, Deactivated.
			if evt.NewState == NMActiveConnectionStateActivated ||
				evt.NewState == NMActiveConnectionStateDeactivating ||
				evt.NewState == NMActiveConnectionStateDeactivated {
				return translateActiveConnTerminalPure(path, evt.NewState, evt.Reason)
			}
			// Activating — non-terminal, keep waiting.
		}
	}
}

// translateActiveConnTerminalPure maps an active-conn terminal
// (state, reason) to the NMDeviceState{Reason} pair the caller expects.
// Emits the translation-fallthrough fingerprint log when the ACR maps to
// Unknown (D3).
func translateActiveConnTerminalPure(path dbus.ObjectPath, state NMActiveConnectionState, acReason NMActiveConnectionStateReason) (NMDeviceState, NMDeviceStateReason, error) {
	devReason := translateActiveConnReason(acReason)
	if devReason == NMDeviceStateReasonUnknown && acReason != NMActiveConnectionStateReasonUnknown {
		log.Printf("WARN: nmclient: WaitForActivation translation-fallthrough path=%s ac_reason=%d", path, acReason)
	}
	switch state {
	case NMActiveConnectionStateActivated:
		return NMDeviceStateActivated, devReason, nil
	case NMActiveConnectionStateDeactivating, NMActiveConnectionStateDeactivated:
		return NMDeviceStateDisconnected, devReason, nil
	case NMActiveConnectionStateUnknown:
		return NMDeviceStateUnknown, devReason, nil
	default:
		return NMDeviceStateUnknown, devReason, nil
	}
}

// translateActiveConnReason maps NMActiveConnectionStateReason to
// NMDeviceStateReason for caller compatibility. Total over defined ACR
// values; unmapped values return Unknown(1) and trigger the
// translation-fallthrough fingerprint log.
//
// brcmfmac firmware-failure modes (DependencyFailed, DeviceRealizeFailed,
// DeviceRemoved) are explicitly enumerated to prevent operator-triage
// degradation on the headline hardware.
func translateActiveConnReason(r NMActiveConnectionStateReason) NMDeviceStateReason {
	switch r {
	case NMActiveConnectionStateReasonNone:
		return NMDeviceStateReasonNone
	case NMActiveConnectionStateReasonUserDisconnected, NMActiveConnectionStateReasonDeviceDisconnect:
		return NMDeviceStateReasonUserRequested
	case NMActiveConnectionStateReasonServiceStopped:
		return NMDeviceStateReasonSupplicantDisconnect
	case NMActiveConnectionStateReasonIPConfigInvalid:
		return NMDeviceStateReasonConfigFailed
	case NMActiveConnectionStateReasonConnectTimeout, NMActiveConnectionStateReasonServiceStartTimeout:
		return NMDeviceStateReasonSupplicantTimeout
	case NMActiveConnectionStateReasonServiceStartFailed:
		return NMDeviceStateReasonSupplicantFailed
	case NMActiveConnectionStateReasonNoSecrets:
		return NMDeviceStateReasonNoSecrets
	case NMActiveConnectionStateReasonLoginFailed:
		return NMDeviceStateReasonSupplicantFailed
	case NMActiveConnectionStateReasonConnectionRemoved:
		return NMDeviceStateReasonConnectionRemoved
	case NMActiveConnectionStateReasonDependencyFailed:
		return NMDeviceStateReasonConfigFailed
	case NMActiveConnectionStateReasonDeviceRealizeFailed:
		return NMDeviceStateReasonFirmwareMissing
	case NMActiveConnectionStateReasonDeviceRemoved:
		return NMDeviceStateReasonRemoved
	default:
		return NMDeviceStateReasonUnknown
	}
}

// isUnknownObjectErr returns true for D-Bus errors indicating the path
// is no longer registered with NM. godbus's dbus.Error.Error() returns
// the human Body, not the Name — match on Name via errors.As to avoid
// fragile substring matching on a non-contractual format.
func isUnknownObjectErr(err error) bool {
	if err == nil {
		return false
	}
	var dbusErr dbus.Error
	if !errors.As(err, &dbusErr) {
		return false
	}
	switch dbusErr.Name {
	case "org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownProperty":
		return true
	}
	return false
}

