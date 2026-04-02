package network

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
)

const signalPollInterval = 30 * time.Second

// signalMonitor periodically polls WiFi RSSI and updates the state machine.
type signalMonitor struct {
	nm         nmclient.Client
	sm         *stateMachine
	mu         sync.Mutex
	wifiDevice dbus.ObjectPath
}

func newSignalMonitor(nm nmclient.Client, sm *stateMachine) *signalMonitor {
	return &signalMonitor{nm: nm, sm: sm}
}

// setDevice updates the WiFi device to monitor (thread-safe).
func (m *signalMonitor) setDevice(path dbus.ObjectPath) {
	m.mu.Lock()
	m.wifiDevice = path
	m.mu.Unlock()
}

// run polls RSSI every 30 seconds and updates the state machine's signal info.
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

	// Only poll when WiFi is the active uplink
	state := m.sm.Current()
	if state != StateWiFiSTA {
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

	// Convert NM's 0–100 percentage to approximate dBm
	dbm := int(strength)*60/100 - 90
	m.sm.updateSignal(dbm)
}
