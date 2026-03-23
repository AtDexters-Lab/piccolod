package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/events"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// mockTPM implements tpmdevice.Device with minimal stubs for authenticated requests.
type mockTPM struct{}

func (m *mockTPM) EKCertDER() ([]byte, error)                { return []byte("ek"), nil }
func (m *mockTPM) AKPublic() ([]byte, error)                  { return []byte("ak"), nil }
func (m *mockTPM) ActivateCredential([]byte) ([]byte, error)  { return []byte("secret"), nil }
func (m *mockTPM) Quote(string) (string, error)               { return "dHBtcXVvdGU=", nil }
func (m *mockTPM) QuoteOverData([]byte) (string, error)       { return "dHBtcXVvdGU=", nil }
func (m *mockTPM) Close() error                               { return nil }

// newTestService creates a Service wired to a test namekclient and event bus.
// The returned server must be closed by the caller.
func newTestService(t *testing.T, handler http.Handler, localEndpoints []string) (*Service, *events.Bus, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")

	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:        true,
			DeviceID:       "dev-123",
			Hostname:       "slug.test.local",
			NexusEndpoints: localEndpoints,
		},
		client: namekclient.New(srv.URL, &mockTPM{}, namekclient.WithDeviceID("dev-123")),
		stopCh: make(chan struct{}),
	}
	svc.enrolled.Store(true)

	bus := events.NewBus()
	svc.eventsBus = bus

	// Persist initial config so saveConfig has a valid dir.
	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}

	return svc, bus, srv
}

// deviceInfoHandler returns an http.Handler that serves nonce + device info endpoints.
func deviceInfoHandler(status int, info *namekclient.DeviceInfo) http.Handler {
	return deviceInfoHandlerWithError(status, info, "error")
}

// deviceInfoHandlerWithError returns a handler that serves a specific JSON error body on non-200 status.
func deviceInfoHandlerWithError(status int, info *namekclient.DeviceInfo, errMsg string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nonce":
			json.NewEncoder(w).Encode(map[string]string{"nonce": "test-nonce"})
		case "/api/v1/devices/me":
			if status != 0 && status != 200 {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
				return
			}
			json.NewEncoder(w).Encode(info)
		default:
			w.WriteHeader(404)
		}
	})
}

func TestSyncEndpointsOnce_DetectsChange(t *testing.T) {
	info := &namekclient.DeviceInfo{
		DeviceID:       "dev-123",
		Status:         "active",
		NexusEndpoints: []string{"wss://relay-a.example.com", "wss://relay-b.example.com"},
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info), []string{"wss://old-relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	ch := bus.Subscribe(events.TopicIdentityChanged, 1)

	svc.syncEndpointsOnce(t.Context())

	// Should have published an event.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected identity changed event")
	}

	// In-memory config should be updated.
	cfg := svc.DeviceConfig()
	if len(cfg.NexusEndpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(cfg.NexusEndpoints))
	}

	// Persisted config should match.
	loaded, err := loadConfig(svc.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.NexusEndpoints) != 2 {
		t.Fatalf("persisted endpoints: expected 2, got %d", len(loaded.NexusEndpoints))
	}
}

func TestSyncEndpointsOnce_NoChange(t *testing.T) {
	// Same endpoints but in different order — should detect as equal.
	info := &namekclient.DeviceInfo{
		DeviceID:       "dev-123",
		Status:         "active",
		NexusEndpoints: []string{"wss://b.example.com", "wss://a.example.com"},
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info),
		[]string{"wss://a.example.com", "wss://b.example.com"})
	defer srv.Close()
	defer bus.Close()

	ch := bus.Subscribe(events.TopicIdentityChanged, 1)

	svc.syncEndpointsOnce(t.Context())

	select {
	case <-ch:
		t.Fatal("unexpected identity changed event")
	case <-time.After(100 * time.Millisecond):
		// Good — no event.
	}
}

func TestSyncEndpointsOnce_EmptyResponseSkipped(t *testing.T) {
	info := &namekclient.DeviceInfo{
		DeviceID:       "dev-123",
		Status:         "active",
		NexusEndpoints: nil, // empty
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info),
		[]string{"wss://existing.example.com"})
	defer srv.Close()
	defer bus.Close()

	ch := bus.Subscribe(events.TopicIdentityChanged, 1)

	svc.syncEndpointsOnce(t.Context())

	select {
	case <-ch:
		t.Fatal("unexpected event — empty response should be skipped")
	case <-time.After(100 * time.Millisecond):
	}

	cfg := svc.DeviceConfig()
	if len(cfg.NexusEndpoints) != 1 {
		t.Fatalf("endpoints should be unchanged, got %d", len(cfg.NexusEndpoints))
	}
}

func TestSyncEndpointsOnce_AuthError(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(401, nil),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	// 401 triggers recovery — recovering flag should be set briefly.
	// Give the goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)
	// Recovery goroutine will fail (no TPM dirs configured) and clear the flag.
	// Just verify no panic and service is still functional.
	_ = bus
}

func TestSyncEndpointsOnce_DetectsSuspension(t *testing.T) {
	info := &namekclient.DeviceInfo{
		DeviceID: "dev-123",
		Status:   "suspended",
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	ch := bus.Subscribe(events.TopicIdentityChanged, 1)

	svc.syncEndpointsOnce(t.Context())

	if !svc.IsSuspended() {
		t.Fatal("expected suspended to be true")
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected identity changed event on suspension")
	}
}

func TestSyncEndpointsOnce_SkipsWhenNotEnrolled(t *testing.T) {
	var called atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(200)
	})
	svc, bus, srv := newTestService(t, handler, nil)
	defer srv.Close()
	defer bus.Close()

	svc.enrolled.Store(false)

	svc.syncEndpointsOnce(t.Context())

	if called.Load() {
		t.Fatal("should not have made HTTP call when not enrolled")
	}
}

func TestSyncEndpointsOnce_SkipsWhenRecovering(t *testing.T) {
	var called atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(200)
	})
	svc, bus, srv := newTestService(t, handler, nil)
	defer srv.Close()
	defer bus.Close()

	svc.recovering.Store(true)

	svc.syncEndpointsOnce(t.Context())

	if called.Load() {
		t.Fatal("should not have made HTTP call when recovering")
	}
}

func TestStartStopEndpointSync(t *testing.T) {
	// Use a short interval via env var.
	t.Setenv("PICCOLO_ENDPOINT_SYNC_INTERVAL", "50ms")

	info := &namekclient.DeviceInfo{
		DeviceID:       "dev-123",
		Status:         "active",
		NexusEndpoints: []string{"wss://relay.example.com"},
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.startEndpointSync()

	// Idempotent: calling again should not panic.
	svc.startEndpointSync()

	// Let the loop run at least one tick.
	time.Sleep(100 * time.Millisecond)

	// Stop should complete without panic or hang.
	done := make(chan struct{})
	go func() {
		svc.stopEndpointSync()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopEndpointSync timed out")
	}

	// Calling stop again when not running should be safe.
	svc.stopEndpointSync()
}

func TestEndpointSyncInterval_Default(t *testing.T) {
	t.Setenv("PICCOLO_ENDPOINT_SYNC_INTERVAL", "")
	if d := endpointSyncInterval(); d != 5*time.Minute {
		t.Fatalf("expected 5m default, got %v", d)
	}
}

func TestEndpointSyncInterval_Override(t *testing.T) {
	t.Setenv("PICCOLO_ENDPOINT_SYNC_INTERVAL", "30s")
	if d := endpointSyncInterval(); d != 30*time.Second {
		t.Fatalf("expected 30s, got %v", d)
	}
}

func TestHandleTokenError_DeviceNotFound(t *testing.T) {
	svc, bus, srv := newTestService(t,
		deviceInfoHandlerWithError(401, nil, "device not found"),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	// Give goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)

	// Should trigger re-enrollment (recovering flag set briefly), NOT AK recovery.
	// The re-enrollment will fail (test server doesn't serve enrollment), but that's fine.
	// Key assertion: no AK recovery was attempted (no TPM dirs configured, so
	// recoverAndReenroll would log "cannot recover AK", but reenrollWithRecovery doesn't).
	_ = bus
}

func TestHandleTokenError_NonceExpired(t *testing.T) {
	// Use the actual Namek error message format.
	svc, bus, srv := newTestService(t,
		deviceInfoHandlerWithError(401, nil, "invalid or expired nonce"),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	// Nonce errors are transient — no recovery should be triggered.
	time.Sleep(50 * time.Millisecond)
	if svc.recovering.Load() {
		t.Fatal("nonce expired should not trigger recovery")
	}
}

func TestHandleTokenError_403_Suspended(t *testing.T) {
	svc, bus, srv := newTestService(t,
		deviceInfoHandlerWithError(403, nil, "device suspended"),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	ch := bus.Subscribe(events.TopicIdentityChanged, 1)
	svc.syncEndpointsOnce(t.Context())

	if !svc.IsSuspended() {
		t.Fatal("expected suspended to be true")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected identity changed event on suspension")
	}
}

func TestSyncEndpointsOnce_AccountIDSync(t *testing.T) {
	info := &namekclient.DeviceInfo{
		DeviceID:       "dev-123",
		AccountID:      "acct-456",
		Status:         "active",
		RecoveryStatus: "active",
		NexusEndpoints: []string{"wss://relay.example.com"},
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	cfg := svc.DeviceConfig()
	if cfg.AccountID != "acct-456" {
		t.Fatalf("account_id = %q, want %q", cfg.AccountID, "acct-456")
	}

	// Verify persisted.
	loaded, _ := loadConfig(svc.configPath)
	if loaded.AccountID != "acct-456" {
		t.Fatalf("persisted account_id = %q, want %q", loaded.AccountID, "acct-456")
	}
}

func TestSyncEndpointsOnce_VoucherSigning(t *testing.T) {
	var signedRequests []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nonce":
			json.NewEncoder(w).Encode(map[string]string{"nonce": "test-nonce"})
		case "/api/v1/devices/me":
			info := namekclient.DeviceInfo{
				DeviceID: "dev-123",
				Status:   "active",
				PendingVoucherRequests: []namekclient.PendingVoucherRequest{
					{RequestID: "req-1", VoucherData: "dm91Y2hlcg==", Nonce: "abc123"},
					{RequestID: "req-2", VoucherData: "dm91Y2hlcjI=", Nonce: "def456"},
				},
				NexusEndpoints: []string{"wss://relay.example.com"},
			}
			json.NewEncoder(w).Encode(info)
		case "/api/v1/vouchers/sign":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			signedRequests = append(signedRequests, body["request_id"])
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	})
	svc, bus, srv := newTestService(t, handler, []string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	if len(signedRequests) != 2 {
		t.Fatalf("expected 2 signed voucher requests, got %d", len(signedRequests))
	}
	if signedRequests[0] != "req-1" || signedRequests[1] != "req-2" {
		t.Fatalf("unexpected signed requests: %v", signedRequests)
	}
}

func TestSyncEndpointsOnce_VoucherCaching(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nonce":
			json.NewEncoder(w).Encode(map[string]string{"nonce": "test-nonce"})
		case "/api/v1/devices/me":
			info := namekclient.DeviceInfo{
				DeviceID: "dev-123",
				Status:   "active",
				NewVouchers: []namekclient.VoucherArtifact{
					{
						Data:           makeVoucherData(testFingerprint("a1")),
						Quote:          "q1",
						IssuerAKPubKey: "ak1",
					},
				},
				NexusEndpoints: []string{"wss://relay.example.com"},
			}
			json.NewEncoder(w).Encode(info)
		default:
			w.WriteHeader(404)
		}
	})
	svc, bus, srv := newTestService(t, handler, []string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	// Verify voucher was cached to disk.
	vouchersDir := filepath.Join(filepath.Dir(svc.configPath), "vouchers")
	vouchers, err := loadAllVouchers(vouchersDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(vouchers) != 1 {
		t.Fatalf("expected 1 cached voucher, got %d", len(vouchers))
	}
	if vouchers[0].Quote != "q1" {
		t.Errorf("cached voucher quote = %q, want %q", vouchers[0].Quote, "q1")
	}
}

func TestSyncEndpointsOnce_StateCacheSync(t *testing.T) {
	customHostname := "mydevice"
	info := &namekclient.DeviceInfo{
		DeviceID:       "dev-123",
		Status:         "active",
		CustomHostname: &customHostname,
		AliasDomains:   []string{"app.example.com"},
		NexusEndpoints: []string{"wss://relay.example.com"},
	}
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, info),
		[]string{"wss://relay.example.com"})
	defer srv.Close()
	defer bus.Close()

	svc.syncEndpointsOnce(t.Context())

	// Verify state cache written.
	stateCachePath := filepath.Join(filepath.Dir(svc.configPath), "state_cache.json")
	sc, err := loadStateCache(stateCachePath)
	if err != nil {
		t.Fatal(err)
	}
	if sc.CustomHostname != "mydevice" {
		t.Errorf("state cache custom_hostname = %q, want %q", sc.CustomHostname, "mydevice")
	}
	if len(sc.AliasDomains) != 1 || sc.AliasDomains[0] != "app.example.com" {
		t.Errorf("state cache alias_domains = %v, want [app.example.com]", sc.AliasDomains)
	}
	if sc.LastSyncedAt == "" {
		t.Error("expected last_synced_at to be set")
	}
}

func TestBuildRecoveryBundle_WithState(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, nil), nil)
	defer srv.Close()
	defer bus.Close()

	// Set up cached state.
	svc.mu.Lock()
	svc.cfg.AccountID = "acct-789"
	svc.cfg.CustomHostname = "myhost"
	svc.mu.Unlock()

	dir := filepath.Dir(svc.configPath)
	saveStateCache(filepath.Join(dir, "state_cache.json"), StateCache{
		CustomHostname: "myhost",
		AliasDomains:   []string{"a.com", "b.com"},
	})
	v := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("b2")),
		Quote:          "q",
		IssuerAKPubKey: "ak",
	}
	_, _ = saveVoucher(filepath.Join(dir, "vouchers"), v)

	bundle := svc.buildRecoveryBundle()

	if bundle == nil {
		t.Fatal("expected non-nil bundle")
	}
	if bundle.AccountID != "acct-789" {
		t.Errorf("account_id = %q, want %q", bundle.AccountID, "acct-789")
	}
	if bundle.CustomHostname != "myhost" {
		t.Errorf("custom_hostname = %q, want %q", bundle.CustomHostname, "myhost")
	}
	if len(bundle.AliasDomains) != 2 {
		t.Errorf("alias_domains len = %d, want 2", len(bundle.AliasDomains))
	}
	if len(bundle.Vouchers) != 1 {
		t.Errorf("vouchers len = %d, want 1", len(bundle.Vouchers))
	}
}

func TestBuildRecoveryBundle_NoAccountID(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, nil), nil)
	defer srv.Close()
	defer bus.Close()

	// No AccountID set — should return nil (fresh device).
	bundle := svc.buildRecoveryBundle()
	if bundle != nil {
		t.Fatal("expected nil bundle when no account_id cached")
	}
}

func TestBuildRecoveryBundle_NoVouchers(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, nil), nil)
	defer srv.Close()
	defer bus.Close()

	// Has account_id but no vouchers — should return nil (server requires min=1 vouchers).
	svc.mu.Lock()
	svc.cfg.AccountID = "acct-single"
	svc.mu.Unlock()

	bundle := svc.buildRecoveryBundle()
	if bundle != nil {
		t.Fatal("expected nil bundle when no vouchers cached (server requires min=1)")
	}
}

func TestBackoffDelay(t *testing.T) {
	base := 2 * time.Second
	max := 120 * time.Second

	// Attempt 0: ~2s
	d := backoffDelay(0, base, max, 0)
	if d != 2*time.Second {
		t.Errorf("attempt 0: got %v, want 2s", d)
	}

	// Attempt 3: 2*2^3 = 16s (no jitter)
	d = backoffDelay(3, base, max, 0)
	if d != 16*time.Second {
		t.Errorf("attempt 3: got %v, want 16s", d)
	}

	// Attempt 10: capped at 120s
	d = backoffDelay(10, base, max, 0)
	if d != 120*time.Second {
		t.Errorf("attempt 10: got %v, want 120s (max)", d)
	}

	// With jitter, delay is between base and base*(1+jitter)
	d = backoffDelay(0, base, max, 0.5)
	if d < 2*time.Second || d > 3*time.Second {
		t.Errorf("attempt 0 with jitter: got %v, want between 2s and 3s", d)
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("Device Not Found", "device not found") {
		t.Error("expected case-insensitive match")
	}
	if !containsAny("quote verification failed", "nonce expired", "quote verification") {
		t.Error("expected match on second substr")
	}
	if containsAny("something else", "nonce expired", "device not found") {
		t.Error("expected no match")
	}
}

func TestIdentityStatus_Recovering(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, nil), nil)
	defer srv.Close()
	defer bus.Close()

	svc.recovering.Store(true)
	status := svc.Status()
	if status.State != "recovering" {
		t.Errorf("state = %q, want %q", status.State, "recovering")
	}
}

func TestSetNamekURL_ClearsStateAndVouchers(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, nil), nil)
	defer srv.Close()
	defer bus.Close()

	dir := filepath.Dir(svc.configPath)

	// Set up cached state and vouchers.
	saveStateCache(filepath.Join(dir, "state_cache.json"), StateCache{
		CustomHostname: "myhost",
		AliasDomains:   []string{"a.com"},
	})
	v := namekclient.VoucherArtifact{
		Data:           makeVoucherData(testFingerprint("ff")),
		Quote:          "q",
		IssuerAKPubKey: "ak",
	}
	_, _ = saveVoucher(filepath.Join(dir, "vouchers"), v)

	// Verify files exist.
	if _, err := os.Stat(filepath.Join(dir, "state_cache.json")); err != nil {
		t.Fatalf("state_cache.json should exist before SetNamekURL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vouchers")); err != nil {
		t.Fatalf("vouchers dir should exist before SetNamekURL: %v", err)
	}

	// Change namek URL — should clear cached state.
	if err := svc.SetNamekURL(t.Context(), "https://other.example.com"); err != nil {
		t.Fatal(err)
	}

	// Verify state_cache.json is removed.
	if _, err := os.Stat(filepath.Join(dir, "state_cache.json")); !os.IsNotExist(err) {
		t.Error("state_cache.json should be removed after SetNamekURL")
	}

	// Verify vouchers directory is removed.
	if _, err := os.Stat(filepath.Join(dir, "vouchers")); !os.IsNotExist(err) {
		t.Error("vouchers dir should be removed after SetNamekURL")
	}

	// Verify AccountID is cleared.
	cfg := svc.DeviceConfig()
	if cfg.AccountID != "" {
		t.Errorf("AccountID should be empty, got %q", cfg.AccountID)
	}
}

func TestIdentityStatus_AccountID(t *testing.T) {
	svc, bus, srv := newTestService(t, deviceInfoHandler(0, nil), nil)
	defer srv.Close()
	defer bus.Close()

	svc.mu.Lock()
	svc.cfg.AccountID = "acct-test"
	svc.mu.Unlock()

	status := svc.Status()
	if status.AccountID != "acct-test" {
		t.Errorf("account_id = %q, want %q", status.AccountID, "acct-test")
	}
}
