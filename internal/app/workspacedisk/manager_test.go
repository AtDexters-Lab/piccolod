package workspacedisk

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mockPathResolver is a test implementation of WorkspacePathResolver
type mockPathResolver struct {
	paths map[string]string
}

func newMockPathResolver() *mockPathResolver {
	return &mockPathResolver{paths: make(map[string]string)}
}

func (m *mockPathResolver) WorkspaceDir(instanceID string) (string, error) {
	if path, ok := m.paths[instanceID]; ok {
		return path, nil
	}
	return "", errors.New("not found")
}

func (m *mockPathResolver) register(instanceID, path string) {
	m.paths[instanceID] = path
}

// mockRuntimeResolver is a test implementation of RuntimeResolver
type mockRuntimeResolver struct{}

func (m *mockRuntimeResolver) GetRuntimeArgs(ctx context.Context, instanceID string) ([]string, error) {
	return []string{"--root", "/mock/root"}, nil
}

// mockImageMounter is a test implementation of BaseImageMounter
type mockImageMounter struct {
	mountedImages map[string]string // imageRef -> rootfsPath
	mountCalls    []string
	unmountCalls  []string
	failMount     bool
	failUnmount   bool
}

func newMockImageMounter() *mockImageMounter {
	return &mockImageMounter{
		mountedImages: make(map[string]string),
	}
}

func (m *mockImageMounter) MountImage(ctx context.Context, imageRef string, args []string) (string, error) {
	m.mountCalls = append(m.mountCalls, imageRef)
	if m.failMount {
		return "", errors.New("mock mount failure")
	}
	path := "/mock/mount/" + imageRef
	m.mountedImages[imageRef] = path
	return path, nil
}

func (m *mockImageMounter) UnmountImage(ctx context.Context, imageRef string, args []string) error {
	m.unmountCalls = append(m.unmountCalls, imageRef)
	if m.failUnmount {
		return errors.New("mock unmount failure")
	}
	delete(m.mountedImages, imageRef)
	return nil
}

func (m *mockImageMounter) ImageRootfs(ctx context.Context, imageRef string, args []string) (string, error) {
	if path, ok := m.mountedImages[imageRef]; ok {
		return path, nil
	}
	return "", errors.New("not mounted")
}

func TestInitOptions_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    InitOptions
		wantErr bool
	}{
		{
			name: "valid options",
			opts: InitOptions{
				BaseImageDigest: "docker.io/library/ubuntu@sha256:abc123",
				BaseImageRef:    "ubuntu:22.04",
				ImageConfig: ImageConfig{
					Cmd: []string{"/bin/bash"},
				},
			},
			wantErr: false,
		},
		{
			name: "missing base image digest",
			opts: InitOptions{
				BaseImageRef: "ubuntu:22.04",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultManager_EnsureInitialized(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	pathResolver := newMockPathResolver()
	pathResolver.register("test-instance", tmpDir)
	runtimeResolver := &mockRuntimeResolver{}

	imageMounter := newMockImageMounter()

	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	opts := InitOptions{
		BaseImageDigest: "docker.io/library/ubuntu@sha256:abc123",
		BaseImageRef:    "ubuntu:22.04",
		ImageConfig: ImageConfig{
			Entrypoint: []string{"/bin/sh"},
			Cmd:        []string{"-c", "echo hello"},
		},
	}

	// First initialization
	if err := mgr.EnsureInitialized(ctx, "test-instance", opts); err != nil {
		t.Fatalf("EnsureInitialized() error = %v", err)
	}

	// Verify meta.json was created
	if !MetaExists(tmpDir) {
		t.Error("meta.json was not created")
	}

	// Verify directories were created
	layout := NewLayout(tmpDir)
	if err := layout.EnsureDirs(); err != nil {
		t.Errorf("directories were not created: %v", err)
	}

	// Second initialization (should be idempotent)
	if err := mgr.EnsureInitialized(ctx, "test-instance", opts); err != nil {
		t.Fatalf("EnsureInitialized() second call error = %v", err)
	}
}

func TestDefaultManager_EnsureInitialized_InvalidOptions(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	pathResolver := newMockPathResolver()
	pathResolver.register("test-instance", tmpDir)
	runtimeResolver := &mockRuntimeResolver{}

	imageMounter := newMockImageMounter()
	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	opts := InitOptions{
		// Missing BaseImageDigest
		BaseImageRef: "ubuntu:22.04",
	}

	err := mgr.EnsureInitialized(ctx, "test-instance", opts)
	if err == nil {
		t.Error("EnsureInitialized() error = nil, want error")
	}
}

func TestDefaultManager_GetLayout(t *testing.T) {
	tmpDir := t.TempDir()

	pathResolver := newMockPathResolver()
	pathResolver.register("test-instance", tmpDir)
	runtimeResolver := &mockRuntimeResolver{}

	imageMounter := newMockImageMounter()
	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	layout, err := mgr.GetLayout("test-instance")
	if err != nil {
		t.Fatalf("GetLayout() error = %v", err)
	}

	if layout.Base != tmpDir {
		t.Errorf("layout.Base = %v, want %v", layout.Base, tmpDir)
	}
}

func TestDefaultManager_GetLayout_NotFound(t *testing.T) {
	pathResolver := newMockPathResolver()
	runtimeResolver := &mockRuntimeResolver{}
	imageMounter := newMockImageMounter()
	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	_, err := mgr.GetLayout("nonexistent-instance")
	if err == nil {
		t.Error("GetLayout() error = nil, want error")
	}
}

func TestDefaultManager_Status(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	pathResolver := newMockPathResolver()
	pathResolver.register("test-instance", tmpDir)
	runtimeResolver := &mockRuntimeResolver{}

	imageMounter := newMockImageMounter()
	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	// Status before initialization
	status, err := mgr.Status(ctx, "test-instance")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Initialized {
		t.Error("Status.Initialized = true before initialization")
	}
	if status.Mounted {
		t.Error("Status.Mounted = true before initialization")
	}

	// Initialize
	opts := InitOptions{
		BaseImageDigest: "test-image@sha256:abc",
		BaseImageRef:    "test-image:latest",
	}
	if err := mgr.EnsureInitialized(ctx, "test-instance", opts); err != nil {
		t.Fatalf("EnsureInitialized() error = %v", err)
	}

	// Status after initialization
	status, err = mgr.Status(ctx, "test-instance")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Initialized {
		t.Error("Status.Initialized = false after initialization")
	}
	if status.BaseImageDigest != opts.BaseImageDigest {
		t.Errorf("Status.BaseImageDigest = %v, want %v", status.BaseImageDigest, opts.BaseImageDigest)
	}
	if status.BaseImageRef != opts.BaseImageRef {
		t.Errorf("Status.BaseImageRef = %v, want %v", status.BaseImageRef, opts.BaseImageRef)
	}
}

func TestDefaultManager_GetMeta(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	pathResolver := newMockPathResolver()
	pathResolver.register("test-instance", tmpDir)
	runtimeResolver := &mockRuntimeResolver{}

	imageMounter := newMockImageMounter()
	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	// Initialize first
	opts := InitOptions{
		BaseImageDigest: "test-image@sha256:abc",
		BaseImageRef:    "test-image:latest",
		ImageConfig: ImageConfig{
			Cmd: []string{"/bin/bash"},
		},
	}
	if err := mgr.EnsureInitialized(ctx, "test-instance", opts); err != nil {
		t.Fatalf("EnsureInitialized() error = %v", err)
	}

	// Get meta
	meta, err := mgr.GetMeta(ctx, "test-instance")
	if err != nil {
		t.Fatalf("GetMeta() error = %v", err)
	}

	if meta.BaseImageDigest != opts.BaseImageDigest {
		t.Errorf("meta.BaseImageDigest = %v, want %v", meta.BaseImageDigest, opts.BaseImageDigest)
	}
	if meta.BaseImageRef != opts.BaseImageRef {
		t.Errorf("meta.BaseImageRef = %v, want %v", meta.BaseImageRef, opts.BaseImageRef)
	}
	if len(meta.ImageConfig.Cmd) != 1 || meta.ImageConfig.Cmd[0] != "/bin/bash" {
		t.Errorf("meta.ImageConfig.Cmd = %v, want [/bin/bash]", meta.ImageConfig.Cmd)
	}
}

func TestDefaultManager_GetMeta_NotInitialized(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	pathResolver := newMockPathResolver()
	pathResolver.register("test-instance", tmpDir)
	runtimeResolver := &mockRuntimeResolver{}

	imageMounter := newMockImageMounter()
	mgr := NewManager(pathResolver, runtimeResolver, imageMounter)

	_, err := mgr.GetMeta(ctx, "test-instance")
	if err == nil {
		t.Error("GetMeta() error = nil, want error for uninitialized workspace")
	}
}

func TestWrapError(t *testing.T) {
	// Test nil error
	if WrapError("test", "op", nil) != nil {
		t.Error("WrapError(nil) should return nil")
	}

	// Test wrapped error
	originalErr := errors.New("original error")
	wrapped := WrapError("test-id", "mount", originalErr)

	if wrapped == nil {
		t.Fatal("WrapError() returned nil")
	}

	expectedMsg := "workspace disk [test-id] mount: original error"
	if wrapped.Error() != expectedMsg {
		t.Errorf("wrapped.Error() = %v, want %v", wrapped.Error(), expectedMsg)
	}

	// Test unwrap
	var wdErr *WorkspaceDiskError
	if !errors.As(wrapped, &wdErr) {
		t.Error("wrapped error should be WorkspaceDiskError")
	}
	if wdErr.InstanceID != "test-id" {
		t.Errorf("InstanceID = %v, want test-id", wdErr.InstanceID)
	}
	if wdErr.Operation != "mount" {
		t.Errorf("Operation = %v, want mount", wdErr.Operation)
	}
	if !errors.Is(wrapped, originalErr) {
		t.Error("errors.Is(wrapped, originalErr) = false, want true")
	}
}

func TestPodmanImageMounter(t *testing.T) {
	// Test creation
	mounter := NewPodmanImageMounter()
	if mounter == nil {
		t.Fatal("NewPodmanImageMounter() returned nil")
	}
}

func TestImageConfig_Empty(t *testing.T) {
	var cfg ImageConfig

	cmd := cfg.BuildOriginalCommand()
	if len(cmd) != 1 || cmd[0] != "/bin/sh" {
		t.Errorf("Empty ImageConfig.BuildOriginalCommand() = %v, want [/bin/sh]", cmd)
	}
}

func TestWorkspaceMeta_CreatedAt(t *testing.T) {
	now := time.Now().UTC()
	meta := &WorkspaceMeta{
		FormatVersion:   MetaFormatVersion,
		BaseImageDigest: "test:latest",
		CreatedAt:       now,
	}

	if err := meta.Validate(); err != nil {
		t.Errorf("Validate() error = %v", err)
	}

	if !meta.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", meta.CreatedAt, now)
	}
}
