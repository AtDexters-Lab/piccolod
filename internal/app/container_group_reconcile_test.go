package app

import (
	"context"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func TestReconcileMultiContainer_StopsServicesWhenAnchorMissingAndDesiredStopped(t *testing.T) {
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
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("ensureStateManager: %v", err)
	}

	// RFC 20260130: listener name is the app identity, set Primary=true for test
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"side": {Image: "alpine:latest", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	layout, err := mgr.ensureAppVolumeLayout(ctx, "demo")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp("demo", layout, ModeService)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main container: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, mainCID); err != nil {
		t.Fatalf("start main container: %v", err)
	}

	sideCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo__side",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "side", "service"),
	})
	if err != nil {
		t.Fatalf("create side container: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, sideCID); err != nil {
		t.Fatalf("start side container: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "demo",
		Status:          "running",
		PrimaryService:  "main",
		NetworkAnchorID: "", // missing anchor (manual deletion or partial failure)
		Containers: map[string]string{
			"main": mainCID,
			"side": sideCID,
		},
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, false); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}

	mainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if mainState.Running {
		t.Fatalf("expected main container to be stopped")
	}
	sideState, _ := mock.InspectContainerState(ctx, runtime, sideCID)
	if sideState.Running {
		t.Fatalf("expected side container to be stopped")
	}
}
