package network

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/events"
	"piccolod/internal/network/nmclient"
)

const signalPollInterval = 30 * time.Second

// signalMonitor periodically polls WiFi RSSI when WiFi is the active uplink
// and publishes a NetworkStateChangedEvent on tier transitions.
type signalMonitor struct {
	nm  nmclient.Client
	mgr *Manager

	mu             sync.Mutex
	wifiDevice     dbus.ObjectPath
	lastSignalDBm  int
	lastSignalTier SignalTier
}

func newSignalMonitor(nm nmclient.Client, mgr *Manager) *signalMonitor {
	return &signalMonitor{nm: nm, mgr: mgr}
}

func (m *signalMonitor) setDevice(path dbus.ObjectPath) {
	m.mu.Lock()
	m.wifiDevice = path
	m.mu.Unlock()
}

// snapshot returns the latest cached signal reading (thread-safe).
func (m *signalMonitor) snapshot() (int, SignalTier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSignalDBm, m.lastSignalTier
}

func (m *signalMonitor) run(ctx context.Context) {
	ticker := time.NewTicker(signalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll()
		}
	}
}

func (m *signalMonitor) poll() {
	m.mu.Lock()
	dev := m.wifiDevice
	m.mu.Unlock()
	if dev == "" {
		return
	}

	// Gate on WiFi being the active uplink — read from supervisor's snapshot.
	if m.mgr == nil || m.mgr.snapshot().ActiveUplink != UplinkWiFi {
		// Clear cached tier when leaving WiFi STA so stale dBm doesn't
		// linger on the legacy wire contract.
		m.mu.Lock()
		if m.lastSignalDBm != 0 {
			m.lastSignalDBm = 0
			m.lastSignalTier = ""
		}
		m.mu.Unlock()
		return
	}

	strength, err := m.nm.SignalStrength(dev)
	if err != nil {
		log.Printf("DEBUG: signal-monitor: poll failed: %v", err)
		return
	}
	if strength == 0 {
		return // no active AP
	}

	dbm := int(strength)*60/100 - 90
	tier := ClassifySignal(dbm)

	m.mu.Lock()
	oldTier := m.lastSignalTier
	m.lastSignalDBm = dbm
	m.lastSignalTier = tier
	m.mu.Unlock()

	if tier != oldTier && m.mgr.events != nil {
		state := deriveLegacyState(m.mgr.lastTick(), m.mgr.snapshot().APMode.Active)
		uplink := m.mgr.lastTick().ActiveUplink
		m.mgr.events.Publish(events.Event{
			Topic: events.TopicNetworkStateChanged,
			Payload: NetworkStateChangedEvent{
				State:        string(state),
				ActiveUplink: string(uplink),
				SignalDBm:    &dbm,
				SignalTier:   string(tier),
			},
		})
	}
}
