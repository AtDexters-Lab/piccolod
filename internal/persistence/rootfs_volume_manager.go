package persistence

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	goldenLVPrefix      = "golden-"
	workspaceLVPrefix   = "ws-"
	svcRootfsLVPrefix   = "svc-rootfs-"
	imageConfigFile     = "image-config.json"
	defaultGoldenLVSize = 10 << 30 // 10 GiB

	btrfsRootfsMountOpts = "compress=zstd:1,discard=async,noatime"

	goldenStagingTeardownAttempts = 5
	goldenStagingTeardownDelay    = 250 * time.Millisecond

	// defaultWorkspaceVirtualSize is the initial virtual size for workspace
	// snapshot LVs. Thin-provisioned — only written blocks consume physical space.
	defaultWorkspaceVirtualSize = 50 << 30 // 50 GiB

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

// resetGoldenStorageBeforePublication settles and removes any stale golden
// storage before publishing the next incomplete marker. It deliberately uses
// the same content-agnostic destruction primitive as reconciliation and GC.
func (m *luksVolumeManager) resetGoldenStorageBeforePublication(ctx context.Context, goldenID string) error {
	return m.destroyGoldenLVLocked(ctx, goldenID)
}

// EnsureGoldenLV creates or reuses a verified golden LV. Existing image
// callers are normalized into the generic OCI-image identity; source adapters
// call it through EnsureGoldenContent.
func (m *luksVolumeManager) EnsureGoldenLV(ctx context.Context, req GoldenLVRequest) (string, error) {
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: req.ImageDigest,
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	if req.Identity != nil {
		identity = *req.Identity
	}
	if err := validateGoldenIdentity(identity); err != nil {
		return "", err
	}
	sourceRef := req.SourceRef
	if sourceRef == "" {
		sourceRef = req.ImageRef
	}
	materialize := req.Materialize
	if materialize == nil {
		if m.flattenFn == nil {
			return "", fmt.Errorf("flattenFn not configured")
		}
		materialize = func(ctx context.Context, targetDir string) (GoldenMaterializationResult, error) {
			imgConfig, err := m.flattenFn(ctx, req.ImageRef, targetDir, req.PrePulledDir)
			if err != nil {
				return GoldenMaterializationResult{}, err
			}
			return GoldenMaterializationResult{ImageConfig: &imgConfig}, nil
		}
	}

	// Lock the preferred shortened storage key so unequal identities that
	// collide are disambiguated serially.
	unlockIdentity, err := m.lockGoldenIdentity(identity)
	if err != nil {
		return "", err
	}
	defer unlockIdentity()

	selection, err := m.selectGoldenStorage(ctx, identity, req.ImageDigest, req.PreferredGoldenID)
	if err != nil {
		return "", err
	}
	goldenID := selection.goldenID
	if selection.ready {
		if err := m.settleReadyGoldenLocked(ctx, goldenID); err != nil {
			return "", fmt.Errorf("prove Ready golden LV %s reusable: %w", goldenID, err)
		}
		m.mu.Lock()
		m.goldenLVs[goldenID] = selection.meta
		m.mu.Unlock()
		return goldenID, nil
	}

	metaDir := paths.VolumeMetaDir(goldenID)
	metaPath := filepath.Join(metaDir, metadataV2File)
	if selection.meta != nil {
		log.Printf("golden LV %s incomplete, recreating", goldenID)
	}
	if err := m.resetGoldenStorageBeforePublication(ctx, goldenID); err != nil {
		return "", err
	}

	// Check thin pool capacity.
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return "", err
	}

	lvName := goldenID
	sizeBytes := int64(defaultGoldenLVSize)
	sizeHint := req.ContentSizeHint
	if sizeHint <= 0 {
		sizeHint = req.ImageSizeHint
	}
	if sizeHint > 0 {
		sizeBytes = goldenLVSizeForImage(sizeHint)
	} else if m.imageSizeFn != nil {
		if imgSize, err := m.imageSizeFn(ctx, sourceRef); err == nil && imgSize > 0 {
			sizeBytes = goldenLVSizeForImage(imgSize)
		} else if err != nil {
			log.Printf("WARN: imageSizeFn failed for %s, using default LV size: %v", sourceRef, err)
		} else if imgSize <= 0 {
			log.Printf("WARN: imageSizeFn returned non-positive size %d for %s, using default LV size", imgSize, sourceRef)
		}
	}

	mapper := volMapperName(goldenID)
	mountDir := paths.MountDir(goldenID)

	// Persist ownership before allocating the LV. A crash anywhere after this
	// point leaves an explicitly incomplete volume record that ordinary rootfs
	// reconciliation can destroy; an unrecorded thin LV must never be the only
	// evidence of an interrupted materialization.
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir golden metadata: %w", err)
	}
	incompleteMeta := &volumeMetaV3{
		Version:              metadataV3Version,
		Type:                 volumeTypeGolden,
		LVName:               lvName,
		VGName:               lvm.DefaultVGName,
		SizeBytes:            sizeBytes,
		FSType:               "btrfs",
		ReadOnly:             true,
		BaseImageDigest:      req.ImageDigest,
		BaseImageRef:         sourceRef,
		GoldenIdentity:       &identity,
		PasswordKeyslotKeyID: KeyslotKeyIDUnprovisioned,
		RecoveryKeyslotKeyID: KeyslotKeyIDUnprovisioned,
	}
	if err := writeVolumeMetaV3(metaPath, incompleteMeta); err != nil {
		return "", fmt.Errorf("write incomplete golden metadata: %w", err)
	}

	// Track which layers have been set up for deferred cleanup.
	var (
		lvActive   bool
		luksOpened bool
		mounted    bool
		success    bool
	)
	defer func() {
		if success {
			m.nudgeVolumeCreation()
		}
	}()
	defer func() {
		// Use a detached context for cleanup — the caller's ctx may be cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		teardownErr := m.teardownGoldenStaging(
			cleanupCtx,
			mountDir,
			mapper,
			lvName,
			&mounted,
			&luksOpened,
			&lvActive,
			goldenStagingTeardownDelay,
		)
		if teardownErr != nil {
			log.Printf("WARN: clean golden LV %s staging stack: %v", goldenID, teardownErr)
		}
		if !success {
			if teardownErr != nil || mounted || luksOpened || lvActive {
				log.Printf("WARN: retain incomplete golden LV %s until its staging stack can be settled", goldenID)
			} else if err := m.destroyGoldenLVLocked(cleanupCtx, goldenID); err != nil {
				log.Printf("WARN: retain incomplete golden LV %s after cleanup failure: %v", goldenID, err)
			}
		}
	}()

	if err := m.lvMgr.CreateThinLV(ctx, lvName, sizeBytes); err != nil {
		return "", fmt.Errorf("create golden LV: %w", err)
	}
	// ActivateLV performs lvchange before udevadm settle. Treat any activation
	// attempt as possibly effective so a settle failure still deactivates the
	// LV during cleanup.
	lvActive = true
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

	// Run the source-specific adapter inside the staging golden filesystem.
	result, err := materialize(ctx, mountDir)
	if err != nil {
		return "", fmt.Errorf("materialize golden content: %w", err)
	}

	// syncfs ensures payload data is durable before the metadata commit marks
	// this exact identity Ready.
	if err := syncfsPath(mountDir); err != nil {
		return "", fmt.Errorf("syncfs golden: %w", err)
	}

	// Ready means the materialized bytes are durable and the transient
	// creation stack is fully settled. Do this before the metadata commit so
	// an EBUSY unmount (or a mapper/LV teardown failure) cannot be reported as
	// successful materialization.
	finalizeCtx, finalizeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if err := m.teardownGoldenStaging(
		finalizeCtx,
		mountDir,
		mapper,
		lvName,
		&mounted,
		&luksOpened,
		&lvActive,
		goldenStagingTeardownDelay,
	); err != nil {
		finalizeCancel()
		return "", fmt.Errorf("settle golden LV staging stack: %w", err)
	}
	finalizeCancel()

	// OCI image projections retain their config for --rootfs consumers. Other
	// artifacts have no image execution metadata.
	if result.ImageConfig != nil {
		imgConfigData, err := json.Marshal(*result.ImageConfig)
		if err != nil {
			return "", fmt.Errorf("marshal image config: %w", err)
		}
		configPath := filepath.Join(metaDir, imageConfigFile)
		if err := fsutil.AtomicWriteFile(configPath, imgConfigData, 0o600); err != nil {
			return "", fmt.Errorf("write image config: %w", err)
		}
	}

	// Write v3 metadata WITH materialize_complete — atomic commit point.
	// On crash before this write: metadata absent/incomplete → reconcile destroys.
	// On crash after this write: Ready evidence is durable and reusable.
	completedAt := time.Now().UTC().Format(time.RFC3339)
	meta := incompleteMeta
	meta.MaterializeComplete = completedAt
	if identity.SourceKind == GoldenSourceOCI && identity.Projection == GoldenProjectionOCIImageRootfs {
		// Preserve the legacy marker for mixed-version metadata readers.
		meta.FlattenComplete = completedAt
	}
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		return "", fmt.Errorf("write golden metadata: %w", err)
	}

	// Mark success so deferred cleanup preserves the LV.
	success = true

	// Cache.
	m.mu.Lock()
	m.goldenLVs[goldenID] = meta
	m.mu.Unlock()

	log.Printf("golden LV created: %s (source=%s identity=%s projection=%s size=%d)",
		goldenID, sourceRef, identity.ResolvedIdentity, identity.Projection, sizeBytes)
	return goldenID, nil
}

// teardownGoldenStaging settles the temporary stack used while populating a
// golden LV. Each lower layer is released only after the layer above it has
// been released, and transient busy errors receive a short bounded retry.
func (m *luksVolumeManager) teardownGoldenStaging(
	ctx context.Context,
	mountDir, mapper, lvName string,
	mounted, luksOpened, lvActive *bool,
	retryDelay time.Duration,
) error {
	retry := func(operation string, run func() error) error {
		var err error
		for attempt := 1; attempt <= goldenStagingTeardownAttempts; attempt++ {
			if err = run(); err == nil {
				return nil
			}
			if attempt == goldenStagingTeardownAttempts {
				break
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return fmt.Errorf("%s: %w", operation, errors.Join(err, ctx.Err()))
			case <-timer.C:
			}
		}
		return fmt.Errorf("%s after %d attempts: %w", operation, goldenStagingTeardownAttempts, err)
	}

	if *mounted {
		if err := retry("unmount "+mountDir, func() error {
			return m.run.Run(ctx, "umount", mountDir)
		}); err != nil {
			return err
		}
		*mounted = false
	}
	if *luksOpened {
		if err := retry("close mapper "+mapper, func() error {
			return m.run.Run(ctx, "cryptsetup", "close", mapper)
		}); err != nil {
			return err
		}
		*luksOpened = false
	}
	if *lvActive {
		if err := retry("deactivate LV "+lvName, func() error {
			return m.lvMgr.DeactivateLV(ctx, lvName)
		}); err != nil {
			return err
		}
		*lvActive = false
	}
	return nil
}

// CreateWorkspaceFromGolden creates a workspace rootfs from a golden LV snapshot.
func (m *luksVolumeManager) CreateWorkspaceFromGolden(ctx context.Context, req WorkspaceRootfsRequest) (RootfsHandle, error) {
	goldenID, err := m.EnsureGoldenLV(ctx, GoldenLVRequest{
		ImageDigest:   req.ImageDigest,
		ImageRef:      req.ImageRef,
		ImageSizeHint: req.ImageSizeHint,
		PrePulledDir:  req.PrePulledDir,
	})
	if err != nil {
		return RootfsHandle{}, err
	}

	volumeID := workspaceLVPrefix + req.InstanceID
	return m.createRootfsFromGolden(ctx, goldenID, volumeID, volumeTypeWorkspace, false, &req.IDMap)
}

// CreateServiceRootfs creates a read-only service rootfs from a golden LV snapshot.
func (m *luksVolumeManager) CreateServiceRootfs(ctx context.Context, req ServiceRootfsRequest) (RootfsHandle, error) {
	goldenID, err := m.EnsureGoldenLV(ctx, GoldenLVRequest{
		ImageDigest:   req.ImageDigest,
		ImageRef:      req.ImageRef,
		ImageSizeHint: req.ImageSizeHint,
		PrePulledDir:  req.PrePulledDir,
	})
	if err != nil {
		return RootfsHandle{}, err
	}

	volumeID := req.VolumeID
	if volumeID == "" {
		volumeID = ServiceRootfsVolumeID(req.InstanceID, req.ServiceName)
	}
	return m.createRootfsFromGolden(ctx, goldenID, volumeID, volumeTypeServiceRootfs, true, &req.IDMap)
}

func readReadyGoldenMeta(goldenID string) (*volumeMetaV3, GoldenContentIdentity, error) {
	metaPath := filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return nil, GoldenContentIdentity{}, fmt.Errorf("read golden metadata: %w", err)
	}
	if meta.Type != volumeTypeGolden || goldenReadyTimestamp(meta) == "" {
		return nil, GoldenContentIdentity{}, fmt.Errorf("golden LV %s is not verified Ready content", goldenID)
	}
	identity, err := goldenIdentityFromMeta(meta)
	if err != nil {
		return nil, GoldenContentIdentity{}, fmt.Errorf("read golden identity: %w", err)
	}
	return meta, identity, nil
}

// createRootfsFromGolden creates a rootfs from a golden LV via snapshot.
func (m *luksVolumeManager) createRootfsFromGolden(ctx context.Context, goldenID, volumeID, volType string, readOnly bool, idmap *IDMapConfig) (RootfsHandle, error) {
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return RootfsHandle{}, err
	}

	// The first read identifies the per-content lock. GC may have acted before
	// that lock is acquired, so this read is never treated as proof that the
	// golden still exists.
	_, identity, err := readReadyGoldenMeta(goldenID)
	if err != nil {
		return RootfsHandle{}, err
	}
	unlockIdentity, err := m.lockGoldenIdentity(identity)
	if err != nil {
		return RootfsHandle{}, err
	}
	defer unlockIdentity()

	// Re-read under the identity lock immediately before snapshot creation.
	// Holding the lock through the rootfs metadata commit makes the new
	// GoldenLV reference and GC destruction mutually exclusive.
	goldenMeta, lockedIdentity, err := readReadyGoldenMeta(goldenID)
	if err != nil {
		return RootfsHandle{}, err
	}
	if lockedIdentity != identity {
		return RootfsHandle{}, fmt.Errorf("golden LV %s identity changed before snapshot creation", goldenID)
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

	// Workspace snapshots get a generous virtual size — thin provisioning
	// means only written blocks consume physical pool space.
	sizeBytes := goldenMeta.SizeBytes
	if volType == volumeTypeWorkspace && defaultWorkspaceVirtualSize > sizeBytes {
		if err := m.lvMgr.ResizeLV(ctx, snapshotName, defaultWorkspaceVirtualSize); err != nil {
			log.Printf("WARN: resize workspace LV %s to default: %v", snapshotName, err)
		} else {
			sizeBytes = defaultWorkspaceVirtualSize
		}
	}

	// Write v3 metadata.
	metaDir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		m.lvMgr.DeactivateLV(ctx, snapshotName)
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		return RootfsHandle{}, fmt.Errorf("mkdir meta: %w", err)
	}

	meta := &volumeMetaV3{
		Version:              metadataV3Version,
		Type:                 volType,
		LVName:               snapshotName,
		VGName:               lvm.DefaultVGName,
		SizeBytes:            sizeBytes,
		FSType:               "btrfs",
		ReadOnly:             readOnly,
		BaseImageDigest:      goldenMeta.BaseImageDigest,
		BaseImageRef:         goldenMeta.BaseImageRef,
		GoldenLV:             goldenID,
		PasswordKeyslotKeyID: KeyslotKeyIDUnprovisioned,
		RecoveryKeyslotKeyID: KeyslotKeyIDUnprovisioned,
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

	// Acquire the per-volume transition lock and route through the same
	// reconciler-shaped attach path AttachRootfs uses. This
	// closes the "fresh LV lands on stale mount" hazard: if a prior destroy
	// left a StaleMountRecord at paths.MountDir(volumeID), the probe sees
	// it and lazy-umounts before the fresh mount runs. attachRootfsLocked
	// re-reads metadata from disk; the metadata we just wrote is what it
	// will see.
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()

	handle, err := m.attachRootfsLocked(ctx, volumeID)
	if err != nil {
		m.lvMgr.DeactivateLV(ctx, snapshotName)
		m.lvMgr.RemoveThinLV(ctx, snapshotName)
		_ = os.RemoveAll(metaDir)
		return RootfsHandle{}, err
	}

	m.nudgeVolumeCreation()
	return handle, nil
}

func goldenHasRootfsReference(goldenID string) (bool, error) {
	volumeIDs, err := listVolumeIDs()
	if err != nil {
		return false, err
	}
	for _, volumeID := range volumeIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
		version, err := readVolumeMetaVersion(metaPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read volume %s metadata version: %w", volumeID, err)
		}
		if version != metadataV3Version {
			continue
		}
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			return false, fmt.Errorf("read volume %s metadata: %w", volumeID, err)
		}
		if meta.GoldenLV == goldenID {
			return true, nil
		}
	}
	return false, nil
}

func goldenHasArtifactReference(goldenID string) (bool, error) {
	references, err := listArtifactReferences()
	if err != nil {
		return false, err
	}
	for _, reference := range references {
		if reference.GoldenID == goldenID {
			return true, nil
		}
	}
	return false, nil
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
		Version:              metadataV3Version,
		Type:                 volumeTypeWorkspace,
		LVName:               cloneVolumeID,
		VGName:               lvm.DefaultVGName,
		SizeBytes:            originMeta.SizeBytes,
		FSType:               "btrfs",
		BaseImageDigest:      originMeta.BaseImageDigest,
		BaseImageRef:         originMeta.BaseImageRef,
		GoldenLV:             originMeta.GoldenLV,
		CloneOf:              originVolumeID,
		IDMap:                originMeta.IDMap,
		PasswordKeyslotKeyID: KeyslotKeyIDUnprovisioned,
		RecoveryKeyslotKeyID: KeyslotKeyIDUnprovisioned,
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

	// Per-volume transition lock + reconciler-shaped attach —
	// see createRootfsFromGolden for rationale.
	lock := m.lockFor(cloneVolumeID)
	lock.Lock()
	defer lock.Unlock()

	handle, err := m.attachRootfsLocked(ctx, cloneVolumeID)
	if err != nil {
		m.lvMgr.DeactivateLV(ctx, cloneVolumeID)
		m.lvMgr.RemoveThinLV(ctx, cloneVolumeID)
		_ = os.RemoveAll(metaDir)
		return RootfsHandle{}, err
	}

	m.nudgeVolumeCreation()
	return handle, nil
}

// ListClones returns volume IDs of clones created from the given origin volume.
func (m *luksVolumeManager) ListClones(ctx context.Context, originVolumeID string) ([]string, error) {
	volIDs, err := listVolumeIDs()
	if err != nil {
		return nil, err
	}

	var clones []string
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
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

// AttachRootfs activates and mounts an existing rootfs volume. Acquires
// the per-volume transition lock; concurrent callers serialize.
//
// Reconciliation shape: probes AttachStateOf under lock first.
//   - Attached → return the metadata-derived RootfsHandle (no cache lookup
//     required; rootfs handle is fully derivable from metadata).
//   - ForeignMountAtPath / KernelStateCorrupted / Unknown → fail loud.
//   - StaleMountRecord → lazy umount, then full attach.
//   - Detached / PartialMapperOnly → run attachRootfsFromMeta, which is
//     naturally idempotent.
//
// The pre-existing m.rootfsMounts fast-path has been removed — every call
// goes through the kernel-state probe. Cost is microseconds per call;
// eliminates the divergence-risk-via-cache-shortcut hazard.
func (m *luksVolumeManager) AttachRootfs(ctx context.Context, volumeID string) (RootfsHandle, error) {
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()
	return m.attachRootfsLocked(ctx, volumeID)
}

// attachRootfsLocked is the lock-already-held variant of AttachRootfs.
// Callers MUST hold m.locks[volumeID]. Three call sites:
//   - AttachRootfs (public entry; takes the lock and calls through here).
//   - createRootfsFromGolden (takes the lock after metadata
//     write, then calls through to get probe-and-reconcile + IDMap
//     fingerprint check + StaleMountRecord lazy-umount for free).
//   - CloneWorkspace (same shape as createRootfsFromGolden).
//
// Re-reads metadata from disk every call. The IDMap fingerprint check
// lives in attachRootfsFromMeta so all callers — including any that bypass
// this function and call attachRootfsFromMeta directly — run the check.
// Additionally, the Attached fast-path here runs the same fingerprint
// check before deriving the handle (codex P2): a volume that stayed
// mounted across a daemon restart with hand-edited metadata would
// otherwise return the derived handle bypassing the immutability guard.
func (m *luksVolumeManager) attachRootfsLocked(ctx context.Context, volumeID string) (RootfsHandle, error) {
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return RootfsHandle{}, fmt.Errorf("read rootfs metadata: %w", err)
	}

	state, probeErr := m.attachStateOfUnderLock(ctx, volumeID)
	if probeErr != nil {
		if errors.Is(probeErr, ErrKernelStateCorrupted) || errors.Is(probeErr, ErrKernelStateAmbiguous) {
			return RootfsHandle{}, probeErr
		}
		log.Printf("WARN: AttachRootfs %s probe error (will refuse with ambiguous): %v", volumeID, probeErr)
	}

	switch state {
	case AttachStateAttached:
		if meta.Type == volumeTypeGolden {
			stack, err := m.inspectGoldenLiveStack(volumeID)
			if err != nil {
				return RootfsHandle{}, err
			}
			if !stack.mounted || !stack.mountReadOnly || !stack.mapperOpen {
				return RootfsHandle{}, fmt.Errorf(
					"Ready golden LV %s is not attached read-only (mounted=%v read_only=%v mapper_open=%v)",
					volumeID,
					stack.mounted,
					stack.mountReadOnly,
					stack.mapperOpen,
				)
			}
		}
		// Fingerprint guard runs on every Attached return — see godoc.
		if err := verifyIDMapFingerprint(volumeID, meta); err != nil {
			return RootfsHandle{}, err
		}
		// Legacy workspace size upgrade must run on every attach path
		// (codex5-P2). Idempotent — no-op when already at default size.
		m.upgradeUndersizedWorkspace(ctx, volumeID, meta)
		return deriveRootfsHandle(volumeID, meta), nil
	case AttachStateForeignMountAtPath:
		return RootfsHandle{}, fmt.Errorf("foreign mount at %s; volume %s in unrecoverable state — logged for diagnostics", paths.MountDir(volumeID), volumeID)
	case AttachStateKernelStateCorrupted:
		return RootfsHandle{}, ErrKernelStateCorrupted
	case AttachStateUnknown:
		return RootfsHandle{}, ErrKernelStateAmbiguous
	case AttachStateStaleMountRecord:
		// Lazy-umount only the paths that are actually mount points right
		// now. Stale-record can present in three shapes (raw + idmap, raw
		// only, idmap only after a partial teardown); we tear down idmap
		// before raw so the bind doesn't pin the underlying mount, then
		// raw. Skipping a path that isn't a mount point is correct (it
		// was already gone) and necessary — running umount on a non-mount
		// errors, and aborting on that error would leave the volume
		// permanently unrecoverable in the idmap-only case (codex6-P2-B).
		if err := m.unmountStaleIfPresent(ctx, paths.MountDir(volumeID)+"-idmap"); err != nil {
			return RootfsHandle{}, fmt.Errorf("attach rootfs %s: %w", volumeID, err)
		}
		if err := m.unmountStaleIfPresent(ctx, paths.MountDir(volumeID)); err != nil {
			return RootfsHandle{}, fmt.Errorf("attach rootfs %s: %w", volumeID, err)
		}
	case AttachStateDetached, AttachStatePartialMapperOnly:
		// Fall through to attachRootfsFromMeta.
	}

	return m.attachRootfsFromMeta(ctx, volumeID, meta)
}

// liveResizeSpec describes which live layers above the LV must be
// resized in lockstep with the LV. Used by resizeConverge to enforce the
// strict ordering codex iter-7 B2 pinned.
//
// Mapper: when non-empty, cryptsetup resize <Mapper> runs with the pool keyfile
// after the LV resize succeeds. Empty means no live LUKS mapper to resize
// (volume is detached, or the strict caller's probe says so).
//
// FSResize: filesystem-specific online-grow callback (btrfs filesystem
// resize for workspace, resize2fs for application). Invoked only when
// Mapper is non-empty AND FSResize is non-nil — the cryptsetup-only
// state covers partial-attach states where the mapper is open but the fs
// is not mounted; the LV+LUKS sizes will be visible naturally on the
// next mount.
//
// Coupling: FSResize is gated by Mapper. Setting FSResize without
// Mapper is a programmer error — the helper rejects it at entry rather
// than silently dropping the callback.
type liveResizeSpec struct {
	Mapper   string
	FSResize func(ctx context.Context) error
}

// liveResizeFromState builds the live-resize spec for a STRICT resize
// from a probed AttachState. Pre-existing strict callers gated only on
// AttachStateAttached, leaving two structural holes codex iter-7
// follow-up surfaced: PartialMapperOnly advanced LV+meta while the
// open cryptsetup mapper stayed at the old size (next mount saw stale
// LUKS-payload-size); ambiguous/hostile states (Foreign / Unknown /
// Corrupted / Stale) collapsed silently into the Detached branch and
// also advanced LV+meta. Both are fixed here:
//
//   - Attached            → mapper + fs resize
//   - Detached            → LV-only (LUKS+fs converge on next attach)
//   - any other state     → refuse the resize; auto-grow's next cycle
//     or admin retry observes the now-clearer
//     kernel state and tries again.
//
// PartialMapperOnly is refused on purpose: the AttachState enum doesn't
// distinguish "mapper open, raw fs not mounted" (the mapper-only sub-
// shape) from "mapper open, raw fs mounted, idmap bind missing" (a
// transient mid-attach shape where the fs IS resizable). Resizing only
// the mapper in the second sub-shape would advance meta while the
// mounted raw fs stays at the old size, recreating the B2 hole inside
// PartialMapperOnly's coarse classification. Refusing forces the caller
// to retry once the reconciler completes the missing layer (typical
// recovery <1s); auto-grow and admin both tolerate that latency.
//
// fsResize is the filesystem-specific online-grow callback (btrfs
// filesystem resize for workspace, resize2fs for application). It is
// invoked only from the Attached branch (mapper open AND fs mounted).
func liveResizeFromState(state AttachState, mapper string, fsResize func(ctx context.Context) error) (liveResizeSpec, error) {
	switch state {
	case AttachStateAttached:
		return liveResizeSpec{Mapper: mapper, FSResize: fsResize}, nil
	case AttachStateDetached:
		return liveResizeSpec{}, nil
	default:
		return liveResizeSpec{}, fmt.Errorf("refusing resize on attach state %s; clear ambiguity before retry", state)
	}
}

// resizeConverge runs the LV → cryptsetup → fs convergence in the strict
// order codex iter-7 B2 pinned: every live layer above the LV converges
// BEFORE meta.SizeBytes is persisted. If any step errors, the metadata
// is NOT advanced — a future attach retries from the still-old size and
// the upgrade fires again. Persisting metadata before live convergence
// would let a future attach skip the upgrade (meta says we're already
// at the new size), freezing the mapper/fs forever at the old size.
//
// Sole owner of the size-write ordering for v3 volumes; new resize
// sites should compose with this helper rather than reimplementing the
// sequence (the bug class B2 closed structurally is "person forgot to
// defer the meta write past the live steps").
//
// metaPath is the absolute path of the volume metadata file that meta
// was read from; it is the file the helper writes back to on success.
// Passing it in (rather than re-deriving from volumeID) avoids
// recomputing filepath.Join + paths.VolumeMetaDir at each callsite.
func (m *luksVolumeManager) resizeConverge(
	ctx context.Context,
	meta *volumeMetaV3,
	metaPath string,
	newSizeBytes int64,
	live liveResizeSpec,
) error {
	if live.FSResize != nil && live.Mapper == "" {
		return fmt.Errorf("resizeConverge: FSResize requires Mapper (meta %s)", metaPath)
	}
	actualSizeBytes, err := m.lvMgr.EnsureLVSizeAtLeast(ctx, meta.LVName, newSizeBytes)
	if err != nil {
		return fmt.Errorf("lvresize: %w", err)
	}
	if live.Mapper != "" {
		if err := m.luksResizeWithPoolKeyfile(ctx, live.Mapper); err != nil {
			return fmt.Errorf("cryptsetup resize: %w", err)
		}
		if live.FSResize != nil {
			if err := live.FSResize(ctx); err != nil {
				return err
			}
		}
	}
	meta.SizeBytes = actualSizeBytes
	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		return fmt.Errorf("update metadata: %w", err)
	}
	return nil
}

// upgradeUndersizedWorkspace resizes a legacy workspace LV up to
// defaultWorkspaceVirtualSize when meta.SizeBytes is still below it.
// Idempotent (no-op when meta is already at or above default). Must run
// on every attach path — pre-attach-truth this only happened on a cold
// attach because the cache miss forced the full path; post-refactor the
// Attached fast-path bypasses attachRootfsFromMeta, so the upgrade has
// to be invoked here too. Single-node only (multi-node managers are
// gated out at construction).
func (m *luksVolumeManager) upgradeUndersizedWorkspace(ctx context.Context, volumeID string, meta *volumeMetaV3) {
	if meta.Type != volumeTypeWorkspace {
		return
	}
	if meta.SizeBytes >= defaultWorkspaceVirtualSize {
		return
	}
	if m.nbdSrv != nil || m.drbdMgr != nil {
		return
	}

	// From the full-attach path the mapper isn't open yet, so the stat
	// below misses and live stays empty; resizeConverge runs LV-only and
	// the subsequent luksOpen+mount see the new size naturally. From the
	// Attached fast-path the mapper is open and we resize cryptsetup +
	// btrfs inline; metadata only persists once both succeed.
	live := liveResizeSpec{}
	mapper := volMapperName(volumeID)
	if _, err := os.Stat("/dev/mapper/" + mapper); err == nil {
		live.Mapper = mapper
		mountDir := paths.MountDir(volumeID)
		if mounted, _ := isMountPoint(mountDir); mounted {
			live.FSResize = func(ctx context.Context) error {
				if err := m.run.Run(ctx, "btrfs", "filesystem", "resize", "max", mountDir); err != nil {
					return fmt.Errorf("btrfs resize: %w", err)
				}
				return nil
			}
		}
	}
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	if err := m.resizeConverge(ctx, meta, metaPath, defaultWorkspaceVirtualSize, live); err != nil {
		log.Printf("WARN: legacy workspace upgrade %s: %v (metadata not advanced; next attach will retry)", volumeID, err)
	}
}

// unmountStaleIfPresent lazy-umounts path iff it's currently a mount
// point; returns nil otherwise. This is the StaleMountRecord recovery
// primitive: post-condition is "path is not a mount." If the path was
// never mounted (or is no longer mounted), that's already the
// post-condition and we return nil.
//
// The check-then-umount pattern is TOCTOU-safe by post-condition rather
// than pre-condition (codex iter-7 B3): if the mount disappears between
// our isMountPoint check and the umount call, umount fails ("not
// mounted") but the desired post-condition is already true, so we
// re-probe and accept that. We only surface a real error when umount
// fails AND the path is still a mount point.
func (m *luksVolumeManager) unmountStaleIfPresent(ctx context.Context, path string) error {
	mounted, err := isMountPoint(path)
	if err != nil {
		return fmt.Errorf("check mount %s: %w", path, err)
	}
	if !mounted {
		return nil
	}
	if umountErr := m.run.Run(ctx, "umount", "-l", path); umountErr != nil {
		// Re-probe: if the umount race lost (mount vanished concurrently),
		// the umount error is benign — post-condition already satisfied.
		stillMounted, probeErr := isMountPoint(path)
		if probeErr == nil && !stillMounted {
			return nil
		}
		return fmt.Errorf("lazy umount %s: %w", path, umountErr)
	}
	return nil
}

// deriveRootfsHandle reconstructs the caller-facing handle from metadata
// and naming convention — the cache that used to hold this is being
// eliminated (Phase 4). All fields are convention-derivable.
func deriveRootfsHandle(volumeID string, meta *volumeMetaV3) RootfsHandle {
	mountDir := paths.MountDir(volumeID)
	resultPath := mountDir
	if meta.IDMap != nil {
		resultPath = mountDir + "-idmap"
	}
	return RootfsHandle{
		VolumeID:  volumeID,
		MountPath: resultPath,
		ReadOnly:  meta.ReadOnly || meta.Type == volumeTypeGolden,
		GoldenLV:  meta.GoldenLV,
	}
}

// attachRootfsFromMeta performs the full attach sequence for a rootfs volume.
//
// Sole direct caller is attachRootfsLocked, which is reached from three
// transition entry points (AttachRootfs, createRootfsFromGolden,
// CloneWorkspace) each holding m.locks[volumeID]. Lock contract is
// inherited from attachRootfsLocked.
//
// IDMap fingerprint check: detects out-of-band hand-edits to metadata.
// The check runs here AND on the Attached fast-path in attachRootfsLocked
// so any return from a rootfs attach has been validated.
func (m *luksVolumeManager) attachRootfsFromMeta(ctx context.Context, volumeID string, meta *volumeMetaV3) (RootfsHandle, error) {
	if err := verifyIDMapFingerprint(volumeID, meta); err != nil {
		return RootfsHandle{}, err
	}

	lvName := meta.LVName
	mapper := volMapperName(volumeID)

	// Build below-LUKS device stack.
	var stack *blockdev.DeviceStack
	var err error

	switch meta.Type {
	case volumeTypeWorkspace:
		// Workspace: ThinLV → NBD → DRBD
		stack, err = m.buildStack(volumeID, lvName, meta.SizeBytes)
	case volumeTypeServiceRootfs, volumeTypeGolden:
		// Service rootfs and golden content: ThinLV only (not replicated).
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

	m.upgradeUndersizedWorkspace(ctx, volumeID, meta)

	// Rollback on failure: close resources in reverse order.
	var openedMapper string
	var mountedDir string
	success := false
	defer func() {
		if success {
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if mountedDir != "" {
			m.run.Run(rctx, "umount", mountedDir)
		}
		if openedMapper != "" {
			m.run.Run(rctx, "cryptsetup", "close", openedMapper)
		}
		stack.Close(rctx)
	}()

	// LUKS open. Tolerate cryptsetup exit 5 (mapper already exists) — the
	// reconciler may be completing a PartialMapperOnly state.
	topDev := stack.Top().Path()
	if err := m.luksOpenWithPoolKeyfile(ctx, topDev, mapper); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != cryptsetupExitDeviceExists {
			return RootfsHandle{}, fmt.Errorf("luks open: %w", err)
		}
		// Mapper already present — do not register for rollback close.
	} else {
		openedMapper = mapper
	}

	luksPath := "/dev/mapper/" + mapper

	// Mount btrfs.
	mountDir := paths.MountDir(volumeID)
	if err := os.MkdirAll(filepath.Dir(mountDir), 0o711); err != nil {
		return RootfsHandle{}, fmt.Errorf("mkdir mounts parent: %w", err)
	}
	_ = os.Chmod(filepath.Dir(mountDir), 0o711)
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		return RootfsHandle{}, fmt.Errorf("mkdir mount: %w", err)
	}

	if meta.FSType != "" && meta.FSType != "btrfs" {
		return RootfsHandle{}, fmt.Errorf("unsupported rootfs FSType %q (expected btrfs); destroy and recreate the volume", meta.FSType)
	}

	// Mount btrfs rootfs. Service rootfs: host-level read-only. The flattened image
	// from podman export contains all OCI bind mount targets (/etc/resolv.conf,
	// /etc/hostname, /etc/hosts, /etc/mtab) — runc bind-mounts over existing paths,
	// it does not need to create them. Workspaces: read-write for user data.
	// Skip when already mounted (PartialMapperOnly recovery may find raw mount present).
	mountOpts := btrfsRootfsMountOpts
	if meta.ReadOnly || meta.Type == volumeTypeGolden {
		mountOpts = "ro," + mountOpts
	}
	mounted, entry, _ := mountAtPath(mountDir)
	if mounted {
		// Ownership check (codex2-P3): refuse to coexist with a foreign mount.
		if entry.Source != luksPath {
			return RootfsHandle{}, fmt.Errorf("foreign mount at %s (source=%s, expected=%s); refusing to coexist", mountDir, entry.Source, luksPath)
		}
	} else {
		if err := m.run.Run(ctx, "mount", "-t", "btrfs", "-o", mountOpts, luksPath, mountDir); err != nil {
			return RootfsHandle{}, fmt.Errorf("mount: %w", err)
		}
		mountedDir = mountDir
	}

	// Grow btrfs to fill the LV — handles workspaces resized beyond golden LV size.
	// Idempotent: no-op if filesystem already at max.
	if meta.Type == volumeTypeWorkspace {
		_ = m.run.Run(ctx, "btrfs", "filesystem", "resize", "max", mountDir)
	}

	// Idmapped mount. Skip when already present (PartialMapperOnly recovery
	// may find raw mount + idmap both already in place). Ownership check
	// (codex2-P3): an idmap bind inherits the underlying mount's source
	// token, so source != luksPath at the idmap path means a foreign
	// mount got there.
	var idmapPath string
	if meta.IDMap != nil {
		idmapPath = mountDir + "-idmap"
		idmapMounted, idmapEntry, _ := mountAtPath(idmapPath)
		if idmapMounted {
			if idmapEntry.Source != luksPath {
				return RootfsHandle{}, fmt.Errorf("foreign mount at %s (source=%s, expected=%s); refusing to coexist", idmapPath, idmapEntry.Source, luksPath)
			}
		} else {
			idmapConfig := fsutil.IDMapConfig{
				AppUID:      meta.IDMap.AppUID,
				AppGID:      meta.IDMap.AppGID,
				SubUIDStart: meta.IDMap.SubUIDStart,
				SubUIDCount: meta.IDMap.SubUIDCount,
				SubGIDStart: meta.IDMap.SubGIDStart,
				SubGIDCount: meta.IDMap.SubGIDCount,
			}
			if err := fsutil.CreateIDMappedMount(mountDir, idmapPath, idmapConfig); err != nil {
				return RootfsHandle{}, fmt.Errorf("idmap mount: %w", err)
			}
		}
	}

	// Track state.
	resultPath := mountDir
	if idmapPath != "" {
		resultPath = idmapPath
	}

	handle := RootfsHandle{
		VolumeID:  volumeID,
		MountPath: resultPath,
		ReadOnly:  meta.ReadOnly || meta.Type == volumeTypeGolden,
		GoldenLV:  meta.GoldenLV,
	}

	success = true

	return handle, nil
}

// DetachRootfs unmounts and deactivates a rootfs volume. Acquires the
// per-volume lock; concurrent transitions on the same volumeID serialize.
func (m *luksVolumeManager) DetachRootfs(ctx context.Context, volumeID string) error {
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()
	return m.detachRootfsLocked(ctx, volumeID)
}

// detachRootfsLocked is the lock-already-held variant. Used by
// destroyRootfsLocked (DestroyVolume's rootfs branch) which acquires the
// lock at its entry. Walks LiveLayers (kernel-state truth) top-down — no
// dependency on the in-memory rootfsMounts cache.
func (m *luksVolumeManager) detachRootfsLocked(ctx context.Context, volumeID string) error {
	layers, err := m.LiveLayers(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("enumerate live layers for %s: %w", volumeID, err)
	}
	if len(layers) == 0 {
		return nil // already detached
	}

	errs := m.tearDownLayers(ctx, layers)

	// Side-state cleanup: clear cooldown so a subsequent re-attach starts fresh.
	m.mu.Lock()
	delete(m.volumeResizeCooldown, volumeID)
	m.mu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("detach rootfs %s: %v", volumeID, errs)
	}
	return nil
}

// DestroyRootfs permanently removes a workspace or service-rootfs volume.
// Golden content is reference-owned and may only be removed by its
// reference-aware garbage-collection path.
// Acquires the per-volume lock; concurrent transitions on the same volumeID
// serialize. Side-state cleanup (m.locks, schedules, cooldowns, unknown counter)
// is owned by DestroyVolume — DestroyRootfs is one of its dispatch branches
// and runs under the same lock.
func (m *luksVolumeManager) DestroyRootfs(ctx context.Context, volumeID string) error {
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()
	return m.destroyRootfsLocked(ctx, volumeID)
}

// destroyRootfsLocked is the lock-already-held variant used by
// DestroyVolume's rootfs dispatch branch — and also reachable directly
// via DestroyRootfs (app teardown, tuple GC). Both paths must purge the
// per-volume side state (unknownCounter, volumeResizeCooldown) so a
// recreated rootfs with the same volumeID doesn't inherit a sticky
// AttachStateKernelStateCorrupted from the prior life.
func (m *luksVolumeManager) destroyRootfsLocked(ctx context.Context, volumeID string) error {
	if strings.HasPrefix(volumeID, goldenLVPrefix) {
		return fmt.Errorf(
			"refuse generic destruction of golden content %s; use reference-aware garbage collection",
			volumeID,
		)
	}
	metaDir := paths.VolumeMetaDir(volumeID)
	metaPath := filepath.Join(metaDir, metadataV2File)
	mountDir := paths.MountDir(volumeID)
	meta, metaErr := readVolumeMetaV3(metaPath)
	if metaErr == nil && meta.Type == volumeTypeGolden {
		return fmt.Errorf(
			"refuse generic destruction of golden content %s; use reference-aware garbage collection",
			volumeID,
		)
	}

	// Detach first (lock-already-held). detachRootfsLocked clears the
	// resize cooldown.
	_ = m.detachRootfsLocked(ctx, volumeID)

	// Side-state purge (must run on every destroy path, including direct
	// DestroyRootfs callers — codex4-P2).
	defer m.unknownCounter.Delete(volumeID)

	if metaErr != nil {
		if os.IsNotExist(metaErr) {
			return nil
		}
		return fmt.Errorf("read metadata: %w", metaErr)
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

type goldenLiveStack struct {
	mounted       bool
	mountReadOnly bool
	mapperOpen    bool
}

func (m *luksVolumeManager) inspectGoldenLiveStack(goldenID string) (goldenLiveStack, error) {
	mapper := volMapperName(goldenID)
	reader := m.kernelSnapshotFn
	if reader == nil {
		reader = readLiveKernelSnapshot
	}
	snapshot, err := reader([]string{mapper})
	if err != nil {
		return goldenLiveStack{}, fmt.Errorf("inspect golden LV %s kernel stack: %w", goldenID, err)
	}

	mounted, mount := snapshot.lookupMount(paths.MountDir(goldenID))
	if mounted && !mountSourceMatchesMapper(snapshot, mount, mapper) {
		return goldenLiveStack{}, fmt.Errorf(
			"refuse to clean golden LV %s: foreign mount at %s",
			goldenID,
			paths.MountDir(goldenID),
		)
	}
	_, mapperOpen := snapshot.dmByName[mapper]
	return goldenLiveStack{
		mounted:       mounted,
		mountReadOnly: mounted && mount.ReadOnly,
		mapperOpen:    mapperOpen,
	}, nil
}

func (m *luksVolumeManager) goldenLVExists(ctx context.Context, goldenID string) (bool, error) {
	if m.lvMgr == nil {
		return false, fmt.Errorf("golden LV manager is not configured")
	}
	exists, err := m.lvMgr.LVExistsExact(ctx, goldenID)
	if err != nil {
		return false, fmt.Errorf("inspect golden LV %s: %w", goldenID, err)
	}
	return exists, nil
}

// destroyGoldenLVLocked is the single persistence-layer destruction primitive
// for golden content. OCI-image rootfs, OCI-artifact and Hugging Face content
// all use this exact path: source/projection metadata never changes teardown
// order or failure handling. The caller holds the golden identity/storage lock.
func (m *luksVolumeManager) destroyGoldenLVLocked(ctx context.Context, goldenID string) error {
	// Ready cache is derived from durable metadata and physical storage. Evict
	// it before teardown so any cleanup failure leaves an unproven
	// orphan, never cache-valid content.
	m.mu.Lock()
	delete(m.goldenLVs, goldenID)
	m.mu.Unlock()

	mapper := volMapperName(goldenID)
	stack, err := m.inspectGoldenLiveStack(goldenID)
	if err != nil {
		return err
	}
	lvExists, err := m.goldenLVExists(ctx, goldenID)
	if err != nil {
		return err
	}

	mounted := stack.mounted
	mapperOpen := stack.mapperOpen
	lvActive := lvExists
	if err := m.teardownGoldenStaging(
		ctx,
		paths.MountDir(goldenID),
		mapper,
		goldenID,
		&mounted,
		&mapperOpen,
		&lvActive,
		goldenStagingTeardownDelay,
	); err != nil {
		return fmt.Errorf("settle golden LV %s before removal: %w", goldenID, err)
	}

	// Command success is not sufficient destruction authority. Re-probe the
	// kernel stack before removing the LV beneath it.
	stack, err = m.inspectGoldenLiveStack(goldenID)
	if err != nil {
		return err
	}
	if stack.mounted || stack.mapperOpen {
		return fmt.Errorf(
			"golden LV %s stack remains live after teardown (mounted=%v mapper_open=%v)",
			goldenID,
			stack.mounted,
			stack.mapperOpen,
		)
	}

	var removeErr error
	if lvExists {
		if err := m.lvMgr.RemoveThinLV(ctx, goldenID); err != nil {
			removeErr = fmt.Errorf("remove golden LV %s: %w", goldenID, err)
		}
	}
	stillExists, verifyErr := m.goldenLVExists(ctx, goldenID)
	if verifyErr != nil {
		if removeErr != nil {
			return errors.Join(removeErr, fmt.Errorf("verify golden LV %s removal: %w", goldenID, verifyErr))
		}
		return fmt.Errorf("verify golden LV %s removal: %w", goldenID, verifyErr)
	}
	if stillExists {
		if removeErr != nil {
			return removeErr
		}
		return fmt.Errorf("golden LV %s still exists after removal", goldenID)
	}

	if err := os.Remove(paths.MountDir(goldenID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove golden mount directory %s: %w", goldenID, err)
	}
	if err := os.RemoveAll(paths.VolumeMetaDir(goldenID)); err != nil {
		return fmt.Errorf("remove golden metadata %s: %w", goldenID, err)
	}
	return nil
}

// settleReadyGoldenLocked enforces the compatibility boundary for Ready
// content created by older daemons. A read-only mounted golden is a legitimate
// consumer attachment; every other live creation layer is settled without
// deleting the verified content.
func (m *luksVolumeManager) settleReadyGoldenLocked(ctx context.Context, goldenID string) error {
	stack, err := m.inspectGoldenLiveStack(goldenID)
	if err != nil {
		return err
	}
	lvExists, err := m.goldenLVExists(ctx, goldenID)
	if err != nil {
		return err
	}
	if !lvExists {
		return fmt.Errorf("Ready golden LV %s is missing", goldenID)
	}

	if stack.mounted && stack.mountReadOnly {
		if !stack.mapperOpen {
			return fmt.Errorf("Ready golden LV %s has a read-only mount without its mapper", goldenID)
		}
		return nil
	}

	mounted := stack.mounted
	mapperOpen := stack.mapperOpen
	// Exact existence does not expose activation state. Deactivation is
	// idempotent, so run it for every detached/legacy-staging shape to prove
	// the Ready LV is settled.
	lvActive := true
	if err := m.teardownGoldenStaging(
		ctx,
		paths.MountDir(goldenID),
		volMapperName(goldenID),
		goldenID,
		&mounted,
		&mapperOpen,
		&lvActive,
		goldenStagingTeardownDelay,
	); err != nil {
		return fmt.Errorf("settle Ready golden LV %s: %w", goldenID, err)
	}

	stack, err = m.inspectGoldenLiveStack(goldenID)
	if err != nil {
		return err
	}
	if stack.mounted || stack.mapperOpen {
		return fmt.Errorf(
			"Ready golden LV %s remains live after settlement (mounted=%v mapper_open=%v)",
			goldenID,
			stack.mounted,
			stack.mapperOpen,
		)
	}
	return nil
}

// GarbageCollectGoldenLVs removes golden LVs with no remaining references.
func (m *luksVolumeManager) GarbageCollectGoldenLVs(ctx context.Context) error {
	volIDs, err := listVolumeIDs()
	if err != nil {
		return err
	}

	// Collect all golden LVs and their references.
	goldenIDs := make(map[string]bool)
	referencedGoldens := make(map[string]bool)

	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
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
		if meta.Type == volumeTypeGolden {
			goldenIDs[volID] = true
		}
		if meta.GoldenLV != "" {
			referencedGoldens[meta.GoldenLV] = true
		}
	}
	artifactReferences, err := listArtifactReferences()
	if err != nil {
		return fmt.Errorf("list artifact golden references: %w", err)
	}
	for _, reference := range artifactReferences {
		referencedGoldens[reference.GoldenID] = true
	}

	// Remove unreferenced golden LVs.
	var gcErrs []error
	for goldenID := range goldenIDs {
		if referencedGoldens[goldenID] {
			continue
		}
		log.Printf("GC: removing unreferenced golden LV %s", goldenID)
		meta, err := readVolumeMetaV3(filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File))
		if err != nil {
			continue
		}
		identity, err := goldenIdentityFromMeta(meta)
		if err != nil {
			continue
		}
		unlockIdentity, err := m.lockGoldenIdentity(identity)
		if err != nil {
			continue
		}

		// The metadata used to choose the lock was read before lock acquisition.
		// Re-read it now and abandon this pass if the Ready identity changed or
		// disappeared; the old read is not destruction authority.
		lockedMeta, lockedIdentity, lockedErr := readReadyGoldenMeta(goldenID)
		if lockedErr != nil || lockedIdentity != identity || !goldenMetaMatchesIdentity(lockedMeta, identity) {
			unlockIdentity()
			continue
		}

		// References may have been published after the inventory above. Recheck
		// both rootfs metadata and artifact records under the same identity lock
		// used by their attachment paths immediately before destruction.
		rootfsReferenced, refErr := goldenHasRootfsReference(goldenID)
		if refErr != nil {
			unlockIdentity()
			return fmt.Errorf("recheck rootfs golden references: %w", refErr)
		}
		if rootfsReferenced {
			unlockIdentity()
			continue
		}
		artifactReferenced, refErr := goldenHasArtifactReference(goldenID)
		if refErr != nil {
			unlockIdentity()
			return fmt.Errorf("recheck artifact golden references: %w", refErr)
		}
		if artifactReferenced {
			unlockIdentity()
			continue
		}
		destroyErr := m.destroyGoldenLVLocked(ctx, goldenID)
		unlockIdentity()
		if destroyErr != nil {
			gcErrs = append(gcErrs, fmt.Errorf("GC golden LV %s: %w", goldenID, destroyErr))
		}
	}

	return errors.Join(gcErrs...)
}

// ReconcileRootfsStates validates rootfs volumes on startup.
func (m *luksVolumeManager) ReconcileRootfsStates(ctx context.Context) error {
	volIDs, err := listVolumeIDs()
	if err != nil {
		return err
	}
	var reconcileErrs []error

	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
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
		case volumeTypeGolden:
			if goldenReadyTimestamp(meta) != "" {
				identity, identityErr := goldenIdentityFromMeta(meta)
				if identityErr != nil {
					reconcileErrs = append(
						reconcileErrs,
						fmt.Errorf("reconcile Ready golden LV %s identity: %w", volID, identityErr),
					)
					continue
				}
				unlockIdentity, lockErr := m.lockGoldenIdentity(identity)
				if lockErr != nil {
					reconcileErrs = append(
						reconcileErrs,
						fmt.Errorf("lock Ready golden LV %s: %w", volID, lockErr),
					)
					continue
				}
				settleErr := m.settleReadyGoldenLocked(ctx, volID)
				if settleErr == nil {
					m.mu.Lock()
					m.goldenLVs[volID] = meta
					m.mu.Unlock()
				}
				unlockIdentity()
				if settleErr != nil {
					reconcileErrs = append(reconcileErrs, settleErr)
				}
			} else {
				// Incomplete golden LV: destroy.
				log.Printf("reconcile: destroying incomplete golden LV %s", volID)
				storageKey := strings.TrimPrefix(volID, goldenLVPrefix)
				mu := m.goldenMutex(storageKey)
				mu.Lock()
				destroyErr := m.destroyGoldenLVLocked(ctx, volID)
				mu.Unlock()
				if destroyErr != nil {
					reconcileErrs = append(
						reconcileErrs,
						fmt.Errorf("reconcile incomplete golden LV %s: %w", volID, destroyErr),
					)
				}
			}

		case volumeTypeWorkspace, volumeTypeServiceRootfs:
			// Validate metadata is parseable. Actual re-attach happens lazily
			// when AppManager starts the container.
			log.Printf("reconcile: rootfs volume %s (type=%s) present", volID, meta.Type)
		}
	}

	// GC golden LVs.
	if err := m.GarbageCollectGoldenLVs(ctx); err != nil {
		reconcileErrs = append(reconcileErrs, err)
	}
	return errors.Join(reconcileErrs...)
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
	case volumeTypeWorkspace:
		return workspaceLVPrefix + instanceID
	case volumeTypeServiceRootfs:
		return svcRootfsLVPrefix + instanceID
	default:
		return instanceID
	}
}

// FindGoldenByImageRef scans the in-memory golden LV cache for a completed
// image-rootfs projection matching imageRef. Linear scan — fine for the
// expected scale (10-30 golden LVs). When multiple entries match (mutable tag
// pulled at different times), returns the most recently materialized one.
func (m *luksVolumeManager) FindGoldenByImageRef(imageRef string) (digest string, goldenID string, found bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var bestTime time.Time
	for candidateID, meta := range m.goldenLVs {
		if meta == nil || meta.BaseImageRef != imageRef || strings.TrimSpace(meta.BaseImageDigest) == "" {
			continue
		}
		legacyImage := meta.GoldenIdentity == nil && meta.FlattenComplete != ""
		typedImage := meta.GoldenIdentity != nil &&
			meta.GoldenIdentity.SourceKind == GoldenSourceOCI &&
			meta.GoldenIdentity.Projection == GoldenProjectionOCIImageRootfs
		if !legacyImage && !typedImage {
			continue
		}
		readyAt := goldenReadyTimestamp(meta)
		if readyAt == "" {
			continue
		}
		if _, err := m.ReadGoldenImageConfig(context.Background(), candidateID); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, readyAt)
		if err != nil {
			continue
		}
		if !found || t.After(bestTime) {
			bestTime = t
			digest = meta.BaseImageDigest
			goldenID = candidateID
			found = true
		}
	}
	return
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

// ReadRootfsImageIdentity returns the image provenance recorded for a rootfs
// volume. Missing or incomplete provenance means the active rootfs cannot be
// used as identity proof for mutable-image update planning.
func (m *luksVolumeManager) ReadRootfsImageIdentity(volumeID string) (RootfsImageIdentity, error) {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return RootfsImageIdentity{}, fmt.Errorf("rootfs volume id is empty")
	}
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return RootfsImageIdentity{}, fmt.Errorf("read rootfs metadata %s: %w", volumeID, err)
	}
	if meta.Type != volumeTypeServiceRootfs {
		return RootfsImageIdentity{}, fmt.Errorf("volume %s is type %s, not service rootfs", volumeID, meta.Type)
	}
	if strings.TrimSpace(meta.BaseImageDigest) == "" {
		return RootfsImageIdentity{}, fmt.Errorf("volume %s missing base image digest", volumeID)
	}
	return RootfsImageIdentity{
		VolumeID:        volumeID,
		BaseImageRef:    meta.BaseImageRef,
		BaseImageDigest: meta.BaseImageDigest,
	}, nil
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

// ResizeWorkspace grows a workspace volume to the specified size. Acquires
// the per-volume lock; concurrent transitions on the same volumeID
// serialize. Auto-grow uses tryResizeWorkspace (TryLock variant) to avoid
// blocking the resize-monitor loop.
func (m *luksVolumeManager) ResizeWorkspace(ctx context.Context, volumeID string, newSizeBytes int64) error {
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()
	return m.resizeWorkspaceLocked(ctx, volumeID, newSizeBytes)
}

// tryResizeWorkspace attempts to acquire the per-volume lock without
// blocking; on contention returns (false, nil). Used by auto-grow.
func (m *luksVolumeManager) tryResizeWorkspace(ctx context.Context, volumeID string, newSizeBytes int64) (bool, error) {
	lock := m.lockFor(volumeID)
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()
	return true, m.resizeWorkspaceLocked(ctx, volumeID, newSizeBytes)
}

func (m *luksVolumeManager) resizeWorkspaceLocked(ctx context.Context, volumeID string, newSizeBytes int64) error {
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if meta.Type != volumeTypeWorkspace {
		return fmt.Errorf("volume %s is type %s, not workspace", volumeID, meta.Type)
	}
	if newSizeBytes <= meta.SizeBytes {
		return fmt.Errorf("new size %d must be larger than current %d", newSizeBytes, meta.SizeBytes)
	}
	if newSizeBytes > workspaceMaxVirtualSize {
		return fmt.Errorf("new size %d exceeds max %d", newSizeBytes, workspaceMaxVirtualSize)
	}

	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return fmt.Errorf("pool capacity: %w", err)
	}

	// Probe under the per-volume lock — AttachStateOf can't churn
	// concurrently. liveResizeFromState fails closed on ambiguous /
	// hostile states (Foreign, Unknown, Corrupted, Stale) so meta
	// never advances while kernel-state truth is unclear.
	state, err := m.attachStateOfUnderLock(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("probe attach state for resize: %w", err)
	}
	mountPath := paths.MountDir(volumeID)
	live, err := liveResizeFromState(state, volMapperName(volumeID), func(ctx context.Context) error {
		if err := m.run.Run(ctx, "btrfs", "filesystem", "resize", "max", mountPath); err != nil {
			return fmt.Errorf("btrfs resize: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := m.resizeConverge(ctx, meta, metaPath, newSizeBytes, live); err != nil {
		return err
	}

	// Record cooldown to prevent the auto-resize monitor from racing.
	m.mu.Lock()
	m.volumeResizeCooldown[volumeID] = time.Now()
	m.mu.Unlock()

	log.Printf("INFO: workspace %s resized to %d bytes", volumeID, newSizeBytes)
	return nil
}

// ResizeApplication grows a per-app application volume (type=service-data)
// to the specified size. Same primitive chain as ResizeWorkspace except
// the filesystem is ext4, so resize2fs is used instead of btrfs online
// grow. Application volumes are not in rootfsMounts; the mount path is
// derived from paths.MountDir and the mapper name from the volume ID.
//
// Acquires the per-volume lock. Auto-grow uses tryResizeApplication to
// avoid blocking.
func (m *luksVolumeManager) ResizeApplication(ctx context.Context, volumeID string, newSizeBytes int64) error {
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()
	return m.resizeApplicationLocked(ctx, volumeID, newSizeBytes)
}

// tryResizeApplication attempts to acquire the per-volume lock without
// blocking. Used by auto-grow.
func (m *luksVolumeManager) tryResizeApplication(ctx context.Context, volumeID string, newSizeBytes int64) (bool, error) {
	lock := m.lockFor(volumeID)
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()
	return true, m.resizeApplicationLocked(ctx, volumeID, newSizeBytes)
}

func (m *luksVolumeManager) resizeApplicationLocked(ctx context.Context, volumeID string, newSizeBytes int64) error {
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if meta.Type != volumeTypeServiceData {
		return fmt.Errorf("volume %s is type %s, not service-data (application)", volumeID, meta.Type)
	}
	if newSizeBytes <= meta.SizeBytes {
		return fmt.Errorf("new size %d must be larger than current %d", newSizeBytes, meta.SizeBytes)
	}

	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return fmt.Errorf("pool capacity: %w", err)
	}

	// Probe under the per-volume lock — AttachStateOf can't churn
	// concurrently. liveResizeFromState fails closed on ambiguous /
	// hostile states so meta never advances while kernel-state truth
	// is unclear.
	state, err := m.attachStateOfUnderLock(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("probe attach state for resize: %w", err)
	}
	mapper := volMapperName(volumeID)
	mapperPath := "/dev/mapper/" + mapper
	live, err := liveResizeFromState(state, mapper, func(ctx context.Context) error {
		if err := m.run.Run(ctx, "resize2fs", mapperPath); err != nil {
			return fmt.Errorf("resize2fs: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := m.resizeConverge(ctx, meta, metaPath, newSizeBytes, live); err != nil {
		return err
	}

	// Record cooldown to prevent the auto-resize monitor from racing.
	m.mu.Lock()
	m.volumeResizeCooldown[volumeID] = time.Now()
	m.mu.Unlock()

	log.Printf("INFO: application volume %s resized to %d bytes", volumeID, newSizeBytes)
	return nil
}
