package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"piccolod/internal/api"
)

func TestIsDigestPinned(t *testing.T) {
	tests := []struct {
		img  string
		want bool
	}{
		{"alpine:3.18", false},
		{"alpine", false},
		{"docker.io/library/nginx:1.25", false},
		{"ghcr.io/my-org/my-app:v1.0", false},
		{"localhost:5000/myapp:v1", false},
		{"nginx@sha256:abc123def456", true},
		{"nginx:1.25@sha256:abc123def456", true},
		{"docker.io/library/nginx@sha256:abc123", true},
	}
	for _, tt := range tests {
		t.Run(tt.img, func(t *testing.T) {
			if got := isDigestPinned(tt.img); got != tt.want {
				t.Errorf("isDigestPinned(%q) = %v, want %v", tt.img, got, tt.want)
			}
		})
	}
}

func TestUpdateImage_WorkspaceMode_Blocked(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_ws_blocked")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "wsapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "ubuntu:22.04", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{
			"mode":           "workspace",
			"workspace_name": "wsapp",
		},
	}
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected error for workspace-mode update")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInstall_MultiServiceMode_RequiresRootfs(t *testing.T) {
	tmp, err := os.MkdirTemp("", "install_multi_rootfs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	// Clear rootfs manager to test the "not configured" error path.
	mgr.SetRootfsManager(nil)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := &api.AppDefinition{
		Type:           "user",
		PrimaryService: "web",
		Listeners:      []api.AppListener{{Name: "multiapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"web":    {Image: "nginx:1.25", BindPorts: []int{80}},
			"worker": {Image: "python:3.12", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	// Install requires rootfs volume manager (block-native architecture).
	_, err = mgr.Install(ctx, def)
	if err == nil {
		t.Fatal("expected error: rootfs manager not configured")
	}
	if !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateImage_PersistentDataRequiresRollbackSnapshotSupport(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_requires_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	blockedReason := mgr.ImageUpdateBlockedReason(ctx, inst.InstanceID)
	if !strings.Contains(blockedReason, "rollback snapshot required") {
		t.Fatalf("blocked reason = %q, want rollback snapshot requirement", blockedReason)
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected persistent-data image refresh to require snapshot support")
	}
	if !errors.Is(err, ErrImageUpdateRejected) {
		t.Fatalf("error = %v, want %v", err, ErrImageUpdateRejected)
	}
	if !strings.Contains(err.Error(), "rollback snapshot required") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertAppContainersRunning(t, mock, inst)
}

func TestUpdateImage_PersistentDataSnapshotFailureRestartsPreviousRuntime(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_snapshot_failure")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
		snapshotErr:       errors.New("thin pool exhausted"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected snapshot failure to abort image refresh")
	}
	if !strings.Contains(err.Error(), "thin pool exhausted") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(volumes.snapshots) != 1 {
		t.Fatalf("snapshot attempts = %d, want 1", len(volumes.snapshots))
	}
	assertAppContainersRunning(t, mock, inst)
}

func TestUpdateImage_RoutesAroundStaleRollbackSnapshotLV(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_stale_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	volumes.artifacts = []string{DataSnapshotLVName(inst.InstanceID, 1)}

	if err := mgr.UpdateImage(ctx, inst.InstanceID); err != nil {
		t.Fatalf("UpdateImage: %v", err)
	}
	if got, want := volumes.snapshots, []string{DataSnapshotLVName(inst.InstanceID, 2)}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("snapshots = %v, want %v", got, want)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	ts, err := state.LoadTupleState(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadTupleState: %v", err)
	}
	if ts == nil {
		t.Fatal("tuple state missing")
	}
	if ts.NextGenNumber < 4 {
		t.Fatalf("NextGenNumber = %d, want advanced past skipped gen1 and active gen", ts.NextGenNumber)
	}
	if snap := ts.LatestSnapshot(); snap == nil || snap.DataSnapshot != DataSnapshotLVName(inst.InstanceID, 2) {
		if snap == nil {
			t.Fatal("latest snapshot missing")
		}
		t.Fatalf("latest snapshot = %q, want %q", snap.DataSnapshot, DataSnapshotLVName(inst.InstanceID, 2))
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestUpdateImage_FinalSnapshotViabilityRunsBeforeStop(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_final_viability")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	viabilityCalls := 0
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
		viabilityHook: func(instanceID string) error {
			viabilityCalls++
			if viabilityCalls == 2 {
				return errors.New("thin pool changed after staging")
			}
			return nil
		},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected final viability failure")
	}
	if !errors.Is(err, ErrImageUpdateRejected) {
		t.Fatalf("error = %v, want %v", err, ErrImageUpdateRejected)
	}
	if !strings.Contains(err.Error(), "thin pool changed after staging") {
		t.Fatalf("unexpected error: %v", err)
	}
	if viabilityCalls != 2 {
		t.Fatalf("viability calls = %d, want 2", viabilityCalls)
	}
	if len(volumes.snapshots) != 0 {
		t.Fatalf("snapshots = %v, want none before stop", volumes.snapshots)
	}
	assertAppContainersRunning(t, mock, inst)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestUpdateImage_StoresRollbackPlanBeforeSnapshotAttempt(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_plan_before_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
		snapshotErr:       errors.New("snapshot create failed"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var phases []string
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID {
			phases = append(phases, txn.Phase)
		}
		return nil
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected snapshot failure")
	}
	if len(phases) < 2 {
		t.Fatalf("stored phases = %v, want planned and runtime switch markers", phases)
	}
	if phases[0] != imageUpdatePhaseSnapshotPlanned {
		t.Fatalf("first stored phase = %q, want %q", phases[0], imageUpdatePhaseSnapshotPlanned)
	}
	if !slices.Contains(phases, imageUpdatePhaseRuntimeSwitch) {
		t.Fatalf("stored phases = %v, want runtime switch marker before snapshot attempt", phases)
	}
	assertAppContainersRunning(t, mock, inst)
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want cleared after pre-candidate abort", err)
	}
	state.storeImageUpdateTransactionHook = nil
	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("rollbacks after reconcile = %v, want no recovery after pre-candidate abort", volumes.rollbacks)
	}
}

func TestUpdateImage_TuplePersistFailureWithSnapshotCleanupClearsTransaction(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_tuple_persist_cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failSnapshotTuple := true
	state.storeTupleStateHook = func(instanceID string, ts *TupleState) error {
		if instanceID == inst.InstanceID && failSnapshotTuple && ts.LatestSnapshot() != nil && ts.ActiveGeneration() == nil {
			failSnapshotTuple = false
			return errors.New("tuple write failed")
		}
		return nil
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "persist tuple state") {
		t.Fatalf("UpdateImage err = %v, want tuple persist failure", err)
	}
	if len(volumes.destroyed) != 1 {
		t.Fatalf("destroyed snapshots = %v, want cleaned snapshot", volumes.destroyed)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist after cleanup", err)
	}
	state.storeTupleStateHook = nil
	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("rollbacks = %v, want no second recovery after cleaned snapshot failure", volumes.rollbacks)
	}
	recovered, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("app missing")
	}
	assertAppContainersRunning(t, mock, recovered)
}

func TestImageUpdateRecovery_PreCandidateCleanupFailureDoesNotStopRuntime(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_precandidate_cleanup_failure")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
		snapshotErr:       errors.New("snapshot create failed"),
		destroyErr:        errors.New("destroy failed"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	originalAnchor := inst.NetworkAnchorID
	originalContainers := cloneStringMap(inst.Containers)

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "snapshot create failed") {
		t.Fatalf("UpdateImage err = %v, want original snapshot create failure", err)
	}
	if strings.Contains(err.Error(), "destroy failed") {
		t.Fatalf("UpdateImage err = %v, want retained cleanup failure to stay out of user-facing result", err)
	}
	if len(volumes.destroyed) != 1 {
		t.Fatalf("destroyed snapshots = %v, want one retained cleanup attempt", volumes.destroyed)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want cleared after retained cleanup failure", err)
	}
	assertAppContainersRunning(t, mock, inst)

	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("rollbacks after pre-candidate cleanup retry = %v, want none", volumes.rollbacks)
	}
	if originalAnchor != "" {
		assertMockContainerRunning(t, mock, "original anchor", originalAnchor)
	}
	for svcName, cid := range originalContainers {
		assertMockContainerRunning(t, mock, "original "+svcName, cid)
	}
}

func TestUpdateImage_CommitIntentStoreFailureWaitsForRecovery(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_commit_intent_store")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failCommitIntent := true
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID && txn.Phase == imageUpdatePhaseCommitIntent && failCommitIntent {
			failCommitIntent = false
			return errors.New("disk write failed")
		}
		return nil
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected commit intent persistence failure")
	}
	if !strings.Contains(err.Error(), "commit intent") {
		t.Fatalf("unexpected error: %v", err)
	}
	for svc, cid := range inst.Containers {
		if c := mock.containers[cid]; c != nil && c.Status == "running" {
			t.Fatalf("%s container %s is running before recovery; want stopped to avoid post-snapshot writes", svc, cid)
		}
	}
	txn, err := state.LoadImageUpdateTransaction(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadImageUpdateTransaction: %v", err)
	}
	if txn.Phase != imageUpdatePhaseCandidateDataRisk {
		t.Fatalf("phase = %q, want %q", txn.Phase, imageUpdatePhaseCandidateDataRisk)
	}

	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want one data restore", volumes.rollbacks)
	}
	restored, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("restored app missing")
	}
	assertAppContainersRunning(t, mock, restored)
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestStart_RecoversPendingImageUpdateBeforeStarting(t *testing.T) {
	tmp, err := os.MkdirTemp("", "start_recovers_image_update")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failCommitIntent := true
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID && txn.Phase == imageUpdatePhaseCommitIntent && failCommitIntent {
			failCommitIntent = false
			return errors.New("disk write failed")
		}
		return nil
	}
	if err := mgr.UpdateImage(ctx, inst.InstanceID); err == nil {
		t.Fatal("expected commit intent persistence failure")
	}
	state.storeImageUpdateTransactionHook = nil

	if err := mgr.Start(ctx, inst.InstanceID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want one data restore before start", volumes.rollbacks)
	}
	recovered, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("recovered app missing")
	}
	if recovered.NetworkAnchorID == "" || len(recovered.Containers) == 0 {
		t.Fatalf("containers not recreated after start recovery: anchor=%q containers=%v", recovered.NetworkAnchorID, recovered.Containers)
	}
	assertAppContainersRunning(t, mock, recovered)
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestUpdateImage_PostGenerationStoreFailureLeavesRecoverableTransaction(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_post_generation_store")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failPostGeneration := true
	state.storeTupleStateHook = func(instanceID string, ts *TupleState) error {
		if instanceID == inst.InstanceID && failPostGeneration && ts.ActiveGeneration() != nil && ts.LatestSnapshot() != nil {
			failPostGeneration = false
			return errors.New("tuple store failed")
		}
		return nil
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "record post-update generation") {
		t.Fatalf("UpdateImage err = %v, want post generation store failure", err)
	}
	txn, err := state.LoadImageUpdateTransaction(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadImageUpdateTransaction: %v", err)
	}
	if !txn.CommitIntent {
		t.Fatalf("commit intent = false, want pending forward repair transaction")
	}
	candidateRootfs := cloneStringMap(txn.CandidateActiveRootfs)

	state.storeTupleStateHook = nil
	mgr.ReconcileOnce(ctx)
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
	repaired, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("repaired app missing")
	}
	ts, err := state.LoadTupleState(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadTupleState: %v", err)
	}
	active := ts.ActiveGeneration()
	if active == nil || !mapsEqual(active.RootfsVolIDs, candidateRootfs) || !mapsEqual(repaired.ActiveRootfs, candidateRootfs) {
		t.Fatalf("active generation = %+v, app rootfs = %v, want candidate %v", active, repaired.ActiveRootfs, candidateRootfs)
	}
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("rollbacks = %v, want no auto-rollback after forward repair", volumes.rollbacks)
	}
}

func TestImageUpdateForwardRepairPreservesCandidateMetadataWhenJournalMissedContainers(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_forward_preserve_candidate_metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID && strings.TrimSpace(txn.CandidateNetworkAnchorID) != "" {
			return errors.New("transaction store failed after candidate containers")
		}
		return nil
	}
	failPostGeneration := true
	state.storeTupleStateHook = func(instanceID string, ts *TupleState) error {
		if instanceID == inst.InstanceID && failPostGeneration && ts.ActiveGeneration() != nil && ts.LatestSnapshot() != nil {
			failPostGeneration = false
			return errors.New("tuple store failed")
		}
		return nil
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "record post-update generation") {
		t.Fatalf("UpdateImage err = %v, want post generation store failure", err)
	}
	candidateApp, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("candidate app missing")
	}
	candidateAnchor := candidateApp.NetworkAnchorID
	candidateContainers := cloneStringMap(candidateApp.Containers)
	candidateRootfs := cloneStringMap(candidateApp.ActiveRootfs)
	if candidateAnchor == "" || len(candidateContainers) == 0 {
		t.Fatalf("candidate metadata missing before repair: anchor=%q containers=%v", candidateAnchor, candidateContainers)
	}
	txn, err := state.LoadImageUpdateTransaction(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadImageUpdateTransaction: %v", err)
	}
	if txn.CandidateNetworkAnchorID != "" || len(txn.CandidateContainers) != 0 {
		t.Fatalf("transaction unexpectedly has candidate container IDs: anchor=%q containers=%v", txn.CandidateNetworkAnchorID, txn.CandidateContainers)
	}

	state.storeImageUpdateTransactionHook = nil
	state.storeTupleStateHook = nil
	mgr.ReconcileOnce(ctx)
	repaired, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("repaired app missing")
	}
	if repaired.NetworkAnchorID != candidateAnchor || !mapsEqual(repaired.Containers, candidateContainers) || !mapsEqual(repaired.ActiveRootfs, candidateRootfs) {
		t.Fatalf("repaired metadata changed: anchor=%q containers=%v rootfs=%v; want anchor=%q containers=%v rootfs=%v",
			repaired.NetworkAnchorID, repaired.Containers, repaired.ActiveRootfs, candidateAnchor, candidateContainers, candidateRootfs)
	}
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("rollbacks = %v, want forward repair without data rollback", volumes.rollbacks)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestImageUpdateRecovery_PromotedSnapshotAttachFailureDoesNotRetryRollback(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_recovery_promoted_attach_failure")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failCommitIntent := true
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID && txn.Phase == imageUpdatePhaseCommitIntent && failCommitIntent {
			failCommitIntent = false
			return errors.New("disk write failed")
		}
		return nil
	}
	if err := mgr.UpdateImage(ctx, inst.InstanceID); err == nil {
		t.Fatal("expected commit intent persistence failure")
	}
	state.storeImageUpdateTransactionHook = nil

	volumes.rollbackResultSet = true
	volumes.rollbackRenamesCommitted = true
	volumes.rollbackSnapshotPromoted = true
	volumes.rollbackErr = errors.New("attach failed")
	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want one attempted restore", volumes.rollbacks)
	}
	txn, err := state.LoadImageUpdateTransaction(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadImageUpdateTransaction: %v", err)
	}
	if txn.SnapshotLVName != "" {
		t.Fatalf("snapshot LV marker = %q, want consumed after promotion", txn.SnapshotLVName)
	}
	ts, err := state.LoadTupleState(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadTupleState: %v", err)
	}
	active := ts.ActiveGeneration()
	if active == nil || active.DataSnapshot != "" {
		t.Fatalf("active generation after promoted snapshot = %+v", active)
	}

	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want no second rollback with consumed snapshot", volumes.rollbacks)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestImageUpdateRecovery_PromotedSnapshotTupleRepairFailureDoesNotRetryRollback(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_recovery_promoted_tuple_failure")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failCommitIntent := true
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID && txn.Phase == imageUpdatePhaseCommitIntent && failCommitIntent {
			failCommitIntent = false
			return errors.New("disk write failed")
		}
		return nil
	}
	if err := mgr.UpdateImage(ctx, inst.InstanceID); err == nil {
		t.Fatal("expected commit intent persistence failure")
	}
	state.storeImageUpdateTransactionHook = nil

	volumes.rollbackResultSet = true
	volumes.rollbackRenamesCommitted = true
	volumes.rollbackSnapshotPromoted = true

	failTupleRepair := true
	state.storeTupleStateHook = func(instanceID string, ts *TupleState) error {
		if instanceID != inst.InstanceID || !failTupleRepair {
			return nil
		}
		if active := ts.ActiveGeneration(); active != nil && active.DataSnapshot == "" {
			failTupleRepair = false
			return errors.New("tuple repair failed")
		}
		return nil
	}
	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want one attempted restore", volumes.rollbacks)
	}
	txn, err := state.LoadImageUpdateTransaction(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadImageUpdateTransaction: %v", err)
	}
	if !txn.DataSnapshotRestored || txn.SnapshotLVName != "" || txn.RestoredSnapshotLVName == "" {
		t.Fatalf("txn after tuple repair failure = %+v, want consumed snapshot marker", txn)
	}

	state.storeTupleStateHook = nil
	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks after retry = %v, want no retry against consumed snapshot", volumes.rollbacks)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestImageUpdateRecovery_DoesNotDestroyAmbiguousPreSwitchSnapshot(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_preswitch_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	txn, _, err := mgr.planImageUpdateRollbackTransaction(ctx, state, inst, "main")
	if err != nil {
		t.Fatalf("planImageUpdateRollbackTransaction: %v", err)
	}
	volumes.artifacts = []string{txn.SnapshotLVName}

	recovered, err := mgr.recoverPendingImageUpdateForApp(ctx, state, inst)
	if err != nil {
		t.Fatalf("recoverPendingImageUpdateForApp: %v", err)
	}
	if !recovered {
		t.Fatal("expected planned transaction recovery")
	}
	if len(volumes.destroyed) != 0 {
		t.Fatalf("destroyed snapshots = %v, want retained ambiguous planned artifact", volumes.destroyed)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
}

func TestRollbackToSnapshotRunsPendingImageUpdateRecoveryFirst(t *testing.T) {
	tmp, err := os.MkdirTemp("", "rollback_runs_image_update_recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	failCommitIntent := true
	state.storeImageUpdateTransactionHook = func(instanceID string, txn *ImageUpdateTransaction) error {
		if instanceID == inst.InstanceID && txn.Phase == imageUpdatePhaseCommitIntent && failCommitIntent {
			failCommitIntent = false
			return errors.New("disk write failed")
		}
		return nil
	}
	if err := mgr.UpdateImage(ctx, inst.InstanceID); err == nil {
		t.Fatal("expected commit intent persistence failure")
	}
	state.storeImageUpdateTransactionHook = nil
	if mgr.HasSnapshotAvailable(ctx, inst.InstanceID) {
		t.Fatal("snapshot should be hidden while image update recovery is pending")
	}

	err = mgr.RollbackToSnapshot(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "no snapshot available") {
		t.Fatalf("RollbackToSnapshot err = %v, want normal no-snapshot result after image recovery consumes snapshot", err)
	}
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want image update recovery to consume snapshot exactly once", volumes.rollbacks)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image update transaction err = %v, want not exist", err)
	}
	mgr.ReconcileOnce(ctx)
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks after reconcile = %v, want no retry against consumed snapshot", volumes.rollbacks)
	}
}

func TestImageUpdateRecoveryRetainsSnapshotWhenTupleStateUnreadable(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_recovery_tuple_unreadable")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	txn, _, err := mgr.planImageUpdateRollbackTransaction(ctx, state, inst, "main")
	if err != nil {
		t.Fatalf("planImageUpdateRollbackTransaction: %v", err)
	}
	txn.Phase = imageUpdatePhaseRuntimeSwitch
	txn.RuntimeSwitchStarted = true
	if err := state.StoreImageUpdateTransaction(inst.InstanceID, txn); err != nil {
		t.Fatalf("StoreImageUpdateTransaction: %v", err)
	}
	genPath := filepath.Join(state.appsDir, inst.InstanceID, generationsFile)
	if err := os.WriteFile(genPath, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("corrupt generations.json: %v", err)
	}

	recovered, err := mgr.recoverPendingImageUpdateForApp(ctx, state, inst)
	if err != nil {
		t.Fatalf("recoverPendingImageUpdateForApp: %v", err)
	}
	if !recovered {
		t.Fatal("expected image update recovery")
	}
	if len(volumes.destroyed) != 0 {
		t.Fatalf("destroyed snapshots = %v, want retention when tuple state is unreadable", volumes.destroyed)
	}
}

func TestRollbackToSnapshot_RoutesAroundStaleFailedDataLV(t *testing.T) {
	tmp, err := os.MkdirTemp("", "rollback_failed_lv_collision")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tmp},
	}
	mgr.SetVolumeManager(volumes)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImagePersistentAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.UpdateImage(ctx, inst.InstanceID); err != nil {
		t.Fatalf("UpdateImage: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	ts, err := state.LoadTupleState(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadTupleState: %v", err)
	}
	active := ts.ActiveGeneration()
	if active == nil {
		t.Fatal("active generation missing")
	}
	activeNumber, ok := parseTupleGenerationNumber(active.ID)
	if !ok {
		t.Fatalf("active generation id = %q, want gen-N", active.ID)
	}
	staleFailedName := FailedDataLVName(inst.InstanceID, activeNumber)
	volumes.artifacts = append(volumes.artifacts, staleFailedName)
	volumes.rollbacks = nil

	if err := mgr.RollbackToSnapshot(ctx, inst.InstanceID); err != nil {
		t.Fatalf("RollbackToSnapshot: %v", err)
	}
	if got, want := volumes.rollbacks, []string{DataSnapshotLVName(inst.InstanceID, 1) + "->" + staleFailedName + "-1"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("rollbacks = %v, want %v", got, want)
	}
}

func updateImagePersistentAppDef() *api.AppDefinition {
	return &api.AppDefinition{
		Type:           "user",
		PrimaryService: "main",
		Listeners: []api.AppListener{{
			Name:      "persistapp",
			GuestPort: 80,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
		}},
		Services: map[string]api.AppService{
			"main": {
				Image:     "alpine:3.18",
				BindPorts: []int{80},
				Storage: &api.AppStorage{Persistent: map[string]api.AppVolume{
					"data": {Container: "/data"},
				}},
			},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
}

func assertAppContainersRunning(t *testing.T, mock *MockContainerManager, inst *AppInstance) {
	t.Helper()
	if inst.NetworkAnchorID != "" {
		assertMockContainerRunning(t, mock, "network anchor", inst.NetworkAnchorID)
	}
	for svcName, cid := range inst.Containers {
		assertMockContainerRunning(t, mock, svcName, cid)
	}
}

func assertMockContainerRunning(t *testing.T, mock *MockContainerManager, label, cid string) {
	t.Helper()
	c := mock.containers[cid]
	if c == nil {
		t.Fatalf("%s container %s missing", label, cid)
	}
	if c.Status != "running" {
		t.Fatalf("%s container %s status = %s, want running", label, cid, c.Status)
	}
}
