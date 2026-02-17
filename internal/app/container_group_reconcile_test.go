package app

import (
	"context"
	"fmt"
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
		Status:          StatusRunning,
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

// newReconcileTestEnv creates a standard test environment for reconcile tests.
// Returns the manager, mock, state manager, app definition, layout, and runtime.
func newReconcileTestEnv(t *testing.T) (*AppManager, *MockContainerManager, *FilesystemStateManager, *api.AppDefinition, appVolumeLayout, container.PodmanRuntime) {
	t.Helper()
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

	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "testapp", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"side": {Image: "alpine:latest", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	layout, err := mgr.ensureAppVolumeLayout(ctx, "testapp")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp("testapp", layout, ModeService)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	return mgr, mock, state, def, layout, runtime
}

func TestReconcile_AnchorStartFails_RecreatesAndRecovers(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Create anchor container (stopped — simulating post-reboot state)
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	// Leave anchor stopped (default state after create)

	// Create service container (stopped)
	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers:      map[string]string{"main": mainCID},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Make anchor start fail (simulating stale runtime state)
	mock.startErrorForContainer = map[string]error{
		anchorCID: fmt.Errorf("podman start failed: namespace path: no such file or directory"),
	}

	// Reconcile should trigger full recreation
	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}

	// Old containers should be removed
	oldAnchorState, _ := mock.InspectContainerState(ctx, runtime, anchorCID)
	if oldAnchorState.Exists {
		t.Error("old anchor should have been removed")
	}
	oldMainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if oldMainState.Exists {
		t.Error("old main container should have been removed")
	}

	// App should recover to running with new containers
	observed := mgr.getObservedStatus("testapp")
	if observed != StatusRunning {
		t.Errorf("expected status %q, got %q", StatusRunning, observed)
	}

	// Startup tracking should be reset (recovery succeeded)
	if appInst.StartupAttempts != 0 {
		t.Errorf("expected StartupAttempts=0 after recovery, got %d", appInst.StartupAttempts)
	}
}

func TestReconcile_AnchorStartFails_RecreationFails_Escalates(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Create anchor container (stopped)
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers:      map[string]string{"main": mainCID},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Make ALL container starts fail (anchor start fails, and recreation also fails)
	mock.startError = fmt.Errorf("simulated runtime failure")

	// Reconcile should fail but increment escalation counter
	err = mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true)
	if err == nil {
		t.Fatal("expected reconcile to return error when recreation fails")
	}

	// Startup failure should be recorded
	if appInst.StartupAttempts < 1 {
		t.Errorf("expected StartupAttempts >= 1, got %d", appInst.StartupAttempts)
	}
}

func TestReconcile_ServiceStartFails_RecreatesService(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Create and start anchor (running)
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, anchorCID); err != nil {
		t.Fatalf("start anchor: %v", err)
	}

	// Create main service container (stopped — simulating post-reboot)
	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	// Create side service container (stopped)
	sideCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__side",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "side", "service"),
	})
	if err != nil {
		t.Fatalf("create side: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
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

	// Make only the main service container fail to start
	mock.startErrorForContainer = map[string]error{
		mainCID: fmt.Errorf("podman start failed: cgroup deleted"),
	}

	// Reconcile should recreate just the failed service
	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}

	// Old main container should be removed
	oldMainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if oldMainState.Exists {
		t.Error("old main container should have been removed")
	}

	// A new container should have been created for "main"
	newMainCID := appInst.Containers["main"]
	if newMainCID == "" || newMainCID == mainCID {
		t.Error("expected a new container ID for 'main' service")
	}

	// The new container should be running
	newMainState, _ := mock.InspectContainerState(ctx, runtime, newMainCID)
	if !newMainState.Running {
		t.Error("new main container should be running")
	}

	// App should be running
	observed := mgr.getObservedStatus("testapp")
	if observed != StatusRunning {
		t.Errorf("expected status %q, got %q", StatusRunning, observed)
	}
}

func TestReconcile_EscalationAfterRepeatedFailures(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Make ALL container starts fail permanently
	mock.startError = fmt.Errorf("simulated permanent failure")

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStarting,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "", // no anchor — forces recreation path
		Containers:      map[string]string{},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Run reconcile repeatedly until escalation
	for i := 0; i < startupEscalateAfterAttempts+1; i++ {
		_ = mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true)
	}

	// After enough failures, status should escalate to error
	observed := mgr.getObservedStatus("testapp")
	if observed != StatusError {
		t.Errorf("expected status %q after %d failures, got %q",
			StatusError, startupEscalateAfterAttempts, observed)
	}
}
