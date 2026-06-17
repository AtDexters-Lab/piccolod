package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
	"piccolod/internal/state/paths"
)

type installedConfigSyncHost struct {
	templates       map[string][]byte
	systemCtx       *InstallSystemContext
	fetchErr        error
	requiresProxy   func(*api.AppDefinition) bool
	registeredProxy *[]string
	deletedProxy    *[]string
	registerErr     error
	registerErrCall int
}

func (h installedConfigSyncHost) FetchCatalogTemplate(ctx context.Context, catalogSource string) ([]byte, error) {
	_ = ctx
	if h.fetchErr != nil {
		return nil, h.fetchErr
	}
	if raw, ok := h.templates[catalogSource]; ok {
		return raw, nil
	}
	return nil, errors.New("missing catalog template")
}

func (h installedConfigSyncHost) FetchInitScript(ctx context.Context, catalogSource, filePath string) ([]byte, error) {
	_ = ctx
	_ = catalogSource
	_ = filePath
	return nil, errors.New("init scripts not used")
}

func (h installedConfigSyncHost) CurrentInstallSystemContext() InstallSystemContext {
	if h.systemCtx != nil {
		return *h.systemCtx
	}
	return InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
}

func (h installedConfigSyncHost) OIDCClientGenerator() OIDCClientGenerator { return nil }

func (h installedConfigSyncHost) PersistOIDCClient(ctx context.Context, clientID, clientSecret, appID string) error {
	_ = ctx
	_ = clientID
	_ = clientSecret
	_ = appID
	return nil
}

func (h installedConfigSyncHost) DeleteOIDCClient(ctx context.Context, clientID string) error {
	_ = ctx
	_ = clientID
	return nil
}

func (h installedConfigSyncHost) RequiresProxyOIDCClient(def *api.AppDefinition) bool {
	if h.requiresProxy != nil {
		return h.requiresProxy(def)
	}
	return false
}

func (h installedConfigSyncHost) RegisterProxyOIDCClient(ctx context.Context, instanceID string) error {
	_ = ctx
	call := 1
	if h.registeredProxy != nil {
		call = len(*h.registeredProxy) + 1
		*h.registeredProxy = append(*h.registeredProxy, instanceID)
	}
	if h.registerErrCall > 0 && h.registerErrCall != call {
		return nil
	}
	return h.registerErr
}

func (h installedConfigSyncHost) DeleteProxyOIDCClient(ctx context.Context, instanceID string) {
	_ = ctx
	if h.deletedProxy != nil {
		*h.deletedProxy = append(*h.deletedProxy, instanceID)
	}
}

func TestReadInstalledConfigRecoversCatalogRawSourceAndRedactsSecrets(t *testing.T) {
	mgr, state, raw, inputs, systemCtx := installedConfigTestApp(t)
	if err := state.StoreInstallState("piclu", &InstallState{
		InstanceID:       "piclu",
		IsLegacyBackfill: true,
		InstallInputs:    inputs,
		InstallSystemCtx: &systemCtx,
	}); err != nil {
		t.Fatalf("store legacy-ish install state: %v", err)
	}
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})

	result, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if result.LedgerHealth != "complete" || result.SourceHash != Sha256Hex(raw) {
		t.Fatalf("expected recovered complete ledger, got health=%s source=%s warnings=%v", result.LedgerHealth, result.SourceHash, result.Warnings)
	}
	var secretField *InstalledConfigField
	for i := range result.Fields {
		if result.Fields[i].Name == "gemini_api_key" {
			secretField = &result.Fields[i]
			break
		}
	}
	if secretField == nil {
		t.Fatalf("secret field not returned")
	}
	if !secretField.Sensitive || secretField.Display != nil {
		t.Fatalf("secret field should be sensitive and redacted, got sensitive=%v display=%v", secretField.Sensitive, secretField.Display)
	}
	if secretField.Schema != nil {
		t.Fatalf("secret field schema should be omitted")
	}
	for i := range result.Fields {
		if result.Fields[i].Name == "webhook_token" && result.Fields[i].Schema != nil {
			t.Fatalf("name-inferred sensitive field schema should be omitted")
		}
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load recovered state: %v", err)
	}
	if !st.isV2Complete() || st.SourceKind != InstallSourceKindCatalog {
		t.Fatalf("state was not promoted to v2 catalog ledger: %+v", st)
	}
	if st.IsLegacyBackfill {
		t.Fatalf("recovered v2 catalog ledger still marked legacy: %+v", st)
	}
}

func TestReadInstalledConfigRecoveryDoesNotPersistOnFollower(t *testing.T) {
	mgr, state, raw, inputs, systemCtx := installedConfigTestApp(t)
	if err := state.StoreInstallState("piclu", &InstallState{
		InstanceID:       "piclu",
		InstallInputs:    inputs,
		InstallSystemCtx: &systemCtx,
	}); err != nil {
		t.Fatalf("store legacy-ish install state: %v", err)
	}
	mgr.leadershipMu.Lock()
	mgr.leadershipState[cluster.ResourceKernel] = cluster.RoleFollower
	mgr.leadershipMu.Unlock()
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})

	result, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if result.LedgerHealth != "complete" {
		t.Fatalf("expected read-only recovered field model, got health=%s warnings=%v", result.LedgerHealth, result.Warnings)
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.isV2Complete() {
		t.Fatalf("follower should not persist recovered v2 ledger")
	}
}

func TestReadInstalledConfigRecoversCatalogRawSourceWithNoInputs(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	raw := []byte(`type: user
inputs:
  provider:
    type: string
    default: local
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/default-only:stable
    bind_ports: [8080]
    environment:
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    map[string]any{},
		SystemContext: systemCtx,
		InstanceID:    "defaultsonly",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render install pipeline: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:          "defaultsonly",
		Enabled:             true,
		PrimaryService:      "main",
		Containers:          map[string]string{"main": "cid-main"},
		ActiveRootfs:        map[string]string{"main": "rootfs-main"},
		CatalogSource:       "defaultsonly",
		CatalogManifestHash: Sha256Hex(raw),
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          res.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("defaultsonly", &InstallState{
		InstanceID:       "defaultsonly",
		InstallSystemCtx: &systemCtx,
	}); err != nil {
		t.Fatalf("store legacy-ish install state: %v", err)
	}
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"defaultsonly": raw}})

	read, err := mgr.ReadInstalledConfig(context.Background(), "defaultsonly")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.LedgerHealth != "complete" {
		t.Fatalf("expected recovered complete ledger, got health=%s warnings=%v", read.LedgerHealth, read.Warnings)
	}
	st, err := state.LoadInstallState("defaultsonly")
	if err != nil {
		t.Fatalf("load recovered state: %v", err)
	}
	if !st.isV2Complete() {
		t.Fatalf("state was not promoted to v2 catalog ledger: %+v", st)
	}
	if st.InstallInputs == nil || len(st.InstallInputs) != 0 {
		t.Fatalf("recovered zero-input ledger inputs = %#v, want empty map", st.InstallInputs)
	}
}

func TestInstalledConfigRejectsLedgerRawTemplateHashMismatch(t *testing.T) {
	mgr, state, _, _, _ := installedConfigTestApp(t)
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	st.RawTemplate = append([]byte("# tampered source\n"), st.RawTemplate...)
	if st.RawTemplateHash == Sha256Hex(st.RawTemplate) {
		t.Fatalf("test setup failed: tampered source still matches stored hash")
	}
	if err := state.StoreInstallState("piclu", st); err != nil {
		t.Fatalf("store tampered install state: %v", err)
	}

	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.LedgerHealth != "unrecoverable" {
		t.Fatalf("expected unrecoverable ledger, got health=%s warnings=%v", read.LedgerHealth, read.Warnings)
	}
	if !strings.Contains(strings.ToLower(strings.Join(read.Warnings, " ")), "source hash") {
		t.Fatalf("expected source hash warning, got %v", read.Warnings)
	}
	_, err = mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{})
	if err == nil {
		t.Fatalf("expected dry run to reject tampered ledger")
	}
	if !errors.Is(err, ErrInstalledConfigUnavailable) || !strings.Contains(strings.ToLower(err.Error()), "source hash") {
		t.Fatalf("unexpected dry run error: %v", err)
	}
}

func TestRefreshInstallSystemContextRestoresLedgerOnSyncFailure(t *testing.T) {
	mgr, state, raw, _, systemCtx := installedConfigTestApp(t)
	before, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	fresh := systemCtx
	fresh.Timezone = "Asia/Kolkata"
	mgr.SetSyncHost(installedConfigSyncHost{
		templates: map[string][]byte{"piclu": raw},
		systemCtx: &fresh,
		fetchErr:  errors.New("catalog temporarily unavailable"),
	})

	err = mgr.RefreshInstallSystemContext(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected refresh-context sync failure")
	}
	after, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load restored install state: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision after failed refresh = %d, want %d", after.Revision, before.Revision)
	}
	if after.InstallSystemCtx == nil || before.InstallSystemCtx == nil {
		t.Fatalf("missing system context after restore: before=%+v after=%+v", before.InstallSystemCtx, after.InstallSystemCtx)
	}
	if after.InstallSystemCtx.Timezone != before.InstallSystemCtx.Timezone {
		t.Fatalf("timezone after failed refresh = %q, want %q", after.InstallSystemCtx.Timezone, before.InstallSystemCtx.Timezone)
	}
}

func TestRefreshInstallSystemContextStagesLedgerThroughManifestApply(t *testing.T) {
	mgr, state, raw, inputs, systemCtx := installedConfigTestApp(t)
	tzRaw := []byte(strings.Replace(string(raw), `      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"`, `      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"
      TZ: "{{ .System.Timezone }}"`, 1))
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   tzRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render timezone template: %v", err)
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found")
	}
	appInst.Definition = res.Definition
	appInst.CatalogManifestHash = Sha256Hex(tzRaw)
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store timezone app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCatalog, "piclu", tzRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store timezone install state: %v", err)
	}

	fresh := systemCtx
	fresh.Timezone = "Asia/Kolkata"
	mgr.SetSyncHost(installedConfigSyncHost{
		templates: map[string][]byte{"piclu": tzRaw},
		systemCtx: &fresh,
	})
	state.storeAppMetadataHook = func(instanceID string, app *AppInstance) error {
		if instanceID == "piclu" && app.Definition != nil {
			if svc, ok := app.Definition.Services["main"]; ok && svc.Environment["TZ"] == "Asia/Kolkata" {
				return errors.New("injected candidate metadata failure")
			}
		}
		return nil
	}

	err = mgr.RefreshInstallSystemContext(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected refresh-context apply failure")
	}
	after, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if after.InstallSystemCtx == nil {
		t.Fatalf("missing install system context")
	}
	if after.InstallSystemCtx.Timezone != systemCtx.Timezone {
		t.Fatalf("timezone committed before manifest apply: got %q want %q", after.InstallSystemCtx.Timezone, systemCtx.Timezone)
	}
}

func TestEditConfigSurfacesPendingCatalogRequiredInput(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	newRaw := []byte(strings.Replace(string(raw), `  webhook_token:
    type: string
    label: Webhook token
    default: default-token`, `  transport_hostname:
    type: string
    label: Transport hostname
    required: true
  webhook_token:
    type: string
    label: Webhook token
    default: default-token`, 1))
	newHash := Sha256Hex(newRaw)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": newRaw}})

	err := mgr.SyncManifest(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected sync to fail on new required input")
	}
	if !strings.Contains(err.Error(), "transport_hostname") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	pendingState, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load pending install state: %v", err)
	}
	if pendingState.PendingReviewFlow != pendingCatalogReviewFlowConfig {
		t.Fatalf("pending review flow = %q, want %q", pendingState.PendingReviewFlow, pendingCatalogReviewFlowConfig)
	}
	info := mgr.PendingCatalogUpdateInfo(context.Background(), "piclu")
	if !info.Pending || info.Flow != pendingCatalogReviewFlowConfig {
		t.Fatalf("pending catalog info = %+v, want config flow", info)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.SourceHash != newHash {
		t.Fatalf("read source hash = %q, want pending hash %q", read.SourceHash, newHash)
	}
	var pendingField *InstalledConfigField
	for i := range read.Fields {
		if read.Fields[i].Name == "transport_hostname" {
			pendingField = &read.Fields[i]
			break
		}
	}
	if pendingField == nil {
		t.Fatalf("pending required input not surfaced in fields: %+v", read.Fields)
	}
	if pendingField.Present {
		t.Fatalf("pending field should not be marked present")
	}
	if len(read.Warnings) == 0 || !strings.Contains(strings.Join(read.Warnings, " "), "Update needs attention") {
		t.Fatalf("expected pending update warning, got %v", read.Warnings)
	}

	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		Inputs: map[string]interface{}{
			"transport_hostname": "transport.piclu.example.com",
		},
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run pending config update: %v", err)
	}
	if !dryRun.Applicable {
		t.Fatalf("dry run not applicable: %s", dryRun.BlockingReason)
	}
	applied, err := mgr.ApplyInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		DryRunToken:        dryRun.DryRunToken,
		CandidateDigest:    dryRun.CandidateDigest,
		LedgerRevision:     dryRun.LedgerRevision,
		SourceHash:         dryRun.SourceHash,
		InputSchemaHash:    dryRun.InputSchemaHash,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
	})
	if err != nil {
		t.Fatalf("apply pending config update: %v", err)
	}
	if !applied.Applicable {
		t.Fatalf("apply result not applicable")
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != newHash {
		t.Fatalf("committed raw hash = %q, want %q", st.RawTemplateHash, newHash)
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != newHash {
		t.Fatalf("catalog hash = %q, want %q", appInst.CatalogManifestHash, newHash)
	}
	if len(st.PendingRawTemplate) != 0 || st.PendingRawTemplateHash != "" {
		t.Fatalf("pending source was not cleared after apply")
	}
	if st.PendingReviewFlow != "" {
		t.Fatalf("pending review flow = %q, want cleared", st.PendingReviewFlow)
	}
	if got := st.InstallInputs["transport_hostname"]; got != "transport.piclu.example.com" {
		t.Fatalf("transport input = %#v", got)
	}
}

func TestCatalogSyncStoresPendingServiceAppReviewForImageChange(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	oldHash := Sha256Hex(raw)
	newRaw := []byte(strings.Replace(string(raw), "docker.io/example/piclu:stable", "docker.io/example/piclu:new", 1))
	newHash := Sha256Hex(newRaw)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": newRaw}})

	err := mgr.SyncManifest(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected sync to require operator review")
	}
	if !strings.Contains(err.Error(), "operator review") || !strings.Contains(err.Error(), "image reference changed") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.PendingRawTemplateHash != newHash || !bytes.Equal(st.PendingRawTemplate, newRaw) {
		t.Fatalf("pending source hash/raw mismatch: hash=%q want %q raw=%q", st.PendingRawTemplateHash, newHash, string(st.PendingRawTemplate))
	}
	if st.PendingReviewFlow != pendingCatalogReviewFlowManifest {
		t.Fatalf("pending review flow = %q, want %q", st.PendingReviewFlow, pendingCatalogReviewFlowManifest)
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != oldHash {
		t.Fatalf("catalog hash advanced to %q, want old hash %q", appInst.CatalogManifestHash, oldHash)
	}
	if appInst.LastSyncAttemptHash != newHash {
		t.Fatalf("attempt hash = %q, want %q", appInst.LastSyncAttemptHash, newHash)
	}
	if !strings.Contains(appInst.LastSyncError, "operator review") {
		t.Fatalf("last sync error = %q", appInst.LastSyncError)
	}
	info := mgr.PendingCatalogUpdateInfo(context.Background(), "piclu")
	if !info.Pending || info.Flow != pendingCatalogReviewFlowManifest {
		t.Fatalf("pending catalog info = %+v, want manifest review flow", info)
	}
	storedDef, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get stored definition: %v", err)
	}
	if storedDef.Services["main"].Image != "docker.io/example/piclu:stable" {
		t.Fatalf("candidate image was applied: %q", storedDef.Services["main"].Image)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.SourceHash != oldHash {
		t.Fatalf("read source hash = %q, want installed hash %q", read.SourceHash, oldHash)
	}
	if strings.Contains(strings.Join(read.Warnings, " "), "pending catalog update requires attention") {
		t.Fatalf("service-app review source leaked into config warnings: %v", read.Warnings)
	}
	if _, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      newHash,
		InputSchemaHash: read.InputSchemaHash,
	}); err == nil {
		t.Fatalf("config dry-run accepted manifest-review pending source hash")
	}

	mgr.containerManager = NewMockContainerManager()
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: paths.CoreRoot()}}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(paths.CoreRoot()))
	appInst.ActiveRootfs[networkAnchorServiceName] = "rootfs-anchor"
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app rootfs state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:     "piclu",
		CatalogPending: true,
	})
	if err != nil {
		t.Fatalf("dry-run pending catalog update: %v", err)
	}
	if !dryRun.Applicable {
		t.Fatalf("pending catalog dry run not applicable: %s", dryRun.BlockingReason)
	}
	if dryRun.UpdateClass != "service_app_update_v2" {
		t.Fatalf("update class = %q, want service_app_update_v2", dryRun.UpdateClass)
	}
	if !slices.Contains(dryRun.RequiredConfirmations, "image_update_review") {
		t.Fatalf("expected image review confirmation, got %v", dryRun.RequiredConfirmations)
	}
	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		CatalogPending:     true,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
		DryRunToken:        dryRun.DryRunToken,
		Confirmations:      dryRun.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply pending catalog update: %v", err)
	}
	if !applied.Applicable {
		t.Fatalf("apply result not applicable")
	}
	st, err = state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state after apply: %v", err)
	}
	if st.RawTemplateHash != newHash {
		t.Fatalf("committed source hash = %q, want %q", st.RawTemplateHash, newHash)
	}
	if len(st.PendingRawTemplate) != 0 || st.PendingRawTemplateHash != "" {
		t.Fatalf("pending source was not cleared after reviewed apply")
	}
	if st.PendingReviewFlow != "" {
		t.Fatalf("pending review flow = %q, want cleared", st.PendingReviewFlow)
	}
	appInst, exists = state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found after apply")
	}
	if appInst.CatalogManifestHash != newHash {
		t.Fatalf("catalog manifest hash = %q, want %q", appInst.CatalogManifestHash, newHash)
	}
	if appInst.LastSyncError != "" {
		t.Fatalf("last sync error = %q, want cleared", appInst.LastSyncError)
	}
	storedDef, err = state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get stored definition after apply: %v", err)
	}
	if storedDef.Services["main"].Image != "docker.io/example/piclu:new" {
		t.Fatalf("reviewed catalog image was not applied: %q", storedDef.Services["main"].Image)
	}
}

func TestCatalogSyncStoresPendingServiceAppReviewForRenderedOnlyTemplate(t *testing.T) {
	mgr, state, raw, inputs, systemCtx := installedConfigTestApp(t)
	oldHash := Sha256Hex(raw)
	newRaw := []byte(strings.Replace(string(raw), "docker.io/example/piclu:stable", "docker.io/example/piclu:new", 1))
	newRaw = []byte(strings.Replace(string(newRaw),
		`  diag_dir:
    type: string
    label: Diagnostics directory
    default: /diagnostics`,
		`  diag_dir:
    type: string
    label: "Diagnostics {{ .Inputs.diag_dir }}"
    description: "Diagnostics {{ .Inputs.diag_dir }}"
    default: "{{ .Inputs.diag_dir }}"
    validation:
      regex: "^.*$"
      message: "Diagnostics {{ .Inputs.diag_dir }}"`, 1))
	newRaw = []byte(strings.Replace(string(newRaw),
		`      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"`,
		`      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"
{{ if .Inputs.gemini_api_key }}
      RENDERED_ONLY_MARKER: "{{ .Inputs.gemini_api_key }}"
{{ end }}`, 1))
	newHash := Sha256Hex(newRaw)
	if _, err := ParseAppSchema(newRaw); err == nil {
		t.Fatalf("test template should require render before schema parse")
	}
	if _, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   newRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil); err != nil {
		t.Fatalf("rendered-only template should render through install pipeline: %v", err)
	}
	installSt, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state before sync: %v", err)
	}
	if installSt.InstallInputs == nil {
		installSt.InstallInputs = map[string]any{}
	}
	installSt.InstallInputs["gemini_api_key"] = "opaque123"
	installSt.InstallInputs["diag_dir"] = "/diagnostics"
	if installSt.InputProvenance == nil {
		installSt.InputProvenance = map[string]string{}
	}
	installSt.InputProvenance["diag_dir"] = InputProvenanceOperator
	if err := state.StoreInstallState("piclu", installSt); err != nil {
		t.Fatalf("store install state before sync: %v", err)
	}

	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": newRaw}})
	err = mgr.SyncManifest(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected sync to require operator review")
	}
	if !strings.Contains(err.Error(), "operator review") || !strings.Contains(err.Error(), "image reference changed") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.PendingRawTemplateHash != newHash || !bytes.Equal(st.PendingRawTemplate, newRaw) {
		t.Fatalf("pending source hash/raw mismatch: hash=%q want %q raw=%q", st.PendingRawTemplateHash, newHash, string(st.PendingRawTemplate))
	}
	if st.PendingReviewFlow != pendingCatalogReviewFlowManifest {
		t.Fatalf("pending review flow = %q, want %q", st.PendingReviewFlow, pendingCatalogReviewFlowManifest)
	}
	info := mgr.PendingCatalogUpdateInfo(context.Background(), "piclu")
	if !info.Pending || info.Hash != newHash || info.Flow != pendingCatalogReviewFlowManifest {
		t.Fatalf("pending catalog info = %+v, want pending hash %q and manifest review flow", info, newHash)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config with pending manifest review: %v", err)
	}
	if read.SourceHash != oldHash {
		t.Fatalf("read source hash = %q, want installed hash %q", read.SourceHash, oldHash)
	}
	if strings.Contains(strings.Join(read.Warnings, " "), "pending catalog update requires attention") {
		t.Fatalf("rendered-only manifest-review source leaked into config warnings: %v", read.Warnings)
	}
	configured, err := mgr.ConfigureCustomManifestUpdate(context.Background(), "piclu", nil, true)
	if err != nil {
		t.Fatalf("configure pending rendered-only catalog update: %v", err)
	}
	if !configured.Eligible {
		t.Fatalf("pending rendered-only catalog update not eligible: %s", configured.BlockingReason)
	}
	if _, ok := configured.Inputs["gemini_api_key"]; !ok {
		t.Fatalf("configured inputs missing gemini_api_key: %v", configured.Inputs)
	}
	diagSchema, ok := configured.Inputs["diag_dir"]
	if !ok {
		t.Fatalf("configured inputs missing diag_dir: %v", configured.Inputs)
	}
	if diagSchema.Label != "" || diagSchema.Description != "" || diagSchema.Default != nil || diagSchema.Validation != nil {
		t.Fatalf("rendered manifest configure leaked display metadata: %+v", diagSchema)
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != oldHash {
		t.Fatalf("catalog hash advanced to %q, want old hash %q", appInst.CatalogManifestHash, oldHash)
	}

	mgr.containerManager = NewMockContainerManager()
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: paths.CoreRoot()}}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(paths.CoreRoot()))
	appInst.ActiveRootfs[networkAnchorServiceName] = "rootfs-anchor"
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app rootfs state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:     "piclu",
		CatalogPending: true,
	})
	if err != nil {
		t.Fatalf("dry-run pending rendered-only catalog update: %v", err)
	}
	if !dryRun.Applicable {
		t.Fatalf("pending rendered-only catalog dry run not applicable: %s", dryRun.BlockingReason)
	}
	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		CatalogPending:     true,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
		DryRunToken:        dryRun.DryRunToken,
		Confirmations:      dryRun.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply pending rendered-only catalog update: %v", err)
	}
	if !applied.Applicable {
		t.Fatalf("apply result not applicable")
	}
	st, err = state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state after apply: %v", err)
	}
	if st.RawTemplateHash != newHash {
		t.Fatalf("committed raw hash = %q, want %q", st.RawTemplateHash, newHash)
	}
	if len(st.PendingRawTemplate) != 0 || st.PendingRawTemplateHash != "" {
		t.Fatalf("pending rendered-only source was not cleared after apply")
	}
	read, err = mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read committed rendered-only config: %v", err)
	}
	if read.SourceHash != newHash {
		t.Fatalf("read source hash = %q, want %q", read.SourceHash, newHash)
	}
	checkedDiagDir := false
	for i := range read.Fields {
		if read.Fields[i].Name != "diag_dir" {
			continue
		}
		checkedDiagDir = true
		if read.Fields[i].Label != "" || read.Fields[i].Description != "" {
			t.Fatalf("rendered installed config leaked field metadata: %+v", read.Fields[i])
		}
		if read.Fields[i].Schema != nil && (read.Fields[i].Schema.Default != nil || read.Fields[i].Schema.Label != "" || read.Fields[i].Schema.Description != "" || read.Fields[i].Schema.Validation != nil) {
			t.Fatalf("rendered installed config leaked schema metadata: %+v", read.Fields[i].Schema)
		}
		if strings.Contains(strings.Join([]string{read.Fields[i].Label, read.Fields[i].Description, fmt.Sprint(read.Fields[i].Display)}, " "), "opaque123") {
			t.Fatalf("rendered installed config leaked stored secret in field: %+v", read.Fields[i])
		}
	}
	if !checkedDiagDir {
		t.Fatalf("read fields missing diag_dir: %+v", read.Fields)
	}
}

func TestRenderedOnlySchemaRejectsStoredValueInputNames(t *testing.T) {
	mgr, state, raw, inputs, systemCtx := installedConfigTestApp(t)
	unsafeRaw := []byte(`type: user
inputs:
  gemini_api_key:
    type: password
    required: true
{{ if .Inputs.gemini_api_key }}
  leak_{{ printf "%.4s" .Inputs.gemini_api_key }}:
    type: string
    default: hidden
{{ end }}
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      GEMINI_API_KEY: "{{ .Inputs.gemini_api_key }}"
x-piccolo:
  mode: service
`)
	if _, err := ParseAppSchema(unsafeRaw); err == nil {
		t.Fatalf("test template should require render before schema parse")
	}
	inputs["gemini_api_key"] = "opaque123"
	if _, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   unsafeRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil); err != nil {
		t.Fatalf("rendered unsafe template should otherwise render: %v", err)
	}

	committed := NewV2InstallState("piclu", InstallSourceKindCustom, "", unsafeRaw, inputs, systemCtx, nil, false)
	if err := state.StoreInstallState("piclu", committed); err != nil {
		t.Fatalf("store unsafe rendered install state: %v", err)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.LedgerHealth != "unrecoverable" {
		t.Fatalf("ledger health = %q, want unrecoverable", read.LedgerHealth)
	}
	if strings.Contains(fmt.Sprint(read), "opaque123") {
		t.Fatalf("read installed config leaked rendered input name: %+v", read)
	}
	if strings.Contains(fmt.Sprint(read), "opaq") {
		t.Fatalf("read installed config leaked rendered input-name fragment: %+v", read)
	}

	pending := NewV2InstallState("piclu", InstallSourceKindCustom, "", raw, inputs, systemCtx, nil, false)
	pending.markPendingCatalogSourceForFlow("piclu", unsafeRaw, "operator review", pendingCatalogReviewFlowManifest)
	if err := state.StoreInstallState("piclu", pending); err != nil {
		t.Fatalf("store unsafe pending install state: %v", err)
	}
	_, err = mgr.ConfigureCustomManifestUpdate(context.Background(), "piclu", nil, true)
	if err == nil {
		t.Fatalf("expected unsafe pending manifest configure to fail")
	}
	if strings.Contains(err.Error(), "opaque123") {
		t.Fatalf("configure error leaked rendered input name: %v", err)
	}
	if strings.Contains(err.Error(), "opaq") {
		t.Fatalf("configure error leaked rendered input-name fragment: %v", err)
	}
}

func TestRenderedOnlySchemaRejectsStickySensitiveInputNameFragments(t *testing.T) {
	mgr, state, _, _, systemCtx := installedConfigTestApp(t)
	raw := []byte(`type: user
inputs:
  license:
    type: string
    required: true
{{ if .Inputs.license }}
  leak_{{ printf "%.4s" .Inputs.license }}:
    type: string
    default: hidden
{{ end }}
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      LICENSE: "{{ .Inputs.license }}"
x-piccolo:
  mode: service
`)
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"license":         "secret-license",
	}
	committed := NewV2InstallState("piclu", InstallSourceKindCustom, "", raw, inputs, systemCtx, nil, false)
	committed.InputSensitive = map[string]bool{"license": true}
	if err := state.StoreInstallState("piclu", committed); err != nil {
		t.Fatalf("store sticky-sensitive rendered install state: %v", err)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.LedgerHealth != "unrecoverable" {
		t.Fatalf("ledger health = %q warnings=%v, want unrecoverable", read.LedgerHealth, read.Warnings)
	}
	if strings.Contains(fmt.Sprint(read), "secret-license") || strings.Contains(fmt.Sprint(read), "secr") {
		t.Fatalf("read installed config leaked sticky-sensitive rendered fragment: %+v", read)
	}
}

func TestRenderedOnlySchemaAllowsNonSecretValueStaticInputNames(t *testing.T) {
	mgr, state, _, inputs, systemCtx := installedConfigTestApp(t)
	raw := []byte(`type: user
inputs:
  gemini_api_key:
    type: password
    required: true
  provider:
    type: string
{{ if .Inputs.provider }}
  local_model:
    type: string
    default: gemma
{{ end }}
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      GEMINI_API_KEY: "{{ .Inputs.gemini_api_key }}"
x-piccolo:
  mode: service
`)
	if _, err := ParseAppSchema(raw); err == nil {
		t.Fatalf("test template should require render before schema parse")
	}
	inputs["provider"] = "local"
	committed := NewV2InstallState("piclu", InstallSourceKindCustom, "", raw, inputs, systemCtx, nil, false)
	if err := state.StoreInstallState("piclu", committed); err != nil {
		t.Fatalf("store rendered install state: %v", err)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.LedgerHealth != "complete" {
		t.Fatalf("ledger health = %q warnings=%v, want complete", read.LedgerHealth, read.Warnings)
	}
	found := false
	for _, field := range read.Fields {
		if field.Name == "local_model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("read fields missing local_model: %+v", read.Fields)
	}
}

func TestCatalogSyncFailsClosedOnMalformedInstallState(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	oldHash := Sha256Hex(raw)
	newRaw := []byte(strings.Replace(string(raw), `      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"`, `      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"
      SYNC_MARKER: pending`, 1))
	newHash := Sha256Hex(newRaw)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": newRaw}})
	if err := os.WriteFile(state.installStatePath("piclu"), []byte("{not-json"), 0600); err != nil {
		t.Fatalf("corrupt install state: %v", err)
	}

	err := mgr.SyncManifest(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected malformed install state sync failure")
	}
	if !strings.Contains(err.Error(), "install_state.json malformed") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != oldHash {
		t.Fatalf("catalog hash advanced to %q, want %q", appInst.CatalogManifestHash, oldHash)
	}
	if appInst.LastSyncAttemptHash != newHash {
		t.Fatalf("attempt hash = %q, want %q", appInst.LastSyncAttemptHash, newHash)
	}
	if appInst.LastSyncError == "" {
		t.Fatalf("expected sync failure to be recorded")
	}
	restoredDef, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get app definition: %v", err)
	}
	if _, ok := restoredDef.Services["main"].Environment["SYNC_MARKER"]; ok {
		t.Fatalf("candidate manifest was applied despite malformed ledger")
	}
}

func TestCatalogSyncRenderInputsSeedSparseDefaults(t *testing.T) {
	raw := []byte(`type: user
inputs:
  diag_dir:
    type: string
    default: /diagnostics
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/default-only:stable
    bind_ports: [8080]
    environment:
      DIAG_DIR: "{{ .Inputs.diag_dir }}"
x-piccolo:
  mode: service
`)
	inputs, ok := catalogSyncRenderInputsForRawTemplate("defaultapp", raw, map[string]any{})
	if !ok {
		t.Fatalf("expected render input derivation to succeed")
	}
	if got := inputs["diag_dir"]; got != "/diagnostics" {
		t.Fatalf("diag_dir render input = %#v, want /diagnostics", got)
	}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate: raw,
		UserInputs:  inputs,
		SystemContext: InstallSystemContext{
			Domain:       "local",
			Architecture: "amd64",
			Timezone:     "Etc/UTC",
		},
		InstanceID: "defaultapp",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render install pipeline: %v", err)
	}
	if got := res.Definition.Services["main"].Environment["DIAG_DIR"]; got != "/diagnostics" {
		t.Fatalf("rendered DIAG_DIR = %q, want /diagnostics", got)
	}
	persistedInputs, persistedProvenance := persistedInstalledConfigLedger(inputs, map[string]string{
		"diag_dir":        InputProvenanceCatalogDefault,
		"__app_address__": InputProvenanceSystem,
	})
	if len(persistedInputs) != 1 || persistedInputs["__app_address__"] != "defaultapp" {
		t.Fatalf("catalog defaults should stay sparse, inputs=%v provenance=%v", persistedInputs, persistedProvenance)
	}
	if len(persistedProvenance) != 1 || persistedProvenance["__app_address__"] != InputProvenanceSystem {
		t.Fatalf("system provenance not preserved: %v", persistedProvenance)
	}
}

func TestMarkCatalogSourceCommittedDropsInputsRemovedFromSchema(t *testing.T) {
	st := &InstallState{
		InstanceID: "piclu",
		InstallInputs: map[string]any{
			"kept":       "visible",
			"old_secret": "hidden-stale",
		},
		InputProvenance: map[string]string{
			"kept":       InputProvenanceOperator,
			"old_secret": InputProvenanceOperator,
		},
		InstallSystemCtx: &InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	}
	raw := []byte(`type: user
inputs:
  kept:
    type: string
    required: true
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      KEPT: "{{ .Inputs.kept }}"
x-piccolo:
  mode: service
`)

	st.markCatalogSourceCommitted("piclu", "piclu", raw)
	if got := st.InstallInputs["kept"]; got != "visible" {
		t.Fatalf("kept input = %v, want visible", got)
	}
	if _, ok := st.InstallInputs["old_secret"]; ok {
		t.Fatalf("removed input old_secret still persisted: %#v", st.InstallInputs)
	}
	if _, ok := st.InputProvenance["old_secret"]; ok {
		t.Fatalf("removed input old_secret provenance still persisted: %#v", st.InputProvenance)
	}
}

func TestFilterInstallStateInputsBlocksRemovedInputRender(t *testing.T) {
	raw := []byte(`type: user
inputs:
  kept:
    type: string
    required: true
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      KEPT: "{{ .Inputs.kept }}"
      HIDDEN: "{{ .Inputs.old_secret }}"
x-piccolo:
  mode: service
`)
	filtered, _, ok := filterInstallStateInputsForRawTemplate("piclu", raw, map[string]any{
		"kept":       "visible",
		"old_secret": "hidden-stale",
	}, map[string]string{
		"kept":       InputProvenanceOperator,
		"old_secret": InputProvenanceOperator,
	})
	if !ok {
		t.Fatalf("expected raw template schema to be parseable")
	}
	if _, ok := filtered["old_secret"]; ok {
		t.Fatalf("removed input old_secret survived filtering: %#v", filtered)
	}
	_, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    filtered,
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
		InstanceID:    "piclu",
	}, nil, nil)
	if err == nil {
		t.Fatalf("expected render to fail instead of using removed hidden input")
	}
	if !strings.Contains(err.Error(), "old_secret") {
		t.Fatalf("unexpected render error: %v", err)
	}
}

func TestDryRunInstalledConfigRejectsStorageChangingInput(t *testing.T) {
	mgr, _, raw, _, _ := installedConfigTestApp(t)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	result, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		Inputs: map[string]interface{}{
			"diag_dir": "/elsewhere",
		},
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if result.Applicable {
		t.Fatalf("expected storage-changing config to be rejected")
	}
	if !strings.Contains(strings.ToLower(result.BlockingReason), "unsupported") {
		t.Fatalf("unexpected blocking reason: %q", result.BlockingReason)
	}
}

func TestDryRunInstalledConfigAllowsUnchangedOIDCClient(t *testing.T) {
	mgr, _, raw := installedConfigOIDCTestApp(t)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"oidcapp": raw}})
	read, err := mgr.ReadInstalledConfig(context.Background(), "oidcapp")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	result, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "oidcapp", InstalledConfigUpdateRequest{
		Inputs: map[string]interface{}{
			"display_name": "Updated OIDC app",
		},
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.Applicable {
		t.Fatalf("expected unchanged oidc_client to be allowed, reason=%q summary=%+v", result.BlockingReason, result.Summary)
	}
}

func TestDryRunInstalledConfigRejectsOIDCClientChanges(t *testing.T) {
	_, state, _ := installedConfigOIDCTestApp(t)
	appInst, ok := state.GetApp("oidcapp")
	if !ok {
		t.Fatalf("oidc app not stored")
	}
	oldDef, err := state.GetAppDefinition("oidcapp")
	if err != nil {
		t.Fatalf("get app definition: %v", err)
	}
	newDef := cloneDefinitionForCompare(oldDef)
	svc := newDef.Services["main"]
	svc.OIDCClient.RedirectURIPaths = append(svc.OIDCClient.RedirectURIPaths, "/changed-callback")
	newDef.Services["main"] = svc

	policy, summary := evaluateInstalledConfigPolicy(oldDef, newDef, appInst)
	if policy.Allowed {
		t.Fatalf("expected oidc_client change to be rejected, summary=%+v", summary)
	}
	if !strings.Contains(policy.Reason, "oidc_client changed") {
		t.Fatalf("unexpected rejection reason: %q", policy.Reason)
	}
}

func TestEvaluateInstalledConfigPolicyRejectsUDPOnlyRuntimeUpdate(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	oldDef.Listeners[0].Flow = api.FlowUDP
	oldDef.Listeners[0].Protocol = api.ListenerProtocolRaw
	oldDef.Listeners[0].Auth = nil
	newDef := customManifestPolicyClone(t, oldDef)
	svc := newDef.Services["main"]
	svc.Environment["PICLU_MODE"] = "device-v2"
	newDef.Services["main"] = svc
	appInst := &AppInstance{InstanceID: "piclu", Enabled: true, Definition: oldDef}

	policy, summary := evaluateInstalledConfigPolicy(oldDef, newDef, appInst)
	if policy.Allowed {
		t.Fatalf("expected UDP-only runtime config update to be rejected, summary=%+v", summary)
	}
	if !strings.Contains(policy.Reason, "UDP") {
		t.Fatalf("unexpected rejection reason: %q", policy.Reason)
	}
}

func TestApplyInstalledConfigNoopUsesTransactionAndBumpsLedgerRevision(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dryRun.Applicable || !dryRun.MetadataOnly {
		t.Fatalf("expected applicable metadata-only dry run, got applicable=%v metadata=%v reason=%q", dryRun.Applicable, dryRun.MetadataOnly, dryRun.BlockingReason)
	}
	applied, err := mgr.ApplyInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		DryRunToken:        dryRun.DryRunToken,
		CandidateDigest:    dryRun.CandidateDigest,
		LedgerRevision:     dryRun.LedgerRevision,
		SourceHash:         dryRun.SourceHash,
		InputSchemaHash:    dryRun.InputSchemaHash,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.LedgerRevision != read.LedgerRevision+1 {
		t.Fatalf("ledger revision = %d, want %d", applied.LedgerRevision, read.LedgerRevision+1)
	}
	appDir := filepath.Join(state.appsDir, "piclu")
	for _, name := range []string{manifestUpdateTxnFilename, manifestUpdateBackupFilename, installStateBackupFilename} {
		if _, err := os.Stat(filepath.Join(appDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be cleared after config apply, stat err=%v", name, err)
		}
	}
}

func TestApplyInstalledConfigRuntimeChangeSnapshotsPersistentData(t *testing.T) {
	mgr, _, raw, _, _ := installedConfigTestApp(t)
	mgr.containerManager = NewMockContainerManager()
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: paths.CoreRoot()}}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(paths.CoreRoot()))
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		Inputs:          map[string]interface{}{"gemini_api_key": "new-secret"},
		SecretActions:   map[string]string{"gemini_api_key": "replace"},
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dryRun.Applicable || dryRun.MetadataOnly {
		t.Fatalf("expected runtime config dry run, got applicable=%v metadata=%v reason=%q", dryRun.Applicable, dryRun.MetadataOnly, dryRun.BlockingReason)
	}
	if !strings.Contains(strings.Join(dryRun.Summary.WillPreserve, "\n"), "private pre-commit failure snapshot") {
		t.Fatalf("dry-run summary did not mention private snapshot: %+v", dryRun.Summary.WillPreserve)
	}

	applied, err := mgr.ApplyInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		DryRunToken:        dryRun.DryRunToken,
		CandidateDigest:    dryRun.CandidateDigest,
		LedgerRevision:     dryRun.LedgerRevision,
		SourceHash:         dryRun.SourceHash,
		InputSchemaHash:    dryRun.InputSchemaHash,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.AccessRepairPending {
		t.Fatalf("unexpected access repair pending: %s", applied.AccessRepairMessage)
	}
	if len(volumes.snapshots) != 1 {
		t.Fatalf("snapshots = %v, want one precommit snapshot", volumes.snapshots)
	}
	if len(volumes.destroyed) != 1 || volumes.destroyed[0] != volumes.snapshots[0] {
		t.Fatalf("destroyed snapshots = %v, want cleanup of %s", volumes.destroyed, volumes.snapshots[0])
	}
}

func TestApplyInstalledConfigRestoresManifestWhenCandidateStoreFails(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		Inputs:          map[string]interface{}{"gemini_api_key": "new-secret"},
		SecretActions:   map[string]string{"gemini_api_key": "replace"},
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	state.storeAppMetadataHook = func(instanceID string, app *AppInstance) error {
		if instanceID != "piclu" || app.Definition == nil {
			return nil
		}
		if svc, ok := app.Definition.Services["main"]; ok && svc.Environment["GEMINI_API_KEY"] == "new-secret" {
			return os.ErrPermission
		}
		return nil
	}

	_, err = mgr.ApplyInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		DryRunToken:        dryRun.DryRunToken,
		CandidateDigest:    dryRun.CandidateDigest,
		LedgerRevision:     dryRun.LedgerRevision,
		SourceHash:         dryRun.SourceHash,
		InputSchemaHash:    dryRun.InputSchemaHash,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
	})
	if err == nil {
		t.Fatalf("expected apply to fail")
	}
	restored, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get restored manifest: %v", err)
	}
	if got := restored.Services["main"].Environment["GEMINI_API_KEY"]; got != "secret-value" {
		t.Fatalf("manifest env after failed apply = %q, want previous secret-value", got)
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if got := st.InstallInputs["gemini_api_key"]; got != "secret-value" {
		t.Fatalf("ledger input after failed apply = %v, want previous secret-value", got)
	}
	appDir := filepath.Join(state.appsDir, "piclu")
	if _, err := os.Stat(filepath.Join(appDir, manifestUpdateTxnFilename)); !os.IsNotExist(err) {
		t.Fatalf("transaction should be cleared after rollback, stat err=%v", err)
	}
}

func TestApplyInstalledConfigRejectsLedgerRawTemplateHashMismatchAfterDryRun(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": raw}})
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}

	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	st.RawTemplate = append([]byte("# tampered after dry run\n"), st.RawTemplate...)
	if err := state.StoreInstallState("piclu", st); err != nil {
		t.Fatalf("store tampered install state: %v", err)
	}

	_, err = mgr.ApplyInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		DryRunToken:        dryRun.DryRunToken,
		CandidateDigest:    dryRun.CandidateDigest,
		LedgerRevision:     dryRun.LedgerRevision,
		SourceHash:         dryRun.SourceHash,
		InputSchemaHash:    dryRun.InputSchemaHash,
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
	})
	if err == nil {
		t.Fatalf("expected apply to reject tampered ledger")
	}
	if !errors.Is(err, ErrInstalledConfigConflict) || !strings.Contains(strings.ToLower(err.Error()), "config ledger") {
		t.Fatalf("unexpected apply error: %v", err)
	}
}

func TestNormalizeInstalledConfigInputsDropsUndeclaredLedgerValues(t *testing.T) {
	inputs, provenance, _, _, err := normalizeInstalledConfigInputs(
		map[string]api.AppInput{
			"provider": {Type: "string", Required: true},
		},
		&InstallState{
			InstallInputs: map[string]any{
				"provider":   "local",
				"old_secret": "stale-secret",
			},
			InputProvenance: map[string]string{
				"provider":   InputProvenanceOperator,
				"old_secret": InputProvenanceOperator,
			},
		},
		&api.AppDefinition{Inputs: map[string]api.AppInput{"provider": {Type: "string"}}},
		InstalledConfigUpdateRequest{},
		"piclu",
	)
	if err != nil {
		t.Fatalf("normalize inputs: %v", err)
	}
	if _, ok := inputs["old_secret"]; ok {
		t.Fatalf("undeclared ledger value was carried into render inputs")
	}
	if _, ok := provenance["old_secret"]; ok {
		t.Fatalf("undeclared provenance was carried into candidate ledger")
	}
	if got := inputs["provider"]; got != "local" {
		t.Fatalf("provider = %v, want local", got)
	}
}

func TestNewV2InstallStateKeepsUntouchedDefaultsOutOfLedger(t *testing.T) {
	raw := []byte(`type: user
inputs:
  provider:
    type: string
    default: local
  retries:
    type: int
    default: 3
  tags:
    type: array
    default: [alpha, beta]
  session_key:
    type: password
    generate: true
  api_key:
    type: password
    required: true
services:
  main:
    image: docker.io/example/app:stable
x-piccolo:
  mode: service
`)
	st := NewV2InstallState(
		"piclu",
		InstallSourceKindCatalog,
		"piclu",
		raw,
		map[string]any{
			"provider":        "local",
			"retries":         3,
			"tags":            []any{"alpha", "beta"},
			"session_key":     "generated-secret",
			"api_key":         "operator-secret",
			"__app_address__": "piclu",
		},
		InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
		nil,
		false,
	)
	for _, name := range []string{"provider", "retries", "tags"} {
		if _, ok := st.InstallInputs[name]; ok {
			t.Fatalf("%s default should not be persisted in initial ledger: %#v", name, st.InstallInputs)
		}
		if _, ok := st.InputProvenance[name]; ok {
			t.Fatalf("%s catalog-default provenance should not be persisted: %#v", name, st.InputProvenance)
		}
	}
	if got := st.InstallInputs["session_key"]; got != "generated-secret" {
		t.Fatalf("session_key = %v, want generated-secret", got)
	}
	if got := st.InputProvenance["session_key"]; got != InputProvenanceGenerated {
		t.Fatalf("session_key provenance = %q, want %q", got, InputProvenanceGenerated)
	}
	if got := st.InstallInputs["api_key"]; got != "operator-secret" {
		t.Fatalf("api_key = %v, want operator-secret", got)
	}
	if got := st.InputProvenance["api_key"]; got != InputProvenanceOperator {
		t.Fatalf("api_key provenance = %q, want %q", got, InputProvenanceOperator)
	}
	if got := st.InputProvenance["__app_address__"]; got != InputProvenanceSystem {
		t.Fatalf("__app_address__ provenance = %q, want %q", got, InputProvenanceSystem)
	}

	edited := NewV2InstallState(
		"piclu",
		InstallSourceKindCatalog,
		"piclu",
		raw,
		map[string]any{"provider": "remote"},
		InstallSystemContext{},
		nil,
		false,
	)
	if got := edited.InstallInputs["provider"]; got != "remote" {
		t.Fatalf("edited provider = %v, want remote", got)
	}
	if got := edited.InputProvenance["provider"]; got != InputProvenanceOperator {
		t.Fatalf("edited provider provenance = %q, want %q", got, InputProvenanceOperator)
	}
}

func TestNormalizeInstalledConfigInputsKeepsAbsentDefaultsUntilEdited(t *testing.T) {
	declared := map[string]api.AppInput{
		"diag_dir": {Type: "string", Default: "/diagnostics"},
		"enabled":  {Type: "boolean", Default: true},
	}
	st := &InstallState{
		InstallInputs:   map[string]any{},
		InputProvenance: map[string]string{},
	}

	inputs, provenance, _, actions, err := normalizeInstalledConfigInputs(declared, st, &api.AppDefinition{Inputs: declared}, InstalledConfigUpdateRequest{}, "piclu")
	if err != nil {
		t.Fatalf("normalize inputs: %v", err)
	}
	if got := inputs["diag_dir"]; got != "/diagnostics" {
		t.Fatalf("diag_dir = %v, want /diagnostics", got)
	}
	if got := inputs["enabled"]; got != true {
		t.Fatalf("enabled = %v, want true", got)
	}
	if got := provenance["diag_dir"]; got != InputProvenanceCatalogDefault {
		t.Fatalf("diag_dir provenance = %q, want %q", got, InputProvenanceCatalogDefault)
	}
	if len(actions) != 0 {
		t.Fatalf("absent defaults should not be reported as operator actions, got %+v", actions)
	}

	inputs, provenance, _, actions, err = normalizeInstalledConfigInputs(
		declared,
		st,
		&api.AppDefinition{Inputs: declared},
		InstalledConfigUpdateRequest{Inputs: map[string]any{"diag_dir": ""}},
		"piclu",
	)
	if err != nil {
		t.Fatalf("normalize edited input: %v", err)
	}
	if got := inputs["diag_dir"]; got != "" {
		t.Fatalf("edited diag_dir = %v, want empty operator value", got)
	}
	if got := provenance["diag_dir"]; got != InputProvenanceOperator {
		t.Fatalf("edited diag_dir provenance = %q, want %q", got, InputProvenanceOperator)
	}
	if len(actions) != 1 || actions[0].Field != "diag_dir" || actions[0].Action != "replace" {
		t.Fatalf("edited default should produce one replace action, got %+v", actions)
	}
}

func TestNormalizeInstalledConfigInputsUsesRequiredDefaults(t *testing.T) {
	declared := map[string]api.AppInput{
		"region": {Type: "string", Required: true, Default: "local"},
		"limit":  {Type: "int", Required: true, Default: 3},
	}
	st := &InstallState{
		InstallInputs:   map[string]any{},
		InputProvenance: map[string]string{},
	}

	inputs, provenance, _, actions, err := normalizeInstalledConfigInputs(declared, st, &api.AppDefinition{Inputs: declared}, InstalledConfigUpdateRequest{}, "piclu")
	if err != nil {
		t.Fatalf("normalize required defaults: %v", err)
	}
	if got := inputs["region"]; got != "local" {
		t.Fatalf("region = %v, want local", got)
	}
	if got := inputs["limit"]; got != float64(3) {
		t.Fatalf("limit = %T(%v), want float64(3)", got, got)
	}
	if got := provenance["region"]; got != InputProvenanceCatalogDefault {
		t.Fatalf("region provenance = %q, want catalog default", got)
	}
	if len(actions) != 0 {
		t.Fatalf("required defaults should not report operator actions: %+v", actions)
	}
}

func TestInstalledConfigKeepsReclassifiedStoredSecretWriteOnly(t *testing.T) {
	previous := &api.AppDefinition{Inputs: map[string]api.AppInput{
		"license": {Type: "password", Required: true},
	}}
	declared := map[string]api.AppInput{
		"license": {Type: "string", Required: true},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"license": "secret-license",
		},
		InputProvenance: map[string]string{
			"license": InputProvenanceOperator,
		},
	}

	fields := installedConfigFields(declared, st, previous, false)
	if len(fields) != 1 {
		t.Fatalf("fields len = %d, want 1", len(fields))
	}
	if !fields[0].Sensitive || fields[0].Display != nil || fields[0].Schema != nil {
		t.Fatalf("reclassified stored secret should remain write-only: %+v", fields[0])
	}
	if !slices.Contains(fields[0].Actions, "keep") || !slices.Contains(fields[0].Actions, "replace") {
		t.Fatalf("reclassified stored secret actions = %+v, want keep/replace", fields[0].Actions)
	}

	inputs, _, inputSensitive, actions, err := normalizeInstalledConfigInputs(declared, st, previous, InstalledConfigUpdateRequest{
		Inputs:        map[string]any{"license": "new-license"},
		SecretActions: map[string]string{"license": "replace"},
	}, "piclu")
	if err != nil {
		t.Fatalf("replace reclassified secret: %v", err)
	}
	if got := inputs["license"]; got != "new-license" {
		t.Fatalf("license = %v, want replacement", got)
	}
	if inputSensitive["license"] {
		t.Fatalf("replacement under plain schema should clear sticky sensitivity")
	}
	if len(actions) != 1 || !actions[0].Sensitive || actions[0].OldDisplay != nil || actions[0].NewDisplay != nil {
		t.Fatalf("replace action leaked display values: %+v", actions)
	}
	if strings.Contains(fmt.Sprint(actions), "secret-license") || strings.Contains(fmt.Sprint(actions), "new-license") {
		t.Fatalf("replace action leaked secret values: %+v", actions)
	}
}

func TestInstalledConfigRejectsIncompatibleKeptSecretWithoutEcho(t *testing.T) {
	previous := &api.AppDefinition{Inputs: map[string]api.AppInput{
		"license": {Type: "password", Required: true},
	}}
	declared := map[string]api.AppInput{
		"license": {Type: "boolean", Required: true},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"license": "secret-license",
		},
		InputProvenance: map[string]string{
			"license": InputProvenanceOperator,
		},
		InputSensitive: map[string]bool{
			"license": true,
		},
	}

	_, _, _, _, err := normalizeInstalledConfigInputs(declared, st, previous, InstalledConfigUpdateRequest{
		SecretActions: map[string]string{"license": "keep"},
	}, "piclu")
	if err == nil {
		t.Fatalf("expected incompatible kept secret to fail")
	}
	if strings.Contains(err.Error(), "secret-license") || strings.Contains(err.Error(), "expected boolean") {
		t.Fatalf("error leaked kept secret or validator detail: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be safely reused") {
		t.Fatalf("error = %v, want generic safe-reuse failure", err)
	}
}

func TestDryRunInstalledConfigRejectsStickySensitiveStructuralKeys(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	pendingRaw := bytes.Replace(raw,
		[]byte(`      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"`),
		[]byte(`      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"
      '{{ printf "%.4s" .Inputs.gemini_api_key }}_KEY': "1"
      '{{ printf "%x" .Inputs.gemini_api_key }}_HEX': "1"
      '{{ printf "%q" .Inputs.gemini_api_key }}_QUOTE': "1"`),
		1,
	)
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	st.InstallInputs["gemini_api_key"] = "zzzz-token"
	st.InputSensitive["gemini_api_key"] = true
	if !st.markPendingCatalogSourceForFlow("piclu", pendingRaw, "pending config review", pendingCatalogReviewFlowConfig) {
		t.Fatalf("mark pending catalog source returned false")
	}
	if err := state.StoreInstallState("piclu", st); err != nil {
		t.Fatalf("store pending install state: %v", err)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run config: %v", err)
	}
	if dryRun.Applicable || dryRun.DryRunToken != "" {
		t.Fatalf("dry run should reject sensitive structural keys without token, got applicable=%v token=%q result=%+v", dryRun.Applicable, dryRun.DryRunToken, dryRun)
	}
	if !strings.Contains(dryRun.BlockingReason, "sensitive or generated values cannot be used in manifest structure") {
		t.Fatalf("blocking reason = %q", dryRun.BlockingReason)
	}
	joined := fmt.Sprint(dryRun)
	for _, leaked := range []string{"zzzz-token", "zzzz_KEY", "zzzz", schemaInputNameSentinelPrefix, fmt.Sprintf("%x", "zzzz-token"), fmt.Sprintf("%q", "zzzz-token")} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("config dry-run result leaked %q: %+v", leaked, dryRun)
		}
	}
}

func TestDryRunInstalledConfigRedactsSensitiveRenderValidationError(t *testing.T) {
	mgr, state, raw, _, _ := installedConfigTestApp(t)
	pendingRaw := bytes.Replace(raw,
		[]byte(`primary_service: main
services:
  main:`),
		[]byte(`primary_service: "{{ .Inputs.gemini_api_key }}"
services:
  "{{ .Inputs.gemini_api_key }}":`),
		1,
	)
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	st.InstallInputs["gemini_api_key"] = "Bad.Secret"
	st.InputSensitive["gemini_api_key"] = true
	if !st.markPendingCatalogSourceForFlow("piclu", pendingRaw, "pending config review", pendingCatalogReviewFlowConfig) {
		t.Fatalf("mark pending catalog source returned false")
	}
	if err := state.StoreInstallState("piclu", st); err != nil {
		t.Fatalf("store pending install state: %v", err)
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "piclu", InstalledConfigUpdateRequest{
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run config returned raw error: %v", err)
	}
	if dryRun.Applicable || dryRun.DryRunToken != "" {
		t.Fatalf("dry run should reject unsafe render failure without token, got applicable=%v token=%q result=%+v", dryRun.Applicable, dryRun.DryRunToken, dryRun)
	}
	if !strings.Contains(dryRun.BlockingReason, "sensitive or generated values cannot be used in manifest structure") {
		t.Fatalf("blocking reason = %q", dryRun.BlockingReason)
	}
	if strings.Contains(fmt.Sprint(dryRun), "Bad.Secret") {
		t.Fatalf("config dry-run result leaked rendered secret: %+v", dryRun)
	}
}

func TestInstalledConfigTreatsBlankRequiredStoredSecretAsMissing(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key": {Type: "password", Required: true},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"api_key": "",
		},
		InputProvenance: map[string]string{
			"api_key": InputProvenanceOperator,
		},
		InputSensitive: map[string]bool{
			"api_key": true,
		},
	}
	fields := installedConfigFields(declared, st, &api.AppDefinition{Inputs: declared}, false)
	if len(fields) != 1 {
		t.Fatalf("fields len = %d, want 1", len(fields))
	}
	if fields[0].Present || !fields[0].Sensitive || !slices.Equal(fields[0].Actions, []string{"replace"}) {
		t.Fatalf("blank stored secret should require replacement: %+v", fields[0])
	}

	_, _, _, _, err := normalizeInstalledConfigInputs(declared, st, &api.AppDefinition{Inputs: declared}, InstalledConfigUpdateRequest{
		SecretActions: map[string]string{"api_key": "keep"},
	}, "piclu")
	if err == nil {
		t.Fatalf("expected blank kept secret to fail")
	}
	if strings.Contains(err.Error(), "expected") || strings.Contains(err.Error(), "\"\"") {
		t.Fatalf("error should be generic, got %v", err)
	}
}

func TestNormalizeInstalledConfigInputsNormalizesNumericDefaults(t *testing.T) {
	declared := map[string]api.AppInput{
		"retry_count": {Type: "int", Default: 3},
		"ratio":       {Type: "number", Default: 2},
	}
	st := &InstallState{
		InstallInputs:   map[string]any{},
		InputProvenance: map[string]string{},
	}

	inputs, _, _, _, err := normalizeInstalledConfigInputs(declared, st, &api.AppDefinition{Inputs: declared}, InstalledConfigUpdateRequest{}, "piclu")
	if err != nil {
		t.Fatalf("normalize inputs: %v", err)
	}
	if got := inputs["retry_count"]; got != float64(3) {
		t.Fatalf("retry_count = %#v (%T), want float64(3)", got, got)
	}
	if got := inputs["ratio"]; got != float64(2) {
		t.Fatalf("ratio = %#v (%T), want float64(2)", got, got)
	}
}

func TestPersistedInstalledConfigLedgerDropsCatalogDefaults(t *testing.T) {
	inputs := map[string]any{
		"__app_address__": "piclu",
		"provider":        "local",
		"enabled":         true,
		"gemini_api_key":  "secret-value",
	}
	provenance := map[string]string{
		"__app_address__": InputProvenanceSystem,
		"provider":        InputProvenanceCatalogDefault,
		"enabled":         InputProvenanceCatalogDefault,
		"gemini_api_key":  InputProvenanceOperator,
	}

	persistedInputs, persistedProvenance := persistedInstalledConfigLedger(inputs, provenance)
	if _, ok := persistedInputs["provider"]; ok {
		t.Fatalf("catalog default provider should not be persisted")
	}
	if _, ok := persistedInputs["enabled"]; ok {
		t.Fatalf("catalog default enabled should not be persisted")
	}
	if got := persistedInputs["__app_address__"]; got != "piclu" {
		t.Fatalf("__app_address__ = %v, want piclu", got)
	}
	if got := persistedInputs["gemini_api_key"]; got != "secret-value" {
		t.Fatalf("gemini_api_key = %v, want secret-value", got)
	}
	if _, ok := persistedProvenance["provider"]; ok {
		t.Fatalf("catalog default provenance should not be persisted")
	}
	if got := persistedProvenance["gemini_api_key"]; got != InputProvenanceOperator {
		t.Fatalf("operator provenance = %q, want %q", got, InputProvenanceOperator)
	}
}

func TestNormalizeInstalledConfigInputsKeepsAbsentOptionalSecretsByDefault(t *testing.T) {
	declared := map[string]api.AppInput{
		"webhook_token": {Type: "string", Default: "default-token"},
	}
	st := &InstallState{
		InstallInputs:   map[string]any{},
		InputProvenance: map[string]string{},
	}

	inputs, provenance, _, actions, err := normalizeInstalledConfigInputs(declared, st, &api.AppDefinition{Inputs: declared}, InstalledConfigUpdateRequest{}, "piclu")
	if err != nil {
		t.Fatalf("normalize inputs: %v", err)
	}
	if _, ok := inputs["webhook_token"]; ok {
		t.Fatalf("absent optional secret should be kept by omission, got input value %v", inputs["webhook_token"])
	}
	if _, ok := provenance["webhook_token"]; ok {
		t.Fatalf("absent optional secret should not gain operator/default provenance")
	}
	if len(actions) != 0 {
		t.Fatalf("absent optional secret keep should not emit actions, got %+v", actions)
	}
}

func TestInstalledConfigFieldsShowAbsentOptionalSecretAsUnset(t *testing.T) {
	declared := map[string]api.AppInput{
		"webhook_token": {Type: "string", Default: "default-token"},
	}
	fields := installedConfigFields(
		declared,
		&InstallState{
			InstallInputs:   map[string]any{},
			InputProvenance: map[string]string{},
		},
		&api.AppDefinition{Inputs: declared},
		false,
	)
	if len(fields) != 1 {
		t.Fatalf("fields len = %d, want 1", len(fields))
	}
	field := fields[0]
	if field.Present || !field.Sensitive {
		t.Fatalf("field = %+v, want absent sensitive", field)
	}
	if field.Provenance != "absent_sensitive" {
		t.Fatalf("provenance = %q, want absent_sensitive", field.Provenance)
	}
	if !slices.Equal(field.Actions, []string{"keep", "replace"}) {
		t.Fatalf("actions = %+v, want keep/replace", field.Actions)
	}

	fields = installedConfigFields(
		declared,
		&InstallState{
			InstallInputs:   map[string]any{"webhook_token": ""},
			InputProvenance: map[string]string{"webhook_token": InputProvenanceOperator},
			InputSensitive:  map[string]bool{"webhook_token": true},
		},
		&api.AppDefinition{Inputs: declared},
		false,
	)
	if len(fields) != 1 {
		t.Fatalf("cleared fields len = %d, want 1", len(fields))
	}
	field = fields[0]
	if field.Present || !field.Sensitive {
		t.Fatalf("cleared field = %+v, want absent sensitive", field)
	}
	if field.Provenance != "absent_sensitive" {
		t.Fatalf("cleared provenance = %q, want absent_sensitive", field.Provenance)
	}
	if !slices.Equal(field.Actions, []string{"keep", "replace"}) {
		t.Fatalf("cleared actions = %+v, want keep/replace", field.Actions)
	}
}

func TestNormalizeInstalledConfigInputsRejectsBlankSecretReplacement(t *testing.T) {
	declared := map[string]api.AppInput{
		"webhook_token": {Type: "string"},
	}
	st := &InstallState{
		InstallInputs:   map[string]any{},
		InputProvenance: map[string]string{},
	}

	_, _, _, _, err := normalizeInstalledConfigInputs(
		declared,
		st,
		&api.AppDefinition{Inputs: declared},
		InstalledConfigUpdateRequest{
			Inputs:        map[string]any{"webhook_token": ""},
			SecretActions: map[string]string{"webhook_token": "replace"},
		},
		"piclu",
	)
	if err == nil {
		t.Fatalf("expected blank secret replacement to fail")
	}
	if !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("unexpected error: %v", err)
	}

	inputs, provenance, _, actions, err := normalizeInstalledConfigInputs(
		declared,
		st,
		&api.AppDefinition{Inputs: declared},
		InstalledConfigUpdateRequest{
			SecretActions: map[string]string{"webhook_token": "clear"},
		},
		"piclu",
	)
	if err != nil {
		t.Fatalf("clear optional secret: %v", err)
	}
	if got := inputs["webhook_token"]; got != "" {
		t.Fatalf("cleared webhook_token = %v, want empty string", got)
	}
	if got := provenance["webhook_token"]; got != InputProvenanceOperator {
		t.Fatalf("clear provenance = %q, want %q", got, InputProvenanceOperator)
	}
	if len(actions) != 1 || actions[0].Action != "clear" {
		t.Fatalf("clear should emit one clear action, got %+v", actions)
	}
}

func TestInstalledConfigFieldsShowsEffectiveDefaultForAbsentNonSensitiveInput(t *testing.T) {
	fields := installedConfigFields(
		map[string]api.AppInput{
			"diag_dir": {Type: "string", Default: "/diagnostics"},
		},
		&InstallState{
			InstallInputs:   map[string]any{},
			InputProvenance: map[string]string{},
		},
		&api.AppDefinition{Inputs: map[string]api.AppInput{"diag_dir": {Type: "string", Default: "/diagnostics"}}},
		false,
	)
	if len(fields) != 1 {
		t.Fatalf("fields len = %d, want 1", len(fields))
	}
	field := fields[0]
	if field.Present {
		t.Fatalf("field should remain absent from ledger")
	}
	if got := field.Display; got != "/diagnostics" {
		t.Fatalf("display = %v, want /diagnostics", got)
	}
	if got := field.Provenance; got != InputProvenanceCatalogDefault {
		t.Fatalf("provenance = %q, want %q", got, InputProvenanceCatalogDefault)
	}
}

func TestInstalledConfigLedgerAllowsEmptyInputMaps(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	raw := []byte(`type: user
inputs:
  provider:
    type: string
    default: local
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/default-only:stable
    bind_ports: [8080]
    environment:
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    map[string]any{},
		SystemContext: systemCtx,
		InstanceID:    "defaultsonly",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render install pipeline: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:          "defaultsonly",
		Enabled:             true,
		PrimaryService:      "main",
		Containers:          map[string]string{"main": "cid-main"},
		ActiveRootfs:        map[string]string{"main": "rootfs-main"},
		CatalogSource:       "defaultsonly",
		CatalogManifestHash: Sha256Hex(raw),
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          res.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("defaultsonly", NewV2InstallState("defaultsonly", InstallSourceKindCatalog, "defaultsonly", raw, map[string]any{}, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}
	st, err := state.LoadInstallState("defaultsonly")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.InstallInputs == nil {
		t.Fatalf("empty install input map should normalize to non-nil")
	}
	if !st.isV2Complete() {
		t.Fatalf("default-only ledger should be complete")
	}
	read, err := mgr.ReadInstalledConfig(context.Background(), "defaultsonly")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if read.LedgerHealth != "complete" || len(read.Fields) == 0 {
		t.Fatalf("unexpected read result health=%s fields=%d warnings=%v", read.LedgerHealth, len(read.Fields), read.Warnings)
	}
	var provider *InstalledConfigField
	for i := range read.Fields {
		if read.Fields[i].Name == "provider" {
			provider = &read.Fields[i]
			break
		}
	}
	if provider == nil {
		t.Fatalf("provider field not found in %+v", read.Fields)
	}
	if provider.Present || provider.Display != "local" {
		t.Fatalf("provider field = %+v, want absent displayed default", *provider)
	}
	dryRun, err := mgr.DryRunInstalledConfigUpdate(context.Background(), "defaultsonly", InstalledConfigUpdateRequest{
		LedgerRevision:  read.LedgerRevision,
		SourceHash:      read.SourceHash,
		InputSchemaHash: read.InputSchemaHash,
	})
	if err != nil {
		t.Fatalf("dry run installed config: %v", err)
	}
	if !dryRun.Applicable || len(dryRun.Actions) != 0 {
		t.Fatalf("default-only dry run applicable=%v actions=%+v reason=%q", dryRun.Applicable, dryRun.Actions, dryRun.BlockingReason)
	}
}

func TestCatalogSyncNoopKeepsCatalogHashBehindLedgerCommit(t *testing.T) {
	mgr, state, oldRaw, _, _ := installedConfigTestApp(t)
	oldHash := Sha256Hex(oldRaw)
	newRaw := append([]byte("# render-equivalent catalog refresh\n"), oldRaw...)
	newHash := Sha256Hex(newRaw)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": newRaw}})
	state.storeInstallStateHook = func(instanceID string, st *InstallState) error {
		if instanceID == "piclu" && st.RawTemplateHash == newHash {
			return errors.New("injected install_state write failure")
		}
		return nil
	}

	err := mgr.SyncManifest(context.Background(), "piclu")
	if err == nil {
		t.Fatalf("expected sync failure")
	}
	if !strings.Contains(err.Error(), "persist config ledger source") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != oldHash {
		t.Fatalf("catalog hash advanced to %q, want old hash %q", appInst.CatalogManifestHash, oldHash)
	}
	if appInst.LastSyncAttemptHash != newHash {
		t.Fatalf("attempt hash = %q, want %q", appInst.LastSyncAttemptHash, newHash)
	}
	if appInst.LastSyncError == "" {
		t.Fatalf("expected sync failure to be recorded")
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != oldHash {
		t.Fatalf("ledger raw hash advanced to %q, want old hash %q", st.RawTemplateHash, oldHash)
	}
}

func TestCatalogSyncRealApplyRollsBackWhenLedgerCommitFails(t *testing.T) {
	mgr, state, oldRaw := installedConfigOIDCTestApp(t)
	oldHash := Sha256Hex(oldRaw)
	newRaw := []byte(strings.Replace(string(oldRaw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))
	newHash := Sha256Hex(newRaw)
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"oidcapp": newRaw}})
	state.storeInstallStateHook = func(instanceID string, st *InstallState) error {
		if instanceID == "oidcapp" && st.RawTemplateHash == newHash {
			return errors.New("injected install_state write failure")
		}
		return nil
	}

	err := mgr.SyncManifest(context.Background(), "oidcapp")
	if err == nil {
		t.Fatalf("expected sync failure")
	}
	if !strings.Contains(err.Error(), "persist config ledger") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != oldHash {
		t.Fatalf("catalog hash advanced to %q, want old hash %q", appInst.CatalogManifestHash, oldHash)
	}
	if appInst.LastSyncAttemptHash != newHash {
		t.Fatalf("attempt hash = %q, want %q", appInst.LastSyncAttemptHash, newHash)
	}
	if appInst.LastSyncError == "" {
		t.Fatalf("expected sync failure to be recorded")
	}
	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != oldHash {
		t.Fatalf("ledger raw hash advanced to %q, want old hash %q", st.RawTemplateHash, oldHash)
	}
	restoredDef, err := state.GetAppDefinition("oidcapp")
	if err != nil {
		t.Fatalf("get restored definition: %v", err)
	}
	if got := restoredDef.Services["main"].OIDCClient.AuthorizePaths; len(got) != 0 {
		t.Fatalf("candidate oidc authorize paths were not rolled back: %v", got)
	}
	if _, err := state.LoadManifestUpdateTransaction("oidcapp"); err == nil {
		t.Fatalf("expected transaction to be cleared after rollback")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load manifest transaction: %v", err)
	}
}

func TestCatalogSyncRevertsProxyOIDCDeltaWhenLedgerCommitFails(t *testing.T) {
	mgr, state, oldRaw := installedConfigOIDCTestApp(t)
	newRaw := []byte(strings.Replace(string(oldRaw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))
	newHash := Sha256Hex(newRaw)
	registered := []string{}
	deleted := []string{}
	mgr.SetSyncHost(installedConfigSyncHost{
		templates: map[string][]byte{"oidcapp": newRaw},
		requiresProxy: func(def *api.AppDefinition) bool {
			if def == nil {
				return false
			}
			for _, svc := range def.Services {
				if svc.OIDCClient != nil && len(svc.OIDCClient.AuthorizePaths) > 0 {
					return true
				}
			}
			return false
		},
		registeredProxy: &registered,
		deletedProxy:    &deleted,
	})
	state.storeInstallStateHook = func(instanceID string, st *InstallState) error {
		if instanceID == "oidcapp" && st.RawTemplateHash == newHash {
			return errors.New("injected install_state write failure")
		}
		return nil
	}

	err := mgr.SyncManifest(context.Background(), "oidcapp")
	if err == nil {
		t.Fatalf("expected sync failure")
	}
	if len(registered) != 1 || registered[0] != "oidcapp" {
		t.Fatalf("proxy registration calls = %v, want [oidcapp]", registered)
	}
	if len(deleted) != 1 || deleted[0] != "oidcapp" {
		t.Fatalf("proxy deletion rollback calls = %v, want [oidcapp]", deleted)
	}
}

func TestCatalogSyncPostCommitPublishFailureKeepsCatalogMetadataCurrent(t *testing.T) {
	mgr, state, oldRaw := installedConfigOIDCTestApp(t)
	oldHash := Sha256Hex(oldRaw)
	newRaw := []byte(strings.Replace(string(oldRaw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))
	newHash := Sha256Hex(newRaw)
	requiresProxy := func(def *api.AppDefinition) bool {
		if def == nil {
			return false
		}
		for _, svc := range def.Services {
			if svc.OIDCClient != nil && len(svc.OIDCClient.AuthorizePaths) > 0 {
				return true
			}
		}
		return false
	}
	registered := []string{}
	mgr.SetSyncHost(installedConfigSyncHost{
		templates:       map[string][]byte{"oidcapp": newRaw},
		requiresProxy:   requiresProxy,
		registeredProxy: &registered,
		registerErr:     errors.New("proxy client registry unavailable"),
		registerErrCall: 2,
	})

	err := mgr.SyncManifest(context.Background(), "oidcapp")
	if err == nil {
		t.Fatalf("expected post-commit publication failure")
	}
	if !strings.Contains(err.Error(), "proxy client registry unavailable") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	if len(registered) != 2 {
		t.Fatalf("proxy registration calls before failure = %v, want precommit + publish attempts", registered)
	}
	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != newHash {
		t.Fatalf("ledger raw hash = %q, want committed hash %q", st.RawTemplateHash, newHash)
	}
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("app not found")
	}
	if appInst.CatalogManifestHash != newHash {
		t.Fatalf("catalog hash = %q, want committed hash %q (old %q)", appInst.CatalogManifestHash, newHash, oldHash)
	}
	if appInst.LastSyncError != "" {
		t.Fatalf("last sync error = %q, want clear after ledger commit", appInst.LastSyncError)
	}
	txn, err := state.LoadManifestUpdateTransaction("oidcapp")
	if err != nil {
		t.Fatalf("load manifest update transaction: %v", err)
	}
	if txn.Phase != "publishing_access" {
		t.Fatalf("transaction phase = %q, want publishing_access", txn.Phase)
	}

	mgr.SetSyncHost(installedConfigSyncHost{
		templates:       map[string][]byte{"oidcapp": newRaw},
		requiresProxy:   requiresProxy,
		registeredProxy: &registered,
	})
	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["oidcapp"] {
		t.Fatalf("recovery should repair access without blocking")
	}
	if _, err := state.LoadManifestUpdateTransaction("oidcapp"); err == nil {
		t.Fatalf("transaction should be cleared after access repair")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
	if len(registered) != 3 {
		t.Fatalf("proxy registration calls after recovery = %v, want recovery retry", registered)
	}
	appInst, exists = state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("app not found after recovery")
	}
	if appInst.CatalogManifestHash != newHash {
		t.Fatalf("catalog hash after recovery = %q, want %q", appInst.CatalogManifestHash, newHash)
	}
	if appInst.LastSyncError != "" {
		t.Fatalf("last sync error after recovery = %q, want clear", appInst.LastSyncError)
	}
}

func TestCatalogSyncPostCommitMetadataFailureRecovered(t *testing.T) {
	mgr, state, oldRaw := installedConfigOIDCTestApp(t)
	newRaw := []byte(strings.Replace(string(oldRaw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))
	newHash := Sha256Hex(newRaw)
	requiresProxy := func(def *api.AppDefinition) bool {
		if def == nil {
			return false
		}
		for _, svc := range def.Services {
			if svc.OIDCClient != nil && len(svc.OIDCClient.AuthorizePaths) > 0 {
				return true
			}
		}
		return false
	}
	registered := []string{}
	mgr.SetSyncHost(installedConfigSyncHost{
		templates:       map[string][]byte{"oidcapp": newRaw},
		requiresProxy:   requiresProxy,
		registeredProxy: &registered,
	})
	failMetadata := true
	state.storeAppMetadataHook = func(instanceID string, app *AppInstance) error {
		if failMetadata && instanceID == "oidcapp" && app.CatalogManifestHash == newHash && app.LastSyncError == "" {
			return errors.New("metadata store unavailable")
		}
		return nil
	}

	err := mgr.SyncManifest(context.Background(), "oidcapp")
	if err != nil {
		t.Fatalf("sync should publish access and leave metadata retry pending, got: %v", err)
	}
	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != newHash {
		t.Fatalf("ledger raw hash = %q, want committed hash %q", st.RawTemplateHash, newHash)
	}
	storedDef, err := state.GetAppDefinition("oidcapp")
	if err != nil {
		t.Fatalf("get committed definition: %v", err)
	}
	if got := storedDef.Services["main"].OIDCClient.AuthorizePaths; len(got) == 0 {
		t.Fatalf("candidate manifest was not committed before metadata recovery")
	}
	txn, err := state.LoadManifestUpdateTransaction("oidcapp")
	if err != nil {
		t.Fatalf("transaction should remain for recovery: %v", err)
	}
	if txn.Phase != "committed_metadata_pending" || !txn.AccessPublished || !strings.Contains(txn.LastError, "metadata store unavailable") {
		t.Fatalf("metadata retry transaction = phase:%q access:%v err:%q", txn.Phase, txn.AccessPublished, txn.LastError)
	}

	failMetadata = false
	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["oidcapp"] {
		t.Fatalf("recovery should retry metadata and access without blocking")
	}
	if _, err := state.LoadManifestUpdateTransaction("oidcapp"); err == nil {
		t.Fatalf("transaction should be cleared after metadata recovery")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
	st, err = state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load recovered install state: %v", err)
	}
	if st.RawTemplateHash != newHash {
		t.Fatalf("recovered ledger raw hash = %q, want committed hash %q", st.RawTemplateHash, newHash)
	}
	storedDef, err = state.GetAppDefinition("oidcapp")
	if err != nil {
		t.Fatalf("get recovered definition: %v", err)
	}
	if got := storedDef.Services["main"].OIDCClient.AuthorizePaths; len(got) == 0 {
		t.Fatalf("recovery restored previous manifest; authorize paths = %v", got)
	}
	fresh, err := NewFilesystemStateManager(state.stateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	appInst, exists := fresh.GetApp("oidcapp")
	if !exists {
		t.Fatalf("app not found after reload")
	}
	if appInst.CatalogManifestHash != newHash {
		t.Fatalf("catalog hash after recovery = %q, want %q", appInst.CatalogManifestHash, newHash)
	}
	if appInst.LastSyncError != "" {
		t.Fatalf("last sync error after recovery = %q, want clear", appInst.LastSyncError)
	}
}

func TestCatalogSyncStructuralApplySnapshotsPersistentData(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: tempDir}}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(tempDir))
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	oldRaw := []byte(`type: user
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      PICLU_MODE: device
    storage:
      persistent:
        data:
          container: /data
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      PICLU_MODE: device
    storage:
      persistent:
        data:
          container: /data
        diagnostics:
          container: /diagnostics
          shared: true
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    map[string]interface{}{},
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:          "piclu",
		Enabled:             true,
		PrimaryService:      "main",
		Containers:          map[string]string{"main": "cid-main"},
		ActiveRootfs:        map[string]string{"main": "rootfs-main"},
		CatalogSource:       "piclu",
		CatalogManifestHash: Sha256Hex(oldRaw),
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          res.Definition,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCatalog, "piclu", oldRaw, map[string]interface{}{}, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}
	mgr.SetSyncHost(installedConfigSyncHost{templates: map[string][]byte{"piclu": newRaw}})

	if err := mgr.SyncManifest(context.Background(), "piclu"); err != nil {
		t.Fatalf("sync manifest: %v", err)
	}
	if len(volumes.snapshots) != 1 {
		t.Fatalf("snapshots = %v, want one precommit snapshot", volumes.snapshots)
	}
	if len(volumes.destroyed) != 1 || volumes.destroyed[0] != volumes.snapshots[0] {
		t.Fatalf("destroyed snapshots = %v, want cleanup of %s", volumes.destroyed, volumes.snapshots[0])
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != Sha256Hex(newRaw) {
		t.Fatalf("ledger hash = %q, want %q", st.RawTemplateHash, Sha256Hex(newRaw))
	}
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app missing after sync")
	}
	if appInst.CatalogManifestHash != Sha256Hex(newRaw) {
		t.Fatalf("catalog hash = %q, want %q", appInst.CatalogManifestHash, Sha256Hex(newRaw))
	}
}

func TestRecoverPendingManifestUpdateRepairsAccessAfterPostCommitPublishFailure(t *testing.T) {
	mgr, state, oldRaw := installedConfigOIDCTestApp(t)
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("oidc app missing")
	}
	newRaw := []byte(strings.Replace(string(oldRaw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))
	oldDef := appInst.Definition
	oldHash, err := canonicalManifestHash(oldDef)
	if err != nil {
		t.Fatalf("old hash: %v", err)
	}
	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.InstallSystemCtx == nil {
		t.Fatalf("install state missing system context")
	}
	rendered, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   newRaw,
		UserInputs:    st.InstallInputs,
		SystemContext: *st.InstallSystemCtx,
		InstanceID:    "oidcapp",
		ExistingOIDC:  st.OIDCCredentials,
	}, nil, nil)
	if err != nil {
		t.Fatalf("render candidate: %v", err)
	}
	candidateHash, err := canonicalManifestHash(rendered.Definition)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate("oidcapp")
	if err != nil {
		t.Fatalf("backup manifest: %v", err)
	}
	nextState := NewV2InstallState("oidcapp", InstallSourceKindCustom, "", newRaw, st.InstallInputs, *st.InstallSystemCtx, st.OIDCCredentials, false)
	nextState.Revision = st.Revision + 1
	appInst.Definition = rendered.Definition
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store candidate app: %v", err)
	}
	if err := state.StoreInstallState("oidcapp", nextState); err != nil {
		t.Fatalf("store candidate install state: %v", err)
	}
	registered := []string{}
	requiresProxy := func(def *api.AppDefinition) bool {
		if def == nil {
			return false
		}
		for _, svc := range def.Services {
			if svc.OIDCClient != nil && len(svc.OIDCClient.AuthorizePaths) > 0 {
				return true
			}
		}
		return false
	}
	mgr.SetSyncHost(installedConfigSyncHost{
		requiresProxy:   requiresProxy,
		registeredProxy: &registered,
	})
	failMetadata := true
	state.storeAppMetadataHook = func(instanceID string, app *AppInstance) error {
		if failMetadata && instanceID == "oidcapp" && app.CatalogManifestHash == nextState.RawTemplateHash && app.LastSyncError == "" {
			return errors.New("metadata store unavailable during recovery")
		}
		return nil
	}
	now := time.Now().UTC()
	if err := state.StoreManifestUpdateTransaction("oidcapp", &ManifestUpdateTransaction{
		OperationID:               "op-access-repair",
		OperationKind:             "service_app_update",
		Phase:                     "publishing_access",
		PreviousManifestHash:      oldHash,
		CandidateManifestHash:     candidateHash,
		PreviousLedgerRevision:    st.Revision,
		CandidateLedgerRevision:   nextState.Revision,
		PreviousLedgerSourceHash:  st.RawTemplateHash,
		CandidateLedgerSourceHash: nextState.RawTemplateHash,
		DryRunToken:               "token",
		RuntimeFingerprint:        "fingerprint",
		BackupPath:                backupPath,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		LastError:                 "register proxy oidc client: proxy client registry unavailable",
	}); err != nil {
		t.Fatalf("store publishing transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["oidcapp"] {
		t.Fatalf("recovery should repair access without blocking")
	}
	txn, err := state.LoadManifestUpdateTransaction("oidcapp")
	if err != nil {
		t.Fatalf("transaction should remain for metadata retry: %v", err)
	}
	if txn.Phase != "committed_metadata_pending" || !txn.AccessPublished || !strings.Contains(txn.LastError, "metadata store unavailable during recovery") {
		t.Fatalf("metadata retry transaction = phase:%q access:%v err:%q", txn.Phase, txn.AccessPublished, txn.LastError)
	}
	if len(registered) != 1 || registered[0] != "oidcapp" {
		t.Fatalf("proxy registration calls = %v, want recovery retry", registered)
	}
	committedDef, err := state.GetAppDefinition("oidcapp")
	if err != nil {
		t.Fatalf("get committed definition: %v", err)
	}
	if got := committedDef.Services["main"].OIDCClient.AuthorizePaths; !slices.Equal(got, []string{"/authorize"}) {
		t.Fatalf("committed authorize paths = %v", got)
	}
	failMetadata = false
	blocked = mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["oidcapp"] {
		t.Fatalf("second recovery should clear metadata retry without blocking")
	}
	if _, err := state.LoadManifestUpdateTransaction("oidcapp"); err == nil {
		t.Fatalf("transaction should be cleared after metadata recovery")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
	if len(registered) != 1 {
		t.Fatalf("proxy registration calls after metadata retry = %v, want no duplicate access repair", registered)
	}
}

func TestRecoverPendingManifestUpdateRevertsProxyOIDCDelta(t *testing.T) {
	mgr, state, oldRaw := installedConfigOIDCTestApp(t)
	newRaw := []byte(strings.Replace(string(oldRaw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))
	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   newRaw,
		UserInputs:    st.InstallInputs,
		SystemContext: *st.InstallSystemCtx,
		InstanceID:    "oidcapp",
		ExistingOIDC:  st.OIDCCredentials,
	}, nil, nil)
	if err != nil {
		t.Fatalf("render candidate: %v", err)
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate("oidcapp")
	if err != nil {
		t.Fatalf("backup manifest: %v", err)
	}
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("app not found")
	}
	appInst.Definition = res.Definition
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store candidate app: %v", err)
	}
	deleted := []string{}
	mgr.SetSyncHost(installedConfigSyncHost{
		requiresProxy: func(def *api.AppDefinition) bool {
			if def == nil {
				return false
			}
			for _, svc := range def.Services {
				if svc.OIDCClient != nil && len(svc.OIDCClient.AuthorizePaths) > 0 {
					return true
				}
			}
			return false
		},
		deletedProxy: &deleted,
	})
	now := time.Now().UTC()
	if err := state.StoreManifestUpdateTransaction("oidcapp", &ManifestUpdateTransaction{
		OperationID:           "op",
		OperationKind:         "catalog_sync",
		Phase:                 "candidate_persisted",
		BackupPath:            backupPath,
		ProxyOIDCDeltaApplied: true,
		CreatedAt:             now,
		UpdatedAt:             now,
		DryRunToken:           "token",
		PreviousManifestHash:  Sha256Hex(oldRaw),
		CandidateManifestHash: Sha256Hex(res.CanonicalBytes),
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["oidcapp"] {
		t.Fatalf("oidcapp should recover successfully")
	}
	if len(deleted) != 1 || deleted[0] != "oidcapp" {
		t.Fatalf("proxy deletion rollback calls = %v, want [oidcapp]", deleted)
	}
	if _, err := state.LoadManifestUpdateTransaction("oidcapp"); err == nil {
		t.Fatalf("expected transaction to be cleared")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
}

func installedConfigTestApp(t *testing.T) (*AppManager, *FilesystemStateManager, []byte, map[string]interface{}, InstallSystemContext) {
	t.Helper()
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.ForceLockState(false)
	mgr.SetMountVerifier(func(string) error { return nil })
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	raw := []byte(`type: user
inputs:
  gemini_api_key:
    type: password
    label: Gemini API key
    required: true
  diag_dir:
    type: string
    label: Diagnostics directory
    default: /diagnostics
  webhook_token:
    type: string
    label: Webhook token
    default: default-token
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      GEMINI_API_KEY: "{{ .Inputs.gemini_api_key }}"
      PICLU_DEVICE_DIAG_DIR: "{{ .Inputs.diag_dir }}"
    storage:
      persistent:
        data:
          container: /data
        diagnostics:
          container: "{{ .Inputs.diag_dir }}"
          shared: true
x-piccolo:
  mode: service
`)
	inputs := map[string]interface{}{
		"gemini_api_key": "secret-value",
		"diag_dir":       "/diagnostics",
	}
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render install pipeline: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:          "piclu",
		Enabled:             true,
		PrimaryService:      "main",
		Containers:          map[string]string{"main": "cid-main"},
		ActiveRootfs:        map[string]string{"main": "rootfs-main"},
		CatalogSource:       "piclu",
		CatalogManifestHash: Sha256Hex(raw),
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          res.Definition,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCatalog, "piclu", raw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}
	return mgr, state, raw, inputs, systemCtx
}

func installedConfigOIDCTestApp(t *testing.T) (*AppManager, *FilesystemStateManager, []byte) {
	t.Helper()
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.ForceLockState(false)
	mgr.SetMountVerifier(func(string) error { return nil })
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	raw := []byte(`type: user
inputs:
  display_name:
    type: string
    label: Display name
    default: OIDC app
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public
primary_service: main
services:
  main:
    image: docker.io/example/oidc:stable
    bind_ports: [8080]
    environment:
      DISPLAY_NAME: "{{ .Inputs.display_name }}"
      CLIENT_ID: "{{ .System.Auth.ClientID }}"
    oidc_client:
      redirect_uri_paths:
        - /callback
      ca_mount_path: /etc/ssl/certs/piccolo-internal-ca.crt
      env:
        ISSUER_URL: "{{ .System.Auth.Issuer }}"
        CLIENT_ID: "{{ .System.Auth.ClientID }}"
        CLIENT_SECRET: "{{ .System.Auth.ClientSecret }}"
        PICCOLO_CA_PATH: /etc/ssl/certs/piccolo-internal-ca.crt
x-piccolo:
  mode: service
`)
	inputs := map[string]interface{}{
		"display_name": "OIDC app",
	}
	systemCtx := InstallSystemContext{
		Domain:       "local",
		Architecture: "amd64",
		Timezone:     "Etc/UTC",
		IssuerHint:   "https://issuer.local",
	}
	creds := &OIDCCredentials{ClientID: "client-id", ClientSecret: "client-secret"}
	res, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   raw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "oidcapp",
		ExistingOIDC:  creds,
	}, nil, nil)
	if err != nil {
		t.Fatalf("render install pipeline: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:          "oidcapp",
		Enabled:             true,
		PrimaryService:      "main",
		Containers:          map[string]string{"main": "cid-main"},
		ActiveRootfs:        map[string]string{"main": "rootfs-main"},
		CatalogSource:       "oidcapp",
		CatalogManifestHash: Sha256Hex(raw),
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          res.Definition,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("oidcapp", NewV2InstallState("oidcapp", InstallSourceKindCatalog, "oidcapp", raw, inputs, systemCtx, creds, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}
	return mgr, state, raw
}
