package workspacedisk

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager handles workspace disk lifecycle operations.
// It is responsible for:
// - Initializing workspace disk layout and metadata
// - Mounting/unmounting the overlay filesystem
// - Cleaning up stale mounts after crashes
// - Providing status information for observability
type Manager interface {
	// EnsureInitialized creates the workspace disk structure and meta.json if not present.
	// If already initialized, it validates the existing metadata.
	EnsureInitialized(ctx context.Context, instanceID string, opts InitOptions) error

	// Mount mounts the overlay filesystem and returns the merged path.
	// Idempotent: returns success if already mounted.
	Mount(ctx context.Context, instanceID string) (mergedPath string, err error)

	// Unmount unmounts the overlay filesystem.
	// Idempotent: returns success if not mounted.
	Unmount(ctx context.Context, instanceID string) error

	// CleanupStale attempts to clean up any stale mounts from previous crashes.
	// This should be called on startup or before Mount.
	CleanupStale(ctx context.Context, instanceID string) error

	// Status returns the current state of the workspace disk.
	Status(ctx context.Context, instanceID string) (Status, error)

	// GetMeta returns the workspace metadata if initialized.
	GetMeta(ctx context.Context, instanceID string) (*WorkspaceMeta, error)

	// GetLayout returns the directory layout for a workspace.
	GetLayout(instanceID string) (Layout, error)
}

// InitOptions contains parameters for initializing a workspace disk.
type InitOptions struct {
	// BaseImageDigest is the canonical digest of the base image.
	// Example: "docker.io/library/ubuntu@sha256:..."
	BaseImageDigest string

	// BaseImageRef is the user-facing reference (e.g., "ubuntu:22.04").
	BaseImageRef string

	// ImageConfig contains the OCI image configuration.
	ImageConfig ImageConfig
}

// Validate checks that all required fields are present.
func (o InitOptions) Validate() error {
	if o.BaseImageDigest == "" {
		return fmt.Errorf("base_image_digest is required")
	}
	return nil
}

// Status represents the current state of a workspace disk.
type Status struct {
	// Initialized is true if meta.json exists and is valid.
	Initialized bool

	// Mounted is true if the overlay is currently mounted.
	Mounted bool

	// MergedPath is the path to the mounted rootfs (empty if not mounted).
	MergedPath string

	// BaseImageDigest from meta.json (empty if not initialized).
	BaseImageDigest string

	// BaseImageRef from meta.json (empty if not initialized).
	BaseImageRef string

	// CreatedAt from meta.json (zero if not initialized).
	CreatedAt time.Time
}

// BaseImageMounter provides access to base image rootfs.
// This interface allows the manager to obtain a read-only mount of the base image
// without directly depending on Podman.
type BaseImageMounter interface {
	// MountImage mounts a container image and returns the path to its rootfs.
	// The args parameter allows passing runtime-specific arguments (e.g., --root).
	// The caller must call UnmountImage when done.
	MountImage(ctx context.Context, imageRef string, args []string) (rootfsPath string, err error)

	// UnmountImage unmounts a previously mounted image.
	UnmountImage(ctx context.Context, imageRef string, args []string) error

	// ImageRootfs returns the rootfs path for an already-mounted image.
	// Returns error if the image is not mounted.
	ImageRootfs(ctx context.Context, imageRef string, args []string) (string, error)
}

// WorkspacePathResolver provides paths for workspace disk directories.
type WorkspacePathResolver interface {
	// WorkspaceDir returns the workspace directory for an instance.
	// Example: /path/to/volume/disk/workspace
	WorkspaceDir(instanceID string) (string, error)
}

// RuntimeResolver provides runtime arguments for container operations.
type RuntimeResolver interface {
	// GetRuntimeArgs returns the podman arguments (e.g. --root, --runroot)
	// required for the specific app instance.
	GetRuntimeArgs(ctx context.Context, instanceID string) ([]string, error)
}

// DefaultManager is the production implementation of Manager.
type DefaultManager struct {
	pathResolver    WorkspacePathResolver
	runtimeResolver RuntimeResolver
	imageMounter    BaseImageMounter

	// mu protects concurrent operations on the same instance
	mu sync.Mutex

	// mountedImages tracks which base images are currently mounted
	mountedImages map[string]string // instanceID -> imageRef
}

// NewManager creates a new workspace disk manager.
func NewManager(pathResolver WorkspacePathResolver, runtimeResolver RuntimeResolver, imageMounter BaseImageMounter) *DefaultManager {
	return &DefaultManager{
		pathResolver:    pathResolver,
		runtimeResolver: runtimeResolver,
		imageMounter:    imageMounter,
		mountedImages:   make(map[string]string),
	}
}

// EnsureInitialized implements Manager.
func (m *DefaultManager) EnsureInitialized(ctx context.Context, instanceID string, opts InitOptions) error {
	if err := opts.Validate(); err != nil {
		return WrapError(instanceID, "validate options", err)
	}

	layout, err := m.GetLayout(instanceID)
	if err != nil {
		return WrapError(instanceID, "get layout", err)
	}

	// Create directories
	if err := layout.EnsureDirs(); err != nil {
		return WrapError(instanceID, "ensure directories", err)
	}

	// Check if already initialized
	if MetaExists(layout.Base) {
		// Validate existing metadata
		meta, err := LoadMeta(layout.Base)
		if err != nil {
			return WrapError(instanceID, "load existing meta", err)
		}

		// Warn if base image digest changed (should not happen normally)
		if meta.BaseImageDigest != opts.BaseImageDigest {
			log.Printf("WARN: workspace %s: base image digest mismatch: meta=%s opts=%s",
				instanceID, meta.BaseImageDigest, opts.BaseImageDigest)
		}

		return nil // Already initialized
	}

	// Create new metadata
	meta := &WorkspaceMeta{
		FormatVersion:   MetaFormatVersion,
		BaseImageDigest: opts.BaseImageDigest,
		BaseImageRef:    opts.BaseImageRef,
		ImageConfig:     opts.ImageConfig,
		CreatedAt:       time.Now().UTC(),
	}

	if err := SaveMeta(layout.Base, meta); err != nil {
		return WrapError(instanceID, "save meta", err)
	}

	log.Printf("INFO: workspace %s: initialized workspace disk (base=%s)", instanceID, opts.BaseImageRef)
	return nil
}

// Mount implements Manager.
func (m *DefaultManager) Mount(ctx context.Context, instanceID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	layout, err := m.GetLayout(instanceID)
	if err != nil {
		return "", WrapError(instanceID, "get layout", err)
	}

	// Check if initialized
	if !MetaExists(layout.Base) {
		return "", WrapError(instanceID, "mount", ErrNotInitialized)
	}

	// Check if already mounted
	mounted, err := isMountedFromMtab(layout.Merged)
	if err != nil {
		return "", WrapError(instanceID, "check mount", err)
	}
	if mounted {
		return layout.Merged, nil // Already mounted, return success
	}

	// Load metadata to get base image
	meta, err := LoadMeta(layout.Base)
	if err != nil {
		return "", WrapError(instanceID, "load meta", err)
	}

	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") == "1" {
		if err := layout.EnsureDirs(); err != nil {
			return "", WrapError(instanceID, "ensure directories", err)
		}
		return layout.Merged, nil
	}

	// Get runtime args for this instance to ensure we access the correct imagestore
	runtimeArgs, err := m.runtimeResolver.GetRuntimeArgs(ctx, instanceID)
	if err != nil {
		return "", WrapError(instanceID, "get runtime args", err)
	}

	// Mount base image
	lowerDir, err := m.imageMounter.MountImage(ctx, meta.BaseImageDigest, runtimeArgs)
	if err != nil {
		return "", WrapError(instanceID, "mount base image", fmt.Errorf("%w: %v", ErrBaseImageMissing, err))
	}

	// Track mounted image for cleanup
	m.mountedImages[instanceID] = meta.BaseImageDigest

	// Mount overlay
	if err := MountOverlay(ctx, layout, lowerDir); err != nil {
		// Cleanup base image mount on failure
		_ = m.imageMounter.UnmountImage(ctx, meta.BaseImageDigest, runtimeArgs)
		delete(m.mountedImages, instanceID)
		return "", WrapError(instanceID, "mount overlay", err)
	}

	log.Printf("INFO: workspace %s: mounted overlay at %s", instanceID, layout.Merged)
	return layout.Merged, nil
}

// Unmount implements Manager.
func (m *DefaultManager) Unmount(ctx context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	layout, err := m.GetLayout(instanceID)
	if err != nil {
		return WrapError(instanceID, "get layout", err)
	}

	// Unmount overlay
	if err := UnmountOverlay(ctx, layout); err != nil {
		return WrapError(instanceID, "unmount overlay", err)
	}

	// Unmount base image if we tracked it
	if imageRef, ok := m.mountedImages[instanceID]; ok {
		// Best effort to get runtime args. If we fail, we try with empty/default args.
		runtimeArgs, _ := m.runtimeResolver.GetRuntimeArgs(ctx, instanceID)

		if err := m.imageMounter.UnmountImage(ctx, imageRef, runtimeArgs); err != nil {
			log.Printf("WARN: workspace %s: failed to unmount base image: %v", instanceID, err)
		}
		delete(m.mountedImages, instanceID)
	}

	log.Printf("INFO: workspace %s: unmounted overlay", instanceID)
	return nil
}

// CleanupStale implements Manager.
func (m *DefaultManager) CleanupStale(ctx context.Context, instanceID string) error {
	layout, err := m.GetLayout(instanceID)
	if err != nil {
		return nil // Can't cleanup if we can't get the layout
	}

	if err := CleanupStaleMount(ctx, layout); err != nil {
		log.Printf("WARN: workspace %s: stale mount cleanup failed: %v", instanceID, err)
	}

	return nil
}

// Status implements Manager.
func (m *DefaultManager) Status(ctx context.Context, instanceID string) (Status, error) {
	layout, err := m.GetLayout(instanceID)
	if err != nil {
		return Status{}, WrapError(instanceID, "get layout", err)
	}

	status := Status{}

	// Check if initialized
	if MetaExists(layout.Base) {
		status.Initialized = true
		meta, err := LoadMeta(layout.Base)
		if err == nil {
			status.BaseImageDigest = meta.BaseImageDigest
			status.BaseImageRef = meta.BaseImageRef
			status.CreatedAt = meta.CreatedAt
		}
	}

	// Check if mounted
	mounted, err := isMountedFromMtab(layout.Merged)
	if err == nil && mounted {
		status.Mounted = true
		status.MergedPath = layout.Merged
	}

	return status, nil
}

// GetMeta implements Manager.
func (m *DefaultManager) GetMeta(ctx context.Context, instanceID string) (*WorkspaceMeta, error) {
	layout, err := m.GetLayout(instanceID)
	if err != nil {
		return nil, WrapError(instanceID, "get layout", err)
	}

	meta, err := LoadMeta(layout.Base)
	if err != nil {
		return nil, WrapError(instanceID, "load meta", err)
	}

	return meta, nil
}

// GetLayout implements Manager.
func (m *DefaultManager) GetLayout(instanceID string) (Layout, error) {
	workspaceDir, err := m.pathResolver.WorkspaceDir(instanceID)
	if err != nil {
		return Layout{}, err
	}
	return NewLayout(workspaceDir), nil
}

// PodmanImageMounter implements BaseImageMounter using Podman.
type PodmanImageMounter struct {
	mu sync.Mutex
	// mountCounts tracks reference counts for mounted images
	// Key is composite: "args|imageRef" to support multiple runtimes/stores
	mountCounts map[string]int
}

// NewPodmanImageMounter creates a new Podman-based image mounter.
func NewPodmanImageMounter() *PodmanImageMounter {
	return &PodmanImageMounter{
		mountCounts: make(map[string]int),
	}
}

func getMountKey(imageRef string, args []string) string {
	return fmt.Sprintf("%s|%s", strings.Join(args, " "), imageRef)
}

// MountImage implements BaseImageMounter.
func (p *PodmanImageMounter) MountImage(ctx context.Context, imageRef string, args []string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := getMountKey(imageRef, args)

	// If already mounted in this runtime context, increment count and return existing path
	if p.mountCounts[key] > 0 {
		path, err := p.getImageMountPath(ctx, imageRef, args)
		if err == nil {
			p.mountCounts[key]++
			return path, nil
		}
		// Mount path not found despite refcount, reset and remount
		p.mountCounts[key] = 0
	}

	cmdArgs := append([]string{}, args...)
	cmdArgs = append(cmdArgs, "image", "mount", imageRef)

	cmd := exec.CommandContext(ctx, "podman", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman image mount failed: %w: %s", err, string(output))
	}

	path := filepath.Clean(string(output[:len(output)-1])) // Remove trailing newline
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("mounted path not accessible: %w", err)
	}

	p.mountCounts[key] = 1
	return path, nil
}

// UnmountImage implements BaseImageMounter.
func (p *PodmanImageMounter) UnmountImage(ctx context.Context, imageRef string, args []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := getMountKey(imageRef, args)

	count := p.mountCounts[key]
	if count <= 0 {
		return nil // Not mounted by us in this context
	}

	count--
	p.mountCounts[key] = count

	if count > 0 {
		return nil // Still referenced
	}

	// Last reference, actually unmount
	delete(p.mountCounts, key)

	cmdArgs := append([]string{}, args...)
	cmdArgs = append(cmdArgs, "image", "unmount", imageRef)

	cmd := exec.CommandContext(ctx, "podman", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Best effort - log but don't fail
		log.Printf("WARN: podman image unmount %s failed: %v: %s", imageRef, err, string(output))
	}

	return nil
}

// ImageRootfs implements BaseImageMounter.
func (p *PodmanImageMounter) ImageRootfs(ctx context.Context, imageRef string, args []string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := getMountKey(imageRef, args)

	if p.mountCounts[key] <= 0 {
		return "", fmt.Errorf("image not mounted in this runtime: %s", imageRef)
	}

	return p.getImageMountPath(ctx, imageRef, args)
}

// getImageMountPath queries podman for the mount path of an image.
func (p *PodmanImageMounter) getImageMountPath(ctx context.Context, imageRef string, args []string) (string, error) {
	// We use mount again to get the path (idempotent)
	cmdArgs := append([]string{}, args...)
	cmdArgs = append(cmdArgs, "image", "mount", imageRef)

	cmd := exec.CommandContext(ctx, "podman", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman image mount query failed: %w: %s", err, string(output))
	}

	return filepath.Clean(string(output[:len(output)-1])), nil
}
