package app

import (
	"context"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func TestStopRemoveContainersForMultiApp_UsesNamesWhenIDsStale(t *testing.T) {
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
	runtime, err := mgr.podmanRuntimeForApp("demo", layout, ModeService)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	// RFC 20260130: listener name is the app identity, set Primary=true for test
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"db":   {Image: "alpine:latest", BindPorts: []int{5432}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{Name: "demo", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("create main container: %v", err)
	}
	dbCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{Name: "demo__db", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("create db container: %v", err)
	}
	_ = mock.StartContainer(ctx, runtime, mainCID)
	_ = mock.StartContainer(ctx, runtime, dbCID)

	appInst := &AppInstance{
		InstanceID:     "demo",
		Status:         StatusRunning,
		PrimaryService: "main",
		Containers: map[string]string{
			"main": "deadbeefdeadbeef", // stale (non-empty) ID should not block name-based stop/remove
			"db":   "deadbeefdeadbeef",
		},
		Definition: def,
	}

	// Stop uses resolved deterministic names even if stored IDs are non-empty and stale.
	if err := mgr.stopContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		t.Fatalf("stopContainersForMultiApp: %v", err)
	}
	mainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if mainState.Running {
		t.Fatalf("expected main container to be stopped")
	}
	dbState, _ := mock.InspectContainerState(ctx, runtime, dbCID)
	if dbState.Running {
		t.Fatalf("expected db container to be stopped")
	}

	// Remove uses resolved deterministic names even if stored IDs are non-empty and stale.
	if err := mgr.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		t.Fatalf("removeContainersForMultiApp: %v", err)
	}
	if _, err := mock.ResolveContainerIDByName(ctx, runtime, "demo"); err == nil {
		t.Fatalf("expected main container to be removed")
	}
	if _, err := mock.ResolveContainerIDByName(ctx, runtime, "demo__db"); err == nil {
		t.Fatalf("expected db container to be removed")
	}
}
