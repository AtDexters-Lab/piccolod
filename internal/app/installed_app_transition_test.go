package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piccolod/internal/api"
)

func TestTransitionPlanHashCanonicalizesOrderingAndEmptyValues(t *testing.T) {
	a := TransitionPlan{
		SchemaVersion:         TransitionPlanSchemaVersion,
		OperationKind:         TransitionOperationModifyApp,
		SourceKind:            TransitionSourceCustomRaw,
		BaseManifestHash:      "base",
		CandidateManifestHash: "candidate",
		LedgerRevision:        7,
		SourceHash:            "source",
		ImageRootfs: []TransitionImageRootfsDecision{
			{EntryKind: "service", ServiceName: "web", ImageRef: "repo/web:latest", ResolvedDigest: "docker.io/acme/web@sha256:bbbb", PlannedRootfsKey: "rootfs-web"},
			{EntryKind: "service", ServiceName: "api", ImageRef: "repo/api:latest", CanonicalDigest: "sha256:aaaa", PlannedRootfsKey: "rootfs-api"},
		},
		Runtime: TransitionRuntimePolicy{
			RuntimeFingerprint:   "runtime",
			PreviousActiveRootfs: nil,
			CandidateActiveRootfs: map[string]string{
				"web": "rootfs-web",
				"api": "rootfs-api",
			},
		},
		Access: TransitionAccessPolicy{
			ReservationKeys: []string{"8443/tcp", "8080/tcp", "8443/tcp"},
		},
		Cleanup: TransitionCleanupPolicy{
			StagedRootfsKeys:             []string{"rootfs-web", "rootfs-api"},
			RetainedListenerReservations: []string{"8443/tcp", "8080/tcp"},
		},
		Review: TransitionReviewPolicy{
			RequiredConfirmations: []string{"storage-reviewed", "access-reviewed", "access-reviewed"},
		},
		ResourceKeys: map[string]string{
			"snapshot": "snap-app-piclu--v2",
			"rootfs":   "rootfs-api",
		},
	}
	b := TransitionPlan{
		SchemaVersion:         TransitionPlanSchemaVersion,
		OperationKind:         TransitionOperationModifyApp,
		SourceKind:            TransitionSourceCustomRaw,
		BaseManifestHash:      "base",
		CandidateManifestHash: "candidate",
		LedgerRevision:        7,
		SourceHash:            "source",
		ImageRootfs: []TransitionImageRootfsDecision{
			{EntryKind: "service", ServiceName: "api", ImageRef: "repo/api:latest", ResolvedDigest: "sha256:aaaa", PlannedRootfsKey: "rootfs-api"},
			{EntryKind: "service", ServiceName: "web", ImageRef: "repo/web:latest", CanonicalDigest: "sha256:bbbb", PlannedRootfsKey: "rootfs-web"},
		},
		Runtime: TransitionRuntimePolicy{
			RuntimeFingerprint:   "runtime",
			PreviousActiveRootfs: map[string]string{},
			CandidateActiveRootfs: map[string]string{
				"api": "rootfs-api",
				"web": "rootfs-web",
			},
		},
		Access: TransitionAccessPolicy{
			ReservationKeys: []string{"8080/tcp", "8443/tcp"},
		},
		Cleanup: TransitionCleanupPolicy{
			StagedRootfsKeys:             []string{"rootfs-api", "rootfs-web"},
			RetainedListenerReservations: []string{"8080/tcp", "8443/tcp"},
		},
		Review: TransitionReviewPolicy{
			RequiredConfirmations: []string{"access-reviewed", "storage-reviewed"},
		},
		ResourceKeys: map[string]string{
			"rootfs":   "rootfs-api",
			"snapshot": "snap-app-piclu--v2",
		},
	}

	hashA, err := a.Hash()
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	hashB, err := b.Hash()
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	if hashA != hashB {
		t.Fatalf("canonical hashes differ:\nA=%s\nB=%s", hashA, hashB)
	}
}

func TestTransitionPlanHashExcludesRecordOnlyFacts(t *testing.T) {
	plan := TransitionPlan{
		SchemaVersion:    TransitionPlanSchemaVersion,
		OperationKind:    TransitionOperationUpdateImage,
		SourceKind:       TransitionSourceCurrentCommitted,
		BaseManifestHash: "base",
		Runtime:          TransitionRuntimePolicy{RuntimeFingerprint: "runtime"},
		Review:           TransitionReviewPolicy{ActionKind: TransitionActionRefreshNow},
	}
	hash, err := plan.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	record := TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationID:   "op-1",
		InstanceID:    "piclu",
		Phase:         TransitionPhaseResourcesPrepared,
		PlanHash:      hash,
		Plan:          plan,
		Resources: TransitionResources{
			StagedRootfs:      map[string]string{"app": "rootfs-new"},
			PreparedEndpoints: []string{"https://piclu.example"},
		},
		LastError: "display-only preparation note",
	}
	recordHash, err := record.Plan.Hash()
	if err != nil {
		t.Fatalf("record plan hash: %v", err)
	}
	if recordHash != hash {
		t.Fatalf("record-only facts changed plan hash: %s != %s", recordHash, hash)
	}
}

func TestDecodeTransitionPlanStrictRejectsUnknownAndSchemaVersion(t *testing.T) {
	raw := []byte(`{"schema_version":1,"operation_kind":"modify_app","source_kind":"custom_raw","display_text":"not hash state"}`)
	if _, err := DecodeTransitionPlanStrict(raw); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field rejection, got %v", err)
	}
	raw = []byte(`{"schema_version":999,"operation_kind":"modify_app","source_kind":"custom_raw"}`)
	if _, err := DecodeTransitionPlanStrict(raw); err == nil || !strings.Contains(err.Error(), "unsupported transition plan schema version") {
		t.Fatalf("expected schema version rejection, got %v", err)
	}
	raw = []byte(`{"schema_version":1,"operation_kind":"modify_app","source_kind":"custom_raw"} {"schema_version":1}`)
	if _, err := DecodeTransitionPlanStrict(raw); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("expected trailing data rejection, got %v", err)
	}
}

func TestTransitionCatalogPolicyForDiff(t *testing.T) {
	tests := []struct {
		name           string
		diff           DiffKind
		reviewRequired bool
		configReview   bool
		want           TransitionCatalogPolicy
	}{
		{name: "none", diff: DiffKindNone, want: TransitionCatalogPolicyNoop},
		{name: "oidc", diff: DiffKindOIDCLibraryOnly, want: TransitionCatalogPolicyLiveMetadataApply},
		{name: "structural allowed", diff: DiffKindStructuralNoImage, want: TransitionCatalogPolicyAutoApply},
		{name: "structural manifest review", diff: DiffKindStructuralNoImage, reviewRequired: true, want: TransitionCatalogPolicyManifestReview},
		{name: "structural config review", diff: DiffKindStructuralNoImage, reviewRequired: true, configReview: true, want: TransitionCatalogPolicyConfigReview},
		{name: "image only", diff: DiffKindImageOnly, want: TransitionCatalogPolicyManifestReview},
		{name: "structural with image", diff: DiffKindStructuralWithImage, want: TransitionCatalogPolicyManifestReview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TransitionCatalogPolicyForDiff(tt.diff, tt.reviewRequired, tt.configReview); got != tt.want {
				t.Fatalf("policy = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPlanInstalledAppTransitionRejectsUnsupportedModesAndLegacyRepair(t *testing.T) {
	tests := []struct {
		name string
		in   TransitionPlanInput
		want string
	}{
		{
			name: "workspace",
			in: TransitionPlanInput{
				OperationKind: TransitionOperationModifyApp,
				SourceKind:    TransitionSourceCustomRaw,
				Mode:          ModeWorkspace,
				Enabled:       true,
			},
			want: "workspace app updates are unsupported",
		},
		{
			name: "disabled runtime changing",
			in: TransitionPlanInput{
				OperationKind:   TransitionOperationModifyApp,
				SourceKind:      TransitionSourceCustomRaw,
				Mode:            ModeService,
				RuntimeChanging: true,
			},
			want: "start app before applying runtime update",
		},
		{
			name: "legacy active",
			in: TransitionPlanInput{
				OperationKind:           TransitionOperationUpdateImage,
				SourceKind:              TransitionSourceCurrentCommitted,
				Mode:                    ModeService,
				Enabled:                 true,
				LegacyTransactionActive: true,
			},
			want: "legacy transition recovery is pending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := PlanInstalledAppTransition(tt.in); err == nil || !errors.Is(err, ErrTransitionPlanRejected) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("plan err = %v, want rejection containing %q", err, tt.want)
			}
		})
	}
}

func TestPlanInstalledAppTransitionRejectsPendingCatalogFlowMismatch(t *testing.T) {
	_, err := PlanInstalledAppTransition(TransitionPlanInput{
		OperationKind:              TransitionOperationCatalogManifestReview,
		SourceKind:                 TransitionSourceCatalogPending,
		PendingCatalogFlow:         pendingCatalogReviewFlowConfig,
		ExpectedPendingCatalogFlow: pendingCatalogReviewFlowManifest,
		Mode:                       ModeService,
		Enabled:                    true,
	})
	if err == nil || !errors.Is(err, ErrTransitionPlanRejected) || !strings.Contains(err.Error(), "pending catalog flow mismatch") {
		t.Fatalf("plan err = %v, want pending catalog flow mismatch", err)
	}
}

func TestPlanInstalledAppTransitionDerivesReviewAction(t *testing.T) {
	plan, err := PlanInstalledAppTransition(TransitionPlanInput{
		OperationKind:              TransitionOperationModifyApp,
		SourceKind:                 TransitionSourceCatalogPending,
		PendingCatalogFlow:         pendingCatalogReviewFlowManifest,
		ExpectedPendingCatalogFlow: pendingCatalogReviewFlowManifest,
		Mode:                       ModeService,
		Enabled:                    true,
		RuntimeChanging:            true,
		BaseManifestHash:           "base",
		CandidateManifestHash:      "candidate",
		ImageRootfs: []TransitionImageRootfsDecision{
			{ServiceName: "app", ImageRef: "repo/app:latest", ResolvedDigest: "docker.io/acme/app@sha256:abcd"},
		},
		RequiredConfirmations: []string{"runtime-reviewed", "access-reviewed"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Review.ActionKind != TransitionActionReviewCatalogUpdate {
		t.Fatalf("action kind = %s, want %s", plan.Review.ActionKind, TransitionActionReviewCatalogUpdate)
	}
	if plan.PendingCatalogFlow != pendingCatalogReviewFlowManifest {
		t.Fatalf("pending flow = %q, want %q", plan.PendingCatalogFlow, pendingCatalogReviewFlowManifest)
	}
	if got := plan.ImageRootfs[0].ResolvedDigest; got != "sha256:abcd" {
		t.Fatalf("resolved digest = %q, want canonical digest", got)
	}
	if got := strings.Join(plan.Review.RequiredConfirmations, ","); got != "access-reviewed,runtime-reviewed" {
		t.Fatalf("confirmations = %q", got)
	}
}

func TestPlanInstalledAppTransitionIgnoresCallerLifecycleHint(t *testing.T) {
	in := TransitionPlanInput{
		OperationKind: TransitionOperationUpdateImage,
		SourceKind:    TransitionSourceCurrentCommitted,
		Mode:          ModeService,
		Enabled:       true,
	}
	plan, err := PlanInstalledAppTransition(in)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Review.ActionKind != TransitionActionRefreshNow {
		t.Fatalf("action kind = %s, want %s", plan.Review.ActionKind, TransitionActionRefreshNow)
	}
}

func TestTransitionAllowsPendingCatalogRefresh(t *testing.T) {
	if !TransitionAllowsPendingCatalogRefresh(nil) {
		t.Fatalf("nil record should allow pending refresh")
	}
	record := &TransitionRecord{
		Phase: TransitionPhasePrepared,
		CatalogSourceSnapshot: &CatalogSourceSnapshot{
			Flow: "manifest_review",
			Hash: "source-a",
		},
	}
	if TransitionAllowsPendingCatalogRefresh(record) {
		t.Fatalf("active transition with source snapshot must freeze pending source")
	}
	record.Phase = TransitionPhaseCommittedMetadataPending
	if TransitionAllowsPendingCatalogRefresh(record) {
		t.Fatalf("metadata-pending transition must keep pending source frozen")
	}
	record.Phase = TransitionPhaseCommitted
	if !TransitionAllowsPendingCatalogRefresh(record) {
		t.Fatalf("committed transition should allow next sync refresh")
	}
}

func TestTransitionFenceEntryPointsCoverSideEffectRoutes(t *testing.T) {
	required := []TransitionFenceEntryPoint{
		TransitionFenceUpdateImage,
		TransitionFenceModifyApp,
		TransitionFenceEditConfig,
		TransitionFenceStart,
		TransitionFenceStop,
		TransitionFenceRollback,
		TransitionFenceUninstall,
		TransitionFenceResizeStorage,
		TransitionFenceCatalogSyncApply,
		TransitionFenceCatalogSyncReview,
		TransitionFenceSyncEnable,
		TransitionFenceSyncDisable,
		TransitionFenceSyncTrigger,
		TransitionFenceSyncRefreshContext,
		TransitionFenceListenerUpdate,
		TransitionFenceNormalReconcile,
		TransitionFenceAccessRepair,
		TransitionFenceMetadataRetry,
		TransitionFenceCleanupRetry,
		TransitionFenceShellExec,
	}
	for _, entry := range required {
		if !TransitionEntryPointRequiresFence(entry) {
			t.Fatalf("%s should require transition fence", entry)
		}
	}
}

func TestTransitionPlanRoundTripStrict(t *testing.T) {
	plan := TransitionPlan{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationKind: TransitionOperationCatalogManifestReview,
		SourceKind:    TransitionSourceCatalogPending,
		Review:        TransitionReviewPolicy{ActionKind: TransitionActionReviewCatalogUpdate},
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeTransitionPlanStrict(data)
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if got.OperationKind != plan.OperationKind || got.SourceKind != plan.SourceKind {
		t.Fatalf("round trip = %#v, want %#v", got, plan)
	}
}

func TestStoreLoadClearTransitionRecord(t *testing.T) {
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	record := &TransitionRecord{
		OperationID: "op-1",
		InstanceID:  "piclu",
		Plan: TransitionPlan{
			SchemaVersion:         TransitionPlanSchemaVersion,
			OperationKind:         TransitionOperationModifyApp,
			SourceKind:            TransitionSourceCustomRaw,
			BaseManifestHash:      "base",
			CandidateManifestHash: "candidate",
			Review:                TransitionReviewPolicy{ActionKind: TransitionActionPreviewRefresh},
		},
		Resources: TransitionResources{
			StagedRootfs:      map[string]string{"app": "rootfs-new"},
			PreparedEndpoints: []string{"8443/tcp"},
		},
	}
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition record: %v", err)
	}
	if record.SchemaVersion != TransitionPlanSchemaVersion {
		t.Fatalf("record schema version = %d", record.SchemaVersion)
	}
	if record.Phase != TransitionPhasePrepared {
		t.Fatalf("record phase = %s, want %s", record.Phase, TransitionPhasePrepared)
	}
	if record.PlanHash == "" {
		t.Fatalf("store did not derive plan hash")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		t.Fatalf("store did not stamp record timestamps: %+v", record)
	}
	info, err := os.Stat(filepath.Join(state.appsDir, "piclu", transitionRecordFilename))
	if err != nil {
		t.Fatalf("stat transition record: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("transition record mode = %o, want 0600", got)
	}
	loaded, err := state.LoadTransitionRecord("piclu")
	if err != nil {
		t.Fatalf("load transition record: %v", err)
	}
	if loaded.PlanHash != record.PlanHash || loaded.OperationID != record.OperationID {
		t.Fatalf("loaded record mismatch: got %+v want %+v", loaded, record)
	}
	if got := loaded.Resources.StagedRootfs["app"]; got != "rootfs-new" {
		t.Fatalf("loaded staged rootfs = %q", got)
	}
	if err := state.ClearTransitionRecord("piclu"); err != nil {
		t.Fatalf("clear transition record: %v", err)
	}
	if _, err := state.LoadTransitionRecord("piclu"); !os.IsNotExist(err) {
		t.Fatalf("load after clear err = %v, want not exist", err)
	}
}

func TestInstalledAppApplyStorePhaseKeepsLegacyPhaseWhenTransitionProjectionFails(t *testing.T) {
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	plan := TransitionPlan{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationKind: TransitionOperationModifyApp,
		SourceKind:    TransitionSourceCustomRaw,
	}
	txn := &ManifestUpdateTransaction{
		OperationID:             "op-1",
		OperationKind:           string(TransitionOperationModifyApp),
		Phase:                   "prepared",
		PreviousManifestHash:    "base",
		CandidateManifestHash:   "candidate",
		DryRunToken:             "dry-run",
		RuntimeFingerprint:      "runtime",
		PreviousLedgerRevision:  1,
		CandidateLedgerRevision: 2,
	}
	if err := state.StoreManifestUpdateTransaction("piclu", txn); err != nil {
		t.Fatalf("store legacy transaction: %v", err)
	}
	record := &TransitionRecord{
		OperationID: "op-1",
		InstanceID:  "piclu",
		Phase:       TransitionPhasePrepared,
		Plan:        plan,
	}
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition record: %v", err)
	}
	state.storeTransitionRecordHook = func(instanceID string, record *TransitionRecord) error {
		if instanceID == "piclu" && record.Phase == TransitionPhaseResourcesPrepared {
			return errors.New("injected transition projection failure")
		}
		return nil
	}
	applyTxn := &installedAppApplyTransaction{
		state:      state,
		spec:       installedAppApplyTransactionSpec{InstanceID: "piclu"},
		txn:        txn,
		transition: record,
	}
	err = applyTxn.storePhase("rootfs_staged")
	if err == nil || !strings.Contains(err.Error(), "injected transition projection failure") {
		t.Fatalf("storePhase err = %v, want injected transition projection failure", err)
	}
	if applyTxn.txn.Phase != "rootfs_staged" {
		t.Fatalf("in-memory legacy phase = %q, want rootfs_staged", applyTxn.txn.Phase)
	}
	legacy, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load legacy transaction: %v", err)
	}
	if legacy.Phase != "rootfs_staged" {
		t.Fatalf("durable legacy phase = %q, want rootfs_staged", legacy.Phase)
	}
	transition, err := state.LoadTransitionRecord("piclu")
	if err != nil {
		t.Fatalf("load transition record: %v", err)
	}
	if transition.Phase != TransitionPhasePrepared {
		t.Fatalf("transition projection phase = %s, want %s", transition.Phase, TransitionPhasePrepared)
	}
}

func TestStoreTransitionRecordRejectsPlanHashDrift(t *testing.T) {
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	record := &TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		InstanceID:    "piclu",
		Phase:         TransitionPhasePrepared,
		PlanHash:      "not-the-plan-hash",
		Plan: TransitionPlan{
			SchemaVersion: TransitionPlanSchemaVersion,
			OperationKind: TransitionOperationUpdateImage,
			SourceKind:    TransitionSourceCurrentCommitted,
		},
	}
	if err := state.StoreTransitionRecord("piclu", record); err == nil || !strings.Contains(err.Error(), "plan hash mismatch") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestLoadTransitionRecordRejectsPlanHashDrift(t *testing.T) {
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	record := &TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationID:   "op-1",
		InstanceID:    "piclu",
		Phase:         TransitionPhasePrepared,
		Plan: TransitionPlan{
			SchemaVersion: TransitionPlanSchemaVersion,
			OperationKind: TransitionOperationUpdateImage,
			SourceKind:    TransitionSourceCurrentCommitted,
		},
	}
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition record: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(state.appsDir, "piclu", transitionRecordFilename))
	if err != nil {
		t.Fatalf("read transition record: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode raw transition record: %v", err)
	}
	raw["plan_hash"] = "sha256-drift"
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal drifted record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state.appsDir, "piclu", transitionRecordFilename), data, 0600); err != nil {
		t.Fatalf("write drifted record: %v", err)
	}
	if _, err := state.LoadTransitionRecord("piclu"); err == nil || !strings.Contains(err.Error(), "plan hash mismatch") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestRejectIfTransitionInProgress(t *testing.T) {
	mgr, err := NewAppManagerForTest(nil, t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	if err := mgr.RejectIfTransitionInProgress("piclu", TransitionFenceUpdateImage); err != nil {
		t.Fatalf("empty transition guard: %v", err)
	}
	record := &TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationID:   "op-1",
		InstanceID:    "piclu",
		Phase:         TransitionPhaseResourcesPrepared,
		Plan: TransitionPlan{
			SchemaVersion: TransitionPlanSchemaVersion,
			OperationKind: TransitionOperationModifyApp,
			SourceKind:    TransitionSourceCustomRaw,
		},
	}
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition: %v", err)
	}
	err = mgr.RejectIfTransitionInProgress("piclu", TransitionFenceUpdateImage)
	if !errors.Is(err, ErrTransitionInProgress) {
		t.Fatalf("guard err = %v, want ErrTransitionInProgress", err)
	}
	record.Phase = TransitionPhaseCommitted
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store committed transition: %v", err)
	}
	if err := mgr.RejectIfTransitionInProgress("piclu", TransitionFenceUpdateImage); err != nil {
		t.Fatalf("committed transition should not fence: %v", err)
	}
}

func TestRecoverPendingTransitionRecordsClearsPreparedWithoutLegacyJournal(t *testing.T) {
	mgr, err := NewAppManagerForTest(nil, t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	if err := state.StoreApp(transitionTestAppInstance("piclu")); err != nil {
		t.Fatalf("store app: %v", err)
	}
	record := transitionTestRecord("piclu", TransitionPhasePrepared)
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition: %v", err)
	}

	blocked := mgr.recoverPendingTransitionRecords(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("prepared transition without legacy journal should not block: %v", blocked)
	}
	if _, err := state.LoadTransitionRecord("piclu"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want cleared", err)
	}
}

func TestRecoverPendingTransitionRecordsBlocksActiveWithoutLegacyJournal(t *testing.T) {
	mgr, err := NewAppManagerForTest(nil, t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	if err := state.StoreApp(transitionTestAppInstance("piclu")); err != nil {
		t.Fatalf("store app: %v", err)
	}
	record := transitionTestRecord("piclu", TransitionPhaseCandidateTouched)
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition: %v", err)
	}

	blocked := mgr.recoverPendingTransitionRecords(context.Background(), state)
	if !blocked["piclu"] {
		t.Fatalf("active transition without legacy journal should block: %v", blocked)
	}
	loaded, err := state.LoadTransitionRecord("piclu")
	if err != nil {
		t.Fatalf("load transition: %v", err)
	}
	if loaded.Phase != TransitionPhaseCandidateTouched {
		t.Fatalf("transition phase = %s, want %s", loaded.Phase, TransitionPhaseCandidateTouched)
	}
}

func TestRecoverPendingTransitionRecordsBlocksWhenTransitionRecordUnreadable(t *testing.T) {
	mgr, err := NewAppManagerForTest(nil, t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	if err := state.StoreApp(transitionTestAppInstance("piclu")); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreImageUpdateTransaction("piclu", &ImageUpdateTransaction{Phase: imageUpdatePhaseCommitted}); err != nil {
		t.Fatalf("store image update transaction: %v", err)
	}
	transitionPath := filepath.Join(state.appsDir, "piclu", transitionRecordFilename)
	if err := os.WriteFile(transitionPath, []byte("{not-json"), 0600); err != nil {
		t.Fatalf("write corrupt transition record: %v", err)
	}

	blocked := mgr.recoverPendingTransitionRecords(context.Background(), state)
	if !blocked["piclu"] {
		t.Fatalf("corrupt v2 record should block recovery: %v", blocked)
	}
	if _, err := state.LoadImageUpdateTransaction("piclu"); err != nil {
		t.Fatalf("image transaction err = %v, want retained", err)
	}
	if _, err := state.LoadTransitionRecord("piclu"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want unreadable record retained", err)
	}
}

func TestLoadTransitionRecordRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{
			name: "top-level",
			mutate: func(doc map[string]interface{}) {
				doc["future_required_field"] = "must-fail-closed"
			},
		},
		{
			name: "nested-plan",
			mutate: func(doc map[string]interface{}) {
				plan, ok := doc["plan"].(map[string]interface{})
				if !ok {
					panic("plan missing")
				}
				plan["future_required_field"] = "must-fail-closed"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := NewAppManagerForTest(nil, t.TempDir())
			if err != nil {
				t.Fatalf("manager: %v", err)
			}
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatalf("state manager: %v", err)
			}
			if err := state.StoreApp(transitionTestAppInstance("piclu")); err != nil {
				t.Fatalf("store app: %v", err)
			}
			if err := state.StoreTransitionRecord("piclu", transitionTestRecord("piclu", TransitionPhasePrepared)); err != nil {
				t.Fatalf("store transition: %v", err)
			}
			transitionPath := filepath.Join(state.appsDir, "piclu", transitionRecordFilename)
			data, err := os.ReadFile(transitionPath)
			if err != nil {
				t.Fatalf("read transition: %v", err)
			}
			var doc map[string]interface{}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("decode transition fixture: %v", err)
			}
			tt.mutate(doc)
			data, err = json.Marshal(doc)
			if err != nil {
				t.Fatalf("encode transition fixture: %v", err)
			}
			if err := os.WriteFile(transitionPath, data, 0600); err != nil {
				t.Fatalf("write transition fixture: %v", err)
			}

			_, err = state.LoadTransitionRecord("piclu")
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("LoadTransitionRecord err = %v, want unknown field", err)
			}
		})
	}
}

func TestStartDoesNotConsumeLegacyImageJournalBeforeActiveTransitionFence(t *testing.T) {
	mgr, err := NewAppManagerForTest(nil, t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	if err := state.StoreApp(transitionTestAppInstance("piclu")); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreTransitionRecord("piclu", transitionTestRecord("piclu", TransitionPhaseSwitchingRuntime)); err != nil {
		t.Fatalf("store transition: %v", err)
	}
	if err := state.StoreImageUpdateTransaction("piclu", &ImageUpdateTransaction{Phase: imageUpdatePhaseCommitted}); err != nil {
		t.Fatalf("store image update transaction: %v", err)
	}

	err = mgr.Start(context.Background(), "piclu")
	if !errors.Is(err, ErrTransitionInProgress) {
		t.Fatalf("start err = %v, want ErrTransitionInProgress", err)
	}
	txn, err := state.LoadImageUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load image update transaction: %v", err)
	}
	if txn.Phase != imageUpdatePhaseCommitted {
		t.Fatalf("image update phase = %s, want %s", txn.Phase, imageUpdatePhaseCommitted)
	}
	record, err := state.LoadTransitionRecord("piclu")
	if err != nil {
		t.Fatalf("load transition: %v", err)
	}
	if record.Phase != TransitionPhaseSwitchingRuntime {
		t.Fatalf("transition phase = %s, want %s", record.Phase, TransitionPhaseSwitchingRuntime)
	}
}

func transitionTestAppInstance(instanceID string) *AppInstance {
	return &AppInstance{
		InstanceID: instanceID,
		Enabled:    true,
		Definition: &api.AppDefinition{
			Type: "user",
			Listeners: []api.AppListener{{
				Name:      instanceID,
				GuestPort: 80,
				Flow:      api.FlowTCP,
				Protocol:  api.ListenerProtocolHTTP,
				Primary:   true,
			}},
			Services: map[string]api.AppService{
				"main": {Image: "docker.io/example/app:stable", BindPorts: []int{80}},
			},
			Extensions: map[string]interface{}{"mode": "service"},
		},
	}
}

func transitionTestRecord(instanceID string, phase TransitionPhase) *TransitionRecord {
	return &TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationID:   "op-1",
		InstanceID:    instanceID,
		Phase:         phase,
		Plan: TransitionPlan{
			SchemaVersion: TransitionPlanSchemaVersion,
			OperationKind: TransitionOperationModifyApp,
			SourceKind:    TransitionSourceCustomRaw,
		},
	}
}
