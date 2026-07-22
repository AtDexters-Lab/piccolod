package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"piccolod/internal/fsutil"
)

const TransitionPlanSchemaVersion = 1

const transitionRecordFilename = "app_transition_v2.json"

var ErrTransitionPlanRejected = errors.New("transition plan rejected")

type TransitionOperationKind string

const (
	TransitionOperationModifyApp             TransitionOperationKind = "modify_app"
	TransitionOperationUpdateImage           TransitionOperationKind = "update_image"
	TransitionOperationEditConfig            TransitionOperationKind = "edit_config"
	TransitionOperationCatalogManifestReview TransitionOperationKind = "catalog_manifest_review"
	TransitionOperationCatalogConfigReview   TransitionOperationKind = "catalog_config_review"
	TransitionOperationCatalogAutoApply      TransitionOperationKind = "catalog_auto_apply"
	TransitionOperationAccessRepair          TransitionOperationKind = "access_repair"
	TransitionOperationCleanupRetry          TransitionOperationKind = "cleanup_retry"
	TransitionOperationMetadataRetry         TransitionOperationKind = "metadata_retry"
	TransitionOperationRuntimeRecovery       TransitionOperationKind = "runtime_recovery"
)

type TransitionSourceKind string

const (
	TransitionSourceCurrentCommitted  TransitionSourceKind = "current_committed"
	TransitionSourceCustomRaw         TransitionSourceKind = "custom_raw"
	TransitionSourceCatalogPending    TransitionSourceKind = "catalog_pending"
	TransitionSourceCatalogRendered   TransitionSourceKind = "catalog_rendered"
	TransitionSourceAutomaticRecovery TransitionSourceKind = "automatic_recovery"
)

type TransitionPhase string

const (
	TransitionPhasePrepared                 TransitionPhase = "prepared"
	TransitionPhaseResourcesPrepared        TransitionPhase = "resources_prepared"
	TransitionPhaseCommitIntent             TransitionPhase = "commit_intent"
	TransitionPhaseSwitchingRuntime         TransitionPhase = "switching_runtime"
	TransitionPhaseCandidateTouched         TransitionPhase = "candidate_touched"
	TransitionPhaseSourceCommitting         TransitionPhase = "source_committing"
	TransitionPhaseSourceCommitted          TransitionPhase = "source_committed"
	TransitionPhasePublishingAccess         TransitionPhase = "publishing_access"
	TransitionPhaseCommittedMetadataPending TransitionPhase = "committed_metadata_pending"
	TransitionPhaseCommittedCleanupPending  TransitionPhase = "committed_cleanup_pending"
	TransitionPhaseCommitted                TransitionPhase = "committed"
	TransitionPhaseRestoringPrevious        TransitionPhase = "restoring_previous"
	TransitionPhaseRestoreFailed            TransitionPhase = "restore_failed"
	TransitionPhaseRuntimeQuarantineIntent  TransitionPhase = "runtime_quarantine_intent"
	TransitionPhaseRuntimeQuarantined       TransitionPhase = "runtime_quarantined"
	TransitionPhaseRuntimeCleanCreated      TransitionPhase = "runtime_clean_created"
	TransitionPhaseRuntimeGroupCommitted    TransitionPhase = "runtime_group_committed"
)

type TransitionActionKind string

const (
	TransitionActionRefreshNow          TransitionActionKind = "refresh_now"
	TransitionActionPreviewRefresh      TransitionActionKind = "preview_refresh"
	TransitionActionReviewCatalogUpdate TransitionActionKind = "review_catalog_update"
	TransitionActionContinueConfig      TransitionActionKind = "continue_config_update"
	TransitionActionAccessRepair        TransitionActionKind = "access_repair"
	TransitionActionFinishCleanup       TransitionActionKind = "finish_cleanup"
	TransitionActionMetadataRetry       TransitionActionKind = "metadata_retry"
	TransitionActionDisabled            TransitionActionKind = "disabled"
)

type TransitionFenceEntryPoint string

const (
	TransitionFenceUpdateImage        TransitionFenceEntryPoint = "update_image"
	TransitionFenceModifyApp          TransitionFenceEntryPoint = "modify_app"
	TransitionFenceEditConfig         TransitionFenceEntryPoint = "edit_config"
	TransitionFenceStart              TransitionFenceEntryPoint = "start"
	TransitionFenceStop               TransitionFenceEntryPoint = "stop"
	TransitionFenceRollback           TransitionFenceEntryPoint = "rollback"
	TransitionFenceUninstall          TransitionFenceEntryPoint = "uninstall"
	TransitionFenceResizeStorage      TransitionFenceEntryPoint = "resize_storage"
	TransitionFenceCatalogSyncApply   TransitionFenceEntryPoint = "catalog_sync_apply"
	TransitionFenceCatalogSyncReview  TransitionFenceEntryPoint = "catalog_sync_review"
	TransitionFenceSyncEnable         TransitionFenceEntryPoint = "sync_enable"
	TransitionFenceSyncDisable        TransitionFenceEntryPoint = "sync_disable"
	TransitionFenceSyncTrigger        TransitionFenceEntryPoint = "sync_trigger"
	TransitionFenceSyncRefreshContext TransitionFenceEntryPoint = "sync_refresh_context"
	TransitionFenceListenerUpdate     TransitionFenceEntryPoint = "listener_update"
	TransitionFenceNormalReconcile    TransitionFenceEntryPoint = "normal_reconcile"
	TransitionFenceAccessRepair       TransitionFenceEntryPoint = "access_repair"
	TransitionFenceMetadataRetry      TransitionFenceEntryPoint = "metadata_retry"
	TransitionFenceCleanupRetry       TransitionFenceEntryPoint = "cleanup_retry"
	TransitionFenceShellExec          TransitionFenceEntryPoint = "shell_exec"
)

var transitionFenceEntryPoints = map[TransitionFenceEntryPoint]struct{}{
	TransitionFenceUpdateImage:        {},
	TransitionFenceModifyApp:          {},
	TransitionFenceEditConfig:         {},
	TransitionFenceStart:              {},
	TransitionFenceStop:               {},
	TransitionFenceRollback:           {},
	TransitionFenceUninstall:          {},
	TransitionFenceResizeStorage:      {},
	TransitionFenceCatalogSyncApply:   {},
	TransitionFenceCatalogSyncReview:  {},
	TransitionFenceSyncEnable:         {},
	TransitionFenceSyncDisable:        {},
	TransitionFenceSyncTrigger:        {},
	TransitionFenceSyncRefreshContext: {},
	TransitionFenceListenerUpdate:     {},
	TransitionFenceNormalReconcile:    {},
	TransitionFenceAccessRepair:       {},
	TransitionFenceMetadataRetry:      {},
	TransitionFenceCleanupRetry:       {},
	TransitionFenceShellExec:          {},
}

func TransitionEntryPointRequiresFence(entry TransitionFenceEntryPoint) bool {
	_, ok := transitionFenceEntryPoints[entry]
	return ok
}

func transitionFenceForSyncDisabled(disabled bool) TransitionFenceEntryPoint {
	if disabled {
		return TransitionFenceSyncDisable
	}
	return TransitionFenceSyncEnable
}

type TransitionImageRootfsDecision struct {
	EntryKind              string `json:"entry_kind,omitempty"`
	ServiceName            string `json:"service_name,omitempty"`
	Action                 string `json:"action,omitempty"`
	ImageRef               string `json:"image_ref,omitempty"`
	ResolvedDigest         string `json:"resolved_digest,omitempty"`
	CanonicalDigest        string `json:"canonical_digest,omitempty"`
	PlannedRootfsKey       string `json:"planned_rootfs_key,omitempty"`
	PreviousRootfsVolumeID string `json:"previous_rootfs_volume_id,omitempty"`
}

type TransitionDataPolicy struct {
	SnapshotRequired      bool   `json:"snapshot_required"`
	CandidateMayTouchData bool   `json:"candidate_may_touch_data"`
	SnapshotName          string `json:"snapshot_name,omitempty"`
	FailedLVName          string `json:"failed_lv_name,omitempty"`
	RollbackBehavior      string `json:"rollback_behavior,omitempty"`
	ViabilityDigest       string `json:"viability_digest,omitempty"`
}

type TransitionRuntimePolicy struct {
	RecreatePolicy             string            `json:"recreate_policy,omitempty"`
	RuntimeFingerprint         string            `json:"runtime_fingerprint,omitempty"`
	PreviousActiveRootfs       map[string]string `json:"previous_active_rootfs,omitempty"`
	CandidateActiveRootfs      map[string]string `json:"candidate_active_rootfs,omitempty"`
	PrimaryService             string            `json:"primary_service,omitempty"`
	CandidateRuntimeNamePolicy string            `json:"candidate_runtime_name_policy,omitempty"`
	ReadinessPolicy            string            `json:"readiness_policy,omitempty"`
	DisabledBehavior           string            `json:"disabled_behavior,omitempty"`
}

type TransitionAccessPolicy struct {
	PrepareRequired     bool     `json:"prepare_required"`
	PublicationStrategy string   `json:"publication_strategy,omitempty"`
	ReservationKeys     []string `json:"reservation_keys,omitempty"`
	ProxyOIDCDelta      string   `json:"proxy_oidc_delta,omitempty"`
}

type TransitionCleanupPolicy struct {
	StagedRootfsKeys             []string `json:"staged_rootfs_keys,omitempty"`
	SupersededRootfsKeys         []string `json:"superseded_rootfs_keys,omitempty"`
	RemovedRootfsKeys            []string `json:"removed_rootfs_keys,omitempty"`
	DataSnapshotNames            []string `json:"data_snapshot_names,omitempty"`
	FailedLVNames                []string `json:"failed_lv_names,omitempty"`
	GeneratedOIDCClientKeys      []string `json:"generated_oidc_client_keys,omitempty"`
	RetainedListenerReservations []string `json:"retained_listener_reservations,omitempty"`
}

type TransitionReviewPolicy struct {
	RequiredConfirmations []string             `json:"required_confirmations,omitempty"`
	ActionKind            TransitionActionKind `json:"action_kind,omitempty"`
}

type TransitionPlanInput struct {
	OperationKind              TransitionOperationKind
	SourceKind                 TransitionSourceKind
	PendingCatalogFlow         string
	ExpectedPendingCatalogFlow string
	Mode                       PiccoloMode
	Enabled                    bool
	RuntimeChanging            bool
	LegacyTransactionActive    bool
	BaseManifestHash           string
	CandidateManifestHash      string
	LedgerRevision             int64
	SourceHash                 string
	InputSchemaHash            string
	ImageRootfs                []TransitionImageRootfsDecision
	Data                       TransitionDataPolicy
	Runtime                    TransitionRuntimePolicy
	Access                     TransitionAccessPolicy
	Cleanup                    TransitionCleanupPolicy
	RequiredConfirmations      []string
	ResourceKeys               map[string]string
}

// TransitionPlan is the immutable apply-bound contract. Fields that can only be
// known after prepare/switch belong in TransitionRecord instead.
type TransitionPlan struct {
	SchemaVersion         int                             `json:"schema_version"`
	OperationKind         TransitionOperationKind         `json:"operation_kind"`
	SourceKind            TransitionSourceKind            `json:"source_kind"`
	PendingCatalogFlow    string                          `json:"pending_catalog_flow,omitempty"`
	BaseManifestHash      string                          `json:"base_manifest_hash,omitempty"`
	CandidateManifestHash string                          `json:"candidate_manifest_hash,omitempty"`
	LedgerRevision        int64                           `json:"ledger_revision,omitempty"`
	SourceHash            string                          `json:"source_hash,omitempty"`
	InputSchemaHash       string                          `json:"input_schema_hash,omitempty"`
	ImageRootfs           []TransitionImageRootfsDecision `json:"image_rootfs,omitempty"`
	Data                  TransitionDataPolicy            `json:"data,omitempty"`
	Runtime               TransitionRuntimePolicy         `json:"runtime,omitempty"`
	Access                TransitionAccessPolicy          `json:"access,omitempty"`
	Cleanup               TransitionCleanupPolicy         `json:"cleanup,omitempty"`
	Review                TransitionReviewPolicy          `json:"review,omitempty"`
	ResourceKeys          map[string]string               `json:"resource_keys,omitempty"`
}

type TransitionRecord struct {
	SchemaVersion         int                    `json:"schema_version"`
	OperationID           string                 `json:"operation_id"`
	InstanceID            string                 `json:"instance_id"`
	Phase                 TransitionPhase        `json:"phase"`
	PlanHash              string                 `json:"plan_hash"`
	Plan                  TransitionPlan         `json:"plan"`
	CatalogSourceSnapshot *CatalogSourceSnapshot `json:"catalog_source_snapshot,omitempty"`
	Resources             TransitionResources    `json:"resources,omitempty"`
	LastError             string                 `json:"last_error,omitempty"`
	CreatedAt             time.Time              `json:"created_at"`
	UpdatedAt             time.Time              `json:"updated_at"`
}

type CatalogSourceSnapshot struct {
	Flow string `json:"flow,omitempty"`
	Hash string `json:"hash,omitempty"`
}

func newCatalogSourceSnapshot(flow, hash string) *CatalogSourceSnapshot {
	flow = normalizePendingCatalogReviewFlow(flow)
	hash = strings.TrimSpace(hash)
	if flow == "" || hash == "" {
		return nil
	}
	return &CatalogSourceSnapshot{Flow: flow, Hash: hash}
}

func cloneCatalogSourceSnapshot(in *CatalogSourceSnapshot) *CatalogSourceSnapshot {
	if in == nil {
		return nil
	}
	return newCatalogSourceSnapshot(in.Flow, in.Hash)
}

func transitionSnapshotFromPlan(plan TransitionPlan) *CatalogSourceSnapshot {
	if plan.SourceKind != TransitionSourceCatalogPending {
		return nil
	}
	return newCatalogSourceSnapshot(plan.PendingCatalogFlow, plan.SourceHash)
}

type TransitionResources struct {
	StagedRootfs             map[string]string `json:"staged_rootfs,omitempty"`
	CreatedRootfs            map[string]string `json:"created_rootfs,omitempty"`
	DataSnapshots            map[string]string `json:"data_snapshots,omitempty"`
	FailedLVs                map[string]string `json:"failed_lvs,omitempty"`
	PreparedEndpoints        []string          `json:"prepared_endpoints,omitempty"`
	RetainedReservations     []string          `json:"retained_reservations,omitempty"`
	GeneratedOIDCClients     map[string]string `json:"generated_oidc_clients,omitempty"`
	CandidateActiveRootfs    map[string]string `json:"candidate_active_rootfs,omitempty"`
	CandidatePrimaryService  string            `json:"candidate_primary_service,omitempty"`
	CandidateNetworkAnchorID string            `json:"candidate_network_anchor_id,omitempty"`
	CandidateContainers      map[string]string `json:"candidate_containers,omitempty"`
	OriginalRuntimeRoot      string            `json:"original_runtime_root,omitempty"`
	QuarantineRuntimeRoot    string            `json:"quarantine_runtime_root,omitempty"`
	OriginalRunRoot          string            `json:"original_run_root,omitempty"`
	QuarantineRunRoot        string            `json:"quarantine_run_root,omitempty"`
}

func transitionImageRootfsFromManifestPlan(in []ManifestUpdateImagePlanItem) []TransitionImageRootfsDecision {
	out := make([]TransitionImageRootfsDecision, 0, len(in))
	for _, item := range in {
		out = append(out, TransitionImageRootfsDecision{
			EntryKind:              item.EntryKind,
			ServiceName:            item.ServiceName,
			Action:                 item.Action,
			ImageRef:               item.ImageRef,
			ResolvedDigest:         item.ResolvedDigest,
			CanonicalDigest:        item.CanonicalDigest,
			PlannedRootfsKey:       item.RootfsVolumeID,
			PreviousRootfsVolumeID: item.PreviousRootfsVolumeID,
		})
	}
	return out
}

func transitionRootfsKeysFromManifestPlan(in []ManifestUpdateImagePlanItem) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		if strings.TrimSpace(item.RootfsVolumeID) != "" {
			out = append(out, item.RootfsVolumeID)
		}
	}
	return out
}

func (p TransitionPlan) Hash() (string, error) {
	normalized, err := normalizeTransitionPlan(p)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("serialize transition plan: %w", err)
	}
	return Sha256Hex(data), nil
}

func DecodeTransitionPlanStrict(data []byte) (*TransitionPlan, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var plan TransitionPlan
	if err := dec.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode transition plan: %w", err)
	}
	var trailing struct{}
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("decode transition plan: trailing data")
	}
	if plan.SchemaVersion != TransitionPlanSchemaVersion {
		return nil, fmt.Errorf("unsupported transition plan schema version %d", plan.SchemaVersion)
	}
	return &plan, nil
}

func PlanInstalledAppTransition(in TransitionPlanInput) (*TransitionPlan, error) {
	if in.LegacyTransactionActive {
		return nil, fmt.Errorf("%w: legacy transition recovery is pending", ErrTransitionPlanRejected)
	}
	if in.OperationKind == "" {
		return nil, fmt.Errorf("%w: operation kind required", ErrTransitionPlanRejected)
	}
	if in.SourceKind == "" {
		return nil, fmt.Errorf("%w: source kind required", ErrTransitionPlanRejected)
	}
	if in.Mode == "" {
		in.Mode = ModeService
	}
	runtimeRecovery := in.OperationKind == TransitionOperationRuntimeRecovery
	if runtimeRecovery {
		if in.SourceKind != TransitionSourceAutomaticRecovery {
			return nil, fmt.Errorf("%w: runtime recovery requires automatic source", ErrTransitionPlanRejected)
		}
		if in.Mode != ModeService && in.Mode != ModeWorkspace {
			return nil, fmt.Errorf("%w: runtime recovery mode %s is unsupported", ErrTransitionPlanRejected, in.Mode)
		}
		if !in.Enabled || !in.RuntimeChanging {
			return nil, fmt.Errorf("%w: runtime recovery requires enabled runtime-changing app", ErrTransitionPlanRejected)
		}
		if len(in.ImageRootfs) != 0 || in.Data != (TransitionDataPolicy{}) ||
			!transitionAccessPolicyEmpty(in.Access) || !transitionCleanupPolicyEmpty(in.Cleanup) ||
			len(in.RequiredConfirmations) != 0 {
			return nil, fmt.Errorf("%w: runtime recovery cannot change data, rootfs, access, cleanup, or review policy", ErrTransitionPlanRejected)
		}
		for _, key := range []string{"original_runtime_root", "quarantine_runtime_root", "original_run_root", "quarantine_run_root"} {
			if strings.TrimSpace(in.ResourceKeys[key]) == "" {
				return nil, fmt.Errorf("%w: runtime recovery resource %s required", ErrTransitionPlanRejected, key)
			}
		}
	} else if in.Mode != ModeService {
		return nil, fmt.Errorf("%w: %s app updates are unsupported by installed app transition v2", ErrTransitionPlanRejected, in.Mode)
	}
	if in.RuntimeChanging && !in.Enabled {
		return nil, fmt.Errorf("%w: start app before applying runtime update", ErrTransitionPlanRejected)
	}
	if in.SourceKind == TransitionSourceCatalogPending {
		if strings.TrimSpace(in.ExpectedPendingCatalogFlow) == "" {
			return nil, fmt.Errorf("%w: pending catalog flow required", ErrTransitionPlanRejected)
		}
		if normalizePendingCatalogReviewFlow(in.PendingCatalogFlow) != normalizePendingCatalogReviewFlow(in.ExpectedPendingCatalogFlow) {
			return nil, fmt.Errorf("%w: pending catalog flow mismatch", ErrTransitionPlanRejected)
		}
	}
	plan := TransitionPlan{
		SchemaVersion:         TransitionPlanSchemaVersion,
		OperationKind:         in.OperationKind,
		SourceKind:            in.SourceKind,
		PendingCatalogFlow:    normalizePendingCatalogReviewFlow(in.PendingCatalogFlow),
		BaseManifestHash:      in.BaseManifestHash,
		CandidateManifestHash: in.CandidateManifestHash,
		LedgerRevision:        in.LedgerRevision,
		SourceHash:            in.SourceHash,
		InputSchemaHash:       in.InputSchemaHash,
		ImageRootfs:           in.ImageRootfs,
		Data:                  in.Data,
		Runtime:               in.Runtime,
		Access:                in.Access,
		Cleanup:               in.Cleanup,
		Review: TransitionReviewPolicy{
			RequiredConfirmations: in.RequiredConfirmations,
			ActionKind:            transitionActionForOperation(in.OperationKind, in.SourceKind),
		},
		ResourceKeys: in.ResourceKeys,
	}
	if in.SourceKind != TransitionSourceCatalogPending {
		plan.PendingCatalogFlow = ""
	}
	normalized, err := normalizeTransitionPlan(plan)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func transitionAccessPolicyEmpty(p TransitionAccessPolicy) bool {
	return !p.PrepareRequired && p.PublicationStrategy == "" && len(p.ReservationKeys) == 0 && p.ProxyOIDCDelta == ""
}

func transitionCleanupPolicyEmpty(p TransitionCleanupPolicy) bool {
	return len(p.StagedRootfsKeys) == 0 && len(p.SupersededRootfsKeys) == 0 &&
		len(p.RemovedRootfsKeys) == 0 && len(p.DataSnapshotNames) == 0 &&
		len(p.FailedLVNames) == 0 && len(p.GeneratedOIDCClientKeys) == 0 &&
		len(p.RetainedListenerReservations) == 0
}

func transitionActionForOperation(operation TransitionOperationKind, source TransitionSourceKind) TransitionActionKind {
	switch operation {
	case TransitionOperationUpdateImage:
		return TransitionActionRefreshNow
	case TransitionOperationModifyApp:
		if source == TransitionSourceCatalogPending {
			return TransitionActionReviewCatalogUpdate
		}
		return TransitionActionPreviewRefresh
	case TransitionOperationEditConfig:
		return TransitionActionContinueConfig
	case TransitionOperationCatalogManifestReview:
		return TransitionActionReviewCatalogUpdate
	case TransitionOperationCatalogConfigReview:
		return TransitionActionContinueConfig
	case TransitionOperationAccessRepair:
		return TransitionActionAccessRepair
	case TransitionOperationCleanupRetry:
		return TransitionActionFinishCleanup
	case TransitionOperationMetadataRetry:
		return TransitionActionMetadataRetry
	default:
		return TransitionActionDisabled
	}
}

func (fsm *FilesystemStateManager) StoreTransitionRecord(instanceID string, record *TransitionRecord) error {
	if record == nil {
		return fmt.Errorf("transition record required")
	}
	if record.SchemaVersion == 0 {
		record.SchemaVersion = TransitionPlanSchemaVersion
	}
	if record.SchemaVersion != TransitionPlanSchemaVersion {
		return fmt.Errorf("unsupported transition record schema version %d", record.SchemaVersion)
	}
	if record.InstanceID == "" {
		record.InstanceID = instanceID
	}
	if record.InstanceID != instanceID {
		return fmt.Errorf("transition record instance mismatch: %s != %s", record.InstanceID, instanceID)
	}
	planHash, err := record.Plan.Hash()
	if err != nil {
		return err
	}
	if strings.TrimSpace(record.PlanHash) == "" {
		record.PlanHash = planHash
	} else if record.PlanHash != planHash {
		return fmt.Errorf("transition record plan hash mismatch: %s != %s", record.PlanHash, planHash)
	}
	if record.Phase == "" {
		record.Phase = TransitionPhasePrepared
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if fsm.storeTransitionRecordHook != nil {
		if err := fsm.storeTransitionRecordHook(instanceID, record); err != nil {
			return err
		}
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()

	appDir := filepath.Join(fsm.appsDir, instanceID)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return fmt.Errorf("create app directory: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize transition record: %w", err)
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(appDir, transitionRecordFilename), data, 0600); err != nil {
		return fmt.Errorf("write transition record: %w", err)
	}
	return nil
}

func (fsm *FilesystemStateManager) LoadTransitionRecord(instanceID string) (*TransitionRecord, error) {
	path := filepath.Join(fsm.appsDir, instanceID, transitionRecordFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record TransitionRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return nil, fmt.Errorf("parse transition record: %w", err)
	}
	var trailing struct{}
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("parse transition record: trailing data")
	}
	if record.SchemaVersion != TransitionPlanSchemaVersion {
		return nil, fmt.Errorf("unsupported transition record schema version %d", record.SchemaVersion)
	}
	if record.InstanceID == "" {
		record.InstanceID = instanceID
	}
	if record.InstanceID != instanceID {
		return nil, fmt.Errorf("transition record instance mismatch: %s != %s", record.InstanceID, instanceID)
	}
	planHash, err := record.Plan.Hash()
	if err != nil {
		return nil, err
	}
	if record.PlanHash != planHash {
		return nil, fmt.Errorf("transition record plan hash mismatch: %s != %s", record.PlanHash, planHash)
	}
	return &record, nil
}

func (fsm *FilesystemStateManager) ClearTransitionRecord(instanceID string) error {
	if fsm.clearTransitionRecordHook != nil {
		if err := fsm.clearTransitionRecordHook(instanceID); err != nil {
			return err
		}
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	path := filepath.Join(fsm.appsDir, instanceID, transitionRecordFilename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RejectIfTransitionInProgress fences legacy or independently-owned mutation
// paths while a v2 installed-app transition owns the app's runtime boundary.
func (m *AppManager) RejectIfTransitionInProgress(instanceID string, entry TransitionFenceEntryPoint) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	return m.rejectIfTransitionInProgress(state, instanceID, entry)
}

func (m *AppManager) rejectIfTransitionInProgress(state *FilesystemStateManager, instanceID string, entry TransitionFenceEntryPoint) error {
	if !TransitionEntryPointRequiresFence(entry) {
		return nil
	}
	record, err := state.LoadTransitionRecord(instanceID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read app transition record: %w", err)
	}
	if record == nil || record.Phase == TransitionPhaseCommitted {
		return nil
	}
	return fmt.Errorf("%w: %s phase=%s operation=%s entry=%s", ErrTransitionInProgress, instanceID, record.Phase, record.Plan.OperationKind, entry)
}

func storeTransitionRecordForManifestTransaction(state *FilesystemStateManager, instanceID string, txn *ManifestUpdateTransaction, appInst *AppInstance, phase TransitionPhase) error {
	record, err := state.LoadTransitionRecord(instanceID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	record.Phase = phase
	record.Resources = transitionResourcesFromManifestTransaction(txn, appInst)
	if txn != nil {
		record.LastError = txn.LastError
	}
	return state.StoreTransitionRecord(instanceID, record)
}

func normalizeTransitionPlan(p TransitionPlan) (TransitionPlan, error) {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = TransitionPlanSchemaVersion
	}
	if p.SchemaVersion != TransitionPlanSchemaVersion {
		return TransitionPlan{}, fmt.Errorf("unsupported transition plan schema version %d", p.SchemaVersion)
	}
	p.ImageRootfs = normalizeTransitionImageRootfs(p.ImageRootfs)
	p.Runtime.PreviousActiveRootfs = normalizeStringMap(p.Runtime.PreviousActiveRootfs)
	p.Runtime.CandidateActiveRootfs = normalizeStringMap(p.Runtime.CandidateActiveRootfs)
	p.Access.ReservationKeys = sortedUniqueStrings(p.Access.ReservationKeys)
	p.Cleanup.StagedRootfsKeys = sortedUniqueStrings(p.Cleanup.StagedRootfsKeys)
	p.Cleanup.SupersededRootfsKeys = sortedUniqueStrings(p.Cleanup.SupersededRootfsKeys)
	p.Cleanup.RemovedRootfsKeys = sortedUniqueStrings(p.Cleanup.RemovedRootfsKeys)
	p.Cleanup.DataSnapshotNames = sortedUniqueStrings(p.Cleanup.DataSnapshotNames)
	p.Cleanup.FailedLVNames = sortedUniqueStrings(p.Cleanup.FailedLVNames)
	p.Cleanup.GeneratedOIDCClientKeys = sortedUniqueStrings(p.Cleanup.GeneratedOIDCClientKeys)
	p.Cleanup.RetainedListenerReservations = sortedUniqueStrings(p.Cleanup.RetainedListenerReservations)
	p.Review.RequiredConfirmations = sortedUniqueStrings(p.Review.RequiredConfirmations)
	p.ResourceKeys = normalizeStringMap(p.ResourceKeys)
	return p, nil
}

func normalizeTransitionImageRootfs(in []TransitionImageRootfsDecision) []TransitionImageRootfsDecision {
	out := make([]TransitionImageRootfsDecision, 0, len(in))
	for _, item := range in {
		item.EntryKind = strings.TrimSpace(item.EntryKind)
		item.ServiceName = strings.TrimSpace(item.ServiceName)
		item.Action = strings.TrimSpace(item.Action)
		item.ImageRef = strings.TrimSpace(item.ImageRef)
		item.CanonicalDigest = canonicalImageDigestKey(firstNonEmpty(item.CanonicalDigest, item.ResolvedDigest))
		item.ResolvedDigest = item.CanonicalDigest
		item.PlannedRootfsKey = strings.TrimSpace(item.PlannedRootfsKey)
		item.PreviousRootfsVolumeID = strings.TrimSpace(item.PreviousRootfsVolumeID)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.EntryKind != b.EntryKind {
			return a.EntryKind < b.EntryKind
		}
		if a.ServiceName != b.ServiceName {
			return a.ServiceName < b.ServiceName
		}
		if a.ImageRef != b.ImageRef {
			return a.ImageRef < b.ImageRef
		}
		return a.PlannedRootfsKey < b.PlannedRootfsKey
	})
	return out
}

func normalizeStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(v)
	}
	return out
}

func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func TransitionAllowsPendingCatalogRefresh(record *TransitionRecord) bool {
	if record == nil || record.CatalogSourceSnapshot == nil || strings.TrimSpace(record.CatalogSourceSnapshot.Hash) == "" {
		return true
	}
	return record.Phase == TransitionPhaseCommitted
}

type TransitionCatalogPolicy string

const (
	TransitionCatalogPolicyNoop              TransitionCatalogPolicy = "noop"
	TransitionCatalogPolicyLiveMetadataApply TransitionCatalogPolicy = "live_metadata_apply"
	TransitionCatalogPolicyAutoApply         TransitionCatalogPolicy = "auto_apply"
	TransitionCatalogPolicyManifestReview    TransitionCatalogPolicy = "manifest_review"
	TransitionCatalogPolicyConfigReview      TransitionCatalogPolicy = "config_review"
	TransitionCatalogPolicyFailClosed        TransitionCatalogPolicy = "fail_closed"
)

func TransitionCatalogPolicyForDiff(diff DiffKind, reviewRequired bool, configReview bool) TransitionCatalogPolicy {
	switch diff {
	case DiffKindNone:
		return TransitionCatalogPolicyNoop
	case DiffKindOIDCLibraryOnly:
		return TransitionCatalogPolicyLiveMetadataApply
	case DiffKindStructuralNoImage:
		if !reviewRequired {
			return TransitionCatalogPolicyAutoApply
		}
		if configReview {
			return TransitionCatalogPolicyConfigReview
		}
		return TransitionCatalogPolicyManifestReview
	case DiffKindImageOnly, DiffKindStructuralWithImage:
		return TransitionCatalogPolicyManifestReview
	default:
		return TransitionCatalogPolicyFailClosed
	}
}
