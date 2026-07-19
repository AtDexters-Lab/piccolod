package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func TestStopRemoveContainersForMultiApp_UsesNamesWhenIDsStale(t *testing.T) {
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
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
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "demo", layout, ModeService, appRuntimeEnsureReady)
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
	zombieCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo__unexpected",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "unexpected", "service"),
	})
	if err != nil {
		t.Fatalf("create unexpected label-owned container: %v", err)
	}
	_ = mock.StartContainer(ctx, runtime, mainCID)
	_ = mock.StartContainer(ctx, runtime, dbCID)
	_ = mock.StartContainer(ctx, runtime, zombieCID)

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

	// The lifecycle quiescence proof uses deterministic names even if stored
	// IDs are non-empty and stale.
	if err := mgr.stopContainerGroup(ctx, nil, appInst, def, layout, runtime); err != nil {
		t.Fatalf("stopContainerGroup: %v", err)
	}
	mainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if mainState.Running {
		t.Fatalf("expected main container to be stopped")
	}
	dbState, _ := mock.InspectContainerState(ctx, runtime, dbCID)
	if dbState.Running {
		t.Fatalf("expected db container to be stopped")
	}
	zombieState, _ := mock.InspectContainerState(ctx, runtime, zombieCID)
	if zombieState.Running {
		t.Fatalf("expected unexpected label-owned container to be stopped")
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

func TestStopContainerGroupResolutionFailureDoesNotDetachRootfs(t *testing.T) {
	tempDir := t.TempDir()
	mock := NewMockContainerManager()
	mock.resolveErrorForName = map[string]error{"demo": errors.New("podman control unavailable")}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	allowHostStorage(t, mgr)
	rootfs := newStubRootfsManager(tempDir)
	rootfs.exists = map[string]bool{"rootfs-main": true}
	mgr.SetRootfsManager(rootfs)

	def := &api.AppDefinition{
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest"},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	appInst := &AppInstance{
		InstanceID:     "demo",
		PrimaryService: "main",
		Containers:     map[string]string{},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		Definition:     def,
	}

	err = mgr.stopContainerGroup(context.Background(), nil, appInst, def, appVolumeLayout{}, container.PodmanRuntime{})
	if err == nil || !strings.Contains(err.Error(), "podman control unavailable") {
		t.Fatalf("resolution failure authorized quiescence: %v", err)
	}
	if len(rootfs.detached) != 0 {
		t.Fatalf("rootfs detached without process-absence proof: %v", rootfs.detached)
	}
}

func TestRemoveContainersForMultiApp_StopsRunningContainersAndResolvesAnchor(t *testing.T) {
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mock.removeRunningError = true
	mgr, err := NewAppManagerForTest(mock, tempDir)
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
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "demo", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{Name: "demo", Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("create main container: %v", err)
	}
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{Name: "demo__netns__", Image: "pause:latest"})
	if err != nil {
		t.Fatalf("create anchor container: %v", err)
	}
	_ = mock.StartContainer(ctx, runtime, mainCID)
	_ = mock.StartContainer(ctx, runtime, anchorCID)

	appInst := &AppInstance{
		InstanceID:      "demo",
		Status:          StatusRunning,
		PrimaryService:  "main",
		NetworkAnchorID: "deadbeefdeadbeef",
		Containers: map[string]string{
			"main": mainCID,
		},
		Definition: def,
	}

	if err := mgr.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		t.Fatalf("removeContainersForMultiApp: %v", err)
	}
	if _, err := mock.ResolveContainerIDByName(ctx, runtime, "demo"); err == nil {
		t.Fatalf("expected main container to be removed")
	}
	if _, err := mock.ResolveContainerIDByName(ctx, runtime, "demo__netns__"); err == nil {
		t.Fatalf("expected anchor container to be removed")
	}
}
