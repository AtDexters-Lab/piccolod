package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/state/paths"
)

// imagePullProgressRange defines the progress percentage range for an image pull.
type imagePullProgressRange struct {
	Min int // Starting progress percentage
	Max int // Ending progress percentage
}

// makeImagePullProgressCallback builds a progress callback that maps pull
// progress (0-100%) into the specified range and emits SSE events to the frontend.
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
		var progress int
		if report.OverallPercent < 0 {
			progress = (progressRange.Min + progressRange.Max) / 2
		} else {
			rangeSpan := progressRange.Max - progressRange.Min
			progress = progressRange.Min + (report.OverallPercent*rangeSpan)/100
		}

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

// rootfsMountInfo holds the result of block-native rootfs preparation.
type rootfsMountInfo struct {
	handle    persistence.RootfsHandle
	imgConfig persistence.GoldenImageConfig
}

// prepareRootfsStorage prepares block-native rootfs for a service container.
// For ModeService: creates golden LV + service rootfs snapshot (read-only idmapped).
// For ModeWorkspace: creates golden LV + workspace rootfs snapshot (read-write idmapped).
// serviceName is used for per-service volume IDs in multi-container apps (empty for workspace).
// imageSizeHint, when > 0, is the uncompressed image size from a prior inspect — avoids a redundant pull.
// prePulledDir, when non-empty, is a podman root dir where the image is already pulled — flattenFn reuses it.
func (m *AppManager) prepareRootfsStorage(
	ctx context.Context,
	mode PiccoloMode,
	instanceID string,
	serviceName string,
	imageDigest, imageRef string,
	idmapConfig persistence.IDMapConfig,
	imageSizeHint int64,
	prePulledDir string,
) (*rootfsMountInfo, error) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return nil, fmt.Errorf("rootfs volume manager not configured")
	}

	var handle persistence.RootfsHandle
	var err error

	switch mode {
	case ModeService:
		handle, err = rootfs.CreateServiceRootfs(ctx, persistence.ServiceRootfsRequest{
			InstanceID:    instanceID,
			ServiceName:   serviceName,
			ImageDigest:   imageDigest,
			ImageRef:      imageRef,
			IDMap:         idmapConfig,
			ImageSizeHint: imageSizeHint,
			PrePulledDir:  prePulledDir,
		})
	case ModeWorkspace:
		handle, err = rootfs.CreateWorkspaceFromGolden(ctx, persistence.WorkspaceRootfsRequest{
			InstanceID:    instanceID,
			ImageDigest:   imageDigest,
			ImageRef:      imageRef,
			IDMap:         idmapConfig,
			ImageSizeHint: imageSizeHint,
			PrePulledDir:  prePulledDir,
		})
	default:
		return nil, fmt.Errorf("unknown mode %q for rootfs", mode)
	}
	if err != nil {
		return nil, fmt.Errorf("create rootfs: %w", err)
	}

	// Get image config from the golden LV.
	imgConfig, err := m.readImageConfigForRootfs(ctx, rootfs, imageDigest)
	if err != nil {
		log.Printf("WARN: rootfs %s: failed to read image config: %v", instanceID, err)
		imgConfig = persistence.GoldenImageConfig{}
	}

	return &rootfsMountInfo{
		handle:    handle,
		imgConfig: imgConfig,
	}, nil
}

// readImageConfigForRootfs reads the golden image config by deriving the golden LV ID.
func (m *AppManager) readImageConfigForRootfs(ctx context.Context, rootfs persistence.RootfsVolumeManager, imageDigest string) (persistence.GoldenImageConfig, error) {
	goldenID := "golden-" + persistence.ShortDigest(imageDigest)
	return rootfs.ReadGoldenImageConfig(ctx, goldenID)
}


// ensureRootfsAttached ensures a rootfs volume is attached.
// Returns (nil, nil) if the volume doesn't exist — the app was installed
// before block-native rootfs and uses the legacy storage path.
func (m *AppManager) ensureRootfsAttached(ctx context.Context, instanceID string, mode PiccoloMode) (*rootfsMountInfo, error) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return nil, nil
	}

	var volumeID string
	switch mode {
	case ModeWorkspace:
		volumeID = rootfs.RootfsVolumeID("workspace", instanceID)
	case ModeService:
		volumeID = rootfs.RootfsVolumeID("service-rootfs", instanceID)
	default:
		return nil, nil
	}

	// Check if rootfs was created for this instance. Apps installed before
	// block-native rootfs won't have metadata — fall through to legacy path.
	if !rootfs.RootfsExists(volumeID) {
		return nil, nil
	}

	handle, err := rootfs.AttachRootfs(ctx, volumeID)
	if err != nil {
		return nil, fmt.Errorf("attach rootfs %s: %w", volumeID, err)
	}

	return &rootfsMountInfo{handle: handle}, nil
}

// appHasBlockNativeRootfs returns true if the app instance has a block-native
// rootfs volume (i.e., was installed with the new storage path).
func (m *AppManager) appHasBlockNativeRootfs(instanceID string, mode PiccoloMode) bool {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return false
	}
	var modeStr string
	switch mode {
	case ModeWorkspace:
		modeStr = "workspace"
	case ModeService:
		modeStr = "service-rootfs"
	default:
		return false
	}
	volumeID := rootfs.RootfsVolumeID(modeStr, instanceID)
	return rootfs.RootfsExists(volumeID)
}

// detachAppRootfs detaches a rootfs volume. Best-effort.
func (m *AppManager) detachAppRootfs(ctx context.Context, instanceID string, mode PiccoloMode) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return
	}

	var volumeID string
	switch mode {
	case ModeWorkspace:
		volumeID = rootfs.RootfsVolumeID("workspace", instanceID)
	case ModeService:
		volumeID = rootfs.RootfsVolumeID("service-rootfs", instanceID)
	default:
		return
	}

	if err := rootfs.DetachRootfs(ctx, volumeID); err != nil {
		log.Printf("WARN: detach rootfs %s: %v", volumeID, err)
	}
}

// destroyAppRootfs destroys a rootfs volume and runs golden LV GC.
func (m *AppManager) destroyAppRootfs(ctx context.Context, instanceID string, mode PiccoloMode) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return
	}

	var volumeID string
	switch mode {
	case ModeWorkspace:
		volumeID = rootfs.RootfsVolumeID("workspace", instanceID)
	case ModeService:
		volumeID = rootfs.RootfsVolumeID("service-rootfs", instanceID)
	default:
		return
	}

	if err := rootfs.DestroyRootfs(ctx, volumeID); err != nil {
		log.Printf("WARN: destroy rootfs %s: %v", volumeID, err)
	}

	if err := rootfs.GarbageCollectGoldenLVs(ctx); err != nil {
		log.Printf("WARN: golden LV GC: %v", err)
	}
}

// --- Multi-rootfs functions for multi-container apps ---

// ensureAllServiceRootfsAttached attaches all per-service rootfs volumes.
// Returns a map of svcName → *rootfsMountInfo, or (nil, nil) if no rootfs exists.
// For workspace mode, delegates to the single-rootfs path.
// When appInst is non-nil and has ActiveRootfs set, uses the versioned volume IDs.
func (m *AppManager) ensureAllServiceRootfsAttached(
	ctx context.Context,
	instanceID string,
	mode PiccoloMode,
	appDef *api.AppDefinition,
	appInst *AppInstance,
) (map[string]*rootfsMountInfo, error) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return nil, nil
	}

	// Workspace mode: single rootfs for the primary service + anchor rootfs.
	if mode == ModeWorkspace {
		rInfo, err := m.ensureRootfsAttached(ctx, instanceID, mode)
		if err != nil || rInfo == nil {
			return nil, err
		}
		// Read image config from golden LV (required for --rootfs mode containers).
		if rInfo.handle.GoldenLV != "" {
			imgCfg, cfgErr := rootfs.ReadGoldenImageConfig(ctx, rInfo.handle.GoldenLV)
			if cfgErr != nil {
				return nil, fmt.Errorf("read image config for workspace rootfs: %w", cfgErr)
			}
			rInfo.imgConfig = imgCfg
		}
		primary := primaryServiceFor(appDef, nil)
		result := map[string]*rootfsMountInfo{primary: rInfo}

		// Also attach anchor rootfs (all multi-container apps have one).
		anchorVolID := ""
		if appInst != nil && appInst.ActiveRootfs != nil {
			anchorVolID = appInst.ActiveRootfs[networkAnchorServiceName]
		}
		if anchorVolID == "" {
			anchorVolID = persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName)
		}
		if rootfs.RootfsExists(anchorVolID) {
			handle, aErr := rootfs.AttachRootfs(ctx, anchorVolID)
			if aErr != nil {
				return nil, fmt.Errorf("attach rootfs for network anchor: %w", aErr)
			}
			var anchorCfg persistence.GoldenImageConfig
			if handle.GoldenLV != "" {
				if cfg, cfgErr := rootfs.ReadGoldenImageConfig(ctx, handle.GoldenLV); cfgErr == nil {
					anchorCfg = cfg
				}
			}
			log.Printf("INFO: attached anchor rootfs %s (mount=%s)", anchorVolID, handle.MountPath)
			result[networkAnchorServiceName] = &rootfsMountInfo{handle: handle, imgConfig: anchorCfg}
		}
		return result, nil
	}

	if appDef == nil || appDef.Services == nil {
		return nil, nil
	}

	// Service mode: per-service rootfs + network anchor rootfs.
	result := make(map[string]*rootfsMountInfo, len(appDef.Services)+1)
	rollbackAttached := func() {
		for _, info := range result {
			if detachErr := rootfs.DetachRootfs(ctx, info.handle.VolumeID); detachErr != nil {
				log.Printf("WARN: rollback detach rootfs %s: %v", info.handle.VolumeID, detachErr)
			}
		}
	}

	// Attach network anchor rootfs (not in appDef.Services — synthetic container).
	anchorVolID := ""
	if appInst != nil && appInst.ActiveRootfs != nil {
		anchorVolID = appInst.ActiveRootfs[networkAnchorServiceName]
	}
	if anchorVolID == "" {
		anchorVolID = persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName)
	}
	if rootfs.RootfsExists(anchorVolID) {
		handle, err := rootfs.AttachRootfs(ctx, anchorVolID)
		if err != nil {
			return nil, fmt.Errorf("attach rootfs for network anchor: %w", err)
		}
		var anchorCfg persistence.GoldenImageConfig
		if handle.GoldenLV != "" {
			if cfg, cfgErr := rootfs.ReadGoldenImageConfig(ctx, handle.GoldenLV); cfgErr == nil {
				anchorCfg = cfg
			}
		}
		log.Printf("INFO: attached anchor rootfs %s (mount=%s)", anchorVolID, handle.MountPath)
		result[networkAnchorServiceName] = &rootfsMountInfo{handle: handle, imgConfig: anchorCfg}
	}

	for svcName, svc := range appDef.Services {
		if svc.Image == "" {
			continue
		}
		// Use ActiveRootfs (versioned) if available, otherwise legacy ID.
		volumeID := ""
		if appInst != nil && appInst.ActiveRootfs != nil {
			volumeID = appInst.ActiveRootfs[svcName]
		}
		if volumeID == "" {
			volumeID = persistence.ServiceRootfsVolumeID(instanceID, svcName)
		}
		if !rootfs.RootfsExists(volumeID) {
			continue
		}
		handle, err := rootfs.AttachRootfs(ctx, volumeID)
		if err != nil {
			rollbackAttached()
			return nil, fmt.Errorf("attach rootfs for service %q: %w", svcName, err)
		}
		var imgCfg persistence.GoldenImageConfig
		if handle.GoldenLV != "" {
			cfg, cfgErr := rootfs.ReadGoldenImageConfig(ctx, handle.GoldenLV)
			if cfgErr != nil {
				rollbackAttached()
				// Also detach the just-attached volume.
				_ = rootfs.DetachRootfs(ctx, volumeID)
				return nil, fmt.Errorf("read image config for service %q: %w", svcName, cfgErr)
			}
			imgCfg = cfg
		}
		log.Printf("INFO: attached service rootfs %s (mount=%s)", volumeID, handle.MountPath)
		result[svcName] = &rootfsMountInfo{handle: handle, imgConfig: imgCfg}
	}

	if len(result) > 0 {
		return result, nil
	}

	// Fallback: legacy single-rootfs volume.
	rInfo, err := m.ensureRootfsAttached(ctx, instanceID, mode)
	if err != nil || rInfo == nil {
		return nil, err
	}
	// Read image config directly from golden LV (no imagestore needed).
	if rInfo.handle.GoldenLV != "" {
		imgCfg, cfgErr := rootfs.ReadGoldenImageConfig(ctx, rInfo.handle.GoldenLV)
		if cfgErr == nil {
			rInfo.imgConfig = imgCfg
		} else {
			log.Printf("WARN: rootfs %s: failed to read legacy image config: %v", instanceID, cfgErr)
		}
	}
	// Apply legacy rootfs to all services with images.
	for svcName, svc := range appDef.Services {
		if svc.Image == "" {
			continue
		}
		result[svcName] = rInfo
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// detachAllServiceRootfs detaches all per-service rootfs volumes. Best-effort.
func (m *AppManager) detachAllServiceRootfs(ctx context.Context, instanceID string, mode PiccoloMode, appDef *api.AppDefinition, appInst *AppInstance) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return
	}

	// Detach network anchor rootfs (all multi-container apps have an anchor).
	if appDef != nil && appDef.Services != nil {
		anchorVolID := ""
		if appInst != nil && appInst.ActiveRootfs != nil {
			anchorVolID = appInst.ActiveRootfs[networkAnchorServiceName]
		}
		if anchorVolID == "" {
			anchorVolID = persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName)
		}
		if err := rootfs.DetachRootfs(ctx, anchorVolID); err != nil {
			log.Printf("WARN: detach anchor rootfs %s: %v", anchorVolID, err)
		}
	}

	// Detach per-service rootfs volumes (service mode only — workspace uses single rootfs).
	if mode == ModeService && appDef != nil && appDef.Services != nil {
		for svcName, svc := range appDef.Services {
			if svc.Image == "" {
				continue
			}
			// Use ActiveRootfs (versioned) if available, otherwise legacy ID.
			volumeID := ""
			if appInst != nil && appInst.ActiveRootfs != nil {
				volumeID = appInst.ActiveRootfs[svcName]
			}
			if volumeID == "" {
				volumeID = persistence.ServiceRootfsVolumeID(instanceID, svcName)
			}
			if err := rootfs.DetachRootfs(ctx, volumeID); err != nil {
				log.Printf("WARN: detach rootfs %s: %v", volumeID, err)
			}
		}
	}

	// Also try legacy single-rootfs.
	m.detachAppRootfs(ctx, instanceID, mode)
}

// destroyAllServiceRootfs destroys all per-service rootfs volumes and runs GC.
// Scans for both legacy (no digest) and versioned (with digest) rootfs volumes
// to ensure complete cleanup on uninstall.
func (m *AppManager) destroyAllServiceRootfs(ctx context.Context, instanceID string, mode PiccoloMode, appDef *api.AppDefinition) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return
	}

	if appDef != nil && appDef.Services != nil {
		// Scan metadata directory for ALL rootfs volumes matching each service prefix.
		// This catches both legacy (svc-rootfs-id--svcName) and versioned
		// (svc-rootfs-id--svcName--digest) volumes from image updates.
		// Not gated on mode — all multi-container apps have an anchor rootfs.
		metaBase := paths.CoreJoin("volumes")
		entries, readErr := os.ReadDir(metaBase)
		if readErr != nil {
			log.Printf("WARN: scan rootfs volumes: %v", readErr)
		}

		// Collect all prefixes: anchor + services.
		prefixes := make([]string, 0, len(appDef.Services)+1)
		prefixes = append(prefixes, persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName))
		for svcName, svc := range appDef.Services {
			if svc.Image == "" {
				continue
			}
			prefixes = append(prefixes, persistence.ServiceRootfsVolumeID(instanceID, svcName))
		}
		for _, prefix := range prefixes {
			for _, e := range entries {
				if e.IsDir() && (e.Name() == prefix || strings.HasPrefix(e.Name(), prefix+"--")) {
					if err := rootfs.DestroyRootfs(ctx, e.Name()); err != nil {
						log.Printf("WARN: destroy rootfs %s: %v", e.Name(), err)
					}
				}
			}
		}
	}

	// Also destroy legacy single-rootfs (includes GC).
	m.destroyAppRootfs(ctx, instanceID, mode)

	// Explicit GC: destroyAppRootfs runs GC too, but we add an explicit call
	// so GC is guaranteed even if the legacy fallback is removed in the future.
	if err := rootfs.GarbageCollectGoldenLVs(ctx); err != nil {
		log.Printf("WARN: golden LV GC: %v", err)
	}
}

// appHasAnyServiceRootfs returns true if any service rootfs exists for the app.
func (m *AppManager) appHasAnyServiceRootfs(instanceID string, mode PiccoloMode, appDef *api.AppDefinition, appInst *AppInstance) bool {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return false
	}

	// Check anchor rootfs (all multi-container apps have an anchor).
	if appDef != nil && appDef.Services != nil {
		anchorVolID := persistence.ServiceRootfsVolumeID(instanceID, networkAnchorServiceName)
		if appInst != nil && appInst.ActiveRootfs != nil {
			if vid := appInst.ActiveRootfs[networkAnchorServiceName]; vid != "" {
				anchorVolID = vid
			}
		}
		if rootfs.RootfsExists(anchorVolID) {
			return true
		}
	}

	// Check per-service volumes (versioned first, then legacy).
	if mode == ModeService && appDef != nil && appDef.Services != nil {
		for svcName, svc := range appDef.Services {
			if svc.Image == "" {
				continue
			}
			// Check ActiveRootfs (versioned) first.
			if appInst != nil && appInst.ActiveRootfs != nil {
				if vid := appInst.ActiveRootfs[svcName]; vid != "" && rootfs.RootfsExists(vid) {
					return true
				}
			}
			volumeID := persistence.ServiceRootfsVolumeID(instanceID, svcName)
			if rootfs.RootfsExists(volumeID) {
				return true
			}
		}
	}

	// Check legacy single-rootfs.
	return m.appHasBlockNativeRootfs(instanceID, mode)
}

// MakeFlattenFn creates the flatten function that extracts an OCI image to a target directory
// and returns its OCI config. Injected into the persistence layer during service construction.
// When prePulledDir is non-empty, the image is already pulled in that podman root dir —
// the function reuses it instead of pulling again (eliminates redundant network round-trip).
func (m *AppManager) MakeFlattenFn() func(ctx context.Context, imageRef, targetDir, prePulledDir string) (persistence.GoldenImageConfig, error) {
	return func(ctx context.Context, imageRef, targetDir, prePulledDir string) (persistence.GoldenImageConfig, error) {
		var cfg persistence.GoldenImageConfig

		var rt container.PodmanRuntime
		var cleanup func()

		if prePulledDir != "" {
			// Reuse the caller's pre-pulled runtime — image is already there.
			rt = newRuntimeFromDir(prePulledDir, m.runtimeUser)
			cleanup = func() {} // caller owns the directory lifecycle
			log.Printf("INFO: flatten %s: reusing pre-pulled runtime at %s", imageRef, prePulledDir)
		} else {
			var rtErr error
			rt, cleanup, rtErr = m.newFlattenRuntime(ctx)
			if rtErr != nil {
				return cfg, fmt.Errorf("ephemeral runtime: %w", rtErr)
			}
			// Pull the image into ephemeral runtime.
			if err := m.containerManager.PullImage(ctx, rt, imageRef); err != nil {
				cleanup()
				return cfg, fmt.Errorf("pull image %s: %w", imageRef, err)
			}
		}
		defer cleanup()

		// Extract image config.
		imgConfig, err := m.containerManager.InspectImage(ctx, rt, imageRef)
		if err != nil {
			return cfg, fmt.Errorf("inspect image %s: %w", imageRef, err)
		}
		cfg = persistence.GoldenImageConfig{
			Entrypoint: imgConfig.Entrypoint,
			Cmd:        imgConfig.Cmd,
			Env:        imgConfig.Env,
			User:       imgConfig.User,
			WorkingDir: imgConfig.WorkingDir,
		}

		// Create throwaway container.
		cid, err := m.containerManager.CreateContainer(ctx, rt, container.ContainerCreateSpec{
			Image:   imageRef,
			Command: []string{"true"},
		})
		if err != nil {
			return cfg, fmt.Errorf("create throwaway container: %w", err)
		}
		cid = strings.TrimSpace(cid)
		defer func() {
			rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = m.containerManager.RemoveContainer(rmCtx, rt, cid)
		}()

		// Export container → tar extract. Uses exec.Command directly for pipe support.
		if err := m.flattenExportToDir(ctx, rt, cid, targetDir); err != nil {
			return cfg, err
		}

		return cfg, nil
	}
}

// MakeImageSizeFn creates a function that returns the uncompressed image size.
// Uses an ephemeral podman runtime — no persistent imagestore.
func (m *AppManager) MakeImageSizeFn() func(ctx context.Context, imageRef string) (int64, error) {
	return func(ctx context.Context, imageRef string) (int64, error) {
		rt, cleanup, rtErr := m.newFlattenRuntime(ctx)
		if rtErr != nil {
			return 0, fmt.Errorf("ephemeral runtime: %w", rtErr)
		}
		defer cleanup()

		if err := m.containerManager.PullImage(ctx, rt, imageRef); err != nil {
			return 0, fmt.Errorf("pull image %s: %w", imageRef, err)
		}

		imgConfig, err := m.containerManager.InspectImage(ctx, rt, imageRef)
		if err != nil {
			return 0, fmt.Errorf("inspect image %s: %w", imageRef, err)
		}
		return imgConfig.Size, nil
	}
}

// flattenExportToDir pipes `podman export` to `tar x` for image flattening.
func (m *AppManager) flattenExportToDir(ctx context.Context, rt container.PodmanRuntime, cid, targetDir string) error {
	pr, pw := io.Pipe()

	// Build podman args with storage flags for the export command.
	exportArgs, err := container.BuildPodmanArgs(rt, []string{"export", cid})
	if err != nil {
		return fmt.Errorf("build export args: %w", err)
	}
	var exportStderr, tarStderr bytes.Buffer

	exportCmd := exec.CommandContext(ctx, "podman", exportArgs...)
	exportCmd.Stdout = pw
	exportCmd.Stderr = &exportStderr
	container.ApplyRuntimeCredential(exportCmd, rt)

	tarCmd := exec.CommandContext(ctx, "tar", "x",
		"--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", targetDir)
	tarCmd.Stdin = pr
	tarCmd.Stderr = &tarStderr

	if err := tarCmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fmt.Errorf("start tar: %w", err)
	}

	var exportErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		exportErr = exportCmd.Run()
		pw.Close()
	}()

	tarErr := tarCmd.Wait()
	pr.Close()
	wg.Wait()

	if exportErr != nil {
		return fmt.Errorf("podman export: %w (stderr: %s)", exportErr, exportStderr.String())
	}
	if tarErr != nil {
		return fmt.Errorf("tar extract: %w (stderr: %s)", tarErr, tarStderr.String())
	}

	return nil
}
