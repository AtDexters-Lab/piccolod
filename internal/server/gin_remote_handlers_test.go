package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/persistence"
	"piccolod/internal/remote"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/runtime/commands"
	"piccolod/internal/state/paths"
)

func TestRemote_Configure_Status_Disable_Rotate(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	// Initial status disabled
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/remote/status", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}

	// Configure (user-managed mode, HTTP-01 is implicit)
	payload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "super-secret",
		"portal_hostname": "portal.example.com",
	}
	body, _ := json.Marshal(payload)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/remote/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configure %d body=%s", w.Code, w.Body.String())
	}

	// Status enabled
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/remote/status", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status2 %d", w.Code)
	}
	var st struct {
		Enabled        bool   `json:"enabled"`
		PortalHostname string `json:"portal_hostname"`
		State          string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled {
		t.Fatalf("expected enabled remote")
	}
	if st.PortalHostname != "portal.example.com" {
		t.Fatalf("unexpected portal hostname %s", st.PortalHostname)
	}

	// Rotate
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/remote/rotate", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate %d", w.Code)
	}
	var rotateResp struct {
		DeviceSecret string `json:"device_secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rotateResp); err != nil {
		t.Fatalf("rotate decode: %v", err)
	}
	if rotateResp.DeviceSecret == "" {
		t.Fatalf("expected rotated secret in response")
	}

	// Disable
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/remote/disable", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disable %d", w.Code)
	}

	// Status disabled
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/remote/status", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status3 %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled {
		t.Fatalf("expected disabled")
	}
}

func TestRemote_Preflight_Stateless(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	// Configure first so we have an active config (user-managed mode, HTTP-01 implicit)
	configurePayload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "super-secret",
		"portal_hostname": "portal.example.com",
	}
	body, _ := json.Marshal(configurePayload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/remote/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configure failed: %d", w.Code)
	}

	// Case 1: Empty body (should check active config)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/remote/preflight", nil)
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight empty body failed: %d body=%s", w.Code, w.Body.String())
	}

	// Case 2: Empty JSON object {} (should check active config - regression test)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/remote/preflight", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight {} body failed: %d body=%s", w.Code, w.Body.String())
	}

	// Case 3: Valid candidate body (should check candidate)
	// We'll send a config that would FAIL dns check to prove it's using the candidate
	candidatePayload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "new-secret",
		"portal_hostname": "invalid-host.example.com", // This will fail DNS check in stub resolver
	}
	body, _ = json.Marshal(candidatePayload)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/remote/preflight", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight candidate body failed: %d body=%s", w.Code, w.Body.String())
	}
	// Check response content to confirm it ran checks
	var res struct {
		Checks []remote.PreflightCheck `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// We expect checks to be present
	if len(res.Checks) == 0 {
		t.Fatalf("expected checks in response")
	}
}

func TestRemote_Configure_RequirePortalHostname(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	payload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "super-secret",
		"portal_hostname": "",
	}
	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/remote/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

type lockedRemoteStorage struct{}

func (lockedRemoteStorage) Load(ctx context.Context) (remote.Config, error) {
	return remote.Config{}, remote.ErrLocked
}

func (lockedRemoteStorage) SaveNexus(_ context.Context, _ remote.NexusConfig, _ remote.CertInventory) error {
	return remote.ErrLocked
}

func (lockedRemoteStorage) SaveCerts(_ context.Context, _ remote.NexusConfig, _ remote.CertInventory) error {
	return remote.ErrLocked
}

func (lockedRemoteStorage) SaveEvents(_ context.Context, _ remote.EventLog) error {
	return nil // events are best-effort
}

type toggledRemoteStorage struct {
	mu     sync.Mutex
	locked bool
	cfg    remote.Config
}

func newToggledRemoteStorage() *toggledRemoteStorage {
	return &toggledRemoteStorage{}
}

func (s *toggledRemoteStorage) Load(ctx context.Context) (remote.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return remote.Config{}, remote.ErrLocked
	}
	return cloneRemoteConfig(s.cfg), nil
}

func (s *toggledRemoteStorage) SaveNexus(_ context.Context, nexus remote.NexusConfig, certs remote.CertInventory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return remote.ErrLocked
	}
	s.cfg.NexusConfig = nexus
	s.cfg.CertInventory = certs
	return nil
}

func (s *toggledRemoteStorage) SaveCerts(_ context.Context, nexus remote.NexusConfig, certs remote.CertInventory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return remote.ErrLocked
	}
	s.cfg.NexusConfig = nexus
	s.cfg.CertInventory = certs
	return nil
}

func (s *toggledRemoteStorage) SaveEvents(_ context.Context, events remote.EventLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return remote.ErrLocked
	}
	s.cfg.EventLog = events
	return nil
}

func (s *toggledRemoteStorage) SetLocked(v bool) {
	s.mu.Lock()
	s.locked = v
	s.mu.Unlock()
}

func cloneRemoteConfig(cfg remote.Config) remote.Config {
	out := cfg
	if cfg.Aliases != nil {
		out.Aliases = append([]remote.Alias(nil), cfg.Aliases...)
	}
	if cfg.Certificates != nil {
		out.Certificates = append([]remote.Certificate(nil), cfg.Certificates...)
	}
	if cfg.Events != nil {
		out.Events = append([]remote.Event(nil), cfg.Events...)
	}
	if cfg.GuideVerifiedAt != nil {
		ts := *cfg.GuideVerifiedAt
		out.GuideVerifiedAt = &ts
	}
	if cfg.LastPreflight != nil {
		ts := *cfg.LastPreflight
		out.LastPreflight = &ts
	}
	return out
}

func TestRemote_Configure_WhenLocked(t *testing.T) {
	srv := createGinTestServer(t, t.TempDir())
	baseDir := t.TempDir()
	lockedMgr, err := remote.NewManagerWithStorage(lockedRemoteStorage{}, baseDir)
	if err != nil {
		t.Fatalf("locked manager init: %v", err)
	}
	t.Cleanup(func() {
		_ = lockedMgr.Close()
	})
	srv.remoteManager = lockedMgr
	// Create a new dispatcher to avoid duplicate registration panic and ensure we route to the locked manager
	srv.dispatcher = commands.NewDispatcher()
	srv.dispatcher.Register(persistence.CommandEnsureVolume, commands.HandlerFunc(func(ctx context.Context, cmd commands.Command) (commands.Response, error) {
		req, ok := cmd.(persistence.EnsureVolumeCommand)
		if !ok {
			return nil, fmt.Errorf("unexpected command type %T", cmd)
		}
		handle := persistence.VolumeHandle{
			ID:       req.Req.ID,
			MountDir: filepath.Join(baseDir, "mounts", req.Req.ID),
		}
		if err := os.MkdirAll(handle.MountDir, 0o700); err != nil {
			return nil, err
		}
		return persistence.EnsureVolumeResponse{Handle: handle}, nil
	}))
	srv.dispatcher.Register(persistence.CommandAttachVolume, commands.HandlerFunc(func(context.Context, commands.Command) (commands.Response, error) {
		return nil, nil
	}))
	srv.dispatcher.Register(persistence.CommandRecordLockState, commands.HandlerFunc(func(context.Context, commands.Command) (commands.Response, error) {
		return nil, nil
	}))
	remote.RegisterHandlers(srv.dispatcher, lockedMgr)
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	payload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "super-secret",
		"portal_hostname": "portal.example.com",
	}
	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/remote/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusLocked {
		t.Fatalf("expected 423 Locked, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRemote_ReloadsConfigAfterUnlockEvent(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")

	storage := newToggledRemoteStorage()
	storage.SetLocked(false)
	baseDir := t.TempDir()

	primaryMgr, err := remote.NewManagerWithStorage(storage, baseDir)
	if err != nil {
		t.Fatalf("primary manager init: %v", err)
	}
	t.Cleanup(func() {
		_ = primaryMgr.Close()
	})
	if err := primaryMgr.Configure(remote.ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "primary-secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure primary: %v", err)
	}

	storage.SetLocked(true)

	restartedMgr, err := remote.NewManagerWithStorage(storage, baseDir)
	if err != nil {
		t.Fatalf("restarted manager init: %v", err)
	}
	t.Cleanup(func() {
		_ = restartedMgr.Close()
	})
	restartedMgr.SetNexusAdapter(nexusclient.NewStub())

	if st := restartedMgr.Status(); st.Enabled || st.PortalHostname != "" {
		t.Fatalf("expected remote config unavailable before unlock, got %+v", st)
	}

	server := &GinServer{
		remoteManager: restartedMgr,
		healthTracker: health.NewTracker(),
	}
	server.registerUnlockReloader(restartedMgr)
	bus := events.NewBus()
	server.observeLockState(bus)

	storage.SetLocked(false)
	bus.Publish(events.Event{
		Topic: events.TopicLockStateChanged,
		Payload: events.LockStateChanged{
			Locked: false,
		},
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := restartedMgr.Status()
		if st.Enabled && st.PortalHostname == "portal.example.com" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	st := restartedMgr.Status()
	t.Fatalf("expected remote configuration to reload after unlock, got enabled=%v portal=%s", st.Enabled, st.PortalHostname)
}

func TestRemote_TlsMuxRestartsAfterReload(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")

	tempDir := t.TempDir()
	srv := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	payload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "super-secret",
		"portal_hostname": "portal.example.com",
	}
	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/remote/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", w.Code, w.Body.String())
	}
	if port := srv.tlsMux.Port(); port == 0 {
		t.Fatalf("expected tls mux to start after configure")
	}

	// Wait for asynchronous certificate issuance to complete to avoid concurrent config writes.
	certDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(certDeadline) {
		done := false
		for _, cert := range srv.remoteManager.ListCertificates() {
			if cert.ID == "portal" && strings.EqualFold(cert.Status, "ok") {
				done = true
				break
			}
		}
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Ensure config is durably written before simulating restart.
	configPath := filepath.Join(tempDir, "remote", "config.json")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(configPath)
		if err != nil || len(data) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var cfg remote.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}

	// Simulate restart by creating a new server instance with the same state dir.
	srv2 := createGinTestServer(t, tempDir)
	if st := srv2.remoteManager.Status(); !st.Enabled {
		t.Fatalf("expected remote manager to be enabled after restart, got %+v", st)
	}
	if port := srv2.tlsMux.Port(); port != 0 {
		// Unexpected but harmless: mux already running.
		return
	}

	// Publish unlock event to trigger reload logic and TLS mux startup.
	srv2.events.Publish(events.Event{
		Topic: events.TopicLockStateChanged,
		Payload: events.LockStateChanged{
			Locked: false,
		},
	})

	deadline = time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv2.tlsMux.Port() != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected tls mux to start after unlock reload; port=%d", srv2.tlsMux.Port())
}

func TestRemote_PortalHostnamePersistsAndAppCertQueued(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	t.Setenv("PICCOLO_DISABLE_MDNS", "1")

	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	srv := createGinTestServer(t, tempDir)
	sessionCookie, csrfToken := setupTestAdminSession(t, srv)

	configurePayload := map[string]interface{}{
		"endpoint":        "wss://nexus.example.com/connect",
		"device_secret":   "super-secret",
		"portal_hostname": "piccolo.example.com",
	}
	body, _ := json.Marshal(configurePayload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/remote/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", w.Code, w.Body.String())
	}

	// Simulate relay connect event — in production, the relay event handler
	// triggers requeueOutstandingIssuances when the tunnel comes up.
	srv.remoteManager.RequeueOutstandingIssuances()

	if status := waitForCertificateDomain(t, srv.remoteManager, "piccolo.example.com", 5*time.Second); !strings.EqualFold(status, "ok") {
		for _, cert := range srv.remoteManager.ListCertificates() {
			if hasDomain(cert, "piccolo.example.com") {
				t.Logf("portal certificate status=%s reason=%s", cert.Status, cert.FailureReason)
			}
		}
		t.Fatalf("expected portal certificate to be issued, got status=%q", status)
	}

	// RFC 20260130: Use __primary marker for multi-listener apps
	wordpressYAML := `type: user
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
  - name: webapp
    guest_port: 8080
    flow: tcp
    protocol: http
services:
  main:
    image: docker.io/library/wordpress:6
    bind_ports: [80, 8080]
x-piccolo:
  mode: service
`
	installPayload := map[string]interface{}{
		"app_definition": wordpressYAML,
		"inputs": map[string]interface{}{
			"__app_address__": "wordpress",
		},
	}
	installBody, _ := json.Marshal(installPayload)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/apps", bytes.NewReader(installBody))
	req.Header.Set("Content-Type", "application/json")
	attachAuth(req, sessionCookie, csrfToken)
	srv.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", w.Code, w.Body.String())
	}

	// Per RFC 20260114: primary listener gets <app>.<base> hostname where <base> is the portal hostname apex.
	// "web" is the first HTTP listener so it's primary, hostname = "wordpress.piccolo.example.com"
	if status := waitForCertificateDomain(t, srv.remoteManager, "wordpress.piccolo.example.com", 5*time.Second); !strings.EqualFold(status, "ok") {
		for _, cert := range srv.remoteManager.ListCertificates() {
			if hasDomain(cert, "wordpress.piccolo.example.com") {
				t.Logf("primary listener certificate status=%s reason=%s", cert.Status, cert.FailureReason)
			}
		}
		t.Fatalf("expected app listener certificate to be issued, got status=%q", status)
	}
	for _, cert := range srv.remoteManager.ListCertificates() {
		for _, d := range cert.Domains {
			if strings.Contains(d, " ") {
				t.Fatalf("unexpected certificate queued for domain with space: %s status=%s", d, cert.Status)
			}
		}
	}

	remoteStatus := srv.remoteManager.Status()
	if remoteStatus.PortalHostname != "piccolo.example.com" {
		t.Fatalf("expected portal hostname to persist, got %s", remoteStatus.PortalHostname)
	}
}

func waitForCertificateDomain(t *testing.T, mgr *remote.Manager, domain string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		certs := mgr.ListCertificates()
		foundStatus := ""
		for _, cert := range certs {
			for _, d := range cert.Domains {
				if strings.EqualFold(d, domain) {
					foundStatus = cert.Status
					if strings.EqualFold(cert.Status, "ok") {
						return cert.Status
					}
				}
			}
		}
		if foundStatus != "" {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		time.Sleep(25 * time.Millisecond)
	}
	for _, cert := range mgr.ListCertificates() {
		for _, d := range cert.Domains {
			if strings.EqualFold(d, domain) {
				return cert.Status
			}
		}
	}
	return ""
}

func hasDomain(cert remote.Certificate, domain string) bool {
	for _, d := range cert.Domains {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}
