package remote

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

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
		{"", true},          // conservative default
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
