package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"piccolod/internal/api"
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
// The layout parameter is required because the workspacePathResolver is an in-memory
// map that may be empty after server restart - we need the layout to locate the workspace.
func (m *AppManager) unmountWorkspaceDisk(ctx context.Context, instanceID string, layout appVolumeLayout) error {
	if m.workspaceDiskMgr == nil {
		return nil
	}

	// Register path for this operation (may not be registered after restart)
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)

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

// workspaceMountInfo holds the result of workspace disk preparation.
// Used by installContainerGroup to configure --rootfs mode containers.
type workspaceMountInfo struct {
	mergedPath string
	meta       *workspacedisk.WorkspaceMeta
}

// imagePullProgressRange defines the progress percentage range for an image pull.
type imagePullProgressRange struct {
	Min int // Starting progress percentage
	Max int // Ending progress percentage
}

// pullImageWithProgress pulls an image with real-time progress events emitted to the frontend.
// The progressRange maps the pull progress (0-100%) to the specified percentage range.
// Always pulls to ensure mutable tags (like "latest") are refreshed.
func (m *AppManager) pullImageWithProgress(
	ctx context.Context,
	runtime container.PodmanRuntime,
	image string,
	instanceID string,
	svcName string,
	progressRange imagePullProgressRange,
) error {
	// Emit initial progress
	m.emitProgressWithMetadata(
		ctx,
		taskTypeInstallApp,
		instanceID,
		taskPhasePullingImage,
		progressRange.Min,
		fmt.Sprintf("Pulling image %s", image),
		false,
		map[string]any{
			"service": svcName,
			"image":   image,
		},
		nil,
	)

	// Create progress callback that maps pull progress to our range
	callback := func(report container.ImagePullReport) {
		// Map the pull progress (0-100) to our range (Min-Max)
		var progress int
		if report.OverallPercent < 0 {
			// Indeterminate - use midpoint
			progress = (progressRange.Min + progressRange.Max) / 2
		} else {
			rangeSpan := progressRange.Max - progressRange.Min
			progress = progressRange.Min + (report.OverallPercent*rangeSpan)/100
		}

		// Build layer progress metadata
		layers := make([]map[string]any, 0, len(report.Layers))
		for _, layer := range report.Layers {
			layers = append(layers, map[string]any{
				"layer_id":      layer.LayerID,
				"status":        layer.Status,
				"bytes_current": layer.BytesCurrent,
				"bytes_total":   layer.BytesTotal,
			})
		}

		message := fmt.Sprintf("Pulling image %s", image)
		if report.Phase == "complete" {
			message = fmt.Sprintf("Image %s pulled successfully", image)
			progress = progressRange.Max
		} else if report.TotalBytes > 0 {
			// Format bytes for display
			downloaded := formatBytes(report.DownloadedBytes)
			total := formatBytes(report.TotalBytes)
			message = fmt.Sprintf("Pulling %s: %s / %s", image, downloaded, total)
		}

		m.emitProgressWithMetadata(
			ctx,
			taskTypeInstallApp,
			instanceID,
			taskPhasePullingImage,
			progress,
			message,
			false,
			map[string]any{
				"service":          svcName,
				"image":            image,
				"phase":            report.Phase,
				"overall_percent":  report.OverallPercent,
				"total_bytes":      report.TotalBytes,
				"downloaded_bytes": report.DownloadedBytes,
				"layers":           layers,
			},
			nil,
		)
	}

	// Pull image with progress, retrying on transient failures.
	retryDelays := []time.Duration{2 * time.Second, 5 * time.Second}
	maxAttempts := len(retryDelays) + 1

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = m.containerManager.PullImageWithProgress(ctx, runtime, image, callback)
		if lastErr == nil {
			return nil
		}

		// Don't retry on context cancellation or deterministic failures
		if ctx.Err() != nil {
			return lastErr
		}
		errMsg := strings.ToLower(lastErr.Error())
		for _, pattern := range []string{
			"invalid image name",
			"manifest unknown",
			"repository does not exist",
			"unauthorized",
			"authentication required",
			"denied:",
		} {
			if strings.Contains(errMsg, pattern) {
				return lastErr
			}
		}

		log.Printf("WARN: install %s: image pull attempt %d/%d failed for %s: %v",
			instanceID, attempt, maxAttempts, image, lastErr)

		if attempt < maxAttempts {
			delay := retryDelays[attempt-1]
			m.emitProgressWithMetadata(
				ctx,
				taskTypeInstallApp,
				instanceID,
				taskPhasePullingImage,
				progressRange.Min,
				fmt.Sprintf("Retrying image pull (attempt %d/%d)", attempt+1, maxAttempts),
				false,
				map[string]any{
					"service": svcName,
					"image":   image,
					"attempt": attempt + 1,
				},
				nil,
			)

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return lastErr
}

// formatBytes formats bytes into a human-readable string.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// prepareServiceStorage prepares storage for a service container.
// For ModeService: pulls the image (best-effort) with progress tracking and returns nil.
// For ModeWorkspace: initializes/mounts workspace disk and returns mount info.
// The progressRange maps the image pull progress to the specified percentage range.
func (m *AppManager) prepareServiceStorage(
	ctx context.Context,
	mode PiccoloMode,
	svcName string,
	def *api.AppDefinition,
	instanceID string,
	layout appVolumeLayout,
	runtime container.PodmanRuntime,
	progressRange imagePullProgressRange,
) (*workspaceMountInfo, error) {
	if def == nil || def.Services == nil {
		return nil, fmt.Errorf("app definition with services required")
	}
	svc, ok := def.Services[svcName]
	if !ok {
		return nil, fmt.Errorf("unknown service '%s'", svcName)
	}

	if mode != ModeWorkspace {
		// Service mode: pull the image with progress tracking (best-effort)
		if svc.Image != "" {
			if err := m.pullImageWithProgress(ctx, runtime, svc.Image, instanceID, svcName, progressRange); err != nil {
				log.Printf("WARN: install %s: image pull failed for service %s: %v", instanceID, svcName, err)
			}
		}
		return nil, nil
	}

	// Workspace mode: prepare workspace disk
	if svc.Image == "" {
		return nil, fmt.Errorf("workspace service '%s' requires an image", svcName)
	}

	// Check if workspace disk is already initialized (reinstall case)
	diskInitialized := m.isWorkspaceDiskInitialized(ctx, instanceID, layout)

	if !diskInitialized {
		// New install: pull base image with progress and initialize workspace disk.
		// Pull is best-effort to preserve offline resilience — if the image is
		// already cached locally, a pull failure (network down, registry issue)
		// should not block installation. The subsequent InspectImage call is the
		// hard gate: if the image isn't available at all, it will fail there.
		pullErr := m.pullImageWithProgress(ctx, runtime, svc.Image, instanceID, svcName, progressRange)
		if pullErr != nil {
			log.Printf("WARN: install %s: image pull failed, will attempt to use cached image: %v", instanceID, pullErr)
		}

		// Get image config for workspace disk metadata.
		// If the pull failed and the image isn't cached either, surface both
		// the pull error (root cause) and the inspect error (symptom).
		imgConfig, err := m.containerManager.InspectImage(ctx, runtime, svc.Image)
		if err != nil {
			if pullErr != nil {
				return nil, fmt.Errorf("image %s unavailable: pull failed: %v, and image not cached locally: %w", svc.Image, pullErr, err)
			}
			return nil, fmt.Errorf("inspect image %s: %w", svc.Image, err)
		}

		// Get canonical digest for the base image
		baseImageDigest := ""
		if len(imgConfig.RepoDigests) > 0 {
			baseImageDigest = imgConfig.RepoDigests[0]
		} else {
			baseImageDigest = imgConfig.Digest
		}
		if baseImageDigest == "" {
			return nil, fmt.Errorf("image digest not available for %s", svc.Image)
		}

		// Initialize and mount workspace disk
		mergedPath, err := m.initWorkspaceDisk(ctx, instanceID, layout, runtime, imgConfig, baseImageDigest, svc.Image)
		if err != nil {
			return nil, fmt.Errorf("init workspace disk: %w", err)
		}

		// Get metadata for return
		meta, err := m.getWorkspaceDiskMeta(ctx, instanceID, layout)
		if err != nil {
			return nil, fmt.Errorf("get workspace metadata: %w", err)
		}

		return &workspaceMountInfo{
			mergedPath: mergedPath,
			meta:       meta,
		}, nil
	}

	// Reinstall: workspace disk exists, just mount it
	mergedPath, err := m.ensureWorkspaceDiskMounted(ctx, instanceID, layout)
	if err != nil {
		return nil, fmt.Errorf("mount workspace disk: %w", err)
	}

	meta, err := m.getWorkspaceDiskMeta(ctx, instanceID, layout)
	if err != nil {
		return nil, fmt.Errorf("get workspace metadata: %w", err)
	}

	log.Printf("INFO: install %s: using existing workspace disk (base=%s)", instanceID, meta.BaseImageRef)

	return &workspaceMountInfo{
		mergedPath: mergedPath,
		meta:       meta,
	}, nil
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

	// Use ModeWorkspace (vfs) consistently for workspace apps.
	// Layer deduplication happens via the shared imagestore, not the driver choice.
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, ModeWorkspace)
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

// getWorkspaceMountInfo returns workspace mount info for an already-mounted workspace disk.
// Call this after ensureWorkspaceDiskMounted to get both the merged path and metadata.
// Returns nil for service-mode apps or if workspace disk is not mounted.
func (m *AppManager) getWorkspaceMountInfo(ctx context.Context, instanceID string) *workspaceMountInfo {
	if m.workspaceDiskMgr == nil {
		return nil
	}

	status, err := m.workspaceDiskMgr.Status(ctx, instanceID)
	if err != nil || !status.Mounted {
		return nil
	}

	meta, err := m.workspaceDiskMgr.GetMeta(ctx, instanceID)
	if err != nil {
		return nil
	}

	return &workspaceMountInfo{
		mergedPath: status.MergedPath,
		meta:       meta,
	}
}
