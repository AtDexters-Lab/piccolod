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
	"sync"

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
// idmapped mounts.
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
	// Injected by AppManager wiring.
	flattenFn func(ctx context.Context, imageRef, targetDir string) (GoldenImageConfig, error)
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
}

// rootfsMountState tracks the full device stack for a mounted rootfs volume.
type rootfsMountState struct {
	stack      *blockdev.DeviceStack
	luksMapper string
	mountPath  string
	idmapPath  string
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
	FlattenFn func(ctx context.Context, imageRef, targetDir string) (GoldenImageConfig, error)
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
		rootfsMounts: make(map[string]*rootfsMountState),
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
			case "service-data":
				// Validate parseable, nothing else needed.
			}
		default:
			log.Printf("WARN: volume %s has unsupported metadata version %d", volID, version)
		}
	}
	return nil
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
		_ = m.Detach(ctx, VolumeHandle{ID: id, MountDir: mountDir})
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
			_ = m.Detach(ctx, VolumeHandle{ID: id, MountDir: mountDir})
			if m.lvMgr != nil && meta.LVName != "" {
				if err := m.lvMgr.RemoveThinLV(ctx, meta.LVName); err != nil {
					log.Printf("WARN: remove thin LV %s: %v", meta.LVName, err)
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
	lvName := "vol-" + id

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
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return VolumeHandle{}, fmt.Errorf("create meta dir: %w", err)
	}

	lvName := "vol-" + req.ID
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

	// Build and open the device stack to get the DRBD device path.
	stack, err := m.buildStack(req.ID, lvName, sizeBytes)
	if err != nil {
		return VolumeHandle{}, fmt.Errorf("build device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return VolumeHandle{}, fmt.Errorf("open device stack: %w", err)
	}
	defer stack.Close(ctx)

	// Generate a random key and wrap it with SDEK.
	keyMaterial, wrappedKey, nonce, err := m.generateWrappedKey(ctx)
	if err != nil {
		return VolumeHandle{}, err
	}
	defer cryptoutil.SecureZero(keyMaterial)

	// LUKS format on the top device (DRBD).
	topDev := stack.Top().Path()
	mapper := "piccolo-vol-" + req.ID
	if err := m.luksFormat(ctx, topDev, keyMaterial); err != nil {
		return VolumeHandle{}, fmt.Errorf("luks format: %w", err)
	}

	// Open LUKS, mkfs, close.
	if err := m.luksOpen(ctx, topDev, mapper, keyMaterial); err != nil {
		return VolumeHandle{}, fmt.Errorf("luks open for mkfs: %w", err)
	}
	mapperPath := "/dev/mapper/" + mapper
	mkfsErr := m.run.Run(ctx, "mkfs.ext4", "-F", "-m", "1", mapperPath)
	_ = m.run.Run(ctx, "cryptsetup", "close", mapper)
	if mkfsErr != nil {
		return VolumeHandle{}, fmt.Errorf("mkfs.ext4: %w", mkfsErr)
	}

	// Persist metadata.
	meta := &volumeMetaV2{
		Version:    metadataV2Version,
		Type:       "luks-thinlv",
		WrappedKey: wrappedKey,
		Nonce:      nonce,
		LVName:     lvName,
		VGName:     lvm.DefaultVGName,
		SizeBytes:  sizeBytes,
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

func (m *luksVolumeManager) attachAppVolume(ctx context.Context, handle VolumeHandle, meta *volumeMetaV2, opts AttachOptions) error {
	// Role check.
	m.mu.Lock()
	checker := m.roleChecker
	m.mu.Unlock()
	if checker != nil && !checker(handle.ID, opts.Role) {
		return fmt.Errorf("role check failed for %s", handle.ID)
	}

	// Build and open the device stack.
	stack, err := m.buildStack(handle.ID, meta.LVName, meta.SizeBytes)
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

	// Unwrap volume key.
	keyMaterial, err := m.unwrapKey(ctx, meta.WrappedKey, meta.Nonce)
	if err != nil {
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return err
	}
	defer cryptoutil.SecureZero(keyMaterial)

	// LUKS open.
	topDev := stack.Top().Path()
	mapper := "piccolo-vol-" + handle.ID
	if err := m.luksOpen(ctx, topDev, mapper, keyMaterial); err != nil {
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("luks open: %w", err)
	}

	// Mount ext4.
	mountDir := handle.MountDir
	// Ensure the mounts/ parent directory is traversable (0o711) so per-app
	// users can reach their own mount points without being able to list siblings.
	mountsParent := filepath.Dir(mountDir)
	if err := os.MkdirAll(mountsParent, 0o711); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("create mounts parent: %w", err)
	}
	if err := os.Chmod(mountsParent, 0o711); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("chmod mounts parent: %w", err)
	}
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("create mount dir: %w", err)
	}

	mapperPath := "/dev/mapper/" + mapper
	if err := m.run.Run(ctx, "mount", "-t", "ext4", "-o", "discard", mapperPath, mountDir); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("mount: %w", err)
	}

	return nil
}

func (m *luksVolumeManager) detachAppVolume(ctx context.Context, handle VolumeHandle) error {
	mapper := "piccolo-vol-" + handle.ID

	// Unmount.
	if err := m.run.Run(ctx, "umount", handle.MountDir); err != nil {
		return fmt.Errorf("umount %s: %w", handle.MountDir, err)
	}

	// LUKS close.
	if err := m.run.Run(ctx, "cryptsetup", "close", mapper); err != nil {
		return fmt.Errorf("luks close %s: %w", mapper, err)
	}

	// Close device stack.
	m.mu.Lock()
	stack := m.stacks[handle.ID]
	delete(m.stacks, handle.ID)
	m.mu.Unlock()

	if stack != nil {
		if err := stack.Close(ctx); err != nil {
			return fmt.Errorf("close device stack: %w", err)
		}
	}

	return nil
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

func (m *luksVolumeManager) luksFormat(ctx context.Context, device string, keyMaterial []byte) error {
	keyPath, cleanup, err := writeKeyToTmpfsDir(m.tmpfsDir, keyMaterial)
	if err != nil {
		return err
	}
	defer cleanup()

	return m.run.Run(ctx, "cryptsetup", "luksFormat",
		"--type", "luks2",
		"--batch-mode",
		"--label", "piccolo-vol",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--hash", "sha256",
		"--pbkdf", "pbkdf2",
		"--pbkdf-force-iterations", "1000",
		"--key-file", keyPath,
		device,
	)
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

// attachAppVolumeV3 attaches a v3 service-data volume using the pool keyfile.
func (m *luksVolumeManager) attachAppVolumeV3(ctx context.Context, handle VolumeHandle, meta *volumeMetaV3, opts AttachOptions) error {
	// Role check.
	m.mu.Lock()
	checker := m.roleChecker
	m.mu.Unlock()
	if checker != nil && !checker(handle.ID, opts.Role) {
		return fmt.Errorf("role check failed for %s", handle.ID)
	}

	stack, err := m.buildStack(handle.ID, meta.LVName, meta.SizeBytes)
	if err != nil {
		return fmt.Errorf("build device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return fmt.Errorf("open device stack: %w", err)
	}

	m.mu.Lock()
	m.stacks[handle.ID] = stack
	m.mu.Unlock()

	topDev := stack.Top().Path()
	mapper := "piccolo-vol-" + handle.ID
	if err := m.luksOpenWithPoolKeyfile(ctx, topDev, mapper); err != nil {
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("luks open: %w", err)
	}

	mountDir := handle.MountDir
	mountsParent := filepath.Dir(mountDir)
	if err := os.MkdirAll(mountsParent, 0o711); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("create mounts parent: %w", err)
	}
	_ = os.Chmod(mountsParent, 0o711)
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("create mount dir: %w", err)
	}

	mapperPath := "/dev/mapper/" + mapper
	if err := m.run.Run(ctx, "mount", "-t", "ext4", "-o", "discard", mapperPath, mountDir); err != nil {
		m.run.Run(ctx, "cryptsetup", "close", mapper)
		stack.Close(ctx)
		m.mu.Lock()
		delete(m.stacks, handle.ID)
		m.mu.Unlock()
		return fmt.Errorf("mount: %w", err)
	}

	return nil
}

// --- Metadata v3 ---

// volumeMetaV3 is the on-disk metadata schema for block-native rootfs volumes
// and v3 service-data volumes using the single LUKS master key.
type volumeMetaV3 struct {
	Version         int            `json:"version"`                    // 3
	Type            string         `json:"type"`                      // golden/workspace/service-rootfs/service-data
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
