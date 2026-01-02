package app

import (
	"context"
	"fmt"
	"log"

	"piccolod/internal/app/workspacedisk"
	"piccolod/internal/container"
)

// workspacePathResolver implements workspacedisk.WorkspacePathResolver
// by looking up volume layouts through the AppManager.
type workspacePathResolver struct {
	layouts map[string]string // instanceID -> workspaceDir
}

func newWorkspacePathResolver() *workspacePathResolver {
	return &workspacePathResolver{
		layouts: make(map[string]string),
	}
}

func (r *workspacePathResolver) WorkspaceDir(instanceID string) (string, error) {
	if dir, ok := r.layouts[instanceID]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("workspace path not registered for instance: %s", instanceID)
}

func (r *workspacePathResolver) Register(instanceID, workspaceDir string) {
	r.layouts[instanceID] = workspaceDir
}

func (r *workspacePathResolver) Unregister(instanceID string) {
	delete(r.layouts, instanceID)
}

// initWorkspaceDisk initializes the workspace disk for a workspace mode app.
// It creates the disk structure, saves metadata, and mounts the overlay.
// Returns the merged rootfs path for use with --rootfs mode.
func (m *AppManager) initWorkspaceDisk(
	ctx context.Context,
	instanceID string,
	layout appVolumeLayout,
	runtime container.PodmanRuntime,
	imgConfig *container.ImageConfig,
	baseImageDigest, baseImageRef string,
) (mergedPath string, err error) {
	if m.workspaceDiskMgr == nil {
		return "", fmt.Errorf("workspace disk manager not configured")
	}

	// Register the workspace path for this instance
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)

	// Convert container.ImageConfig to workspacedisk.ImageConfig
	// We preserve the full image config (entrypoint, cmd, env, workdir, user)
	// because Podman does not apply image config automatically in --rootfs mode.
	wsImageConfig := workspacedisk.ImageConfig{}
	if imgConfig != nil {
		wsImageConfig.Entrypoint = imgConfig.Entrypoint
		wsImageConfig.Cmd = imgConfig.Cmd
		wsImageConfig.Env = imgConfig.Env
		wsImageConfig.WorkingDir = imgConfig.WorkingDir
		wsImageConfig.User = imgConfig.User
	}

	// Initialize the workspace disk
	opts := workspacedisk.InitOptions{
		BaseImageDigest: baseImageDigest,
		BaseImageRef:    baseImageRef,
		ImageConfig:     wsImageConfig,
	}

	if err := m.workspaceDiskMgr.EnsureInitialized(ctx, instanceID, opts); err != nil {
		m.workspacePathResolver.Unregister(instanceID)
		return "", fmt.Errorf("initialize workspace disk: %w", err)
	}

	// Mount the overlay filesystem
	mergedPath, err = m.workspaceDiskMgr.Mount(ctx, instanceID)
	if err != nil {
		m.workspacePathResolver.Unregister(instanceID)
		return "", fmt.Errorf("mount workspace disk: %w", err)
	}

	return mergedPath, nil
}

// unmountWorkspaceDisk unmounts the workspace disk overlay.
func (m *AppManager) unmountWorkspaceDisk(ctx context.Context, instanceID string) error {
	if m.workspaceDiskMgr == nil {
		return nil
	}

	if err := m.workspaceDiskMgr.Unmount(ctx, instanceID); err != nil {
		log.Printf("WARN: workspace %s: unmount failed: %v", instanceID, err)
		return err
	}

	m.workspacePathResolver.Unregister(instanceID)
	return nil
}

// cleanupStaleWorkspaceMounts attempts to cleanup stale mounts from previous crashes.
func (m *AppManager) cleanupStaleWorkspaceMounts(ctx context.Context, instanceID string, layout appVolumeLayout) {
	if m.workspaceDiskMgr == nil {
		return
	}

	// Register path temporarily for cleanup
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)
	defer m.workspacePathResolver.Unregister(instanceID)

	if err := m.workspaceDiskMgr.CleanupStale(ctx, instanceID); err != nil {
		log.Printf("WARN: workspace %s: stale mount cleanup failed: %v", instanceID, err)
	}
}

// getWorkspaceDiskMeta retrieves metadata for an existing workspace disk.
func (m *AppManager) getWorkspaceDiskMeta(ctx context.Context, instanceID string, layout appVolumeLayout) (*workspacedisk.WorkspaceMeta, error) {
	if m.workspaceDiskMgr == nil {
		return nil, fmt.Errorf("workspace disk manager not configured")
	}

	// Register path temporarily
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)
	defer m.workspacePathResolver.Unregister(instanceID)

	return m.workspaceDiskMgr.GetMeta(ctx, instanceID)
}

// isWorkspaceDiskInitialized checks if a workspace disk has been initialized.
func (m *AppManager) isWorkspaceDiskInitialized(ctx context.Context, instanceID string, layout appVolumeLayout) bool {
	if m.workspaceDiskMgr == nil {
		return false
	}

	// Register path temporarily
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)
	defer m.workspacePathResolver.Unregister(instanceID)

	status, err := m.workspaceDiskMgr.Status(ctx, instanceID)
	if err != nil {
		return false
	}
	return status.Initialized
}

// ensureWorkspaceDiskMounted ensures the workspace disk is mounted, mounting if needed.
// It also ensures the base image is present locally before mounting (RFC §5.3).
// Returns the merged rootfs path.
func (m *AppManager) ensureWorkspaceDiskMounted(ctx context.Context, instanceID string, layout appVolumeLayout) (string, error) {
	if m.workspaceDiskMgr == nil {
		return "", fmt.Errorf("workspace disk manager not configured")
	}

	// Register the workspace path for this instance
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)

	// Check status
	status, err := m.workspaceDiskMgr.Status(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("check workspace disk status: %w", err)
	}

	if !status.Initialized {
		return "", fmt.Errorf("workspace disk not initialized")
	}

	if status.Mounted {
		return status.MergedPath, nil
	}

	// Before mounting, ensure the base image is present locally (RFC §5.3, §8.1).
	// The image may have been GC'd, or this could be a failover to a new node.
	meta, err := m.workspaceDiskMgr.GetMeta(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("get workspace metadata: %w", err)
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return "", fmt.Errorf("get podman runtime: %w", err)
	}

	// Check if the base image exists locally, pull if not
	exists, err := m.containerManager.ImageExists(ctx, runtime, meta.BaseImageDigest)
	if err != nil {
		log.Printf("WARN: workspace %s: failed to check base image existence: %v", instanceID, err)
	}

	if !exists {
		log.Printf("INFO: workspace %s: base image not found locally, pulling %s", instanceID, meta.BaseImageDigest)

		// Try to pull by digest first
		if err := m.containerManager.PullImage(ctx, runtime, meta.BaseImageDigest); err != nil {
			// If digest pull fails, try the original reference as fallback
			// (some registries don't support pulling by digest directly)
			log.Printf("WARN: workspace %s: pull by digest failed, trying reference %s: %v",
				instanceID, meta.BaseImageRef, err)
			if err := m.containerManager.PullImage(ctx, runtime, meta.BaseImageRef); err != nil {
				return "", fmt.Errorf("failed to pull base image: %w", err)
			}
		}
	}

	// Mount the overlay
	return m.workspaceDiskMgr.Mount(ctx, instanceID)
}
