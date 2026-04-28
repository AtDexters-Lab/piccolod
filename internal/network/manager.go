package network

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/network/ap"
	"piccolod/internal/network/captive"
	"piccolod/internal/network/nmclient"
	"piccolod/internal/runner"
)

// Manager is the network management supervisor component. It owns the
// connectivity state machine, network watchdog, WiFi signal monitor, and
// coordinates AP mode lifecycle.
//
// Implements supervisor.Component (Name/Start/Stop).
type Manager struct {
	nm     nmclient.Client
	runner runner.CommandRunner
	events *events.Bus

	state        *stateMachine
	wd           *watchdog
	sigMon       *signalMonitor
	apMgr        *ap.Manager
	portalServer *captive.Server

	// WiFi device tracking
	mu             sync.RWMutex
	wifiDevice     *nmclient.WiFiDevice
	ethDevices     []nmclient.EthernetDevice
	wifiAvailable  bool
	apSuppressed   bool
	apRetryActive  atomic.Bool  // true while AP retry goroutine is running
	apRetryCount   atomic.Int32 // persists across goroutine restarts from WiFi bounces

	// Scan cache
	scanCache     []ScanResult
	scanCacheTime time.Time
	scanMu        sync.Mutex

	// Health reporting
	healthTracker *health.Tracker

	// Lifecycle
	stopOnce sync.Once
	stopCh   chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewManager creates a network Manager. It does NOT start any goroutines;
// call Start() to begin monitoring.
func NewManager(nm nmclient.Client, r runner.CommandRunner, bus *events.Bus) *Manager {
	sm := newStateMachine(bus)
	m := &Manager{
		nm:     nm,
		runner: r,
		events: bus,
		state:  sm,
		sigMon: newSignalMonitor(nm, sm),
		stopCh: make(chan struct{}),
	}
	m.wd = newWatchdog(nm, r, bus, sm.Current)
	m.apMgr = ap.NewManager(nm, r)
	return m
}

// Name implements supervisor.Component.
func (m *Manager) Name() string { return "network" }

// Start implements supervisor.Component. It runs startup reconciliation,
// discovers devices, and launches background goroutines.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

	// Wire state transition callback. AP lifecycle is unconditional (core
	// functionality), health reporting is optional (observability).
	m.state.onTransition = func(newState ConnState) {
		if m.healthTracker != nil {
			switch newState {
			case StateEthernet, StateWiFiSTA:
				m.healthTracker.Setf("network", health.LevelOK, "connected (%s)", newState)
			case StateReconnecting, StateAPMode:
				m.healthTracker.Setf("network", health.LevelWarn, "%s", newState)
			case StateDisconnected:
				m.healthTracker.Setf("network", health.LevelError, "disconnected")
			}
		}
		m.handleAPTransition(newState)
	}

	// Startup reconciliation: clean stale AP/firewall/dnsmasq from previous crash
	reconcileStartup(m.ctx, m.nm, m.runner)

	// Discover devices
	if err := m.discoverDevices(); err != nil {
		log.Printf("WARN: network: device discovery failed: %v", err)
	}

	// Wire WiFi state query for handleEthernetDown — lets the state machine
	// check if WiFi STA is already active when Ethernet drops.
	m.state.wifiConnected = func() bool {
		m.mu.RLock()
		dev := m.wifiDevice
		m.mu.RUnlock()
		if dev == nil {
			return false
		}
		state, err := m.nm.DeviceState(dev.Path)
		if err != nil {
			return false // D-Bus error — safe default
		}
		return state.IsConnected()
	}

	// Load AP suppression flag
	m.loadAPSuppression()

	// Wire captive portal to AP manager
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

	// Determine initial state based on current NM status
	m.determineInitialState()

	// Launch background loops
	go m.wd.run(m.ctx)
	go m.sigMon.run(m.ctx)
	go m.deviceEventLoop(m.ctx)
	go m.nmStateLoop(m.ctx)

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
func (m *Manager) Status() Status {
	connState, uplink, signalDBm, signalTier := m.state.snapshot()

	m.mu.RLock()
	wifiAvail := m.wifiAvailable
	var ssid, ipAddr, band string
	var freqMHz uint32

	if m.wifiDevice != nil && uplink == UplinkWiFi {
		info, err := m.nm.ActiveConnectionInfo(m.wifiDevice.Path)
		if err == nil && info != nil {
			ssid = info.ID
			ipAddr = info.IP4Address
		}
	}
	m.mu.RUnlock()

	// Query NM directly — no cached state.
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
	if signalDBm != 0 && uplink == UplinkWiFi {
		s.SignalDBm = &signalDBm
		s.SignalTier = signalTier
	}
	if freqMHz != 0 {
		s.FrequencyMHz = &freqMHz
	}
	return s
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

	// During AP mode, the WiFi adapter cannot scan — serve cached results.
	// The pre-scan in handleAPTransition populated scanCache before AP started.
	// Check local hint first (fast), then NM ground truth (catches orphaned
	// hotspot where local active bool desynced from NM).
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

	// Filter out our own AP SSID if we're broadcasting one
	ownSSID := m.apSSID()
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

// Connect attempts to join a WiFi network. It uses the state machine's
// preemptible transition mechanism so Ethernet can cancel it.
//
// Profile deletion is deferred until after successful connection to prevent
// the state machine from entering AP mode during the intermediate disconnect.
// When NM disconnects from the old network to try the new one, the old profile
// still exists, so handleWiFiDisconnected sees hasSavedWifi=true and transitions
// to StateReconnecting (not StateAPMode), avoiding the AP/STA race on the
// single WiFi radio.
func (m *Manager) Connect(ctx context.Context, ssid, passphrase string) error {
	staCtx, ok := m.state.beginLongTransition()
	if !ok {
		return errTransitionInProgress
	}
	defer m.state.endLongTransition()

	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return errNoWifiDevice
	}

	// Snapshot for rollback and collect other-SSID paths for deferred deletion.
	//
	// Two cases:
	// 1. Switching SSIDs (A → B): snapshot A for rollback, defer A's deletion.
	// 2. Same-SSID reconnect (A → A with new password): snapshot A for
	//    rollback. nmclient.Connect deletes same-SSID profiles internally,
	//    so the snapshot is the only way to restore on failure.
	profiles, _ := m.nm.SavedWiFiConnections()
	var rollbackSnapshot *nmclient.ConnectionSnapshot
	var deferredDeletePaths []dbus.ObjectPath

	// First pass: prefer snapshotting the same-SSID profile (for password
	// change rollback — nmclient.Connect deletes same-SSID profiles internally).
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
	// Fallback: if no same-SSID profile exists (pure cross-SSID switch),
	// snapshot the first other-SSID profile for rollback.
	if rollbackSnapshot == nil && len(deferredDeletePaths) > 0 {
		snap, err := m.nm.SnapshotConnection(deferredDeletePaths[0])
		if err == nil && snap != nil {
			snap.Device = dev.Path
			rollbackSnapshot = snap
		}
	}

	// Connect: nmclient.Connect deletes same-SSID profiles (D-Bus type
	// round-trip issue) and creates a fresh one. Other-SSID profiles are
	// kept alive so the state machine stays in StateReconnecting, not AP mode.
	// Returns the new profile's D-Bus path for precise cleanup on failure.
	newProfilePath, err := m.nm.Connect(dev.Path, ssid, passphrase)
	if err != nil {
		// nmclient.Connect may have already deleted the same-SSID profile
		// before failing. Restore from snapshot to avoid losing the only
		// saved WiFi configuration.
		if rollbackSnapshot != nil {
			if restoreErr := m.nm.RestoreConnection(rollbackSnapshot); restoreErr != nil {
				log.Printf("ERROR: network: WiFi rollback after connect failure: %v", restoreErr)
			}
		}
		return err
	}

	// Wait for activation (up to 30s) or preemption
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	var connectErr error
	select {
	case <-staCtx.Done():
		connectErr = context.Canceled
	case <-ctx.Done():
		connectErr = ctx.Err()
	case <-timer.C:
		// Timeout — verify connected to the CORRECT SSID, not the old
		// network (NM autoconnect) or an orphaned AP hotspot.
		info, err := m.nm.ActiveConnectionInfo(dev.Path)
		if err != nil || info == nil || info.ID != ssid {
			connectErr = errConnectTimeout
		}
	}

	if connectErr != nil {
		// Smart rollback: NM may have auto-reconnected to the old profile
		// (it still exists with autoconnect=true for cross-SSID case).
		// Check before restoring to avoid creating duplicate profiles.
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
		// Delete the specific failed profile by its D-Bus path — no SSID
		// scanning needed, no risk of deleting the just-restored profile.
		if newProfilePath != "" {
			_ = m.nm.DeleteConnection(newProfilePath)
		}
		return connectErr
	}

	// Success — enforce single-SSID policy by deleting old profiles.
	// The new connection is established, so this won't trigger AP mode.
	for _, path := range deferredDeletePaths {
		if err := m.nm.DeleteConnection(path); err != nil {
			log.Printf("WARN: network: failed to delete old WiFi profile %s: %v", path, err)
		}
	}

	m.state.handleWiFiConnected()
	return nil
}

// ForgetNetwork deletes the saved WiFi connection profile.
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

	m.state.mu.Lock()
	if m.state.current == StateWiFiSTA || m.state.current == StateReconnecting || m.state.current == StateDisconnected {
		m.state.transition(StateAPMode, UplinkNone)
	}
	m.state.mu.Unlock()

	return nil
}

// APStatus returns the current AP mode status.
func (m *Manager) APStatus() APStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	active := m.state.Current() == StateAPMode
	ssid := ""
	if active {
		ssid = m.apSSID()
	}
	return APStatus{
		Active:     active,
		SSID:       ssid,
		Suppressed: m.apSuppressed,
	}
}

// SetAPSuppressed enables or disables AP mode suppression.
func (m *Manager) SetAPSuppressed(suppress bool) error {
	m.mu.Lock()
	m.apSuppressed = suppress
	m.mu.Unlock()
	return m.saveAPSuppression(suppress)
}

// waitForAP polls for AP activation for up to 20s.
// Checks both the local fast-path (Active) and NM ground truth (ActiveOnNM)
// to catch cases where Start()'s WaitForActivation misread a transient state.
func (m *Manager) waitForAP() bool {
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return false
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 40; i++ {
		if m.apMgr.Active() {
			return true
		}
		if m.apMgr.ActiveOnNM(dev.Path) {
			log.Printf("WARN: network: AP detected on NM but local state is inactive (desync) — portal may not be running")
			return true
		}
		select {
		case <-m.ctx.Done():
			return false
		case <-ticker.C:
		}
	}
	return false
}

// ForceAPMode transitions directly to AP mode and waits for the AP + portal
// to be ready. Bypasses the state machine's slow backoff escalation. Used by
// the manual integration test and could be used for forced AP recovery.
func (m *Manager) ForceAPMode() error {
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return errNoWifiDevice
	}

	m.state.mu.Lock()
	m.state.transition(StateAPMode, UplinkNone)
	m.state.mu.Unlock()
	// handleAPTransition fires from onTransition, starts AP in a goroutine

	if !m.waitForAP() {
		return fmt.Errorf("AP activation timed out")
	}
	return nil
}

// handleAPTransition starts or stops AP mode based on state transitions.
// Called from the onTransition callback (which fires under sm.mu — so this
// method must not acquire sm.mu).
func (m *Manager) handleAPTransition(newState ConnState) {
	m.mu.RLock()
	dev := m.wifiDevice
	suppressed := m.apSuppressed
	m.mu.RUnlock()

	if newState == StateAPMode {
		if suppressed || dev == nil {
			if suppressed {
				log.Printf("INFO: network: AP mode suppressed by user setting")
			} else {
				log.Printf("WARN: network: cannot activate AP — no WiFi device")
			}
			go m.revertAPState() // async: called from onTransition which holds sm.mu
			return
		}
		// Guard: only one retry goroutine at a time. During AP retry, NM
		// may briefly autoconnect to saved WiFi (causing a state bounce
		// through wifi_connected back to ap_mode), which would re-trigger
		// handleAPTransition. The guard prevents spawning a duplicate;
		// the persistent apRetryCount survives goroutine restarts so the
		// counter advances across bounces instead of resetting to 0.
		//
		// Note: connectFromCaptivePortal handles its own AP restart path
		// and only runs when the AP is active (apRetryActive is false).
		if !m.apRetryActive.CompareAndSwap(false, true) {
			return // retry goroutine already running
		}
		go func() {
			defer m.apRetryActive.Store(false)
			defer m.apRetryCount.Store(0) // reset for next AP cycle on any exit

			// Pre-scan while adapter is still in STA mode. Once AP starts,
			// NM cannot scan — ScanNetworks serves these cached results.
			// Clear stale cache first so a failed pre-scan shows manual
			// entry instead of networks from a previous environment.
			m.scanMu.Lock()
			m.scanCache = nil
			m.scanMu.Unlock()
			if _, err := m.ScanNetworks(true); err != nil {
				log.Printf("WARN: network: pre-AP scan failed: %v", err)
			}

			const maxAPRetries = 3
			for {
				attempt := int(m.apRetryCount.Load())
				if attempt >= maxAPRetries {
					break
				}

				// Re-read device each attempt (handles USB hotplug)
				m.mu.RLock()
				dev := m.wifiDevice
				m.mu.RUnlock()
				if dev == nil {
					break
				}

				// Abort if state changed away from APMode (WiFi/Ethernet arrived)
				if m.state.Current() != StateAPMode {
					return
				}

				// Backoff before retries (not before first attempt)
				if attempt > 0 {
					delay := time.Duration(attempt) * 10 * time.Second
					log.Printf("INFO: network: AP retry %d/%d in %v", attempt+1, maxAPRetries, delay)
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-m.ctx.Done():
						timer.Stop()
						return
					}
					if m.state.Current() != StateAPMode {
						return
					}
				}

				if err := m.apMgr.Start(m.ctx, dev.Path, dev.HWAddress); err == nil {
					return // success
				} else {
					m.apRetryCount.Add(1)
					log.Printf("ERROR: network: AP activation failed (attempt %d/%d): %v", attempt+1, maxAPRetries, err)
				}
			}
			log.Printf("ERROR: network: AP activation failed after %d attempts, giving up", maxAPRetries)
			m.revertAPState()
		}()
	} else {
		// Leaving AP mode — tear down hotspot. Use ForceStop to catch
		// orphaned hotspots where the local active bool desynced from NM
		// (e.g., WaitForActivation misread a transient NM state).
		if dev != nil {
			go func() {
				m.apMgr.ForceStop(m.ctx, dev.Path)
			}()
		}
	}
}

// connectFromCaptivePortal handles the WiFi connect flow initiated from the
// captive portal. Unlike Connect() (called from the REST API), this method
// explicitly tears down the AP before attempting STA connection, and restarts
// the AP + portal with an error message on failure. This avoids the slow
// state-machine backoff path (which takes minutes to re-enter AP mode).
func (m *Manager) connectFromCaptivePortal(ssid, passphrase string) {
	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()
	if dev == nil {
		return
	}

	// 1. Stop captive portal + AP (card can't do AP+STA simultaneously)
	log.Printf("INFO: network: captive portal connect to %q — tearing down AP", ssid)
	if m.portalServer != nil {
		m.portalServer.Stop()
	}
	m.apMgr.Stop(m.ctx, dev.Path)

	// 2. Attempt STA connection (with snapshot/rollback)
	err := m.Connect(m.ctx, ssid, passphrase)

	if err == nil {
		// Connect() calls handleWiFiConnected which updates the state machine
		log.Printf("INFO: network: captive portal connect to %q succeeded", ssid)
		return
	}

	// 3. Failed — reactivate AP (unless rollback already restored connectivity).
	log.Printf("WARN: network: captive portal connect to %q failed: %v — reactivating AP", ssid, err)
	select {
	case <-time.After(2 * time.Second): // let NM settle after failed connect
	case <-m.ctx.Done():
		return
	}

	// If rollback reconnected WiFi or Ethernet appeared during the settle
	// window, the state machine already reflects working connectivity.
	// Don't override it by forcing AP mode.
	m.state.mu.Lock()
	cur := m.state.current
	if cur == StateWiFiSTA || cur == StateEthernet {
		m.state.mu.Unlock()
		log.Printf("INFO: network: uplink recovered after failed connect to %q — not reactivating AP", ssid)
		return
	}

	// If state machine is already in APMode (common: wrong password never
	// leaves APMode), transition() is a no-op. Start AP directly.
	// If state changed (e.g., Reconnecting), transition to APMode and
	// let handleAPTransition start it.
	alreadyAP := cur == StateAPMode
	if !alreadyAP {
		m.state.transition(StateAPMode, UplinkNone)
	}
	m.state.mu.Unlock()

	if alreadyAP {
		go func() {
			// Pre-scan while adapter is in STA mode (AP was torn down for
			// connect attempt). Refreshes the portal's network list.
			m.ScanNetworks(true)
			if startErr := m.apMgr.Start(m.ctx, dev.Path, dev.HWAddress); startErr != nil {
				log.Printf("ERROR: network: AP reactivation failed: %v", startErr)
			}
		}()
	}

	if !m.waitForAP() {
		log.Printf("ERROR: network: AP reactivation timed out")
		return
	}

	// Set error message on the new portal server
	if m.portalServer != nil {
		m.portalServer.SetConnectError("Connection to '" + ssid + "' failed — check your password and try again.")
	}
}

// apSSID returns the current AP SSID if broadcasting, empty otherwise.
func (m *Manager) apSSID() string {
	if m.apMgr != nil {
		return m.apMgr.SSID()
	}
	return ""
}

// revertAPState transitions out of StateAPMode when AP activation cannot
// proceed (hardware unsupported, suppressed, no device). Without this the
// watchdog and state machine would be wedged — both skip during APMode.
func (m *Manager) revertAPState() {
	m.state.mu.Lock()
	if m.state.current == StateAPMode {
		m.state.transition(StateDisconnected, UplinkNone)
	}
	m.state.mu.Unlock()
}

// savedWifiInfo queries NM for saved WiFi profiles. Returns (exists, ssid).
func (m *Manager) savedWifiInfo() (bool, string) {
	profiles, err := m.nm.SavedWiFiConnections()
	if err != nil || len(profiles) == 0 {
		return false, ""
	}
	return true, profiles[0].SSID
}

// SetHealthTracker wires the health tracker (called before Start).
func (m *Manager) SetHealthTracker(ht *health.Tracker) {
	m.healthTracker = ht
}

// SetEventBus wires the event bus (called before Start).
func (m *Manager) SetEventBus(bus *events.Bus) {
	m.events = bus
	m.state.events = bus
	if m.wd != nil {
		m.wd.events = bus
	}
}

// discoverDevices queries NM for current devices and selects the primary WiFi
// and Ethernet interfaces.
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
		dev := wifiDevs[0] // lowest wlan* index (sorted by nmclient)
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

// determineInitialState sets the state machine based on current NM status.
func (m *Manager) determineInitialState() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Check Ethernet first (priority)
	for _, eth := range m.ethDevices {
		if eth.State.IsConnected() {
			m.state.mu.Lock()
			m.state.transition(StateEthernet, UplinkEthernet)
			m.state.mu.Unlock()
			return
		}
	}

	// Check WiFi
	if m.wifiDevice != nil && m.wifiDevice.State.IsConnected() {
		m.state.mu.Lock()
		m.state.transition(StateWiFiSTA, UplinkWiFi)
		m.state.mu.Unlock()
		return
	}

	// Check for saved WiFi connections
	if hasSaved, _ := m.savedWifiInfo(); hasSaved {
		// Only enter Reconnecting if the WiFi device is present and its
		// radio is enabled — otherwise NM cannot auto-connect and the state
		// would wedge (watchdog skips Reconnecting).
		if m.wifiDevice != nil && m.wifiDevice.State != nmclient.NMDeviceStateUnavailable {
			m.state.mu.Lock()
			m.state.transition(StateReconnecting, UplinkNone)
			m.state.mu.Unlock()
			return
		}
	}

	// No usable WiFi connection. Activate AP mode only when the WiFi
	// hardware is present, the radio is available, and AP is not suppressed.
	if m.wifiDevice != nil && m.wifiDevice.State != nmclient.NMDeviceStateUnavailable && !m.apSuppressed {
		m.state.mu.Lock()
		m.state.transition(StateAPMode, UplinkNone)
		m.state.mu.Unlock()
		return
	}

	// No usable WiFi or AP suppressed — genuinely disconnected.
	m.state.mu.Lock()
	m.state.transition(StateDisconnected, UplinkNone)
	m.state.mu.Unlock()
}

// deviceEventLoop listens for NM device hotplug events.
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

			// Re-discover devices
			if err := m.discoverDevices(); err != nil {
				log.Printf("WARN: network: device re-discovery failed: %v", err)
			}
		}
	}
}

// nmStateLoop listens for NM device state changes and drives the state machine.
func (m *Manager) nmStateLoop(ctx context.Context) {
	m.mu.RLock()
	var wifiPath, ethPaths []dbus.ObjectPath
	if m.wifiDevice != nil {
		wifiPath = append(wifiPath, m.wifiDevice.Path)
	}
	for _, eth := range m.ethDevices {
		ethPaths = append(ethPaths, eth.Path)
	}
	m.mu.RUnlock()

	// Subscribe to WiFi device state changes
	for _, path := range wifiPath {
		ch, err := m.nm.SubscribeDeviceStateChanges(ctx, path)
		if err != nil {
			log.Printf("WARN: network: WiFi state subscription failed: %v", err)
			continue
		}
		go m.handleWifiStateChanges(ctx, ch)
	}

	// Subscribe to Ethernet device state changes
	for _, path := range ethPaths {
		ch, err := m.nm.SubscribeDeviceStateChanges(ctx, path)
		if err != nil {
			log.Printf("WARN: network: Ethernet state subscription failed: %v", err)
			continue
		}
		go m.handleEthStateChanges(ctx, ch)
	}

	// Block until context is cancelled
	<-ctx.Done()
}

// handleWifiStateChanges processes WiFi device state transitions.
func (m *Manager) handleWifiStateChanges(ctx context.Context, ch <-chan nmclient.DeviceStateChange) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			switch {
			case evt.NewState.IsConnected():
				// NM reports state 100 (Activated) for both STA and AP-mode
				// connections on the same WiFi device. Always query NM to
				// distinguish — never trust the local apMgr.active bool,
				// which can desync when WaitForActivation misreads a
				// transient NM state.
				info, err := m.nm.ActiveConnectionInfo(evt.Device)
				if err != nil {
					log.Printf("WARN: network: WiFi state=connected but cannot classify: %v", err)
					continue
				}
				if info == nil || info.IsHotspot() {
					continue
				}
				m.state.handleWiFiConnected()
			case evt.OldState.IsConnected() && !evt.NewState.IsConnected():
				hasSaved, _ := m.savedWifiInfo()
				m.state.handleWiFiDisconnected(hasSaved)
			case evt.NewState == nmclient.NMDeviceStateFailed:
				// NM failed to reconnect. Only match Failed(120), NOT
				// Disconnected(30) — NM fires both per attempt and matching
				// both would double-count failures.
				//
				// NM state sequence for auth failure:
				//   Disconnected→Prepare→Config→NeedAuth→Failed→Disconnected
				cur := m.state.Current()
				if cur != StateReconnecting && cur != StateDisconnected {
					continue
				}
				log.Printf("INFO: network: WiFi reconnect failed (reason=%d)", evt.Reason)
				switch evt.Reason {
				case nmclient.NMDeviceStateReasonNoSecrets,
					nmclient.NMDeviceStateReasonSupplicantConfigFailed,
					nmclient.NMDeviceStateReasonSupplicantFailed,
					nmclient.NMDeviceStateReasonSupplicantTimeout:
					// Deterministic: credentials are wrong, retrying is pointless.
					// - NoSecrets(7): NM has no new secrets (headless, wrong PSK stored)
					// - SupplicantConfigFailed(9): supplicant rejected config
					// - SupplicantFailed(10): WPA handshake rejected
					// - SupplicantTimeout(11): 4-way handshake timed out (wrong PSK)
					//
					// Note: SupplicantDisconnect(8) is deliberately NOT here — it
					// is ambiguous (wrong-password handshake AND transient signal loss).
					m.state.handleAuthFailure()
				default:
					// Transient (DHCP failure, signal loss, etc.): backoff and retry.
					m.state.handleReconnectResult(false)
				}
			}
		}
	}
}

// handleEthStateChanges processes Ethernet device state transitions with debounce.
// Uses a generation counter to prevent a stale AfterFunc goroutine from
// executing handleEthernetUp after Ethernet has already disconnected.
func (m *Manager) handleEthStateChanges(ctx context.Context, ch <-chan nmclient.DeviceStateChange) {
	var debounceTimer *time.Timer
	var ethGen atomic.Int64
	for {
		select {
		case <-ctx.Done():
			ethGen.Add(1)
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if evt.NewState.IsConnected() {
				// Ethernet up — debounce 5s before promoting
				gen := ethGen.Add(1)
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(ethDebounceDuration, func() {
					if ethGen.Load() == gen {
						m.state.handleEthernetUp()
					}
				})
			} else if evt.OldState.IsConnected() && !evt.NewState.IsConnected() {
				// Ethernet down — invalidate any pending debounce goroutine
				ethGen.Add(1)
				if debounceTimer != nil {
					debounceTimer.Stop()
					debounceTimer = nil
				}
				hasSaved, _ := m.savedWifiInfo()
				m.state.handleEthernetDown(hasSaved)
			}
		}
	}
}

// loadAPSuppression reads the AP suppression flag from disk.
func (m *Manager) loadAPSuppression() {
	_, err := os.Stat("/etc/piccolo/ap-mode-disabled")
	m.mu.Lock()
	m.apSuppressed = err == nil
	m.mu.Unlock()
}

// saveAPSuppression persists the AP suppression flag.
func (m *Manager) saveAPSuppression(suppress bool) error {
	path := "/etc/piccolo/ap-mode-disabled"
	if suppress {
		return os.WriteFile(path, []byte("1\n"), 0o644)
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Sentinel errors
var (
	errTransitionInProgress = &networkError{"transition already in progress"}
	errNoWifiDevice         = &networkError{"no WiFi device available"}
	errConnectTimeout       = &networkError{"connection timed out"}
)

type networkError struct{ msg string }

func (e *networkError) Error() string { return e.msg }
