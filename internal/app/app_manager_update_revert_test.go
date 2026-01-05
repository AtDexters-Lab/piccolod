package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/api"
)

func TestAppManager_UpdateImage_And_Revert(t *testing.T) {
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

	// Install initial app
	def := &api.AppDefinition{
		Name: "demoapp", Image: "alpine:3.18", Type: "user",
		Listeners:  []api.AppListener{{Name: "web", GuestPort: 80}},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	inst, err := mgr.Install(ctx, def, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	firstCID := inst.ContainerID
	instanceID := inst.InstanceID

	// Update tag to 3.19
	tag := "3.19"
	if err := mgr.UpdateImage(ctx, instanceID, &tag); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Verify new image written
	defPath := filepath.Join(tmp, AppsDir, instanceID, "app.yaml")
	curData, err := os.ReadFile(defPath)
	if err != nil {
		t.Fatalf("read app.yaml: %v", err)
	}
	cur, err := ParseAppDefinition(curData)
	if err != nil {
		t.Fatalf("parse app.yaml: %v", err)
	}
	if cur.Image != "alpine:3.19" {
		t.Fatalf("expected image alpine:3.19, got %s", cur.Image)
	}
	// Instance should have new CID
	inst2, err := mgr.Get(ctx, instanceID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if inst2.ContainerID == firstCID {
		t.Fatalf("expected new container id after update")
	}
	if inst2.Status != "running" {
		t.Fatalf("expected status running after update, got %s", inst2.Status)
	}
	if c := mock.containers[inst2.ContainerID]; c == nil || c.Status != "running" {
		t.Fatalf("expected container to be started after update")
	}

	// Revert back to previous
	if err := mgr.Revert(ctx, instanceID); err != nil {
		t.Fatalf("revert: %v", err)
	}
	curData2, err := os.ReadFile(defPath)
	if err != nil {
		t.Fatalf("read app.yaml after revert: %v", err)
	}
	cur2, err := ParseAppDefinition(curData2)
	if err != nil {
		t.Fatalf("parse app.yaml after revert: %v", err)
	}
	// Expect image to be 3.18 again
	if cur2.Image != "alpine:3.18" {
		t.Fatalf("expected image alpine:3.18 after revert, got %s", cur2.Image)
	}
	inst3, err := mgr.Get(ctx, instanceID)
	if err != nil {
		t.Fatalf("get app after revert: %v", err)
	}
	if inst3.Status != "running" {
		t.Fatalf("expected status running after revert, got %s", inst3.Status)
	}
	if c := mock.containers[inst3.ContainerID]; c == nil || c.Status != "running" {
		t.Fatalf("expected container to be started after revert")
	}
}

func TestAppManager_Logs(t *testing.T) {
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

	def := &api.AppDefinition{
		Name:       "demo",
		Image:      "alpine:latest",
		Type:       "user",
		Listeners:  []api.AppListener{{Name: "web", GuestPort: 80}},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	inst, err := mgr.Install(ctx, def, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if inst.ContainerID == "" {
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
