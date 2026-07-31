package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/resources/pressure"
)

func TestReconcileMultiContainer_StopsServicesWhenAnchorMissingAndDesiredStopped(t *testing.T) {
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)

	ctx := context.Background()
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("ensureStateManager: %v", err)
	}

	// RFC 20260130: listener name is the app identity, set Primary=true for test
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"side": {Image: "alpine:latest", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	layout, err := mgr.ensureAppVolumeLayout(ctx, "demo")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "demo", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main container: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, mainCID); err != nil {
		t.Fatalf("start main container: %v", err)
	}

	sideCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo__side",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "side", "service"),
	})
	if err != nil {
		t.Fatalf("create side container: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, sideCID); err != nil {
		t.Fatalf("start side container: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "demo",
		Status:          StatusRunning,
		PrimaryService:  "main",
		NetworkAnchorID: "", // missing anchor (manual deletion or partial failure)
		Containers: map[string]string{
			"main": mainCID,
			"side": sideCID,
		},
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, false); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}

	mainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if mainState.Running {
		t.Fatalf("expected main container to be stopped")
	}
	sideState, _ := mock.InspectContainerState(ctx, runtime, sideCID)
	if sideState.Running {
		t.Fatalf("expected side container to be stopped")
	}
}

// newReconcileTestEnv creates a standard test environment for reconcile tests.
// Returns the manager, mock, state manager, app definition, layout, and runtime.
func newReconcileTestEnv(t *testing.T) (*AppManager, *MockContainerManager, *FilesystemStateManager, *api.AppDefinition, appVolumeLayout, container.PodmanRuntime) {
	t.Helper()
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)

	ctx := context.Background()
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("ensureStateManager: %v", err)
	}

	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "testapp", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"side": {Image: "alpine:latest", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	layout, err := mgr.ensureAppVolumeLayout(ctx, "testapp")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "testapp", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	return mgr, mock, state, def, layout, runtime
}

func createRunningReconcileGroup(t *testing.T, mock *MockContainerManager, state *FilesystemStateManager, def *api.AppDefinition, runtime container.PodmanRuntime) *AppInstance {
	t.Helper()
	ctx := context.Background()
	anchorID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   networkAnchorContainerName("testapp"),
		Labels: piccoloLabels("testapp", "", "anchor"),
		Ports:  []container.PortMapping{{Host: 32001, Container: 8080, Protocol: "tcp"}},
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, anchorID); err != nil {
		t.Fatalf("start anchor: %v", err)
	}

	containers := make(map[string]string, len(def.Services))
	primary := primaryServiceFor(def, nil)
	for serviceName := range def.Services {
		id, createErr := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
			Name:   containerNameForService("testapp", serviceName, primary),
			Labels: piccoloLabels("testapp", serviceName, "service"),
		})
		if createErr != nil {
			t.Fatalf("create service %s: %v", serviceName, createErr)
		}
		if startErr := mock.StartContainer(ctx, runtime, id); startErr != nil {
			t.Fatalf("start service %s: %v", serviceName, startErr)
		}
		containers[serviceName] = id
	}
	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Enabled:         true,
		Status:          StatusRunning,
		PrimaryService:  primary,
		NetworkAnchorID: anchorID,
		Containers:      containers,
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	return appInst
}

func TestReconcileContainerGroupDoesNotAttachArtifactsForHealthyRuntime(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	def.Artifacts = map[string]api.AppArtifact{
		"model": {
			Source: api.ArtifactSource{
				Type:       "huggingface",
				Repository: "example/model",
				Revision:   "commit",
				Path:       ".",
			},
		},
	}
	mainService := def.Services["main"]
	mainService.Storage = &api.AppStorage{
		Artifacts: map[string]api.AppArtifactMount{
			"model": {Container: "/models/model"},
		},
	}
	def.Services["main"] = mainService
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	appInst.ArtifactReferences = map[string]string{"model": "ref-model"}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store artifact app: %v", err)
	}
	endpoints, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners)
	if err != nil {
		t.Fatalf("allocate steady-state publication: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("publication endpoints = %+v, want one", endpoints)
	}
	mock.containers[appInst.NetworkAnchorID].Spec.Ports = []container.PortMapping{{
		Host:      endpoints[0].HostBind,
		Container: endpoints[0].GuestPort,
		Protocol:  "tcp",
	}}
	mgr.serviceManager.SetAppContainerID(appInst.InstanceID, appInst.NetworkAnchorID)
	rootfs := &compensationRootfsManager{
		stubRootfsManager: newStubRootfsManager(t.TempDir()),
	}
	mgr.SetRootfsManager(rootfs)

	if err := mgr.reconcileContainerGroup(
		context.Background(),
		state,
		appInst,
		def,
		layout,
		runtime,
		true,
	); err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	if len(rootfs.artifactAttached) != 0 {
		t.Fatalf("steady-state reconcile attached artifacts: %v", rootfs.artifactAttached)
	}

	missingServiceID := appInst.Containers["side"]
	if err := mock.RemoveContainer(context.Background(), runtime, missingServiceID); err != nil {
		t.Fatalf("remove service for repair: %v", err)
	}
	if err := mgr.reconcileContainerGroup(
		context.Background(),
		state,
		appInst,
		def,
		layout,
		runtime,
		true,
	); err != nil {
		t.Fatalf("missing-service reconcile: %v", err)
	}
	if !reflect.DeepEqual(rootfs.artifactAttached, []string{"ref-model"}) {
		t.Fatalf("missing-service reconcile artifact attachments = %v, want one recorded reference", rootfs.artifactAttached)
	}
}

func TestCommitRecreatedAppMetadataPublishesOnlyAfterDurableWrite(t *testing.T) {
	state := newCapabilityTestState(t)
	current := &AppInstance{
		InstanceID:         "consumer",
		Enabled:            true,
		Definition:         capabilityConsumerDefinition("OPENAI_BASE_URL"),
		NetworkAnchorID:    "old-anchor",
		Containers:         map[string]string{"main": "old-container"},
		CapabilityBindings: map[string]string{api.CapabilityAIInferenceOpenAIV1: "provider-a"},
		CreatedAt:          time.Now().Add(-time.Hour),
		UpdatedAt:          time.Now().Add(-time.Minute),
	}
	if err := state.StoreApp(current); err != nil {
		t.Fatalf("store current app: %v", err)
	}
	recreated := &AppInstance{
		InstanceID:         "consumer",
		PrimaryService:     "main",
		NetworkAnchorID:    "new-anchor",
		Containers:         map[string]string{"main": "new-container"},
		CapabilityBindings: map[string]string{api.CapabilityAIInferenceOpenAIV1: "provider-b"},
	}

	writeErr := errors.New("injected metadata write failure")
	state.storeAppMetadataHook = func(string, *AppInstance) error { return writeErr }
	if err := commitRecreatedAppMetadata(state, current, recreated); !errors.Is(err, writeErr) {
		t.Fatalf("commit error = %v, want injected write failure", err)
	}
	cached, ok := state.GetApp("consumer")
	if !ok {
		t.Fatal("current app disappeared after failed metadata write")
	}
	if got := cached.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1]; got != "provider-a" {
		t.Fatalf("failed write published cache binding %q", got)
	}
	if current.NetworkAnchorID != "old-anchor" ||
		current.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] != "provider-a" {
		t.Fatalf("failed write mutated caller state: %+v", current)
	}

	state.storeAppMetadataHook = nil
	if err := commitRecreatedAppMetadata(state, current, recreated); err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	cached, ok = state.GetApp("consumer")
	if !ok {
		t.Fatal("recreated app missing after successful retry")
	}
	if got := cached.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1]; got != "provider-b" {
		t.Fatalf("successful retry binding = %q, want provider-b", got)
	}
	if cached.NetworkAnchorID != "new-anchor" || current.NetworkAnchorID != "new-anchor" {
		t.Fatalf("successful retry did not publish replacement: cached=%+v current=%+v", cached, current)
	}
	current.Containers["main"] = "caller-only-mutation"
	current.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] = "caller-only-provider"
	if got := cached.Containers["main"]; got != "new-container" {
		t.Fatalf("successful commit retained caller container-map alias: %q", got)
	}
	if got := cached.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1]; got != "provider-b" {
		t.Fatalf("successful commit retained caller binding-map alias: %q", got)
	}
}

func TestCommitRemovedContainerGroupPublishesOnlyAfterDurableWrite(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	current := &AppInstance{
		InstanceID:         "testapp",
		Enabled:            true,
		Definition:         def,
		NetworkAnchorID:    "old-anchor",
		Containers:         map[string]string{"main": "old-main"},
		AcceleratorDevices: []string{"/dev/dri/renderD128"},
		CapabilityBindings: map[string]string{api.CapabilityAIInferenceOpenAIV1: "provider"},
	}
	if err := state.StoreApp(current); err != nil {
		t.Fatalf("store current app: %v", err)
	}
	injected := errors.New("injected cleared metadata failure")
	state.storeAppMetadataHook = func(string, *AppInstance) error { return injected }

	if err := mgr.commitRemovedContainerGroup(state, current); !errors.Is(err, injected) {
		t.Fatalf("commit removed error = %v, want injected failure", err)
	}
	stored, ok := state.GetApp(current.InstanceID)
	if !ok || stored.NetworkAnchorID != "old-anchor" || stored.Containers["main"] != "old-main" {
		t.Fatalf("failed cleared commit published candidate: %+v", stored)
	}
	if current.NetworkAnchorID != "old-anchor" ||
		len(current.AcceleratorDevices) != 1 ||
		current.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] != "provider" {
		t.Fatalf("failed cleared commit mutated caller: %+v", current)
	}
}

func prebuiltReconcileRootfs(t *testing.T, def *api.AppDefinition) map[string]*rootfsMountInfo {
	t.Helper()
	mountPath := t.TempDir()
	result := make(map[string]*rootfsMountInfo, 1+len(def.Services))
	for serviceName := range def.Services {
		result[serviceName] = &rootfsMountInfo{handle: persistence.RootfsHandle{
			VolumeID:  persistence.ServiceRootfsVolumeID("testapp", serviceName),
			MountPath: mountPath,
			ReadOnly:  true,
		}}
	}
	result[networkAnchorServiceName] = &rootfsMountInfo{handle: persistence.RootfsHandle{
		VolumeID:  persistence.ServiceRootfsVolumeID("testapp", networkAnchorServiceName),
		MountPath: mountPath,
		ReadOnly:  true,
	}}
	return result
}

func newMissingReconcileApp(
	t *testing.T,
) (*AppManager, *MockContainerManager, *FilesystemStateManager, *api.AppDefinition, appVolumeLayout, container.PodmanRuntime, *AppInstance) {
	t.Helper()
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	mgr.serviceManager.UseInMemoryNetworkForTest()
	mgr.SetRootfsManager(newStubRootfsManager(t.TempDir()))
	now := time.Now()
	current := &AppInstance{
		InstanceID:     "testapp",
		Enabled:        true,
		PrimaryService: "main",
		Definition:     def,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := state.StoreApp(current); err != nil {
		t.Fatalf("store missing app: %v", err)
	}
	return mgr, mock, state, def, layout, runtime, current
}

type compensationRootfsManager struct {
	*stubRootfsManager
	artifactAttached   []string
	artifactDetached   []string
	artifactDestroyed  []string
	artifactGCRetained []map[string]struct{}
}

func (m *compensationRootfsManager) EnsureGoldenContent(
	_ context.Context,
	req persistence.GoldenContentRequest,
) (persistence.GoldenContentHandle, error) {
	return persistence.GoldenContentHandle{
		GoldenID: "golden-candidate-model",
		Identity: req.Identity,
	}, nil
}

func (m *compensationRootfsManager) CreateArtifactReference(
	_ context.Context,
	req persistence.ArtifactReferenceRequest,
) (persistence.ArtifactHandle, error) {
	return persistence.ArtifactHandle{
		MountPath: m.baseDir,
		Created:   true,
	}, nil
}

func (m *compensationRootfsManager) AttachArtifactReference(
	_ context.Context,
	referenceID string,
) (persistence.ArtifactHandle, error) {
	m.artifactAttached = append(m.artifactAttached, referenceID)
	return persistence.ArtifactHandle{
		MountPath: m.baseDir,
	}, nil
}

func (m *compensationRootfsManager) DetachArtifactReference(_ context.Context, referenceID string) error {
	m.artifactDetached = append(m.artifactDetached, referenceID)
	return nil
}

func (m *compensationRootfsManager) DestroyArtifactReference(_ context.Context, referenceID string) error {
	m.artifactDestroyed = append(m.artifactDestroyed, referenceID)
	return nil
}

func (m *compensationRootfsManager) GarbageCollectArtifactReferences(
	_ context.Context,
	retained map[string]struct{},
) error {
	copied := make(map[string]struct{}, len(retained))
	for referenceID := range retained {
		copied[referenceID] = struct{}{}
	}
	m.artifactGCRetained = append(m.artifactGCRetained, copied)
	return nil
}

func compensationCandidate(
	t *testing.T,
) (*AppManager, *MockContainerManager, *FilesystemStateManager, *api.AppDefinition, container.PodmanRuntime, *AppInstance, *AppInstance, *compensationRootfsManager) {
	t.Helper()
	mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
	mgr.serviceManager.UseInMemoryNetworkForTest()
	candidate := createRunningReconcileGroup(t, mock, state, def, runtime)
	candidate.ArtifactReferences = map[string]string{
		"shared": "ref-shared",
		"new":    "ref-new",
	}
	candidate.AcceleratorDevices = []string{"/dev/dri/renderD128"}
	candidate.ActiveRootfs = map[string]string{
		networkAnchorServiceName: "rootfs-anchor",
		"main":                   "rootfs-main",
		"side":                   "rootfs-side",
	}
	committed, err := detachedAppCandidate(candidate)
	if err != nil {
		t.Fatalf("clone committed app: %v", err)
	}
	committed.NetworkAnchorID = ""
	committed.Containers = nil
	committed.ArtifactReferences = map[string]string{"shared": "ref-shared"}
	committed.AcceleratorDevices = nil
	committed.ActiveRootfs = nil
	if err := state.StoreApp(committed); err != nil {
		t.Fatalf("store committed app: %v", err)
	}
	if _, err := mgr.serviceManager.AllocateForApp(candidate.InstanceID, def.Listeners); err != nil {
		t.Fatalf("allocate candidate publication: %v", err)
	}
	mgr.serviceManager.SetAppContainerID(candidate.InstanceID, candidate.NetworkAnchorID)
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = candidate.InstanceID
	durable.AcceleratorGrant = &acceleratorGrantRecord{
		Owner:   candidate.InstanceID,
		UIDs:    []uint32{1000},
		Devices: append([]string(nil), candidate.AcceleratorDevices...),
	}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store accelerator grant: %v", err)
	}
	rootfs := &compensationRootfsManager{
		stubRootfsManager: newStubRootfsManager(t.TempDir()),
	}
	mgr.SetRootfsManager(rootfs)
	return mgr, mock, state, def, runtime, committed, candidate, rootfs
}

func TestCompensateUncommittedContainerGroupRetainsSelectedAcceleratorPermission(t *testing.T) {
	mgr, mock, state, def, runtime, committed, candidate, rootfs := compensationCandidate(t)
	mgr.acceleratorPermission = func(context.Context, uint32, []string, bool) error {
		t.Fatal("candidate compensation changed selected-app accelerator permission")
		return nil
	}

	if err := mgr.compensateUncommittedContainerGroup(state, committed, candidate, def, runtime); err != nil {
		t.Fatalf("compensate candidate: %v", err)
	}
	if len(mock.containers) != 0 {
		t.Fatalf("candidate containers survived compensation: %+v", mock.containers)
	}
	if mgr.serviceManager.AppPublicationActive(candidate.InstanceID) {
		t.Fatal("candidate publication survived compensation")
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if durable.AcceleratorGrant == nil || durable.AcceleratorGrant.Owner != candidate.InstanceID {
		t.Fatalf("selected-app accelerator grant was not retained: %+v", durable.AcceleratorGrant)
	}
	if !reflect.DeepEqual(rootfs.artifactDetached, []string{"ref-new", "ref-shared"}) {
		t.Fatalf("detached artifact refs = %v", rootfs.artifactDetached)
	}
	if !reflect.DeepEqual(rootfs.artifactDestroyed, []string{"ref-new"}) {
		t.Fatalf("destroyed artifact refs = %v", rootfs.artifactDestroyed)
	}
	for _, volumeID := range []string{"rootfs-anchor", "rootfs-main", "rootfs-side"} {
		if !slices.Contains(rootfs.detached, volumeID) {
			t.Fatalf("candidate rootfs %s was not detached: %v", volumeID, rootfs.detached)
		}
	}
}

func TestCompensateUncommittedContainerGroupRemovalFailureStaysFailClosed(t *testing.T) {
	mgr, mock, state, def, runtime, committed, candidate, rootfs := compensationCandidate(t)
	mock.removeError = errors.New("injected remove failure")
	mgr.userSessionQuiescer = func(context.Context, string) error {
		return errors.New("injected quiesce failure")
	}

	err := mgr.compensateUncommittedContainerGroup(state, committed, candidate, def, runtime)
	if err == nil || !strings.Contains(err.Error(), "injected remove failure") {
		t.Fatalf("compensation error = %v, want removal failure", err)
	}
	if mgr.serviceManager.AppPublicationActive(candidate.InstanceID) {
		t.Fatal("failed compensation left candidate publication active")
	}
	if len(rootfs.artifactDetached) != 0 || len(rootfs.artifactDestroyed) != 0 || len(rootfs.detached) != 0 {
		t.Fatalf(
			"failed removal detached live mounts: artifact_detached=%v artifact_destroyed=%v rootfs=%v",
			rootfs.artifactDetached,
			rootfs.artifactDestroyed,
			rootfs.detached,
		)
	}
	durable, loadErr := state.loadCapabilityState()
	if loadErr != nil {
		t.Fatalf("load capability state: %v", loadErr)
	}
	if durable.AcceleratorGrant == nil || durable.AcceleratorGrant.Owner != candidate.InstanceID {
		t.Fatalf("failed removal dropped live-process accelerator fence: %+v", durable.AcceleratorGrant)
	}
}

func TestCompensateUncommittedContainerGroupUsesUserSessionAbsenceProofAndRetainsSelectedPermission(t *testing.T) {
	mgr, mock, state, def, runtime, committed, candidate, rootfs := compensationCandidate(t)
	mock.removeError = errors.New("injected remove failure")
	var quiesced string
	mgr.userSessionQuiescer = func(_ context.Context, instanceID string) error {
		quiesced = instanceID
		return nil
	}
	mgr.acceleratorPermission = func(_ context.Context, _ uint32, _ []string, grant bool) error {
		t.Fatalf("candidate compensation changed selected-app accelerator permission (grant=%v)", grant)
		return nil
	}

	if err := mgr.compensateUncommittedContainerGroup(
		state,
		committed,
		candidate,
		def,
		runtime,
	); err != nil {
		t.Fatalf("compensate after user-session quiescence: %v", err)
	}
	if quiesced != candidate.InstanceID {
		t.Fatalf("quiesced instance = %q, want %q", quiesced, candidate.InstanceID)
	}
	if mgr.serviceManager.AppPublicationActive(candidate.InstanceID) {
		t.Fatal("candidate publication survived user-session quiescence")
	}
	durable, err := state.loadCapabilityState()
	if err != nil {
		t.Fatalf("load capability state: %v", err)
	}
	if durable.AcceleratorGrant == nil || durable.AcceleratorGrant.Owner != candidate.InstanceID {
		t.Fatalf("selected-app accelerator permission was not retained: %+v", durable.AcceleratorGrant)
	}
	if len(rootfs.artifactDetached) == 0 || len(rootfs.detached) == 0 {
		t.Fatalf(
			"mount ownership survived authoritative process quiescence: artifacts=%v rootfs=%v",
			rootfs.artifactDetached,
			rootfs.detached,
		)
	}
}

func TestRecreateMissingMultiContainerMetadataFailureCompensatesCandidate(t *testing.T) {
	mgr, mock, state, def, layout, runtime, current := newMissingReconcileApp(t)
	prebuilt := prebuiltReconcileRootfs(t, def)
	injected := errors.New("injected recovered metadata failure")
	state.storeAppMetadataHook = func(_ string, candidate *AppInstance) error {
		if candidate.NetworkAnchorID != "" {
			return injected
		}
		return nil
	}

	err := mgr.recreateMissingMultiContainer(
		context.Background(),
		state,
		current,
		def,
		layout,
		runtime,
		prebuilt,
	)
	if !errors.Is(err, injected) {
		t.Fatalf("recreate error = %v, want injected metadata failure", err)
	}
	if len(mock.containers) != 0 {
		t.Fatalf("uncommitted candidate containers survived: %+v", mock.containers)
	}
	if mgr.serviceManager.AppPublicationActive(current.InstanceID) {
		t.Fatal("uncommitted candidate publication remained active")
	}
	if id, ok := mgr.serviceManager.GetAppContainerID(current.InstanceID); ok && id != "" {
		t.Fatalf("uncommitted backend ID remained published: %q", id)
	}
	stored, ok := state.GetApp(current.InstanceID)
	if !ok || stored.NetworkAnchorID != "" || len(stored.Containers) != 0 {
		t.Fatalf("failed metadata commit changed committed app: %+v", stored)
	}

	state.storeAppMetadataHook = nil
	if err := mgr.recreateMissingMultiContainer(
		context.Background(),
		state,
		current,
		def,
		layout,
		runtime,
		prebuilt,
	); err != nil {
		t.Fatalf("retry recreate: %v", err)
	}
	if current.NetworkAnchorID == "" || len(current.Containers) != len(def.Services) {
		t.Fatalf("retry did not commit recreated group: %+v", current)
	}
	if !mgr.serviceManager.AppPublicationActive(current.InstanceID) {
		t.Fatal("successful retry did not publish services")
	}
}

func TestPreparedRecreateMetadataFailureKeepsPublicationSuspended(t *testing.T) {
	mgr, mock, state, def, layout, runtime, current := newMissingReconcileApp(t)
	prebuilt := prebuiltReconcileRootfs(t, def)
	if _, err := mgr.serviceManager.AllocateForApp(current.InstanceID, def.Listeners); err != nil {
		t.Fatalf("allocate existing publication: %v", err)
	}
	resumeToken := mgr.serviceManager.SuspendAppPublication(current.InstanceID)
	plan, err := mgr.serviceManager.PrepareReconcile(current.InstanceID, def.Listeners)
	if err != nil {
		t.Fatalf("prepare recreate listeners: %v", err)
	}
	injected := errors.New("injected prepared metadata failure")
	state.storeAppMetadataHook = func(_ string, candidate *AppInstance) error {
		if candidate.NetworkAnchorID != "" {
			return injected
		}
		return nil
	}

	err = mgr.recreateMissingMultiContainerPrepared(
		context.Background(),
		state,
		current,
		def,
		layout,
		runtime,
		prebuilt,
		plan,
		resumeToken,
	)
	if !errors.Is(err, injected) {
		t.Fatalf("prepared recreate error = %v, want injected metadata failure", err)
	}
	if len(mock.containers) != 0 {
		t.Fatalf("prepared candidate containers survived: %+v", mock.containers)
	}
	if mgr.serviceManager.AppPublicationActive(current.InstanceID) {
		t.Fatal("failed prepared candidate reactivated suspended publication")
	}
	if id, ok := mgr.serviceManager.GetAppContainerID(current.InstanceID); ok && id != "" {
		t.Fatalf("failed prepared candidate retained backend ID: %q", id)
	}

	state.storeAppMetadataHook = nil
	resumeToken = mgr.serviceManager.SuspendAppPublication(current.InstanceID)
	plan, err = mgr.serviceManager.PrepareReconcile(current.InstanceID, def.Listeners)
	if err != nil {
		t.Fatalf("prepare retry listeners: %v", err)
	}
	if err := mgr.recreateMissingMultiContainerPrepared(
		context.Background(),
		state,
		current,
		def,
		layout,
		runtime,
		prebuilt,
		plan,
		resumeToken,
	); err != nil {
		t.Fatalf("prepared recreate retry: %v", err)
	}
	if !mgr.serviceManager.AppPublicationActive(current.InstanceID) {
		t.Fatal("successful prepared retry did not publish services")
	}
}

func TestDetachedCommitHelpersDoNotPublishCallerAliases(t *testing.T) {
	tests := []struct {
		name       string
		commit     func(*FilesystemStateManager, *AppInstance, *AppInstance) error
		injectFail func(*FilesystemStateManager, error)
		clearFail  func(*FilesystemStateManager)
	}{
		{
			name:   "definition and metadata",
			commit: commitDetachedApp,
			injectFail: func(state *FilesystemStateManager, injected error) {
				state.storeAppDefinitionHook = func(string, *AppInstance) error { return injected }
			},
			clearFail: func(state *FilesystemStateManager) {
				state.storeAppDefinitionHook = nil
			},
		},
		{
			name:   "metadata only",
			commit: commitDetachedAppMetadata,
			injectFail: func(state *FilesystemStateManager, injected error) {
				state.storeAppMetadataHook = func(string, *AppInstance) error { return injected }
			},
			clearFail: func(state *FilesystemStateManager) {
				state.storeAppMetadataHook = nil
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			state := newCapabilityTestState(t)
			current := &AppInstance{
				InstanceID:         "consumer",
				Enabled:            true,
				Definition:         capabilityConsumerDefinition("OPENAI_BASE_URL"),
				Containers:         map[string]string{"main": "old-container"},
				CapabilityBindings: map[string]string{api.CapabilityAIInferenceOpenAIV1: "provider-a"},
			}
			if err := state.StoreApp(current); err != nil {
				t.Fatalf("store current app: %v", err)
			}
			candidate, err := detachedAppCandidate(current)
			if err != nil {
				t.Fatalf("detached candidate: %v", err)
			}
			candidate.Containers["main"] = "new-container"
			candidate.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] = "provider-b"

			injected := errors.New("injected durable write failure")
			test.injectFail(state, injected)
			if err := test.commit(state, current, candidate); !errors.Is(err, injected) {
				t.Fatalf("commit error = %v, want injected failure", err)
			}
			cached, ok := state.GetApp("consumer")
			if !ok {
				t.Fatal("current app disappeared after failed write")
			}
			if got := cached.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1]; got != "provider-a" {
				t.Fatalf("failed write published binding %q", got)
			}

			test.clearFail(state)
			if err := test.commit(state, current, candidate); err != nil {
				t.Fatalf("retry commit: %v", err)
			}
			cached, ok = state.GetApp("consumer")
			if !ok {
				t.Fatal("candidate missing after successful retry")
			}
			candidate.Containers["main"] = "candidate-only-mutation"
			candidate.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] = "candidate-only-provider"
			current.Containers["main"] = "current-only-mutation"
			current.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1] = "current-only-provider"
			if got := cached.Containers["main"]; got != "new-container" {
				t.Fatalf("cache retained caller container-map alias: %q", got)
			}
			if got := cached.CapabilityBindings[api.CapabilityAIInferenceOpenAIV1]; got != "provider-b" {
				t.Fatalf("cache retained caller binding-map alias: %q", got)
			}
		})
	}
}

func TestReconcileUnknownObservationPreservesRunningProjectionRoutesAndAttemptBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*MockContainerManager, *AppInstance)
	}{
		{
			name: "anchor inspect",
			inject: func(mock *MockContainerManager, app *AppInstance) {
				mock.inspectErrorForContainer = map[string]error{app.NetworkAnchorID: fmt.Errorf("pthread_create failed: resource temporarily unavailable")}
			},
		},
		{
			name: "service inspect",
			inject: func(mock *MockContainerManager, app *AppInstance) {
				mock.inspectErrorForContainer = map[string]error{app.Containers["main"]: fmt.Errorf("podman inspect timed out")}
			},
		},
		{
			name: "container enumeration",
			inject: func(mock *MockContainerManager, _ *AppInstance) {
				mock.listError = fmt.Errorf("podman ps failed")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
			appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
			mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
			if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
				t.Fatalf("allocate route: %v", err)
			}
			tc.inject(mock, appInst)

			if err := mgr.reconcileApp(context.Background(), state, appInst); err != nil {
				t.Fatalf("reconcile unknown observation: %v", err)
			}
			if appInst.StartupAttempts != 0 {
				t.Fatalf("unknown observation consumed startup attempts: %d", appInst.StartupAttempts)
			}
			if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
				t.Fatalf("status = %q, want retained %q", got, StatusRunning)
			}
			if _, err := mgr.serviceManager.GetByApp(appInst.InstanceID); err != nil {
				t.Fatalf("route was deactivated after unknown observation: %v", err)
			}
			if _, ok := mock.containers[appInst.NetworkAnchorID]; !ok {
				t.Fatal("anchor was mutated after unknown observation")
			}
			for serviceName, id := range appInst.Containers {
				if _, ok := mock.containers[id]; !ok {
					t.Fatalf("service %s was mutated after unknown observation", serviceName)
				}
			}
		})
	}
}

func TestRestoreServicesPreservesExistingRouteWhenObservationIsUnknown(t *testing.T) {
	mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
		t.Fatalf("allocate route: %v", err)
	}
	mock.inspectErrorForContainer = map[string]error{appInst.NetworkAnchorID: fmt.Errorf("podman unavailable")}

	mgr.RestoreServices(context.Background())

	if _, err := mgr.serviceManager.GetByApp(appInst.InstanceID); err != nil {
		t.Fatalf("restore deactivated last-known route on unknown: %v", err)
	}
}

func TestReconcileMissingAnchorWarningAfterRootfsPreservesActivePublication(t *testing.T) {
	pressure.DefaultAdmission.ResetForTest()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)

	mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
	mgr.serviceManager.UseInMemoryNetworkForTest()
	claim := 32080
	def.Listeners[0].PortClaim = &claim
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
	mgr.setObservedStatusMessage(appInst.InstanceID, "healthy")
	if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
		t.Fatalf("publish route: %v", err)
	}
	if !mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
		t.Fatal("precondition: route-bearing publication is not active")
	}

	firstFailure := time.Now().Add(-time.Minute)
	appInst.StartupAttempts = 2
	appInst.FirstStartupFailureAt = &firstFailure
	appInst.ActiveRootfs = map[string]string{"main": "rootfs-main"}
	if err := state.StoreAppMetadata(appInst); err != nil {
		t.Fatalf("store pre-reconcile state: %v", err)
	}
	beforeRegistry := mgr.serviceManager.SnapshotRegistry()
	beforeClaims := mgr.serviceManager.ActivePortClaims()
	beforeContainers := make(map[string]string, len(appInst.Containers))
	for serviceName, id := range appInst.Containers {
		beforeContainers[serviceName] = id
	}
	beforeNextID := mock.nextID

	// Preserve running services but remove the anchor, then fence admission
	// exactly after the rootfs preflight. The second lifecycle check must stop
	// before cleanup or publication withdrawal.
	delete(mock.containers, appInst.NetworkAnchorID)
	rootfs := newStubRootfsManager(t.TempDir())
	rootfs.exists = map[string]bool{"rootfs-main": true}
	rootfs.attachHook = pressure.DefaultAdmission.Fence
	mgr.SetRootfsManager(rootfs)

	err := mgr.reconcileApp(context.Background(), state, appInst)
	if !pressure.IsAdmissionError(err) {
		t.Fatalf("reconcile error = %v, want task-pressure admission error", err)
	}
	if mock.nextID != beforeNextID {
		t.Fatalf("container recreation started: next ID = %d, want %d", mock.nextID, beforeNextID)
	}
	for serviceName, id := range beforeContainers {
		got, ok := mock.containers[id]
		if !ok || got.Status != "running" {
			t.Fatalf("service %s changed across admission fence: %+v", serviceName, got)
		}
	}
	if !mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
		t.Fatal("active publication was withdrawn; app aliases would become ineligible")
	}
	if got := mgr.serviceManager.SnapshotRegistry(); !reflect.DeepEqual(got, beforeRegistry) {
		t.Fatalf("route registry changed across admission fence: got %+v want %+v", got, beforeRegistry)
	}
	if got := mgr.serviceManager.ActivePortClaims(); !reflect.DeepEqual(got, beforeClaims) {
		t.Fatalf("active port claims changed across admission fence: got %+v want %+v", got, beforeClaims)
	}
	if status, message := mgr.getObservedStatusAndMessage(appInst.InstanceID); status != StatusRunning || message != "healthy" {
		t.Fatalf("observed projection = (%q, %q), want (%q, %q)", status, message, StatusRunning, "healthy")
	}
	if appInst.StartupAttempts != 2 || appInst.FirstStartupFailureAt == nil || !appInst.FirstStartupFailureAt.Equal(firstFailure) {
		t.Fatalf("startup history changed: attempts=%d first=%v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if mgr.startupAttemptActive(appInst.InstanceID) {
		t.Fatal("admission pause retained startup-attempt ownership")
	}
	stored, ok := state.GetApp(appInst.InstanceID)
	if !ok {
		t.Fatal("stored app missing")
	}
	if stored.NetworkAnchorID != appInst.NetworkAnchorID || !reflect.DeepEqual(stored.Containers, beforeContainers) || stored.StartupAttempts != 2 {
		t.Fatalf("stored lifecycle state changed: anchor=%q containers=%v attempts=%d", stored.NetworkAnchorID, stored.Containers, stored.StartupAttempts)
	}
}

func TestDesiredRunningWarningAfterFinalRemoveCommitsInactiveClearedState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MockContainerManager, *AppInstance)
	}{
		{
			name: "missing anchor cleanup",
			mutate: func(mock *MockContainerManager, appInst *AppInstance) {
				delete(mock.containers, appInst.NetworkAnchorID)
			},
		},
		{
			name: "stale anchor recovery",
			mutate: func(mock *MockContainerManager, _ *AppInstance) {
				for _, item := range mock.containers {
					item.Status = "stale"
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pressure.DefaultAdmission.ResetForTest()
			t.Cleanup(pressure.DefaultAdmission.ResetForTest)

			mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
			mgr.serviceManager.UseInMemoryNetworkForTest()
			claim := 32080
			def.Listeners[0].PortClaim = &claim
			appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
			mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
			mgr.setObservedStatusMessage(appInst.InstanceID, "healthy")
			if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
				t.Fatalf("publish route: %v", err)
			}
			firstFailure := time.Now().Add(-time.Minute)
			appInst.StartupAttempts = 2
			appInst.FirstStartupFailureAt = &firstFailure
			if err := state.StoreAppMetadata(appInst); err != nil {
				t.Fatalf("store pre-reconcile state: %v", err)
			}
			beforeRegistry := mgr.serviceManager.SnapshotRegistry()
			tc.mutate(mock, appInst)
			mock.removeHook = func(string) {
				if len(mock.containers) == 0 {
					pressure.DefaultAdmission.Fence()
				}
			}

			err := mgr.reconcileApp(context.Background(), state, appInst)
			if !pressure.IsAdmissionError(err) {
				t.Fatalf("reconcile error = %v, want task-pressure admission error", err)
			}
			if len(mock.containers) != 0 {
				t.Fatalf("postcondition: final removal did not complete: %+v", mock.containers)
			}
			if mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
				t.Fatal("publication remained active after its runtime was authoritatively removed")
			}
			if got := mgr.serviceManager.SnapshotRegistry(); !reflect.DeepEqual(got, beforeRegistry) {
				t.Fatalf("suspended route registry changed: got %+v want %+v", got, beforeRegistry)
			}
			if got := mgr.serviceManager.ActivePortClaims(); len(got) != 0 {
				t.Fatalf("active port claims remained after runtime removal: %+v", got)
			}
			if status, message := mgr.getObservedStatusAndMessage(appInst.InstanceID); status != StatusStarting || message != "Containers removed; recreation pending" {
				t.Fatalf("observed projection = (%q, %q), want safe pending projection", status, message)
			}
			if appInst.NetworkAnchorID != "" || len(appInst.Containers) != 0 {
				t.Fatalf("in-memory IDs were not cleared: anchor=%q containers=%v", appInst.NetworkAnchorID, appInst.Containers)
			}
			if appInst.StartupAttempts != 2 || appInst.FirstStartupFailureAt == nil || !appInst.FirstStartupFailureAt.Equal(firstFailure) {
				t.Fatalf("startup history changed: attempts=%d first=%v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
			}
			stored, ok := state.GetApp(appInst.InstanceID)
			if !ok {
				t.Fatal("stored app missing")
			}
			if stored.NetworkAnchorID != "" || len(stored.Containers) != 0 || stored.StartupAttempts != 2 {
				t.Fatalf("stored cleared state = anchor=%q containers=%v attempts=%d", stored.NetworkAnchorID, stored.Containers, stored.StartupAttempts)
			}
			if containerID, ok := mgr.serviceManager.GetAppContainerID(appInst.InstanceID); ok && containerID != "" {
				t.Fatalf("service publication retained stale container ID %q", containerID)
			}

			pressure.DefaultAdmission.ResetForTest()
			mock.removeHook = nil
			if err := mgr.reconcileApp(context.Background(), state, appInst); err != nil {
				t.Fatalf("retry reconcile after pressure cleared: %v", err)
			}
			if appInst.NetworkAnchorID == "" || len(appInst.Containers) != len(def.Services) {
				t.Fatalf("retry did not persist replacement IDs: anchor=%q containers=%v", appInst.NetworkAnchorID, appInst.Containers)
			}
			if !mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
				t.Fatal("retry did not reactivate publication with its fresh suspension token")
			}
			if got := mgr.serviceManager.ActivePortClaims(); len(got) != 1 || got[0].Port != claim {
				t.Fatalf("retry active port claims = %+v, want claim %d", got, claim)
			}
			if status, _ := mgr.getObservedStatusAndMessage(appInst.InstanceID); status != StatusRunning {
				t.Fatalf("retry observed status = %q, want %q", status, StatusRunning)
			}
		})
	}
}

func TestDesiredStoppedReconcileStillWithdrawsPublicationUnderWarning(t *testing.T) {
	pressure.DefaultAdmission.ResetForTest()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)

	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	mgr.serviceManager.UseInMemoryNetworkForTest()
	claim := 32080
	def.Listeners[0].PortClaim = &claim
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
	if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
		t.Fatalf("publish route: %v", err)
	}
	pressure.DefaultAdmission.Fence()

	if err := mgr.reconcileContainerGroup(context.Background(), state, appInst, def, layout, runtime, false); err != nil {
		t.Fatalf("desired-stopped reconcile: %v", err)
	}
	if mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
		t.Fatal("desired-stopped reconcile retained active publication")
	}
	if claims := mgr.serviceManager.ActivePortClaims(); len(claims) != 0 {
		t.Fatalf("desired-stopped reconcile retained active port claims: %+v", claims)
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusStopped {
		t.Fatalf("observed status = %q, want %q", got, StatusStopped)
	}
}

func seedDesiredStoppedAcceleratorGrant(
	t *testing.T,
	state *FilesystemStateManager,
	appInst *AppInstance,
) {
	t.Helper()
	appInst.AcceleratorDevices = []string{"/dev/dri/renderD128"}
	if err := state.StoreAppMetadata(appInst); err != nil {
		t.Fatalf("store accelerator generation: %v", err)
	}
	durable := newCapabilityState()
	durable.Defaults[api.CapabilityAIInferenceOpenAIV1] = appInst.InstanceID
	durable.AcceleratorGrant = &acceleratorGrantRecord{
		Owner:   appInst.InstanceID,
		UIDs:    []uint32{1000},
		Devices: append([]string(nil), appInst.AcceleratorDevices...),
	}
	if err := state.storeCapabilityState(durable); err != nil {
		t.Fatalf("store accelerator grant: %v", err)
	}
}

func TestDesiredStoppedReconcileRetainsSelectedAcceleratorPermission(t *testing.T) {
	for _, recordedAnchor := range []bool{true, false} {
		name := "recorded-anchor"
		if !recordedAnchor {
			name = "missing-anchor"
		}
		t.Run(name, func(t *testing.T) {
			mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
			mgr.serviceManager.UseInMemoryNetworkForTest()
			appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
			if !recordedAnchor {
				appInst.NetworkAnchorID = ""
			}
			mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
			if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
				t.Fatalf("publish route: %v", err)
			}
			seedDesiredStoppedAcceleratorGrant(t, state, appInst)

			stopErr := errors.New("injected graceful stop failure")
			mock.stopError = stopErr
			quiesceCalls := 0
			mgr.userSessionQuiescer = func(_ context.Context, instanceID string) error {
				quiesceCalls++
				if instanceID != appInst.InstanceID {
					t.Fatalf("quiesced instance = %q, want %q", instanceID, appInst.InstanceID)
				}
				if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
					t.Fatalf("status before process-absence proof = %q, want %q", got, StatusRunning)
				}
				if !mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
					t.Fatal("follower publication withdrawn before process-absence proof")
				}
				durable, err := state.loadCapabilityState()
				if err != nil {
					t.Fatalf("load grant before process-absence proof: %v", err)
				}
				if durable.AcceleratorGrant == nil {
					t.Fatal("accelerator fence withdrawn before process-absence proof")
				}
				return nil
			}
			revokeCalls := 0
			mgr.acceleratorPermission = func(_ context.Context, _ uint32, _ []string, grant bool) error {
				revokeCalls++
				t.Fatalf("desired-stopped reconcile changed selected-app accelerator permission (grant=%v)", grant)
				return nil
			}

			if err := mgr.reconcileContainerGroup(
				context.Background(),
				state,
				appInst,
				def,
				layout,
				runtime,
				false,
			); err != nil {
				t.Fatalf("desired-stopped reconcile: %v", err)
			}
			if quiesceCalls != 1 {
				t.Fatalf("PID 1 quiesce calls = %d, want 1", quiesceCalls)
			}
			if revokeCalls != 0 {
				t.Fatalf("accelerator permission calls = %d, want zero", revokeCalls)
			}
			if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusStopped {
				t.Fatalf("observed status = %q, want %q", got, StatusStopped)
			}
			durable, err := state.loadCapabilityState()
			if err != nil {
				t.Fatalf("load accelerator state: %v", err)
			}
			if durable.AcceleratorGrant == nil || durable.AcceleratorGrant.Owner != appInst.InstanceID {
				t.Fatalf("selected-app accelerator grant was not retained: %+v", durable.AcceleratorGrant)
			}
		})
	}
}

func TestDesiredStoppedReconcileDoubleFailureRetainsAcceleratorFence(t *testing.T) {
	for _, recordedAnchor := range []bool{true, false} {
		name := "recorded-anchor"
		if !recordedAnchor {
			name = "missing-anchor"
		}
		t.Run(name, func(t *testing.T) {
			mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
			mgr.serviceManager.UseInMemoryNetworkForTest()
			appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
			appInst.Enabled = false
			if !recordedAnchor {
				appInst.NetworkAnchorID = ""
			}
			mgr.setObservedStatus(appInst.InstanceID, StatusRunning)
			if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
				t.Fatalf("publish route: %v", err)
			}
			seedDesiredStoppedAcceleratorGrant(t, state, appInst)

			stopErr := errors.New("injected graceful stop failure")
			proofErr := errors.New("injected PID 1 proof failure")
			mock.stopError = stopErr
			mgr.userSessionQuiescer = func(context.Context, string) error {
				if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
					t.Fatalf("status before failed process-absence proof = %q, want %q", got, StatusRunning)
				}
				if mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
					t.Fatal("manual-disable publication retained during failed process-absence proof")
				}
				return proofErr
			}
			revokeCalls := 0
			mgr.acceleratorPermission = func(context.Context, uint32, []string, bool) error {
				revokeCalls++
				return nil
			}

			err := mgr.reconcileContainerGroup(
				context.Background(),
				state,
				appInst,
				def,
				layout,
				runtime,
				false,
			)
			if !errors.Is(err, stopErr) || !errors.Is(err, proofErr) {
				t.Fatalf("desired-stopped error = %v, want stop and PID 1 proof failures", err)
			}
			if revokeCalls != 0 {
				t.Fatalf("accelerator ACL revoke calls = %d, want zero without absence proof", revokeCalls)
			}
			if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
				t.Fatalf("observed status = %q, want retained %q", got, StatusRunning)
			}
			if mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
				t.Fatal("manual-disable publication retained after failed process-absence proof")
			}
			durable, loadErr := state.loadCapabilityState()
			if loadErr != nil {
				t.Fatalf("load accelerator state: %v", loadErr)
			}
			if durable.AcceleratorGrant == nil || durable.AcceleratorGrant.Owner != appInst.InstanceID {
				t.Fatalf("accelerator fence dropped without process-absence proof: %+v", durable.AcceleratorGrant)
			}
		})
	}
}

func TestDesiredRunningCleanupAdmissionErrorFailsPublicationClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*MockContainerManager)
	}{
		{
			name: "stop",
			inject: func(mock *MockContainerManager) {
				mock.stopError = &pressure.AdmissionError{Class: pressure.WorkPodman}
			},
		},
		{
			name: "remove",
			inject: func(mock *MockContainerManager) {
				mock.removeError = &pressure.AdmissionError{Class: pressure.WorkPodman}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pressure.DefaultAdmission.ResetForTest()
			t.Cleanup(pressure.DefaultAdmission.ResetForTest)

			mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
			mgr.serviceManager.UseInMemoryNetworkForTest()
			claim := 32080
			def.Listeners[0].PortClaim = &claim
			appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
			if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
				t.Fatalf("publish route: %v", err)
			}
			beforeRegistry := mgr.serviceManager.SnapshotRegistry()
			beforeNextID := mock.nextID
			tc.inject(mock)

			_, err := mgr.cleanupDesiredRunningGroupForRecreate(context.Background(), state, appInst, def, runtime, "test")
			if !pressure.IsAdmissionError(err) {
				t.Fatalf("cleanup error = %v, want task-pressure admission error", err)
			}
			if mock.nextID != beforeNextID {
				t.Fatalf("cleanup recreated containers: next ID = %d, want %d", mock.nextID, beforeNextID)
			}
			for serviceName, id := range appInst.Containers {
				if _, ok := mock.containers[id]; !ok {
					t.Fatalf("service %s was removed after admission error", serviceName)
				}
			}
			if mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
				t.Fatal("publication remained active after cleanup could have partially stopped the group")
			}
			if got := mgr.serviceManager.SnapshotRegistry(); !reflect.DeepEqual(got, beforeRegistry) {
				t.Fatalf("suspended route registry changed: got %+v want %+v", got, beforeRegistry)
			}
			if got := mgr.serviceManager.ActivePortClaims(); len(got) != 0 {
				t.Fatalf("active port claims remained after uncertain teardown: %+v", got)
			}
		})
	}
}

func TestDesiredRunningCleanupRemovalFailureDoesNotProceedToRecreation(t *testing.T) {
	pressure.DefaultAdmission.ResetForTest()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)

	mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
	mgr.serviceManager.UseInMemoryNetworkForTest()
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	if _, err := mgr.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners); err != nil {
		t.Fatalf("publish route: %v", err)
	}
	mock.removeError = errors.New("remove unavailable")

	_, err := mgr.cleanupDesiredRunningGroupForRecreate(context.Background(), state, appInst, def, runtime, "test")
	if err == nil || !strings.Contains(err.Error(), "remove unavailable") {
		t.Fatalf("cleanup removal error = %v, want explicit retryable failure", err)
	}
	if mgr.serviceManager.AppPublicationActive(appInst.InstanceID) {
		t.Fatal("publication remained active after backends were stopped but removal failed")
	}
	if got := mgr.serviceManager.ActivePortClaims(); len(got) != 0 {
		t.Fatalf("active port claims remained after failed teardown: %+v", got)
	}
	if appInst.NetworkAnchorID == "" || len(appInst.Containers) == 0 {
		t.Fatalf("failed removal falsely committed absent runtime: anchor=%q containers=%v", appInst.NetworkAnchorID, appInst.Containers)
	}
}

func TestContainerStatusesRejectsPartialUnknownProjection(t *testing.T) {
	mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	mock.inspectErrorForContainer = map[string]error{appInst.Containers["side"]: fmt.Errorf("inspect unavailable")}

	if _, err := mgr.ContainerStatuses(context.Background(), appInst.InstanceID); err == nil {
		t.Fatal("ContainerStatuses projected a partial observation as stopped")
	}
}

func TestAutomaticStartupRecoveryUsesOneAttemptAcrossFailureAndSuccess(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	appInst := &AppInstance{
		InstanceID: "testapp",
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("first automatic recovery attempt was rejected")
	}
	if appInst.StartupAttempts != 1 {
		t.Fatalf("attempts after begin = %d, want 1", appInst.StartupAttempts)
	}
	// Nested recovery may quiesce the current group before recreate. That
	// interrupts probation but must retain ownership of this in-flight attempt.
	mgr.interruptStartupProbation(appInst.InstanceID)
	if !mgr.startupRecovery[appInst.InstanceID].AttemptActive {
		t.Fatal("quiescence erased the in-flight recovery attempt")
	}
	mgr.handleStartupFailure(state, appInst)
	if appInst.StartupAttempts != 1 {
		t.Fatalf("failed in-flight attempt counted twice: %d", appInst.StartupAttempts)
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("second automatic recovery attempt was rejected")
	}
	mgr.markStartupRecoverySucceeded(state, appInst)
	if appInst.StartupAttempts != 2 {
		t.Fatalf("successful recovery erased history: attempts=%d", appInst.StartupAttempts)
	}
	window := mgr.startupRecovery[appInst.InstanceID]
	if window.ProbationSince.IsZero() || window.AttemptActive {
		t.Fatalf("success window = %+v, want inactive probation", window)
	}
	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("recovery after probation loss was rejected")
	}
	window = mgr.startupRecovery[appInst.InstanceID]
	if appInst.StartupAttempts != 3 || !window.ProbationSince.IsZero() || !window.AttemptActive {
		t.Fatalf("probation loss state = attempts %d window %+v", appInst.StartupAttempts, window)
	}
	mgr.markStartupRecoverySucceeded(state, appInst)
	window = mgr.startupRecovery[appInst.InstanceID]

	window.ProbationSince = time.Now().Add(-startupEscalateAfterDuration - time.Second)
	mgr.startupRecovery[appInst.InstanceID] = window
	mgr.markStartupRecoverySucceeded(state, appInst)
	if appInst.StartupAttempts != 0 || appInst.FirstStartupFailureAt != nil {
		t.Fatalf("history after continuous probation = attempts %d first %v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if _, ok := mgr.startupRecovery[appInst.InstanceID]; ok {
		t.Fatal("probation entry retained after stability window")
	}
}

func TestAutomaticStartupRecoveryAllowsFifthAttemptThenGuardsSixth(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	appInst := &AppInstance{
		InstanceID:            "testapp",
		Enabled:               true,
		CreatedAt:             now,
		UpdatedAt:             now,
		Definition:            def,
		StartupAttempts:       startupEscalateAfterAttempts - 1,
		FirstStartupFailureAt: &now,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("fifth automatic recovery attempt should run")
	}
	if appInst.StartupAttempts != startupEscalateAfterAttempts {
		t.Fatalf("attempts = %d, want %d", appInst.StartupAttempts, startupEscalateAfterAttempts)
	}
	mgr.handleStartupFailure(state, appInst)
	if mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("sixth automatic recovery attempt bypassed escalation guard")
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusError {
		t.Fatalf("observed status = %q, want %q", got, StatusError)
	}
}

func TestReconcileKnownMissingWorkloadProjectsErrorAfterRecoveryExhausted(t *testing.T) {
	mgr, mock, state, def, _, runtime := newReconcileTestEnv(t)
	appInst := createRunningReconcileGroup(t, mock, state, def, runtime)
	mgr.setObservedStatus(appInst.InstanceID, StatusRunning)

	firstFailure := time.Now().Add(-time.Minute)
	appInst.StartupAttempts = startupEscalateAfterAttempts
	appInst.FirstStartupFailureAt = &firstFailure
	if err := state.StoreAppMetadata(appInst); err != nil {
		t.Fatalf("store exhausted startup history: %v", err)
	}

	// Model the alpha failure: the API still projects Running after the service
	// process/container has authoritatively disappeared and automatic recovery
	// has no remaining attempt.
	delete(mock.containers, appInst.Containers["main"])
	beforeNextID := mock.nextID

	if err := mgr.reconcileApp(context.Background(), state, appInst); err != nil {
		t.Fatalf("reconcile known missing workload: %v", err)
	}
	if status, message := mgr.getObservedStatusAndMessage(appInst.InstanceID); status != StatusError || message != msgStartupFailed {
		t.Fatalf("observed projection = (%q, %q), want (%q, %q)", status, message, StatusError, msgStartupFailed)
	}
	if appInst.StartupAttempts != startupEscalateAfterAttempts {
		t.Fatalf("startup attempts = %d, want exhausted %d", appInst.StartupAttempts, startupEscalateAfterAttempts)
	}
	if mock.nextID != beforeNextID {
		t.Fatalf("exhausted recovery created another container: next ID = %d, want %d", mock.nextID, beforeNextID)
	}
}

func TestExhaustedAutomaticGuardBreaksOldProbationBeforeManualSuccess(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	firstFailure := now.Add(-time.Minute)
	oldProbation := now.Add(-startupEscalateAfterDuration - time.Minute)
	appInst := &AppInstance{
		InstanceID:            "testapp",
		Enabled:               true,
		CreatedAt:             now,
		UpdatedAt:             now,
		Definition:            def,
		StartupAttempts:       startupEscalateAfterAttempts,
		FirstStartupFailureAt: &firstFailure,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	mgr.startupRecovery[appInst.InstanceID] = startupRecoveryWindow{ProbationSince: oldProbation}

	if mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("exhausted automatic recovery guard allowed another attempt")
	}
	window := mgr.startupRecovery[appInst.InstanceID]
	if !window.ProbationSince.IsZero() {
		t.Fatalf("guard rejection retained pre-loss probation: %v", window.ProbationSince)
	}

	// Manual Start bypasses the automatic guard. Its later success must begin a
	// fresh probation rather than applying the healthy time from before loss.
	mgr.interruptStartupProbation(appInst.InstanceID)
	mgr.markStartupRecoverySucceeded(state, appInst)
	window = mgr.startupRecovery[appInst.InstanceID]
	if appInst.StartupAttempts != startupEscalateAfterAttempts || appInst.FirstStartupFailureAt == nil {
		t.Fatalf("manual success cleared exhausted history: attempts=%d first=%v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if window.ProbationSince.IsZero() || window.ProbationSince.Before(now) {
		t.Fatalf("manual success did not start fresh probation: %v", window.ProbationSince)
	}
}

func TestDisabledReconcileCompletionClearsStartupHistory(t *testing.T) {
	mgr, _, state, def, _, _ := newReconcileTestEnv(t)
	now := time.Now()
	appInst := &AppInstance{
		InstanceID:            "testapp",
		Enabled:               false,
		CreatedAt:             now,
		UpdatedAt:             now,
		Definition:            def,
		StartupAttempts:       startupEscalateAfterAttempts,
		FirstStartupFailureAt: &now,
		Containers:            map[string]string{},
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}
	mgr.startupRecovery[appInst.InstanceID] = startupRecoveryWindow{ProbationSince: now}
	mgr.beginObservationPass()
	mgr.recordUnknownObservation(appInst.InstanceID, errors.New("podman unavailable"))

	if err := mgr.reconcileApp(context.Background(), state, appInst); err != nil {
		t.Fatalf("disabled reconcile: %v", err)
	}
	if appInst.StartupAttempts != 0 || appInst.FirstStartupFailureAt != nil {
		t.Fatalf("disabled stop completion retained history: attempts=%d first=%v", appInst.StartupAttempts, appInst.FirstStartupFailureAt)
	}
	if _, ok := mgr.startupRecovery[appInst.InstanceID]; ok {
		t.Fatal("disabled stop completion retained recovery window")
	}
	if snapshot := mgr.RuntimeObservationPressureSnapshot(); len(snapshot) != 0 {
		t.Fatalf("disabled stop completion retained observation pressure: %+v", snapshot)
	}
}

func TestReconcile_AnchorStartFails_RecreatesAndRecovers(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Create anchor container (stopped — simulating post-reboot state)
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	// Leave anchor stopped (default state after create)

	// Create service container (stopped)
	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers:      map[string]string{"main": mainCID},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Make anchor start fail (simulating stale runtime state)
	mock.startErrorForContainer = map[string]error{
		anchorCID: fmt.Errorf("podman start failed: namespace path: no such file or directory"),
	}

	// Reconcile should trigger full recreation
	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}

	// Old containers should be removed
	oldAnchorState, _ := mock.InspectContainerState(ctx, runtime, anchorCID)
	if oldAnchorState.Exists {
		t.Error("old anchor should have been removed")
	}
	oldMainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if oldMainState.Exists {
		t.Error("old main container should have been removed")
	}

	// App should recover to running with new containers
	observed := mgr.getObservedStatus("testapp")
	if observed != StatusRunning {
		t.Errorf("expected status %q, got %q", StatusRunning, observed)
	}

	// Startup tracking should be reset (recovery succeeded)
	if appInst.StartupAttempts != 0 {
		t.Errorf("expected StartupAttempts=0 after recovery, got %d", appInst.StartupAttempts)
	}
}

func TestReconcile_AnchorStartAndStopFail_QuiescesSessionAndRecovers(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers:      map[string]string{"main": mainCID},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	mock.startErrorForContainer = map[string]error{
		anchorCID: fmt.Errorf("podman start failed: conmon process killed"),
	}
	mock.stopError = fmt.Errorf("podman stop failed: conmon process killed")
	quiesceCalls := 0
	mgr.userSessionQuiescer = func(_ context.Context, instanceID string) error {
		quiesceCalls++
		if instanceID != appInst.InstanceID {
			t.Fatalf("quiesced instance = %q, want %q", instanceID, appInst.InstanceID)
		}
		return nil
	}

	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}
	if quiesceCalls != 1 {
		t.Fatalf("quiesce calls = %d, want 1", quiesceCalls)
	}
	if oldAnchorState, _ := mock.InspectContainerState(ctx, runtime, anchorCID); oldAnchorState.Exists {
		t.Fatal("old anchor should have been removed after PID 1 quiescence")
	}
	if oldMainState, _ := mock.InspectContainerState(ctx, runtime, mainCID); oldMainState.Exists {
		t.Fatal("old service should have been removed after PID 1 quiescence")
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
		t.Fatalf("observed status = %q, want %q", got, StatusRunning)
	}
}

func TestReconcile_StaleAnchorQuiescesSessionWithoutNoOpStart(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}
	mock.containers[anchorCID].Status = "stale"
	mock.containers[mainCID].Status = "stale"

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusRunning,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers:      map[string]string{"main": mainCID},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	mock.stopError = fmt.Errorf("podman stop succeeded without changing stale metadata")
	quiesceCalls := 0
	mgr.userSessionQuiescer = func(context.Context, string) error {
		quiesceCalls++
		return nil
	}

	if !mgr.beginAutomaticStartupAttempt(state, appInst) {
		t.Fatal("startup recovery attempt was rejected")
	}
	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}
	mgr.markStartupRecoverySucceeded(state, appInst)

	if quiesceCalls != 1 {
		t.Fatalf("quiesce calls = %d, want 1", quiesceCalls)
	}
	if appInst.StartupAttempts != 1 {
		t.Fatalf("startup attempts = %d, want one owned attempt", appInst.StartupAttempts)
	}
	if appInst.NetworkAnchorID == anchorCID || appInst.Containers["main"] == mainCID {
		t.Fatalf("stale container IDs survived recovery: anchor=%s main=%s", appInst.NetworkAnchorID, appInst.Containers["main"])
	}
	if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
		t.Fatalf("observed status = %q, want %q", got, StatusRunning)
	}
}

func TestStartContainerGroupRecoversStaleContainerState(t *testing.T) {
	for _, staleRole := range []string{"anchor", "service"} {
		t.Run(staleRole, func(t *testing.T) {
			mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
			ctx := context.Background()
			anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
				Name:   "testapp__netns__",
				Image:  "alpine:latest",
				Labels: piccoloLabels("testapp", "", "anchor"),
			})
			if err != nil {
				t.Fatalf("create anchor: %v", err)
			}
			mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
				Name:   "testapp",
				Image:  "alpine:latest",
				Labels: piccoloLabels("testapp", "main", "service"),
			})
			if err != nil {
				t.Fatalf("create main: %v", err)
			}
			if staleRole == "anchor" {
				mock.containers[anchorCID].Status = "stale"
			} else {
				mock.containers[mainCID].Status = "stale"
			}

			now := time.Now()
			appInst := &AppInstance{
				InstanceID:      "testapp",
				Status:          StatusStopped,
				Enabled:         true,
				PrimaryService:  "main",
				NetworkAnchorID: anchorCID,
				Containers:      map[string]string{"main": mainCID},
				CreatedAt:       now,
				UpdatedAt:       now,
				Definition:      def,
			}
			if err := state.StoreApp(appInst); err != nil {
				t.Fatalf("StoreApp: %v", err)
			}

			if err := mgr.startContainerGroup(ctx, state, appInst, def, layout, runtime); err != nil {
				t.Fatalf("startContainerGroup: %v", err)
			}
			if old, _ := mock.InspectContainerState(ctx, runtime, anchorCID); old.Exists {
				t.Fatal("manual stale recovery retained old anchor")
			}
			if old, _ := mock.InspectContainerState(ctx, runtime, mainCID); old.Exists {
				t.Fatal("manual stale recovery retained old service")
			}
			if appInst.NetworkAnchorID == anchorCID || appInst.Containers["main"] == mainCID {
				t.Fatalf("manual stale recovery retained old IDs: anchor=%s service=%s", appInst.NetworkAnchorID, appInst.Containers["main"])
			}
			if got := mgr.getObservedStatus(appInst.InstanceID); got != StatusRunning {
				t.Fatalf("observed status = %q, want %q", got, StatusRunning)
			}
		})
	}
}

func TestReconcile_AnchorStartFails_RecreationFails_Escalates(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Create anchor container (stopped)
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}

	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers:      map[string]string{"main": mainCID},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Make ALL container starts fail (anchor start fails, and recreation also fails)
	mock.startError = fmt.Errorf("simulated runtime failure")

	// Reconcile should fail but increment escalation counter
	err = mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true)
	if err == nil {
		t.Fatal("expected reconcile to return error when recreation fails")
	}

	// Startup failure should be recorded
	if appInst.StartupAttempts < 1 {
		t.Errorf("expected StartupAttempts >= 1, got %d", appInst.StartupAttempts)
	}
}

func TestReconcile_ServiceStartFails_RecreatesService(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Create and start anchor (running)
	anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__netns__",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "", "anchor"),
	})
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, anchorCID); err != nil {
		t.Fatalf("start anchor: %v", err)
	}

	// Create main service container (stopped — simulating post-reboot)
	mainCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "main", "service"),
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}

	// Create side service container (stopped)
	sideCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "testapp__side",
		Image:  "alpine:latest",
		Labels: piccoloLabels("testapp", "side", "service"),
	})
	if err != nil {
		t.Fatalf("create side: %v", err)
	}

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStopped,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: anchorCID,
		Containers: map[string]string{
			"main": mainCID,
			"side": sideCID,
		},
		CreatedAt:  now,
		UpdatedAt:  now,
		Definition: def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Make only the main service container fail to start
	mock.startErrorForContainer = map[string]error{
		mainCID: fmt.Errorf("podman start failed: cgroup deleted"),
	}

	// Reconcile should recreate just the failed service
	if err := mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true); err != nil {
		t.Fatalf("reconcileContainerGroup: %v", err)
	}

	// Old main container should be removed
	oldMainState, _ := mock.InspectContainerState(ctx, runtime, mainCID)
	if oldMainState.Exists {
		t.Error("old main container should have been removed")
	}

	// A new container should have been created for "main"
	newMainCID := appInst.Containers["main"]
	if newMainCID == "" || newMainCID == mainCID {
		t.Error("expected a new container ID for 'main' service")
	}

	// The new container should be running
	newMainState, _ := mock.InspectContainerState(ctx, runtime, newMainCID)
	if !newMainState.Running {
		t.Error("new main container should be running")
	}

	// App should be running
	observed := mgr.getObservedStatus("testapp")
	if observed != StatusRunning {
		t.Errorf("expected status %q, got %q", StatusRunning, observed)
	}
}

func TestReconcile_EscalationAfterRepeatedFailures(t *testing.T) {
	mgr, mock, state, def, layout, runtime := newReconcileTestEnv(t)
	ctx := context.Background()

	// Make ALL container starts fail permanently
	mock.startError = fmt.Errorf("simulated permanent failure")

	now := time.Now()
	appInst := &AppInstance{
		InstanceID:      "testapp",
		Status:          StatusStarting,
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "", // no anchor — forces recreation path
		Containers:      map[string]string{},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("StoreApp: %v", err)
	}

	// Run reconcile repeatedly until escalation
	for i := 0; i < startupEscalateAfterAttempts+1; i++ {
		_ = mgr.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true)
	}

	// After enough failures, status should escalate to error
	observed := mgr.getObservedStatus("testapp")
	if observed != StatusError {
		t.Errorf("expected status %q after %d failures, got %q",
			StatusError, startupEscalateAfterAttempts, observed)
	}
}
