package app

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/container"
	"piccolod/internal/persistence"
)

type MockContainerManager struct {
	containers              map[string]*mockContainer
	nextID                  int
	createError             error
	startError              error
	startErrorForContainer  map[string]error // per-container start errors (checked before global startError)
	stopError               error
	removeError             error
	removedImages           []string
	removeImageErr          error
	reloadedContainers      []string
	reloadErr               error
}

type mockContainer struct {
	ID     string
	Status string
	Spec   container.ContainerCreateSpec
}

func NewMockContainerManager() *MockContainerManager {
	return &MockContainerManager{containers: make(map[string]*mockContainer), nextID: 1}
}

func (m *MockContainerManager) CreateContainer(ctx context.Context, runtime container.PodmanRuntime, spec container.ContainerCreateSpec) (string, error) {
	_ = runtime
	if m.createError != nil {
		return "", m.createError
	}
	if m.containers == nil {
		m.containers = make(map[string]*mockContainer)
	}
	id := generateMockContainerID(m.nextID)
	m.nextID++
	m.containers[id] = &mockContainer{ID: id, Status: "created", Spec: spec}
	return id, nil
}

func (m *MockContainerManager) StartContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error {
	_ = runtime
	if err, ok := m.startErrorForContainer[containerID]; ok {
		return err
	}
	if m.startError != nil {
		return m.startError
	}
	if c, ok := m.containers[containerID]; ok {
		c.Status = "running"
		return nil
	}
	return container.ErrContainerNotFound(containerID)
}

func (m *MockContainerManager) StopContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error {
	_ = runtime
	if m.stopError != nil {
		return m.stopError
	}
	if c, ok := m.containers[containerID]; ok {
		c.Status = "stopped"
		return nil
	}
	return container.ErrContainerNotFound(containerID)
}

func (m *MockContainerManager) RemoveContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error {
	_ = runtime
	if m.removeError != nil {
		return m.removeError
	}
	if _, ok := m.containers[containerID]; ok {
		delete(m.containers, containerID)
		return nil
	}
	return container.ErrContainerNotFound(containerID)
}

func (m *MockContainerManager) ListContainersByLabel(ctx context.Context, runtime container.PodmanRuntime, labelKey, labelValue string) ([]container.ContainerListItem, error) {
	_ = ctx
	_ = runtime
	out := []container.ContainerListItem{}
	for id, c := range m.containers {
		if c == nil || c.Spec.Labels == nil {
			continue
		}
		if c.Spec.Labels[labelKey] == labelValue {
			out = append(out, container.ContainerListItem{ID: id, Name: c.Spec.Name})
		}
	}
	return out, nil
}

func (m *MockContainerManager) PullImage(ctx context.Context, runtime container.PodmanRuntime, image string) error {
	_ = runtime
	_ = image
	return nil
}

func (m *MockContainerManager) PullImageWithProgress(ctx context.Context, runtime container.PodmanRuntime, image string, callback container.ImagePullCallback) error {
	_ = runtime
	_ = image
	// Mock: if callback provided, emit a quick progress sequence
	if callback != nil {
		callback(container.ImagePullReport{
			Image:          image,
			OverallPercent: 0,
			Phase:          "pulling",
		})
		callback(container.ImagePullReport{
			Image:          image,
			OverallPercent: 100,
			Phase:          "complete",
		})
	}
	return nil
}

func (m *MockContainerManager) Logs(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int) ([]string, error) {
	_ = runtime
	if _, ok := m.containers[containerID]; !ok {
		return nil, container.ErrContainerNotFound(containerID)
	}
	if lines <= 0 {
		lines = 3
	}
	out := make([]string, lines)
	for i := range out {
		out[i] = "log line"
	}
	return out, nil
}

func (m *MockContainerManager) LogsStream(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int, timestamps bool) (io.ReadCloser, error) {
	_ = ctx
	_ = runtime
	_ = timestamps
	if _, ok := m.containers[containerID]; !ok {
		return nil, container.ErrContainerNotFound(containerID)
	}
	if lines <= 0 {
		lines = 3
	}
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString("log line\n")
	}
	return io.NopCloser(strings.NewReader(b.String())), nil
}

func (m *MockContainerManager) ResolveContainerIDByName(ctx context.Context, runtime container.PodmanRuntime, name string) (string, error) {
	_ = ctx
	_ = runtime
	for id, c := range m.containers {
		if c.Spec.Name == name {
			return id, nil
		}
	}
	return "", container.ErrContainerNotFound(name)
}

func (m *MockContainerManager) InspectContainerState(ctx context.Context, runtime container.PodmanRuntime, containerID string) (container.ContainerState, error) {
	_ = ctx
	_ = runtime
	c, ok := m.containers[containerID]
	if !ok {
		return container.ContainerState{Exists: false, Running: false}, nil
	}
	return container.ContainerState{Exists: true, Running: c.Status == "running"}, nil
}

// NOTE: duplicated in internal/server/gin_app_handlers_test.go (cross-package).
func (m *MockContainerManager) InspectPublishedPorts(ctx context.Context, runtime container.PodmanRuntime, containerID string) (map[string]int, error) {
	_ = ctx
	_ = runtime
	c, ok := m.containers[containerID]
	if !ok {
		return nil, container.ErrContainerNotFound(containerID)
	}
	out := make(map[string]int, len(c.Spec.Ports))
	for _, p := range c.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", p.Container, proto)
		out[key] = p.Host
	}
	return out, nil
}

func (m *MockContainerManager) UpdatePublishAdd(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error {
	_ = ctx
	_ = runtime
	c, ok := m.containers[containerID]
	if !ok {
		return container.ErrContainerNotFound(containerID)
	}
	c.Spec.Ports = append(c.Spec.Ports, container.PortMapping{Host: hostBind, Container: guestPort})
	return nil
}

func (m *MockContainerManager) UpdatePublishRemove(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error {
	_ = ctx
	_ = runtime
	c, ok := m.containers[containerID]
	if !ok {
		return container.ErrContainerNotFound(containerID)
	}
	out := make([]container.PortMapping, 0, len(c.Spec.Ports))
	for _, p := range c.Spec.Ports {
		if p.Host == hostBind && p.Container == guestPort {
			continue
		}
		out = append(out, p)
	}
	c.Spec.Ports = out
	return nil
}

func (m *MockContainerManager) ResetStorage(ctx context.Context, runtime container.PodmanRuntime) error {
	_ = ctx
	_ = runtime
	return nil
}

func (m *MockContainerManager) ValidateAndRepairStorage(ctx context.Context, runtime container.PodmanRuntime) (bool, error) {
	_ = ctx
	_ = runtime
	return false, nil
}

func (m *MockContainerManager) CommitContainer(ctx context.Context, runtime container.PodmanRuntime, containerID, imageName string) error {
	_ = ctx
	_ = runtime
	if _, ok := m.containers[containerID]; !ok {
		return container.ErrContainerNotFound(containerID)
	}
	// Mock: just succeed (image is not actually tracked in the mock)
	return nil
}

func (m *MockContainerManager) ImageExists(ctx context.Context, runtime container.PodmanRuntime, imageName string) (bool, error) {
	_ = ctx
	_ = runtime
	_ = imageName
	// Mock: always return false (no images stored in mock)
	return false, nil
}

func (m *MockContainerManager) RemoveImage(ctx context.Context, runtime container.PodmanRuntime, imageName string) error {
	_ = ctx
	_ = runtime
	m.removedImages = append(m.removedImages, imageName)
	return m.removeImageErr
}

func (m *MockContainerManager) InspectImage(ctx context.Context, runtime container.PodmanRuntime, imageName string) (*container.ImageConfig, error) {
	_ = ctx
	_ = runtime
	_ = imageName
	// Mock: return a typical image config with shell defaults
	return &container.ImageConfig{
		Entrypoint:  nil,
		Cmd:         []string{"/bin/sh"},
		Digest:      "sha256:mockdigest",
		RepoDigests: []string{"docker.io/library/mock-image@sha256:mockdigest"},
		Size:        500 << 20,
	}, nil
}

func (m *MockContainerManager) SearchRegistry(ctx context.Context, runtime container.PodmanRuntime, query string, limit int) ([]container.ImageSearchResult, error) {
	_ = ctx
	_ = runtime
	_ = limit
	// Mock: return some example results based on query
	return []container.ImageSearchResult{
		{Index: "docker.io", Name: "library/" + query, Description: "Mock image", Stars: 100, Official: "[OK]"},
	}, nil
}

func (m *MockContainerManager) NetworkReload(ctx context.Context, runtime container.PodmanRuntime, containerNameOrID string) error {
	_ = ctx
	_ = runtime
	if m.reloadErr != nil {
		return m.reloadErr
	}
	m.reloadedContainers = append(m.reloadedContainers, containerNameOrID)
	return nil
}

func (m *MockContainerManager) ExecShellCmd(runtime container.PodmanRuntime, containerID string) (*exec.Cmd, error) {
	_ = runtime
	// Mock: return a simple echo command for testing purposes
	// In production this would exec into the container
	return exec.Command("echo", "mock shell"), nil
}

func generateMockContainerID(id int) string {
	return "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcd" + string(rune('0'+id%10))
}

// stubRootfsManager is a minimal RootfsVolumeManager for unit tests.
// It creates real temp directories so mount paths resolve for container specs.
type stubRootfsManager struct {
	baseDir string // temp dir for mock mount points
}

func newStubRootfsManager(baseDir string) *stubRootfsManager {
	return &stubRootfsManager{baseDir: baseDir}
}

func (s *stubRootfsManager) EnsureGoldenLV(_ context.Context, req persistence.GoldenLVRequest) (string, error) {
	id := fmt.Sprintf("golden-%s", sanitize(req.ImageDigest))
	return id, nil
}

func (s *stubRootfsManager) CreateWorkspaceFromGolden(_ context.Context, req persistence.WorkspaceRootfsRequest) (persistence.RootfsHandle, error) {
	volID := fmt.Sprintf("ws-%s", req.InstanceID)
	mp := filepath.Join(s.baseDir, "rootfs", volID)
	os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{VolumeID: volID, MountPath: mp, ReadOnly: false}, nil
}

func (s *stubRootfsManager) CreateServiceRootfs(_ context.Context, req persistence.ServiceRootfsRequest) (persistence.RootfsHandle, error) {
	volID := req.VolumeID
	if volID == "" {
		volID = fmt.Sprintf("svc-rootfs-%s--%s", req.InstanceID, req.ServiceName)
	}
	mp := filepath.Join(s.baseDir, "rootfs", volID)
	os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{VolumeID: volID, MountPath: mp, ReadOnly: true}, nil
}

func (s *stubRootfsManager) CloneWorkspace(_ context.Context, originID, cloneID string, _ *persistence.IDMapConfig) (persistence.RootfsHandle, error) {
	mp := filepath.Join(s.baseDir, "rootfs", cloneID)
	os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{VolumeID: cloneID, MountPath: mp}, nil
}

func (s *stubRootfsManager) ListClones(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s *stubRootfsManager) AttachRootfs(_ context.Context, volumeID string) (persistence.RootfsHandle, error) {
	mp := filepath.Join(s.baseDir, "rootfs", volumeID)
	os.MkdirAll(mp, 0o755)
	return persistence.RootfsHandle{VolumeID: volumeID, MountPath: mp, ReadOnly: true}, nil
}

func (s *stubRootfsManager) DetachRootfs(_ context.Context, _ string) error { return nil }
func (s *stubRootfsManager) DestroyRootfs(_ context.Context, _ string) error { return nil }
func (s *stubRootfsManager) GarbageCollectGoldenLVs(_ context.Context) error { return nil }
func (s *stubRootfsManager) ReconcileRootfsStates(_ context.Context) error   { return nil }

func (s *stubRootfsManager) ReadGoldenImageConfig(_ context.Context, _ string) (persistence.GoldenImageConfig, error) {
	return persistence.GoldenImageConfig{
		Cmd: []string{"/bin/sh"},
	}, nil
}

func (s *stubRootfsManager) RootfsVolumeID(mode, instanceID string) string {
	return fmt.Sprintf("%s-%s", mode, instanceID)
}

func (s *stubRootfsManager) RootfsExists(_ string) bool { return true }
func (s *stubRootfsManager) FindGoldenByImageRef(_ string) (string, string, bool) {
	return "", "", false
}
func (s *stubRootfsManager) ResizeWorkspace(_ context.Context, _ string, _ int64) error {
	return nil
}

func sanitize(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
