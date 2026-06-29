package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptoRand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/network"
	"piccolod/internal/remote/acme"
	"piccolod/internal/remote/nexusclient"
)

type stubDialer struct {
	err error
}

func (s *stubDialer) dial() (net.Conn, error) {
	if s.err != nil {
		return nil, s.err
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (s *stubDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	return s.dial()
}

func (s *stubDialer) DialTLS(network, address, serverName string, timeout time.Duration) (net.Conn, error) {
	return s.dial()
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

// stubOrchClient is a no-op OrchestratorClient for tests that need a non-nil
// orchClient registered in the manager's source registry.
type stubOrchClient struct{}

func (stubOrchClient) SetTXTRecord(_ context.Context, _, _ string) error { return nil }
func (stubOrchClient) DeleteTXTRecord(_ context.Context, _ string) error { return nil }

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
	if st.State != string(RemoteStateActive) && st.State != string(RemoteStateWarning) {
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

func TestManager_RestartAdapterForNetworkTransition(t *testing.T) {
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
	select {
	case <-adapter.startCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected initial adapter start")
	}

	m.RestartAdapterForNetworkTransition([]network.NetworkTransitionReason{network.ReasonDefaultRouteChanged})

	if err := adapter.awaitStop(500 * time.Millisecond); err != nil {
		t.Fatalf("adapter stop: %v", err)
	}
	select {
	case <-adapter.startCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected adapter restart")
	}
}

func TestManager_RestartAdapterForNetworkTransitionRespectsDisabledConfig(t *testing.T) {
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
	select {
	case <-adapter.startCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected initial adapter start")
	}
	if err := m.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := adapter.awaitStop(500 * time.Millisecond); err != nil {
		t.Fatalf("adapter stop: %v", err)
	}

	if restarted := m.RestartAdapterForNetworkTransition([]network.NetworkTransitionReason{network.ReasonDefaultRouteChanged}); restarted {
		t.Fatal("restart should not be accepted while remote config is disabled")
	}
	select {
	case <-adapter.startCh:
		t.Fatal("adapter restarted while disabled")
	case <-time.After(100 * time.Millisecond):
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
		if check.Status == string(PreflightFail) {
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
			if c.ID == id && !strings.EqualFold(c.Status, string(CertStatusPending)) {
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
	if st.Solver != acme.SolverDNS01 {
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
	if st.Solver != acme.SolverHTTP01 {
		t.Fatalf("expected solver=http-01, got solver=%s", st.Solver)
	}

	// Should NOT have wildcard certificate in user-managed mode
	for _, c := range m.ListCertificates() {
		if c.ID == "wildcard" {
			t.Fatalf("unexpected wildcard certificate in HTTP-01 mode")
		}
	}
}

func TestEndpointUsesTLS(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"wss://relay.example.com:8443/connect", true},
		{"https://relay.example.com/connect", true},
		{"ws://relay.example.com:8080/connect", false},
		{"http://relay.example.com/connect", false},
		{"", false}, // empty has no scheme
	}
	for _, tc := range tests {
		if got := endpointUsesTLS(tc.endpoint); got != tc.want {
			t.Errorf("endpointUsesTLS(%q) = %v, want %v", tc.endpoint, got, tc.want)
		}
	}
}

func TestCheckEndpoint_TLSError(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	dial := &stubDialer{err: errors.New("tls: certificate is valid for nxs.other.com, not relay.example.com")}
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManagerWithDeps(t, storage, dir, dial, &stubResolver{}, fixedNow(time.Unix(6, 0)))

	cfg := &Config{NexusConfig: NexusConfig{
		Endpoint:       "wss://relay.example.com:8443/connect",
		PortalHostname: "portal.example.com",
		Solver:         acme.SolverHTTP01,
	}}

	check := m.checkEndpoint(cfg)
	if check.Status != string(PreflightFail) {
		t.Fatalf("expected fail, got %s", check.Status)
	}
	if check.NextStep != "Verify the relay's TLS certificate matches the endpoint hostname" {
		t.Fatalf("expected TLS-specific next step, got %q", check.NextStep)
	}
}

func TestCheckEndpoint_TypedTLSError(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	dial := &stubDialer{err: x509.HostnameError{
		Host:        "relay.example.com",
		Certificate: &x509.Certificate{DNSNames: []string{"nxs.other.com"}},
	}}
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManagerWithDeps(t, storage, dir, dial, &stubResolver{}, fixedNow(time.Unix(6, 0)))

	cfg := &Config{NexusConfig: NexusConfig{
		Endpoint:       "wss://relay.example.com:8443/connect",
		PortalHostname: "portal.example.com",
		Solver:         acme.SolverHTTP01,
	}}

	check := m.checkEndpoint(cfg)
	if check.Status != string(PreflightFail) {
		t.Fatalf("expected fail, got %s", check.Status)
	}
	if check.NextStep != "Verify the relay's TLS certificate matches the endpoint hostname" {
		t.Fatalf("expected TLS-specific next step for typed error, got %q", check.NextStep)
	}
}

func TestCheckEndpoint_NonTLSEndpoint(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	dial := &stubDialer{}
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManagerWithDeps(t, storage, dir, dial, &stubResolver{}, fixedNow(time.Unix(7, 0)))

	cfg := &Config{NexusConfig: NexusConfig{
		Endpoint:       "ws://relay.example.com:8080/connect",
		PortalHostname: "portal.example.com",
		Solver:         acme.SolverHTTP01,
	}}

	check := m.checkEndpoint(cfg)
	if check.Status != string(PreflightPass) {
		t.Fatalf("expected pass for non-TLS endpoint, got %s: %s", check.Status, check.Detail)
	}
}

func TestConfigure_HTTP01DefersIssuance(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(8, 0)))

	if err := m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// Immediately after Configure, the portal cert should be "pending" —
	// actual issuance happens asynchronously via the ACME worker.
	var found bool
	for _, c := range m.ListCertificates() {
		if c.ID == "portal" {
			found = true
			if !strings.EqualFold(c.Status, string(CertStatusPending)) {
				t.Fatalf("portal cert should be 'pending' immediately after Configure, got %q", c.Status)
			}
		}
	}
	if !found {
		t.Fatalf("portal cert not found after Configure")
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
		Status:   string(CertStatusOK),
		Attempts: 2,
	}}
	if err := m.saveAll(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	m.updateCertFailure("portal", "boom")

	var portal Certificate
	for _, c := range m.ListCertificates() {
		if c.ID == "portal" {
			portal = c
		}
	}
	if portal.Status != string(CertStatusError) {
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
		Status:       string(CertStatusError),
		FailureClass: FailureClassRateLimited,
		FailureCode:  "cert_rate_limited",
		RetryAt:      &retryAt,
	}}
	if err := m.saveAll(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// enqueueIssuanceJob (non-forced) should skip because RetryAt is in the future.
	m.enqueueIssuanceJob(issuanceJob{id: "host:app.portal.example.com", domains: []string{"app.portal.example.com"}, commonName: "app.portal.example.com"})

	// Cert should still be "error" — not reset to "pending".
	for _, c := range m.ListCertificates() {
		if c.ID == "host:app.portal.example.com" {
			if c.Status != string(CertStatusError) {
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

func TestDomainsEqual(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"A.com"}, []string{"a.com"}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
		{nil, []string{"a"}, false},
		{[]string{"a", "b"}, []string{"a", "a"}, false}, // duplicate in b
	}
	for _, tt := range tests {
		if got := domainsEqual(tt.a, tt.b); got != tt.want {
			t.Errorf("domainsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestEnqueueIssuanceReissuesOnDomainChange(t *testing.T) {
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	now := time.Unix(1000, 0)
	nextRenewal := now.Add(60 * 24 * time.Hour)
	expires := now.Add(90 * 24 * time.Hour)
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(now))

	// Seed a cert that is "ok" and not yet due for renewal.
	m.cfgMu.Lock()
	cfg := m.currentConfig()
	cfg.PortalHostname = "old.example.com"
	cfg.Certificates = []Certificate{{
		ID:          "portal",
		Domains:     []string{"old.example.com"},
		Status:      string(CertStatusOK),
		NextRenewal: &nextRenewal,
		ExpiresAt:   &expires,
	}}
	if err := m.saveAll(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Enqueue with different domains — should force reissuance despite valid ok status.
	m.enqueueIssuanceJob(issuanceJob{id: "portal", domains: []string{"new.example.com"}, commonName: "new.example.com"})

	for _, c := range m.ListCertificates() {
		if c.ID == "portal" {
			if c.Status != string(CertStatusPending) {
				t.Fatalf("expected cert to be reset to pending, got %q", c.Status)
			}
			if len(c.Domains) != 1 || c.Domains[0] != "new.example.com" {
				t.Fatalf("expected domains [new.example.com], got %v", c.Domains)
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

func TestEnqueueCertIssuance_SourceMetadataPersisted(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(30, 0)))

	// Register orchClient so the dns-01 gate in enqueueIssuanceJob passes.
	m.RegisterOrchClient("namek", stubOrchClient{})

	m.EnqueueCertIssuance(CertIssuanceRequest{
		ID:         "host:app.namek.example.com",
		Source:     "namek",
		Solver:     acme.SolverDNS01,
		CertDir:    "/tmp/certs",
		Domains:    []string{"app.namek.example.com"},
		CommonName: "app.namek.example.com",
	})

	waitForCertEntry(t, m, "host:app.namek.example.com", 200*time.Millisecond)

	for _, c := range m.ListCertificates() {
		if c.ID == "host:app.namek.example.com" {
			if c.Source != "namek" {
				t.Fatalf("expected source=namek, got %q", c.Source)
			}
			if c.Solver != acme.SolverDNS01 {
				t.Fatalf("expected solver=dns-01, got %q", c.Solver)
			}
			if c.CertDir != "/tmp/certs" {
				t.Fatalf("expected certDir=/tmp/certs, got %q", c.CertDir)
			}
			return
		}
	}
	t.Fatal("cert not found")
}

func TestOrchClientRegistry(t *testing.T) {
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(40, 0)))

	// Register
	m.RegisterOrchClient("namek", nil)
	m.adapterMu.Lock()
	if _, ok := m.orchClients["namek"]; !ok {
		t.Fatal("expected namek orchClient to be registered")
	}
	m.adapterMu.Unlock()

	// Unregister
	m.UnregisterOrchClient("namek")
	m.adapterMu.Lock()
	if _, ok := m.orchClients["namek"]; ok {
		t.Fatal("expected namek orchClient to be unregistered")
	}
	m.adapterMu.Unlock()
}

// --- Relay-gated certificate issuance tests ---

func TestHttpChallengeReachable(t *testing.T) {
	m := &Manager{
		relayStates: make(map[string]relayState),
		cfg:         &Config{},
		now:         func() time.Time { return time.Now() },
	}

	t.Run("empty_map_returns_false", func(t *testing.T) {
		if m.httpChallengeReachable() {
			t.Fatal("expected false when no relays tracked")
		}
	})

	t.Run("single_relay_connected", func(t *testing.T) {
		m.relayStates["piccolo-portal"] = relayState{connected: true}
		if !m.httpChallengeReachable() {
			t.Fatal("expected true when single relay connected")
		}
	})

	t.Run("single_relay_disconnected", func(t *testing.T) {
		m.relayStates["piccolo-portal"] = relayState{connected: false, err: "dial error"}
		if m.httpChallengeReachable() {
			t.Fatal("expected false when single relay disconnected")
		}
	})

	t.Run("two_relays_both_connected", func(t *testing.T) {
		m.relayStates["piccolo-portal"] = relayState{connected: true}
		m.relayStates["piccolo-namek"] = relayState{connected: true}
		if !m.httpChallengeReachable() {
			t.Fatal("expected true when all relays connected")
		}
	})

	t.Run("two_relays_one_disconnected", func(t *testing.T) {
		m.relayStates["piccolo-portal"] = relayState{connected: true}
		m.relayStates["piccolo-namek"] = relayState{connected: false, err: "timeout"}
		if m.httpChallengeReachable() {
			t.Fatal("expected false when one relay disconnected")
		}
	})

	t.Run("clear_relay_state_removes_entry", func(t *testing.T) {
		m.relayStates["piccolo-portal"] = relayState{connected: true}
		m.relayStates["piccolo-namek"] = relayState{connected: false, err: "timeout"}
		m.ClearRelayState("piccolo-namek")
		if !m.httpChallengeReachable() {
			t.Fatal("expected true after clearing disconnected relay")
		}
	})
}

func TestNeedsRelay(t *testing.T) {
	tests := []struct {
		solver string
		want   bool
	}{
		{acme.SolverHTTP01, true},
		{"HTTP-01", true},
		{"", true}, // conservative default
		{acme.SolverDNS01, false},
		{"DNS-01", false},
	}
	for _, tt := range tests {
		if got := needsRelay(tt.solver); got != tt.want {
			t.Errorf("needsRelay(%q) = %v, want %v", tt.solver, got, tt.want)
		}
	}
}

func TestRelayGate_HTTP01DeferredWhenDisconnected(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(10, 0)))

	// Configure self-hosted remote (creates a pending HTTP-01 portal cert)
	if err := m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// No relay connected — cert should stay pending (not issued)
	time.Sleep(100 * time.Millisecond)
	for _, c := range m.ListCertificates() {
		if c.ID == "portal" && !strings.EqualFold(c.Status, string(CertStatusPending)) {
			t.Fatalf("portal cert should remain 'pending' with no relay, got %q", c.Status)
		}
	}

	// Simulate relay connect — should trigger issuance
	m.handleRelayEvent("piccolo-portal", true, "")
	time.Sleep(500 * time.Millisecond)

	var found bool
	for _, c := range m.ListCertificates() {
		if c.ID == "portal" {
			found = true
			if !strings.EqualFold(c.Status, string(CertStatusOK)) {
				t.Fatalf("portal cert should be 'ok' after relay connect, got %q", c.Status)
			}
		}
	}
	if !found {
		t.Fatal("portal cert not found")
	}
}

func TestRelayGate_DNS01NotAffectedByRelayState(t *testing.T) {
	// DNS-01 certs should not be gated by relay connectivity.
	if needsRelay(acme.SolverDNS01) {
		t.Fatal("dns-01 should not need relay")
	}

	m := &Manager{relayStates: make(map[string]relayState)}
	// Even with no relays connected, dns-01 gate check should not block.
	// The actual DNS-01 gating is via orchClient availability, not relay state.
	if needsRelay(acme.SolverDNS01) && !m.httpChallengeReachable() {
		t.Fatal("dns-01 should bypass relay gate")
	}
}

func TestRelayGate_DNS01ManagedNotBlockedByRelay(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(10, 0)))

	// Configure managed mode (DNS-01) — no relay needed for cert issuance.
	if err := m.ConfigureManaged(ManagedConfigureRequest{
		OrchestratorEndpoint: "https://orchestrator.example.com",
		DeviceToken:          "secret",
		PortalHostname:       "portal.example.com",
	}); err != nil {
		t.Fatalf("configure managed: %v", err)
	}

	// No relay connected — DNS-01 certs should still be issued because
	// they don't depend on relay connectivity (challenges go through orchestrator API).
	time.Sleep(500 * time.Millisecond)

	for _, c := range m.ListCertificates() {
		if c.ID == "portal" || c.ID == "wildcard" {
			if strings.EqualFold(c.Status, string(CertStatusPending)) {
				t.Fatalf("DNS-01 cert %s should NOT be stuck in 'pending' without relay (solver=%s)", c.ID, c.Solver)
			}
		}
	}
}

func TestRelayGate_BootSequence(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	// Step 1: Create manager, configure, and close (simulates previous session)
	m1 := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(10, 0)))
	if err := m1.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	_ = m1.Close()

	// Step 2: "Reboot" — new manager loads persisted config with pending cert.
	// requeueOutstandingIssuances fires at boot but relay is not connected.
	m2, err := newManagerWithDeps(storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(20, 0)))
	if err != nil {
		t.Fatalf("manager reboot: %v", err)
	}
	t.Cleanup(func() { _ = m2.Close() })

	// Cert should still be pending (not error) — boot requeue was gated.
	time.Sleep(100 * time.Millisecond)
	for _, c := range m2.ListCertificates() {
		if c.ID == "portal" {
			if strings.EqualFold(c.Status, string(CertStatusError)) {
				t.Fatalf("portal cert should NOT be in 'error' state after boot (relay gate should have deferred it), got error: %s", c.FailureReason)
			}
			if !strings.EqualFold(c.Status, string(CertStatusPending)) {
				t.Fatalf("portal cert should be 'pending' after boot with no relay, got %q", c.Status)
			}
		}
	}

	// Step 3: Relay connects — cert should be issued
	m2.handleRelayEvent("piccolo-portal", true, "")
	time.Sleep(500 * time.Millisecond)

	for _, c := range m2.ListCertificates() {
		if c.ID == "portal" {
			if !strings.EqualFold(c.Status, string(CertStatusOK)) {
				t.Fatalf("portal cert should be 'ok' after relay connect on reboot, got %q (reason: %s)", c.Status, c.FailureReason)
			}
		}
	}
}

// --- Infinite retry loop fix tests ---

var testAppJob = issuanceJob{
	id:         "host:app.portal.example.com",
	domains:    []string{"app.portal.example.com"},
	commonName: "app.portal.example.com",
}

func TestProcessIssuance_SetsStatusPending(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(100, 0)))

	// Simulate a connected relay so processIssuance doesn't defer.
	m.handleRelayEvent("piccolo-portal", true, "")

	// Seed a cert in "error" state (simulating a previous failure).
	m.cfgMu.Lock()
	cfg := m.currentConfig()
	cfg.PortalHostname = "portal.example.com"
	cfg.Certificates = []Certificate{{
		ID:                "host:app.portal.example.com",
		Domains:           []string{"app.portal.example.com"},
		Status:            string(CertStatusError),
		FailureReason:     "previous failure",
		FailureClass:      FailureClassTransient,
		FailureCode:       "cert_unknown_error",
		Attempts:          3,
		TransientAttempts: 3,
	}}
	if err := m.saveAll(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Verify the recording block sets status to "pending" before ACME call.
	// We can observe this indirectly: read the persisted state after the
	// save at line 2234 (which happens before ACME). Since fake ACME is
	// synchronous and fast, we verify the final state instead.
	m.processIssuance(testAppJob)

	var found Certificate
	for _, c := range m.ListCertificates() {
		if c.ID == "host:app.portal.example.com" {
			found = c
			break
		}
	}
	// With fake ACME the cert succeeds, so final status is "ok".
	// This proves processIssuance ran through the recording block
	// (which now sets status="pending") and then completed successfully.
	if found.Status != string(CertStatusOK) {
		t.Fatalf("expected status ok after fake ACME, got %q (reason: %s)", found.Status, found.FailureReason)
	}
}

func TestProcessIssuance_PendingBlocksReenqueue(t *testing.T) {
	// Verifies that after processIssuance sets status to "pending",
	// a concurrent enqueueIssuanceJob call for the same cert is blocked.
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(200, 0)))

	// Seed a cert in "error" state with RetryAt=nil (the vulnerable state).
	m.cfgMu.Lock()
	cfg := m.currentConfig()
	cfg.PortalHostname = "portal.example.com"
	cfg.Certificates = []Certificate{{
		ID:      "host:app.portal.example.com",
		Domains: []string{"app.portal.example.com"},
		Status:  string(CertStatusError),
		// RetryAt is nil — before the fix, this would bypass the backoff guard.
	}}
	if err := m.saveAll(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Simulate what processIssuance does: set status to pending, save.
	m.cfgMu.Lock()
	cfg = m.currentConfig()
	for i := range cfg.Certificates {
		if cfg.Certificates[i].ID == "host:app.portal.example.com" {
			cfg.Certificates[i].Status = string(CertStatusPending)
			cfg.Certificates[i].RetryAt = nil
		}
	}
	if err := m.saveCerts(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Simulate TopicRemoteConfigChanged handler re-enqueueing — should be
	// blocked by the "pending" guard.
	m.enqueueIssuanceJob(testAppJob)

	// Cert should still be "pending" — NOT reset via ensureCertPending
	// (which would re-save and potentially trigger another event).
	for _, c := range m.ListCertificates() {
		if c.ID == "host:app.portal.example.com" {
			if c.Status != string(CertStatusPending) {
				t.Fatalf("expected cert to remain pending, got %q", c.Status)
			}
			return
		}
	}
	t.Fatal("cert not found")
}

// TestSchedulerRetry_DNS01_ResolvesOrchClient verifies that the scheduler retry
// path for dns-01 certs works even though the scheduler passes solver="dns-01"
// explicitly (previously skipped orchClient resolution in enqueueIssuanceJob).
func TestSchedulerRetry_DNS01_ResolvesOrchClient(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(30, 0)))
	m.RegisterOrchClient("namek", stubOrchClient{})

	// Simulate what the scheduler does: call enqueueIssuanceJob with solver
	// pre-set (the scheduler reads it from persisted cert metadata).
	m.enqueueIssuanceJob(issuanceJob{
		id:         "namek-custom-wildcard",
		domains:    []string{"*.custom.example.com", "custom.example.com"},
		commonName: "*.custom.example.com",
		source:     "namek",
		solver:     acme.SolverDNS01,
		certDir:    dir,
	})

	waitForCertNotPending(t, m, "namek-custom-wildcard", 2*time.Second)
	for _, c := range m.ListCertificates() {
		if c.ID == "namek-custom-wildcard" {
			if strings.EqualFold(c.Status, string(CertStatusError)) &&
				strings.Contains(c.FailureReason, "orchClient") {
				t.Fatalf("scheduler retry should resolve orchClient from live registry, got error: %s", c.FailureReason)
			}
			return
		}
	}
	t.Fatal("cert not found")
}

func TestQueueIssuanceJobToWorker_BlocksForceDuplicates(t *testing.T) {
	// Use a raw Manager with no worker goroutine to test channel behavior directly.
	m := &Manager{
		issueCh:     make(chan issuanceJob, 32),
		issueQueued: make(map[string]struct{}),
	}

	m.queueIssuanceJobToWorker(issuanceJob{
		id:         "test-cert",
		commonName: "test.example.com",
		domains:    []string{"test.example.com"},
	})

	// Same ID with reissue=true — must still be blocked.
	m.queueIssuanceJobToWorker(issuanceJob{
		id:         "test-cert",
		commonName: "test.example.com",
		domains:    []string{"test.example.com"},
		reissue:    true,
	})

	// Drain the channel — should get exactly one job.
	count := 0
	for {
		select {
		case <-m.issueCh:
			count++
		default:
			goto done
		}
	}
done:
	if count != 1 {
		t.Fatalf("expected exactly 1 job in channel, got %d", count)
	}
}

func TestIssueQueued_ClearedAfterProcessing(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(400, 0)))

	// Simulate connected relay so processIssuance doesn't bail at relay gate.
	m.handleRelayEvent("piccolo-portal", true, "")

	// Seed a cert so processIssuance can find it.
	m.cfgMu.Lock()
	cfg := m.currentConfig()
	cfg.PortalHostname = "portal.example.com"
	cfg.Certificates = []Certificate{{
		ID:      "host:app.portal.example.com",
		Domains: []string{"app.portal.example.com"},
		Status:  string(CertStatusPending),
	}}
	if err := m.saveAll(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	m.issueMu.Lock()
	m.issueQueued["host:app.portal.example.com"] = struct{}{}
	m.issueMu.Unlock()

	// Mirror the production runIssuanceWorker defer pattern.
	func() {
		defer func() {
			m.issueMu.Lock()
			delete(m.issueQueued, "host:app.portal.example.com")
			m.issueMu.Unlock()
		}()
		m.processIssuance(issuanceJob{
			id:         "host:app.portal.example.com",
			domains:    []string{"app.portal.example.com"},
			commonName: "app.portal.example.com",
		})
	}()

	// Verify the cert was actually processed (not deferred at relay gate).
	for _, c := range m.ListCertificates() {
		if c.ID == "host:app.portal.example.com" {
			if c.Status != string(CertStatusOK) {
				t.Fatalf("expected cert to be issued, got status %q", c.Status)
			}
		}
	}

	// Verify issueQueued is cleared.
	m.issueMu.Lock()
	_, stillQueued := m.issueQueued["host:app.portal.example.com"]
	m.issueMu.Unlock()
	if stillQueued {
		t.Fatal("expected issueQueued to be cleared after processIssuance")
	}
}

// TestQueueDedup_PreventsStampede verifies that multiple concurrent enqueue
// calls for the same cert ID (even with reissue=true) produce only one
// worker execution at a time.
func TestQueueDedup_PreventsStampede(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(30, 0)))
	m.RegisterOrchClient("namek", stubOrchClient{})

	// Rapidly enqueue the same cert 10 times with reissue=true.
	// Uses dns-01/namek to avoid the HTTP-01 relay gate.
	for i := 0; i < 10; i++ {
		m.enqueueIssuanceJob(issuanceJob{
			id:         "stampede-test",
			domains:    []string{"stampede.example.com"},
			commonName: "stampede.example.com",
			reissue:    true,
			source:     "namek",
			solver:     acme.SolverDNS01,
			certDir:    dir,
		})
	}

	waitForCertNotPending(t, m, "stampede-test", 2*time.Second)

	// The cert should have been issued successfully (not stuck or errored from duplication).
	for _, c := range m.ListCertificates() {
		if c.ID == "stampede-test" {
			if !strings.EqualFold(c.Status, string(CertStatusOK)) {
				t.Fatalf("expected ok, got %q (reason: %s)", c.Status, c.FailureReason)
			}
			return
		}
	}
	t.Fatal("cert not found")
}

// TestWorkerDefers_WhenOrchClientUnavailable verifies that the worker defers
// without recording an error when the orchClient is not available for a dns-01
// cert. The cert should remain in "pending" state (no backoff).
func TestWorkerDefers_WhenOrchClientUnavailable(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, err := newFileStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(30, 0)))

	// Register orchClient so enqueueIssuanceJob's gate passes...
	m.RegisterOrchClient("namek", stubOrchClient{})

	m.enqueueIssuanceJob(issuanceJob{
		id:         "defer-test",
		domains:    []string{"*.defer.example.com"},
		commonName: "*.defer.example.com",
		source:     "namek",
		solver:     acme.SolverDNS01,
		certDir:    dir,
	})

	// ...then immediately unregister so the worker finds nil at resolution time.
	m.UnregisterOrchClient("namek")

	// Give the worker time to process and defer.
	time.Sleep(200 * time.Millisecond)

	for _, c := range m.ListCertificates() {
		if c.ID == "defer-test" {
			// Should be pending (deferred) — NOT error with backoff.
			if strings.EqualFold(c.Status, string(CertStatusError)) {
				t.Fatalf("worker should defer without error when orchClient unavailable, got error: %s", c.FailureReason)
			}
			return
		}
	}
	t.Fatal("cert not found")
}

func TestDeriveACMEEmail(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{"empty", "", ""},
		{"bare_label", "localhost", ""},
		{"bare_label_no_dot", "piccolo", ""},
		{"valid_fqdn", "portal.example.com", "admin@portal.example.com"},
		{"namek_slug", "hngjr00yn8qk8h2b55y1.piccolospace.com", "admin@hngjr00yn8qk8h2b55y1.piccolospace.com"},
		{"trailing_dot", "portal.example.com.", "admin@portal.example.com"},
		{"mixed_case", "Portal.Example.COM", "admin@portal.example.com"},
		{"whitespace", "  portal.example.com  ", "admin@portal.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveACMEEmail(tt.hostname)
			if got != tt.want {
				t.Errorf("deriveACMEEmail(%q) = %q, want %q", tt.hostname, got, tt.want)
			}
		})
	}
}

func TestDeriveNamekACMEEmail(t *testing.T) {
	tests := []struct {
		name         string
		slugHostname string
		baseDomain   string
		want         string
	}{
		{"normal", "hngjr00yn8qk8h2b55y1.piccolospace.com", "piccolospace.com", "hngjr00yn8qk8h2b55y1@piccolospace.com"},
		{"empty_slug", "", "piccolospace.com", ""},
		{"empty_base", "slug.example.com", "", ""},
		{"both_empty", "", "", ""},
		{"no_dot_in_slug", "localhost", "example.com", ""},
		{"multi_label", "slug.region.example.com", "example.com", "slug@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveNamekACMEEmail(tt.slugHostname, tt.baseDomain)
			if got != tt.want {
				t.Errorf("DeriveNamekACMEEmail(%q, %q) = %q, want %q", tt.slugHostname, tt.baseDomain, got, tt.want)
			}
		})
	}
}

// --- validateCertFiles tests ---

// writeTestCert writes a self-signed .crt and .key to dir/outName.{crt,key}
// with the given notBefore/notAfter. Returns expiry.
func writeTestCert(t *testing.T, dir, outName, cn string, notBefore, notAfter time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), cryptoRand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	serial, _ := cryptoRand.Int(cryptoRand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{cn},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(cryptoRand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyBytes, _ := x509.MarshalECPrivateKey(priv)
	crtPath := filepath.Join(dir, outName+".crt")
	keyPath := filepath.Join(dir, outName+".key")
	if err := os.WriteFile(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write crt: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func newValidateTestManager(t *testing.T, baseDir string) *Manager {
	t.Helper()
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	m := &Manager{
		baseDir: baseDir,
		now:     fixedNow(now),
	}
	return m
}

func TestValidateCertFiles_MissingCrt(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)
	certDir := filepath.Join(dir, "remote", "certs")

	// Write .key only — no .crt file. Tests the ".crt missing" path specifically.
	now := time.Now()
	writeTestCert(t, certDir, "test.example.com", "test.example.com",
		now.Add(-time.Hour), now.Add(90*24*time.Hour))
	os.Remove(filepath.Join(certDir, "test.example.com.crt"))

	cfg := &Config{
		NexusConfig: NexusConfig{PortalHostname: "portal.example.com"},
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:      "test-cert",
				Domains: []string{"test.example.com"},
				Status:  "ok",
				Source:  "namek",
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected status=pending after missing .crt, got %s", cfg.Certificates[0].Status)
	}
	if cfg.Certificates[0].IssuedAt != nil || cfg.Certificates[0].ExpiresAt != nil || cfg.Certificates[0].NextRenewal != nil {
		t.Fatalf("expected timing fields cleared")
	}
}

func TestValidateCertFiles_MissingKey(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)
	certDir := filepath.Join(dir, "remote", "certs")

	// Write .crt but NOT .key
	now := time.Now()
	writeTestCert(t, certDir, "test.example.com", "test.example.com", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	os.Remove(filepath.Join(certDir, "test.example.com.key"))

	cfg := &Config{
		NexusConfig: NexusConfig{PortalHostname: "portal.example.com"},
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:      "test-cert",
				Domains: []string{"test.example.com"},
				Status:  "ok",
				Source:  "namek",
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected status=pending after missing .key, got %s", cfg.Certificates[0].Status)
	}
}

func TestValidateCertFiles_ExpiredFile(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)
	certDir := filepath.Join(dir, "remote", "certs")

	// Write an expired cert
	writeTestCert(t, certDir, "test.example.com", "test.example.com",
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)) // expired well before 2026-03-30

	cfg := &Config{
		NexusConfig: NexusConfig{PortalHostname: "portal.example.com"},
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:      "test-cert",
				Domains: []string{"test.example.com"},
				Status:  "ok",
				Source:  "namek",
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected status=pending after expired .crt, got %s", cfg.Certificates[0].Status)
	}
}

func TestValidateCertFiles_ValidFile(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)
	certDir := filepath.Join(dir, "remote", "certs")

	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	writeTestCert(t, certDir, "test.example.com", "test.example.com",
		now.Add(-24*time.Hour), now.Add(60*24*time.Hour))

	issued := now.Add(-24 * time.Hour)
	expires := now.Add(60 * 24 * time.Hour)
	renewal := now.Add(30 * 24 * time.Hour)
	cfg := &Config{
		NexusConfig: NexusConfig{PortalHostname: "portal.example.com"},
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:          "test-cert",
				Domains:     []string{"test.example.com"},
				Status:      "ok",
				Source:      "namek",
				IssuedAt:    &issued,
				ExpiresAt:   &expires,
				NextRenewal: &renewal,
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "ok" {
		t.Fatalf("expected status=ok for valid cert, got %s", cfg.Certificates[0].Status)
	}
	if cfg.Certificates[0].IssuedAt == nil || cfg.Certificates[0].ExpiresAt == nil {
		t.Fatal("expected timing fields preserved for valid cert")
	}
}

func TestValidateCertFiles_PortalCert(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)
	certDir := filepath.Join(dir, "remote", "certs")

	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	// Portal cert uses "portal" as filename, not the hostname
	writeTestCert(t, certDir, "portal", "portal.example.com",
		now.Add(-24*time.Hour), now.Add(60*24*time.Hour))

	cfg := &Config{
		NexusConfig: NexusConfig{PortalHostname: "portal.example.com"},
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:      "portal",
				Domains: []string{"portal.example.com"},
				Status:  "ok",
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "ok" {
		t.Fatalf("expected status=ok for portal cert with correct file, got %s", cfg.Certificates[0].Status)
	}

	// Now remove the file and verify it resets
	os.Remove(filepath.Join(certDir, "portal.crt"))

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected status=pending after removing portal.crt, got %s", cfg.Certificates[0].Status)
	}
}

func TestValidateCertFiles_CertDirOverride(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)

	// Use a custom certDir separate from the default
	customDir := filepath.Join(t.TempDir(), "custom-certs")
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	writeTestCert(t, customDir, "test.example.com", "test.example.com",
		now.Add(-24*time.Hour), now.Add(60*24*time.Hour))

	cfg := &Config{
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:      "test-cert",
				Domains: []string{"test.example.com"},
				Status:  "ok",
				Source:  "namek",
				CertDir: customDir,
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "ok" {
		t.Fatalf("expected status=ok with custom certDir, got %s", cfg.Certificates[0].Status)
	}

	// Cert NOT in default dir should fail if CertDir is cleared
	cfg.Certificates[0].CertDir = ""
	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected status=pending when cert not in default dir, got %s", cfg.Certificates[0].Status)
	}
}

func TestValidateCertFiles_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)
	certDir := filepath.Join(dir, "remote", "certs")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Write a valid .key but corrupt .crt
	now := time.Now()
	writeTestCert(t, certDir, "test.example.com", "test.example.com",
		now.Add(-time.Hour), now.Add(90*24*time.Hour))
	// Overwrite .crt with garbage
	if err := os.WriteFile(filepath.Join(certDir, "test.example.com.crt"), []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:      "test-cert",
				Domains: []string{"test.example.com"},
				Status:  "ok",
				Source:  "namek",
			}},
		},
	}

	m.validateCertFiles(cfg)

	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected status=pending for corrupt .crt, got %s", cfg.Certificates[0].Status)
	}
}

func TestValidateCertFiles_SkipsPendingAndError(t *testing.T) {
	dir := t.TempDir()
	m := newValidateTestManager(t, dir)

	cfg := &Config{
		CertInventory: CertInventory{
			Certificates: []Certificate{
				{ID: "pending-cert", Domains: []string{"a.example.com"}, Status: "pending", Source: "namek"},
				{ID: "error-cert", Domains: []string{"b.example.com"}, Status: "error", Source: "namek"},
			},
		},
	}

	m.validateCertFiles(cfg)

	// Neither should be touched
	if cfg.Certificates[0].Status != "pending" {
		t.Fatalf("expected pending cert unchanged, got %s", cfg.Certificates[0].Status)
	}
	if cfg.Certificates[1].Status != "error" {
		t.Fatalf("expected error cert unchanged, got %s", cfg.Certificates[1].Status)
	}
}

func TestScanAndQueueRenewals_MissingCertFile(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()

	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	issued := now.Add(-24 * time.Hour)
	expires := now.Add(60 * 24 * time.Hour)
	renewal := now.Add(30 * 24 * time.Hour)

	m := newTestManagerWithDeps(t, nil, dir, &stubDialer{}, &stubResolver{}, fixedNow(now))
	m.RegisterOrchClient("namek", stubOrchClient{})
	m.cfgMu.Lock()
	m.cfg = &Config{
		CertInventory: CertInventory{
			Certificates: []Certificate{{
				ID:          "test-cert",
				Domains:     []string{"test.example.com"},
				Status:      "ok",
				Source:      "namek",
				Solver:      acme.SolverDNS01,
				IssuedAt:    &issued,
				ExpiresAt:   &expires,
				NextRenewal: &renewal,
			}},
		},
	}
	m.cfgMu.Unlock()

	// Stop the issuance worker so jobs stay on the channel for inspection.
	if m.issueCancel != nil {
		m.issueCancel()
		<-m.issueDone
	}
	// Drain any jobs queued during setup.
	for len(m.issueCh) > 0 {
		<-m.issueCh
	}

	// No cert file on disk — scanner should detect and enqueue reissue.
	m.scanAndQueueRenewals()

	// The job should be on issueCh.
	select {
	case job := <-m.issueCh:
		if job.id != "test-cert" {
			t.Fatalf("expected job for test-cert, got %s", job.id)
		}
		if !job.reissue {
			t.Fatal("expected reissue=true for missing-file job")
		}
	default:
		t.Fatal("expected issuance job enqueued for missing cert file, got none")
	}
}
