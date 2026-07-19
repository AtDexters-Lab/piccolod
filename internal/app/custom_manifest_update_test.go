package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/services"
	"piccolod/internal/state/paths"
)

func TestEvaluateCustomManifestUpdatePolicy_AllowsAdditiveStorage(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	newDef := customManifestPolicyClone(t, oldDef)
	svc := newDef.Services["main"]
	svc.Storage.Persistent["diagnostics"] = api.AppVolume{
		Container: "/diagnostics",
		Shared:    true,
	}
	newDef.Services["main"] = svc

	policy, summary := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if !policy.Allowed {
		t.Fatalf("policy rejected additive storage update: %s", policy.Reason)
	}
	if policy.MetadataOnly {
		t.Fatalf("storage update must not be metadata-only")
	}
	if len(summary.WillRestart) == 0 {
		t.Fatalf("expected restart summary for structural update")
	}
	joined := strings.Join(summary.WillChange, "\n")
	if strings.Contains(joined, "/diagnostics") {
		t.Fatalf("summary must not leak storage values, got %q", joined)
	}
	decision := findManifestDecision(policy.Classification.Decisions, "persistent_storage_added")
	if decision == nil || decision.Outcome != "supported" {
		t.Fatalf("expected supported persistent_storage_added decision, got %+v", decision)
	}
	if policy.Classification.DataSafety == nil || !policy.Classification.DataSafety.SnapshotRequired {
		t.Fatalf("additive storage restart with existing persistent data must require private snapshot, got %+v", policy.Classification.DataSafety)
	}
}

func TestEvaluateCustomManifestUpdatePolicy_EnvWithPersistentStorageRequiresDataImpactReview(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	newDef := customManifestPolicyClone(t, oldDef)
	svc := newDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	newDef.Services["main"] = svc

	policy, _ := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if policy.Allowed {
		t.Fatalf("expected env change with existing persistent storage to require v2 review")
	}
	if policy.UpdateClass != "service_app_update_v2" {
		t.Fatalf("update class = %q, want service_app_update_v2", policy.UpdateClass)
	}
	decision := findManifestDecision(policy.Classification.Decisions, "service_environment_changed")
	if decision == nil || decision.Outcome != "operator_review" {
		t.Fatalf("expected operator_review service_environment_changed decision, got %+v", decision)
	}
	if !slices.Contains(policy.Classification.RequiredConfirmations, "data_impact_review") {
		t.Fatalf("expected data_impact_review confirmation, got %v", policy.Classification.RequiredConfirmations)
	}
	if policy.Classification.DataSafety == nil || !policy.Classification.DataSafety.SnapshotRequired {
		t.Fatalf("expected data safety snapshot requirement, got %+v", policy.Classification.DataSafety)
	}
}

func TestCleanupCommittedManifestUpdateKeepsLegacyJournalWhenTransitionClearFails(t *testing.T) {
	tmp, err := os.MkdirTemp("", "manifest_committed_cleanup_v2_clear")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mgr, err := NewAppManagerForTest(NewMockContainerManager(), tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID: "op-1",
		Phase:       "committed_cleanup_pending",
	}
	if err := state.StoreManifestUpdateTransaction("piclu", txn); err != nil {
		t.Fatalf("store manifest txn: %v", err)
	}
	record := transitionTestRecord("piclu", TransitionPhaseCommittedCleanupPending)
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition record: %v", err)
	}
	state.clearTransitionRecordHook = func(instanceID string) error {
		if instanceID == "piclu" {
			return errors.New("transition clear failed")
		}
		return nil
	}

	err = mgr.cleanupCommittedManifestUpdateTransaction(context.Background(), state, "piclu", txn)
	if err == nil || !strings.Contains(err.Error(), "clear committed v2 transition") {
		t.Fatalf("cleanup err = %v, want transition clear failure", err)
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); err != nil {
		t.Fatalf("manifest update transaction should remain after v2 clear failure: %v", err)
	}
	if _, err := state.LoadTransitionRecord("piclu"); err != nil {
		t.Fatalf("transition record should remain after failed clear: %v", err)
	}
}

func TestEvaluateCustomManifestUpdatePolicyRejectsUDPOnlyRuntimeUpdate(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	oldDef.Listeners[0].Flow = api.FlowUDP
	oldDef.Listeners[0].Protocol = api.ListenerProtocolRaw
	oldDef.Listeners[0].Auth = nil
	newDef := customManifestPolicyClone(t, oldDef)
	svc := newDef.Services["main"]
	svc.Environment["PICLU_MODE"] = "device-v2"
	newDef.Services["main"] = svc

	policy, summary := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if policy.Stageable {
		t.Fatalf("expected UDP-only runtime update to be rejected, summary=%+v", summary)
	}
	if !strings.Contains(policy.Reason, "UDP") {
		t.Fatalf("unexpected rejection reason: %q", policy.Reason)
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

func TestEvaluateCustomManifestUpdatePolicy_ClassifiesOperatorReviewDeltas(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*api.AppDefinition)
		flag    string
		confirm string
	}{
		{
			name: "image",
			mutate: func(def *api.AppDefinition) {
				svc := def.Services["main"]
				svc.Image = "docker.io/example/piclu:new"
				def.Services["main"] = svc
			},
			flag:    "image_refs_changed",
			confirm: "image_update_review",
		},
		{
			name: "listener auth",
			mutate: func(def *api.AppDefinition) {
				def.Listeners[0].Auth = &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "protected"}}}
			},
			flag:    "listener_topology_changed",
			confirm: exposureReviewConfirmationID("listeners.piclu"),
		},
		{
			name: "added service",
			mutate: func(def *api.AppDefinition) {
				def.Services["worker"] = api.AppService{Image: "docker.io/library/alpine:3.18", BindPorts: []int{}, Storage: &api.AppStorage{
					Persistent: map[string]api.AppVolume{"worker-data": {Container: "/worker"}},
				}}
			},
			flag:    "services_added",
			confirm: "service_shape_review",
		},
		{
			name: "removed stateless service",
			mutate: func(def *api.AppDefinition) {
				def.Services["worker"] = api.AppService{Image: "docker.io/library/alpine:3.18", BindPorts: []int{}}
				delete(def.Services, "worker")
			},
			flag:    "services_removed",
			confirm: "service_removal_review",
		},
		{
			name: "temporary storage",
			mutate: func(def *api.AppDefinition) {
				svc := def.Services["main"]
				if svc.Storage == nil {
					svc.Storage = &api.AppStorage{}
				}
				svc.Storage.Temporary = map[string]api.AppVolume{
					"scratch": {Container: "/scratch"},
				}
				def.Services["main"] = svc
			},
			flag:    "temporary_storage_changed",
			confirm: "service_shape_review",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldDef := customManifestPolicyBaseDef()
			if tc.name == "removed stateless service" {
				oldDef.Services["worker"] = api.AppService{Image: "docker.io/library/alpine:3.18", BindPorts: []int{}}
			}
			newDef := customManifestPolicyClone(t, oldDef)
			tc.mutate(newDef)
			policy, _ := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
			if policy.Allowed {
				t.Fatalf("expected operator review to block v1 apply")
			}
			if policy.UpdateClass != "service_app_update_v2" {
				t.Fatalf("update class = %q, want service_app_update_v2", policy.UpdateClass)
			}
			decision := findManifestDecision(policy.Classification.Decisions, tc.flag)
			if decision == nil || decision.Outcome != "operator_review" {
				t.Fatalf("expected operator_review %s decision, got %+v", tc.flag, decision)
			}
			if !slices.Contains(policy.Classification.RequiredConfirmations, tc.confirm) {
				t.Fatalf("expected confirmation %q in %v", tc.confirm, policy.Classification.RequiredConfirmations)
			}
		})
	}
}

func TestEvaluateCustomManifestUpdatePolicy_RejectsUnsupportedDeltas(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*api.AppDefinition)
		flag   string
	}{
		{
			name: "storage mount change",
			mutate: func(def *api.AppDefinition) {
				svc := def.Services["main"]
				vol := svc.Storage.Persistent["data"]
				vol.Container = "/var/lib/piclu"
				svc.Storage.Persistent["data"] = vol
				def.Services["main"] = svc
			},
			flag: "existing_persistent_storage_mutated",
		},
		{
			name: "added service with existing storage attachment",
			mutate: func(def *api.AppDefinition) {
				def.Services["worker"] = api.AppService{
					Image:     "docker.io/library/alpine:3.18",
					BindPorts: []int{},
					Storage: &api.AppStorage{Persistent: map[string]api.AppVolume{
						"data": {Container: "/data"},
					}},
				}
			},
			flag: "services_added",
		},
		{
			name: "removed service with persistent storage",
			mutate: func(def *api.AppDefinition) {
				delete(def.Services, "main")
			},
			flag: "services_removed",
		},
		{
			name: "added service with oidc client",
			mutate: func(def *api.AppDefinition) {
				def.Services["worker"] = api.AppService{
					Image:     "docker.io/library/alpine:3.18",
					BindPorts: []int{},
					OIDCClient: &api.ServiceOIDCClient{
						RedirectURIPaths: []string{"/callback"},
						CAMountPath:      "/ca",
						Env:              map[string]string{"CLIENT_ID": "CLIENT_ID"},
					},
				}
			},
			flag: "services_added",
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
			flag: "oidc_client_changed",
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
			decision := findManifestDecision(policy.Classification.Decisions, tc.flag)
			if decision == nil || decision.Outcome != "rejected" {
				t.Fatalf("expected rejected %s decision, got %+v; reason=%q", tc.flag, decision, policy.Reason)
			}
		})
	}
}

func TestEvaluateCustomManifestUpdatePolicy_RejectsRemovedOIDCService(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	oldDef.Services["worker"] = api.AppService{
		Image:     "docker.io/library/alpine:3.18",
		BindPorts: []int{},
		OIDCClient: &api.ServiceOIDCClient{
			RedirectURIPaths: []string{"/callback"},
			CAMountPath:      "/ca",
			Env:              map[string]string{"CLIENT_ID": "CLIENT_ID"},
		},
	}
	newDef := customManifestPolicyClone(t, oldDef)
	delete(newDef.Services, "worker")

	policy, _ := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if policy.Allowed {
		t.Fatalf("expected removed OIDC service to be rejected")
	}
	decision := findManifestDecision(policy.Classification.Decisions, "services_removed")
	if decision == nil || decision.Outcome != "rejected" {
		t.Fatalf("expected rejected services_removed decision, got %+v; reason=%q", decision, policy.Reason)
	}
}

func TestEvaluateCustomManifestUpdatePolicy_OIDCAuthorizePathsRequireV2Apply(t *testing.T) {
	oldDef := customManifestPolicyBaseDef()
	oldSvc := oldDef.Services["main"]
	oldSvc.OIDCClient = &api.ServiceOIDCClient{
		RedirectURIPaths: []string{"/oidc/callback"},
		CAMountPath:      "/etc/piccolo/ca",
		Env:              map[string]string{"OIDC_CLIENT_ID": "client-id"},
	}
	oldDef.Services["main"] = oldSvc
	newDef := customManifestPolicyClone(t, oldDef)
	newSvc := newDef.Services["main"]
	newSvc.OIDCClient.AuthorizePaths = []string{"/auth/start"}
	newDef.Services["main"] = newSvc

	policy, _ := evaluateCustomManifestUpdatePolicy(oldDef, newDef)
	if policy.Allowed {
		t.Fatalf("proxy OIDC authorize-path changes must wait for v2 apply machinery")
	}
	if policy.UpdateClass != "service_app_update_v2" {
		t.Fatalf("update class = %q, want service_app_update_v2", policy.UpdateClass)
	}
	decision := findManifestDecision(policy.Classification.Decisions, "proxy_oidc_authorize_paths_changed")
	if decision == nil || decision.Outcome != "supported" {
		t.Fatalf("expected supported proxy OIDC authorize-path decision, got %+v", decision)
	}
	confirmation := exposureReviewConfirmationID("services.main.oidc_client.authorize_paths")
	if !slices.Contains(policy.Classification.RequiredConfirmations, confirmation) {
		t.Fatalf("expected %s confirmation, got %v", confirmation, policy.Classification.RequiredConfirmations)
	}
	if len(policy.Classification.ExposureReview) != 1 || policy.Classification.ExposureReview[0].Confirmation != confirmation {
		t.Fatalf("expected exposure review row for OIDC authorize-path delta, got %+v", policy.Classification.ExposureReview)
	}
	if !strings.Contains(policy.Reason, "service app update v2 apply is required") {
		t.Fatalf("reason = %q, want v2 apply requirement", policy.Reason)
	}
}

func TestDryRunCustomManifestUpdate_AllowsOIDCAuthorizePathDeltaWithExistingCredentials(t *testing.T) {
	mgr, state, raw := installedConfigOIDCTestApp(t)
	mgr.containerManager = NewMockContainerManager()
	rootfs := newStubRootfsManager(t.TempDir())
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/oidc:stable",
			BaseImageDigest: "sha256:mockdigest",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:mockdigest",
		},
	}
	mgr.SetRootfsManager(rootfs)
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("oidc app missing")
	}
	appInst.CatalogSource = ""
	appInst.CatalogManifestHash = ""
	appInst.ActiveRootfs[networkAnchorServiceName] = "rootfs-anchor"
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store custom oidc app: %v", err)
	}
	nextRaw := []byte(strings.Replace(string(raw), "      redirect_uri_paths:\n        - /callback", "      authorize_paths:\n        - /authorize\n      redirect_uri_paths:\n        - /callback", 1))

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "oidcapp",
		RawTemplate:   nextRaw,
		Inputs:        map[string]interface{}{"display_name": "OIDC app"},
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC", IssuerHint: "https://issuer.local"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.Applicable {
		t.Fatalf("authorize-path delta should be stageable, reason=%q", result.BlockingReason)
	}
	if result.UpdateClass != "service_app_update_v2" {
		t.Fatalf("update class = %q, want service_app_update_v2", result.UpdateClass)
	}
	decision := findManifestDecision(result.Decisions, "proxy_oidc_authorize_paths_changed")
	if decision == nil || decision.Outcome != "supported" {
		t.Fatalf("expected supported proxy OIDC authorize-path decision, got %+v", decision)
	}
	if got := strings.Join(result.ListenerRoutingAuth, "\n"); !strings.Contains(got, "proxy OIDC routing delta") {
		t.Fatalf("listener routing/auth summary missing proxy OIDC delta: %q", got)
	}
	confirmation := exposureReviewConfirmationID("services.main.oidc_client.authorize_paths")
	if !slices.Contains(result.RequiredConfirmations, confirmation) {
		t.Fatalf("expected %s confirmation, got %v", confirmation, result.RequiredConfirmations)
	}
	if len(result.ExposureReview) != 1 || result.ExposureReview[0].Confirmation != confirmation {
		t.Fatalf("expected exposure review row for OIDC authorize-path delta, got %+v", result.ExposureReview)
	}
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "oidcapp",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		TransitionPlanHash: result.TransitionPlanHash,
		DryRunToken:        result.DryRunToken,
	})
	if !errors.Is(err, ErrManifestUpdateRejected) || !strings.Contains(err.Error(), confirmation) {
		t.Fatalf("apply err = %v, want missing %s confirmation rejection", err, confirmation)
	}
}

func TestApplyCustomManifestUpdateRequiresTransitionPlanHash(t *testing.T) {
	mgr, state, raw := installedConfigOIDCTestApp(t)
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("oidc app missing")
	}
	appInst.CatalogSource = ""
	appInst.CatalogManifestHash = ""
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store custom oidc app: %v", err)
	}
	nextRaw := []byte(strings.Replace(string(raw), "  display_name:\n    type: string", "  operator_note:\n    type: string\n    label: Operator note\n    default: preserved\n  display_name:\n    type: string", 1))
	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "oidcapp",
		RawTemplate:   nextRaw,
		Inputs:        map[string]interface{}{"display_name": "OIDC app"},
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC", IssuerHint: "https://issuer.local"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if result.TransitionPlanHash == "" {
		t.Fatalf("dry run missing transition plan hash")
	}
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "oidcapp",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		Confirmations:      result.RequiredConfirmations,
	})
	if !errors.Is(err, ErrManifestUpdateConflict) || !strings.Contains(err.Error(), "transition plan hash is required") {
		t.Fatalf("apply err = %v, want required transition hash conflict", err)
	}
}

func TestApplyCustomManifestUpdate_PreservesExistingOIDCCredentialsInFallbackLedger(t *testing.T) {
	mgr, state, raw := installedConfigOIDCTestApp(t)
	appInst, exists := state.GetApp("oidcapp")
	if !exists {
		t.Fatalf("oidc app missing")
	}
	appInst.CatalogSource = ""
	appInst.CatalogManifestHash = ""
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store custom oidc app: %v", err)
	}
	nextRaw := []byte(strings.Replace(string(raw), "  display_name:\n    type: string", "  operator_note:\n    type: string\n    label: Operator note\n    default: preserved\n  display_name:\n    type: string", 1))

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "oidcapp",
		RawTemplate:   nextRaw,
		Inputs:        map[string]interface{}{"display_name": "OIDC app"},
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC", IssuerHint: "https://issuer.local"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.Applicable {
		t.Fatalf("input metadata update should be stageable, reason=%q", result.BlockingReason)
	}
	if !result.MetadataOnly {
		t.Fatalf("input metadata update should be metadata-only")
	}
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "oidcapp",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		TransitionPlanHash: result.TransitionPlanHash,
		DryRunToken:        result.DryRunToken,
		Confirmations:      result.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.OIDCCredentials == nil {
		t.Fatalf("stored OIDC credentials = nil")
	}
	if st.OIDCCredentials.ClientID != "client-id" || st.OIDCCredentials.ClientSecret != "client-secret" {
		t.Fatalf("stored OIDC credentials = %+v, want existing credentials", st.OIDCCredentials)
	}
}

func TestNormalizeManifestUpdateInputs_GeneratedValuesAreExplicitAndAppAddressPinned(t *testing.T) {
	declared := map[string]api.AppInput{
		"__app_address__": {Type: "string", Required: true},
		"api_key":         {Type: "password", Required: true, Generate: true},
		"provider":        {Type: "string", Default: "local"},
	}
	if _, _, err := normalizeManifestUpdateInputs(declared, nil, map[string]interface{}{}, nil, nil, "piclu"); err == nil {
		t.Fatalf("expected missing generated input to fail")
	}

	inputs, sensitive, err := normalizeManifestUpdateInputs(declared, nil, map[string]interface{}{"__app_address__": "other"}, []string{"api_key"}, nil, "piclu")
	if err != nil {
		t.Fatalf("normalize with regenerate: %v", err)
	}
	if got := inputs["__app_address__"]; got != "piclu" {
		t.Fatalf("__app_address__ = %v, want pinned piclu", got)
	}
	if got := strings.TrimSpace(inputs["api_key"].(string)); got == "" {
		t.Fatalf("regenerated api_key is empty")
	}
	if !sensitive["api_key"] {
		t.Fatalf("regenerated api_key should be marked sensitive")
	}
	if got := inputs["provider"]; got != "local" {
		t.Fatalf("provider = %v, want default", got)
	}
}

func TestNormalizeManifestUpdateInputsPreservesExistingLedgerValues(t *testing.T) {
	declared := map[string]api.AppInput{
		"__app_address__": {Type: "string", Required: true},
		"api_key":         {Type: "password", Required: true},
		"provider":        {Type: "string", Required: true},
		"session_key":     {Type: "password", Required: true, Generate: true},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"api_key":     "kept-api-key",
			"provider":    "kept-provider",
			"session_key": "kept-session",
		},
	}
	inputs, _, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{}, nil, nil, "piclu")
	if err != nil {
		t.Fatalf("normalize preserving ledger inputs: %v", err)
	}
	if got := inputs["api_key"]; got != "kept-api-key" {
		t.Fatalf("api_key = %v, want existing value", got)
	}
	if got := inputs["provider"]; got != "kept-provider" {
		t.Fatalf("provider = %v, want existing value", got)
	}
	if got := inputs["session_key"]; got != "kept-session" {
		t.Fatalf("session_key = %v, want existing value", got)
	}
	if got := inputs["__app_address__"]; got != "piclu" {
		t.Fatalf("__app_address__ = %v, want pinned piclu", got)
	}
}

func TestNormalizeManifestUpdateInputsRejectsIncompatibleKeptSecretWithoutEcho(t *testing.T) {
	declared := map[string]api.AppInput{
		"license": {Type: "boolean", Required: true},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"license": "secret-license",
		},
		InputSensitive: map[string]bool{
			"license": true,
		},
	}
	_, _, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{}, nil, nil, "piclu")
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

func TestNormalizeManifestUpdateInputsRejectsBlankRequiredSecretReplacement(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key": {Type: "password", Required: true},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"api_key": "kept-api-key",
		},
	}
	if _, _, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{"api_key": ""}, nil, nil, "piclu"); err == nil {
		t.Fatalf("expected blank required secret replacement to fail")
	}
}

func TestNormalizeManifestUpdateInputsClearsOptionalSecretOnlyExplicitly(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key": {Type: "password", Default: "manifest-default"},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"api_key": "kept-api-key",
		},
	}
	if _, _, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{"api_key": ""}, nil, nil, "piclu"); err == nil {
		t.Fatalf("expected blank optional secret replacement to require explicit clear")
	}
	inputs, sensitive, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{}, nil, []string{"api_key"}, "piclu")
	if err != nil {
		t.Fatalf("normalize with explicit clear: %v", err)
	}
	if got := inputs["api_key"]; got != "" {
		t.Fatalf("api_key = %v, want cleared empty value", got)
	}
	if sensitive["api_key"] {
		t.Fatalf("cleared api_key should not remain sensitive")
	}
}

func TestManifestUpdateDisplayInputsRedactInferredSensitiveDefaults(t *testing.T) {
	declared := map[string]api.AppInput{
		"__app_address__": {Type: "string"},
		"api_key":         {Type: "string", Required: true, Default: "secret-default"},
		"provider":        {Type: "string", Default: "local"},
	}
	display := manifestUpdateDisplayInputs(declared, "piclu", false)
	if got := display["__app_address__"].Default; got != "piclu" {
		t.Fatalf("__app_address__ default = %v, want piclu", got)
	}
	if got := display["api_key"].Default; got != nil {
		t.Fatalf("api_key display default = %v, want redacted", got)
	}
	if got := display["provider"].Default; got != "local" {
		t.Fatalf("provider display default = %v, want local", got)
	}
	if got := declared["api_key"].Default; got != "secret-default" {
		t.Fatalf("declared api_key default mutated to %v", got)
	}
}

func TestNormalizeManifestUpdateInputsClearsReclassifiedStoredSecret(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key": {Type: "string"},
	}
	st := &InstallState{
		RawTemplate: []byte(`type: user
inputs:
  api_key:
    type: password
services:
  main:
    image: docker.io/example/piclu:stable
x-piccolo:
  mode: service
`),
		InstallInputs: map[string]any{
			"api_key": "kept-api-key",
		},
	}
	if _, _, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{"api_key": ""}, nil, nil, "piclu"); err == nil {
		t.Fatalf("expected blank reclassified secret replacement to require explicit clear")
	}
	inputs, sensitive, err := normalizeManifestUpdateInputs(declared, st, map[string]interface{}{}, nil, []string{"api_key"}, "piclu")
	if err != nil {
		t.Fatalf("normalize with explicit clear: %v", err)
	}
	if got := inputs["api_key"]; got != "" {
		t.Fatalf("api_key = %v, want cleared zero value", got)
	}
	if sensitive["api_key"] {
		t.Fatalf("cleared reclassified api_key should not remain sensitive")
	}
}

func TestManifestUpdateInputFieldsDescribePreservedCurrentValues(t *testing.T) {
	declared := map[string]api.AppInput{
		"__app_address__": {Type: "string", Required: true},
		"api_key":         {Type: "password", Required: true},
		"new_secret":      {Type: "password", Required: true},
		"provider":        {Type: "string", Required: true},
		"webhook_token":   {Type: "password"},
	}
	st := &InstallState{
		RawTemplate: []byte(`type: user
inputs:
  api_key:
    type: password
    required: true
  provider:
    type: string
    required: true
services:
  main:
    image: docker.io/example/piclu:stable
x-piccolo:
  mode: service
`),
		InstallInputs: map[string]any{
			"api_key":  "kept-api-key",
			"provider": "kept-provider",
		},
	}
	displayInputs, fields, preflight := manifestUpdateInputFields(declared, st, "piclu")
	if got := displayInputs["provider"].Default; got != nil {
		t.Fatalf("provider display default = %v, want current value hidden", got)
	}
	if got := displayInputs["api_key"].Default; got != nil {
		t.Fatalf("api_key display default = %v, want hidden secret", got)
	}
	byName := map[string]ManifestUpdateInputField{}
	for _, field := range fields {
		byName[field.Name] = field
	}
	if got := byName["api_key"].Provenance; got != "Current stored value will be kept" {
		t.Fatalf("api_key provenance = %q", got)
	}
	if !byName["api_key"].HasCurrentValue || !byName["api_key"].CurrentValueSensitive {
		t.Fatalf("api_key current metadata = %+v, want sensitive current value", byName["api_key"])
	}
	if !byName["api_key"].Sensitive || !byName["new_secret"].Sensitive {
		t.Fatalf("sensitive metadata = api_key:%+v new_secret:%+v, want sensitive fields", byName["api_key"], byName["new_secret"])
	}
	if got := byName["webhook_token"].Provenance; got != "Optional secret; leave blank to keep unset" {
		t.Fatalf("webhook_token provenance = %q", got)
	}
	if got := byName["provider"].Provenance; got != "Current value will be kept" {
		t.Fatalf("provider provenance = %q", got)
	}
	if !byName["provider"].HasCurrentValue || byName["provider"].CurrentValueSensitive {
		t.Fatalf("provider current metadata = %+v, want non-sensitive current value", byName["provider"])
	}
	if got := byName["provider"].CurrentValueDisplay; got != "kept-provider" {
		t.Fatalf("provider current display = %q, want kept-provider", got)
	}
	if len(preflight) != 1 || preflight[0] != "new_secret" {
		t.Fatalf("preflight = %#v, want only new_secret", preflight)
	}
}

func TestManifestUpdateCatalogPendingFieldsRedactInferredSensitiveDefaults(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key":       {Type: "string", Required: true, Default: "secret-default"},
		"provider":      {Type: "string", Required: true, Default: "local"},
		"webhook_token": {Type: "password"},
	}
	st := &InstallState{InstallInputs: map[string]any{}}
	fields, preflight := manifestUpdateInputFieldsForCatalogPending(declared, st, "piclu")
	byName := map[string]ManifestUpdateInputField{}
	for _, field := range fields {
		byName[field.Name] = field
	}
	if !byName["api_key"].Sensitive || byName["api_key"].Provenance != "Re-enter required" {
		t.Fatalf("api_key field = %+v, want inferred-sensitive re-entry", byName["api_key"])
	}
	if byName["provider"].Sensitive || byName["provider"].Provenance != "Required for this update" {
		t.Fatalf("provider field = %+v, want ordinary required catalog field", byName["provider"])
	}
	if got := byName["webhook_token"].Provenance; got != "Optional secret; leave blank to keep unset" {
		t.Fatalf("webhook_token provenance = %q", got)
	}
	if !slices.Contains(preflight, "api_key") || slices.Contains(preflight, "provider") || slices.Contains(preflight, "webhook_token") {
		t.Fatalf("preflight = %+v, want api_key only", preflight)
	}
	display := manifestUpdateDisplayInputs(declared, "piclu", false)
	if got := display["api_key"].Default; got != nil {
		t.Fatalf("api_key display default = %v, want redacted", got)
	}
}

func TestManifestUpdateCatalogPendingFieldsSurfaceStickySensitiveCurrentValues(t *testing.T) {
	declared := map[string]api.AppInput{
		"license":  {Type: "string", Required: true},
		"provider": {Type: "string", Required: true},
	}
	st := &InstallState{
		RawTemplate: []byte(`type: user
inputs:
  license:
    type: string
  provider:
    type: string
services:
  main:
    image: docker.io/example/piclu:stable
x-piccolo:
  mode: service
`),
		InstallInputs: map[string]any{
			"license":  "secret-license",
			"provider": "local",
		},
		InputSensitive: map[string]bool{
			"license": true,
		},
	}
	fields, preflight := manifestUpdateInputFieldsForCatalogPending(declared, st, "piclu")
	byName := map[string]ManifestUpdateInputField{}
	for _, field := range fields {
		byName[field.Name] = field
	}
	license := byName["license"]
	if license.Name == "" {
		t.Fatalf("sticky-sensitive license field was hidden: %+v", fields)
	}
	if !license.HasCurrentValue || !license.CurrentValueSensitive || !license.Sensitive {
		t.Fatalf("license current metadata = %+v, want sensitive current value", license)
	}
	if license.CurrentValueDisplay != "" {
		t.Fatalf("license display leaked current value: %+v", license)
	}
	if _, ok := byName["provider"]; ok {
		t.Fatalf("non-sensitive stored provider should stay hidden from pending form: %+v", byName["provider"])
	}
	if len(preflight) != 0 {
		t.Fatalf("preflight = %+v, want none for reusable current value", preflight)
	}
}

func TestManifestUpdateInputFieldsHideReclassifiedStoredSecret(t *testing.T) {
	declared := map[string]api.AppInput{
		"session": {Type: "string", Required: true},
	}
	st := &InstallState{
		RawTemplate: []byte(`type: user
inputs:
  session:
    type: password
    required: true
services:
  main:
    image: docker.io/example/piclu:stable
x-piccolo:
  mode: service
`),
		InstallInputs: map[string]any{
			"session": "super-secret-session",
		},
	}
	displayInputs, fields, preflight := manifestUpdateInputFields(declared, st, "piclu")
	if got := displayInputs["session"].Default; got != nil {
		t.Fatalf("session display default = %v, want hidden reclassified secret", got)
	}
	if len(preflight) != 0 {
		t.Fatalf("preflight = %#v, want none for preserved current value", preflight)
	}
	if len(fields) != 1 {
		t.Fatalf("fields = %#v, want one", fields)
	}
	field := fields[0]
	if !field.HasCurrentValue || !field.CurrentValueSensitive {
		t.Fatalf("session field metadata = %+v, want sensitive current value", field)
	}
	if field.CurrentValueDisplay != "" {
		t.Fatalf("session current display = %q, want hidden", field.CurrentValueDisplay)
	}
	if got := field.Provenance; got != "Current stored value will be kept" {
		t.Fatalf("session provenance = %q", got)
	}
}

func TestManifestUpdateInputFieldsHideStoredValueAbsentFromPreviousSchema(t *testing.T) {
	declared := map[string]api.AppInput{
		"provider": {Type: "string", Required: true},
	}
	st := &InstallState{
		RawTemplate: []byte(`type: user
inputs:
  model:
    type: string
services:
  main:
    image: docker.io/example/piclu:stable
x-piccolo:
  mode: service
`),
		InstallInputs: map[string]any{
			"provider": "stored-provider-secret",
		},
	}
	displayInputs, fields, preflight := manifestUpdateInputFields(declared, st, "piclu")
	if got := displayInputs["provider"].Default; got != nil {
		t.Fatalf("provider display default = %v, want hidden stored value", got)
	}
	if len(preflight) != 0 {
		t.Fatalf("preflight = %#v, want none for preserved current value", preflight)
	}
	if len(fields) != 1 {
		t.Fatalf("fields = %#v, want one", fields)
	}
	field := fields[0]
	if !field.HasCurrentValue || !field.CurrentValueSensitive {
		t.Fatalf("provider field metadata = %+v, want sensitive current value", field)
	}
	if field.CurrentValueDisplay != "" {
		t.Fatalf("provider current display = %q, want hidden", field.CurrentValueDisplay)
	}
}

func TestNormalizePendingCatalogManifestReviewInputsAcceptsProvidedSecrets(t *testing.T) {
	declared := map[string]api.AppInput{
		"__app_address__": {Type: "string", Required: true},
		"api_key":         {Type: "password", Required: true},
		"session_key":     {Type: "password", Required: true, Generate: true},
		"region":          {Type: "string", Default: "local"},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"existing": "kept",
		},
		InputProvenance: map[string]string{
			"existing": InputProvenanceOperator,
		},
	}

	inputs, provenance, sensitive, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		Inputs: map[string]interface{}{
			"api_key":     "typed-api-key",
			"session_key": "typed-session-key",
		},
	}, "piclu")
	if err != nil {
		t.Fatalf("normalize pending catalog review inputs: %v", err)
	}
	if got := inputs["api_key"]; got != "typed-api-key" {
		t.Fatalf("api_key = %v, want typed value", got)
	}
	if got := provenance["api_key"]; got != InputProvenanceOperator {
		t.Fatalf("api_key provenance = %q, want operator", got)
	}
	if got := inputs["session_key"]; got != "typed-session-key" {
		t.Fatalf("session_key = %v, want typed value", got)
	}
	if got := provenance["session_key"]; got != InputProvenanceOperator {
		t.Fatalf("session_key provenance = %q, want operator", got)
	}
	if !sensitive["api_key"] || !sensitive["session_key"] {
		t.Fatalf("provided secrets should be marked sensitive: %+v", sensitive)
	}
	if _, ok := inputs["region"]; !ok {
		t.Fatalf("region default missing from normalized inputs")
	}
	if got := provenance["region"]; got != InputProvenanceCatalogDefault {
		t.Fatalf("region provenance = %q, want catalog default", got)
	}

	inputs, provenance, sensitive, err = normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		Inputs:           map[string]interface{}{"api_key": "typed-api-key"},
		RegenerateInputs: []string{"session_key"},
	}, "piclu")
	if err != nil {
		t.Fatalf("normalize pending catalog regenerate input: %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(inputs["session_key"])); got == "" {
		t.Fatalf("regenerated session_key is empty")
	}
	if got := provenance["session_key"]; got != InputProvenanceGenerated {
		t.Fatalf("session_key regenerate provenance = %q, want generated", got)
	}
	if !sensitive["session_key"] {
		t.Fatalf("regenerated session_key should be marked sensitive")
	}
}

func TestNormalizePendingCatalogManifestReviewInputsRejectsIncompatibleKeptSecretWithoutEcho(t *testing.T) {
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
	_, _, _, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{}, "piclu")
	if err == nil {
		t.Fatalf("expected incompatible kept catalog secret to fail")
	}
	if strings.Contains(err.Error(), "secret-license") || strings.Contains(err.Error(), "expected boolean") {
		t.Fatalf("error leaked kept secret or validator detail: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be safely reused") {
		t.Fatalf("error = %v, want generic safe-reuse failure", err)
	}
}

func TestNormalizePendingCatalogManifestReviewInputsClearsOptionalSecretsOnlyExplicitly(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key":     {Type: "password", Default: "manifest-default"},
		"session_key": {Type: "password", Generate: true, Default: "session-default"},
	}
	st := &InstallState{
		InstallInputs: map[string]any{
			"api_key":     "kept-api-key",
			"session_key": "kept-session-key",
		},
		InputProvenance: map[string]string{
			"api_key":     InputProvenanceOperator,
			"session_key": InputProvenanceGenerated,
		},
	}

	if _, _, _, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		Inputs: map[string]interface{}{"api_key": ""},
	}, "piclu"); err == nil {
		t.Fatalf("expected blank optional catalog secret replacement to require explicit clear")
	}
	if _, _, _, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		Inputs: map[string]interface{}{"session_key": ""},
	}, "piclu"); err == nil {
		t.Fatalf("expected blank optional generated catalog replacement to require explicit clear or regenerate")
	}

	inputs, provenance, sensitive, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		ClearInputs: []string{"api_key", "session_key"},
	}, "piclu")
	if err != nil {
		t.Fatalf("normalize pending catalog review with explicit clear: %v", err)
	}
	if got := inputs["api_key"]; got != "" {
		t.Fatalf("api_key = %v, want cleared empty value", got)
	}
	if got := provenance["api_key"]; got != InputProvenanceOperator {
		t.Fatalf("api_key provenance = %q, want operator", got)
	}
	if got := inputs["session_key"]; got != "" {
		t.Fatalf("session_key = %v, want cleared empty value", got)
	}
	if got := provenance["session_key"]; got != InputProvenanceOperator {
		t.Fatalf("session_key provenance = %q, want operator", got)
	}
	if sensitive["api_key"] || sensitive["session_key"] {
		t.Fatalf("cleared secrets should not remain sensitive: %+v", sensitive)
	}
}

func TestNormalizePendingCatalogManifestReviewInputsClearsReclassifiedStoredSecret(t *testing.T) {
	declared := map[string]api.AppInput{
		"api_key": {Type: "string"},
	}
	st := &InstallState{
		RawTemplate: []byte(`type: user
inputs:
  api_key:
    type: password
services:
  main:
    image: docker.io/example/piclu:stable
x-piccolo:
  mode: service
`),
		InstallInputs: map[string]any{
			"api_key": "kept-api-key",
		},
		InputProvenance: map[string]string{
			"api_key": InputProvenanceOperator,
		},
	}
	if _, _, _, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		Inputs: map[string]interface{}{"api_key": ""},
	}, "piclu"); err == nil {
		t.Fatalf("expected blank catalog reclassified secret replacement to require explicit clear")
	}
	inputs, provenance, sensitive, err := normalizePendingCatalogManifestReviewInputs(declared, st, ManifestUpdateRequest{
		ClearInputs: []string{"api_key"},
	}, "piclu")
	if err != nil {
		t.Fatalf("normalize catalog with explicit clear: %v", err)
	}
	if got := inputs["api_key"]; got != "" {
		t.Fatalf("api_key = %v, want cleared zero value", got)
	}
	if got := provenance["api_key"]; got != InputProvenanceOperator {
		t.Fatalf("api_key provenance = %q, want operator", got)
	}
	if sensitive["api_key"] {
		t.Fatalf("cleared reclassified api_key should not remain sensitive")
	}
}

func TestDryRunCustomManifestUpdate_MaterializesCandidateAndToken(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
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

	rawTemplate := []byte(`type: user
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
	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:  "piclu",
		RawTemplate: rawTemplate,
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
	cand := mgr.manifestUpdateCandidates[result.DryRunToken]
	if cand == nil {
		t.Fatalf("stored candidate not found for token %q", result.DryRunToken)
	}
	if cand.TransitionPlan.SourceHash != Sha256Hex(rawTemplate) {
		t.Fatalf("transition source hash = %q, want raw template hash %q", cand.TransitionPlan.SourceHash, Sha256Hex(rawTemplate))
	}
	if _, err := os.Stat(filepath.Join(tempDir, AppsDir, "piclu", "install_state.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run must not create install_state.json, stat err=%v", err)
	}
}

func TestDryRunCustomManifestUpdate_ClassifiesImageReviewCandidate(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
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
    image: docker.io/example/piclu:new
    bind_ports: [8080]
    environment:
      PICLU_MODE: device
    storage:
      persistent:
        data:
          container: /data
x-piccolo:
  mode: service
`),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.Applicable {
		t.Fatalf("expected image review candidate to be stageable: %s", result.BlockingReason)
	}
	if result.DryRunToken == "" {
		t.Fatalf("stageable dry run must mint token")
	}
	if result.UpdateClass != "service_app_update_v2" {
		t.Fatalf("update class = %q, want service_app_update_v2", result.UpdateClass)
	}
	decision := findManifestDecision(result.Decisions, "image_refs_changed")
	if decision == nil || decision.Outcome != "operator_review" {
		t.Fatalf("expected operator_review image decision, got %+v", decision)
	}
	if !slices.Contains(result.RequiredConfirmations, "image_update_review") {
		t.Fatalf("expected image_update_review confirmation, got %v", result.RequiredConfirmations)
	}
	if result.DataSafety == nil || !result.DataSafety.SnapshotRequired {
		t.Fatalf("expected data safety snapshot requirement, got %+v", result.DataSafety)
	}
	if got := strings.Join(result.StagedImageRootfs, "\n"); !strings.Contains(got, "service main will stage") || !strings.Contains(got, "sha256:mockdigest") || !strings.Contains(got, "expected rootfs") {
		t.Fatalf("expected dry-run image plan with digest/rootfs, got %q", got)
	}
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		TransitionPlanHash: result.TransitionPlanHash,
	})
	if !errors.Is(err, ErrManifestUpdateRejected) || !strings.Contains(err.Error(), "image_update_review") {
		t.Fatalf("apply err = %v, want missing image_update_review confirmation rejection", err)
	}
}

func TestApplyCustomManifestUpdateRefreshesSameRefDigestDrift(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		digest := "sha256:new"
		repoDigest := "docker.io/example/piclu@sha256:new"
		if imageName == networkAnchorImage() {
			digest = "sha256:pause"
			repoDigest = networkAnchorImage() + "@sha256:pause"
		}
		return &container.ImageConfig{
			Cmd:         []string{"/bin/sh"},
			Digest:      digest,
			RepoDigests: []string{repoDigest},
			Size:        500 << 20,
		}, nil
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		if imageRef == networkAnchorImage() {
			return networkAnchorImage() + "@sha256:pause", nil
		}
		return "docker.io/example/piclu@sha256:new", nil
	}
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: tempDir}}
	mgr.SetVolumeManager(volumes)
	rootfs := newStubRootfsManager(tempDir)
	rootfs.exists = map[string]bool{
		"rootfs-main":   true,
		"rootfs-anchor": true,
	}
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "sha256:old",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestEnvUpdateRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !slices.Contains(result.RequiredConfirmations, "image_update_review") {
		t.Fatalf("expected image_update_review confirmation, got %v", result.RequiredConfirmations)
	}
	if got := strings.Join(result.StagedImageRootfs, "\n"); !strings.Contains(got, "service main will refresh") || !strings.Contains(got, "sha256:new") {
		t.Fatalf("expected same-ref refresh image plan, got %q", got)
	}

	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		TransitionPlanHash: result.TransitionPlanHash,
		Confirmations:      result.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Applicable {
		t.Fatalf("apply result not applicable: %+v", applied)
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	wantRootfs := persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:new"))
	if got := stored.ActiveRootfs["main"]; got != wantRootfs {
		t.Fatalf("active rootfs main = %q, want refreshed %q", got, wantRootfs)
	}
	if got := stored.ActiveRootfs[networkAnchorServiceName]; got != "rootfs-anchor" {
		t.Fatalf("anchor rootfs = %q, want preserved rootfs-anchor", got)
	}
	if len(volumes.snapshots) == 0 {
		t.Fatalf("same-ref rootfs refresh for persistent app did not create a precommit data snapshot")
	}
}

func TestApplyCustomManifestUpdateImageOnlySameRefRefreshRequiresSnapshotViability(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		digest := "sha256:new"
		repoDigest := "docker.io/example/piclu@sha256:new"
		if imageName == networkAnchorImage() {
			digest = "sha256:pause"
			repoDigest = networkAnchorImage() + "@sha256:pause"
		}
		return &container.ImageConfig{
			Cmd:         []string{"/bin/sh"},
			Digest:      digest,
			RepoDigests: []string{repoDigest},
			Size:        500 << 20,
		}, nil
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		if imageRef == networkAnchorImage() {
			return networkAnchorImage() + "@sha256:pause", nil
		}
		return "docker.io/example/piclu@sha256:new", nil
	}
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		viabilityErr:      errors.New("thin pool metadata usage 91.0% exceeds threshold"),
	}
	mgr.SetVolumeManager(volumes)
	rootfs := newStubRootfsManager(tempDir)
	rootfs.exists = map[string]bool{
		"rootfs-main":   true,
		"rootfs-anchor": true,
	}
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "sha256:old",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	oldContainerID := "cid-old-main"
	mock.containers[oldContainerID] = &mockContainer{ID: oldContainerID, Status: "running", Spec: container.ContainerCreateSpec{Name: "piclu-main"}}
	baseDef := customManifestPolicyBaseDef()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": oldContainerID},
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestBaseRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !slices.Contains(result.RequiredConfirmations, "image_update_review") {
		t.Fatalf("expected image_update_review confirmation, got %v", result.RequiredConfirmations)
	}
	if result.DataSafety == nil || !result.DataSafety.SnapshotRequired {
		t.Fatalf("expected image-only rootfs refresh to require data snapshot, got %+v", result.DataSafety)
	}
	if got := strings.Join(result.StagedImageRootfs, "\n"); !strings.Contains(got, "service main will refresh") || !strings.Contains(got, "sha256:new") {
		t.Fatalf("expected same-ref refresh image plan, got %q", got)
	}

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		TransitionPlanHash: result.TransitionPlanHash,
		Confirmations:      result.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "precommit data snapshot viability") {
		t.Fatalf("apply err = %v, want snapshot viability rejection", err)
	}
	if len(volumes.snapshots) != 0 {
		t.Fatalf("snapshot should not be created after failed viability gate, got %v", volumes.snapshots)
	}
	if oldContainer, exists := mock.containers[oldContainerID]; !exists {
		t.Fatalf("old container %q missing from mock runtime", oldContainerID)
	} else if oldContainer.Status != "running" {
		t.Fatalf("viability preflight should leave old container running, status=%q", oldContainer.Status)
	}
}

func TestApplyCustomManifestUpdateRejectsExistingTargetRootfsDigestMismatch(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		digest := "sha256:new"
		repoDigest := "docker.io/example/piclu@sha256:new"
		if imageName == networkAnchorImage() {
			digest = "sha256:pause"
			repoDigest = networkAnchorImage() + "@sha256:pause"
		}
		return &container.ImageConfig{
			Cmd:         []string{"/bin/sh"},
			Digest:      digest,
			RepoDigests: []string{repoDigest},
			Size:        500 << 20,
		}, nil
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		if imageRef == networkAnchorImage() {
			return networkAnchorImage() + "@sha256:pause", nil
		}
		return "docker.io/example/piclu@sha256:new", nil
	}
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: tempDir}}
	mgr.SetVolumeManager(volumes)
	rootfs := newStubRootfsManager(tempDir)
	targetRootfs := persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:new"))
	rootfs.exists = map[string]bool{
		"rootfs-main":   true,
		"rootfs-anchor": true,
		targetRootfs:    true,
	}
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "sha256:old",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
		targetRootfs: {
			VolumeID:        targetRootfs,
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "sha256:stale-target",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestEnvUpdateRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		TransitionPlanHash: result.TransitionPlanHash,
		Confirmations:      result.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match planned image identity") {
		t.Fatalf("apply err = %v, want existing target rootfs identity rejection", err)
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if got := stored.ActiveRootfs["main"]; got != "rootfs-main" {
		t.Fatalf("active rootfs main = %q, want previous rootfs-main", got)
	}
	if slices.Contains(rootfs.detached, "rootfs-main") || slices.Contains(rootfs.destroyed, "rootfs-main") {
		t.Fatalf("previous active rootfs should not be detached/destroyed, detached=%v destroyed=%v", rootfs.detached, rootfs.destroyed)
	}
}

func TestDryRunCustomManifestUpdateFailsFastWhenSameRefDigestUnavailable(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		return "", fmt.Errorf("registry unavailable for %s", imageRef)
	}
	rootfs := newStubRootfsManager(tempDir)
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "sha256:old",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	_, err = mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestEnvUpdateRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err == nil || !strings.Contains(err.Error(), "resolve manifest update image plan") || !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("dry run err = %v, want registry digest failure", err)
	}
	if _, ok := mgr.manifestUpdateCandidates["piclu"]; ok {
		t.Fatalf("dry run failure must not store a candidate")
	}
}

func TestDryRunCustomManifestUpdatePreservesSameRefWhenRootfsIdentityFresh(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		t.Fatalf("unexpected image inspect for preserved rootfs: %s", imageName)
		return nil, fmt.Errorf("unexpected image inspect")
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		if imageRef == networkAnchorImage() {
			return networkAnchorImage() + "@sha256:pause", nil
		}
		return "docker.io/example/piclu@sha256:stable", nil
	}
	rootfs := newStubRootfsManager(tempDir)
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "docker.io/example/piclu@sha256:stable",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestEnvUpdateRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if slices.Contains(result.RequiredConfirmations, "image_update_review") {
		t.Fatalf("did not expect image_update_review confirmation, got %v", result.RequiredConfirmations)
	}
	if len(result.StagedImageRootfs) != 0 {
		t.Fatalf("expected no staged image rootfs decisions, got %v", result.StagedImageRootfs)
	}
}

func TestDryRunCustomManifestUpdateFallsBackWhenSkopeoUnavailable(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	t.Setenv("PATH", tempDir)
	mock := NewMockContainerManager()
	inspectCalls := 0
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		inspectCalls++
		switch imageName {
		case "docker.io/example/piclu:stable":
			return &container.ImageConfig{
				Digest:      "sha256:stable",
				RepoDigests: []string{"docker.io/example/piclu@sha256:stable"},
				Size:        500 << 20,
			}, nil
		case networkAnchorImage():
			return &container.ImageConfig{
				Digest:      "sha256:pause",
				RepoDigests: []string{networkAnchorImage() + "@sha256:pause"},
				Size:        8 << 20,
			}, nil
		default:
			t.Fatalf("unexpected image inspect for %s", imageName)
			return nil, fmt.Errorf("unexpected image inspect")
		}
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = nil
	rootfs := newStubRootfsManager(tempDir)
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "docker.io/example/piclu@sha256:stable",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestEnvUpdateRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if slices.Contains(result.RequiredConfirmations, "image_update_review") {
		t.Fatalf("did not expect image_update_review confirmation, got %v", result.RequiredConfirmations)
	}
	if len(result.StagedImageRootfs) != 0 {
		t.Fatalf("expected no staged image rootfs decisions, got %v", result.StagedImageRootfs)
	}
	if inspectCalls == 0 {
		t.Fatalf("expected pull/inspect digest fallback when skopeo is unavailable")
	}
}

func TestApplyCustomManifestUpdateRefreshesNetworkAnchorDigestDrift(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		if imageName != networkAnchorImage() {
			t.Fatalf("unexpected image inspect for %s", imageName)
		}
		return &container.ImageConfig{
			Cmd:         []string{"/pause"},
			Digest:      "sha256:anchor-new",
			RepoDigests: []string{networkAnchorImage() + "@sha256:anchor-new"},
			Size:        8 << 20,
		}, nil
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		if imageRef == networkAnchorImage() {
			return networkAnchorImage() + "@sha256:anchor-new", nil
		}
		return "docker.io/example/piclu@sha256:stable", nil
	}
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: tempDir}}
	mgr.SetVolumeManager(volumes)
	rootfs := newStubRootfsManager(tempDir)
	rootfs.exists = map[string]bool{
		"rootfs-main":   true,
		"rootfs-anchor": true,
	}
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main": {
			VolumeID:        "rootfs-main",
			BaseImageRef:    "docker.io/example/piclu:stable",
			BaseImageDigest: "sha256:stable",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:anchor-old",
		},
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestEnvUpdateRaw(),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !slices.Contains(result.RequiredConfirmations, "image_update_review") {
		t.Fatalf("expected image_update_review confirmation, got %v", result.RequiredConfirmations)
	}
	if got := strings.Join(result.StagedImageRootfs, "\n"); !strings.Contains(got, "Piccolo runtime support will refresh") || strings.Contains(got, "service main will refresh") {
		t.Fatalf("expected only runtime anchor refresh plan, got %q", got)
	}

	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		TransitionPlanHash: result.TransitionPlanHash,
		Confirmations:      result.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Applicable {
		t.Fatalf("apply result not applicable: %+v", applied)
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if got := stored.ActiveRootfs["main"]; got != "rootfs-main" {
		t.Fatalf("main rootfs = %q, want preserved rootfs-main", got)
	}
	wantAnchor := persistence.VersionedServiceRootfsVolumeID("piclu", networkAnchorServiceName, persistence.ShortDigest("sha256:anchor-new"))
	if got := stored.ActiveRootfs[networkAnchorServiceName]; got != wantAnchor {
		t.Fatalf("anchor rootfs = %q, want refreshed %q", got, wantAnchor)
	}
	if !slices.Contains(rootfs.destroyed, "rootfs-anchor") {
		t.Fatalf("destroyed rootfs = %v, want old runtime anchor rootfs cleaned up", rootfs.destroyed)
	}
}

func TestResolveManifestUpdateImagePlanDigestPinnedRootfsIdentity(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	inspectCalls := 0
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		inspectCalls++
		if imageName != "docker.io/example/piclu@sha256:pinned" {
			t.Fatalf("unexpected image inspect for %s", imageName)
		}
		return &container.ImageConfig{
			Digest:      "sha256:pinned",
			RepoDigests: []string{"docker.io/example/piclu@sha256:pinned"},
			Size:        500 << 20,
		}, nil
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.imageDigestResolver = func(_ context.Context, imageRef string) (string, error) {
		if imageRef == networkAnchorImage() {
			return networkAnchorImage() + "@sha256:pause", nil
		}
		t.Fatalf("digest-pinned refs must not resolve remote digest: %s", imageRef)
		return "", fmt.Errorf("unexpected registry lookup")
	}
	rootfs := newStubRootfsManager(tempDir)
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main-fresh": {
			VolumeID:        "rootfs-main-fresh",
			BaseImageRef:    "docker.io/example/piclu@sha256:pinned",
			BaseImageDigest: "sha256:pinned",
		},
		"rootfs-anchor": {
			VolumeID:        "rootfs-anchor",
			BaseImageRef:    networkAnchorImage(),
			BaseImageDigest: "sha256:pause",
		},
	}
	mgr.SetRootfsManager(rootfs)
	curDef := customManifestPolicyBaseDef()
	svc := curDef.Services["main"]
	svc.Image = "docker.io/example/piclu@sha256:pinned"
	curDef.Services["main"] = svc
	candidateDef := customManifestPolicyClone(t, curDef)
	appInst := &AppInstance{
		InstanceID:   "piclu",
		ActiveRootfs: map[string]string{"main": "rootfs-main-fresh", networkAnchorServiceName: "rootfs-anchor"},
	}

	plan, err := mgr.resolveManifestUpdateImagePlan(context.Background(), appInst.InstanceID, appInst, curDef, candidateDef, true)
	if err != nil {
		t.Fatalf("fresh pinned image plan: %v", err)
	}
	if len(plan) != 0 {
		t.Fatalf("expected fresh pinned rootfs to be preserved, got %+v", plan)
	}
	if inspectCalls != 0 {
		t.Fatalf("fresh pinned rootfs should not be inspected, got %d calls", inspectCalls)
	}

	rootfs.identities["rootfs-main-stale"] = persistence.RootfsImageIdentity{
		VolumeID:        "rootfs-main-stale",
		BaseImageRef:    "docker.io/example/piclu@sha256:pinned",
		BaseImageDigest: "sha256:old",
	}
	appInst.ActiveRootfs["main"] = "rootfs-main-stale"
	plan, err = mgr.resolveManifestUpdateImagePlan(context.Background(), appInst.InstanceID, appInst, curDef, candidateDef, true)
	if err != nil {
		t.Fatalf("stale pinned image plan: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("expected one stale pinned refresh item, got %+v", plan)
	}
	if got := plan[0]; got.ServiceName != "main" || got.Action != manifestUpdateImageActionRefresh || got.CanonicalDigest != "sha256:pinned" {
		t.Fatalf("unexpected pinned refresh item: %+v", got)
	}
	if inspectCalls != 1 {
		t.Fatalf("stale pinned rootfs should be inspected once, got %d calls", inspectCalls)
	}
}

func TestApplyCustomManifestUpdateRejectsImageDigestDriftAfterDryRun(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	digest := "docker.io/example/piclu@sha256:dryrun"
	mock.inspectImageHook = func(imageName string) (*container.ImageConfig, error) {
		return &container.ImageConfig{
			Cmd:         []string{"/bin/sh"},
			Digest:      strings.TrimPrefix(digest, "docker.io/example/piclu@"),
			RepoDigests: []string{digest},
			Size:        500 << 20,
		}, nil
	}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: tempDir}}
	mgr.SetVolumeManager(volumes)
	rootfs := newStubRootfsManager(tempDir)
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     customManifestPolicyBaseDef(),
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	result, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   customManifestImageUpdateRaw("docker.io/example/piclu:new"),
		SystemContext: InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if result.DryRunToken == "" {
		t.Fatalf("expected dry-run token")
	}

	digest = "docker.io/example/piclu@sha256:changed"
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   result.BaseManifestHash,
		RuntimeFingerprint: result.RuntimeFingerprint,
		DryRunToken:        result.DryRunToken,
		TransitionPlanHash: result.TransitionPlanHash,
		Confirmations:      result.RequiredConfirmations,
	})
	if !errors.Is(err, ErrManifestUpdateConflict) || !strings.Contains(err.Error(), "image digest for service main changed after dry run") {
		t.Fatalf("apply err = %v, want digest drift conflict", err)
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if stored.Definition.Services["main"].Image != "docker.io/example/piclu:stable" {
		t.Fatalf("digest drift should keep previous manifest, got image %q", stored.Definition.Services["main"].Image)
	}
	if len(volumes.snapshots) != 0 {
		t.Fatalf("digest drift should reject before data snapshot, got %v", volumes.snapshots)
	}
}

func TestApplyCustomManifestUpdate_ImageReviewStagesRootfsAndPrivateSnapshot(t *testing.T) {
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
	rootfs := newStubRootfsManager(tempDir)
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))
	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply image update: %v", err)
	}
	if applied.UpdateClass != "service_app_update_v2" {
		t.Fatalf("update class = %q, want service_app_update_v2", applied.UpdateClass)
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("updated app missing")
	}
	wantRootfs := persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:mockdigest"))
	if got := stored.ActiveRootfs["main"]; got != wantRootfs {
		t.Fatalf("active rootfs main = %q, want %q", got, wantRootfs)
	}
	if stored.Definition.Services["main"].Image != "docker.io/example/piclu:new" {
		t.Fatalf("stored image = %q", stored.Definition.Services["main"].Image)
	}
	if len(volumes.snapshots) != 1 {
		t.Fatalf("snapshots = %v, want one private snapshot", volumes.snapshots)
	}
	if !slices.Equal(volumes.snapshots, volumes.destroyed) {
		t.Fatalf("snapshot cleanup mismatch: snapshots=%v destroyed=%v", volumes.snapshots, volumes.destroyed)
	}
	if !slices.Contains(rootfs.destroyed, "rootfs-main") {
		t.Fatalf("destroyed rootfs = %v, want old rootfs-main cleanup", rootfs.destroyed)
	}
	if slices.Contains(rootfs.destroyed, wantRootfs) {
		t.Fatalf("destroyed candidate rootfs %s: %v", wantRootfs, rootfs.destroyed)
	}
	txnPath := filepath.Join(tempDir, AppsDir, "piclu", manifestUpdateTxnFilename)
	if _, err := os.Stat(txnPath); !os.IsNotExist(err) {
		t.Fatalf("transaction should be cleared after commit, stat err=%v", err)
	}
}

func TestApplyCustomManifestUpdate_StopsRuntimeBeforePrecommitSnapshot(t *testing.T) {
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
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	mock.containers["anchor-old"] = &mockContainer{ID: "anchor-old", Status: "running"}
	mock.containers["main-old"] = &mockContainer{ID: "main-old", Status: "running"}
	appInst := &AppInstance{
		InstanceID:      "piclu",
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "anchor-old",
		Containers:      map[string]string{"main": "main-old"},
		ActiveRootfs:    map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	sawStoppedSnapshot := false
	volumes.snapshotHook = func(instanceID, snapshotLVName string) error {
		_ = snapshotLVName
		if instanceID != "piclu" {
			t.Fatalf("snapshot instance = %q, want piclu", instanceID)
		}
		if got := mock.containers["main-old"].Status; got != "stopped" {
			t.Fatalf("main container status at snapshot = %q, want stopped", got)
		}
		if got := mock.containers["anchor-old"].Status; got != "stopped" {
			t.Fatalf("anchor container status at snapshot = %q, want stopped", got)
		}
		sawStoppedSnapshot = true
		return nil
	}

	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply image update: %v", err)
	}
	if !sawStoppedSnapshot {
		t.Fatalf("snapshot hook did not observe stopped old runtime")
	}
}

func TestApplyCustomManifestUpdate_MarksAccessSuspendedDuringRuntimeSwitch(t *testing.T) {
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
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	sawAccessSuspended := false
	state.storeManifestUpdateTransactionHook = func(instanceID string, txn *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && txn.Phase == "access_suspended" {
			sawAccessSuspended = true
		}
		return nil
	}

	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply image update: %v", err)
	}
	if !sawAccessSuspended {
		t.Fatalf("runtime switch did not persist access_suspended phase")
	}
}

func TestApplyCustomManifestUpdate_PostCommitAccessFailureReturnsRepairPending(t *testing.T) {
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
	oldRaw := customManifestOIDCRaw(false)
	newRaw := customManifestOIDCRaw(true)
	inputs := map[string]interface{}{"display_name": "OIDC app"}
	systemCtx := InstallSystemContext{
		Domain:       "local",
		Architecture: "amd64",
		Timezone:     "Etc/UTC",
		IssuerHint:   "https://issuer.local",
	}
	creds := &OIDCCredentials{ClientID: "client-id", ClientSecret: "client-secret"}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "oidcapp",
		ExistingOIDC:  creds,
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	newRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   newRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "oidcapp",
		ExistingOIDC:  creds,
	}, nil, nil)
	if err != nil {
		t.Fatalf("render new manifest: %v", err)
	}
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:     "oidcapp",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("oidcapp", NewV2InstallState("oidcapp", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, creds, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}
	baseHash, err := canonicalManifestHash(oldRes.Definition)
	if err != nil {
		t.Fatalf("base hash: %v", err)
	}
	candidateHash, err := canonicalManifestHash(newRes.Definition)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		t.Fatalf("runtime fingerprint: %v", err)
	}
	ledgerExists, ledgerRevision, ledgerSourceHash, err := loadInstallLedgerFingerprint(state, "oidcapp")
	if err != nil {
		t.Fatalf("ledger fingerprint: %v", err)
	}
	policy, summary := evaluateCustomManifestUpdatePolicy(oldRes.Definition, newRes.Definition)
	nextState := NewV2InstallState("oidcapp", InstallSourceKindCustom, "", newRaw, inputs, systemCtx, creds, false)
	cand := &manifestUpdateCandidate{
		Token:                "token-access-repair",
		InstanceID:           "oidcapp",
		RawTemplate:          newRaw,
		Inputs:               inputs,
		SystemContext:        systemCtx,
		BaseManifestHash:     baseHash,
		RuntimeFingerprint:   runtimeFingerprint,
		BaseLedgerExists:     ledgerExists,
		BaseLedgerRevision:   ledgerRevision,
		BaseLedgerSourceHash: ledgerSourceHash,
		CandidateDigest:      candidateHash,
		InstallState:         nextState,
		DiffKind:             classifyDiff(cloneDefinitionForCompare(oldRes.Definition), cloneDefinitionForCompare(newRes.Definition)),
		MetadataOnly:         true,
		Definition:           newRes.Definition,
		Summary:              summary,
		Classification:       policy.Classification,
		CreatedAt:            now,
		ExpiresAt:            now.Add(manifestUpdateTokenTTL),
	}
	mgr.storeManifestUpdateCandidate(cand)
	requiresProxy := func(def *api.AppDefinition) bool {
		for _, svc := range def.Services {
			if svc.OIDCClient != nil && len(svc.OIDCClient.AuthorizePaths) > 0 {
				return true
			}
		}
		return false
	}
	registered := []string{}
	mgr.SetSyncHost(installedConfigSyncHost{
		requiresProxy:   requiresProxy,
		registeredProxy: &registered,
		registerErr:     errors.New("proxy registry unavailable"),
	})

	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "oidcapp",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply should return committed repair-pending result, got error: %v", err)
	}
	if !applied.AccessRepairPending || !strings.Contains(applied.AccessRepairMessage, "proxy registry unavailable") {
		t.Fatalf("access repair result = pending:%v message:%q", applied.AccessRepairPending, applied.AccessRepairMessage)
	}
	if len(registered) != 1 || registered[0] != "oidcapp" {
		t.Fatalf("proxy registration calls = %v, want failed publish attempt", registered)
	}
	storedDef, err := state.GetAppDefinition("oidcapp")
	if err != nil {
		t.Fatalf("get stored definition: %v", err)
	}
	if len(storedDef.Services["main"].OIDCClient.AuthorizePaths) == 0 {
		t.Fatalf("candidate manifest was not committed")
	}
	st, err := state.LoadInstallState("oidcapp")
	if err != nil {
		t.Fatalf("load install state: %v", err)
	}
	if st.RawTemplateHash != Sha256Hex(newRaw) {
		t.Fatalf("ledger hash = %q, want committed candidate hash %q", st.RawTemplateHash, Sha256Hex(newRaw))
	}
	txn, err := state.LoadManifestUpdateTransaction("oidcapp")
	if err != nil {
		t.Fatalf("load repair transaction: %v", err)
	}
	if txn.Phase != "publishing_access" || txn.AccessPublished {
		t.Fatalf("repair transaction = phase:%q access_published:%v", txn.Phase, txn.AccessPublished)
	}
	if !strings.Contains(txn.LastError, "proxy registry unavailable") {
		t.Fatalf("transaction last error = %q", txn.LastError)
	}
	detail, err := mgr.Get(context.Background(), "oidcapp")
	if err != nil {
		t.Fatalf("get app detail: %v", err)
	}
	if !detail.AccessRepairPending || !strings.Contains(detail.AccessRepairMessage, "proxy registry unavailable") {
		t.Fatalf("detail access repair = pending:%v message:%q", detail.AccessRepairPending, detail.AccessRepairMessage)
	}
	listed, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	found := false
	for _, app := range listed {
		if app.InstanceID != "oidcapp" {
			continue
		}
		found = true
		if !app.AccessRepairPending || !strings.Contains(app.AccessRepairMessage, "proxy registry unavailable") {
			t.Fatalf("list access repair = pending:%v message:%q", app.AccessRepairPending, app.AccessRepairMessage)
		}
	}
	if !found {
		t.Fatalf("oidcapp missing from list")
	}
}

func TestApplyCustomManifestUpdate_ServiceRemovalPrunesAndCleansRootfs(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	rootfs := newStubRootfsManager(tempDir)
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	mainSvc := baseDef.Services["main"]
	mainSvc.Storage = nil
	baseDef.Services["main"] = mainSvc
	baseDef.Services["worker"] = api.AppService{
		Image:     "docker.io/example/worker:stable",
		BindPorts: []int{},
	}
	candidateDef := customManifestPolicyClone(t, baseDef)
	delete(candidateDef.Services, "worker")
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs: map[string]string{
			"main":                   "rootfs-main",
			"worker":                 "rootfs-worker",
			networkAnchorServiceName: "rootfs-anchor",
		},
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("remove worker"))

	applied, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Applicable {
		t.Fatalf("apply result applicable=false")
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if _, exists := stored.Definition.Services["worker"]; exists {
		t.Fatalf("worker service still present in committed manifest")
	}
	if got := stored.ActiveRootfs["worker"]; got != "" {
		t.Fatalf("removed worker ActiveRootfs = %q, want pruned", got)
	}
	if got := stored.ActiveRootfs["main"]; got != "rootfs-main" {
		t.Fatalf("main ActiveRootfs = %q, want preserved", got)
	}
	if !slices.Contains(rootfs.destroyed, "rootfs-worker") {
		t.Fatalf("destroyed rootfs = %v, want rootfs-worker cleanup", rootfs.destroyed)
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction after committed cleanup err = %v, want not exist", err)
	}
}

func TestInstalledAppApplyTransactionKeepsPreparedListenerReservationAfterPublishFailure(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	serviceManager := services.NewServiceManager()
	serviceManager.UseInMemoryNetworkForTest()
	mgr, err := NewAppManagerForTestWithServices(nil, tempDir, serviceManager, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}

	port := 18080

	now := time.Now().UTC()
	prevDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, prevDef)
	candidateDef.Listeners[0].PortClaim = &port
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     prevDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	prevHash, err := canonicalManifestHash(prevDef)
	if err != nil {
		t.Fatalf("previous hash: %v", err)
	}
	candidateHash, err := canonicalManifestHash(candidateDef)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	txn, err := mgr.beginInstalledAppApplyTransaction(context.Background(), state, installedAppApplyTransactionSpec{
		OperationKind:           "service_app_update",
		TaskType:                taskTypeUpdateServiceApp,
		RollbackPrefix:          "app update rolled back",
		InstanceID:              "piclu",
		AppInst:                 appInst,
		PreviousDefinition:      prevDef,
		CandidateDefinition:     candidateDef,
		PreviousManifestHash:    prevHash,
		CandidateManifestHash:   candidateHash,
		DryRunToken:             "token-listener-publish-failure",
		RuntimeFingerprint:      "runtime-fingerprint",
		MetadataOnly:            true,
		ApplyPhase:              taskPhaseApplyingManifest,
		ApplyMessage:            "Persisting manifest",
		FinalizingMessage:       "Saving config ledger",
		CandidateLedgerRevision: 1,
	})
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() {
		if txn.listenerPlan != nil {
			txn.listenerPlan.Release()
		}
	}()
	if err := txn.prepareListenersIfNeeded(); err != nil {
		t.Fatalf("prepare listeners: %v", err)
	}

	serviceManager.SetTCPListenForTest(func(network, address string) (net.Listener, error) {
		return nil, fmt.Errorf("test bind failure on %s", address)
	})
	publishErr := txn.publishAccess()
	if publishErr == nil {
		t.Fatalf("publish err = nil, want bind failure")
	}
	if !strings.Contains(publishErr.Error(), "publish prepared listeners") {
		t.Fatalf("publish err = %v, want prepared listener publish failure", publishErr)
	}
	_ = txn.markAccessRepairPending(publishErr)

	otherPlan, otherErr := serviceManager.PrepareReconcile("other", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &port,
	}})
	if otherErr == nil {
		otherPlan.Release()
		t.Fatalf("port claim %d was reusable after repair-pending publish failure", port)
	}
	serviceManager.UseInMemoryNetworkForTest()
	if err := serviceManager.RestorePreparedPublication("piclu", txn.txn.PreparedListenerEndpoints); err != nil {
		t.Fatalf("same-process access repair rejected retained prepared endpoints: %v", err)
	}
	if _, ok := serviceManager.GetAppListener("piclu", "piclu"); !ok {
		t.Fatalf("same-process access repair did not publish prepared endpoint")
	}
}

func TestInstalledAppApplyTransactionRetainsPreparedReservationWhenPublishMarkerFails(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	serviceManager := services.NewServiceManager()
	serviceManager.UseInMemoryNetworkForTest()
	mgr, err := NewAppManagerForTestWithServices(nil, tempDir, serviceManager, nil)
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
	candidateDef := customManifestPolicyClone(t, prevDef)
	candidateDef.Listeners[0].Name = "piclu-admin"
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     prevDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	prevHash, err := canonicalManifestHash(prevDef)
	if err != nil {
		t.Fatalf("previous hash: %v", err)
	}
	candidateHash, err := canonicalManifestHash(candidateDef)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	txn, err := mgr.beginInstalledAppApplyTransaction(context.Background(), state, installedAppApplyTransactionSpec{
		OperationKind:           "service_app_update",
		TaskType:                taskTypeUpdateServiceApp,
		RollbackPrefix:          "app update rolled back",
		InstanceID:              "piclu",
		AppInst:                 appInst,
		PreviousDefinition:      prevDef,
		CandidateDefinition:     candidateDef,
		PreviousManifestHash:    prevHash,
		CandidateManifestHash:   candidateHash,
		DryRunToken:             "token-listener-publish-marker-failure",
		RuntimeFingerprint:      "runtime-fingerprint",
		MetadataOnly:            true,
		ApplyPhase:              taskPhaseApplyingManifest,
		ApplyMessage:            "Persisting manifest",
		FinalizingMessage:       "Saving config ledger",
		CandidateLedgerRevision: 1,
	})
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	if err := txn.prepareListenersIfNeeded(); err != nil {
		t.Fatalf("prepare listeners: %v", err)
	}
	if len(txn.txn.PreparedListenerEndpoints) != 1 {
		t.Fatalf("prepared endpoints = %+v, want one", txn.txn.PreparedListenerEndpoints)
	}
	prepared := txn.txn.PreparedListenerEndpoints[0]
	failPublishMarker := true
	state.storeManifestUpdateTransactionHook = func(instanceID string, update *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && update.Phase == "publishing_access" && failPublishMarker {
			failPublishMarker = false
			return os.ErrPermission
		}
		return nil
	}

	publishErr := txn.publishAccess()
	if publishErr == nil {
		t.Fatalf("publishAccess err = nil, want publishing_access marker failure")
	}
	if !strings.Contains(publishErr.Error(), "persist access publication transaction marker") {
		t.Fatalf("publishAccess err = %v, want marker failure", publishErr)
	}
	_ = txn.markAccessRepairPending(publishErr)

	otherPlan, otherErr := serviceManager.PrepareReconcile("other", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      prepared.Flow,
		Protocol:  prepared.Protocol,
		PortClaim: &prepared.PublicPort,
	}})
	if otherErr == nil {
		otherPlan.Release()
		t.Fatalf("prepared public port %d was reusable after publish marker failure", prepared.PublicPort)
	}
	if err := serviceManager.RestorePreparedPublication("piclu", txn.txn.PreparedListenerEndpoints); err != nil {
		t.Fatalf("same-process access repair rejected retained prepared endpoints: %v", err)
	}
	if _, ok := serviceManager.GetAppListener("piclu", prepared.Name); !ok {
		t.Fatalf("same-process access repair did not publish prepared endpoint")
	}
}

func TestInstalledAppApplyTransactionKeepsAccessSuspendedWhenAccessCommitStoreFails(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	serviceManager := services.NewServiceManager()
	serviceManager.UseInMemoryNetworkForTest()
	mgr, err := NewAppManagerForTestWithServices(nil, tempDir, serviceManager, nil)
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
	candidateDef := customManifestPolicyClone(t, prevDef)
	candidateDef.Listeners[0].Name = "piclu-admin"
	candidateDef.Listeners[0].Primary = true
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": "cid-main"},
		ActiveRootfs:   map[string]string{"main": "rootfs-main"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     prevDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	prevHash, err := canonicalManifestHash(prevDef)
	if err != nil {
		t.Fatalf("previous hash: %v", err)
	}
	candidateHash, err := canonicalManifestHash(candidateDef)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	txn, err := mgr.beginInstalledAppApplyTransaction(context.Background(), state, installedAppApplyTransactionSpec{
		OperationKind:           "service_app_update",
		TaskType:                taskTypeUpdateServiceApp,
		RollbackPrefix:          "app update rolled back",
		InstanceID:              "piclu",
		AppInst:                 appInst,
		PreviousDefinition:      prevDef,
		CandidateDefinition:     candidateDef,
		PreviousManifestHash:    prevHash,
		CandidateManifestHash:   candidateHash,
		DryRunToken:             "token-access-published-failure",
		RuntimeFingerprint:      "runtime-fingerprint",
		MetadataOnly:            false,
		ApplyPhase:              taskPhaseApplyingManifest,
		ApplyMessage:            "Persisting manifest",
		FinalizingMessage:       "Saving config ledger",
		CandidateLedgerRevision: 1,
	})
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	if err := txn.prepareListenersIfNeeded(); err != nil {
		t.Fatalf("prepare listeners: %v", err)
	}
	if len(txn.txn.PreparedListenerEndpoints) == 0 {
		t.Fatalf("prepared listener endpoints missing")
	}
	if err := txn.suspendAccessForRuntimeSwitch(); err != nil {
		t.Fatalf("suspend access: %v", err)
	}
	state.storeManifestUpdateTransactionHook = func(instanceID string, update *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && update.Phase == "access_published" {
			return os.ErrPermission
		}
		return nil
	}

	publishErr := txn.publishAccess()
	if publishErr == nil {
		t.Fatalf("publishAccess err = nil, want access_published store failure")
	}
	if !strings.Contains(publishErr.Error(), "persist access published transaction marker") {
		t.Fatalf("publishAccess err = %v, want access_published store failure", publishErr)
	}
	_ = txn.markAccessRepairPending(publishErr)

	storedTxn, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load repair transaction: %v", err)
	}
	if !storedTxn.AccessSuspended {
		t.Fatalf("repair transaction cleared access_suspended after failed access commit")
	}
	if storedTxn.AccessPublished {
		t.Fatalf("repair transaction access_published = true, want false")
	}
	if len(storedTxn.PreparedListenerEndpoints) == 0 {
		t.Fatalf("repair transaction lost prepared listener endpoints")
	}
}

func TestApplyCustomManifestUpdate_RollsBackWhenRuntimeReadinessFails(t *testing.T) {
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
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	readinessCalled := false
	mgr.runtimeReadinessProbe = func(ctx context.Context, endpoints []services.ServiceEndpoint, timeout time.Duration) error {
		_ = ctx
		_ = endpoints
		_ = timeout
		readinessCalled = true
		return errors.New("candidate backend unreachable")
	}

	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil {
		t.Fatalf("apply err = nil, want readiness rollback failure")
	}
	if !strings.Contains(err.Error(), "candidate backend unreachable") {
		t.Fatalf("apply err = %v, want readiness failure", err)
	}
	if !readinessCalled {
		t.Fatalf("readiness probe was not called")
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if stored.Definition.Services["main"].Image != "docker.io/example/piclu:stable" {
		t.Fatalf("readiness failure should restore previous manifest, got image %q", stored.Definition.Services["main"].Image)
	}
	if got := stored.ActiveRootfs["main"]; got != "rootfs-main" {
		t.Fatalf("readiness failure should restore active rootfs, got %q", got)
	}
}

func TestApplyCustomManifestUpdate_CleansSnapshotWhenHealthCheckFailsBeforeCandidateInstall(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		healthErr:         errors.New("snapshot metadata unreadable"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(tempDir))
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot metadata unreadable") {
		t.Fatalf("apply err = %v, want snapshot health failure", err)
	}
	if len(volumes.snapshots) != 1 {
		t.Fatalf("snapshots = %v, want one created snapshot", volumes.snapshots)
	}
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("snapshot rollback = %v, want no rollback before candidate install", volumes.rollbacks)
	}
	if !slices.Contains(volumes.destroyed, volumes.snapshots[0]) {
		t.Fatalf("destroyed snapshots = %v, want cleanup of %s", volumes.destroyed, volumes.snapshots[0])
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); err == nil {
		t.Fatalf("transaction should be cleared after rollback")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
	stored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if stored.Definition.Services["main"].Image != "docker.io/example/piclu:stable" {
		t.Fatalf("snapshot health failure should restore previous manifest, got %q", stored.Definition.Services["main"].Image)
	}
}

func TestRecoverPendingManifestUpdatePublishesPreparedListenerPlanWithoutSuspendedFlag(t *testing.T) {
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
	oldDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, oldDef)
	candidateDef.Listeners[0].Name = "piclu-admin"
	candidateDef.Listeners[0].Primary = true
	oldHash, err := canonicalManifestHash(oldDef)
	if err != nil {
		t.Fatalf("old hash: %v", err)
	}
	candidateHash, err := canonicalManifestHash(candidateDef)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	oldRaw, err := SerializeAppDefinition(oldDef)
	if err != nil {
		t.Fatalf("serialize old def: %v", err)
	}
	candidateRaw, err := SerializeAppDefinition(candidateDef)
	if err != nil {
		t.Fatalf("serialize candidate def: %v", err)
	}
	oldState := NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, map[string]interface{}{"__app_address__": "piclu"}, InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}, nil, false)
	oldState.Revision = 1
	nextState := NewV2InstallState("piclu", InstallSourceKindCustom, "", candidateRaw, oldState.InstallInputs, *oldState.InstallSystemCtx, nil, false)
	nextState.Revision = 2
	appInst := &AppInstance{
		InstanceID:      "piclu",
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "anchor",
		Containers:      map[string]string{"main": "main"},
		ActiveRootfs:    map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      oldDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store old app: %v", err)
	}
	if err := state.StoreInstallState("piclu", oldState); err != nil {
		t.Fatalf("store old install state: %v", err)
	}
	backupPath, err := state.BackupCurrentAppDefinitionForManifestUpdate("piclu")
	if err != nil {
		t.Fatalf("backup manifest: %v", err)
	}
	if err := mgr.serviceManager.RestorePreparedPublication("piclu", []services.ServiceEndpoint{{
		App:                "piclu",
		Name:               "piclu",
		GuestPort:          8080,
		HostBind:           15080,
		PublicPort:         0,
		Flow:               api.FlowTCP,
		Protocol:           api.ListenerProtocolHTTP,
		RequiresTLSMuxAuth: true,
	}}); err != nil {
		t.Fatalf("seed old publication: %v", err)
	}
	appInst.Definition = candidateDef
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store candidate app: %v", err)
	}
	if err := state.StoreInstallState("piclu", nextState); err != nil {
		t.Fatalf("store candidate install state: %v", err)
	}
	prepared := []services.ServiceEndpoint{{
		App:                "piclu",
		Name:               "piclu-admin",
		GuestPort:          8080,
		HostBind:           15080,
		PublicPort:         0,
		Flow:               api.FlowTCP,
		Protocol:           api.ListenerProtocolHTTP,
		RequiresTLSMuxAuth: true,
	}}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:               "op-listener-repair",
		OperationKind:             "service_app_update",
		Phase:                     "publishing_access",
		PreviousManifestHash:      oldHash,
		CandidateManifestHash:     candidateHash,
		PreviousLedgerRevision:    oldState.Revision,
		CandidateLedgerRevision:   nextState.Revision,
		PreviousLedgerSourceHash:  oldState.RawTemplateHash,
		CandidateLedgerSourceHash: nextState.RawTemplateHash,
		DryRunToken:               "token",
		RuntimeFingerprint:        "fingerprint",
		BackupPath:                backupPath,
		PreparedListenerEndpoints: prepared,
		AccessSuspended:           false,
		RuntimeTouched:            true,
		CreatedAt:                 now,
		UpdatedAt:                 now,
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("recovery should publish prepared listener plan without blocking")
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); err == nil {
		t.Fatalf("transaction should be cleared after recovery")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
	endpoints, err := mgr.serviceManager.GetByApp("piclu")
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0].Name != "piclu-admin" {
		t.Fatalf("recovered endpoints = %+v, want prepared candidate listener", endpoints)
	}
}

func TestApplyCustomManifestUpdate_CleansCreatedRootfsOnStagingRollback(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	rootfs := newStubRootfsManager(tempDir)
	rootfs.exists = map[string]bool{
		"rootfs-main":             true,
		networkAnchorServiceName:  true,
		"rootfs-anchor":           true,
		"svc-rootfs-piclu--main":  true,
		"svc-rootfs-piclu--admin": true,
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))
	wantRootfs := persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:mockdigest"))
	sawRootfsStagingMarker := false
	state.storeManifestUpdateTransactionHook = func(instanceID string, txn *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && txn.Phase == "rootfs_staging" && slices.Contains(txn.CreatedRootfs, wantRootfs) {
			sawRootfsStagingMarker = true
		}
		if instanceID == "piclu" && txn.Phase == "rootfs_staged" {
			return os.ErrPermission
		}
		return nil
	}

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "persist staged rootfs transaction marker") {
		t.Fatalf("apply err = %v, want staged rootfs persistence failure", err)
	}
	if !sawRootfsStagingMarker {
		t.Fatalf("created rootfs cleanup marker was not persisted before rootfs_staged")
	}
	if !slices.Contains(rootfs.detached, wantRootfs) {
		t.Fatalf("detached rootfs = %v, want %s", rootfs.detached, wantRootfs)
	}
	if !slices.Contains(rootfs.destroyed, wantRootfs) {
		t.Fatalf("destroyed rootfs = %v, want %s", rootfs.destroyed, wantRootfs)
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); err == nil {
		t.Fatalf("transaction should be cleared after successful rollback")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
}

func TestApplyCustomManifestUpdate_DoesNotDetachPreviousActiveRootfsOnSameDigestRollback(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	wantRootfs := persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:mockdigest"))
	rootfs := newStubRootfsManager(tempDir)
	rootfs.exists = map[string]bool{
		wantRootfs:      true,
		"rootfs-anchor": true,
	}
	mgr.SetRootfsManager(rootfs)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": wantRootfs, networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:alias"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("same digest image update"))
	state.storeManifestUpdateTransactionHook = func(instanceID string, txn *ManifestUpdateTransaction) error {
		if instanceID == "piclu" && txn.Phase == "rootfs_staged" {
			return os.ErrPermission
		}
		return nil
	}

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "persist staged rootfs transaction marker") {
		t.Fatalf("apply err = %v, want staged rootfs persistence failure", err)
	}
	if slices.Contains(rootfs.detached, wantRootfs) {
		t.Fatalf("previous active rootfs %s was detached during rollback: %v", wantRootfs, rootfs.detached)
	}
	if slices.Contains(rootfs.destroyed, wantRootfs) {
		t.Fatalf("previous active rootfs %s was destroyed during rollback: %v", wantRootfs, rootfs.destroyed)
	}
}

func TestApplyCustomManifestUpdate_PreservesCommittedCleanupWhenSnapshotDestroyFails(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		destroyErr:        errors.New("thin snapshot busy"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(tempDir))
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err != nil {
		t.Fatalf("apply should commit despite cleanup retry marker: %v", err)
	}
	txn, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load cleanup-pending transaction: %v", err)
	}
	if txn.Phase != "committed_cleanup_pending" {
		t.Fatalf("phase = %q, want committed_cleanup_pending", txn.Phase)
	}
	if txn.PrecommitDataSnapshotID == "" {
		t.Fatalf("snapshot id should be preserved for retry")
	}
	volumes.destroyErr = nil
	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("cleanup-pending app should not be blocked")
	}
	if _, err := state.LoadManifestUpdateTransaction("piclu"); err == nil {
		t.Fatalf("transaction should be cleared after cleanup retry")
	} else if !os.IsNotExist(err) {
		t.Fatalf("load transaction: %v", err)
	}
	if !slices.Contains(volumes.destroyed, txn.PrecommitDataSnapshotID) {
		t.Fatalf("destroyed snapshots = %v, want %s", volumes.destroyed, txn.PrecommitDataSnapshotID)
	}
}

func TestBeginInstalledAppApplyTransactionRejectsPendingCleanup(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		destroyErr:        errors.New("thin snapshot busy"),
	}
	mgr.SetVolumeManager(volumes)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	def := customManifestPolicyBaseDef()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:             "op-cleanup",
		OperationKind:           "service_app_update",
		Phase:                   "committed_cleanup_pending",
		PrecommitDataSnapshotID: "snap-app-piclu--manifest-op-cleanup",
		CreatedAt:               now,
		UpdatedAt:               now,
	}); err != nil {
		t.Fatalf("store cleanup-pending transaction: %v", err)
	}

	_, err = mgr.beginInstalledAppApplyTransaction(context.Background(), state, installedAppApplyTransactionSpec{
		OperationKind:         "service_app_update",
		InstanceID:            "piclu",
		AppInst:               appInst,
		PreviousDefinition:    def,
		CandidateDefinition:   def,
		PreviousManifestHash:  "old",
		CandidateManifestHash: "new",
	})
	if !errors.Is(err, ErrManifestUpdateConflict) || !strings.Contains(err.Error(), "previous update cleanup is still pending") {
		t.Fatalf("begin transaction err = %v, want cleanup-pending conflict", err)
	}
	txn, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load cleanup-pending transaction: %v", err)
	}
	if txn.Phase != "committed_cleanup_pending" {
		t.Fatalf("phase = %q, want committed_cleanup_pending", txn.Phase)
	}
}

func TestTransitionFollowUpReportsLegacyManifestCleanupStillPending(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mgr, err := NewAppManagerForTest(NewMockContainerManager(), tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.ForceLockState(false)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		destroyErr:        errors.New("thin snapshot busy"),
	}
	mgr.SetVolumeManager(volumes)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	def := customManifestPolicyBaseDef()
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:             "op-cleanup",
		OperationKind:           "service_app_update",
		Phase:                   "committed_cleanup_pending",
		PrecommitDataSnapshotID: "snap-app-piclu--manifest-op-cleanup",
		CreatedAt:               now,
		UpdatedAt:               now,
	}); err != nil {
		t.Fatalf("store cleanup-pending transaction: %v", err)
	}
	record := transitionTestRecord("piclu", TransitionPhaseCommittedCleanupPending)
	if err := state.StoreTransitionRecord("piclu", record); err != nil {
		t.Fatalf("store transition: %v", err)
	}

	err = mgr.RetryTransitionFollowUp(context.Background(), "piclu", TransitionActionFinishCleanup)
	if !errors.Is(err, ErrTransitionFollowUpUnavailable) || !strings.Contains(err.Error(), "thin snapshot busy") {
		t.Fatalf("follow-up err = %v, want pending cleanup failure", err)
	}
	txn, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load cleanup-pending transaction: %v", err)
	}
	if txn.Phase != "committed_cleanup_pending" || !strings.Contains(txn.LastError, "thin snapshot busy") {
		t.Fatalf("transaction = phase:%q err:%q, want cleanup pending", txn.Phase, txn.LastError)
	}
}

func TestApplyCustomManifestUpdate_DataImpactRequiresPrivateSnapshotCapability(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}

	svc := candidateDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("env update"))
	if cand.Classification.DataSafety == nil || !cand.Classification.DataSafety.SnapshotRequired {
		t.Fatalf("expected snapshot-required candidate, got %+v", cand.Classification.DataSafety)
	}
	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "precommit data snapshot required") {
		t.Fatalf("apply err = %v, want private snapshot capability rejection", err)
	}
	storedDef, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get stored def: %v", err)
	}
	if _, exists := storedDef.Services["main"].Environment["PICLU_DEVICE_DIAG_DIR"]; exists {
		t.Fatalf("rollback should restore previous environment, got %+v", storedDef.Services["main"].Environment)
	}
}

func TestApplyCustomManifestUpdate_RejectsWhenSnapshotViabilityFails(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		viabilityErr:      errors.New("thin pool metadata usage 91.0% exceeds threshold"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(tempDir))
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	oldContainerID := "cid-old-main"
	mock.containers[oldContainerID] = &mockContainer{ID: oldContainerID, Status: "running", Spec: container.ContainerCreateSpec{Name: "piclu-main"}}
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		Containers:     map[string]string{"main": oldContainerID},
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if _, _, err := mgr.serviceManager.Reconcile("piclu", baseDef.Listeners); err != nil {
		t.Fatalf("seed listener publication: %v", err)
	}
	svc := candidateDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("env update"))

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "precommit data snapshot viability") {
		t.Fatalf("apply err = %v, want snapshot viability rejection", err)
	}
	if len(volumes.snapshots) != 0 {
		t.Fatalf("snapshot should not be created after failed viability gate, got %v", volumes.snapshots)
	}
	storedApp, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("stored app missing")
	}
	if got := strings.TrimSpace(storedApp.Containers["main"]); got != oldContainerID {
		t.Fatalf("viability preflight should not replace running container metadata, got %q want %q", got, oldContainerID)
	}
	if oldContainer, exists := mock.containers[oldContainerID]; !exists {
		t.Fatalf("old container %q missing from mock runtime", oldContainerID)
	} else if oldContainer.Status != "running" {
		t.Fatalf("viability preflight should leave old container running, status=%q", oldContainer.Status)
	}
	if _, ok := mgr.serviceManager.GetAppListener("piclu", "piclu"); !ok {
		t.Fatalf("old listener publication missing after snapshot viability gate")
	}
	storedDef, err := state.GetAppDefinition("piclu")
	if err != nil {
		t.Fatalf("get stored def: %v", err)
	}
	if _, exists := storedDef.Services["main"].Environment["PICLU_DEVICE_DIAG_DIR"]; exists {
		t.Fatalf("rollback should restore previous environment, got %+v", storedDef.Services["main"].Environment)
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
		TransitionPlanHash: dryRun.TransitionPlanHash,
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

func TestDryRunCustomManifestUpdateSurfacesKeptSecretSemanticReview(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	oldRaw := customManifestKeptSecretRaw("Gemini API key", "Key for Gemini extraction", "PICLU_API_KEY")
	newRaw := customManifestKeptSecretRaw("OpenAI compatible API key", "Key for OpenAI-compatible extraction", "OPENAI_COMPATIBLE_API_KEY")
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"gemini_api_key":  "stored-secret",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dryRun.KeptValueReview) != 1 {
		t.Fatalf("kept value review = %+v, want one item", dryRun.KeptValueReview)
	}
	item := dryRun.KeptValueReview[0]
	if item.Field != "gemini_api_key" {
		t.Fatalf("field = %q, want gemini_api_key", item.Field)
	}
	if item.RiskKind != "kept_secret_semantic_changed" {
		t.Fatalf("risk kind = %q", item.RiskKind)
	}
	if item.Confirmation == "" || !slices.Contains(dryRun.RequiredConfirmations, item.Confirmation) {
		t.Fatalf("required confirmations %v do not include kept value confirmation %q", dryRun.RequiredConfirmations, item.Confirmation)
	}
	if !slices.Contains(item.SemanticDelta, "label changed") || !slices.Contains(item.SemanticDelta, "description changed") || !slices.Contains(item.SemanticDelta, "template usage changed") {
		t.Fatalf("semantic delta = %+v", item.SemanticDelta)
	}
	if len(item.OldUsage) == 0 || len(item.NewUsage) == 0 {
		t.Fatalf("usage refs missing: old=%v new=%v", item.OldUsage, item.NewUsage)
	}
	if item.BlockingReason != "" {
		t.Fatalf("confirmable kept value review should not have blocking reason: %+v", item)
	}
}

func TestApplyCustomManifestUpdateKeepsReclassifiedSecretWriteOnly(t *testing.T) {
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
	oldRaw := customManifestReclassifiedSecretRaw("password")
	newRaw := customManifestReclassifiedSecretRaw("string")
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"license":         "zzzz-license",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dryRun.KeptValueReview) != 1 {
		t.Fatalf("kept value review = %+v, want one item", dryRun.KeptValueReview)
	}
	if dryRun.KeptValueReview[0].RiskKind != "kept_secret_semantic_changed" {
		t.Fatalf("risk kind = %q", dryRun.KeptValueReview[0].RiskKind)
	}
	if !slices.Contains(dryRun.RequiredConfirmations, dryRun.KeptValueReview[0].Confirmation) {
		t.Fatalf("required confirmations %v missing kept value confirmation %q", dryRun.RequiredConfirmations, dryRun.KeptValueReview[0].Confirmation)
	}

	if _, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   dryRun.BaseManifestHash,
		RuntimeFingerprint: dryRun.RuntimeFingerprint,
		DryRunToken:        dryRun.DryRunToken,
		TransitionPlanHash: dryRun.TransitionPlanHash,
		Confirmations:      dryRun.RequiredConfirmations,
	}); err != nil {
		t.Fatalf("apply manifest update: %v", err)
	}
	nextLedger, err := state.LoadInstallState("piclu")
	if err != nil {
		t.Fatalf("load updated install state: %v", err)
	}
	if !nextLedger.InputSensitive["license"] {
		t.Fatalf("updated ledger did not preserve sticky sensitivity: %+v", nextLedger.InputSensitive)
	}

	read, err := mgr.ReadInstalledConfig(context.Background(), "piclu")
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	var licenseField *InstalledConfigField
	for i := range read.Fields {
		if read.Fields[i].Name == "license" {
			licenseField = &read.Fields[i]
			break
		}
	}
	if licenseField == nil {
		t.Fatalf("license field missing from read config: %+v", read.Fields)
	}
	if !licenseField.Sensitive || licenseField.Display != nil || licenseField.Schema != nil {
		t.Fatalf("reclassified stored secret should remain write-only after apply: %+v", licenseField)
	}
	if strings.Contains(fmt.Sprint(read), "secret-license") {
		t.Fatalf("read installed config leaked reclassified secret: %+v", read)
	}
}

func TestDryRunCustomManifestUpdateSurfacesKeptValueDefaultReview(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  provider:
    type: string
    required: true
    default: local
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
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  provider:
    type: string
    required: true
    default: cloud
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
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"provider":        "local",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dryRun.KeptValueReview) != 1 {
		t.Fatalf("kept value review = %+v, want one item", dryRun.KeptValueReview)
	}
	item := dryRun.KeptValueReview[0]
	if item.Field != "provider" {
		t.Fatalf("field = %q, want provider", item.Field)
	}
	if !slices.Contains(item.SemanticDelta, "default changed") {
		t.Fatalf("semantic delta = %+v, want default changed", item.SemanticDelta)
	}
	if item.RiskKind != "kept_value_semantic_changed" {
		t.Fatalf("risk kind = %q, want kept_value_semantic_changed", item.RiskKind)
	}
}

func TestDryRunCustomManifestUpdateRedactsSensitiveDefaultReview(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  api_key:
    type: string
    required: true
    default: old-secret-default
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
      API_KEY: "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  api_key:
    type: string
    required: true
    default: new-secret-default
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
      API_KEY: "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"api_key":         "stored-secret",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dryRun.KeptValueReview) != 1 {
		t.Fatalf("kept value review = %+v, want one item", dryRun.KeptValueReview)
	}
	item := dryRun.KeptValueReview[0]
	if item.Field != "api_key" {
		t.Fatalf("field = %q, want api_key", item.Field)
	}
	if item.RiskKind != "kept_secret_semantic_changed" {
		t.Fatalf("risk kind = %q, want kept_secret_semantic_changed", item.RiskKind)
	}
	if !slices.Contains(item.SemanticDelta, "default changed") {
		t.Fatalf("semantic delta = %+v, want default changed", item.SemanticDelta)
	}
	for _, semantic := range append(append([]string{}, item.OldSemantic...), item.NewSemantic...) {
		if strings.Contains(semantic, "old-secret-default") || strings.Contains(semantic, "new-secret-default") {
			t.Fatalf("sensitive default leaked in semantic summary: old=%v new=%v", item.OldSemantic, item.NewSemantic)
		}
	}
	if !slices.Contains(item.OldSemantic, "default=<redacted>") || !slices.Contains(item.NewSemantic, "default=<redacted>") {
		t.Fatalf("semantic summary = old=%v new=%v, want redacted defaults", item.OldSemantic, item.NewSemantic)
	}
}

func TestDryRunCustomManifestUpdateRedactsRenderedSemanticMetadata(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  api_key:
    type: password
    required: true
  provider:
    type: string
    required: true
    label: "Provider {{ .Inputs.api_key }}"
    description: "Provider secret {{ .Inputs.api_key }}"
    validation:
      regex: "^.*$"
      message: "Must match {{ .Inputs.api_key }}"
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
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  api_key:
    type: password
    required: true
  provider:
    type: string
    required: true
    label: "Provider changed {{ .Inputs.api_key }}"
    description: "Provider secret changed {{ .Inputs.api_key }}"
    validation:
      regex: "^.+$"
      message: "Must match changed {{ .Inputs.api_key }}"
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
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"api_key":         "stored-secret",
		"provider":        "local",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var item *ManifestUpdateKeptValueReviewItem
	for i := range dryRun.KeptValueReview {
		if dryRun.KeptValueReview[i].Field == "provider" {
			item = &dryRun.KeptValueReview[i]
			break
		}
	}
	if item == nil {
		t.Fatalf("kept value review = %+v, want provider item", dryRun.KeptValueReview)
	}
	for _, want := range []string{"label changed", "description changed", "validation changed"} {
		if !slices.Contains(item.SemanticDelta, want) {
			t.Fatalf("semantic delta = %+v, want %q", item.SemanticDelta, want)
		}
	}
	allSemantic := strings.Join(append(append([]string{}, item.OldSemantic...), item.NewSemantic...), "\n")
	if strings.Contains(allSemantic, "stored-secret") {
		t.Fatalf("semantic summary leaked stored secret: old=%v new=%v", item.OldSemantic, item.NewSemantic)
	}
	for _, want := range []string{"label=<present>", "description=<present>", "validation_regex=<present>", "validation_message=<present>"} {
		if !strings.Contains(allSemantic, want) {
			t.Fatalf("semantic summary = %q, want %q", allSemantic, want)
		}
	}
}

func TestDryRunCustomManifestUpdateRejectsRenderedOnlySecretDerivedInputSpecs(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  api_key:
    type: password
    required: true
{{ if .Inputs.api_key }}
  provider:
    type: string
    required: true
    default: "{{ .Inputs.api_key }}"
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
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  api_key:
    type: password
    required: true
{{ if .Inputs.api_key }}
  provider:
    type: string
    required: true
    default: "changed-{{ .Inputs.api_key }}"
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
      PROVIDER: "{{ .Inputs.provider }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"api_key":         "stored-secret",
		"provider":        "local",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	_, err = mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err == nil {
		t.Fatalf("expected secret-derived rendered input specs to fail closed")
	}
	if strings.Contains(err.Error(), "stored-secret") {
		t.Fatalf("dry-run error leaked stored secret: %v", err)
	}
}

func TestDryRunCustomManifestUpdateRejectsStickySensitiveStructuralKeys(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  license:
    type: password
    required: true
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
      SAFE_KEY: "1"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  license:
    type: password
    required: true
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
      SAFE_KEY: "1"
      '{{ printf "%.4s" .Inputs.license }}_KEY': "1"
      '{{ printf "%x" .Inputs.license }}_HEX': "1"
      '{{ printf "%q" .Inputs.license }}_QUOTE': "1"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"license":         "secret-license",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dryRun.Applicable || dryRun.DryRunToken != "" {
		t.Fatalf("dry run should reject sensitive structural keys without token, got applicable=%v token=%q result=%+v", dryRun.Applicable, dryRun.DryRunToken, dryRun)
	}
	if !strings.Contains(dryRun.BlockingReason, "sensitive or generated values cannot be used in manifest structure") {
		t.Fatalf("blocking reason = %q", dryRun.BlockingReason)
	}
	joined := fmt.Sprint(dryRun)
	for _, leaked := range []string{"zzzz-license", "zzzz_KEY", "services.zzzz", "zzzz", schemaInputNameSentinelPrefix, fmt.Sprintf("%x", "zzzz-license"), fmt.Sprintf("%q", "zzzz-license")} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("dry-run result leaked %q: %+v", leaked, dryRun)
		}
	}
}

func TestDryRunCustomManifestUpdateRedactsSensitiveRenderValidationError(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  license:
    type: password
    required: true
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
      SAFE_KEY: "1"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  license:
    type: password
    required: true
listeners:
  - name: __primary
    guest_port: 8080
    flow: tcp
    protocol: http
primary_service: "{{ .Inputs.license }}"
services:
  "{{ .Inputs.license }}":
    image: docker.io/example/piclu:stable
    bind_ports: [8080]
    environment:
      SAFE_KEY: "1"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"__app_address__": "piclu",
		"license":         "Bad.Secret",
	}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    inputs,
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, inputs, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run returned raw error: %v", err)
	}
	if dryRun.Applicable || dryRun.DryRunToken != "" {
		t.Fatalf("dry run should reject unsafe render failure without token, got applicable=%v token=%q result=%+v", dryRun.Applicable, dryRun.DryRunToken, dryRun)
	}
	if !strings.Contains(dryRun.BlockingReason, "sensitive or generated values cannot be used in manifest structure") {
		t.Fatalf("blocking reason = %q", dryRun.BlockingReason)
	}
	if strings.Contains(fmt.Sprint(dryRun), "Bad.Secret") {
		t.Fatalf("dry-run result leaked rendered secret: %+v", dryRun)
	}
}

func TestManifestRenderedInputUsageRefsRedactsSecretRenderedKeys(t *testing.T) {
	raw := []byte(`type: user
inputs:
  api_key:
    type: password
  provider:
    type: string
metadata:
  "{{ .System.Auth.ClientSecret }}":
    "{{ .Inputs.provider }}": "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	systemCtx := &InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC", IssuerHint: "https://issuer.local"}
	refs, ok := manifestRenderedInputUsageRefs(raw, map[string]interface{}{
		"api_key":  "stored-api-key",
		"provider": "rendered-provider",
	}, systemCtx, &OIDCCredentials{
		ClientID:     "client-id",
		ClientSecret: "oidc-client-secret",
	}, "api_key", map[string]struct{}{"provider": {}})
	if !ok || len(refs) == 0 {
		t.Fatalf("usage refs = %v ok=%v, want rendered refs", refs, ok)
	}
	for _, ref := range refs {
		if strings.Contains(ref, "oidc-client-secret") || strings.Contains(ref, "rendered-provider") || strings.Contains(ref, "stored-api-key") {
			t.Fatalf("usage ref leaked rendered secret/input value: %q", ref)
		}
	}
	if !strings.Contains(strings.Join(refs, " "), "<key") {
		t.Fatalf("usage refs = %v, want structural key labels", refs)
	}
}

func TestManifestRenderedInputUsageRefsRedactsOIDCClientIDKeys(t *testing.T) {
	raw := []byte(`type: user
inputs:
  api_key:
    type: password
metadata:
  "{{ .System.Auth.ClientID }}":
    value: "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	systemCtx := &InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC", IssuerHint: "https://issuer.local"}
	refs, ok := manifestRenderedInputUsageRefs(raw, map[string]interface{}{
		"api_key": "stored-api-key",
	}, systemCtx, &OIDCCredentials{
		ClientID:     "oidc-client-id",
		ClientSecret: "oidc-client-secret",
	}, "api_key", nil)
	if !ok || len(refs) == 0 {
		t.Fatalf("usage refs = %v ok=%v, want rendered refs", refs, ok)
	}
	joined := strings.Join(refs, " ")
	for _, leaked := range []string{"oidc-client-id", "oidc-client-secret", "stored-api-key"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("usage refs leaked %q: %v", leaked, refs)
		}
	}
	if !strings.Contains(joined, "<key") || !strings.Contains(joined, "#") {
		t.Fatalf("usage refs = %v, want hashed structural key labels", refs)
	}
}

func TestManifestRenderedInputUsageRefsRedactsPartialUnsafeInputKeys(t *testing.T) {
	raw := []byte(`type: user
inputs:
  api_key:
    type: password
  license:
    type: string
metadata:
  "{{ printf "%.4s" .Inputs.license }}":
    value: "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	systemCtx := &InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	refs, ok := manifestRenderedInputUsageRefs(raw, map[string]interface{}{
		"api_key": "stored-api-key",
		"license": "secret-license",
	}, systemCtx, nil, "api_key", map[string]struct{}{"license": {}})
	if !ok || len(refs) == 0 {
		t.Fatalf("usage refs = %v ok=%v, want rendered refs", refs, ok)
	}
	joined := strings.Join(refs, " ")
	for _, leaked := range []string{"secret-license", "secr", "stored-api-key"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("usage refs leaked %q: %v", leaked, refs)
		}
	}
	if !strings.Contains(joined, "<key") || !strings.Contains(joined, "#") {
		t.Fatalf("usage refs = %v, want hashed structural key labels", refs)
	}
}

func TestManifestRenderedInputUsageRefsPreserveStaticKeyPrecision(t *testing.T) {
	oldRaw := []byte(`type: user
inputs:
  api_key:
    type: password
services:
  main:
    image: docker.io/example/piclu:stable
    environment:
      OLD_KEY: |
        {{ .Inputs.api_key }}
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  api_key:
    type: password
services:
  main:
    image: docker.io/example/piclu:stable
    environment:
      NEW_KEY: |
        {{ .Inputs.api_key }}
x-piccolo:
  mode: service
`)
	systemCtx := &InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{"api_key": "stored-api-key"}
	oldRefs, oldOK := manifestRenderedInputUsageRefs(oldRaw, inputs, systemCtx, nil, "api_key", nil)
	newRefs, newOK := manifestRenderedInputUsageRefs(newRaw, inputs, systemCtx, nil, "api_key", nil)
	if !oldOK || !newOK {
		t.Fatalf("usage refs ok = old:%v new:%v", oldOK, newOK)
	}
	if slices.Equal(oldRefs, newRefs) {
		t.Fatalf("usage refs should differ when static parent key changes: old=%v new=%v", oldRefs, newRefs)
	}
	if !strings.Contains(strings.Join(oldRefs, " "), "OLD_KEY") || !strings.Contains(strings.Join(newRefs, " "), "NEW_KEY") {
		t.Fatalf("usage refs did not preserve static key names: old=%v new=%v", oldRefs, newRefs)
	}
	if strings.Contains(strings.Join(append(oldRefs, newRefs...), " "), "stored-api-key") {
		t.Fatalf("usage refs leaked stored value: old=%v new=%v", oldRefs, newRefs)
	}
}

func TestManifestRenderedInputUsageRefsPreserveRedactedDynamicKeyPrecision(t *testing.T) {
	oldRaw := []byte(`type: user
inputs:
  api_key:
    type: password
  provider:
    type: string
services:
  main:
    image: docker.io/example/piclu:stable
    environment:
      "OLD_{{ .Inputs.provider }}": |
        {{ .Inputs.api_key }}
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  api_key:
    type: password
  provider:
    type: string
services:
  main:
    image: docker.io/example/piclu:stable
    environment:
      "NEW_{{ .Inputs.provider }}": |
        {{ .Inputs.api_key }}
x-piccolo:
  mode: service
`)
	systemCtx := &InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	inputs := map[string]interface{}{
		"api_key":  "stored-api-key",
		"provider": "rendered-provider",
	}
	oldRefs, oldOK := manifestRenderedInputUsageRefs(oldRaw, inputs, systemCtx, nil, "api_key", map[string]struct{}{"provider": {}})
	newRefs, newOK := manifestRenderedInputUsageRefs(newRaw, inputs, systemCtx, nil, "api_key", map[string]struct{}{"provider": {}})
	if !oldOK || !newOK {
		t.Fatalf("usage refs ok = old:%v new:%v", oldOK, newOK)
	}
	if slices.Equal(oldRefs, newRefs) {
		t.Fatalf("usage refs should differ when dynamic parent key prefix changes: old=%v new=%v", oldRefs, newRefs)
	}
	joined := strings.Join(append(oldRefs, newRefs...), " ")
	for _, leaked := range []string{"stored-api-key", "rendered-provider", "OLD_rendered-provider", "NEW_rendered-provider"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("usage refs leaked rendered value %q: old=%v new=%v", leaked, oldRefs, newRefs)
		}
	}
	if !strings.Contains(joined, "<key") || !strings.Contains(joined, "#") {
		t.Fatalf("usage refs = old:%v new:%v, want redacted key identity hash", oldRefs, newRefs)
	}
}

func TestDryRunCustomManifestUpdateFailsClosedOnAmbiguousKeptSecretReuse(t *testing.T) {
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
	oldRaw := []byte(`type: user
inputs:
  gemini_api_key:
    type: password
    required: true
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
	newRaw := []byte(`type: user
inputs:
  gemini_api_key:
    type: password
    required: true
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
      NEW_GEMINI_API_KEY: "{{ .Inputs.gemini_api_key }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    map[string]interface{}{"__app_address__": "piclu", "gemini_api_key": "stored-secret"},
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	installState := NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, map[string]interface{}{"__app_address__": "piclu", "gemini_api_key": "stored-secret"}, systemCtx, nil, false)
	installState.RawTemplate = []byte("not: [valid")
	installState.RawTemplateHash = Sha256Hex(installState.RawTemplate)
	if err := state.StoreInstallState("piclu", installState); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dryRun.Applicable {
		t.Fatalf("dry run should reject ambiguous kept secret reuse: %+v", dryRun)
	}
	if !strings.Contains(dryRun.BlockingReason, "previous source schema unavailable") {
		t.Fatalf("blocking reason = %q, want previous source schema unavailable", dryRun.BlockingReason)
	}
	if len(dryRun.KeptValueReview) != 1 {
		t.Fatalf("kept value review = %+v, want one item", dryRun.KeptValueReview)
	}
	if dryRun.KeptValueReview[0].RiskKind != "kept_secret_semantic_changed" {
		t.Fatalf("risk kind = %q, want kept_secret_semantic_changed", dryRun.KeptValueReview[0].RiskKind)
	}
}

func TestDryRunCustomManifestUpdateSkipsKeptReviewForClearedStoredSecret(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	oldRaw := []byte(`type: user
inputs:
  api_key:
    type: password
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
      API_KEY: "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	newRaw := []byte(`type: user
inputs:
  api_key:
    type: string
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
      API_KEY: "{{ .Inputs.api_key }}"
x-piccolo:
  mode: service
`)
	systemCtx := InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"}
	oldRes, err := RunInstallPipeline(context.Background(), InstallPipelineInput{
		RawTemplate:   oldRaw,
		UserInputs:    map[string]interface{}{"__app_address__": "piclu", "api_key": "stored-secret"},
		SystemContext: systemCtx,
		InstanceID:    "piclu",
	}, nil, nil)
	if err != nil {
		t.Fatalf("render old manifest: %v", err)
	}
	now := time.Now().UTC()
	if err := state.StoreApp(&AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     oldRes.Definition,
	}); err != nil {
		t.Fatalf("store app: %v", err)
	}
	if err := state.StoreInstallState("piclu", NewV2InstallState("piclu", InstallSourceKindCustom, "", oldRaw, map[string]interface{}{"__app_address__": "piclu", "api_key": "stored-secret"}, systemCtx, nil, false)); err != nil {
		t.Fatalf("store install state: %v", err)
	}

	dryRun, err := mgr.DryRunCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:    "piclu",
		RawTemplate:   newRaw,
		ClearInputs:   []string{"api_key"},
		SystemContext: systemCtx,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dryRun.KeptValueReview) != 0 {
		t.Fatalf("kept value review = %+v, want none for cleared value", dryRun.KeptValueReview)
	}
	if slices.Contains(dryRun.Summary.WillPreserve, "current value for input api_key after review") {
		t.Fatalf("summary incorrectly preserves cleared api_key: %+v", dryRun.Summary.WillPreserve)
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
		InstanceID:          "piclu",
		Enabled:             false,
		PrimaryService:      "main",
		CatalogSource:       "piclu",
		CatalogManifestHash: "old-catalog-template-hash",
		LastSyncError:       "previous catalog drift",
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          prevDef,
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

func TestRecoverPendingManifestUpdate_RestoresPrecommitDataSnapshot(t *testing.T) {
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

	now := time.Now().UTC()
	prevDef := customManifestPolicyBaseDef()
	app := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-old", networkAnchorServiceName: "rootfs-anchor"},
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
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	app.Definition = candidateDef
	app.ActiveRootfs = map[string]string{"main": "rootfs-new", networkAnchorServiceName: "rootfs-anchor"}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store candidate app: %v", err)
	}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:             "op-recovery",
		OperationKind:           "manifest_update",
		Phase:                   "recreating_runtime",
		BackupPath:              backupPath,
		CreatedAt:               now,
		UpdatedAt:               now,
		DryRunToken:             "token",
		PreviousManifestHash:    "old",
		CandidateManifestHash:   "new",
		PreviousActiveRootfs:    map[string]string{"main": "rootfs-old", networkAnchorServiceName: "rootfs-anchor"},
		CandidateActiveRootfs:   map[string]string{"main": "rootfs-new", networkAnchorServiceName: "rootfs-anchor"},
		PrecommitDataSnapshotID: "snap-app-piclu--manifest-test",
		FailedDataLVName:        "vol-app-piclu--failed-manifest-test",
		RuntimeTouched:          true,
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}
	if err := state.StoreTransitionRecord("piclu", transitionTestRecord("piclu", TransitionPhaseCandidateTouched)); err != nil {
		t.Fatalf("store transition record: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("piclu should recover successfully")
	}
	if _, err := state.LoadTransitionRecord("piclu"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition record err = %v, want cleared", err)
	}
	if len(volumes.rollbacks) != 1 || volumes.rollbacks[0] != "snap-app-piclu--manifest-test->vol-app-piclu--failed-manifest-test" {
		t.Fatalf("rollbacks = %v", volumes.rollbacks)
	}
	restored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("restored app missing")
	}
	if got := restored.ActiveRootfs["main"]; got != "rootfs-old" {
		t.Fatalf("active rootfs main = %q, want rootfs-old", got)
	}
	if restored.Definition.Services["main"].Image != "docker.io/example/piclu:stable" {
		t.Fatalf("restored image = %q", restored.Definition.Services["main"].Image)
	}
	tupleState, err := state.LoadTupleState("piclu")
	if err != nil {
		t.Fatalf("load tuple state: %v", err)
	}
	if tupleState == nil {
		t.Fatalf("tuple state missing")
	}
	foundFailedLV := false
	for _, gen := range tupleState.Generations {
		if gen.FailedLVName != "vol-app-piclu--failed-manifest-test" {
			continue
		}
		foundFailedLV = true
		if gen.Status != TupleStatusFailed {
			t.Fatalf("failed LV generation status = %q, want failed", gen.Status)
		}
	}
	if !foundFailedLV {
		t.Fatalf("failed data LV was not tracked for GC: %+v", tupleState.Generations)
	}
}

func TestRecoverPendingManifestUpdate_RestoresPrecommitDataSnapshotWhenDisabled(t *testing.T) {
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

	now := time.Now().UTC()
	prevDef := customManifestPolicyBaseDef()
	app := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-old", networkAnchorServiceName: "rootfs-anchor"},
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
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	app.Enabled = false
	app.Definition = candidateDef
	app.ActiveRootfs = map[string]string{"main": "rootfs-new", networkAnchorServiceName: "rootfs-anchor"}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store disabled candidate app: %v", err)
	}
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:             "op-recovery-disabled",
		OperationKind:           "manifest_update",
		Phase:                   "recreating_runtime",
		BackupPath:              backupPath,
		CreatedAt:               now,
		UpdatedAt:               now,
		DryRunToken:             "token",
		PreviousManifestHash:    "old",
		CandidateManifestHash:   "new",
		PreviousActiveRootfs:    map[string]string{"main": "rootfs-old", networkAnchorServiceName: "rootfs-anchor"},
		CandidateActiveRootfs:   map[string]string{"main": "rootfs-new", networkAnchorServiceName: "rootfs-anchor"},
		PrecommitDataSnapshotID: "snap-app-piclu--manifest-disabled",
		FailedDataLVName:        "vol-app-piclu--failed-manifest-disabled",
		RuntimeTouched:          true,
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}

	blocked := mgr.recoverPendingManifestUpdates(context.Background(), state)
	if blocked["piclu"] {
		t.Fatalf("piclu should recover successfully")
	}
	if len(volumes.rollbacks) != 1 || volumes.rollbacks[0] != "snap-app-piclu--manifest-disabled->vol-app-piclu--failed-manifest-disabled" {
		t.Fatalf("rollbacks = %v", volumes.rollbacks)
	}
	if len(volumes.destroyed) != 0 {
		t.Fatalf("snapshot should be restored, not destroyed: %v", volumes.destroyed)
	}
	restored, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("restored app missing")
	}
	if restored.Enabled {
		t.Fatalf("recovery should preserve disabled state")
	}
	if got := restored.ActiveRootfs["main"]; got != "rootfs-old" {
		t.Fatalf("active rootfs main = %q, want rootfs-old", got)
	}
	if restored.Definition.Services["main"].Image != "docker.io/example/piclu:stable" {
		t.Fatalf("restored image = %q", restored.Definition.Services["main"].Image)
	}
}

func TestRestorePrecommitDataSnapshotTracksFailedLVAfterCommittedRollbackError(t *testing.T) {
	for _, tc := range []struct {
		name             string
		snapshotPromoted bool
		wantSnapshotID   string
	}{
		{name: "snapshot-promoted", snapshotPromoted: true, wantSnapshotID: ""},
		{name: "snapshot-not-promoted", snapshotPromoted: false, wantSnapshotID: "snap-app-piclu--manifest-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			paths.SetCoreRootForTest(t, tempDir)
			paths.SetPodmanRootForTest(t, filepath.Join(tempDir, "podman"))
			t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", filepath.Join(tempDir, "podman-run"))
			mgr, err := NewAppManagerForTest(nil, tempDir)
			if err != nil {
				t.Fatalf("new manager: %v", err)
			}
			volumes := &manifestUpdateSnapshotVolumeManager{
				stubVolumeManager:        &stubVolumeManager{root: tempDir},
				rollbackResultSet:        true,
				rollbackRenamesCommitted: true,
				rollbackSnapshotPromoted: tc.snapshotPromoted,
				rollbackErr:              errors.New("rollback interrupted after rename"),
			}
			mgr.SetVolumeManager(volumes)
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatalf("state manager: %v", err)
			}
			txn := &ManifestUpdateTransaction{
				OperationID:             "op-recovery",
				PrecommitDataSnapshotID: "snap-app-piclu--manifest-test",
				FailedDataLVName:        "vol-app-piclu--failed-manifest-test",
			}

			err = mgr.restorePrecommitDataSnapshot(context.Background(), state, &AppInstance{InstanceID: "piclu"}, nil, txn)
			if err == nil {
				t.Fatalf("restore err = nil, want committed rollback error")
			}
			if !strings.Contains(err.Error(), "LV rename committed=true") {
				t.Fatalf("restore err = %v, want committed rename context", err)
			}
			if txn.FailedDataLVName != "vol-app-piclu--failed-manifest-test" {
				t.Fatalf("failed LV marker = %q", txn.FailedDataLVName)
			}
			if txn.PrecommitDataSnapshotID != tc.wantSnapshotID {
				t.Fatalf("snapshot marker = %q, want %q", txn.PrecommitDataSnapshotID, tc.wantSnapshotID)
			}
			tupleState, err := state.LoadTupleState("piclu")
			if err != nil {
				t.Fatalf("load tuple state: %v", err)
			}
			if tupleState == nil {
				t.Fatalf("tuple state missing")
			}
			foundFailedLV := false
			for _, gen := range tupleState.Generations {
				if gen.FailedLVName == "vol-app-piclu--failed-manifest-test" && gen.Status == TupleStatusFailed {
					foundFailedLV = true
				}
			}
			if !foundFailedLV {
				t.Fatalf("failed data LV was not tracked in tuple state: %+v", tupleState.Generations)
			}
		})
	}
}

func TestRestorePrecommitDataSnapshotRetriesAfterPartialRenameError(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	paths.SetPodmanRootForTest(t, filepath.Join(tempDir, "podman"))
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", filepath.Join(tempDir, "podman-run"))
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager:        &stubVolumeManager{root: tempDir},
		rollbackResultSet:        true,
		rollbackRenamesCommitted: true,
		rollbackSnapshotPromoted: false,
		rollbackErr:              errors.New("snapshot promotion interrupted"),
	}
	mgr.SetVolumeManager(volumes)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID:             "op-recovery",
		PrecommitDataSnapshotID: "snap-app-piclu--manifest-test",
		FailedDataLVName:        "vol-app-piclu--failed-manifest-test",
	}

	err = mgr.restorePrecommitDataSnapshot(context.Background(), state, &AppInstance{InstanceID: "piclu"}, nil, txn)
	if err == nil {
		t.Fatalf("restore err = nil, want partial rename failure")
	}
	if txn.PrecommitDataSnapshotID != "snap-app-piclu--manifest-test" {
		t.Fatalf("snapshot marker after partial failure = %q", txn.PrecommitDataSnapshotID)
	}
	volumes.rollbackResultSet = false
	volumes.rollbackErr = nil
	err = mgr.restorePrecommitDataSnapshot(context.Background(), state, &AppInstance{InstanceID: "piclu"}, nil, txn)
	if err != nil {
		t.Fatalf("retry restore precommit snapshot: %v", err)
	}
	if txn.PrecommitDataSnapshotID != "" {
		t.Fatalf("snapshot marker after retry = %q, want cleared", txn.PrecommitDataSnapshotID)
	}
	if len(volumes.rollbacks) != 2 {
		t.Fatalf("rollback attempts = %v, want two attempts", volumes.rollbacks)
	}
}

func TestRestorePrecommitDataSnapshotRefusesRollbackWithoutQuiescenceProof(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	paths.SetPodmanRootForTest(t, filepath.Join(tempDir, "podman"))
	t.Setenv("PICCOLO_PODMAN_RUNROOT_BASE", filepath.Join(tempDir, "podman-run"))
	mgr, err := NewAppManagerForTest(nil, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		ensureErr:         errors.New("active LV missing"),
	}
	mgr.SetVolumeManager(volumes)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	txn := &ManifestUpdateTransaction{
		OperationID:             "op-recovery",
		PrecommitDataSnapshotID: "snap-app-piclu--manifest-test",
		FailedDataLVName:        "vol-app-piclu--failed-manifest-test",
	}

	err = mgr.restorePrecommitDataSnapshot(context.Background(), state, &AppInstance{InstanceID: "piclu"}, customManifestPolicyBaseDef(), txn)
	if err == nil || !strings.Contains(err.Error(), "ensure layout before data snapshot restore") {
		t.Fatalf("restore precommit snapshot error = %v, want quiescence precondition failure", err)
	}
	if txn.PrecommitDataSnapshotID == "" {
		t.Fatal("snapshot marker cleared without a safe rollback")
	}
	if len(volumes.rollbacks) != 0 {
		t.Fatalf("rollback attempts = %v, want none without quiescence proof", volumes.rollbacks)
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
		InstanceID:          "piclu",
		Enabled:             false,
		PrimaryService:      "main",
		CatalogSource:       "piclu",
		CatalogManifestHash: "old-catalog-template-hash",
		LastSyncError:       "previous catalog drift",
		CreatedAt:           now,
		UpdatedAt:           now,
		Definition:          prevDef,
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
	appInst, exists := state.GetApp("piclu")
	if !exists {
		t.Fatalf("app missing after recovery")
	}
	if appInst.CatalogManifestHash != "old-catalog-template-hash" {
		t.Fatalf("catalog hash after custom ledger recovery = %q, want old catalog hash", appInst.CatalogManifestHash)
	}
	if appInst.LastSyncError != "previous catalog drift" {
		t.Fatalf("last sync error after custom ledger recovery = %q, want preserved catalog sync state", appInst.LastSyncError)
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

func TestManifestTransactionRuntimeSwitchStartedInfersMarkers(t *testing.T) {
	tests := []struct {
		name string
		txn  *ManifestUpdateTransaction
		want bool
	}{
		{
			name: "explicit switch marker",
			txn:  &ManifestUpdateTransaction{OperationKind: "manifest_update", Phase: "access_suspended", RuntimeSwitchStarted: true},
			want: true,
		},
		{
			name: "runtime touched implies switch for existing transactions",
			txn:  &ManifestUpdateTransaction{OperationKind: "manifest_update", Phase: "recreating_runtime", RuntimeTouched: true},
			want: true,
		},
		{
			name: "pre-switch phase",
			txn:  &ManifestUpdateTransaction{OperationKind: "manifest_update", Phase: "access_suspended"},
			want: false,
		},
		{
			name: "legacy switch phase",
			txn:  &ManifestUpdateTransaction{Phase: "runtime_switch_started"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manifestTransactionRuntimeSwitchStarted(tt.txn); got != tt.want {
				t.Fatalf("manifestTransactionRuntimeSwitchStarted() = %v, want %v", got, tt.want)
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
	if txn.Phase != "candidate_persisted" || txn.RuntimeTouched || txn.RuntimeSwitchStarted || !txn.UpdatedAt.Equal(initialUpdatedAt) {
		t.Fatalf("in-memory txn was not restored after failed marker write: %+v", txn)
	}
	stored, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load stored transaction: %v", err)
	}
	if stored.Phase != "candidate_persisted" || stored.RuntimeTouched || stored.RuntimeSwitchStarted {
		t.Fatalf("durable txn changed after failed marker write: %+v", stored)
	}
}

func TestStoreManifestUpdateTransactionUsesPrivateFileMode(t *testing.T) {
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
	if err := state.StoreManifestUpdateTransaction("piclu", &ManifestUpdateTransaction{
		OperationID:           "op",
		OperationKind:         "manifest_update",
		Phase:                 "candidate_persisted",
		PreviousManifestHash:  "old",
		CandidateManifestHash: "new",
		BackupPath:            filepath.Join(tempDir, "backup.yaml"),
		DryRunToken:           "token",
		CreatedAt:             now,
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("store transaction: %v", err)
	}
	info, err := os.Stat(filepath.Join(state.appsDir, "piclu", manifestUpdateTxnFilename))
	if err != nil {
		t.Fatalf("stat transaction: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("transaction mode = %o, want 0600", got)
	}
}

func TestApplyCustomManifestUpdate_PreservesPlannedSnapshotMarkerWhenCreateCleanupFails(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	volumes := &manifestUpdateSnapshotVolumeManager{
		stubVolumeManager: &stubVolumeManager{root: tempDir},
		snapshotErr:       errors.New("thin snapshot create interrupted"),
		destroyErr:        errors.New("snapshot cleanup unavailable"),
	}
	mgr.SetVolumeManager(volumes)
	mgr.SetRootfsManager(newStubRootfsManager(tempDir))
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}
	now := time.Now().UTC()
	baseDef := customManifestPolicyBaseDef()
	candidateDef := customManifestPolicyClone(t, baseDef)
	appInst := &AppInstance{
		InstanceID:     "piclu",
		Enabled:        true,
		PrimaryService: "main",
		ActiveRootfs:   map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
		CreatedAt:      now,
		UpdatedAt:      now,
		Definition:     baseDef,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	svc := candidateDef.Services["main"]
	svc.Environment["PICLU_DEVICE_DIAG_DIR"] = "/diagnostics"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("env update"))

	_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
		InstanceID:         "piclu",
		BaseManifestHash:   cand.BaseManifestHash,
		RuntimeFingerprint: cand.RuntimeFingerprint,
		DryRunToken:        cand.Token,
		Confirmations:      cand.Classification.RequiredConfirmations,
	})
	if err == nil || !strings.Contains(err.Error(), "thin snapshot create interrupted") {
		t.Fatalf("apply err = %v, want snapshot create failure", err)
	}
	if len(volumes.snapshots) != 1 {
		t.Fatalf("snapshots = %v, want one create attempt", volumes.snapshots)
	}
	txn, err := state.LoadManifestUpdateTransaction("piclu")
	if err != nil {
		t.Fatalf("load transaction: %v", err)
	}
	if txn.PrecommitDataSnapshotID != volumes.snapshots[0] {
		t.Fatalf("txn snapshot marker = %q, want %q", txn.PrecommitDataSnapshotID, volumes.snapshots[0])
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

func customManifestBaseRaw() []byte {
	return []byte(`type: user
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

func customManifestKeptSecretRaw(label, description, envName string) []byte {
	return []byte(fmt.Sprintf(`type: user
inputs:
  gemini_api_key:
    type: password
    label: %s
    description: %s
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
      %s: "{{ .Inputs.gemini_api_key }}"
    storage:
      persistent:
        data:
          container: /data
x-piccolo:
  mode: service
`, label, description, envName))
}

func customManifestReclassifiedSecretRaw(inputType string) []byte {
	return []byte(fmt.Sprintf(`type: user
inputs:
  license:
    type: %s
    label: License
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
      LICENSE: "{{ .Inputs.license }}"
x-piccolo:
  mode: service
`, inputType))
}

func findManifestDecision(decisions []ManifestUpdateDecision, flag string) *ManifestUpdateDecision {
	for i := range decisions {
		if decisions[i].Flag == flag {
			return &decisions[i]
		}
	}
	return nil
}

func storeManifestUpdateCandidateForTest(t *testing.T, mgr *AppManager, state *FilesystemStateManager, appInst *AppInstance, candidateDef *api.AppDefinition, raw []byte) *manifestUpdateCandidate {
	t.Helper()
	baseHash, err := canonicalManifestHash(appInst.Definition)
	if err != nil {
		t.Fatalf("base hash: %v", err)
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		t.Fatalf("runtime fingerprint: %v", err)
	}
	ledgerExists, ledgerRevision, ledgerSourceHash, err := loadInstallLedgerFingerprint(state, appInst.InstanceID)
	if err != nil {
		t.Fatalf("ledger fingerprint: %v", err)
	}
	candidateHash, err := canonicalManifestHash(candidateDef)
	if err != nil {
		t.Fatalf("candidate hash: %v", err)
	}
	policy, summary := evaluateCustomManifestUpdatePolicy(appInst.Definition, candidateDef)
	imagePlan, err := mgr.resolveManifestUpdateImagePlan(context.Background(), appInst.InstanceID, appInst, appInst.Definition, candidateDef, !policy.MetadataOnly)
	if err != nil {
		t.Fatalf("image plan: %v", err)
	}
	if len(imagePlan) > 0 {
		applyManifestUpdateImagePlanClassification(&policy.Classification, &summary, appInst.Definition, imagePlan)
	}
	token := "token-" + candidateHash[:12]
	cand := &manifestUpdateCandidate{
		Token:                token,
		InstanceID:           appInst.InstanceID,
		RawTemplate:          raw,
		Inputs:               map[string]interface{}{"__app_address__": appInst.InstanceID},
		SystemContext:        InstallSystemContext{Domain: "local", Architecture: "amd64", Timezone: "Etc/UTC"},
		BaseManifestHash:     baseHash,
		RuntimeFingerprint:   runtimeFingerprint,
		BaseLedgerExists:     ledgerExists,
		BaseLedgerRevision:   ledgerRevision,
		BaseLedgerSourceHash: ledgerSourceHash,
		CandidateDigest:      candidateHash,
		DiffKind:             classifyDiff(cloneDefinitionForCompare(appInst.Definition), cloneDefinitionForCompare(candidateDef)),
		MetadataOnly:         policy.MetadataOnly,
		Definition:           candidateDef,
		Summary:              summary,
		Classification:       policy.Classification,
		ImagePlan:            cloneManifestUpdateImagePlan(imagePlan),
		CreatedAt:            time.Now().UTC(),
		ExpiresAt:            time.Now().UTC().Add(manifestUpdateTokenTTL),
	}
	mgr.storeManifestUpdateCandidate(cand)
	return cand
}

type manifestUpdateSnapshotVolumeManager struct {
	*stubVolumeManager
	snapshots                []string
	destroyed                []string
	rollbacks                []string
	ensureErr                error
	viabilityErr             error
	healthErr                error
	snapshotErr              error
	destroyErr               error
	rollbackResultSet        bool
	rollbackRenamesCommitted bool
	rollbackSnapshotPromoted bool
	rollbackErr              error
	rollbackHook             func(call int, instanceID, snapshotLVName, failedLVName string) (bool, bool, error)
	snapshotHook             func(instanceID, snapshotLVName string) error
	viabilityHook            func(instanceID string) error
	artifacts                []string
}

func (m *manifestUpdateSnapshotVolumeManager) EnsureVolume(ctx context.Context, req persistence.VolumeRequest) (persistence.VolumeHandle, error) {
	if m.ensureErr != nil {
		return persistence.VolumeHandle{}, m.ensureErr
	}
	return m.stubVolumeManager.EnsureVolume(ctx, req)
}

func (m *manifestUpdateSnapshotVolumeManager) CheckDataSnapshotViability(ctx context.Context, instanceID string) error {
	_ = ctx
	if m.viabilityHook != nil {
		if err := m.viabilityHook(instanceID); err != nil {
			return err
		}
	}
	return m.viabilityErr
}

func (m *manifestUpdateSnapshotVolumeManager) CheckDataSnapshotHealth(ctx context.Context, snapshotLVName string) error {
	_ = ctx
	_ = snapshotLVName
	return m.healthErr
}

func (m *manifestUpdateSnapshotVolumeManager) SnapshotDataVolume(ctx context.Context, instanceID, snapshotLVName string) error {
	_ = ctx
	if m.snapshotHook != nil {
		if err := m.snapshotHook(instanceID, snapshotLVName); err != nil {
			return err
		}
	}
	m.snapshots = append(m.snapshots, snapshotLVName)
	return m.snapshotErr
}

func (m *manifestUpdateSnapshotVolumeManager) DestroyDataSnapshot(ctx context.Context, snapshotLVName string) error {
	_ = ctx
	m.destroyed = append(m.destroyed, snapshotLVName)
	return m.destroyErr
}

func (m *manifestUpdateSnapshotVolumeManager) ListAppDataRollbackArtifacts(ctx context.Context, instanceID string) ([]string, error) {
	_ = ctx
	_ = instanceID
	out := make([]string, len(m.artifacts))
	copy(out, m.artifacts)
	return out, nil
}

func (m *manifestUpdateSnapshotVolumeManager) RollbackDataVolume(ctx context.Context, instanceID string, snapshotLVName, failedLVName string) (bool, bool, error) {
	_ = ctx
	_ = instanceID
	m.rollbacks = append(m.rollbacks, snapshotLVName+"->"+failedLVName)
	if m.rollbackHook != nil {
		return m.rollbackHook(len(m.rollbacks), instanceID, snapshotLVName, failedLVName)
	}
	if m.rollbackResultSet {
		return m.rollbackRenamesCommitted, m.rollbackSnapshotPromoted, m.rollbackErr
	}
	return true, true, nil
}

func customManifestImageUpdateRaw(image string) []byte {
	return []byte(`type: user
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
    image: ` + image + `
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
}

func customManifestOIDCRaw(withAuthorizePaths bool) []byte {
	authorizePaths := ""
	if withAuthorizePaths {
		authorizePaths = "      authorize_paths:\n        - /authorize\n"
	}
	return []byte(`type: user
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
` + authorizePaths + `      redirect_uri_paths:
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
}

func customManifestEnvUpdateRaw() []byte {
	return []byte(`type: user
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
      PICLU_DEVICE_DIAG_DIR: /diagnostics
    storage:
      persistent:
        data:
          container: /data
x-piccolo:
  mode: service
`)
}
