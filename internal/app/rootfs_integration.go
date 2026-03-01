package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"piccolod/internal/container"
	"piccolod/internal/persistence"
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
func (m *AppManager) prepareRootfsStorage(
	ctx context.Context,
	mode PiccoloMode,
	instanceID string,
	imageDigest, imageRef string,
	idmapConfig persistence.IDMapConfig,
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
			InstanceID:  instanceID,
			ImageDigest: imageDigest,
			ImageRef:    imageRef,
			IDMap:       idmapConfig,
		})
	case ModeWorkspace:
		handle, err = rootfs.CreateWorkspaceFromGolden(ctx, persistence.WorkspaceRootfsRequest{
			InstanceID:  instanceID,
			ImageDigest: imageDigest,
			ImageRef:    imageRef,
			IDMap:       idmapConfig,
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

// readImageConfigForRootfsFromInstance reads the golden image config for an existing app instance.
// It inspects the primary service image to derive the golden LV ID.
func (m *AppManager) readImageConfigForRootfsFromInstance(ctx context.Context, appInst *AppInstance) (persistence.GoldenImageConfig, error) {
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return persistence.GoldenImageConfig{}, fmt.Errorf("rootfs volume manager not configured")
	}
	if appInst == nil || appInst.Definition == nil || appInst.Definition.Services == nil {
		return persistence.GoldenImageConfig{}, fmt.Errorf("app instance has no definition")
	}
	// Find the first service with an image to derive the golden LV ID.
	for _, svc := range appInst.Definition.Services {
		if svc.Image == "" {
			continue
		}
		// Inspect the image to get the digest (needed for golden LV ID derivation).
		imageRuntime, err := m.podmanImageRuntime()
		if err != nil {
			return persistence.GoldenImageConfig{}, fmt.Errorf("get image runtime: %w", err)
		}
		imgConfig, err := m.containerManager.InspectImage(ctx, imageRuntime, svc.Image)
		if err != nil {
			return persistence.GoldenImageConfig{}, fmt.Errorf("inspect image %s: %w", svc.Image, err)
		}
		imageDigest := ""
		if len(imgConfig.RepoDigests) > 0 {
			imageDigest = imgConfig.RepoDigests[0]
		} else {
			imageDigest = imgConfig.Digest
		}
		return m.readImageConfigForRootfs(ctx, rootfs, imageDigest)
	}
	return persistence.GoldenImageConfig{}, fmt.Errorf("no service with image found in definition")
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

// MakeFlattenFn creates the flatten function that extracts an OCI image to a target directory
// and returns its OCI config. Injected into the persistence layer during service construction.
func (m *AppManager) MakeFlattenFn() func(ctx context.Context, imageRef, targetDir string) (persistence.GoldenImageConfig, error) {
	return func(ctx context.Context, imageRef, targetDir string) (persistence.GoldenImageConfig, error) {
		var cfg persistence.GoldenImageConfig

		rt, err := m.podmanImageRuntime()
		if err != nil {
			return cfg, fmt.Errorf("image runtime: %w", err)
		}

		// Pull the image (best-effort for offline resilience).
		if pullErr := m.pullToImagestore(ctx, imageRef, nil); pullErr != nil {
			log.Printf("WARN: flatten: image pull failed, will attempt with cached: %v", pullErr)
		}

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
// It performs a best-effort pull first (priming the cache for the subsequent flatten),
// then inspects the image to get its size.
func (m *AppManager) MakeImageSizeFn() func(ctx context.Context, imageRef string) (int64, error) {
	return func(ctx context.Context, imageRef string) (int64, error) {
		rt, err := m.podmanImageRuntime()
		if err != nil {
			return 0, fmt.Errorf("image runtime: %w", err)
		}

		// Best-effort pull — primes cache so flattenFn's pull is a no-op.
		if pullErr := m.pullToImagestore(ctx, imageRef, nil); pullErr != nil {
			log.Printf("WARN: imageSizeFn: image pull failed, will attempt with cached: %v", pullErr)
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
