package remote

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/remote/nexusclient"
)

type stubDialer struct {
	err error
}

func (s *stubDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if s.err != nil {
		return nil, s.err
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

type stubResolver struct {
	hosts  map[string][]string
	cnames map[string]string
}

func (s *stubResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if addresses, ok := s.hosts[host]; ok {
		return addresses, nil
	}
	return nil, errors.New("host not found")
}

func (s *stubResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	if cname, ok := s.cnames[host]; ok {
		return cname, nil
	}
	return "", errors.New("cname not found")
}

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newTestManagerWithDeps(t *testing.T, storage Storage, dir string, d dialer, r resolver, now func() time.Time) *Manager {
	t.Helper()
	m, err := newManagerWithDeps(storage, dir, d, r, now)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestRunPreflightSuccess(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	dial := &stubDialer{}
	res := &stubResolver{
		hosts: map[string][]string{
			"portal.example.com":     {"1.2.3.4"},
			"app.portal.example.com": {"1.2.3.4"},
		},
		cnames: map[string]string{
			"portal.example.com": "nexus.example.com.",
		},
	}

	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManagerWithDeps(t, storage, dir, dial, res, fixedNow(time.Unix(1, 0)))

	err = m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	})
	if err != nil {
		t.Fatalf("configure failed: %v", err)
	}
	waitForCertNotPending(t, m, "portal", 200*time.Millisecond)

	result, err := m.RunPreflight(nil)
	if err != nil {
		t.Fatalf("preflight failed: %v", err)
	}
	if len(result.Checks) < 3 {
		t.Fatalf("expected checks, got %v", result.Checks)
	}

	st := m.Status()
	if st.State != "active" && st.State != "warning" {
		t.Fatalf("unexpected state %s", st.State)
	}
	if st.PortalHostname != "portal.example.com" {
		t.Fatalf("unexpected portal host %s", st.PortalHostname)
	}
}

type fakeAdapter struct {
	mu      sync.Mutex
	config  nexusclient.Config
	startCh chan struct{}
	stopCh  chan struct{}
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{startCh: make(chan struct{}, 1), stopCh: make(chan struct{}, 1)}
}

func (f *fakeAdapter) Configure(cfg nexusclient.Config) error {
	f.mu.Lock()
	f.config = cfg
	f.mu.Unlock()
	return nil
}

func (f *fakeAdapter) getConfig() nexusclient.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config
}

func (f *fakeAdapter) Start(ctx context.Context) error {
	select {
	case f.startCh <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeAdapter) Stop(ctx context.Context) error {
	select {
	case f.stopCh <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeAdapter) awaitStop(timeout time.Duration) error {
	select {
	case <-f.stopCh:
		return nil
	case <-time.After(timeout):
		return errors.New("adapter stop timeout")
	}
}

func TestManager_NexusAdapterLifecycle(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(3, 0)))
	adapter := newFakeAdapter()
	m.SetNexusAdapter(adapter)

	if err := m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	waitForCertNotPending(t, m, "portal", 200*time.Millisecond)
	if cfg := adapter.getConfig(); cfg.PortalHostname != "portal.example.com" {
		t.Fatalf("expected PortalHostname to propagate, got %s", cfg.PortalHostname)
	}

	select {
	case <-adapter.startCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected adapter start")
	}

	if err := m.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if err := adapter.awaitStop(500 * time.Millisecond); err != nil {
		t.Fatalf("adapter stop: %v", err)
	}
}

func TestRunPreflightFailures(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	dial := &stubDialer{err: errors.New("dial failed")}
	res := &stubResolver{}
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManagerWithDeps(t, storage, dir, dial, res, fixedNow(time.Unix(2, 0)))

	// Configure with HTTP-01 (user-managed mode)
	_ = m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	})
	waitForCertNotPending(t, m, "portal", 200*time.Millisecond)

	result, err := m.RunPreflight(nil)
	if err != nil {
		t.Fatalf("preflight failed: %v", err)
	}

	foundFail := false
	for _, check := range result.Checks {
		if check.Status == "fail" {
			foundFail = true
		}
	}
	if !foundFail {
		t.Fatalf("expected failure check, got %+v", result.Checks)
	}
}

func waitForCertNotPending(t *testing.T, m *Manager, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range m.ListCertificates() {
			if c.ID == id && !strings.EqualFold(c.Status, "pending") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConfigureManaged_DNS01SeedsWildcardWithApex(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(4, 0)))

	// ConfigureManaged uses DNS-01 via orchestrator (managed mode)
	if err := m.ConfigureManaged(ManagedConfigureRequest{
		OrchestratorEndpoint: "https://orchestrator.example.com",
		DeviceToken:          "secret",
		PortalHostname:       "portal.example.com",
	}); err != nil {
		t.Fatalf("configure managed: %v", err)
	}

	// Check that managed mode is set
	st := m.Status()
	if !st.Managed {
		t.Fatalf("expected managed=true, got managed=%v", st.Managed)
	}
	if st.Solver != "dns-01" {
		t.Fatalf("expected solver=dns-01, got solver=%s", st.Solver)
	}

	var wildcard *Certificate
	for _, c := range m.ListCertificates() {
		if c.ID == "wildcard" {
			cc := c
			wildcard = &cc
			break
		}
	}
	if wildcard == nil {
		t.Fatalf("expected wildcard certificate entry")
	}
	if len(wildcard.Domains) != 2 || wildcard.Domains[0] != "*.portal.example.com" || wildcard.Domains[1] != "portal.example.com" {
		t.Fatalf("unexpected wildcard domains: %v", wildcard.Domains)
	}
}

func TestConfigure_HTTP01NoWildcard(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(5, 0)))

	// Configure with HTTP-01 (user-managed mode) - no wildcard support
	if err := m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// Check that managed mode is false
	st := m.Status()
	if st.Managed {
		t.Fatalf("expected managed=false, got managed=%v", st.Managed)
	}
	if st.Solver != "http-01" {
		t.Fatalf("expected solver=http-01, got solver=%s", st.Solver)
	}

	// Should NOT have wildcard certificate in user-managed mode
	for _, c := range m.ListCertificates() {
		if c.ID == "wildcard" {
			t.Fatalf("unexpected wildcard certificate in HTTP-01 mode")
		}
	}
}

func TestUpdateCertFailureSetsRetryAt(t *testing.T) {
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(10, 0)))
	now := m.now()
	m.cfgMu.Lock()
	cfg := m.currentConfig()
	cfg.Certificates = []Certificate{{
		ID:       "portal",
		Domains:  []string{"portal.example.com"},
		Status:   "ok",
		Attempts: 2,
	}}
	// save() releases cfgMu.Lock()
	if err := m.save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	m.updateCertFailure("portal", "boom")

	var portal Certificate
	for _, c := range m.ListCertificates() {
		if c.ID == "portal" {
			portal = c
		}
	}
	if portal.Status != "error" {
		t.Fatalf("expected status error, got %s", portal.Status)
	}
	if portal.RetryAt == nil || !portal.RetryAt.After(now) {
		t.Fatalf("expected retry_at after now, got %v", portal.RetryAt)
	}
}

func TestEnqueueIssuanceRespectsRetryAt(t *testing.T) {
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	now := time.Unix(1000, 0)
	retryAt := now.Add(1 * time.Hour)
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(now))

	// Seed a cert in error state with RetryAt in the future (e.g., rate_limited).
	m.cfgMu.Lock()
	cfg := m.currentConfig()
	cfg.PortalHostname = "portal.example.com"
	cfg.Certificates = []Certificate{{
		ID:           "host:app.portal.example.com",
		Domains:      []string{"app.portal.example.com"},
		Status:       "error",
		FailureClass: FailureClassRateLimited,
		FailureCode:  "cert_rate_limited",
		RetryAt:      &retryAt,
	}}
	if err := m.save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// enqueueIssuance (non-forced) should skip because RetryAt is in the future.
	m.enqueueIssuance("host:app.portal.example.com", []string{"app.portal.example.com"}, "app.portal.example.com")

	// Cert should still be "error" — not reset to "pending".
	for _, c := range m.ListCertificates() {
		if c.ID == "host:app.portal.example.com" {
			if c.Status != "error" {
				t.Fatalf("expected cert to stay in error state, got %q", c.Status)
			}
			if c.RetryAt == nil || !c.RetryAt.Equal(retryAt) {
				t.Fatalf("expected RetryAt preserved, got %v", c.RetryAt)
			}
			return
		}
	}
	t.Fatal("cert not found")
}

func TestRemoveAliasRemovesCertificateEntry(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(20, 0)))

	alias, err := m.AddAlias("portal", "foo.example.com")
	if err != nil {
		t.Fatalf("add alias: %v", err)
	}

	waitForCertEntry(t, m, "alias:foo.example.com", 200*time.Millisecond)

	if err := m.RemoveAlias(alias.ID); err != nil {
		t.Fatalf("remove alias: %v", err)
	}

	for _, c := range m.ListCertificates() {
		if c.ID == "alias:foo.example.com" {
			t.Fatalf("expected alias cert to be removed, still present")
		}
	}
}

func waitForCertEntry(t *testing.T, m *Manager, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range m.ListCertificates() {
			if c.ID == id {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}
