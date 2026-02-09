package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func TestFlushAndReloadNetavarkRules(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")

	t.Run("reloads_running_anchors", func(t *testing.T) {
		tempDir := t.TempDir()

		mock := NewMockContainerManager()
		mgr, err := NewAppManager(mock, tempDir)
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

		layout1, err := mgr.ensureAppVolumeLayout(ctx, "app1")
		if err != nil {
			t.Fatalf("ensureAppVolumeLayout app1: %v", err)
		}
		runtime1, err := mgr.podmanRuntimeForApp("app1", layout1, ModeService)
		if err != nil {
			t.Fatalf("podmanRuntimeForApp app1: %v", err)
		}

		// Create anchor containers to simulate running state.
		anchor1CID, err := mock.CreateContainer(ctx, runtime1, container.ContainerCreateSpec{
			Name:   networkAnchorContainerName("app1"),
			Image:  "alpine:latest",
			Labels: piccoloLabels("app1", "anchor", "anchor"),
		})
		if err != nil {
			t.Fatalf("create anchor1: %v", err)
		}
		_ = mock.StartContainer(ctx, runtime1, anchor1CID)

		layout2, err := mgr.ensureAppVolumeLayout(ctx, "app2")
		if err != nil {
			t.Fatalf("ensureAppVolumeLayout app2: %v", err)
		}
		runtime2, err := mgr.podmanRuntimeForApp("app2", layout2, ModeService)
		if err != nil {
			t.Fatalf("podmanRuntimeForApp app2: %v", err)
		}

		anchor2CID, err := mock.CreateContainer(ctx, runtime2, container.ContainerCreateSpec{
			Name:   networkAnchorContainerName("app2"),
			Image:  "alpine:latest",
			Labels: piccoloLabels("app2", "anchor", "anchor"),
		})
		if err != nil {
			t.Fatalf("create anchor2: %v", err)
		}
		_ = mock.StartContainer(ctx, runtime2, anchor2CID)

		now := time.Now()
		def := &api.AppDefinition{
			Listeners: []api.AppListener{
				{Name: "app1", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
			},
			PrimaryService: "main",
			Services: map[string]api.AppService{
				"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			},
			Extensions: map[string]interface{}{"mode": "service"},
		}
		SetDefaults(def)
		def2 := &api.AppDefinition{
			Listeners: []api.AppListener{
				{Name: "app2", GuestPort: 9090, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
			},
			PrimaryService: "main",
			Services: map[string]api.AppService{
				"main": {Image: "alpine:latest", BindPorts: []int{9090}},
			},
			Extensions: map[string]interface{}{"mode": "service"},
		}
		SetDefaults(def2)

		if err := state.StoreApp(&AppInstance{
			InstanceID:      "app1",
			Enabled:         true,
			PrimaryService:  "main",
			NetworkAnchorID: anchor1CID,
			Containers:      map[string]string{"main": anchor1CID},
			CreatedAt:       now,
			UpdatedAt:       now,
			Definition:      def,
		}); err != nil {
			t.Fatalf("StoreApp app1: %v", err)
		}
		if err := state.StoreApp(&AppInstance{
			InstanceID:      "app2",
			Enabled:         true,
			PrimaryService:  "main",
			NetworkAnchorID: anchor2CID,
			Containers:      map[string]string{"main": anchor2CID},
			CreatedAt:       now,
			UpdatedAt:       now,
			Definition:      def2,
		}); err != nil {
			t.Fatalf("StoreApp app2: %v", err)
		}

		mgr.flushAndReloadNetavarkRules(ctx)

		if len(mock.reloadedContainers) != 2 {
			t.Fatalf("expected 2 network reloads, got %d: %v", len(mock.reloadedContainers), mock.reloadedContainers)
		}
		// Verify both anchor names appear in reloaded list.
		reloaded := make(map[string]bool)
		for _, name := range mock.reloadedContainers {
			reloaded[name] = true
		}
		if !reloaded[networkAnchorContainerName("app1")] {
			t.Errorf("expected reload for app1 anchor")
		}
		if !reloaded[networkAnchorContainerName("app2")] {
			t.Errorf("expected reload for app2 anchor")
		}
	})

	t.Run("skips_disabled_apps", func(t *testing.T) {
		tempDir := t.TempDir()

		mock := NewMockContainerManager()
		mgr, err := NewAppManager(mock, tempDir)
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

		now := time.Now()
		def := &api.AppDefinition{
			Listeners: []api.AppListener{
				{Name: "disabled-app", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
			},
			PrimaryService: "main",
			Services: map[string]api.AppService{
				"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			},
			Extensions: map[string]interface{}{"mode": "service"},
		}
		SetDefaults(def)

		if err := state.StoreApp(&AppInstance{
			InstanceID:      "disabled-app",
			Enabled:         false, // Disabled
			PrimaryService:  "main",
			NetworkAnchorID: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe01",
			Containers:      map[string]string{"main": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe01"},
			CreatedAt:       now,
			UpdatedAt:       now,
			Definition:      def,
		}); err != nil {
			t.Fatalf("StoreApp: %v", err)
		}

		mgr.flushAndReloadNetavarkRules(ctx)

		if len(mock.reloadedContainers) != 0 {
			t.Fatalf("expected 0 network reloads for disabled app, got %d: %v", len(mock.reloadedContainers), mock.reloadedContainers)
		}
	})

	t.Run("reload_failure_nonfatal", func(t *testing.T) {
		tempDir := t.TempDir()

		mock := NewMockContainerManager()
		mock.reloadErr = fmt.Errorf("simulated reload failure")
		mgr, err := NewAppManager(mock, tempDir)
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

		layout, err := mgr.ensureAppVolumeLayout(ctx, "fail-app")
		if err != nil {
			t.Fatalf("ensureAppVolumeLayout: %v", err)
		}
		runtime, err := mgr.podmanRuntimeForApp("fail-app", layout, ModeService)
		if err != nil {
			t.Fatalf("podmanRuntimeForApp: %v", err)
		}

		anchorCID, err := mock.CreateContainer(ctx, runtime, container.ContainerCreateSpec{
			Name:   networkAnchorContainerName("fail-app"),
			Image:  "alpine:latest",
			Labels: piccoloLabels("fail-app", "anchor", "anchor"),
		})
		if err != nil {
			t.Fatalf("create anchor: %v", err)
		}
		_ = mock.StartContainer(ctx, runtime, anchorCID)

		now := time.Now()
		def := &api.AppDefinition{
			Listeners: []api.AppListener{
				{Name: "fail-app", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
			},
			PrimaryService: "main",
			Services: map[string]api.AppService{
				"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			},
			Extensions: map[string]interface{}{"mode": "service"},
		}
		SetDefaults(def)

		if err := state.StoreApp(&AppInstance{
			InstanceID:      "fail-app",
			Enabled:         true,
			PrimaryService:  "main",
			NetworkAnchorID: anchorCID,
			Containers:      map[string]string{"main": anchorCID},
			CreatedAt:       now,
			UpdatedAt:       now,
			Definition:      def,
		}); err != nil {
			t.Fatalf("StoreApp: %v", err)
		}

		// This should not panic despite reload errors.
		mgr.flushAndReloadNetavarkRules(ctx)

		// Reload was attempted even though it failed.
		if len(mock.reloadedContainers) != 0 {
			t.Fatalf("expected 0 successful reloads (mock returns error), got %d", len(mock.reloadedContainers))
		}
	})

	t.Run("skips_apps_without_anchor", func(t *testing.T) {
		tempDir := t.TempDir()

		mock := NewMockContainerManager()
		mgr, err := NewAppManager(mock, tempDir)
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

		now := time.Now()
		def := &api.AppDefinition{
			Listeners: []api.AppListener{
				{Name: "no-anchor", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
			},
			PrimaryService: "main",
			Services: map[string]api.AppService{
				"main": {Image: "alpine:latest", BindPorts: []int{8080}},
			},
			Extensions: map[string]interface{}{"mode": "service"},
		}
		SetDefaults(def)

		if err := state.StoreApp(&AppInstance{
			InstanceID:      "no-anchor",
			Enabled:         true,
			PrimaryService:  "main",
			NetworkAnchorID: "", // No anchor yet
			Containers:      nil,
			CreatedAt:       now,
			UpdatedAt:       now,
			Definition:      def,
		}); err != nil {
			t.Fatalf("StoreApp: %v", err)
		}

		mgr.flushAndReloadNetavarkRules(ctx)

		if len(mock.reloadedContainers) != 0 {
			t.Fatalf("expected 0 network reloads for app without anchor, got %d: %v", len(mock.reloadedContainers), mock.reloadedContainers)
		}
	})
}

