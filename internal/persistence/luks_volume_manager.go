package persistence

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
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

	appVolumeDefaultSize = 10 << 30 // 10 GiB (thin-provisioned)
	appLogsVolumeSize    = 2 << 30  // 2 GiB cap for the singleton app-logs store
)

// volumeMetaV2 is the on-disk metadata schema for block-native volumes.
type volumeMetaV2 struct {
	Version    int    `json:"version"`
	Type       string `json:"type"` // volumeTypeLUKSLoop or volumeTypeLUKSThinLV
	WrappedKey string `json:"wrapped_key"`
	Nonce      string `json:"nonce"`
	LVName     string `json:"lv_name,omitempty"`   // luks-thinlv only
	VGName     string `json:"vg_name,omitempty"`   // luks-thinlv only
	LoopFile   string `json:"loop_file,omitempty"` // luks-loop only
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

	mu                  sync.Mutex
	roleChecker         func(string, VolumeRole) bool
	volumeCreationNudge func() // RFC 20260510 — invoked after a v3 volume reaches stable creation success

	// Golden LV management.
	goldenLVs    map[string]*volumeMetaV3 // golden volume ID → Ready metadata
	goldenMu     map[string]*sync.Mutex   // identity/storage-key transition lock
	goldenMuLock sync.Mutex               // protects goldenMu map

	// Workspace resize monitor.
	wsResizeCancel       context.CancelFunc
	volumeResizeCooldown map[string]time.Time // volumeID → last resize time

	// Application-volume auto-grow scheduling (D-5a two-stage).
	// Defined in workspace_resize_monitor.go.
	appResizeSchedules map[string]*appResizeSchedule // volumeID → schedule

	// Per-volume transition serialization (volume-attach-truth, RFC 2026-04-25).
	// volumeID → *sync.Mutex. Held for entire transition (Attach, Detach,
	// AttachRootfs, DetachRootfs, ResizeApplication, ResizeWorkspace).
	// Phase 1 wires this map for the AttachStateOf forced-under-lock escape
	// hatch and the admin clear-corrupted-state endpoint; Phase 2 acquires
	// it from transition entry points.
	locks sync.Map

	// Per-volume Unknown-streak counter for the AttachStateOf K-escape ladder.
	// volumeID → *unknownCounterEntry. Process-local, cleared on restart.
	unknownCounter sync.Map

	// Kernel-state snapshot reader. nil = use the live reader. Tests inject
	// a fake to drive the AttachStateOf partition without touching the
	// real kernel.
	kernelSnapshotFn kernelSnapshotReader
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

// NewLUKSVolumeManager creates the unified volume manager. Single-node only —
// returns ErrMultiNodeUnsupportedForAttachTruth if cfg.NBDSrv or cfg.DRBDMgr
// is non-nil. Multi-node enablement requires extending LiveLayers and
// AttachStateOf to query NBD and DRBD layers (see project_multi_node_prereq.md
// and.claude/plans/volume-attach-truth.md §"Multi-node prerequisite gate").
func NewLUKSVolumeManager(cfg LUKSVolumeManagerConfig) (*luksVolumeManager, error) {
	if cfg.NBDSrv != nil || cfg.DRBDMgr != nil {
		return nil, ErrMultiNodeUnsupportedForAttachTruth
	}
	return &luksVolumeManager{
		run:                  cfg.Run,
		crypto:               cfg.Crypto,
		bus:                  cfg.Bus,
		tmpfsDir:             "/run/piccolo",
		lvMgr:                cfg.LVMgr,
		poolMgr:              cfg.PoolMgr,
		nbdSrv:               cfg.NBDSrv,
		drbdMgr:              cfg.DRBDMgr,
		flattenFn:            cfg.FlattenFn,
		imageSizeFn:          cfg.ImageSizeFn,
		loopVol:              NewLUKSLoopVolume(cfg.Run),
		goldenLVs:            make(map[string]*volumeMetaV3),
		goldenMu:             make(map[string]*sync.Mutex),
		volumeResizeCooldown: make(map[string]time.Time),
		appResizeSchedules:   make(map[string]*appResizeSchedule),
	}, nil
}

// SetRoleChecker sets the function used to check if a volume operation
// is permitted for a given role.
// SetVolumeCreationNudge registers a callback invoked after a newly-created
// v3 volume reaches its stable success state (metadata persisted and any
// transient creation stack settled). Lets the keyslot reconciler pick up the
// new volume on its next pass without waiting for the operator's next
// /generate or password change. nil-safe: if no callback is registered,
// volume creation proceeds normally and the next reconciler signal picks up
// the volume.
func (m *luksVolumeManager) SetVolumeCreationNudge(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.volumeCreationNudge = fn
}

func (m *luksVolumeManager) nudgeVolumeCreation() {
	m.mu.Lock()
	fn := m.volumeCreationNudge
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

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
			case volumeTypeGolden, volumeTypeWorkspace, volumeTypeServiceRootfs:
				continue
			case volumeTypeServiceData, volumeTypeEphemeral:
				// Validate parseable, nothing else needed.
			}
		default:
			log.Printf("WARN: volume %s has unsupported metadata version %d", volID, version)
		}
	}

	// Persist IDMap fingerprints for any pre-existing volumes that don't
	// have one yet. Runs here — at startup, before any other transition
	// can race — to avoid the unlocked-backfill clobber that codex2-P2-A
	// flagged.
	m.backfillIDMapFingerprintsAtStartup(context.Background())
	return nil
}

// ReconcileOrphanLVs scans the LVM thin pool for LVs that have no corresponding
// metadata under the core-root volumes directory. This handles stale LVs left after an OS
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
			knownLVs[volID] = true             // golden-*, ws-*, svc-rootfs-*
			knownLVs[ephLVPrefix+volID] = true // eph-*
			knownLVs[appLVPrefix+volID] = true // vol-*
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
		// Skip app rollback-shaped LVs. The app layer owns their lifecycle
		// because only app metadata can distinguish user rollback points,
		// transaction-private snapshots, and failed-data CoW dependencies.
		if strings.HasPrefix(lv.Name, "snap-") {
			continue
		}
		if strings.Contains(lv.Name, "--failed-gen") {
			continue
		}
		if strings.Contains(lv.Name, "--failed-manifest-") {
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
		if strings.HasPrefix(lv.Name, goldenLVPrefix) {
			storageKey := strings.TrimPrefix(lv.Name, goldenLVPrefix)
			mu := m.goldenMutex(storageKey)
			mu.Lock()
			destroyErr := m.destroyGoldenLVLocked(ctx, lv.Name)
			mu.Unlock()
			if destroyErr != nil {
				log.Printf("WARN: failed to remove orphan golden LV %s: %v", lv.Name, destroyErr)
				errs = append(errs, fmt.Errorf("remove golden %s: %w", lv.Name, destroyErr))
			}
			continue
		}
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
		return m.ensureServiceDataVolume(ctx, req, appVolumeDefaultSize)
	case VolumeClassAppLogs:
		// Singleton app-logs store — same v3 service-data backing as an
		// application volume, just a fixed cap and a non-app LV name.
		return m.ensureServiceDataVolume(ctx, req, appLogsVolumeSize)
	default:
		return VolumeHandle{}, fmt.Errorf("unknown volume class: %s", req.Class)
	}
}

// Attach mounts a volume, making it available for I/O. Acquires the
// per-volume transition lock for the duration of the call; concurrent
// callers serialize. Dispatches based on metadata version: v2 uses
// per-volume wrapped keys, v3 service-data uses the pool keyfile. v3
// rootfs types must use AttachRootfs.
//
// Reconciliation shape: probes AttachStateOf under lock first.
// - Attached → reconcile size invariants, return success (idempotent for
// callers like the post-unlock RestoreServices+ReconcileOnce fan-out).
// - ForeignMountAtPath → fail loud; surfaced to UI as unrecoverable.
// - KernelStateCorrupted → ErrKernelStateCorrupted; admin clear required.
// - Unknown → ErrKernelStateAmbiguous; caller may retry.
// - StaleMountRecord → lazy umount the stale path, then full attach.
// - Detached / PartialMapperOnly → run the type-specific full attach,
// which is naturally idempotent at each step (cryptsetup luksOpen
// tolerates exit 5; mount tolerates EBUSY; resize2fs is idempotent).
func (m *luksVolumeManager) Attach(ctx context.Context, handle VolumeHandle, opts AttachOptions) error {
	lock := m.lockFor(handle.ID)
	lock.Lock()
	defer lock.Unlock()

	metaPath := filepath.Join(paths.VolumeMetaDir(handle.ID), metadataV2File)

	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrLocked
		}
		return fmt.Errorf("read volume metadata version: %w", err)
	}

	// Probe under lock — by-construction snapshot is consistent against
	// concurrent transitions on this volumeID.
	state, probeErr := m.attachStateOfUnderLock(ctx, handle.ID)
	if probeErr != nil {
		// Unknown/Corrupted carry their own errors. Corrupt metadata also
		// returns Unknown via probe; surface to caller.
		if errors.Is(probeErr, ErrKernelStateCorrupted) || errors.Is(probeErr, ErrKernelStateAmbiguous) {
			return probeErr
		}
		// Other probe errors (corrupt metadata, transient procfs/sysfs read
		// failure). attachStateOfInternal returns these with state ==
		// AttachStateUnknown; the switch below fail-closes on Unknown by
		// returning ErrKernelStateAmbiguous. Log carries the underlying
		// cause for operator triage.
		log.Printf("WARN: Attach %s probe error (will refuse with ambiguous): %v", handle.ID, probeErr)
	}
	switch state {
	case AttachStateAttached:
		// Steady state — reconcile size invariants then short-circuit. This
		// is the path the "loser" goroutine in the post-unlock fan-out hits.
		m.reconcileAttachedSize(ctx, handle.ID, version)
		return nil
	case AttachStateForeignMountAtPath:
		return fmt.Errorf("foreign mount at %s; volume %s in unrecoverable state — logged for diagnostics", paths.MountDir(handle.ID), handle.ID)
	case AttachStateKernelStateCorrupted:
		return ErrKernelStateCorrupted
	case AttachStateUnknown:
		return ErrKernelStateAmbiguous
	case AttachStateStaleMountRecord:
		// Lazy-umount only when actually mounted. The stale-record state
		// can present even when the mountinfo entry has already vanished
		// from underneath (concurrent kernel cleanup) — running umount
		// on a non-mountpoint errors, and aborting on that would refuse
		// recovery for state that's already in the desired shape.
		if err := m.unmountStaleIfPresent(ctx, paths.MountDir(handle.ID)); err != nil {
			return fmt.Errorf("attach %s: %w", handle.ID, err)
		}
	case AttachStateDetached, AttachStatePartialMapperOnly:
		// Fall through to dispatch.
	}

	switch version {
	case metadataV2Version:
		meta, err := readVolumeMetaV2(metaPath)
		if err != nil {
			return fmt.Errorf("read v2 metadata: %w", err)
		}
		switch meta.Type {
		case volumeTypeLUKSLoop:
			return m.attachControlVolume(ctx, handle, meta)
		case volumeTypeLUKSThinLV:
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
		case volumeTypeServiceData:
			return m.attachAppVolumeV3(ctx, handle, meta, opts)
		case volumeTypeEphemeral:
			return m.attachEphemeralVolume(ctx, handle, meta)
		case volumeTypeGolden, volumeTypeWorkspace, volumeTypeServiceRootfs:
			return fmt.Errorf("rootfs volume %s (type=%s): use AttachRootfs instead", handle.ID, meta.Type)
		default:
			return fmt.Errorf("unknown v3 volume type: %s", meta.Type)
		}

	default:
		return fmt.Errorf("%w: unsupported version %d", ErrVolumeMetadataCorrupted, version)
	}
}

// reconcileAttachedSize handles the Attached-branch size invariant:
// run cryptsetup resize + resize2fs (or btrfs filesystem resize) to
// converge the upper layers if the LV was resized while attached.
// Idempotent — both ops are no-ops when sizes already match.
func (m *luksVolumeManager) reconcileAttachedSize(ctx context.Context, volumeID string, metaVersion int) {
	if metaVersion != metadataV3Version {
		// v2 (luks-loop / luks-thinlv legacy) does not participate in the
		// resize cascade; nothing to reconcile.
		return
	}
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		return
	}
	switch meta.Type {
	case volumeTypeServiceData:
		mapper := volMapperName(volumeID)
		_ = m.luksResizeWithPoolKeyfile(ctx, mapper)
		_, _ = m.run.RunWithOutput(ctx, "resize2fs", "/dev/mapper/"+mapper)
	case volumeTypeWorkspace:
		mapper := volMapperName(volumeID)
		_ = m.luksResizeWithPoolKeyfile(ctx, mapper)
		_ = m.run.Run(ctx, "btrfs", "filesystem", "resize", "max", paths.MountDir(volumeID))
	}
}

// Detach unmounts a volume and tears down its device stack. Acquires the
// per-volume lock for the duration; serializes against concurrent Attach,
// Resize, and DestroyVolume on the same volumeID.
func (m *luksVolumeManager) Detach(ctx context.Context, handle VolumeHandle) error {
	lock := m.lockFor(handle.ID)
	lock.Lock()
	defer lock.Unlock()
	return m.detachLocked(ctx, handle)
}

// detachLocked is the lock-already-held variant. Used by DestroyVolume,
// which acquires the lock at its own entry and dispatches through the
// type-specific destroy flow.
func (m *luksVolumeManager) detachLocked(ctx context.Context, handle VolumeHandle) error {
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
		case volumeTypeLUKSLoop:
			return m.detachControlVolume(ctx, handle, meta)
		case volumeTypeLUKSThinLV:
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
		case volumeTypeServiceData:
			return m.detachAppVolume(ctx, handle)
		case volumeTypeEphemeral:
			return m.detachEphemeralVolume(ctx, handle)
		case volumeTypeGolden, volumeTypeWorkspace, volumeTypeServiceRootfs:
			return fmt.Errorf("rootfs volume %s: use DetachRootfs", handle.ID)
		default:
			return fmt.Errorf("unknown v3 volume type: %s", meta.Type)
		}
	default:
		return fmt.Errorf("%w: unsupported version %d", ErrVolumeMetadataCorrupted, version)
	}
}

// DestroyVolume permanently removes a volume and its metadata. Acquires
// the per-volume lock unconditionally — including the no-metadata branch
// where cleanupStale*State is the only operation — then runs the
// per-volume side-state cleanup contract (purges m.appResizeSchedules,
// m.volumeResizeCooldown, and the unknown counter) regardless of whether
// destroy succeeds. Half-destroyed volumes do not leak side state.
//
// Lock-map churn: the m.locks entry for `id` is intentionally
// NOT deleted. Two-phase deletion (Unlock-then-Delete vs Delete-then-Unlock)
// both have TOCTOU windows where two concurrent transitions can hold
// different lock instances for the same volumeID. Keeping the *sync.Mutex
// in the map for process lifetime eliminates the entire family of
// map-churn races. Memory cost is ~24 bytes × destroyed-volumes per
// process — negligible on a consumer appliance with low destroy frequency.
func (m *luksVolumeManager) DestroyVolume(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer func() {
		// Side-state cleanup contract: purge per-volume in-memory state
		// owned by the manager. The m.locks entry is intentionally retained
		// for process lifetime (see godoc). Any future
		// per-volume map MUST be added here.
		m.mu.Lock()
		delete(m.appResizeSchedules, id)
		delete(m.volumeResizeCooldown, id)
		m.mu.Unlock()
		m.unknownCounter.Delete(id)
		lock.Unlock()
	}()

	// The golden namespace is durable ownership evidence even when metadata is
	// missing or unreadable. Generic stale-volume cleanup must never infer that
	// a golden-shaped ID is an ordinary app or ephemeral volume.
	if strings.HasPrefix(id, goldenLVPrefix) {
		return fmt.Errorf(
			"refuse generic destruction of golden content %s; use reference-aware garbage collection",
			id,
		)
	}

	metaDir := paths.VolumeMetaDir(id)
	metaPath := filepath.Join(metaDir, metadataV2File)
	mountDir := paths.MountDir(id)

	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No metadata — run both cleanups defensively. They expect the
			// caller (us) to hold the per-volume lock per the contract.
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
		if err := m.detachLocked(ctx, VolumeHandle{ID: id, MountDir: mountDir}); err != nil {
			log.Printf("WARN: detach volume %s during destroy: %v", id, err)
		}
		switch meta.Type {
		case volumeTypeLUKSLoop:
			_ = os.Remove(paths.CoreJoin(meta.LoopFile))
		case volumeTypeLUKSThinLV:
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
		case volumeTypeGolden:
			return fmt.Errorf(
				"refuse generic destruction of golden content %s; use reference-aware garbage collection",
				id,
			)
		case volumeTypeWorkspace, volumeTypeServiceRootfs:
			// Rootfs destroy needs its own per-volume lock; we hold it. Use
			// the lock-already-held variant to avoid re-entry deadlock.
			return m.destroyRootfsLocked(ctx, id)
		case volumeTypeServiceData:
			if err := m.detachLocked(ctx, VolumeHandle{ID: id, MountDir: mountDir}); err != nil {
				log.Printf("WARN: detach volume %s during destroy: %v", id, err)
			}
			if m.lvMgr != nil && meta.LVName != "" {
				if err := m.lvMgr.RemoveThinLV(ctx, meta.LVName); err != nil {
					log.Printf("WARN: remove thin LV %s: %v", meta.LVName, err)
				}
			}
		case volumeTypeEphemeral:
			if err := m.detachLocked(ctx, VolumeHandle{ID: id, MountDir: mountDir}); err != nil {
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
//
// Lock contract: caller MUST hold m.locks[id]. All callers (EnsureVolume,
// DestroyVolume) acquire this lock at their top-level entry. This helper
// MUST NOT call back into any lock-acquiring transition (DestroyVolume,
// Detach, Attach, AttachRootfs, DetachRootfs, ResizeApplication,
// ResizeWorkspace) — re-entering m.locks[id] would deadlock (Go's
// sync.Mutex is not reentrant).
func (m *luksVolumeManager) cleanupStaleAppState(ctx context.Context, id string) {
	mountDir := paths.MountDir(id)
	mapper := volMapperName(id)
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
		Type:       volumeTypeLUKSLoop,
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

// ensureServiceDataVolume provisions a v3 service-data volume (thin LV on the
// data pool + per-volume LUKS2 via the pool keyfile + ext4). Shared by
// application volumes and the singleton app-logs store; the only difference is
// the size cap and the LV name (derived from req.ID). Both ride the same v3
// attach/detach/destroy/keyslot-reconciliation paths thereafter.
func (m *luksVolumeManager) ensureServiceDataVolume(ctx context.Context, req VolumeRequest, sizeBytes int64) (VolumeHandle, error) {
	metaDir := paths.VolumeMetaDir(req.ID)
	lvName := appLVPrefix + req.ID

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
	nudgeOnSuccess := false
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := stack.Close(closeCtx); err != nil {
			log.Printf("WARN: failed to close device stack for %s after creation: %v", req.ID, err)
		}
		if nudgeOnSuccess {
			m.nudgeVolumeCreation()
		}
	}()

	// LUKS format with the shared master key (pool keyfile as passphrase).
	topDev := stack.Top().Path()
	if err := m.luksFormatWithMasterKey(ctx, topDev); err != nil {
		return VolumeHandle{}, fmt.Errorf("luks format: %w", err)
	}

	// Open LUKS, mkfs, close.
	mapper := volMapperName(req.ID)
	if err := m.luksOpenWithPoolKeyfile(ctx, topDev, mapper); err != nil {
		return VolumeHandle{}, fmt.Errorf("luks open for mkfs: %w", err)
	}
	mapperPath := "/dev/mapper/" + mapper
	mkfsErr := m.run.Run(ctx, "mkfs.ext4", "-F", "-m", "1", mapperPath)
	_ = m.run.Run(ctx, "cryptsetup", "close", mapper)
	if mkfsErr != nil {
		return VolumeHandle{}, fmt.Errorf("mkfs.ext4: %w", mkfsErr)
	}

	// Persist v3 metadata. RFC 20260510 §Volume-creation atomicity stamps
	// kskey_id for both slots: "unprovisioned" sentinel when no pending
	// blob exists (operator initiates rotation later to populate), or the
	// blob's key_id when a rotation is in flight (sub-case i — the
	// post-success reconciler nudge will then provision this volume's slots
	// against the captured passphrase). The nudge happens after the
	// transient device stack has closed so the reconciler does not race
	// creation teardown.
	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      volumeTypeServiceData,
		LVName:    lvName,
		VGName:    lvm.DefaultVGName,
		SizeBytes: sizeBytes,
		FSType:    "ext4",
		// New v3 volumes are stamped "unprovisioned" for both slots
		// (RFC 20260510 §Volume-creation atomicity sub-case ii). Even
		// when a /generate or password-change rotation is in flight at
		// creation time, the post-creation reconciler nudge picks the
		// volume up and the reconciler's case-KeyslotKeyIDUnprovisioned
		// arm provisions it via the D7 pre-kill probe (kill is a no-op
		// on the empty slot, only add runs).
		PasswordKeyslotKeyID: KeyslotKeyIDUnprovisioned,
		RecoveryKeyslotKeyID: KeyslotKeyIDUnprovisioned,
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return VolumeHandle{}, fmt.Errorf("create meta dir: %w", err)
	}
	if err := writeVolumeMetaV3(filepath.Join(metaDir, metadataV2File), meta); err != nil {
		return VolumeHandle{}, fmt.Errorf("write metadata: %w", err)
	}

	nudgeOnSuccess = true

	return VolumeHandle{
		ID:       req.ID,
		MountDir: paths.MountDir(req.ID),
	}, nil
}

// CountKeyslotUnprovisioned returns slot 1 + slot 2 counts in a single
// metadata walk (RFC 20260510 §Status surface, S7). Counted on-demand
// from on-disk metadata rather than reconciler-pass-cached state so the
// boot-surface count reflects the current truth even between reconciler
// passes. Single walk avoids the 2× IO of separate per-slot calls under
// the 3-second frontend boot poll.
func (m *luksVolumeManager) CountKeyslotUnprovisioned() (slot1, slot2 int, err error) {
	ids, listErr := listVolumeIDs()
	if listErr != nil {
		return 0, 0, listErr
	}
	for _, id := range ids {
		metaPath := filepath.Join(paths.VolumeMetaDir(id), metadataV2File)
		version, _ := readVolumeMetaVersion(metaPath)
		if version != metadataV3Version {
			continue
		}
		meta, mErr := readVolumeMetaV3(metaPath)
		if mErr != nil {
			continue
		}
		if meta.Type == volumeTypeEphemeral {
			continue
		}
		if meta.PasswordKeyslotKeyID == KeyslotKeyIDUnprovisioned {
			slot1++
		}
		if meta.RecoveryKeyslotKeyID == KeyslotKeyIDUnprovisioned {
			slot2++
		}
	}
	return slot1, slot2, nil
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

	// Build and open the device stack. The stack object is local to this
	// transition; rollback closes it directly without touching any shared
	// in-memory map (Phase 4 of volume-attach-truth eliminated the cache).
	stack, err := m.buildStack(handle.ID, lvName, sizeBytes)
	if err != nil {
		return fmt.Errorf("build device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return fmt.Errorf("open device stack: %w", err)
	}

	mapper := volMapperName(handle.ID)
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
	}()

	// LUKS open. Tolerate cryptsetup exit 5 (mapper already exists) — the
	// reconciler may be completing a PartialMapperOnly state where the
	// mapper survived but the mount didn't.
	topDev := stack.Top().Path()
	if err := luksOpenFn(topDev, mapper); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != cryptsetupExitDeviceExists {
			return fmt.Errorf("luks open: %w", err)
		}
		// Mapper already present — do not register it for rollback close,
		// since we didn't open it this call.
	} else {
		openedMapper = mapper
	}

	// Mount ext4. Tolerate "already mounted" — mount returns EBUSY when
	// the path is already a mount point (idempotent for re-attach paths).
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
	mounted, entry, _ := mountAtPath(mountDir)
	if mounted {
		// Ownership check (codex2-P3): verify the existing mount belongs
		// to our mapper before skipping. Window between top-level probe
		// and this branch is small but nonzero; refuse to silently treat
		// a foreign mount as ours.
		if entry.Source != mapperPath {
			return fmt.Errorf("foreign mount at %s (source=%s, expected=%s); refusing to coexist", mountDir, entry.Source, mapperPath)
		}
	} else {
		if err := m.run.Run(ctx, "mount", "-t", "ext4", "-o", "discard", mapperPath, mountDir); err != nil {
			return fmt.Errorf("mount: %w", err)
		}
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
	mapper := volMapperName(handle.ID)
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

	// Deactivate the LV (kernel-state truth: read meta to find LV name —
	// no dependency on the deprecated cache). Single-node only — multi-node
	// stack tear-down is gated out at construction.
	metaPath := filepath.Join(paths.VolumeMetaDir(handle.ID), metadataV2File)
	if version, err := readVolumeMetaVersion(metaPath); err == nil {
		var lvName string
		switch version {
		case metadataV2Version:
			if v2, err := readVolumeMetaV2(metaPath); err == nil {
				lvName = v2.LVName
			}
		case metadataV3Version:
			if v3, err := readVolumeMetaV3(metaPath); err == nil {
				lvName = v3.LVName
			}
		}
		if m.lvMgr != nil && lvName != "" {
			_ = m.lvMgr.DeactivateLV(ctx, lvName)
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
		Type:      volumeTypeEphemeral,
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
	// Caller (Attach dispatcher) probed under lock and reaches here only
	// for Detached / PartialMapperOnly / StaleMountRecord. The mount step
	// below is idempotent (skips when already mounted) so PartialMapperOnly
	// completes naturally; no second probe needed.
	thinDev := blockdev.NewThinLVDevice(m.lvMgr, meta.LVName, meta.SizeBytes)
	stack, err := blockdev.NewDeviceStack(handle.ID, thinDev)
	if err != nil {
		return fmt.Errorf("build ephemeral device stack: %w", err)
	}
	if err := stack.Open(ctx); err != nil {
		return fmt.Errorf("open ephemeral device stack: %w", err)
	}

	success := false
	defer func() {
		if !success {
			stack.Close(ctx)
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
	// already be gone (systemd race) and LV deactivation still needs to run.
	if err := m.run.Run(ctx, "umount", handle.MountDir); err != nil {
		log.Printf("umount %s failed, trying lazy unmount", handle.MountDir)
		if lazyErr := m.run.Run(ctx, "umount", "-l", handle.MountDir); lazyErr != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", handle.MountDir, lazyErr))
		}
	}

	// Deactivate the LV via metadata (kernel-state truth).
	metaPath := filepath.Join(paths.VolumeMetaDir(handle.ID), metadataV2File)
	if v3, err := readVolumeMetaV3(metaPath); err == nil && v3.LVName != "" && m.lvMgr != nil {
		_ = m.lvMgr.DeactivateLV(ctx, v3.LVName)
	}
	return errors.Join(errs...)
}

// cleanupStaleEphemeralState tears down leftover mounts, LVs, and dirs for an
// ephemeral volume that has no metadata (e.g., partial creation that was interrupted).
//
// Lock contract: caller MUST hold m.locks[id]. Same reasoning as
// cleanupStaleAppState — see its godoc.
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

func (m *luksVolumeManager) luksResizeWithPoolKeyfile(ctx context.Context, mapper string) error {
	if m.crypto == nil {
		return errors.New("crypto manager unavailable")
	}
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

	return m.run.Run(ctx, "cryptsetup", "resize",
		"--key-file", keyPath,
		mapper,
	)
}

// --- LUKS keyslot provisioning ---

// provisionKeyslotOnAllVolumesAndStamp is the sync path with metadata
// stamping (RFC 20260510 + codex iter-3 P2). Called by the locked
// /reset-password handler that must complete the rotation before the
// deferred relock; after the cryptsetup-add succeeds per volume, stamps
// PasswordKeyslotKeyID / RecoveryKeyslotKeyID = stampKeyID so the async
// reconciler doesn't redundantly re-provision later. stampKeyID == "" is
// the legacy no-stamp variant (called via provisionKeyslotOnAllVolumes
// wrapper).
func (m *luksVolumeManager) provisionKeyslotOnAllVolumesAndStamp(ctx context.Context, slot int, passphrase []byte, stampKeyID string) error {
	if slot < 1 || slot > 2 {
		return fmt.Errorf("invalid keyslot %d: only slots 1 and 2 are provisionable", slot)
	}
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
		if meta.Type == volumeTypeEphemeral {
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
			continue
		}
		if stampKeyID != "" {
			// Read-modify-write so a concurrent writer (resize updating
			// SizeBytes, attach-time fingerprint backfill) is not
			// clobbered by writing back the stale `t.meta` snapshot.
			// Mirror of the reconciler's keyslot_reconciler.go:452-462
			// pattern.
			metaPath := filepath.Join(paths.VolumeMetaDir(t.id), metadataV2File)
			latest, rerr := readVolumeMetaV3(metaPath)
			if rerr != nil {
				log.Printf("WARN: re-read meta %s slot=%d: %v", t.id, slot, rerr)
				continue
			}
			setMetaKeyslotKeyID(latest, KeyslotSlot(slot), stampKeyID)
			if werr := writeVolumeMetaV3(metaPath, latest); werr != nil {
				log.Printf("WARN: stamp kskey_id %s slot=%d: %v", t.id, slot, werr)
			}
		}
	}
	return errors.Join(errs...)
}

// provisionKeyslotOnAllVolumes iterates all v3 volumes and adds (or replaces)
// a passphrase on the given LUKS keyslot. Volumes that fail are logged and
// collected; the caller gets a joined error. Stub legacy wrapper — no
// metadata stamping. Tests call this directly; production callers should
// route through provisionKeyslotOnAllVolumesAndStamp with a non-empty
// fingerprint.
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
		if meta.Type == volumeTypeEphemeral {
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

// --- keyslotReconcilerVM adapter ---
//
// These methods expose the subset of luksVolumeManager the KeyslotReconciler
// (RFC 20260510 §Reconciler shape) needs. Implementing the interface here
// keeps the reconciler decoupled from volume-manager internals while
// reusing the existing helpers for volume enumeration, metadata I/O, and
// the kill+add primitive.

// listKeyslotVolumes returns every v3 non-ephemeral volume's id + metadata.
// Ephemeral volumes have no LUKS container so they cannot host a slot-1
// or slot-2 passphrase; the existing provisionKeyslotOnAllVolumes iterator
// skips them and the reconciler inherits that filter.
func (m *luksVolumeManager) listKeyslotVolumes() ([]keyslotVolume, error) {
	ids, err := listVolumeIDs()
	if err != nil {
		return nil, err
	}
	var out []keyslotVolume
	for _, id := range ids {
		metaPath := filepath.Join(paths.VolumeMetaDir(id), metadataV2File)
		version, _ := readVolumeMetaVersion(metaPath)
		if version != metadataV3Version {
			continue
		}
		meta, err := readVolumeMetaV3(metaPath)
		if err != nil {
			continue
		}
		if meta.Type == volumeTypeEphemeral {
			continue
		}
		out = append(out, keyslotVolume{ID: id, Meta: meta})
	}
	return out, nil
}

func (m *luksVolumeManager) readKeyslotMeta(volID string) (*volumeMetaV3, error) {
	metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
	return readVolumeMetaV3(metaPath)
}

func (m *luksVolumeManager) writeKeyslotMeta(volID string, meta *volumeMetaV3) error {
	metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
	return writeVolumeMetaV3(metaPath, meta)
}

func (m *luksVolumeManager) provisionKeyslotOnVolumeByID(ctx context.Context, volID string, meta *volumeMetaV3, slot int, passphrase, masterKey []byte) error {
	return m.provisionKeyslotOnVolume(ctx, volID, meta, slot, passphrase, masterKey)
}

func (m *luksVolumeManager) unwrapMasterKey() ([]byte, error) {
	return m.crypto.UnwrapLUKSMasterKey()
}

// resolveLUKSDevice returns the block device path for a volume's LUKS container.
// For already-attached volumes, uses LUKSBackingDevice (kernel-state truth).
// For inactive volumes, activates the underlying device and returns a cleanup
// function. Single-node only — multi-node is gated out at construction.
func (m *luksVolumeManager) resolveLUKSDevice(ctx context.Context, volID string, meta *volumeMetaV3) (string, func(), error) {
	noop := func() {}

	switch meta.Type {
	case volumeTypeEphemeral:
		return "", noop, fmt.Errorf("ephemeral volumes have no LUKS device")

	case volumeTypeGolden, volumeTypeWorkspace, volumeTypeServiceRootfs:
		// Rootfs types: LUKS sits directly on the LV (no DRBD in stack).
		if dev, err := m.LUKSBackingDevice(ctx, volID); err == nil {
			return dev, noop, nil
		}
		if err := m.lvMgr.ActivateLV(ctx, meta.LVName); err != nil {
			return "", nil, fmt.Errorf("activate LV %s: %w", meta.LVName, err)
		}
		return m.lvMgr.LVPath(meta.LVName), func() {
			m.lvMgr.DeactivateLV(ctx, meta.LVName)
		}, nil

	case volumeTypeServiceData:
		// Service-data on single-node: LUKS sits directly on the thin LV.
		if dev, err := m.LUKSBackingDevice(ctx, volID); err == nil {
			return dev, noop, nil
		}
		// Not attached — activate the LV transiently for keyslot ops.
		if err := m.lvMgr.ActivateLV(ctx, meta.LVName); err != nil {
			return "", nil, fmt.Errorf("activate LV %s: %w", meta.LVName, err)
		}
		return m.lvMgr.LVPath(meta.LVName), func() {
			m.lvMgr.DeactivateLV(ctx, meta.LVName)
		}, nil

	default:
		return "", nil, fmt.Errorf("unsupported volume type: %s", meta.Type)
	}
}

// luksSetKeyslot installs `passphrase` in the given LUKS keyslot, using the
// master key for authentication. RFC 20260510 D7 — the kill→add pair is the
// critical span where the slot is transiently empty. Two defenses combine
// to keep the slot from transitioning through empty under
// explicit-cancellation failure modes (SIGKILL during Argon2id remains the
// residual hazard; rotate-to-recover policy is the documented recourse):
//
// 1. Pre-kill probe via `cryptsetup luksDump`: if the slot is already
// empty, the kill is skipped entirely and we go straight to add. This
// collapses the "unprovisioned" sentinel case (RFC §Volume-creation
// atomicity sub-case ii) to a single non-destructive operation.
// 2. The kill+add pair is wrapped in `context.WithoutCancel`: a
// reconciler nudge, per-pass timeout, or process-exit signal mid-pair
// cannot tear them apart. The compute time is bounded by Argon2id
// (single-digit seconds per slot), so withholding cancellation for
// this window is a finite, acceptable trade against the silent-empty
// hazard.
//
// Net invariant: the slot transitions atomically from old-passphrase to
// new-passphrase OR stays at old-passphrase; it never transitions through
// empty under the explicit-cancellation failure modes the reconciler is
// responsible for.
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

	addKey := func(c context.Context) error {
		return m.run.Run(c, "cryptsetup", "luksAddKey",
			"--master-key-file", masterKeyPath,
			"--key-slot", slotStr,
			"--batch-mode",
			device,
			passphrasePath,
		)
	}

	// Pre-kill probe — read-only, runs under the caller's context so
	// external cancellation is observable here.
	occupied, probeErr := m.luksSlotOccupied(ctx, device, slot)
	switch {
	case probeErr == nil && !occupied:
		// Sub-case ii / "unprovisioned" sentinel: add-only, no kill. No
		// empty-window risk because the slot is already empty.
		return addKey(ctx)
	case probeErr != nil:
		// Probe failure means we don't know the slot state. Try the
		// optimistic add first (the historical behavior) so a transient
		// `luksDump` issue does not block legitimate provisioning of an
		// empty slot. If the add fails (slot was occupied), fall through
		// to the atomic kill+add pair.
		log.Printf("WARN: luksDump probe slot=%d on %s: %v; trying optimistic add", slot, device, probeErr)
		if err := addKey(ctx); err == nil {
			return nil
		}
	}

	// Slot is occupied (or probe failed and optimistic add failed) —
	// execute kill+add under WithoutCancel so the pair stays atomic
	// against external cancellation.
	uncancellable := context.WithoutCancel(ctx)
	if killErr := m.run.Run(uncancellable, "cryptsetup", "luksKillSlot",
		"--master-key-file", masterKeyPath,
		"--batch-mode",
		device,
		slotStr,
	); killErr != nil {
		log.Printf("luksKillSlot %s on %s: %v", slotStr, device, killErr)
	}
	return addKey(uncancellable)
}

// luksSlotOccupied reports whether the given LUKS keyslot holds a key. It
// shells out to `cryptsetup luksDump --dump-json-metadata` and parses the
// returned JSON. Pre-kill probe per RFC D7.
//
// Returns (false, nil) when the dump succeeds and the slot is unmarked.
// Returns (true, nil) when the dump succeeds and the slot is enabled.
// Returns (_, err) for transport / parse failures — caller falls back to
// the optimistic add-first path so a probe-only failure doesn't gate
// legitimate rotations.
func (m *luksVolumeManager) luksSlotOccupied(ctx context.Context, device string, slot int) (bool, error) {
	out, err := m.run.RunWithOutput(ctx, "cryptsetup", "luksDump", "--dump-json-metadata", device)
	if err != nil {
		return false, fmt.Errorf("luksDump: %w", err)
	}
	return luksDumpSlotOccupied(out, slot)
}

// luksDumpSlotOccupied parses the `cryptsetup luksDump --dump-json-metadata`
// output and reports whether the keyslot ID `slot` is present in the
// `keyslots` object. Pure function for unit-testing the probe shape
// without invoking cryptsetup.
func luksDumpSlotOccupied(dump []byte, slot int) (bool, error) {
	var doc struct {
		Keyslots map[string]json.RawMessage `json:"keyslots"`
	}
	if err := json.Unmarshal(dump, &doc); err != nil {
		return false, fmt.Errorf("parse luksDump JSON: %w", err)
	}
	_, present := doc.Keyslots[fmt.Sprintf("%d", slot)]
	return present, nil
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
	Version         int        `json:"version"` // 3
	Type            string     `json:"type"`    // golden/workspace/service-rootfs/service-data/ephemeral
	LVName          string     `json:"lv_name"`
	VGName          string     `json:"vg_name"`
	SizeBytes       int64      `json:"size_bytes,omitempty"`
	FSType          string     `json:"fs_type"`
	ReadOnly        bool       `json:"read_only,omitempty"`
	BaseImageDigest string     `json:"base_image_digest,omitempty"`
	BaseImageRef    string     `json:"base_image_ref,omitempty"`
	GoldenLV        string     `json:"golden_lv,omitempty"`
	CloneOf         string     `json:"clone_of,omitempty"`
	IDMap           *IDMapMeta `json:"idmap,omitempty"`
	FlattenComplete string     `json:"flatten_complete,omitempty"` // RFC3339 timestamp
	// GoldenIdentity is the complete source+resolved-content+projection identity
	// for generic golden content. Legacy image goldens without this field are
	// matched through BaseImageDigest and backfilled only when rewritten.
	GoldenIdentity      *GoldenContentIdentity `json:"golden_identity,omitempty"`
	MaterializeComplete string                 `json:"materialize_complete,omitempty"` // RFC3339 timestamp

	// IDMapFingerprint is the lowercase hex BLAKE2b-256 of canonicalIDMapBytes(*IDMap).
	// Empty for volumes without IDMap. Once non-empty, mutations to the IDMap
	// fields are rejected by writeVolumeMetaV3 (ErrIDMapImmutable). Backfilled
	// from the in-memory IDMap on first read for volumes created before the
	// volume-attach-truth work shipped — pre-existing hand-edited metadata is
	// grandfathered (see plan §"Pre-Phase-1 fleet grandfathering").
	IDMapFingerprint string `json:"idmap_fingerprint,omitempty"`

	// PasswordKeyslotKeyID / RecoveryKeyslotKeyID record which generation's
	// passphrase is currently provisioned in LUKS keyslot 1 (admin password)
	// and keyslot 2 (recovery mnemonic) respectively. RFC 20260510 §Data
	// shape additions. Typed sentinel values are load-bearing:
	//
	// "" = pre-RFC-existing volume; reconciler kill+re-adds
	// unconditionally (slot may hold any old generation)
	// "unprovisioned" = volume created in steady state with no in-flight
	// passphrase; slot is empty; reconciler adds-only
	// (skips the kill via pre-kill luksDump probe)
	// <hex digest> = current generation fingerprint; reconciler skips
	// this volume for that slot
	PasswordKeyslotKeyID string `json:"password_keyslot_key_id,omitempty"`
	RecoveryKeyslotKeyID string `json:"recovery_keyslot_key_id,omitempty"`
}

const (
	// KeyslotKeyIDUnprovisioned marks a volume created in steady state with
	// no in-flight passphrase material — the LUKS slot is known empty so the
	// reconciler skips the kill and goes straight to add. Distinct from the
	// empty-string sentinel (pre-RFC unknown state), which forces kill+add.
	KeyslotKeyIDUnprovisioned = "unprovisioned"
)

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

// readVolumeMetaV3 reads v3 metadata. Computes IDMapFingerprint in-memory
// for volumes created before this work shipped (so the immutability guard
// works for the duration of this process), but does NOT persist the
// backfill. A persisting backfill from this read path raced ResizeWorkspace
// and other writers (codex2-P2-A) — the rewrite would clobber concurrent
// updates. Persistence happens via writeVolumeMetaV3 on the next legitimate
// metadata write (always under the per-volume lock), or explicitly through
// backfillIDMapFingerprintsAtStartup before any concurrent transition runs.
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
	if meta.IDMap != nil && meta.IDMapFingerprint == "" {
		meta.IDMapFingerprint = computeIDMapFingerprint(meta.IDMap)
	}
	return &meta, nil
}

// backfillIDMapFingerprintsAtStartup walks every v3 metadata file and
// persists IDMapFingerprint for volumes that don't yet have one. Runs
// once during ReconcileAllVolumeStates before any other transition can
// race; uses the per-volume lock to serialize against any unlikely
// concurrent caller. Volumes with their fingerprint already set are
// touched only in-memory (no rewrite), so this is idempotent.
func (m *luksVolumeManager) backfillIDMapFingerprintsAtStartup(ctx context.Context) {
	volIDs, err := listVolumeIDs()
	if err != nil {
		return
	}
	for _, volID := range volIDs {
		metaPath := filepath.Join(paths.VolumeMetaDir(volID), metadataV2File)
		// Quick filter: only v3 metadata, only volumes with IDMap and no
		// fingerprint. Read without taking the lock — we re-read after.
		preview, err := readVolumeMetaV3(metaPath)
		if err != nil || preview.IDMap == nil {
			continue
		}
		// readVolumeMetaV3 already computed the fingerprint in-memory; if
		// the on-disk file was missing one, persist it now under the lock.
		// Re-read under the lock to avoid clobbering a concurrent write.
		lock := m.lockFor(volID)
		lock.Lock()
		fresh, ferr := readVolumeMetaV3(metaPath)
		if ferr != nil || fresh.IDMap == nil {
			lock.Unlock()
			continue
		}
		// If the on-disk file already has a fingerprint, no work to do.
		if persistedFp, _ := readPersistedIDMapFingerprint(metaPath); persistedFp != "" {
			lock.Unlock()
			continue
		}
		// writeVolumeMetaV3 recomputes + persists the fingerprint and
		// honors the immutability guard.
		if err := writeVolumeMetaV3(metaPath, fresh); err != nil {
			log.Printf("WARN: backfill IDMap fingerprint for %s: %v", volID, err)
		}
		lock.Unlock()
		_ = ctx // honor caller context shape; nothing inside takes it
	}
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

// writeVolumeMetaV3 writes v3 metadata with two contracts:
//
// 1. IDMap immutability: if a fingerprint already exists on disk for this
// volume, the incoming meta's IDMap fields must produce the same
// fingerprint. Otherwise: ErrIDMapImmutable. This blocks the
// hand-edit-the-JSON workflow at the write site; the attach-time
// fingerprint check (Phase 2 reconciler) is the second line of defense.
// Removing IDMap entirely (setting it to nil when a fingerprint is
// persisted) is also rejected — otherwise an attacker could clear the
// guard by deleting the idmap field, then re-add a different IDMap on
// the next write (which would have no fingerprint to compare against).
// 2. Fingerprint maintenance: when meta.IDMap != nil and meta.IDMapFingerprint
// is empty, the fingerprint is computed and stored. When IDMap == nil
// AND no fingerprint was previously persisted, the fingerprint stays
// empty (legitimate case: volume created without idmap).
func writeVolumeMetaV3(path string, meta *volumeMetaV3) error {
	existingFp, _ := readPersistedIDMapFingerprint(path)
	if meta.IDMap == nil {
		if existingFp != "" {
			return fmt.Errorf("%w: cannot remove IDMap (persisted fingerprint %s)", ErrIDMapImmutable, existingFp)
		}
		meta.IDMapFingerprint = ""
	} else {
		newFp := computeIDMapFingerprint(meta.IDMap)
		if existingFp != "" && existingFp != newFp {
			return fmt.Errorf("%w: persisted fingerprint %s vs new %s", ErrIDMapImmutable, existingFp, newFp)
		}
		meta.IDMapFingerprint = newFp
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

// readPersistedIDMapFingerprint reads only the IDMapFingerprint field from
// an existing on-disk metadata file. Returns ("", nil) when the file does
// not exist — used to allow first-write through writeVolumeMetaV3.
func readPersistedIDMapFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var v struct {
		IDMapFingerprint string `json:"idmap_fingerprint"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		// Treat unparseable existing metadata as "no fingerprint" — the
		// caller will overwrite it; corruption is handled by the read path.
		return "", nil
	}
	return v.IDMapFingerprint, nil
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

func (m *luksVolumeManager) CheckDataSnapshotViability(ctx context.Context, instanceID string) error {
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return err
	}
	if m.lvMgr != nil {
		originLV := "vol-app-" + instanceID
		if !m.lvMgr.LVExists(ctx, originLV) {
			return fmt.Errorf("data volume LV %s does not exist", originLV)
		}
	}
	return nil
}

func (m *luksVolumeManager) CheckDataSnapshotHealth(ctx context.Context, snapshotLVName string) error {
	if err := m.checkThinPoolCapacity(ctx); err != nil {
		return err
	}
	if m.lvMgr != nil && !m.lvMgr.LVExists(ctx, snapshotLVName) {
		return fmt.Errorf("data snapshot LV %s does not exist", snapshotLVName)
	}
	return nil
}

// ListAppDataRollbackArtifacts returns app rollback-shaped LV names. The app
// layer owns classification; this low-level method only exposes observed LVM
// names so allocation and cleanup can avoid blind pattern-only policy.
func (m *luksVolumeManager) ListAppDataRollbackArtifacts(ctx context.Context, instanceID string) ([]string, error) {
	if m.lvMgr == nil || strings.TrimSpace(instanceID) == "" {
		return nil, nil
	}
	lvs, err := m.lvMgr.ListLVs(ctx)
	if err != nil {
		return nil, err
	}
	snapGenPrefix := "snap-app-" + instanceID + "--gen"
	snapManifestPrefix := "snap-app-" + instanceID + "--manifest-"
	failedGenPrefix := "vol-app-" + instanceID + "--failed-gen"
	failedManifestPrefix := "vol-app-" + instanceID + "--failed-manifest-"
	out := make([]string, 0)
	for _, lv := range lvs {
		name := strings.TrimSpace(lv.Name)
		if name == "" {
			continue
		}
		switch {
		case strings.HasPrefix(name, snapGenPrefix):
			out = append(out, name)
		case strings.HasPrefix(name, snapManifestPrefix):
			out = append(out, name)
		case strings.HasPrefix(name, failedGenPrefix):
			out = append(out, name)
		case strings.HasPrefix(name, failedManifestPrefix):
			out = append(out, name)
		}
	}
	return out, nil
}

// DestroyDataSnapshot removes a data volume snapshot LV.
func (m *luksVolumeManager) DestroyDataSnapshot(ctx context.Context, snapshotLVName string) error {
	if m.lvMgr != nil && !m.lvMgr.LVExists(ctx, snapshotLVName) {
		return nil
	}
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
// 1. Detach data volume (unmount ext4, cryptsetup close, close device stack)
// 2. Rename active LV → failedLVName
// 3. Rename snapshotLVName → active LV name
// 4. Attach data volume (open stack, LUKS open, mount ext4)
//
// Returns (renamesCommitted, snapshotPromoted, error):
// - (false, false, err): failed before LV renames (detach or first rename failed), no LV state change
// - (true, false, err): active→failed rename committed, but snapshot→active failed (partial state)
// - (true, true, nil): fully succeeded (both renames + re-attach)
// - (true, true, err): both renames committed but re-attach failed; caller must update tuple state
//
// The volume handle ID stays the same — only the underlying LV changes.
// All containers must be stopped before calling this method.
func (m *luksVolumeManager) RollbackDataVolume(ctx context.Context, instanceID, snapshotLVName, failedLVName string) (bool, bool, error) {
	volumeID := "app-" + instanceID
	handle := VolumeHandle{ID: volumeID, MountDir: paths.MountDir(volumeID)}
	activeLV := "vol-app-" + instanceID

	if m.lvMgr != nil &&
		m.lvMgr.LVExists(ctx, activeLV) &&
		m.lvMgr.LVExists(ctx, failedLVName) &&
		!m.lvMgr.LVExists(ctx, snapshotLVName) {
		log.Printf("WARN: rollback data volume for %s: completing previously promoted snapshot state (failed=%s consumed_snapshot=%s)", instanceID, failedLVName, snapshotLVName)
		if err := m.Attach(ctx, handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
			return true, true, fmt.Errorf("re-attach data volume after completed rollback: %w", err)
		}
		return true, true, nil
	}

	resumingPartial := false
	if m.lvMgr != nil &&
		!m.lvMgr.LVExists(ctx, activeLV) &&
		m.lvMgr.LVExists(ctx, failedLVName) &&
		m.lvMgr.LVExists(ctx, snapshotLVName) {
		resumingPartial = true
		log.Printf("WARN: rollback data volume for %s: resuming partial LV rename state (failed=%s snapshot=%s)", instanceID, failedLVName, snapshotLVName)
	}

	if !resumingPartial {
		// 1. Full teardown: unmount + LUKS close + device stack close.
		if err := m.Detach(ctx, handle); err != nil {
			return false, false, fmt.Errorf("detach data volume before rollback: %w", err)
		}

		// 2. Rename active → failed.
		if err := m.lvMgr.RenameLV(ctx, activeLV, failedLVName); err != nil {
			// Attempt recovery: re-attach original.
			_ = m.Attach(ctx, handle, AttachOptions{Role: VolumeRoleLeader})
			return false, false, fmt.Errorf("rename active LV to failed: %w", err)
		}
	}

	// 3. Rename snapshot → active.
	if err := m.lvMgr.RenameLV(ctx, snapshotLVName, activeLV); err != nil {
		if resumingPartial {
			return true, false, fmt.Errorf("promote snapshot LV after partial rollback: %w", err)
		}
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
