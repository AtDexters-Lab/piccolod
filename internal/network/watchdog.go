package network

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// watchdog implements the network health monitor ported from
// piccolo-net-watchdog.sh. It periodically ARP-probes the default gateway
// and escalates through bounce → reboot when the network is genuinely broken.
//
// The hardware watchdog (systemd WatchdogSec) handles the "piccolod hung" case.
// After a hardware-watchdog restart, this in-process watchdog detects persistent
// network failures and escalates.
type watchdog struct {
	nm     nmclient.Client
	runner runner.CommandRunner
	events *events.Bus

	// Current default route (refreshed each tick)
	gateway string
	iface   string

	// Escalation state (volatile — resets on restart)
	failures   int    // consecutive ARP failures
	lastAction string // "" or "bounce"

	// Rate limiting
	bounces []time.Time // timestamps within bounceWindow
	reboots []time.Time // timestamps within rebootWindow (loaded from disk)

	// Coordination
	stateFn    func() ConnState // queries current connectivity state
	onboarding bool             // true during first-run setup

	mu sync.Mutex
}

const (
	watchdogInterval   = 30 * time.Second
	failureThreshold   = 3
	maxBounces         = 3
	bounceWindow       = time.Hour
	maxReboots         = 1
	rebootWindow       = 2 * time.Hour
	rebootsFilePath    = "/var/lib/piccolo/net-watchdog-reboots"
	volatileDir        = "/run/piccolo"
)

func newWatchdog(nm nmclient.Client, r runner.CommandRunner, bus *events.Bus, stateFn func() ConnState) *watchdog {
	w := &watchdog{
		nm:      nm,
		runner:  r,
		events:  bus,
		stateFn: stateFn,
	}
	w.loadRebootTimestamps()
	return w
}

// run is the main watchdog loop. It runs until ctx is cancelled.
func (w *watchdog) run(ctx context.Context) {
	// Subscribe to onboarding state changes
	if w.events != nil {
		ch, cancel := w.events.SubscribeWithCancel(events.TopicOnboardingStateChanged, 4)
		defer cancel()
		go w.watchOnboarding(ctx, ch)
	}

	// Check initial onboarding state
	w.checkInitialOnboarding()

	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick performs one watchdog cycle.
func (w *watchdog) tick(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Skip during onboarding
	if w.onboarding {
		return
	}

	// Defer to state machine during active recovery or AP mode
	state := w.stateFn()
	switch state {
	case StateReconnecting, StateAPMode:
		return
	}

	// Refresh default route
	w.refreshDefaultRoute(ctx)
	if w.gateway == "" || w.iface == "" {
		// No default route — check for WiFi radio soft-block recovery
		w.checkWifiRadioRecovery(ctx)
		w.failures = 0
		w.lastAction = ""
		return
	}

	// ARP probe
	if w.arpProbe(ctx) {
		// Gateway reachable — reset state
		w.failures = 0
		w.lastAction = ""
		return
	}

	// Gateway unreachable
	w.failures++
	if w.failures < failureThreshold {
		return
	}

	// False-positive check: can we reach the internet?
	if w.pingInternet(ctx) {
		log.Printf("WARN: net-watchdog: ARP to %s via %s failed but internet reachable — skipping recovery", w.gateway, w.iface)
		w.failures = 0
		w.lastAction = ""
		return
	}

	// Network is genuinely broken — escalate
	if w.lastAction == "bounce" {
		// Bounce already tried — escalate to reboot
		w.escalateReboot(ctx)
	} else {
		// First escalation — bounce the interface
		w.escalateBounce(ctx)
	}
}

// escalateBounce disconnects and reconnects the active network interface.
func (w *watchdog) escalateBounce(ctx context.Context) {
	w.pruneTimestamps(&w.bounces, bounceWindow)
	if len(w.bounces) >= maxBounces {
		log.Printf("WARN: net-watchdog: bounce limit reached (%d/%d in %v), skipping", len(w.bounces), maxBounces, bounceWindow)
		return
	}

	log.Printf("INFO: net-watchdog: ARP to %s via %s failed (%d/%d), bouncing interface", w.gateway, w.iface, failureThreshold, failureThreshold)

	// Record intent before bounce (so escalation works even if killed mid-bounce)
	w.lastAction = "bounce"

	// Detect if this is a WiFi or Ethernet interface and bounce accordingly
	if w.isWifiInterface() {
		w.bounceWifi(ctx)
	} else {
		w.bounceEthernet(ctx)
	}

	w.bounces = append(w.bounces, time.Now())
	w.failures = 0

	// Post-bounce verification (10s allows for DHCP renewal)
	time.Sleep(10 * time.Second)
	if w.arpProbe(ctx) {
		log.Printf("INFO: net-watchdog: post-bounce: gateway %s reachable — recovery successful", w.gateway)
		w.lastAction = ""
	} else {
		log.Printf("WARN: net-watchdog: post-bounce: gateway %s still unreachable — will escalate on next cycle", w.gateway)
	}
}

// escalateReboot triggers a system reboot as a last resort.
func (w *watchdog) escalateReboot(ctx context.Context) {
	w.pruneTimestamps(&w.reboots, rebootWindow)
	if len(w.reboots) >= maxReboots {
		log.Printf("WARN: net-watchdog: reboot limit reached (%d/%d in %v), giving up", len(w.reboots), maxReboots, rebootWindow)
		return
	}

	w.reboots = append(w.reboots, time.Now())
	w.saveRebootTimestamps()

	log.Printf("WARN: net-watchdog: ARP to %s via %s failed after bounce — escalating to reboot", w.gateway, w.iface)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := w.runner.Run(timeoutCtx, "systemctl", "reboot"); err != nil {
		log.Printf("ERROR: net-watchdog: reboot failed: %v", err)
	}
}

// bounceWifi toggles the WiFi radio off and on.
func (w *watchdog) bounceWifi(ctx context.Context) {
	if err := w.nm.SetWirelessEnabled(false); err != nil {
		log.Printf("WARN: net-watchdog: WiFi radio off failed: %v", err)
		return
	}
	time.Sleep(2 * time.Second)
	if err := w.nm.SetWirelessEnabled(true); err != nil {
		log.Printf("WARN: net-watchdog: WiFi radio on failed: %v", err)
	}
}

// bounceEthernet disconnects and reconnects the Ethernet interface.
func (w *watchdog) bounceEthernet(ctx context.Context) {
	// Find the device path for the interface
	devices, err := w.nm.EthernetDevices()
	if err != nil {
		log.Printf("WARN: net-watchdog: list ethernet devices: %v", err)
		return
	}
	for _, dev := range devices {
		if dev.Interface == w.iface {
			if err := w.nm.Disconnect(dev.Path); err != nil {
				log.Printf("WARN: net-watchdog: disconnect %s: %v", w.iface, err)
				return
			}
			time.Sleep(2 * time.Second)
			// NM will auto-reconnect; we don't need to explicitly reconnect.
			return
		}
	}
	log.Printf("WARN: net-watchdog: ethernet device %s not found for bounce", w.iface)
}

// isWifiInterface returns true if the current default-route interface is WiFi.
func (w *watchdog) isWifiInterface() bool {
	return strings.HasPrefix(w.iface, "wl")
}

// arpProbe sends an ARP request to the gateway and returns true if reachable.
func (w *watchdog) arpProbe(ctx context.Context) bool {
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := w.runner.Run(timeoutCtx, "arping", "-c", "1", "-w", "3", "-I", w.iface, w.gateway)
	return err == nil
}

// pingInternet sends an ICMP ping to 8.8.8.8 as a false-positive check.
func (w *watchdog) pingInternet(ctx context.Context) bool {
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := w.runner.Run(timeoutCtx, "ping", "-c", "1", "-W", "3", "8.8.8.8")
	return err == nil
}

// refreshDefaultRoute reads the current IPv4 default route.
func (w *watchdog) refreshDefaultRoute(ctx context.Context) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := w.runner.RunWithOutput(timeoutCtx, "ip", "-4", "route", "show", "default")
	if err != nil || len(out) == 0 {
		w.gateway = ""
		w.iface = ""
		return
	}

	line := strings.Split(string(out), "\n")[0]
	w.gateway = extractField(line, "via")
	w.iface = extractField(line, "dev")
}

// checkWifiRadioRecovery re-enables WiFi radio if it's soft-blocked and WiFi
// connections are configured.
func (w *watchdog) checkWifiRadioRecovery(ctx context.Context) {
	enabled, err := w.nm.WirelessEnabled()
	if err != nil || enabled {
		return
	}

	// Only re-enable if WiFi connections exist
	conns, err := w.nm.SavedWiFiConnections()
	if err != nil || len(conns) == 0 {
		return
	}

	w.pruneTimestamps(&w.bounces, bounceWindow)
	if len(w.bounces) >= maxBounces {
		log.Printf("WARN: net-watchdog: WiFi radio disabled but bounce limit reached, leaving off")
		return
	}

	log.Printf("INFO: net-watchdog: no default route and WiFi radio disabled — re-enabling")
	if err := w.nm.SetWirelessEnabled(true); err != nil {
		log.Printf("WARN: net-watchdog: WiFi radio enable failed: %v", err)
	}
	w.bounces = append(w.bounces, time.Now())
}

// checkInitialOnboarding queries the onboarding state synchronously at startup
// to avoid a race where the watchdog runs before the onboarding event fires.
func (w *watchdog) checkInitialOnboarding() {
	// The onboarding file is at a well-known path. If it exists and indicates
	// setup is in progress, suppress the watchdog.
	data, err := os.ReadFile("/piccolo-core/network-bootstrap/onboarding.json")
	if err != nil {
		return // file missing = post-onboarding
	}
	s := string(data)
	// Quick parse: look for "state":"pending" or "state":"install_disk"
	if strings.Contains(s, `"pending"`) || strings.Contains(s, `"install_disk"`) {
		w.mu.Lock()
		w.onboarding = true
		w.mu.Unlock()
	}
}

// watchOnboarding listens for onboarding state changes and updates the guard.
func (w *watchdog) watchOnboarding(ctx context.Context, ch <-chan events.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			// The onboarding event payload has a State field.
			// When state transitions to "complete" or "try_piccolo", disable guard.
			type statePayload struct {
				State string `json:"state"`
			}
			if sp, ok := evt.Payload.(statePayload); ok {
				w.mu.Lock()
				w.onboarding = sp.State == "pending" || sp.State == "install_disk"
				w.mu.Unlock()
			}
		}
	}
}

// pruneTimestamps removes entries older than maxAge from the slice.
func (w *watchdog) pruneTimestamps(ts *[]time.Time, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for _, t := range *ts {
		if t.After(cutoff) {
			(*ts)[n] = t
			n++
		}
	}
	*ts = (*ts)[:n]
}

// loadRebootTimestamps reads persistent reboot timestamps from disk.
func (w *watchdog) loadRebootTimestamps() {
	data, err := os.ReadFile(rebootsFilePath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ts, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
			w.reboots = append(w.reboots, time.Unix(ts, 0))
		}
	}
	w.pruneTimestamps(&w.reboots, rebootWindow)
}

// saveRebootTimestamps writes reboot timestamps to disk.
func (w *watchdog) saveRebootTimestamps() {
	_ = os.MkdirAll(volatileDir, 0o755)
	dir := rebootsFilePath[:strings.LastIndex(rebootsFilePath, "/")]
	_ = os.MkdirAll(dir, 0o755)

	var lines []string
	for _, t := range w.reboots {
		lines = append(lines, fmt.Sprintf("%d", t.Unix()))
	}
	_ = os.WriteFile(rebootsFilePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// extractField finds the value following a keyword in a space-separated string.
// e.g., extractField("default via 192.168.1.1 dev eth0", "via") → "192.168.1.1"
func extractField(line, keyword string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == keyword && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
