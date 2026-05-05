package network

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// Probe constants — see RFC §"Probe constants".
const (
	TickInterval     = 30 * time.Second
	ColdBootGrace    = 60 * time.Second
	NofM             = 3 // 3-of-3 dampening
	QuietPeriod      = 90 * time.Second // 3 ticks post-action
)

// Prober coordinates the per-tick observation pass: WiFi + Eth device probes,
// L3 probe, ActiveUplink derivation, dampening (3-of-3), quiet period
// suppression.
//
// State held by the prober is strictly self-state: dampener counters and the
// signal-cached last NMReason per device. World state is reprobed every tick.
type Prober struct {
	nm     nmclient.Client
	runner runner.CommandRunner
	sys    SystemState

	// dampener counters
	mu              sync.Mutex
	consecHWFault   map[DeviceKind]int
	consecL3Down    int
	lastReasonByDev map[dbus.ObjectPath]nmclient.NMDeviceStateReason

	// device path cache — written by probeWiFi/probeEth, read by L3 probe
	// and signal subscription. Reset on each tick by Probe().
	devicePath map[DeviceKind]dbus.ObjectPath

	// signal goroutine management
	signalCtx    context.Context
	signalCancel context.CancelFunc
	signalOnce   sync.Once
	signalWG     sync.WaitGroup
}

// NewProber constructs a Prober. Callers must call Run(ctx) to start the
// signal-subscription goroutine that caches the last NMReason per device.
func NewProber(nm nmclient.Client, r runner.CommandRunner, sys SystemState) *Prober {
	return &Prober{
		nm:              nm,
		runner:          r,
		sys:             sys,
		consecHWFault:   make(map[DeviceKind]int),
		lastReasonByDev: make(map[dbus.ObjectPath]nmclient.NMDeviceStateReason),
		devicePath:      make(map[DeviceKind]dbus.ObjectPath),
	}
}

// Run starts the signal-subscription background goroutines (one per device).
// Idempotent: only the first call has effect. Goroutines exit on ctx.Done().
//
// F-S6: signals refresh the cached NMReason but never advance N-of-M counters.
func (p *Prober) Run(ctx context.Context) {
	p.signalOnce.Do(func() {
		p.signalCtx, p.signalCancel = context.WithCancel(ctx)
		p.signalWG.Add(1)
		go p.signalLoop()
	})
}

// signalLoop discovers devices once at startup and subscribes to per-device
// state changes. On each event, caches the reason. Re-discovers if an "added"
// event arrives — in scope only for the simple single-device-per-kind case.
func (p *Prober) signalLoop() {
	defer p.signalWG.Done()

	// Subscribe to device add/remove so we can resubscribe if the device
	// path changes (USB hotplug — uncommon but possible).
	devEvents, err := p.nm.SubscribeDeviceAddedRemoved(p.signalCtx)
	if err != nil {
		log.Printf("WARN: net-probe: device-event subscribe: %v", err)
	}

	// Per-device subscription cancel funcs.
	subs := make(map[dbus.ObjectPath]context.CancelFunc)
	subscribe := func(path dbus.ObjectPath) {
		if _, ok := subs[path]; ok {
			return
		}
		subCtx, cancel := context.WithCancel(p.signalCtx)
		ch, err := p.nm.SubscribeDeviceStateChanges(subCtx, path)
		if err != nil {
			cancel()
			log.Printf("WARN: net-probe: subscribe %s: %v", path, err)
			return
		}
		subs[path] = cancel
		p.signalWG.Add(1)
		go func() {
			defer p.signalWG.Done()
			for evt := range ch {
				p.mu.Lock()
				p.lastReasonByDev[evt.Device] = evt.Reason
				p.mu.Unlock()
			}
		}()
	}

	// Initial discovery — subscribe to current devices.
	if devs, err := p.nm.WiFiDevices(); err == nil {
		for _, d := range devs {
			subscribe(d.Path)
		}
	}
	if devs, err := p.nm.EthernetDevices(); err == nil {
		for _, d := range devs {
			subscribe(d.Path)
		}
	}

	for {
		select {
		case <-p.signalCtx.Done():
			for _, cancel := range subs {
				cancel()
			}
			return
		case evt, ok := <-devEvents:
			if !ok {
				continue
			}
			if evt.Type == nmclient.DeviceAdded {
				subscribe(evt.Device)
			} else if cancel, ok := subs[evt.Device]; ok {
				cancel()
				delete(subs, evt.Device)
				p.mu.Lock()
				delete(p.lastReasonByDev, evt.Device)
				p.mu.Unlock()
			}
		}
	}
}

// Stop tears down the signal goroutines. Safe to call before/without Run.
func (p *Prober) Stop() {
	if p.signalCancel != nil {
		p.signalCancel()
	}
	p.signalWG.Wait()
}

// Probe runs one full observation pass and returns a Tick. ledger is read
// (not mutated) for quiet-period anchors. Pure: side effects limited to
// arping and L3 TCP-connect.
func (p *Prober) Probe(ctx context.Context, led ActionLedger, now time.Time) Tick {
	// Reset device-path cache; populated by per-device probes below.
	p.mu.Lock()
	p.devicePath = make(map[DeviceKind]dbus.ObjectPath)
	p.mu.Unlock()

	uptime := readSystemUptime()

	// Per-device raw probes.
	rawWiFi := p.probeWiFi()
	rawEth := p.probeEth()

	devices := map[DeviceKind]DeviceObservation{
		DeviceWiFi:     p.resolveWiFi(rawWiFi, led, uptime, QuietPeriod, ColdBootGrace, now),
		DeviceEthernet: p.resolveEth(rawEth, led, uptime, QuietPeriod, ColdBootGrace, now),
	}

	// L3 + per-device gateway ARP. Run after device probes so we know which
	// interfaces have IP and which gateway to ARP.
	l3, gwMap := p.probeL3(ctx, devices)
	for k, gw := range gwMap {
		obs := devices[k]
		obs.GwReachable = gw
		devices[k] = obs
	}

	// Dampen L3 (3-of-3 Down → L3ProbeDown; otherwise L3ProbeUp).
	l3Final := p.dampenL3(l3)

	nmConn := nmConnAdvisory(p.nm)
	busy, reason := p.sys.SystemBusy()

	return Tick{
		Devices:          devices,
		NMConn:           nmConn,
		L3Probe:          l3Final,
		ActiveUplink:     activeUplinkFor(devices),
		SystemBusy:       busy,
		SystemBusyReason: reason,
		SystemUptime:     uptime,
		At:               now,
	}
}

// dampenHW applies 3-of-3 dampening + quiet-period suppression for HW health.
// Returns the smoothed Tri.
//
// Quiet period rule (F-B1): for `quietPeriod` after the last bounce on this
// device, do not advance the counter and report Healthy. This prevents the
// post-bounce "still recovering" state from being mistaken for a fresh
// fault and ensures decideHW returns Wait (which it does anyway via its
// own quiet-period gate).
func (p *Prober) dampenHW(kind DeviceKind, candidate Tri, led ActionLedger, quietPeriod time.Duration, now time.Time) Tri {
	if last, ok := led.LastBounceAt[kind]; ok && now.Sub(last) < quietPeriod {
		// Reset counter; suppress fault report during quiet period.
		p.mu.Lock()
		p.consecHWFault[kind] = 0
		p.mu.Unlock()
		return TriHealthy
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if candidate == TriFaulted {
		p.consecHWFault[kind]++
		if p.consecHWFault[kind] >= NofM {
			return TriFaulted
		}
		return TriHealthy
	}
	p.consecHWFault[kind] = 0
	return candidate
}

func (p *Prober) dampenL3(raw L3ProbeResult) L3ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if raw == L3ProbeDown {
		p.consecL3Down++
		if p.consecL3Down >= NofM {
			return L3ProbeDown
		}
		return L3ProbeUp
	}
	p.consecL3Down = 0
	return L3ProbeUp
}

// lastReason returns the cached NMDeviceStateReason for the given device path.
func (p *Prober) lastReason(path dbus.ObjectPath) (nmclient.NMDeviceStateReason, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.lastReasonByDev[path]
	return r, ok
}

// readSystemUptime returns the system-wide uptime (NOT process uptime —
// S1-rev: piccolod restart doesn't reset grace if the system has been up
// for hours).
//
// Test seam: tests override this to inject a fixed value.
var readSystemUptime = func() time.Duration {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}
