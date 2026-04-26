package persistence

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

	// Application-volume auto-grow policy (D-5 / D-5a two-stage form).
	// Schedule a grow at 70% usage; wait up to 5 min for a ≥30s idle-write
	// window (via /proc/diskstats) before executing; if the volume stays
	// active or crosses 80%, grow immediately regardless. Protects ongoing
	// user uploads (immich mobile backup, nextcloud sync) from the 1–5s
	// write-stall caused by lvresize + cryptsetup resize + resize2fs.
	appEarlyScheduleThreshold = 0.70
	appHardGrowThreshold      = 0.80
	appIdleWindow             = 30 * time.Second
	appDeferBudget            = 5 * time.Minute

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

// appResizeSchedule tracks per-volume state for the two-stage auto-grow
// policy (D-5a). One entry per application volume that has crossed the
// early-schedule threshold (70%) but not yet the hard-grow threshold (80%).
type appResizeSchedule struct {
	scheduledAt      time.Time
	lastWritesPoll   uint64    // writes counter at last observation
	lastWritesChange time.Time // when the counter last moved
}

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
	// Walk on-disk metadata + filter by AttachStateOf (kernel-state truth).
	// Decoupled from any in-memory cache — survives the cross-cache-and-truth
	// drift that motivated volume-attach-truth.
	type wsEntry struct {
		volumeID     string
		rawMountPath string
		luksMapper   string
	}
	volIDs, err := listVolumeIDs()
	if err != nil {
		log.Printf("WARN: workspace auto-grow: list volumes: %v", err)
		return
	}
	var workspaces []wsEntry
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			continue
		}
		if meta.Type != volumeTypeWorkspace {
			continue
		}
		if !m.IsAttachedAdvisory(ctx, volID) {
			continue
		}
		// Workspace's writable raw mount is paths.MountDir(volID) (the
		// idmap bind, when present, layers on top — but statfs walks the
		// underlying btrfs from either mount).
		workspaces = append(workspaces, wsEntry{
			volumeID:     volID,
			rawMountPath: paths.MountDir(volID),
			luksMapper:   volMapperName(volID),
		})
	}

	for _, ws := range workspaces {
		m.maybeResizeWorkspace(ctx, ws.volumeID, ws.rawMountPath, ws.luksMapper)
	}
}

func (m *luksVolumeManager) maybeResizeWorkspace(ctx context.Context, volumeID, mountPath, luksMapper string) {
	// Check cooldown.
	m.mu.Lock()
	if lastResize, ok := m.volumeResizeCooldown[volumeID]; ok && time.Since(lastResize) < workspaceResizeCooldown {
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
	if meta.Type != volumeTypeWorkspace {
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

	// Auto-grow uses TryLock to avoid blocking the monitor loop on long
	// transitions (RollbackDataVolume, multi-step destroy). On contention,
	// skip this tick — the next tick re-evaluates fresh.
	acquired, err := m.tryResizeWorkspace(ctx, volumeID, newSize)
	if !acquired {
		log.Printf("INFO: workspace %s auto-resize deferred (per-volume lock held); will retry next tick", volumeID)
		return
	}
	if err != nil {
		log.Printf("ERROR: workspace auto-resize %s: %v", volumeID, err)
	}
}

// checkApplicationUsage walks per-app application volumes (type=service-data)
// and auto-grows any whose filesystem usage crosses the threshold. Uses the
// two-stage policy from D-5a (70% schedule, ≥30s idle-write window, 80%
// hard-grow, 5-min defer budget).
//
// Ordering (D-8): when multiple volumes cross threshold in a single tick,
// candidates are sorted by current usage ratio descending. The app closest
// to its own ceiling grows first — beats Go-map iteration randomness when
// pool budget is constrained.
func (m *luksVolumeManager) checkApplicationUsage(ctx context.Context) {
	// Walk on-disk metadata + filter by AttachStateOf (kernel-state truth).
	// Decoupled from any in-memory cache — was the user's bug: stale cache
	// after the post-unlock fan-out race made the volume invisible to
	// auto-grow despite being attached and full.
	volIDs, err := listVolumeIDs()
	if err != nil {
		log.Printf("WARN: app auto-grow: list volumes: %v", err)
		return
	}

	// Collect candidates + their usage ratios, filtered to service-data only.
	type candidate struct {
		volumeID   string
		usageRatio float64
	}
	var candidates []candidate
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil || meta.Type != volumeTypeServiceData {
			continue
		}
		if !m.IsAttachedAdvisory(ctx, volID) {
			continue
		}
		mountPath := paths.MountDir(volID)
		var st unix.Statfs_t
		if err := unix.Statfs(mountPath, &st); err != nil {
			continue
		}
		totalBytes := int64(st.Blocks) * int64(st.Bsize)
		if totalBytes == 0 {
			continue
		}
		freeBytes := int64(st.Bavail) * int64(st.Bsize)
		usage := float64(totalBytes-freeBytes) / float64(totalBytes)
		candidates = append(candidates, candidate{volumeID: volID, usageRatio: usage})
	}

	// Sort: highest usage first. Apps closest to their own ceiling get
	// first shot at scarce pool headroom.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].usageRatio > candidates[j].usageRatio
	})

	for _, c := range candidates {
		m.maybeResizeApplication(ctx, c.volumeID)
	}
}

func (m *luksVolumeManager) maybeResizeApplication(ctx context.Context, volumeID string) {
	// Cooldown shared with workspace path.
	m.mu.Lock()
	if lastResize, ok := m.volumeResizeCooldown[volumeID]; ok && time.Since(lastResize) < workspaceResizeCooldown {
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
	if meta.Type != volumeTypeServiceData {
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

	// Below the early-schedule threshold: clear any pending schedule.
	if usageRatio < appEarlyScheduleThreshold {
		m.clearAppResizeSchedule(volumeID)
		return
	}

	// Hard threshold: grow immediately regardless of write activity or
	// scheduled defer state. Prevents unbounded deferral and matches the
	// workspace behavior for the hot path.
	if usageRatio >= appHardGrowThreshold {
		m.performApplicationGrow(ctx, volumeID, meta, usedBytes, totalBytes, usageRatio, "hard-threshold")
		return
	}

	// Between thresholds: two-stage schedule + idle-window defer.
	writes, haveWrites := readMapperWriteCounter(volMapperName(volumeID))
	now := time.Now()

	m.mu.Lock()
	sched, existed := m.appResizeSchedules[volumeID]
	if !existed {
		sched = &appResizeSchedule{
			scheduledAt:      now,
			lastWritesPoll:   writes,
			lastWritesChange: now,
		}
		m.appResizeSchedules[volumeID] = sched
		m.mu.Unlock()
		log.Printf("INFO: application volume %s at %.0f%% usage, scheduled for grow (waiting for idle-write window)",
			volumeID, usageRatio*100)
		return
	}

	// Defer budget exhausted: grow even though writes are still active.
	if now.Sub(sched.scheduledAt) > appDeferBudget {
		delete(m.appResizeSchedules, volumeID)
		m.mu.Unlock()
		m.performApplicationGrow(ctx, volumeID, meta, usedBytes, totalBytes, usageRatio, "defer-budget-exhausted")
		return
	}

	// If we couldn't measure writes, fall back to scheduledAt-based timing:
	// grow after 30s of "above threshold" state.
	if !haveWrites {
		if now.Sub(sched.scheduledAt) >= appIdleWindow {
			delete(m.appResizeSchedules, volumeID)
			m.mu.Unlock()
			m.performApplicationGrow(ctx, volumeID, meta, usedBytes, totalBytes, usageRatio, "idle-probe-unavailable")
			return
		}
		m.mu.Unlock()
		return
	}

	// Writes changed: reset the idle-clock but keep the defer budget counting.
	if writes != sched.lastWritesPoll {
		sched.lastWritesPoll = writes
		sched.lastWritesChange = now
		m.mu.Unlock()
		return
	}

	// Writes unchanged since lastWritesChange: idle window accumulates.
	if now.Sub(sched.lastWritesChange) >= appIdleWindow {
		delete(m.appResizeSchedules, volumeID)
		m.mu.Unlock()
		m.performApplicationGrow(ctx, volumeID, meta, usedBytes, totalBytes, usageRatio, "idle-window-met")
		return
	}
	m.mu.Unlock()
}

// clearAppResizeSchedule removes any pending schedule entry for volumeID.
func (m *luksVolumeManager) clearAppResizeSchedule(volumeID string) {
	m.mu.Lock()
	delete(m.appResizeSchedules, volumeID)
	m.mu.Unlock()
}

// performApplicationGrow computes the new size, checks per-grow pool
// admission (D-8), and invokes ResizeApplication. Factored out of
// maybeResizeApplication so all defer-resolution branches share a single
// growth-size + logging + admission path.
func (m *luksVolumeManager) performApplicationGrow(ctx context.Context, volumeID string, meta *volumeMetaV3, usedBytes, totalBytes int64, usageRatio float64, reason string) {
	newSize := meta.SizeBytes * workspaceResizeGrowFactor
	if newSize > applicationMaxVirtualSize {
		if meta.SizeBytes >= applicationMaxVirtualSize {
			log.Printf("WARN: application volume %s at %.0f%% usage, already at max virtual size", volumeID, usageRatio*100)
			return
		}
		newSize = applicationMaxVirtualSize
	}

	// Per-grow pool admission (D-8): refuse when the projected post-grow
	// pool usage would exceed 95%. Conservative: assumes the growth delta
	// could be fully consumed (worst case). In practice thin provisioning
	// only consumes on write, so this errs on the side of leaving headroom
	// for siblings rather than letting one app drain the pool.
	if m.poolMgr != nil {
		poolStats, err := m.poolMgr.PoolStatus(ctx)
		if err == nil && poolStats.TotalDataBytes > 0 {
			delta := newSize - meta.SizeBytes
			projectedUsed := poolStats.UsedDataBytes + delta
			projectedPct := float64(projectedUsed) / float64(poolStats.TotalDataBytes) * 100.0
			if projectedPct > 95.0 {
				log.Printf("WARN: application volume %s grow refused (trigger: %s): projected pool usage %.1f%% > 95%% (current %.1f%% + %d bytes delta)",
					volumeID, reason, projectedPct, poolStats.DataPercent, delta)
				return
			}
		}
	}

	log.Printf("INFO: application volume %s at %.0f%% usage (%d/%d bytes), resizing from %d to %d (trigger: %s)",
		volumeID, usageRatio*100, usedBytes, totalBytes, meta.SizeBytes, newSize, reason)
	acquired, err := m.tryResizeApplication(ctx, volumeID, newSize)
	if !acquired {
		log.Printf("INFO: application volume %s auto-resize deferred (per-volume lock held); will retry next tick", volumeID)
		return
	}
	if err != nil {
		log.Printf("ERROR: application volume auto-resize %s: %v", volumeID, err)
	}
}

// readMapperWriteCounter resolves /dev/mapper/<name> to its dm device and
// reads the writes-completed field from /sys/block/<dev>/stat. Returns
// (counter, true) on success; (0, false) on any error (device renamed,
// mapper unavailable, stat file unreadable). Callers that can't measure
// idle fall back to a scheduledAt-based timer.
func readMapperWriteCounter(mapperName string) (uint64, bool) {
	// /dev/mapper/<name> is a symlink to /dev/dm-N. Readlink resolves it.
	link, err := os.Readlink("/dev/mapper/" + mapperName)
	if err != nil {
		return 0, false
	}
	// link is typically "../dm-N" (relative). Basename gives "dm-N".
	devName := filepath.Base(link)
	if !strings.HasPrefix(devName, "dm-") {
		return 0, false
	}
	statPath := fmt.Sprintf("/sys/block/%s/stat", devName)
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	// /sys/block/*/stat fields: rdIOs rdMerges rdSectors rdTicks wrIOs ...
	// Field 5 (1-indexed) is writes_completed.
	fields := strings.Fields(string(data))
	if len(fields) < 5 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[4], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
