package mdns

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GetDeviceModel returns the device model string from sysfs (ARM devicetree
// or x86 DMI), or empty string if neither is available. Hardware doesn't
// change at runtime, so the result is cached after the first call.
//
// Callers that need a user-visible placeholder (e.g., the mDNS TXT record)
// should default to their own label on empty return — the producer should
// not fabricate one, since some consumers (e.g., namek enrollment) prefer
// to omit the field entirely rather than show a misleading "Unknown".
var GetDeviceModel = sync.OnceValue(func() string {
	if data, err := os.ReadFile("/sys/firmware/devicetree/base/model"); err == nil {
		if model := strings.TrimRight(string(data), "\x00\n\r"); model != "" {
			return model
		}
	}
	if data, err := os.ReadFile("/sys/devices/virtual/dmi/id/product_name"); err == nil {
		if model := strings.TrimSpace(string(data)); model != "" {
			return model
		}
	}
	return ""
})

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
