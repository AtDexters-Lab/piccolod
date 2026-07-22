package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/fsutil"
	"piccolod/internal/persistence"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/services"

	"gopkg.in/yaml.v3"
)

const (
	manifestUpdateTokenTTL       = 30 * time.Minute
	manifestUpdateTxnFilename    = "manifest_update_transaction.json"
	manifestUpdateBackupFilename = "app.manifest-update.prev.yaml"
	installStateBackupFilename   = "install_state.manifest-update.prev.json"
	exposureReviewConfirmation   = "exposure_review"
	inputUsageScanSentinelPrefix = "__PICCOLO_INPUT_USAGE_SCAN_SENTINEL_"

	manifestUpdateImageEntryAppService    = "app_service"
	manifestUpdateImageEntryRuntimeAnchor = "runtime_anchor"

	manifestUpdateImageActionStage   = "stage"
	manifestUpdateImageActionRefresh = "refresh"
)

var (
	ErrManifestUpdateRejected = errors.New("custom manifest update rejected")
	ErrManifestUpdateConflict = errors.New("custom manifest update conflict")
)

type ManifestUpdateInputField struct {
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	Provenance            string `json:"provenance"`
	Required              bool   `json:"required"`
	Generate              bool   `json:"generate"`
	Locked                bool   `json:"locked"`
	Sensitive             bool   `json:"sensitive,omitempty"`
	HasCurrentValue       bool   `json:"has_current_value,omitempty"`
	CurrentValueSensitive bool   `json:"current_value_sensitive,omitempty"`
	CurrentValueDisplay   string `json:"current_value_display,omitempty"`
}

type ManifestUpdateConfigureResult struct {
	Inputs                   map[string]api.AppInput    `json:"inputs"`
	Fields                   []ManifestUpdateInputField `json:"fields"`
	SecretGeneratedPreflight []string                   `json:"secret_generated_preflight"`
	Eligible                 bool                       `json:"eligible"`
	BlockingReason           string                     `json:"blocking_reason,omitempty"`
}

type ManifestUpdateRequest struct {
	InstanceID         string
	RawTemplate        []byte
	Inputs             map[string]interface{}
	RegenerateInputs   []string
	ClearInputs        []string
	Confirmations      []string
	CatalogPending     bool
	SystemContext      InstallSystemContext
	BaseManifestHash   string
	RuntimeFingerprint string
	TransitionPlanHash string
	DryRunToken        string
}

type ManifestUpdateSummary struct {
	WillChange           []string `json:"will_change"`
	WillRestart          []string `json:"will_restart"`
	WillPreserve         []string `json:"will_preserve"`
	ExpectedInterruption []string `json:"expected_interruption"`
	Rejected             []string `json:"rejected,omitempty"`
}

type ManifestUpdateDecision struct {
	Flag    string `json:"flag"`
	Path    string `json:"path,omitempty"`
	Outcome string `json:"outcome"`
	Summary string `json:"summary"`
	Reason  string `json:"reason,omitempty"`
}

type ManifestUpdateReviewItem struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Old          string `json:"old,omitempty"`
	New          string `json:"new,omitempty"`
	Confirmation string `json:"confirmation"`
}

type ManifestUpdateKeptValueReviewItem struct {
	Field          string   `json:"field"`
	RiskKind       string   `json:"risk_kind"`
	OldSemantic    []string `json:"old_semantic,omitempty"`
	NewSemantic    []string `json:"new_semantic,omitempty"`
	SemanticDelta  []string `json:"semantic_delta,omitempty"`
	OldUsage       []string `json:"old_usage,omitempty"`
	NewUsage       []string `json:"new_usage,omitempty"`
	Confirmation   string   `json:"confirmation"`
	BlockingReason string   `json:"blocking_reason,omitempty"`
}

type ManifestUpdateImagePlanItem struct {
	ServiceName            string `json:"service_name"`
	EntryKind              string `json:"entry_kind,omitempty"`
	Action                 string `json:"action,omitempty"`
	Reason                 string `json:"reason,omitempty"`
	ImageRef               string `json:"image_ref"`
	ResolvedDigest         string `json:"resolved_digest"`
	CanonicalDigest        string `json:"canonical_digest,omitempty"`
	RootfsVolumeID         string `json:"rootfs_volume_id"`
	PreviousRootfsVolumeID string `json:"previous_rootfs_volume_id,omitempty"`
}

type ManifestUpdateDataSafetySummary struct {
	SnapshotRequired bool   `json:"snapshot_required"`
	Reason           string `json:"reason,omitempty"`
	FailureBehavior  string `json:"failure_behavior,omitempty"`
	RollbackLimit    string `json:"rollback_limit,omitempty"`
}

type ManifestUpdateResult struct {
	InstanceID            string                              `json:"instance_id"`
	BaseManifestHash      string                              `json:"base_manifest_hash"`
	RuntimeFingerprint    string                              `json:"runtime_fingerprint"`
	TransitionPlanHash    string                              `json:"transition_plan_hash,omitempty"`
	DryRunToken           string                              `json:"dry_run_token,omitempty"`
	RenderedAppID         string                              `json:"rendered_app_id"`
	DiffKind              string                              `json:"diff_kind"`
	UpdateClass           string                              `json:"update_class,omitempty"`
	Applicable            bool                                `json:"applicable"`
	BlockingReason        string                              `json:"blocking_reason,omitempty"`
	MetadataOnly          bool                                `json:"metadata_only"`
	AccessRepairPending   bool                                `json:"access_repair_pending,omitempty"`
	AccessRepairMessage   string                              `json:"access_repair_message,omitempty"`
	Summary               ManifestUpdateSummary               `json:"summary"`
	Decisions             []ManifestUpdateDecision            `json:"decisions,omitempty"`
	ExposureReview        []ManifestUpdateReviewItem          `json:"exposure_review,omitempty"`
	KeptValueReview       []ManifestUpdateKeptValueReviewItem `json:"kept_value_review,omitempty"`
	RequiredConfirmations []string                            `json:"required_confirmations,omitempty"`
	OperationRiskFlags    []string                            `json:"operation_risk_flags,omitempty"`
	RuntimeReadiness      []string                            `json:"runtime_readiness,omitempty"`
	StagedImageRootfs     []string                            `json:"staged_image_rootfs,omitempty"`
	ListenerRoutingAuth   []string                            `json:"listener_routing_auth,omitempty"`
	StorageBoundary       []string                            `json:"storage_boundary,omitempty"`
	DataSafety            *ManifestUpdateDataSafetySummary    `json:"data_safety,omitempty"`
}

type manifestUpdateCandidate struct {
	Token                string
	InstanceID           string
	RawTemplate          []byte
	Inputs               map[string]interface{}
	InputSensitive       map[string]bool
	SystemContext        InstallSystemContext
	BaseManifestHash     string
	RuntimeFingerprint   string
	BaseLedgerExists     bool
	BaseLedgerRevision   int64
	BaseLedgerSourceHash string
	SourceKind           string
	CatalogSource        string
	PendingSourceHash    string
	CandidateDigest      string
	InstallState         *InstallState
	OIDCCredentials      *OIDCCredentials
	DiffKind             DiffKind
	MetadataOnly         bool
	Definition           *api.AppDefinition
	Summary              ManifestUpdateSummary
	Classification       manifestUpdateClassification
	ImagePlan            []ManifestUpdateImagePlanItem
	TransitionPlan       TransitionPlan
	TransitionPlanHash   string
	CreatedAt            time.Time
	ExpiresAt            time.Time
}

type ManifestUpdateTransaction struct {
	OperationID               string                        `json:"operation_id"`
	OperationKind             string                        `json:"operation_kind,omitempty"`
	Phase                     string                        `json:"phase"`
	PreviousManifestHash      string                        `json:"previous_manifest_hash"`
	CandidateManifestHash     string                        `json:"candidate_manifest_hash"`
	PreviousLedgerRevision    int64                         `json:"previous_ledger_revision,omitempty"`
	CandidateLedgerRevision   int64                         `json:"candidate_ledger_revision,omitempty"`
	PreviousLedgerSourceHash  string                        `json:"previous_ledger_source_hash,omitempty"`
	CandidateLedgerSourceHash string                        `json:"candidate_ledger_source_hash,omitempty"`
	DryRunToken               string                        `json:"dry_run_token"`
	RuntimeFingerprint        string                        `json:"runtime_fingerprint"`
	BackupPath                string                        `json:"backup_path"`
	BackupInstallStatePath    string                        `json:"backup_install_state_path,omitempty"`
	PreviousActiveRootfs      map[string]string             `json:"previous_active_rootfs,omitempty"`
	CandidateActiveRootfs     map[string]string             `json:"candidate_active_rootfs,omitempty"`
	RemovedRootfs             []string                      `json:"removed_rootfs,omitempty"`
	StagedRootfs              []string                      `json:"staged_rootfs,omitempty"`
	CreatedRootfs             []string                      `json:"created_rootfs,omitempty"`
	ResolvedImages            []ManifestUpdateImagePlanItem `json:"resolved_images,omitempty"`
	PreparedListenerEndpoints []services.ServiceEndpoint    `json:"prepared_listener_endpoints,omitempty"`
	PrecommitDataSnapshotID   string                        `json:"precommit_data_snapshot_id,omitempty"`
	FailedDataLVName          string                        `json:"failed_data_lv_name,omitempty"`
	CreatedInstallState       bool                          `json:"created_install_state,omitempty"`
	CreatedOIDCClientID       string                        `json:"created_oidc_client_id,omitempty"`
	ProxyOIDCDeltaApplied     bool                          `json:"proxy_oidc_delta_applied,omitempty"`
	RuntimeSwitchStarted      bool                          `json:"runtime_switch_started,omitempty"`
	RuntimeTouched            bool                          `json:"runtime_touched,omitempty"`
	AccessSuspended           bool                          `json:"access_suspended,omitempty"`
	AccessPublished           bool                          `json:"access_published,omitempty"`
	CreatedAt                 time.Time                     `json:"created_at"`
	UpdatedAt                 time.Time                     `json:"updated_at"`
	LastError                 string                        `json:"last_error,omitempty"`
}

func (m *AppManager) ConfigureCustomManifestUpdate(ctx context.Context, instanceID string, raw []byte, catalogPending ...bool) (*ManifestUpdateConfigureResult, error) {
	usePendingCatalog := len(catalogPending) > 0 && catalogPending[0]
	if len(bytes.TrimSpace(raw)) == 0 && !usePendingCatalog {
		return nil, fmt.Errorf("manifest update configure: empty manifest")
	}
	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, errAppNotFound(instanceID)
	}
	if err := customManifestBasicEligibility(appInst, usePendingCatalog); err != nil {
		return nil, err
	}

	if usePendingCatalog {
		pendingRaw, pendingState, err := m.pendingCatalogManifestUpdateSource(state, instanceID)
		if err != nil {
			return nil, err
		}
		raw = pendingRaw
		currentDef, _ := state.GetAppDefinition(instanceID)
		def, renderedSchema, err := m.schemaForInstallStateRawTemplateWithOrigin(ctx, instanceID, raw, pendingState, currentDef)
		if err != nil {
			return nil, fmt.Errorf("pending catalog manifest schema: %w", err)
		}
		fields, preflight := manifestUpdateInputFieldsForCatalogPending(def.Inputs, pendingState, instanceID)
		return &ManifestUpdateConfigureResult{
			Inputs:                   manifestUpdateDisplayInputs(def.Inputs, instanceID, renderedSchema),
			Fields:                   fields,
			SecretGeneratedPreflight: preflight,
			Eligible:                 true,
		}, nil
	}

	def, err := ParseAppSchema(raw)
	if err != nil {
		return nil, fmt.Errorf("parse manifest schema: %w", err)
	}
	PrepareSmartDefaultsForUpdate(def, instanceID)
	inputState, err := manifestUpdateInputState(state, instanceID)
	if err != nil {
		return nil, err
	}
	displayInputs, fields, preflight := manifestUpdateInputFields(def.Inputs, inputState, instanceID)
	result := &ManifestUpdateConfigureResult{
		Inputs:                   displayInputs,
		Fields:                   fields,
		SecretGeneratedPreflight: preflight,
		Eligible:                 true,
	}
	return result, nil
}

func PrepareSmartDefaultsForUpdate(schema *api.AppDefinition, instanceID string) {
	if schema == nil {
		return
	}
	hasPrimaryMarker := detectPrimaryMarker(schema.Listeners)
	if hasPrimaryMarker {
		if schema.Inputs == nil {
			schema.Inputs = make(map[string]api.AppInput)
		}
		input := schema.Inputs["__app_address__"]
		input.Type = "string"
		if strings.TrimSpace(input.Label) == "" {
			input.Label = "App Address"
		}
		if strings.TrimSpace(input.Description) == "" {
			input.Description = "The existing app address for this installed app"
		}
		input.Required = true
		input.Generate = false
		input.Default = instanceID
		if input.Validation == nil {
			input.Validation = &api.AppInputValidation{
				Regex:   "^[a-z][a-z0-9]{0,15}$",
				Message: "Lowercase letters and numbers only, must start with letter, max 16 chars",
			}
		}
		schema.Inputs["__app_address__"] = input
	}
	if schema.Inputs == nil {
		return
	}
	for key, input := range schema.Inputs {
		if input.Type == "password" || input.Generate {
			input.Default = nil
			schema.Inputs[key] = input
		}
	}
}

func manifestUpdateDisplayInputs(inputs map[string]api.AppInput, instanceID string, redactMetadata bool) map[string]api.AppInput {
	displayInputs := make(map[string]api.AppInput, len(inputs))
	for name, spec := range inputs {
		displayInputs[name] = clientDisplayInputSpec(name, spec, instanceID, redactMetadata)
	}
	return displayInputs
}

func manifestUpdateInputFields(inputs map[string]api.AppInput, st *InstallState, instanceID string) (map[string]api.AppInput, []ManifestUpdateInputField, []string) {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	displayInputs := make(map[string]api.AppInput, len(inputs))
	fields := make([]ManifestUpdateInputField, 0, len(names))
	preflight := []string{}
	for _, name := range names {
		spec := inputs[name]
		provenance := "Entered now"
		locked := false
		currentUsable := false
		if st != nil && st.InstallInputs != nil {
			value, present := st.InstallInputs[name]
			currentUsable = present && installInputValueUsableForSchema(value, spec)
		}
		currentSensitive := manifestUpdateStoredInputSensitive(name, spec, st)
		sensitive := inputIsSensitive(name, spec) || currentSensitive
		currentDisplay := ""
		if name == "__app_address__" {
			provenance = "Locked current value"
			locked = true
		} else if currentUsable {
			if sensitive {
				provenance = "Current stored value will be kept"
			} else {
				provenance = "Current value will be kept"
				currentDisplay = manifestUpdateCurrentValueDisplay(st.InstallInputs[name])
			}
		} else if sensitive {
			if spec.Required || spec.Generate {
				provenance = "Re-enter required"
			} else {
				provenance = "Optional secret; leave blank to keep unset"
			}
			if spec.Required || spec.Generate {
				preflight = append(preflight, name)
			}
		} else if spec.Default != nil {
			provenance = "New manifest default"
		} else if spec.Required {
			provenance = "Enter required"
		}
		fields = append(fields, ManifestUpdateInputField{
			Name:                  name,
			Type:                  spec.Type,
			Provenance:            provenance,
			Required:              spec.Required,
			Generate:              spec.Generate,
			Locked:                locked,
			Sensitive:             sensitive,
			HasCurrentValue:       currentUsable && name != "__app_address__",
			CurrentValueSensitive: currentSensitive,
			CurrentValueDisplay:   currentDisplay,
		})
	}
	displayInputs = manifestUpdateDisplayInputs(inputs, instanceID, false)
	return displayInputs, fields, preflight
}

func manifestUpdateCurrentValueDisplay(value any) string {
	if value == nil {
		return ""
	}
	var text string
	switch v := value.(type) {
	case string:
		text = v
	default:
		if encoded, err := json.Marshal(v); err == nil {
			text = string(encoded)
		} else {
			text = fmt.Sprint(v)
		}
	}
	text = strings.TrimSpace(text)
	if len(text) > 160 {
		text = text[:157] + "..."
	}
	return text
}

func manifestUpdateStoredInputSensitive(name string, newSpec api.AppInput, st *InstallState) bool {
	if inputIsSensitive(name, newSpec) || newSpec.Generate {
		return true
	}
	if st == nil || st.InstallInputs == nil {
		return false
	}
	if _, ok := st.InstallInputs[name]; !ok {
		return false
	}
	if st.InputSensitive != nil && st.InputSensitive[name] {
		return true
	}
	oldRaw := st.RawTemplate
	if len(bytes.TrimSpace(oldRaw)) == 0 {
		return true
	}
	oldSchema, err := ParseAppSchema(oldRaw)
	if err != nil {
		return true
	}
	oldSpec, ok := oldSchema.Inputs[name]
	if !ok {
		return true
	}
	return inputIsSensitive(name, oldSpec) || oldSpec.Generate
}

func manifestUpdateInputFieldsForCatalogPending(inputs map[string]api.AppInput, st *InstallState, instanceID string) ([]ManifestUpdateInputField, []string) {
	if st == nil {
		return nil, nil
	}
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	fields := []ManifestUpdateInputField{}
	preflight := []string{}
	for _, name := range names {
		spec := inputs[name]
		if name == "__app_address__" {
			spec.Default = instanceID
			inputs[name] = spec
			continue
		}
		value, oldPresent := any(nil), false
		if st.InstallInputs != nil {
			value, oldPresent = st.InstallInputs[name]
		}
		currentUsable := oldPresent && installInputValueUsableForSchema(value, spec)
		currentSensitive := manifestUpdateStoredInputSensitive(name, spec, st)
		if currentUsable && !currentSensitive && !spec.Generate {
			continue
		}
		if !spec.Required && !spec.Generate && !currentSensitive {
			continue
		}
		provenance := "Required for this update"
		if currentUsable {
			if currentSensitive || spec.Generate {
				provenance = "Current stored value will be kept"
			} else {
				provenance = "Current value will be kept"
			}
		} else if inputIsSensitive(name, spec) || spec.Generate {
			if spec.Required || spec.Generate {
				if spec.Generate {
					provenance = "Generate or enter required"
				} else {
					provenance = "Re-enter required"
				}
				preflight = append(preflight, name)
			} else {
				provenance = "Optional secret; leave blank to keep unset"
			}
		}
		fields = append(fields, ManifestUpdateInputField{
			Name:                  name,
			Type:                  spec.Type,
			Provenance:            provenance,
			Required:              spec.Required,
			Generate:              spec.Generate,
			Sensitive:             inputIsSensitive(name, spec) || currentSensitive,
			HasCurrentValue:       currentUsable,
			CurrentValueSensitive: currentSensitive,
			CurrentValueDisplay: func() string {
				if currentUsable && !currentSensitive {
					return manifestUpdateCurrentValueDisplay(value)
				}
				return ""
			}(),
		})
	}
	return fields, preflight
}

func manifestUpdateInputState(state *FilesystemStateManager, instanceID string) (*InstallState, error) {
	st, err := state.LoadInstallState(instanceID)
	if errors.Is(err, ErrInstallStateNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load config ledger: %v", ErrInstalledConfigUnavailable, err)
	}
	if !st.isV2Complete() {
		return nil, nil
	}
	return st, nil
}

func (m *AppManager) pendingCatalogManifestUpdateSource(state *FilesystemStateManager, instanceID string) ([]byte, *InstallState, error) {
	st, err := state.LoadInstallState(instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: load config ledger: %v", ErrInstalledConfigUnavailable, err)
	}
	if !st.isV2Complete() {
		return nil, nil, fmt.Errorf("%w: install config ledger is incomplete or legacy", ErrInstalledConfigUnavailable)
	}
	pendingRaw, _, _, ok := st.pendingCatalogSourceForFlow(pendingCatalogReviewFlowManifest)
	if !ok {
		return nil, nil, fmt.Errorf("%w: no pending update requires review", ErrManifestUpdateRejected)
	}
	return pendingRaw, st, nil
}

func (m *AppManager) DryRunCustomManifestUpdate(ctx context.Context, req ManifestUpdateRequest) (*ManifestUpdateResult, error) {
	cand, result, err := m.renderCustomManifestUpdateCandidate(ctx, req)
	if err != nil {
		return nil, err
	}
	if !result.Applicable {
		return result, nil
	}
	token, err := randomManifestUpdateToken()
	if err != nil {
		return nil, err
	}
	cand.Token = token
	cand.CreatedAt = time.Now().UTC()
	cand.ExpiresAt = cand.CreatedAt.Add(manifestUpdateTokenTTL)
	result.DryRunToken = token
	m.storeManifestUpdateCandidate(cand)
	return result, nil
}

func (m *AppManager) ApplyCustomManifestUpdate(ctx context.Context, req ManifestUpdateRequest) (res *ManifestUpdateResult, err error) {
	defer pressure.BeginLifecycleOwner("app:" + req.InstanceID)()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return nil, err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	taskType := taskTypeUpdateServiceApp
	accessRepairPending := false
	m.emitProgress(ctx, taskType, req.InstanceID, taskPhaseValidating, 0, "Validating app update", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskType, req.InstanceID, taskPhaseComplete, 100, "App update failed", true, err)
		} else if accessRepairPending {
			m.emitProgress(ctx, taskType, req.InstanceID, taskPhaseComplete, 100, "App update applied; access repair pending", true, nil)
		} else {
			m.emitProgress(ctx, taskType, req.InstanceID, taskPhaseComplete, 100, "App update complete", true, nil)
		}
	}()

	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	if err := m.rejectIfTransitionInProgress(state, req.InstanceID, TransitionFenceModifyApp); err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(req.InstanceID)
	if !exists {
		return nil, errAppNotFound(req.InstanceID)
	}
	if err := customManifestBasicEligibility(appInst, true); err != nil {
		return nil, err
	}

	cand, ok := m.takeManifestUpdateCandidate(req.DryRunToken)
	if !ok {
		return nil, fmt.Errorf("%w: dry-run token is expired or unknown", ErrManifestUpdateConflict)
	}
	if cand.InstanceID != req.InstanceID {
		return nil, fmt.Errorf("%w: dry-run token belongs to a different app", ErrManifestUpdateConflict)
	}
	if req.BaseManifestHash != "" && req.BaseManifestHash != cand.BaseManifestHash {
		return nil, fmt.Errorf("%w: base manifest hash does not match dry run", ErrManifestUpdateConflict)
	}
	if req.RuntimeFingerprint != "" && req.RuntimeFingerprint != cand.RuntimeFingerprint {
		return nil, fmt.Errorf("%w: runtime fingerprint does not match dry run", ErrManifestUpdateConflict)
	}
	if cand.TransitionPlanHash != "" && strings.TrimSpace(req.TransitionPlanHash) == "" {
		return nil, fmt.Errorf("%w: transition plan hash is required", ErrManifestUpdateConflict)
	}
	if req.TransitionPlanHash != "" && req.TransitionPlanHash != cand.TransitionPlanHash {
		return nil, fmt.Errorf("%w: transition plan hash does not match dry run", ErrManifestUpdateConflict)
	}
	if missing := missingManifestUpdateConfirmations(cand.Classification.RequiredConfirmations, req.Confirmations); len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing required confirmation(s): %s", ErrManifestUpdateRejected, strings.Join(missing, ", "))
	}

	curDef, err := state.GetAppDefinition(req.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("read current manifest: %w", err)
	}
	curHash, err := canonicalManifestHash(curDef)
	if err != nil {
		return nil, fmt.Errorf("hash current manifest: %w", err)
	}
	if curHash != cand.BaseManifestHash {
		return nil, fmt.Errorf("%w: manifest changed after dry run", ErrManifestUpdateConflict)
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return nil, fmt.Errorf("fingerprint runtime: %w", err)
	}
	if runtimeFingerprint != cand.RuntimeFingerprint {
		return nil, fmt.Errorf("%w: runtime changed after dry run", ErrManifestUpdateConflict)
	}
	currentLedgerExists, currentLedgerRevision, currentLedgerSourceHash, err := loadInstallLedgerFingerprint(state, req.InstanceID)
	if err != nil {
		return nil, err
	}
	if currentLedgerExists != cand.BaseLedgerExists ||
		currentLedgerRevision != cand.BaseLedgerRevision ||
		currentLedgerSourceHash != cand.BaseLedgerSourceHash {
		return nil, fmt.Errorf("%w: config ledger changed after dry run", ErrManifestUpdateConflict)
	}
	if cand.SourceKind == InstallSourceKindCatalog {
		currentState, err := state.LoadInstallState(req.InstanceID)
		if err != nil {
			return nil, fmt.Errorf("%w: load config ledger: %v", ErrManifestUpdateConflict, err)
		}
		_, pendingHash, _, ok := currentState.pendingCatalogSourceForFlow(pendingCatalogReviewFlowManifest)
		if !ok {
			return nil, fmt.Errorf("%w: pending update changed after dry run", ErrManifestUpdateConflict)
		}
		if pendingHash != cand.PendingSourceHash {
			return nil, fmt.Errorf("%w: pending catalog source changed after dry run", ErrManifestUpdateConflict)
		}
	}

	candidateLedgerRevision := currentLedgerRevision + 1
	if candidateLedgerRevision <= 0 {
		candidateLedgerRevision = 1
	}
	nextState := cand.InstallState
	if nextState == nil {
		nextState = NewV2InstallState(
			req.InstanceID,
			InstallSourceKindCustom,
			"",
			cand.RawTemplate,
			cand.Inputs,
			cand.SystemContext,
			cand.OIDCCredentials,
			false,
		)
		nextState.InputSensitive = persistedInstallInputSensitive(nextState.InstallInputs, cand.InputSensitive)
	}
	nextState.Revision = candidateLedgerRevision
	applyTxn, err := m.beginInstalledAppApplyTransaction(ctx, state, installedAppApplyTransactionSpec{
		OperationKind:             "service_app_update",
		TaskType:                  taskType,
		RollbackPrefix:            "app update rolled back",
		InstanceID:                req.InstanceID,
		AppInst:                   appInst,
		PreviousDefinition:        curDef,
		CandidateDefinition:       cand.Definition,
		PreviousManifestHash:      cand.BaseManifestHash,
		CandidateManifestHash:     cand.CandidateDigest,
		PreviousLedgerRevision:    currentLedgerRevision,
		CandidateLedgerRevision:   nextState.Revision,
		PreviousLedgerSourceHash:  currentLedgerSourceHash,
		CandidateLedgerSourceHash: nextState.RawTemplateHash,
		DryRunToken:               cand.Token,
		RuntimeFingerprint:        cand.RuntimeFingerprint,
		TransitionPlan:            cand.TransitionPlan,
		TransitionPlanHash:        cand.TransitionPlanHash,
		CatalogSourceSnapshot:     transitionSnapshotFromPlan(cand.TransitionPlan),
		ImagePlan:                 cand.ImagePlan,
		MetadataOnly:              cand.MetadataOnly,
		ApplyPhase:                taskPhaseApplyingManifest,
		ApplyMessage:              "Persisting manifest",
		FinalizingMessage:         "Saving config ledger",
	})
	if err != nil {
		return nil, err
	}
	if err := applyTxn.stageRuntimeRootfsIfNeeded(cand.Classification); err != nil {
		return nil, err
	}
	if err := applyTxn.persistCandidateManifest(); err != nil {
		return nil, err
	}
	m.ReconcileAllSlicePolicies()
	if err := applyTxn.recreateRuntimeIfNeeded(); err != nil {
		return nil, err
	}
	if err := applyTxn.commitLedger(nextState); err != nil {
		return nil, err
	}
	var catalogMetadataErr error
	if cand.SourceKind == InstallSourceKindCatalog && appInst.CatalogSource != "" {
		if err := storeCommittedCatalogMetadata(state, appInst, nextState.RawTemplateHash); err != nil {
			catalogMetadataErr = err
			log.Printf("WARN: manifest update %s: committed catalog metadata pending retry: %v", req.InstanceID, err)
		}
	}
	accessRepairMessage := ""
	if err := applyTxn.publishAccess(); err != nil {
		accessRepairPending = true
		if catalogMetadataErr != nil {
			err = errors.Join(err, catalogMetadataErr)
		}
		accessRepairMessage = applyTxn.markAccessRepairPending(err)
	} else if catalogMetadataErr != nil {
		applyTxn.markCatalogMetadataPending(catalogMetadataErr)
	} else {
		applyTxn.complete()
	}
	return &ManifestUpdateResult{
		InstanceID:            req.InstanceID,
		BaseManifestHash:      cand.BaseManifestHash,
		RuntimeFingerprint:    cand.RuntimeFingerprint,
		TransitionPlanHash:    cand.TransitionPlanHash,
		RenderedAppID:         req.InstanceID,
		DiffKind:              cand.DiffKind.String(),
		UpdateClass:           cand.Classification.UpdateClass,
		Applicable:            true,
		MetadataOnly:          cand.MetadataOnly,
		AccessRepairPending:   accessRepairPending,
		AccessRepairMessage:   accessRepairMessage,
		Summary:               cand.Summary,
		Decisions:             cand.Classification.Decisions,
		ExposureReview:        cand.Classification.ExposureReview,
		KeptValueReview:       cand.Classification.KeptValueReview,
		RequiredConfirmations: cand.Classification.RequiredConfirmations,
		OperationRiskFlags:    cand.Classification.OperationRiskFlags,
		RuntimeReadiness:      cand.Classification.RuntimeReadiness,
		StagedImageRootfs:     cand.Classification.StagedImageRootfs,
		ListenerRoutingAuth:   cand.Classification.ListenerRoutingAuth,
		StorageBoundary:       cand.Classification.StorageBoundary,
		DataSafety:            cand.Classification.DataSafety,
	}, nil
}

func (m *AppManager) restoreInstalledAppApplyFailure(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, prevDef, failedDef *api.AppDefinition, txn *ManifestUpdateTransaction, taskType, operationKind string, publicationResumeToken services.PublicationResumeToken, cause error) error {
	instanceID := appInst.InstanceID
	txn.Phase = "restoring_previous"
	txn.LastError = cause.Error()
	txn.UpdatedAt = time.Now().UTC()
	_ = state.StoreManifestUpdateTransaction(instanceID, txn)
	_ = storeTransitionRecordForManifestTransaction(state, instanceID, txn, appInst, TransitionPhaseRestoringPrevious)
	m.emitProgress(ctx, taskType, instanceID, taskPhaseRestoringManifest, 75, "Restoring previous manifest", false, nil)

	var rollbackErrs []error
	appInst.Definition = prevDef
	if txn.PreviousActiveRootfs != nil || txn.CandidateActiveRootfs != nil {
		appInst.ActiveRootfs = cloneStringMap(txn.PreviousActiveRootfs)
	}
	if err := state.StoreApp(appInst); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("restore manifest failed: %w", err))
	}
	if err := state.RestoreInstallStateForTransaction(instanceID, txn); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("restore install state failed: %w", err))
	}
	if err := m.cleanupTransactionCreatedOIDCClient(ctx, txn); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if manifestTransactionRuntimeTouched(txn) {
		if err := m.restorePrecommitDataSnapshot(ctx, state, appInst, failedDef, txn); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	} else if err := m.cleanupPrecommitDataSnapshot(ctx, txn); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	runtimePublicationReady := !manifestTransactionRuntimeSwitchStarted(txn)
	if manifestTransactionRuntimeSwitchStarted(txn) {
		if err := m.recreateContainersInPlaceWithHookAndPublicationResumeToken(ctx, instanceID, prevDef, failedDef, appInst, nil, publicationResumeToken); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("recreate previous containers failed: %w", err))
		} else {
			runtimePublicationReady = true
		}
	}
	if txn.AccessSuspended && m.serviceManager != nil && runtimePublicationReady {
		if err := m.serviceManager.ResumeAppPublicationWithResumeTokenContext(pressure.WithTransitionContinuation(ctx), publicationResumeToken, instanceID); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("resume app publication failed: %w", err))
		} else {
			txn.AccessSuspended = false
		}
	}
	m.configureOIDCAuthorizePaths(instanceID, prevDef)
	if err := m.rollbackTransactionProxyOIDCDelta(ctx, instanceID, txn, prevDef, failedDef); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if err := m.cleanupManifestUpdateStagedRootfs(ctx, txn); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if restoreErr := errors.Join(rollbackErrs...); restoreErr != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("apply failed: %v; rollback failed: %v", cause, restoreErr)
		txn.UpdatedAt = time.Now().UTC()
		_ = state.StoreManifestUpdateTransaction(instanceID, txn)
		_ = storeTransitionRecordForManifestTransaction(state, instanceID, txn, appInst, TransitionPhaseRestoreFailed)
		m.setObservedStatus(instanceID, StatusError)
		m.emitProgress(ctx, taskType, instanceID, taskPhaseRestoreFailed, 95, "Restore failed", false, restoreErr)
		return fmt.Errorf("%s failed: %w; rollback failed: %w", operationKind, cause, restoreErr)
	}
	m.ReconcileAllSlicePolicies()
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		log.Printf("WARN: manifest update %s: cleanup v2 transition after rollback: %v", instanceID, err)
	} else if err := state.ClearManifestUpdateTransaction(instanceID, txn.BackupPath); err != nil {
		log.Printf("WARN: manifest update %s: cleanup after rollback: %v", instanceID, err)
	}
	return nil
}

func (m *AppManager) cleanupTransactionCreatedOIDCClient(ctx context.Context, txn *ManifestUpdateTransaction) error {
	if txn == nil || strings.TrimSpace(txn.CreatedOIDCClientID) == "" {
		return nil
	}
	clientID := strings.TrimSpace(txn.CreatedOIDCClientID)
	host := m.currentSyncHost()
	if host == nil {
		return fmt.Errorf("delete created oidc client %s: %w", clientID, ErrSyncHostUnavailable)
	}
	if err := host.DeleteOIDCClient(ctx, clientID); err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return fmt.Errorf("delete created oidc client %s: %w", clientID, err)
	}
	return nil
}

func (m *AppManager) cleanupPrecommitDataSnapshot(ctx context.Context, txn *ManifestUpdateTransaction) error {
	snapshotID := strings.TrimSpace(txn.PrecommitDataSnapshotID)
	if snapshotID == "" {
		return nil
	}
	snapshotter, ok := m.currentVolumeManager().(dataVolumeSnapshotter)
	if !ok {
		return fmt.Errorf("cleanup precommit data snapshot %s: volume manager does not support snapshots", snapshotID)
	}
	if err := snapshotter.DestroyDataSnapshot(ctx, snapshotID); err != nil {
		return fmt.Errorf("cleanup precommit data snapshot %s: %w", snapshotID, err)
	}
	txn.PrecommitDataSnapshotID = ""
	return nil
}

func storeManifestUpdateTransactionAndTransition(state *FilesystemStateManager, instanceID string, txn *ManifestUpdateTransaction, appInst *AppInstance) error {
	if err := state.StoreManifestUpdateTransaction(instanceID, txn); err != nil {
		return err
	}
	if err := storeTransitionRecordForManifestTransaction(state, instanceID, txn, appInst, transitionPhaseForManifestPhase(txn.Phase)); err != nil {
		return fmt.Errorf("store v2 transition projection: %w", err)
	}
	return nil
}

func (m *AppManager) cleanupCommittedManifestUpdateTransaction(ctx context.Context, state *FilesystemStateManager, instanceID string, txn *ManifestUpdateTransaction) error {
	if err := errors.Join(
		m.cleanupPrecommitDataSnapshot(ctx, txn),
		m.cleanupManifestUpdateRemovedRootfs(ctx, txn),
	); err != nil {
		txn.Phase = "committed_cleanup_pending"
		txn.LastError = err.Error()
		txn.UpdatedAt = time.Now().UTC()
		if storeErr := state.StoreManifestUpdateTransaction(instanceID, txn); storeErr != nil {
			return errors.Join(err, fmt.Errorf("persist committed cleanup retry marker: %w", storeErr))
		}
		if storeErr := storeTransitionRecordForManifestTransaction(state, instanceID, txn, nil, TransitionPhaseCommittedCleanupPending); storeErr != nil {
			return errors.Join(err, fmt.Errorf("persist v2 committed cleanup retry marker: %w", storeErr))
		}
		return err
	}
	txn.Phase = "committed"
	txn.LastError = ""
	txn.UpdatedAt = time.Now().UTC()
	if err := state.StoreManifestUpdateTransaction(instanceID, txn); err != nil {
		return fmt.Errorf("persist committed cleanup state: %w", err)
	}
	if err := storeTransitionRecordForManifestTransaction(state, instanceID, txn, nil, TransitionPhaseCommitted); err != nil {
		return fmt.Errorf("persist committed v2 transition state: %w", err)
	}
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		return fmt.Errorf("clear committed v2 transition: %w", err)
	}
	if err := state.ClearManifestUpdateTransaction(instanceID, txn.BackupPath); err != nil {
		return fmt.Errorf("clear committed transaction: %w", err)
	}
	return nil
}

func (m *AppManager) repairCommittedManifestUpdateAccess(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, prevDef, candidateDef *api.AppDefinition, txn *ManifestUpdateTransaction) error {
	if appInst == nil || txn == nil {
		return fmt.Errorf("manifest update access repair requires app and transaction")
	}
	instanceID := appInst.InstanceID
	txn.Phase = "publishing_access"
	txn.UpdatedAt = time.Now().UTC()
	if err := storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst); err != nil {
		return fmt.Errorf("manifest update recovery %s: persist access repair marker: %w", instanceID, err)
	}
	if appInst.Enabled && (manifestTransactionRuntimeSwitchStarted(txn) || txn.AccessSuspended || len(txn.PreparedListenerEndpoints) > 0) {
		var publicationResumeToken services.PublicationResumeToken
		if m.serviceManager != nil {
			// Recovery may be re-entering a process that never observed the
			// original suspension. Reassert it to withdraw any partial routes
			// and mint fresh process-local resume authority.
			publicationResumeToken = m.serviceManager.SuspendAppPublication(instanceID)
		}
		if len(txn.PreparedListenerEndpoints) > 0 && m.serviceManager != nil {
			if err := m.serviceManager.RestorePreparedPublicationWithResumeTokenContext(pressure.WithTransitionContinuation(ctx), publicationResumeToken, instanceID, txn.PreparedListenerEndpoints); err != nil {
				txn.LastError = err.Error()
				txn.UpdatedAt = time.Now().UTC()
				_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
				return fmt.Errorf("manifest update recovery %s: repair prepared listener publication: %w", instanceID, err)
			}
		} else if txn.AccessSuspended && m.serviceManager != nil {
			if endpoints, err := m.serviceManager.GetByApp(instanceID); err == nil && len(endpoints) > 0 && reflect.DeepEqual(prevDef.Listeners, candidateDef.Listeners) {
				if err := m.serviceManager.ResumeAppPublicationWithResumeTokenContext(pressure.WithTransitionContinuation(ctx), publicationResumeToken, instanceID); err != nil {
					txn.LastError = err.Error()
					txn.UpdatedAt = time.Now().UTC()
					_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
					return fmt.Errorf("manifest update recovery %s: resume listener publication: %w", instanceID, err)
				}
			} else if err := m.restoreManifestUpdateAccessFromRuntime(ctx, publicationResumeToken, appInst, candidateDef); err != nil {
				txn.LastError = err.Error()
				txn.UpdatedAt = time.Now().UTC()
				_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
				return fmt.Errorf("manifest update recovery %s: repair access: %w", instanceID, err)
			}
		} else if err := m.restoreManifestUpdateAccessFromRuntime(ctx, publicationResumeToken, appInst, candidateDef); err != nil {
			txn.LastError = err.Error()
			txn.UpdatedAt = time.Now().UTC()
			_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
			return fmt.Errorf("manifest update recovery %s: repair access: %w", instanceID, err)
		}
	} else if !appInst.Enabled && m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}
	proxyDeltaApplied := false
	if host := m.currentSyncHost(); host != nil && proxyOIDCDeltaRequired(host, prevDef, candidateDef) {
		if err := m.applyProxyOIDCDelta(ctx, host, instanceID, prevDef, candidateDef); err != nil {
			txn.LastError = err.Error()
			txn.UpdatedAt = time.Now().UTC()
			_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
			return fmt.Errorf("manifest update recovery %s: repair proxy oidc delta: %w", instanceID, err)
		}
		proxyDeltaApplied = true
	}
	m.configureOIDCAuthorizePaths(instanceID, candidateDef)
	txn.AccessSuspended = false
	txn.AccessPublished = true
	if proxyDeltaApplied {
		txn.ProxyOIDCDeltaApplied = true
	}
	txn.LastError = ""
	txn.UpdatedAt = time.Now().UTC()
	if err := storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst); err != nil {
		return fmt.Errorf("manifest update recovery %s: persist access repaired marker: %w", instanceID, err)
	}
	return nil
}

func (m *AppManager) restoreManifestUpdateAccessFromRuntime(ctx context.Context, publicationResumeToken services.PublicationResumeToken, appInst *AppInstance, def *api.AppDefinition) error {
	if m.serviceManager == nil || def == nil {
		return nil
	}
	instanceID := appInst.InstanceID
	publishCID := strings.TrimSpace(appInst.PublishContainerID())
	if publishCID == "" {
		if len(def.Listeners) == 0 {
			m.serviceManager.DeactivateApp(instanceID)
			return nil
		}
		return fmt.Errorf("publish container unavailable")
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, piccoloModeFromExtensions(def.Extensions), appRuntimeEnsureReady)
	if err != nil {
		return fmt.Errorf("podman runtime: %w", err)
	}
	ports, err := m.containerManager.InspectPublishedPorts(ctx, runtime, publishCID)
	if err != nil {
		return fmt.Errorf("inspect published ports: %w", err)
	}
	if len(ports) == 0 {
		m.serviceManager.DeactivateApp(instanceID)
		if len(def.Listeners) > 0 {
			return fmt.Errorf("no published ports observed")
		}
		return nil
	}
	if _, err := m.serviceManager.RestoreFromPodmanWithResumeTokenContext(pressure.WithTransitionContinuation(ctx), publicationResumeToken, instanceID, def.Listeners, ports); err != nil {
		return fmt.Errorf("restore service publication: %w", err)
	}
	m.serviceManager.SetAppContainerID(instanceID, publishCID)
	return nil
}

func (m *AppManager) cleanupManifestUpdateStagedRootfs(ctx context.Context, txn *ManifestUpdateTransaction) error {
	if txn == nil || (len(txn.StagedRootfs) == 0 && len(txn.CreatedRootfs) == 0) {
		return nil
	}
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return fmt.Errorf("cleanup staged rootfs: rootfs volume manager not configured")
	}
	previousActive := map[string]struct{}{}
	for _, volID := range txn.PreviousActiveRootfs {
		volID = strings.TrimSpace(volID)
		if volID != "" {
			previousActive[volID] = struct{}{}
		}
	}
	var errs []error
	detached := map[string]struct{}{}
	for _, volID := range txn.StagedRootfs {
		volID = strings.TrimSpace(volID)
		if volID == "" {
			continue
		}
		if _, ok := detached[volID]; ok {
			continue
		}
		if _, ok := previousActive[volID]; ok {
			continue
		}
		detached[volID] = struct{}{}
		if err := rootfs.DetachRootfs(ctx, volID); err != nil {
			errs = append(errs, fmt.Errorf("detach staged rootfs %s: %w", volID, err))
		}
	}
	destroyed := map[string]struct{}{}
	for _, volID := range txn.CreatedRootfs {
		volID = strings.TrimSpace(volID)
		if volID == "" {
			continue
		}
		if _, ok := destroyed[volID]; ok {
			continue
		}
		if _, ok := previousActive[volID]; ok {
			continue
		}
		if !rootfs.RootfsExists(volID) {
			continue
		}
		destroyed[volID] = struct{}{}
		if err := rootfs.DestroyRootfs(ctx, volID); err != nil {
			errs = append(errs, fmt.Errorf("destroy staged rootfs %s: %w", volID, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("cleanup staged rootfs: %w", err)
	}
	txn.StagedRootfs = nil
	txn.CreatedRootfs = nil
	txn.CandidateActiveRootfs = nil
	return nil
}

func manifestUpdateRemovedActiveRootfs(instanceID string, active map[string]string, prevDef, candidateDef *api.AppDefinition) []string {
	if strings.TrimSpace(instanceID) == "" || prevDef == nil || candidateDef == nil {
		return nil
	}
	removed := []string{}
	seen := map[string]struct{}{}
	add := func(volID string) {
		volID = strings.TrimSpace(volID)
		if volID == "" {
			return
		}
		if _, ok := seen[volID]; ok {
			return
		}
		seen[volID] = struct{}{}
		removed = append(removed, volID)
	}
	for svcName, oldSvc := range prevDef.Services {
		if oldSvc.Image == "" {
			continue
		}
		newSvc, exists := candidateDef.Services[svcName]
		if exists && newSvc.Image != "" {
			continue
		}
		if len(active) > 0 {
			add(active[svcName])
		}
		add(persistence.ServiceRootfsVolumeID(instanceID, svcName))
	}
	slices.Sort(removed)
	return removed
}

func manifestUpdateSupersededActiveRootfs(previous, candidate map[string]string) []string {
	if len(previous) == 0 || len(candidate) == 0 {
		return nil
	}
	candidateIDs := make(map[string]struct{}, len(candidate))
	for _, raw := range candidate {
		volID := strings.TrimSpace(raw)
		if volID != "" {
			candidateIDs[volID] = struct{}{}
		}
	}
	removed := []string{}
	seen := map[string]struct{}{}
	for _, raw := range previous {
		volID := strings.TrimSpace(raw)
		if volID == "" {
			continue
		}
		if _, stillActive := candidateIDs[volID]; stillActive {
			continue
		}
		if _, ok := seen[volID]; ok {
			continue
		}
		seen[volID] = struct{}{}
		removed = append(removed, volID)
	}
	slices.Sort(removed)
	return removed
}

func mergeManifestUpdateRootfsCleanup(existing, additional []string) []string {
	if len(additional) == 0 {
		return existing
	}
	out := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(out)+len(additional))
	for _, raw := range out {
		volID := strings.TrimSpace(raw)
		if volID != "" {
			seen[volID] = struct{}{}
		}
	}
	for _, raw := range additional {
		volID := strings.TrimSpace(raw)
		if volID == "" {
			continue
		}
		if _, ok := seen[volID]; ok {
			continue
		}
		seen[volID] = struct{}{}
		out = append(out, volID)
	}
	slices.Sort(out)
	return out
}

func activeRootfsForDefinition(active map[string]string, def *api.AppDefinition) map[string]string {
	if active == nil {
		return nil
	}
	out := map[string]string{}
	if anchor := strings.TrimSpace(active[networkAnchorServiceName]); anchor != "" {
		out[networkAnchorServiceName] = anchor
	}
	if def != nil {
		for svcName, svc := range def.Services {
			if svc.Image == "" {
				continue
			}
			if volID := strings.TrimSpace(active[svcName]); volID != "" {
				out[svcName] = volID
			}
		}
	}
	return out
}

func (m *AppManager) cleanupManifestUpdateRemovedRootfs(ctx context.Context, txn *ManifestUpdateTransaction) error {
	if txn == nil || len(txn.RemovedRootfs) == 0 {
		return nil
	}
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return fmt.Errorf("cleanup removed rootfs: rootfs volume manager not configured")
	}
	var errs []error
	remaining := make([]string, 0, len(txn.RemovedRootfs))
	seen := map[string]struct{}{}
	for _, raw := range txn.RemovedRootfs {
		volID := strings.TrimSpace(raw)
		if volID == "" {
			continue
		}
		if _, ok := seen[volID]; ok {
			continue
		}
		seen[volID] = struct{}{}
		if !rootfs.RootfsExists(volID) {
			continue
		}
		if err := rootfs.DestroyRootfs(ctx, volID); err != nil {
			remaining = append(remaining, volID)
			errs = append(errs, fmt.Errorf("destroy removed rootfs %s: %w", volID, err))
		}
	}
	txn.RemovedRootfs = remaining
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("cleanup removed rootfs: %w", err)
	}
	return nil
}

func (m *AppManager) restorePrecommitDataSnapshot(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, failedDef *api.AppDefinition, txn *ManifestUpdateTransaction) error {
	snapshotID := strings.TrimSpace(txn.PrecommitDataSnapshotID)
	if snapshotID == "" {
		if failedLVName := strings.TrimSpace(txn.FailedDataLVName); failedLVName != "" {
			return m.trackManifestFailedDataLV(state, appInst.InstanceID, failedLVName, txn.OperationID)
		}
		return nil
	}
	instanceID := appInst.InstanceID
	rollbacker, ok := m.currentVolumeManager().(dataVolumeRollbacker)
	if !ok {
		return fmt.Errorf("restore precommit data snapshot: volume manager does not support rollback")
	}
	failedLVName := strings.TrimSpace(txn.FailedDataLVName)
	if failedLVName == "" {
		failedLVName = manifestUpdateFailedDataLVName(instanceID, txn.OperationID)
	}
	if failedDef != nil {
		layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("ensure layout before data snapshot restore: %w", err)
		}
		if err := m.quiesceContainerGroup(ctx, state, appInst, failedDef, layout); err != nil {
			return fmt.Errorf("quiesce runtime before data snapshot restore: %w", err)
		}
	}
	renamesCommitted, snapshotPromoted, err := rollbacker.RollbackDataVolume(ctx, instanceID, snapshotID, failedLVName)
	if err != nil && !renamesCommitted {
		return fmt.Errorf("restore precommit data snapshot: %w", err)
	}
	if err != nil {
		txn.FailedDataLVName = failedLVName
		if snapshotPromoted {
			txn.PrecommitDataSnapshotID = ""
		}
		if trackErr := m.trackManifestFailedDataLV(state, instanceID, failedLVName, txn.OperationID); trackErr != nil {
			err = errors.Join(err, trackErr)
		}
		return fmt.Errorf("restore precommit data snapshot: LV rename committed=%t snapshot_promoted=%t: %w", renamesCommitted, snapshotPromoted, err)
	}
	txn.PrecommitDataSnapshotID = ""
	txn.FailedDataLVName = failedLVName
	return m.trackManifestFailedDataLV(state, instanceID, failedLVName, txn.OperationID)
}

func (m *AppManager) trackManifestFailedDataLV(state *FilesystemStateManager, instanceID, failedLVName, operationID string) error {
	failedLVName = strings.TrimSpace(failedLVName)
	if failedLVName == "" {
		return nil
	}
	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return fmt.Errorf("track failed manifest data LV: load tuple state: %w", err)
	}
	if ts == nil {
		ts = &TupleState{
			InstanceID:    instanceID,
			NextGenNumber: 1,
		}
	}
	for _, gen := range ts.Generations {
		if gen.FailedLVName == failedLVName {
			return nil
		}
	}
	failedAt := time.Now().UTC()
	ts.Generations = append(ts.Generations, TupleGeneration{
		ID:           "gen-failed-manifest-" + manifestUpdateShortOperationID(operationID),
		Status:       TupleStatusFailed,
		FailedLVName: failedLVName,
		FailedAt:     &failedAt,
		CreatedAt:    failedAt,
	})
	if err := state.StoreTupleState(instanceID, ts); err != nil {
		return fmt.Errorf("track failed manifest data LV: store tuple state: %w", err)
	}
	return nil
}

func (m *AppManager) rollbackTransactionProxyOIDCDelta(ctx context.Context, instanceID string, txn *ManifestUpdateTransaction, prevDef, failedDef *api.AppDefinition) error {
	if txn == nil || !txn.ProxyOIDCDeltaApplied {
		return nil
	}
	host := m.currentSyncHost()
	if host == nil {
		return fmt.Errorf("rollback proxy oidc delta: %w", ErrSyncHostUnavailable)
	}
	if err := m.applyProxyOIDCDelta(ctx, host, instanceID, failedDef, prevDef); err != nil {
		return fmt.Errorf("rollback proxy oidc delta: %w", err)
	}
	txn.ProxyOIDCDeltaApplied = false
	return nil
}

func manifestTransactionRuntimeTouched(txn *ManifestUpdateTransaction) bool {
	if txn == nil {
		return false
	}
	if txn.RuntimeTouched {
		return true
	}
	if strings.TrimSpace(txn.OperationKind) != "" {
		return false
	}
	switch txn.Phase {
	case "recreating_runtime", "ledger_committing", "restoring_previous", "restore_failed":
		return true
	default:
		return false
	}
}

func manifestTransactionRuntimeSwitchStarted(txn *ManifestUpdateTransaction) bool {
	if txn == nil {
		return false
	}
	if txn.RuntimeSwitchStarted || manifestTransactionRuntimeTouched(txn) {
		return true
	}
	if strings.TrimSpace(txn.OperationKind) != "" {
		return false
	}
	switch txn.Phase {
	case "runtime_switch_started":
		return true
	default:
		return false
	}
}

func markManifestTransactionRuntimeSwitchStarted(state *FilesystemStateManager, instanceID string, txn *ManifestUpdateTransaction) error {
	if txn == nil {
		return fmt.Errorf("manifest update transaction required")
	}
	if txn.RuntimeSwitchStarted {
		return nil
	}
	prevPhase := txn.Phase
	prevSwitchStarted := txn.RuntimeSwitchStarted
	prevUpdatedAt := txn.UpdatedAt
	txn.RuntimeSwitchStarted = true
	txn.Phase = "runtime_switch_started"
	txn.UpdatedAt = time.Now().UTC()
	if err := state.StoreManifestUpdateTransaction(instanceID, txn); err != nil {
		txn.Phase = prevPhase
		txn.RuntimeSwitchStarted = prevSwitchStarted
		txn.UpdatedAt = prevUpdatedAt
		return err
	}
	return nil
}

func markManifestTransactionRuntimeTouched(state *FilesystemStateManager, instanceID string, txn *ManifestUpdateTransaction) error {
	if txn == nil {
		return fmt.Errorf("manifest update transaction required")
	}
	prevPhase := txn.Phase
	prevSwitchStarted := txn.RuntimeSwitchStarted
	prevRuntimeTouched := txn.RuntimeTouched
	prevUpdatedAt := txn.UpdatedAt
	txn.RuntimeSwitchStarted = true
	txn.RuntimeTouched = true
	txn.Phase = "recreating_runtime"
	txn.UpdatedAt = time.Now().UTC()
	if err := state.StoreManifestUpdateTransaction(instanceID, txn); err != nil {
		txn.Phase = prevPhase
		txn.RuntimeSwitchStarted = prevSwitchStarted
		txn.RuntimeTouched = prevRuntimeTouched
		txn.UpdatedAt = prevUpdatedAt
		return err
	}
	return nil
}

func (m *AppManager) renderCustomManifestUpdateCandidate(ctx context.Context, req ManifestUpdateRequest) (*manifestUpdateCandidate, *ManifestUpdateResult, error) {
	if len(bytes.TrimSpace(req.RawTemplate)) == 0 && !req.CatalogPending {
		return nil, nil, fmt.Errorf("manifest update dry-run: empty manifest")
	}
	if err := m.ensureUnlocked(); err != nil {
		return nil, nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, nil, err
	}
	appInst, exists := state.GetApp(req.InstanceID)
	if !exists {
		return nil, nil, errAppNotFound(req.InstanceID)
	}
	if err := customManifestBasicEligibility(appInst, req.CatalogPending); err != nil {
		return nil, nil, err
	}
	curDef, err := state.GetAppDefinition(req.InstanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read current manifest: %w", err)
	}
	existingOIDC, err := manifestUpdateExistingOIDC(state, req.InstanceID, curDef)
	if err != nil {
		return nil, nil, err
	}

	rawTemplate := append([]byte(nil), req.RawTemplate...)
	systemContext := req.SystemContext
	sourceKind := InstallSourceKindCustom
	catalogSource := ""
	pendingSourceHash := ""
	var catalogState *InstallState
	var candidateInstallState *InstallState
	var inputState *InstallState
	if req.CatalogPending {
		pendingRaw, st, err := m.pendingCatalogManifestUpdateSource(state, req.InstanceID)
		if err != nil {
			return nil, nil, err
		}
		_, pendingHash, _, _ := st.pendingCatalogSourceForFlow(pendingCatalogReviewFlowManifest)
		rawTemplate = pendingRaw
		sourceKind = InstallSourceKindCatalog
		catalogSource = appInst.CatalogSource
		pendingSourceHash = pendingHash
		catalogState = st
		if st.InstallSystemCtx == nil {
			return nil, nil, fmt.Errorf("%w: catalog install system context is missing", ErrInstalledConfigUnavailable)
		}
		systemContext = *st.InstallSystemCtx
		if existingOIDC == nil {
			existingOIDC = st.OIDCCredentials
		}
	} else {
		inputState, err = manifestUpdateInputState(state, req.InstanceID)
		if err != nil {
			return nil, nil, err
		}
	}

	var preSchema *api.AppDefinition
	preSchemaRendered := false
	if req.CatalogPending {
		preSchema, preSchemaRendered, err = m.schemaForInstallStateRawTemplateWithOrigin(ctx, req.InstanceID, rawTemplate, catalogState, curDef)
		if err != nil {
			return nil, nil, fmt.Errorf("pending catalog manifest schema: %w", err)
		}
	} else {
		preSchema, err = ParseAppSchema(rawTemplate)
		if err != nil {
			return nil, nil, fmt.Errorf("parse manifest schema: %w", err)
		}
		PrepareSmartDefaultsForUpdate(preSchema, req.InstanceID)
	}
	if hasOIDCClient(preSchema.Services) && existingOIDC == nil {
		return manifestUpdateRejectedResult(req.InstanceID, curDef, appInst, "candidate adds service-level oidc_client; first v2 implementation rejects OIDC credential lifecycle changes")
	}
	var inputs map[string]interface{}
	var inputSensitive map[string]bool
	if req.CatalogPending {
		normalized, provenance, sensitive, err := normalizePendingCatalogManifestReviewInputs(preSchema.Inputs, catalogState, req, req.InstanceID)
		if err != nil {
			return nil, nil, err
		}
		inputs = normalized
		inputSensitive = sensitive
		nextState := *catalogState
		nextState.markCatalogSourceCommitted(req.InstanceID, appInst.CatalogSource, rawTemplate)
		nextState.InstallInputs, nextState.InputProvenance = persistedInstalledConfigLedger(inputs, provenance)
		nextState.InputSensitive = persistedInstallInputSensitive(nextState.InstallInputs, inputSensitive)
		candidateInstallState = &nextState
	} else {
		inputs, inputSensitive, err = normalizeManifestUpdateInputs(preSchema.Inputs, inputState, req.Inputs, req.RegenerateInputs, req.ClearInputs, req.InstanceID)
		if err != nil {
			return nil, nil, err
		}
	}
	renderState := inputState
	if req.CatalogPending {
		renderState = catalogState
	}
	unsafeRender := manifestRenderHasUnsafeValues(renderState, curDef, preSchema, inputs, existingOIDC)
	res, err := RunInstallPipeline(ctx, InstallPipelineInput{
		RawTemplate:   rawTemplate,
		UserInputs:    inputs,
		SystemContext: systemContext,
		InstanceID:    req.InstanceID,
		ExistingOIDC:  existingOIDC,
	}, nil, m.syncSelfSkippingLister(req.InstanceID))
	if err != nil {
		if unsafeRender {
			return manifestUpdateRejectedResult(req.InstanceID, curDef, appInst, sensitiveStructuralRenderRejectedReason)
		}
		return nil, nil, err
	}
	if hasOIDCClient(res.Definition.Services) && existingOIDC == nil {
		return manifestUpdateRejectedResult(req.InstanceID, curDef, appInst, "rendered candidate adds service-level oidc_client; first v2 implementation rejects OIDC credential lifecycle changes")
	}

	baseHash, err := canonicalManifestHash(curDef)
	if err != nil {
		return nil, nil, err
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return nil, nil, err
	}
	ledgerExists, ledgerRevision, ledgerSourceHash, err := loadInstallLedgerFingerprint(state, req.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	reuseState := renderState
	displayOldDef := curDef
	displayNewDef := res.Definition
	displayNew, displayChanged, err := m.renderDisplayDefinitionWithSentinels(ctx, req.InstanceID, rawTemplate, inputs, systemContext, reuseState, curDef, res.Definition, existingOIDC)
	if err != nil {
		return nil, nil, fmt.Errorf("render safe manifest update display: %w", err)
	}
	if displayChanged {
		displayNewDef = displayNew
		if reuseState != nil && len(bytes.TrimSpace(reuseState.RawTemplate)) > 0 && reuseState.InstallSystemCtx != nil {
			displayOld, _, err := m.renderDisplayDefinitionWithSentinels(ctx, req.InstanceID, reuseState.RawTemplate, reuseState.InstallInputs, *reuseState.InstallSystemCtx, reuseState, curDef, curDef, reuseState.OIDCCredentials)
			if err == nil {
				displayOldDef = displayOld
			}
		}
	}
	realPolicy, realSummary := evaluateCustomManifestUpdatePolicy(curDef, res.Definition)
	policy, summary := evaluateCustomManifestUpdatePolicy(displayOldDef, displayNewDef)
	if displayChanged {
		policy = mergeCustomManifestDisplayPolicy(realPolicy, policy)
		summary = mergeDisplaySummaryRuntimeSemantics(summary, realSummary, "rendered app structure changed through sensitive input")
		if reason := manifestSensitiveStructuralDriftReason(manifestSensitiveStructuralDrift(res.Definition, displayNewDef)); reason != "" {
			scrubSensitiveStructuralDriftClassification(&policy.Classification, reason)
			scrubSensitiveStructuralDriftSummary(&summary, reason)
		}
	} else {
		policy = realPolicy
		summary = realSummary
	}
	keptValueReview := m.manifestUpdateKeptValueReviewItems(ctx, req.InstanceID, reuseState, rawTemplate, preSchema, preSchemaRendered, inputs, systemContext, existingOIDC, req)
	if len(keptValueReview) > 0 {
		policy.Classification.KeptValueReview = keptValueReview
		for _, item := range keptValueReview {
			if reason := manifestUpdateKeptValueFailClosedReason(item); reason != "" {
				markManifestUpdateRejected(&policy.Classification, &summary, "kept_value_reuse_ambiguous", "inputs."+item.Field, reason)
				continue
			}
			addManifestUpdateRequiredConfirmation(&policy.Classification, item.Confirmation)
			addManifestUpdateRiskFlag(&policy.Classification, item.RiskKind)
			summary.WillPreserve = append(summary.WillPreserve, fmt.Sprintf("current value for input %s after review", item.Field))
		}
	}
	if policy.Classification.HasRejected {
		policy.Stageable = false
		if policy.Reason == "" {
			policy.Reason = policy.Classification.FirstRejectedReason
		}
	}
	diffKind := classifyDiff(cloneDefinitionForCompare(curDef), cloneDefinitionForCompare(res.Definition))
	candidateDigest := Sha256Hex(res.CanonicalBytes)
	unsafeDisplayFragments := manifestUpdateUnsafeDisplayFragments(reuseState, curDef, res.Definition, inputs, existingOIDC)
	sanitizeManifestUpdateSummaryForDisplay(&summary, unsafeDisplayFragments)
	sanitizeManifestUpdateClassificationForDisplay(&policy.Classification, unsafeDisplayFragments)
	policy.Reason = redactUnsafeDisplayText(policy.Reason, unsafeDisplayFragments)
	if !policy.Stageable {
		result := &ManifestUpdateResult{
			InstanceID:         req.InstanceID,
			BaseManifestHash:   baseHash,
			RuntimeFingerprint: runtimeFingerprint,
			RenderedAppID:      req.InstanceID,
			DiffKind:           diffKind.String(),
			UpdateClass:        policy.UpdateClass,
			Applicable:         false,
			BlockingReason:     policy.Reason,
			MetadataOnly:       policy.MetadataOnly,
			Summary:            summary,
		}
		applyManifestUpdateClassification(result, policy.Classification)
		return nil, result, nil
	}
	imagePlan, err := m.resolveManifestUpdateImagePlan(ctx, req.InstanceID, appInst, curDef, res.Definition, !policy.MetadataOnly)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve manifest update image plan: %w", err)
	}
	if len(imagePlan) > 0 {
		applyManifestUpdateImagePlanClassification(&policy.Classification, &summary, curDef, imagePlan)
		sanitizeManifestUpdateClassificationForDisplay(&policy.Classification, unsafeDisplayFragments)
	}
	policy.UpdateClass = policy.Classification.UpdateClass
	operationKind := TransitionOperationModifyApp
	transitionSourceKind := TransitionSourceCustomRaw
	pendingFlow := ""
	expectedPendingFlow := ""
	transitionSourceHash := Sha256Hex(rawTemplate)
	if req.CatalogPending {
		operationKind = TransitionOperationCatalogManifestReview
		transitionSourceKind = TransitionSourceCatalogPending
		pendingFlow = pendingCatalogReviewFlowManifest
		expectedPendingFlow = pendingCatalogReviewFlowManifest
		transitionSourceHash = pendingSourceHash
	}
	requiresSnapshot := false
	if policy.Classification.DataSafety != nil && policy.Classification.DataSafety.SnapshotRequired {
		requiresSnapshot = true
	}
	if !requiresSnapshot && !policy.MetadataOnly && len(imagePlan) > 0 && appHasPersistentStorage(curDef) {
		requiresSnapshot = true
	}
	legacyTransitionActive, err := transitionLegacyJournalExists(state, req.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	transitionPlan, err := PlanInstalledAppTransition(TransitionPlanInput{
		OperationKind:              operationKind,
		SourceKind:                 transitionSourceKind,
		PendingCatalogFlow:         pendingFlow,
		ExpectedPendingCatalogFlow: expectedPendingFlow,
		Mode:                       appInst.Mode(),
		Enabled:                    appInst.Enabled,
		RuntimeChanging:            !policy.MetadataOnly,
		LegacyTransactionActive:    legacyTransitionActive,
		BaseManifestHash:           baseHash,
		CandidateManifestHash:      candidateDigest,
		LedgerRevision:             ledgerRevision,
		SourceHash:                 transitionSourceHash,
		ImageRootfs:                transitionImageRootfsFromManifestPlan(imagePlan),
		Data: TransitionDataPolicy{
			SnapshotRequired:      requiresSnapshot,
			CandidateMayTouchData: !policy.MetadataOnly,
		},
		Runtime: TransitionRuntimePolicy{
			RuntimeFingerprint:   runtimeFingerprint,
			PreviousActiveRootfs: cloneStringMap(appInst.ActiveRootfs),
			PrimaryService:       primaryServiceFor(res.Definition, appInst),
		},
		Access: TransitionAccessPolicy{
			PrepareRequired: !policy.MetadataOnly,
		},
		Cleanup: TransitionCleanupPolicy{
			StagedRootfsKeys: transitionRootfsKeysFromManifestPlan(imagePlan),
		},
		RequiredConfirmations: policy.Classification.RequiredConfirmations,
	})
	if err != nil {
		return nil, nil, err
	}
	transitionPlanHash, err := transitionPlan.Hash()
	if err != nil {
		return nil, nil, err
	}
	result := &ManifestUpdateResult{
		InstanceID:         req.InstanceID,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: runtimeFingerprint,
		TransitionPlanHash: transitionPlanHash,
		RenderedAppID:      req.InstanceID,
		DiffKind:           diffKind.String(),
		UpdateClass:        policy.UpdateClass,
		Applicable:         true,
		MetadataOnly:       policy.MetadataOnly,
		Summary:            summary,
	}
	applyManifestUpdateClassification(result, policy.Classification)
	cand := &manifestUpdateCandidate{
		InstanceID:           req.InstanceID,
		RawTemplate:          append([]byte(nil), rawTemplate...),
		Inputs:               inputs,
		InputSensitive:       inputSensitive,
		SystemContext:        systemContext,
		BaseManifestHash:     baseHash,
		RuntimeFingerprint:   runtimeFingerprint,
		BaseLedgerExists:     ledgerExists,
		BaseLedgerRevision:   ledgerRevision,
		BaseLedgerSourceHash: ledgerSourceHash,
		SourceKind:           sourceKind,
		CatalogSource:        catalogSource,
		PendingSourceHash:    pendingSourceHash,
		CandidateDigest:      candidateDigest,
		InstallState:         candidateInstallState,
		OIDCCredentials:      res.OIDCCredentials,
		DiffKind:             diffKind,
		MetadataOnly:         policy.MetadataOnly,
		Definition:           res.Definition,
		Summary:              summary,
		Classification:       policy.Classification,
		ImagePlan:            cloneManifestUpdateImagePlan(imagePlan),
		TransitionPlan:       *transitionPlan,
		TransitionPlanHash:   transitionPlanHash,
	}
	return cand, result, nil
}

type customManifestPolicyResult struct {
	Allowed        bool
	Stageable      bool
	Reason         string
	MetadataOnly   bool
	UpdateClass    string
	Classification manifestUpdateClassification
}

type manifestUpdateClassification struct {
	UpdateClass           string
	Decisions             []ManifestUpdateDecision
	ExposureReview        []ManifestUpdateReviewItem
	RequiredConfirmations []string
	OperationRiskFlags    []string
	RuntimeReadiness      []string
	StagedImageRootfs     []string
	ListenerRoutingAuth   []string
	StorageBoundary       []string
	DataSafety            *ManifestUpdateDataSafetySummary
	KeptValueReview       []ManifestUpdateKeptValueReviewItem
	HasOperatorReview     bool
	HasRejected           bool
	RequiresV2Apply       bool
	FirstRejectedReason   string
	V1StructuralRestart   bool
}

func applyManifestUpdateClassification(result *ManifestUpdateResult, c manifestUpdateClassification) {
	if result == nil {
		return
	}
	if strings.TrimSpace(c.UpdateClass) != "" {
		result.UpdateClass = c.UpdateClass
	}
	result.Decisions = append([]ManifestUpdateDecision(nil), c.Decisions...)
	result.ExposureReview = append([]ManifestUpdateReviewItem(nil), c.ExposureReview...)
	result.KeptValueReview = append([]ManifestUpdateKeptValueReviewItem(nil), c.KeptValueReview...)
	result.RequiredConfirmations = append([]string(nil), c.RequiredConfirmations...)
	result.OperationRiskFlags = append([]string(nil), c.OperationRiskFlags...)
	result.RuntimeReadiness = append([]string(nil), c.RuntimeReadiness...)
	result.StagedImageRootfs = append([]string(nil), c.StagedImageRootfs...)
	result.ListenerRoutingAuth = append([]string(nil), c.ListenerRoutingAuth...)
	result.StorageBoundary = append([]string(nil), c.StorageBoundary...)
	result.DataSafety = c.DataSafety
}

func addManifestUpdateRequiredConfirmation(classification *manifestUpdateClassification, value string) {
	if classification == nil || strings.TrimSpace(value) == "" || slices.Contains(classification.RequiredConfirmations, value) {
		return
	}
	classification.RequiredConfirmations = append(classification.RequiredConfirmations, value)
}

func addManifestUpdateRiskFlag(classification *manifestUpdateClassification, value string) {
	if classification == nil || strings.TrimSpace(value) == "" || slices.Contains(classification.OperationRiskFlags, value) {
		return
	}
	classification.OperationRiskFlags = append(classification.OperationRiskFlags, value)
}

func applyManifestUpdateImagePlanClassification(classification *manifestUpdateClassification, summary *ManifestUpdateSummary, previousDef *api.AppDefinition, imagePlan []ManifestUpdateImagePlanItem) {
	if classification == nil || len(imagePlan) == 0 {
		return
	}
	classification.UpdateClass = "service_app_update_v2"
	classification.HasOperatorReview = true
	addManifestUpdateRequiredConfirmation(classification, "image_update_review")
	addManifestUpdateRiskFlag(classification, "image_refs_changed")
	classification.StagedImageRootfs = manifestUpdateImagePlanSummary(imagePlan)
	if summary != nil && !slices.Contains(summary.WillRestart, "existing containers will be recreated using planned image/rootfs updates") {
		summary.WillRestart = append(summary.WillRestart, "existing containers will be recreated using planned image/rootfs updates")
	}
	if appHasPersistentStorage(previousDef) && (classification.DataSafety == nil || !classification.DataSafety.SnapshotRequired) {
		classification.DataSafety = &ManifestUpdateDataSafetySummary{
			SnapshotRequired: true,
			Reason:           "planned image/rootfs refresh can mount existing persistent storage",
			FailureBehavior:  "reject before runtime switch if rollback snapshot capacity, headroom, health, or creation preflight fails",
			RollbackLimit:    "snapshot is for pre-commit failure recovery only and is hidden after successful commit",
		}
	}
}

func markManifestUpdateRejected(classification *manifestUpdateClassification, summary *ManifestUpdateSummary, flag, path, reason string) {
	if classification == nil || strings.TrimSpace(reason) == "" {
		return
	}
	classification.HasRejected = true
	if classification.FirstRejectedReason == "" {
		classification.FirstRejectedReason = reason
	}
	classification.Decisions = append(classification.Decisions, ManifestUpdateDecision{
		Flag:    flag,
		Path:    path,
		Outcome: "rejected",
		Summary: reason,
		Reason:  reason,
	})
	if summary != nil && !slices.Contains(summary.Rejected, reason) {
		summary.Rejected = append(summary.Rejected, reason)
	}
}

func manifestSensitiveStructuralDriftReason(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return sensitiveStructuralRenderRejectedReason
}

func scrubSensitiveStructuralDriftSummary(summary *ManifestUpdateSummary, reason string) {
	if summary == nil {
		return
	}
	summary.WillChange = []string{sensitiveStructuralRenderRejectedSummary}
	summary.WillRestart = nil
	summary.ExpectedInterruption = nil
	if !slices.Contains(summary.Rejected, reason) {
		summary.Rejected = append(summary.Rejected, reason)
	}
}

func scrubSensitiveStructuralDriftClassification(classification *manifestUpdateClassification, reason string) {
	if classification == nil || strings.TrimSpace(reason) == "" {
		return
	}
	classification.HasRejected = true
	classification.HasOperatorReview = false
	classification.RequiresV2Apply = false
	classification.V1StructuralRestart = false
	classification.FirstRejectedReason = reason
	classification.Decisions = []ManifestUpdateDecision{{
		Flag:    "sensitive_value_in_manifest_structure",
		Path:    "manifest",
		Outcome: "rejected",
		Summary: reason,
		Reason:  reason,
	}}
	classification.ExposureReview = nil
	classification.KeptValueReview = nil
	classification.RequiredConfirmations = nil
	classification.OperationRiskFlags = nil
	classification.RuntimeReadiness = nil
	classification.StagedImageRootfs = nil
	classification.ListenerRoutingAuth = nil
	classification.StorageBoundary = nil
	classification.DataSafety = nil
}

func manifestSensitiveStructuralDrift(realDef, displayDef *api.AppDefinition) []string {
	realSig := manifestPublicStructureSignature(realDef)
	displaySig := manifestPublicStructureSignature(displayDef)
	changed := make([]string, 0)
	for path, realValue := range realSig {
		displayValue, ok := displaySig[path]
		if !ok || displayValue != realValue {
			changed = append(changed, path)
		}
	}
	for path := range displaySig {
		if _, ok := realSig[path]; !ok {
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	return changed
}

func manifestPublicStructureSignature(def *api.AppDefinition) map[string]string {
	out := map[string]string{}
	if def == nil {
		return out
	}
	data, err := SerializeAppDefinition(def)
	if err != nil {
		out["$"] = "unserializable"
		return out
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		out["$"] = "unparseable"
		return out
	}
	manifestCollectPublicStructure(&root, nil, "$", true, out)
	return out
}

func manifestCollectPublicStructure(node *yaml.Node, segments []string, path string, includeScalars bool, out map[string]string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		manifestCollectPublicStructure(node.Content[0], segments, path, includeScalars, out)
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			label := fmt.Sprintf("#%d", i/2)
			if key != nil && key.Kind == yaml.ScalarNode && strings.TrimSpace(key.Value) != "" {
				label = key.Value
			}
			childPath := path + "." + manifestPublicStructurePathLabel(label)
			out[childPath+"#key"] = label
			childSegments := append(append([]string(nil), segments...), label)
			childScalars := includeScalars && !manifestPublicStructureDataValuePath(childSegments)
			manifestCollectPublicStructure(value, childSegments, childPath, childScalars, out)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			manifestCollectPublicStructure(child, append(append([]string(nil), segments...), fmt.Sprintf("[%d]", i)), fmt.Sprintf("%s[%d]", path, i), includeScalars, out)
		}
	case yaml.ScalarNode:
		if includeScalars {
			out[path] = node.Value
		}
	}
}

func manifestPublicStructureDataValuePath(segments []string) bool {
	if len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		if segment == "app_config" {
			return true
		}
	}
	last := segments[len(segments)-1]
	return last == "environment" || last == "env"
}

func manifestPublicStructurePathLabel(label string) string {
	if strings.TrimSpace(label) == "" {
		return "<empty>"
	}
	replacer := strings.NewReplacer("\\", "\\\\", ".", "\\.", "[", "\\[", "]", "\\]", "#", "\\#")
	return replacer.Replace(label)
}

func manifestUpdateKeptValueFailClosedReason(item ManifestUpdateKeptValueReviewItem) string {
	if item.RiskKind != "kept_secret_semantic_changed" {
		return ""
	}
	if slices.Contains(item.SemanticDelta, "previous source schema unavailable") ||
		slices.Contains(item.SemanticDelta, "previous source usage unavailable") ||
		slices.Contains(item.SemanticDelta, "candidate source usage unavailable") {
		if strings.TrimSpace(item.BlockingReason) != "" {
			return item.BlockingReason
		}
		return fmt.Sprintf("current stored value for input %s cannot be safely reused because previous meaning or usage could not be verified", item.Field)
	}
	return ""
}

func (m *AppManager) manifestUpdateKeptValueReviewItems(ctx context.Context, instanceID string, st *InstallState, newRaw []byte, newSchema *api.AppDefinition, newSchemaRendered bool, normalizedInputs map[string]interface{}, systemContext InstallSystemContext, existingOIDC *OIDCCredentials, req ManifestUpdateRequest) []ManifestUpdateKeptValueReviewItem {
	if st == nil || st.InstallInputs == nil || newSchema == nil {
		return nil
	}
	oldRaw := st.RawTemplate
	var oldSchema *api.AppDefinition
	oldSchemaRendered := false
	oldSchemaUnavailable := false
	if len(bytes.TrimSpace(oldRaw)) > 0 {
		var err error
		oldSchema, oldSchemaRendered, err = m.schemaForInstallStateRawTemplateWithOrigin(ctx, instanceID, oldRaw, st, nil)
		if err != nil {
			oldSchemaUnavailable = true
		}
	} else {
		oldSchemaUnavailable = true
	}
	regen := map[string]bool{}
	for _, name := range req.RegenerateInputs {
		regen[name] = true
	}
	clearInputs := map[string]bool{}
	for _, name := range req.ClearInputs {
		clearInputs[name] = true
	}
	names := make([]string, 0, len(newSchema.Inputs))
	for name := range newSchema.Inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	items := []ManifestUpdateKeptValueReviewItem{}
	for _, name := range names {
		if name == "__app_address__" {
			continue
		}
		if _, ok := st.InstallInputs[name]; !ok {
			continue
		}
		if _, provided := req.Inputs[name]; provided || regen[name] || clearInputs[name] {
			continue
		}
		newSpec := newSchema.Inputs[name]
		oldSpec, oldSpecPresent := api.AppInput{}, false
		if oldSchema != nil {
			oldSpec, oldSpecPresent = oldSchema.Inputs[name]
		}
		oldUnsafeInputs := displayUnsafeStoredInputNames(st, oldSchema, oldSchema)
		newUnsafeInputs := displayUnsafeStoredInputNames(st, oldSchema, newSchema)
		var oldUsage []string
		oldUsageOK := false
		if !oldSchemaUnavailable {
			oldUsage, oldUsageOK = manifestRenderedInputUsageRefs(oldRaw, st.InstallInputs, st.InstallSystemCtx, st.OIDCCredentials, name, oldUnsafeInputs)
		}
		newUsage, newUsageOK := manifestRenderedInputUsageRefs(newRaw, normalizedInputs, &systemContext, existingOIDC, name, newUnsafeInputs)
		oldUsageRaw := manifestInputUsageRefs(oldRaw, name)
		newUsageRaw := manifestInputUsageRefs(newRaw, name)
		delta := manifestInputSemanticDelta(name, oldSpec, oldSpecPresent, newSpec)
		if oldSchemaUnavailable {
			delta = append(delta, "previous source schema unavailable")
		}
		if !oldUsageOK {
			delta = append(delta, "previous source usage unavailable")
			oldUsage = manifestInputUsageRefs(oldRaw, name)
		}
		if !newUsageOK {
			delta = append(delta, "candidate source usage unavailable")
			newUsage = manifestInputUsageRefs(newRaw, name)
		}
		if !slices.Equal(oldUsage, newUsage) || !slices.Equal(oldUsageRaw, newUsageRaw) {
			delta = append(delta, "template usage changed")
		}
		if len(delta) == 0 {
			continue
		}
		riskKind := "kept_value_semantic_changed"
		storedSensitive := manifestUpdateStoredInputSensitive(name, newSpec, st)
		if inputIsSensitive(name, newSpec) || newSpec.Generate || storedSensitive || (oldSpecPresent && (inputIsSensitive(name, oldSpec) || oldSpec.Generate)) {
			riskKind = "kept_secret_semantic_changed"
		} else if slices.Contains(delta, "template usage changed") {
			riskKind = "kept_value_usage_changed"
		}
		blockingReason := ""
		if oldSchemaUnavailable {
			blockingReason = "previous source schema unavailable; replace or regenerate the current stored value"
		} else if !oldUsageOK || !newUsageOK {
			blockingReason = "template usage unavailable; replace or regenerate the current stored value"
		}
		items = append(items, ManifestUpdateKeptValueReviewItem{
			Field:          name,
			RiskKind:       riskKind,
			OldSemantic:    manifestInputSemanticSummary(name, oldSpec, oldSpecPresent, oldSchemaRendered),
			NewSemantic:    manifestInputSemanticSummary(name, newSpec, true, newSchemaRendered),
			SemanticDelta:  delta,
			OldUsage:       oldUsage,
			NewUsage:       newUsage,
			Confirmation:   keptValueReviewConfirmationID(name),
			BlockingReason: blockingReason,
		})
	}
	return items
}

func manifestInputSemanticSummary(name string, spec api.AppInput, present bool, redactDefault bool) []string {
	if !present {
		return []string{"not declared"}
	}
	defaultSummary := manifestInputDefaultSummary(spec)
	if spec.Default != nil && (redactDefault || inputIsSensitive(name, spec) || spec.Generate) {
		defaultSummary = "<redacted>"
	}
	out := []string{
		"type=" + strings.TrimSpace(spec.Type),
		fmt.Sprintf("required=%t", spec.Required),
		fmt.Sprintf("generate=%t", spec.Generate),
		fmt.Sprintf("sensitive=%t", inputIsSensitive(name, spec)),
		"default=" + defaultSummary,
	}
	if strings.TrimSpace(spec.Label) != "" {
		out = append(out, "label=<present>")
	}
	if strings.TrimSpace(spec.Description) != "" {
		out = append(out, "description=<present>")
	}
	if spec.Validation != nil {
		if strings.TrimSpace(spec.Validation.Regex) != "" {
			out = append(out, "validation_regex=<present>")
		}
		if strings.TrimSpace(spec.Validation.Message) != "" {
			out = append(out, "validation_message=<present>")
		}
	}
	return out
}

func manifestInputSemanticDelta(name string, oldSpec api.AppInput, oldPresent bool, newSpec api.AppInput) []string {
	if !oldPresent {
		return []string{"input was not declared in previous source"}
	}
	delta := []string{}
	if strings.TrimSpace(oldSpec.Type) != strings.TrimSpace(newSpec.Type) {
		delta = append(delta, fmt.Sprintf("type changed from %q to %q", oldSpec.Type, newSpec.Type))
	}
	if oldSpec.Required != newSpec.Required {
		delta = append(delta, fmt.Sprintf("required changed from %t to %t", oldSpec.Required, newSpec.Required))
	}
	if oldSpec.Generate != newSpec.Generate {
		delta = append(delta, fmt.Sprintf("generate changed from %t to %t", oldSpec.Generate, newSpec.Generate))
	}
	if inputIsSensitive(name, oldSpec) != inputIsSensitive(name, newSpec) {
		delta = append(delta, fmt.Sprintf("sensitive changed from %t to %t", inputIsSensitive(name, oldSpec), inputIsSensitive(name, newSpec)))
	}
	if !manifestInputDefaultsEqual(oldSpec, newSpec) {
		delta = append(delta, "default changed")
	}
	if strings.TrimSpace(oldSpec.Label) != strings.TrimSpace(newSpec.Label) {
		delta = append(delta, "label changed")
	}
	if strings.TrimSpace(oldSpec.Description) != strings.TrimSpace(newSpec.Description) {
		delta = append(delta, "description changed")
	}
	if !reflect.DeepEqual(oldSpec.Validation, newSpec.Validation) {
		delta = append(delta, "validation changed")
	}
	return delta
}

func manifestInputDefaultSummary(spec api.AppInput) string {
	if spec.Default == nil {
		return "<none>"
	}
	value := normalizeInputValueForValidation(spec.Type, spec.Default)
	if encoded, err := json.Marshal(value); err == nil {
		return string(encoded)
	}
	return fmt.Sprint(value)
}

func manifestInputDefaultsEqual(oldSpec, newSpec api.AppInput) bool {
	oldValue := normalizeInputValueForValidation(oldSpec.Type, oldSpec.Default)
	newValue := normalizeInputValueForValidation(newSpec.Type, newSpec.Default)
	if reflect.DeepEqual(oldValue, newValue) {
		return true
	}
	oldStrings, oldOK := inputArrayStrings(oldValue)
	newStrings, newOK := inputArrayStrings(newValue)
	return oldOK && newOK && slices.Equal(oldStrings, newStrings)
}

func manifestInputUsageRefs(raw []byte, name string) []string {
	if len(bytes.TrimSpace(raw)) == 0 || strings.TrimSpace(name) == "" {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	out := []string{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.Contains(trimmed, "Inputs") || !strings.Contains(trimmed, name) {
			continue
		}
		if len(trimmed) > 140 {
			trimmed = trimmed[:137] + "..."
		}
		out = append(out, fmt.Sprintf("line %d: %s", i+1, trimmed))
	}
	return out
}

func manifestRenderedInputUsageRefs(raw []byte, inputs map[string]interface{}, systemCtx *InstallSystemContext, creds *OIDCCredentials, name string, unsafeInputs map[string]struct{}) ([]string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || strings.TrimSpace(name) == "" || systemCtx == nil {
		return nil, false
	}
	sentinel := inputUsageScanSentinelPrefix + Sha256Hex([]byte(name))[:16] + "__"
	scanInputs := make(map[string]interface{}, len(inputs)+1)
	unsafeKeyFragments := []string{"<redacted>"}
	for key, value := range inputs {
		if key == name {
			continue
		}
		if _, unsafe := unsafeInputs[key]; unsafe || inputNameLooksSensitive(key) {
			value := inputUsageScanSentinelPrefix + Sha256Hex([]byte(key))[:16] + "__"
			scanInputs[key] = value
			unsafeKeyFragments = append(unsafeKeyFragments, inputUsageSentinelKeyFragments(value)...)
			continue
		}
		if fragment := strings.TrimSpace(fmt.Sprint(value)); fragment != "" {
			unsafeKeyFragments = append(unsafeKeyFragments, fragment)
		}
		scanInputs[key] = value
	}
	scanInputs[name] = sentinel
	if creds != nil {
		clientID, clientSecret := inputUsageOIDCSentinels()
		unsafeKeyFragments = append(unsafeKeyFragments, inputUsageSentinelKeyFragments(clientID)...)
		unsafeKeyFragments = append(unsafeKeyFragments, inputUsageSentinelKeyFragments(clientSecret)...)
	}
	rendered, err := RenderManifest(raw, scanInputs, manifestInputUsageSystemContext(*systemCtx, creds))
	if err != nil {
		return nil, false
	}
	var root yaml.Node
	if err := yaml.Unmarshal(rendered, &root); err != nil {
		return nil, false
	}
	refs := []string{}
	collectManifestInputUsageRefs(&root, "$", sentinel, unsafeKeyFragments, &refs)
	slices.Sort(refs)
	return slices.Compact(refs), true
}

func manifestInputUsageSystemContext(ctx InstallSystemContext, creds *OIDCCredentials) map[string]interface{} {
	out := map[string]interface{}{
		"Domain":       ctx.Domain,
		"Architecture": ctx.Architecture,
		"Timezone":     ctx.Timezone,
	}
	if creds != nil {
		clientID, clientSecret := inputUsageOIDCSentinels()
		out["Auth"] = map[string]string{
			"Issuer":       ctx.IssuerHint,
			"ClientID":     clientID,
			"ClientSecret": clientSecret,
		}
	}
	return out
}

func inputUsageOIDCSentinels() (string, string) {
	return inputUsageScanSentinelPrefix + "oidc_client_id__", inputUsageScanSentinelPrefix + "oidc_client_secret__"
}

func collectManifestInputUsageRefs(node *yaml.Node, path, sentinel string, unsafeKeyFragments []string, refs *[]string) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			collectManifestInputUsageRefs(child, path, sentinel, unsafeKeyFragments, refs)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			keyPath := manifestInputUsageKeyPath(path, key, sentinel, unsafeKeyFragments, i/2)
			childPath := keyPath + ".value"
			if key != nil && key.Kind == yaml.ScalarNode && strings.Contains(key.Value, sentinel) {
				*refs = append(*refs, keyPath+" (key)")
			}
			collectManifestInputUsageRefs(value, childPath, sentinel, unsafeKeyFragments, refs)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			collectManifestInputUsageRefs(child, fmt.Sprintf("%s[%d]", path, i), sentinel, unsafeKeyFragments, refs)
		}
	case yaml.ScalarNode:
		if strings.Contains(node.Value, sentinel) {
			*refs = append(*refs, path)
		}
	}
}

func manifestInputUsageKeyPath(path string, key *yaml.Node, sentinel string, unsafeKeyFragments []string, index int) string {
	fallback := fmt.Sprintf("%s.<key%d>", path, index)
	if key == nil || key.Kind != yaml.ScalarNode {
		return fallback
	}
	label := strings.TrimSpace(key.Value)
	if label == "" {
		return fallback
	}
	if manifestInputUsageKeyLabelUnsafe(label, sentinel, unsafeKeyFragments) {
		return fallback + "#" + Sha256Hex([]byte(label))[:8]
	}
	label = manifestInputUsagePathLabel(label)
	if label == "" {
		return fallback
	}
	return path + "." + label
}

func manifestInputUsageKeyLabelUnsafe(label, sentinel string, unsafeFragments []string) bool {
	if label == "" || strings.Contains(label, sentinel) || strings.Contains(label, inputUsageScanSentinelPrefix) {
		return true
	}
	for _, fragment := range unsafeFragments {
		fragment = strings.TrimSpace(fragment)
		if fragment != "" && strings.Contains(label, fragment) {
			return true
		}
	}
	return false
}

func inputUsageSentinelKeyFragments(value string) []string {
	value = strings.TrimSpace(value)
	if len(value) < 4 {
		return nil
	}
	fragments := []string{}
	for i := 4; i <= len(value); i++ {
		fragments = append(fragments, value[:i])
	}
	return fragments
}

func manifestInputUsagePathLabel(label string) string {
	const maxUsagePathLabel = 64
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxUsagePathLabel {
			break
		}
	}
	return strings.Trim(b.String(), "_")
}

func keptValueReviewConfirmationID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "kept_value_review"
	}
	return "kept_value_review:" + name
}

func evaluateCustomManifestUpdatePolicy(oldDef, newDef *api.AppDefinition) (customManifestPolicyResult, ManifestUpdateSummary) {
	oldCmp := cloneDefinitionForCompare(oldDef)
	newCmp := cloneDefinitionForCompare(newDef)
	summary := ManifestUpdateSummary{
		WillPreserve: []string{
			"app identity and primary listener",
			"immutable existing persistent storage declarations",
		},
	}
	classification := manifestUpdateClassification{
		UpdateClass: "manifest_update_v1",
		RuntimeReadiness: []string{
			"app detail remains authoritative for active operation state",
		},
		StorageBoundary: []string{
			"existing persistent storage declarations must keep the same name, mount path, type, backing, and service attachment",
		},
	}
	addDecision := func(flag, path, outcome, text, reason string) {
		decision := ManifestUpdateDecision{
			Flag:    flag,
			Path:    path,
			Outcome: outcome,
			Summary: text,
			Reason:  reason,
		}
		classification.Decisions = append(classification.Decisions, decision)
		switch outcome {
		case "rejected":
			classification.HasRejected = true
			if classification.FirstRejectedReason == "" {
				if reason != "" {
					classification.FirstRejectedReason = reason
				} else {
					classification.FirstRejectedReason = text
				}
			}
			if reason != "" {
				summary.Rejected = append(summary.Rejected, reason)
			} else {
				summary.Rejected = append(summary.Rejected, text)
			}
		case "operator_review":
			classification.UpdateClass = "service_app_update_v2"
			classification.HasOperatorReview = true
			summary.WillChange = append(summary.WillChange, text)
		case "supported":
			summary.WillChange = append(summary.WillChange, text)
			switch flag {
			case "persistent_storage_added", "service_environment_changed", "app_config_changed":
				classification.V1StructuralRestart = true
			}
		}
	}
	addConfirmation := func(value string) {
		if strings.TrimSpace(value) == "" || slices.Contains(classification.RequiredConfirmations, value) {
			return
		}
		classification.RequiredConfirmations = append(classification.RequiredConfirmations, value)
	}
	addRiskFlag := func(value string) {
		if strings.TrimSpace(value) == "" || slices.Contains(classification.OperationRiskFlags, value) {
			return
		}
		classification.OperationRiskFlags = append(classification.OperationRiskFlags, value)
	}
	addRuntimeReadiness := func(value string) {
		if strings.TrimSpace(value) == "" || slices.Contains(classification.RuntimeReadiness, value) {
			return
		}
		classification.RuntimeReadiness = append(classification.RuntimeReadiness, value)
	}
	addExposureReview := func(path, kind, oldValue, newValue string) {
		confirmation := exposureReviewConfirmationID(path)
		addConfirmation(confirmation)
		classification.ExposureReview = append(classification.ExposureReview, ManifestUpdateReviewItem{
			Path:         path,
			Kind:         kind,
			Old:          oldValue,
			New:          newValue,
			Confirmation: confirmation,
		})
	}
	addRejected := func(flag, path, reason string) {
		addDecision(flag, path, "rejected", reason, reason)
	}
	if oldCmp == nil || newCmp == nil {
		addRejected("manifest_required", "", "current and candidate manifests are required")
		return customManifestPolicyResult{Allowed: false, Stageable: false, Reason: classification.FirstRejectedReason, UpdateClass: classification.UpdateClass, Classification: classification}, summary
	}
	if piccoloModeFromExtensions(oldCmp.Extensions) != ModeService || piccoloModeFromExtensions(newCmp.Extensions) != ModeService {
		addRejected("app_type_changed", "x-piccolo.mode", "service app update only supports service-mode apps")
	}
	if oldCmp.Type != newCmp.Type {
		addRejected("app_type_changed", "type", "app type changes require reinstall")
	}
	if oldCmp.WorkspaceName != newCmp.WorkspaceName {
		addRejected("workspace_identity_changed", "workspace_name", "workspace identity changes require reinstall")
	}
	if oldCmp.PrimaryService != newCmp.PrimaryService {
		addRejected("primary_service_changed", "primary_service", "primary_service changes require reinstall or a future flow")
	}
	if !reflect.DeepEqual(oldCmp.Listeners, newCmp.Listeners) {
		classification.UpdateClass = "service_app_update_v2"
		addDecision("listener_topology_changed", "listeners", "operator_review", "listener topology, routing, or auth changed", "")
		addRiskFlag("listener_or_auth_changed")
		classification.ListenerRoutingAuth = append(classification.ListenerRoutingAuth, "listener endpoint/routing/auth changes require prepared routing and explicit review")
		addListenerExposureReviewItems(oldCmp.Listeners, newCmp.Listeners, addExposureReview)
	}
	if !reflect.DeepEqual(oldCmp.Permissions, newCmp.Permissions) {
		addRejected("permissions_changed", "permissions", "permission changes require reinstall or a future flow")
	}
	if !reflect.DeepEqual(oldCmp.Resources, newCmp.Resources) {
		addRejected("resources_changed", "resources", "resource policy changes require reinstall or a future flow")
	}
	if !reflect.DeepEqual(oldCmp.HealthCheck, newCmp.HealthCheck) {
		addRejected("healthcheck_changed", "healthcheck", "healthcheck changes require reinstall or a future flow")
	}
	if !reflect.DeepEqual(oldCmp.AppConfig, newCmp.AppConfig) {
		if appHasPersistentStorage(oldCmp) {
			classification.UpdateClass = "service_app_update_v2"
			addDecision("app_config_changed", "app_config", "operator_review", "rendered app_config changed and existing persistent data may be affected", "")
			addConfirmation("data_impact_review")
			addRiskFlag("data_semantic_config_changed")
		} else {
			addDecision("app_config_changed", "app_config", "supported", "rendered app_config changed", "")
			classification.V1StructuralRestart = true
		}
	}
	if !reflect.DeepEqual(oldCmp.Auth, newCmp.Auth) {
		addRejected("top_level_auth_changed", "auth", "top-level auth changes require reinstall or a future listener-auth flow")
	}
	if !reflect.DeepEqual(oldCmp.Extensions, newCmp.Extensions) {
		addRejected("x_piccolo_changed", "x-piccolo", "x-piccolo extension changes require reinstall or a future RFC")
	}
	if !reflect.DeepEqual(oldCmp.Environment, newCmp.Environment) {
		addRejected("top_level_environment_changed", "environment", "top-level environment changes are not supported; use service environment entries")
	}
	if !reflect.DeepEqual(oldCmp.Lifecycle, newCmp.Lifecycle) {
		addRejected("lifecycle_changed", "lifecycle", "lifecycle changes require reinstall or a future flow")
	}

	classifyStorageDiff("top-level storage", "storage", oldCmp.Storage, newCmp.Storage, appPersistentStorageNames(oldCmp), addDecision, addConfirmation)

	oldPersistentNames := appPersistentStorageNames(oldCmp)
	serviceNames := make([]string, 0, len(oldCmp.Services)+len(newCmp.Services))
	for name := range oldCmp.Services {
		serviceNames = append(serviceNames, name)
	}
	for name := range newCmp.Services {
		if _, exists := oldCmp.Services[name]; !exists {
			serviceNames = append(serviceNames, name)
		}
	}
	slices.Sort(serviceNames)
	for _, name := range serviceNames {
		oldSvc, oldExists := oldCmp.Services[name]
		newSvc, newExists := newCmp.Services[name]
		path := fmt.Sprintf("services.%s", name)
		switch {
		case !oldExists && newExists:
			classification.UpdateClass = "service_app_update_v2"
			outcome := "operator_review"
			reason := ""
			if newSvc.OIDCClient != nil {
				outcome = "rejected"
				reason = fmt.Sprintf("service %q declares oidc_client; first v2 implementation rejects OIDC credential lifecycle changes for added services", name)
			} else if serviceMountsExistingPersistentStorage(newSvc, oldPersistentNames) {
				outcome = "rejected"
				reason = fmt.Sprintf("service %q mounts pre-existing persistent storage; new attachments to existing storage require a future flow", name)
			}
			addDecision("services_added", path, outcome, fmt.Sprintf("service %q added", name), reason)
			if outcome == "operator_review" {
				addConfirmation("service_shape_review")
				addRiskFlag("service_shape_changed")
				addRuntimeReadiness("candidate service additions require private runtime verification")
			}
			continue
		case oldExists && !newExists:
			classification.UpdateClass = "service_app_update_v2"
			if oldSvc.OIDCClient != nil {
				addRejected("services_removed", path, fmt.Sprintf("service %q declares oidc_client and cannot be removed in v2", name))
			} else if serviceHasPersistentStorage(oldSvc) {
				addRejected("services_removed", path, fmt.Sprintf("service %q has persistent storage references and cannot be removed in v2", name))
			} else {
				addDecision("services_removed", path, "operator_review", fmt.Sprintf("service %q removed", name), "")
				addConfirmation("service_removal_review")
				addRiskFlag("service_shape_changed")
			}
			continue
		}
		if oldSvc.Image != newSvc.Image {
			classification.UpdateClass = "service_app_update_v2"
			addDecision("image_refs_changed", path+".image", "operator_review", fmt.Sprintf("service %q image reference changed", name), "")
			addConfirmation("image_update_review")
			addRiskFlag("image_refs_changed")
			classification.StagedImageRootfs = append(classification.StagedImageRootfs, fmt.Sprintf("service %s requires immutable digest resolution and staged rootfs identity", name))
		}
		if !reflect.DeepEqual(oldSvc.BindPorts, newSvc.BindPorts) {
			classification.UpdateClass = "service_app_update_v2"
			addDecision("service_bind_ports_changed", path+".bind_ports", "operator_review", fmt.Sprintf("service %q bind_ports changed", name), "")
			addExposureReview(path+".bind_ports", "service_bind_ports", intSliceSummary(oldSvc.BindPorts), intSliceSummary(newSvc.BindPorts))
			addRiskFlag("listener_or_auth_changed")
		}
		if !reflect.DeepEqual(oldSvc.After, newSvc.After) {
			classification.UpdateClass = "service_app_update_v2"
			addDecision("service_startup_order_changed", path+".after", "operator_review", fmt.Sprintf("service %q startup order changed", name), "")
			addConfirmation("service_shape_review")
		}
		if oldSvc.Init != newSvc.Init {
			addRejected("init_changed", path+".init", fmt.Sprintf("service %q init mode changed; init behavior requires reinstall or a future flow", name))
		}
		if !reflect.DeepEqual(oldSvc.InitScript, newSvc.InitScript) {
			addRejected("init_script_changed", path+".init_script", fmt.Sprintf("service %q init_script changed; init scripts are not replayed by v2", name))
		}
		classifyStorageDiff(fmt.Sprintf("service %s storage", name), path+".storage", oldSvc.Storage, newSvc.Storage, oldPersistentNames, addDecision, addConfirmation)
		if !reflect.DeepEqual(oldSvc.OIDCClient, newSvc.OIDCClient) {
			if oidcAuthorizePathsOnlyChanged(oldSvc.OIDCClient, newSvc.OIDCClient) {
				classification.UpdateClass = "service_app_update_v2"
				classification.RequiresV2Apply = true
				addDecision("proxy_oidc_authorize_paths_changed", path+".oidc_client.authorize_paths", "supported", fmt.Sprintf("service %q proxy OIDC authorize paths changed", name), "")
				addRiskFlag("listener_or_auth_changed")
				classification.ListenerRoutingAuth = append(classification.ListenerRoutingAuth, fmt.Sprintf("service %s proxy OIDC routing delta must be fingerprinted", name))
				addExposureReview(path+".oidc_client.authorize_paths", "proxy_oidc_authorize_paths", stringSliceSummary(oldSvc.OIDCClient.AuthorizePaths), stringSliceSummary(newSvc.OIDCClient.AuthorizePaths))
			} else {
				addRejected("oidc_client_changed", path+".oidc_client", fmt.Sprintf("service %q OIDC client lifecycle or credential material changed; first v2 implementation rejects this", name))
			}
		}
		for _, key := range changedStringMapKeys(oldSvc.Environment, newSvc.Environment) {
			if appHasPersistentStorage(oldCmp) {
				classification.UpdateClass = "service_app_update_v2"
				addDecision("service_environment_changed", path+".environment."+key, "operator_review", fmt.Sprintf("service %s environment key %s", name, key), "")
				addConfirmation("data_impact_review")
				addRiskFlag("data_semantic_config_changed")
			} else {
				addDecision("service_environment_changed", path+".environment."+key, "supported", fmt.Sprintf("service %s environment key %s", name, key), "")
				classification.V1StructuralRestart = true
			}
		}
	}

	oldBytes, _ := SerializeAppDefinition(oldCmp)
	newBytes, _ := SerializeAppDefinition(newCmp)
	diffKind := classifyDiff(oldCmp, newCmp)
	metadataOnly := diffKind == DiffKindNone && !bytes.Equal(oldBytes, newBytes)
	if metadataOnly {
		summary.WillChange = append(summary.WillChange, "manifest metadata/input schema")
		addDecision("manifest_metadata_changed", "inputs", "supported", "manifest metadata/input schema changed", "")
	} else if classification.V1StructuralRestart {
		summary.WillRestart = append(summary.WillRestart, "existing containers will be recreated using current images/rootfs")
		summary.ExpectedInterruption = append(summary.ExpectedInterruption, "services may disconnect while containers restart")
	}
	if classification.UpdateClass == "service_app_update_v2" && !classification.HasRejected {
		addRuntimeReadiness("candidate runtime must be verified privately before data/runtime commit")
	}
	requiresCandidateRuntime := classification.HasOperatorReview || classification.RequiresV2Apply || classification.V1StructuralRestart
	if requiresCandidateRuntime && !manifestUpdateHasProbeableRuntimeListener(newCmp) {
		addRejected("runtime_readiness_unprobeable", "listeners", "runtime-changing updates with only UDP listeners require a future UDP readiness probe")
	}
	if requiresCandidateRuntime && appHasPersistentStorage(oldCmp) {
		classification.DataSafety = &ManifestUpdateDataSafetySummary{
			SnapshotRequired: true,
			Reason:           "candidate containers may mount existing persistent storage",
			FailureBehavior:  "reject before runtime switch if rollback snapshot capacity, headroom, health, or creation preflight fails",
			RollbackLimit:    "snapshot is for pre-commit failure recovery only and is hidden after successful commit",
		}
	} else {
		classification.DataSafety = &ManifestUpdateDataSafetySummary{
			SnapshotRequired: false,
			Reason:           "no v2 candidate runtime with existing persistent storage is required for this dry run",
			RollbackLimit:    "no user-initiated data rollback is created by manifest update",
		}
	}
	if len(summary.WillChange) == 0 && len(summary.WillRestart) == 0 && len(summary.Rejected) == 0 {
		summary.WillChange = append(summary.WillChange, "no runtime changes")
	}
	reason := classification.FirstRejectedReason
	allowed := !classification.HasRejected && !classification.HasOperatorReview && !classification.RequiresV2Apply
	if classification.HasOperatorReview {
		allowed = false
		if reason == "" {
			reason = "service app update v2 review/apply is required before this candidate can be applied"
			for _, decision := range classification.Decisions {
				if decision.Outcome == "operator_review" && strings.TrimSpace(decision.Summary) != "" {
					reason += ": " + decision.Summary
					break
				}
			}
		}
	} else if classification.RequiresV2Apply {
		allowed = false
		if reason == "" {
			reason = "service app update v2 apply is required before this candidate can be applied"
			for _, decision := range classification.Decisions {
				if decision.Outcome == "supported" && strings.TrimSpace(decision.Summary) != "" {
					reason += ": " + decision.Summary
					break
				}
			}
		}
	}
	if classification.HasRejected && reason == "" {
		reason = "candidate contains unsupported manifest changes"
	}
	return customManifestPolicyResult{
		Allowed:        allowed,
		Stageable:      !classification.HasRejected,
		Reason:         reason,
		MetadataOnly:   metadataOnly,
		UpdateClass:    classification.UpdateClass,
		Classification: classification,
	}, summary
}

func classifyStorageDiff(label, path string, oldStorage, newStorage *api.AppStorage, preExistingPersistent map[string]bool, addDecision func(flag, path, outcome, text, reason string), addConfirmation func(value string)) {
	oldPersistent, oldTemporary := storageMaps(oldStorage)
	newPersistent, newTemporary := storageMaps(newStorage)
	for name, oldVol := range oldPersistent {
		newVol, exists := newPersistent[name]
		itemPath := path + ".persistent." + name
		if !exists {
			addDecision("existing_persistent_storage_removed", itemPath, "rejected", fmt.Sprintf("%s persistent volume %q removed", label, name), fmt.Sprintf("%s persistent volume %q was removed; persistent storage removal requires a future migration flow", label, name))
			continue
		}
		if !reflect.DeepEqual(oldVol, newVol) {
			addDecision("existing_persistent_storage_mutated", itemPath, "rejected", fmt.Sprintf("%s persistent volume %q changed", label, name), fmt.Sprintf("%s persistent volume %q changed; existing persistent storage declarations are immutable in v2", label, name))
		}
	}
	for name := range newPersistent {
		if _, exists := oldPersistent[name]; exists {
			continue
		}
		itemPath := path + ".persistent." + name
		if preExistingPersistent[name] {
			addDecision("existing_service_attaches_pre_existing_storage", itemPath, "rejected", fmt.Sprintf("%s persistent volume %q attached", label, name), fmt.Sprintf("%s persistent volume %q already exists elsewhere; new attachments to pre-existing persistent storage are rejected", label, name))
			continue
		}
		addDecision("persistent_storage_added", itemPath, "supported", fmt.Sprintf("%s persistent volume %s added", label, name), "")
	}
	if !reflect.DeepEqual(oldTemporary, newTemporary) {
		addDecision("temporary_storage_changed", path+".temporary", "operator_review", fmt.Sprintf("%s temporary storage changed", label), "")
		addConfirmation("service_shape_review")
	}
}

func appHasPersistentStorage(def *api.AppDefinition) bool {
	return len(appPersistentStorageNames(def)) > 0
}

func appPersistentStorageNames(def *api.AppDefinition) map[string]bool {
	out := map[string]bool{}
	if def == nil {
		return out
	}
	addStorageNames(out, def.Storage)
	for _, svc := range def.Services {
		addStorageNames(out, svc.Storage)
	}
	return out
}

func addStorageNames(out map[string]bool, st *api.AppStorage) {
	if st == nil {
		return
	}
	for name := range st.Persistent {
		out[name] = true
	}
}

func serviceHasPersistentStorage(svc api.AppService) bool {
	if svc.Storage == nil {
		return false
	}
	return len(svc.Storage.Persistent) > 0
}

func serviceMountsExistingPersistentStorage(svc api.AppService, preExisting map[string]bool) bool {
	if svc.Storage == nil {
		return false
	}
	for name := range svc.Storage.Persistent {
		if preExisting[name] {
			return true
		}
	}
	return false
}

func oidcAuthorizePathsOnlyChanged(oldOIDC, newOIDC *api.ServiceOIDCClient) bool {
	if oldOIDC == nil || newOIDC == nil {
		return false
	}
	oldCmp := *oldOIDC
	newCmp := *newOIDC
	oldCmp.AuthorizePaths = nil
	newCmp.AuthorizePaths = nil
	return reflect.DeepEqual(oldCmp, newCmp) && !equalStringSliceUnordered(oldOIDC.AuthorizePaths, newOIDC.AuthorizePaths)
}

func listenerSummary(listeners []api.AppListener) string {
	if len(listeners) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(listeners))
	for _, l := range listeners {
		parts = append(parts, fmt.Sprintf("%s:%d/%s/%s primary=%t", l.Name, l.GuestPort, l.Flow, l.Protocol, l.Primary))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

func addListenerExposureReviewItems(oldListeners, newListeners []api.AppListener, add func(path, kind, oldValue, newValue string)) {
	oldByName := make(map[string]api.AppListener, len(oldListeners))
	newByName := make(map[string]api.AppListener, len(newListeners))
	names := make([]string, 0, len(oldListeners)+len(newListeners))
	for _, l := range oldListeners {
		oldByName[l.Name] = l
		names = append(names, l.Name)
	}
	for _, l := range newListeners {
		newByName[l.Name] = l
		if _, exists := oldByName[l.Name]; !exists {
			names = append(names, l.Name)
		}
	}
	slices.Sort(names)

	added := false
	for _, name := range names {
		oldL, oldExists := oldByName[name]
		newL, newExists := newByName[name]
		path := "listeners." + name
		switch {
		case oldExists && !newExists:
			add(path, "listener_removed", listenerDetailSummary(oldL), "none")
			added = true
		case !oldExists && newExists:
			add(path, "listener_added", "none", listenerDetailSummary(newL))
			added = true
		case oldExists && newExists && !reflect.DeepEqual(oldL, newL):
			add(path, "listener_changed", listenerDetailSummary(oldL), listenerDetailSummary(newL))
			added = true
		}
	}
	if !added {
		add("listeners", "listener_order_or_duplicate_shape", listenerSummary(oldListeners), listenerSummary(newListeners))
	}
}

func exposureReviewConfirmationID(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return exposureReviewConfirmation
	}
	return exposureReviewConfirmation + ":" + path
}

func listenerDetailSummary(l api.AppListener) string {
	parts := []string{
		fmt.Sprintf("guest=%d", l.GuestPort),
		fmt.Sprintf("flow=%s", l.Flow),
		fmt.Sprintf("protocol=%s", l.Protocol),
		fmt.Sprintf("primary=%t", l.Primary),
		fmt.Sprintf("host_route=%t", l.IsEligibleForHostRouting()),
	}
	if l.PortClaim != nil {
		parts = append(parts, fmt.Sprintf("port_claim=%d", *l.PortClaim))
	}
	if l.TLSWrap {
		parts = append(parts, "tls_wrap=true")
	}
	if len(l.RemotePorts) > 0 {
		parts = append(parts, "remote_ports="+intSliceSummary(l.RemotePorts))
	}
	if len(l.Middleware) > 0 {
		names := make([]string, 0, len(l.Middleware))
		for _, mw := range l.Middleware {
			names = append(names, strings.TrimSpace(mw.Name))
		}
		parts = append(parts, "middleware="+strings.Join(names, "|"))
	}
	parts = append(parts, listenerAuthSummary(l.Auth))
	parts = append(parts, connectionAuthSummary(l.ConnectionAuth))
	return strings.Join(parts, " ")
}

func listenerAuthSummary(auth *api.ListenerAuth) string {
	if auth == nil || len(auth.Rules) == 0 {
		return "auth=default_protected"
	}
	rules := make([]string, 0, len(auth.Rules))
	for _, rule := range auth.Rules {
		rules = append(rules, strings.Join([]string{
			strings.TrimSpace(rule.Type),
			strings.TrimSpace(rule.Path),
			strings.TrimSpace(rule.Strategy),
		}, ":"))
	}
	return "auth=" + strings.Join(rules, "|")
}

func connectionAuthSummary(auth *api.ConnectionAuth) string {
	if auth == nil || (!auth.HasIPRules() && !auth.RequiresMTLS()) {
		return "connection_auth=default_allow"
	}
	parts := []string{}
	if auth.HasIPRules() {
		defaultAction := strings.TrimSpace(auth.Default)
		if defaultAction == "" {
			defaultAction = "allow"
		}
		parts = append(parts, "default="+defaultAction)
		for _, rule := range auth.Rules {
			parts = append(parts, strings.TrimSpace(rule.Match)+":"+strings.TrimSpace(rule.Strategy))
		}
	}
	if auth.RequiresMTLS() {
		parts = append(parts, "mtls="+strings.TrimSpace(auth.MTLS.Verifier.Type))
	}
	return "connection_auth=" + strings.Join(parts, "|")
}

func manifestUpdateHasProbeableRuntimeListener(def *api.AppDefinition) bool {
	if def == nil {
		return false
	}
	for _, listener := range def.Listeners {
		if listener.Flow != api.FlowUDP {
			return true
		}
	}
	return false
}

func stringSliceSummary(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	parts := append([]string(nil), values...)
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

func intSliceSummary(values []int) string {
	if len(values) == 0 {
		return "none"
	}
	parts := append([]int(nil), values...)
	slices.Sort(parts)
	labels := make([]string, 0, len(parts))
	for _, value := range parts {
		labels = append(labels, fmt.Sprintf("%d", value))
	}
	return strings.Join(labels, ", ")
}

func missingManifestUpdateConfirmations(required, accepted []string) []string {
	acceptedSet := make(map[string]bool, len(accepted))
	for _, value := range accepted {
		value = strings.TrimSpace(value)
		if value != "" {
			acceptedSet[value] = true
		}
	}
	missing := []string{}
	for _, value := range required {
		value = strings.TrimSpace(value)
		if value != "" && !acceptedSet[value] && !slices.Contains(missing, value) {
			missing = append(missing, value)
		}
	}
	return missing
}

func existingServiceImageRefsChanged(oldDef, newDef *api.AppDefinition) bool {
	if oldDef == nil || newDef == nil {
		return false
	}
	for name, oldSvc := range oldDef.Services {
		newSvc, exists := newDef.Services[name]
		if !exists {
			continue
		}
		if oldSvc.Image != newSvc.Image {
			return true
		}
	}
	return false
}

type manifestUpdateRuntimeStage struct {
	prebuiltRootfs                map[string]*rootfsMountInfo
	candidateActiveRootfs         map[string]string
	stagedRootfs                  []string
	createdRootfs                 []string
	requiresPrecommitDataSnapshot bool
}

type manifestUpdateImagePlanRequest struct {
	serviceName             string
	entryKind               string
	action                  string
	reason                  string
	imageRef                string
	expectedCanonicalDigest string
	previousRootfsVolumeID  string
}

func (m *AppManager) resolveManifestUpdateImagePlan(ctx context.Context, instanceID string, appInst *AppInstance, curDef, candidateDef *api.AppDefinition, planRuntimeRootfs bool) ([]ManifestUpdateImagePlanItem, error) {
	if candidateDef == nil || !planRuntimeRootfs {
		return nil, nil
	}
	requests, err := m.manifestUpdateImagePlanRequests(ctx, appInst, curDef, candidateDef)
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, nil
	}
	if m.containerManager == nil {
		return nil, fmt.Errorf("container manager not configured")
	}
	ephRT, ephCleanup, err := m.newFlattenRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("create ephemeral runtime: %w", err)
	}
	defer ephCleanup()

	slices.SortFunc(requests, func(a, b manifestUpdateImagePlanRequest) int {
		if a.serviceName != b.serviceName {
			return strings.Compare(a.serviceName, b.serviceName)
		}
		return strings.Compare(a.entryKind, b.entryKind)
	})

	plan := make([]ManifestUpdateImagePlanItem, 0, len(requests))
	for _, req := range requests {
		item, err := m.resolveManifestUpdateImagePlanItem(ctx, ephRT, instanceID, req)
		if err != nil {
			return nil, err
		}
		plan = append(plan, item)
	}
	return plan, nil
}

func (m *AppManager) manifestUpdateImagePlanRequests(ctx context.Context, appInst *AppInstance, curDef, candidateDef *api.AppDefinition) ([]manifestUpdateImagePlanRequest, error) {
	if candidateDef == nil {
		return nil, nil
	}
	activeRootfs := map[string]string{}
	if appInst != nil {
		activeRootfs = appInst.ActiveRootfs
	}
	rootfs := m.currentRootfsManager()

	serviceNames := make([]string, 0, len(candidateDef.Services))
	for svcName := range candidateDef.Services {
		serviceNames = append(serviceNames, svcName)
	}
	slices.Sort(serviceNames)

	requests := make([]manifestUpdateImagePlanRequest, 0)
	for _, svcName := range serviceNames {
		newSvc := candidateDef.Services[svcName]
		imageRef := strings.TrimSpace(newSvc.Image)
		if imageRef == "" {
			continue
		}
		oldSvc, existed := api.AppService{}, false
		if curDef != nil {
			oldSvc, existed = curDef.Services[svcName]
		}
		previousRootfs := strings.TrimSpace(activeRootfs[svcName])
		switch {
		case !existed:
			requests = append(requests, manifestUpdateImagePlanRequest{
				serviceName:            svcName,
				entryKind:              manifestUpdateImageEntryAppService,
				action:                 manifestUpdateImageActionStage,
				reason:                 "service added",
				imageRef:               imageRef,
				previousRootfsVolumeID: previousRootfs,
			})
			continue
		case strings.TrimSpace(oldSvc.Image) != imageRef:
			requests = append(requests, manifestUpdateImagePlanRequest{
				serviceName:            svcName,
				entryKind:              manifestUpdateImageEntryAppService,
				action:                 manifestUpdateImageActionStage,
				reason:                 "image reference changed",
				imageRef:               imageRef,
				previousRootfsVolumeID: previousRootfs,
			})
			continue
		}
		req, refresh, err := m.manifestUpdateRootfsRefreshRequest(ctx, rootfs, svcName, manifestUpdateImageEntryAppService, imageRef, previousRootfs)
		if err != nil {
			return nil, err
		}
		if refresh {
			requests = append(requests, req)
		}
	}

	anchorImageRef := strings.TrimSpace(networkAnchorImage())
	if anchorImageRef != "" {
		req, refresh, err := m.manifestUpdateRootfsRefreshRequest(ctx, rootfs, networkAnchorServiceName, manifestUpdateImageEntryRuntimeAnchor, anchorImageRef, strings.TrimSpace(activeRootfs[networkAnchorServiceName]))
		if err != nil {
			return nil, err
		}
		if refresh {
			requests = append(requests, req)
		}
	}
	return requests, nil
}

func (m *AppManager) manifestUpdateRootfsRefreshRequest(ctx context.Context, rootfs persistence.RootfsVolumeManager, serviceName, entryKind, imageRef, previousRootfs string) (manifestUpdateImagePlanRequest, bool, error) {
	targetCanonical := ""
	if isDigestPinned(imageRef) {
		targetCanonical = canonicalImageDigestKey(imageRef)
		if targetCanonical == "" {
			return manifestUpdateImagePlanRequest{}, false, fmt.Errorf("image %s (%s) has no canonical digest", imageRef, serviceName)
		}
	} else {
		remoteDigest, err := m.resolveRemoteImageDigest(ctx, imageRef)
		if err != nil {
			return manifestUpdateImagePlanRequest{}, false, fmt.Errorf("resolve image digest %s (%s): %w", imageRef, serviceName, err)
		}
		targetCanonical = canonicalImageDigestKey(remoteDigest)
		if targetCanonical == "" {
			return manifestUpdateImagePlanRequest{}, false, fmt.Errorf("resolve image digest %s (%s): digest unavailable", imageRef, serviceName)
		}
	}
	matches, _, err := manifestUpdateRootfsProvesDigest(rootfs, previousRootfs, targetCanonical)
	if err == nil && matches {
		return manifestUpdateImagePlanRequest{}, false, nil
	}
	reason := "current rootfs identity unavailable"
	if err == nil && !matches {
		reason = "current image digest differs from active rootfs"
	}
	if entryKind == manifestUpdateImageEntryRuntimeAnchor {
		reason = "runtime network anchor " + reason
	}
	return manifestUpdateImagePlanRequest{
		serviceName:             serviceName,
		entryKind:               entryKind,
		action:                  manifestUpdateImageActionRefresh,
		reason:                  reason,
		imageRef:                imageRef,
		expectedCanonicalDigest: targetCanonical,
		previousRootfsVolumeID:  previousRootfs,
	}, true, nil
}

func (m *AppManager) resolveManifestUpdateImagePlanItem(ctx context.Context, ephRT container.PodmanRuntime, instanceID string, req manifestUpdateImagePlanRequest) (ManifestUpdateImagePlanItem, error) {
	imageRef := strings.TrimSpace(req.imageRef)
	serviceName := strings.TrimSpace(req.serviceName)
	if serviceName == "" {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("image plan service name is empty")
	}
	if imageRef == "" {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("image plan image ref is empty for %s", serviceName)
	}
	if err := m.containerManager.PullImage(ctx, ephRT, imageRef); err != nil {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("pull image %s (%s): %w", imageRef, serviceName, err)
	}
	imgConfig, err := m.containerManager.InspectImage(ctx, ephRT, imageRef)
	if err != nil {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("inspect image %s (%s): %w", imageRef, serviceName, err)
	}
	digest := imageConfigDigest(imgConfig)
	if digest == "" {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("inspect image %s (%s): digest unavailable", imageRef, serviceName)
	}
	canonicalDigest := canonicalImageDigestKey(digest)
	if canonicalDigest == "" {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("inspect image %s (%s): canonical digest unavailable", imageRef, serviceName)
	}
	if req.expectedCanonicalDigest != "" && canonicalDigest != req.expectedCanonicalDigest {
		return ManifestUpdateImagePlanItem{}, fmt.Errorf("%w: image digest for %s changed during planning", ErrManifestUpdateConflict, serviceName)
	}
	entryKind := req.entryKind
	if entryKind == "" {
		entryKind = manifestUpdateImageEntryAppService
	}
	action := req.action
	if action == "" {
		action = manifestUpdateImageActionStage
	}
	return ManifestUpdateImagePlanItem{
		ServiceName:            serviceName,
		EntryKind:              entryKind,
		Action:                 action,
		Reason:                 req.reason,
		ImageRef:               imageRef,
		ResolvedDigest:         digest,
		CanonicalDigest:        canonicalDigest,
		RootfsVolumeID:         persistence.VersionedServiceRootfsVolumeID(instanceID, serviceName, persistence.ShortDigest(canonicalDigest)),
		PreviousRootfsVolumeID: strings.TrimSpace(req.previousRootfsVolumeID),
	}, nil
}

func (m *AppManager) stageManifestUpdateRootfs(ctx context.Context, taskType, instanceID string, appInst *AppInstance, curDef, candidateDef *api.AppDefinition, expectedPlan []ManifestUpdateImagePlanItem, requiresPrecommitDataSnapshot bool, markCreatedRootfs func(string) error) (*manifestUpdateRuntimeStage, error) {
	planned, err := m.resolveManifestUpdateImagePlan(ctx, instanceID, appInst, curDef, candidateDef, true)
	if err != nil {
		return nil, err
	}
	if err := manifestUpdateImagePlanMatchesExpected(expectedPlan, planned); err != nil {
		return nil, err
	}
	if len(planned) == 0 {
		if !requiresPrecommitDataSnapshot {
			return nil, nil
		}
		return &manifestUpdateRuntimeStage{requiresPrecommitDataSnapshot: true}, nil
	}
	expectedByService, err := manifestUpdateImagePlanByService(expectedPlan)
	if err != nil {
		return nil, err
	}
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return nil, fmt.Errorf("rootfs volume manager not configured")
	}
	if m.containerManager == nil {
		return nil, fmt.Errorf("container manager not configured")
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("ensure layout: %w", err)
	}
	mode := piccoloModeFromExtensions(candidateDef.Extensions)
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return nil, fmt.Errorf("podman runtime: %w", err)
	}

	prebuiltRootfs := map[string]*rootfsMountInfo{}
	if existing, err := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, curDef, appInst); err != nil {
		return nil, fmt.Errorf("attach current rootfs: %w", err)
	} else {
		for svcName, info := range existing {
			if svcName == networkAnchorServiceName || candidateDef.Services[svcName].Image != "" {
				prebuiltRootfs[svcName] = info
			}
		}
	}

	candidateActiveRootfs := cloneStringMap(appInst.ActiveRootfs)
	if candidateActiveRootfs == nil {
		candidateActiveRootfs = map[string]string{}
	}
	for svcName := range curDef.Services {
		if _, exists := candidateDef.Services[svcName]; !exists {
			delete(candidateActiveRootfs, svcName)
			delete(prebuiltRootfs, svcName)
		}
	}

	var idmap persistence.IDMapConfig
	if runtime.Credential != nil {
		idmap = persistence.IDMapConfig{
			AppUID: runtime.Credential.Uid,
			AppGID: runtime.Credential.Gid,
		}
		username := container.AppUsername(instanceID)
		if subStart, subCount, lookupErr := container.LookupSubUIDRange(username); lookupErr == nil {
			idmap.SubUIDStart = subStart
			idmap.SubUIDCount = subCount
			idmap.SubGIDStart = subStart
			idmap.SubGIDCount = subCount
		} else {
			log.Printf("WARN: manifest update %s: subuid lookup failed for %s: %v", instanceID, username, lookupErr)
		}
	}

	ephRT, ephCleanup, err := m.newFlattenRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("create ephemeral runtime: %w", err)
	}
	defer ephCleanup()

	stagedRootfs := make([]string, 0, len(planned))
	createdRootfs := make([]string, 0, len(planned))
	for i, plannedItem := range planned {
		svcName := plannedItem.ServiceName
		expected := expectedByService[svcName]
		imageRef := plannedItem.ImageRef
		m.emitProgress(ctx, taskType, instanceID, taskPhasePullingImage, 10+(20*i)/len(planned), fmt.Sprintf("Pulling image for %s", manifestUpdateImagePlanDisplayName(plannedItem)), false, nil)
		if err := m.containerManager.PullImage(ctx, ephRT, imageRef); err != nil {
			return nil, fmt.Errorf("pull image %s (%s): %w", imageRef, svcName, err)
		}
		imgConfig, err := m.containerManager.InspectImage(ctx, ephRT, imageRef)
		if err != nil {
			return nil, fmt.Errorf("inspect image %s (%s): %w", imageRef, svcName, err)
		}
		digest := imageConfigDigest(imgConfig)
		if digest == "" {
			return nil, fmt.Errorf("inspect image %s (%s): digest unavailable", imageRef, svcName)
		}
		canonicalDigest := canonicalImageDigestKey(digest)
		if canonicalDigest == "" {
			return nil, fmt.Errorf("inspect image %s (%s): canonical digest unavailable", imageRef, svcName)
		}
		if canonicalDigest != manifestUpdateImagePlanCanonicalDigest(expected) {
			return nil, fmt.Errorf("%w: image digest for %s changed after dry run", ErrManifestUpdateConflict, manifestUpdateImagePlanErrorSubject(plannedItem))
		}
		volID := persistence.VersionedServiceRootfsVolumeID(instanceID, svcName, persistence.ShortDigest(canonicalDigest))
		if volID != expected.RootfsVolumeID {
			return nil, fmt.Errorf("%w: rootfs identity for %s changed after dry run", ErrManifestUpdateConflict, svcName)
		}
		var handle persistence.RootfsHandle
		if rootfs.RootfsExists(volID) {
			if err := verifyRootfsIdentityForDigest(rootfs, volID, canonicalDigest); err != nil {
				return nil, fmt.Errorf("existing planned rootfs for %s does not match planned image identity: %w", manifestUpdateImagePlanDisplayName(plannedItem), err)
			}
			handle, err = rootfs.AttachRootfs(ctx, volID)
			if err != nil {
				return nil, fmt.Errorf("attach staged rootfs %s: %w", volID, err)
			}
		} else {
			if markCreatedRootfs != nil {
				if err := markCreatedRootfs(volID); err != nil {
					return nil, err
				}
			}
			m.emitProgress(ctx, taskType, instanceID, taskPhaseCreatingRootfs, 30+(15*i)/len(planned), fmt.Sprintf("Creating rootfs for %s", manifestUpdateImagePlanDisplayName(plannedItem)), false, nil)
			handle, err = rootfs.CreateServiceRootfs(ctx, persistence.ServiceRootfsRequest{
				InstanceID:    instanceID,
				ServiceName:   svcName,
				ImageDigest:   canonicalDigest,
				ImageRef:      imageRef,
				IDMap:         idmap,
				VolumeID:      volID,
				ImageSizeHint: imgConfig.Size,
				PrePulledDir:  filepath.Dir(ephRT.Root),
			})
			if err != nil {
				return nil, fmt.Errorf("create rootfs for %s: %w", manifestUpdateImagePlanDisplayName(plannedItem), err)
			}
			createdRootfs = append(createdRootfs, volID)
		}
		goldenCfg, cfgErr := m.readImageConfigForRootfs(ctx, rootfs, canonicalDigest)
		if cfgErr != nil {
			log.Printf("WARN: manifest update %s: read image config for %s: %v", instanceID, svcName, cfgErr)
		}
		prebuiltRootfs[svcName] = &rootfsMountInfo{handle: handle, imgConfig: goldenCfg}
		candidateActiveRootfs[svcName] = volID
		stagedRootfs = append(stagedRootfs, volID)
	}

	return &manifestUpdateRuntimeStage{
		prebuiltRootfs:                prebuiltRootfs,
		candidateActiveRootfs:         candidateActiveRootfs,
		stagedRootfs:                  stagedRootfs,
		createdRootfs:                 createdRootfs,
		requiresPrecommitDataSnapshot: requiresPrecommitDataSnapshot,
	}, nil
}

func manifestUpdateRootfsProvesDigest(rootfs persistence.RootfsVolumeManager, volumeID, targetCanonicalDigest string) (bool, string, error) {
	if rootfs == nil {
		return false, "", fmt.Errorf("rootfs volume manager not configured")
	}
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return false, "", fmt.Errorf("active rootfs volume id is empty")
	}
	identity, err := rootfs.ReadRootfsImageIdentity(volumeID)
	if err != nil {
		return false, "", err
	}
	actual := canonicalImageDigestKey(identity.BaseImageDigest)
	if actual == "" {
		return false, "", fmt.Errorf("active rootfs %s missing canonical image digest", volumeID)
	}
	return actual == targetCanonicalDigest, actual, nil
}

func verifyRootfsIdentityForDigest(rootfs persistence.RootfsVolumeManager, volumeID, targetCanonicalDigest string) error {
	matches, actual, err := manifestUpdateRootfsProvesDigest(rootfs, volumeID, targetCanonicalDigest)
	if err != nil {
		return fmt.Errorf("verify existing rootfs %s identity: %w", strings.TrimSpace(volumeID), err)
	}
	if !matches {
		return fmt.Errorf("verify existing rootfs %s identity: base image digest %s does not match planned digest %s", strings.TrimSpace(volumeID), actual, targetCanonicalDigest)
	}
	return nil
}

func manifestUpdateImagePlanMatchesExpected(expected, planned []ManifestUpdateImagePlanItem) error {
	expectedByService, err := manifestUpdateImagePlanByService(expected)
	if err != nil {
		return err
	}
	if len(expectedByService) != len(planned) {
		return fmt.Errorf("%w: reviewed image plan no longer matches candidate image plan", ErrManifestUpdateConflict)
	}
	for _, plannedItem := range planned {
		expectedItem, ok := expectedByService[plannedItem.ServiceName]
		if !ok {
			return fmt.Errorf("%w: reviewed image plan missing %s", ErrManifestUpdateConflict, plannedItem.ServiceName)
		}
		if err := manifestUpdateCompareImagePlanItem(expectedItem, plannedItem); err != nil {
			return err
		}
	}
	return nil
}

func manifestUpdateCompareImagePlanItem(expected, planned ManifestUpdateImagePlanItem) error {
	serviceName := planned.ServiceName
	if strings.TrimSpace(expected.ImageRef) != strings.TrimSpace(planned.ImageRef) {
		return fmt.Errorf("%w: image ref for %s changed after dry run", ErrManifestUpdateConflict, serviceName)
	}
	if manifestUpdateImagePlanCanonicalDigest(expected) != manifestUpdateImagePlanCanonicalDigest(planned) {
		return fmt.Errorf("%w: image digest for %s changed after dry run", ErrManifestUpdateConflict, manifestUpdateImagePlanErrorSubject(planned))
	}
	if strings.TrimSpace(expected.RootfsVolumeID) != strings.TrimSpace(planned.RootfsVolumeID) {
		return fmt.Errorf("%w: rootfs identity for %s changed after dry run", ErrManifestUpdateConflict, serviceName)
	}
	if manifestUpdateImagePlanAction(expected) != manifestUpdateImagePlanAction(planned) {
		return fmt.Errorf("%w: image action for %s changed after dry run", ErrManifestUpdateConflict, serviceName)
	}
	if manifestUpdateImagePlanEntryKind(expected) != manifestUpdateImagePlanEntryKind(planned) {
		return fmt.Errorf("%w: image entry kind for %s changed after dry run", ErrManifestUpdateConflict, serviceName)
	}
	return nil
}

func manifestUpdateImagePlanCanonicalDigest(item ManifestUpdateImagePlanItem) string {
	if canonical := strings.TrimSpace(item.CanonicalDigest); canonical != "" {
		return canonical
	}
	return canonicalImageDigestKey(item.ResolvedDigest)
}

func manifestUpdateImagePlanAction(item ManifestUpdateImagePlanItem) string {
	if action := strings.TrimSpace(item.Action); action != "" {
		return action
	}
	return manifestUpdateImageActionStage
}

func manifestUpdateImagePlanEntryKind(item ManifestUpdateImagePlanItem) string {
	if kind := strings.TrimSpace(item.EntryKind); kind != "" {
		return kind
	}
	return manifestUpdateImageEntryAppService
}

func manifestUpdateImagePlanDisplayName(item ManifestUpdateImagePlanItem) string {
	if manifestUpdateImagePlanEntryKind(item) == manifestUpdateImageEntryRuntimeAnchor {
		return "Piccolo runtime support"
	}
	return item.ServiceName
}

func manifestUpdateImagePlanErrorSubject(item ManifestUpdateImagePlanItem) string {
	if manifestUpdateImagePlanEntryKind(item) == manifestUpdateImageEntryRuntimeAnchor {
		return "Piccolo runtime support"
	}
	return "service " + item.ServiceName
}

func manifestUpdateImagePlanByService(plan []ManifestUpdateImagePlanItem) (map[string]ManifestUpdateImagePlanItem, error) {
	out := make(map[string]ManifestUpdateImagePlanItem, len(plan))
	for _, item := range plan {
		serviceName := strings.TrimSpace(item.ServiceName)
		if serviceName == "" {
			return nil, fmt.Errorf("%w: reviewed image plan has empty service name", ErrManifestUpdateConflict)
		}
		if _, exists := out[serviceName]; exists {
			return nil, fmt.Errorf("%w: reviewed image plan has duplicate service %s", ErrManifestUpdateConflict, serviceName)
		}
		out[serviceName] = item
	}
	return out, nil
}

func manifestUpdateImagePlanSummary(plan []ManifestUpdateImagePlanItem) []string {
	if len(plan) == 0 {
		return nil
	}
	items := make([]string, 0, len(plan))
	for _, item := range plan {
		display := "service " + item.ServiceName
		if manifestUpdateImagePlanEntryKind(item) == manifestUpdateImageEntryRuntimeAnchor {
			display = "Piccolo runtime support"
		}
		action := "will stage"
		if manifestUpdateImagePlanAction(item) == manifestUpdateImageActionRefresh {
			action = "will refresh"
		}
		reason := strings.TrimSpace(item.Reason)
		if reason != "" {
			reason = "; " + reason
		}
		items = append(items, fmt.Sprintf("%s %s image %s at digest %s; expected rootfs %s%s", display, action, item.ImageRef, manifestUpdateImagePlanCanonicalDigest(item), item.RootfsVolumeID, reason))
	}
	slices.Sort(items)
	return items
}

func cloneManifestUpdateImagePlan(in []ManifestUpdateImagePlanItem) []ManifestUpdateImagePlanItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]ManifestUpdateImagePlanItem, len(in))
	copy(out, in)
	return out
}

func (m *AppManager) manifestUpdateRuntimeEndpoints(instanceID string, candidateDef, removeDef *api.AppDefinition, listenerPlan *services.PreparedReconcile) ([]services.ServiceEndpoint, error) {
	if listenerPlan != nil {
		return listenerPlan.Endpoints(), nil
	}
	if removeDef == nil || !reflect.DeepEqual(removeDef.Listeners, candidateDef.Listeners) {
		return nil, fmt.Errorf("listener changes require prepared listener plan")
	}
	endpoints, _ := m.serviceManager.GetByApp(instanceID)
	if len(endpoints) == 0 && !allowMissingListenerEndpointsForTest() {
		return nil, fmt.Errorf("existing listener endpoints unavailable")
	}
	return endpoints, nil
}

func (m *AppManager) recreateContainersInPlaceWithPreparedListeners(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance, listenerPlan *services.PreparedReconcile) error {
	return m.recreateContainersInPlaceWithPreparedListenersAndHook(ctx, instanceID, candidateDef, removeDef, appInst, listenerPlan, nil)
}

func (m *AppManager) recreateContainersInPlaceWithPreparedListenersAndHook(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance, listenerPlan *services.PreparedReconcile, beforeInstall func() error) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}
	mode := piccoloModeFromExtensions(candidateDef.Extensions)
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return fmt.Errorf("podman runtime: %w", err)
	}
	if err := m.removeContainersForMultiApp(ctx, appInst, removeDef, runtime); err != nil {
		return fmt.Errorf("remove previous containers: %w", err)
	}

	endpoints, err := m.manifestUpdateRuntimeEndpoints(instanceID, candidateDef, removeDef, listenerPlan)
	if err != nil {
		return err
	}
	if beforeInstall != nil {
		if err := beforeInstall(); err != nil {
			return err
		}
	}

	var prebuiltRootfs map[string]*rootfsMountInfo
	if rootfs := m.currentRootfsManager(); rootfs != nil {
		prebuiltRootfs, err = m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, candidateDef, appInst)
		if err != nil {
			return fmt.Errorf("attach existing rootfs: %w", err)
		}
	}
	result, err := m.installContainerGroup(ctx, candidateDef, instanceID, layout, runtime, endpoints, prebuiltRootfs)
	if err != nil {
		return fmt.Errorf("install container group: %w", err)
	}
	appInst.NetworkAnchorID = result.NetworkAnchorID
	appInst.Containers = result.Containers
	appInst.PrimaryService = result.PrimaryService
	appInst.ActiveRootfs = activeRootfsForDefinition(appInst.ActiveRootfs, candidateDef)
	if err := state.StoreApp(appInst); err != nil {
		if rmErr := m.removeContainersForMultiApp(ctx, result, candidateDef, runtime); rmErr != nil {
			log.Printf("WARN: manifest update %s: cleanup after persist failure: %v", instanceID, rmErr)
		}
		return fmt.Errorf("persist container ids: %w", err)
	}
	if appInst.Enabled {
		m.setObservedStatus(instanceID, StatusRunning)
	}
	return nil
}

func (m *AppManager) recreateContainersFromStagedRootfs(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance, stage *manifestUpdateRuntimeStage, listenerPlan *services.PreparedReconcile) error {
	return m.recreateContainersFromStagedRootfsWithHook(ctx, instanceID, candidateDef, removeDef, appInst, stage, listenerPlan, nil)
}

func (m *AppManager) recreateContainersFromStagedRootfsWithHook(ctx context.Context, instanceID string, candidateDef, removeDef *api.AppDefinition, appInst *AppInstance, stage *manifestUpdateRuntimeStage, listenerPlan *services.PreparedReconcile, beforeInstall func() error) error {
	if stage == nil || len(stage.prebuiltRootfs) == 0 {
		return m.recreateContainersInPlaceWithPreparedListenersAndHook(ctx, instanceID, candidateDef, removeDef, appInst, listenerPlan, beforeInstall)
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}
	mode := piccoloModeFromExtensions(candidateDef.Extensions)
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return fmt.Errorf("podman runtime: %w", err)
	}
	if err := m.removeContainersForMultiApp(ctx, appInst, removeDef, runtime); err != nil {
		return fmt.Errorf("remove previous containers: %w", err)
	}

	endpoints, err := m.manifestUpdateRuntimeEndpoints(instanceID, candidateDef, removeDef, listenerPlan)
	if err != nil {
		return err
	}
	if beforeInstall != nil {
		if err := beforeInstall(); err != nil {
			return err
		}
	}

	result, err := m.installContainerGroup(ctx, candidateDef, instanceID, layout, runtime, endpoints, stage.prebuiltRootfs)
	if err != nil {
		return fmt.Errorf("install container group: %w", err)
	}
	appInst.NetworkAnchorID = result.NetworkAnchorID
	appInst.Containers = result.Containers
	appInst.PrimaryService = result.PrimaryService
	appInst.ActiveRootfs = cloneStringMap(stage.candidateActiveRootfs)
	if err := state.StoreApp(appInst); err != nil {
		if rmErr := m.removeContainersForMultiApp(ctx, result, candidateDef, runtime); rmErr != nil {
			log.Printf("WARN: manifest update %s: cleanup after persist failure: %v", instanceID, rmErr)
		}
		return fmt.Errorf("persist container ids: %w", err)
	}
	if appInst.Enabled {
		m.setObservedStatus(instanceID, StatusRunning)
	}
	return nil
}

func imageConfigDigest(cfg *container.ImageConfig) string {
	if cfg == nil {
		return ""
	}
	if len(cfg.RepoDigests) > 0 {
		return cfg.RepoDigests[0]
	}
	return cfg.Digest
}

func allowMissingListenerEndpointsForTest() bool {
	return os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") == "1"
}

func normalizeManifestUpdateInputs(declared map[string]api.AppInput, st *InstallState, provided map[string]interface{}, regenerate []string, clear []string, instanceID string) (map[string]interface{}, map[string]bool, error) {
	out := make(map[string]interface{}, len(provided)+len(declared)+1)
	inputSensitive := map[string]bool{}
	regen := map[string]bool{}
	for _, name := range regenerate {
		regen[name] = true
	}
	clearInputs := map[string]bool{}
	for _, name := range clear {
		clearInputs[name] = true
	}
	out["__app_address__"] = instanceID
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		spec := declared[name]
		if name == "__app_address__" {
			out[name] = instanceID
			continue
		}
		oldValue, oldPresent := any(nil), false
		if st != nil && st.InstallInputs != nil {
			oldValue, oldPresent = st.InstallInputs[name]
		}
		oldUsable := oldPresent && installInputValueUsableForSchema(oldValue, spec)
		currentSensitive := oldPresent && manifestUpdateStoredInputSensitive(name, spec, st)
		value, providedValue := provided[name]
		providedNonEmpty := providedValue && strings.TrimSpace(fmt.Sprint(value)) != ""
		if providedValue && !providedNonEmpty && currentSensitive && !inputIsSensitive(name, spec) && !spec.Generate {
			return nil, nil, fmt.Errorf("input %q replacement value cannot be empty; clear it explicitly", name)
		}
		if clearInputs[name] {
			if spec.Required {
				return nil, nil, fmt.Errorf("input %q is required and cannot be cleared", name)
			}
			out[name] = manifestUpdateClearedInputValue(spec)
			continue
		}
		switch {
		case spec.Generate:
			switch {
			case regen[name]:
				value, err := GenerateSecurePassword()
				if err != nil {
					return nil, nil, fmt.Errorf("generate input %q: %w", name, err)
				}
				out[name] = value
				inputSensitive[name] = true
			case providedNonEmpty:
				out[name] = value
				inputSensitive[name] = true
			case providedValue:
				return nil, nil, fmt.Errorf("input %q must be provided or explicitly regenerated", name)
			case oldUsable:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, err
				}
				out[name] = oldValue
				inputSensitive[name] = true
			case oldPresent:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, err
				}
				return nil, nil, fmt.Errorf("input %q must be provided or explicitly regenerated", name)
			default:
				return nil, nil, fmt.Errorf("input %q must be provided or explicitly regenerated", name)
			}
		case inputIsSensitive(name, spec):
			switch {
			case providedNonEmpty:
				out[name] = value
				inputSensitive[name] = true
			case providedValue && spec.Required:
				return nil, nil, fmt.Errorf("input %q replacement value cannot be empty", name)
			case providedValue:
				return nil, nil, fmt.Errorf("input %q replacement value cannot be empty; clear it explicitly", name)
			case oldUsable:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, err
				}
				out[name] = oldValue
				inputSensitive[name] = true
			case oldPresent:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, err
				}
				if spec.Required {
					return nil, nil, fmt.Errorf("input %q is required", name)
				}
			case spec.Required:
				return nil, nil, fmt.Errorf("input %q is required", name)
			case spec.Default != nil:
				out[name] = normalizeInputValueForValidation(spec.Type, spec.Default)
			default:
				out[name] = zeroInputValue(spec.Type)
			}
		default:
			switch {
			case providedValue:
				out[name] = value
			case oldUsable:
				if err := validateKeptInstallInputValue(name, spec, oldValue, currentSensitive); err != nil {
					return nil, nil, err
				}
				out[name] = oldValue
				if currentSensitive {
					inputSensitive[name] = true
				}
			case oldPresent && currentSensitive:
				if err := validateKeptInstallInputValue(name, spec, oldValue, currentSensitive); err != nil {
					return nil, nil, err
				}
			case spec.Default != nil:
				out[name] = normalizeInputValueForValidation(spec.Type, spec.Default)
			case spec.Required:
				return nil, nil, fmt.Errorf("input %q is required", name)
			default:
				out[name] = zeroInputValue(spec.Type)
			}
		}
	}
	if err := ValidateInputs(declared, out); err != nil {
		return nil, nil, err
	}
	return out, persistedInstallInputSensitive(out, inputSensitive), nil
}

func manifestUpdateClearedInputValue(spec api.AppInput) interface{} {
	return zeroInputValue(spec.Type)
}

func normalizePendingCatalogManifestReviewInputs(declared map[string]api.AppInput, st *InstallState, req ManifestUpdateRequest, instanceID string) (map[string]interface{}, map[string]string, map[string]bool, error) {
	if st == nil {
		return nil, nil, nil, fmt.Errorf("%w: catalog install state is missing", ErrInstalledConfigUnavailable)
	}
	out := make(map[string]interface{}, len(declared)+1)
	provenance := make(map[string]string, len(declared)+1)
	inputSensitive := map[string]bool{}
	regen := map[string]bool{}
	for _, name := range req.RegenerateInputs {
		regen[name] = true
	}
	clearInputs := map[string]bool{}
	for _, name := range req.ClearInputs {
		clearInputs[name] = true
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		spec := declared[name]
		if value, exists := st.InstallInputs[name]; exists {
			out[name] = value
		}
		if value := st.InputProvenance[name]; value != "" {
			provenance[name] = value
		}
		oldValue, oldPresent := out[name]
		oldUsable := oldPresent && installInputValueUsableForSchema(oldValue, spec)
		if name == "__app_address__" {
			out[name] = instanceID
			provenance[name] = InputProvenanceSystem
			continue
		}
		value, provided := req.Inputs[name]
		providedNonEmpty := provided && strings.TrimSpace(fmt.Sprint(value)) != ""
		currentSensitive := oldPresent && manifestUpdateStoredInputSensitive(name, spec, st)
		if provided && !providedNonEmpty && currentSensitive && !inputIsSensitive(name, spec) && !spec.Generate {
			return nil, nil, nil, fmt.Errorf("input %q replacement value cannot be empty; clear it explicitly", name)
		}
		if clearInputs[name] {
			if spec.Required {
				return nil, nil, nil, fmt.Errorf("input %q is required and cannot be cleared", name)
			}
			out[name] = manifestUpdateClearedInputValue(spec)
			provenance[name] = InputProvenanceOperator
			continue
		}
		switch {
		case spec.Generate:
			switch {
			case regen[name]:
				generated, err := GenerateSecurePassword()
				if err != nil {
					return nil, nil, nil, fmt.Errorf("generate input %q: %w", name, err)
				}
				out[name] = generated
				provenance[name] = InputProvenanceGenerated
				inputSensitive[name] = true
			case providedNonEmpty:
				out[name] = value
				provenance[name] = InputProvenanceOperator
				inputSensitive[name] = true
			case provided:
				return nil, nil, nil, fmt.Errorf("input %q replacement value cannot be empty; clear or regenerate it explicitly", name)
			case !oldUsable:
				if oldPresent {
					if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
						return nil, nil, nil, err
					}
				}
				return nil, nil, nil, fmt.Errorf("input %q is missing and must be provided or regenerated", name)
			default:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, nil, err
				}
				inputSensitive[name] = true
			}
		case inputIsSensitive(name, spec):
			switch {
			case providedNonEmpty:
				out[name] = value
				provenance[name] = InputProvenanceOperator
				inputSensitive[name] = true
			case provided && spec.Required:
				return nil, nil, nil, fmt.Errorf("input %q replacement value cannot be empty", name)
			case provided:
				return nil, nil, nil, fmt.Errorf("input %q replacement value cannot be empty; clear it explicitly", name)
			case oldUsable:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, nil, err
				}
				inputSensitive[name] = true
			case oldPresent:
				if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
					return nil, nil, nil, err
				}
				if spec.Required {
					return nil, nil, nil, fmt.Errorf("input %q is missing and must be provided", name)
				}
			case spec.Required:
				return nil, nil, nil, fmt.Errorf("input %q is missing and must be provided", name)
			case spec.Default != nil:
				out[name] = normalizeInputValueForValidation(spec.Type, spec.Default)
				provenance[name] = InputProvenanceCatalogDefault
			default:
				out[name] = zeroInputValue(spec.Type)
				provenance[name] = InputProvenanceCatalogDefault
			}
		default:
			if provided {
				out[name] = value
				provenance[name] = InputProvenanceOperator
				continue
			}
			if oldUsable {
				if err := validateKeptInstallInputValue(name, spec, oldValue, currentSensitive); err != nil {
					return nil, nil, nil, err
				}
				if currentSensitive {
					inputSensitive[name] = true
				}
				continue
			}
			if oldPresent && currentSensitive {
				if err := validateKeptInstallInputValue(name, spec, oldValue, currentSensitive); err != nil {
					return nil, nil, nil, err
				}
			}
			if spec.Default != nil {
				out[name] = normalizeInputValueForValidation(spec.Type, spec.Default)
				provenance[name] = InputProvenanceCatalogDefault
			} else if spec.Required {
				return nil, nil, nil, fmt.Errorf("input %q is required", name)
			} else {
				out[name] = zeroInputValue(spec.Type)
				provenance[name] = InputProvenanceCatalogDefault
			}
		}
	}
	if err := ValidateInputs(declared, out); err != nil {
		return nil, nil, nil, err
	}
	return out, provenance, persistedInstallInputSensitive(out, inputSensitive), nil
}

func zeroInputValue(inputType string) interface{} {
	switch inputType {
	case "boolean":
		return false
	case "int", "number":
		return float64(0)
	case "array":
		return []interface{}{}
	default:
		return ""
	}
}

func customManifestBasicEligibility(appInst *AppInstance, allowCatalog bool) error {
	if appInst == nil {
		return ErrAppNotFound
	}
	if appInst.CatalogSource != "" && !allowCatalog {
		return fmt.Errorf("%w: catalog-backed apps use catalog sync", ErrManifestUpdateRejected)
	}
	if !appInst.Enabled {
		return fmt.Errorf("%w: disabled apps must be started before manifest update", ErrManifestUpdateRejected)
	}
	if appInst.Mode() != ModeService {
		return fmt.Errorf("%w: only service-mode custom apps are supported", ErrManifestUpdateRejected)
	}
	return nil
}

func manifestUpdateExistingOIDC(state *FilesystemStateManager, instanceID string, curDef *api.AppDefinition) (*OIDCCredentials, error) {
	if curDef == nil || !hasOIDCClient(curDef.Services) {
		return nil, nil
	}
	st, err := state.LoadInstallState(instanceID)
	if err != nil {
		return nil, fmt.Errorf("%w: current app uses service-level oidc_client but stored OIDC credentials are unavailable", ErrManifestUpdateRejected)
	}
	if st.OIDCCredentials == nil || strings.TrimSpace(st.OIDCCredentials.ClientID) == "" || strings.TrimSpace(st.OIDCCredentials.ClientSecret) == "" {
		return nil, fmt.Errorf("%w: current app uses service-level oidc_client but stored OIDC credentials are incomplete", ErrManifestUpdateRejected)
	}
	creds := *st.OIDCCredentials
	return &creds, nil
}

func loadInstallLedgerFingerprint(state *FilesystemStateManager, instanceID string) (bool, int64, string, error) {
	st, err := state.LoadInstallState(instanceID)
	if errors.Is(err, ErrInstallStateNotFound) {
		return false, 0, "", nil
	}
	if err != nil {
		return false, 0, "", fmt.Errorf("load config ledger: %w", err)
	}
	return true, st.Revision, st.RawTemplateHash, nil
}

func manifestUpdateRejectedResult(instanceID string, curDef *api.AppDefinition, appInst *AppInstance, reason string) (*manifestUpdateCandidate, *ManifestUpdateResult, error) {
	baseHash, _ := canonicalManifestHash(curDef)
	fp, _ := manifestRuntimeFingerprint(appInst)
	summary := ManifestUpdateSummary{Rejected: []string{reason}}
	if reason == sensitiveStructuralRenderRejectedReason {
		summary.WillChange = []string{sensitiveStructuralRenderRejectedSummary}
	}
	decision := ManifestUpdateDecision{
		Flag:    "manifest_update_rejected",
		Outcome: "rejected",
		Summary: reason,
		Reason:  reason,
	}
	updateClass := "manifest_update_v1"
	if strings.Contains(reason, "oidc_client") {
		updateClass = "service_app_update_v2"
		decision.Flag = "oidc_client_changed"
		decision.Path = "services.*.oidc_client"
	}
	return nil, &ManifestUpdateResult{
		InstanceID:         instanceID,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: fp,
		DiffKind:           DiffKindStructuralNoImage.String(),
		UpdateClass:        updateClass,
		Applicable:         false,
		BlockingReason:     reason,
		Summary:            summary,
		Decisions:          []ManifestUpdateDecision{decision},
		DataSafety: &ManifestUpdateDataSafetySummary{
			SnapshotRequired: false,
			Reason:           "candidate rejected before v2 runtime preparation",
			RollbackLimit:    "no user-initiated data rollback is created by manifest update",
		},
	}, nil
}

func storageMaps(st *api.AppStorage) (map[string]api.AppVolume, map[string]api.AppVolume) {
	if st == nil {
		return nil, nil
	}
	return st.Persistent, st.Temporary
}

func changedStringMapKeys(oldMap, newMap map[string]string) []string {
	keys := map[string]bool{}
	for k, oldVal := range oldMap {
		if newVal, exists := newMap[k]; !exists || newVal != oldVal {
			keys[k] = true
		}
	}
	for k := range newMap {
		if _, exists := oldMap[k]; !exists {
			keys[k] = true
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func manifestUpdatePrecommitSnapshotLVName(instanceID, operationID string) string {
	return fmt.Sprintf("snap-app-%s--manifest-%s", instanceID, manifestUpdateShortOperationID(operationID))
}

func manifestUpdateFailedDataLVName(instanceID, operationID string) string {
	return fmt.Sprintf("vol-app-%s--failed-manifest-%s", instanceID, manifestUpdateShortOperationID(operationID))
}

func manifestUpdateShortOperationID(operationID string) string {
	operationID = strings.TrimSpace(operationID)
	if len(operationID) > 12 {
		return operationID[:12]
	}
	if operationID == "" {
		return "unknown"
	}
	return operationID
}

func canonicalManifestHash(def *api.AppDefinition) (string, error) {
	c := cloneDefinitionForCompare(def)
	data, err := SerializeAppDefinition(c)
	if err != nil {
		return "", err
	}
	return Sha256Hex(data), nil
}

func cloneDefinitionForCompare(def *api.AppDefinition) *api.AppDefinition {
	if def == nil {
		return nil
	}
	data, err := SerializeAppDefinition(def)
	if err != nil {
		return def
	}
	clone, err := ParseAppDefinition(data)
	if err != nil {
		return def
	}
	return clone
}

func manifestRuntimeFingerprint(appInst *AppInstance) (string, error) {
	if appInst == nil {
		return "", fmt.Errorf("app instance required")
	}
	payload := struct {
		InstanceID      string            `json:"instance_id"`
		PrimaryService  string            `json:"primary_service"`
		NetworkAnchorID string            `json:"network_anchor_id"`
		Containers      map[string]string `json:"containers"`
		ActiveRootfs    map[string]string `json:"active_rootfs"`
		Enabled         bool              `json:"enabled"`
	}{
		InstanceID:      appInst.InstanceID,
		PrimaryService:  appInst.PrimaryService,
		NetworkAnchorID: appInst.NetworkAnchorID,
		Containers:      appInst.Containers,
		ActiveRootfs:    appInst.ActiveRootfs,
		Enabled:         appInst.Enabled,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return Sha256Hex(data), nil
}

func randomManifestUpdateToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (m *AppManager) storeManifestUpdateCandidate(c *manifestUpdateCandidate) {
	m.manifestUpdateMu.Lock()
	defer m.manifestUpdateMu.Unlock()
	if m.manifestUpdateCandidates == nil {
		m.manifestUpdateCandidates = make(map[string]*manifestUpdateCandidate)
	}
	now := time.Now().UTC()
	for token, existing := range m.manifestUpdateCandidates {
		if existing == nil || existing.ExpiresAt.Before(now) || existing.InstanceID == c.InstanceID {
			delete(m.manifestUpdateCandidates, token)
		}
	}
	m.manifestUpdateCandidates[c.Token] = c
}

func (m *AppManager) takeManifestUpdateCandidate(token string) (*manifestUpdateCandidate, bool) {
	if strings.TrimSpace(token) == "" {
		return nil, false
	}
	m.manifestUpdateMu.Lock()
	defer m.manifestUpdateMu.Unlock()
	c, ok := m.manifestUpdateCandidates[token]
	if !ok || c == nil || c.ExpiresAt.Before(time.Now().UTC()) {
		delete(m.manifestUpdateCandidates, token)
		return nil, false
	}
	delete(m.manifestUpdateCandidates, token)
	return c, true
}

func (fsm *FilesystemStateManager) BackupCurrentAppDefinitionForManifestUpdate(instanceID string) (string, error) {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	appDir := filepath.Join(fsm.appsDir, instanceID)
	cur := filepath.Join(appDir, "app.yaml")
	backup := filepath.Join(appDir, manifestUpdateBackupFilename)
	data, err := os.ReadFile(cur)
	if err != nil {
		return "", fmt.Errorf("read current app.yaml: %w", err)
	}
	if err := fsutil.AtomicWriteFile(backup, data, 0644); err != nil {
		return "", fmt.Errorf("write manifest update backup: %w", err)
	}
	return backup, nil
}

func (fsm *FilesystemStateManager) BackupInstallStateForManifestUpdate(instanceID string) (string, error) {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	appDir := filepath.Join(fsm.appsDir, instanceID)
	cur := filepath.Join(appDir, installStateFilename)
	data, err := os.ReadFile(cur)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read install state: %w", err)
	}
	backup := filepath.Join(appDir, installStateBackupFilename)
	if err := fsutil.AtomicWriteFile(backup, data, 0600); err != nil {
		return "", fmt.Errorf("write install state backup: %w", err)
	}
	return backup, nil
}

func (fsm *FilesystemStateManager) RestoreInstallStateBackup(instanceID, backupPath string) error {
	if strings.TrimSpace(backupPath) == "" {
		return nil
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read install state backup: %w", err)
	}
	if err := fsutil.AtomicWriteFile(fsm.installStatePath(instanceID), data, 0600); err != nil {
		return fmt.Errorf("restore install state backup: %w", err)
	}
	return nil
}

func (fsm *FilesystemStateManager) RestoreInstallStateForTransaction(instanceID string, txn *ManifestUpdateTransaction) error {
	if txn == nil {
		return nil
	}
	if strings.TrimSpace(txn.BackupInstallStatePath) != "" {
		return fsm.RestoreInstallStateBackup(instanceID, txn.BackupInstallStatePath)
	}
	if txn.CreatedInstallState {
		return fsm.DeleteInstallState(instanceID)
	}
	return nil
}

func (fsm *FilesystemStateManager) ClearInstallStateBackup(backupPath string) error {
	if strings.TrimSpace(backupPath) == "" {
		return nil
	}
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (fsm *FilesystemStateManager) StoreManifestUpdateTransaction(instanceID string, txn *ManifestUpdateTransaction) error {
	if txn == nil {
		return fmt.Errorf("manifest update transaction required")
	}
	if fsm.storeManifestUpdateTransactionHook != nil {
		if err := fsm.storeManifestUpdateTransactionHook(instanceID, txn); err != nil {
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
	return fsutil.AtomicWriteFile(filepath.Join(appDir, manifestUpdateTxnFilename), data, 0600)
}

func (fsm *FilesystemStateManager) LoadManifestUpdateTransaction(instanceID string) (*ManifestUpdateTransaction, error) {
	path := filepath.Join(fsm.appsDir, instanceID, manifestUpdateTxnFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var txn ManifestUpdateTransaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return nil, err
	}
	return &txn, nil
}

func (fsm *FilesystemStateManager) ClearManifestUpdateTransaction(instanceID, backupPath string) error {
	fsm.fsMu.Lock()
	defer fsm.fsMu.Unlock()
	appDir := filepath.Join(fsm.appsDir, instanceID)
	var firstErr error
	txnPath := filepath.Join(appDir, manifestUpdateTxnFilename)
	installStateBackupPath := ""
	if data, err := os.ReadFile(txnPath); err == nil {
		var txn ManifestUpdateTransaction
		if json.Unmarshal(data, &txn) == nil {
			installStateBackupPath = txn.BackupInstallStatePath
		}
	}
	if err := os.Remove(txnPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		firstErr = err
	}
	if strings.TrimSpace(backupPath) == "" {
		backupPath = filepath.Join(appDir, manifestUpdateBackupFilename)
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	if strings.TrimSpace(installStateBackupPath) != "" {
		if err := os.Remove(installStateBackupPath); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *AppManager) recoverPendingManifestUpdates(ctx context.Context, state *FilesystemStateManager, skipMaps ...map[string]bool) map[string]bool {
	blocked := map[string]bool{}
	for _, appInst := range state.ListApps() {
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
			continue
		}
		if manifestRecoverySkipped(appInst.InstanceID, skipMaps...) {
			continue
		}
		txn, err := state.LoadManifestUpdateTransaction(appInst.InstanceID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Printf("WARN: manifest update recovery %s: load transaction: %v", appInst.InstanceID, err)
			m.setObservedStatus(appInst.InstanceID, StatusError)
			blocked[appInst.InstanceID] = true
			continue
		}
		recoveryCtx, admitted := admitPendingTransitionRecovery(ctx)
		if !admitted {
			return blocked
		}
		if txn.Phase == "committed" || txn.Phase == "committed_cleanup_pending" {
			if err := m.cleanupCommittedManifestUpdateTransaction(recoveryCtx, state, appInst.InstanceID, txn); err != nil {
				log.Printf("WARN: manifest update recovery %s: cleanup committed transaction: %v", appInst.InstanceID, err)
			}
		} else if err := m.recoverOneManifestUpdate(recoveryCtx, state, appInst, txn); err != nil {
			log.Printf("ERROR: manifest update recovery %s: %v", appInst.InstanceID, err)
			blocked[appInst.InstanceID] = true
		}
		if transitionRecoveryMustYield(recoveryCtx) {
			return blocked
		}
	}
	return blocked
}

func manifestRecoverySkipped(instanceID string, skipMaps ...map[string]bool) bool {
	for _, skip := range skipMaps {
		if skip != nil && skip[instanceID] {
			return true
		}
	}
	return false
}

func (m *AppManager) recoverOneManifestUpdate(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, txn *ManifestUpdateTransaction) error {
	instanceID := appInst.InstanceID
	if txn.Phase == "committed" || txn.Phase == "committed_cleanup_pending" {
		if err := m.cleanupCommittedManifestUpdateTransaction(ctx, state, instanceID, txn); err != nil {
			log.Printf("WARN: manifest update recovery %s: cleanup committed transaction: %v", instanceID, err)
		}
		return nil
	}
	if recoverLedgerCommitReached(state, appInst, txn) {
		if !txn.AccessPublished {
			backupPath := txn.BackupPath
			if strings.TrimSpace(backupPath) == "" {
				backupPath = filepath.Join(state.appsDir, instanceID, manifestUpdateBackupFilename)
			}
			prevDef, err := loadManifestUpdateBackupDefinition(backupPath)
			if err != nil {
				txn.Phase = "publishing_access"
				txn.LastError = fmt.Sprintf("load backup for access repair: %v", err)
				txn.UpdatedAt = time.Now().UTC()
				_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
				m.setObservedStatus(instanceID, StatusError)
				return fmt.Errorf("manifest update recovery %s: load backup for access repair: %w", instanceID, err)
			}
			if err := m.repairCommittedManifestUpdateAccess(ctx, state, appInst, prevDef, appInst.Definition, txn); err != nil {
				m.setObservedStatus(instanceID, StatusError)
				return err
			}
		}
		if catalogHash, ok := recoveredLedgerCommitCatalogHash(state, appInst, txn); ok {
			if err := storeCommittedCatalogMetadata(state, appInst, catalogHash); err != nil {
				txn.Phase = "committed_metadata_pending"
				txn.LastError = err.Error()
				txn.AccessPublished = true
				txn.UpdatedAt = time.Now().UTC()
				if storeErr := storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst); storeErr != nil {
					return fmt.Errorf("manifest update recovery %s: persist metadata retry marker: %w", instanceID, storeErr)
				}
				return nil
			}
		}
		txn.Phase = "committed"
		txn.LastError = ""
		txn.UpdatedAt = time.Now().UTC()
		if err := storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst); err != nil {
			log.Printf("WARN: manifest update recovery %s: mark committed after ledger recovery: %v", instanceID, err)
		}
		if err := m.cleanupCommittedManifestUpdateTransaction(ctx, state, instanceID, txn); err != nil {
			log.Printf("WARN: manifest update recovery %s: cleanup recovered ledger commit: %v", instanceID, err)
		}
		log.Printf("INFO: manifest update recovery %s: completed recovered ledger commit", instanceID)
		return nil
	}

	backupPath := txn.BackupPath
	if strings.TrimSpace(backupPath) == "" {
		backupPath = filepath.Join(state.appsDir, instanceID, manifestUpdateBackupFilename)
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("load backup: %v", err)
		_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("manifest update recovery %s: load backup: %w", instanceID, err)
	}
	prevDef, err := ParseAppDefinition(data)
	if err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("parse backup: %v", err)
		_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("manifest update recovery %s: parse backup: %w", instanceID, err)
	}
	failedDef := appInst.Definition
	txn.Phase = "restoring_previous"
	_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
	appInst.Definition = prevDef
	if txn.PreviousActiveRootfs != nil || txn.CandidateActiveRootfs != nil {
		appInst.ActiveRootfs = cloneStringMap(txn.PreviousActiveRootfs)
	}
	var rollbackErrs []error
	if err := state.StoreApp(appInst); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("store backup manifest: %w", err))
	}
	if err := state.RestoreInstallStateForTransaction(instanceID, txn); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("restore install state: %w", err))
	}
	if err := m.cleanupTransactionCreatedOIDCClient(ctx, txn); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if manifestTransactionRuntimeTouched(txn) {
		if err := m.restorePrecommitDataSnapshot(ctx, state, appInst, failedDef, txn); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	} else if err := m.cleanupPrecommitDataSnapshot(ctx, txn); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	var publicationResumeToken services.PublicationResumeToken
	if txn.AccessSuspended && m.serviceManager != nil {
		// The original token was process-local and cannot survive a crash.
		// Reassert suspension before rebuilding the previous runtime so only
		// this recovery attempt can publish it again.
		publicationResumeToken = m.serviceManager.SuspendAppPublication(instanceID)
	}
	runtimePublicationReady := !manifestTransactionRuntimeSwitchStarted(txn) || !appInst.Enabled
	if manifestTransactionRuntimeSwitchStarted(txn) && appInst.Enabled {
		if err := m.recreateContainersInPlaceWithHookAndPublicationResumeToken(ctx, instanceID, prevDef, failedDef, appInst, nil, publicationResumeToken); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("recreate previous containers: %w", err))
		} else {
			runtimePublicationReady = true
		}
	}
	if txn.AccessSuspended && m.serviceManager != nil && runtimePublicationReady {
		if err := m.serviceManager.ResumeAppPublicationWithResumeTokenContext(pressure.WithTransitionContinuation(ctx), publicationResumeToken, instanceID); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("resume app publication: %w", err))
		} else {
			txn.AccessSuspended = false
		}
	}
	m.configureOIDCAuthorizePaths(instanceID, prevDef)
	if err := m.rollbackTransactionProxyOIDCDelta(ctx, instanceID, txn, prevDef, failedDef); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if err := m.cleanupManifestUpdateStagedRootfs(ctx, txn); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	if restoreErr := errors.Join(rollbackErrs...); restoreErr != nil {
		txn.Phase = "restore_failed"
		txn.LastError = restoreErr.Error()
		_ = storeManifestUpdateTransactionAndTransition(state, instanceID, txn, appInst)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("manifest update recovery %s: %w", instanceID, restoreErr)
	}
	m.ReconcileAllSlicePolicies()
	if err := state.ClearTransitionRecord(instanceID); err != nil {
		log.Printf("WARN: manifest update recovery %s: cleanup v2 transition: %v", instanceID, err)
	} else if err := state.ClearManifestUpdateTransaction(instanceID, backupPath); err != nil {
		log.Printf("WARN: manifest update recovery %s: cleanup transaction: %v", instanceID, err)
	}
	log.Printf("INFO: manifest update recovery %s: restored previous manifest/runtime from transaction phase %s", instanceID, txn.Phase)
	return nil
}

func loadManifestUpdateBackupDefinition(path string) (*api.AppDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseAppDefinition(data)
}

func recoverLedgerCommitReached(state *FilesystemStateManager, appInst *AppInstance, txn *ManifestUpdateTransaction) bool {
	if state == nil || appInst == nil || txn == nil {
		return false
	}
	switch txn.Phase {
	case "ledger_committing", "ledger_committed", "publishing_access", "access_published", "committed_metadata_pending":
	default:
		return false
	}
	if txn.CandidateLedgerRevision <= 0 ||
		strings.TrimSpace(txn.CandidateLedgerSourceHash) == "" ||
		strings.TrimSpace(txn.CandidateManifestHash) == "" {
		return false
	}
	st, err := state.LoadInstallState(appInst.InstanceID)
	if err != nil {
		return false
	}
	if st.Revision != txn.CandidateLedgerRevision || st.RawTemplateHash != txn.CandidateLedgerSourceHash {
		return false
	}
	currentManifestHash, err := canonicalManifestHash(appInst.Definition)
	if err != nil {
		return false
	}
	return currentManifestHash == txn.CandidateManifestHash
}

func recoveredLedgerCommitCatalogHash(state *FilesystemStateManager, appInst *AppInstance, txn *ManifestUpdateTransaction) (string, bool) {
	if state == nil || appInst == nil || txn == nil || strings.TrimSpace(appInst.CatalogSource) == "" {
		return "", false
	}
	st, err := state.LoadInstallState(appInst.InstanceID)
	if err != nil {
		return "", false
	}
	if st.SourceKind != InstallSourceKindCatalog ||
		st.Revision != txn.CandidateLedgerRevision ||
		st.RawTemplateHash != txn.CandidateLedgerSourceHash {
		return "", false
	}
	hash := strings.TrimSpace(st.RawTemplateHash)
	return hash, hash != ""
}
