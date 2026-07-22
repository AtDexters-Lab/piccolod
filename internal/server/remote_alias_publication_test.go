package server

import (
	"context"
	"sync"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/remote"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/services"
)

type capturingAliasAdapter struct {
	mu      sync.Mutex
	configs []nexusclient.Config
}

func (a *capturingAliasAdapter) Configure(cfg nexusclient.Config) error {
	copied := cfg
	copied.Aliases = append([]nexusclient.AliasEntry(nil), cfg.Aliases...)
	copied.ClaimMappings = append([]api.PortClaimInfo(nil), cfg.ClaimMappings...)
	a.mu.Lock()
	a.configs = append(a.configs, copied)
	a.mu.Unlock()
	return nil
}

func (*capturingAliasAdapter) Start(context.Context) error { return nil }
func (*capturingAliasAdapter) Stop(context.Context) error  { return nil }

func (a *capturingAliasAdapter) latestConfig(t *testing.T) nexusclient.Config {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.configs) == 0 {
		t.Fatal("adapter was not configured")
	}
	return a.configs[len(a.configs)-1]
}

func TestNexusAliasPublicationFollowsRuntimeRouteProof(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	rm, err := remote.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("remote manager: %v", err)
	}
	t.Cleanup(func() {
		if err := rm.Close(); err != nil {
			t.Errorf("close remote manager: %v", err)
		}
	})
	if err := rm.Configure(remote.ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure remote: %v", err)
	}
	if _, err := rm.AddAlias("portal", "home.example.net"); err != nil {
		t.Fatalf("add portal alias: %v", err)
	}
	if _, err := rm.AddAlias("demo", "demo.example.net"); err != nil {
		t.Fatalf("add app alias: %v", err)
	}

	svcMgr := services.NewServiceManager()
	svcMgr.UseInMemoryNetworkForTest()
	t.Cleanup(svcMgr.Stop)
	rm.SetPortClaimProvider(svcMgr)
	rm.SetAliasPublicationFilter(func(alias nexusclient.AliasEntry) bool {
		return nexusAliasPublicationEligible(svcMgr, alias)
	})
	adapter := &capturingAliasAdapter{}
	rm.SetNexusAdapter(adapter)

	assertPublishedAliasHostnames(t, adapter.latestConfig(t), "home.example.net")
	if desired := rm.ListAliases(); len(desired) != 2 {
		t.Fatalf("persisted desired aliases changed by publication filter: %+v", desired)
	}

	listeners := []api.AppListener{{
		Name:        "demo",
		GuestPort:   8080,
		Protocol:    api.ListenerProtocolHTTP,
		Flow:        api.FlowTCP,
		RemotePorts: []int{80, 443},
		Primary:     true,
	}}
	if _, err := svcMgr.AllocateForApp("demo", listeners); err != nil {
		t.Fatalf("publish app route: %v", err)
	}
	resolver := newServiceRemoteResolver(svcMgr)
	resolver.UpdateAliases(map[string]string{"demo.example.net": "demo"})
	resolver.SetTlsMuxPort(9443)
	if port, ok := resolver.Resolve("demo.example.net", 443, true); !ok || port != 9443 {
		t.Fatalf("active alias route = %d/%v, want 9443/true", port, ok)
	}
	rm.RefreshPortClaims()
	assertPublishedAliasHostnames(t, adapter.latestConfig(t), "home.example.net", "demo.example.net")

	// Re-evaluation without route removal models an unknown observation: the
	// last proven route remains registered, so its alias must remain published.
	rm.RefreshPortClaims()
	assertPublishedAliasHostnames(t, adapter.latestConfig(t), "home.example.net", "demo.example.net")

	// Suspension preserves the endpoint registry, so withdrawal proves that
	// publication eligibility is active-route state rather than mere presence.
	svcMgr.SuspendAppPublication("demo")
	if port, ok := resolver.Resolve("demo.example.net", 443, true); ok || port != 0 {
		t.Fatalf("suspended alias route = %d/%v, want 0/false", port, ok)
	}
	rm.RefreshPortClaims()
	assertPublishedAliasHostnames(t, adapter.latestConfig(t), "home.example.net")
	if desired := rm.ListAliases(); len(desired) != 2 {
		t.Fatalf("route withdrawal mutated persisted aliases: %+v", desired)
	}
}

func assertPublishedAliasHostnames(t *testing.T, cfg nexusclient.Config, want ...string) {
	t.Helper()
	got := make([]string, 0, len(cfg.Aliases))
	for _, alias := range cfg.Aliases {
		got = append(got, alias.Hostname)
	}
	if len(got) != len(want) {
		t.Fatalf("published aliases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published aliases = %v, want %v", got, want)
		}
	}
}
