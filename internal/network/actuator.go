package network

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/ap"
	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// DeviceActuator bounces a specific device kind. Implementations dispatch
// by kind and hold a per-device mutex so a WiFi bounce cannot race with an
// AP-mode entry on the same radio (S5-rev / G8).
type DeviceActuator interface {
	Bounce(ctx context.Context, dev DeviceKind) error
}

// APModeActuator manages the AP-mode hotspot. Snapshot consumers query
// Active() to decide whether to render captive-portal copy.
type APModeActuator interface {
	Enter(ctx context.Context) error
	Exit(ctx context.Context) error
	Active() bool
	SSID() string
}

// SystemActuator triggers system reboot. Preconditions are checked at the
// call site (advisory query against SystemState); the underlying TOCTOU is
// bounded by transactional-update's restart-tolerant integrity model.
type SystemActuator interface {
	Reboot(ctx context.Context, reason string) error
}

// ErrRebootDeferred indicates the reboot was not issued because SystemState
// reports busy. Caller surfaces WedgedAwaitingPrecondition; orchestrator
// re-evaluates next tick.
var ErrRebootDeferred = errors.New("reboot deferred: system busy")

// ----- DeviceActuator implementation -----

// realDeviceActuator dispatches Bounce by kind. WiFi: rfkill soft-unblock +
// nmcli radio wifi off/on. Ethernet: nmcli device disconnect/connect.
//
// Holds a per-device mutex (apMu shared with APModeActuator on the same
// device path) so AP-mode entry cannot race with a WiFi bounce.
type realDeviceActuator struct {
	nm     nmclient.Client
	runner runner.CommandRunner

	// devicePathFn returns the current device path; orchestrator updates
	// after each probe (USB hotplug). Returns "" if device absent.
	devicePathFn func(DeviceKind) dbus.ObjectPath
	ifaceFn      func(DeviceKind) string

	// apActuator: shared mutex anchor for radio contention.
	apActive func() bool

	// per-kind mutex
	mu map[DeviceKind]*sync.Mutex

	bounceSettleDelay time.Duration
}

// NewDeviceActuator wires a real DeviceActuator against the supplied nmclient.
func NewDeviceActuator(nm nmclient.Client, r runner.CommandRunner, pathFn func(DeviceKind) dbus.ObjectPath, ifaceFn func(DeviceKind) string, apActive func() bool) DeviceActuator {
	return &realDeviceActuator{
		nm:                nm,
		runner:            r,
		devicePathFn:      pathFn,
		ifaceFn:           ifaceFn,
		apActive:          apActive,
		mu:                map[DeviceKind]*sync.Mutex{DeviceWiFi: {}, DeviceEthernet: {}},
		bounceSettleDelay: 2 * time.Second,
	}
}

func (a *realDeviceActuator) Bounce(ctx context.Context, dev DeviceKind) error {
	mu, ok := a.mu[dev]
	if !ok {
		return fmt.Errorf("device-actuator: unknown kind %s", dev)
	}
	mu.Lock()
	defer mu.Unlock()

	switch dev {
	case DeviceWiFi:
		return a.bounceWiFi(ctx)
	case DeviceEthernet:
		return a.bounceEth(ctx)
	default:
		return fmt.Errorf("device-actuator: unsupported kind %s", dev)
	}
}

func (a *realDeviceActuator) bounceWiFi(ctx context.Context) error {
	if a.apActive != nil && a.apActive() {
		// AP active on the radio — bouncing would tear it down. Caller
		// (orchestrator) is expected to APExit first via APModeActuator.
		// Defensive: refuse if reached anyway.
		return errors.New("device-actuator: refusing wifi bounce while AP active")
	}

	// rfkill soft-unblock first (covers A6: soft-block + profile case).
	rfctx, rfcancel := context.WithTimeout(ctx, 5*time.Second)
	if err := a.runner.Run(rfctx, "rfkill", "unblock", "wifi"); err != nil {
		log.Printf("WARN: device-actuator: rfkill unblock wifi: %v", err)
		// Non-fatal: proceed with NM toggle.
	}
	rfcancel()

	if err := a.nm.SetWirelessEnabled(false); err != nil {
		return fmt.Errorf("device-actuator: wireless off: %w", err)
	}
	select {
	case <-time.After(a.bounceSettleDelay):
	case <-ctx.Done():
		_ = enableWirelessWithRetry(ctx, a.nm) // best-effort restore
		return ctx.Err()
	}
	if err := enableWirelessWithRetry(ctx, a.nm); err != nil {
		// Critical: returning here leaves the radio off. We've retried
		// the maximum number of times — surface the error so the caller
		// records the bounce attempt as failed (next-tick observation
		// will see HW Faulted and re-escalate via the normal ladder).
		return fmt.Errorf("device-actuator: wireless on after retries: %w", err)
	}
	return nil
}

// enableWirelessWithRetry calls nm.SetWirelessEnabled(true) up to 3 times
// with brief backoff. The radio MUST come back on after a bounce — leaving
// it off makes the supervisor's next-tick probe observe HW=Unavailable
// and re-escalate, which would just bounce again (off→fail-on, repeat).
// Three retries is enough to ride out transient D-Bus hiccups without
// hanging the actuator goroutine.
func enableWirelessWithRetry(ctx context.Context, nm nmclient.Client) error {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := nm.SetWirelessEnabled(true); err == nil {
			return nil
		} else {
			lastErr = err
			log.Printf("WARN: device-actuator: wireless on attempt %d/%d: %v", i+1, attempts, err)
		}
		select {
		case <-time.After(time.Duration(i+1) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}

func (a *realDeviceActuator) bounceEth(ctx context.Context) error {
	path := a.devicePathFn(DeviceEthernet)
	iface := a.ifaceFn(DeviceEthernet)
	if path == "" {
		return errors.New("device-actuator: ethernet device absent")
	}
	if err := a.nm.Disconnect(path); err != nil {
		log.Printf("WARN: device-actuator: ethernet disconnect %s: %v", iface, err)
		// fall-through to settle + reconnect; NM may auto-reconnect.
	}
	select {
	case <-time.After(a.bounceSettleDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	// NM autoconnect handles reconnection; no explicit reconnect call.
	return nil
}

// ----- APModeActuator implementation -----

// realAPActuator wraps the existing internal/network/ap.Manager. The
// orchestrator constructs this with a function that supplies the WiFi
// device path + MAC at Enter() time (re-read each call for hotplug safety),
// plus an optional pre-scan hook that runs while the radio is still in
// STA mode — once AP starts, the WiFi adapter cannot scan, so the
// captive portal serves the cache populated by this hook.
type realAPActuator struct {
	apMgr        *ap.Manager
	wifiDeviceFn func() (dbus.ObjectPath, net.HardwareAddr, bool)
	prescanFn    func(ctx context.Context) error
}

// NewAPModeActuator wires the ap.Manager wrapper. prescanFn (if non-nil)
// runs immediately before AP entry so the captive portal has a fresh
// network list to display — without it, first-run users see an empty
// list because Manager.ScanNetworks returns scanCache while AP is up
// and the cache has not been primed.
func NewAPModeActuator(apMgr *ap.Manager, wifiDeviceFn func() (dbus.ObjectPath, net.HardwareAddr, bool), prescanFn func(ctx context.Context) error) APModeActuator {
	return &realAPActuator{apMgr: apMgr, wifiDeviceFn: wifiDeviceFn, prescanFn: prescanFn}
}

func (a *realAPActuator) Enter(ctx context.Context) error {
	path, mac, ok := a.wifiDeviceFn()
	if !ok {
		return errors.New("ap-actuator: no wifi device for AP entry")
	}
	// Pre-scan while the adapter is still in STA mode — once the hotspot
	// starts, NM cannot scan on the same radio.
	if a.prescanFn != nil {
		if err := a.prescanFn(ctx); err != nil {
			log.Printf("WARN: ap-actuator: pre-AP scan failed: %v", err)
		}
	}
	return a.apMgr.Start(ctx, path, mac)
}

func (a *realAPActuator) Exit(ctx context.Context) error {
	path, _, ok := a.wifiDeviceFn()
	if !ok {
		// USB WiFi adapter removed (or otherwise gone) while AP was up.
		// ForceStop tears down local AP state (teardown — captive portal
		// stop, firewall + DNS cleanup) regardless of NM device-level
		// reachability; without this branch the supervisor would see
		// apActive=true forever and keep deciding APExit while the
		// captive portal stays running on a missing radio.
		a.apMgr.ForceStop(ctx, "")
		return nil
	}
	return a.apMgr.Stop(ctx, path)
}

func (a *realAPActuator) Active() bool {
	if a.apMgr == nil {
		return false
	}
	return a.apMgr.Active()
}

func (a *realAPActuator) SSID() string {
	if a.apMgr == nil {
		return ""
	}
	return a.apMgr.SSID()
}

// ----- SystemActuator implementation -----

type realSystemActuator struct {
	runner runner.CommandRunner
	sys    SystemState
}

// NewSystemActuator constructs the reboot actuator. The advisory SystemBusy
// check happens at Reboot() time (call site, not constructor) so the
// decision tracks the latest cached onboarding phase.
func NewSystemActuator(r runner.CommandRunner, sys SystemState) SystemActuator {
	return &realSystemActuator{runner: r, sys: sys}
}

func (a *realSystemActuator) Reboot(ctx context.Context, reason string) error {
	if busy, why := a.sys.SystemBusy(); busy {
		log.Printf("WARN: system-actuator: reboot deferred — system busy (%s)", why)
		return ErrRebootDeferred
	}

	log.Printf("WARN: system-actuator: rebooting — reason=%q", reason)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.runner.Run(cctx, "systemctl", "reboot"); err != nil {
		return fmt.Errorf("system-actuator: systemctl reboot: %w", err)
	}
	return nil
}
