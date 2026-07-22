package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piccolod/internal/container"
	"piccolod/internal/events"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/state/paths"
)

func runtimeRecoveryPlanInput(mode PiccoloMode) TransitionPlanInput {
	return TransitionPlanInput{
		OperationKind:   TransitionOperationRuntimeRecovery,
		SourceKind:      TransitionSourceAutomaticRecovery,
		Mode:            mode,
		Enabled:         true,
		RuntimeChanging: true,
		Runtime:         TransitionRuntimePolicy{RecreatePolicy: "metadata_quarantine"},
		ResourceKeys: map[string]string{
			"original_runtime_root":   "/run/piccolo/podman/apps/demo",
			"quarantine_runtime_root": "/run/piccolo/podman/apps/demo" + runtimeQuarantineSuffix,
			"original_run_root":       "/run/piccolo/podman/app-demo-disk",
			"quarantine_run_root":     "/run/piccolo/podman/app-demo-disk" + runtimeQuarantineSuffix,
		},
	}
}

func TestRuntimeRecoveryPlannerIsNarrowServiceWorkspaceException(t *testing.T) {
	for _, mode := range []PiccoloMode{ModeService, ModeWorkspace} {
		plan, err := PlanInstalledAppTransition(runtimeRecoveryPlanInput(mode))
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		if plan.Review.ActionKind != TransitionActionDisabled {
			t.Fatalf("mode %s action = %s, want disabled", mode, plan.Review.ActionKind)
		}
	}

	manual := runtimeRecoveryPlanInput(ModeService)
	manual.SourceKind = TransitionSourceCurrentCommitted
	if _, err := PlanInstalledAppTransition(manual); !errors.Is(err, ErrTransitionPlanRejected) {
		t.Fatalf("manual source err = %v, want rejection", err)
	}
	dataChanging := runtimeRecoveryPlanInput(ModeService)
	dataChanging.Data.SnapshotRequired = true
	if _, err := PlanInstalledAppTransition(dataChanging); !errors.Is(err, ErrTransitionPlanRejected) {
		t.Fatalf("data-changing err = %v, want rejection", err)
	}
	unsupported := runtimeRecoveryPlanInput(PiccoloMode("database"))
	if _, err := PlanInstalledAppTransition(unsupported); !errors.Is(err, ErrTransitionPlanRejected) {
		t.Fatalf("unsupported mode err = %v, want rejection", err)
	}
}

func TestUnknownObservationWindowCountsOncePerPassAndVetoesSharedCause(t *testing.T) {
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetTaskPressureNormal(func() bool { return true })

	mgr.beginObservationPass()
	first := mgr.recordUnknownObservation("alpha", errors.New("podman unavailable"))
	duplicate := mgr.recordUnknownObservation("alpha", errors.New("podman unavailable"))
	if first.Count != 1 || duplicate.Count != 1 {
		t.Fatalf("same-pass counts = %d, %d; want one", first.Count, duplicate.Count)
	}

	mgr.unknownObservationMu.Lock()
	alpha := mgr.unknownObservations["alpha"]
	alpha.Count = persistentUnknownAttempts
	alpha.FirstAt = time.Now().Add(-persistentUnknownDuration - time.Second)
	mgr.unknownObservations["alpha"] = alpha
	mgr.unknownObservationMu.Unlock()
	if !mgr.unknownRecoveryEligible("alpha", alpha) {
		t.Fatal("isolated persistent Podman failure should be eligible")
	}

	mgr.unknownObservationMu.Lock()
	mgr.unknownObservations["beta"] = unknownObservationWindow{
		Cause:          observationCausePodmanControl,
		Count:          persistentUnknownAttempts,
		FirstAt:        time.Now().Add(-persistentUnknownDuration - time.Second),
		LastAt:         time.Now(),
		LastGeneration: mgr.observationGeneration,
	}
	mgr.unknownObservationMu.Unlock()
	if mgr.unknownRecoveryEligible("alpha", alpha) {
		t.Fatal("equivalent failure in another app must veto quarantine")
	}
}

func TestRuntimePressureRetainsIndependentWarningUntilBothCausesClear(t *testing.T) {
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	mgr.SetEventBus(bus)
	pressureEvents := bus.Subscribe(events.TopicResourcePressure, 8)

	mgr.beginObservationPass()
	mgr.recordUnknownObservation("alpha", errors.New("podman unavailable"))
	assertRuntimePressureEvent(t, pressureEvents, events.PressureSeverityWarn, "observation_unknown")

	mgr.SuppressAutomaticRecovery("alpha", "Automatic recovery paused")
	assertRuntimePressureEvent(t, pressureEvents, events.PressureSeverityWarn, "automatic_recovery_suppressed")

	mgr.clearUnknownObservation("alpha")
	assertRuntimePressureEvent(t, pressureEvents, events.PressureSeverityWarn, "automatic_recovery_suppressed")

	mgr.beginObservationPass()
	mgr.recordUnknownObservation("alpha", errors.New("podman unavailable"))
	assertRuntimePressureEvent(t, pressureEvents, events.PressureSeverityWarn, "automatic_recovery_suppressed")

	mgr.clearAutomaticRecoverySuppression("alpha")
	assertRuntimePressureEvent(t, pressureEvents, events.PressureSeverityWarn, "observation_unknown")

	mgr.clearUnknownObservation("alpha")
	assertRuntimePressureEvent(t, pressureEvents, events.PressureSeverityOK, "normal")
}

func TestRuntimePressureSnapshotCoalescesCausesPerApp(t *testing.T) {
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.beginObservationPass()
	mgr.recordUnknownObservation("alpha", errors.New("podman unavailable"))
	mgr.SuppressAutomaticRecovery("alpha", "Automatic recovery paused")

	snapshot := mgr.RuntimeObservationPressureSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("runtime pressure snapshot = %+v, want one event", snapshot)
	}
	if snapshot[0].ReasonCode != "automatic_recovery_suppressed" || snapshot[0].Severity != events.PressureSeverityWarn {
		t.Fatalf("runtime pressure snapshot = %+v", snapshot[0])
	}
}

func TestFailedManualStartDoesNotClearAutomaticRecoverySuppression(t *testing.T) {
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.SuppressAutomaticRecovery("missing", "Automatic recovery paused")

	err = mgr.Start(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "app instance not found") {
		t.Fatalf("Start error = %v, want app not found", err)
	}
	if !mgr.automaticRecoverySuppressed("missing") {
		t.Fatal("failed manual Start cleared automatic recovery suppression")
	}
}

func TestRuntimeRecoveryQuiescenceFailureRetainsLastKnownPublication(t *testing.T) {
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.serviceManager.UseInMemoryNetworkForTest()
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatal(err)
	}
	appInst := transitionTestAppInstance("piclu")
	if err := state.StoreApp(appInst); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, appInst.Definition.Listeners); err != nil {
		t.Fatalf("publish last-known route: %v", err)
	}
	mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
	proofErr := errors.New("PID 1 empty-cgroup proof unavailable")
	mgr.userSessionQuiescer = func(context.Context, string) error { return proofErr }

	err = mgr.recoverPersistentUnknownRuntime(context.Background(), state, appInst, appInst.Definition, appVolumeLayout{}, ModeService)
	if !errors.Is(err, proofErr) {
		t.Fatalf("runtime recovery error = %v, want %v", err, proofErr)
	}
	if !mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
		t.Fatal("failed empty-cgroup proof withdrew the last-known route")
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
		t.Fatalf("status after failed proof = %q, want retained %q", got, StatusRunning)
	}
}

func assertRuntimePressureEvent(t *testing.T, ch <-chan events.Event, severity, reason string) {
	t.Helper()
	select {
	case event := <-ch:
		payload, ok := event.Payload.(events.ResourcePressureEvent)
		if !ok {
			t.Fatalf("pressure payload type = %T", event.Payload)
		}
		if payload.Severity != severity || payload.ReasonCode != reason {
			t.Fatalf("pressure event = %+v, want severity=%s reason=%s", payload, severity, reason)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pressure event severity=%s reason=%s", severity, reason)
	}
}

func TestRuntimeRecoveryQuarantineIntentReplaysAfterRecordWriteFault(t *testing.T) {
	stateDir := t.TempDir()
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	allowHostStorage(t, mgr)
	runBase := filepath.Join(stateDir, "podman-run")
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", runBase)
	paths.SetPodmanRootForTest(t, filepath.Join(stateDir, "podman"))

	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatal(err)
	}
	appInst := transitionTestAppInstance("piclu")
	if err := state.StoreApp(appInst); err != nil {
		t.Fatal(err)
	}
	runtime := container.PodmanRuntime{
		Root:    paths.PodmanJoin("apps", "piclu"),
		RunRoot: filepath.Join(runBase, appVolumeID("piclu")),
	}
	for _, dir := range []string{runtime.Root, runtime.RunRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "old-metadata"), []byte("bad"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record, err := newRuntimeRecoveryRecord(appInst, ModeService, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
		t.Fatal(err)
	}

	fault := errors.New("injected transition write failure")
	state.storeTransitionRecordHook = func(_ string, next *TransitionRecord) error {
		if next.Phase == TransitionPhaseRuntimeQuarantined {
			return fault
		}
		return nil
	}
	if err := mgr.recoverRuntimeRecoveryTransition(context.Background(), state, appInst, record); !errors.Is(err, fault) {
		t.Fatalf("first recovery err = %v, want injected fault", err)
	}
	for _, original := range []string{runtime.Root, runtime.RunRoot} {
		if _, err := os.Stat(original); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("original %s err = %v, want quarantined", original, err)
		}
		if _, err := os.Stat(original + runtimeQuarantineSuffix); err != nil {
			t.Fatalf("quarantine %s: %v", original, err)
		}
	}

	loaded, err := state.LoadTransitionRecord(appInst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != TransitionPhaseRuntimeQuarantineIntent {
		t.Fatalf("durable phase = %s, want intent", loaded.Phase)
	}

	state.storeTransitionRecordHook = func(_ string, next *TransitionRecord) error {
		if next.Phase == TransitionPhaseRuntimeCleanCreated {
			return fault
		}
		return nil
	}
	if err := mgr.recoverRuntimeRecoveryTransition(context.Background(), state, appInst, loaded); !errors.Is(err, fault) {
		t.Fatalf("replay err = %v, want second injected fault", err)
	}
	loaded, err = state.LoadTransitionRecord(appInst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != TransitionPhaseRuntimeQuarantined {
		t.Fatalf("durable phase after replay = %s, want quarantined", loaded.Phase)
	}
	for _, original := range []string{runtime.Root, runtime.RunRoot} {
		if _, err := os.Stat(original); err != nil {
			t.Fatalf("clean runtime %s was not recreated: %v", original, err)
		}
		if _, err := os.Stat(filepath.Join(original+runtimeQuarantineSuffix, "old-metadata")); err != nil {
			t.Fatalf("old quarantine was not retained: %v", err)
		}
	}

	// Replay through clean-runtime creation, then fail the record write after
	// the replacement containers have started. The durable clean-created phase
	// must be sufficient to re-observe the running group without creating a
	// second quarantine or losing the retained one.
	state.storeTransitionRecordHook = func(_ string, next *TransitionRecord) error {
		if next.Phase == TransitionPhaseRuntimeGroupCommitted {
			return fault
		}
		return nil
	}
	if err := mgr.recoverRuntimeRecoveryTransition(context.Background(), state, appInst, loaded); !errors.Is(err, fault) {
		t.Fatalf("post-container-start recovery err = %v, want injected fault", err)
	}
	loaded, err = state.LoadTransitionRecord(appInst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != TransitionPhaseRuntimeCleanCreated {
		t.Fatalf("durable phase after container start = %s, want clean-created", loaded.Phase)
	}
	if appInst.NetworkAnchorID == "" || len(appInst.Containers) == 0 {
		t.Fatalf("replacement group was not persisted before fault: anchor=%q containers=%v", appInst.NetworkAnchorID, appInst.Containers)
	}
	for _, original := range []string{runtime.Root, runtime.RunRoot} {
		if _, err := os.Stat(filepath.Join(original+runtimeQuarantineSuffix, "old-metadata")); err != nil {
			t.Fatalf("quarantine disappeared before group commit: %v", err)
		}
	}

	// The next replay records the group commit, removes the quarantine, and
	// then loses the committed record write. Durable group-committed must make
	// that cleanup idempotent on the following process start.
	state.storeTransitionRecordHook = func(_ string, next *TransitionRecord) error {
		if next.Phase == TransitionPhaseCommitted {
			return fault
		}
		return nil
	}
	if err := mgr.recoverRuntimeRecoveryTransition(context.Background(), state, appInst, loaded); !errors.Is(err, fault) {
		t.Fatalf("post-cleanup recovery err = %v, want injected fault", err)
	}
	loaded, err = state.LoadTransitionRecord(appInst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != TransitionPhaseRuntimeGroupCommitted {
		t.Fatalf("durable phase after cleanup = %s, want group-committed", loaded.Phase)
	}
	for _, original := range []string{runtime.Root, runtime.RunRoot} {
		if _, err := os.Stat(original + runtimeQuarantineSuffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("quarantine %s survived committed cleanup: %v", original, err)
		}
	}

	state.storeTransitionRecordHook = nil
	if err := mgr.recoverRuntimeRecoveryTransition(context.Background(), state, appInst, loaded); err != nil {
		t.Fatalf("final idempotent cleanup replay: %v", err)
	}
	if _, err := state.LoadTransitionRecord(appInst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record after final cleanup = %v, want not-exist", err)
	}
}

func TestRuntimeRecoveryWarningStopsAtNextDurablePhase(t *testing.T) {
	stateDir := t.TempDir()
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	allowHostStorage(t, mgr)
	runBase := filepath.Join(stateDir, "podman-run")
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", runBase)
	paths.SetPodmanRootForTest(t, filepath.Join(stateDir, "podman"))

	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatal(err)
	}
	appInst := transitionTestAppInstance("piclu")
	if err := state.StoreApp(appInst); err != nil {
		t.Fatal(err)
	}
	runtime := container.PodmanRuntime{
		Root:    paths.PodmanJoin("apps", "piclu"),
		RunRoot: filepath.Join(runBase, appVolumeID("piclu")),
	}
	for _, dir := range []string{runtime.Root, runtime.RunRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	record, err := newRuntimeRecoveryRecord(appInst, ModeService, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
		t.Fatal(err)
	}

	pressure.DefaultAdmission.Fence()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)
	ctx := withTransitionRecoveryAdmission(context.Background())
	blocked := mgr.recoverPendingTransitionRecords(ctx, state)
	if blocked[appInst.InstanceID] {
		t.Fatalf("runtime recovery blocked: %v", blocked)
	}
	loaded, err := state.LoadTransitionRecord(appInst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != TransitionPhaseRuntimeQuarantined {
		t.Fatalf("phase = %s, want exactly next durable phase %s", loaded.Phase, TransitionPhaseRuntimeQuarantined)
	}
	for _, original := range []string{runtime.Root, runtime.RunRoot} {
		if _, err := os.Stat(original); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("clean runtime %s was created beyond the authorized phase: %v", original, err)
		}
		if _, err := os.Stat(original + runtimeQuarantineSuffix); err != nil {
			t.Fatalf("quarantine %s: %v", original, err)
		}
	}
}

func TestRuntimeRecoveryPathValidationRejectsSymlinkAncestor(t *testing.T) {
	base := t.TempDir()
	paths.SetPodmanRootForTest(t, filepath.Join(base, "podman"))
	runBase := filepath.Join(base, "run")
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", runBase)
	if err := os.MkdirAll(paths.PodmanRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, paths.PodmanJoin("apps")); err != nil {
		t.Fatal(err)
	}
	root := paths.PodmanJoin("apps", "demo")
	runRoot := filepath.Join(runBase, "app-demo-disk")
	if err := validateRuntimeRecoveryPaths(root, root+runtimeQuarantineSuffix, runRoot, runRoot+runtimeQuarantineSuffix); err == nil {
		t.Fatal("symlink ancestor should be rejected")
	}
}
