package nmclient

import (
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

// HotspotIDPrefix is the NM connection ID prefix for our AP hotspot profiles.
// Used to distinguish hotspot connections from STA connections.
const HotspotIDPrefix = "piccolo-ap-"

// ActivateHotspot configures and activates an open AP-mode hotspot on the
// given WiFi device. NM handles hostapd internally. The connection profile
// is created with the specified firewalld zone assignment.
func (c *DBusClient) ActivateHotspot(device dbus.ObjectPath, ssid string, opts HotspotOpts) error {
	settings := map[string]map[string]dbus.Variant{
		"connection": {
			"id":          dbus.MakeVariant(HotspotIDPrefix + ssid),
			"type":        dbus.MakeVariant("802-11-wireless"),
			"autoconnect": dbus.MakeVariant(false),
		},
		"802-11-wireless": {
			"ssid": dbus.MakeVariant([]byte(ssid)),
			"mode": dbus.MakeVariant("ap"),
		},
		"ipv4": {
			"method": dbus.MakeVariant("shared"),
		},
		"ipv6": {
			"method": dbus.MakeVariant("ignore"),
		},
	}

	// Assign firewalld zone on the connection so the AP interface gets the
	// correct zone automatically. Without this, NM assigns the default zone
	// and AP traffic reaches the GinServer instead of the captive portal.
	if opts.Zone != "" {
		settings["connection"]["zone"] = dbus.MakeVariant(opts.Zone)
	}

	// Channel and band selection
	if opts.Band != "" {
		settings["802-11-wireless"]["band"] = dbus.MakeVariant(opts.Band)
	}
	if opts.Channel > 0 {
		settings["802-11-wireless"]["channel"] = dbus.MakeVariant(uint32(opts.Channel))
	}

	// AP isolation: prevent clients from communicating with each other.
	// NM passes this through to hostapd as ap_isolate=1.
	settings["802-11-wireless"]["ap-isolation"] = dbus.MakeVariant(int32(1)) // NM_TERNARY_TRUE

	var settingsPath, activePath dbus.ObjectPath
	err := c.nm().Call(nmInterface+".AddAndActivateConnection", 0,
		settings, device, dbus.ObjectPath("/")).Store(&settingsPath, &activePath)
	if err != nil {
		return fmt.Errorf("nmclient: activate hotspot %q: %w", ssid, err)
	}
	return nil
}

// DeactivateHotspot tears down any active hotspot connection on the device
// and removes the transient connection profile.
func (c *DBusClient) DeactivateHotspot(device dbus.ObjectPath) error {
	// Find the active connection on this device
	v, err := c.prop(device, nmDeviceInterface, "ActiveConnection")
	if err != nil {
		return nil // no active connection
	}
	acPath, ok := v.Value().(dbus.ObjectPath)
	if !ok || !acPath.IsValid() || acPath == "/" {
		return nil
	}

	// Read the connection settings path for cleanup
	sv, err := c.prop(acPath, nmActiveConnInterface, "Connection")
	if err != nil {
		// Can't get connection path — just deactivate
		return c.nm().Call(nmInterface+".DeactivateConnection", 0, acPath).Err
	}
	connPath, _ := sv.Value().(dbus.ObjectPath)

	// Deactivate
	if err := c.nm().Call(nmInterface+".DeactivateConnection", 0, acPath).Err; err != nil {
		return fmt.Errorf("nmclient: deactivate hotspot: %w", err)
	}

	// Delete the transient hotspot profile (we create a new one each time)
	if connPath.IsValid() && connPath != "/" {
		// Check if it's our AP profile before deleting
		settings, err := c.getConnectionSettings(connPath)
		if err == nil {
			if id := variantString(settings["connection"]["id"]); strings.HasPrefix(id, HotspotIDPrefix) {
				_ = c.obj(connPath).Call(nmSettingsConnIface+".Delete", 0).Err
			}
		}
	}

	return nil
}
