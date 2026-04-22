package persistence

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"piccolod/internal/crypt"
	"piccolod/internal/cryptoutil"
	"piccolod/internal/events"
	"piccolod/internal/fsutil"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/blockdev"
	"piccolod/internal/storage/drbd"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/storage/nbd"
)

const (
	metadataV2Version = 2
	metadataV3Version = 3
	metadataV2File    = "piccolo.volume.json"

	controlPlaneLoopFile = "control-plane.luks"
	controlPlaneSize     = 256 << 20 // 256 MiB

	ephLVPrefix    = "eph-"
	appLVPrefix    = "vol-"
	ephDefaultSize = 50 << 30 // 50 GiB (thin-provisioned — only written blocks consume physical space)
)

// volumeMetaV2 is the on-disk metadata schema for block-native volumes.
type volumeMetaV2 struct {
	Version    int    `json:"version"`
	Type       string `json:"type"` // "luks-loop" or "luks-thinlv"
	WrappedKey string `json:"wrapped_key"`
	Nonce      string `json:"nonce"`
	LVName     string `json:"lv_name,omitempty"`     // luks-thinlv only
	VGName     string `json:"vg_name,omitempty"`     // luks-thinlv only
	LoopFile   string `json:"loop_file,omitempty"`   // luks-loop only
	SizeBytes  int64  `json:"size_bytes"`
	FSType     string `json:"fs_type"`
}

// RoleCheckable allows setting a role checker function.
type RoleCheckable interface {
	SetRoleChecker(fn func(string, VolumeRole) bool)
}

// Reconcilable allows reconciling all volume states on startup.
type Reconcilable interface {
	ReconcileAllVolumeStates() error
}

// luksVolumeManager implements VolumeManager, RootfsVolumeManager,
// RoleCheckable, and Reconcilable.
// It dispatches to LUKSLoopVolume for control volumes and DeviceStack + LUKS
// for application volumes. For rootfs volumes, it manages golden LVs and
// idmapped mounts. For ephemeral volumes, it manages unencrypted thin LVs
// with btrfs+zstd.
type luksVolumeManager struct {
	run      runner.CommandRunner
	crypto   *crypt.Manager
	bus      *events.Bus
	tmpfsDir string // directory for ephemeral key material (default: /run/piccolo)

	// Block device stack dependencies.
	lvMgr   *lvm.LVManager
	poolMgr *lvm.PoolManager
	nbdSrv  *nbd.Server
	drbdMgr *drbd.ResourceManager

	// LUKS loop for control plane.
	loopVol *LUKSLoopVolume

	// Flatten function: extracts OCI image to a directory and returns image config.
	// When prePulledDir is non-empty, the image is already pulled in that podman
	// root directory — the function should skip the pull step and reuse it.
	flattenFn func(ctx context.Context, imageRef, targetDir, prePulledDir string) (GoldenImageConfig, error)
	// ImageSizeFn returns the uncompressed image size in bytes for right-sizing golden LVs.
	imageSizeFn func(ctx context.Context, imageRef string) (int64, error)

	mu          sync.Mutex
	roleChecker func(string, VolumeRole) bool
	stacks      map[string]*blockdev.DeviceStack // volumeID → active stack

	// Golden LV management.
	goldenLVs    map[string]*volumeMetaV3 // imageDigestShort → meta cache
	goldenMu     map[string]*sync.Mutex   // per-image-digest lock
	goldenMuLock sync.Mutex               // protects goldenMu map

	// Rootfs mount tracking.
	rootfsMounts map[string]*rootfsMountState // volumeID → mount state

	// Workspace resize monitor.
	wsResizeCancel   context.CancelFunc
	wsResizeCooldown map[string]time.Time // volumeID → last resize time
}

// rootfsMountState tracks the full device stack for a mounted rootfs volume.
type rootfsMountState struct {
	stack      *blockdev.DeviceStack
	luksMapper string
	rawMountPath string // raw btrfs mount (used for unmount)
	idmapPath    string // idmapped bind mount (used for unmount)
	handle     RootfsHandle // caller-facing handle — single source of truth for cached returns
}

// LUKSVolumeManagerConfig holds dependencies for the unified volume manager.
type LUKSVolumeManagerConfig struct {
	Run     runner.CommandRunner
	Crypto  *crypt.Manager
	Bus     *events.Bus
	LVMgr   *lvm.LVManager
	PoolMgr *lvm.PoolManager
	NBDSrv  *nbd.Server
	DRBDMgr *drbd.ResourceManager
	// FlattenFn extracts an OCI image to a target directory and returns image config.
	// When prePulledDir is non-empty, the image is already pulled there — skip pull.
	FlattenFn func(ctx context.Context, imageRef, targetDir, prePulledDir string) (GoldenImageConfig, error)
	// ImageSizeFn returns the uncompressed image size in bytes for right-sizing golden LVs.
	ImageSizeFn func(ctx context.Context, imageRef string) (int64, error)
}

// NewLUKSVolumeManager creates the unified volume manager.
func NewLUKSVolumeManager(cfg LUKSVolumeManagerConfig) *luksVolumeManager {
	return &luksVolumeManager{
		run:          cfg.Run,
		crypto:       cfg.Crypto,
		bus:          cfg.Bus,
		tmpfsDir:     "/run/piccolo",
		lvMgr:        cfg.LVMgr,
		poolMgr:      cfg.PoolMgr,
		nbdSrv:       cfg.NBDSrv,
		drbdMgr:      cfg.DRBDMgr,
		flattenFn:    cfg.FlattenFn,
		imageSizeFn:  cfg.ImageSizeFn,
		loopVol:      NewLUKSLoopVolume(cfg.Run),
		stacks:       make(map[string]*blockdev.DeviceStack),
		goldenLVs:    make(map[string]*volumeMetaV3),
		goldenMu:     make(map[string]*sync.Mutex),
		rootfsMounts:      make(map[string]*rootfsMountState),
		wsResizeCooldown:  make(map[string]time.Time),
	}
}

// SetRoleChecker sets the function used to check if a volume operation
// is permitted for a given role.
func (m *luksVolumeManager) SetRoleChecker(fn func(string, VolumeRole) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roleChecker = fn
}

// ReconcileAllVolumeStates scans persisted volume metadata and validates
// consistency on startup. Skips v3 rootfs volumes (handled by ReconcileRootfsStates).
func (m *luksVolumeManager) ReconcileAllVolumeStates() error {
	volIDs, err := listVolumeIDs()
	if err != nil {
		return err
	}

	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
		if _, err := os.Stat(metaPath); os.IsNotExist(err) {
			continue
		}
		version, err := readVolumeMetaVersion(metaPath)
		if err != nil {
			log.Printf("WARN: volume %s metadata corrupted: %v", volID, err)
			continue
		}
		switch version {
		case metadataV2Version:
			if _, err := readVolumeMetaV2(metaPath); err != nil {
				log.Printf("WARN: volume %s v2 metadata corrupted: %v", volID, err)
			}
		case metadataV3Version:
			meta, err := readVolumeMetaV3(metaPath)
			if err != nil {
				log.Printf("WARN: volume %s v3 metadata corrupted: %v", volID, err)
				continue
			}
			// Skip rootfs types — handled by ReconcileRootfsStates.
			switch meta.Type {
			case "golden", "workspace", "service-rootfs":
				continue
			case "service-data", "ephemeral":
				// Validate parseable, nothing else needed.
			}
		default:
			log.Printf("WARN: volume %s has unsupported metadata version %d", volID, version)
		}
	}
	return nil
}

// ReconcileOrphanLVs scans the LVM thin pool for LVs that have no corresponding
// metadata in /piccolo-core/volumes/. This handles stale LVs left after an OS
// reinstall (metadata on root disk is wiped, but LVs on the data partition persist).
// Must be called after pool activation.
func (m *luksVolumeManager) ReconcileOrphanLVs(ctx context.Context) error {
	if m.lvMgr == nil {
		return nil
	}

	lvs, err := m.lvMgr.ListLVs(ctx)
	if err != nil {
		return fmt.Errorf("list LVs: %w", err)
	}

	volIDs, err := listVolumeIDs()
	if err != nil {
		return fmt.Errorf("list volume IDs: %w", err)
	}

	// Build set of known LV names from metadata. If metadata exists but
	// can't be parsed (corruption, partial write), protect the LV by adding
	// all plausible LV name derivations — do not treat unreadable metadata
	// as proof that the LV is orphaned.
	knownLVs := make(map[string]bool, len(volIDs))
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
		version, _ := readVolumeMetaVersion(metaPath)
		parsed := false
		switch version {
		case metadataV2Version:
			if meta, err := readVolumeMetaV2(metaPath); err == nil {
				knownLVs[meta.LVName] = true
				parsed = true
			}
		case metadataV3Version:
			if meta, err := readVolumeMetaV3(metaPath); err == nil {
				knownLVs[meta.LVName] = true
				parsed = true
			}
		}
		if !parsed {
			// Metadata unreadable — protect all possible LV names for this volume ID.
			knownLVs[volID] = true                   // golden-*, ws-*, svc-rootfs-*
			knownLVs[ephLVPrefix+volID] = true        // eph-*
			knownLVs[appLVPrefix+volID] = true        // vol-*
		}
	}

	// Piccolo LV prefixes — any LV not matching these is not ours.
	piccoloPrefixes := []string{
		ephLVPrefix,
		appLVPrefix,
		goldenLVPrefix,
		workspaceLVPrefix,
		svcRootfsLVPrefix,
	}

	var errs []error
	for _, lv := range lvs {
		if knownLVs[lv.Name] {
			continue
		}
		// Skip the thin pool LV itself.
		if lv.Name == lvm.DefaultThinPoolName {
			continue
		}
		// Skip tuple-managed LVs: data snapshots and failed rollback LVs.
		if strings.HasPrefix(lv.Name, "snap-") {
			continue
		}
		if strings.Contains(lv.Name, "--failed-gen") {
			continue
		}
		// Only touch LVs with known piccolo prefixes.
		isPiccolo := false
		for _, prefix := range piccoloPrefixes {
			if strings.HasPrefix(lv.Name, prefix) {
				isPiccolo = true
				break
			}
		}
		if !isPiccolo {
			continue
		}

		log.Printf("WARN: removing orphan LV %s (no metadata found)", lv.Name)
		if lv.Active {
			_ = m.lvMgr.DeactivateLV(ctx, lv.Name)
		}
		if err := m.lvMgr.RemoveThinLV(ctx, lv.Name); err != nil {
			log.Printf("WARN: failed to remove orphan LV %s: %v", lv.Name, err)
			errs = append(errs, fmt.Errorf("remove %s: %w", lv.Name, err))
		}
	}
	return errors.Join(errs...)
}

// EnsureVolume creates a volume if it doesn't exist, or returns an existing one.
// On a fresh system (before /crypto/setup), the crypto manager is not initialized.
// In that case, we return a handle with the expected mount path without creating
// the volume — the actual creation happens during the setup flow.
func (m *luksVolumeManager) EnsureVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error) {
	handle := VolumeHandle{
		ID:       req.ID,
		MountDir: paths.MountDir(req.ID),
	}

	metaDir := paths.VolumeMetaDir(req.ID)
	metaPath := filepath.Join(metaDir, metadataV2File)

	// Check if volume already exists.
	if _, err := os.Stat(metaPath); err == nil {
		return handle, nil
	}

	// Ephemeral volumes bypass the crypto gate — no LUKS, no crypto dependency.
	if req.Class == VolumeClassEphemeral {
		return m.ensureEphemeralVolume(ctx, req)
	}

	// If crypto is not initialized yet (fresh system before setup), return
	// a handle without creating the volume. Attach will fail with ErrLocked
	// until the setup flow creates and initializes the volume.
	if m.crypto == nil || !m.crypto.IsInitialized() {
		return handle, nil
	}

	switch req.Class {
	case VolumeClassControl:
		return m.ensureControlVolume(ctx, req)
	case VolumeClassApplication:
		return m.ensureAppVolume(ctx, req)
	default:
		return VolumeHandle{}, fmt.Errorf("unknown volume class: %s", req.Class)
	}
}

// Attach mounts a volume, making it available for I/O.
// Dispatches based on metadata version: v2 uses per-volume wrapped keys,
// v3 service-data uses the pool keyfile. v3 rootfs types must use AttachRootfs.
func (m *luksVolumeManager) Attach(ctx context.Context, handle VolumeHandle, opts AttachOptions) error {
	metaPath := filepath.Join(paths.VolumeMetaDir(handle.ID), metadataV2File)

	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrLocked
		}
		return fmt.Errorf("read volume metadata version: %w", err)
	}

	switch version {
	case metadataV2Version:
		meta, err := readVolumeMetaV2(metaPath)
		if err != nil {
			return fmt.Errorf("read v2 metadata: %w", err)
		}
		switch meta.Type {
		case "luks-loop":
			return m.attachControlVolume(ctx, handle, meta)
		case "luks-thinlv":
			return m.attachAppVolume(ctx, handle, meta, opts)
		default:
			return fmt.Errorf("unknown v2 volume type: %s", meta.Type)
		}

	case metadataV3Version:
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			return fmt.Errorf("read v3 metadata: %w", err)
		}
		switch meta.Type {
		case "service-data":
			return m.attachAppVolumeV3(ctx, handle, meta, opts)
		case "ephemeral":
			return m.attachEphemeralVolume(ctx, handle, meta)
		case "golden", "workspace", "service-rootfs":
			return fmt.Errorf("rootfs volume %s (type=%s): use AttachRootfs instead", handle.ID, meta.Type)
		default:
			return fmt.Errorf("unknown v3 volume type: %s", meta.Type)
		}

	default:
		return fmt.Errorf("%w: unsupported version %d", ErrVolumeMetadataCorrupted, version)
	}
}

// Detach unmounts a volume and tears down its device stack.
func (m *luksVolumeManager) Detach(ctx context.Context, handle VolumeHandle) error {
	metaPath := filepath.Join(paths.VolumeMetaDir(handle.ID), metadataV2File)
	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		return fmt.Errorf("read volume metadata version: %w", err)
	}

	switch version {
	case metadataV2Version:
		meta, err := readVolumeMetaV2(metaPath)
		if err != nil {
			return fmt.Errorf("read v2 metadata: %w", err)
		}
		switch meta.Type {
		case "luks-loop":
			return m.detachControlVolume(ctx, handle, meta)
		case "luks-thinlv":
			return m.detachAppVolume(ctx, handle)
		default:
			return fmt.Errorf("unknown v2 volume type: %s", meta.Type)
		}
	case metadataV3Version:
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			return fmt.Errorf("read v3 metadata: %w", err)
		}
		switch meta.Type {
		case "service-data":
			return m.detachAppVolume(ctx, handle)
		case "ephemeral":
			return m.detachEphemeralVolume(ctx, handle)
		case "golden", "workspace", "service-rootfs":
			return fmt.Errorf("rootfs volume %s: use DetachRootfs", handle.ID)
		default:
			return fmt.Errorf("unknown v3 volume type: %s", meta.Type)
		}
	default:
		return fmt.Errorf("%w: unsupported version %d", ErrVolumeMetadataCorrupted, version)
	}
}

// DestroyVolume permanently removes a volume and its metadata.
func (m *luksVolumeManager) DestroyVolume(ctx context.Context, id string) error {
	metaDir := paths.VolumeMetaDir(id)
	metaPath := filepath.Join(metaDir, metadataV2File)
	mountDir := paths.MountDir(id)

	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			m.cleanupStaleAppState(ctx, id)
			m.cleanupStaleEphemeralState(ctx, id)
			return nil
		}
		return fmt.Errorf("read volume metadata version: %w", err)
	}

	switch version {
	case metadataV2Version:
		meta, err := readVolumeMetaV2(metaPath)
		if err != nil {
			return fmt.Errorf("read v2 metadata: %w", err)
		}
		if err := m.Detach(ctx, VolumeHandle{ID: id, MountDir: mountDir}); err != nil {
			log.Printf("WARN: detach volume %s during destroy: %v", id, err)
		}
		switch meta.Type {
		case "luks-loop":
			_ = os.Remove(paths.CoreJoin(meta.LoopFile))
		case "luks-thinlv":
			if m.lvMgr != nil && meta.LVName != "" {
				if err := m.lvMgr.RemoveThinLV(ctx, meta.LVName); err != nil {
					log.Printf("WARN: remove thin LV %s: %v", meta.LVName, err)
				}
			}
		}

	case metadataV3Version:
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			return fmt.Errorf("read v3 metadata: %w", err)
		}
		switch meta.Type {
		case "golden", "workspace", "service-rootfs":
			return m.DestroyRootfs(ctx, id)
		case "service-data":
			if err := m.Detach(ctx, VolumeHandle{ID: id, MountDir: mountDir}); err != nil {
				log.Printf("WARN: detach volume %s during destroy: %v", id, err)
			}
			if m.lvMgr != nil && meta.LVName != "" {
				if err := m.lvMgr.RemoveThinLV(ctx, meta.LVName); err != nil {
					log.Printf("WARN: remove thin LV %s: %v", meta.LVName, err)
				}
			}
		case "ephemeral":
			// Detach dispatches to detachEphemeralVolume (no LUKS close).
			if err := m.Detach(ctx, VolumeHandle{ID: id, MountDir: mountDir}); err != nil {
				log.Printf("WARN: detach ephemeral volume %s during destroy: %v", id, err)
			}
			if m.lvMgr != nil && meta.LVName != "" {
				if err := m.lvMgr.RemoveThinLV(ctx, meta.LVName); err != nil {
					log.Printf("WARN: remove ephemeral thin LV %s: %v", meta.LVName, err)
				}
			}
		}

	default:
		return fmt.Errorf("%w: unsupported version %d", ErrVolumeMetadataCorrupted, version)
	}

	if err := os.RemoveAll(metaDir); err != nil {
		return fmt.Errorf("remove metadata dir: %w", err)
	}
	_ = os.RemoveAll(mountDir)
	return nil
}

// cleanupStaleAppState tears down leftover LUKS mappers, LVs, and dirs for a
// volume that has no metadata (e.g., partial creation that was interrupted).
func (m *luksVolumeManager) cleanupStaleAppState(ctx context.Context, id string) {
	mountDir := paths.MountDir(id)
	mapper := "piccolo-vol-" + id
	lvName := appLVPrefix + id

	// Best-effort teardown: unmount → close LUKS → deactivate LV → remove LV.
	if m.run != nil {
		_ = m.run.Run(ctx, "umount", mountDir)
		_ = m.run.Run(ctx, "cryptsetup", "close", mapper)
	}
	if m.lvMgr != nil && m.lvMgr.LVExists(ctx, lvName) {
		_ = m.lvMgr.RemoveThinLV(ctx, lvName)
	}
	_ = os.RemoveAll(paths.VolumeMetaDir(id))
	_ = os.RemoveAll(mountDir)
}

// RoleStream returns a channel that emits role changes for a volume.
// Currently single-node only — emits Leader once and stays.
func (m *luksVolumeManager) RoleStream(volumeID string) (<-chan VolumeRole, error) {
	ch := make(chan VolumeRole, 1)
	ch <- VolumeRoleLeader
	return ch, nil
}

// --- Control volume (LUKS loop) ---

func (m *luksVolumeManager) ensureControlVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error) {
	metaDir := paths.VolumeMetaDir(req.ID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return VolumeHandle{}, fmt.Errorf("create meta dir: %w", err)
	}

	// Generate a random key and wrap it with SDEK.
	keyMaterial, wrappedKey, nonce, err := m.generateWrappedKey(ctx)
	if err != nil {
		return VolumeHandle{}, err
	}
	defer cryptoutil.SecureZero(keyMaterial)

	loopFile := paths.CoreJoin(controlPlaneLoopFile)

	// Initialize the loop volume.
	if err := m.loopVol.Init(ctx, loopFile, controlPlaneSize, keyMaterial); err != nil {
		return VolumeHandle{}, fmt.Errorf("init control loop volume: %w", err)
	}

	// Persist metadata.
	meta := &volumeMetaV2{
		Version:    metadataV2Version,
		Type:       "luks-loop",
		WrappedKey: wrappedKey,
		Nonce:      nonce,
		LoopFile:   controlPlaneLoopFile,
		SizeBytes:  controlPlaneSize,
		FSType:     "ext4",
	}
	if err := writeVolumeMeta(filepath.Join(metaDir, metadataV2File), meta); err != nil {
		return VolumeHandle{}, fmt.Errorf("write metadata: %w", err)
	}

	return VolumeHandle{
		ID:       req.ID,
		MountDir: paths.MountDir(req.ID),
	}, nil
}

func (m *luksVolumeManager) attachControlVolume(ctx context.Context, handle VolumeHandle, meta *volumeMetaV2) error {
	keyMaterial, err := m.unwrapKey(ctx, meta.WrappedKey, meta.Nonce)
	if err != nil {
		return err
	}
	defer cryptoutil.SecureZero(keyMaterial)

	loopFile := paths.CoreJoin(meta.LoopFile)
	return m.loopVol.Open(ctx, loopFile, keyMaterial, handle.MountDir)
}

func (m *luksVolumeManager) detachControlVolume(ctx context.Context, handle VolumeHandle, meta *volumeMetaV2) error {
	if m.loopVol == nil {
		return nil
	}
	loopFile := paths.CoreJoin(meta.LoopFile)
	return m.loopVol.Close(ctx, loopFile, handle.MountDir)
}

// --- Application volume (DeviceStack + LUKS) ---

func (m *luksVolumeManager) ensureAppVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error) {
	metaDir := paths.VolumeMetaDir(req.ID)
	lvName := appLVPrefix + req.ID
	sizeBytes := int64(10 << 30) // 10 GiB default

	if m.lvMgr.LVExists(ctx, lvName) {
		// LV exists from a partial previous attempt. Tear down stale LUKS/mount
		// state so we can re-format the device cleanly.
		m.cleanupStaleAppState(ctx, req.ID)
		// Re-create: remove the old LV and start fresh.
		_ = m.lvMgr.RemoveThinLV(ctx, lvName)
	}
	if err := m.lvMgr.CreateThinLV(ctx, lvName, sizeBytes); err != nil {
		return VolumeHandle{}, fmt.Errorf("create thin LV: %w", err)
	}

	// Build and open the device stack to get the top device path.
	stack, err := m.buildStack(req.ID, lvName, sizeBytes)
	if err != nil {
		return VolumeHandle{}, fmt.Errorf("build device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return VolumeHandle{}, fmt.Errorf("open device stack: %w", err)
	}
	defer stack.Close(ctx)

	// LUKS format with the shared master key (pool keyfile as passphrase).
	topDev := stack.Top().Path()
	if err := m.luksFormatWithMasterKey(ctx, topDev); err != nil {
		return VolumeHandle{}, fmt.Errorf("luks format: %w", err)
	}

	// Open LUKS, mkfs, close.
	mapper := "piccolo-vol-" + req.ID
	if err := m.luksOpenWithPoolKeyfile(ctx, topDev, mapper); err != nil {
		return VolumeHandle{}, fmt.Errorf("luks open for mkfs: %w", err)
	}
	mapperPath := "/dev/mapper/" + mapper
	mkfsErr := m.run.Run(ctx, "mkfs.ext4", "-F", "-m", "1", mapperPath)
	_ = m.run.Run(ctx, "cryptsetup", "close", mapper)
	if mkfsErr != nil {
		return VolumeHandle{}, fmt.Errorf("mkfs.ext4: %w", mkfsErr)
	}

	// Persist v3 metadata.
	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      "service-data",
		LVName:    lvName,
		VGName:    lvm.DefaultVGName,
		SizeBytes: sizeBytes,
		FSType:    "ext4",
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return VolumeHandle{}, fmt.Errorf("create meta dir: %w", err)
	}
	if err := writeVolumeMetaV3(filepath.Join(metaDir, metadataV2File), meta); err != nil {
		return VolumeHandle{}, fmt.Errorf("write metadata: %w", err)
	}

	return VolumeHandle{
		ID:       req.ID,
		MountDir: paths.MountDir(req.ID),
	}, nil
}

func (m *luksVolumeManager) attachAppVolume(ctx context.Context, handle VolumeHandle, meta *volumeMetaV2, opts AttachOptions) error {
	return m.attachAppVolumeCommon(ctx, handle, meta.LVName, meta.SizeBytes, opts,
		func(topDev, mapper string) error {
			keyMaterial, err := m.unwrapKey(ctx, meta.WrappedKey, meta.Nonce)
			if err != nil {
				return err
			}
			defer cryptoutil.SecureZero(keyMaterial)
			return m.luksOpen(ctx, topDev, mapper, keyMaterial)
		})
}

// attachAppVolumeCommon is the shared attach path for v2 and v3 app volumes.
// The luksOpenFn closure handles LUKS opening with the appropriate key material.
func (m *luksVolumeManager) attachAppVolumeCommon(ctx context.Context, handle VolumeHandle, lvName string, sizeBytes int64, opts AttachOptions, luksOpenFn func(topDev, mapper string) error) error {
	// Role check.
	m.mu.Lock()
	checker := m.roleChecker
	m.mu.Unlock()
	if checker != nil && !checker(handle.ID, opts.Role) {
		return fmt.Errorf("role check failed for %s", handle.ID)
	}

	// Build and open the device stack.
	stack, err := m.buildStack(handle.ID, lvName, sizeBytes)
	if err != nil {
		return fmt.Errorf("build device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return fmt.Errorf("open device stack: %w", err)
	}

	// Track the active stack.
	m.mu.Lock()
	m.stacks[handle.ID] = stack
	m.mu.Unlock()

	// Rollback on failure: close LUKS mapper (if opened), close stack, remove tracking.
	mapper := "piccolo-vol-" + handle.ID
	var openedMapper string
	success := false
	defer func() {
		if success {
			return
		}
		if openedMapper != "" {
			m.run.Run(ctx, "cryptsetup", "close", openedMapper)
		}
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
	}()

	// LUKS open.
	topDev := stack.Top().Path()
	if err := luksOpenFn(topDev, mapper); err != nil {
		return fmt.Errorf("luks open: %w", err)
	}
	openedMapper = mapper

	// Mount ext4.
	mountDir := handle.MountDir
	// Ensure the mounts/ parent directory is traversable (0o711) so per-app
	// users can reach their own mount points without being able to list siblings.
	mountsParent := filepath.Dir(mountDir)
	if err := os.MkdirAll(mountsParent, 0o711); err != nil {
		return fmt.Errorf("create mounts parent: %w", err)
	}
	if err := os.Chmod(mountsParent, 0o711); err != nil {
		return fmt.Errorf("chmod mounts parent: %w", err)
	}
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		return fmt.Errorf("create mount dir: %w", err)
	}

	mapperPath := "/dev/mapper/" + mapper
	if err := m.run.Run(ctx, "mount", "-t", "ext4", "-o", "discard", mapperPath, mountDir); err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	// If the LV was resized while detached (via ResizeApplication), the ext4
	// filesystem is still at its old size. Run resize2fs to grow the fs to
	// fill the underlying device. Idempotent — no-op when sizes already match.
	// Mirrors the workspace-mode pattern where btrfs online grow handles the
	// equivalent case at attach time.
	if out, err := m.run.RunWithOutput(ctx, "resize2fs", mapperPath); err != nil {
		return fmt.Errorf("resize2fs on attach: %w: %s", err, strings.TrimSpace(string(out)))
	}

	success = true
	return nil
}

func (m *luksVolumeManager) detachAppVolume(ctx context.Context, handle VolumeHandle) error {
	mapper := "piccolo-vol-" + handle.ID
	var errs []error

	// Unmount with lazy fallback. Continue regardless — the mount may
	// already be gone (systemd race) and cryptsetup close still needs to run.
	if err := m.run.Run(ctx, "umount", handle.MountDir); err != nil {
		log.Printf("umount %s failed, trying lazy unmount", handle.MountDir)
		if lazyErr := m.run.Run(ctx, "umount", "-l", handle.MountDir); lazyErr != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", handle.MountDir, lazyErr))
		}
	}

	if err := m.run.Run(ctx, "cryptsetup", "close", mapper); err != nil {
		errs = append(errs, fmt.Errorf("luks close %s: %w", mapper, err))
	}

	// Close device stack. Always remove from tracking — during shutdown
	// there is no retry opportunity; a stale entry is worse than a missing one.
	m.mu.Lock()
	stack := m.stacks[handle.ID]
	delete(m.stacks, handle.ID)
	m.mu.Unlock()

	if stack != nil {
		if err := stack.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close device stack: %w", err))
		}
	}

	return errors.Join(errs...)
}

// --- Ephemeral volume (unencrypted thin LV + btrfs) ---

func (m *luksVolumeManager) ensureEphemeralVolume(ctx context.Context, req VolumeRequest) (VolumeHandle, error) {
	handle := VolumeHandle{
		ID:       req.ID,
		MountDir: paths.MountDir(req.ID),
	}

	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return VolumeHandle{}, err
	}

	metaDir := paths.VolumeMetaDir(req.ID)
	lvName := ephLVPrefix + req.ID

	if m.lvMgr.LVExists(ctx, lvName) {
		m.cleanupStaleEphemeralState(ctx, req.ID)
	}
	if err := m.lvMgr.CreateThinLV(ctx, lvName, ephDefaultSize); err != nil {
		return VolumeHandle{}, fmt.Errorf("create ephemeral thin LV: %w", err)
	}

	// Build single-element stack (ThinLV only — no NBD/DRBD/LUKS).
	thinDev := blockdev.NewThinLVDevice(m.lvMgr, lvName, ephDefaultSize)
	stack, err := blockdev.NewDeviceStack(req.ID, thinDev)
	if err != nil {
		return VolumeHandle{}, fmt.Errorf("build ephemeral device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return VolumeHandle{}, fmt.Errorf("open ephemeral device stack: %w", err)
	}
	defer stack.Close(ctx)

	if err := m.run.Run(ctx, "mkfs.btrfs", "-f", stack.Top().Path()); err != nil {
		return VolumeHandle{}, fmt.Errorf("mkfs.btrfs: %w", err)
	}

	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      "ephemeral",
		LVName:    lvName,
		VGName:    lvm.DefaultVGName,
		SizeBytes: ephDefaultSize,
		FSType:    "btrfs",
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return VolumeHandle{}, fmt.Errorf("create meta dir: %w", err)
	}
	if err := writeVolumeMetaV3(filepath.Join(metaDir, metadataV2File), meta); err != nil {
		return VolumeHandle{}, fmt.Errorf("write metadata: %w", err)
	}

	log.Printf("INFO: created ephemeral volume %s (LV=%s)", req.ID, lvName)
	return handle, nil
}

func (m *luksVolumeManager) attachEphemeralVolume(ctx context.Context, handle VolumeHandle, meta *volumeMetaV3) error {
	// Idempotent: if already attached, skip.
	m.mu.Lock()
	_, exists := m.stacks[handle.ID]
	m.mu.Unlock()
	if exists {
		return nil
	}

	thinDev := blockdev.NewThinLVDevice(m.lvMgr, meta.LVName, meta.SizeBytes)
	stack, err := blockdev.NewDeviceStack(handle.ID, thinDev)
	if err != nil {
		return fmt.Errorf("build ephemeral device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return fmt.Errorf("open ephemeral device stack: %w", err)
	}

	m.mu.Lock()
	m.stacks[handle.ID] = stack
	m.mu.Unlock()

	success := false
	defer func() {
		if !success {
			stack.Close(ctx)
			m.mu.Lock()
			delete(m.stacks, handle.ID)
			m.mu.Unlock()
		}
	}()

	mountDir := handle.MountDir
	mountsParent := filepath.Dir(mountDir)
	if err := os.MkdirAll(mountsParent, 0o711); err != nil {
		return fmt.Errorf("create mounts parent: %w", err)
	}
	if err := os.Chmod(mountsParent, 0o711); err != nil {
		return fmt.Errorf("chmod mounts parent: %w", err)
	}
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		return fmt.Errorf("create mount dir: %w", err)
	}

	if err := m.run.Run(ctx, "mount", "-t", "btrfs", "-o", btrfsRootfsMountOpts, stack.Top().Path(), mountDir); err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	success = true
	return nil
}

func (m *luksVolumeManager) detachEphemeralVolume(ctx context.Context, handle VolumeHandle) error {
	var errs []error

	// Unmount with lazy fallback. Continue regardless — the mount may
	// already be gone (systemd race) and stack.Close still needs to run.
	if err := m.run.Run(ctx, "umount", handle.MountDir); err != nil {
		log.Printf("umount %s failed, trying lazy unmount", handle.MountDir)
		if lazyErr := m.run.Run(ctx, "umount", "-l", handle.MountDir); lazyErr != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", handle.MountDir, lazyErr))
		}
	}

	// Close device stack. Always remove from tracking — during shutdown
	// there is no retry opportunity; a stale entry is worse than a missing one.
	m.mu.Lock()
	stack := m.stacks[handle.ID]
	delete(m.stacks, handle.ID)
	m.mu.Unlock()

	if stack != nil {
		if err := stack.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close device stack: %w", err))
		}
	}

	return errors.Join(errs...)
}

// cleanupStaleEphemeralState tears down leftover mounts, LVs, and dirs for an
// ephemeral volume that has no metadata (e.g., partial creation that was interrupted).
func (m *luksVolumeManager) cleanupStaleEphemeralState(ctx context.Context, id string) {
	mountDir := paths.MountDir(id)
	lvName := ephLVPrefix + id

	if m.run != nil {
		_ = m.run.Run(ctx, "umount", mountDir)
	}
	if m.lvMgr != nil {
		_ = m.lvMgr.DeactivateLV(ctx, lvName)
		if m.lvMgr.LVExists(ctx, lvName) {
			if err := m.lvMgr.RemoveThinLV(ctx, lvName); err != nil {
				log.Printf("WARN: cleanup stale ephemeral LV %s: %v", lvName, err)
			}
		}
	}
	_ = os.RemoveAll(paths.VolumeMetaDir(id))
	_ = os.RemoveAll(mountDir)
}

// --- Helpers ---

func (m *luksVolumeManager) buildStack(volumeID, lvName string, sizeBytes int64) (*blockdev.DeviceStack, error) {
	thinDev := blockdev.NewThinLVDevice(m.lvMgr, lvName, sizeBytes)

	// Single-node mode: when NBD/DRBD managers are nil, the stack is just
	// the thin LV. LUKS is applied directly on the LV device.
	if m.nbdSrv == nil || m.drbdMgr == nil {
		return blockdev.NewDeviceStack(volumeID, thinDev)
	}

	nbdDev := blockdev.NewNBDDevice(
		m.nbdSrv,
		volumeID,
		m.lvMgr.LVPath(lvName),
		sizeBytes,
		nbd.DefaultHooks(),
	)

	drbdOps := drbd.NewResourceOps(m.run, m.drbdMgr.MetaDir(), drbd.ResourceConfig{
		Name:          volumeID,
		BackingDevice: "", // set dynamically after NBD opens
		NodeID:        0,
	})
	drbdDev := blockdev.NewDRBDDevice(drbdOps, volumeID, sizeBytes)

	return blockdev.NewDeviceStack(volumeID, thinDev, nbdDev, drbdDev)
}

func (m *luksVolumeManager) generateWrappedKey(ctx context.Context) (raw []byte, wrappedKey, nonce string, err error) {
	if m.crypto == nil {
		return nil, "", "", errors.New("crypto manager unavailable")
	}
	raw = make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", "", fmt.Errorf("generate volume key: %w", err)
	}
	err = m.crypto.WithSDEK(func(sdek []byte) error {
		wrappedKey, nonce, err = crypt.SealVolumeKey(sdek, raw)
		return err
	})
	if err != nil {
		cryptoutil.SecureZero(raw)
		return nil, "", "", err
	}
	return raw, wrappedKey, nonce, nil
}

func (m *luksVolumeManager) unwrapKey(ctx context.Context, wrappedKey, nonce string) ([]byte, error) {
	if m.crypto == nil {
		return nil, errors.New("crypto manager unavailable")
	}
	var key []byte
	err := m.crypto.WithSDEK(func(sdek []byte) error {
		var unwrapErr error
		key, unwrapErr = crypt.UnwrapVolumeKey(sdek, wrappedKey, nonce)
		if unwrapErr != nil {
			if errors.Is(unwrapErr, crypt.ErrKeyDataCorrupted) {
				return fmt.Errorf("%w: %v", ErrVolumeMetadataCorrupted, unwrapErr)
			}
			return unwrapErr
		}
		return nil
	})
	return key, err
}

func (m *luksVolumeManager) luksOpen(ctx context.Context, device, mapper string, keyMaterial []byte) error {
	keyPath, cleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, keyMaterial)
	if err != nil {
		return err
	}
	defer cleanup()

	return m.run.Run(ctx, "cryptsetup", "open",
		"--type", "luks2",
		"--allow-discards",
		"--key-file", keyPath,
		device, mapper,
	)
}

// luksFormatWithMasterKey formats a LUKS2 device using the single master key.
// The pool keyfile is set as the keyslot 0 passphrase.
func (m *luksVolumeManager) luksFormatWithMasterKey(ctx context.Context, device string) error {
	masterKey, err := m.crypto.EnsureLUKSMasterKey()
	if err != nil {
		return fmt.Errorf("get master key: %w", err)
	}
	defer cryptoutil.SecureZero(masterKey)

	poolKey, err := m.crypto.EnsurePoolKeyfile()
	if err != nil {
		return fmt.Errorf("ensure pool keyfile: %w", err)
	}
	defer cryptoutil.SecureZero(poolKey)

	masterKeyPath, masterCleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, masterKey)
	if err != nil {
		return err
	}
	defer masterCleanup()

	poolKeyPath, poolCleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, poolKey)
	if err != nil {
		return err
	}
	defer poolCleanup()

	return m.run.Run(ctx, "cryptsetup", "luksFormat",
		"--type", "luks2",
		"--batch-mode",
		"--label", "piccolo-vol",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--hash", "sha256",
		"--pbkdf", "pbkdf2",
		"--pbkdf-force-iterations", "1000",
		"--master-key-file", masterKeyPath,
		"--key-file", poolKeyPath,
		device,
	)
}

// luksOpenWithPoolKeyfile opens a LUKS device using the pool keyfile.
// Used for all v3 volumes (master-key-formatted).
// Uses UnwrapPoolKeyfile (not EnsurePoolKeyfile) because the key must already exist:
// auto-generating on transient errors would overwrite the real key, making volumes inaccessible.
func (m *luksVolumeManager) luksOpenWithPoolKeyfile(ctx context.Context, device, mapper string) error {
	poolKey, err := m.crypto.UnwrapPoolKeyfile()
	if err != nil {
		return fmt.Errorf("unwrap pool keyfile: %w", err)
	}
	defer cryptoutil.SecureZero(poolKey)

	keyPath, cleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, poolKey)
	if err != nil {
		return err
	}
	defer cleanup()

	return m.run.Run(ctx, "cryptsetup", "open",
		"--type", "luks2",
		"--allow-discards",
		"--key-file", keyPath,
		device, mapper,
	)
}

// --- LUKS keyslot provisioning ---

// provisionKeyslotOnAllVolumes iterates all v3 volumes and adds (or replaces)
// a passphrase on the given LUKS keyslot. Volumes that fail are logged and
// collected; the caller gets a joined error.
func (m *luksVolumeManager) provisionKeyslotOnAllVolumes(ctx context.Context, slot int, passphrase []byte) error {
	if slot < 1 || slot > 2 {
		return fmt.Errorf("invalid keyslot %d: only slots 1 and 2 are provisionable", slot)
	}

	// Collect eligible volumes first — if none exist (e.g., first boot with no
	// apps), return early without attempting the master key unwrap.
	volIDs, err := listVolumeIDs()
	if err != nil {
		return err
	}

	type candidate struct {
		id   string
		meta *volumeMetaV3
	}
	var targets []candidate
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
		version, _ := readVolumeMetaVersion(metaPath)
		if version != metadataV3Version {
			continue
		}
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			continue
		}
		// Ephemeral volumes have no LUKS container — skip keyslot provisioning.
		if meta.Type == "ephemeral" {
			continue
		}
		targets = append(targets, candidate{id: volID, meta: meta})
	}
	if len(targets) == 0 {
		return nil
	}

	masterKey, err := m.crypto.UnwrapLUKSMasterKey()
	if err != nil {
		return fmt.Errorf("unwrap master key: %w", err)
	}
	defer cryptoutil.SecureZero(masterKey)

	var errs []error
	for _, t := range targets {
		if err := m.provisionKeyslotOnVolume(ctx, t.id, t.meta, slot, passphrase, masterKey); err != nil {
			log.Printf("WARN: keyslot %d failed for %s: %v", slot, t.id, err)
			errs = append(errs, fmt.Errorf("%s: %w", t.id, err))
		}
	}
	return errors.Join(errs...)
}

// provisionKeyslotOnVolume adds a passphrase to a specific keyslot on one volume.
func (m *luksVolumeManager) provisionKeyslotOnVolume(ctx context.Context, volID string, meta *volumeMetaV3, slot int, passphrase, masterKey []byte) error {
	device, cleanup, err := m.resolveLUKSDevice(ctx, volID, meta)
	if err != nil {
		return err
	}
	defer cleanup()
	return m.luksSetKeyslot(ctx, device, slot, passphrase, masterKey)
}

// resolveLUKSDevice returns the block device path for a volume's LUKS container.
// For already-active volumes, uses the existing device. For inactive volumes,
// activates the underlying device and returns a cleanup function.
func (m *luksVolumeManager) resolveLUKSDevice(ctx context.Context, volID string, meta *volumeMetaV3) (string, func(), error) {
	noop := func() {}

	switch meta.Type {
	case "ephemeral":
		return "", noop, fmt.Errorf("ephemeral volumes have no LUKS device")

	case "golden", "workspace", "service-rootfs":
		// Rootfs types: LUKS sits directly on the LV (no DRBD in stack).
		// NOTE: if workspace replication via DRBD is added, this must
		// resolve through the device stack instead of the raw LV.
		m.mu.Lock()
		state := m.rootfsMounts[volID]
		m.mu.Unlock()
		if state != nil {
			return m.lvMgr.LVPath(meta.LVName), noop, nil
		}
		if err := m.lvMgr.ActivateLV(ctx, meta.LVName); err != nil {
			return "", nil, fmt.Errorf("activate LV %s: %w", meta.LVName, err)
		}
		return m.lvMgr.LVPath(meta.LVName), func() {
			m.lvMgr.DeactivateLV(ctx, meta.LVName)
		}, nil

	case "service-data":
		// Service-data: LUKS sits on top of the device stack.
		m.mu.Lock()
		stack := m.stacks[volID]
		m.mu.Unlock()
		if stack != nil {
			return stack.Top().Path(), noop, nil
		}
		tmpStack, err := m.buildStack(volID, meta.LVName, meta.SizeBytes)
		if err != nil {
			return "", nil, fmt.Errorf("build stack: %w", err)
		}
		if err := tmpStack.Open(ctx); err != nil {
			return "", nil, fmt.Errorf("open stack: %w", err)
		}
		return tmpStack.Top().Path(), func() { tmpStack.Close(ctx) }, nil

	default:
		return "", nil, fmt.Errorf("unsupported volume type: %s", meta.Type)
	}
}

// luksSetKeyslot adds a passphrase to a specific LUKS keyslot, using the master
// key for authentication. If the slot is already occupied, it is killed first.
func (m *luksVolumeManager) luksSetKeyslot(ctx context.Context, device string, slot int, passphrase, masterKey []byte) error {
	masterKeyPath, mkCleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, masterKey)
	if err != nil {
		return err
	}
	defer mkCleanup()

	passphrasePath, ppCleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, passphrase)
	if err != nil {
		return err
	}
	defer ppCleanup()

	slotStr := fmt.Sprintf("%d", slot)

	// Try adding directly; if the slot is occupied, kill and retry.
	err = m.run.Run(ctx, "cryptsetup", "luksAddKey",
		"--master-key-file", masterKeyPath,
		"--key-slot", slotStr,
		"--batch-mode",
		device,
		passphrasePath,
	)
	if err == nil {
		return nil
	}

	// Slot may be occupied. Kill and retry. Log the original error for diagnostics.
	log.Printf("luksAddKey slot %s on %s failed (will retry): %v", slotStr, device, err)
	if killErr := m.run.Run(ctx, "cryptsetup", "luksKillSlot",
		"--master-key-file", masterKeyPath,
		"--batch-mode",
		device,
		slotStr,
	); killErr != nil {
		log.Printf("luksKillSlot %s on %s: %v", slotStr, device, killErr)
	}
	return m.run.Run(ctx, "cryptsetup", "luksAddKey",
		"--master-key-file", masterKeyPath,
		"--key-slot", slotStr,
		"--batch-mode",
		device,
		passphrasePath,
	)
}

// attachAppVolumeV3 attaches a v3 service-data volume using the pool keyfile.
func (m *luksVolumeManager) attachAppVolumeV3(ctx context.Context, handle VolumeHandle, meta *volumeMetaV3, opts AttachOptions) error {
	return m.attachAppVolumeCommon(ctx, handle, meta.LVName, meta.SizeBytes, opts,
		func(topDev, mapper string) error {
			return m.luksOpenWithPoolKeyfile(ctx, topDev, mapper)
		})
}

// --- Metadata v3 ---

// volumeMetaV3 is the on-disk metadata schema for block-native rootfs volumes
// and v3 service-data volumes using the single LUKS master key.
type volumeMetaV3 struct {
	Version         int            `json:"version"`                    // 3
	Type            string         `json:"type"`                      // golden/workspace/service-rootfs/service-data/ephemeral
	LVName          string         `json:"lv_name"`
	VGName          string         `json:"vg_name"`
	SizeBytes       int64          `json:"size_bytes,omitempty"`
	FSType   string `json:"fs_type"`
	ReadOnly bool   `json:"read_only,omitempty"`
	BaseImageDigest string         `json:"base_image_digest,omitempty"`
	BaseImageRef    string         `json:"base_image_ref,omitempty"`
	GoldenLV        string         `json:"golden_lv,omitempty"`
	CloneOf         string         `json:"clone_of,omitempty"`
	IDMap           *IDMapMeta     `json:"idmap,omitempty"`
	FlattenComplete string         `json:"flatten_complete,omitempty"` // RFC3339 timestamp
}

// IDMapMeta persists idmap configuration for rootfs volumes.
type IDMapMeta struct {
	AppUID      uint32 `json:"app_uid"`
	AppGID      uint32 `json:"app_gid"`
	SubUIDStart uint32 `json:"sub_uid_start"`
	SubUIDCount uint32 `json:"sub_uid_count"`
	SubGIDStart uint32 `json:"sub_gid_start"`
	SubGIDCount uint32 `json:"sub_gid_count"`
}

// --- Metadata I/O ---

// listVolumeIDs returns subdirectory names under the volumes metadata base
// directory. Each subdirectory name corresponds to a volume ID.
// Returns nil, nil if the directory does not exist.
func listVolumeIDs() ([]string, error) {
	metaBase := paths.CoreJoin("volumes")
	entries, err := os.ReadDir(metaBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read volumes dir: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ids = append(ids, e.Name())
	}
	return ids, nil
}

// readVolumeMetaVersion reads only the version field from a metadata file.
func readVolumeMetaVersion(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var v struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrVolumeMetadataCorrupted, err)
	}
	return v.Version, nil
}

// readVolumeMetaV2 reads v2 metadata.
func readVolumeMetaV2(path string) (*volumeMetaV2, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta volumeMetaV2
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVolumeMetadataCorrupted, err)
	}
	if meta.Version != metadataV2Version {
		return nil, fmt.Errorf("%w: expected v2, got %d", ErrVolumeMetadataCorrupted, meta.Version)
	}
	return &meta, nil
}

// readVolumeMetaV3 reads v3 metadata.
func readVolumeMetaV3(path string) (*volumeMetaV3, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta volumeMetaV3
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVolumeMetadataCorrupted, err)
	}
	if meta.Version != metadataV3Version {
		return nil, fmt.Errorf("%w: expected v3, got %d", ErrVolumeMetadataCorrupted, meta.Version)
	}
	return &meta, nil
}

// readVolumeMeta reads v2 metadata (backward-compatible reader for existing callers).
func readVolumeMeta(path string) (*volumeMetaV2, error) {
	return readVolumeMetaV2(path)
}

func writeVolumeMeta(path string, meta *volumeMetaV2) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

func writeVolumeMetaV3(path string, meta *volumeMetaV3) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

// --- Data volume snapshot and rollback (RFC 20260302 Phases 2-3) ---

// SnapshotDataVolume creates a thin LV snapshot of an app's data volume.
// The origin LV name is derived deterministically: "vol-app-" + instanceID.
// LVM thin snapshots are metadata-only operations — safe while origin is active/mounted.
func (m *luksVolumeManager) SnapshotDataVolume(ctx context.Context, instanceID, snapshotLVName string) error {
	originLV := "vol-app-" + instanceID
	if err := m.lvMgr.CreateSnapshot(ctx, originLV, snapshotLVName); err != nil {
		return fmt.Errorf("snapshot data volume %s as %s: %w", originLV, snapshotLVName, err)
	}
	return nil
}

// DestroyDataSnapshot removes a data volume snapshot LV.
func (m *luksVolumeManager) DestroyDataSnapshot(ctx context.Context, snapshotLVName string) error {
	// Deactivate before removal (may already be inactive).
	_ = m.lvMgr.DeactivateLV(ctx, snapshotLVName)
	if err := m.lvMgr.RemoveThinLV(ctx, snapshotLVName); err != nil {
		return fmt.Errorf("destroy data snapshot %s: %w", snapshotLVName, err)
	}
	return nil
}

// RollbackDataVolume performs a LUKS-aware LV rename swap with full detach/attach cycle.
//
// Sequence:
//  1. Detach data volume (unmount ext4, cryptsetup close, close device stack)
//  2. Rename active LV → failedLVName
//  3. Rename snapshotLVName → active LV name
//  4. Attach data volume (open stack, LUKS open, mount ext4)
//
// Returns (renamesCommitted, snapshotPromoted, error):
//   - (false, false, err): failed before LV renames (detach or first rename failed), no LV state change
//   - (true, false, err):  active→failed rename committed, but snapshot→active failed (partial state)
//   - (true, true, nil):   fully succeeded (both renames + re-attach)
//   - (true, true, err):   both renames committed but re-attach failed; caller must update tuple state
//
// The volume handle ID stays the same — only the underlying LV changes.
// All containers must be stopped before calling this method.
func (m *luksVolumeManager) RollbackDataVolume(ctx context.Context, instanceID, snapshotLVName, failedLVName string) (bool, bool, error) {
	volumeID := "app-" + instanceID
	handle := VolumeHandle{ID: volumeID, MountDir: paths.MountDir(volumeID)}

	// 1. Full teardown: unmount + LUKS close + device stack close.
	if err := m.Detach(ctx, handle); err != nil {
		return false, false, fmt.Errorf("detach data volume before rollback: %w", err)
	}

	// 2. Rename active → failed.
	activeLV := "vol-app-" + instanceID
	if err := m.lvMgr.RenameLV(ctx, activeLV, failedLVName); err != nil {
		// Attempt recovery: re-attach original.
		_ = m.Attach(ctx, handle, AttachOptions{Role: VolumeRoleLeader})
		return false, false, fmt.Errorf("rename active LV to failed: %w", err)
	}

	// 3. Rename snapshot → active.
	if err := m.lvMgr.RenameLV(ctx, snapshotLVName, activeLV); err != nil {
		// Attempt recovery: reverse step 2 and re-attach.
		if reverseErr := m.lvMgr.RenameLV(ctx, failedLVName, activeLV); reverseErr != nil {
			// Reverse rename also failed — active LV is now named failedLVName,
			// snapshot LV still named snapshotLVName. Neither is named activeLV.
			log.Printf("ERROR: rollback data volume %s: reverse rename also failed: %v", instanceID, reverseErr)
			return true, false, fmt.Errorf("promote snapshot LV (reverse rename also failed): %w", err)
		}
		if attachErr := m.Attach(ctx, handle, AttachOptions{Role: VolumeRoleLeader}); attachErr != nil {
			log.Printf("WARN: rollback data volume %s: reverse rename succeeded but re-attach failed: %v", instanceID, attachErr)
			return false, false, fmt.Errorf("promote snapshot LV (re-attach after reversal failed): %w", err)
		}
		return false, false, fmt.Errorf("promote snapshot LV: %w", err)
	}

	// Both renames succeeded — LV swap is committed.

	// 4. Re-attach (opens the promoted LV through full LUKS + mount stack).
	if err := m.Attach(ctx, handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		log.Printf("WARN: rollback data volume %s: LV renames succeeded but re-attach failed: %v", instanceID, err)
		return true, true, fmt.Errorf("re-attach data volume after rollback: %w", err)
	}

	log.Printf("INFO: rollback data volume for %s: %s → %s (failed: %s)", instanceID, snapshotLVName, activeLV, failedLVName)
	return true, true, nil
}
