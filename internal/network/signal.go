package network

import (
	"context"
	"log"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/network/nmclient"
)

const signalPollInterval = 30 * time.Second

// signalMonitor periodically polls WiFi RSSI when WiFi is the active uplink
// and publishes a signal-only event on tier transitions.
type signalMonitor struct {
	nm  nmclient.Client
	mgr *Manager

	mu             sync.Mutex
	lastSignalDBm  int
	lastSignalTier SignalTier
}

func newSignalMonitor(nm nmclient.Client, mgr *Manager) *signalMonitor {
	return &signalMonitor{nm: nm, mgr: mgr}
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
	if m.mgr == nil || m.mgr.supervisor == nil {
		return
	}
	state, _, ok := m.mgr.supervisor.CurrentNetworkState()
	if !ok || state.ActiveUplink != UplinkWiFi {
		// Clear cached tier when leaving WiFi so status readers do not see a
		// stale signal after the uplink changes.
		m.mu.Lock()
		if m.lastSignalDBm != 0 {
			m.lastSignalDBm = 0
			m.lastSignalTier = ""
		}
		m.mu.Unlock()
		return
	}
	dev := m.mgr.wifiDeviceForInterface(state.ActiveUplinkIface)
	if dev == nil {
		return
	}

	strength, err := m.nm.SignalStrength(dev.Path)
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
		m.mgr.events.Publish(events.Event{
			Topic: events.TopicWiFiSignalChanged,
			Payload: WiFiSignalChangedEvent{
				SignalDBM:  dbm,
				SignalTier: tier,
			},
		})
	}
}
