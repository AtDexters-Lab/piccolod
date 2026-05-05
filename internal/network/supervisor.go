package network

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/events"
	"piccolod/internal/network/ap"
	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// Supervisor coordinates the per-tick probe → decide → act → snapshot
// pipeline. Implements supervisor.Component (Name/Start/Stop) and the
// public Snapshot/SetForceAP/SetSuppressAP/RequestProbeAndDecide API.
type Supervisor struct {
	prober *Prober
	led    *LedgerStore
	sys    SystemState

	devAct  DeviceActuator
	apAct   APModeActuator
	sysAct  SystemActuator

	bus *events.Bus

	// Lifecycle.
	mu        sync.Mutex
	stopOnce  sync.Once
	cancelCtx context.CancelFunc
	doneCh    chan struct{}
	closer    interface{ Close() error }

	// User intent — boot-volatile (N4-S4); both default false on every start.
	intentMu sync.RWMutex
	intent   UserIntent

	// Atomic-swap snapshot pointer. Snapshot() readers see whole-snapshot
	// consistency.
	snap atomic.Pointer[Snapshot]

	// Early-tick request channel; size 1, drop-if-pending.
	requestCh chan struct{}

	// Latest tick (for legacy wire-contract derivation in manager.go).
	tickPtr atomic.Pointer[Tick]

	// Whether actuators are wired (Stage 4+ flips this to true). When false,
	// the orchestrator only probes + classifies; decisions are computed but
	// no side effects fire. Stage-1/2 testing path.
	enableActuation bool

	// Dedupe key for publishLegacyEvent so subscribers (identity, stun,
	// health) are woken only on actual transitions, not every 30s tick.
	legacyEventMu   sync.Mutex
	lastLegacyEvent legacyEventKey
}

type legacyEventKey struct {
	state    string
	uplink   string
	apActive bool
	apSSID   string
}

// NewSupervisor constructs a probe-only supervisor. Use SetActuators to
// wire actuators and EnableActuation to start them firing.
func NewSupervisor(prober *Prober, led *LedgerStore, sys SystemState) *Supervisor {
	return &Supervisor{
		prober:    prober,
		led:       led,
		sys:       sys,
		doneCh:    make(chan struct{}),
		requestCh: make(chan struct{}, 1),
	}
}

// SetActuators wires the side-effecting actuators. Call before Start.
func (s *Supervisor) SetActuators(dev DeviceActuator, apA APModeActuator, sysA SystemActuator) {
	s.devAct = dev
	s.apAct = apA
	s.sysAct = sysA
}

// SetEventBus sets the optional event bus. Network state-change events are
// published to TopicNetworkStateChanged for backwards compatibility with
// existing subscribers (identity, stun).
func (s *Supervisor) SetEventBus(bus *events.Bus) { s.bus = bus }

// EnableActuation flips the supervisor from observer-only to active. Once
// true, decisions fire actuators. Stage-4 cutover toggles this.
func (s *Supervisor) EnableActuation(enable bool) { s.enableActuation = enable }

// Name implements supervisor.Component.
func (s *Supervisor) Name() string { return "network-supervisor" }

// Start implements supervisor.Component. Loads the ledger, starts the
// signal-subscription goroutines, and launches the tick loop.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancelCtx != nil {
		return fmt.Errorf("network-supervisor: already started")
	}

	led := s.led.Load()
	s.led.Prune(time.Now())
	log.Printf("INFO: net-supervisor: ledger loaded — reboots=%d bounces=%v actuation=%v",
		len(led.Reboots), summarizeBounces(led.Bounces), s.enableActuation)

	runCtx, cancel := context.WithCancel(ctx)
	s.cancelCtx = cancel
	s.prober.Run(runCtx)

	go s.tickLoop(runCtx)
	return nil
}

// Stop implements supervisor.Component. Safe to call before Start: cancel
// is nil and tickLoop never runs, so we skip the doneCh wait.
func (s *Supervisor) Stop(_ context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		cancel := s.cancelCtx
		s.mu.Unlock()
		if cancel == nil {
			// Start was never called — nothing to wait on.
			s.prober.Stop()
			if s.closer != nil {
				_ = s.closer.Close()
			}
			return
		}
		cancel()
		s.prober.Stop()
		<-s.doneCh
		if s.closer != nil {
			_ = s.closer.Close()
		}
	})
	return nil
}

// Snapshot returns the latest published snapshot. O(1) atomic read. Returns
// the zero Snapshot if no tick has run yet.
func (s *Supervisor) Snapshot() Snapshot {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return Snapshot{}
}

// LastTick returns the most recent Tick observation. Used by Manager.Status
// to construct legacy wire-contract responses (deriveLegacyState).
func (s *Supervisor) LastTick() Tick {
	if p := s.tickPtr.Load(); p != nil {
		return *p
	}
	return Tick{}
}

// SetForceAP sets the boot-volatile ForceAP intent. Effects observable on
// the next tick boundary; callers may invoke RequestProbeAndDecide to
// hasten a tick.
func (s *Supervisor) SetForceAP(v bool) {
	s.intentMu.Lock()
	s.intent.ForceAP = v
	s.intentMu.Unlock()
}

// SetSuppressAP sets the boot-volatile SuppressAP intent.
func (s *Supervisor) SetSuppressAP(v bool) {
	s.intentMu.Lock()
	s.intent.SuppressAP = v
	s.intentMu.Unlock()
}

// IntentSnapshot returns a copy of the current intent.
func (s *Supervisor) IntentSnapshot() UserIntent {
	s.intentMu.RLock()
	defer s.intentMu.RUnlock()
	return s.intent
}

// RecordExternalAPExit records an AP-toggle Exit event in the ledger. Used
// by callers that drive ap.Manager.Stop directly (e.g., the captive-portal
// connect flow tears down AP synchronously before invoking nmclient.Connect
// because the single radio cannot do AP+STA simultaneously). Without this
// call, the rate-shaper undercounts toggles and a flapping captive-portal
// failure cycle would not engage the entry budget.
func (s *Supervisor) RecordExternalAPExit(reason string) {
	s.led.RecordAPToggle(false, time.Now(), reason)
}

// RequestProbeAndDecide asks the orchestrator to run an early tick. Buffered
// channel size 1 — callers are non-blocking; if a request is already pending
// it is coalesced (drop semantics). N4-S2: dropped requests do not lose
// intent state because the next tick reads intent unconditionally.
func (s *Supervisor) RequestProbeAndDecide() {
	select {
	case s.requestCh <- struct{}{}:
	default:
	}
}

// tickLoop runs the main tick loop. Tick at 30s steady, plus opportunistic
// early ticks via RequestProbeAndDecide.
func (s *Supervisor) tickLoop(ctx context.Context) {
	defer close(s.doneCh)

	// First tick fires immediately so we have a snapshot before 30s elapses.
	s.runTick(ctx)

	tk := time.NewTicker(TickInterval)
	defer tk.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.runTick(ctx)
		case <-s.requestCh:
			// Drain any racing requests (coalesce).
			select {
			case <-s.requestCh:
			default:
			}
			s.runTick(ctx)
			// Reset the ticker so we don't immediately re-tick after the
			// requested run.
			tk.Reset(TickInterval)
		}
	}
}

// runTick performs one full pipeline pass.
func (s *Supervisor) runTick(ctx context.Context) {
	now := time.Now()
	s.led.Prune(now)
	led := s.led.Snapshot()
	tick := s.prober.Probe(ctx, led, now)
	s.tickPtr.Store(&tick)

	intent := s.IntentSnapshot()
	apActive := s.isAPActive()

	hwWiFi := decideHW(tick.Devices[DeviceWiFi], led, tick, DeviceWiFi)
	hwEth := decideHW(tick.Devices[DeviceEthernet], led, tick, DeviceEthernet)
	apA := decideAP(tick, led, intent, apActive)

	// Step 4a: synchronous ledger writes for actions about to fire. Snapshot
	// must reflect these so quiet-period gates and budget views are
	// consistent across the same tick (RFC §"Snapshot construction order").
	// rebootPersisted gates the actuator dispatch in step 4b — if the
	// reboot timestamp could not be persisted, we MUST NOT issue the
	// reboot, otherwise the next boot reads a stale ledger and the same
	// fault can immediately fire another reboot (loop).
	rebootPersisted := s.recordIntent(now, hwWiFi, hwEth, apA)

	// Step 4b: spawn actuator goroutines (async; outcomes observed next tick).
	if s.enableActuation {
		s.act(ctx, hwWiFi, hwEth, apA, rebootPersisted)
	}

	// Step 5: construct + publish snapshot. Only LastBounceAt is needed
	// from the post-action ledger — avoid the full clone.
	apSSID := ""
	if s.apAct != nil {
		apSSID = s.apAct.SSID()
	}
	apReason := apReasonFor(apA, intent, apActive)
	snap := buildSnapshot(tick, s.led.LastBounceAtSnapshot(), s.isAPActive(), apSSID, apReason)
	s.snap.Store(&snap)

	// Step 6: emit legacy wire-contract event for existing subscribers.
	s.publishLegacyEvent(tick, snap)

	logTick(tick, hwWiFi, hwEth, apA)
}

// recordIntent writes ledger entries synchronously for the actions about to
// fire. F-B2: writes happen on initiation, not completion.
//
// Returns rebootPersisted=true iff a Reboot was decided AND the persistent
// ledger write succeeded. Caller MUST gate the actuator dispatch on this:
// firing a reboot whose timestamp was not persisted creates a reboot loop
// (next boot reads a stale ledger and the same fault re-fires immediately).
// Bounces and AP toggles are best-effort; their persist failures only lose
// rate-shaping precision, not safety.
func (s *Supervisor) recordIntent(now time.Time, hwW, hwE HWAction, apA APAction) (rebootPersisted bool) {
	rebootPersisted = s.recordHW(hwW, now) || s.recordHW(hwE, now)

	switch a := apA.(type) {
	case APEnter:
		s.led.RecordAPToggle(true, now, a.Reason)
	case APExit:
		s.led.RecordAPToggle(false, now, a.Reason)
	}
	return rebootPersisted
}

// recordHW handles one HWAction's persistence side. Returns true iff a
// Reboot was decided AND its timestamp was successfully persisted (caller
// uses this to gate the actuator dispatch — see recordIntent).
func (s *Supervisor) recordHW(act HWAction, now time.Time) bool {
	switch a := act.(type) {
	case HWBounce:
		s.led.RecordBounce(a.Device, now)
	case HWReboot:
		if err := s.led.RecordReboot(now); err != nil {
			log.Printf("ERROR: net-supervisor: persist reboot ledger failed (%v) — SUPPRESSING reboot to avoid loop on next boot", err)
			return false
		}
		return true
	}
	return false
}

// act dispatches actuators for the decided actions. Each side-effecting
// call runs in its own goroutine; outcomes observed by next tick's probe.
//
// rebootPersisted gates the Reboot fire — see recordIntent.
func (s *Supervisor) act(ctx context.Context, hwW, hwE HWAction, apA APAction, rebootPersisted bool) {
	if s.devAct != nil {
		if act, ok := hwW.(HWBounce); ok {
			go s.fireBounce(ctx, act)
		}
		if act, ok := hwE.(HWBounce); ok {
			go s.fireBounce(ctx, act)
		}
	}
	if s.sysAct != nil && rebootPersisted {
		if act, ok := hwW.(HWReboot); ok {
			go s.fireReboot(ctx, "wifi: "+act.Reason)
		} else if act, ok := hwE.(HWReboot); ok {
			go s.fireReboot(ctx, "eth: "+act.Reason)
		}
	}
	if s.apAct != nil {
		switch apA.(type) {
		case APEnter:
			go s.fireAPEnter(ctx)
		case APExit:
			go s.fireAPExit(ctx)
		}
	}
}

func (s *Supervisor) fireBounce(ctx context.Context, act HWBounce) {
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	log.Printf("INFO: net-supervisor: Bounce(%s) — reason=%q", act.Device, act.Reason)
	if err := s.devAct.Bounce(bctx, act.Device); err != nil {
		log.Printf("ERROR: net-supervisor: Bounce(%s) failed: %v", act.Device, err)
	}
}

func (s *Supervisor) fireReboot(ctx context.Context, reason string) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.sysAct.Reboot(rctx, reason); err != nil {
		log.Printf("ERROR: net-supervisor: Reboot failed: %v", err)
	}
}

func (s *Supervisor) fireAPEnter(ctx context.Context) {
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	log.Printf("INFO: net-supervisor: APEnter")
	if err := s.apAct.Enter(actx); err != nil {
		log.Printf("ERROR: net-supervisor: APEnter failed: %v", err)
	}
}

func (s *Supervisor) fireAPExit(ctx context.Context) {
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	log.Printf("INFO: net-supervisor: APExit")
	if err := s.apAct.Exit(actx); err != nil {
		log.Printf("ERROR: net-supervisor: APExit failed: %v", err)
	}
}

func (s *Supervisor) isAPActive() bool {
	if s.apAct == nil {
		return false
	}
	return s.apAct.Active()
}

func apReasonFor(a APAction, intent UserIntent, apActive bool) string {
	if intent.ForceAP {
		return "user-forced"
	}
	if apActive {
		return "recovery"
	}
	if _, ok := a.(APEnter); ok {
		return "recovery"
	}
	return ""
}

// publishLegacyEvent emits NetworkStateChangedEvent on TopicNetworkStateChanged
// so existing subscribers (identity.NotifyNetworkUp, stun.Trigger, health
// tracker) continue receiving uplink-up signals after the cutover.
//
// Dedupes against the previously-published (state, uplink, apActive, apSSID)
// tuple so subscribers are only woken on actual transitions — pre-cutover
// publishers fired only on state changes, and the new code's downstream
// subscribers (identity refresh, STUN trigger) coalesce poorly with 30s-tick
// spam.
func (s *Supervisor) publishLegacyEvent(tick Tick, snap Snapshot) {
	if s.bus == nil {
		return
	}
	state := deriveLegacyState(tick, snap.APMode.Active)
	uplink := tick.ActiveUplink
	cur := legacyEventKey{
		state:    string(state),
		uplink:   string(uplink),
		apActive: snap.APMode.Active,
		apSSID:   snap.APMode.SSID,
	}
	s.legacyEventMu.Lock()
	if s.lastLegacyEvent == cur {
		s.legacyEventMu.Unlock()
		return
	}
	s.lastLegacyEvent = cur
	s.legacyEventMu.Unlock()

	s.bus.Publish(events.Event{
		Topic: events.TopicNetworkStateChanged,
		Payload: NetworkStateChangedEvent{
			State:        cur.state,
			ActiveUplink: cur.uplink,
			APActive:     cur.apActive,
			APSSID:       cur.apSSID,
		},
	})
}

func logTick(t Tick, hwW, hwE HWAction, apA APAction) {
	wifi := t.Devices[DeviceWiFi]
	eth := t.Devices[DeviceEthernet]
	log.Printf("INFO: net-supervisor.tick: uplink=%s l3=%s nmconn=%s busy=%v wifi=[hw=%s cfg=%s gw=%s rfkill_hard=%v] eth=[hw=%s cfg=%s gw=%s carrier=%v] decisions=[wifi=%s eth=%s ap=%s]",
		t.ActiveUplink, t.L3Probe, t.NMConn, t.SystemBusy,
		wifi.HWHealth, wifi.ConfigHealth, wifi.GwReachable, wifi.RfkillHard,
		eth.HWHealth, eth.ConfigHealth, eth.GwReachable, eth.LinkUp,
		hwActionString(hwW), hwActionString(hwE), apActionString(apA),
	)
}

func hwActionString(a HWAction) string {
	switch v := a.(type) {
	case HWBounce:
		return "bounce(" + v.Device.String() + ")"
	case HWReboot:
		return "reboot"
	default:
		return "wait"
	}
}

func apActionString(a APAction) string {
	switch a.(type) {
	case APEnter:
		return "enter"
	case APExit:
		return "exit"
	default:
		return "unchanged"
	}
}

// ----- Construction helpers -----

// BuildSupervisor wires the production probe stack against a private dbus
// connection. Used from gin_server.go.
//
// The caller must subsequently:
//   - call SetActuators (typically by calling WireActuators below or
//     building actuators directly)
//   - call SetEventBus(bus) for legacy wire-contract events
//   - call EnableActuation(true) once the cutover is complete
func BuildSupervisor(_ context.Context, r runner.CommandRunner, sys SystemState) (*Supervisor, error) {
	cli, err := nmclient.NewPrivateDBusClient(r)
	if err != nil {
		return nil, err
	}
	prober := NewProber(cli, r, sys)
	store := NewLedgerStore()
	sup := NewSupervisor(prober, store, sys)
	sup.closer = cli
	return sup, nil
}

// WireActuators constructs and installs the production actuators. Caller
// supplies the AP manager (existing one from manager.go); the device-path
// resolver reads from the prober's most recent observation.
func (s *Supervisor) WireActuators(nm nmclient.Client, r runner.CommandRunner, apMgr *ap.Manager) {
	pathFn := func(k DeviceKind) dbus.ObjectPath {
		s.prober.mu.Lock()
		defer s.prober.mu.Unlock()
		return s.prober.devicePath[k]
	}
	ifaceFn := func(k DeviceKind) string {
		t := s.LastTick()
		if obs, ok := t.Devices[k]; ok {
			return obs.Iface
		}
		return ""
	}
	wifiDeviceFn := func() (dbus.ObjectPath, net.HardwareAddr, bool) {
		path := pathFn(DeviceWiFi)
		if path == "" {
			return "", nil, false
		}
		// Fetch the MAC from nmclient — required by ap.Manager.Start.
		devs, err := nm.WiFiDevices()
		if err != nil || len(devs) == 0 {
			return "", nil, false
		}
		return devs[0].Path, devs[0].HWAddress, true
	}

	devAct := NewDeviceActuator(nm, r, pathFn, ifaceFn, func() bool { return s.isAPActive() })
	apAct := NewAPModeActuator(apMgr, wifiDeviceFn)
	sysAct := NewSystemActuator(r, s.sys)
	s.SetActuators(devAct, apAct, sysAct)
}

func summarizeBounces(b map[DeviceKind][]time.Time) string {
	if len(b) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(b))
	for k, v := range b {
		parts = append(parts, fmt.Sprintf("%s=%d", k, len(v)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
