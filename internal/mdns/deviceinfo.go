package mdns

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// GetDeviceModel returns the device model string.
// Priority: ARM devicetree > DMI product_name > "Unknown"
func GetDeviceModel() string {
	// Try ARM devicetree first (Raspberry Pi and other ARM devices)
	if data, err := os.ReadFile("/sys/firmware/devicetree/base/model"); err == nil {
		// Remove trailing null byte if present
		model := strings.TrimRight(string(data), "\x00\n\r")
		if model != "" {
			return model
		}
	}

	// Try DMI product_name (x86 systems)
	if data, err := os.ReadFile("/sys/devices/virtual/dmi/id/product_name"); err == nil {
		model := strings.TrimSpace(string(data))
		if model != "" {
			return model
		}
	}

	return "Unknown"
}

// GetBootTime calculates the boot time from /proc/uptime.
// Returns the time.Time when the system booted.
func GetBootTime() time.Time {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		// Fallback to current time if we can't read uptime
		return time.Now()
	}

	// /proc/uptime format: "uptime_seconds idle_seconds"
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return time.Now()
	}

	uptimeSeconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Now()
	}

	// Calculate boot time by subtracting uptime from current time
	return time.Now().Add(-time.Duration(uptimeSeconds * float64(time.Second)))
}
