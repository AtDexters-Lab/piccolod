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

// SetSupervisor installs the externally-built supervisor (private bus,
// shared with the rest of gin_server.go). Caller invokes WireActuators
// after this so the supervisor can drive the AP/system actuators.
func (m *Manager) SetSupervisor(sup *Supervisor) {
	m.supervisor = sup
}

// Supervisor returns the underlying supervisor (used by WireActuators
// callers that pass our ap.Manager into the wire path).
func (m *Manager) Supervisor() *Supervisor { return m.supervisor }

// APMgr returns the underlying AP manager. Used by gin_server.go at wire
// time to construct the APModeActuator.
func (m *Manager) APMgr() *ap.Manager { return m.apMgr }

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
	var ssid, ipAddr, band string
	var freqMHz uint32
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
		Band:            band,
	}
	if signalDBm != nil {
		s.SignalDBm = signalDBm
		s.SignalTier = signalTier
	}
	if freqMHz != 0 {
		s.FrequencyMHz = &freqMHz
	}
	return s
}

// APStatus reports AP mode state read from the supervisor's snapshot.
func (m *Manager) APStatus() APStatus {
	snap := m.snapshot()
	suppressed := false
	if m.supervisor != nil {
		suppressed = m.supervisor.IntentSnapshot().SuppressAP
	}
	return APStatus{
		Active:     snap.APMode.Active,
		SSID:       snap.APMode.SSID,
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
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return errNoWifiDevice
	}

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

	newProfilePath, err := m.nm.Connect(dev.Path, ssid, passphrase)
	if err != nil {
		if rollbackSnapshot != nil {
			if rerr := m.nm.RestoreConnection(rollbackSnapshot); rerr != nil {
				log.Printf("ERROR: network: WiFi rollback after connect failure: %v", rerr)
			}
		}
		return err
	}

	// Wait for activation up to 30s.
	finalState, _, err := m.nm.WaitForActivation(ctx, dev.Path)
	if err != nil || finalState != nmclient.NMDeviceStateActivated {
		// Smart rollback: NM may have auto-reconnected.
		if rollbackSnapshot != nil {
			needsRestore := true
			info, err := m.nm.ActiveConnectionInfo(dev.Path)
			if err == nil && info != nil && info.ID == rollbackSnapshot.SSID() {
				needsRestore = false
			}
			if needsRestore {
				if err := m.nm.RestoreConnection(rollbackSnapshot); err != nil {
					log.Printf("ERROR: network: WiFi rollback failed: %v", err)
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
	// any AP active will exit because anyDeviceGwReachable() returns true.
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
	errNoWifiDevice   = &networkError{"no WiFi device available"}
	errConnectTimeout = &networkError{"connection timed out"}
)

type networkError struct{ msg string }

func (e *networkError) Error() string { return e.msg }
