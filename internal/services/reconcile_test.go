package services

import (
	"piccolod/internal/api"
	"testing"
)

func TestReconcile_AddRemoveChange(t *testing.T) {
	m := NewServiceManager()
	useFakeProxyListeners(m)
	// Seed existing app endpoints
	eps, err := m.AllocateForApp("app", []api.AppListener{{Name: "a", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw}})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint")
	}
	oldHB := eps[0].HostBind
	oldPP := eps[0].PublicPort

	// Reconcile: change guest port for a, add b, remove nothing
	rec, _, err := m.Reconcile("app", []api.AppListener{
		{Name: "a", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw},
		{Name: "b", GuestPort: 22, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.GuestPortChanged) != 1 {
		t.Fatalf("want 1 guest change, got %d", len(rec.GuestPortChanged))
	}
	if rec.GuestPortChanged[0].Old.HostBind != oldHB || rec.GuestPortChanged[0].Old.PublicPort != oldPP {
		t.Fatalf("host/public must be preserved for changed listener")
	}
	if len(rec.Added) != 1 {
		t.Fatalf("want 1 added, got %d", len(rec.Added))
	}
}

func TestReconcile_AuthAddition(t *testing.T) {
	m := NewServiceManager()
	useFakeProxyListeners(m)

	// Allocate with no auth
	eps, err := m.AllocateForApp("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if eps[0].Auth != nil {
		t.Fatal("expected nil auth after initial allocation")
	}

	// Reconcile: add auth rules
	auth := &api.ListenerAuth{
		Rules: []api.ListenerAuthRule{
			{Path: "/", Type: "prefix", Strategy: "public"},
		},
	}
	rec, containerChange, err := m.Reconcile("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if containerChange {
		t.Fatal("auth-only change should not require container restart")
	}
	if len(rec.ProxyOnlyChanged) != 1 {
		t.Fatalf("want 1 proxy-only change, got %d", len(rec.ProxyOnlyChanged))
	}
	if rec.ProxyOnlyChanged[0].Auth == nil {
		t.Fatal("proxy-only changed endpoint should have auth set")
	}

	// Verify registry has auth
	ep, ok := m.GetAppListener("app", "web")
	if !ok {
		t.Fatal("endpoint not found in registry")
	}
	if ep.Auth == nil || len(ep.Auth.Rules) != 1 || ep.Auth.Rules[0].Strategy != "public" {
		t.Fatalf("registry endpoint auth mismatch: %+v", ep.Auth)
	}
}

func TestPrepareReconcileDefersProxyOnlyPublish(t *testing.T) {
	m := NewServiceManager()
	useFakeProxyListeners(m)
	m.registry["app"] = map[string]ServiceEndpoint{
		"web": {
			App:        "app",
			Name:       "web",
			GuestPort:  80,
			HostBind:   18080,
			PublicPort: 28080,
			Flow:       api.FlowTCP,
			Protocol:   api.ListenerProtocolHTTP,
		},
	}
	m.allocator.usedHost[18080] = struct{}{}
	m.allocator.usedPublic[publicKey(28080, "tcp")] = struct{}{}

	auth := &api.ListenerAuth{
		Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}},
	}
	prepared, err := m.PrepareReconcile("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
	})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	if len(prepared.Result().ProxyOnlyChanged) != 1 {
		t.Fatalf("want proxy-only change, got %+v", prepared.Result())
	}
	ep, ok := m.GetAppListener("app", "web")
	if !ok {
		t.Fatalf("endpoint missing before publish")
	}
	if ep.Auth != nil {
		t.Fatalf("prepare should not publish auth into registry, got %+v", ep.Auth)
	}

	if _, _, err := prepared.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	t.Cleanup(m.StopAll)
	ep, ok = m.GetAppListener("app", "web")
	if !ok {
		t.Fatalf("endpoint missing after publish")
	}
	if ep.Auth == nil || ep.Auth.Rules[0].Strategy != "public" {
		t.Fatalf("publish did not update registry auth: %+v", ep.Auth)
	}
}

func TestReconcile_AuthRemoval(t *testing.T) {
	m := NewServiceManager()
	useFakeProxyListeners(m)

	// Allocate with auth
	auth := &api.ListenerAuth{
		Rules: []api.ListenerAuthRule{
			{Path: "/", Type: "prefix", Strategy: "public"},
		},
	}
	_, err := m.AllocateForApp("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
	})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	// Reconcile: remove auth
	rec, containerChange, err := m.Reconcile("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if containerChange {
		t.Fatal("auth-only change should not require container restart")
	}
	if len(rec.ProxyOnlyChanged) != 1 {
		t.Fatalf("want 1 proxy-only change, got %d", len(rec.ProxyOnlyChanged))
	}

	ep, ok := m.GetAppListener("app", "web")
	if !ok {
		t.Fatal("endpoint not found in registry")
	}
	if ep.Auth != nil {
		t.Fatalf("expected nil auth after removal, got %+v", ep.Auth)
	}
}

func TestReconcile_NewListenerWithAuth(t *testing.T) {
	m := NewServiceManager()
	useFakeProxyListeners(m)

	// Allocate with one listener, no auth
	_, err := m.AllocateForApp("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	// Reconcile: add new listener with auth
	auth := &api.ListenerAuth{
		Rules: []api.ListenerAuthRule{
			{Path: "/api", Type: "prefix", Strategy: "public"},
		},
	}
	rec, _, err := m.Reconcile("app", []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		{Name: "api", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.Added) != 1 {
		t.Fatalf("want 1 added, got %d", len(rec.Added))
	}

	ep, ok := m.GetAppListener("app", "api")
	if !ok {
		t.Fatal("new endpoint not found in registry")
	}
	if ep.Auth == nil || len(ep.Auth.Rules) != 1 || ep.Auth.Rules[0].Strategy != "public" {
		t.Fatalf("new endpoint auth mismatch: %+v", ep.Auth)
	}
}

func TestAuthEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b *api.ListenerAuth
		want bool
	}{
		{"both_nil", nil, nil, true},
		{"nil_vs_empty_rules", nil, &api.ListenerAuth{}, true},
		{"empty_rules_vs_nil", &api.ListenerAuth{}, nil, true},
		{"both_empty_rules", &api.ListenerAuth{}, &api.ListenerAuth{}, true},
		{"nil_rules_vs_empty_slice", &api.ListenerAuth{Rules: nil}, &api.ListenerAuth{Rules: []api.ListenerAuthRule{}}, true},
		{"same_rules", &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}},
		}, &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}},
		}, true},
		{"different_strategy", &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}},
		}, &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "protected"}},
		}, false},
		{"different_length", &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}},
		}, &api.ListenerAuth{}, false},
		{"same_rules_different_order", &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{
				{Path: "/api", Type: "prefix", Strategy: "public"},
				{Path: "/admin", Type: "prefix", Strategy: "protected"},
			},
		}, &api.ListenerAuth{
			Rules: []api.ListenerAuthRule{
				{Path: "/admin", Type: "prefix", Strategy: "protected"},
				{Path: "/api", Type: "prefix", Strategy: "public"},
			},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("authEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}
