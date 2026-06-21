package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
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
	setMockRegistryDigest(mock, "sha256:update-requires-snapshot")

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
	setMockRegistryDigest(mock, "sha256:update-snapshot-failure")

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
	setMockRegistryDigest(mock, "sha256:update-stale-snapshot")
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

func TestUpdateImageStoresAndClearsTransitionRecord(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_transition_record")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	setMockRegistryDigest(mock, "sha256:update-transition-record")
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var phases []TransitionPhase
	var firstRecord *TransitionRecord
	state.storeTransitionRecordHook = func(instanceID string, record *TransitionRecord) error {
		if instanceID != inst.InstanceID {
			return nil
		}
		phases = append(phases, record.Phase)
		if firstRecord == nil {
			copy := *record
			firstRecord = &copy
		}
		return nil
	}

	if err := mgr.UpdateImage(ctx, inst.InstanceID); err != nil {
		t.Fatalf("UpdateImage: %v", err)
	}
	if firstRecord == nil {
		t.Fatal("transition record was not stored")
	}
	if firstRecord.Plan.OperationKind != TransitionOperationUpdateImage {
		t.Fatalf("operation kind = %s, want %s", firstRecord.Plan.OperationKind, TransitionOperationUpdateImage)
	}
	if firstRecord.Plan.SourceKind != TransitionSourceCurrentCommitted {
		t.Fatalf("source kind = %s, want %s", firstRecord.Plan.SourceKind, TransitionSourceCurrentCommitted)
	}
	if firstRecord.Plan.BaseManifestHash == "" || firstRecord.Plan.BaseManifestHash != firstRecord.Plan.CandidateManifestHash {
		t.Fatalf("manifest hashes = base %q candidate %q, want current-source update", firstRecord.Plan.BaseManifestHash, firstRecord.Plan.CandidateManifestHash)
	}
	if len(firstRecord.Plan.ImageRootfs) != 1 || firstRecord.Plan.ImageRootfs[0].ServiceName != "main" {
		t.Fatalf("image rootfs plan = %+v, want main refresh", firstRecord.Plan.ImageRootfs)
	}
	if firstRecord.Plan.Runtime.CandidateRuntimeNamePolicy != "deterministic_app_service_container_names_v1" {
		t.Fatalf("runtime name policy = %q", firstRecord.Plan.Runtime.CandidateRuntimeNamePolicy)
	}
	if got := firstRecord.Plan.Runtime.CandidateActiveRootfs["main"]; got == "" || got == firstRecord.Plan.Runtime.PreviousActiveRootfs["main"] {
		t.Fatalf("candidate active rootfs = %q, previous = %q", got, firstRecord.Plan.Runtime.PreviousActiveRootfs["main"])
	}
	if got := firstRecord.Plan.ResourceKeys["runtime:anchor"]; got != networkAnchorContainerName(inst.InstanceID) {
		t.Fatalf("runtime anchor resource key = %q, want %q", got, networkAnchorContainerName(inst.InstanceID))
	}
	wantContainerName := containerNameForService(inst.InstanceID, "main", firstRecord.Plan.Runtime.PrimaryService)
	if got := firstRecord.Plan.ResourceKeys["runtime:service:main"]; got != wantContainerName {
		t.Fatalf("runtime service resource key = %q, want %q", got, wantContainerName)
	}
	if !slices.Contains(phases, TransitionPhasePrepared) || !slices.Contains(phases, TransitionPhaseCommitted) {
		t.Fatalf("transition phases = %v, want prepared and committed", phases)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want not exist after successful update", err)
	}
}

func TestImageUpdateRecoveryCleansV2OnlyResourcesPreparedWithoutRestart(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_v2_abort")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:v2abort")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	candidateRootfs := imagePlan[0].RootfsVolumeID
	rootfs := mgr.currentRootfsManager().(*stubRootfsManager)
	if rootfs.exists == nil {
		rootfs.exists = map[string]bool{}
	}
	rootfs.exists[candidateRootfs] = true
	record.Phase = TransitionPhaseResourcesPrepared
	record.Resources.StagedRootfs = map[string]string{candidateRootfs: candidateRootfs}
	record.Resources.CreatedRootfs = map[string]string{candidateRootfs: candidateRootfs}
	if err := state.StoreTransitionRecord(inst.InstanceID, record); err != nil {
		t.Fatalf("store transition: %v", err)
	}
	beforeAnchor := inst.NetworkAnchorID
	beforeContainers := cloneStringMap(inst.Containers)
	beforeRootfs := cloneStringMap(inst.ActiveRootfs)

	if err := mgr.recoverV2OnlyImageUpdateTransition(ctx, state, inst, record); err != nil {
		t.Fatalf("recover v2-only image update: %v", err)
	}

	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want not exist after abort recovery", err)
	}
	if !slices.Contains(rootfs.destroyed, candidateRootfs) {
		t.Fatalf("destroyed rootfs = %v, want staged candidate %s", rootfs.destroyed, candidateRootfs)
	}
	restored, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("restored app missing")
	}
	if got := restored.ActiveRootfs["main"]; got != inst.ActiveRootfs["main"] {
		t.Fatalf("active rootfs = %q, want previous %q", got, inst.ActiveRootfs["main"])
	}
	if restored.NetworkAnchorID != beforeAnchor || !mapsEqual(restored.Containers, beforeContainers) || !mapsEqual(restored.ActiveRootfs, beforeRootfs) {
		t.Fatalf("resources-prepared recovery should not restart runtime: before anchor=%q containers=%v rootfs=%v after=%q containers=%v rootfs=%v", beforeAnchor, beforeContainers, beforeRootfs, restored.NetworkAnchorID, restored.Containers, restored.ActiveRootfs)
	}
	assertAppContainersRunning(t, mock, restored)
}

func TestImageUpdateRecoveryClearsPreparedPersistentV2OnlyBeforeLegacyJournal(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_v2_prepared_persistent")
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
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:v2prepared")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	if !record.Plan.Data.SnapshotRequired || !record.Plan.Data.CandidateMayTouchData {
		t.Fatalf("test expected persistent-data transition plan, got %+v", record.Plan.Data)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image journal err = %v, want absent before rollback plan", err)
	}

	if err := mgr.recoverV2OnlyImageUpdateTransition(ctx, state, inst, record); err != nil {
		t.Fatalf("recover prepared v2-only image update: %v", err)
	}

	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want cleared", err)
	}
	assertAppContainersRunning(t, mock, inst)
}

func TestImageUpdateRecoveryRemovesUnnamedCandidateRuntimeByDeterministicName(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_v2_candidate_name_cleanup")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:v2-candidate-name-cleanup")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	candidateRootfs := imagePlan[0].RootfsVolumeID
	rootfs := mgr.currentRootfsManager().(*stubRootfsManager)
	if rootfs.exists == nil {
		rootfs.exists = map[string]bool{}
	}
	rootfs.exists[candidateRootfs] = true

	for _, id := range inst.Containers {
		delete(mock.containers, id)
	}
	delete(mock.containers, inst.NetworkAnchorID)
	candidateAnchor := "cid-v2-name-cleanup-anchor"
	candidateMain := "cid-v2-name-cleanup-main"
	primary := primaryServiceFor(curDef, inst)
	mock.containers[candidateAnchor] = &mockContainer{
		ID:     candidateAnchor,
		Status: "running",
		Spec: container.ContainerCreateSpec{
			Name: networkAnchorContainerName(inst.InstanceID),
		},
	}
	mock.containers[candidateMain] = &mockContainer{
		ID:     candidateMain,
		Status: "running",
		Spec: container.ContainerCreateSpec{
			Name: containerNameForService(inst.InstanceID, "main", primary),
		},
	}
	record.Phase = TransitionPhaseSwitchingRuntime
	record.Resources.StagedRootfs = map[string]string{candidateRootfs: candidateRootfs}
	record.Resources.CreatedRootfs = map[string]string{candidateRootfs: candidateRootfs}
	record.Resources.CandidateActiveRootfs = map[string]string{"main": candidateRootfs}
	if err := state.StoreTransitionRecord(inst.InstanceID, record); err != nil {
		t.Fatalf("store transition: %v", err)
	}

	if err := mgr.recoverV2OnlyImageUpdateTransition(ctx, state, inst, record); err != nil {
		t.Fatalf("recover v2-only image update: %v", err)
	}

	if _, ok := mock.containers[candidateAnchor]; ok {
		t.Fatalf("candidate anchor %s survived recovery", candidateAnchor)
	}
	if _, ok := mock.containers[candidateMain]; ok {
		t.Fatalf("candidate service %s survived recovery", candidateMain)
	}
	if !slices.Contains(rootfs.destroyed, candidateRootfs) {
		t.Fatalf("destroyed rootfs = %v, want %s", rootfs.destroyed, candidateRootfs)
	}
	restored, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("restored app missing")
	}
	if got := restored.ActiveRootfs["main"]; got != inst.ActiveRootfs["main"] {
		t.Fatalf("active rootfs = %q, want previous %q", got, inst.ActiveRootfs["main"])
	}
	assertAppContainersRunning(t, mock, restored)
}

func TestTransitionFollowUpFinishesV2OnlyImageCleanup(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_v2_followup_cleanup")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:v2-followup-cleanup")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	candidateRootfs := imagePlan[0].RootfsVolumeID
	record.Phase = TransitionPhaseCommittedCleanupPending
	record.Resources.CandidateActiveRootfs = map[string]string{"main": candidateRootfs}
	if err := state.StoreTransitionRecord(inst.InstanceID, record); err != nil {
		t.Fatalf("store transition: %v", err)
	}

	err = mgr.RetryTransitionFollowUp(ctx, inst.InstanceID, TransitionActionAccessRepair)
	if !errors.Is(err, ErrTransitionFollowUpUnavailable) {
		t.Fatalf("access follow-up err = %v, want %v", err, ErrTransitionFollowUpUnavailable)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); err != nil {
		t.Fatalf("transition record should remain after rejected follow-up: %v", err)
	}

	if err := mgr.RetryTransitionFollowUp(ctx, inst.InstanceID, TransitionActionFinishCleanup); err != nil {
		t.Fatalf("finish cleanup follow-up: %v", err)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want cleared", err)
	}
	recovered, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("recovered app missing")
	}
	if got := recovered.ActiveRootfs["main"]; got != candidateRootfs {
		t.Fatalf("active rootfs = %q, want %q", got, candidateRootfs)
	}
}

func TestImageUpdateRecoveryForwardCompletesV2OnlyCommitIntent(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_v2_forward")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:v2forward")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	candidateRootfs := imagePlan[0].RootfsVolumeID
	candidateAnchor := "cid-v2-forward-anchor"
	candidateMain := "cid-v2-forward-main"
	mock.containers[candidateAnchor] = &mockContainer{ID: candidateAnchor, Status: "running"}
	mock.containers[candidateMain] = &mockContainer{ID: candidateMain, Status: "running"}
	record.Phase = TransitionPhaseCommitIntent
	record.Resources.StagedRootfs = map[string]string{candidateRootfs: candidateRootfs}
	record.Resources.CandidateActiveRootfs = map[string]string{"main": candidateRootfs}
	record.Resources.CandidatePrimaryService = "main"
	record.Resources.CandidateNetworkAnchorID = candidateAnchor
	record.Resources.CandidateContainers = map[string]string{"main": candidateMain}
	if err := state.StoreTransitionRecord(inst.InstanceID, record); err != nil {
		t.Fatalf("store transition: %v", err)
	}

	if err := mgr.recoverV2OnlyImageUpdateTransition(ctx, state, inst, record); err != nil {
		t.Fatalf("recover v2-only image update: %v", err)
	}

	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want not exist after forward recovery", err)
	}
	recovered, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("recovered app missing")
	}
	if got := recovered.ActiveRootfs["main"]; got != candidateRootfs {
		t.Fatalf("active rootfs = %q, want %q", got, candidateRootfs)
	}
	if recovered.NetworkAnchorID != candidateAnchor {
		t.Fatalf("network anchor = %q, want %q", recovered.NetworkAnchorID, candidateAnchor)
	}
	if got := recovered.Containers["main"]; got != candidateMain {
		t.Fatalf("main container = %q, want %q", got, candidateMain)
	}
}

func TestTransitionRecoveryDelegatesLegacyImageJournalThroughV2(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_v2_legacy_delegate")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:v2legacy")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	txn := &ImageUpdateTransaction{
		OperationID:          record.OperationID,
		Phase:                imageUpdatePhaseSnapshotPlanned,
		PreviousActiveRootfs: cloneStringMap(inst.ActiveRootfs),
		StagedRootfs:         imageUpdateStagedRootfsMap(transitionRootfsKeysFromManifestPlan(imagePlan)),
	}
	if err := state.StoreImageUpdateTransaction(inst.InstanceID, txn); err != nil {
		t.Fatalf("store image txn: %v", err)
	}

	blocked := mgr.recoverPendingTransitionRecords(ctx, state)
	if blocked[inst.InstanceID] {
		t.Fatalf("transition recovery blocked for legacy-backed image journal")
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image transaction err = %v, want not exist", err)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want not exist", err)
	}
}

func TestImageUpdateRecoveryKeepsLegacyJournalWhenTransitionClearFails(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_legacy_journal_v2_clear")
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
	setMockRegistryDigest(mock, "sha256:update-v2-clear-fails")
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
	if err == nil || !strings.Contains(err.Error(), "commit intent") {
		t.Fatalf("UpdateImage err = %v, want commit intent persistence failure", err)
	}
	state.storeImageUpdateTransactionHook = nil
	txn, err := state.LoadImageUpdateTransaction(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadImageUpdateTransaction: %v", err)
	}
	if txn.Phase != imageUpdatePhaseCandidateDataRisk {
		t.Fatalf("phase = %q, want %q", txn.Phase, imageUpdatePhaseCandidateDataRisk)
	}
	pending, ok := state.GetApp(inst.InstanceID)
	if !ok {
		t.Fatalf("pending app missing")
	}
	state.clearTransitionRecordHook = func(instanceID string) error {
		if instanceID == inst.InstanceID {
			return errors.New("transition clear failed")
		}
		return nil
	}

	err = mgr.recoverOneImageUpdate(ctx, state, pending, txn)
	if err == nil || !strings.Contains(err.Error(), "clear image update transition") {
		t.Fatalf("recoverOneImageUpdate err = %v, want transition clear failure", err)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); err != nil {
		t.Fatalf("image update transaction should remain recoverable after v2 clear failure: %v", err)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); err != nil {
		t.Fatalf("transition record should remain after failed clear: %v", err)
	}
	if len(volumes.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want one data restore before clear failure", volumes.rollbacks)
	}
}

func TestImageUpdatePreCandidateAbortKeepsLegacyJournalWhenTransitionClearFails(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_pre_candidate_v2_clear")
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
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:pre-candidate-clear-fails")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	txn := &ImageUpdateTransaction{
		OperationID:          record.OperationID,
		Phase:                imageUpdatePhaseSnapshotPlanned,
		PreviousActiveRootfs: cloneStringMap(inst.ActiveRootfs),
		StagedRootfs:         imageUpdateStagedRootfsMap(transitionRootfsKeysFromManifestPlan(imagePlan)),
	}
	if err := state.StoreImageUpdateTransaction(inst.InstanceID, txn); err != nil {
		t.Fatalf("store image txn: %v", err)
	}
	if err := storeTransitionRecordForImageUpdate(state, inst.InstanceID, txn, inst); err != nil {
		t.Fatalf("store transition projection: %v", err)
	}
	state.clearTransitionRecordHook = func(instanceID string) error {
		if instanceID == inst.InstanceID {
			return errors.New("transition clear failed")
		}
		return nil
	}

	err = mgr.recoverOneImageUpdate(ctx, state, inst, txn)
	if err == nil || !strings.Contains(err.Error(), "transition clear failed") {
		t.Fatalf("recoverOneImageUpdate err = %v, want transition clear failure", err)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); err != nil {
		t.Fatalf("image update transaction should remain after v2 clear failure: %v", err)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); err != nil {
		t.Fatalf("transition record should remain after failed clear: %v", err)
	}
}

func TestImageUpdateCommittedCleanupKeepsLegacyJournalWhenTransitionClearFails(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_committed_cleanup_v2_clear")
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
	curDef, err := state.GetAppDefinition(inst.InstanceID)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	imagePlan := updateImagePlanForTest(inst, curDef, "sha256:committed-cleanup-clear-fails")
	record, err := mgr.beginUpdateImageTransitionRecord(ctx, state, inst, curDef, imagePlan)
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	txn := &ImageUpdateTransaction{
		OperationID:          record.OperationID,
		Phase:                imageUpdatePhaseCleanupPending,
		PreviousActiveRootfs: cloneStringMap(inst.ActiveRootfs),
		StagedRootfs:         imageUpdateStagedRootfsMap(transitionRootfsKeysFromManifestPlan(imagePlan)),
		CandidateActiveRootfs: map[string]string{
			"main": imagePlan[0].RootfsVolumeID,
		},
	}
	if err := state.StoreImageUpdateTransaction(inst.InstanceID, txn); err != nil {
		t.Fatalf("store image txn: %v", err)
	}
	if err := storeTransitionRecordForImageUpdate(state, inst.InstanceID, txn, inst); err != nil {
		t.Fatalf("store transition projection: %v", err)
	}
	state.clearTransitionRecordHook = func(instanceID string) error {
		if instanceID == inst.InstanceID {
			return errors.New("transition clear failed")
		}
		return nil
	}

	err = mgr.recoverOneImageUpdate(ctx, state, inst, txn)
	if err == nil || !strings.Contains(err.Error(), "transition clear failed") {
		t.Fatalf("recoverOneImageUpdate err = %v, want transition clear failure", err)
	}
	if _, err := state.LoadImageUpdateTransaction(inst.InstanceID); err != nil {
		t.Fatalf("image update transaction should remain after v2 clear failure: %v", err)
	}
	if _, err := state.LoadTransitionRecord(inst.InstanceID); err != nil {
		t.Fatalf("transition record should remain after failed clear: %v", err)
	}
}

func TestBeginInstalledAppApplyTransactionRejectsPendingImageJournal(t *testing.T) {
	tmp, err := os.MkdirTemp("", "apply_rejects_image_journal")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := state.StoreImageUpdateTransaction(inst.InstanceID, &ImageUpdateTransaction{Phase: imageUpdatePhaseSnapshotPlanned}); err != nil {
		t.Fatalf("store image txn: %v", err)
	}

	_, err = mgr.beginInstalledAppApplyTransaction(ctx, state, installedAppApplyTransactionSpec{InstanceID: inst.InstanceID})
	if err == nil || !errors.Is(err, ErrManifestUpdateConflict) || !strings.Contains(err.Error(), "image update transaction") {
		t.Fatalf("begin apply err = %v, want image transaction conflict", err)
	}
}

func TestUpdateImageRecordsStagedRootfsBeforeCreateFailure(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_rootfs_create_marker")
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
	rootfs := newStubRootfsManager(tmp)
	rootfs.exists = map[string]bool{}
	rootfs.identities = map[string]persistence.RootfsImageIdentity{}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		return &container.ImageConfig{
			Entrypoint:  nil,
			Cmd:         []string{"/bin/sh"},
			Digest:      "sha256:update-create-fail",
			RepoDigests: []string{imageName + "@sha256:update-create-fail"},
			Size:        500 << 20,
		}, nil
	}
	rootfs.createServiceErr = errors.New("synthetic rootfs create failure after allocation")

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "synthetic rootfs create failure") {
		t.Fatalf("UpdateImage err = %v, want synthetic rootfs create failure", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	record, err := state.LoadTransitionRecord(inst.InstanceID)
	if err != nil {
		t.Fatalf("load transition record: %v", err)
	}
	if len(transitionStagedRootfsIDs(record)) == 0 {
		t.Fatalf("transition record did not retain staged rootfs inventory: %+v", record)
	}
	if len(transitionCreatedRootfsIDs(record)) == 0 {
		t.Fatalf("transition record did not retain created rootfs inventory: %+v", record)
	}
	rootfs.createServiceErr = nil
	if err := mgr.recoverV2OnlyImageUpdateTransition(ctx, state, inst, record); err != nil {
		t.Fatalf("recover transition: %v", err)
	}
	for _, volID := range transitionCreatedRootfsIDs(record) {
		if !slices.Contains(rootfs.destroyed, volID) {
			t.Fatalf("destroyed rootfs = %v, want %s", rootfs.destroyed, volID)
		}
	}
}

func TestImageUpdateAbortCleanupPreservesPreExistingStagedRootfs(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_preserve_preexisting_staged_rootfs")
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
	rootfs := newStubRootfsManager(tmp)
	rootfs.exists = map[string]bool{"rootfs-preexisting": true, "rootfs-created": true}
	mgr.SetRootfsManager(rootfs)

	active := map[string]string{"main": "rootfs-active"}
	if err := mgr.cleanupImageUpdateStagedRootfs(
		context.Background(),
		active,
		[]string{"rootfs-preexisting", "rootfs-created"},
		[]string{"rootfs-created"},
	); err != nil {
		t.Fatalf("cleanup rootfs: %v", err)
	}
	if !slices.Contains(rootfs.detached, "rootfs-preexisting") {
		t.Fatalf("detached rootfs = %v, want pre-existing staged rootfs detached", rootfs.detached)
	}
	if slices.Contains(rootfs.destroyed, "rootfs-preexisting") {
		t.Fatalf("pre-existing staged rootfs was destroyed: %v", rootfs.destroyed)
	}
	if !rootfs.exists["rootfs-preexisting"] {
		t.Fatalf("pre-existing staged rootfs no longer exists")
	}
	if !slices.Contains(rootfs.destroyed, "rootfs-created") {
		t.Fatalf("destroyed rootfs = %v, want created rootfs destroyed", rootfs.destroyed)
	}
}

func TestUpdateImageStoreAppFailureKeepsCandidateWhenRestoreMarkerFails(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_store_app_restore_marker")
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

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	setMockRegistryDigest(mock, "sha256:update-store-app-fails")
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	state.storeAppMetadataHook = func(instanceID string, app *AppInstance) error {
		if instanceID == inst.InstanceID {
			return errors.New("metadata write failed")
		}
		return nil
	}
	state.storeTransitionRecordHook = func(instanceID string, record *TransitionRecord) error {
		if instanceID == inst.InstanceID && record.Phase == TransitionPhaseRestoringPrevious {
			return errors.New("restore marker write failed")
		}
		return nil
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "store image update transition restore marker") {
		t.Fatalf("UpdateImage err = %v, want restore marker write failure", err)
	}
	record, err := state.LoadTransitionRecord(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadTransitionRecord: %v", err)
	}
	if record.Phase != TransitionPhaseCommitIntent {
		t.Fatalf("transition phase = %s, want %s", record.Phase, TransitionPhaseCommitIntent)
	}
	if record.Resources.CandidateNetworkAnchorID == "" || len(record.Resources.CandidateContainers) == 0 {
		t.Fatalf("candidate runtime missing from commit-intent record: %+v", record.Resources)
	}
	if c := mock.containers[record.Resources.CandidateNetworkAnchorID]; c == nil {
		t.Fatalf("candidate network anchor %s was removed", record.Resources.CandidateNetworkAnchorID)
	}
	for svcName, cid := range record.Resources.CandidateContainers {
		if c := mock.containers[cid]; c == nil {
			t.Fatalf("candidate container for %s (%s) was removed", svcName, cid)
		}
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
	setMockRegistryDigest(mock, "sha256:update-final-viability")

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

func TestUpdateImageRejectsLegacyJournalBeforeRegistryPull(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*FilesystemStateManager, string) error
	}{
		{
			name: "image journal",
			prepare: func(state *FilesystemStateManager, instanceID string) error {
				return state.StoreImageUpdateTransaction(instanceID, &ImageUpdateTransaction{Phase: imageUpdatePhaseSnapshotPlanned})
			},
		},
		{
			name: "manifest journal",
			prepare: func(state *FilesystemStateManager, instanceID string) error {
				return state.StoreManifestUpdateTransaction(instanceID, &ManifestUpdateTransaction{Phase: "prepared"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp, err := os.MkdirTemp("", "update_rejects_legacy_before_pull")
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

			def := updateImageMutableServiceAppDef()
			inst, err := mgr.Install(ctx, def)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatalf("state: %v", err)
			}
			if err := tt.prepare(state, inst.InstanceID); err != nil {
				t.Fatalf("store legacy journal: %v", err)
			}
			mock.pulledImages = nil
			inspectCalled := false
			mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
				inspectCalled = true
				return nil, fmt.Errorf("inspect should not run for %s", imageName)
			}

			err = mgr.UpdateImage(ctx, inst.InstanceID)
			if !errors.Is(err, ErrImageUpdateRejected) {
				t.Fatalf("UpdateImage err = %v, want %v", err, ErrImageUpdateRejected)
			}
			if len(mock.pulledImages) != 0 || inspectCalled {
				t.Fatalf("registry path ran before legacy rejection: pulls=%v inspect=%v", mock.pulledImages, inspectCalled)
			}
		})
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
	setMockRegistryDigest(mock, "sha256:update-plan-before-snapshot")
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
	setMockRegistryDigest(mock, "sha256:update-post-generation-store")
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
	setMockRegistryDigest(mock, "sha256:update-forward-preserve-candidate")
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
	setMockRegistryDigest(mock, "sha256:update-promoted-attach-failure")
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
	transition, err := state.LoadTransitionRecord(inst.InstanceID)
	if err != nil {
		t.Fatalf("LoadTransitionRecord: %v", err)
	}
	if transition.Phase != TransitionPhaseCandidateTouched {
		t.Fatalf("transition phase = %s, want %s", transition.Phase, TransitionPhaseCandidateTouched)
	}
	if transition.OperationID != txn.OperationID {
		t.Fatalf("transition operation id = %q, want image txn id %q", transition.OperationID, txn.OperationID)
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
	if _, err := state.LoadTransitionRecord(inst.InstanceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want not exist after image recovery", err)
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
	setMockRegistryDigest(mock, "sha256:update-promoted-tuple-failure")
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
	setMockRegistryDigest(mock, "sha256:update-post-generation-store")
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
	setMockRegistryDigest(mock, "sha256:update-forward-preserve-candidate")
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
	setMockRegistryDigest(mock, "sha256:update-promoted-attach-failure")
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
	setMockRegistryDigest(mock, "sha256:update-promoted-tuple-failure")
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
	setMockRegistryDigest(mock, "sha256:rollback-recovers-image-update")
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
	setMockRegistryDigest(mock, "sha256:rollback-stale-failed-lv")
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

func TestUpdateImageRejectsExistingTargetRootfsDigestMismatch(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_target_rootfs_mismatch")
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
	rootfs := newStubRootfsManager(tmp)
	rootfs.identities = map[string]persistence.RootfsImageIdentity{}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := updateImageMutableServiceAppDef()
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	activeRootfs := inst.ActiveRootfs["main"]
	if activeRootfs == "" {
		t.Fatalf("installed app did not record active rootfs: %+v", inst.ActiveRootfs)
	}
	rootfs.identities[activeRootfs] = persistence.RootfsImageIdentity{
		VolumeID:        activeRootfs,
		BaseImageRef:    "alpine:3.18",
		BaseImageDigest: "sha256:stale-target",
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "does not match planned image identity") {
		t.Fatalf("update image err = %v, want existing target rootfs identity rejection", err)
	}
	assertAppContainersRunning(t, mock, inst)
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

func updateImageMutableServiceAppDef() *api.AppDefinition {
	return &api.AppDefinition{
		Type:           "user",
		PrimaryService: "main",
		Listeners: []api.AppListener{{
			Name:      "mutableapp",
			GuestPort: 80,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
		}},
		Services: map[string]api.AppService{
			"main": {
				Image:     "alpine:3.18",
				BindPorts: []int{80},
			},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
}

func updateImagePlanForTest(inst *AppInstance, def *api.AppDefinition, canonicalDigest string) []ManifestUpdateImagePlanItem {
	imageRef := ""
	if def != nil {
		imageRef = def.Services["main"].Image
	}
	rootfsID := persistence.VersionedServiceRootfsVolumeID(inst.InstanceID, "main", persistence.ShortDigest(canonicalDigest))
	previousRootfs := ""
	if inst.ActiveRootfs != nil {
		previousRootfs = inst.ActiveRootfs["main"]
	}
	return []ManifestUpdateImagePlanItem{{
		ServiceName:            "main",
		EntryKind:              manifestUpdateImageEntryAppService,
		Action:                 manifestUpdateImageActionRefresh,
		Reason:                 "test image refresh",
		ImageRef:               imageRef,
		ResolvedDigest:         canonicalDigest,
		CanonicalDigest:        canonicalDigest,
		RootfsVolumeID:         rootfsID,
		PreviousRootfsVolumeID: previousRootfs,
	}}
}

func setMockRegistryDigest(mock *MockContainerManager, digest string) {
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		return &container.ImageConfig{
			Entrypoint:  nil,
			Cmd:         []string{"/bin/sh"},
			Digest:      digest,
			RepoDigests: []string{imageName + "@" + digest},
			Size:        500 << 20,
		}, nil
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
