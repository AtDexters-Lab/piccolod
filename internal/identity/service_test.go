package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		cfg: Config{
			Enabled:        true,
			DeviceID:       "dev-123",
			Hostname:       "slug.test.local",
			NexusEndpoints: localEndpoints,
		},
		client: namekclient.New(srv.URL, &mockTPM{}, namekclient.WithDeviceID("dev-123")),
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nonce":
			json.NewEncoder(w).Encode(map[string]string{"nonce": "test-nonce"})
		case "/api/v1/devices/me":
			if status != 0 && status != 200 {
				w.WriteHeader(status)
				w.Write([]byte("error"))
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
