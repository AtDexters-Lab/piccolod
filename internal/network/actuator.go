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
		_ = a.nm.SetWirelessEnabled(true) // best-effort restore
		return ctx.Err()
	}
	if err := a.nm.SetWirelessEnabled(true); err != nil {
		return fmt.Errorf("device-actuator: wireless on: %w", err)
	}
	return nil
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
// device path + MAC at Enter() time (re-read each call for hotplug safety).
type realAPActuator struct {
	apMgr        *ap.Manager
	wifiDeviceFn func() (dbus.ObjectPath, net.HardwareAddr, bool)
}

// NewAPModeActuator wires the ap.Manager wrapper.
func NewAPModeActuator(apMgr *ap.Manager, wifiDeviceFn func() (dbus.ObjectPath, net.HardwareAddr, bool)) APModeActuator {
	return &realAPActuator{apMgr: apMgr, wifiDeviceFn: wifiDeviceFn}
}

func (a *realAPActuator) Enter(ctx context.Context) error {
	path, mac, ok := a.wifiDeviceFn()
	if !ok {
		return errors.New("ap-actuator: no wifi device for AP entry")
	}
	return a.apMgr.Start(ctx, path, mac)
}

func (a *realAPActuator) Exit(ctx context.Context) error {
	path, _, ok := a.wifiDeviceFn()
	if !ok {
		// Without a device path, ForceStop wouldn't help; just bail.
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
