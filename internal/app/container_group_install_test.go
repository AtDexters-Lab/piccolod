package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/services"
)

func TestInstallMultiContainer_UnprovenCleanupRetainsOwnershipUntilRetry(t *testing.T) {
	tempDir := t.TempDir()
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	rootfs := newStubRootfsManager(t.TempDir())
	mgr.SetRootfsManager(rootfs)

	def := &api.AppDefinition{
		Listeners: []api.AppListener{{
			Name: "demo", GuestPort: 8080, Flow: api.FlowTCP,
			Protocol: api.ListenerProtocolHTTP, Primary: true,
		}},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	ctx := context.Background()
	layout, err := mgr.ensureAppVolumeLayout(ctx, "demo")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp(ctx, "demo", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}
	endpoints, err := mgr.serviceManager.AllocateForApp("demo", def.Listeners)
	if err != nil {
		t.Fatalf("allocate publication: %v", err)
	}
	mgr.serviceManager.SetAppContainerID("demo", "candidate")

	startErr := errors.New("injected candidate start failure")
	removeErr := errors.New("injected candidate remove failure")
	quiesceErr := errors.New("injected candidate quiesce failure")
	mock.startError = startErr
	mock.removeError = removeErr
	mgr.userSessionQuiescer = func(context.Context, string) error { return quiesceErr }
	_, err = mgr.installContainerGroup(
		ctx,
		def,
		"demo",
		layout,
		runtime,
		endpoints,
		prebuiltReconcileRootfs(t, def),
		false,
		true,
	)
	if !uncommittedContainerGroupMaySurvive(err) {
		t.Fatalf("install error = %v, want uncommitted survivor classification", err)
	}
	for _, want := range []string{startErr.Error(), removeErr.Error(), quiesceErr.Error(), "candidate containers remain"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("install error = %v, want %q", err, want)
		}
	}
	if len(mock.containers) == 0 {
		t.Fatal("removal failure unexpectedly removed the candidate")
	}
	if mgr.serviceManager.AppPublicationActive("demo") {
		t.Fatal("uncommitted candidate remained reachable after cleanup failure")
	}
	if len(rootfs.detached) != 0 {
		t.Fatalf("unproven cleanup detached rootfs ownership: %v", rootfs.detached)
	}

	mock.startError = nil
	mock.removeError = nil
	mgr.userSessionQuiescer = func(context.Context, string) error { return nil }
	if err := mgr.removeUncommittedContainerGroup(
		ctx,
		&AppInstance{InstanceID: "demo", PrimaryService: "main"},
		def,
		runtime,
	); err != nil {
		t.Fatalf("retry candidate removal: %v", err)
	}
	if len(mock.containers) != 0 {
		t.Fatalf("candidate containers survived retry: %+v", mock.containers)
	}
	if _, err := mgr.installContainerGroup(
		ctx,
		def,
		"demo",
		layout,
		runtime,
		endpoints,
		prebuiltReconcileRootfs(t, def),
		false,
		true,
	); err != nil {
		t.Fatalf("install after candidate removal: %v", err)
	}
}

func TestInstallMultiContainer_PrunesZombiesBeforeCreate(t *testing.T) {
	tempDir := t.TempDir()

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatalf("NewAppManager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)

	ctx := context.Background()
	layout, err := mgr.ensureAppVolumeLayout(ctx, "demo")
	if err != nil {
		t.Fatalf("ensureAppVolumeLayout: %v", err)
	}
	runtime, err := mgr.podmanRuntimeForApp(context.Background(), "demo", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatalf("podmanRuntimeForApp: %v", err)
	}

	// Simulate a prior partial install leaving a labeled container that is not part of the expected set.
	zombieID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
		Name:   "demo__zombie",
		Image:  "alpine:latest",
		Labels: piccoloLabels("demo", "zombie", "service"),
	})
	if err != nil {
		t.Fatalf("create zombie container: %v", err)
	}
	if err := mock.StartContainer(ctx, runtime, zombieID); err != nil {
		t.Fatalf("start zombie container: %v", err)
	}

	// RFC 20260130: listener name is the app identity, set Primary=true for test
	def := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			"db":   {Image: "alpine:latest", BindPorts: []int{5432}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	SetDefaults(def)

	_, err = mgr.installContainerGroup(ctx, def, "demo", layout, runtime, []services.ServiceEndpoint{
		{App: "demo", Name: "demo", GuestPort: 8080, HostBind: 18080, PublicPort: 28080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}, nil, false, true)
	if err != nil {
		t.Fatalf("installContainerGroup: %v", err)
	}

	for _, c := range mock.containers {
		if c != nil && c.Spec.Name == "demo__zombie" {
			t.Fatalf("expected zombie container to be pruned before install")
		}
	}
}
