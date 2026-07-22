package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"piccolod/internal/fsutil"
	"piccolod/internal/persistence"
)

const (
	imageUpdateTxnFilename = "image_update_transaction.json"

	imageUpdatePhaseSnapshotPlanned     = "snapshot_planned"
	imageUpdatePhaseSnapshotCreated     = "snapshot_created"
	imageUpdatePhaseRuntimeSwitch       = "runtime_switch_started"
	imageUpdatePhaseCandidateDataRisk   = "candidate_data_risk"
	imageUpdatePhaseCommitIntent        = "commit_intent"
	imageUpdatePhaseCommitted           = "committed"
	imageUpdatePhaseCleanupPending      = "committed_cleanup_pending"
	imageUpdatePhaseRestoringPrevious   = "restoring_previous"
	imageUpdatePhaseRestoreFailed       = "restore_failed"
	imageUpdatePhaseForwardRepairFailed = "forward_repair_failed"
)

// ImageUpdateTransaction records the rollback boundary for image refreshes.
// Image refresh does not change app.yaml, but it does switch rootfs LVs and can
// expose persistent data to candidate containers. This journal is the durable
// boundary that lets recovery distinguish "restore previous data" from
// "forward-complete the update".
type ImageUpdateTransaction struct {
	OperationID              string            `json:"operation_id"`
	Phase                    string            `json:"phase"`
	SnapshotGenerationID     string            `json:"snapshot_generation_id"`
	SnapshotGenerationNumber int               `json:"snapshot_generation_number"`
	SnapshotLVName           string            `json:"snapshot_lv_name"`
	RestoredSnapshotLVName   string            `json:"restored_snapshot_lv_name,omitempty"`
	FailedDataLVName         string            `json:"failed_data_lv_name"`
	PreviousActiveRootfs     map[string]string `json:"previous_active_rootfs,omitempty"`
	StagedRootfs             map[string]string `json:"staged_rootfs,omitempty"`
	CreatedRootfs            map[string]string `json:"created_rootfs,omitempty"`
	CandidateActiveRootfs    map[string]string `json:"candidate_active_rootfs,omitempty"`
	CandidatePrimaryService  string            `json:"candidate_primary_service,omitempty"`
	CandidateNetworkAnchorID string            `json:"candidate_network_anchor_id,omitempty"`
	CandidateContainers      map[string]string `json:"candidate_containers,omitempty"`
	RuntimeSwitchStarted     bool              `json:"runtime_switch_started,omitempty"`
	CandidateDataRisk        bool              `json:"candidate_data_risk,omitempty"`
	CommitIntent             bool              `json:"commit_intent,omitempty"`
	DataSnapshotRestored     bool              `json:"data_snapshot_restored,omitempty"`
	CreatedAt                time.Time         `json:"created_at"`
	UpdatedAt                time.Time         `json:"updated_at"`
	LastError                string            `json:"last_error,omitempty"`
}

type appDataRollbackArtifactLister interface {
	ListAppDataRollbackArtifacts(ctx context.Context, instanceID string) ([]string, error)
}

func (fsm *FilesystemStateManager) StoreImageUpdateTransaction(instanceID string, txn *ImageUpdateTransaction) error {
	if txn == nil {
		return fmt.Errorf("image update transaction required")
	}
	if fsm.storeImageUpdateTransactionHook != nil {
		if err := fsm.storeImageUpdateTransactionHook(instanceID, txn); err != nil {
			return err
		}
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, instanceID)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}
	txn.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(txn, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(filepath.Join(appDir, imageUpdateTxnFilename), data, 0600)
}

func (fsm *FilesystemStateManager) LoadImageUpdateTransaction(instanceID string) (*ImageUpdateTransaction, error) {
	path := filepath.Join(fsm.appsDir, instanceID, imageUpdateTxnFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var txn ImageUpdateTransaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return nil, err
	}
	return &txn, nil
}

func storeImageUpdateTransactionAndTransition(state *FilesystemStateManager, instanceID string, txn *ImageUpdateTransaction, appInst *AppInstance) error {
	if err := state.StoreImageUpdateTransaction(instanceID, txn); err != nil {
		return err
	}
	if err := storeTransitionRecordForImageUpdate(state, instanceID, txn, appInst); err != nil {
		return fmt.Errorf("store image update v2 transition projection: %w", err)
	}
	return nil
}

func (fsm *FilesystemStateManager) ClearImageUpdateTransaction(instanceID string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	path := filepath.Join(fsm.appsDir, instanceID, imageUpdateTxnFilename)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func storeTransitionRecordForImageUpdate(state *FilesystemStateManager, instanceID string, txn *ImageUpdateTransaction, appInst *AppInstance) error {
	record, err := state.LoadTransitionRecord(instanceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	record.Phase = transitionPhaseForImageUpdatePhase("")
	if txn != nil {
		record.Phase = transitionPhaseForImageUpdatePhase(txn.Phase)
		record.Resources = transitionResourcesFromImageUpdateTransaction(txn, appInst)
		record.LastError = txn.LastError
	}
	return state.StoreTransitionRecord(instanceID, record)
}

func storeTransitionRecordForImageUpdateNoJournal(state *FilesystemStateManager, instanceID string, phase TransitionPhase, stagedRootfs []string, createdRootfs []string, candidate *AppInstance, lastErr error) error {
	record, err := state.LoadTransitionRecord(instanceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	record.Phase = phase
	if len(stagedRootfs) > 0 {
		if record.Resources.StagedRootfs == nil {
			record.Resources.StagedRootfs = map[string]string{}
		}
		for _, volID := range stagedRootfs {
			volID = strings.TrimSpace(volID)
			if volID != "" {
				record.Resources.StagedRootfs[volID] = volID
			}
		}
	}
	if len(createdRootfs) > 0 {
		if record.Resources.CreatedRootfs == nil {
			record.Resources.CreatedRootfs = map[string]string{}
		}
		for _, volID := range createdRootfs {
			volID = strings.TrimSpace(volID)
			if volID != "" {
				record.Resources.CreatedRootfs[volID] = volID
			}
		}
	}
	if candidate != nil {
		record.Resources.CandidateActiveRootfs = cloneStringMap(candidate.ActiveRootfs)
		record.Resources.CandidatePrimaryService = candidate.PrimaryService
		record.Resources.CandidateNetworkAnchorID = candidate.NetworkAnchorID
		record.Resources.CandidateContainers = cloneStringMap(candidate.Containers)
	}
	if lastErr != nil {
		record.LastError = lastErr.Error()
	} else {
		record.LastError = ""
	}
	return state.StoreTransitionRecord(instanceID, record)
}

func transitionPhaseForImageUpdatePhase(phase string) TransitionPhase {
	switch phase {
	case imageUpdatePhaseSnapshotPlanned, imageUpdatePhaseSnapshotCreated:
		return TransitionPhaseResourcesPrepared
	case imageUpdatePhaseRuntimeSwitch:
		return TransitionPhaseSwitchingRuntime
	case imageUpdatePhaseCandidateDataRisk:
		return TransitionPhaseCandidateTouched
	case imageUpdatePhaseCommitIntent:
		return TransitionPhaseCommitIntent
	case imageUpdatePhaseForwardRepairFailed, imageUpdatePhaseCleanupPending:
		return TransitionPhaseCommittedCleanupPending
	case imageUpdatePhaseCommitted:
		return TransitionPhaseCommitted
	case imageUpdatePhaseRestoringPrevious:
		return TransitionPhaseRestoringPrevious
	case imageUpdatePhaseRestoreFailed:
		return TransitionPhaseRestoreFailed
	default:
		return TransitionPhasePrepared
	}
}

func transitionResourcesFromImageUpdateTransaction(txn *ImageUpdateTransaction, appInst *AppInstance) TransitionResources {
	resources := TransitionResources{}
	if txn == nil {
		return resources
	}
	if len(txn.StagedRootfs) > 0 {
		resources.StagedRootfs = cloneStringMap(txn.StagedRootfs)
	}
	if len(txn.CreatedRootfs) > 0 {
		resources.CreatedRootfs = cloneStringMap(txn.CreatedRootfs)
	}
	if len(txn.CandidateActiveRootfs) > 0 {
		if resources.StagedRootfs == nil {
			resources.StagedRootfs = map[string]string{}
		}
		for service, volID := range txn.CandidateActiveRootfs {
			if strings.TrimSpace(volID) == "" || (txn.PreviousActiveRootfs != nil && txn.PreviousActiveRootfs[service] == volID) {
				continue
			}
			resources.StagedRootfs[volID] = volID
		}
	}
	if strings.TrimSpace(txn.SnapshotLVName) != "" {
		resources.DataSnapshots = map[string]string{
			txn.SnapshotLVName: txn.SnapshotLVName,
		}
	}
	if strings.TrimSpace(txn.RestoredSnapshotLVName) != "" {
		if resources.DataSnapshots == nil {
			resources.DataSnapshots = map[string]string{}
		}
		resources.DataSnapshots[txn.RestoredSnapshotLVName] = txn.RestoredSnapshotLVName
	}
	if strings.TrimSpace(txn.FailedDataLVName) != "" {
		resources.FailedLVs = map[string]string{
			txn.FailedDataLVName: txn.FailedDataLVName,
		}
	}
	if len(txn.CandidateContainers) > 0 {
		resources.CandidateContainers = cloneStringMap(txn.CandidateContainers)
	} else if appInst != nil && len(appInst.Containers) > 0 {
		resources.CandidateContainers = cloneStringMap(appInst.Containers)
	}
	return resources
}

func (m *AppManager) recoverV2OnlyImageUpdateTransition(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, record *TransitionRecord) error {
	if appInst == nil || record == nil {
		return nil
	}
	if record.Plan.OperationKind != TransitionOperationUpdateImage || record.Plan.SourceKind != TransitionSourceCurrentCommitted {
		return fmt.Errorf("unsupported v2-only transition operation %s/%s", record.Plan.OperationKind, record.Plan.SourceKind)
	}
	if record.Phase == TransitionPhasePrepared {
		return state.ClearTransitionRecord(appInst.InstanceID)
	}
	if record.Plan.Data.CandidateMayTouchData || record.Plan.Data.SnapshotRequired {
		return fmt.Errorf("v2-only image update %s may have touched data without legacy recovery journal", record.Phase)
	}
	switch record.Phase {
	case TransitionPhaseResourcesPrepared:
		if err := m.cleanupV2OnlyImageUpdateStagedRootfs(ctx, appInst, record); err != nil {
			return err
		}
		return state.ClearTransitionRecord(appInst.InstanceID)
	case TransitionPhaseSwitchingRuntime, TransitionPhaseRestoringPrevious, TransitionPhaseRestoreFailed:
		return m.abortV2OnlyImageUpdateTransition(ctx, state, appInst, record)
	case TransitionPhaseCommitIntent, TransitionPhaseCommittedCleanupPending, TransitionPhaseCommitted:
		return m.forwardCompleteV2OnlyImageUpdateTransition(ctx, state, appInst, record)
	default:
		return fmt.Errorf("unsupported v2-only image update phase %s", record.Phase)
	}
}

func (m *AppManager) abortV2OnlyImageUpdateTransition(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, record *TransitionRecord) error {
	instanceID := appInst.InstanceID
	def, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("read current manifest: %w", err)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}
	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, runtimeUsable, err := m.quiesceContainerGroupRuntime(ctx, state, appInst, def, layout)
	if err != nil {
		return fmt.Errorf("quiesce uncertain image update runtime: %w", err)
	}
	if runtimeUsable {
		if err := m.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
			return fmt.Errorf("remove uncertain image update runtime: %w", err)
		}
	}
	if err := m.cleanupV2OnlyImageUpdateStagedRootfs(ctx, appInst, record); err != nil {
		return err
	}
	if appInst.Enabled {
		runtime, err = m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
		if err != nil {
			return fmt.Errorf("reacquire runtime for previous image generation: %w", err)
		}
		endpoints, _ := m.serviceManager.GetByApp(instanceID)
		if len(endpoints) == 0 {
			var allocErr error
			endpoints, allocErr = m.serviceManager.AllocateForApp(instanceID, def.Listeners)
			if allocErr != nil {
				return fmt.Errorf("allocate endpoints for previous runtime: %w", allocErr)
			}
		}
		m.configureOIDCAuthorizePaths(instanceID, def)
		prebuiltRootfs, err := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, def, appInst)
		if err != nil {
			return fmt.Errorf("attach previous rootfs: %w", err)
		}
		restored, err := m.installContainerGroup(ctx, def, instanceID, layout, runtime, endpoints, prebuiltRootfs)
		if err != nil {
			return fmt.Errorf("recreate previous runtime: %w", err)
		}
		appInst.PrimaryService = restored.PrimaryService
		appInst.NetworkAnchorID = restored.NetworkAnchorID
		appInst.Containers = cloneStringMap(restored.Containers)
		appInst.Definition = def
		if err := state.StoreApp(appInst); err != nil {
			return fmt.Errorf("store restored previous runtime: %w", err)
		}
		m.setObservedStatus(instanceID, StatusRunning)
	} else {
		m.setObservedStatus(instanceID, StatusStopped)
	}
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		return fmt.Errorf("clear aborted image update transition: %w", err)
	}
	log.Printf("INFO: image update recovery %s: aborted v2-only transition from phase %s", instanceID, record.Phase)
	return nil
}

func (m *AppManager) forwardCompleteV2OnlyImageUpdateTransition(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, record *TransitionRecord) error {
	_ = ctx
	instanceID := appInst.InstanceID
	def, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("read current manifest: %w", err)
	}
	candidateActiveRootfs := cloneStringMap(record.Resources.CandidateActiveRootfs)
	if len(candidateActiveRootfs) == 0 {
		candidateActiveRootfs = transitionCandidateActiveRootfs(record, appInst)
	}
	if len(candidateActiveRootfs) > 0 {
		appInst.ActiveRootfs = candidateActiveRootfs
	}
	if len(record.Resources.CandidateContainers) > 0 {
		appInst.Containers = cloneStringMap(record.Resources.CandidateContainers)
	}
	if strings.TrimSpace(record.Resources.CandidateNetworkAnchorID) != "" {
		appInst.NetworkAnchorID = record.Resources.CandidateNetworkAnchorID
		if m.serviceManager != nil {
			m.serviceManager.SetAppContainerID(instanceID, record.Resources.CandidateNetworkAnchorID)
		}
	}
	if strings.TrimSpace(record.Resources.CandidatePrimaryService) != "" {
		appInst.PrimaryService = record.Resources.CandidatePrimaryService
	}
	appInst.Definition = def
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst); err != nil {
		return fmt.Errorf("store forward-completed image update metadata: %w", err)
	}
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		return fmt.Errorf("clear forward-completed image update transition: %w", err)
	}
	if appInst.Enabled {
		m.setObservedStatus(instanceID, StatusRunning)
	} else {
		m.setObservedStatus(instanceID, StatusStopped)
	}
	log.Printf("INFO: image update recovery %s: forward-completed v2-only transition", instanceID)
	return nil
}

func (m *AppManager) cleanupV2OnlyImageUpdateStagedRootfs(ctx context.Context, appInst *AppInstance, record *TransitionRecord) error {
	if record == nil {
		return nil
	}
	return m.cleanupImageUpdateStagedRootfs(ctx, appInst.ActiveRootfs, transitionStagedRootfsIDs(record), transitionCreatedRootfsIDs(record))
}

func transitionStagedRootfsIDs(record *TransitionRecord) []string {
	if record == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(record.Resources.StagedRootfs)+len(record.Plan.Cleanup.StagedRootfsKeys))
	for _, volID := range record.Resources.StagedRootfs {
		volID = strings.TrimSpace(volID)
		if volID != "" && !seen[volID] {
			seen[volID] = true
			out = append(out, volID)
		}
	}
	for _, volID := range record.Plan.Cleanup.StagedRootfsKeys {
		volID = strings.TrimSpace(volID)
		if volID != "" && !seen[volID] {
			seen[volID] = true
			out = append(out, volID)
		}
	}
	return out
}

func transitionCreatedRootfsIDs(record *TransitionRecord) []string {
	if record == nil {
		return nil
	}
	return uniqueRootfsIDsFromMap(record.Resources.CreatedRootfs)
}

func imageUpdateStagedRootfsIDs(txn *ImageUpdateTransaction) []string {
	if txn == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(txn.StagedRootfs)+len(txn.CandidateActiveRootfs))
	for _, volID := range txn.StagedRootfs {
		volID = strings.TrimSpace(volID)
		if volID != "" && !seen[volID] {
			seen[volID] = true
			out = append(out, volID)
		}
	}
	for service, volID := range txn.CandidateActiveRootfs {
		volID = strings.TrimSpace(volID)
		if volID == "" || (txn.PreviousActiveRootfs != nil && txn.PreviousActiveRootfs[service] == volID) || seen[volID] {
			continue
		}
		seen[volID] = true
		out = append(out, volID)
	}
	return out
}

func imageUpdateCreatedRootfsIDs(txn *ImageUpdateTransaction) []string {
	if txn == nil {
		return nil
	}
	return uniqueRootfsIDsFromMap(txn.CreatedRootfs)
}

func uniqueRootfsIDsFromMap(values map[string]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for key, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
			continue
		}
		key = strings.TrimSpace(key)
		if key != "" && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func imageUpdateStagedRootfsMap(ids []string) map[string]string {
	out := map[string]string{}
	for _, volID := range ids {
		volID = strings.TrimSpace(volID)
		if volID != "" {
			out[volID] = volID
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m *AppManager) cleanupImageUpdateStagedRootfs(ctx context.Context, active map[string]string, staged []string, created []string) error {
	if len(staged) == 0 && len(created) == 0 {
		return nil
	}
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return fmt.Errorf("rootfs volume manager not configured")
	}
	activeRootfs := map[string]bool{}
	for _, volID := range active {
		volID = strings.TrimSpace(volID)
		if volID != "" {
			activeRootfs[volID] = true
		}
	}
	for _, volID := range staged {
		volID = strings.TrimSpace(volID)
		if volID == "" || activeRootfs[volID] {
			continue
		}
		if err := rootfs.DetachRootfs(ctx, volID); err != nil {
			return fmt.Errorf("detach staged rootfs %s: %w", volID, err)
		}
	}
	for _, volID := range created {
		volID = strings.TrimSpace(volID)
		if volID == "" || activeRootfs[volID] {
			continue
		}
		if rootfs.RootfsExists(volID) {
			if err := rootfs.DestroyRootfs(ctx, volID); err != nil {
				return fmt.Errorf("destroy created rootfs %s: %w", volID, err)
			}
		}
	}
	return nil
}

func transitionCandidateActiveRootfs(record *TransitionRecord, appInst *AppInstance) map[string]string {
	out := cloneStringMap(record.Plan.Runtime.PreviousActiveRootfs)
	if len(out) == 0 && appInst != nil {
		out = cloneStringMap(appInst.ActiveRootfs)
	}
	if out == nil {
		out = map[string]string{}
	}
	for _, decision := range record.Plan.ImageRootfs {
		if strings.TrimSpace(decision.ServiceName) == "" || strings.TrimSpace(decision.PlannedRootfsKey) == "" {
			continue
		}
		out[decision.ServiceName] = decision.PlannedRootfsKey
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapContainsValue(values map[string]string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func (m *AppManager) recoverPendingImageUpdates(ctx context.Context, state *FilesystemStateManager, skip map[string]bool) map[string]bool {
	blocked := map[string]bool{}
	for _, appInst := range state.ListApps() {
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
			continue
		}
		if skip != nil && skip[appInst.InstanceID] {
			continue
		}
		txn, err := state.LoadImageUpdateTransaction(appInst.InstanceID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Printf("ERROR: image update recovery %s: load transaction: %v", appInst.InstanceID, err)
			m.setObservedStatus(appInst.InstanceID, StatusError)
			blocked[appInst.InstanceID] = true
			continue
		}
		recoveryCtx, admitted := admitPendingTransitionRecovery(ctx)
		if !admitted {
			return blocked
		}
		err = m.recoverOneImageUpdate(recoveryCtx, state, appInst, txn)
		if err != nil {
			log.Printf("ERROR: image update recovery %s: %v", appInst.InstanceID, err)
			m.setObservedStatus(appInst.InstanceID, StatusError)
			blocked[appInst.InstanceID] = true
		}
		if transitionRecoveryMustYield(recoveryCtx) {
			return blocked
		}
	}
	return blocked
}

func (m *AppManager) recoverPendingImageUpdateForApp(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) (bool, error) {
	if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
		return false, nil
	}
	txn, err := state.LoadImageUpdateTransaction(appInst.InstanceID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("load transaction: %w", err)
	}
	return true, m.recoverOneImageUpdate(ctx, state, appInst, txn)
}

func (m *AppManager) recoverOneImageUpdate(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, txn *ImageUpdateTransaction) error {
	if txn == nil {
		return nil
	}
	if err := storeTransitionRecordForImageUpdate(state, appInst.InstanceID, txn, appInst); err != nil {
		return fmt.Errorf("sync image update transition record: %w", err)
	}
	instanceID := appInst.InstanceID
	if txn.CommitIntent || txn.Phase == imageUpdatePhaseForwardRepairFailed {
		return m.forwardCompleteImageUpdate(ctx, state, appInst, txn)
	}
	switch txn.Phase {
	case imageUpdatePhaseCommitted, imageUpdatePhaseCleanupPending:
		if err := state.ClearTransitionRecord(instanceID); err != nil {
			return err
		}
		return state.ClearImageUpdateTransaction(instanceID)
	case imageUpdatePhaseSnapshotPlanned, imageUpdatePhaseSnapshotCreated, imageUpdatePhaseRuntimeSwitch, imageUpdatePhaseCandidateDataRisk, imageUpdatePhaseRestoringPrevious, imageUpdatePhaseRestoreFailed:
		return m.restorePreCommitImageUpdate(ctx, state, appInst, txn)
	default:
		txn.LastError = fmt.Sprintf("unknown image update transaction phase %q", txn.Phase)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return fmt.Errorf("%s", txn.LastError)
	}
}

func (m *AppManager) forwardCompleteImageUpdate(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, txn *ImageUpdateTransaction) error {
	instanceID := appInst.InstanceID
	def, err := state.GetAppDefinition(instanceID)
	if err != nil {
		txn.Phase = imageUpdatePhaseForwardRepairFailed
		txn.LastError = fmt.Sprintf("read app definition: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return fmt.Errorf("read app definition: %w", err)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		txn.Phase = imageUpdatePhaseForwardRepairFailed
		txn.LastError = fmt.Sprintf("app volume layout: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return err
	}
	runtime, runtimeUsable, err := m.quiesceContainerGroupRuntime(ctx, state, appInst, def, layout)
	if err != nil {
		txn.Phase = imageUpdatePhaseForwardRepairFailed
		txn.LastError = fmt.Sprintf("quiesce runtime: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return err
	}
	candidateMetadataAlreadyStored := len(txn.CandidateActiveRootfs) > 0 && mapsEqual(appInst.ActiveRootfs, txn.CandidateActiveRootfs)
	if len(txn.CandidateContainers) == 0 && txn.CandidateNetworkAnchorID == "" {
		if !candidateMetadataAlreadyStored {
			if runtimeUsable {
				if err := m.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
					log.Printf("WARN: image update recovery %s: remove pre-commit containers: %v", instanceID, err)
				}
			}
			appInst.Containers = nil
			appInst.NetworkAnchorID = ""
		}
	} else {
		appInst.Containers = cloneStringMap(txn.CandidateContainers)
		appInst.NetworkAnchorID = txn.CandidateNetworkAnchorID
		appInst.PrimaryService = txn.CandidatePrimaryService
	}
	if len(txn.CandidateActiveRootfs) > 0 {
		appInst.ActiveRootfs = cloneStringMap(txn.CandidateActiveRootfs)
	}
	appInst.Definition = def
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst); err != nil {
		txn.Phase = imageUpdatePhaseForwardRepairFailed
		txn.LastError = fmt.Sprintf("store app metadata: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return fmt.Errorf("store app metadata: %w", err)
	}
	if err := ensureImageUpdateActiveGeneration(state, appInst); err != nil {
		txn.Phase = imageUpdatePhaseForwardRepairFailed
		txn.LastError = fmt.Sprintf("record active generation: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return err
	}
	txn.Phase = imageUpdatePhaseCommitted
	txn.LastError = ""
	if err := state.StoreImageUpdateTransaction(instanceID, txn); err != nil {
		return fmt.Errorf("mark image update committed: %w", err)
	}
	if err := storeTransitionRecordForImageUpdate(state, instanceID, txn, appInst); err != nil {
		return fmt.Errorf("mark image update transition committed: %w", err)
	}
	if err := state.ClearImageUpdateTransaction(instanceID); err != nil {
		return fmt.Errorf("clear image update transaction: %w", err)
	}
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		return fmt.Errorf("clear image update transition: %w", err)
	}
	if appInst.Enabled {
		m.setObservedStatus(instanceID, StatusStarting)
	} else {
		m.setObservedStatus(instanceID, StatusStopped)
	}
	log.Printf("INFO: image update recovery %s: forward-completed commit intent", instanceID)
	return nil
}

func (m *AppManager) restorePreCommitImageUpdate(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, txn *ImageUpdateTransaction) error {
	instanceID := appInst.InstanceID
	if !txn.RuntimeSwitchStarted && !txn.CandidateDataRisk {
		return m.clearImageUpdatePreCandidateAbort(ctx, state, instanceID, txn)
	}
	if !txn.CandidateDataRisk && !txn.CommitIntent {
		txn.Phase = imageUpdatePhaseRestoringPrevious
		txn.LastError = ""
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		if err := m.clearImageUpdatePreCandidateAbort(ctx, state, instanceID, txn); err != nil {
			return m.markImageUpdateRestoreFailed(state, instanceID, txn, err)
		}
		log.Printf("INFO: image update recovery %s: cleared pre-candidate rollback state from phase %s", instanceID, txn.Phase)
		return nil
	}
	def, err := state.GetAppDefinition(instanceID)
	if err != nil {
		txn.Phase = imageUpdatePhaseRestoreFailed
		txn.LastError = fmt.Sprintf("read app definition: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return fmt.Errorf("read app definition: %w", err)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		txn.Phase = imageUpdatePhaseRestoreFailed
		txn.LastError = fmt.Sprintf("app volume layout: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return err
	}
	runtime, runtimeUsable, err := m.quiesceContainerGroupRuntime(ctx, state, appInst, def, layout)
	if err != nil {
		txn.Phase = imageUpdatePhaseRestoreFailed
		txn.LastError = fmt.Sprintf("quiesce runtime: %v", err)
		_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		return err
	}
	txn.Phase = imageUpdatePhaseRestoringPrevious
	txn.LastError = ""
	_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)

	if txn.RuntimeSwitchStarted && runtimeUsable {
		if err := m.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
			log.Printf("WARN: image update recovery %s: remove candidate containers: %v", instanceID, err)
		}
	}
	if txn.CandidateDataRisk && txn.DataSnapshotRestored {
		snapshotName := strings.TrimSpace(txn.RestoredSnapshotLVName)
		if snapshotName == "" {
			snapshotName = strings.TrimSpace(txn.SnapshotLVName)
		}
		if snapshotName != "" {
			if err := markImageSnapshotRestoredActive(state, instanceID, snapshotName); err != nil {
				return m.markImageUpdateRestoreFailed(state, instanceID, txn, err)
			}
		}
	} else if txn.CandidateDataRisk && strings.TrimSpace(txn.SnapshotLVName) != "" {
		rollbacker, ok := m.currentVolumeManager().(dataVolumeRollbacker)
		if !ok {
			return m.markImageUpdateRestoreFailed(state, instanceID, txn, fmt.Errorf("volume manager does not support rollback"))
		}
		snapshotName := strings.TrimSpace(txn.SnapshotLVName)
		failedName := strings.TrimSpace(txn.FailedDataLVName)
		if failedName == "" {
			name, allocErr := m.allocateFailedDataLVName(ctx, state, instanceID, txn.SnapshotGenerationNumber, nil)
			if allocErr != nil {
				return m.markImageUpdateRestoreFailed(state, instanceID, txn, allocErr)
			}
			failedName = name
			txn.FailedDataLVName = name
			_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		}
		renamesCommitted, snapshotPromoted, rollbackErr := rollbacker.RollbackDataVolume(ctx, instanceID, snapshotName, failedName)
		if rollbackErr != nil && !renamesCommitted {
			return m.markImageUpdateRestoreFailed(state, instanceID, txn, fmt.Errorf("restore data snapshot: %w", rollbackErr))
		}
		if renamesCommitted {
			if err := trackImageFailedDataLV(state, instanceID, failedName, txn.OperationID); err != nil {
				log.Printf("WARN: image update recovery %s: track failed data LV %s: %v", instanceID, failedName, err)
			}
		}
		if snapshotPromoted {
			txn.SnapshotLVName = ""
			txn.RestoredSnapshotLVName = snapshotName
			txn.DataSnapshotRestored = true
			txn.Phase = imageUpdatePhaseRestoringPrevious
			txn.LastError = ""
			if storeErr := storeImageUpdateTransactionAndTransition(state, instanceID, txn, appInst); storeErr != nil {
				log.Printf("WARN: image update recovery %s: mark restored data snapshot %s: %v", instanceID, snapshotName, storeErr)
			}
			if err := markImageSnapshotRestoredActive(state, instanceID, snapshotName); err != nil {
				return m.markImageUpdateRestoreFailed(state, instanceID, txn, err)
			}
		}
		if rollbackErr != nil {
			return m.markImageUpdateRestoreFailed(state, instanceID, txn, fmt.Errorf("restore data snapshot after LV rename: %w", rollbackErr))
		}
		if !snapshotPromoted {
			if err := markImageSnapshotRestoredActive(state, instanceID, snapshotName); err != nil {
				return m.markImageUpdateRestoreFailed(state, instanceID, txn, err)
			}
		}
	} else if strings.TrimSpace(txn.SnapshotLVName) != "" {
		referenced, refErr := tupleReferencesDataSnapshot(state, instanceID, txn.SnapshotLVName)
		if refErr != nil {
			log.Printf("WARN: image update recovery %s: retain snapshot %s because tuple state could not be checked: %v", instanceID, txn.SnapshotLVName, refErr)
		} else if !referenced {
			if snapshotter, ok := m.currentVolumeManager().(dataVolumeSnapshotter); ok {
				if err := snapshotter.DestroyDataSnapshot(ctx, txn.SnapshotLVName); err != nil {
					log.Printf("WARN: image update recovery %s: destroy unused snapshot %s: %v", instanceID, txn.SnapshotLVName, err)
				}
			}
		}
	}
	if len(txn.PreviousActiveRootfs) > 0 {
		appInst.ActiveRootfs = cloneStringMap(txn.PreviousActiveRootfs)
	}
	appInst.Containers = nil
	appInst.NetworkAnchorID = ""
	appInst.Definition = def
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst); err != nil {
		return m.markImageUpdateRestoreFailed(state, instanceID, txn, fmt.Errorf("store previous app state: %w", err))
	}
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		return fmt.Errorf("clear image update transition: %w", err)
	}
	if err := state.ClearImageUpdateTransaction(instanceID); err != nil {
		return fmt.Errorf("clear image update transaction: %w", err)
	}
	log.Printf("INFO: image update recovery %s: restored previous runtime from phase %s", instanceID, txn.Phase)
	return nil
}

func (m *AppManager) clearImageUpdatePreCandidateAbort(ctx context.Context, state *FilesystemStateManager, instanceID string, txn *ImageUpdateTransaction) error {
	if txn == nil || txn.CandidateDataRisk || txn.CommitIntent {
		return nil
	}
	if _, err := state.LoadImageUpdateTransaction(instanceID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read pre-candidate image update transaction: %w", err)
	}
	snapshotName := strings.TrimSpace(txn.SnapshotLVName)
	if snapshotName != "" {
		referenced, refErr := tupleReferencesDataSnapshot(state, instanceID, snapshotName)
		if refErr != nil {
			log.Printf("WARN: image update recovery %s: retain pre-candidate snapshot %s because tuple state could not be checked: %v", instanceID, snapshotName, refErr)
		} else if !referenced && txn.Phase == imageUpdatePhaseSnapshotPlanned && imageUpdateRollbackArtifactListed(ctx, m.currentVolumeManager(), instanceID, snapshotName) {
			log.Printf("WARN: image update recovery %s: retain pre-candidate snapshot %s because the planned artifact already exists outside tuple metadata", instanceID, snapshotName)
		} else if !referenced {
			if snapshotter, ok := m.currentVolumeManager().(dataVolumeSnapshotter); ok {
				if err := snapshotter.DestroyDataSnapshot(ctx, snapshotName); err != nil {
					log.Printf("WARN: image update recovery %s: retain pre-candidate snapshot %s because cleanup failed: %v", instanceID, snapshotName, err)
				}
			}
		}
	}
	if err := m.cleanupImageUpdateStagedRootfs(ctx, txn.PreviousActiveRootfs, imageUpdateStagedRootfsIDs(txn), imageUpdateCreatedRootfsIDs(txn)); err != nil {
		return err
	}
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		return err
	}
	return state.ClearImageUpdateTransaction(instanceID)
}

func imageUpdateRollbackArtifactListed(ctx context.Context, volumeManager any, instanceID, artifact string) bool {
	lister, ok := volumeManager.(appDataRollbackArtifactLister)
	if !ok {
		return false
	}
	names, err := lister.ListAppDataRollbackArtifacts(ctx, instanceID)
	if err != nil {
		log.Printf("WARN: image update recovery %s: retain planned rollback artifact %s because artifacts could not be listed: %v", instanceID, artifact, err)
		return true
	}
	for _, name := range names {
		if strings.TrimSpace(name) == strings.TrimSpace(artifact) {
			return true
		}
	}
	return false
}

func (m *AppManager) markImageUpdateRestoreFailed(state *FilesystemStateManager, instanceID string, txn *ImageUpdateTransaction, err error) error {
	txn.Phase = imageUpdatePhaseRestoreFailed
	txn.LastError = err.Error()
	_ = storeImageUpdateTransactionAndTransition(state, instanceID, txn, nil)
	m.setObservedStatus(instanceID, StatusError)
	return err
}

func tupleReferencesDataSnapshot(state *FilesystemStateManager, instanceID, snapshotLVName string) (bool, error) {
	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return false, err
	}
	if ts == nil {
		return false, nil
	}
	for _, gen := range ts.Generations {
		if gen.DataSnapshot == snapshotLVName {
			return true, nil
		}
	}
	return false, nil
}

func ensureImageUpdateActiveGeneration(state *FilesystemStateManager, appInst *AppInstance) error {
	ts, err := state.LoadTupleState(appInst.InstanceID)
	if err != nil {
		return fmt.Errorf("load tuple state: %w", err)
	}
	if ts == nil {
		ts = &TupleState{InstanceID: appInst.InstanceID, NextGenNumber: 1}
	}
	rootfsVolIDs := cloneStringMap(appInst.ActiveRootfs)
	if active := ts.ActiveGeneration(); active != nil && mapsEqual(active.RootfsVolIDs, rootfsVolIDs) {
		ts.CurrentGeneration = active.ID
		return state.StoreTupleState(appInst.InstanceID, ts)
	}
	genID := ts.AllocateGenerationID()
	ts.Generations = append(ts.Generations, TupleGeneration{
		ID:           genID,
		RootfsVolIDs: rootfsVolIDs,
		CreatedAt:    time.Now(),
		Status:       TupleStatusActive,
	})
	ts.CurrentGeneration = genID
	return state.StoreTupleState(appInst.InstanceID, ts)
}

func trackImageFailedDataLV(state *FilesystemStateManager, instanceID, failedLVName, operationID string) error {
	if strings.TrimSpace(failedLVName) == "" {
		return nil
	}
	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return fmt.Errorf("load tuple state: %w", err)
	}
	if ts == nil {
		ts = &TupleState{InstanceID: instanceID, NextGenNumber: 1}
	}
	for i := range ts.Generations {
		if ts.Generations[i].FailedLVName == failedLVName {
			return nil
		}
	}
	now := time.Now()
	shortID := manifestUpdateShortOperationID(operationID)
	if shortID == "" {
		shortID = fmt.Sprintf("%d", now.UnixNano())
	}
	ts.Generations = append(ts.Generations, TupleGeneration{
		ID:           "gen-failed-image-" + shortID,
		Status:       TupleStatusFailed,
		FailedLVName: failedLVName,
		FailedAt:     &now,
		CreatedAt:    now,
	})
	return state.StoreTupleState(instanceID, ts)
}

func markImageSnapshotRestoredActive(state *FilesystemStateManager, instanceID, snapshotLVName string) error {
	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return fmt.Errorf("load tuple state: %w", err)
	}
	if ts == nil {
		return nil
	}
	now := time.Now()
	found := false
	for i := range ts.Generations {
		gen := &ts.Generations[i]
		switch {
		case gen.DataSnapshot == snapshotLVName:
			gen.Status = TupleStatusActive
			gen.DataSnapshot = ""
			gen.DeprecatedAt = nil
			ts.CurrentGeneration = gen.ID
			found = true
		case gen.Status == TupleStatusActive:
			gen.Status = TupleStatusFailed
			gen.FailedAt = &now
		}
	}
	if !found {
		return nil
	}
	return state.StoreTupleState(instanceID, ts)
}

func (m *AppManager) planImageUpdateRollbackTransaction(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, primary string, operationID ...string) (*ImageUpdateTransaction, *TupleState, error) {
	if state == nil || appInst == nil {
		return nil, nil, fmt.Errorf("image update rollback plan requires state and app")
	}
	instanceID := appInst.InstanceID
	if existing, err := state.LoadImageUpdateTransaction(instanceID); err == nil && existing != nil {
		return nil, nil, fmt.Errorf("%w: image update already has pending rollback state in phase %s", ErrImageUpdateRejected, existing.Phase)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("%w: image update rollback state unreadable: %v", ErrImageUpdateRejected, err)
	}

	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("load tuple state: %w", err)
	}
	if ts == nil {
		ts = &TupleState{InstanceID: instanceID, NextGenNumber: 1}
	}
	if ts.InstanceID == "" {
		ts.InstanceID = instanceID
	}
	if ts.NextGenNumber <= 0 {
		ts.NextGenNumber = 1
	}

	reserved, err := m.reservedAppRollbackArtifactNames(ctx, state, instanceID, ts)
	if err != nil {
		return nil, nil, err
	}
	genNumber := ts.NextGenNumber
	for {
		snapshotName := DataSnapshotLVName(instanceID, genNumber)
		failedName := FailedDataLVName(instanceID, genNumber)
		if !reserved[snapshotName] && !reserved[failedName] {
			break
		}
		genNumber++
	}
	ts.NextGenNumber = genNumber + 1
	if err := state.StoreTupleState(instanceID, ts); err != nil {
		return nil, nil, fmt.Errorf("reserve tuple generation number: %w", err)
	}

	opID := ""
	if len(operationID) > 0 {
		opID = strings.TrimSpace(operationID[0])
	}
	if opID == "" {
		var err error
		opID, err = randomManifestUpdateToken()
		if err != nil {
			return nil, nil, fmt.Errorf("generate image update operation id: %w", err)
		}
	}
	rootfsVolIDs := cloneStringMap(appInst.ActiveRootfs)
	if len(rootfsVolIDs) == 0 && primary != "" {
		rootfsVolIDs = map[string]string{primary: persistence.ServiceRootfsVolumeID(instanceID, primary)}
	}
	now := time.Now().UTC()
	txn := &ImageUpdateTransaction{
		OperationID:              opID,
		Phase:                    imageUpdatePhaseSnapshotPlanned,
		SnapshotGenerationID:     fmt.Sprintf("gen-%d", genNumber),
		SnapshotGenerationNumber: genNumber,
		SnapshotLVName:           DataSnapshotLVName(instanceID, genNumber),
		FailedDataLVName:         FailedDataLVName(instanceID, genNumber),
		PreviousActiveRootfs:     rootfsVolIDs,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if err := state.StoreImageUpdateTransaction(instanceID, txn); err != nil {
		return nil, nil, fmt.Errorf("store image update rollback plan: %w", err)
	}
	if err := storeTransitionRecordForImageUpdate(state, instanceID, txn, appInst); err != nil {
		return nil, nil, fmt.Errorf("store image update transition plan: %w", err)
	}
	return txn, ts, nil
}

func (m *AppManager) reservedAppRollbackArtifactNames(ctx context.Context, state *FilesystemStateManager, instanceID string, ts *TupleState) (map[string]bool, error) {
	reserved := map[string]bool{}
	if ts != nil {
		for _, gen := range ts.Generations {
			if strings.TrimSpace(gen.DataSnapshot) != "" {
				reserved[gen.DataSnapshot] = true
			}
			if strings.TrimSpace(gen.FailedLVName) != "" {
				reserved[gen.FailedLVName] = true
			}
		}
	}
	if txn, err := state.LoadManifestUpdateTransaction(instanceID); err == nil && txn != nil {
		if strings.TrimSpace(txn.PrecommitDataSnapshotID) != "" {
			reserved[txn.PrecommitDataSnapshotID] = true
		}
		if strings.TrimSpace(txn.FailedDataLVName) != "" {
			reserved[txn.FailedDataLVName] = true
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: manifest update rollback state unreadable: %v", ErrImageUpdateRejected, err)
	}
	if lister, ok := m.currentVolumeManager().(appDataRollbackArtifactLister); ok {
		names, err := lister.ListAppDataRollbackArtifacts(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("%w: list rollback artifacts: %v", ErrImageUpdateRejected, err)
		}
		for _, name := range names {
			if strings.TrimSpace(name) != "" {
				reserved[name] = true
			}
		}
	}
	return reserved, nil
}

func (m *AppManager) allocateFailedDataLVName(ctx context.Context, state *FilesystemStateManager, instanceID string, preferredGenNumber int, ts *TupleState) (string, error) {
	if preferredGenNumber <= 0 {
		preferredGenNumber = 1
	}
	reserved, err := m.reservedAppRollbackArtifactNames(ctx, state, instanceID, ts)
	if err != nil {
		return "", err
	}
	base := FailedDataLVName(instanceID, preferredGenNumber)
	if !reserved[base] {
		return base, nil
	}
	for suffix := 1; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if !reserved[candidate] {
			return candidate, nil
		}
	}
}

func parseTupleGenerationNumber(id string) (int, bool) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "gen-") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, "gen-"))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
