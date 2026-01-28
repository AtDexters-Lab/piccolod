package persistence

import (
	"os"
	"strings"
)

// mountpointGuardEnabled reports whether mount directory protection is active.
// Precedence: PICCOLO_DISABLE_MOUNTPOINT_GUARD=1 (always off) >
// PICCOLO_ENABLE_MOUNTPOINT_GUARD=1 (always on) > running as root (on).
func mountpointGuardEnabled() bool {
	if strings.TrimSpace(os.Getenv("PICCOLO_DISABLE_MOUNTPOINT_GUARD")) == "1" {
		return false
	}
	if strings.TrimSpace(os.Getenv("PICCOLO_ENABLE_MOUNTPOINT_GUARD")) == "1" {
		return true
	}
	return os.Geteuid() == 0
}
