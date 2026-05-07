package nmclient

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// Connect creates or updates an NM connection profile for the given SSID and
// activates it on the device. It creates a new profile or reuses an existing
// one matching the SSID. Returns the new settings path and the active
// connection path. The latter is needed by WaitForActivation to disambiguate
// the requested connection from a still-Activated old profile (TOCTOU).
//
// Returns when the activation request is submitted (not when the connection
// is established — callers should watch DeviceStateChange).
func (c *DBusClient) Connect(device dbus.ObjectPath, ssid, passphrase string) (dbus.ObjectPath, dbus.ObjectPath, error) {
	// Check for existing profile for this SSID
	profiles, err := c.SavedWiFiConnections()
	if err != nil {
		return "", "", fmt.Errorf("nmclient: list saved connections: %w", err)
	}

	var existingPath dbus.ObjectPath
	for _, p := range profiles {
		if p.SSID == ssid {
			existingPath = p.Path
			break
		}
	}

	if existingPath != "" {
		// Delete the old profile and create a fresh one. NM's GetSettings
		// returns D-Bus types (e.g., ipv6.addresses as a(ayuay)) that don't
		// round-trip through Go's dbus library (serialized as aav), causing
		// Update to fail with type mismatches. Creating fresh avoids this.
		if err := c.obj(existingPath).Call(nmSettingsConnIface+".Delete", 0).Err; err != nil {
			return "", "", fmt.Errorf("nmclient: delete old profile: %w", err)
		}
	}

	// Create a new connection profile and activate it
	connSettings := map[string]map[string]dbus.Variant{
		"connection": {
			"id":          dbus.MakeVariant(ssid),
			"type":        dbus.MakeVariant("802-11-wireless"),
			"autoconnect": dbus.MakeVariant(true),
		},
		"802-11-wireless": {
			"ssid":   dbus.MakeVariant([]byte(ssid)),
			"mode":   dbus.MakeVariant("infrastructure"),
			"hidden": dbus.MakeVariant(false),
		},
		"802-11-wireless-security": {
			"key-mgmt": dbus.MakeVariant("wpa-psk"),
			"psk":      dbus.MakeVariant(passphrase),
		},
		"ipv4": {
			"method": dbus.MakeVariant("auto"),
		},
		"ipv6": {
			"method": dbus.MakeVariant("auto"),
		},
	}

	var settingsPath, activePath dbus.ObjectPath
	err = c.nm().Call(nmInterface+".AddAndActivateConnection", 0,
		connSettings, device, dbus.ObjectPath("/")).Store(&settingsPath, &activePath)
	return settingsPath, activePath, err
}

// Disconnect deactivates the active connection on the device.
func (c *DBusClient) Disconnect(device dbus.ObjectPath) error {
	v, err := c.prop(device, nmDeviceInterface, "ActiveConnection")
	if err != nil {
		return err
	}
	acPath, ok := v.Value().(dbus.ObjectPath)
	if !ok || !acPath.IsValid() || acPath == "/" {
		return nil // already disconnected
	}
	return c.nm().Call(nmInterface+".DeactivateConnection", 0, acPath).Err
}

// SavedWiFiConnections returns all persisted WiFi connection profiles.
func (c *DBusClient) SavedWiFiConnections() ([]ConnectionProfile, error) {
	settingsObj := c.conn.Object(nmBusName, "/org/freedesktop/NetworkManager/Settings")

	var connPaths []dbus.ObjectPath
	if err := settingsObj.Call(nmSettingsInterface+".ListConnections", 0).Store(&connPaths); err != nil {
		return nil, fmt.Errorf("nmclient: list connections: %w", err)
	}

	var profiles []ConnectionProfile
	for _, path := range connPaths {
		settings, err := c.getConnectionSettings(path)
		if err != nil {
			continue
		}

		connType := variantString(settings["connection"]["type"])
		if connType != "802-11-wireless" {
			continue
		}

		prof := ConnectionProfile{
			Path: path,
			ID:   variantString(settings["connection"]["id"]),
			UUID: variantString(settings["connection"]["uuid"]),
			Type: connType,
		}

		if ssidBytes, ok := settings["802-11-wireless"]["ssid"].Value().([]byte); ok {
			prof.SSID = string(ssidBytes)
		}

		profiles = append(profiles, prof)
	}
	return profiles, nil
}

// DeleteConnection removes a saved connection profile.
func (c *DBusClient) DeleteConnection(path dbus.ObjectPath) error {
	return c.obj(path).Call(nmSettingsConnIface+".Delete", 0).Err
}

// SnapshotConnection captures the full settings of a connection profile.
func (c *DBusClient) SnapshotConnection(path dbus.ObjectPath) (*ConnectionSnapshot, error) {
	settings, err := c.getConnectionSettings(path)
	if err != nil {
		return nil, err
	}

	// Also get secrets (NM strips them from GetSettings)
	secrets, err := c.getConnectionSecrets(path, "802-11-wireless-security")
	if err == nil {
		for section, vals := range secrets {
			if settings[section] == nil {
				settings[section] = vals
			} else {
				for k, v := range vals {
					settings[section][k] = v
				}
			}
		}
	}

	return &ConnectionSnapshot{
		Path:     path,
		Settings: settings,
	}, nil
}

// RestoreConnection recreates a WiFi connection profile from a snapshot.
// Builds clean settings from the snapshot's key fields to avoid D-Bus type
// mismatches (NM's GetSettings returns types like a(ayuay) that don't
// round-trip through godbus). This also handles the case where Connect()
// deleted the original profile (the snapshot path no longer exists).
func (c *DBusClient) RestoreConnection(snapshot *ConnectionSnapshot) error {
	// Extract the essential fields from the snapshot
	ssid := snapshot.SSID()
	psk := snapshot.PSK()
	connID := ""
	if conn, ok := snapshot.Settings["connection"]; ok {
		connID = variantString(conn["id"])
	}

	if ssid == "" {
		return fmt.Errorf("nmclient: restore: snapshot has no SSID")
	}
	if connID == "" {
		connID = ssid
	}

	// Build a clean profile with known-good D-Bus types
	cleanSettings := map[string]map[string]dbus.Variant{
		"connection": {
			"id":          dbus.MakeVariant(connID),
			"type":        dbus.MakeVariant("802-11-wireless"),
			"autoconnect": dbus.MakeVariant(true),
		},
		"802-11-wireless": {
			"ssid":   dbus.MakeVariant([]byte(ssid)),
			"mode":   dbus.MakeVariant("infrastructure"),
			"hidden": dbus.MakeVariant(false),
		},
		"ipv4": {
			"method": dbus.MakeVariant("auto"),
		},
		"ipv6": {
			"method": dbus.MakeVariant("auto"),
		},
	}
	if psk != "" {
		cleanSettings["802-11-wireless-security"] = map[string]dbus.Variant{
			"key-mgmt": dbus.MakeVariant("wpa-psk"),
			"psk":      dbus.MakeVariant(psk),
		}
	}

	// Use AddAndActivateConnection (not just AddConnection) so the restored
	// profile is immediately activated. AddConnection only saves to disk —
	// the device would stay disconnected until NM's autoconnect scanner
	// picks it up (30-120s+), during which the state machine could escalate
	// to AP mode.
	var settingsPath, activePath dbus.ObjectPath
	return c.nm().Call(nmInterface+".AddAndActivateConnection", 0,
		cleanSettings, snapshot.Device, dbus.ObjectPath("/")).Store(&settingsPath, &activePath)
}

// getConnectionSettings reads the settings dict from a connection profile.
func (c *DBusClient) getConnectionSettings(path dbus.ObjectPath) (map[string]map[string]dbus.Variant, error) {
	var settings map[string]map[string]dbus.Variant
	err := c.obj(path).Call(nmSettingsConnIface+".GetSettings", 0).Store(&settings)
	if err != nil {
		return nil, fmt.Errorf("nmclient: get settings %s: %w", path, err)
	}
	return settings, nil
}

// getConnectionSecrets reads secrets for a specific setting section.
func (c *DBusClient) getConnectionSecrets(path dbus.ObjectPath, section string) (map[string]map[string]dbus.Variant, error) {
	var secrets map[string]map[string]dbus.Variant
	err := c.obj(path).Call(nmSettingsConnIface+".GetSecrets", 0, section).Store(&secrets)
	return secrets, err
}

// variantString extracts a string from a dbus.Variant, returning "" if nil or wrong type.
func variantString(v dbus.Variant) string {
	if s, ok := v.Value().(string); ok {
		return s
	}
	return ""
}
