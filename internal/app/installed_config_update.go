package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"time"

	"piccolod/internal/api"
)

const (
	installStateSchemaVersionConfig = 2
	configUpdateTokenTTL            = 30 * time.Minute

	InstallSourceKindCatalog = "catalog"
	InstallSourceKindCustom  = "custom"

	InputProvenanceOperator       = "operator"
	InputProvenanceCatalogDefault = "catalog_default"
	InputProvenanceGenerated      = "generated"
	InputProvenanceSystem         = "system"
	InputProvenanceLegacyUnknown  = "legacy_unknown"

	pendingCatalogReviewFlowConfig   = "config"
	pendingCatalogReviewFlowManifest = "manifest_review"
)

var (
	ErrInstalledConfigUnavailable = errors.New("installed config unavailable")
	ErrInstalledConfigRejected    = errors.New("installed config update rejected")
	ErrInstalledConfigConflict    = errors.New("installed config update conflict")
)

type InstalledConfigField struct {
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Label       string        `json:"label,omitempty"`
	Description string        `json:"description,omitempty"`
	Required    bool          `json:"required"`
	Generate    bool          `json:"generate"`
	Sensitive   bool          `json:"sensitive"`
	Present     bool          `json:"present"`
	Display     interface{}   `json:"display,omitempty"`
	Provenance  string        `json:"provenance"`
	Editable    bool          `json:"editable"`
	Actions     []string      `json:"actions,omitempty"`
	Schema      *api.AppInput `json:"schema,omitempty"`
}

type InstalledConfigReadResult struct {
	InstanceID      string                 `json:"instance_id"`
	SourceKind      string                 `json:"source_kind,omitempty"`
	SourceRef       string                 `json:"source_ref,omitempty"`
	LedgerHealth    string                 `json:"ledger_health"`
	LedgerRevision  int64                  `json:"ledger_revision,omitempty"`
	SourceHash      string                 `json:"source_hash,omitempty"`
	InputSchemaHash string                 `json:"input_schema_hash,omitempty"`
	Fields          []InstalledConfigField `json:"fields,omitempty"`
	Warnings        []string               `json:"warnings,omitempty"`
}

type InstalledConfigUpdateRequest struct {
	Inputs             map[string]interface{} `json:"inputs,omitempty"`
	SecretActions      map[string]string      `json:"secret_actions,omitempty"`
	RegenerateInputs   []string               `json:"regenerate_inputs,omitempty"`
	DryRunToken        string                 `json:"dry_run_token,omitempty"`
	CandidateDigest    string                 `json:"candidate_digest,omitempty"`
	LedgerRevision     int64                  `json:"ledger_revision,omitempty"`
	SourceHash         string                 `json:"source_hash,omitempty"`
	InputSchemaHash    string                 `json:"input_schema_hash,omitempty"`
	BaseManifestHash   string                 `json:"base_manifest_hash,omitempty"`
	RuntimeFingerprint string                 `json:"runtime_fingerprint,omitempty"`
}

type InstalledConfigActionSummary struct {
	Field       string      `json:"field"`
	Action      string      `json:"action"`
	Sensitive   bool        `json:"sensitive"`
	OldDisplay  interface{} `json:"old_display,omitempty"`
	NewDisplay  interface{} `json:"new_display,omitempty"`
	Consequence string      `json:"consequence,omitempty"`
}

type InstalledConfigUpdateResult struct {
	InstanceID          string                         `json:"instance_id"`
	LedgerRevision      int64                          `json:"ledger_revision"`
	SourceHash          string                         `json:"source_hash"`
	InputSchemaHash     string                         `json:"input_schema_hash"`
	BaseManifestHash    string                         `json:"base_manifest_hash"`
	RuntimeFingerprint  string                         `json:"runtime_fingerprint"`
	DryRunToken         string                         `json:"dry_run_token,omitempty"`
	CandidateDigest     string                         `json:"candidate_digest,omitempty"`
	DiffKind            string                         `json:"diff_kind"`
	Applicable          bool                           `json:"applicable"`
	BlockingReason      string                         `json:"blocking_reason,omitempty"`
	MetadataOnly        bool                           `json:"metadata_only"`
	AccessRepairPending bool                           `json:"access_repair_pending,omitempty"`
	AccessRepairMessage string                         `json:"access_repair_message,omitempty"`
	Actions             []InstalledConfigActionSummary `json:"actions,omitempty"`
	Summary             ManifestUpdateSummary          `json:"summary"`
}

type installedConfigCandidate struct {
	Token              string
	InstanceID         string
	LedgerRevision     int64
	SourceHash         string
	InputSchemaHash    string
	BaseManifestHash   string
	RuntimeFingerprint string
	CandidateDigest    string
	DiffKind           DiffKind
	MetadataOnly       bool
	RequiresSnapshot   bool
	Definition         *api.AppDefinition
	InstallState       *InstallState
	Actions            []InstalledConfigActionSummary
	Summary            ManifestUpdateSummary
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

type installedConfigPolicyResult struct {
	Allowed          bool
	Reason           string
	MetadataOnly     bool
	RequiresSnapshot bool
}

func NewV2InstallState(instanceID, sourceKind, sourceRef string, rawTemplate []byte, inputs map[string]interface{}, systemCtx InstallSystemContext, creds *OIDCCredentials, syncBlocked bool) *InstallState {
	copiedInputs, provenance := initialInstallConfigLedger(instanceID, rawTemplate, inputs)
	inputSensitive := initialInstallInputSensitive(instanceID, rawTemplate, copiedInputs, provenance)
	rawCopy := append([]byte(nil), rawTemplate...)
	return &InstallState{
		InstanceID:        instanceID,
		SchemaVersion:     installStateSchemaVersionConfig,
		Revision:          1,
		SourceKind:        sourceKind,
		SourceRef:         sourceRef,
		RawTemplate:       rawCopy,
		RawTemplateHash:   Sha256Hex(rawCopy),
		InputProvenance:   provenance,
		InputSensitive:    inputSensitive,
		InstallInputs:     copiedInputs,
		InstallSystemCtx:  &systemCtx,
		OIDCCredentials:   creds,
		SyncBlocked:       syncBlocked,
		SyncBlockedReason: installStateSyncBlockedReason(syncBlocked),
	}
}

func initialInstallConfigLedger(instanceID string, rawTemplate []byte, inputs map[string]interface{}) (map[string]any, map[string]string) {
	declared := map[string]api.AppInput{}
	if schema, err := ParseAppSchema(rawTemplate); err == nil && schema != nil {
		PrepareSmartDefaultsForUpdate(schema, instanceID)
		declared = schema.Inputs
	}
	copiedInputs := make(map[string]any, len(inputs))
	provenance := make(map[string]string, len(inputs))
	for name, value := range inputs {
		prov := InputProvenanceOperator
		if name == "__app_address__" {
			prov = InputProvenanceSystem
		}
		if spec, ok := declared[name]; ok {
			value = normalizeInputValueForValidation(spec.Type, value)
			if name == "__app_address__" {
				prov = InputProvenanceSystem
			} else if spec.Generate {
				prov = InputProvenanceGenerated
			} else if !spec.Required && spec.Default != nil && inputValueMatchesDefault(spec, value) {
				prov = InputProvenanceCatalogDefault
			}
		}
		copiedInputs[name] = value
		provenance[name] = prov
	}
	return persistedInstalledConfigLedger(copiedInputs, provenance)
}

func initialInstallInputSensitive(instanceID string, rawTemplate []byte, inputs map[string]any, provenance map[string]string) map[string]bool {
	declared := map[string]api.AppInput{}
	if schema, err := ParseAppSchema(rawTemplate); err == nil && schema != nil {
		PrepareSmartDefaultsForUpdate(schema, instanceID)
		declared = schema.Inputs
	}
	out := map[string]bool{}
	for name := range inputs {
		if name == "__app_address__" {
			continue
		}
		if provenance != nil && provenance[name] == InputProvenanceGenerated {
			out[name] = true
			continue
		}
		if inputNameLooksSensitive(name) {
			out[name] = true
			continue
		}
		if spec, ok := declared[name]; ok && (inputIsSensitive(name, spec) || spec.Generate) {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func inputValueMatchesDefault(spec api.AppInput, value interface{}) bool {
	value = normalizeInputValueForValidation(spec.Type, value)
	defaultValue := normalizeInputValueForValidation(spec.Type, spec.Default)
	if reflect.DeepEqual(value, defaultValue) {
		return true
	}
	if spec.Type != "array" {
		return false
	}
	valueStrings, ok := inputArrayStrings(value)
	if !ok {
		return false
	}
	defaultStrings, ok := inputArrayStrings(defaultValue)
	if !ok {
		return false
	}
	return slices.Equal(valueStrings, defaultStrings)
}

func inputArrayStrings(value interface{}) ([]string, bool) {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...), true
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

func installStateSyncBlockedReason(blocked bool) string {
	if !blocked {
		return ""
	}
	return "catalog uses oidc client_secret only in init_script scope; sync disabled to prevent silent rotation on container recreate"
}

func (st *InstallState) isV2Complete() bool {
	return st != nil &&
		st.SchemaVersion >= installStateSchemaVersionConfig &&
		len(bytes.TrimSpace(st.RawTemplate)) > 0 &&
		strings.TrimSpace(st.RawTemplateHash) != "" &&
		st.RawTemplateHash == Sha256Hex(st.RawTemplate) &&
		st.InstallSystemCtx != nil
}

func (st *InstallState) hasInvalidV2RawTemplateHash() bool {
	return st != nil &&
		st.SchemaVersion >= installStateSchemaVersionConfig &&
		len(bytes.TrimSpace(st.RawTemplate)) > 0 &&
		(strings.TrimSpace(st.RawTemplateHash) == "" || st.RawTemplateHash != Sha256Hex(st.RawTemplate))
}

func (st *InstallState) pendingCatalogSource() ([]byte, string, string, bool) {
	if st == nil || st.SchemaVersion < installStateSchemaVersionConfig || len(bytes.TrimSpace(st.PendingRawTemplate)) == 0 {
		return nil, "", "", false
	}
	hash := Sha256Hex(st.PendingRawTemplate)
	if strings.TrimSpace(st.PendingRawTemplateHash) != hash {
		return nil, "", "pending catalog source hash mismatch; ignoring pending schema", false
	}
	return st.PendingRawTemplate, hash, st.PendingReason, true
}

func (st *InstallState) pendingCatalogSourceForFlow(flow string) ([]byte, string, string, bool) {
	pendingRaw, hash, reason, ok := st.pendingCatalogSource()
	if !ok {
		return nil, "", reason, false
	}
	if st.pendingCatalogReviewFlow() != normalizePendingCatalogReviewFlow(flow) {
		return nil, "", "", false
	}
	return pendingRaw, hash, reason, true
}

func (st *InstallState) pendingCatalogReviewFlow() string {
	if st == nil {
		return pendingCatalogReviewFlowConfig
	}
	if flow := normalizePendingCatalogReviewFlow(st.PendingReviewFlow); flow != "" {
		return flow
	}
	// Older pending sources did not record a consumer. Preserve config-render
	// fallback, but route the known service-app review reason to the manifest
	// review flow so Edit Config cannot consume it after an upgrade.
	reason := strings.ToLower(strings.TrimSpace(st.PendingReason))
	if strings.Contains(reason, "operator review") || strings.Contains(reason, "service app update policy") {
		return pendingCatalogReviewFlowManifest
	}
	return pendingCatalogReviewFlowConfig
}

func normalizePendingCatalogReviewFlow(flow string) string {
	switch strings.TrimSpace(flow) {
	case pendingCatalogReviewFlowConfig, pendingCatalogReviewFlowManifest:
		return strings.TrimSpace(flow)
	default:
		return ""
	}
}

type PendingCatalogUpdateInfo struct {
	Pending bool   `json:"pending"`
	Hash    string `json:"hash,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Flow    string `json:"flow,omitempty"`
}

func (m *AppManager) PendingCatalogUpdateInfo(ctx context.Context, instanceID string) PendingCatalogUpdateInfo {
	_ = ctx
	state, err := m.ensureStateManager()
	if err != nil {
		return PendingCatalogUpdateInfo{}
	}
	st, err := state.LoadInstallState(instanceID)
	if err != nil {
		return PendingCatalogUpdateInfo{}
	}
	_, hash, reason, ok := st.pendingCatalogSource()
	if !ok {
		return PendingCatalogUpdateInfo{}
	}
	return PendingCatalogUpdateInfo{Pending: true, Hash: hash, Reason: reason, Flow: st.pendingCatalogReviewFlow()}
}

func storeCommittedCatalogMetadata(state *FilesystemStateManager, appInst *AppInstance, sourceHash string) error {
	if state == nil || appInst == nil || strings.TrimSpace(appInst.CatalogSource) == "" || strings.TrimSpace(sourceHash) == "" {
		return nil
	}
	appInst.CatalogManifestHash = sourceHash
	appInst.LastSyncAttemptHash = ""
	appInst.LastSyncError = ""
	if err := state.StoreAppMetadata(appInst); err != nil {
		return fmt.Errorf("persist committed catalog metadata: %w", err)
	}
	return nil
}

func (st *InstallState) markPendingCatalogSource(instanceID string, raw []byte, reason string) bool {
	return st.markPendingCatalogSourceForFlow(instanceID, raw, reason, pendingCatalogReviewFlowConfig)
}

func (st *InstallState) markPendingCatalogSourceForFlow(instanceID string, raw []byte, reason string, flow string) bool {
	if st == nil || len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	flow = normalizePendingCatalogReviewFlow(flow)
	if flow == "" {
		flow = pendingCatalogReviewFlowConfig
	}
	hash := Sha256Hex(raw)
	if st.InstanceID == "" {
		st.InstanceID = instanceID
	}
	if bytes.Equal(st.PendingRawTemplate, raw) && st.PendingRawTemplateHash == hash && st.PendingReason == reason && st.pendingCatalogReviewFlow() == flow {
		return false
	}
	st.SchemaVersion = installStateSchemaVersionConfig
	st.PendingRawTemplate = append([]byte(nil), raw...)
	st.PendingRawTemplateHash = hash
	st.PendingReason = reason
	st.PendingReviewFlow = flow
	st.Revision++
	if st.Revision <= 0 {
		st.Revision = 1
	}
	return true
}

func (st *InstallState) clearPendingCatalogSource() {
	if st == nil {
		return
	}
	st.PendingRawTemplate = nil
	st.PendingRawTemplateHash = ""
	st.PendingReason = ""
	st.PendingReviewFlow = ""
}

func (st *InstallState) markCatalogSourceCommitted(instanceID, catalogSource string, raw []byte) {
	if st == nil {
		return
	}
	if st.InstanceID == "" {
		st.InstanceID = instanceID
	}
	st.SchemaVersion = installStateSchemaVersionConfig
	st.Revision++
	if st.Revision <= 0 {
		st.Revision = 1
	}
	st.IsLegacyBackfill = false
	st.SourceKind = InstallSourceKindCatalog
	st.SourceRef = catalogSource
	st.RawTemplate = append([]byte(nil), raw...)
	st.RawTemplateHash = Sha256Hex(raw)
	st.clearPendingCatalogSource()
	if inputs, provenance, ok := filterInstallStateInputsForRawTemplate(instanceID, raw, st.InstallInputs, st.InputProvenance); ok {
		st.InstallInputs = inputs
		st.InputProvenance = provenance
		st.InputSensitive = mergeInstallInputSensitive(
			filterInstallInputSensitive(inputs, st.InputSensitive),
			initialInstallInputSensitive(instanceID, raw, inputs, provenance),
		)
	}
	if st.InputProvenance == nil {
		st.InputProvenance = make(map[string]string, len(st.InstallInputs))
	}
	for name := range st.InstallInputs {
		if st.InputProvenance[name] == "" {
			if name == "__app_address__" {
				st.InputProvenance[name] = InputProvenanceSystem
			} else {
				st.InputProvenance[name] = InputProvenanceOperator
			}
		}
	}
}

func filterInstallStateInputsForRawTemplate(instanceID string, raw []byte, inputs map[string]any, provenance map[string]string) (map[string]any, map[string]string, bool) {
	schema, err := ParseAppSchema(raw)
	if err != nil || schema == nil {
		return inputs, provenance, false
	}
	PrepareSmartDefaultsForUpdate(schema, instanceID)
	filteredInputs := make(map[string]any, len(schema.Inputs))
	filteredProvenance := make(map[string]string, len(schema.Inputs))
	for name, value := range inputs {
		if _, ok := schema.Inputs[name]; !ok {
			continue
		}
		filteredInputs[name] = value
		if provenance != nil && provenance[name] != "" {
			filteredProvenance[name] = provenance[name]
		}
	}
	return filteredInputs, filteredProvenance, true
}

func filterInstallInputSensitive(inputs map[string]any, sensitive map[string]bool) map[string]bool {
	if len(inputs) == 0 || len(sensitive) == 0 {
		return nil
	}
	out := map[string]bool{}
	for name := range inputs {
		if sensitive[name] {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeInstallInputSensitive(items ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		for name, sensitive := range item {
			if sensitive {
				out[name] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func catalogSyncRenderInputsForRawTemplate(instanceID string, raw []byte, inputs map[string]any) (map[string]any, bool) {
	schema, err := ParseAppSchema(raw)
	if err != nil || schema == nil {
		return inputs, false
	}
	PrepareSmartDefaultsForUpdate(schema, instanceID)
	filteredInputs := make(map[string]any, len(schema.Inputs))
	for name, value := range inputs {
		if _, ok := schema.Inputs[name]; !ok {
			continue
		}
		filteredInputs[name] = value
	}
	if _, ok := schema.Inputs["__app_address__"]; ok {
		filteredInputs["__app_address__"] = instanceID
	}
	return backfillInputDefaults(schema.Inputs, filteredInputs), true
}

type schemaOnlyOIDCGenerator struct{}

func (schemaOnlyOIDCGenerator) GenerateCredentials() (string, string, error) {
	return "__schema_only_client_id__", "__schema_only_client_secret__", nil
}

func (m *AppManager) schemaForInstallStateRawTemplate(ctx context.Context, instanceID string, raw []byte, st *InstallState) (*api.AppDefinition, error) {
	schema, _, err := m.schemaForInstallStateRawTemplateWithOrigin(ctx, instanceID, raw, st, nil)
	return schema, err
}

func (m *AppManager) schemaForInstallStateRawTemplateWithOrigin(ctx context.Context, instanceID string, raw []byte, st *InstallState, previous *api.AppDefinition) (*api.AppDefinition, bool, error) {
	schema, parseErr := ParseAppSchema(raw)
	if parseErr == nil {
		PrepareSmartDefaultsForUpdate(schema, instanceID)
		return schema, false, nil
	}
	if st == nil || st.InstallSystemCtx == nil {
		return nil, false, parseErr
	}
	res, err := RunInstallPipeline(ctx, InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    st.InstallInputs,
		SystemContext: *st.InstallSystemCtx,
		InstanceID:    instanceID,
		ExistingOIDC:  st.OIDCCredentials,
	}, schemaOnlyOIDCGenerator{}, m.syncSelfSkippingLister(instanceID))
	if err != nil {
		return nil, false, fmt.Errorf("parse raw schema: %v; render stored source for schema: %w", parseErr, err)
	}
	PrepareSmartDefaultsForUpdate(res.Definition, instanceID)
	if !m.renderedSchemaInputSpecsStableWithSentinels(ctx, instanceID, raw, st, previous, res.Definition) {
		return nil, false, fmt.Errorf("rendered stored source contains input names derived from stored values")
	}
	return res.Definition, true, nil
}

func (m *AppManager) ReadInstalledConfig(ctx context.Context, instanceID string) (*InstalledConfigReadResult, error) {
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
	st, err := state.LoadInstallState(instanceID)
	if err != nil {
		if errors.Is(err, ErrInstallStateNotFound) {
			return unrecoverableInstalledConfig(instanceID, "install config ledger not found"), nil
		}
		return unrecoverableInstalledConfig(instanceID, "install config ledger is malformed"), nil
	}
	if st.hasInvalidV2RawTemplateHash() {
		return unrecoverableInstalledConfig(instanceID, "install config ledger source hash mismatch"), nil
	}
	if !st.isV2Complete() {
		if recovered, reason := m.tryRecoverCatalogInstallState(ctx, state, appInst, st); recovered != nil {
			st = recovered
		} else if reason != "" {
			return unrecoverableInstalledConfig(instanceID, reason), nil
		}
	}
	if !st.isV2Complete() {
		return unrecoverableInstalledConfig(instanceID, "install config ledger is incomplete or legacy"), nil
	}
	rawTemplate := st.RawTemplate
	sourceHash := st.RawTemplateHash
	pendingReason := ""
	if pendingRaw, pendingHash, reason, ok := st.pendingCatalogSourceForFlow(pendingCatalogReviewFlowConfig); ok {
		rawTemplate = pendingRaw
		sourceHash = pendingHash
		pendingReason = reason
	}
	currentDef, _ := state.GetAppDefinition(instanceID)
	schema, renderedSchema, err := m.schemaForInstallStateRawTemplateWithOrigin(ctx, instanceID, rawTemplate, st, currentDef)
	if err != nil {
		return unrecoverableInstalledConfig(instanceID, "stored app source cannot be parsed"), nil
	}
	fields := installedConfigFields(schema.Inputs, st, currentDef, renderedSchema)
	result := &InstalledConfigReadResult{
		InstanceID:      instanceID,
		SourceKind:      st.SourceKind,
		SourceRef:       st.SourceRef,
		LedgerHealth:    "complete",
		LedgerRevision:  st.Revision,
		SourceHash:      sourceHash,
		InputSchemaHash: inputSchemaHash(schema.Inputs),
		Fields:          fields,
	}
	if pendingReason != "" {
		result.Warnings = append(result.Warnings, "Update needs attention: "+pendingReason)
	}
	if appInst.Mode() != ModeService {
		result.Warnings = append(result.Warnings, "workspace config apply is not supported in v1")
	}
	if !appInst.Enabled {
		result.Warnings = append(result.Warnings, "runtime-affecting config apply requires the app to be running")
	}
	return result, nil
}

func unrecoverableInstalledConfig(instanceID, warning string) *InstalledConfigReadResult {
	return &InstalledConfigReadResult{
		InstanceID:   instanceID,
		LedgerHealth: "unrecoverable",
		Warnings:     []string{warning},
	}
}

const (
	schemaInputNameSentinelPrefix            = "__PICCOLO_SCHEMA_INPUT_NAME_SENTINEL_"
	sensitiveStructuralRenderRejectedReason  = "sensitive or generated values cannot be used in manifest structure; use them only as field values"
	sensitiveStructuralRenderRejectedSummary = "candidate manifest structure depends on sensitive or generated values"
)

func (m *AppManager) renderedSchemaInputSpecsStableWithSentinels(ctx context.Context, instanceID string, raw []byte, st *InstallState, previous *api.AppDefinition, rendered *api.AppDefinition) bool {
	if st == nil || st.InstallSystemCtx == nil || rendered == nil {
		return false
	}
	sentinelInputs, inputsChanged := renderedSchemaSentinelInputs(st, previous, rendered)
	sentinelCreds, credsChanged := renderedSchemaSentinelOIDCCredentials(st.OIDCCredentials)
	if !inputsChanged && !credsChanged {
		return true
	}
	res, err := RunInstallPipeline(ctx, InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    sentinelInputs,
		SystemContext: *st.InstallSystemCtx,
		InstanceID:    instanceID,
		ExistingOIDC:  sentinelCreds,
	}, schemaOnlyOIDCGenerator{}, m.syncSelfSkippingLister(instanceID))
	if err != nil {
		return false
	}
	PrepareSmartDefaultsForUpdate(res.Definition, instanceID)
	return inputSchemaHash(rendered.Inputs) == inputSchemaHash(res.Definition.Inputs)
}

func renderedSchemaSentinelInputs(st *InstallState, previous *api.AppDefinition, rendered *api.AppDefinition) (map[string]interface{}, bool) {
	out := make(map[string]interface{}, len(st.InstallInputs))
	for name, value := range st.InstallInputs {
		out[name] = value
	}
	unsafe := displayUnsafeStoredInputNames(st, previous, rendered)
	changed := false
	for name := range unsafe {
		if _, ok := out[name]; !ok {
			continue
		}
		out[name] = schemaInputNameSentinelPrefix + Sha256Hex([]byte(name))[:16] + "__"
		changed = true
	}
	return out, changed
}

func renderedSchemaSentinelOIDCCredentials(creds *OIDCCredentials) (*OIDCCredentials, bool) {
	if creds == nil {
		return nil, false
	}
	out := *creds
	changed := false
	if strings.TrimSpace(out.ClientID) != "" {
		out.ClientID = schemaInputNameSentinelPrefix + "oidc_client_id__"
		changed = true
	}
	if strings.TrimSpace(out.ClientSecret) != "" {
		out.ClientSecret = schemaInputNameSentinelPrefix + "oidc_client_secret__"
		changed = true
	}
	return &out, changed
}

func displayRenderSentinelInputs(st *InstallState, previous *api.AppDefinition, rendered *api.AppDefinition, inputs map[string]interface{}) (map[string]interface{}, bool) {
	out := make(map[string]interface{}, len(inputs))
	for name, value := range inputs {
		out[name] = value
	}
	unsafe := displayUnsafeStoredInputNames(st, previous, rendered)
	changed := false
	for name := range unsafe {
		if _, ok := out[name]; !ok {
			continue
		}
		out[name] = schemaInputNameSentinelPrefix + Sha256Hex([]byte(name))[:16] + "__"
		changed = true
	}
	return out, changed
}

func (m *AppManager) renderDisplayDefinitionWithSentinels(ctx context.Context, instanceID string, raw []byte, inputs map[string]interface{}, systemCtx InstallSystemContext, st *InstallState, previous *api.AppDefinition, rendered *api.AppDefinition, creds *OIDCCredentials) (*api.AppDefinition, bool, error) {
	sentinelInputs, inputsChanged := displayRenderSentinelInputs(st, previous, rendered, inputs)
	sentinelCreds, credsChanged := renderedSchemaSentinelOIDCCredentials(creds)
	if !inputsChanged && !credsChanged {
		return rendered, false, nil
	}
	res, err := RunInstallPipeline(ctx, InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    sentinelInputs,
		SystemContext: systemCtx,
		InstanceID:    instanceID,
		ExistingOIDC:  sentinelCreds,
	}, schemaOnlyOIDCGenerator{}, m.syncSelfSkippingLister(instanceID))
	if err != nil {
		return nil, false, err
	}
	return res.Definition, true, nil
}

func mergeCustomManifestDisplayPolicy(real, display customManifestPolicyResult) customManifestPolicyResult {
	display.Allowed = real.Allowed
	display.Stageable = real.Stageable
	display.MetadataOnly = real.MetadataOnly
	display.UpdateClass = real.UpdateClass
	display.Classification.UpdateClass = real.Classification.UpdateClass
	display.Classification.HasOperatorReview = real.Classification.HasOperatorReview
	display.Classification.HasRejected = real.Classification.HasRejected
	display.Classification.RequiresV2Apply = real.Classification.RequiresV2Apply
	display.Classification.V1StructuralRestart = real.Classification.V1StructuralRestart
	if !real.Stageable && strings.TrimSpace(display.Reason) == "" {
		display.Reason = "candidate contains unsupported manifest changes"
	}
	if real.Stageable {
		display.Reason = ""
	}
	return display
}

func mergeDisplaySummaryRuntimeSemantics(display, real ManifestUpdateSummary, genericChange string) ManifestUpdateSummary {
	if summaryOnlySaysNoRuntimeChanges(display.WillChange) && !summaryOnlySaysNoRuntimeChanges(real.WillChange) {
		display.WillChange = []string{genericChange}
	}
	if len(display.WillRestart) == 0 && len(real.WillRestart) > 0 {
		display.WillRestart = append([]string(nil), real.WillRestart...)
	}
	if len(display.ExpectedInterruption) == 0 && len(real.ExpectedInterruption) > 0 {
		display.ExpectedInterruption = append([]string(nil), real.ExpectedInterruption...)
	}
	display.WillPreserve = appendMissingStrings(display.WillPreserve, real.WillPreserve)
	return display
}

func summaryOnlySaysNoRuntimeChanges(values []string) bool {
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && strings.TrimSpace(values[0]) == "no runtime changes"
}

func appendMissingStrings(values []string, additions []string) []string {
	for _, value := range additions {
		if strings.TrimSpace(value) == "" || slices.Contains(values, value) {
			continue
		}
		values = append(values, value)
	}
	return values
}

func mergeInstalledConfigDisplayPolicy(real, display installedConfigPolicyResult) installedConfigPolicyResult {
	display.Allowed = real.Allowed
	display.MetadataOnly = real.MetadataOnly
	display.RequiresSnapshot = real.RequiresSnapshot
	if !real.Allowed && strings.TrimSpace(display.Reason) == "" {
		display.Reason = "rendered candidate changes unsupported app structure; use manifest update or reinstall"
	}
	if real.Allowed {
		display.Reason = ""
	}
	return display
}

func displayUnsafeStoredInputNames(st *InstallState, previous *api.AppDefinition, rendered *api.AppDefinition) map[string]struct{} {
	unsafe := map[string]struct{}{}
	addSchema := func(inputs map[string]api.AppInput) {
		for name, spec := range inputs {
			if inputIsSensitive(name, spec) || spec.Generate {
				unsafe[name] = struct{}{}
			}
		}
	}
	if previous != nil {
		addSchema(previous.Inputs)
	}
	if rendered != nil {
		addSchema(rendered.Inputs)
	}
	if st == nil {
		return unsafe
	}
	for name, sensitive := range st.InputSensitive {
		if sensitive {
			unsafe[name] = struct{}{}
		}
	}
	for name, provenance := range st.InputProvenance {
		if provenance == InputProvenanceGenerated {
			unsafe[name] = struct{}{}
		}
	}
	for name := range st.InstallInputs {
		if inputNameLooksSensitive(name) {
			unsafe[name] = struct{}{}
		}
	}
	delete(unsafe, "__app_address__")
	return unsafe
}

func manifestRenderHasUnsafeValues(st *InstallState, previous, rendered *api.AppDefinition, inputs map[string]interface{}, creds *OIDCCredentials) bool {
	unsafeNames := displayUnsafeStoredInputNames(st, previous, rendered)
	for name := range unsafeNames {
		if inputs != nil {
			if _, ok := inputs[name]; ok {
				return true
			}
		}
		if st != nil && st.InstallInputs != nil {
			if _, ok := st.InstallInputs[name]; ok {
				return true
			}
		}
	}
	return creds != nil && (strings.TrimSpace(creds.ClientID) != "" || strings.TrimSpace(creds.ClientSecret) != "")
}

func manifestUpdateUnsafeDisplayFragments(st *InstallState, previous, rendered *api.AppDefinition, inputs map[string]interface{}, creds *OIDCCredentials) []string {
	unsafeNames := displayUnsafeStoredInputNames(st, previous, rendered)
	fragments := map[string]struct{}{}
	addValue := func(value any) {
		for _, fragment := range unsafeDisplayFragments(value) {
			fragments[fragment] = struct{}{}
		}
	}
	for name := range unsafeNames {
		if inputs != nil {
			if value, ok := inputs[name]; ok {
				addValue(value)
			}
		}
		if st != nil && st.InstallInputs != nil {
			if value, ok := st.InstallInputs[name]; ok {
				addValue(value)
			}
		}
	}
	if creds != nil {
		for _, fragment := range unsafeCredentialDisplayFragments(creds.ClientID) {
			fragments[fragment] = struct{}{}
		}
		for _, fragment := range unsafeCredentialDisplayFragments(creds.ClientSecret) {
			fragments[fragment] = struct{}{}
		}
	}
	out := make([]string, 0, len(fragments))
	for fragment := range fragments {
		out = append(out, fragment)
	}
	slices.SortFunc(out, func(a, b string) int {
		if len(a) > len(b) {
			return -1
		}
		if len(a) < len(b) {
			return 1
		}
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	})
	return out
}

func unsafeDisplayFragments(value any) []string {
	return unsafeDisplayFragmentsWithMinimums(value, 4, 8)
}

func unsafeCredentialDisplayFragments(value any) []string {
	return unsafeDisplayFragmentsWithMinimums(value, 8, 12)
}

func unsafeDisplayFragmentsWithMinimums(value any, minPrefix, minInner int) []string {
	raw := strings.TrimSpace(fmt.Sprint(value))
	if len(raw) < minPrefix {
		return nil
	}
	const maxUnsafeDisplaySource = 128
	original := raw
	if len(raw) > maxUnsafeDisplaySource {
		raw = raw[:maxUnsafeDisplaySource]
	}
	fragments := map[string]struct{}{
		original: {},
		raw:      {},
	}
	addFragments := func(source string) {
		if len(source) < minPrefix {
			return
		}
		for end := minPrefix; end <= len(source); end++ {
			fragments[source[:end]] = struct{}{}
		}
		for start := 1; start < len(source); start++ {
			maxEnd := start + 32
			if maxEnd > len(source) {
				maxEnd = len(source)
			}
			for end := start + minInner; end <= maxEnd; end++ {
				fragments[source[start:end]] = struct{}{}
			}
		}
	}
	addFragments(raw)
	if len(original) > maxUnsafeDisplaySource {
		addFragments(original[len(original)-maxUnsafeDisplaySource:])
	}
	out := make([]string, 0, len(fragments))
	for fragment := range fragments {
		out = append(out, fragment)
	}
	return out
}

func redactUnsafeDisplayText(text string, fragments []string) string {
	if text == "" || len(fragments) == 0 {
		return text
	}
	out := text
	for _, fragment := range fragments {
		if fragment == "" || fragment == "<redacted>" {
			continue
		}
		out = strings.ReplaceAll(out, fragment, "<redacted>")
	}
	return out
}

func sanitizeManifestUpdateSummaryForDisplay(summary *ManifestUpdateSummary, fragments []string) {
	if summary == nil || len(fragments) == 0 {
		return
	}
	summary.WillChange = redactUnsafeDisplaySlice(summary.WillChange, fragments)
	summary.WillRestart = redactUnsafeDisplaySlice(summary.WillRestart, fragments)
	summary.WillPreserve = redactUnsafeDisplaySlice(summary.WillPreserve, fragments)
	summary.ExpectedInterruption = redactUnsafeDisplaySlice(summary.ExpectedInterruption, fragments)
	summary.Rejected = redactUnsafeDisplaySlice(summary.Rejected, fragments)
}

func sanitizeManifestUpdateClassificationForDisplay(classification *manifestUpdateClassification, fragments []string) {
	if classification == nil || len(fragments) == 0 {
		return
	}
	for i := range classification.Decisions {
		classification.Decisions[i].Path = redactUnsafeDisplayText(classification.Decisions[i].Path, fragments)
		classification.Decisions[i].Summary = redactUnsafeDisplayText(classification.Decisions[i].Summary, fragments)
		classification.Decisions[i].Reason = redactUnsafeDisplayText(classification.Decisions[i].Reason, fragments)
	}
	for i := range classification.ExposureReview {
		classification.ExposureReview[i].Path = redactUnsafeDisplayText(classification.ExposureReview[i].Path, fragments)
		classification.ExposureReview[i].Old = redactUnsafeDisplayText(classification.ExposureReview[i].Old, fragments)
		classification.ExposureReview[i].New = redactUnsafeDisplayText(classification.ExposureReview[i].New, fragments)
		classification.ExposureReview[i].Confirmation = redactUnsafeDisplayText(classification.ExposureReview[i].Confirmation, fragments)
	}
	for i := range classification.KeptValueReview {
		classification.KeptValueReview[i].Field = redactUnsafeDisplayText(classification.KeptValueReview[i].Field, fragments)
		classification.KeptValueReview[i].OldSemantic = redactUnsafeDisplaySlice(classification.KeptValueReview[i].OldSemantic, fragments)
		classification.KeptValueReview[i].NewSemantic = redactUnsafeDisplaySlice(classification.KeptValueReview[i].NewSemantic, fragments)
		classification.KeptValueReview[i].SemanticDelta = redactUnsafeDisplaySlice(classification.KeptValueReview[i].SemanticDelta, fragments)
		classification.KeptValueReview[i].OldUsage = redactUnsafeDisplaySlice(classification.KeptValueReview[i].OldUsage, fragments)
		classification.KeptValueReview[i].NewUsage = redactUnsafeDisplaySlice(classification.KeptValueReview[i].NewUsage, fragments)
		classification.KeptValueReview[i].Confirmation = redactUnsafeDisplayText(classification.KeptValueReview[i].Confirmation, fragments)
		classification.KeptValueReview[i].BlockingReason = redactUnsafeDisplayText(classification.KeptValueReview[i].BlockingReason, fragments)
	}
	classification.RequiredConfirmations = redactUnsafeDisplaySlice(classification.RequiredConfirmations, fragments)
	classification.RuntimeReadiness = redactUnsafeDisplaySlice(classification.RuntimeReadiness, fragments)
	classification.StagedImageRootfs = redactUnsafeDisplaySlice(classification.StagedImageRootfs, fragments)
	classification.ListenerRoutingAuth = redactUnsafeDisplaySlice(classification.ListenerRoutingAuth, fragments)
	classification.StorageBoundary = redactUnsafeDisplaySlice(classification.StorageBoundary, fragments)
	classification.FirstRejectedReason = redactUnsafeDisplayText(classification.FirstRejectedReason, fragments)
	if classification.DataSafety != nil {
		classification.DataSafety.Reason = redactUnsafeDisplayText(classification.DataSafety.Reason, fragments)
		classification.DataSafety.FailureBehavior = redactUnsafeDisplayText(classification.DataSafety.FailureBehavior, fragments)
		classification.DataSafety.RollbackLimit = redactUnsafeDisplayText(classification.DataSafety.RollbackLimit, fragments)
	}
}

func redactUnsafeDisplaySlice(values []string, fragments []string) []string {
	for i := range values {
		values[i] = redactUnsafeDisplayText(values[i], fragments)
	}
	return values
}

func storedInputDisplaySensitive(name string, st *InstallState, previous *api.AppDefinition, candidate map[string]api.AppInput) bool {
	if name == "__app_address__" {
		return false
	}
	if spec, ok := candidate[name]; ok && (inputIsSensitive(name, spec) || spec.Generate) {
		return true
	}
	if st == nil || st.InstallInputs == nil {
		return false
	}
	if _, present := st.InstallInputs[name]; !present {
		return false
	}
	if st.InputSensitive != nil && st.InputSensitive[name] {
		return true
	}
	if st.InputProvenance != nil && st.InputProvenance[name] == InputProvenanceGenerated {
		return true
	}
	if inputNameLooksSensitive(name) {
		return true
	}
	if previous == nil {
		return true
	}
	oldSpec, ok := previous.Inputs[name]
	if !ok {
		return true
	}
	return inputIsSensitive(name, oldSpec) || oldSpec.Generate
}

func installInputValueUsableForSchema(value any, spec api.AppInput) bool {
	if value == nil {
		return false
	}
	if spec.Generate || (spec.Required && inputTypeStringLike(spec)) {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return false
		}
	}
	return validateInputValue("__redacted__", spec, value) == nil
}

func inputTypeStringLike(spec api.AppInput) bool {
	return spec.Type == "string" || spec.Type == "password"
}

func validateKeptInstallInputValue(name string, spec api.AppInput, value any, currentSensitive bool) error {
	if currentSensitive && validateInputValue("__redacted__", spec, value) != nil {
		return fmt.Errorf("input %q stored value cannot be safely reused with the new schema; replace or regenerate it", name)
	}
	if !installInputValueUsableForSchema(value, spec) {
		return fmt.Errorf("input %q has no usable stored value; replace or regenerate it", name)
	}
	return nil
}

func (m *AppManager) tryRecoverCatalogInstallState(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, st *InstallState) (*InstallState, string) {
	if appInst == nil || st == nil || strings.TrimSpace(appInst.CatalogSource) == "" {
		return nil, ""
	}
	if st.InstallSystemCtx == nil {
		return nil, "install config ledger lacks system context"
	}
	expectedHash := strings.TrimSpace(appInst.CatalogManifestHash)
	if expectedHash == "" {
		return nil, "catalog install does not record the installed source hash"
	}
	host := m.currentSyncHost()
	if host == nil {
		return nil, "catalog source recovery requires the catalog sync host"
	}
	raw, err := host.FetchCatalogTemplate(ctx, appInst.CatalogSource)
	if err != nil {
		return nil, "catalog source recovery failed: " + err.Error()
	}
	if got := Sha256Hex(raw); got != expectedHash {
		return nil, "catalog source no longer matches the installed manifest hash; reinstall, apply manifest YAML, or wait for a successful catalog sync"
	}
	next := *st
	if next.InstallInputs == nil {
		next.InstallInputs = map[string]any{}
	}
	next.markCatalogSourceCommitted(appInst.InstanceID, appInst.CatalogSource, raw)
	if err := m.ensureKernelLeader(); err != nil {
		return &next, ""
	}
	if err := state.StoreInstallState(appInst.InstanceID, &next); err != nil {
		return nil, "catalog source recovery could not persist the config ledger: " + err.Error()
	}
	return &next, ""
}

func installedConfigFields(inputs map[string]api.AppInput, st *InstallState, previous *api.AppDefinition, redactMetadata bool) []InstalledConfigField {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	fields := make([]InstalledConfigField, 0, len(names))
	for _, name := range names {
		spec := inputs[name]
		currentSensitive := storedInputDisplaySensitive(name, st, previous, inputs)
		sensitive := inputIsSensitive(name, spec) || spec.Generate || currentSensitive
		displaySpec := clientDisplayInputSpec(name, spec, "", redactMetadata)
		value, present := st.InstallInputs[name]
		provenance := st.InputProvenance[name]
		clearedOptionalSensitive := present && sensitive && !spec.Required && inputTypeStringLike(spec) && strings.TrimSpace(fmt.Sprint(value)) == ""
		usablePresent := present && !clearedOptionalSensitive && installInputValueUsableForSchema(value, spec)
		if clearedOptionalSensitive {
			provenance = "absent_sensitive"
		}
		field := InstalledConfigField{
			Name:        name,
			Type:        spec.Type,
			Label:       displaySpec.Label,
			Description: displaySpec.Description,
			Required:    spec.Required,
			Generate:    spec.Generate,
			Sensitive:   sensitive,
			Present:     usablePresent,
			Editable:    name != "__app_address__",
		}
		if provenance == "" {
			if !usablePresent && spec.Default != nil && !spec.Generate && !sensitive {
				provenance = InputProvenanceCatalogDefault
			} else if !usablePresent && sensitive && !spec.Required {
				provenance = "absent_sensitive"
			} else {
				provenance = InputProvenanceLegacyUnknown
			}
		}
		field.Provenance = provenance
		if !sensitive {
			field.Schema = &displaySpec
			if usablePresent {
				field.Display = value
			} else if !spec.Generate {
				if displaySpec.Default != nil {
					field.Display = displaySpec.Default
				} else if !spec.Required {
					field.Display = zeroInputValue(spec.Type)
				}
			}
		}
		if name == "__app_address__" {
			field.Actions = []string{"keep"}
		} else if spec.Generate {
			if usablePresent {
				field.Actions = []string{"keep", "regenerate"}
			} else {
				field.Actions = []string{"regenerate"}
			}
		} else if sensitive {
			if usablePresent {
				field.Actions = []string{"keep", "replace"}
			} else if spec.Required {
				field.Actions = []string{"replace"}
			} else {
				field.Actions = []string{"keep", "replace"}
			}
			if usablePresent && !spec.Required && (spec.Type == "string" || spec.Type == "password") {
				field.Actions = append(field.Actions, "clear")
			}
		} else {
			field.Actions = []string{"replace"}
		}
		fields = append(fields, field)
	}
	return fields
}

func (m *AppManager) DryRunInstalledConfigUpdate(ctx context.Context, instanceID string, req InstalledConfigUpdateRequest) (*InstalledConfigUpdateResult, error) {
	cand, result, err := m.renderInstalledConfigCandidate(ctx, instanceID, req)
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
	cand.ExpiresAt = cand.CreatedAt.Add(configUpdateTokenTTL)
	result.DryRunToken = token
	m.storeInstalledConfigCandidate(cand)
	return result, nil
}

func (m *AppManager) ApplyInstalledConfigUpdate(ctx context.Context, instanceID string, req InstalledConfigUpdateRequest) (res *InstalledConfigUpdateResult, err error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	accessRepairPending := false
	m.emitProgress(ctx, taskTypeUpdateConfig, instanceID, taskPhaseValidating, 0, "Validating config update", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeUpdateConfig, instanceID, taskPhaseComplete, 100, "Config update failed", true, err)
		} else if accessRepairPending {
			m.emitProgress(ctx, taskTypeUpdateConfig, instanceID, taskPhaseComplete, 100, "Config update applied; access repair pending", true, nil)
		} else {
			m.emitProgress(ctx, taskTypeUpdateConfig, instanceID, taskPhaseComplete, 100, "Config update complete", true, nil)
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
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, errAppNotFound(instanceID)
	}
	cand, ok := m.takeInstalledConfigCandidate(req.DryRunToken)
	if !ok {
		return nil, fmt.Errorf("%w: dry-run token is expired or unknown", ErrInstalledConfigConflict)
	}
	if cand.InstanceID != instanceID {
		return nil, fmt.Errorf("%w: dry-run token belongs to a different app", ErrInstalledConfigConflict)
	}
	if req.CandidateDigest != "" && req.CandidateDigest != cand.CandidateDigest {
		return nil, fmt.Errorf("%w: candidate digest does not match dry run", ErrInstalledConfigConflict)
	}
	if req.LedgerRevision != 0 && req.LedgerRevision != cand.LedgerRevision {
		return nil, fmt.Errorf("%w: ledger revision does not match dry run", ErrInstalledConfigConflict)
	}
	if req.SourceHash != "" && req.SourceHash != cand.SourceHash {
		return nil, fmt.Errorf("%w: source hash does not match dry run", ErrInstalledConfigConflict)
	}
	if req.InputSchemaHash != "" && req.InputSchemaHash != cand.InputSchemaHash {
		return nil, fmt.Errorf("%w: input schema hash does not match dry run", ErrInstalledConfigConflict)
	}
	if req.BaseManifestHash != "" && req.BaseManifestHash != cand.BaseManifestHash {
		return nil, fmt.Errorf("%w: base manifest hash does not match dry run", ErrInstalledConfigConflict)
	}
	if req.RuntimeFingerprint != "" && req.RuntimeFingerprint != cand.RuntimeFingerprint {
		return nil, fmt.Errorf("%w: runtime fingerprint does not match dry run", ErrInstalledConfigConflict)
	}

	currentState, err := state.LoadInstallState(instanceID)
	if err != nil {
		return nil, fmt.Errorf("%w: load config ledger: %v", ErrInstalledConfigConflict, err)
	}
	if !currentState.isV2Complete() {
		return nil, fmt.Errorf("%w: config ledger is incomplete or corrupted; reload config form", ErrInstalledConfigConflict)
	}
	currentSourceHash := currentState.RawTemplateHash
	if _, pendingHash, _, ok := currentState.pendingCatalogSourceForFlow(pendingCatalogReviewFlowConfig); ok {
		currentSourceHash = pendingHash
	}
	if currentState.Revision != cand.LedgerRevision || currentSourceHash != cand.SourceHash {
		return nil, fmt.Errorf("%w: config ledger changed after dry run", ErrInstalledConfigConflict)
	}
	curDef, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return nil, fmt.Errorf("read current manifest: %w", err)
	}
	curHash, err := canonicalManifestHash(curDef)
	if err != nil {
		return nil, fmt.Errorf("hash current manifest: %w", err)
	}
	if curHash != cand.BaseManifestHash {
		return nil, fmt.Errorf("%w: manifest changed after dry run", ErrInstalledConfigConflict)
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return nil, fmt.Errorf("fingerprint runtime: %w", err)
	}
	if runtimeFingerprint != cand.RuntimeFingerprint {
		return nil, fmt.Errorf("%w: runtime changed after dry run", ErrInstalledConfigConflict)
	}

	if !cand.MetadataOnly && !appInst.Enabled {
		return nil, fmt.Errorf("%w: start app before applying runtime config", ErrInstalledConfigRejected)
	}

	cand.InstallState.Revision = cand.LedgerRevision + 1
	applyTxn, err := m.beginInstalledAppApplyTransaction(ctx, state, installedAppApplyTransactionSpec{
		OperationKind:             "config_update",
		TaskType:                  taskTypeUpdateConfig,
		RollbackPrefix:            "config update rolled back",
		InstanceID:                instanceID,
		AppInst:                   appInst,
		PreviousDefinition:        curDef,
		CandidateDefinition:       cand.Definition,
		PreviousManifestHash:      cand.BaseManifestHash,
		CandidateManifestHash:     cand.CandidateDigest,
		PreviousLedgerRevision:    cand.LedgerRevision,
		CandidateLedgerRevision:   cand.InstallState.Revision,
		PreviousLedgerSourceHash:  currentState.RawTemplateHash,
		CandidateLedgerSourceHash: cand.InstallState.RawTemplateHash,
		DryRunToken:               cand.Token,
		RuntimeFingerprint:        cand.RuntimeFingerprint,
		MetadataOnly:              cand.MetadataOnly,
		RequiresPrecommitSnapshot: cand.RequiresSnapshot,
		ApplyPhase:                taskPhaseApplyingConfig,
		ApplyMessage:              "Persisting rendered config",
		FinalizingMessage:         "Saving config ledger",
	})
	if err != nil {
		return nil, err
	}
	if err := applyTxn.persistCandidateManifest(); err != nil {
		return nil, err
	}
	if err := applyTxn.recreateRuntimeIfNeeded(); err != nil {
		return nil, err
	}
	if err := applyTxn.commitLedger(cand.InstallState); err != nil {
		return nil, err
	}
	var catalogMetadataErr error
	if cand.InstallState.SourceKind == InstallSourceKindCatalog &&
		appInst.CatalogSource != "" &&
		currentState.RawTemplateHash != cand.InstallState.RawTemplateHash {
		if err := storeCommittedCatalogMetadata(state, appInst, cand.InstallState.RawTemplateHash); err != nil {
			catalogMetadataErr = err
			log.Printf("WARN: config update %s: committed catalog metadata pending retry: %v", instanceID, err)
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
	return &InstalledConfigUpdateResult{
		InstanceID:          instanceID,
		LedgerRevision:      cand.InstallState.Revision,
		SourceHash:          cand.SourceHash,
		InputSchemaHash:     cand.InputSchemaHash,
		BaseManifestHash:    cand.BaseManifestHash,
		RuntimeFingerprint:  cand.RuntimeFingerprint,
		CandidateDigest:     cand.CandidateDigest,
		DiffKind:            cand.DiffKind.String(),
		Applicable:          true,
		MetadataOnly:        cand.MetadataOnly,
		AccessRepairPending: accessRepairPending,
		AccessRepairMessage: accessRepairMessage,
		Actions:             cand.Actions,
		Summary:             cand.Summary,
	}, nil
}

func (m *AppManager) renderInstalledConfigCandidate(ctx context.Context, instanceID string, req InstalledConfigUpdateRequest) (*installedConfigCandidate, *InstalledConfigUpdateResult, error) {
	if err := m.ensureUnlocked(); err != nil {
		return nil, nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, nil, errAppNotFound(instanceID)
	}
	st, err := state.LoadInstallState(instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInstalledConfigUnavailable, err)
	}
	if st.hasInvalidV2RawTemplateHash() {
		return nil, nil, fmt.Errorf("%w: install config ledger source hash mismatch", ErrInstalledConfigUnavailable)
	}
	if !st.isV2Complete() {
		if recovered, reason := m.tryRecoverCatalogInstallState(ctx, state, appInst, st); recovered != nil {
			st = recovered
		} else if reason != "" {
			return nil, nil, fmt.Errorf("%w: %s", ErrInstalledConfigUnavailable, reason)
		}
	}
	if !st.isV2Complete() {
		return nil, nil, fmt.Errorf("%w: install config ledger is incomplete or legacy", ErrInstalledConfigUnavailable)
	}
	if req.LedgerRevision != 0 && req.LedgerRevision != st.Revision {
		return nil, nil, fmt.Errorf("%w: config ledger changed; reload config form", ErrInstalledConfigConflict)
	}
	rawTemplate := st.RawTemplate
	sourceHash := st.RawTemplateHash
	pendingSource := false
	if pendingRaw, pendingHash, _, ok := st.pendingCatalogSourceForFlow(pendingCatalogReviewFlowConfig); ok {
		rawTemplate = pendingRaw
		sourceHash = pendingHash
		pendingSource = true
	}
	if req.SourceHash != "" && req.SourceHash != sourceHash {
		return nil, nil, fmt.Errorf("%w: app source changed; reload config form", ErrInstalledConfigConflict)
	}
	curDef, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read current manifest: %w", err)
	}
	schema, _, err := m.schemaForInstallStateRawTemplateWithOrigin(ctx, instanceID, rawTemplate, st, curDef)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: parse stored source: %v", ErrInstalledConfigUnavailable, err)
	}
	schemaHash := inputSchemaHash(schema.Inputs)
	if req.InputSchemaHash != "" && req.InputSchemaHash != schemaHash {
		return nil, nil, fmt.Errorf("%w: input schema changed; reload config form", ErrInstalledConfigConflict)
	}

	inputs, provenance, inputSensitive, actions, err := normalizeInstalledConfigInputs(schema.Inputs, st, curDef, req, instanceID)
	if err != nil {
		return nil, nil, err
	}
	baseHash, err := canonicalManifestHash(curDef)
	if err != nil {
		return nil, nil, err
	}
	if req.BaseManifestHash != "" && req.BaseManifestHash != baseHash {
		return nil, nil, fmt.Errorf("%w: manifest changed; reload config form", ErrInstalledConfigConflict)
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return nil, nil, err
	}
	if req.RuntimeFingerprint != "" && req.RuntimeFingerprint != runtimeFingerprint {
		return nil, nil, fmt.Errorf("%w: runtime changed; rerun dry run", ErrInstalledConfigConflict)
	}
	unsafeRender := manifestRenderHasUnsafeValues(st, curDef, schema, inputs, st.OIDCCredentials)
	res, err := RunInstallPipeline(ctx, InstallPipelineInput{
		RawTemplate:   rawTemplate,
		UserInputs:    inputs,
		SystemContext: *st.InstallSystemCtx,
		InstanceID:    instanceID,
		ExistingOIDC:  st.OIDCCredentials,
	}, nil, m.syncSelfSkippingLister(instanceID))
	if err != nil {
		if unsafeRender {
			reason := sensitiveStructuralRenderRejectedReason
			return nil, &InstalledConfigUpdateResult{
				InstanceID:         instanceID,
				LedgerRevision:     st.Revision,
				SourceHash:         sourceHash,
				InputSchemaHash:    schemaHash,
				BaseManifestHash:   baseHash,
				RuntimeFingerprint: runtimeFingerprint,
				DiffKind:           DiffKindStructuralNoImage.String(),
				Applicable:         false,
				BlockingReason:     reason,
				Summary: ManifestUpdateSummary{
					WillChange: []string{sensitiveStructuralRenderRejectedSummary},
					Rejected:   []string{reason},
				},
			}, nil
		}
		return nil, nil, err
	}
	displayOldDef := curDef
	displayNewDef := res.Definition
	displayNew, displayChanged, err := m.renderDisplayDefinitionWithSentinels(ctx, instanceID, rawTemplate, inputs, *st.InstallSystemCtx, st, curDef, res.Definition, st.OIDCCredentials)
	if err != nil {
		return nil, nil, fmt.Errorf("render safe config display: %w", err)
	}
	if displayChanged {
		displayNewDef = displayNew
		displayOld, _, err := m.renderDisplayDefinitionWithSentinels(ctx, instanceID, st.RawTemplate, st.InstallInputs, *st.InstallSystemCtx, st, curDef, curDef, st.OIDCCredentials)
		if err != nil {
			return nil, nil, fmt.Errorf("render safe current config display: %w", err)
		}
		displayOldDef = displayOld
	}
	realPolicy, realSummary := evaluateInstalledConfigPolicy(curDef, res.Definition, appInst)
	policy, summary := evaluateInstalledConfigPolicy(displayOldDef, displayNewDef, appInst)
	if displayChanged {
		policy = mergeInstalledConfigDisplayPolicy(realPolicy, policy)
		summary = mergeDisplaySummaryRuntimeSemantics(summary, realSummary, "sensitive config value changed")
		if reason := manifestSensitiveStructuralDriftReason(manifestSensitiveStructuralDrift(res.Definition, displayNewDef)); reason != "" {
			policy.Allowed = false
			if policy.Reason == "" {
				policy.Reason = reason
			}
			scrubSensitiveStructuralDriftSummary(&summary, reason)
		}
	} else {
		policy = realPolicy
		summary = realSummary
	}
	unsafeDisplayFragments := manifestUpdateUnsafeDisplayFragments(st, curDef, res.Definition, inputs, st.OIDCCredentials)
	sanitizeManifestUpdateSummaryForDisplay(&summary, unsafeDisplayFragments)
	policy.Reason = redactUnsafeDisplayText(policy.Reason, unsafeDisplayFragments)
	diffKind := classifyDiff(cloneDefinitionForCompare(curDef), cloneDefinitionForCompare(res.Definition))
	candidateDigest := Sha256Hex(res.CanonicalBytes)
	result := &InstalledConfigUpdateResult{
		InstanceID:         instanceID,
		LedgerRevision:     st.Revision,
		SourceHash:         sourceHash,
		InputSchemaHash:    schemaHash,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: runtimeFingerprint,
		CandidateDigest:    candidateDigest,
		DiffKind:           diffKind.String(),
		Applicable:         policy.Allowed,
		BlockingReason:     policy.Reason,
		MetadataOnly:       policy.MetadataOnly,
		Actions:            actions,
		Summary:            summary,
	}
	if !policy.Allowed {
		return nil, result, nil
	}
	nextState := *st
	if pendingSource {
		nextState.RawTemplate = append([]byte(nil), rawTemplate...)
		nextState.RawTemplateHash = sourceHash
		nextState.clearPendingCatalogSource()
	}
	nextState.InstallInputs, nextState.InputProvenance = persistedInstalledConfigLedger(inputs, provenance)
	nextState.InputSensitive = persistedInstallInputSensitive(nextState.InstallInputs, inputSensitive)
	nextState.RawTemplateHash = Sha256Hex(nextState.RawTemplate)
	cand := &installedConfigCandidate{
		InstanceID:         instanceID,
		LedgerRevision:     st.Revision,
		SourceHash:         sourceHash,
		InputSchemaHash:    schemaHash,
		BaseManifestHash:   baseHash,
		RuntimeFingerprint: runtimeFingerprint,
		CandidateDigest:    candidateDigest,
		DiffKind:           diffKind,
		MetadataOnly:       policy.MetadataOnly,
		RequiresSnapshot:   policy.RequiresSnapshot,
		Definition:         res.Definition,
		InstallState:       &nextState,
		Actions:            actions,
		Summary:            summary,
	}
	return cand, result, nil
}

func normalizeInstalledConfigInputs(declared map[string]api.AppInput, st *InstallState, previous *api.AppDefinition, req InstalledConfigUpdateRequest, instanceID string) (map[string]any, map[string]string, map[string]bool, []InstalledConfigActionSummary, error) {
	out := make(map[string]any, len(declared)+1)
	provenance := make(map[string]string, len(declared)+1)
	inputSensitive := map[string]bool{}
	regen := map[string]bool{}
	for _, name := range req.RegenerateInputs {
		regen[name] = true
	}
	actions := []InstalledConfigActionSummary{}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		spec := declared[name]
		oldValue, oldPresent := any(nil), false
		if st != nil && st.InstallInputs != nil {
			oldValue, oldPresent = st.InstallInputs[name]
		}
		oldUsable := oldPresent && installInputValueUsableForSchema(oldValue, spec)
		if value := st.InputProvenance[name]; value != "" {
			provenance[name] = value
		}
		if name == "__app_address__" {
			out[name] = instanceID
			provenance[name] = InputProvenanceSystem
			continue
		}
		sensitive := inputIsSensitive(name, spec) || spec.Generate || storedInputDisplaySensitive(name, st, previous, declared)
		switch {
		case spec.Generate:
			if regen[name] {
				value, err := GenerateSecurePassword()
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("generate input %q: %w", name, err)
				}
				out[name] = value
				provenance[name] = InputProvenanceGenerated
				inputSensitive[name] = true
				actions = append(actions, InstalledConfigActionSummary{Field: name, Action: "regenerate", Sensitive: true, Consequence: "existing sessions or integrations may be invalidated"})
			} else if !oldUsable {
				if oldPresent {
					if err := validateKeptInstallInputValue(name, spec, oldValue, true); err != nil {
						return nil, nil, nil, nil, err
					}
				}
				return nil, nil, nil, nil, fmt.Errorf("input %q is missing and must be regenerated", name)
			} else {
				out[name] = oldValue
				inputSensitive[name] = true
			}
		case sensitive:
			action := strings.TrimSpace(req.SecretActions[name])
			if action == "" {
				action = "keep"
			}
			switch action {
			case "keep":
				if !oldUsable && spec.Required {
					if oldPresent {
						if err := validateKeptInstallInputValue(name, spec, oldValue, sensitive); err != nil {
							return nil, nil, nil, nil, err
						}
					}
					return nil, nil, nil, nil, fmt.Errorf("input %q is missing and must be replaced", name)
				}
				if oldUsable {
					if err := validateKeptInstallInputValue(name, spec, oldValue, sensitive); err != nil {
						return nil, nil, nil, nil, err
					}
					out[name] = oldValue
					inputSensitive[name] = true
				}
			case "replace":
				value, exists := req.Inputs[name]
				if !exists {
					return nil, nil, nil, nil, fmt.Errorf("input %q replacement value is required", name)
				}
				if strings.TrimSpace(fmt.Sprint(value)) == "" {
					return nil, nil, nil, nil, fmt.Errorf("input %q replacement value cannot be empty; use clear for optional secrets", name)
				}
				out[name] = value
				provenance[name] = InputProvenanceOperator
				if inputIsSensitive(name, spec) || spec.Generate {
					inputSensitive[name] = true
				}
				actions = append(actions, InstalledConfigActionSummary{Field: name, Action: "replace", Sensitive: true, Consequence: "external integrations may need the new value"})
			case "clear":
				if spec.Required || (spec.Type != "string" && spec.Type != "password") {
					return nil, nil, nil, nil, fmt.Errorf("input %q cannot be cleared", name)
				}
				out[name] = ""
				provenance[name] = InputProvenanceOperator
				actions = append(actions, InstalledConfigActionSummary{Field: name, Action: "clear", Sensitive: true, Consequence: "external integrations using this value may stop working"})
			default:
				return nil, nil, nil, nil, fmt.Errorf("input %q has invalid secret action %q", name, action)
			}
		default:
			if value, exists := req.Inputs[name]; exists {
				out[name] = value
				provenance[name] = InputProvenanceOperator
				actions = append(actions, InstalledConfigActionSummary{
					Field:     name,
					Action:    "replace",
					Sensitive: false,
					OldDisplay: func() any {
						if oldUsable {
							return oldValue
						}
						return nil
					}(),
					NewDisplay: value,
				})
			} else if oldUsable {
				if err := validateKeptInstallInputValue(name, spec, oldValue, sensitive); err != nil {
					return nil, nil, nil, nil, err
				}
				out[name] = oldValue
			} else {
				if spec.Default != nil {
					out[name] = normalizeInputValueForValidation(spec.Type, spec.Default)
					provenance[name] = InputProvenanceCatalogDefault
				} else if spec.Required {
					return nil, nil, nil, nil, fmt.Errorf("input %q is required", name)
				} else {
					out[name] = zeroInputValue(spec.Type)
					provenance[name] = InputProvenanceCatalogDefault
				}
			}
		}
	}
	if err := ValidateInputs(declared, out); err != nil {
		return nil, nil, nil, nil, err
	}
	return out, provenance, persistedInstallInputSensitive(out, inputSensitive), actions, nil
}

func persistedInstalledConfigLedger(inputs map[string]any, provenance map[string]string) (map[string]any, map[string]string) {
	persistedInputs := make(map[string]any, len(inputs))
	persistedProvenance := make(map[string]string, len(provenance))
	for name, value := range inputs {
		if provenance[name] == InputProvenanceCatalogDefault {
			continue
		}
		persistedInputs[name] = value
	}
	for name, value := range provenance {
		if value == "" || value == InputProvenanceCatalogDefault {
			continue
		}
		if _, ok := persistedInputs[name]; !ok && value != InputProvenanceSystem {
			continue
		}
		persistedProvenance[name] = value
	}
	return persistedInputs, persistedProvenance
}

func persistedInstallInputSensitive(inputs map[string]any, sensitive map[string]bool) map[string]bool {
	if len(inputs) == 0 || len(sensitive) == 0 {
		return nil
	}
	out := map[string]bool{}
	for name := range inputs {
		if sensitive[name] {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func evaluateInstalledConfigPolicy(oldDef, newDef *api.AppDefinition, appInst *AppInstance) (installedConfigPolicyResult, ManifestUpdateSummary) {
	oldCmp := cloneDefinitionForCompare(oldDef)
	newCmp := cloneDefinitionForCompare(newDef)
	summary := ManifestUpdateSummary{
		WillPreserve: []string{
			"app identity and primary listener",
			"existing image references and active rootfs volumes",
			"no image pull",
		},
	}
	reject := func(reason string) (installedConfigPolicyResult, ManifestUpdateSummary) {
		summary.Rejected = append(summary.Rejected, reason)
		return installedConfigPolicyResult{Allowed: false, Reason: reason}, summary
	}
	if oldCmp == nil || newCmp == nil {
		return reject("current and candidate manifests are required")
	}
	if appInst == nil || appInst.Mode() != ModeService || piccoloModeFromExtensions(oldCmp.Extensions) != ModeService || piccoloModeFromExtensions(newCmp.Extensions) != ModeService {
		return reject("installed config update v1 only supports service-mode apps")
	}
	if oldCmp.Type != newCmp.Type ||
		oldCmp.WorkspaceName != newCmp.WorkspaceName ||
		oldCmp.PrimaryService != newCmp.PrimaryService ||
		!reflect.DeepEqual(oldCmp.Listeners, newCmp.Listeners) ||
		!reflect.DeepEqual(oldCmp.Permissions, newCmp.Permissions) ||
		!reflect.DeepEqual(oldCmp.Resources, newCmp.Resources) ||
		!reflect.DeepEqual(oldCmp.HealthCheck, newCmp.HealthCheck) ||
		!reflect.DeepEqual(oldCmp.Auth, newCmp.Auth) ||
		!reflect.DeepEqual(oldCmp.Extensions, newCmp.Extensions) ||
		!reflect.DeepEqual(oldCmp.Environment, newCmp.Environment) ||
		!reflect.DeepEqual(oldCmp.Lifecycle, newCmp.Lifecycle) ||
		!reflect.DeepEqual(oldCmp.Storage, newCmp.Storage) {
		return reject("rendered candidate changes unsupported app structure; use manifest update or reinstall")
	}
	if len(oldCmp.Services) != len(newCmp.Services) {
		return reject("service additions or removals require manifest update or reinstall")
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
			return reject(fmt.Sprintf("service %q was removed or renamed; unsupported in v1", name))
		}
		if oldSvc.Image != newSvc.Image ||
			!reflect.DeepEqual(oldSvc.BindPorts, newSvc.BindPorts) ||
			!reflect.DeepEqual(oldSvc.After, newSvc.After) ||
			oldSvc.Init != newSvc.Init ||
			!reflect.DeepEqual(oldSvc.InitScript, newSvc.InitScript) ||
			!reflect.DeepEqual(oldSvc.Storage, newSvc.Storage) {
			return reject(fmt.Sprintf("service %q changed unsupported structure; use manifest update or reinstall", name))
		}
		if !reflect.DeepEqual(oldSvc.OIDCClient, newSvc.OIDCClient) {
			return reject(fmt.Sprintf("service %q oidc_client changed; use manifest update or reinstall", name))
		}
		for _, key := range changedStringMapKeys(oldSvc.Environment, newSvc.Environment) {
			summary.WillChange = append(summary.WillChange, fmt.Sprintf("service %s environment key %s", name, key))
		}
	}
	for name := range newCmp.Services {
		if _, exists := oldCmp.Services[name]; !exists {
			return reject(fmt.Sprintf("service %q was added; unsupported in v1", name))
		}
	}
	appConfigChanged := !reflect.DeepEqual(oldCmp.AppConfig, newCmp.AppConfig)
	if appConfigChanged {
		summary.WillChange = append(summary.WillChange, "app_config")
	}
	oldCmp.Inputs = nil
	newCmp.Inputs = nil
	oldCmp.AppConfig = nil
	newCmp.AppConfig = nil
	for name, svc := range oldCmp.Services {
		svc.Environment = nil
		oldCmp.Services[name] = svc
	}
	for name, svc := range newCmp.Services {
		svc.Environment = nil
		newCmp.Services[name] = svc
	}
	if !reflect.DeepEqual(oldCmp, newCmp) {
		return reject("rendered candidate changes unsupported app structure; use manifest update or reinstall")
	}
	diffKind := classifyDiff(cloneDefinitionForCompare(oldDef), cloneDefinitionForCompare(newDef))
	oldBytes, _ := SerializeAppDefinition(cloneDefinitionForCompare(oldDef))
	newBytes, _ := SerializeAppDefinition(cloneDefinitionForCompare(newDef))
	metadataOnly := diffKind == DiffKindNone && !bytes.Equal(oldBytes, newBytes)
	if metadataOnly {
		summary.WillChange = append(summary.WillChange, "config metadata/input schema")
	} else if len(summary.WillChange) > 0 {
		summary.WillRestart = append(summary.WillRestart, "existing containers will be recreated using current images/rootfs")
		summary.ExpectedInterruption = append(summary.ExpectedInterruption, "services may disconnect while containers restart")
	}
	if len(summary.WillChange) == 0 && len(summary.WillRestart) == 0 {
		summary.WillChange = append(summary.WillChange, "no runtime changes")
		metadataOnly = true
	}
	if !metadataOnly && !manifestUpdateHasProbeableRuntimeListener(newCmp) {
		return reject("runtime-changing config updates with only UDP listeners require a future UDP readiness probe")
	}
	requiresSnapshot := !metadataOnly && appHasPersistentStorage(oldDef)
	if requiresSnapshot {
		summary.WillPreserve = append(summary.WillPreserve, "persistent app data volumes preserved with private pre-commit failure snapshot")
	} else {
		summary.WillPreserve = append(summary.WillPreserve, "persistent app data volumes preserved but not snapshotted")
	}
	if !metadataOnly && !appInst.Enabled {
		return reject("start app before applying runtime config")
	}
	return installedConfigPolicyResult{Allowed: true, MetadataOnly: metadataOnly, RequiresSnapshot: requiresSnapshot}, summary
}

func inputIsSensitive(name string, spec api.AppInput) bool {
	if spec.Type == "password" || spec.Generate {
		return true
	}
	return inputNameLooksSensitive(name + " " + spec.Label + " " + spec.Description)
}

func clientDisplayInputSpec(name string, spec api.AppInput, instanceID string, redactMetadata bool) api.AppInput {
	display := spec
	if name == "__app_address__" && instanceID != "" {
		display.Default = instanceID
	} else if redactMetadata || inputIsSensitive(name, spec) || spec.Generate {
		display.Default = nil
	}
	if redactMetadata {
		display.Label = ""
		display.Description = ""
		display.Validation = nil
	}
	return display
}

func inputNameLooksSensitive(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{"secret", "token", "key", "password", "credential"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func inputSchemaHash(inputs map[string]api.AppInput) string {
	payload := struct {
		Inputs map[string]api.AppInput `json:"inputs"`
	}{Inputs: inputs}
	data, _ := jsonMarshalStable(payload)
	return Sha256Hex(data)
}

func jsonMarshalStable(v interface{}) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (m *AppManager) storeInstalledConfigCandidate(c *installedConfigCandidate) {
	m.configUpdateMu.Lock()
	defer m.configUpdateMu.Unlock()
	if m.configUpdateCandidates == nil {
		m.configUpdateCandidates = make(map[string]*installedConfigCandidate)
	}
	now := time.Now().UTC()
	for token, existing := range m.configUpdateCandidates {
		if existing == nil || existing.ExpiresAt.Before(now) || existing.InstanceID == c.InstanceID {
			delete(m.configUpdateCandidates, token)
		}
	}
	m.configUpdateCandidates[c.Token] = c
}

func (m *AppManager) takeInstalledConfigCandidate(token string) (*installedConfigCandidate, bool) {
	if strings.TrimSpace(token) == "" {
		return nil, false
	}
	m.configUpdateMu.Lock()
	defer m.configUpdateMu.Unlock()
	c, ok := m.configUpdateCandidates[token]
	if !ok || c == nil || c.ExpiresAt.Before(time.Now().UTC()) {
		delete(m.configUpdateCandidates, token)
		return nil, false
	}
	delete(m.configUpdateCandidates, token)
	return c, true
}
