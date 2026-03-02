package persistence

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/blockdev"
	"piccolod/internal/storage/lvm"
)

const (
	goldenLVPrefix       = "golden-"
	workspaceLVPrefix    = "ws-"
	svcRootfsLVPrefix    = "svc-rootfs-"
	flattenSentinelFile  = ".piccolo_flatten_incomplete"
	imageConfigFile      = "image-config.json"
	defaultGoldenLVSize  = 10 << 30 // 10 GiB

	btrfsRootfsMountOpts = "compress=zstd:1,discard=async,noatime"

	// svcRootfsDelimiter separates instanceID from serviceName in per-service
	// rootfs volume IDs. Service names (YAML map keys) don't contain "--".
	svcRootfsDelimiter = "--"
)

// ServiceRootfsVolumeID returns the volume ID for a service rootfs.
// When serviceName is empty, returns the legacy single-rootfs ID.
func ServiceRootfsVolumeID(instanceID, serviceName string) string {
	if serviceName == "" {
		return svcRootfsLVPrefix + instanceID
	}
	return svcRootfsLVPrefix + instanceID + svcRootfsDelimiter + serviceName
}

// VersionedServiceRootfsVolumeID returns a digest-qualified volume ID.
// Used during image updates to create rootfs alongside the original (RFC 20260302).
func VersionedServiceRootfsVolumeID(instanceID, serviceName, shortDigest string) string {
	base := ServiceRootfsVolumeID(instanceID, serviceName)
	return base + svcRootfsDelimiter + shortDigest
}

// goldenLVSizeForImage returns the right-sized LV allocation for a given
// uncompressed image size. Uses max(1.5x, image + 1 GiB) with a 256 MiB
// floor (btrfs minimum for DUP metadata is ~109 MiB).
func goldenLVSizeForImage(imageSizeBytes int64) int64 {
	if imageSizeBytes <= 0 {
		return defaultGoldenLVSize
	}
	oneGiB := int64(1 << 30)
	minSize := int64(256 << 20)
	sizeA := imageSizeBytes + imageSizeBytes/2 // 1.5x
	sizeB := imageSizeBytes + oneGiB           // + 1 GiB
	result := sizeA
	if sizeB > result {
		result = sizeB
	}
	if result < minSize {
		result = minSize
	}
	return result
}

// ShortDigest returns the first 12 hex chars of the SHA-256 of a digest string.
// Used for golden LV naming.
func ShortDigest(imageDigest string) string {
	h := sha256.Sum256([]byte(imageDigest))
	return hex.EncodeToString(h[:6])
}

// goldenMutex returns the per-image-digest mutex, creating one if needed.
func (m *luksVolumeManager) goldenMutex(digestShort string) *sync.Mutex {
	m.goldenMuLock.Lock()
	defer m.goldenMuLock.Unlock()
	mu, ok := m.goldenMu[digestShort]
	if !ok {
		mu = &sync.Mutex{}
		m.goldenMu[digestShort] = mu
	}
	return mu
}

// checkThinPoolCapacity ensures the thin pool has sufficient free space.
func (m *luksVolumeManager) checkThinPoolCapacity(ctx context.Context) error {
	if m.poolMgr == nil {
		return nil
	}
	stats, err := m.poolMgr.PoolStatus(ctx)
	if err != nil {
		return fmt.Errorf("check thin pool: %w", err)
	}
	if stats.DataPercent > 85 {
		return fmt.Errorf("thin pool data usage %.1f%% exceeds 85%% threshold", stats.DataPercent)
	}
	if stats.MetadataPercent > 75 {
		return fmt.Errorf("thin pool metadata usage %.1f%% exceeds 75%% threshold", stats.MetadataPercent)
	}
	return nil
}

// EnsureGoldenLV creates or reuses a golden LV for the given image.
func (m *luksVolumeManager) EnsureGoldenLV(ctx context.Context, req GoldenLVRequest) (string, error) {
	digestShort := ShortDigest(req.ImageDigest)
	goldenID := goldenLVPrefix + digestShort

	// Per-image-digest lock (avoids holding global mu during 30s+ flatten).
	mu := m.goldenMutex(digestShort)
	mu.Lock()
	defer mu.Unlock()

	// Fast path: cached + flatten complete.
	m.mu.Lock()
	if cached, ok := m.goldenLVs[digestShort]; ok && cached.FlattenComplete != "" {
		m.mu.Unlock()
		return goldenID, nil
	}
	m.mu.Unlock()

	metaDir := paths.VolumeMetaDir(goldenID)
	metaPath := filepath.Join(metaDir, metadataV2File)

	// Check disk: metadata exists and no sentinel → return.
	if meta, err := readVolumeMetaV3(metaPath); err == nil {
		if meta.FlattenComplete != "" {
			m.mu.Lock()
			m.goldenLVs[digestShort] = meta
			m.mu.Unlock()
			return goldenID, nil
		}
		// Sentinel or incomplete — destroy and recreate.
		log.Printf("golden LV %s incomplete (no flatten_complete), recreating", goldenID)
		m.destroyGoldenLVUnsafe(ctx, goldenID)
	}

	if m.flattenFn == nil {
		return "", fmt.Errorf("flattenFn not configured")
	}

	// Check thin pool capacity.
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return "", err
	}

	lvName := goldenID
	sizeBytes := int64(defaultGoldenLVSize)
	if m.imageSizeFn != nil {
		if imgSize, err := m.imageSizeFn(ctx, req.ImageRef); err == nil && imgSize > 0 {
			sizeBytes = goldenLVSizeForImage(imgSize)
		} else if err != nil {
			log.Printf("WARN: imageSizeFn failed for %s, using default LV size: %v", req.ImageRef, err)
		} else if imgSize <= 0 {
			log.Printf("WARN: imageSizeFn returned non-positive size %d for %s, using default LV size", imgSize, req.ImageRef)
		}
	}

	mapper := "piccolo-vol-" + goldenID
	mountDir := paths.MountDir(goldenID)

	// Track which layers have been set up for deferred cleanup.
	var (
		lvCreated  bool
		luksOpened bool
		mounted    bool
		success    bool
	)
	defer func() {
		// Use a detached context for cleanup — the caller's ctx may be cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Always tear down the transient mount stack — golden LV is only
		// activated during flatten, then deactivated.
		if mounted {
			m.run.Run(cleanupCtx, "umount", mountDir)
		}
		if luksOpened {
			m.run.Run(cleanupCtx, "cryptsetup", "close", mapper)
		}
		if lvCreated {
			m.lvMgr.DeactivateLV(cleanupCtx, lvName)
			if !success {
				m.lvMgr.RemoveThinLV(cleanupCtx, lvName)
			}
		}
	}()

	// Ensure clean slate — previous crashed run may have left active mounts/mappings.
	if m.lvMgr.LVExists(ctx, lvName) {
		m.destroyGoldenLVUnsafe(ctx, goldenID)
	}
	if err := m.lvMgr.CreateThinLV(ctx, lvName, sizeBytes); err != nil {
		return "", fmt.Errorf("create golden LV: %w", err)
	}
	lvCreated = true
	if err := m.lvMgr.ActivateLV(ctx, lvName); err != nil {
		return "", fmt.Errorf("activate golden LV: %w", err)
	}

	lvPath := m.lvMgr.LVPath(lvName)

	// LUKS format + open.
	if err := m.luksFormatWithMasterKey(ctx, lvPath); err != nil {
		return "", fmt.Errorf("luks format golden: %w", err)
	}
	if err := m.luksOpenWithPoolKeyfile(ctx, lvPath, mapper); err != nil {
		return "", fmt.Errorf("luks open golden: %w", err)
	}
	luksOpened = true

	luksPath := "/dev/mapper/" + mapper

	// mkfs.btrfs.
	if err := m.run.Run(ctx, "mkfs.btrfs", "-f", luksPath); err != nil {
		return "", fmt.Errorf("mkfs golden: %w", err)
	}

	// Mount.
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir golden mount: %w", err)
	}
	if err := m.run.Run(ctx, "mount", "-t", "btrfs", "-o", btrfsRootfsMountOpts, luksPath, mountDir); err != nil {
		return "", fmt.Errorf("mount golden: %w", err)
	}
	mounted = true

	// Write sentinel.
	sentinelPath := filepath.Join(mountDir, flattenSentinelFile)
	if err := os.WriteFile(sentinelPath, []byte("incomplete"), 0o600); err != nil {
		return "", fmt.Errorf("write sentinel: %w", err)
	}

	// Flatten: extract OCI image to mount point and get image config.
	imgConfig, err := m.flattenFn(ctx, req.ImageRef, mountDir)
	if err != nil {
		return "", fmt.Errorf("flatten image: %w", err)
	}

	// syncfs: ensure all flattened data is durable on disk before marking complete.
	if err := syncfsPath(mountDir); err != nil {
		return "", fmt.Errorf("syncfs golden: %w", err)
	}

	// Ensure metadata dir exists.
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir meta: %w", err)
	}

	// Write image config alongside golden LV metadata.
	imgConfigData, err := json.Marshal(imgConfig)
	if err != nil {
		return "", fmt.Errorf("marshal image config: %w", err)
	}
	configPath := filepath.Join(metaDir, imageConfigFile)
	if err := fsutil.AtomicWriteFile(configPath, imgConfigData, 0o600); err != nil {
		return "", fmt.Errorf("write image config: %w", err)
	}

	// Write v3 metadata WITH flatten_complete — atomic commit point.
	// On crash before this write: metadata absent/incomplete → reconcile destroys.
	// On crash after this write: FlattenComplete set → reconcile caches (correct).
	meta := &volumeMetaV3{
		Version:         metadataV3Version,
		Type:            "golden",
		LVName:          lvName,
		VGName:          lvm.DefaultVGName,
		SizeBytes:       sizeBytes,
		FSType:          "btrfs",
		BaseImageDigest: req.ImageDigest,
		BaseImageRef:    req.ImageRef,
		FlattenComplete: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		return "", fmt.Errorf("write golden metadata: %w", err)
	}

	// Remove sentinel (cleanup only — reconcile checks FlattenComplete, not sentinel).
	_ = os.Remove(sentinelPath)

	// Mark success so deferred cleanup preserves the LV.
	success = true

	// Cache.
	m.mu.Lock()
	m.goldenLVs[digestShort] = meta
	m.mu.Unlock()

	log.Printf("golden LV created: %s (image=%s, digest=%s, size=%d)", goldenID, req.ImageRef, req.ImageDigest, sizeBytes)
	return goldenID, nil
}

// CreateWorkspaceFromGolden creates a workspace rootfs from a golden LV snapshot.
func (m *luksVolumeManager) CreateWorkspaceFromGolden(ctx context.Context, req WorkspaceRootfsRequest) (RootfsHandle, error) {
	goldenID, err := m.EnsureGoldenLV(ctx, GoldenLVRequest{
		ImageDigest: req.ImageDigest,
		ImageRef:    req.ImageRef,
	})
	if err != nil {
		return RootfsHandle{}, err
	}

	volumeID := workspaceLVPrefix + req.InstanceID
	return m.createRootfsFromGolden(ctx, goldenID, volumeID, "workspace", false, &req.IDMap)
}

// CreateServiceRootfs creates a read-only service rootfs from a golden LV snapshot.
func (m *luksVolumeManager) CreateServiceRootfs(ctx context.Context, req ServiceRootfsRequest) (RootfsHandle, error) {
	goldenID, err := m.EnsureGoldenLV(ctx, GoldenLVRequest{
		ImageDigest: req.ImageDigest,
		ImageRef:    req.ImageRef,
	})
	if err != nil {
		return RootfsHandle{}, err
	}

	volumeID := req.VolumeID
	if volumeID == "" {
		volumeID = ServiceRootfsVolumeID(req.InstanceID, req.ServiceName)
	}
	return m.createRootfsFromGolden(ctx, goldenID, volumeID, "service-rootfs", true, &req.IDMap)
}

// createRootfsFromGolden creates a rootfs from a golden LV via snapshot.
func (m *luksVolumeManager) createRootfsFromGolden(ctx context.Context, goldenID, volumeID, volType string, readOnly bool, idmap *IDMapConfig) (RootfsHandle, error) {
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return RootfsHandle{}, err
	}

	// Read golden LV metadata.
	goldenMetaPath := filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File)
	goldenMeta, err := readVolumeMetaV3(goldenMetaPath)
	if err != nil {
		return RootfsHandle{}, fmt.Errorf("read golden metadata: %w", err)
	}

	snapshotName := volumeID
	if err := m.lvMgr.CreateSnapshot(ctx, goldenID, snapshotName); err != nil {
		return RootfsHandle{}, fmt.Errorf("create snapshot: %w", err)
	}

	if err := m.lvMgr.ActivateLV(ctx, snapshotName); err != nil {
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		return RootfsHandle{}, fmt.Errorf("activate snapshot: %w", err)
	}

	// Change LUKS UUID to avoid collision with golden LV.
	lvPath := m.lvMgr.LVPath(snapshotName)
	if err := m.run.Run(ctx, "cryptsetup", "luksUUID", "--batch-mode", "--uuid",
		newUUID(), lvPath); err != nil {
		m.lvMgr.DeactivateLV(ctx, snapshotName)
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		return RootfsHandle{}, fmt.Errorf("set LUKS UUID: %w", err)
	}

	// Write v3 metadata.
	metaDir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		m.lvMgr.DeactivateLV(ctx, snapshotName)
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		return RootfsHandle{}, fmt.Errorf("mkdir meta: %w", err)
	}

	meta := &volumeMetaV3{
		Version:         metadataV3Version,
		Type:            volType,
		LVName:          snapshotName,
		VGName:          lvm.DefaultVGName,
		SizeBytes:       goldenMeta.SizeBytes,
		FSType:          "btrfs",
		ReadOnly:        readOnly,
		BaseImageDigest: goldenMeta.BaseImageDigest,
		BaseImageRef:    goldenMeta.BaseImageRef,
		GoldenLV:        goldenID,
	}
	if idmap != nil {
		meta.IDMap = &IDMapMeta{
			AppUID:      idmap.AppUID,
			AppGID:      idmap.AppGID,
			SubUIDStart: idmap.SubUIDStart,
			SubUIDCount: idmap.SubUIDCount,
			SubGIDStart: idmap.SubGIDStart,
			SubGIDCount: idmap.SubGIDCount,
		}
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		m.lvMgr.DeactivateLV(ctx, snapshotName)
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		return RootfsHandle{}, fmt.Errorf("write metadata: %w", err)
	}

	// Attach the rootfs.
	handle, err := m.attachRootfsFromMeta(ctx, volumeID, meta)
	if err != nil {
		m.lvMgr.DeactivateLV(ctx, snapshotName)
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		_ = os.RemoveAll(metaDir)
		return RootfsHandle{}, err
	}

	return handle, nil
}

// CloneWorkspace creates a clone of an existing workspace.
// When idmap is non-nil, it overrides the origin's IDMap in the clone metadata.
func (m *luksVolumeManager) CloneWorkspace(ctx context.Context, originID, cloneID string, idmap *IDMapConfig) (RootfsHandle, error) {
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return RootfsHandle{}, err
	}

	originVolumeID := workspaceLVPrefix + originID
	cloneVolumeID := workspaceLVPrefix + cloneID

	// Read origin metadata.
	originMetaPath := filepath.Join(paths.VolumeMetaDir(originVolumeID), metadataV2File)
	originMeta, err := readVolumeMetaV3(originMetaPath)
	if err != nil {
		return RootfsHandle{}, fmt.Errorf("read origin metadata: %w", err)
	}

	if err := m.lvMgr.CreateSnapshot(ctx, originVolumeID, cloneVolumeID); err != nil {
		return RootfsHandle{}, fmt.Errorf("create clone snapshot: %w", err)
	}

	if err := m.lvMgr.ActivateLV(ctx, cloneVolumeID); err != nil {
		m.lvMgr.RemoveThinLV(ctx, cloneVolumeID)
		return RootfsHandle{}, fmt.Errorf("activate clone: %w", err)
	}

	lvPath := m.lvMgr.LVPath(cloneVolumeID)
	if err := m.run.Run(ctx, "cryptsetup", "luksUUID", "--batch-mode", "--uuid",
		newUUID(), lvPath); err != nil {
		m.lvMgr.DeactivateLV(ctx, cloneVolumeID)
		m.lvMgr.RemoveThinLV(ctx, cloneVolumeID)
		return RootfsHandle{}, fmt.Errorf("set clone LUKS UUID: %w", err)
	}

	metaDir := paths.VolumeMetaDir(cloneVolumeID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		m.lvMgr.DeactivateLV(ctx, cloneVolumeID)
		m.lvMgr.RemoveThinLV(ctx, cloneVolumeID)
		return RootfsHandle{}, fmt.Errorf("mkdir clone meta: %w", err)
	}

	meta := &volumeMetaV3{
		Version:         metadataV3Version,
		Type:            "workspace",
		LVName:          cloneVolumeID,
		VGName:          lvm.DefaultVGName,
		SizeBytes:       originMeta.SizeBytes,
		FSType:          "btrfs",
		BaseImageDigest: originMeta.BaseImageDigest,
		BaseImageRef:    originMeta.BaseImageRef,
		GoldenLV:        originMeta.GoldenLV,
		CloneOf:         originVolumeID,
		IDMap:           originMeta.IDMap,
	}
	// Override IDMap when the clone belongs to a different per-app user.
	if idmap != nil {
		meta.IDMap = &IDMapMeta{
			AppUID:      idmap.AppUID,
			AppGID:      idmap.AppGID,
			SubUIDStart: idmap.SubUIDStart,
			SubUIDCount: idmap.SubUIDCount,
			SubGIDStart: idmap.SubGIDStart,
			SubGIDCount: idmap.SubGIDCount,
		}
	}
	metaPath := filepath.Join(metaDir, metadataV2File)
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		m.lvMgr.DeactivateLV(ctx, cloneVolumeID)
		m.lvMgr.RemoveThinLV(ctx, cloneVolumeID)
		return RootfsHandle{}, fmt.Errorf("write clone metadata: %w", err)
	}

	handle, err := m.attachRootfsFromMeta(ctx, cloneVolumeID, meta)
	if err != nil {
		m.lvMgr.DeactivateLV(ctx, cloneVolumeID)
		m.lvMgr.RemoveThinLV(ctx, cloneVolumeID)
		_ = os.RemoveAll(metaDir)
		return RootfsHandle{}, err
	}

	return handle, nil
}

// ListClones returns volume IDs of clones created from the given origin volume.
func (m *luksVolumeManager) ListClones(ctx context.Context, originVolumeID string) ([]string, error) {
	metaBase := paths.CoreJoin("volumes")
	entries, err := os.ReadDir(metaBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read volumes dir: %w", err)
	}

	var clones []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volID := e.Name()
		metaPath := filepath.Join(metaBase, volID, metadataV2File)
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			continue
		}
		if meta.CloneOf == originVolumeID {
			clones = append(clones, volID)
		}
	}
	return clones, nil
}

// AttachRootfs activates and mounts an existing rootfs volume.
func (m *luksVolumeManager) AttachRootfs(ctx context.Context, volumeID string) (RootfsHandle, error) {
	// Check if already mounted.
	m.mu.Lock()
	if state, ok := m.rootfsMounts[volumeID]; ok {
		m.mu.Unlock()
		mountPath := state.idmapPath
		if mountPath == "" {
			mountPath = state.mountPath
		}
		return RootfsHandle{VolumeID: volumeID, MountPath: mountPath, GoldenLV: state.goldenLV}, nil
	}
	m.mu.Unlock()

	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return RootfsHandle{}, fmt.Errorf("read rootfs metadata: %w", err)
	}

	return m.attachRootfsFromMeta(ctx, volumeID, meta)
}

// attachRootfsFromMeta performs the full attach sequence for a rootfs volume.
func (m *luksVolumeManager) attachRootfsFromMeta(ctx context.Context, volumeID string, meta *volumeMetaV3) (RootfsHandle, error) {
	lvName := meta.LVName
	mapper := "piccolo-vol-" + volumeID

	// Build below-LUKS device stack.
	var stack *blockdev.DeviceStack
	var err error

	switch meta.Type {
	case "workspace":
		// Workspace: ThinLV → NBD → DRBD
		stack, err = m.buildStack(volumeID, lvName, meta.SizeBytes)
	case "service-rootfs":
		// Service rootfs: ThinLV only (not replicated)
		thinDev := blockdev.NewThinLVDevice(m.lvMgr, lvName, meta.SizeBytes)
		stack, err = blockdev.NewDeviceStack(volumeID, thinDev)
	default:
		return RootfsHandle{}, fmt.Errorf("cannot attach rootfs type %q", meta.Type)
	}
	if err != nil {
		return RootfsHandle{}, fmt.Errorf("build stack: %w", err)
	}

	if err := stack.Open(ctx); err != nil {
		return RootfsHandle{}, fmt.Errorf("open stack: %w", err)
	}

	// Rollback helper.
	rollback := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stack.Close(rctx)
	}

	// LUKS open.
	topDev := stack.Top().Path()
	if err := m.luksOpenWithPoolKeyfile(ctx, topDev, mapper); err != nil {
		rollback()
		return RootfsHandle{}, fmt.Errorf("luks open: %w", err)
	}

	luksPath := "/dev/mapper/" + mapper

	// Mount btrfs.
	mountDir := paths.MountDir(volumeID)
	if err := os.MkdirAll(filepath.Dir(mountDir), 0o711); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		rollback()
		return RootfsHandle{}, fmt.Errorf("mkdir mounts parent: %w", err)
	}
	_ = os.Chmod(filepath.Dir(mountDir), 0o711)
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		rollback()
		return RootfsHandle{}, fmt.Errorf("mkdir mount: %w", err)
	}

	if meta.FSType != "" && meta.FSType != "btrfs" {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		rollback()
		return RootfsHandle{}, fmt.Errorf("unsupported rootfs FSType %q (expected btrfs); destroy and recreate the volume", meta.FSType)
	}

	// Mount btrfs rootfs. Service rootfs: host-level read-only. The flattened image
	// from podman export contains all OCI bind mount targets (/etc/resolv.conf,
	// /etc/hostname, /etc/hosts, /etc/mtab) — runc bind-mounts over existing paths,
	// it does not need to create them. Workspaces: read-write for user data.
	mountOpts := btrfsRootfsMountOpts
	if meta.ReadOnly {
		mountOpts = "ro," + mountOpts
	}
	if err := m.run.Run(ctx, "mount", "-t", "btrfs", "-o", mountOpts, luksPath, mountDir); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		rollback()
		return RootfsHandle{}, fmt.Errorf("mount: %w", err)
	}

	// Idmapped mount.
	var idmapPath string
	if meta.IDMap != nil {
		idmapPath = mountDir + "-idmap"
		idmapConfig := fsutil.IDMapConfig{
			AppUID:      meta.IDMap.AppUID,
			AppGID:      meta.IDMap.AppGID,
			SubUIDStart: meta.IDMap.SubUIDStart,
			SubUIDCount: meta.IDMap.SubUIDCount,
			SubGIDStart: meta.IDMap.SubGIDStart,
			SubGIDCount: meta.IDMap.SubGIDCount,
		}
		if err := fsutil.CreateIDMappedMount(mountDir, idmapPath, idmapConfig); err != nil {
			m.run.Run(ctx, "umount", mountDir)
			m.run.Run(ctx, "cryptsetup", "close", mapper)
			rollback()
			return RootfsHandle{}, fmt.Errorf("idmap mount: %w", err)
		}
	}

	// Track state.
	state := &rootfsMountState{
		stack:      stack,
		luksMapper: mapper,
		mountPath:  mountDir,
		idmapPath:  idmapPath,
		goldenLV:   meta.GoldenLV,
	}
	m.mu.Lock()
	m.rootfsMounts[volumeID] = state
	m.stacks[volumeID] = stack
	m.mu.Unlock()

	resultPath := mountDir
	if idmapPath != "" {
		resultPath = idmapPath
	}

	return RootfsHandle{
		VolumeID:  volumeID,
		MountPath: resultPath,
		ReadOnly:  meta.ReadOnly,
		GoldenLV:  meta.GoldenLV,
	}, nil
}

// DetachRootfs unmounts and deactivates a rootfs volume.
func (m *luksVolumeManager) DetachRootfs(ctx context.Context, volumeID string) error {
	m.mu.Lock()
	state := m.rootfsMounts[volumeID]
	m.mu.Unlock()

	if state == nil {
		return nil // already detached
	}

	var errs []error

	// Unmount idmapped bind mount.
	if state.idmapPath != "" {
		if err := m.run.Run(ctx, "umount", state.idmapPath); err != nil {
			// Try lazy unmount on EBUSY.
			if err2 := m.run.Run(ctx, "umount", "-l", state.idmapPath); err2 != nil {
				errs = append(errs, fmt.Errorf("umount idmap %s: %w", state.idmapPath, err2))
			}
		}
	}

	// Unmount btrfs.
	if err := m.run.Run(ctx, "umount", state.mountPath); err != nil {
		if err2 := m.run.Run(ctx, "umount", "-l", state.mountPath); err2 != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", state.mountPath, err2))
		}
	}

	// LUKS close.
	if err := m.run.Run(ctx, "cryptsetup", "close", state.luksMapper); err != nil {
		errs = append(errs, fmt.Errorf("luks close %s: %w", state.luksMapper, err))
	}

	// Device stack close.
	if state.stack != nil {
		if err := state.stack.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stack close: %w", err))
		}
	}

	// Delete from tracking maps only after teardown completes.
	m.mu.Lock()
	delete(m.rootfsMounts, volumeID)
	delete(m.stacks, volumeID)
	m.mu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("detach rootfs %s: %v", volumeID, errs)
	}
	return nil
}

// DestroyRootfs permanently removes a rootfs volume (workspace, service-rootfs, or golden).
func (m *luksVolumeManager) DestroyRootfs(ctx context.Context, volumeID string) error {
	// Detach first.
	_ = m.DetachRootfs(ctx, volumeID)

	metaDir := paths.VolumeMetaDir(volumeID)
	metaPath := filepath.Join(metaDir, metadataV2File)
	mountDir := paths.MountDir(volumeID)

	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read metadata: %w", err)
	}

	// Remove thin LV.
	if m.lvMgr != nil && meta.LVName != "" {
		if err := m.lvMgr.RemoveThinLV(ctx, meta.LVName); err != nil {
			log.Printf("WARN: remove LV %s: %v", meta.LVName, err)
		}
	}

	// Remove metadata and mount dirs.
	_ = os.RemoveAll(metaDir)
	_ = os.RemoveAll(mountDir)
	_ = os.RemoveAll(mountDir + "-idmap")

	return nil
}

// destroyGoldenLVUnsafe destroys a golden LV without lock. Called under goldenMu.
func (m *luksVolumeManager) destroyGoldenLVUnsafe(ctx context.Context, goldenID string) {
	mapper := "piccolo-vol-" + goldenID

	// Best-effort teardown of any active state.
	m.run.Run(ctx, "umount", paths.MountDir(goldenID))
	m.run.Run(ctx, "cryptsetup", "close", mapper)
	m.lvMgr.DeactivateLV(ctx, goldenID)
	m.lvMgr.RemoveThinLV(ctx, goldenID)
	_ = os.RemoveAll(paths.VolumeMetaDir(goldenID))
	_ = os.RemoveAll(paths.MountDir(goldenID))
}

// GarbageCollectGoldenLVs removes golden LVs with no remaining references.
func (m *luksVolumeManager) GarbageCollectGoldenLVs(ctx context.Context) error {
	metaBase := paths.CoreJoin("volumes")
	entries, err := os.ReadDir(metaBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read volumes dir: %w", err)
	}

	// Collect all golden LVs and their references.
	goldenIDs := make(map[string]bool)
	referencedGoldens := make(map[string]bool)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volID := e.Name()
		metaPath := filepath.Join(metaBase, volID, metadataV2File)
		version, err := readVolumeMetaVersion(metaPath)
		if err != nil {
			continue
		}
		if version != metadataV3Version {
			continue
		}
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			continue
		}
		if meta.Type == "golden" {
			goldenIDs[volID] = true
		}
		if meta.GoldenLV != "" {
			referencedGoldens[meta.GoldenLV] = true
		}
	}

	// Remove unreferenced golden LVs.
	for goldenID := range goldenIDs {
		if referencedGoldens[goldenID] {
			continue
		}
		log.Printf("GC: removing unreferenced golden LV %s", goldenID)
		digestShort := strings.TrimPrefix(goldenID, goldenLVPrefix)
		mu := m.goldenMutex(digestShort)
		mu.Lock()
		m.destroyGoldenLVUnsafe(ctx, goldenID)
		m.mu.Lock()
		delete(m.goldenLVs, digestShort)
		m.mu.Unlock()
		// Remove from goldenMu map.
		m.goldenMuLock.Lock()
		delete(m.goldenMu, digestShort)
		m.goldenMuLock.Unlock()
		mu.Unlock()
	}

	return nil
}

// ReconcileRootfsStates validates rootfs volumes on startup.
func (m *luksVolumeManager) ReconcileRootfsStates(ctx context.Context) error {
	metaBase := paths.CoreJoin("volumes")
	entries, err := os.ReadDir(metaBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read volumes dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volID := e.Name()
		metaPath := filepath.Join(metaBase, volID, metadataV2File)
		version, err := readVolumeMetaVersion(metaPath)
		if err != nil {
			continue
		}
		if version != metadataV3Version {
			continue
		}
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			log.Printf("WARN: rootfs volume %s metadata corrupted: %v", volID, err)
			continue
		}

		switch meta.Type {
		case "golden":
			digestShort := strings.TrimPrefix(volID, goldenLVPrefix)
			if meta.FlattenComplete != "" {
				// Fast path: flatten complete, cache and skip.
				m.mu.Lock()
				m.goldenLVs[digestShort] = meta
				m.mu.Unlock()
			} else {
				// Incomplete golden LV: destroy.
				log.Printf("reconcile: destroying incomplete golden LV %s", volID)
				mu := m.goldenMutex(digestShort)
				mu.Lock()
				m.destroyGoldenLVUnsafe(ctx, volID)
				mu.Unlock()
			}

		case "workspace", "service-rootfs":
			// Validate metadata is parseable. Actual re-attach happens lazily
			// when AppManager starts the container.
			log.Printf("reconcile: rootfs volume %s (type=%s) present", volID, meta.Type)
		}
	}

	// GC golden LVs.
	return m.GarbageCollectGoldenLVs(ctx)
}

// ReadGoldenImageConfig returns the OCI image config for a golden LV.
func (m *luksVolumeManager) ReadGoldenImageConfig(ctx context.Context, goldenID string) (GoldenImageConfig, error) {
	configPath := filepath.Join(paths.VolumeMetaDir(goldenID), imageConfigFile)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return GoldenImageConfig{}, fmt.Errorf("read golden image config: %w", err)
	}
	var cfg GoldenImageConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return GoldenImageConfig{}, fmt.Errorf("parse golden image config: %w", err)
	}
	return cfg, nil
}

// RootfsVolumeID returns the rootfs volume ID for a given instance and mode.
func (m *luksVolumeManager) RootfsVolumeID(mode string, instanceID string) string {
	switch mode {
	case "workspace":
		return workspaceLVPrefix + instanceID
	case "service-rootfs":
		return svcRootfsLVPrefix + instanceID
	default:
		return instanceID
	}
}

// syncfsPath syncs all pending writes on the filesystem containing path.
func syncfsPath(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s for syncfs: %w", path, err)
	}
	defer syscall.Close(fd)
	return unix.Syncfs(fd)
}

// RootfsExists checks if rootfs volume metadata exists on disk.
func (m *luksVolumeManager) RootfsExists(volumeID string) bool {
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	_, err := os.Stat(metaPath)
	return err == nil
}

// newUUID generates a random UUID v4 using crypto/rand.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
