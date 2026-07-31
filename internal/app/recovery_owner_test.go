package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/state/paths"
)

func TestDesiredRecoveryAppOwnersUsesDurableEnabledOrderWithoutMutation(t *testing.T) {
	mgr, _, state := newRecoveryOwnerTestManager(t)
	storeRecoveryOwnerApp(t, state, "zulu", true, nil)
	storeRecoveryOwnerApp(t, state, "alpha", true, recoveryOwnerWorkspaceDefinition("alpha"))
	storeRecoveryOwnerApp(t, state, "disabled", false, nil)

	owners, err := mgr.DesiredRecoveryAppOwners(context.Background())
	if err != nil {
		t.Fatalf("desired owners: %v", err)
	}
	if want := []DesiredAppRecoveryOwner{
		{InstanceID: "alpha", RouteBearing: false},
		{InstanceID: "zulu", RouteBearing: true},
	}; !reflect.DeepEqual(owners, want) {
		t.Fatalf("owners = %v, want %v", owners, want)
	}
	for instanceID, wantEnabled := range map[string]bool{"alpha": true, "zulu": true, "disabled": false} {
		appInst, ok := state.GetApp(instanceID)
		if !ok || appInst.Enabled != wantEnabled {
			t.Fatalf("durable desire for %s mutated: %+v", instanceID, appInst)
		}
	}
}

func TestDesiredRecoveryAppOwnersLockAcquisitionHonorsDeadline(t *testing.T) {
	mgr, _, _ := newRecoveryOwnerTestManager(t)
	release, err := mgr.lifecycleGate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	owners, err := mgr.DesiredRecoveryAppOwners(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || owners != nil {
		t.Fatalf("owners=%v err=%v, want nil/%v", owners, err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("deadline-aware lock returned after %s", elapsed)
	}
}

func TestLifecycleAdmissionCancelsQueuedRequestButNotAdmittedExecution(t *testing.T) {
	mgr, _, _ := newRecoveryOwnerTestManager(t)
	held, err := mgr.lifecycleGate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	executionCtx, cancelExecution := context.WithTimeout(context.Background(), time.Second)
	defer cancelExecution()
	ctx := WithLifecycleAdmissionContext(executionCtx, requestCtx)
	queued := make(chan error, 1)
	go func() {
		_, err := mgr.DesiredRecoveryAppOwners(ctx)
		queued <- err
	}()
	cancelRequest()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued admission error = %v, want context canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("queued lifecycle admission ignored request cancellation")
	}
	held()

	requestCtx, cancelRequest = context.WithCancel(context.Background())
	ctx = WithLifecycleAdmissionContext(executionCtx, requestCtx)
	admittedCtx, release, err := mgr.acquireLifecycle(ctx)
	if err != nil {
		t.Fatalf("initial admission: %v", err)
	}
	cancelRequest()
	release()

	// Consumption is shared with the original operation context, so both the
	// admitted child and a compensating operation that reuses the parent's
	// context no longer follow request cancellation.
	_, release, err = mgr.acquireLifecycle(admittedCtx)
	if err != nil {
		t.Fatalf("post-admission execution inherited request cancellation: %v", err)
	}
	release()
	_, release, err = mgr.acquireLifecycle(ctx)
	if err != nil {
		t.Fatalf("compensating acquisition on original operation context inherited request cancellation: %v", err)
	}
	release()
}

func TestLifecycleShutdownFenceJoinsOwnerAndRejectsQueuedAndLateWork(t *testing.T) {
	mgr, _, _ := newRecoveryOwnerTestManager(t)
	held, err := mgr.lifecycleGate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		release func()
		err     error
	}
	shutdown := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := mgr.lifecycleGate.fenceAndAcquire(ctx)
		shutdown <- result{release: release, err: err}
	}()

	deadline := time.Now().Add(time.Second)
	for !mgr.lifecycleGate.isFenced() {
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not fence lifecycle admission")
		}
		time.Sleep(time.Millisecond)
	}
	held()
	got := <-shutdown
	if got.err != nil {
		t.Fatalf("shutdown join: %v", got.err)
	}
	got.release()

	if release, err := mgr.lifecycleGate.acquire(context.Background()); release != nil || !errors.Is(err, errLifecycleAdmissionFenced) {
		t.Fatalf("late admission = (%v, %v), want fenced", release != nil, err)
	}
}

func TestAutomaticReconcileYieldsWhenLifecycleGateBusy(t *testing.T) {
	mgr, _, _ := newRecoveryOwnerTestManager(t)
	held, err := mgr.lifecycleGate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	started := time.Now()
	mgr.ReconcileOnce(context.Background())
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("automatic reconcile waited %s for busy lifecycle gate", elapsed)
	}
}

func TestAutomaticReconcileIntervalStartsAfterPassCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan time.Time, 1)
	passCount := 0
	go runCompletionRelativeReconcileLoop(ctx, 40*time.Millisecond, func(context.Context) {
		passCount++
		switch passCount {
		case 1:
			close(firstStarted)
			<-releaseFirst
		case 2:
			secondStarted <- time.Now()
		}
	})

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first automatic reconcile pass did not start")
	}
	time.Sleep(60 * time.Millisecond)
	firstCompleted := time.Now()
	close(releaseFirst)

	select {
	case started := <-secondStarted:
		if gap := started.Sub(firstCompleted); gap < 25*time.Millisecond {
			t.Fatalf("next pass started %s after prior completion; interval was measured while pass ran", gap)
		}
	case <-time.After(time.Second):
		t.Fatal("second automatic reconcile pass did not start")
	}
}

func TestRecoverDesiredAppTouchesExactlyOneOwnerAndPublishesRoute(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	alpha := createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", true)
	beta := createRecoveryOwnerRuntime(t, mgr, mock, state, "beta", false)

	// A pending record owned by beta proves the one-app API does not invoke the
	// bulk transition recovery scan as a hidden prelude.
	if err := state.StoreTransitionRecord("beta", &TransitionRecord{Phase: TransitionPhaseCommitted}); err != nil {
		t.Fatalf("store beta transition: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := mgr.RecoverDesiredApp(ctx, "alpha")
	if err != nil {
		t.Fatalf("recover alpha: %v", err)
	}
	if !result.Recovered || !result.RouteBearing || !result.ActivePublication || !result.StabilityProven() {
		t.Fatalf("alpha result = %+v, want active publication proof", result)
	}
	if !mgr.serviceManager.AppPublicationActive("alpha") {
		t.Fatal("alpha listener is not actively published")
	}
	if mgr.serviceManager.AppPublicationActive("beta") {
		t.Fatal("one-app recovery published beta")
	}
	if betaAnchor := mock.containers[beta.NetworkAnchorID]; betaAnchor == nil || betaAnchor.Status != "created" {
		t.Fatalf("beta anchor changed = %+v, want untouched created state", betaAnchor)
	}
	if alphaAnchor := mock.containers[alpha.NetworkAnchorID]; alphaAnchor == nil || alphaAnchor.Status != "running" {
		t.Fatalf("alpha anchor changed = %+v, want running", alphaAnchor)
	}
	if _, err := state.LoadTransitionRecord("beta"); err != nil {
		t.Fatalf("beta transition was consumed by alpha attempt: %v", err)
	}
}

func TestRecoverDesiredAppRepairsIncompleteMultiListenerBindingsBeforeStability(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	def := recoveryOwnerDefinition("alpha")
	def.Listeners = append(def.Listeners, api.AppListener{
		Name:      "admin",
		GuestPort: 9090,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	})
	service := def.Services["main"]
	service.BindPorts = []int{8080, 9090}
	def.Services["main"] = service
	appInst := createRecoveryOwnerRuntimeWithDefinition(t, mgr, mock, state, "alpha", true, def)
	originalAnchorID := appInst.NetworkAnchorID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := mgr.RecoverDesiredApp(ctx, "alpha")
	if err != nil {
		t.Fatalf("recover incomplete listener set: %v", err)
	}
	if !result.StabilityProven() || !result.ActivePublication {
		t.Fatalf("recovery result = %+v, want complete active publication", result)
	}
	recovered, ok := state.GetApp("alpha")
	if !ok || recovered == nil {
		t.Fatal("recovered app metadata missing")
	}
	if recovered.NetworkAnchorID == originalAnchorID {
		t.Fatal("incomplete Podman binding set was accepted without container recreation")
	}
	anchor := mock.containers[recovered.NetworkAnchorID]
	if anchor == nil || len(anchor.Spec.Ports) != len(def.Listeners) {
		t.Fatalf("replacement anchor ports = %+v, want %d complete bindings", anchor, len(def.Listeners))
	}
	endpoints, err := mgr.serviceManager.GetByApp("alpha")
	if err != nil || len(endpoints) != len(def.Listeners) {
		t.Fatalf("published endpoints = %v, err=%v; want %d", endpoints, err, len(def.Listeners))
	}
}

func TestRecoverDesiredListenerlessWorkspaceProvesRecoveryWithoutPublication(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	createRecoveryOwnerRuntimeWithDefinition(t, mgr, mock, state, "workspace", true, recoveryOwnerWorkspaceDefinition("workspace"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := mgr.RecoverDesiredApp(ctx, "workspace")
	if err != nil {
		t.Fatalf("recover listenerless workspace: %v", err)
	}
	if !result.Recovered || result.RouteBearing || result.ActivePublication || !result.StabilityProven() {
		t.Fatalf("listenerless recovery result = %+v, want stable recovery without publication", result)
	}
	if mgr.serviceManager.AppPublicationActive("workspace") {
		t.Fatal("listenerless workspace manufactured an active publication")
	}

	mock.listError = errors.New("podman observation unavailable")
	unknownCtx, unknownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer unknownCancel()
	unknown, err := mgr.RecoverDesiredApp(unknownCtx, "workspace")
	if !errors.Is(err, ErrRecoveryObservationUnknown) {
		t.Fatalf("listenerless unknown observation error = %v, want %v", err, ErrRecoveryObservationUnknown)
	}
	if unknown.Recovered || unknown.ActivePublication || unknown.StabilityProven() {
		t.Fatalf("listenerless unknown observation returned recovery proof: %+v", unknown)
	}
}

func TestRecoverDesiredAppUnknownReturnsNoNewProofAndRetainsLastKnownRoute(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	first, err := mgr.RecoverDesiredApp(ctx, "alpha")
	cancel()
	if err != nil || !first.StabilityProven() || !first.ActivePublication {
		t.Fatalf("initial recovery = %+v, err=%v", first, err)
	}

	mock.listError = errors.New("podman temporarily unavailable")
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	unknown, err := mgr.RecoverDesiredApp(ctx, "alpha")
	cancel()
	if !errors.Is(err, ErrRecoveryObservationUnknown) {
		t.Fatalf("unknown error = %v, want %v", err, ErrRecoveryObservationUnknown)
	}
	if unknown.Recovered || unknown.ActivePublication || unknown.StabilityProven() {
		t.Fatalf("unknown attempt returned active proof: %+v", unknown)
	}
	if !mgr.serviceManager.AppPublicationActive("alpha") {
		t.Fatal("safe unknown observation withdrew a previously proven active route")
	}
}

func TestObserveDesiredAppRecoveryActiveFailsClosedOnLostRuntimeProof(t *testing.T) {
	newActiveOwner := func(t *testing.T) (*AppManager, *FilesystemStateManager) {
		t.Helper()
		mgr, mock, state := newRecoveryOwnerTestManager(t)
		createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := mgr.RecoverDesiredApp(ctx, "alpha")
		if err != nil || !result.StabilityProven() {
			t.Fatalf("initial recovery = %+v, err=%v", result, err)
		}
		return mgr, state
	}

	t.Run("running publication is active", func(t *testing.T) {
		mgr, _ := newActiveOwner(t)
		active, err := mgr.ObserveDesiredAppRecoveryActive(context.Background(), "alpha")
		if err != nil || !active {
			t.Fatalf("active=%v err=%v, want true/nil", active, err)
		}
	})

	t.Run("status loss", func(t *testing.T) {
		mgr, _ := newActiveOwner(t)
		mgr.setObservedStatus("alpha", StatusStopped)
		active, err := mgr.ObserveDesiredAppRecoveryActive(context.Background(), "alpha")
		if err != nil || active {
			t.Fatalf("active=%v err=%v, want false/nil", active, err)
		}
	})

	t.Run("publication loss", func(t *testing.T) {
		mgr, _ := newActiveOwner(t)
		mgr.serviceManager.DeactivateApp("alpha")
		active, err := mgr.ObserveDesiredAppRecoveryActive(context.Background(), "alpha")
		if err != nil || active {
			t.Fatalf("active=%v err=%v, want false/nil", active, err)
		}
	})

	t.Run("unknown observation", func(t *testing.T) {
		mgr, _ := newActiveOwner(t)
		mgr.unknownObservationMu.Lock()
		mgr.unknownObservations["alpha"] = unknownObservationWindow{Cause: observationCausePodmanControl, Count: 1}
		mgr.unknownObservationMu.Unlock()
		active, err := mgr.ObserveDesiredAppRecoveryActive(context.Background(), "alpha")
		if active || !errors.Is(err, ErrRecoveryObservationUnknown) {
			t.Fatalf("active=%v err=%v, want false/%v", active, err, ErrRecoveryObservationUnknown)
		}
	})

	t.Run("owner no longer desired", func(t *testing.T) {
		mgr, state := newActiveOwner(t)
		if err := state.UpdateAppEnabled("alpha", false); err != nil {
			t.Fatal(err)
		}
		active, err := mgr.ObserveDesiredAppRecoveryActive(context.Background(), "alpha")
		if active || !errors.Is(err, ErrRecoveryAppNotDesired) {
			t.Fatalf("active=%v err=%v, want false/%v", active, err, ErrRecoveryAppNotDesired)
		}
	})

	t.Run("busy lifecycle is unknown", func(t *testing.T) {
		mgr, _ := newActiveOwner(t)
		release, err := mgr.lifecycleGate.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		active, err := mgr.ObserveDesiredAppRecoveryActive(context.Background(), "alpha")
		release()
		if active || !errors.Is(err, ErrRecoveryObservationUnknown) {
			t.Fatalf("active=%v err=%v, want false/%v", active, err, ErrRecoveryObservationUnknown)
		}
	})
}

func TestRecoverDesiredAppRequiresFiniteLiveContext(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	appInst := createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", false)
	before := mock.containers[appInst.NetworkAnchorID].Status

	if result, err := mgr.RecoverDesiredApp(context.Background(), "alpha"); !errors.Is(err, ErrRecoveryDeadlineRequired) || result.Recovered || result.ActivePublication {
		t.Fatalf("unbounded recovery = %+v, err=%v", result, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	if result, err := mgr.RecoverDesiredApp(ctx, "alpha"); !errors.Is(err, context.Canceled) || result.Recovered || result.ActivePublication {
		t.Fatalf("canceled recovery = %+v, err=%v", result, err)
	}
	if anchor := mock.containers[appInst.NetworkAnchorID]; anchor == nil || anchor.Status != before {
		t.Fatalf("rejected recovery changed anchor from %q: %+v", before, anchor)
	}
}

func TestRecoverDesiredAppRechecksCancellationAfterSerializationWait(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	appInst := createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", false)
	before := mock.containers[appInst.NetworkAnchorID].Status

	release, err := mgr.lifecycleGate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	type outcome struct {
		result AppRecoveryResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := mgr.RecoverDesiredApp(ctx, "alpha")
		done <- outcome{result: result, err: err}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(250 * time.Millisecond):
		release()
		t.Fatal("deadline-bound recovery did not return while lifecycle lock remained held")
	}
	release()
	if !errors.Is(got.err, context.DeadlineExceeded) || got.result.Recovered || got.result.ActivePublication {
		t.Fatalf("serialized cancellation = %+v, err=%v", got.result, got.err)
	}
	if anchor := mock.containers[appInst.NetworkAnchorID]; anchor == nil || anchor.Status != before {
		t.Fatalf("canceled waiter changed anchor from %q: %+v", before, anchor)
	}
}

func TestRecoverDesiredAppHonorsLifecycleAdmission(t *testing.T) {
	pressure.DefaultAdmission.ResetForTest()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	appInst := createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", false)
	before := mock.containers[appInst.NetworkAnchorID].Status
	pressure.DefaultAdmission.Fence()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := mgr.RecoverDesiredApp(ctx, "alpha")
	if !pressure.IsAdmissionError(err) || result.Recovered || result.ActivePublication {
		t.Fatalf("fenced recovery = %+v, err=%v", result, err)
	}
	if anchor := mock.containers[appInst.NetworkAnchorID]; anchor == nil || anchor.Status != before {
		t.Fatalf("fenced recovery changed anchor from %q: %+v", before, anchor)
	}
}

func TestRecoverDesiredAppFailureDoesNotClaimPublication(t *testing.T) {
	mgr, mock, state := newRecoveryOwnerTestManager(t)
	createRecoveryOwnerRuntime(t, mgr, mock, state, "alpha", false)
	mock.startError = errors.New("start failed")
	mock.createError = errors.New("recreate failed")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := mgr.RecoverDesiredApp(ctx, "alpha")
	if err == nil {
		t.Fatal("failed runtime recovery returned nil error")
	}
	if result.Recovered || result.ActivePublication || mgr.serviceManager.AppPublicationActive("alpha") {
		t.Fatalf("failed recovery claimed publication: %+v", result)
	}
}

func newRecoveryOwnerTestManager(t *testing.T) (*AppManager, *MockContainerManager, *FilesystemStateManager) {
	t.Helper()
	root := t.TempDir()
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, root)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	paths.SetPodmanRootForTest(t, filepath.Join(root, "podman"))
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", filepath.Join(root, "runroot"))
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	return mgr, mock, state
}

func storeRecoveryOwnerApp(t *testing.T, state *FilesystemStateManager, instanceID string, enabled bool, def *api.AppDefinition) *AppInstance {
	t.Helper()
	if def == nil {
		def = recoveryOwnerDefinition(instanceID)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     instanceID,
		Enabled:        enabled,
		PrimaryService: "main",
		Containers:     map[string]string{},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app %s: %v", instanceID, err)
	}
	return appInst
}

func recoveryOwnerDefinition(instanceID string) *api.AppDefinition {
	def := &api.AppDefinition{
		Listeners: []api.AppListener{{
			Name:      instanceID,
			GuestPort: 8080,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
		}},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)
	return def
}

func recoveryOwnerWorkspaceDefinition(instanceID string) *api.AppDefinition {
	def := &api.AppDefinition{
		WorkspaceName:  instanceID,
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest"},
		},
		Extensions: map[string]interface{}{"mode": "workspace"},
	}
	SetDefaults(def)
	return def
}

func createRecoveryOwnerRuntime(t *testing.T, mgr *AppManager, mock *MockContainerManager, state *FilesystemStateManager, instanceID string, running bool) *AppInstance {
	t.Helper()
	return createRecoveryOwnerRuntimeWithDefinition(t, mgr, mock, state, instanceID, running, recoveryOwnerDefinition(instanceID))
}

func createRecoveryOwnerRuntimeWithDefinition(t *testing.T, mgr *AppManager, mock *MockContainerManager, state *FilesystemStateManager, instanceID string, running bool, def *api.AppDefinition) *AppInstance {
	t.Helper()
	ctx := context.Background()
	layout, err := mgr.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		t.Fatalf("ensure layout %s: %v", instanceID, err)
	}
	runtime, err := mgr.podmanRuntimeForApp(ctx, instanceID, layout, piccoloModeFromExtensions(def.Extensions), appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("ensure runtime %s: %v", instanceID, err)
	}
	anchorPorts := []container.PortMapping(nil)
	if len(def.Listeners) > 0 {
		anchorPorts = []container.PortMapping{{Host: 20000 + int(instanceID[0]), Container: 8080, Protocol: "tcp"}}
	}
	anchorID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   networkAnchorContainerName(instanceID),
		Labels: piccoloLabels(instanceID, "", "anchor"),
		Ports:  anchorPorts,
	})
	if err != nil {
		t.Fatalf("create anchor %s: %v", instanceID, err)
	}
	serviceID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   containerNameForService(instanceID, "main", "main"),
		Labels: piccoloLabels(instanceID, "main", "service"),
	})
	if err != nil {
		t.Fatalf("create service %s: %v", instanceID, err)
	}
	if running {
		if err := mock.StartContainer(ctx, runtime, anchorID); err != nil {
			t.Fatalf("start anchor %s: %v", instanceID, err)
		}
		if err := mock.StartContainer(ctx, runtime, serviceID); err != nil {
			t.Fatalf("start service %s: %v", instanceID, err)
		}
	}
	appInst := storeRecoveryOwnerApp(t, state, instanceID, true, def)
	appInst.NetworkAnchorID = anchorID
	appInst.Containers = map[string]string{"main": serviceID}
	if err := state.StoreAppMetadata(appInst); err != nil {
		t.Fatalf("store runtime metadata %s: %v", instanceID, err)
	}
	return appInst
}
