package app

import (
	"context"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/services"
)

func TestInstallMultiContainer_PrunesZombiesBeforeCreate(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManager(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)

	ctx := context.Background()
	layout, err := mgr.ensureAppVolumeLayout(ctx, "demo")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp("demo", layout)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	// Simulate a prior partial install leaving a labeled container that is not part of the expected set.
	zombieID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo__zombie",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "zombie", "service"),
	})
	if err != nil {
		t.Fatalf("create zombie container: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, zombieID); err != nil {
		t.Fatalf("start zombie container: %v", err)
	}

	def := &api.AppDefinition{
		Name: "demo",
		Listeners: []api.AppListener{
			{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"db":   {Image: "alpine:latest", BindPorts: []int{5432}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)
	if err := ValidateAppDefinition(def); err != nil {
		t.Fatalf("ValidateAppDefinition: %v", err)
	}

	_, err = mgr.installMultiContainer(ctx, def, "demo", "", layout, runtime, []services.ServiceEndpoint{
		{App: "demo", Name: "web", GuestPort: 8080, HostBind: 18080, PublicPort: 28080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("installMultiContainer: %v", err)
	}

	for _, c := range mock.containers {
		if c != nil && c.Spec.Name == "demo__zombie" {
			t.Fatalf("expected zombie container to be pruned before install")
		}
	}
}
