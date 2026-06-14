package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/services"
)

const manifestUpdateRuntimeReadinessTimeout = 30 * time.Second

type installedAppApplyTransactionSpec struct {
	OperationKind             string
	TaskType                  string
	RollbackPrefix            string
	InstanceID                string
	AppInst                   *AppInstance
	PreviousDefinition        *api.AppDefinition
	CandidateDefinition       *api.AppDefinition
	PreviousManifestHash      string
	CandidateManifestHash     string
	PreviousLedgerRevision    int64
	CandidateLedgerRevision   int64
	PreviousLedgerSourceHash  string
	CandidateLedgerSourceHash string
	DryRunToken               string
	RuntimeFingerprint        string
	ImagePlan                 []ManifestUpdateImagePlanItem
	MetadataOnly              bool
	RequiresPrecommitSnapshot bool
	ApplyPhase                string
	ApplyMessage              string
	FinalizingMessage         string
}

type installedAppApplyTransaction struct {
	manager      *AppManager
	ctx          context.Context
	state        *FilesystemStateManager
	spec         installedAppApplyTransactionSpec
	txn          *ManifestUpdateTransaction
	runtimeStage *manifestUpdateRuntimeStage
	listenerPlan *services.PreparedReconcile
}

func (m *AppManager) beginInstalledAppApplyTransaction(ctx context.Context, state *FilesystemStateManager, spec installedAppApplyTransactionSpec) (*installedAppApplyTransaction, error) {
	existing, err := state.LoadManifestUpdateTransaction(spec.InstanceID)
	if err == nil {
		switch existing.Phase {
		case "committed", "committed_cleanup_pending":
			if cleanupErr := m.cleanupCommittedManifestUpdateTransaction(ctx, state, spec.InstanceID, existing); cleanupErr != nil {
				return nil, fmt.Errorf("%w: previous update cleanup is still pending: %v", ErrManifestUpdateConflict, cleanupErr)
			}
		default:
			return nil, fmt.Errorf("%w: manifest update transaction already in progress (phase %s)", ErrManifestUpdateConflict, existing.Phase)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load existing manifest update transaction: %w", err)
	}

	operationID, err := randomManifestUpdateToken()
	if err != nil {
		return nil, err
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate(spec.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("backup current manifest: %w", err)
	}
	backupInstallStatePath, err := state.BackupInstallStateForManifestUpdate(spec.InstanceID)
	if err != nil {
		_ = state.ClearManifestUpdateTransaction(spec.InstanceID, backupPath)
		return nil, fmt.Errorf("backup install state: %w", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID:               operationID,
		OperationKind:             spec.OperationKind,
		Phase:                     "prepared",
		PreviousManifestHash:      spec.PreviousManifestHash,
		CandidateManifestHash:     spec.CandidateManifestHash,
		PreviousLedgerRevision:    spec.PreviousLedgerRevision,
		CandidateLedgerRevision:   spec.CandidateLedgerRevision,
		PreviousLedgerSourceHash:  spec.PreviousLedgerSourceHash,
		CandidateLedgerSourceHash: spec.CandidateLedgerSourceHash,
		DryRunToken:               spec.DryRunToken,
		RuntimeFingerprint:        spec.RuntimeFingerprint,
		BackupPath:                backupPath,
		BackupInstallStatePath:    backupInstallStatePath,
		PreviousActiveRootfs:      cloneStringMap(spec.AppInst.ActiveRootfs),
		RemovedRootfs:             manifestUpdateRemovedActiveRootfs(spec.InstanceID, spec.AppInst.ActiveRootfs, spec.PreviousDefinition, spec.CandidateDefinition),
		ResolvedImages:            cloneManifestUpdateImagePlan(spec.ImagePlan),
		CreatedAt:                 time.Now().UTC(),
		UpdatedAt:                 time.Now().UTC(),
	}
	if err := state.StoreManifestUpdateTransaction(spec.InstanceID, txn); err != nil {
		_ = state.ClearManifestUpdateTransaction(spec.InstanceID, backupPath)
		_ = state.ClearInstallStateBackup(backupInstallStatePath)
		return nil, fmt.Errorf("store apply transaction: %w", err)
	}
	return &installedAppApplyTransaction{
		manager: m,
		ctx:     ctx,
		state:   state,
		spec:    spec,
		txn:     txn,
	}, nil
}

func (t *installedAppApplyTransaction) persistCandidateManifest() error {
	if t.spec.ApplyPhase != "" || t.spec.ApplyMessage != "" {
		t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, t.spec.ApplyPhase, 20, t.spec.ApplyMessage, false, nil)
	}
	var previous *api.AppDefinition
	if t.spec.AppInst.Definition != nil {
		copy := *t.spec.AppInst.Definition
		previous = &copy
	}
	t.spec.AppInst.Definition = t.spec.CandidateDefinition
	t.spec.AppInst.UpdatedAt = time.Now()
	if err := t.state.StoreApp(t.spec.AppInst); err != nil {
		t.spec.AppInst.Definition = previous
		cause := fmt.Errorf("persist candidate manifest: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return cause
	}
	if err := t.storePhase("candidate_persisted"); err != nil {
		cause := fmt.Errorf("persist candidate transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return cause
	}
	return nil
}

func (t *installedAppApplyTransaction) stageRuntimeRootfsIfNeeded(classification manifestUpdateClassification) error {
	if t.spec.MetadataOnly {
		return nil
	}
	requiresSnapshot := classification.DataSafety != nil && classification.DataSafety.SnapshotRequired
	stage, err := t.manager.stageManifestUpdateRootfs(
		t.ctx,
		t.spec.TaskType,
		t.spec.InstanceID,
		t.spec.AppInst,
		t.spec.PreviousDefinition,
		t.spec.CandidateDefinition,
		t.spec.ImagePlan,
		requiresSnapshot,
		t.markCreatedRootfsForCleanup,
	)
	if err != nil {
		cause := fmt.Errorf("stage candidate runtime: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	if stage == nil {
		return nil
	}
	t.runtimeStage = stage
	t.txn.CandidateActiveRootfs = cloneStringMap(stage.candidateActiveRootfs)
	t.txn.RemovedRootfs = mergeManifestUpdateRootfsCleanup(t.txn.RemovedRootfs, manifestUpdateSupersededActiveRootfs(t.txn.PreviousActiveRootfs, stage.candidateActiveRootfs))
	t.txn.StagedRootfs = append([]string(nil), stage.stagedRootfs...)
	t.txn.CreatedRootfs = append([]string(nil), stage.createdRootfs...)
	if err := t.storePhase("rootfs_staged"); err != nil {
		cause := fmt.Errorf("persist staged rootfs transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) markCreatedRootfsForCleanup(volID string) error {
	volID = strings.TrimSpace(volID)
	if volID == "" {
		return nil
	}
	if !slices.Contains(t.txn.CreatedRootfs, volID) {
		t.txn.CreatedRootfs = append(t.txn.CreatedRootfs, volID)
	}
	if err := t.storePhase("rootfs_staging"); err != nil {
		return fmt.Errorf("persist created rootfs cleanup marker: %w", err)
	}
	return nil
}

func (t *installedAppApplyTransaction) recreateRuntimeIfNeeded() error {
	if t.spec.MetadataOnly {
		return nil
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseRecreatingContainer, 50, "Recreating containers", false, nil)
	if err := t.preflightPrecommitDataSnapshotIfNeeded(); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	if err := t.prepareListenersIfNeeded(); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	if err := t.suspendAccessForRuntimeSwitch(); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	if err := markManifestTransactionRuntimeSwitchStarted(t.state, t.spec.InstanceID, t.txn); err != nil {
		cause := fmt.Errorf("persist runtime switch transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	if err := t.quiesceRuntimeForPrecommitDataSnapshotIfNeeded(); err != nil {
		cause := fmt.Errorf("quiesce runtime before precommit data snapshot: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	if err := t.createPrecommitDataSnapshotIfNeeded(); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	beforeInstall := func() error {
		if err := markManifestTransactionRuntimeTouched(t.state, t.spec.InstanceID, t.txn); err != nil {
			return fmt.Errorf("persist runtime transaction marker: %w", err)
		}
		return nil
	}
	recreate := t.manager.recreateContainersInPlace
	if t.runtimeStage != nil {
		recreate = func(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance) error {
			return t.manager.recreateContainersFromStagedRootfsWithHook(ctx, instanceID, candidateDef, removeDef, appInst, t.runtimeStage, t.listenerPlan, beforeInstall)
		}
	} else if t.listenerPlan != nil {
		recreate = func(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance) error {
			return t.manager.recreateContainersInPlaceWithPreparedListenersAndHook(ctx, instanceID, candidateDef, removeDef, appInst, t.listenerPlan, beforeInstall)
		}
	} else {
		recreate = func(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance) error {
			return t.manager.recreateContainersInPlaceWithHook(ctx, instanceID, candidateDef, removeDef, appInst, beforeInstall)
		}
	}
	if err := recreate(t.ctx, t.spec.InstanceID, t.spec.CandidateDefinition, t.spec.PreviousDefinition, t.spec.AppInst); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	if err := t.verifyRuntimeReadiness(); err != nil {
		if restoreErr := t.rollback(err); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, err)
	}
	return nil
}

func (t *installedAppApplyTransaction) verifyRuntimeReadiness() error {
	if t.spec.MetadataOnly {
		return nil
	}
	endpoints, err := t.manager.manifestUpdateRuntimeEndpoints(t.spec.InstanceID, t.spec.CandidateDefinition, t.spec.PreviousDefinition, t.listenerPlan)
	if err != nil {
		return fmt.Errorf("resolve runtime readiness endpoints: %w", err)
	}
	probe := t.manager.runtimeReadinessProbe
	if probe == nil {
		probe = defaultRuntimeReadinessProbe
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseVerifyingReadiness, 75, "Verifying runtime readiness", false, nil)
	if err := probe(t.ctx, endpoints, manifestUpdateRuntimeReadinessTimeout); err != nil {
		return fmt.Errorf("verify candidate runtime readiness: %w", err)
	}
	return nil
}

func defaultRuntimeReadinessProbe(ctx context.Context, endpoints []services.ServiceEndpoint, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = manifestUpdateRuntimeReadinessTimeout
	}
	probeable := false
	for _, ep := range endpoints {
		if ep.Flow != api.FlowUDP {
			probeable = true
			break
		}
	}
	if !probeable {
		return fmt.Errorf("no probeable TCP runtime endpoints")
	}
	deadline := time.Now().Add(timeout)
	var lastFailures []string
	for {
		lastFailures = lastFailures[:0]
		for _, ep := range endpoints {
			if ep.Flow == api.FlowUDP {
				continue
			}
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.HostBind))
			dialer := net.Dialer{Timeout: 500 * time.Millisecond}
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				lastFailures = append(lastFailures, fmt.Sprintf("%s/%s %s: %v", ep.App, ep.Name, addr, err))
				continue
			}
			_ = conn.Close()
		}
		if len(lastFailures) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s: %s", timeout, strings.Join(lastFailures, "; "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func testRuntimeReadinessProbe(ctx context.Context, endpoints []services.ServiceEndpoint, timeout time.Duration) error {
	_ = ctx
	_ = endpoints
	_ = timeout
	return nil
}

func (t *installedAppApplyTransaction) prepareListenersIfNeeded() error {
	if t.listenerPlan != nil {
		return nil
	}
	if t.spec.PreviousDefinition != nil && reflect.DeepEqual(t.spec.PreviousDefinition.Listeners, t.spec.CandidateDefinition.Listeners) {
		return nil
	}
	plan, err := t.manager.serviceManager.PrepareReconcile(t.spec.InstanceID, t.spec.CandidateDefinition.Listeners)
	if err != nil {
		return fmt.Errorf("prepare listener publication: %w", err)
	}
	t.listenerPlan = plan
	t.txn.PreparedListenerEndpoints = plan.Endpoints()
	if err := t.storePhase("listeners_prepared"); err != nil {
		plan.Release()
		t.listenerPlan = nil
		t.txn.PreparedListenerEndpoints = nil
		return fmt.Errorf("persist listener prepare transaction marker: %w", err)
	}
	return nil
}

func (t *installedAppApplyTransaction) precommitDataSnapshotRequired() bool {
	return t.spec.RequiresPrecommitSnapshot || (t.runtimeStage != nil && t.runtimeStage.requiresPrecommitDataSnapshot)
}

func (t *installedAppApplyTransaction) preflightPrecommitDataSnapshotIfNeeded() error {
	if !t.precommitDataSnapshotRequired() {
		return nil
	}
	if strings.TrimSpace(t.txn.PrecommitDataSnapshotID) != "" {
		return nil
	}
	volumeManager := t.manager.currentVolumeManager()
	if _, ok := volumeManager.(dataVolumeSnapshotter); !ok {
		return fmt.Errorf("precommit data snapshot required but volume manager does not support snapshots")
	}
	if checker, ok := volumeManager.(dataSnapshotViabilityChecker); ok {
		if err := checker.CheckDataSnapshotViability(t.ctx, t.spec.InstanceID); err != nil {
			return fmt.Errorf("precommit data snapshot viability: %w", err)
		}
	}
	return nil
}

func (t *installedAppApplyTransaction) quiesceRuntimeForPrecommitDataSnapshotIfNeeded() error {
	if !t.precommitDataSnapshotRequired() {
		return nil
	}
	if t.spec.PreviousDefinition == nil {
		return fmt.Errorf("previous manifest required")
	}
	layout, err := t.manager.ensureAppVolumeLayout(t.ctx, t.spec.InstanceID)
	if err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}
	mode := piccoloModeFromExtensions(t.spec.PreviousDefinition.Extensions)
	runtime, err := t.manager.podmanRuntimeForApp(t.spec.InstanceID, layout, mode)
	if err != nil {
		return fmt.Errorf("podman runtime: %w", err)
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseStopping, 48, "Stopping containers", false, nil)
	if err := t.manager.stopContainersForMultiApp(t.ctx, t.spec.AppInst, t.spec.PreviousDefinition, runtime); err != nil {
		return err
	}
	return nil
}

func (t *installedAppApplyTransaction) createPrecommitDataSnapshotIfNeeded() error {
	if !t.precommitDataSnapshotRequired() {
		return nil
	}
	if strings.TrimSpace(t.txn.PrecommitDataSnapshotID) != "" {
		return nil
	}
	volumeManager := t.manager.currentVolumeManager()
	snapshotter, ok := volumeManager.(dataVolumeSnapshotter)
	if !ok {
		return fmt.Errorf("precommit data snapshot required but volume manager does not support snapshots")
	}
	snapshotID := manifestUpdatePrecommitSnapshotLVName(t.spec.InstanceID, t.txn.OperationID)
	t.txn.PrecommitDataSnapshotID = snapshotID
	t.txn.FailedDataLVName = manifestUpdateFailedDataLVName(t.spec.InstanceID, t.txn.OperationID)
	if err := t.storePhase("data_snapshot_planned"); err != nil {
		t.txn.PrecommitDataSnapshotID = ""
		t.txn.FailedDataLVName = ""
		return fmt.Errorf("persist precommit data snapshot plan: %w", err)
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseSnapshotting, 45, "Snapshotting data", false, nil)
	if err := snapshotter.SnapshotDataVolume(t.ctx, t.spec.InstanceID, snapshotID); err != nil {
		cleanupErr := snapshotter.DestroyDataSnapshot(t.ctx, snapshotID)
		if cleanupErr == nil {
			t.txn.PrecommitDataSnapshotID = ""
			t.txn.FailedDataLVName = ""
			_ = t.storePhase("data_snapshot_failed")
		}
		if cleanupErr != nil {
			return errors.Join(fmt.Errorf("create precommit data snapshot: %w", err), fmt.Errorf("cleanup failed precommit data snapshot %s: %w", snapshotID, cleanupErr))
		}
		return fmt.Errorf("create precommit data snapshot: %w", err)
	}
	if err := t.storePhase("data_snapshot_created"); err != nil {
		return fmt.Errorf("persist precommit data snapshot marker: %w", err)
	}
	if checker, ok := volumeManager.(dataSnapshotHealthChecker); ok {
		if err := checker.CheckDataSnapshotHealth(t.ctx, snapshotID); err != nil {
			return fmt.Errorf("precommit data snapshot health: %w", err)
		}
	}
	return nil
}

func (t *installedAppApplyTransaction) markCreatedOIDCClient(clientID string) error {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil
	}
	prevClientID := t.txn.CreatedOIDCClientID
	t.txn.CreatedOIDCClientID = clientID
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		t.txn.CreatedOIDCClientID = prevClientID
		cause := fmt.Errorf("persist oidc client transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) markProxyOIDCDeltaApplied() error {
	if t.txn.ProxyOIDCDeltaApplied {
		return nil
	}
	prevApplied := t.txn.ProxyOIDCDeltaApplied
	t.txn.ProxyOIDCDeltaApplied = true
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		t.txn.ProxyOIDCDeltaApplied = prevApplied
		cause := fmt.Errorf("persist proxy oidc transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) commitLedger(nextState *InstallState) error {
	if nextState == nil {
		return nil
	}
	t.txn.CreatedInstallState = t.txn.BackupInstallStatePath == ""
	t.txn.CandidateLedgerRevision = nextState.Revision
	t.txn.CandidateLedgerSourceHash = nextState.RawTemplateHash
	if err := t.storePhase("ledger_committing"); err != nil {
		cause := fmt.Errorf("persist ledger transaction marker: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhaseFinalizing, 85, t.spec.FinalizingMessage, false, nil)
	if err := t.state.StoreInstallState(t.spec.InstanceID, nextState); err != nil {
		cause := fmt.Errorf("persist config ledger: %w", err)
		if restoreErr := t.rollback(cause); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", t.spec.RollbackPrefix, cause)
	}
	return nil
}

func (t *installedAppApplyTransaction) suspendAccessForRuntimeSwitch() error {
	if t.spec.MetadataOnly || t.txn.AccessSuspended {
		return nil
	}
	prevSuspended := t.txn.AccessSuspended
	t.txn.AccessSuspended = true
	if err := t.storePhase("access_suspending"); err != nil {
		t.txn.AccessSuspended = prevSuspended
		return fmt.Errorf("persist access suspend transaction marker: %w", err)
	}
	if t.manager.serviceManager != nil {
		t.manager.serviceManager.SuspendAppPublication(t.spec.InstanceID)
	}
	if err := t.storePhase("access_suspended"); err != nil {
		return fmt.Errorf("persist access suspended transaction marker: %w", err)
	}
	return nil
}

func (t *installedAppApplyTransaction) publishAccess() error {
	if err := t.storePhase("publishing_access"); err != nil {
		if t.listenerPlan != nil {
			t.listenerPlan.RetainReservationsForRepair()
			t.listenerPlan = nil
		}
		return fmt.Errorf("persist access publication transaction marker: %w", err)
	}
	if t.listenerPlan != nil {
		t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhasePublishingAccess, 90, "Publishing access", false, nil)
		if _, _, err := t.listenerPlan.Publish(); err != nil {
			t.listenerPlan.RetainReservationsForRepair()
			t.listenerPlan = nil
			return fmt.Errorf("publish prepared listeners: %w", err)
		}
		t.listenerPlan = nil
	} else if t.txn.AccessSuspended && t.manager.serviceManager != nil {
		t.manager.emitProgress(t.ctx, t.spec.TaskType, t.spec.InstanceID, taskPhasePublishingAccess, 90, "Publishing access", false, nil)
		if err := t.manager.serviceManager.ResumeAppPublicationChecked(t.spec.InstanceID); err != nil {
			return fmt.Errorf("resume app publication: %w", err)
		}
	}
	proxyDeltaApplied := false
	if host := t.manager.currentSyncHost(); host != nil && proxyOIDCDeltaRequired(host, t.spec.PreviousDefinition, t.spec.CandidateDefinition) {
		if err := t.manager.applyProxyOIDCDelta(t.ctx, host, t.spec.InstanceID, t.spec.PreviousDefinition, t.spec.CandidateDefinition); err != nil {
			return err
		}
		proxyDeltaApplied = true
	}
	t.manager.configureOIDCAuthorizePaths(t.spec.InstanceID, t.spec.CandidateDefinition)
	prevSuspended := t.txn.AccessSuspended
	prevPublished := t.txn.AccessPublished
	prevProxyOIDCDeltaApplied := t.txn.ProxyOIDCDeltaApplied
	t.txn.AccessSuspended = false
	t.txn.AccessPublished = true
	if proxyDeltaApplied {
		t.txn.ProxyOIDCDeltaApplied = true
	}
	if err := t.storePhase("access_published"); err != nil {
		t.txn.AccessSuspended = prevSuspended
		t.txn.AccessPublished = prevPublished
		t.txn.ProxyOIDCDeltaApplied = prevProxyOIDCDeltaApplied
		return fmt.Errorf("persist access published transaction marker: %w", err)
	}
	return nil
}

func (t *installedAppApplyTransaction) markAccessRepairPending(cause error) string {
	message := fmt.Sprintf("Update committed, but access publication needs repair: %v", cause)
	t.txn.Phase = "publishing_access"
	t.txn.LastError = cause.Error()
	t.txn.AccessPublished = false
	t.txn.UpdatedAt = time.Now().UTC()
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		return message + fmt.Sprintf("; additionally failed to persist repair details: %v", err)
	}
	return message
}

func (t *installedAppApplyTransaction) markCatalogMetadataPending(cause error) {
	if cause == nil {
		return
	}
	t.txn.Phase = "committed_metadata_pending"
	t.txn.LastError = cause.Error()
	t.txn.UpdatedAt = time.Now().UTC()
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		log.Printf("WARN: %s %s: persist catalog metadata retry marker: %v", t.spec.OperationKind, t.spec.InstanceID, err)
	}
}

func (t *installedAppApplyTransaction) complete() {
	if err := t.storePhase("committed"); err != nil {
		log.Printf("WARN: %s %s: mark committed: %v", t.spec.OperationKind, t.spec.InstanceID, err)
	}
	if err := t.manager.cleanupCommittedManifestUpdateTransaction(t.ctx, t.state, t.spec.InstanceID, t.txn); err != nil {
		log.Printf("WARN: %s %s: cleanup committed transaction: %v", t.spec.OperationKind, t.spec.InstanceID, err)
	}
}

func (t *installedAppApplyTransaction) storePhase(phase string) error {
	prevPhase := t.txn.Phase
	prevUpdatedAt := t.txn.UpdatedAt
	t.txn.Phase = phase
	t.txn.UpdatedAt = time.Now().UTC()
	if err := t.state.StoreManifestUpdateTransaction(t.spec.InstanceID, t.txn); err != nil {
		t.txn.Phase = prevPhase
		t.txn.UpdatedAt = prevUpdatedAt
		return err
	}
	return nil
}

func (t *installedAppApplyTransaction) rollback(cause error) error {
	if t.listenerPlan != nil {
		t.listenerPlan.Release()
		t.listenerPlan = nil
	}
	return t.manager.restoreInstalledAppApplyFailure(
		t.ctx,
		t.state,
		t.spec.AppInst,
		t.spec.PreviousDefinition,
		t.spec.CandidateDefinition,
		t.txn,
		t.spec.TaskType,
		t.spec.OperationKind,
		cause,
	)
}
