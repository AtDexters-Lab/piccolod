package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"piccolod/internal/api"
)

func TestReplaceImageTag(t *testing.T) {
	tests := []struct {
		name   string
		img    string
		tag    string
		want   string
	}{
		{"simple_with_tag", "alpine:3.18", "3.19", "alpine:3.19"},
		{"no_tag", "alpine", "3.19", "alpine:3.19"},
		{"registry_with_tag", "docker.io/library/nginx:1.25", "1.26", "docker.io/library/nginx:1.26"},
		{"registry_no_tag", "docker.io/library/nginx", "latest", "docker.io/library/nginx:latest"},
		{"custom_registry", "ghcr.io/my-org/my-app:v1.0", "v2.0", "ghcr.io/my-org/my-app:v2.0"},
		{"digest_pinned", "nginx@sha256:abc123def456", "1.26", "nginx:1.26"},
		{"digest_with_tag", "nginx:1.25@sha256:abc123def456", "1.26", "nginx:1.26"},
		{"registry_digest", "docker.io/library/nginx@sha256:abc123", "1.26", "docker.io/library/nginx:1.26"},
		{"registry_with_port", "localhost:5000/myapp:v1", "v2", "localhost:5000/myapp:v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceImageTag(tt.img, tt.tag)
			if got != tt.want {
				t.Errorf("replaceImageTag(%q, %q) = %q, want %q", tt.img, tt.tag, got, tt.want)
			}
		})
	}
}

func TestUpdateImage_WorkspaceMode_Blocked(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	tmp, err := os.MkdirTemp("", "update_ws_blocked")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManager(mock, tmp)
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

	tag := "24.04"
	err = mgr.UpdateImage(ctx, inst.InstanceID, &tag)
	if err == nil {
		t.Fatal("expected error for workspace-mode update")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateImage_MultiServiceMode_RequiresRootfs(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	tmp, err := os.MkdirTemp("", "update_multi_rootfs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	mock := NewMockContainerManager()
	mgr, err := NewAppManager(mock, tmp)
	if err != nil {
		t.Fatalf("new app manager: %v", err)
	}
	allowHostStorage(t, mgr)
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
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Multi-service update now proceeds (not blocked), but requires rootfs manager.
	tag := "1.26"
	err = mgr.UpdateImage(ctx, inst.InstanceID, &tag)
	if err == nil {
		t.Fatal("expected error: rootfs manager not configured in unit test")
	}
	if !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
}
