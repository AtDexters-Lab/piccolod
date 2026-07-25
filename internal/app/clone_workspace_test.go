package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"piccolod/internal/api"
)

func TestCloneWorkspace_OriginNotFound(t *testing.T) {
	tmp, err := os.MkdirTemp("", "clone_not_found")
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

	_, err = mgr.CloneWorkspace(context.Background(), "noexist", "myclone")
	if err == nil {
		t.Fatal("expected error for missing origin")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCloneWorkspaceSeedsTruthfulTaskLifecycle(t *testing.T) {
	state := newCapabilityTestState(t)
	manager, err := NewAppManagerForTest(NewMockContainerManager(), state.stateDir)
	if err != nil {
		t.Fatalf("NewAppManagerForTest: %v", err)
	}
	manager.stateManager = state
	reporter := &recordingArtifactProgressReporter{}
	manager.SetProgressReporter(reporter)

	ctx := WithTaskID(context.Background(), "clone-task")
	if _, err := manager.CloneWorkspace(ctx, "missing", "clone"); err == nil {
		t.Fatal("CloneWorkspace succeeded with missing origin")
	}
	events := reporter.snapshot()
	if len(events) != 2 {
		t.Fatalf("clone events = %d, want start and completion", len(events))
	}
	for _, event := range events {
		if event.TaskType != taskTypeCloneApp {
			t.Fatalf("clone event task type = %q, want %q", event.TaskType, taskTypeCloneApp)
		}
	}
	if events[0].Phase != taskPhaseValidating || !events[1].IsComplete || events[1].Error == "" {
		t.Fatalf("unexpected clone task lifecycle: %#v", events)
	}
}

func TestCloneWorkspace_OriginNotWorkspace(t *testing.T) {
	tmp, err := os.MkdirTemp("", "clone_not_ws")
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

	// Install a service-mode app.
	def := &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{
			{Name: "svcapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:latest", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}
	inst, err := mgr.Install(context.Background(), def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Stop the app so we don't hit the "must be stopped" check before "not a workspace".
	if err := mgr.Stop(context.Background(), inst.InstanceID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	_, err = mgr.CloneWorkspace(context.Background(), inst.InstanceID, "myclone")
	if err == nil {
		t.Fatal("expected error for non-workspace origin")
	}
	if !strings.Contains(err.Error(), "not a workspace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCloneWorkspace_OriginRunning(t *testing.T) {
	tmp, err := os.MkdirTemp("", "clone_running")
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

	// Install a workspace-mode app (leaves it running).
	def := &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{
			{Name: "wsapp", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {Image: "ubuntu:22.04", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{
			"mode":           "workspace",
			"workspace_name": "wsapp",
		},
	}
	_, err = mgr.Install(context.Background(), def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	_, err = mgr.CloneWorkspace(context.Background(), "wsapp", "myclone")
	if err == nil {
		t.Fatal("expected error for running origin")
	}
	if !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCloneWorkspace_InvalidCloneName(t *testing.T) {
	tmp, err := os.MkdirTemp("", "clone_badname")
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

	// Install and stop a workspace app.
	def := &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{
			{Name: "wsapp2", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {Image: "ubuntu:22.04", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{
			"mode":           "workspace",
			"workspace_name": "wsapp2",
		},
	}
	inst, err := mgr.Install(context.Background(), def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Stop(context.Background(), inst.InstanceID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Invalid name with hyphens.
	_, err = mgr.CloneWorkspace(context.Background(), inst.InstanceID, "my-clone")
	if err == nil {
		t.Fatal("expected error for invalid clone name")
	}
	if !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCloneWorkspace_CloneNameCollision(t *testing.T) {
	tmp, err := os.MkdirTemp("", "clone_collision")
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

	// Install a workspace app.
	def := &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{
			{Name: "wsapp3", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {Image: "ubuntu:22.04", BindPorts: []int{80}},
		},
		Extensions: map[string]interface{}{
			"mode":           "workspace",
			"workspace_name": "wsapp3",
		},
	}
	inst, err := mgr.Install(ctx, def)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Stop(ctx, inst.InstanceID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Try to clone with the same name as the origin.
	_, err = mgr.CloneWorkspace(ctx, inst.InstanceID, inst.InstanceID)
	if err == nil {
		t.Fatal("expected error for name collision")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCloneWorkspace_LockedState(t *testing.T) {
	tmp, err := os.MkdirTemp("", "clone_locked")
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
	mgr.ForceLockState(true)

	_, err = mgr.CloneWorkspace(context.Background(), "origin", "clone")
	if err == nil {
		t.Fatal("expected error when locked")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("unexpected error: %v", err)
	}
}
