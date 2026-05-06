// Package network implements WiFi LAN uplink management, AP mode with
// captive portal, and network health monitoring for piccolod.
//
// The state machine and procedural watchdog from earlier versions have been
// replaced with a layered probe → decide → act architecture rooted in
// internal/network/supervisor.go. The Manager is now a thin facade that
// preserves existing public API surface (Status, APStatus, Connect,
// ForgetNetwork, ScanNetworks, ForceAPMode, SetAPSuppressed) while
// delegating connectivity classification to the Supervisor.
package network

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/network/ap"
	"piccolod/internal/network/captive"
	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

const apSuppressionPath = "/etc/piccolo/ap-mode-disabled"

// Manager exposes the network REST/event surface and owns the lifecycle of
// the AP machinery + captive portal. The connectivity classification it
// reports is read directly from Supervisor.Snapshot().
type Manager struct {
	nm     nmclient.Client
	runner runner.CommandRunner
	events *events.Bus

	supervisor *Supervisor
	apMgr      *ap.Manager
	sigMon     *signalMonitor

	portalServer *captive.Server

	mu            sync.RWMutex
	wifiDevice    *nmclient.WiFiDevice
	ethDevices    []nmclient.EthernetDevice
	wifiAvailable bool

	scanCache     []ScanResult
	scanCacheTime time.Time
	scanMu        sync.Mutex

	// connectMu serializes Connect attempts. Pre-cutover the state machine's
	// beginLongTransition gate handled this; without serialization,
	// concurrent /api/wifi/connect + captive-portal connects can race on
	// profile snapshot/delete/restore and corrupt the saved-profile set.
	connectMu sync.Mutex

	healthTracker *health.Tracker

	stopOnce sync.Once
	stopCh   chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewManager constructs a Manager. The supervisor is constructed lazily
// from Start (gin_server.go calls SetSupervisor before Start to install the
// production private-bus supervisor). NewManager does not start goroutines.
func NewManager(nm nmclient.Client, r runner.CommandRunner, bus *events.Bus) *Manager {
	m := &Manager{
		nm:     nm,
		runner: r,
		events: bus,
		stopCh: make(chan struct{}),
	}
	m.apMgr = ap.NewManager(nm, r)
	m.sigMon = newSignalMonitor(nm, m)
	return m
}

// AttachSupervisor installs the externally-built supervisor (private bus,
// shared with gin_server.go), wires its actuators against the manager's
// nmclient + ap.Manager, propagates the event bus, loads the persisted
// AP-suppression flag, and enables actuation. One call replaces the
// previous wire dance of SetSupervisor + SetEventBus + WireActuators +
// EnableActuation + LoadAPSuppressionFlag.
func (m *Manager) AttachSupervisor(sup *Supervisor) {
	m.supervisor = sup
	sup.SetEventBus(m.events)
	// Prescan hook: refresh the scan cache before AP entry so the
	// captive portal has fresh BSS results to display. Once AP is up,
	// the WiFi adapter cannot scan, so this is the only chance to
	// prime the cache for first-run / recovery flows.
	prescan := func(ctx context.Context) error {
		_, err := m.ScanNetworks(true)
		return err
	}
	sup.WireActuators(m.nm, m.runner, m.apMgr, prescan)
	m.LoadAPSuppressionFlag()
	sup.EnableActuation(true)
}

// Name implements supervisor.Component.
func (m *Manager) Name() string { return "network" }

// Start implements supervisor.Component.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

	reconcileStartup(m.ctx, m.nm, m.runner)

	if err := m.discoverDevices(); err != nil {
		log.Printf("WARN: network: device discovery failed: %v", err)
	}

	// Wire captive portal callbacks. Connect path drives the supervisor's
	// intent + early-tick request rather than synthesizing its own state
	// transitions.
	m.apMgr.SetPortalCallbacks(
		func(ctx context.Context, listenAddr string) error {
			scanFn := func(forceRefresh bool) ([]captive.ScanResult, error) {
				results, err := m.ScanNetworks(forceRefresh)
				if err != nil {
					return nil, err
				}
				out := make([]captive.ScanResult, len(results))
				for i, r := range results {
					out[i] = captive.ScanResult{
						SSID: r.SSID, Security: r.Security, SignalDBm: r.SignalDBm,
						SignalTier: string(r.SignalTier), FrequencyMHz: r.FrequencyMHz, Band: r.Band,
					}
				}
				return out, nil
			}
			connectFn := func(ssid, passphrase string) {
				m.connectFromCaptivePortal(ssid, passphrase)
			}
			srv, err := captive.NewServer(scanFn, connectFn, m.apMgr.RecordHTTPActivity)
			if err != nil {
				return err
			}
			m.portalServer = srv
			return srv.Start(ctx, listenAddr)
		},
		func() {
			if m.portalServer != nil {
				m.portalServer.Stop()
				m.portalServer = nil
			}
		},
	)

	// Subscribe to the supervisor's legacy state events so we can update
	// the health tracker. Stage-4 cutover collapses the old onTransition
	// callback into this loop.
	if m.events != nil {
		go m.healthLoop(m.ctx)
	}

	go m.sigMon.run(m.ctx)
	go m.deviceEventLoop(m.ctx)
	return nil
}

// Stop implements supervisor.Component.
func (m *Manager) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.cancel()
	})
	return nil
}

// Status returns a snapshot of the current network state for API responses.
// Wire contract preserved via deriveLegacyState.
func (m *Manager) Status() Status {
	tick := m.lastTick()
	snap := m.snapshot()

	connState := deriveLegacyState(tick, snap.APMode.Active)
	uplink := tick.ActiveUplink

	wifiAvail := m.wifiPresent()
	var ssid, ipAddr string
	var signalDBm *int
	var signalTier SignalTier

	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev != nil && uplink == UplinkWiFi {
		if info, err := m.nm.ActiveConnectionInfo(dev.Path); err == nil && info != nil {
			ssid = info.ID
			ipAddr = info.IP4Address
		}
		dbm, tier := m.sigMon.snapshot()
		if dbm != 0 {
			signalDBm = &dbm
			signalTier = tier
		}
	}

	hasSaved, savedSSID := m.savedWifiInfo()

	s := Status{
		WifiAvailable:   wifiAvail,
		State:           connState,
		ActiveUplink:    uplink,
		SSID:            ssid,
		HasSavedNetwork: hasSaved,
		SavedSSID:       savedSSID,
		IPAddress:       ipAddr,
	}
	if signalDBm != nil {
		s.SignalDBm = signalDBm
		s.SignalTier = signalTier
	}
	return s
}

// APStatus reports AP mode state. Reads ap.Manager directly (not the
// supervisor's last-tick snapshot) so callers polling immediately after
// ForceAPMode/SetAPSuppressed don't see stale state — actuators run
// asynchronously after the tick snapshot is built; without a live read,
// state would lag up to 30s.
func (m *Manager) APStatus() APStatus {
	suppressed := false
	if m.supervisor != nil {
		suppressed = m.supervisor.IntentSnapshot().SuppressAP
	}
	return APStatus{
		Active:     m.apMgr.Active(),
		SSID:       m.apMgr.SSID(),
		Suppressed: suppressed,
	}
}

// ScanNetworks triggers a WiFi scan and returns results.
func (m *Manager) ScanNetworks(forceRefresh bool) ([]ScanResult, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return nil, nil
	}

	// During AP mode the WiFi adapter cannot scan — serve cached.
	if m.apMgr.Active() || m.apMgr.ActiveOnNM(dev.Path) {
		return m.scanCache, nil
	}

	if !forceRefresh && time.Since(m.scanCacheTime) < 15*time.Second && len(m.scanCache) > 0 {
		return m.scanCache, nil
	}

	aps, err := m.nm.Scan(dev.Path)
	if err != nil {
		return nil, err
	}

	ownSSID := m.apMgr.SSID()
	results := make([]ScanResult, 0, len(aps))
	for _, ap := range aps {
		if ownSSID != "" && ap.SSID == ownSSID {
			continue
		}
		dbm := ap.SignalDBm()
		results = append(results, ScanResult{
			SSID:         ap.SSID,
			Security:     ap.SecurityType(),
			SignalDBm:    dbm,
			SignalTier:   ClassifySignal(dbm),
			FrequencyMHz: ap.Frequency,
			Band:         ap.Band(),
		})
	}

	m.scanCache = results
	m.scanCacheTime = time.Now()
	return results, nil
}

// Connect attempts to join a WiFi network. Snapshots the current profile
// for rollback; on success, requests a probe-and-decide so the supervisor
// re-evaluates immediately.
func (m *Manager) Connect(ctx context.Context, ssid, passphrase string) error {
	m.connectMu.Lock()
	defer m.connectMu.Unlock()

	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return errNoWifiDevice
	}

	// Byte-level boundary log — what reached us vs. what NM is about to see.
	// Lengths only; never log secret content. Combined with the device-state
	// snapshot below this lets us distinguish A1 (empty/wrong PSK reaches NM)
	// from A4/A5 (right PSK, NM rejects on its own). devState helps spot a
	// post-AP-teardown race where wlan0 is not yet in a clean managed state.
	devState, _ := m.nm.DeviceState(dev.Path)
	log.Printf("INFO: network: Connect entry ssid_len=%d psk_len=%d wlan0_state=%s",
		len(ssid), len(passphrase), devState)

	profiles, _ := m.nm.SavedWiFiConnections()
	var rollbackSnapshot *nmclient.ConnectionSnapshot
	var deferredDeletePaths []dbus.ObjectPath

	for _, p := range profiles {
		if p.SSID != ssid {
			deferredDeletePaths = append(deferredDeletePaths, p.Path)
		} else if rollbackSnapshot == nil {
			snap, err := m.nm.SnapshotConnection(p.Path)
			if err == nil && snap != nil {
				snap.Device = dev.Path
				rollbackSnapshot = snap
			}
		}
	}
	if rollbackSnapshot == nil && len(deferredDeletePaths) > 0 {
		snap, err := m.nm.SnapshotConnection(deferredDeletePaths[0])
		if err == nil && snap != nil {
			snap.Device = dev.Path
			rollbackSnapshot = snap
		}
	}

	newProfilePath, newActivePath, err := m.nm.Connect(dev.Path, ssid, passphrase)
	if err != nil {
		if rollbackSnapshot != nil {
			if rerr := m.nm.RestoreConnection(rollbackSnapshot); rerr != nil {
				log.Printf("ERROR: network: WiFi rollback after connect failure: %v", rerr)
			}
		}
		return err
	}

	// Wait for activation up to 30s. The 30s budget is enforced by a
	// derived context — the caller's ctx is typically m.ctx (long-lived),
	// and NM can stall indefinitely in an Activating state without
	// emitting a terminal signal (saw this on rtw88_8723de under load).
	// Without the bound, the captive-portal flow blocks here forever and
	// the AP never re-enters until piccolod stops.
	//
	// WaitForActivation returns Activated for ANY currently-active
	// connection on the device — including the still-active old profile
	// if NM hasn't transitioned yet (TOCTOU on the synchronous-state-check
	// fast path). Verify the activated SSID matches the requested one
	// before declaring success; mismatch routes to the failure branch
	// and triggers rollback.
	waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
	waitStart := time.Now()
	finalState, finalReason, err := m.nm.WaitForActivation(waitCtx, dev.Path, newActivePath)
	cancelWait()
	waitElapsed := time.Since(waitStart)
	activated := err == nil && finalState == nmclient.NMDeviceStateActivated
	var activeID string
	if activated {
		info, infoErr := m.nm.ActiveConnectionInfo(dev.Path)
		if infoErr != nil || info == nil || info.ID != ssid {
			activated = false
			if info != nil {
				activeID = info.ID
			}
			if err == nil {
				err = errConnectWrongSSID
			}
		}
	}
	if !activated {
		// Replace the "connection timed out" black box with what actually
		// happened: NM's terminal device state, the reason code (e.g.
		// NoSecrets=7 vs SupplicantTimeout=11 vs ConnectionRemoved=38),
		// elapsed wait, and any active SSID mismatch.
		log.Printf("WARN: network: Connect failed ssid=%q final_state=%s reason=%s wait_elapsed=%s wait_err=%v active_id=%q",
			ssid, finalState, finalReason, waitElapsed, err, activeID)
		// Smart rollback: NM may have auto-reconnected.
		if rollbackSnapshot != nil {
			needsRestore := true
			info, infoErr := m.nm.ActiveConnectionInfo(dev.Path)
			if infoErr == nil && info != nil && info.ID == rollbackSnapshot.SSID() {
				needsRestore = false
			}
			if needsRestore {
				if rerr := m.nm.RestoreConnection(rollbackSnapshot); rerr != nil {
					log.Printf("ERROR: network: WiFi rollback failed: %v", rerr)
				}
			}
		}
		if newProfilePath != "" {
			_ = m.nm.DeleteConnection(newProfilePath)
		}
		if err != nil {
			return err
		}
		return errConnectTimeout
	}

	// Success — delete old profiles to enforce single-SSID policy.
	for _, path := range deferredDeletePaths {
		if err := m.nm.DeleteConnection(path); err != nil {
			log.Printf("WARN: network: failed to delete old WiFi profile %s: %v", path, err)
		}
	}

	// Hint the supervisor — ConfigHealth flips Healthy on the next tick;
	// any AP active will exit because anyDeviceWorking() returns true.
	if m.supervisor != nil {
		m.supervisor.RequestProbeAndDecide()
	}

	return nil
}

// ForgetNetwork deletes all saved WiFi profiles. The supervisor's APArbiter
// observes ConfigHealth=Inactive and enters AP mode on the next tick.
func (m *Manager) ForgetNetwork() error {
	profiles, err := m.nm.SavedWiFiConnections()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if err := m.nm.DeleteConnection(p.Path); err != nil {
			return err
		}
	}
	if m.supervisor != nil {
		m.supervisor.RequestProbeAndDecide()
	}
	return nil
}

// ForceAPMode signals the supervisor to enter AP mode and waits for the AP
// + portal to come up. Used by manual integration tests.
func (m *Manager) ForceAPMode() error {
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return errNoWifiDevice
	}
	if m.supervisor == nil {
		return errors.New("network: supervisor not wired")
	}
	m.supervisor.SetForceAP(true)
	m.supervisor.RequestProbeAndDecide()

	if !m.waitForAP() {
		return fmt.Errorf("AP activation timed out")
	}
	return nil
}

// SetAPSuppressed enables or disables AP-mode suppression. Persisted to
// /etc/piccolo/ap-mode-disabled and propagated to the supervisor's intent.
func (m *Manager) SetAPSuppressed(suppress bool) error {
	if m.supervisor != nil {
		m.supervisor.SetSuppressAP(suppress)
	}
	if suppress {
		return os.WriteFile(apSuppressionPath, []byte("1\n"), 0o644)
	}
	if err := os.Remove(apSuppressionPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetHealthTracker wires the health tracker (called before Start).
func (m *Manager) SetHealthTracker(ht *health.Tracker) { m.healthTracker = ht }

// SetEventBus wires the event bus (called before Start).
func (m *Manager) SetEventBus(bus *events.Bus) {
	m.events = bus
	if m.supervisor != nil {
		m.supervisor.SetEventBus(bus)
	}
}

// connectFromCaptivePortal handles the captive-portal connect flow. Tear
// down AP first (single radio), attempt the STA connection, then either
// celebrate via supervisor.RequestProbeAndDecide or signal failure (the
// supervisor will re-enter AP automatically next tick if config is broken).
func (m *Manager) connectFromCaptivePortal(ssid, passphrase string) {
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return
	}

	log.Printf("INFO: network: captive portal connect to %q — tearing down AP", ssid)
	if m.portalServer != nil {
		m.portalServer.Stop()
	}
	// Record the AP exit in the supervisor's ledger BEFORE the direct stop,
	// so the rate-shaper sees the toggle even though we bypass the
	// supervisor's actuator (single-radio constraint forces a synchronous
	// stop here — the actuator path would race with the impending Connect).
	if m.supervisor != nil {
		m.supervisor.RecordExternalAPExit("captive-portal-connect")
	}
	m.apMgr.Stop(m.ctx, dev.Path)

	// Clear the user-forced flag (captive portal credential entry implies
	// "let recovery resume" — N4-S2). Defensive no-op when already false.
	if m.supervisor != nil {
		m.supervisor.SetForceAP(false)
	}

	if err := m.Connect(m.ctx, ssid, passphrase); err == nil {
		log.Printf("INFO: network: captive portal connect to %q succeeded", ssid)
		return
	} else {
		log.Printf("WARN: network: captive portal connect to %q failed: %v", ssid, err)
	}

	// Supervisor will re-enter AP on the next tick if config still broken.
	// Surface error on the next portal server (constructed by the AP
	// re-enter path).
	if m.supervisor != nil {
		m.supervisor.RequestProbeAndDecide()
	}
	if !m.waitForAP() {
		log.Printf("ERROR: network: AP reactivation timed out")
		return
	}
	if m.portalServer != nil {
		m.portalServer.SetConnectError("Connection to '" + ssid + "' failed — check your password and try again.")
	}
}

func (m *Manager) waitForAP() bool {
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return false
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for i := 0; i < 60; i++ { // up to 30s
		if m.apMgr.Active() || m.apMgr.ActiveOnNM(dev.Path) {
			return true
		}
		select {
		case <-m.ctx.Done():
			return false
		case <-tick.C:
		}
	}
	return false
}

// healthLoop subscribes to TopicNetworkStateChanged and updates the health
// tracker level.
func (m *Manager) healthLoop(ctx context.Context) {
	if m.events == nil {
		return
	}
	ch, cancel := m.events.SubscribeWithCancel(events.TopicNetworkStateChanged, 8)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			ne, ok := evt.Payload.(NetworkStateChangedEvent)
			if !ok || m.healthTracker == nil {
				continue
			}
			switch ConnState(ne.State) {
			case StateEthernet, StateWiFiSTA:
				m.healthTracker.Setf("network", health.LevelOK, "connected (%s)", ne.State)
			case StateReconnecting, StateAPMode:
				m.healthTracker.Setf("network", health.LevelWarn, "%s", ne.State)
			case StateDisconnected:
				m.healthTracker.Setf("network", health.LevelError, "disconnected")
			}
		}
	}
}

// savedWifiInfo reports whether any WiFi profile is saved + the first SSID.
func (m *Manager) savedWifiInfo() (bool, string) {
	profiles, err := m.nm.SavedWiFiConnections()
	if err != nil || len(profiles) == 0 {
		return false, ""
	}
	return true, profiles[0].SSID
}

// snapshot returns the supervisor's latest snapshot (or zero if absent).
func (m *Manager) snapshot() Snapshot {
	if m.supervisor == nil {
		return Snapshot{}
	}
	return m.supervisor.Snapshot()
}

// lastTick returns the supervisor's most recent tick (or zero if absent).
func (m *Manager) lastTick() Tick {
	if m.supervisor == nil {
		return Tick{}
	}
	return m.supervisor.LastTick()
}

func (m *Manager) wifiPresent() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.wifiAvailable
}

// discoverDevices queries NM for current devices and selects the primary
// WiFi + Ethernet interfaces. Re-run on every device add/remove event.
func (m *Manager) discoverDevices() error {
	wifiDevs, err := m.nm.WiFiDevices()
	if err != nil {
		return err
	}
	ethDevs, err := m.nm.EthernetDevices()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.ethDevices = ethDevs

	if len(wifiDevs) > 0 {
		dev := wifiDevs[0]
		m.wifiDevice = &dev
		m.wifiAvailable = true
		m.sigMon.setDevice(dev.Path)
		log.Printf("INFO: network: WiFi device: %s (driver=%s)", dev.Interface, dev.Driver)
	} else {
		m.wifiAvailable = false
		log.Printf("INFO: network: no WiFi device found")
	}
	for _, eth := range ethDevs {
		log.Printf("INFO: network: Ethernet device: %s (carrier=%v)", eth.Interface, eth.Carrier)
	}

	return nil
}

// deviceEventLoop listens for hotplug events and re-discovers devices.
func (m *Manager) deviceEventLoop(ctx context.Context) {
	ch, err := m.nm.SubscribeDeviceAddedRemoved(ctx)
	if err != nil {
		log.Printf("WARN: network: device event subscription failed: %v", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			log.Printf("INFO: network: device %s: %s", map[nmclient.DeviceEventType]string{
				nmclient.DeviceAdded:   "added",
				nmclient.DeviceRemoved: "removed",
			}[evt.Type], evt.Device)
			if err := m.discoverDevices(); err != nil {
				log.Printf("WARN: network: device re-discovery failed: %v", err)
			}
			if m.supervisor != nil {
				m.supervisor.RequestProbeAndDecide()
			}
		}
	}
}

// LoadAPSuppressionFlag reads /etc/piccolo/ap-mode-disabled at supervisor
// wire-time and, if present, propagates it as boot-default SuppressAP=true.
// Boot-volatile semantics (N4-S4): only this disk-backed file persists; the
// in-memory intent is always rebuilt from it on each piccolod start.
func (m *Manager) LoadAPSuppressionFlag() {
	_, err := os.Stat(apSuppressionPath)
	if err == nil && m.supervisor != nil {
		m.supervisor.SetSuppressAP(true)
	}
}

// Sentinel errors.
var (
	errNoWifiDevice     = &networkError{"no WiFi device available"}
	errConnectTimeout   = &networkError{"connection timed out"}
	errConnectWrongSSID = &networkError{"activation reported success on a different SSID"}
)

type networkError struct{ msg string }

func (e *networkError) Error() string { return e.msg }
