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
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
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
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "demo", layout, ModeService, appRuntimeEnsureReady)
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
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
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
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "testapp", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	return mgr, mock, state, def, layout, runtime
}

func TestAutomaticStartupRecoveryUsesOneAttemptAcrossFailureAndSuccess(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	appInst := &AppInstance{
		InstanceID: "testapp",
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("first automatic recovery attempt was rejected")
	}
	if appInst.StartupAttempts != 1 {
		t.Fatalf("attempts after begin = %d, want 1", appInst.StartupAttempts)
	}
	// Nested recovery may quiesce the current group before recreate. That
	// interrupts probation but must retain ownership of this in-flight attempt.
	mgr.interruptStartupProbation(appInst.InstanceID)
	if !mgr.startupRecovery[appInst.InstanceID].AttemptActive {
		t.Fatal("quiescence erased the in-flight recovery attempt")
	}
	mgr.handleStartupFailure(state, appInst)
	if appInst.StartupAttempts != 1 {
		t.Fatalf("failed in-flight attempt counted twice: %d", appInst.StartupAttempts)
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("second automatic recovery attempt was rejected")
	}
	mgr.markStartupRecoverySucceeded(state, appInst)
	if appInst.StartupAttempts != 2 {
		t.Fatalf("successful recovery erased history: attempts=%d", appInst.StartupAttempts)
	}
	window := mgr.startupRecovery[appInst.InstanceID]
	if window.ProbationSince.IsZero() || window.AttemptActive {
		t.Fatalf("success window = %+v, want inactive probation", window)
	}
	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("recovery after probation loss was rejected")
	}
	window = mgr.startupRecovery[appInst.InstanceID]
	if appInst.StartupAttempts != 3 || !window.ProbationSince.IsZero() || !window.AttemptActive {
		t.Fatalf("probation loss state = attempts %d window %+v", appInst.StartupAttempts, window)
	}
	mgr.markStartupRecoverySucceeded(state, appInst)
	window = mgr.startupRecovery[appInst.InstanceID]

	window.ProbationSince = time.Now().Add(-startupEscalateAfterDuration - time.Second)
	mgr.startupRecovery[appInst.InstanceID] = window
	mgr.markStartupRecoverySucceeded(state, appInst)
	if appInst.StartupAttempts != 0 || appInst.FirstStartupFailureAt != nil {
		t.Fatalf("history after continuous probation = attempts %d first %v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if _, ok := mgr.startupRecovery[appInst.InstanceID]; ok {
		t.Fatal("probation entry retained after stability window")
	}
}

func TestAutomaticStartupRecoveryAllowsFifthAttemptThenGuardsSixth(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	appInst := &AppInstance{
		InstanceID:            "testapp",
		Enabled:               true,
		CreatedAt:             now,
		UpdatedAt:             now,
		Definition:            def,
		StartupAttempts:       startupEscalateAfterAttempts - 1,
		FirstStartupFailureAt: &now,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("fifth automatic recovery attempt should run")
	}
	if appInst.StartupAttempts != startupEscalateAfterAttempts {
		t.Fatalf("attempts = %d, want %d", appInst.StartupAttempts, startupEscalateAfterAttempts)
	}
	mgr.handleStartupFailure(state, appInst)
	if mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("sixth automatic recovery attempt bypassed escalation guard")
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusError {
		t.Fatalf("observed status = %q, want %q", got, StatusError)
	}
}

func TestExhaustedAutomaticGuardBreaksOldProbationBeforeManualSuccess(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	firstFailure := now.Add(-time.Minute)
	oldProbation := now.Add(-startupEscalateAfterDuration - time.Minute)
	appInst := &AppInstance{
		InstanceID:            "testapp",
		Enabled:               true,
		CreatedAt:             now,
		UpdatedAt:             now,
		Definition:            def,
		StartupAttempts:       startupEscalateAfterAttempts,
		FirstStartupFailureAt: &firstFailure,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	mgr.startupRecovery[appInst.InstanceID] = startupRecoveryWindow{ProbationSince: oldProbation}

	if mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("exhausted automatic recovery guard allowed another attempt")
	}
	window := mgr.startupRecovery[appInst.InstanceID]
	if !window.ProbationSince.IsZero() {
		t.Fatalf("guard rejection retained pre-loss probation: %v", window.ProbationSince)
	}

	// Manual Start bypasses the automatic guard. Its later success must begin a
	// fresh probation rather than applying the healthy time from before loss.
	mgr.interruptStartupProbation(appInst.InstanceID)
	mgr.markStartupRecoverySucceeded(state, appInst)
	window = mgr.startupRecovery[appInst.InstanceID]
	if appInst.StartupAttempts != startupEscalateAfterAttempts || appInst.FirstStartupFailureAt == nil {
		t.Fatalf("manual success cleared exhausted history: attempts=%d first=%v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if window.ProbationSince.IsZero() || window.ProbationSince.Before(now) {
		t.Fatalf("manual success did not start fresh probation: %v", window.ProbationSince)
	}
}

func TestDisabledReconcileCompletionClearsStartupHistory(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	appInst := &AppInstance{
		InstanceID:            "testapp",
		Enabled:               false,
		CreatedAt:             now,
		UpdatedAt:             now,
		Definition:            def,
		StartupAttempts:       startupEscalateAfterAttempts,
		FirstStartupFailureAt: &now,
		Containers:            map[string]string{},
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	mgr.startupRecovery[appInst.InstanceID] = startupRecoveryWindow{ProbationSince: now}

	if err := mgr.reconcileApp(context.Background(), state, appInst); err != nil {
		t.Fatalf("disabled reconcile: %v", err)
	}
	if appInst.StartupAttempts != 0 || appInst.FirstStartupFailureAt != nil {
		t.Fatalf("disabled stop completion retained history: attempts=%d first=%v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if _, ok := mgr.startupRecovery[appInst.InstanceID]; ok {
		t.Fatal("disabled stop completion retained recovery window")
	}
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

func TestReconcile_AnchorStartAndStopFail_QuiescesSessionAndRecovers(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

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

	mock.startErrorForContainer = map[string]error{
		anchorCID: fmt.Errorf("podman start failed: conmon process killed"),
	}
	mock.stopError = fmt.Errorf("podman stop failed: conmon process killed")
	quiesceCalls := 0
	mgr.userSessionQuiescer = func(_ context.Context, instanceID string) error {
		quiesceCalls++
		if instanceID != appInst.InstanceID {
			t.Fatalf("quiesced instance = %q, want %q", instanceID, appInst.InstanceID)
		}
		return nil
	}

	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}
	if quiesceCalls != 1 {
		t.Fatalf("quiesce calls = %d, want 1", quiesceCalls)
	}
	if oldAnchorState, _ := mock.InspectContainerState(ctx, runtime, anchorCID); oldAnchorState.Exists {
		t.Fatal("old anchor should have been removed after PID 1 quiescence")
	}
	if oldMainState, _ := mock.InspectContainerState(ctx, runtime, mainCID); oldMainState.Exists {
		t.Fatal("old service should have been removed after PID 1 quiescence")
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
		t.Fatalf("observed status = %q, want %q", got, StatusRunning)
	}
}

func TestReconcile_StaleAnchorQuiescesSessionWithoutNoOpStart(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

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
	mock.containers[anchorCID].Status = "stale"
	mock.containers[mainCID].Status = "stale"

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusRunning,
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

	mock.stopError = fmt.Errorf("podman stop succeeded without changing stale metadata")
	quiesceCalls := 0
	mgr.userSessionQuiescer = func(context.Context, string) error {
		quiesceCalls++
		return nil
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("startup recovery attempt was rejected")
	}
	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}
	mgr.markStartupRecoverySucceeded(state, appInst)

	if quiesceCalls != 1 {
		t.Fatalf("quiesce calls = %d, want 1", quiesceCalls)
	}
	if appInst.StartupAttempts != 1 {
		t.Fatalf("startup attempts = %d, want one owned attempt", appInst.StartupAttempts)
	}
	if appInst.NetworkAnchorID == anchorCID || appInst.Containers["main"] == mainCID {
		t.Fatalf("stale container IDs survived recovery: anchor=%s main=%s", appInst.NetworkAnchorID, appInst.Containers["main"])
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
		t.Fatalf("observed status = %q, want %q", got, StatusRunning)
	}
}

func TestStartContainerGroupRecoversStaleContainerState(t *testing.T) {
	for _, staleRole := range []string{"anchor", "service"} {
		t.Run(staleRole, func(t *testing.T) {
			mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
			ctx := context.Background()
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
			if staleRole == "anchor" {
				mock.containers[anchorCID].Status = "stale"
			} else {
				mock.containers[mainCID].Status = "stale"
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

			if err := mgr.startContainerGroup(ctx, state, appInst, def, layout, runtime); err != nil {
				t.Fatalf("startContainerGroup: %v", err)
			}
			if old, _ := mock.InspectContainerState(ctx, runtime, anchorCID); old.Exists {
				t.Fatal("manual stale recovery retained old anchor")
			}
			if old, _ := mock.InspectContainerState(ctx, runtime, mainCID); old.Exists {
				t.Fatal("manual stale recovery retained old service")
			}
			if appInst.NetworkAnchorID == anchorCID || appInst.Containers["main"] == mainCID {
				t.Fatalf("manual stale recovery retained old IDs: anchor=%s service=%s", appInst.NetworkAnchorID, appInst.Containers["main"])
			}
			if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
				t.Fatalf("observed status = %q, want %q", got, StatusRunning)
			}
		})
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
