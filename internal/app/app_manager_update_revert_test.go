package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"piccolod/internal/api"
)

func TestAppManager_UpdateImage_And_Revert(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	tmp, err := os.MkdirTemp("", "fs_update_revert")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManager(mock, tmp)
	if err != nil {
		t.Fatalf("fs manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	ctx := context.Background()

	// Install initial app - RFC 20260130: listener name is the app identity
	def := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "demoapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "alpine:3.18", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	instanceID := inst.InstanceID

	// UpdateImage for single-service service-mode apps requires a rootfs manager.
	// Without one, it returns an error about rootfs not being configured.
	tag := "3.19"
	err = mgr.UpdateImage(ctx, instanceID, &tag)
	if err == nil {
		t.Fatalf("expected update image to fail without rootfs manager")
	}
	if !strings.Contains(err.Error(), "rootfs volume manager not configured") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Revert is also not supported for service-mode apps.
	if err := mgr.Revert(ctx, instanceID); err == nil {
		t.Fatalf("expected revert to fail for service-mode apps")
	}
}

func TestAppManager_Logs(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	tmp, err := os.MkdirTemp("", "fs_logs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	mock := NewMockContainerManager()
	mgr, err := NewAppManager(mock, tmp)
	if err != nil {
		t.Fatalf("fs manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	ctx := context.Background()

	// RFC 20260130: listener name is the app identity
	def := &api.AppDefinition{
		Type:      "user",
		Listeners: []api.AppListener{{Name: "demo", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if inst.PrimaryContainerID() == "" {
		t.Fatalf("no container id")
	}
	lines, err := mgr.Logs(ctx, inst.InstanceID, 5)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
}
