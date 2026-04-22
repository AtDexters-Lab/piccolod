package persistence

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"piccolod/internal/state/paths"
)

const (
	workspaceResizeCheckInterval = 30 * time.Second
	workspaceResizeThreshold     = 0.80  // resize at 80% usage
	workspaceResizeGrowFactor    = 2     // double the virtual size
	workspaceMaxVirtualSize      = 500 << 30 // 500 GiB cap
	workspaceResizeCooldown      = 5 * time.Minute

	// Application-volume auto-grow policy (D-5 / D-5a simplified form).
	// The initial implementation mirrors the workspace 80% threshold;
	// the two-stage schedule-and-defer variant (70% + idle-window) is a
	// follow-up (P5.3a).
	applicationResizeThreshold = 0.80
	// applicationMaxVirtualSize is a global ceiling. Per-app storage.max
	// (D-5 "hidden/advanced override") is NOT wired through yet —
	// doing so requires adding a Max field to volumeMetaV3 and writing
	// it from manifest at provisioning time. Tracked as a P5.3 follow-up.
	// For today, all application volumes share this 500 GiB ceiling,
	// which matches the plan's default and is the safer-for-shared-pool
	// bound on consumer-hardware (pool_total × 0.4 on typical 256-512 GB
	// SSDs is ≤ 200 GiB anyway).
	applicationMaxVirtualSize = 500 << 30 // 500 GiB
)

// StartWorkspaceResizeMonitor launches a background goroutine that monitors
// mounted workspace volumes and auto-resizes them when usage exceeds the threshold.
// Only operates in single-node mode (no NBD/DRBD).
func (m *luksVolumeManager) StartWorkspaceResizeMonitor() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.wsResizeCancel != nil {
		return // already running
	}
	// Only operate in single-node mode — online resize through a replicated
	// DRBD stack requires peer coordination.
	if m.nbdSrv != nil || m.drbdMgr != nil {
		log.Printf("INFO: workspace resize monitor skipped (multi-node mode)")
		return
	}
	if m.lvMgr == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.wsResizeCancel = cancel
	go m.workspaceResizeLoop(ctx)
	log.Printf("INFO: workspace resize monitor started (interval=%s, threshold=%.0f%%)", workspaceResizeCheckInterval, workspaceResizeThreshold*100)
}

// StopWorkspaceResizeMonitor stops the background resize monitor.
func (m *luksVolumeManager) StopWorkspaceResizeMonitor() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.wsResizeCancel != nil {
		m.wsResizeCancel()
		m.wsResizeCancel = nil
	}
}

func (m *luksVolumeManager) workspaceResizeLoop(ctx context.Context) {
	ticker := time.NewTicker(workspaceResizeCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkWorkspaceUsage(ctx)
			m.checkApplicationUsage(ctx)
		}
	}
}

func (m *luksVolumeManager) checkWorkspaceUsage(ctx context.Context) {
	// Snapshot rootfsMounts under lock, then release for I/O.
	m.mu.Lock()
	type wsEntry struct {
		volumeID     string
		rawMountPath string
		luksMapper   string
	}
	var workspaces []wsEntry
	for volID, state := range m.rootfsMounts {
		if state.handle.ReadOnly {
			continue // service rootfs, not workspace
		}
		if state.rawMountPath == "" {
			continue
		}
		workspaces = append(workspaces, wsEntry{
			volumeID:     volID,
			rawMountPath: state.rawMountPath,
			luksMapper:   state.luksMapper,
		})
	}
	m.mu.Unlock()

	for _, ws := range workspaces {
		m.maybeResizeWorkspace(ctx, ws.volumeID, ws.rawMountPath, ws.luksMapper)
	}
}

func (m *luksVolumeManager) maybeResizeWorkspace(ctx context.Context, volumeID, mountPath, luksMapper string) {
	// Check cooldown.
	m.mu.Lock()
	if lastResize, ok := m.wsResizeCooldown[volumeID]; ok && time.Since(lastResize) < workspaceResizeCooldown {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// Check filesystem usage.
	var st unix.Statfs_t
	if err := unix.Statfs(mountPath, &st); err != nil {
		return // mount may have been removed between snapshot and check
	}
	totalBytes := int64(st.Blocks) * int64(st.Bsize)
	freeBytes := int64(st.Bavail) * int64(st.Bsize)
	if totalBytes == 0 {
		return
	}
	usedBytes := totalBytes - freeBytes
	usageRatio := float64(usedBytes) / float64(totalBytes)
	if usageRatio < workspaceResizeThreshold {
		return
	}

	// Read metadata to compute target size.
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		log.Printf("WARN: workspace resize: read metadata %s: %v", volumeID, err)
		return
	}
	if meta.Type != "workspace" {
		return
	}

	newSize := meta.SizeBytes * workspaceResizeGrowFactor
	if newSize > workspaceMaxVirtualSize {
		if meta.SizeBytes >= workspaceMaxVirtualSize {
			log.Printf("WARN: workspace %s at %.0f%% usage, already at max virtual size", volumeID, usageRatio*100)
			return
		}
		newSize = workspaceMaxVirtualSize
	}

	log.Printf("INFO: workspace %s at %.0f%% usage (%d/%d bytes), resizing from %d to %d",
		volumeID, usageRatio*100, usedBytes, totalBytes, meta.SizeBytes, newSize)

	// Delegate the actual resize (LV + LUKS + btrfs + metadata + cooldown) to ResizeWorkspace.
	if err := m.ResizeWorkspace(ctx, volumeID, newSize); err != nil {
		log.Printf("ERROR: workspace auto-resize %s: %v", volumeID, err)
	}
}

// checkApplicationUsage walks per-app application volumes (type=service-data)
// and auto-grows any whose filesystem usage crosses the threshold. Mirrors
// checkWorkspaceUsage but iterates m.stacks (application volumes are not
// tracked in rootfsMounts).
//
// See plan P5.1, P5.2, P5.3. Two-stage auto-grow (P5.3a) and per-grow
// pool admission (P5.3b) are follow-ups; this first pass preserves the
// proven workspace behaviour (80% single-threshold, thin-pool capacity
// gate at ResizeApplication level).
func (m *luksVolumeManager) checkApplicationUsage(ctx context.Context) {
	m.mu.Lock()
	var appVols []string
	for volID := range m.stacks {
		appVols = append(appVols, volID)
	}
	m.mu.Unlock()

	for _, volID := range appVols {
		m.maybeResizeApplication(ctx, volID)
	}
}

func (m *luksVolumeManager) maybeResizeApplication(ctx context.Context, volumeID string) {
	// Cooldown shared with workspace path.
	m.mu.Lock()
	if lastResize, ok := m.wsResizeCooldown[volumeID]; ok && time.Since(lastResize) < workspaceResizeCooldown {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// Read metadata to filter by type + compute target size.
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return // not all stacks have v3 metadata (golden LVs etc.); skip silently
	}
	if meta.Type != "service-data" {
		return
	}

	mountPath := paths.MountDir(volumeID)
	var st unix.Statfs_t
	if err := unix.Statfs(mountPath, &st); err != nil {
		return
	}
	totalBytes := int64(st.Blocks) * int64(st.Bsize)
	freeBytes := int64(st.Bavail) * int64(st.Bsize)
	if totalBytes == 0 {
		return
	}
	usedBytes := totalBytes - freeBytes
	usageRatio := float64(usedBytes) / float64(totalBytes)
	if usageRatio < applicationResizeThreshold {
		return
	}

	newSize := meta.SizeBytes * workspaceResizeGrowFactor
	if newSize > applicationMaxVirtualSize {
		if meta.SizeBytes >= applicationMaxVirtualSize {
			log.Printf("WARN: application volume %s at %.0f%% usage, already at max virtual size", volumeID, usageRatio*100)
			return
		}
		newSize = applicationMaxVirtualSize
	}

	log.Printf("INFO: application volume %s at %.0f%% usage (%d/%d bytes), resizing from %d to %d",
		volumeID, usageRatio*100, usedBytes, totalBytes, meta.SizeBytes, newSize)

	if err := m.ResizeApplication(ctx, volumeID, newSize); err != nil {
		log.Printf("ERROR: application volume auto-resize %s: %v", volumeID, err)
	}
}
