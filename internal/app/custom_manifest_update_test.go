package app

import (
	"context"
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
