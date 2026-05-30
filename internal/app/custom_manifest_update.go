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
	"piccolod/internal/fsutil"
)

const (
	manifestUpdateTokenTTL       = 30 * time.Minute
	manifestUpdateTxnFilename    = "manifest_update_transaction.json"
	manifestUpdateBackupFilename = "app.manifest-update.prev.yaml"
)

var (
	ErrManifestUpdateRejected = errors.New("custom manifest update rejected")
	ErrManifestUpdateConflict = errors.New("custom manifest update conflict")
)

type ManifestUpdateInputField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Provenance string `json:"provenance"`
	Required   bool   `json:"required"`
	Generate   bool   `json:"generate"`
	Locked     bool   `json:"locked"`
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
	SystemContext      InstallSystemContext
	BaseManifestHash   string
	RuntimeFingerprint string
	DryRunToken        string
}

type ManifestUpdateSummary struct {
	WillChange           []string `json:"will_change"`
	WillRestart          []string `json:"will_restart"`
	WillPreserve         []string `json:"will_preserve"`
	ExpectedInterruption []string `json:"expected_interruption"`
	Rejected             []string `json:"rejected,omitempty"`
}

type ManifestUpdateResult struct {
	InstanceID         string                `json:"instance_id"`
	BaseManifestHash   string                `json:"base_manifest_hash"`
	RuntimeFingerprint string                `json:"runtime_fingerprint"`
	DryRunToken        string                `json:"dry_run_token,omitempty"`
	RenderedAppID      string                `json:"rendered_app_id"`
	DiffKind           string                `json:"diff_kind"`
	Applicable         bool                  `json:"applicable"`
	BlockingReason     string                `json:"blocking_reason,omitempty"`
	MetadataOnly       bool                  `json:"metadata_only"`
	Summary            ManifestUpdateSummary `json:"summary"`
}

type manifestUpdateCandidate struct {
	Token              string
	InstanceID         string
	BaseManifestHash   string
	RuntimeFingerprint string
	CandidateDigest    string
	DiffKind           DiffKind
	MetadataOnly       bool
	Definition         *api.AppDefinition
	Summary            ManifestUpdateSummary
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

type ManifestUpdateTransaction struct {
	OperationID           string    `json:"operation_id"`
	Phase                 string    `json:"phase"`
	PreviousManifestHash  string    `json:"previous_manifest_hash"`
	CandidateManifestHash string    `json:"candidate_manifest_hash"`
	DryRunToken           string    `json:"dry_run_token"`
	RuntimeFingerprint    string    `json:"runtime_fingerprint"`
	BackupPath            string    `json:"backup_path"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	LastError             string    `json:"last_error,omitempty"`
}

func (m *AppManager) ConfigureCustomManifestUpdate(ctx context.Context, instanceID string, raw []byte) (*ManifestUpdateConfigureResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
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
	if err := customManifestBasicEligibility(appInst); err != nil {
		return nil, err
	}

	def, err := ParseAppSchema(raw)
	if err != nil {
		return nil, fmt.Errorf("parse manifest schema: %w", err)
	}
	PrepareSmartDefaultsForUpdate(def, instanceID)
	fields, preflight := manifestUpdateInputFields(def.Inputs)
	result := &ManifestUpdateConfigureResult{
		Inputs:                   def.Inputs,
		Fields:                   fields,
		SecretGeneratedPreflight: preflight,
		Eligible:                 true,
	}
	if hasOIDCClient(def.Services) {
		result.Eligible = false
		result.BlockingReason = "service-level oidc_client is not supported by custom manifest update v1; reinstall is required"
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

func manifestUpdateInputFields(inputs map[string]api.AppInput) ([]ManifestUpdateInputField, []string) {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	fields := make([]ManifestUpdateInputField, 0, len(names))
	preflight := []string{}
	for _, name := range names {
		spec := inputs[name]
		provenance := "Entered now"
		locked := false
		if name == "__app_address__" {
			provenance = "Locked current value"
			locked = true
		} else if spec.Type == "password" || spec.Generate {
			provenance = "Re-enter required"
			preflight = append(preflight, name)
		} else if spec.Default != nil {
			provenance = "New manifest default"
		}
		fields = append(fields, ManifestUpdateInputField{
			Name:       name,
			Type:       spec.Type,
			Provenance: provenance,
			Required:   spec.Required,
			Generate:   spec.Generate,
			Locked:     locked,
		})
	}
	return fields, preflight
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
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	m.emitProgress(ctx, taskTypeUpdateManifest, req.InstanceID, taskPhaseValidating, 0, "Validating manifest update", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeUpdateManifest, req.InstanceID, taskPhaseComplete, 100, "Manifest update failed", true, err)
		} else {
			m.emitProgress(ctx, taskTypeUpdateManifest, req.InstanceID, taskPhaseComplete, 100, "Manifest update complete", true, nil)
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
	appInst, exists := state.GetApp(req.InstanceID)
	if !exists {
		return nil, errAppNotFound(req.InstanceID)
	}
	if err := customManifestBasicEligibility(appInst); err != nil {
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

	operationID, err := randomManifestUpdateToken()
	if err != nil {
		return nil, err
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate(req.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("backup current manifest: %w", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID:           operationID,
		Phase:                 "prepared",
		PreviousManifestHash:  cand.BaseManifestHash,
		CandidateManifestHash: cand.CandidateDigest,
		DryRunToken:           cand.Token,
		RuntimeFingerprint:    cand.RuntimeFingerprint,
		BackupPath:            backupPath,
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	}
	if err := state.StoreManifestUpdateTransaction(req.InstanceID, txn); err != nil {
		return nil, fmt.Errorf("store manifest update transaction: %w", err)
	}

	prevDef := curDef
	prevDefinition := *appInst.Definition
	appInst.Definition = cand.Definition
	appInst.UpdatedAt = time.Now()
	m.emitProgress(ctx, taskTypeUpdateManifest, req.InstanceID, taskPhaseApplyingManifest, 20, "Persisting manifest", false, nil)
	if err := state.StoreApp(appInst); err != nil {
		appInst.Definition = &prevDefinition
		_ = state.ClearManifestUpdateTransaction(req.InstanceID, backupPath)
		return nil, fmt.Errorf("persist candidate manifest: %w", err)
	}
	txn.Phase = "candidate_persisted"
	txn.UpdatedAt = time.Now().UTC()
	_ = state.StoreManifestUpdateTransaction(req.InstanceID, txn)

	if !cand.MetadataOnly {
		txn.Phase = "recreating_runtime"
		txn.UpdatedAt = time.Now().UTC()
		_ = state.StoreManifestUpdateTransaction(req.InstanceID, txn)
		m.emitProgress(ctx, taskTypeUpdateManifest, req.InstanceID, taskPhaseRecreatingContainer, 50, "Recreating containers", false, nil)
		if applyErr := m.recreateContainersInPlace(ctx, req.InstanceID, cand.Definition, prevDef, appInst); applyErr != nil {
			restoreErr := m.restoreManifestUpdateFailure(ctx, state, appInst, prevDef, cand.Definition, txn, applyErr)
			if restoreErr != nil {
				return nil, restoreErr
			}
			return nil, fmt.Errorf("manifest update rolled back: %w", applyErr)
		}
	}

	txn.Phase = "committed"
	txn.UpdatedAt = time.Now().UTC()
	_ = state.StoreManifestUpdateTransaction(req.InstanceID, txn)
	if err := state.ClearManifestUpdateTransaction(req.InstanceID, backupPath); err != nil {
		log.Printf("WARN: manifest update %s: cleanup transaction: %v", req.InstanceID, err)
	}
	return &ManifestUpdateResult{
		InstanceID:         req.InstanceID,
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		RenderedAppID:      req.InstanceID,
		DiffKind:           cand.DiffKind.String(),
		Applicable:         true,
		MetadataOnly:       cand.MetadataOnly,
		Summary:            cand.Summary,
	}, nil
}

func (m *AppManager) restoreManifestUpdateFailure(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, prevDef, failedDef *api.AppDefinition, txn *ManifestUpdateTransaction, cause error) error {
	instanceID := appInst.InstanceID
	txn.Phase = "restoring_previous"
	txn.LastError = cause.Error()
	txn.UpdatedAt = time.Now().UTC()
	_ = state.StoreManifestUpdateTransaction(instanceID, txn)
	m.emitProgress(ctx, taskTypeUpdateManifest, instanceID, taskPhaseRestoringManifest, 75, "Restoring previous manifest", false, nil)

	appInst.Definition = prevDef
	if err := state.StoreApp(appInst); err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("apply failed: %v; restore manifest failed: %v", cause, err)
		txn.UpdatedAt = time.Now().UTC()
		_ = state.StoreManifestUpdateTransaction(instanceID, txn)
		m.setObservedStatus(instanceID, StatusError)
		m.emitProgress(ctx, taskTypeUpdateManifest, instanceID, taskPhaseRestoreFailed, 95, "Restore failed", false, err)
		return fmt.Errorf("manifest update failed: %w; restore manifest failed: %v", cause, err)
	}
	if err := m.recreateContainersInPlace(ctx, instanceID, prevDef, failedDef, appInst); err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("apply failed: %v; recreate previous containers failed: %v", cause, err)
		txn.UpdatedAt = time.Now().UTC()
		_ = state.StoreManifestUpdateTransaction(instanceID, txn)
		m.setObservedStatus(instanceID, StatusError)
		m.emitProgress(ctx, taskTypeUpdateManifest, instanceID, taskPhaseRestoreFailed, 95, "Restore failed", false, err)
		return fmt.Errorf("manifest update failed: %w; recreate previous containers failed: %v", cause, err)
	}
	if err := state.ClearManifestUpdateTransaction(instanceID, txn.BackupPath); err != nil {
		log.Printf("WARN: manifest update %s: cleanup after rollback: %v", instanceID, err)
	}
	return nil
}

func (m *AppManager) renderCustomManifestUpdateCandidate(ctx context.Context, req ManifestUpdateRequest) (*manifestUpdateCandidate, *ManifestUpdateResult, error) {
	if len(bytes.TrimSpace(req.RawTemplate)) == 0 {
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
	if err := customManifestBasicEligibility(appInst); err != nil {
		return nil, nil, err
	}
	curDef, err := state.GetAppDefinition(req.InstanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read current manifest: %w", err)
	}
	if hasOIDCClient(curDef.Services) {
		return manifestUpdateRejectedResult(req.InstanceID, curDef, appInst, "current app uses service-level oidc_client; custom manifest update v1 requires reinstall")
	}
	preSchema, err := ParseAppSchema(req.RawTemplate)
	if err != nil {
		return nil, nil, fmt.Errorf("parse manifest schema: %w", err)
	}
	if hasOIDCClient(preSchema.Services) {
		return manifestUpdateRejectedResult(req.InstanceID, curDef, appInst, "candidate uses service-level oidc_client; custom manifest update v1 requires reinstall")
	}
	PrepareSmartDefaultsForUpdate(preSchema, req.InstanceID)
	inputs, err := normalizeManifestUpdateInputs(preSchema.Inputs, req.Inputs, req.RegenerateInputs, req.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	res, err := RunInstallPipeline(ctx, InstallPipelineInput{
		RawTemplate:   req.RawTemplate,
		UserInputs:    inputs,
		SystemContext: req.SystemContext,
		InstanceID:    req.InstanceID,
		ExistingOIDC:  nil,
	}, nil, m.syncSelfSkippingLister(req.InstanceID))
	if err != nil {
		return nil, nil, err
	}
	if hasOIDCClient(res.Definition.Services) {
		return manifestUpdateRejectedResult(req.InstanceID, curDef, appInst, "rendered candidate uses service-level oidc_client; custom manifest update v1 requires reinstall")
	}

	baseHash, err := canonicalManifestHash(curDef)
	if err != nil {
		return nil, nil, err
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return nil, nil, err
	}
	policy, summary := evaluateCustomManifestUpdatePolicy(curDef, res.Definition)
	diffKind := classifyDiff(cloneDefinitionForCompare(curDef), cloneDefinitionForCompare(res.Definition))
	candidateDigest := Sha256Hex(res.CanonicalBytes)
	result := &ManifestUpdateResult{
		InstanceID:         req.InstanceID,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: runtimeFingerprint,
		RenderedAppID:      req.InstanceID,
		DiffKind:           diffKind.String(),
		Applicable:         policy.Allowed,
		BlockingReason:     policy.Reason,
		MetadataOnly:       policy.MetadataOnly,
		Summary:            summary,
	}
	if !policy.Allowed {
		return nil, result, nil
	}
	cand := &manifestUpdateCandidate{
		InstanceID:         req.InstanceID,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: runtimeFingerprint,
		CandidateDigest:    candidateDigest,
		DiffKind:           diffKind,
		MetadataOnly:       policy.MetadataOnly,
		Definition:         res.Definition,
		Summary:            summary,
	}
	return cand, result, nil
}

type customManifestPolicyResult struct {
	Allowed      bool
	Reason       string
	MetadataOnly bool
}

func evaluateCustomManifestUpdatePolicy(oldDef, newDef *api.AppDefinition) (customManifestPolicyResult, ManifestUpdateSummary) {
	oldCmp := cloneDefinitionForCompare(oldDef)
	newCmp := cloneDefinitionForCompare(newDef)
	summary := ManifestUpdateSummary{
		WillPreserve: []string{
			"app identity and primary listener",
			"existing image references and active rootfs volumes",
			"listener topology unchanged: same listener names, primary listener, ports/claims, public exposure, flow/protocol, auth/middleware, and routing",
			"persistent app data volumes preserved but not snapshotted",
			"no image pull",
		},
	}
	reject := func(reason string) (customManifestPolicyResult, ManifestUpdateSummary) {
		summary.Rejected = append(summary.Rejected, reason)
		return customManifestPolicyResult{Allowed: false, Reason: reason}, summary
	}
	if oldCmp == nil || newCmp == nil {
		return reject("current and candidate manifests are required")
	}
	if piccoloModeFromExtensions(oldCmp.Extensions) != ModeService || piccoloModeFromExtensions(newCmp.Extensions) != ModeService {
		return reject("custom manifest update v1 only supports service-mode apps")
	}
	if hasOIDCClient(oldCmp.Services) || hasOIDCClient(newCmp.Services) {
		return reject("service-level oidc_client is not supported by custom manifest update v1")
	}
	if oldCmp.Type != newCmp.Type {
		return reject("type changes are not supported by custom manifest update v1")
	}
	if oldCmp.WorkspaceName != newCmp.WorkspaceName {
		return reject("workspace_name changes are not supported by custom manifest update v1")
	}
	if oldCmp.PrimaryService != newCmp.PrimaryService {
		return reject("primary_service changes are not supported by custom manifest update v1")
	}
	if !reflect.DeepEqual(oldCmp.Listeners, newCmp.Listeners) {
		return reject("listener topology or auth changed; reinstall or a future v2 flow is required")
	}
	if !reflect.DeepEqual(oldCmp.Permissions, newCmp.Permissions) {
		return reject("permission changes are not supported by custom manifest update v1")
	}
	if !reflect.DeepEqual(oldCmp.Resources, newCmp.Resources) {
		return reject("resource policy changes are not supported by custom manifest update v1")
	}
	if !reflect.DeepEqual(oldCmp.HealthCheck, newCmp.HealthCheck) {
		return reject("healthcheck changes are not supported by custom manifest update v1")
	}
	if !reflect.DeepEqual(oldCmp.AppConfig, newCmp.AppConfig) {
		return reject("app_config changes are covered by the runtime app config track and are not supported here")
	}
	if !reflect.DeepEqual(oldCmp.Auth, newCmp.Auth) {
		return reject("auth changes are not supported by custom manifest update v1")
	}
	if !reflect.DeepEqual(oldCmp.Extensions, newCmp.Extensions) {
		return reject("x-piccolo changes are not supported by custom manifest update v1")
	}
	if !reflect.DeepEqual(oldCmp.Environment, newCmp.Environment) {
		return reject("top-level environment changes are not supported by custom manifest update v1; use service environment entries")
	}
	if !reflect.DeepEqual(oldCmp.Lifecycle, newCmp.Lifecycle) {
		return reject("lifecycle changes are not supported by custom manifest update v1")
	}
	addedTopStorage, ok, reason := storageAddOnly(oldCmp.Storage, newCmp.Storage, "top-level storage")
	if !ok {
		return reject(reason)
	}
	for _, item := range addedTopStorage {
		summary.WillChange = append(summary.WillChange, item)
	}

	if len(oldCmp.Services) != len(newCmp.Services) {
		return reject("service additions or removals require reinstall")
	}
	serviceNames := make([]string, 0, len(oldCmp.Services))
	for name := range oldCmp.Services {
		serviceNames = append(serviceNames, name)
	}
	slices.Sort(serviceNames)
	for _, name := range serviceNames {
		oldSvc := oldCmp.Services[name]
		newSvc, exists := newCmp.Services[name]
		if !exists {
			return reject(fmt.Sprintf("service %q was removed or renamed; reinstall is required", name))
		}
		if oldSvc.Image != newSvc.Image {
			return reject(fmt.Sprintf("service %q image reference changed; run image update or reinstall", name))
		}
		if !reflect.DeepEqual(oldSvc.BindPorts, newSvc.BindPorts) {
			return reject(fmt.Sprintf("service %q bind_ports changed; listener topology changes are not supported in v1", name))
		}
		if !reflect.DeepEqual(oldSvc.After, newSvc.After) {
			return reject(fmt.Sprintf("service %q startup order changed; unsupported in v1", name))
		}
		if oldSvc.Init != newSvc.Init {
			return reject(fmt.Sprintf("service %q init mode changed; unsupported in v1", name))
		}
		if !reflect.DeepEqual(oldSvc.InitScript, newSvc.InitScript) {
			return reject(fmt.Sprintf("service %q init_script changed; custom manifest update v1 does not replay init scripts", name))
		}
		addedStorage, ok, reason := storageAddOnly(oldSvc.Storage, newSvc.Storage, fmt.Sprintf("service %s storage", name))
		if !ok {
			return reject(reason)
		}
		for _, item := range addedStorage {
			summary.WillChange = append(summary.WillChange, item)
		}
		for _, key := range changedStringMapKeys(oldSvc.Environment, newSvc.Environment) {
			summary.WillChange = append(summary.WillChange, fmt.Sprintf("service %s environment key %s", name, key))
		}
	}
	for name := range newCmp.Services {
		if _, exists := oldCmp.Services[name]; !exists {
			return reject(fmt.Sprintf("service %q was added; reinstall is required", name))
		}
	}

	oldBytes, _ := SerializeAppDefinition(oldCmp)
	newBytes, _ := SerializeAppDefinition(newCmp)
	diffKind := classifyDiff(oldCmp, newCmp)
	if diffKind == DiffKindImageOnly || diffKind == DiffKindStructuralWithImage {
		return reject("image reference changes are not supported by custom manifest update v1")
	}
	metadataOnly := diffKind == DiffKindNone && !bytes.Equal(oldBytes, newBytes)
	if metadataOnly {
		summary.WillChange = append(summary.WillChange, "manifest metadata/input schema")
	} else if diffKind == DiffKindStructuralNoImage {
		summary.WillRestart = append(summary.WillRestart, "existing containers will be recreated using current images/rootfs")
		summary.ExpectedInterruption = append(summary.ExpectedInterruption, "services may disconnect while containers restart")
	}
	if len(summary.WillChange) == 0 && len(summary.WillRestart) == 0 {
		summary.WillChange = append(summary.WillChange, "no runtime changes")
	}
	return customManifestPolicyResult{Allowed: true, MetadataOnly: metadataOnly}, summary
}

func normalizeManifestUpdateInputs(declared map[string]api.AppInput, provided map[string]interface{}, regenerate []string, instanceID string) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(provided)+len(declared)+1)
	for k, v := range provided {
		out[k] = v
	}
	regen := map[string]bool{}
	for _, name := range regenerate {
		regen[name] = true
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
		if spec.Generate {
			if regen[name] {
				value, err := GenerateSecurePassword()
				if err != nil {
					return nil, fmt.Errorf("generate input %q: %w", name, err)
				}
				out[name] = value
				continue
			}
			if value, exists := out[name]; exists && value != nil && strings.TrimSpace(fmt.Sprint(value)) != "" {
				continue
			}
			return nil, fmt.Errorf("input %q must be provided or explicitly regenerated", name)
		}
		if _, exists := out[name]; exists {
			continue
		}
		if spec.Required {
			return nil, fmt.Errorf("input %q is required", name)
		}
		if spec.Default != nil {
			out[name] = spec.Default
			continue
		}
		out[name] = zeroInputValue(spec.Type)
	}
	if err := ValidateInputs(declared, out); err != nil {
		return nil, err
	}
	return out, nil
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

func customManifestBasicEligibility(appInst *AppInstance) error {
	if appInst == nil {
		return ErrAppNotFound
	}
	if appInst.CatalogSource != "" {
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

func manifestUpdateRejectedResult(instanceID string, curDef *api.AppDefinition, appInst *AppInstance, reason string) (*manifestUpdateCandidate, *ManifestUpdateResult, error) {
	baseHash, _ := canonicalManifestHash(curDef)
	fp, _ := manifestRuntimeFingerprint(appInst)
	summary := ManifestUpdateSummary{Rejected: []string{reason}}
	return nil, &ManifestUpdateResult{
		InstanceID:         instanceID,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: fp,
		DiffKind:           DiffKindStructuralNoImage.String(),
		Applicable:         false,
		BlockingReason:     reason,
		Summary:            summary,
	}, nil
}

func storageAddOnly(oldStorage, newStorage *api.AppStorage, label string) ([]string, bool, string) {
	added := []string{}
	oldPersistent, oldTemporary := storageMaps(oldStorage)
	newPersistent, newTemporary := storageMaps(newStorage)
	pAdded, ok, reason := volumeMapAddOnly(oldPersistent, newPersistent, label+".persistent")
	if !ok {
		return nil, false, reason
	}
	tAdded, ok, reason := volumeMapAddOnly(oldTemporary, newTemporary, label+".temporary")
	if !ok {
		return nil, false, reason
	}
	added = append(added, pAdded...)
	added = append(added, tAdded...)
	return added, true, ""
}

func storageMaps(st *api.AppStorage) (map[string]api.AppVolume, map[string]api.AppVolume) {
	if st == nil {
		return nil, nil
	}
	return st.Persistent, st.Temporary
}

func volumeMapAddOnly(oldMap, newMap map[string]api.AppVolume, label string) ([]string, bool, string) {
	added := []string{}
	for name, oldVol := range oldMap {
		newVol, exists := newMap[name]
		if !exists {
			return nil, false, fmt.Sprintf("%s volume %q was removed; unsupported in v1", label, name)
		}
		if !reflect.DeepEqual(oldVol, newVol) {
			return nil, false, fmt.Sprintf("%s volume %q changed; removals, renames, and mount-path changes are unsupported in v1", label, name)
		}
	}
	for name := range newMap {
		if _, exists := oldMap[name]; !exists {
			added = append(added, fmt.Sprintf("%s volume %s added", label, name))
		}
	}
	slices.Sort(added)
	return added, true, ""
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

func (fsm *FilesystemStateManager) StoreManifestUpdateTransaction(instanceID string, txn *ManifestUpdateTransaction) error {
	if txn == nil {
		return fmt.Errorf("manifest update transaction required")
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
	return fsutil.AtomicWriteFile(filepath.Join(appDir, manifestUpdateTxnFilename), data, 0644)
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
	if err := os.Remove(txnPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		firstErr = err
	}
	if strings.TrimSpace(backupPath) == "" {
		backupPath = filepath.Join(appDir, manifestUpdateBackupFilename)
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (m *AppManager) recoverPendingManifestUpdates(ctx context.Context, state *FilesystemStateManager) map[string]bool {
	blocked := map[string]bool{}
	for _, appInst := range state.ListApps() {
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
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
		if txn.Phase == "committed" {
			if err := state.ClearManifestUpdateTransaction(appInst.InstanceID, txn.BackupPath); err != nil {
				log.Printf("WARN: manifest update recovery %s: cleanup committed transaction: %v", appInst.InstanceID, err)
			}
			continue
		}
		if err := m.recoverOneManifestUpdate(ctx, state, appInst, txn); err != nil {
			log.Printf("ERROR: manifest update recovery %s: %v", appInst.InstanceID, err)
			blocked[appInst.InstanceID] = true
		}
	}
	return blocked
}

func (m *AppManager) recoverOneManifestUpdate(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, txn *ManifestUpdateTransaction) error {
	instanceID := appInst.InstanceID
	backupPath := txn.BackupPath
	if strings.TrimSpace(backupPath) == "" {
		backupPath = filepath.Join(state.appsDir, instanceID, manifestUpdateBackupFilename)
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("load backup: %v", err)
		_ = state.StoreManifestUpdateTransaction(instanceID, txn)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("manifest update recovery %s: load backup: %w", instanceID, err)
	}
	prevDef, err := ParseAppDefinition(data)
	if err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("parse backup: %v", err)
		_ = state.StoreManifestUpdateTransaction(instanceID, txn)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("manifest update recovery %s: parse backup: %w", instanceID, err)
	}
	failedDef := appInst.Definition
	txn.Phase = "restoring_previous"
	_ = state.StoreManifestUpdateTransaction(instanceID, txn)
	appInst.Definition = prevDef
	if err := state.StoreApp(appInst); err != nil {
		txn.Phase = "restore_failed"
		txn.LastError = fmt.Sprintf("store backup manifest: %v", err)
		_ = state.StoreManifestUpdateTransaction(instanceID, txn)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("manifest update recovery %s: store backup manifest: %w", instanceID, err)
	}
	if appInst.Enabled {
		if err := m.recreateContainersInPlace(ctx, instanceID, prevDef, failedDef, appInst); err != nil {
			txn.Phase = "restore_failed"
			txn.LastError = fmt.Sprintf("recreate previous containers: %v", err)
			_ = state.StoreManifestUpdateTransaction(instanceID, txn)
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("manifest update recovery %s: recreate previous containers: %w", instanceID, err)
		}
	}
	if err := state.ClearManifestUpdateTransaction(instanceID, backupPath); err != nil {
		log.Printf("WARN: manifest update recovery %s: cleanup transaction: %v", instanceID, err)
	}
	log.Printf("INFO: manifest update recovery %s: restored previous manifest/runtime from transaction phase %s", instanceID, txn.Phase)
	return nil
}
