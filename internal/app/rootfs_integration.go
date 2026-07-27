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
	"sync/atomic"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/state/paths"
)

// transferProgressRange defines the lifecycle progress range assigned to one
// content transfer, regardless of whether the source is an image or artifact.
type transferProgressRange struct {
	Min int // Starting progress percentage
	Max int // Ending progress percentage
}

type transferByteHighWater struct {
	mu         sync.Mutex
	downloaded int64
	total      int64
}

func (h *transferByteHighWater) observe(downloaded, total int64) (int64, int64) {
	if downloaded < 0 {
		downloaded = 0
	}
	if total < 0 {
		total = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if downloaded > h.downloaded {
		h.downloaded = downloaded
	}
	if total > h.total {
		h.total = total
	}
	if h.total > 0 && h.downloaded > h.total {
		h.total = h.downloaded
	}
	return h.downloaded, h.total
}

func (h *transferByteHighWater) snapshot() (int64, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.downloaded, h.total
}

func progressRangeForUnit(minimum, maximum, index, total int) transferProgressRange {
	if total <= 0 {
		return transferProgressRange{Min: minimum, Max: maximum}
	}
	if index < 0 {
		index = 0
	}
	if index >= total {
		index = total - 1
	}
	span := maximum - minimum
	return transferProgressRange{
		Min: minimum + (span*index)/total,
		Max: minimum + (span*(index+1))/total,
	}
}

func mapTransferProgress(progressRange transferProgressRange, rawPercent int) int {
	if progressRange.Max < progressRange.Min {
		progressRange.Max = progressRange.Min
	}
	if rawPercent < 0 {
		return progressRange.Min
	}
	if rawPercent > 100 {
		rawPercent = 100
	}
	return progressRange.Min +
		(rawPercent*(progressRange.Max-progressRange.Min))/100
}

// makeImagePullProgressCallback builds a progress callback that maps pull
// progress (0-100%) into the specified range and emits SSE events to the frontend.
func (m *AppManager) makeImagePullProgressCallback(
	ctx context.Context,
	instanceID string,
	svcName string,
	image string,
	progressRange transferProgressRange,
) func(container.ImagePullReport) {
	taskType, inheritedProgress := m.inheritedTaskProgress(
		ctx,
		taskTypeInstallApp,
		progressRange.Min,
	)
	if inheritedProgress > progressRange.Min {
		progressRange.Min = inheritedProgress
	}
	if progressRange.Max < progressRange.Min {
		progressRange.Max = progressRange.Min
	}
	var lifecycleProgress atomic.Int64
	lifecycleProgress.Store(int64(progressRange.Min))
	var reportedPercent atomic.Int64
	reportedPercent.Store(-1)
	var byteHighWater transferByteHighWater
	started := time.Now()

	// Strip @sha256:... digest from display name — it's noise in the UI.
	displayImage := image
	if idx := strings.Index(displayImage, "@sha256:"); idx > 0 {
		displayImage = displayImage[:idx]
	}

	return func(report container.ImagePullReport) {
		displayedDownloaded, displayedTotal := byteHighWater.observe(
			report.DownloadedBytes,
			report.TotalBytes,
		)
		rawPercent := report.OverallPercent
		if report.Phase == "complete" {
			rawPercent = 100
		}
		for {
			current := reportedPercent.Load()
			if int64(rawPercent) <= current ||
				reportedPercent.CompareAndSwap(current, int64(rawPercent)) {
				break
			}
		}

		mapped := mapTransferProgress(progressRange, report.OverallPercent)
		if report.Phase == "complete" {
			mapped = progressRange.Max
		}
		for {
			current := lifecycleProgress.Load()
			if int64(mapped) <= current ||
				lifecycleProgress.CompareAndSwap(current, int64(mapped)) {
				break
			}
		}
		progress := int(lifecycleProgress.Load())

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
		} else if displayedTotal > 0 {
			downloaded := formatBytes(displayedDownloaded)
			total := formatBytes(displayedTotal)
			message = fmt.Sprintf(
				"Pulling %s: %s / %s (%ds elapsed)",
				displayImage,
				downloaded,
				total,
				int64(time.Since(started)/time.Second),
			)
		} else {
			message = fmt.Sprintf(
				"Pulling image %s (%ds elapsed)",
				displayImage,
				int64(time.Since(started)/time.Second),
			)
		}

		m.emitProgressWithMetadata(
			ctx,
			taskType,
			instanceID,
			taskPhasePullingImage,
			progress,
			message,
			false,
			map[string]any{
				"service":          svcName,
				"image":            image,
				"phase":            report.Phase,
				"overall_percent":  reportedPercent.Load(),
				"total_bytes":      displayedTotal,
				"downloaded_bytes": displayedDownloaded,
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
	imgConfig, err := m.readImageConfigForGoldenRootfs(ctx, rootfs, handle.GoldenLV, imageDigest)
	if err != nil {
		log.Printf("WARN: rootfs %s: failed to read image config: %v", instanceID, err)
		imgConfig = persistence.GoldenImageConfig{}
	}

	return &rootfsMountInfo{
		handle:    handle,
		imgConfig: imgConfig,
	}, nil
}

// readImageConfigForGoldenRootfs prefers the actual golden LV recorded on the
// rootfs handle. Digest-derived candidates remain as a compatibility fallback
// for callers that predate collision-disambiguated golden identities.
func (m *AppManager) readImageConfigForGoldenRootfs(
	ctx context.Context,
	rootfs persistence.RootfsVolumeManager,
	goldenID,
	imageDigest string,
) (persistence.GoldenImageConfig, error) {
	canonical := canonicalImageDigestKey(imageDigest)
	candidates := make([]string, 0, 3)
	if strings.TrimSpace(goldenID) != "" {
		candidates = append(candidates, strings.TrimSpace(goldenID))
	}
	if canonical != "" {
		candidate := "golden-" + persistence.ShortDigest(canonical)
		if candidate != goldenID {
			candidates = append(candidates, candidate)
		}
	}
	trimmed := strings.TrimSpace(imageDigest)
	if trimmed != "" && trimmed != canonical {
		candidates = append(candidates, "golden-"+persistence.ShortDigest(trimmed))
	}
	if len(candidates) == 0 {
		return persistence.GoldenImageConfig{}, fmt.Errorf("image digest unavailable")
	}
	var lastErr error
	for _, goldenID := range candidates {
		cfg, err := rootfs.ReadGoldenImageConfig(ctx, goldenID)
		if err == nil {
			return cfg, nil
		}
		lastErr = err
	}
	return persistence.GoldenImageConfig{}, lastErr
}

func canonicalImageDigestKey(digest string) string {
	return strings.TrimSpace(extractDigestHash(strings.TrimSpace(digest)))
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
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	pr, pw := io.Pipe()

	// Build podman args with storage flags for the export command.
	exportArgs, err := container.BuildPodmanArgs(rt, []string{"export", cid})
	if err != nil {
		return fmt.Errorf("build export args: %w", err)
	}
	var exportStderr, tarStderr bytes.Buffer

	exportCmd := newRootfsExportCommand(ctx, rt, exportArgs, pw, &exportStderr)

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

	// This call is the sole Wait owner for the tar child; exportCmd.Run owns
	// the Podman child from start through wait in the sibling goroutine.
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

func newRootfsExportCommand(
	ctx context.Context,
	rt container.PodmanRuntime,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	container.ApplyRuntimeCredential(cmd, rt)
	return cmd
}
