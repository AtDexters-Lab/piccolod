package remote

import (
	"piccolod/internal/api"
	"testing"
	"time"
)

type mockPortClaimProvider struct {
	claims []api.PortClaimInfo
}

func (m *mockPortClaimProvider) ActivePortClaims() []api.PortClaimInfo {
	return m.claims
}

func TestManager_RefreshPortClaims(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	dir := t.TempDir()
	storage, _ := newFileStorage(dir)
	m := newTestManagerWithDeps(t, storage, dir, &stubDialer{}, &stubResolver{}, fixedNow(time.Unix(1, 0)))

	adapter := newFakeAdapter()
	m.SetNexusAdapter(adapter)

	provider := &mockPortClaimProvider{
		claims: []api.PortClaimInfo{
			{Port: 1234, HostBind: 5678, Protocol: "tcp"},
		},
	}
	m.SetPortClaimProvider(provider)

	// Initial configuration to enable the adapter
	err := m.Configure(ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	})
	if err != nil {
		t.Fatalf("configure failed: %v", err)
	}

	// Verify initial claims are applied
	cfg := adapter.getConfig()
	if len(cfg.ClaimMappings) != 1 || cfg.ClaimMappings[0].Port != 1234 {
		t.Fatalf("expected 1 claim, got %v", cfg.ClaimMappings)
	}

	// Update claims and refresh
	provider.claims = append(provider.claims, api.PortClaimInfo{Port: 8080, HostBind: 9090, Protocol: "tcp"})
	m.RefreshPortClaims()

	// Verify updated claims are applied
	cfg = adapter.getConfig()
	if len(cfg.ClaimMappings) != 2 {
		t.Fatalf("expected 2 claims, got %v", cfg.ClaimMappings)
	}

	// Verify stable sorting
	provider.claims = []api.PortClaimInfo{
		{Port: 8080, HostBind: 9090, Protocol: "tcp"},
		{Port: 1234, HostBind: 5678, Protocol: "tcp"},
	}
	m.RefreshPortClaims()
	cfg = adapter.getConfig()
	if cfg.ClaimMappings[0].Port != 1234 || cfg.ClaimMappings[1].Port != 8080 {
		t.Fatalf("claims should be sorted by port, got %v", cfg.ClaimMappings)
	}
}
