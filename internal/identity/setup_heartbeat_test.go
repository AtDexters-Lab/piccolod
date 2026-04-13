package identity

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// heartbeatRecorder is an httptest handler that captures heartbeat POSTs and
// stubs the nonce endpoint so SendHeartbeat can complete without a real namek.
type heartbeatRecorder struct {
	mu       sync.Mutex
	requests []namekclient.HeartbeatRequest
}

func (h *heartbeatRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nonce":
			_ = json.NewEncoder(w).Encode(map[string]string{"nonce": "test-nonce"})
		case "/api/v1/devices/me/heartbeat":
			var req namekclient.HeartbeatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.mu.Lock()
			h.requests = append(h.requests, req)
			h.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (h *heartbeatRecorder) snapshot() []namekclient.HeartbeatRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]namekclient.HeartbeatRequest, len(h.requests))
	copy(out, h.requests)
	return out
}

// shortenHeartbeatTimers swaps the package-level timing vars to test-friendly
// values and returns a restore func.
func shortenHeartbeatTimers(t *testing.T) {
	t.Helper()
	origInterval := setupHeartbeatInterval
	origDelay := setupHeartbeatInitialDelay
	origTimeout := setupHeartbeatTimeout
	setupHeartbeatInterval = 20 * time.Millisecond
	setupHeartbeatInitialDelay = 5 * time.Millisecond
	setupHeartbeatTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		setupHeartbeatInterval = origInterval
		setupHeartbeatInitialDelay = origDelay
		setupHeartbeatTimeout = origTimeout
	})
}

// newHeartbeatService is a minimal Service wired to the recorder; it bypasses
// newTestService so we can override the setup-complete callback per test.
func newHeartbeatService(t *testing.T, srvURL string) *Service {
	t.Helper()
	svc := &Service{
		configPath: t.TempDir() + "/identity.json",
		tpmDev:     &mockTPM{},
		cfg:        Config{Enabled: true, DeviceID: "dev-hb"},
		client:     namekclient.New(srvURL, &mockTPM{}, namekclient.WithDeviceID("dev-hb")),
		stopCh:     make(chan struct{}),
	}
	svc.enrolled.Store(true)
	if err := saveConfig(svc.configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestSetupHeartbeat_TerminatesOnCompletion(t *testing.T) {
	shortenHeartbeatTimers(t)
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)

	var ticks atomic.Int32
	svc.SetSetupCompleteCheck(func() bool {
		// Allow three non-terminal sends, then signal completion on tick 4.
		return ticks.Add(1) > 3
	})

	svc.startSetupHeartbeat()
	t.Cleanup(svc.stopSetupHeartbeat)

	// Wait for the loop's done channel — it should self-exit after sending
	// the terminal heartbeat. Generous bound for CI flakiness.
	svc.mu.RLock()
	done := svc.setupHBDone
	svc.mu.RUnlock()
	if done == nil {
		t.Fatal("setupHBDone was nil after start")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop did not self-terminate")
	}

	got := rec.snapshot()
	if len(got) == 0 {
		t.Fatal("expected at least one heartbeat")
	}
	final := got[len(got)-1]
	if !final.SetupComplete {
		t.Fatalf("expected terminal heartbeat to have SetupComplete=true, got %+v", final)
	}
	if len(final.LANIPs) != 0 {
		t.Errorf("terminal heartbeat should have empty LANIPs, got %v", final.LANIPs)
	}
	for i, hb := range got[:len(got)-1] {
		if hb.SetupComplete {
			t.Errorf("non-terminal heartbeat %d unexpectedly has SetupComplete=true", i)
		}
		if len(hb.LANIPs) == 0 {
			t.Errorf("non-terminal heartbeat %d has no LAN IPs", i)
		}
	}
}

func TestSetupHeartbeat_NotifySetupCompleteWakesLoop(t *testing.T) {
	shortenHeartbeatTimers(t)
	// Long interval so the wake-up is the only path that can trigger the
	// terminal send within the test budget.
	setupHeartbeatInterval = time.Hour
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)
	var complete atomic.Bool
	svc.SetSetupCompleteCheck(func() bool { return complete.Load() })

	svc.startSetupHeartbeat()
	t.Cleanup(svc.stopSetupHeartbeat)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.snapshot()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if len(rec.snapshot()) == 0 {
		t.Fatal("loop did not send the initial non-terminal heartbeat")
	}

	complete.Store(true)
	svc.NotifySetupComplete()

	svc.mu.RLock()
	done := svc.setupHBDone
	svc.mu.RUnlock()
	if done == nil {
		t.Fatal("setupHBDone was nil")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not self-terminate after NotifySetupComplete")
	}

	got := rec.snapshot()
	if !got[len(got)-1].SetupComplete {
		t.Errorf("expected last heartbeat to be terminal, got %+v", got[len(got)-1])
	}
}

func TestSetupHeartbeat_StopsOnContextCancel(t *testing.T) {
	if len(collectLANIPs()) == 0 {
		// sendSetupHeartbeat skips ticks with no LAN IPs, so the
		// "at least one heartbeat sent" assertion below would be
		// vacuous on a network-sandboxed runner.
		t.Skip("no LAN interfaces in this environment")
	}
	shortenHeartbeatTimers(t)
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)
	svc.SetSetupCompleteCheck(func() bool { return false })

	svc.startSetupHeartbeat()

	// Let at least one heartbeat go out, then cancel.
	time.Sleep(80 * time.Millisecond)
	svc.stopSetupHeartbeat()

	// stopSetupHeartbeat must NOT have sent a terminal heartbeat — shutdown
	// leaves the row to age out via TTL. Also assert that *some* heartbeat
	// went out so the no-terminal check isn't vacuous on a slow CI runner.
	got := rec.snapshot()
	if len(got) == 0 {
		t.Fatal("expected at least one non-terminal heartbeat before stop, got none — sleep was too short or loop never ran")
	}
	for _, hb := range got {
		if hb.SetupComplete {
			t.Errorf("shutdown unexpectedly sent setup_complete=true heartbeat")
		}
	}

	// Loop should be fully gone.
	svc.mu.RLock()
	if svc.setupHBCancel != nil || svc.setupHBDone != nil {
		t.Errorf("setupHB lifecycle fields not cleared after stop")
	}
	svc.mu.RUnlock()

	// Stop is idempotent.
	svc.stopSetupHeartbeat()
}

func TestSetupHeartbeat_StartIdempotent(t *testing.T) {
	shortenHeartbeatTimers(t)
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)
	svc.SetSetupCompleteCheck(func() bool { return false })

	svc.startSetupHeartbeat()
	svc.startSetupHeartbeat() // second call must no-op
	t.Cleanup(svc.stopSetupHeartbeat)

	svc.mu.RLock()
	cancel := svc.setupHBCancel
	svc.mu.RUnlock()
	if cancel == nil {
		t.Fatal("expected setupHBCancel after start")
	}
}

func TestMaybeStartSetupHeartbeat_NilCallback(t *testing.T) {
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)
	// No SetSetupCompleteCheck.

	svc.maybeStartSetupHeartbeat()

	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if svc.setupHBCancel != nil {
		t.Fatal("heartbeat unexpectedly started with nil callback")
	}
}

func TestMaybeStartSetupHeartbeat_AlreadyComplete(t *testing.T) {
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)
	svc.SetSetupCompleteCheck(func() bool { return true })

	svc.maybeStartSetupHeartbeat()

	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if svc.setupHBCancel != nil {
		t.Fatal("heartbeat started even though setup already complete")
	}
}

func TestSetupHeartbeat_StoppedServiceNoStart(t *testing.T) {
	rec := &heartbeatRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	svc := newHeartbeatService(t, srv.URL)
	svc.SetSetupCompleteCheck(func() bool { return false })
	svc.stopped.Store(true)

	svc.startSetupHeartbeat()

	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if svc.setupHBCancel != nil {
		t.Fatal("heartbeat started on a stopped service")
	}
}

func TestIsAcceptedPrivateIPv4(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// RFC1918
		{"10.0.0.5", true},
		{"172.16.0.1", true},
		{"172.31.255.254", true},
		{"192.168.1.1", true},
		// RFC6598 CGNAT
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		// Outside CGNAT
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		// Public
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		// Link-local
		{"169.254.1.1", false},
		// Loopback
		{"127.0.0.1", false},
		// Multicast
		{"224.0.0.1", false},
		// Unspecified
		{"0.0.0.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip).To4()
			if ip == nil {
				t.Fatalf("unparseable IPv4: %s", tc.ip)
			}
			if got := isAcceptedPrivateIPv4(ip); got != tc.want {
				t.Errorf("isAcceptedPrivateIPv4(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestTruncateHardwareModel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short ascii", "Raspberry Pi 4 Model B Rev 1.5", "Raspberry Pi 4 Model B Rev 1.5"},
		{"exact 128", strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{"129 ascii", strings.Repeat("a", 129), strings.Repeat("a", 128)},
		{"long ascii", strings.Repeat("Z", 500), strings.Repeat("Z", 128)},
		// 4-byte rune (😀 = U+1F600) right at byte 126: full rune fits at 126..129;
		// cap at 128 forces backtrack to 126 to avoid splitting it.
		{
			"multibyte at boundary",
			strings.Repeat("a", 126) + "😀" + "tail",
			strings.Repeat("a", 126),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateHardwareModel(tc.in)
			if got != tc.want {
				t.Errorf("truncateHardwareModel(len=%d) = %q (len=%d), want %q (len=%d)",
					len(tc.in), got, len(got), tc.want, len(tc.want))
			}
			if len(got) > maxHardwareModelLen {
				t.Errorf("result exceeds cap: len=%d", len(got))
			}
		})
	}
}

func TestIsVirtualInterfaceName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// virtual
		{"podman0", true},
		{"podman1", true},
		{"docker0", true},
		{"br-1a2b3c", true},
		{"veth8badf00d", true},
		{"virbr0", true},
		{"tun0", true},
		{"tap0", true},
		{"wg0", true},
		{"tailscale0", true},
		{"flannel.1", true},
		{"cali12345", true},
		// physical / WiFi
		{"eth0", false},
		{"enp3s0", false},
		{"wlan0", false},
		{"wlp4s0", false},
		{"end0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVirtualInterfaceName(tc.name); got != tc.want {
				t.Errorf("isVirtualInterfaceName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestCollectLANIPs_FiltersUnusable(t *testing.T) {
	// Smoke test against the real interface table. We can't assert specific
	// IPs (CI environments vary), but every returned IP must pass the filter
	// — that catches regressions where someone weakens the filter.
	got := collectLANIPs()
	if len(got) == 0 {
		t.Skip("no LAN interfaces in this environment")
	}
	for _, s := range got {
		ip := net.ParseIP(s).To4()
		if ip == nil {
			t.Errorf("collectLANIPs returned non-IPv4 %q", s)
			continue
		}
		if !isAcceptedPrivateIPv4(ip) {
			t.Errorf("collectLANIPs returned %q which fails private filter", s)
		}
	}
}
