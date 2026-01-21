package mdns

import (
	"testing"
	"time"
)

func TestGetDeviceModel(t *testing.T) {
	// GetDeviceModel should return a non-empty string
	model := GetDeviceModel()
	if model == "" {
		t.Error("GetDeviceModel() returned empty string")
	}

	// The model should either be a device name or "Unknown"
	t.Logf("Device model: %s", model)
}

func TestGetBootTime(t *testing.T) {
	bootTime := GetBootTime()

	// Boot time should be in the past
	if bootTime.After(time.Now()) {
		t.Errorf("GetBootTime() returned future time: %v", bootTime)
	}

	// Boot time should be after Unix epoch (sanity check)
	// Note: We don't check for "recent" boot time because long-running
	// servers or CI hosts may have uptimes exceeding one year.
	unixEpoch := time.Unix(0, 0)
	if bootTime.Before(unixEpoch) {
		t.Errorf("GetBootTime() returned pre-Unix-epoch time: %v", bootTime)
	}

	t.Logf("Boot time: %v (uptime: %v)", bootTime, time.Since(bootTime))
}

func TestGetBootTimeConsistency(t *testing.T) {
	// Multiple calls should return similar boot times
	boot1 := GetBootTime()
	time.Sleep(100 * time.Millisecond)
	boot2 := GetBootTime()

	// Boot times should be within 1 second of each other (accounting for timing)
	diff := boot2.Sub(boot1)
	if diff < 0 {
		diff = -diff
	}

	if diff > time.Second {
		t.Errorf("GetBootTime() inconsistent: %v vs %v (diff: %v)", boot1, boot2, diff)
	}
}
