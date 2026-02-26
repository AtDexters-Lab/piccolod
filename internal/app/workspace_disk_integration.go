package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/app/workspacedisk"
	"piccolod/internal/container"
)

// buildWorkspaceMountOpts creates MountOptions for the workspace overlay.
// Populates UID/GID mapping fields for per-app user isolation. With kernel
// overlay, these mappings are not applied at the mount level — the container's
// user namespace handles UID translation instead.
//
// The mapping has three entries:
//  1. Host root (UID 0) → per-app user: for files created by piccolod in the upper layer
//  2. Image runtime UID → per-app user: for image root files (stored as runtime UID)
//  3. Image runtime subuids → per-app subuids: for non-root image files
func buildWorkspaceMountOpts(instanceID string, runtime container.PodmanRuntime, imageRuntime *container.PodmanRuntime) workspacedisk.MountOptions {
	opts := workspacedisk.MountOptions{SquashUID: -1, SquashGID: -1}
	if runtime.Credential == nil {
		return opts
	}

	appUID := int(runtime.Credential.Uid)
	appGID := int(runtime.Credential.Gid)
	username := container.AppUsername(instanceID)

	// squashFallback sets squash_to_uid/gid as a degraded fallback when
	// proper UID mapping cannot be constructed (e.g., subuid lookup failure).
	squashFallback := func(reason string, err error) workspacedisk.MountOptions {
		log.Printf("WARN: workspace %s: %s (%v), falling back to squash_to_uid", instanceID, reason, err)
		opts.SquashUID = appUID
		opts.SquashGID = appGID
		return opts
	}

	appSubStart, appSubCount, err := container.LookupSubUIDRange(username)
	if err != nil {
		return squashFallback("subuid lookup failed", err)
	}

	// Image layers are stored with the image runtime user's remapped UIDs
	// (rootless Podman remaps during extraction). We need to map from the
	// runtime's UID space to the per-app user's UID space.
	// Format: "disk_uid:overlay_uid:count" entries separated by colons.
	if imageRuntime != nil && imageRuntime.Credential != nil {
		rtUID := int(imageRuntime.Credential.Uid)
		rtGID := int(imageRuntime.Credential.Gid)
		rtUsername := container.RuntimeUsername
		rtSubStart, rtSubCount, err := container.LookupSubUIDRange(rtUsername)
		if err != nil {
			return squashFallback("runtime subuid lookup failed", err)
		}
		subCount := rtSubCount
		if appSubCount < subCount {
			subCount = appSubCount
		}
		// Entry 1: host root (UID 0) → per-app user (for upper layer dirs created by piccolod)
		// Entry 2: runtime UID → per-app user (image root = stored as runtime UID)
		// Entry 3: runtime subuids → per-app subuids (image non-root UIDs)
		opts.UIDMapping = fmt.Sprintf("0:%d:1:%d:%d:1:%d:%d:%d", appUID, rtUID, appUID, rtSubStart, appSubStart, subCount)
		opts.GIDMapping = fmt.Sprintf("0:%d:1:%d:%d:1:%d:%d:%d", appGID, rtGID, appGID, rtSubStart, appSubStart, subCount)
	} else {
		// No image runtime — assume layers have original UIDs (pulled by root).
		opts.UIDMapping = fmt.Sprintf("0:%d:1:1:%d:%d", appUID, appSubStart, appSubCount)
		opts.GIDMapping = fmt.Sprintf("0:%d:1:1:%d:%d", appGID, appSubStart, appSubCount)
	}
	return opts
}

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

// withWorkspacePath temporarily registers a workspace path for the duration of
// a workspace disk operation. Returns a cleanup function that unregisters the path.
// Usage: defer m.withWorkspacePath(instanceID, layout)()
func (m *AppManager) withWorkspacePath(instanceID string, layout appVolumeLayout) func() {
	m.workspacePathResolver.Register(instanceID, layout.WorkspaceDir)
	return func() { m.workspacePathResolver.Unregister(instanceID) }
}

// imageConfigToWorkspace converts a container.ImageConfig to workspacedisk.ImageConfig.
// Returns a zero-value ImageConfig if cfg is nil.
func imageConfigToWorkspace(cfg *container.ImageConfig) workspacedisk.ImageConfig {
	if cfg == nil {
		return workspacedisk.ImageConfig{}
	}
	return workspacedisk.ImageConfig{
		Entrypoint: cfg.Entrypoint,
		Cmd:        cfg.Cmd,
		Env:        cfg.Env,
		WorkingDir: cfg.WorkingDir,
		User:       cfg.User,
	}
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

	// Initialize the workspace disk.
	// imageConfigToWorkspace preserves the full image config (entrypoint, cmd,
	// env, workdir, user) because Podman does not apply image config in --rootfs mode.
	opts := workspacedisk.InitOptions{
		BaseImageDigest: baseImageDigest,
		BaseImageRef:    baseImageRef,
		ImageConfig:     imageConfigToWorkspace(imgConfig),
	}

	if err := m.workspaceDiskMgr.EnsureInitialized(ctx, instanceID, opts); err != nil {
		m.workspacePathResolver.Unregister(instanceID)
		return "", fmt.Errorf("initialize workspace disk: %w", err)
	}

	// Mount the overlay filesystem with UID mapping for per-app user isolation.
	imgRt, imgRtErr := m.podmanImageRuntime()
	if imgRtErr != nil {
		log.Printf("WARN: workspace %s: image runtime unavailable, mount opts will lack runtime UID mapping: %v", instanceID, imgRtErr)
	}
	mountOpts := buildWorkspaceMountOpts(instanceID, runtime, &imgRt)
	mergedPath, err = m.workspaceDiskMgr.Mount(ctx, instanceID, mountOpts)
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

	defer m.withWorkspacePath(instanceID, layout)()

	if err := m.workspaceDiskMgr.CleanupStale(ctx, instanceID); err != nil {
		log.Printf("WARN: workspace %s: stale mount cleanup failed: %v", instanceID, err)
	}
}

// getWorkspaceDiskMeta retrieves metadata for an existing workspace disk.
func (m *AppManager) getWorkspaceDiskMeta(ctx context.Context, instanceID string, layout appVolumeLayout) (*workspacedisk.WorkspaceMeta, error) {
	if m.workspaceDiskMgr == nil {
		return nil, fmt.Errorf("workspace disk manager not configured")
	}

	defer m.withWorkspacePath(instanceID, layout)()

	return m.workspaceDiskMgr.GetMeta(ctx, instanceID)
}

// isWorkspaceDiskInitialized checks if a workspace disk has been initialized.
func (m *AppManager) isWorkspaceDiskInitialized(ctx context.Context, instanceID string, layout appVolumeLayout) bool {
	if m.workspaceDiskMgr == nil {
		return false
	}

	defer m.withWorkspacePath(instanceID, layout)()

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

// makeImagePullProgressCallback builds a progress callback that maps pull
// progress (0-100%) into the specified range and emits SSE events to the frontend.
// The ctx must carry the install task ID (via TaskIDFromContext) for events to be emitted.
func (m *AppManager) makeImagePullProgressCallback(
	ctx context.Context,
	instanceID string,
	svcName string,
	image string,
	progressRange imagePullProgressRange,
) func(container.ImagePullReport) {
	// Strip @sha256:... digest from display name — it's noise in the UI.
	displayImage := image
	if idx := strings.Index(displayImage, "@sha256:"); idx > 0 {
		displayImage = displayImage[:idx]
	}

	return func(report container.ImagePullReport) {
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

		message := fmt.Sprintf("Pulling image %s", displayImage)
		if report.Phase == "complete" {
			message = fmt.Sprintf("Image %s pulled successfully", displayImage)
			progress = progressRange.Max
		} else if report.TotalBytes > 0 {
			downloaded := formatBytes(report.DownloadedBytes)
			total := formatBytes(report.TotalBytes)
			message = fmt.Sprintf("Pulling %s: %s / %s", displayImage, downloaded, total)
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
		// Service mode: pull the image using the shared image runtime (piccolo-runtime).
		// Per-app users have group-read-only access to the imagestore — pulls must use
		// the shared runtime which has write access.
		if svc.Image != "" {
			callback := m.makeImagePullProgressCallback(ctx, instanceID, svcName, svc.Image, progressRange)
			if err := m.pullToImagestore(ctx, svc.Image, callback); err != nil {
				return nil, fmt.Errorf("image pull failed for service %s: %w", svcName, err)
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

	var mergedPath string
	if !diskInitialized {
		// New install: pull base image with progress and initialize workspace disk.
		// Use the shared image runtime (overlay driver, shared root) for workspace
		// base images. This provides layer deduplication across apps and persistent
		// metadata that survives app uninstall/reinstall cycles.
		imageRuntime, err := m.podmanImageRuntime()
		if err != nil {
			return nil, fmt.Errorf("get image runtime: %w", err)
		}

		// Pull is best-effort to preserve offline resilience — if the image is
		// already cached locally, a pull failure (network down, registry issue)
		// should not block installation. The subsequent InspectImage call is the
		// hard gate: if the image isn't available at all, it will fail there.
		callback := m.makeImagePullProgressCallback(ctx, instanceID, svcName, svc.Image, progressRange)
		pullErr := m.pullToImagestore(ctx, svc.Image, callback)
		if pullErr != nil {
			log.Printf("WARN: install %s: image pull failed, will attempt to use cached image: %v", instanceID, pullErr)
		}

		// Get image config for workspace disk metadata.
		// If the pull failed and the image isn't cached either, surface both
		// the pull error (root cause) and the inspect error (symptom).
		imgConfig, err := m.containerManager.InspectImage(ctx, imageRuntime, svc.Image)
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
		mp, err := m.initWorkspaceDisk(ctx, instanceID, layout, runtime, imgConfig, baseImageDigest, svc.Image)
		if err != nil {
			return nil, fmt.Errorf("init workspace disk: %w", err)
		}
		mergedPath = mp
	} else {
		// Reinstall: workspace disk exists, just mount it
		mp, err := m.ensureWorkspaceDiskMounted(ctx, instanceID, layout)
		if err != nil {
			return nil, fmt.Errorf("mount workspace disk: %w", err)
		}
		mergedPath = mp
	}

	// Common: retrieve metadata and return mount info.
	meta, err := m.getWorkspaceDiskMeta(ctx, instanceID, layout)
	if err != nil {
		return nil, fmt.Errorf("get workspace metadata: %w", err)
	}

	if diskInitialized {
		log.Printf("INFO: install %s: using existing workspace disk (base=%s)", instanceID, meta.BaseImageRef)
	}

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

	// Use the shared image runtime (overlay driver, shared root) for base image operations.
	// This provides layer deduplication and persistent metadata across app lifecycles.
	//
	// Migration note: workspace apps installed before the image runtime existed have their
	// base images in the old per-app VFS root + shared imagestore. On first start after
	// upgrade, ImageExists will return false and PullImage will re-pull from the registry.
	// This requires network access for the first post-upgrade start only.
	imageRuntime, err := m.podmanImageRuntime()
	if err != nil {
		return "", fmt.Errorf("get image runtime: %w", err)
	}

	// Check if the base image exists locally, pull if not
	exists, err := m.containerManager.ImageExists(ctx, imageRuntime, meta.BaseImageDigest)
	if err != nil {
		log.Printf("WARN: workspace %s: failed to check base image existence: %v", instanceID, err)
	}

	if !exists {
		log.Printf("INFO: workspace %s: base image not found locally, pulling %s", instanceID, meta.BaseImageDigest)
		m.setObservedStatusMessage(instanceID, "Re-pulling base image")

		// Try to pull by digest first
		if err := m.pullToImagestore(ctx, meta.BaseImageDigest, nil); err != nil {
			// If digest pull fails, try the original reference as fallback
			// (some registries don't support pulling by digest directly)
			log.Printf("WARN: workspace %s: pull by digest failed, trying reference %s: %v",
				instanceID, meta.BaseImageRef, err)
			if err := m.pullToImagestore(ctx, meta.BaseImageRef, nil); err != nil {
				return "", fmt.Errorf("failed to pull base image: %w", err)
			}
		}
	}

	// Mount the overlay with per-app user UID mapping if available.
	var rt container.PodmanRuntime
	if appUser, err := container.ResolveAppUser(instanceID); err == nil && appUser != nil {
		rt.Credential = appUser.Credential
	}
	imgRt, imgRtErr := m.podmanImageRuntime()
	if imgRtErr != nil {
		log.Printf("WARN: workspace %s: image runtime unavailable, mount opts will lack runtime UID mapping: %v", instanceID, imgRtErr)
	}
	mountOpts := buildWorkspaceMountOpts(instanceID, rt, &imgRt)
	return m.workspaceDiskMgr.Mount(ctx, instanceID, mountOpts)
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
