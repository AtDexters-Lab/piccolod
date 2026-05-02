// Package timezone wraps `timedatectl set-timezone` and the /etc/localtime
// readback so the rest of piccolod doesn't shell out directly. The
// auto-unlock scheduler gates window-cycle firing on IsSet to avoid
// 3-5AM-UTC-instead-of-3-5AM-local fires when the operator hasn't yet
// configured the device's zone.
package timezone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"piccolod/internal/runner"
)

// ErrInvalidTimezone is returned when Set is called with an IANA zone that
// time.LoadLocation rejects. Caller maps to 400.
var ErrInvalidTimezone = errors.New("timezone: invalid IANA zone")

// Manager wraps timezone get/set/probe via timedatectl + /etc/localtime.
// Construction is cheap; one instance per piccolod process.
type Manager struct {
	runner runner.CommandRunner
}

// NewManager constructs a Manager bound to the given command runner.
func NewManager(r runner.CommandRunner) *Manager {
	return &Manager{runner: r}
}

// Get returns the current system timezone as an IANA zone name (e.g.
// "America/New_York"). Falls back to "Etc/UTC" when the symlink isn't
// readable — same fail-closed policy as the scheduler probe.
func (m *Manager) Get() string {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return "Etc/UTC"
	}
	const prefix = "/usr/share/zoneinfo/"
	if idx := strings.Index(target, prefix); idx != -1 {
		zone := target[idx+len(prefix):]
		if _, err := time.LoadLocation(zone); err == nil {
			return zone
		}
	}
	if target == "UTC" || strings.HasSuffix(target, "/UTC") {
		return "Etc/UTC"
	}
	return "Etc/UTC"
}

// IsSet reports whether the timezone has been configured away from the
// MicroOS UTC default. Auto-unlock scheduler reads this every tick to gate
// window-cycle firing — once the operator (or X-Browser-Timezone middleware)
// runs Set, the gate opens with no restart needed.
//
// Conservative on errors: any read failure returns false. The scheduler-
// fire path is the dangerous direction; gating an extra cycle is cheap,
// firing at the wrong wall-clock hour is not.
func (m *Manager) IsSet() bool {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return false
	}
	if target == "UTC" || strings.HasSuffix(target, "/UTC") {
		return false
	}
	return true
}

// Set installs the given IANA timezone via `timedatectl set-timezone`.
// Validates the zone with time.LoadLocation before shelling out so callers
// get a clean ErrInvalidTimezone on garbage input rather than a timedatectl
// stderr blob.
func (m *Manager) Set(ctx context.Context, zone string) error {
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidTimezone, zone)
	}
	return m.runner.Run(ctx, "timedatectl", "set-timezone", zone)
}
