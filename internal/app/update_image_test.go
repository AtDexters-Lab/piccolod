package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"piccolod/internal/api"
)

func TestIsDigestPinned(t *testing.T) {
	tests := []struct {
		img  string
		want bool
	}{
		{"alpine:3.18", false},
		{"alpine", false},
		{"docker.io/library/nginx:1.25", false},
		{"ghcr.io/my-org/my-app:v1.0", false},
		{"localhost:5000/myapp:v1", false},
		{"nginx@sha256:abc123def456", true},
		{"nginx:1.25@sha256:abc123def456", true},
		{"docker.io/library/nginx@sha256:abc123", true},
	}
	for _, tt := range tests {
		t.Run(tt.img, func(t *testing.T) {
			if got := isDigestPinned(tt.img); got != tt.want {
				t.Errorf("isDigestPinned(%q) = %v, want %v", tt.img, got, tt.want)
			}
		})
	}
}

func TestUpdateImage_WorkspaceMode_Blocked(t *testing.T) {
	tmp, err := os.MkdirTemp("", "update_ws_blocked")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{{Name: "wsapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "ubuntu:22.04", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{
			"mode":           "workspace",
			"workspace_name": "wsapp",
		},
	}
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	err = mgr.UpdateImage(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("expected error for workspace-mode update")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInstall_MultiServiceMode_RequiresRootfs(t *testing.T) {
	tmp, err := os.MkdirTemp("", "install_multi_rootfs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTest(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
	// Clear rootfs manager to test the "not configured" error path.
	mgr.SetRootfsManager(nil)
	mgr.ForceLockState(false)
	ctx := context.Background()

	def := &api.AppDefinition{
		Type:           "user",
		PrimaryService: "web",
		Listeners:      []api.AppListener{{Name: "multiapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true}},
		Services: map[string]api.AppService{
			"web":    {Image: "nginx:1.25", BindPorts: []int{80}},
			"worker": {Image: "python:3.12", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	// Install requires rootfs volume manager (block-native architecture).
	_, err = mgr.Install(ctx, def)
	if err == nil {
		t.Fatal("expected error: rootfs manager not configured")
	}
	if !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
}
