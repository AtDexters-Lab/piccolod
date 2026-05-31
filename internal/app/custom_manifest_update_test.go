package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/state/paths"
)

func TestEvaluateCustomManifestUpdatePolicy_AllowsEnvAndAdditiveStorage(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	newDef := customManifestPolicyClone(t, oldDef)
	svc := newDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	svc.Storage.Persistent["diagnostics"] = api.AppVolume{
		Container: "/diagnostics",
		Shared:    true,
	}
	newDef.Services["main"] = svc

	policy, summary := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if !policy.Allowed {
		t.Fatalf("policy rejected allowed env/storage update: %s", policy.Reason)
	}
	if policy.MetadataOnly {
		t.Fatalf("env/storage update must not be metadata-only")
	}
	if len(summary.WillRestart) == 0 {
		t.Fatalf("expected restart summary for structural update")
	}
	joined := strings.Join(summary.WillChange, "\n")
	if !strings.Contains(joined, "PICLU_DEVICE_DIAG_DIR") {
		t.Fatalf("expected env key in summary, got %q", joined)
	}
	if strings.Contains(joined, "/diagnostics") {
		t.Fatalf("summary must not leak env/storage values, got %q", joined)
	}
}

func TestEvaluateCustomManifestUpdatePolicy_AllowsInputMetadataOnly(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	newDef := customManifestPolicyClone(t, oldDef)
	newDef.Inputs = map[string]api.AppInput{
		"transcribe_provider": {
			Type:        "string",
			Label:       "Transcribe Provider",
			Description: "Provider choice",
			Default:     "local",
		},
	}

	policy, summary := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if !policy.Allowed {
		t.Fatalf("policy rejected input metadata-only change: %s", policy.Reason)
	}
	if !policy.MetadataOnly {
		t.Fatalf("expected metadata-only=true")
	}
	if len(summary.WillRestart) != 0 {
		t.Fatalf("metadata-only update must not restart, got %v", summary.WillRestart)
	}
}

func TestEvaluateCustomManifestUpdatePolicy_RejectsOutOfScopeDeltas(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*api.AppDefinition)
		want   string
	}{
		{
			name: "image",
			mutate: func(def *api.AppDefinition) {
				svc := def.Services["main"]
				svc.Image = "docker.io/example/piclu:new"
				def.Services["main"] = svc
			},
			want: "image",
		},
		{
			name: "listener auth",
			mutate: func(def *api.AppDefinition) {
				def.Listeners[0].Auth = &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "protected"}}}
			},
			want: "listener",
		},
		{
			name: "added service",
			mutate: func(def *api.AppDefinition) {
				def.Services["worker"] = api.AppService{Image: "docker.io/library/alpine:3.18", BindPorts: []int{}}
			},
			want: "service",
		},
		{
			name: "storage mount change",
			mutate: func(def *api.AppDefinition) {
				svc := def.Services["main"]
				vol := svc.Storage.Persistent["data"]
				vol.Container = "/var/lib/piclu"
				svc.Storage.Persistent["data"] = vol
				def.Services["main"] = svc
			},
			want: "volume",
		},
		{
			name: "oidc",
			mutate: func(def *api.AppDefinition) {
				svc := def.Services["main"]
				svc.OIDCClient = &api.ServiceOIDCClient{
					RedirectURIPaths: []string{"/callback"},
					CAMountPath:      "/ca",
					Env:              map[string]string{"CLIENT_ID": "CLIENT_ID"},
				}
				def.Services["main"] = svc
			},
			want: "oidc_client",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldDef := customManifestPolicyBaseDef()
			newDef := customManifestPolicyClone(t, oldDef)
			tc.mutate(newDef)
			policy, _ := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
			if policy.Allowed {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(policy.Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", policy.Reason, tc.want)
			}
		})
	}
}

func TestNormalizeManifestUpdateInputs_GeneratedValuesAreExplicitAndAppAddressPinned(t *testing.T) {
	declared := map[string]api.AppInput{
		"__app_address__": {Type: "string", Required: true},
		"api_key":         {Type: "password", Required: true, Generate: true},
		"provider":        {Type: "string", Default: "local"},
	}
	if _, err := normalizeManifestUpdateInputs(declared, map[string]interface{}{}, nil, "piclu"); err == nil {
		t.Fatalf("expected missing generated input to fail")
	}

	inputs, err := normalizeManifestUpdateInputs(declared, map[string]interface{}{"__app_address__": "other"}, []string{"api_key"}, "piclu")
	if err != nil {
		t.Fatalf("normalize with regenerate: %v", err)
	}
	if got := inputs["__app_address__"]; got != "piclu" {
		t.Fatalf("__app_address__ = %v, want pinned piclu", got)
	}
	if got := strings.TrimSpace(inputs["api_key"].(string)); got == "" {
		t.Fatalf("regenerated api_key is empty")
	}
	if got := inputs["provider"]; got != "local" {
		t.Fatalf("provider = %v, want default", got)
	}
}

func TestDryRunCustomManifestUpdate_MaterializesCandidateAndToken(t *testing.T) {
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
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID: "piclu",
		RawTemplate: []byte(`type: user
inputs:
  diag_dir:
    type: string
    label: Diagnostics directory
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
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      PICLU_MODE: device
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
`),
		Inputs: map[string]interface{}{
			"__app_address__": "other",
			"diag_dir":        "/diagnostics",
		},
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.Applicable {
		t.Fatalf("dry run rejected: %s", result.BlockingReason)
	}
	if result.DryRunToken == "" {
		t.Fatalf("expected dry-run token")
	}
	if result.RenderedAppID != "piclu" {
		t.Fatalf("rendered app id = %q, want piclu", result.RenderedAppID)
	}
	if _, err := os.Stat(filepath.Join(tempDir, AppsDir, "piclu", "install_state.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run must not create install_state.json, stat err=%v", err)
	}
}

func TestApplyCustomManifestUpdate_MetadataOnlyDoesNotUseAppPrev(t *testing.T) {
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
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	previousLedger := NewV2InstallState(
		"piclu",
		InstallSourceKindCustom,
		"",
		[]byte("previous raw template"),
		map[string]interface{}{"__app_address__": "piclu"},
		systemCtx,
		nil,
		false,
	)
	previousLedger.Revision = 7
	if err := state.StoreInstallState("piclu", previousLedger); err != nil {
		t.Fatalf("store previous install state: %v", err)
	}

	raw := []byte(`type: user
inputs:
  transcribe_provider:
    type: string
    label: Transcribe provider
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
	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   raw,
		Inputs:        map[string]interface{}{"transcribe_provider": "local"},
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dryRun.MetadataOnly {
		t.Fatalf("expected metadata-only dry run")
	}
	if _, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
		DryRunToken:        dryRun.DryRunToken,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	updated, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get updated def: %v", err)
	}
	if _, ok := updated.Inputs["transcribe_provider"]; !ok {
		t.Fatalf("metadata-only apply did not persist input schema")
	}
	nextLedger, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load updated install state: %v", err)
	}
	if nextLedger.Revision != 8 {
		t.Fatalf("ledger revision = %d, want 8", nextLedger.Revision)
	}
	if nextLedger.RawTemplateHash != Sha256Hex(raw) {
		t.Fatalf("ledger raw hash = %q, want %q", nextLedger.RawTemplateHash, Sha256Hex(raw))
	}
	appDir := filepath.Join(tempDir, AppsDir, "piclu")
	if _, err := os.Stat(filepath.Join(appDir, "app.prev.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest update must not write app.prev.yaml, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, manifestUpdateTxnFilename)); !os.IsNotExist(err) {
		t.Fatalf("transaction should be cleared after commit, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, manifestUpdateBackupFilename)); !os.IsNotExist(err) {
		t.Fatalf("transient backup should be cleared after commit, stat err=%v", err)
	}
}

func TestApplyCustomManifestUpdateConflictsOnStaleConfigLedger(t *testing.T) {
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
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	previousRaw := []byte("previous raw template")
	previousLedger := NewV2InstallState(
		"piclu",
		InstallSourceKindCustom,
		"",
		previousRaw,
		map[string]interface{}{"__app_address__": "piclu"},
		systemCtx,
		nil,
		false,
	)
	previousLedger.Revision = 7
	if err := state.StoreInstallState("piclu", previousLedger); err != nil {
		t.Fatalf("store previous install state: %v", err)
	}

	raw := []byte(`type: user
inputs:
  transcribe_provider:
    type: string
    label: Transcribe provider
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
	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   raw,
		Inputs:        map[string]interface{}{"transcribe_provider": "local"},
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dryRun.MetadataOnly {
		t.Fatalf("expected metadata-only dry run")
	}

	interveningLedger := NewV2InstallState(
		"piclu",
		InstallSourceKindCustom,
		"",
		previousRaw,
		map[string]interface{}{"__app_address__": "piclu"},
		systemCtx,
		nil,
		false,
	)
	interveningLedger.Revision = 8
	if err := state.StoreInstallState("piclu", interveningLedger); err != nil {
		t.Fatalf("store intervening install state: %v", err)
	}

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
		DryRunToken:        dryRun.DryRunToken,
	})
	if !errors.Is(err, ErrManifestUpdateConflict) {
		t.Fatalf("apply err = %v, want %v", err, ErrManifestUpdateConflict)
	}
	currentLedger, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load current install state: %v", err)
	}
	if currentLedger.Revision != 8 || currentLedger.RawTemplateHash != Sha256Hex(previousRaw) {
		t.Fatalf("stale apply changed ledger: revision=%d hash=%q", currentLedger.Revision, currentLedger.RawTemplateHash)
	}
	currentDef, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get app definition: %v", err)
	}
	if _, ok := currentDef.Inputs["transcribe_provider"]; ok {
		t.Fatalf("stale apply changed manifest schema")
	}
}

func TestApplyCustomManifestUpdateRequiresCreatedLedgerMarkerBeforeInstallState(t *testing.T) {
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
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	raw := []byte(`type: user
inputs:
  transcribe_provider:
    type: string
    label: Transcribe provider
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
	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   raw,
		Inputs:        map[string]interface{}{"transcribe_provider": "local"},
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dryRun.MetadataOnly {
		t.Fatalf("expected metadata-only dry run")
	}
	state.storeManifestUpdateTransactionHook = func(instanceID string, txn *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && txn.Phase == "ledger_committing" && txn.CreatedInstallState {
			return os.ErrPermission
		}
		return nil
	}

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
		DryRunToken:        dryRun.DryRunToken,
	})
	if err == nil {
		t.Fatalf("expected apply to fail")
	}
	if _, err := state.LoadInstallState("piclu"); !errors.Is(err, ErrInstallStateNotFound) {
		t.Fatalf("install state after failed apply err = %v, want %v", err, ErrInstallStateNotFound)
	}
	restored, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get restored manifest: %v", err)
	}
	if _, ok := restored.Inputs["transcribe_provider"]; ok {
		t.Fatalf("candidate input schema was not restored")
	}
	appDir := filepath.Join(tempDir, AppsDir, "piclu")
	if _, err := os.Stat(filepath.Join(appDir, manifestUpdateTxnFilename)); !os.IsNotExist(err) {
		t.Fatalf("transaction should be cleared after rollback, stat err=%v", err)
	}
}

func TestRecoverPendingManifestUpdates_BlocksOnlyFailedApp(t *testing.T) {
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
	now := time.Now().UTC()
	for _, name := range []string{"broken", "healthy"} {
		if err := state.StoreApp(&AppInstance{
			InstanceID:     name,
			Enabled:        true,
			PrimaryService: "main",
			Containers:     map[string]string{"main": "cid-main"},
			ActiveRootfs:   map[string]string{"main": "rootfs-main"},
			CreatedAt:      now,
			UpdatedAt:      now,
			Definition:     customManifestPolicyBaseDef(),
		}); err != nil {
			t.Fatalf("store app %s: %v", name, err)
		}
	}
	if err := state.StoreManifestUpdateTransaction("broken", &ManifestUpdateTransaction{
		OperationID: "op",
		Phase:       "candidate_persisted",
		BackupPath:  filepath.Join(tempDir, "missing-backup.yaml"),
		CreatedAt:   now,
		UpdatedAt:   now,
		DryRunToken: "token",
		LastError:   "",
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if !blocked["broken"] {
		t.Fatalf("expected broken app to be blocked")
	}
	if blocked["healthy"] {
		t.Fatalf("healthy app should not be blocked")
	}
	if got := mgr.getObservedStatus("broken"); got != StatusError {
		t.Fatalf("broken observed status = %q, want %q", got, StatusError)
	}
}

func TestRecoverOneManifestUpdateTreatsCommittedAsTerminal(t *testing.T) {
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
	now := time.Now().UTC()
	def := customManifestPolicyBaseDef()
	svc := def.Services["main"]
	svc.Environment["COMMITTED_VALUE"] = "kept"
	def.Services["main"] = svc
	app := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        false,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     def,
	}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store app: %v", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID: "op",
		Phase:       "committed",
		BackupPath:  filepath.Join(tempDir, "missing-backup.yaml"),
		CreatedAt:   now,
		UpdatedAt:   now,
		DryRunToken: "token",
	}
	if err := state.StoreManifestUpdateTransaction("piclu", txn); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	if err := mgr.recoverOneManifestUpdate(context.Background(), state, app, txn); err != nil {
		t.Fatalf("recover committed transaction: %v", err)
	}
	kept, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get app definition: %v", err)
	}
	if got := kept.Services["main"].Environment["COMMITTED_VALUE"]; got != "kept" {
		t.Fatalf("committed manifest was restored unexpectedly, env=%q", got)
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); err == nil {
		t.Fatalf("expected committed transaction to be cleared")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
}

func TestRecoverPendingManifestUpdate_RemovesCreatedInstallState(t *testing.T) {
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

	now := time.Now().UTC()
	prevDef := customManifestPolicyBaseDef()
	app := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        false,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     prevDef,
	}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store previous app: %v", err)
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate("piclu")
	if err != nil {
		t.Fatalf("backup previous manifest: %v", err)
	}

	candidateDef := customManifestPolicyClone(t, prevDef)
	svc := candidateDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	candidateDef.Services["main"] = svc
	app.Definition = candidateDef
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store candidate app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState(
		"piclu",
		InstallSourceKindCustom,
		"",
		[]byte("name: piclu\n"),
		map[string]interface{}{"PICLU_DEVICE_DIAG_DIR": "/diagnostics"},
		InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
		nil,
		false,
	)); err != nil {
		t.Fatalf("store candidate install state: %v", err)
	}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:           "op",
		OperationKind:         "manifest_update",
		Phase:                 "ledger_committing",
		BackupPath:            backupPath,
		CreatedInstallState:   true,
		CreatedAt:             now,
		UpdatedAt:             now,
		DryRunToken:           "token",
		PreviousManifestHash:  "old",
		CandidateManifestHash: "new",
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("piclu should recover successfully")
	}
	if _, err := state.LoadInstallState("piclu"); err != ErrInstallStateNotFound {
		t.Fatalf("install state after recovery err = %v, want %v", err, ErrInstallStateNotFound)
	}
	restored, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get restored app: %v", err)
	}
	if _, ok := restored.Services["main"].Environment["PICLU_DEVICE_DIAG_DIR"]; ok {
		t.Fatalf("candidate manifest was not restored")
	}
}

func TestRecoverPendingManifestUpdate_PreservesReachedLedgerCommit(t *testing.T) {
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

	now := time.Now().UTC()
	prevDef := customManifestPolicyBaseDef()
	app := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        false,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     prevDef,
	}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store previous app: %v", err)
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate("piclu")
	if err != nil {
		t.Fatalf("backup previous manifest: %v", err)
	}

	candidateDef := customManifestPolicyClone(t, prevDef)
	svc := candidateDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	candidateDef.Services["main"] = svc
	app.Definition = candidateDef
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store candidate app: %v", err)
	}
	raw := []byte("name: piclu\n")
	candidateLedger := NewV2InstallState(
		"piclu",
		InstallSourceKindCustom,
		"",
		raw,
		map[string]interface{}{"PICLU_DEVICE_DIAG_DIR": "/diagnostics"},
		InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
		nil,
		false,
	)
	candidateLedger.Revision = 3
	if err := state.StoreInstallState("piclu", candidateLedger); err != nil {
		t.Fatalf("store candidate install state: %v", err)
	}
	candidateHash, err := canonicalManifestHash(candidateDef)
	if err != nil {
		t.Fatalf("hash candidate manifest: %v", err)
	}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:               "op",
		OperationKind:             "config_update",
		Phase:                     "ledger_committing",
		BackupPath:                backupPath,
		CreatedInstallState:       true,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		DryRunToken:               "token",
		PreviousManifestHash:      "old",
		CandidateManifestHash:     candidateHash,
		CandidateLedgerRevision:   candidateLedger.Revision,
		CandidateLedgerSourceHash: candidateLedger.RawTemplateHash,
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("piclu should recover successfully")
	}
	st, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load install state after recovery: %v", err)
	}
	if st.Revision != candidateLedger.Revision || st.RawTemplateHash != candidateLedger.RawTemplateHash {
		t.Fatalf("install state was not preserved: revision=%d hash=%q", st.Revision, st.RawTemplateHash)
	}
	kept, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get app definition: %v", err)
	}
	if got := kept.Services["main"].Environment["PICLU_DEVICE_DIAG_DIR"]; got != "/diagnostics" {
		t.Fatalf("candidate manifest was not preserved, env=%q", got)
	}
	appDir := filepath.Join(tempDir, AppsDir, "piclu")
	if _, err := os.Stat(filepath.Join(appDir, manifestUpdateTxnFilename)); !os.IsNotExist(err) {
		t.Fatalf("transaction should be cleared after preserving commit, stat err=%v", err)
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup should be cleared after preserving commit, stat err=%v", err)
	}
}

func TestManifestTransactionRuntimeTouchedInfersLegacyRuntimePhase(t *testing.T) {
	tests := []struct {
		name string
		txn  *ManifestUpdateTransaction
		want bool
	}{
		{
			name: "new metadata transaction stays metadata only",
			txn:  &ManifestUpdateTransaction{OperationKind: "config_update", Phase: "ledger_committing"},
			want: false,
		},
		{
			name: "new explicit runtime transaction",
			txn:  &ManifestUpdateTransaction{OperationKind: "config_update", Phase: "ledger_committing", RuntimeTouched: true},
			want: true,
		},
		{
			name: "legacy runtime phase",
			txn:  &ManifestUpdateTransaction{Phase: "recreating_runtime"},
			want: true,
		},
		{
			name: "legacy candidate persisted before runtime",
			txn:  &ManifestUpdateTransaction{Phase: "candidate_persisted"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manifestTransactionRuntimeTouched(tt.txn); got != tt.want {
				t.Fatalf("manifestTransactionRuntimeTouched() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMarkManifestTransactionRuntimeTouchedRequiresDurableWrite(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	txn := &ManifestUpdateTransaction{
		OperationID:           "op",
		OperationKind:         "config_update",
		Phase:                 "candidate_persisted",
		PreviousManifestHash:  "old",
		CandidateManifestHash: "new",
		BackupPath:            filepath.Join(tempDir, "backup.yaml"),
		DryRunToken:           "token",
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := state.StoreManifestUpdateTransaction("piclu", txn); err != nil {
		t.Fatalf("store initial transaction: %v", err)
	}
	initialUpdatedAt := txn.UpdatedAt
	state.storeManifestUpdateTransactionHook = func(instanceID string, txn *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && txn.Phase == "recreating_runtime" {
			return os.ErrPermission
		}
		return nil
	}

	if err := markManifestTransactionRuntimeTouched(state, "piclu", txn); err == nil {
		t.Fatalf("expected runtime marker write to fail")
	}
	if txn.Phase != "candidate_persisted" || txn.RuntimeTouched || !txn.UpdatedAt.Equal(initialUpdatedAt) {
		t.Fatalf("in-memory txn was not restored after failed marker write: %+v", txn)
	}
	stored, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load stored transaction: %v", err)
	}
	if stored.Phase != "candidate_persisted" || stored.RuntimeTouched {
		t.Fatalf("durable txn changed after failed marker write: %+v", stored)
	}
}

func customManifestPolicyBaseDef() *api.AppDefinition {
	return &api.AppDefinition{
		Type:           "user",
		PrimaryService: "main",
		Listeners: []api.AppListener{{
			Name:      "piclu",
			GuestPort: 8080,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
			Auth:      &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}}},
		}},
		Services: map[string]api.AppService{
			"main": {
				Image:     "docker.io/example/piclu:stable",
				BindPorts: []int{8080},
				Environment: map[string]string{
					"PICLU_MODE": "device",
				},
				Storage: &api.AppStorage{
					Persistent: map[string]api.AppVolume{
						"data": {Container: "/data"},
					},
				},
			},
		},
		Extensions: map[string]interface{}{"mode": string(ModeService)},
	}
}

func customManifestPolicyClone(t *testing.T, def *api.AppDefinition) *api.AppDefinition {
	t.Helper()
	data, err := SerializeAppDefinition(def)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	clone, err := ParseAppDefinition(data)
	if err != nil {
		t.Fatalf("parse clone: %v", err)
	}
	return clone
}
