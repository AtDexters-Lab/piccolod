package app

import (
	"context"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func TestAppManager_LogsForService_MultiContainer(t *testing.T) {
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
			"db":   {Image: "alpine:latest", BindPorts: []int{}},
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

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{Name: "demo", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("create main container: %v", err)
	}
	dbCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{Name: "demo__db", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("create db container: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "demo",
		Status:          StatusRunning,
		PrimaryService:  "main",
		NetworkAnchorID: "netns",
		Containers: map[string]string{
			"main": mainCID,
			"db":   dbCID,
		},
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: def,
	}

	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	lines, err := mgr.LogsForService(ctx, "demo", "db", 5)
	if err != nil {
		t.Fatalf("LogsForService: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
}
