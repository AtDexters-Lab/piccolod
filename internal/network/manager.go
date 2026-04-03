package network

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
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
	sm := newStateMachine(nm, bus)
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
				m.healthTracker.Setf("network", health.LevelOK, fmt.Sprintf("connected (%s)", newState))
			case StateReconnecting, StateAPMode:
				m.healthTracker.Setf("network", health.LevelWarn, string(newState))
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
	var ssid, savedSSID, ipAddr, band string
	var freqMHz uint32
	var hasSaved bool

	if m.wifiDevice != nil && uplink == UplinkWiFi {
		info, err := m.nm.ActiveConnectionInfo(m.wifiDevice.Path)
		if err == nil && info != nil {
			ssid = info.ID
			ipAddr = info.IP4Address
		}
	}

	sm := m.state
	sm.mu.Lock()
	hasSaved = sm.hasSavedWifi
	savedSSID = sm.savedSSID
	sm.mu.Unlock()
	m.mu.RUnlock()

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

	if !forceRefresh && time.Since(m.scanCacheTime) < 15*time.Second && len(m.scanCache) > 0 {
		return m.scanCache, nil
	}

	m.mu.RLock()
	dev := m.wifiDevice
	m.mu.RUnlock()

	if dev == nil {
		return nil, nil
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

	// Snapshot for rollback (includes device path so RestoreConnection can activate)
	profiles, _ := m.nm.SavedWiFiConnections()
	var snapshot *nmclient.ConnectionSnapshot
	for _, p := range profiles {
		if p.SSID == ssid || len(profiles) == 1 {
			snapshot, _ = m.nm.SnapshotConnection(p.Path)
			if snapshot != nil {
				snapshot.Device = dev.Path
			}
			break
		}
	}

	// Delete other WiFi profiles (single-SSID policy)
	for _, p := range profiles {
		if p.SSID != ssid {
			_ = m.nm.DeleteConnection(p.Path)
		}
	}

	// Connect
	if err := m.nm.Connect(dev.Path, ssid, passphrase); err != nil {
		if snapshot != nil {
			if err := m.nm.RestoreConnection(snapshot); err != nil {
				log.Printf("ERROR: network: WiFi rollback failed: %v", err)
			}
		}
		return err
	}

	// Wait for activation (up to 30s) or preemption
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	select {
	case <-staCtx.Done():
		// Preempted by Ethernet — rollback
		if snapshot != nil {
			if err := m.nm.RestoreConnection(snapshot); err != nil {
				log.Printf("ERROR: network: WiFi rollback failed: %v", err)
			}
		}
		return context.Canceled
	case <-ctx.Done():
		// Caller cancelled
		if snapshot != nil {
			if err := m.nm.RestoreConnection(snapshot); err != nil {
				log.Printf("ERROR: network: WiFi rollback failed: %v", err)
			}
		}
		return ctx.Err()
	case <-timer.C:
		// Timeout — check if connected
		state, err := m.nm.DeviceState(dev.Path)
		if err != nil || !state.IsConnected() {
			if snapshot != nil {
				if err := m.nm.RestoreConnection(snapshot); err != nil {
				log.Printf("ERROR: network: WiFi rollback failed: %v", err)
			}
			}
			return errConnectTimeout
		}
	}

	// Update state machine
	m.state.handleWiFiConnected()

	// Update saved SSID tracking
	m.state.mu.Lock()
	m.state.hasSavedWifi = true
	m.state.savedSSID = ssid
	m.state.mu.Unlock()

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
	m.state.hasSavedWifi = false
	m.state.savedSSID = ""
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

// waitForAP polls apMgr.Active for up to 20s, respecting context cancellation.
func (m *Manager) waitForAP() bool {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 40; i++ {
		if m.apMgr.Active() {
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
		if suppressed {
			log.Printf("INFO: network: AP mode suppressed by user setting")
			return
		}
		if dev == nil {
			log.Printf("WARN: network: cannot activate AP — no WiFi device")
			return
		}
		go func() {
			if err := m.apMgr.Start(m.ctx, dev.Path, dev.HWAddress); err != nil {
				log.Printf("ERROR: network: AP activation failed: %v", err)
			}
		}()
	} else if m.apMgr.Active() {
		go func() {
			if dev != nil {
				m.apMgr.Stop(m.ctx, dev.Path)
			}
		}()
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
		// Connect() already sets hasSavedWifi + savedSSID and calls handleWiFiConnected
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
	profiles, err := m.nm.SavedWiFiConnections()
	if err == nil && len(profiles) > 0 {
		m.state.mu.Lock()
		m.state.hasSavedWifi = true
		m.state.savedSSID = profiles[0].SSID
		m.state.mu.Unlock()

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
				// NM reports state 100 for both STA and hotspot on the
				// same WiFi device. When our AP is active, check the
				// connection ID to filter hotspot activation events.
				//
				// Active() may block if Start() is in progress — this
				// is intentional: Start() holds apMgr.mu, so we only
				// evaluate after AP activation succeeds or fails.
				if m.apMgr.Active() {
					info, err := m.nm.ActiveConnectionInfo(evt.Device)
					if err != nil {
						log.Printf("WARN: network: WiFi connected during AP — cannot classify, assuming hotspot: %v", err)
						continue
					}
					if info == nil || strings.HasPrefix(info.ID, nmclient.HotspotIDPrefix) {
						continue
					}
				}
				m.state.handleWiFiConnected()
			case evt.OldState.IsConnected() && !evt.NewState.IsConnected():
				m.state.handleWiFiDisconnected()
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
				m.state.handleEthernetDown()
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
