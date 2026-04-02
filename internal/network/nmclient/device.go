package nmclient

import (
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/godbus/dbus/v5"
)

// WiFiDevices returns all WiFi-capable network devices, sorted by interface
// name (lowest wlan* index first per device selection policy).
func (c *DBusClient) WiFiDevices() ([]WiFiDevice, error) {
	return c.devicesByType(NMDeviceTypeWiFi)
}

// EthernetDevices returns all Ethernet network devices.
func (c *DBusClient) EthernetDevices() ([]EthernetDevice, error) {
	paths, err := c.allDevicePaths()
	if err != nil {
		return nil, err
	}

	var devices []EthernetDevice
	for _, path := range paths {
		dt, err := c.deviceType(path)
		if err != nil || dt != NMDeviceTypeEthernet {
			continue
		}

		dev := EthernetDevice{Path: path}

		if v, err := c.prop(path, nmDeviceInterface, "Interface"); err == nil {
			dev.Interface, _ = v.Value().(string)
		}
		if v, err := c.prop(path, nmDeviceInterface, "HwAddress"); err == nil {
			if hwStr, ok := v.Value().(string); ok {
				dev.HWAddress, _ = net.ParseMAC(hwStr)
			}
		}
		if v, err := c.prop(path, nmDeviceInterface, "State"); err == nil {
			dev.State = NMDeviceState(v.Value().(uint32))
		}
		// Carrier: physical link state (for Ethernet only)
		if v, err := c.prop(path, "org.freedesktop.NetworkManager.Device.Wired", "Carrier"); err == nil {
			dev.Carrier, _ = v.Value().(bool)
		}

		devices = append(devices, dev)
	}
	return devices, nil
}

// Scan triggers a WiFi scan on the given device and returns visible APs.
func (c *DBusClient) Scan(device dbus.ObjectPath) ([]AccessPoint, error) {
	// Read LastScan timestamp before requesting a new scan so we can detect
	// when NM finishes. LastScan is available since NM 1.12 (CLOCK_BOOTTIME ms).
	var lastScanBefore int64
	canPoll := false
	if lsV, err := c.prop(device, nmWirelessInterface, "LastScan"); err == nil {
		if ts, ok := lsV.Value().(int64); ok && ts > 0 {
			lastScanBefore = ts
			canPoll = true
		}
	}

	// Request a fresh scan. This is asynchronous — NM starts scanning in the
	// background and updates AccessPoints when done.
	opts := map[string]dbus.Variant{}
	if err := c.obj(device).Call(nmWirelessInterface+".RequestScan", 0, opts).Err; err != nil {
		// NM may return an error if a scan is already in progress — not fatal.
		// We still read the current AP list below.
	}

	// Wait for NM to complete the scan.
	if canPoll {
		// Poll LastScan for a change (up to 6s, leaving headroom within the
		// captive portal's WriteTimeout). Check before sleeping so fast scans
		// are detected promptly.
		for i := 0; i < 12; i++ {
			if lsV, err := c.prop(device, nmWirelessInterface, "LastScan"); err == nil {
				if ts, ok := lsV.Value().(int64); ok && ts > lastScanBefore {
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		// LastScan not available (NM < 1.12 or read failed). Fixed 3s wait
		// as fallback — long enough for most scans, short enough to not annoy.
		time.Sleep(3 * time.Second)
	}

	// Read the list of access points from the (now-updated) cache.
	v, err := c.prop(device, nmWirelessInterface, "AccessPoints")
	if err != nil {
		return nil, fmt.Errorf("nmclient: list APs: %w", err)
	}
	apPaths, ok := v.Value().([]dbus.ObjectPath)
	if !ok {
		return nil, nil
	}

	seen := make(map[string]AccessPoint) // deduplicate by SSID
	for _, apPath := range apPaths {
		ap := AccessPoint{Path: apPath}

		if sv, err := c.prop(apPath, nmAPInterface, "Ssid"); err == nil {
			if ssidBytes, ok := sv.Value().([]byte); ok {
				ap.SSID = string(ssidBytes)
			}
		}
		if ap.SSID == "" {
			continue // skip hidden networks
		}
		if sv, err := c.prop(apPath, nmAPInterface, "HwAddress"); err == nil {
			ap.HWAddress, _ = sv.Value().(string)
		}
		if sv, err := c.prop(apPath, nmAPInterface, "Frequency"); err == nil {
			ap.Frequency, _ = sv.Value().(uint32)
		}
		if sv, err := c.prop(apPath, nmAPInterface, "Strength"); err == nil {
			ap.Strength, _ = sv.Value().(uint8)
		}
		if sv, err := c.prop(apPath, nmAPInterface, "RsnFlags"); err == nil {
			ap.RSNFlags = NM80211ApSecurityFlags(sv.Value().(uint32))
		}
		if sv, err := c.prop(apPath, nmAPInterface, "WpaFlags"); err == nil {
			ap.WPAFlags = NM80211ApSecurityFlags(sv.Value().(uint32))
		}
		if sv, err := c.prop(apPath, nmAPInterface, "Flags"); err == nil {
			ap.Flags, _ = sv.Value().(uint32)
		}

		// Keep the strongest signal for each SSID
		if existing, dup := seen[ap.SSID]; !dup || ap.Strength > existing.Strength {
			seen[ap.SSID] = ap
		}
	}

	results := make([]AccessPoint, 0, len(seen))
	for _, ap := range seen {
		results = append(results, ap)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Strength > results[j].Strength
	})
	return results, nil
}

// devicesByType returns WiFi devices sorted by interface name.
func (c *DBusClient) devicesByType(dt NMDeviceType) ([]WiFiDevice, error) {
	paths, err := c.allDevicePaths()
	if err != nil {
		return nil, err
	}

	var devices []WiFiDevice
	for _, path := range paths {
		devType, err := c.deviceType(path)
		if err != nil || devType != dt {
			continue
		}

		dev := WiFiDevice{Path: path}

		if v, err := c.prop(path, nmDeviceInterface, "Interface"); err == nil {
			dev.Interface, _ = v.Value().(string)
		}
		if v, err := c.prop(path, nmDeviceInterface, "HwAddress"); err == nil {
			if hwStr, ok := v.Value().(string); ok {
				dev.HWAddress, _ = net.ParseMAC(hwStr)
			}
		}
		if v, err := c.prop(path, nmDeviceInterface, "State"); err == nil {
			dev.State = NMDeviceState(v.Value().(uint32))
		}
		if v, err := c.prop(path, nmDeviceInterface, "Driver"); err == nil {
			dev.Driver, _ = v.Value().(string)
		}

		devices = append(devices, dev)
	}

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Interface < devices[j].Interface
	})
	return devices, nil
}

// allDevicePaths returns all device object paths from NM.
func (c *DBusClient) allDevicePaths() ([]dbus.ObjectPath, error) {
	var paths []dbus.ObjectPath
	err := c.nm().Call(nmInterface+".GetDevices", 0).Store(&paths)
	if err != nil {
		return nil, fmt.Errorf("nmclient: get devices: %w", err)
	}
	return paths, nil
}

// deviceType returns the NMDeviceType for a device.
func (c *DBusClient) deviceType(path dbus.ObjectPath) (NMDeviceType, error) {
	v, err := c.prop(path, nmDeviceInterface, "DeviceType")
	if err != nil {
		return NMDeviceTypeUnknown, err
	}
	return NMDeviceType(v.Value().(uint32)), nil
}
