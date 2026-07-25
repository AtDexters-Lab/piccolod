package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func newCapabilityTestState(t *testing.T) *FilesystemStateManager {
	t.Helper()
	state, err := NewFilesystemStateManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStateManager: %v", err)
	}
	return state
}

func capabilityProviderDefinition(basePath string) *api.AppDefinition {
	return &api.AppDefinition{
		Extensions: map[string]interface{}{
			"mode":              string(ModeService),
			"requires_features": []string{api.FeatureCapabilityBindingsV1},
		},
		Services: map[string]api.AppService{
			"main": {Image: "provider:latest", BindPorts: []int{8000}},
		},
		Listeners: []api.AppListener{{
			Name:      "inference",
			GuestPort: 8000,
			Flow:      api.FlowTCP,
			Protocol:  api.ListenerProtocolHTTP,
			Primary:   true,
			Provides: []api.CapabilityProvider{{
				Capability: api.CapabilityAIInferenceOpenAIV1,
				BasePath:   basePath,
			}},
		}},
	}
}

func TestImageUserUIDResolvesNumericAndNamedUsers(t *testing.T) {
	t.Parallel()

	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(rootfs, "etc", "passwd"),
		[]byte("root:x:0:0:root:/root:/bin/sh\ninference:x:1000:1000::/app:/bin/sh\n"),
		0o644,
	); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	for _, test := range []struct {
		spec string
		want uint32
	}{
		{spec: "", want: 0},
		{spec: "1001:1001", want: 1001},
		{spec: "inference", want: 1000},
		{spec: "inference:models", want: 1000},
	} {
		got, err := imageUserUID(rootfs, test.spec)
		if err != nil {
			t.Fatalf("imageUserUID(%q): %v", test.spec, err)
		}
		if got != test.want {
			t.Fatalf("imageUserUID(%q) = %d, want %d", test.spec, got, test.want)
		}
	}
}

func TestMapContainerUIDToHostMatchesRootlessRange(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		containerUID uint32
		want         uint32
	}{
		{containerUID: 0, want: 1234},
		{containerUID: 1, want: 200000},
		{containerUID: 1000, want: 200999},
		{containerUID: 65536, want: 265535},
	} {
		got, err := mapContainerUIDToHost(1234, 200000, 65536, test.containerUID)
		if err != nil {
			t.Fatalf("map UID %d: %v", test.containerUID, err)
		}
		if got != test.want {
			t.Fatalf("map UID %d = %d, want %d", test.containerUID, got, test.want)
		}
	}
	if _, err := mapContainerUIDToHost(1234, 200000, 65536, 65537); err == nil {
		t.Fatal("out-of-range container UID was accepted")
	}
}

func capabilityConsumerDefinition(envName string) *api.AppDefinition {
	return &api.AppDefinition{
		Services: map[string]api.AppService{
			"main": {
				Image:     "consumer:latest",
				BindPorts: []int{},
				Consumes: []api.CapabilityConsumer{{
					Capability: api.CapabilityAIInferenceOpenAIV1,
					Env:        map[string]string{envName: api.CapabilityBindingBaseURL},
				}},
			},
		},
	}
}

func TestInstallFirstCapabilityProviderConvergesDefaultBeforeReturn(t *testing.T) {
	newManager := func(t *testing.T) (*AppManager, *MockContainerManager) {
		t.Helper()
		mock := NewMockContainerManager()
		manager, err := NewAppManagerForTest(mock, t.TempDir())
		if err != nil {
			t.Fatalf("NewAppManagerForTest: %v", err)
		}
		allowHostStorage(t, manager)
		manager.ForceLockState(false)
		manager.SetRootfsManager(newStubRootfsManager(t.TempDir()))
		return manager, mock
	}
	withInitScript := func(definition *api.AppDefinition) {
		service := definition.Services["main"]
		service.InitScript = &api.ServiceInitScript{
			File:        "init.sh",
			Shell:       "/bin/sh",
			FileContent: []byte("#!/bin/sh\n"),
		}
		definition.Services["main"] = service
	}

	t.Run("success includes automatic default and runtime effects", func(t *testing.T) {
		manager, mock := newManager(t)
		definition := capabilityProviderDefinition("/v1")
		definition.Listeners[0].Name = "provider"
		withInitScript(definition)

		installed, err := manager.Install(context.Background(), definition)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if installed == nil || installed.InstanceID != "provider" {
			t.Fatalf("installed app = %#v", installed)
		}
		state, err := manager.ensureStateManager()
		if err != nil {
			t.Fatalf("ensureStateManager: %v", err)
		}
		durable, err := state.loadCapabilityState()
		if err != nil {
			t.Fatalf("loadCapabilityState: %v", err)
		}
		if got := durable.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != installed.InstanceID {
			t.Fatalf("default at install return = %q, want %q", got, installed.InstanceID)
		}
		if mock.nextID != 5 {
			t.Fatalf("created container generations = %d, want initial and selected-provider generations", mock.nextID-1)
		}
		if installed.NetworkAnchorID == "" {
			t.Fatal("returned install omitted the converged selected-provider anchor")
		}
		if got := mock.execScriptCallCount(); got != 1 {
			t.Fatalf("init executions after install and mandatory default recreation = %d, want 1", got)
		}
	})

	t.Run("effect failure returns committed install as reconciliation pending", func(t *testing.T) {
		manager, mock := newManager(t)
		definition := capabilityProviderDefinition("/v1")
		definition.Listeners[0].Name = "provider"
		withInitScript(definition)
		injected := errors.New("injected capability recreation failure")
		mock.removeError = injected

		installed, err := manager.Install(context.Background(), definition)
		var pending *CapabilitySelectionReconcilePendingError
		if !errors.As(err, &pending) || !errors.Is(err, injected) {
			t.Fatalf("Install error = %v, want capability reconciliation pending", err)
		}
		if installed == nil || installed.InstanceID != "provider" {
			t.Fatalf("committed install result = %#v", installed)
		}
		state, stateErr := manager.ensureStateManager()
		if stateErr != nil {
			t.Fatalf("ensureStateManager: %v", stateErr)
		}
		if _, exists := state.GetApp(installed.InstanceID); !exists {
			t.Fatal("pending capability effects removed the committed install")
		}
		durable, loadErr := state.loadCapabilityState()
		if loadErr != nil {
			t.Fatalf("loadCapabilityState: %v", loadErr)
		}
		if got := durable.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != installed.InstanceID {
			t.Fatalf("pending default = %q, want %q", got, installed.InstanceID)
		}
		if got := mock.execScriptCallCount(); got != 1 {
			t.Fatalf("init executions after pending default rebind = %d, want 1", got)
		}
		if manager.serviceManager.AppPublicationActive(installed.InstanceID) {
			t.Fatal("failed capability recreation left provider publication active")
		}

		mock.removeError = nil
		if retryErr := manager.SelectCapabilityProvider(
			context.Background(),
			api.CapabilityAIInferenceOpenAIV1,
			installed.InstanceID,
		); retryErr != nil {
			t.Fatalf("repair committed install capability effects: %v", retryErr)
		}
		for containerID, candidate := range mock.containers {
			if candidate == nil || candidate.Status != "running" {
				t.Fatalf("container %s after retry = %#v, want running", containerID, candidate)
			}
		}
		if !manager.serviceManager.AppPublicationActive(installed.InstanceID) {
			t.Fatal("successful retry did not restore provider publication")
		}
		endpoints, endpointErr := manager.serviceManager.GetByApp(installed.InstanceID)
		if endpointErr != nil || len(endpoints) != 1 {
			t.Fatalf("provider endpoints after retry = %+v, err=%v", endpoints, endpointErr)
		}
		if got := mock.execScriptCallCount(); got != 1 {
			t.Fatalf("init executions after capability retry = %d, want 1", got)
		}
	})
}

func TestReconcileCapabilityDefaultsIsDeterministicAndRetainsStoppedSelection(t *testing.T) {
	state := newCapabilityTestState(t)
	now := time.Now().UTC()
	state.cache["newer"] = &AppInstance{
		InstanceID: "newer",
		Enabled:    true,
		CreatedAt:  now.Add(time.Minute),
		Definition: capabilityProviderDefinition("/v3"),
	}
	state.cache["older"] = &AppInstance{
		InstanceID: "older",
		Enabled:    true,
		CreatedAt:  now,
		Definition: capabilityProviderDefinition("/v1"),
	}

	manager := &AppManager{
		observedStatus:        make(map[string]string),
		observedStatusMessage: make(map[string]string),
	}
	changes, err := manager.reconcileCapabilityDefaults(state)
	if err != nil {
		t.Fatalf("reconcileCapabilityDefaults: %v", err)
	}
	if got := changes[api.CapabilityAIInferenceOpenAIV1]; got != [2]string{"", "older"} {
		t.Fatalf("initial change = %#v, want oldest provider", got)
	}
	if status := manager.getObservedStatus("older"); status != StatusStarting {
		t.Fatalf("new default status = %q, want fail-closed starting fence", status)
	}

	state.cache["older"].Enabled = false
	changes, err = manager.reconcileCapabilityDefaults(state)
	if err != nil {
		t.Fatalf("reconcile stopped default: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("manual stop changed selection: %#v", changes)
	}

	state.cache["older"].Definition = capabilityConsumerDefinition("OPENAI_BASE_URL")
	changes, err = manager.reconcileCapabilityDefaults(state)
	if err != nil {
		t.Fatalf("reconcile removed provider declaration: %v", err)
	}
	if got := changes[api.CapabilityAIInferenceOpenAIV1]; got != [2]string{"older", "newer"} {
		t.Fatalf("replacement change = %#v, want newer provider", got)
	}
}

func TestCapabilityProviderStartingFenceRestoresExactStatus(t *testing.T) {
	manager := &AppManager{
		observedStatus:        map[string]string{"provider": StatusRunning},
		observedStatusMessage: map[string]string{"provider": "healthy"},
	}
	restore := manager.fenceCapabilityProviderStarting("provider")
	status, message := manager.getObservedStatusAndMessage("provider")
	if status != StatusStarting || message != "" {
		t.Fatalf("fenced status=%q message=%q", status, message)
	}
	restore()
	status, message = manager.getObservedStatusAndMessage("provider")
	if status != StatusRunning || message != "healthy" {
		t.Fatalf("restored status=%q message=%q", status, message)
	}
}

func TestCapabilityDefaultPublicationFencesBeforeWriteAndRestoresOnFailure(t *testing.T) {
	tests := []struct {
		name string
		run  func(*AppManager, *FilesystemStateManager) error
	}{
		{
			name: "automatic selection",
			run: func(manager *AppManager, state *FilesystemStateManager) error {
				_, err := manager.reconcileCapabilityDefaults(state)
				return err
			},
		},
		{
			name: "explicit selection",
			run: func(manager *AppManager, _ *FilesystemStateManager) error {
				return manager.SelectCapabilityProvider(
					context.Background(),
					api.CapabilityAIInferenceOpenAIV1,
					"provider",
				)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			state := newCapabilityTestState(t)
			state.cache["provider"] = &AppInstance{
				InstanceID: "provider",
				Enabled:    true,
				Definition: capabilityProviderDefinition("/v1"),
			}
			manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
			if err != nil {
				t.Fatalf("NewAppManagerForTest: %v", err)
			}
			manager.stateManager = state
			manager.updateStatusAndMessageWithEvent("provider", StatusRunning, "healthy")

			injected := fmt.Errorf("capability state write failed")
			state.storeCapabilityStateHook = func(_ *capabilityState) error {
				status, _ := manager.getObservedStatusAndMessage("provider")
				if status != StatusStarting {
					t.Fatalf("status at durable write = %q, want %q", status, StatusStarting)
				}
				return injected
			}
			if err := test.run(manager, state); !errors.Is(err, injected) {
				t.Fatalf("publication error = %v, want injected write failure", err)
			}
			status, message := manager.getObservedStatusAndMessage("provider")
			if status != StatusRunning || message != "healthy" {
				t.Fatalf("restored status=%q message=%q", status, message)
			}
			durable, err := state.loadCapabilityState()
			if err != nil {
				t.Fatalf("load capability state: %v", err)
			}
			if durable.Defaults[api.CapabilityAIInferenceOpenAIV1] != "" {
				t.Fatalf("failed write published default: %#v", durable.Defaults)
			}
		})
	}
}

func TestAutomaticCapabilityEffectsRespectExistingTransitionOwners(t *testing.T) {
	t.Run("pending new provider remains the durable default", func(t *testing.T) {
		state := newCapabilityTestState(t)
		provider := &AppInstance{
			InstanceID: "provider",
			Enabled:    true,
			CreatedAt:  time.Now().UTC(),
			Definition: capabilityProviderDefinition("/v1"),
		}
		if err := state.StoreApp(provider); err != nil {
			t.Fatalf("store provider: %v", err)
		}
		if err := state.StoreTransitionRecord(
			provider.InstanceID,
			transitionTestRecord(provider.InstanceID, TransitionPhaseSwitchingRuntime),
		); err != nil {
			t.Fatalf("store provider transition: %v", err)
		}

		manager := &AppManager{
			observedStatus:        make(map[string]string),
			observedStatusMessage: make(map[string]string),
		}
		err := manager.reconcileCapabilityDefaultsAndEffects(context.Background(), state)
		if !errors.Is(err, ErrTransitionInProgress) {
			t.Fatalf("automatic capability reconciliation err = %v, want ErrTransitionInProgress", err)
		}
		durable, err := state.loadCapabilityState()
		if err != nil {
			t.Fatalf("load capability state: %v", err)
		}
		if got := durable.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != provider.InstanceID {
			t.Fatalf("durable default = %q, want pending provider %q", got, provider.InstanceID)
		}
		if err := state.ClearTransitionRecord(provider.InstanceID); err != nil {
			t.Fatalf("clear provider transition: %v", err)
		}
		if err := manager.reconcileCapabilityDefaultsAndEffects(context.Background(), state); err != nil {
			t.Fatalf("retry automatic capability reconciliation after transition clear: %v", err)
		}
	})

	t.Run("old grant owner is not revoked", func(t *testing.T) {
		state := newCapabilityTestState(t)
		oldProvider := &AppInstance{
			InstanceID: "old-provider",
			Enabled:    true,
			Definition: capabilityProviderDefinition("/v1"),
		}
		if err := state.StoreApp(oldProvider); err != nil {
			t.Fatalf("store old provider: %v", err)
		}
		if err := state.StoreImageUpdateTransaction(
			oldProvider.InstanceID,
			&ImageUpdateTransaction{Phase: imageUpdatePhaseRuntimeSwitch},
		); err != nil {
			t.Fatalf("store old-provider image journal: %v", err)
		}
		durable := newCapabilityState()
		durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "new-provider"
		durable.AcceleratorGrant = &acceleratorGrantRecord{
			Owner:   oldProvider.InstanceID,
			UIDs:    []uint32{4321},
			Devices: []string{"/dev/dri/renderD128"},
		}
		if err := state.storeCapabilityState(durable); err != nil {
			t.Fatalf("store accelerator owner: %v", err)
		}

		permissionCalls := 0
		manager := &AppManager{
			acceleratorPermission: func(context.Context, uint32, []string, bool) error {
				permissionCalls++
				return nil
			},
		}
		err := manager.applyCapabilityDefaultChange(
			context.Background(),
			state,
			api.CapabilityAIInferenceOpenAIV1,
			oldProvider.InstanceID,
			"new-provider",
			nil,
		)
		if !errors.Is(err, ErrTransitionInProgress) {
			t.Fatalf("old-provider capability effect err = %v, want ErrTransitionInProgress", err)
		}
		if permissionCalls != 0 {
			t.Fatalf("accelerator permission calls = %d, want zero", permissionCalls)
		}
		stored, err := state.loadCapabilityState()
		if err != nil {
			t.Fatalf("reload accelerator owner: %v", err)
		}
		if stored.AcceleratorGrant == nil || stored.AcceleratorGrant.Owner != oldProvider.InstanceID {
			t.Fatalf("transition-owned accelerator grant was mutated: %#v", stored.AcceleratorGrant)
		}
	})

	t.Run("consumer legacy journal owns its runtime", func(t *testing.T) {
		state := newCapabilityTestState(t)
		consumer := &AppInstance{
			InstanceID: "consumer",
			Enabled:    true,
			Definition: capabilityConsumerDefinition("OPENAI_BASE_URL"),
		}
		if err := state.StoreApp(consumer); err != nil {
			t.Fatalf("store consumer: %v", err)
		}
		if err := state.StoreManifestUpdateTransaction(
			consumer.InstanceID,
			&ManifestUpdateTransaction{Phase: "switching_runtime"},
		); err != nil {
			t.Fatalf("store consumer manifest journal: %v", err)
		}

		manager := &AppManager{}
		err := manager.applyCapabilityDefaultChange(
			context.Background(),
			state,
			api.CapabilityAIInferenceOpenAIV1,
			"",
			"",
			nil,
		)
		if !errors.Is(err, ErrTransitionInProgress) {
			t.Fatalf("consumer capability effect err = %v, want ErrTransitionInProgress", err)
		}
	})
}

func TestManualCapabilitySelectionDefersToTransitionAndRetriesAfterClear(t *testing.T) {
	state := newCapabilityTestState(t)
	provider := &AppInstance{
		InstanceID: "provider",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v1"),
	}
	if err := state.StoreApp(provider); err != nil {
		t.Fatalf("store provider: %v", err)
	}
	if err := state.StoreTransitionRecord(
		provider.InstanceID,
		transitionTestRecord(provider.InstanceID, TransitionPhaseCandidateTouched),
	); err != nil {
		t.Fatalf("store provider transition: %v", err)
	}

	manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	manager.stateManager = state
	manager.ForceLockState(false)
	manager.acceleratorDiscover = func() ([]string, error) { return nil, nil }

	err = manager.SelectCapabilityProvider(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		provider.InstanceID,
	)
	var pending *CapabilitySelectionReconcilePendingError
	if !errors.As(err, &pending) || !errors.Is(err, ErrTransitionInProgress) {
		t.Fatalf("manual selection err = %v, want reconciliation-pending transition fence", err)
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load persisted manual selection: %v", err)
	}
	if got := durable.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != provider.InstanceID {
		t.Fatalf("durable default = %q, want %q", got, provider.InstanceID)
	}

	if err := state.ClearTransitionRecord(provider.InstanceID); err != nil {
		t.Fatalf("clear provider transition: %v", err)
	}
	if err := manager.SelectCapabilityProvider(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		provider.InstanceID,
	); err != nil {
		t.Fatalf("retry manual selection after transition clear: %v", err)
	}
}

func TestCapabilityRecreationChecksTransitionBeforeRuntimePreparation(t *testing.T) {
	manager, err := NewAppManagerForTest(NewMockContainerManager(), t.TempDir())
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	allowHostStorage(t, manager)
	state, err := manager.ensureStateManager()
	if err != nil {
		t.Fatalf("ensureStateManager: %v", err)
	}
	def := capabilityProviderDefinition("/v1")
	SetDefaults(def)
	provider := &AppInstance{
		InstanceID: "provider",
		Enabled:    true,
		Definition: def,
		ActiveRootfs: map[string]string{
			"main":                   "rootfs-main",
			networkAnchorServiceName: "rootfs-anchor",
		},
	}
	if err := state.StoreApp(provider); err != nil {
		t.Fatalf("store provider: %v", err)
	}
	if err := state.StoreTransitionRecord(
		provider.InstanceID,
		transitionTestRecord(provider.InstanceID, TransitionPhaseSwitchingRuntime),
	); err != nil {
		t.Fatalf("store provider transition: %v", err)
	}
	rootfs := manager.currentRootfsManager().(*stubRootfsManager)
	attachCalls := 0
	rootfs.attachHook = func() { attachCalls++ }
	afterRemovalCalls := 0

	err = manager.recreateAppForCapabilityEffects(
		context.Background(),
		state,
		provider.InstanceID,
		func() error {
			afterRemovalCalls++
			return nil
		},
	)
	if !errors.Is(err, ErrTransitionInProgress) {
		t.Fatalf("capability recreation err = %v, want ErrTransitionInProgress", err)
	}
	if attachCalls != 0 {
		t.Fatalf("rootfs attach calls = %d, want zero before transition ownership clears", attachCalls)
	}
	if afterRemovalCalls != 0 {
		t.Fatalf("after-removal calls = %d, want zero before transition ownership clears", afterRemovalCalls)
	}
}

func TestCapabilityRecreationFailsClosedOnUnreadableTransitionState(t *testing.T) {
	for _, test := range []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "v2 record",
			filename: transitionRecordFilename,
			want:     "read app transition record",
		},
		{
			name:     "legacy manifest journal",
			filename: manifestUpdateTxnFilename,
			want:     "read legacy transition journals",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			state := newCapabilityTestState(t)
			instanceID := "provider"
			appDir := filepath.Join(state.appsDir, instanceID)
			if err := os.MkdirAll(appDir, 0o755); err != nil {
				t.Fatalf("mkdir app state: %v", err)
			}
			transitionPath := filepath.Join(appDir, test.filename)
			if err := os.WriteFile(transitionPath, []byte("{not-json"), 0o600); err != nil {
				t.Fatalf("write unreadable transition: %v", err)
			}

			afterRemovalCalls := 0
			manager := &AppManager{}
			err := manager.recreateAppForCapabilityEffects(
				context.Background(),
				state,
				instanceID,
				func() error {
					afterRemovalCalls++
					return nil
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unreadable transition err = %v, want fail-closed %q", err, test.want)
			}
			if afterRemovalCalls != 0 {
				t.Fatalf("after-removal effect ran %d times before transition ownership was known", afterRemovalCalls)
			}

			if err := os.Remove(transitionPath); err != nil {
				t.Fatalf("remove unreadable transition: %v", err)
			}
			if err := manager.recreateAppForCapabilityEffects(
				context.Background(),
				state,
				instanceID,
				func() error {
					afterRemovalCalls++
					return nil
				},
			); err != nil {
				t.Fatalf("retry after clearing unreadable transition: %v", err)
			}
			if afterRemovalCalls != 1 {
				t.Fatalf("after-removal effect calls = %d, want one after clear", afterRemovalCalls)
			}
		})
	}
}

func TestDesiredCapabilityBindingsRecordsInjectedBaseURLInputs(t *testing.T) {
	state := newCapabilityTestState(t)
	state.cache["provider"] = &AppInstance{
		InstanceID: "provider",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v3"),
	}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider"
	durable.IngressPorts["consumer"] = map[string]int{api.CapabilityAIInferenceOpenAIV1: 27001}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	desired, err := desiredCapabilityBindings(state, "consumer", capabilityConsumerDefinition("OPENAI_BASE_URL"))
	if err != nil {
		t.Fatalf("desiredCapabilityBindings: %v", err)
	}
	signature := desired[api.CapabilityAIInferenceOpenAIV1]
	if signature != "/v3\x0027001" {
		t.Fatalf("binding signature = %q", signature)
	}
	if capabilityGenerationMatches(&AppInstance{CapabilityBindings: desired}, desired) != true {
		t.Fatalf("matching committed generation was rejected")
	}

	state.cache["replacement"] = &AppInstance{
		InstanceID: "replacement",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v3"),
	}
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "replacement"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store same-path replacement: %v", err)
	}
	sameValue, err := desiredCapabilityBindings(state, "consumer", capabilityConsumerDefinition("OPENAI_BASE_URL"))
	if err != nil {
		t.Fatalf("desiredCapabilityBindings after same-path provider change: %v", err)
	}
	if !capabilityGenerationMatches(&AppInstance{CapabilityBindings: desired}, sameValue) {
		t.Fatal("same injected base URL unnecessarily invalidated consumer generation")
	}

	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("restore provider selection: %v", err)
	}
	state.cache["provider"].Definition = capabilityProviderDefinition("/v4")
	next, err := desiredCapabilityBindings(state, "consumer", capabilityConsumerDefinition("OPENAI_BASE_URL"))
	if err != nil {
		t.Fatalf("desiredCapabilityBindings after path change: %v", err)
	}
	if capabilityGenerationMatches(&AppInstance{CapabilityBindings: desired}, next) {
		t.Fatalf("provider base_path change did not invalidate consumer generation")
	}

	state.cache["provider"].Definition = capabilityProviderDefinition("/v3")
	durable.IngressPorts["consumer"][api.CapabilityAIInferenceOpenAIV1] = 27002
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store changed capability ingress: %v", err)
	}
	next, err = desiredCapabilityBindings(state, "consumer", capabilityConsumerDefinition("OPENAI_BASE_URL"))
	if err != nil {
		t.Fatalf("desiredCapabilityBindings after ingress change: %v", err)
	}
	if capabilityGenerationMatches(&AppInstance{CapabilityBindings: desired}, next) {
		t.Fatal("private ingress port change did not invalidate consumer generation")
	}
}

func TestDesiredAcceleratorDevicesFollowSelectedAppInstance(t *testing.T) {
	state := newCapabilityTestState(t)
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	discoveries := 0
	manager := &AppManager{
		acceleratorDiscover: func() ([]string, error) {
			discoveries++
			return []string{"/dev/dri/renderD128", "/dev/accel/accel0"}, nil
		},
	}
	devices, err := manager.desiredAcceleratorDevices(
		state,
		"provider",
		capabilityProviderDefinition("/v1"),
	)
	if err != nil {
		t.Fatalf("selected provider devices: %v", err)
	}
	if !slices.Equal(devices, []string{"/dev/dri/renderD128", "/dev/accel/accel0"}) {
		t.Fatalf("selected provider devices = %v", devices)
	}

	devices, err = manager.desiredAcceleratorDevices(
		state,
		"provider",
		&api.AppDefinition{},
	)
	if err != nil {
		t.Fatalf("selected non-provider devices: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("selected app without capability received devices: %v", devices)
	}

	devices, err = manager.desiredAcceleratorDevices(
		state,
		"other-provider",
		capabilityProviderDefinition("/v1"),
	)
	if err != nil {
		t.Fatalf("unselected provider devices: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("unselected provider received devices: %v", devices)
	}
	if discoveries != 1 {
		t.Fatalf("accelerator discovery calls = %d, want one selected provider lookup", discoveries)
	}
}

func TestCapabilityProviderChangeRequiresDisclosureAcknowledgement(t *testing.T) {
	state := newCapabilityTestState(t)
	now := time.Now().UTC()
	for _, instanceID := range []string{"provider-old", "provider-new"} {
		if err := state.StoreApp(&AppInstance{
			InstanceID: instanceID,
			Enabled:    true,
			Definition: capabilityProviderDefinition("/v1"),
			CreatedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			t.Fatalf("store %s: %v", instanceID, err)
		}
	}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider-old"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	manager.stateManager = state
	manager.ForceLockState(false)

	statuses, err := manager.ListCapabilities(context.Background())
	if err != nil {
		t.Fatalf("ListCapabilities: %v", err)
	}
	if len(statuses) != 1 || statuses[0].ProviderChangeDisclosure != CapabilityProviderChangeDisclosure {
		t.Fatalf("capability disclosure = %+v, want %q", statuses, CapabilityProviderChangeDisclosure)
	}

	err = manager.SelectCapabilityProviderAcknowledged(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		"provider-new",
		false,
	)
	var confirmationRequired *CapabilityProviderChangeConfirmationRequiredError
	if !errors.As(err, &confirmationRequired) ||
		confirmationRequired.Current != "provider-old" ||
		confirmationRequired.Candidate != "provider-new" {
		t.Fatalf("unacknowledged switch error = %#v, want provider-change confirmation", err)
	}
	stored, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state after rejected switch: %v", err)
	}
	if got := stored.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != "provider-old" {
		t.Fatalf("default changed without acknowledgement: %q", got)
	}

	err = manager.UninstallAcknowledged(context.Background(), "provider-old", false)
	confirmationRequired = nil
	if !errors.As(err, &confirmationRequired) ||
		confirmationRequired.Capability != api.CapabilityAIInferenceOpenAIV1 ||
		confirmationRequired.Current != "provider-old" {
		t.Fatalf("unacknowledged selected-provider removal error = %#v, want provider-change confirmation", err)
	}
	if app, ok := state.GetApp("provider-old"); !ok || app == nil || !app.Enabled {
		t.Fatalf("selected provider changed before removal acknowledgement: %#v", app)
	}

	err = manager.SelectCapabilityProviderAcknowledged(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		"provider-new",
		true,
	)
	var pending *CapabilitySelectionReconcilePendingError
	if err != nil && !errors.As(err, &pending) {
		t.Fatalf("acknowledged switch: %v", err)
	}
	stored, err = state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state after acknowledged switch: %v", err)
	}
	if got := stored.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != "provider-new" {
		t.Fatalf("acknowledged switch default = %q, want provider-new", got)
	}

	err = manager.SelectCapabilityProviderAcknowledged(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		"provider-new",
		false,
	)
	pending = nil
	if err != nil && !errors.As(err, &pending) {
		t.Fatalf("same-provider repair without acknowledgement: %v", err)
	}
	stored, err = state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state after same-provider repair: %v", err)
	}
	if got := stored.Defaults[api.CapabilityAIInferenceOpenAIV1]; got != "provider-new" {
		t.Fatalf("same-provider repair default = %q, want provider-new", got)
	}
}

func TestUpdateListenersRejectsCapabilityProviderAuthorityChanges(t *testing.T) {
	state := newCapabilityTestState(t)
	definition := capabilityProviderDefinition("/v1")
	definition.Extensions["mode"] = string(ModeWorkspace)
	app := &AppInstance{
		InstanceID: "provider",
		Enabled:    true,
		Definition: definition,
	}
	if err := state.StoreApp(app); err != nil {
		t.Fatalf("store provider: %v", err)
	}
	manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	manager.stateManager = state
	manager.ForceLockState(false)

	listeners := append([]api.AppListener(nil), definition.Listeners...)
	listeners[0].Provides = nil
	if _, err := manager.UpdateListeners(context.Background(), app.InstanceID, listeners); err == nil ||
		!strings.Contains(err.Error(), "manifest update review") {
		t.Fatalf("capability authority listener update error = %v, want manifest-review rejection", err)
	}
	current, err := state.GetAppDefinition(app.InstanceID)
	if err != nil {
		t.Fatalf("load current provider: %v", err)
	}
	if _, _, ok := providedCapability(current, api.CapabilityAIInferenceOpenAIV1); !ok {
		t.Fatal("legacy listener update removed provider authority")
	}
}

func TestEnsureAcceleratorAccessBlocksOverlappingRecordedOwner(t *testing.T) {
	state := newCapabilityTestState(t)
	state.cache["old"] = &AppInstance{
		InstanceID:         "old",
		AcceleratorDevices: []string{"/dev/dri/renderD128"},
	}
	state.cache["new"] = &AppInstance{InstanceID: "new"}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "new"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	grants := 0
	manager := &AppManager{
		acceleratorDiscover: func() ([]string, error) {
			return []string{"/dev/dri/renderD128"}, nil
		},
		acceleratorPermission: func(_ context.Context, _ uint32, _ []string, grant bool) error {
			if grant {
				grants++
			}
			return nil
		},
	}
	runtimeConfig := container.PodmanRuntime{Credential: &syscall.Credential{Uid: 1234, Gid: 1234}}
	if _, err := manager.ensureAcceleratorAccess(context.Background(), state, "new", runtimeConfig, capabilityProviderDefinition("/v1"), []uint32{1234}); err == nil {
		t.Fatalf("overlapping recorded accelerator owner was accepted")
	}
	if grants != 0 {
		t.Fatalf("host grant applied despite overlapping owner")
	}

	state.cache["old"].AcceleratorDevices = nil
	devices, err := manager.ensureAcceleratorAccess(
		context.Background(),
		state,
		"new",
		runtimeConfig,
		capabilityProviderDefinition("/v1"),
		[]uint32{200999, 1234},
	)
	if err != nil {
		t.Fatalf("ensureAcceleratorAccess after withdrawal: %v", err)
	}
	if len(devices) != 1 || grants != 2 {
		t.Fatalf("grant result devices=%v grants=%d", devices, grants)
	}
	stored, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if stored.AcceleratorGrant == nil ||
		stored.AcceleratorGrant.Owner != "new" ||
		!slices.Equal(stored.AcceleratorGrant.UIDs, []uint32{1234, 200999}) {
		t.Fatalf("persisted accelerator grant = %#v", stored.AcceleratorGrant)
	}
}

func TestEnsureAcceleratorAccessRetainsFenceAfterFailedCompensation(t *testing.T) {
	state := newCapabilityTestState(t)
	state.cache["provider-a"] = &AppInstance{InstanceID: "provider-a"}
	state.cache["provider-b"] = &AppInstance{InstanceID: "provider-b"}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider-a"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	permissionCalls := 0
	manager := &AppManager{
		acceleratorDiscover: func() ([]string, error) {
			return []string{"/dev/dri/renderD128"}, nil
		},
		acceleratorPermission: func(_ context.Context, _ uint32, _ []string, _ bool) error {
			permissionCalls++
			return syscall.EIO
		},
	}
	runtimeConfig := container.PodmanRuntime{Credential: &syscall.Credential{Uid: 1234, Gid: 1234}}
	if _, err := manager.ensureAcceleratorAccess(context.Background(), state, "provider-a", runtimeConfig, capabilityProviderDefinition("/v1"), []uint32{1234}); err == nil {
		t.Fatal("failed accelerator grant and compensation were accepted")
	}
	stored, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if stored.AcceleratorGrant == nil || stored.AcceleratorGrant.Owner != "provider-a" {
		t.Fatalf("failed compensation lost durable fence: %#v", stored.AcceleratorGrant)
	}

	stored.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider-b"
	if err := state.storeCapabilityState(stored); err != nil {
		t.Fatalf("select provider-b: %v", err)
	}
	if _, err := manager.ensureAcceleratorAccess(context.Background(), state, "provider-b", runtimeConfig, capabilityProviderDefinition("/v1"), []uint32{1234}); err == nil {
		t.Fatal("provider-b bypassed persisted provider-a fence")
	}
	if permissionCalls != 2 {
		t.Fatalf("permission calls = %d, want only provider-a grant and compensation", permissionCalls)
	}
}

func TestRevokeAcceleratorAccessUsesPersistedUIDAndDevices(t *testing.T) {
	state := newCapabilityTestState(t)
	durable := newCapabilityState()
	durable.AcceleratorGrant = &acceleratorGrantRecord{
		Owner:   "provider",
		UIDs:    []uint32{4321},
		Devices: []string{"/dev/dri/renderD128"},
	}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	var gotUIDs []uint32
	var gotDevices []string
	var gotGrant bool
	manager := &AppManager{
		acceleratorPermission: func(_ context.Context, uid uint32, devices []string, grant bool) error {
			gotUIDs = append(gotUIDs, uid)
			gotDevices = append([]string(nil), devices...)
			gotGrant = grant
			return nil
		},
	}
	if err := manager.revokeAcceleratorAccess(context.Background(), state, "provider"); err != nil {
		t.Fatalf("revokeAcceleratorAccess: %v", err)
	}
	if !slices.Equal(gotUIDs, []uint32{4321}) || gotGrant || len(gotDevices) != 1 || gotDevices[0] != "/dev/dri/renderD128" {
		t.Fatalf("revoke permission uids=%v devices=%v grant=%v", gotUIDs, gotDevices, gotGrant)
	}
	stored, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if stored.AcceleratorGrant != nil {
		t.Fatalf("successful revoke retained fence: %#v", stored.AcceleratorGrant)
	}
}

func TestReconcileOrphanAcceleratorGrantRequiresProcessAbsenceProof(t *testing.T) {
	state := newCapabilityTestState(t)
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider-b"
	durable.AcceleratorGrant = &acceleratorGrantRecord{
		Owner:   "uncommitted-provider",
		UIDs:    []uint32{4321},
		Devices: []string{"/dev/dri/renderD128"},
	}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	quiesceErr := errors.New("injected absence-proof failure")
	revokeCalls := 0
	manager := &AppManager{
		userSessionQuiescer: func(context.Context, string) error { return quiesceErr },
		acceleratorPermission: func(_ context.Context, _ uint32, _ []string, grant bool) error {
			if !grant {
				revokeCalls++
			}
			return nil
		},
	}
	err := manager.reconcilePersistedAcceleratorGrant(context.Background(), state, nil)
	if !errors.Is(err, quiesceErr) {
		t.Fatalf("reconcile error = %v, want absence-proof failure", err)
	}
	if revokeCalls != 0 {
		t.Fatalf("grant revoked without process-absence proof: calls=%d", revokeCalls)
	}
	stored, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if stored.AcceleratorGrant == nil || stored.AcceleratorGrant.Owner != "uncommitted-provider" {
		t.Fatalf("failed absence proof dropped durable grant fence: %#v", stored.AcceleratorGrant)
	}

	manager.userSessionQuiescer = func(context.Context, string) error { return nil }
	if err := manager.reconcilePersistedAcceleratorGrant(context.Background(), state, nil); err != nil {
		t.Fatalf("reconcile after process absence: %v", err)
	}
	if revokeCalls != 1 {
		t.Fatalf("grant revocation calls = %d, want 1", revokeCalls)
	}
	stored, err = state.loadCapabilityState()
	if err != nil {
		t.Fatalf("reload capability state: %v", err)
	}
	if stored.AcceleratorGrant != nil {
		t.Fatalf("successful process quiescence retained grant: %#v", stored.AcceleratorGrant)
	}
}

func TestSelectSameCapabilityProviderRetriesPendingEffects(t *testing.T) {
	state := newCapabilityTestState(t)
	provider := &AppInstance{
		InstanceID: "provider",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v1"),
	}
	if err := state.StoreApp(provider); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider"
	durable.AcceleratorGrant = &acceleratorGrantRecord{
		Owner:   "previous-provider",
		UIDs:    []uint32{4321},
		Devices: []string{"/dev/dri/renderD128"},
	}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}

	manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	manager.stateManager = state
	manager.acceleratorDiscover = func() ([]string, error) { return nil, nil }
	revokeFails := true
	manager.acceleratorPermission = func(_ context.Context, _ uint32, _ []string, grant bool) error {
		if !grant && revokeFails {
			return syscall.EIO
		}
		return nil
	}

	err = manager.SelectCapabilityProvider(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		"provider",
	)
	var pending *CapabilitySelectionReconcilePendingError
	if !errors.As(err, &pending) {
		t.Fatalf("same-provider retry error = %v, want reconciliation pending", err)
	}

	revokeFails = false
	if err := manager.SelectCapabilityProvider(
		context.Background(),
		api.CapabilityAIInferenceOpenAIV1,
		"provider",
	); err != nil {
		t.Fatalf("same-provider repair retry: %v", err)
	}
	stored, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if stored.AcceleratorGrant != nil {
		t.Fatalf("successful retry retained stale grant: %#v", stored.AcceleratorGrant)
	}
}

func TestSelectCapabilityProviderSeedsTruthfulTaskLifecycle(t *testing.T) {
	state := newCapabilityTestState(t)
	provider := &AppInstance{
		InstanceID: "provider",
		Enabled:    true,
		Definition: capabilityProviderDefinition("/v1"),
	}
	if err := state.StoreApp(provider); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = "provider"
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store capability state: %v", err)
	}
	manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	manager.stateManager = state
	manager.acceleratorDiscover = func() ([]string, error) { return nil, nil }
	reporter := &recordingArtifactProgressReporter{}
	manager.SetProgressReporter(reporter)

	ctx := WithTaskID(context.Background(), "selection-task")
	if err := manager.SelectCapabilityProvider(
		ctx,
		api.CapabilityAIInferenceOpenAIV1,
		"provider",
	); err != nil {
		t.Fatalf("SelectCapabilityProvider: %v", err)
	}
	events := reporter.snapshot()
	if len(events) < 2 {
		t.Fatalf("selection events = %d, want start and completion", len(events))
	}
	if events[0].TaskType != taskTypeSelectCapability || events[0].Phase != taskPhaseValidating {
		t.Fatalf("unexpected selection start event: %#v", events[0])
	}
	last := events[len(events)-1]
	if last.TaskType != taskTypeSelectCapability || !last.IsComplete || last.Error != "" {
		t.Fatalf("unexpected selection completion event: %#v", last)
	}
}
