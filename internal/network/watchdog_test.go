package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"piccolod/internal/network/nmclient"
	"piccolod/internal/testutil"
)

func newTestWatchdog() (*watchdog, *nmclient.StubClient, *testutil.FakeRunner) {
	stub := nmclient.NewStubClient()
	fr := &testutil.FakeRunner{}
	w := newWatchdog(stub, fr, nil, func() ConnState { return StateEthernet })
	return w, stub, fr
}

func callCount(fr *testutil.FakeRunner, prefix string) int {
	n := 0
	for _, c := range fr.GetCalls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestWatchdog_SkipsDuringOnboarding(t *testing.T) {
	w, _, fr := newTestWatchdog()
	w.onboarding = true
	w.gateway = "192.168.1.1"
	w.iface = "eth0"

	w.tick(context.Background())

	if callCount(fr, "arping") > 0 {
		t.Fatal("watchdog should skip during onboarding")
	}
}

func TestWatchdog_SkipsDuringReconnecting(t *testing.T) {
	w, _, fr := newTestWatchdog()
	w.gateway = "192.168.1.1"
	w.iface = "eth0"
	w.stateFn = func() ConnState { return StateReconnecting }

	w.tick(context.Background())

	if callCount(fr, "arping") > 0 {
		t.Fatal("watchdog should skip during Reconnecting state")
	}
}

func TestWatchdog_SkipsDuringAPMode(t *testing.T) {
	w, _, fr := newTestWatchdog()
	w.gateway = "192.168.1.1"
	w.iface = "eth0"
	w.stateFn = func() ConnState { return StateAPMode }

	w.tick(context.Background())

	if callCount(fr, "arping") > 0 {
		t.Fatal("watchdog should skip during AP mode")
	}
}

func TestWatchdog_SuccessfulProbe_ResetsState(t *testing.T) {
	w, _, fr := newTestWatchdog()
	w.gateway = "192.168.1.1"
	w.iface = "eth0"
	w.failures = 2
	w.lastAction = "bounce"

	// ip returns a route, arping succeeds (default: no error)
	fr.Outputs = map[string]string{
		"ip": "default via 192.168.1.1 dev eth0\n",
	}

	w.tick(context.Background())

	if w.failures != 0 {
		t.Fatalf("failures = %d, want 0 after successful probe", w.failures)
	}
	if w.lastAction != "" {
		t.Fatalf("lastAction = %q, want empty after successful probe", w.lastAction)
	}
}

func TestWatchdog_FailuresAccumulate(t *testing.T) {
	w, _, fr := newTestWatchdog()
	w.gateway = "192.168.1.1"
	w.iface = "eth0"

	// Make arping fail
	fr.Errs = map[string]error{
		"arping": errors.New("timeout"),
	}
	fr.Outputs = map[string]string{
		"ip": "default via 192.168.1.1 dev eth0\n",
	}

	w.tick(context.Background())
	if w.failures != 1 {
		t.Fatalf("failures = %d, want 1", w.failures)
	}

	w.tick(context.Background())
	if w.failures != 2 {
		t.Fatalf("failures = %d, want 2", w.failures)
	}
}

func TestWatchdog_BounceRateLimit(t *testing.T) {
	w, _, _ := newTestWatchdog()

	// Fill up bounce budget
	now := time.Now()
	for i := 0; i < maxBounces; i++ {
		w.bounces = append(w.bounces, now.Add(-time.Duration(i)*time.Minute))
	}

	initialBounces := len(w.bounces)

	// Bounce should be skipped due to rate limit
	w.escalateBounce(context.Background())

	// Rate-limited: no new bounce should be recorded
	if len(w.bounces) != initialBounces {
		t.Fatalf("bounces = %d, want %d (rate limit should prevent new bounce)", len(w.bounces), initialBounces)
	}
}

func TestWatchdog_RebootRateLimit(t *testing.T) {
	w, _, _ := newTestWatchdog()
	w.reboots = []time.Time{time.Now()}
	w.gateway = "192.168.1.1"
	w.iface = "eth0"
	w.lastAction = "bounce"

	w.escalateReboot(context.Background())

	if len(w.reboots) != 1 {
		t.Fatalf("reboots count = %d, want 1 (rate limit should prevent reboot)", len(w.reboots))
	}
}

func TestWatchdog_PruneTimestamps(t *testing.T) {
	w, _, _ := newTestWatchdog()

	now := time.Now()
	ts := []time.Time{
		now.Add(-2 * time.Hour),    // expired
		now.Add(-30 * time.Minute), // valid
		now.Add(-5 * time.Minute),  // valid
	}

	w.pruneTimestamps(&ts, time.Hour)
	if len(ts) != 2 {
		t.Fatalf("after prune: len = %d, want 2", len(ts))
	}
}

func TestWatchdog_IsWifiInterface(t *testing.T) {
	w, _, _ := newTestWatchdog()

	tests := []struct {
		iface string
		want  bool
	}{
		{"wlan0", true},
		{"wlp2s0", true},
		{"eth0", false},
		{"enp0s3", false},
		{"eno1", false},
	}
	for _, tt := range tests {
		w.iface = tt.iface
		got := w.isWifiInterface()
		if got != tt.want {
			t.Errorf("isWifiInterface(%q) = %v, want %v", tt.iface, got, tt.want)
		}
	}
}

func TestExtractField(t *testing.T) {
	line := "default via 192.168.1.1 dev eth0 proto dhcp metric 100"

	tests := []struct {
		keyword string
		want    string
	}{
		{"via", "192.168.1.1"},
		{"dev", "eth0"},
		{"metric", "100"},
		{"missing", ""},
	}
	for _, tt := range tests {
		got := extractField(line, tt.keyword)
		if got != tt.want {
			t.Errorf("extractField(%q) = %q, want %q", tt.keyword, got, tt.want)
		}
	}
}
