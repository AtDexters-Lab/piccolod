package persistence

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/state/paths"
)

// AttachState is the seven-state partition over a volume's kernel-derived
// attach status. The enum value is constant across volume types; the meaning
// of each branch is type-aware in the reconciler.
type AttachState int

const (
	// AttachStateDetached: nothing for this volume in kernel state. Safe to
	// run a full attach.
	AttachStateDetached AttachState = iota
	// AttachStatePartialMapperOnly: the LUKS mapper (or, for ephemeral, the
	// thin LV) is present, but at least one required mount is missing. The
	// reconciler completes the missing layers.
	AttachStatePartialMapperOnly
	// AttachStateStaleMountRecord: a stale mount record exists in mountinfo
	// but the underlying device (mapper / LV) is gone. Reconciler lazy-umounts
	// then drops to Detached for full attach.
	AttachStateStaleMountRecord
	// AttachStateAttached: every required layer for this volume's type is
	// present and consistent.
	AttachStateAttached
	// AttachStateForeignMountAtPath: a different volume (or different fs type)
	// is occupying this volume's mount path. Fail loud — operator must reset.
	AttachStateForeignMountAtPath
	// AttachStateUnknown: probe could not produce a consistent snapshot
	// (mountinfo / sysfs disagreement) within bounded retry. Caller treats as
	// advisory; transition callers re-probe under per-volume lock.
	AttachStateUnknown
	// AttachStateKernelStateCorrupted: sticky terminal state set after
	// K_FATAL forced-under-lock Unknowns. Cleared only by a clean re-probe
	// via the admin clear-corrupted-state endpoint, or by process restart.
	AttachStateKernelStateCorrupted
)

// String returns a human-readable name for the state. Used in logs and
// admin endpoint responses.
func (s AttachState) String() string {
	switch s {
	case AttachStateDetached:
		return "Detached"
	case AttachStatePartialMapperOnly:
		return "PartialMapperOnly"
	case AttachStateStaleMountRecord:
		return "StaleMountRecord"
	case AttachStateAttached:
		return "Attached"
	case AttachStateForeignMountAtPath:
		return "ForeignMountAtPath"
	case AttachStateUnknown:
		return "Unknown"
	case AttachStateKernelStateCorrupted:
		return "KernelStateCorrupted"
	default:
		return fmt.Sprintf("AttachState(%d)", int(s))
	}
}

// LiveLayer describes a single observed kernel-side layer of a volume,
// emitted in tear-down order (top first). Used by Detach to enumerate what
// must be undone without consulting any in-memory cache.
type LiveLayer struct {
	Kind LiveLayerKind
	// Name is a human-readable identifier (mapper name, LV name, mount path).
	Name string
	// Path is the device or mount path used to operate on the layer
	// (e.g., /dev/mapper/<name>, <core-root>/mounts/<id>).
	Path string
}

// LiveLayerKind enumerates layer types that the probe can observe.
type LiveLayerKind int

const (
	LiveLayerKindIDMapBind LiveLayerKind = iota
	LiveLayerKindFSMount
	LiveLayerKindLUKSMapper
	LiveLayerKindThinLV
	LiveLayerKindLoopDev
)

func (k LiveLayerKind) String() string {
	switch k {
	case LiveLayerKindIDMapBind:
		return "idmap-bind"
	case LiveLayerKindFSMount:
		return "fs-mount"
	case LiveLayerKindLUKSMapper:
		return "luks-mapper"
	case LiveLayerKindThinLV:
		return "thin-lv"
	case LiveLayerKindLoopDev:
		return "loop-dev"
	default:
		return fmt.Sprintf("LiveLayerKind(%d)", int(k))
	}
}

// Errors specific to the kernel-state-as-truth surface.
var (
	// ErrKernelStateAmbiguous is returned by the probe when two-source
	// disagreement persists across bounded retry. Recoverable: caller may
	// retry later (advisory) or re-probe under lock (transitions).
	ErrKernelStateAmbiguous = errors.New("persistence: kernel state ambiguous (probe disagreement)")

	// ErrKernelStateCorrupted is returned by the probe when sticky
	// AttachStateKernelStateCorrupted has been set for the volume. Recovery
	// is via the admin clear-corrupted-state endpoint or process restart.
	ErrKernelStateCorrupted = errors.New("persistence: kernel state corrupted (operator action required)")

	// ErrUnsupportedVolumeType is returned by LUKSBackingDevice for volume
	// types that have no LUKS layer (ephemeral).
	ErrUnsupportedVolumeType = errors.New("persistence: unsupported volume type for this primitive")

	// ErrNotAttached is returned by LUKSBackingDevice when the volume is not
	// in AttachStateAttached.
	ErrNotAttached = errors.New("persistence: volume not attached")

	// ErrMultiNodeUnsupportedForAttachTruth is returned by NewLUKSVolumeManager
	// when constructed with multi-node managers (NBDSrv / DRBDMgr) wired in.
	// Multi-node enablement is gated on extending LiveLayers / AttachStateOf
	// to query NBD and DRBD layers (see project_multi_node_prereq.md).
	ErrMultiNodeUnsupportedForAttachTruth = errors.New("persistence: multi-node managers unsupported by attach-truth (single-node only — see project_multi_node_prereq.md)")

	// ErrIDMapImmutable is returned by writeVolumeMetaV3 when a metadata
	// write attempts to mutate IDMap fields after the volume's
	// IDMapFingerprint was already established (i.e., the volume has been
	// attached / fingerprint-backfilled at least once).
	ErrIDMapImmutable = errors.New("persistence: idmap is immutable post-creation")
)

// Volume type identifiers persisted in volumeMetaV2.Type / volumeMetaV3.Type.
// The volume type controls dispatch in every transition (Attach, Detach,
// DestroyVolume, LiveLayers, evaluatePartition, the resize monitors) — a
// typo at any one site silently routes wrong, so use the constants.
const (
	volumeTypeLUKSLoop      = "luks-loop"      // v2: control plane (loop file + LUKS)
	volumeTypeLUKSThinLV    = "luks-thinlv"    // v2 legacy: thin LV + LUKS (replaced by service-data)
	volumeTypeServiceData   = "service-data"   // v3: per-app data (thin LV + LUKS + ext4)
	volumeTypeEphemeral     = "ephemeral"      // v3: thin LV + btrfs, no LUKS
	volumeTypeGolden        = "golden"         // v3: golden rootfs LV (read-only origin)
	volumeTypeServiceRootfs = "service-rootfs" // v3: per-instance service rootfs (snapshot of golden)
	volumeTypeWorkspace     = "workspace"      // v3: per-instance workspace rootfs (writable snapshot)
)

// K-counter thresholds for the Unknown escape ladder.
const (
	kUnknownAdvisoryWarn   = 3  // log WARN at this many consecutive advisory Unknowns
	kUnknownAdvisoryForced = 5  // promote next call to forced-under-lock probe
	kUnknownForcedFatal    = 10 // sticky KernelStateCorrupted after this many forced Unknowns
)

// Probe retry budget.
const (
	probeMaxRetries    = 3
	probeBackoffBase   = 10 * time.Millisecond
	probeBackoffJitter = 5 * time.Millisecond
)

// kernelSnapshot bundles the two kernel-state sources the probe depends on,
// captured as close to atomically as a userspace probe can manage. mountinfo
// is genuinely atomic (single read); sysfs is read after, with two-source
// agreement guarding cross-source races.
//
// dmByName / dmByDev are populated sparsely for the mapper names that the
// probe explicitly cares about (typically just the one mapper for the
// volume being probed). The reader avoids walking all of /sys/block/dm-*;
// instead, /dev/mapper/<expected-name> is readlink'd to its dm-N target
// and that dm-N's dev file is read directly. Drops per-probe sysfs syscalls
// from O(N) — proportional to system DM count — to O(1).
type kernelSnapshot struct {
	// mounts indexes mount entries by absolute, filepath.Clean'd mount path.
	mounts map[string]mountEntry
	// dmByName maps a dm mapper name to the corresponding /sys/block/dm-N
	// directory observed in sysfs. Sparse: only contains the mapper names
	// the probe was asked to resolve.
	dmByName map[string]string
	// dmByDev maps "major:minor" to the dm mapper name. Sparse: only
	// contains mappers the probe resolved. Used by mountSourceMatchesMapper
	// when a mount source is /dev/dm-N rather than /dev/mapper/<name>.
	dmByDev map[string]string
}

// mountEntry holds the fields of a /proc/self/mountinfo line that the probe
// uses to discriminate the partition.
type mountEntry struct {
	Major  int
	Minor  int
	FSType string
	// Source is the device path or other source token (mountinfo field 10).
	Source string
	// ReadOnly is derived from mountinfo's per-mount options. Golden content
	// uses it to distinguish a legitimate consumer attachment from writable
	// creation staging left behind by an older daemon.
	ReadOnly bool
}

// kernelSnapshotReader returns a fresh snapshot. The expectedMappers argument
// names the mapper(s) the probe wants resolved — the reader looks each up
// directly via /dev/mapper/<name> readlink + sysfs/dev read instead of
// walking the system DM namespace. Tests inject a fake reader; tests do
// not need to honor expectedMappers because they pre-build the snapshot.
type kernelSnapshotReader func(expectedMappers []string) (kernelSnapshot, error)

// readLiveKernelSnapshot is the production snapshot reader. It reads
// /proc/self/mountinfo (single atomic syscall) and looks up only the
// requested mappers via readlink — no system-wide /sys/block scan. Reads
// do not open /dev/mapper/control and so do not generate udev events —
// eliminating the self-perturbation hazard from dmsetup info.
func readLiveKernelSnapshot(expectedMappers []string) (kernelSnapshot, error) {
	mounts, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		return kernelSnapshot{}, fmt.Errorf("read mountinfo: %w", err)
	}
	dmByName := make(map[string]string)
	dmByDev := make(map[string]string)
	for _, name := range expectedMappers {
		if name == "" {
			continue
		}
		if err := lookupMapper("/dev/mapper", "/sys/block", name, dmByName, dmByDev); err != nil {
			return kernelSnapshot{}, fmt.Errorf("look up mapper %s: %w", name, err)
		}
	}
	return kernelSnapshot{mounts: mounts, dmByName: dmByName, dmByDev: dmByDev}, nil
}

// lookupMapper resolves /dev/mapper/<name> to its /sys/block/dm-N entry
// and major:minor via readlink + ReadFile. Populates byName and byDev
// in place. Falls back to a /sys/block scan if readlink returns ENOENT —
// /dev/mapper/<name> is created by udev, which can lag the kernel's dm
// device by hundreds of milliseconds during volume bring-up/tear-down.
// The fallback walks /sys/block looking for a dm-N whose dm/name file
// matches our expected name; this is the precise two-source check that
// the pre-F-EFF-2 system-wide scan provided. Costs one ReadDir + a few
// small file reads, only on the readlink-miss path.
func lookupMapper(devMapperDir, sysBlock, name string, byName, byDev map[string]string) error {
	link, err := os.Readlink(filepath.Join(devMapperDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			// Fast path missed. Sysfs scan as second source.
			return lookupMapperViaSysfs(sysBlock, name, byName, byDev)
		}
		return err
	}
	devName := filepath.Base(link) // "dm-N"
	if !strings.HasPrefix(devName, "dm-") {
		return nil
	}
	recordMapper(sysBlock, devName, name, byName, byDev)
	return nil
}

// lookupMapperViaSysfs walks /sys/block for dm-N entries whose dm/name
// matches expectedName. Used as the second source when /dev/mapper/<name>
// is missing — distinguishes "kernel device gone" from "udev lag." Adds
// to byName/byDev iff the kernel device is found.
func lookupMapperViaSysfs(sysBlock, expectedName string, byName, byDev map[string]string) error {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		dmName := e.Name()
		if !strings.HasPrefix(dmName, "dm-") {
			continue
		}
		nameBytes, err := os.ReadFile(filepath.Join(sysBlock, dmName, "dm", "name"))
		if err != nil {
			continue // dm-N can disappear mid-walk; skip
		}
		if strings.TrimSpace(string(nameBytes)) != expectedName {
			continue
		}
		recordMapper(sysBlock, dmName, expectedName, byName, byDev)
		return nil
	}
	return nil
}

// recordMapper populates byName/byDev for a confirmed mapper.
func recordMapper(sysBlock, dmName, mapperName string, byName, byDev map[string]string) {
	sysfsPath := filepath.Join(sysBlock, dmName)
	byName[mapperName] = sysfsPath
	devBytes, err := os.ReadFile(filepath.Join(sysfsPath, "dev"))
	if err != nil {
		// dm-N may have disappeared between resolution and ReadFile.
		return
	}
	byDev[strings.TrimSpace(string(devBytes))] = mapperName
}

// readMountInfo parses /proc/self/mountinfo and returns a map keyed by
// mount path. Path values are decoded (mountinfo octal-escapes whitespace).
func readMountInfo(path string) (map[string]mountEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]mountEntry)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 10 {
			continue
		}
		// Locate the optional-fields separator "-".
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) {
			continue
		}
		majMin := fields[2]
		mp := decodeMountPoint(fields[4])
		mountOptions := strings.Split(fields[5], ",")
		fsType := fields[sep+1]
		source := fields[sep+2]

		colonIdx := strings.IndexByte(majMin, ':')
		if colonIdx <= 0 {
			continue
		}
		maj, errMa := strconv.Atoi(majMin[:colonIdx])
		min, errMi := strconv.Atoi(majMin[colonIdx+1:])
		if errMa != nil || errMi != nil {
			continue
		}
		readOnly := false
		for _, option := range mountOptions {
			if option == "ro" {
				readOnly = true
				break
			}
		}
		out[filepath.Clean(mp)] = mountEntry{
			Major:    maj,
			Minor:    min,
			FSType:   fsType,
			Source:   source,
			ReadOnly: readOnly,
		}
	}
	return out, nil
}

// unknownCounterEntry tracks K-consecutive Unknown escalation per-volume.
// All fields are guarded by mu.
type unknownCounterEntry struct {
	mu             sync.Mutex
	advisoryStreak int  // consecutive advisory Unknowns; reset on non-Unknown advisory or any clean forced result
	forcedStreak   int  // consecutive forced-under-lock Unknowns
	sticky         bool // true once forcedStreak has hit kUnknownForcedFatal
}

// observeAdvisory records an advisory probe result. Returns whether a
// WARN should be emitted on this transition. Promotion to forced-under-
// lock is decided atomically by decidePromote (same critical section)
// before the probe runs, so this only records the post-probe outcome.
func (e *unknownCounterEntry) observeAdvisory(unknown bool) (warn bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !unknown {
		e.advisoryStreak = 0
		return false
	}
	e.advisoryStreak++
	return e.advisoryStreak == kUnknownAdvisoryWarn
}

// decidePromote atomically reads the advisory streak and reports whether
// the next probe should run forced-under-lock. Used in place of the older
// peekShouldPromote / observeAdvisory split — collapsing
// the two into one critical section eliminates the TOCTOU window where
// two concurrent advisory observers could both decide to promote and
// double-increment the forced streak.
func (e *unknownCounterEntry) decidePromote() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.advisoryStreak >= kUnknownAdvisoryForced
}

// observeForced records a forced-under-lock probe result. Returns whether
// sticky has just transitioned to true.
func (e *unknownCounterEntry) observeForced(unknown bool) (becameSticky bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !unknown {
		// Clean forced read clears advisory and forced streaks but leaves
		// sticky unchanged (sticky clears via admin endpoint or restart).
		e.advisoryStreak = 0
		e.forcedStreak = 0
		return false
	}
	if e.sticky {
		// Already terminal — no further state change.
		return false
	}
	e.forcedStreak++
	if e.forcedStreak >= kUnknownForcedFatal {
		e.sticky = true
		return true
	}
	return false
}

// isSticky reports whether KernelStateCorrupted has been latched.
func (e *unknownCounterEntry) isSticky() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sticky
}

// reset clears all counter state including the sticky flag. Used by the
// admin clear-corrupted-state endpoint after a clean re-probe.
func (e *unknownCounterEntry) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.advisoryStreak = 0
	e.forcedStreak = 0
	e.sticky = false
}

// volumeMetaSummary is the type-agnostic view the probe needs. Both v2
// (control / luks-loop) and v3 (rootfs, service-data, ephemeral) metadata
// flatten into this shape.
type volumeMetaSummary struct {
	version  int
	typ      string
	lvName   string
	vgName   string
	loopFile string
	fsType   string
	hasIDMap bool
}

// readVolumeMetaSummary loads the metadata file for volumeID and returns a
// type-agnostic summary, or (nil, nil) on missing metadata. Corrupt
// metadata yields (nil, ErrVolumeMetadataCorrupted-wrapped error).
//
// Control volume special case: although its metadata still lives at
// paths.VolumeMetaDir("control-plane"), it is v2 and bootstrapped before
// any encrypted mount. The probe reads it via the same path indirection;
// it is *not* gated on crypto state.
func readVolumeMetaSummary(volumeID string) (*volumeMetaSummary, error) {
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	if _, err := os.Stat(metaPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat metadata: %w", err)
	}
	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		return nil, err
	}
	switch version {
	case metadataV2Version:
		v, err := readVolumeMetaV2(metaPath)
		if err != nil {
			return nil, err
		}
		return &volumeMetaSummary{
			version:  v.Version,
			typ:      v.Type,
			lvName:   v.LVName,
			vgName:   v.VGName,
			loopFile: v.LoopFile,
			fsType:   v.FSType,
		}, nil
	case metadataV3Version:
		v, err := readVolumeMetaV3(metaPath)
		if err != nil {
			return nil, err
		}
		return &volumeMetaSummary{
			version:  v.Version,
			typ:      v.Type,
			lvName:   v.LVName,
			vgName:   v.VGName,
			fsType:   v.FSType,
			hasIDMap: v.IDMap != nil,
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported version %d", ErrVolumeMetadataCorrupted, version)
	}
}

// volMapperName returns the dm mapper name for a service-data or rootfs
// volume given its ID. Single source of truth for the "piccolo-vol-<id>"
// convention. Control volumes (luks-loop) compute their mapper from the
// loop file basename — use expectedMapperName when metadata is in scope
// and the volume type is unknown.
func volMapperName(volumeID string) string {
	return "piccolo-vol-" + volumeID
}

// expectedMapperName returns the dm mapper name a volume of this type would
// own, or empty string for types with no mapper (ephemeral).
func expectedMapperName(volumeID string, meta *volumeMetaSummary) string {
	switch meta.typ {
	case volumeTypeLUKSLoop:
		// Mapper derived from loop file's basename — same convention as
		// LUKSLoopVolume.mapperName.
		base := filepath.Base(meta.loopFile)
		return "piccolo-loop-" + strings.TrimSuffix(base, filepath.Ext(base))
	case volumeTypeEphemeral:
		return ""
	default:
		// service-data, golden, workspace, service-rootfs share the same convention.
		return volMapperName(volumeID)
	}
}

// expectedMountPath returns the primary mount path (raw fs mount, before any
// idmap bind). Same for every type today.
func expectedMountPath(volumeID string) string {
	return paths.MountDir(volumeID)
}

// expectedIDMapPath returns the idmap bind path for any rootfs volume
// (workspace, service-rootfs, golden) whose metadata declares an idmap.
// attachRootfsFromMeta creates the bind whenever meta.IDMap != nil
// regardless of type — service-rootfs anchors and per-service rootfs both
// commonly carry idmap binds. Empty string when no bind is expected.
func expectedIDMapPath(volumeID string, meta *volumeMetaSummary) string {
	if !meta.hasIDMap {
		return ""
	}
	switch meta.typ {
	case volumeTypeWorkspace, volumeTypeServiceRootfs, volumeTypeGolden:
		return paths.MountDir(volumeID) + "-idmap"
	}
	return ""
}

// AttachStateOf observes kernel state and returns the current AttachState
// for the volume. Single-node only — multi-node managers cause
// NewLUKSVolumeManager to fail (see ErrMultiNodeUnsupportedForAttachTruth).
//
// Atomicity model: best-effort with bounded retry + jitter. Two-source
// disagreement escalates to AttachStateUnknown, never to ForeignMountAtPath
// (defends against udev-storm false positives). On persistent disagreement,
// returns (Unknown, ErrKernelStateAmbiguous).
//
// Sticky state: once kUnknownForcedFatal forced-under-lock Unknowns
// accumulate, the volume becomes AttachStateKernelStateCorrupted until the
// admin clear-corrupted-state endpoint clears it (or the process restarts).
//
// Caller contract:
//   - Lock-free advisory callers (auto-grow, precondition checks) treat the
//     result as advisory and re-probe later.
//   - Transition callers (Attach, Detach, Resize, ...) MUST acquire the
//     per-volume lock and re-probe before acting on the result, because
//     a snapshot's within-snapshot consistency does not guarantee freshness
//     against concurrent transitions completing.
func (m *luksVolumeManager) AttachStateOf(ctx context.Context, volumeID string) (AttachState, error) {
	return m.attachStateOfInternal(ctx, volumeID, false /* underLock */)
}

// attachStateOfUnderLock is the forced-under-lock variant. Caller MUST hold
// m.locks[volumeID]. Used internally by the K=5 escape hatch and by the
// admin clear-corrupted-state endpoint.
func (m *luksVolumeManager) attachStateOfUnderLock(ctx context.Context, volumeID string) (AttachState, error) {
	return m.attachStateOfInternal(ctx, volumeID, true /* underLock */)
}

// IsAttachedAdvisory reports whether the volume is currently in
// AttachStateAttached. Probe errors and any non-Attached state (Detached,
// PartialMapperOnly, StaleMountRecord, ForeignMountAtPath, Unknown,
// KernelStateCorrupted) all return false — the volume is not safely
// usable as a backing source for the caller's purpose.
//
// Use this for "is this volume currently a usable backing source?"
// questions: auto-grow's enumeration filter, scratch-flatten reuse
// check, anywhere that wants the boolean predicate without re-encoding
// the state→action mapping.
func (m *luksVolumeManager) IsAttachedAdvisory(ctx context.Context, volumeID string) bool {
	state, err := m.AttachStateOf(ctx, volumeID)
	if err != nil {
		return false
	}
	return state == AttachStateAttached
}

func (m *luksVolumeManager) attachStateOfInternal(ctx context.Context, volumeID string, underLock bool) (AttachState, error) {
	counter := m.unknownCounterFor(volumeID)
	if counter.isSticky() {
		return AttachStateKernelStateCorrupted, ErrKernelStateCorrupted
	}

	meta, err := readVolumeMetaSummary(volumeID)
	if err != nil {
		// Corrupt metadata is terminal for this call — cannot meaningfully
		// retry. Map to Unknown so callers don't act on it.
		return AttachStateUnknown, err
	}
	if meta == nil {
		// Missing metadata — never created or already destroyed. Detached.
		return AttachStateDetached, nil
	}

	// Advisory promotion: if K=5 advisory Unknowns have accumulated, force
	// next probe under the per-volume lock. decidePromote is a single
	// critical section on the counter — no TOCTOU between checking and
	// acting.
	if !underLock && counter.decidePromote() {
		lock := m.lockFor(volumeID)
		lock.Lock()
		defer lock.Unlock()
		underLock = true
		// While we waited for the lock, another forced probe may have
		// latched sticky. Re-check before running our probe — otherwise
		// we could return Attached/Detached for a volume that's already
		// been declared KernelStateCorrupted, breaking the sticky-until-
		// explicit-clear contract (codex4-P3).
		if counter.isSticky() {
			return AttachStateKernelStateCorrupted, ErrKernelStateCorrupted
		}
	}

	state, ambiguous := m.probeAttachStateWithRetry(ctx, volumeID, meta)

	// Counter bookkeeping.
	if underLock {
		becameSticky := counter.observeForced(state == AttachStateUnknown)
		if becameSticky {
			log.Printf("CRITICAL: volume %s entered KernelStateCorrupted (sticky) after %d forced-under-lock Unknowns; operator clear required", volumeID, kUnknownForcedFatal)
			return AttachStateKernelStateCorrupted, ErrKernelStateCorrupted
		}
	} else {
		if warn := counter.observeAdvisory(state == AttachStateUnknown); warn {
			log.Printf("WARN: volume %s probe returned Unknown %d times consecutively (will force probe under lock at %d); meta=%s", volumeID, kUnknownAdvisoryWarn, kUnknownAdvisoryForced, meta.typ)
		}
	}

	if state == AttachStateUnknown {
		if ambiguous {
			return state, ErrKernelStateAmbiguous
		}
		return state, nil
	}
	return state, nil
}

// probeAttachStateWithRetry runs the snapshot read + partition evaluation
// up to probeMaxRetries times, with jittered backoff between retries.
// Returns (Unknown, true) on persistent two-source disagreement.
func (m *luksVolumeManager) probeAttachStateWithRetry(ctx context.Context, volumeID string, meta *volumeMetaSummary) (AttachState, bool) {
	reader := m.kernelSnapshotFn
	if reader == nil {
		reader = readLiveKernelSnapshot
	}
	expected := mappersOfInterest(volumeID, meta)
	for attempt := 0; attempt < probeMaxRetries; attempt++ {
		if ctx.Err() != nil {
			return AttachStateUnknown, true
		}
		snap, err := reader(expected)
		if err != nil {
			// Read errors propagate as Unknown; we don't retry I/O errors
			// here — the caller's K-counter handles persistence.
			return AttachStateUnknown, false
		}
		state, consistent := evaluatePartition(snap, volumeID, meta)
		if consistent {
			return state, false
		}
		// Jittered backoff before next attempt.
		jitter := time.Duration(rand.Int63n(int64(2*probeBackoffJitter+1))) - probeBackoffJitter //nolint:gosec
		sleep := probeBackoffBase + jitter
		select {
		case <-ctx.Done():
			return AttachStateUnknown, true
		case <-time.After(sleep):
		}
	}
	return AttachStateUnknown, true
}

// mappersOfInterest returns the dm mapper names the probe needs resolved
// for one volume. Today this is at most one entry (the volume's mapper),
// or empty for ephemeral volumes that have no LUKS layer.
func mappersOfInterest(volumeID string, meta *volumeMetaSummary) []string {
	if name := expectedMapperName(volumeID, meta); name != "" {
		return []string{name}
	}
	return nil
}

// evaluatePartition applies the partition rules to a snapshot for a single
// volume. Returns (state, consistent) — when consistent==false, the caller
// should retry the probe; when state==Unknown && consistent==true, the
// result is "evidence-based Unknown" (e.g., corrupt metadata) and not a
// retry candidate.
//
// Three structural shapes (not seven branches):
//   - raw-mount only          (ephemeral): no mapper, btrfs mount on the LV.
//   - single-fs LUKS stack    (luks-loop, service-data): mapper + fs mount.
//   - two-fs LUKS stack       (golden / service-rootfs / workspace): mapper +
//     raw fs mount + optional idmap bind.
func evaluatePartition(snap kernelSnapshot, volumeID string, meta *volumeMetaSummary) (AttachState, bool) {
	mountPath := expectedMountPath(volumeID)
	mapper := expectedMapperName(volumeID, meta)
	idmapPath := expectedIDMapPath(volumeID, meta)

	switch meta.typ {
	case volumeTypeEphemeral:
		return evaluateRawMount(snap, mountPath, "btrfs", expectedLVPath(meta))
	case volumeTypeLUKSLoop, volumeTypeServiceData:
		return evaluateLUKSStack(snap, mountPath, mapper, "ext4", "")
	case volumeTypeGolden, volumeTypeServiceRootfs, volumeTypeWorkspace:
		return evaluateLUKSStack(snap, mountPath, mapper, "btrfs", idmapPath)
	default:
		return AttachStateUnknown, true
	}
}

// expectedLVPath returns the canonical LV device path /dev/<vg>/<lv> for
// volumes mounted directly off the LV (ephemeral). Empty when vg/lv aren't
// known.
func expectedLVPath(meta *volumeMetaSummary) string {
	if meta.vgName == "" || meta.lvName == "" {
		return ""
	}
	return "/dev/" + meta.vgName + "/" + meta.lvName
}

// evaluateRawMount checks an unencrypted volume — just a single fs mount
// on the LV directly. No mapper, no idmap. Used for ephemeral volumes.
// Source verification: a foreign btrfs mount at the same path would pass
// the fs-type check but not the source-path check.
func evaluateRawMount(snap kernelSnapshot, mountPath, expectedFS, expectedSource string) (AttachState, bool) {
	mounted, primary := snap.lookupMount(mountPath)
	if !mounted {
		return AttachStateDetached, true
	}
	if primary.FSType != expectedFS {
		return AttachStateForeignMountAtPath, true
	}
	// Source must match the expected LV path. mount(8) records what the
	// caller passed; for ephemeral we always pass /dev/<vg>/<lv>.
	if expectedSource != "" && primary.Source != expectedSource {
		return AttachStateForeignMountAtPath, true
	}
	return AttachStateAttached, true
}

// evaluateLUKSStack handles the single- and two-fs LUKS stack shapes.
// idmapPath == "" means single-fs (service-data, luks-loop); non-empty
// means two-fs (rootfs trio with idmap bind on top of the raw fs mount).
//
// Per-layer two-source agreement: declare ForeignMountAtPath only when
// the fs-type doesn't match AND the mount source resolves to the expected
// mapper (i.e., the kernel agrees the wrong fs is on our mapper). Pure
// single-source disagreement (sysfs lag vs mountinfo) escalates to Unknown
// for retry — defense against udev-storm false positives.
func evaluateLUKSStack(snap kernelSnapshot, mountPath, mapper, expectedFS, idmapPath string) (AttachState, bool) {
	mountedAtPrimary, primary := snap.lookupMount(mountPath)
	mapperPresent, mapperConsistent := snap.mapperPresent(mapper)

	mountedAtIDMap := false
	var idmapEntry mountEntry
	if idmapPath != "" {
		mountedAtIDMap, idmapEntry = snap.lookupMount(idmapPath)
	}

	if !mountedAtPrimary && !mountedAtIDMap && !mapperPresent {
		return AttachStateDetached, true
	}

	// Mountinfo entry without backing mapper. Cases:
	//   - sysfs lag: mapperConsistent=false → Unknown, retry.
	//   - mount source still names our mapper: genuine stale, we own it.
	//   - mount source names something else (slot reuse / foreign caller):
	//     not ours → ForeignMountAtPath. Avoids detachRootfsLocked unmounting
	//     a foreign mount during destroy (codex3-P2-C).
	if (mountedAtPrimary || mountedAtIDMap) && !mapperPresent {
		if !mapperConsistent {
			return AttachStateUnknown, false
		}
		ownsPrimary := !mountedAtPrimary || mountSourceMatchesMapper(snap, primary, mapper)
		ownsIDMap := !mountedAtIDMap || mountSourceMatchesMapper(snap, idmapEntry, mapper)
		if ownsPrimary && ownsIDMap {
			return AttachStateStaleMountRecord, true
		}
		return AttachStateForeignMountAtPath, true
	}

	// Mapper present but missing one or more required mounts → partial.
	// (Includes "idmap present but primary missing" — the bind has no
	// underlying source, treat as partial so the reconciler resets cleanly.)
	if mapperPresent && !mountedAtPrimary {
		return AttachStatePartialMapperOnly, true
	}

	// Primary mount present. Validate fs type + two-source agreement.
	if primary.FSType != expectedFS {
		if mountSourceMatchesMapper(snap, primary, mapper) {
			return AttachStateForeignMountAtPath, true
		}
		return AttachStateUnknown, false
	}
	if !mountSourceMatchesMapper(snap, primary, mapper) {
		return AttachStateUnknown, false
	}

	// IDMap layer (when required): same checks at the bind path.
	if idmapPath != "" {
		if !mountedAtIDMap {
			return AttachStatePartialMapperOnly, true
		}
		if idmapEntry.FSType != expectedFS {
			if mountSourceMatchesMapper(snap, idmapEntry, mapper) {
				return AttachStateForeignMountAtPath, true
			}
			return AttachStateUnknown, false
		}
		if !mountSourceMatchesMapper(snap, idmapEntry, mapper) {
			return AttachStateUnknown, false
		}
	}
	return AttachStateAttached, true
}

// lookupMount returns whether mountPath is a mount point in the snapshot
// and the corresponding entry if so.
func (s kernelSnapshot) lookupMount(mountPath string) (bool, mountEntry) {
	e, ok := s.mounts[filepath.Clean(mountPath)]
	return ok, e
}

// mapperPresent returns (present, consistent). When the snapshot has no
// dm-* listing at all (e.g., DM kernel module unloaded), returns
// (false, false) for present-but-not-found; the caller treats lack of
// agreement as inconsistency.
func (s kernelSnapshot) mapperPresent(mapper string) (bool, bool) {
	if mapper == "" {
		return false, true
	}
	_, ok := s.dmByName[mapper]
	return ok, true
}

// mountSourceMatchesMapper checks whether a mountinfo entry's source major:minor
// resolves (via sysfs dmByDev) to the expected mapper name. Two-source
// agreement: both mountinfo's source major:minor AND sysfs name agree on
// the same mapper.
//
// For mappers that the snapshot didn't observe in sysfs at all (mapper not
// present), returns false — the caller will already have routed to
// StaleMountRecord or Unknown via the present checks.
func mountSourceMatchesMapper(snap kernelSnapshot, mnt mountEntry, mapper string) bool {
	if mapper == "" {
		// No mapper expected — treat any device as agreeing trivially.
		// (Used by ephemeral, where the source is an LV path.)
		return true
	}
	devKey := strconv.Itoa(mnt.Major) + ":" + strconv.Itoa(mnt.Minor)
	mappedName, ok := snap.dmByDev[devKey]
	if !ok {
		// sysfs has no dm-N for this minor; might be a non-dm device
		// (workspace bind mount on a regular fs, foreign mount). Inspect
		// the source token; if it points at our mapper path, accept.
		return mnt.Source == "/dev/mapper/"+mapper
	}
	return mappedName == mapper
}

// LiveLayers returns the kernel-side layers belonging to the volume in
// tear-down order (top first). Returns an empty slice when the volume is
// not attached. Single-node only — the construction gate enforces this.
//
// The order reproduces what Detach must undo: idmap bind (top, if present)
// → fs mount → LUKS mapper → thin LV (or loop dev for control).
func (m *luksVolumeManager) LiveLayers(ctx context.Context, volumeID string) ([]LiveLayer, error) {
	meta, err := readVolumeMetaSummary(volumeID)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, nil
	}
	reader := m.kernelSnapshotFn
	if reader == nil {
		reader = readLiveKernelSnapshot
	}
	snap, err := reader(mappersOfInterest(volumeID, meta))
	if err != nil {
		return nil, fmt.Errorf("read kernel snapshot: %w", err)
	}

	var layers []LiveLayer
	mountPath := expectedMountPath(volumeID)
	idmapPath := expectedIDMapPath(volumeID, meta)
	mapper := expectedMapperName(volumeID, meta)

	// Top-down ordering. Only emit layers where the mount source actually
	// belongs to our mapper — a daemon restart can find the path reused
	// by an unrelated mount, and tearDownLayers (umount + cryptsetup close)
	// must not act on foreign state. mountSourceMatchesMapper is the same
	// two-source check that AttachStateOf uses to declare ForeignMountAtPath.
	if idmapPath != "" {
		if mounted, entry := snap.lookupMount(idmapPath); mounted && mountSourceMatchesMapper(snap, entry, mapper) {
			layers = append(layers, LiveLayer{
				Kind: LiveLayerKindIDMapBind,
				Name: filepath.Base(idmapPath),
				Path: idmapPath,
			})
		}
	}
	if mounted, entry := snap.lookupMount(mountPath); mounted && mountSourceMatchesMapper(snap, entry, mapper) {
		layers = append(layers, LiveLayer{
			Kind: LiveLayerKindFSMount,
			Name: filepath.Base(mountPath),
			Path: mountPath,
		})
	}
	if mapper != "" {
		if _, ok := snap.dmByName[mapper]; ok {
			layers = append(layers, LiveLayer{
				Kind: LiveLayerKindLUKSMapper,
				Name: mapper,
				Path: "/dev/mapper/" + mapper,
			})
		}
	}
	// Bottom layer: ephemeral has no mapper but uses LV directly; service-data,
	// rootfs use the LV under the mapper. Control uses a loop device.
	switch meta.typ {
	case volumeTypeLUKSLoop:
		// Loop device path is dynamic (losetup -j). We don't enumerate it
		// from sysfs here — Detach already discovers it via findLoop. Emit
		// a marker layer with empty Path so callers know the bottom layer
		// kind without forcing a losetup call from the probe.
		layers = append(layers, LiveLayer{
			Kind: LiveLayerKindLoopDev,
			Name: filepath.Base(meta.loopFile),
			Path: "",
		})
	case volumeTypeEphemeral:
		// Bottom is the LV itself; emit only when the mount or LV is live.
		// Without a mapper we use the mount as the indicator: if mount is
		// present, the LV is active.
		if mounted, _ := snap.lookupMount(mountPath); mounted {
			layers = append(layers, LiveLayer{
				Kind: LiveLayerKindThinLV,
				Name: meta.lvName,
				Path: "",
			})
		}
	default:
		// service-data, golden, workspace, service-rootfs: emit the thin
		// LV when the mapper above it is live. /dev/<vg>/<lv> activation
		// is implied by mapper existence.
		if _, ok := snap.dmByName[mapper]; ok {
			layers = append(layers, LiveLayer{
				Kind: LiveLayerKindThinLV,
				Name: meta.lvName,
				Path: "",
			})
		}
	}
	return layers, nil
}

// LUKSBackingDevice returns the kernel path of the device LUKS sits on
// (below the mapper) — the path suitable for in-place keyslot operations
// like cryptsetup luksAddKey / luksChangeKey. Distinct from LiveLayers,
// which is teardown enumeration.
//
// Behavior by type:
//   - service-data, golden, workspace, service-rootfs: returns the thin LV
//     path (/dev/<vg>/<lv>).
//   - luks-loop (control): returns the loop device path. Control passphrase
//     rotation already goes through LUKSLoopVolume's own primitives in
//     practice, so this branch exists for symmetry but is not exercised by
//     current callers.
//   - ephemeral: no LUKS layer — returns ErrUnsupportedVolumeType.
//
// Returns ErrNotAttached when the volume is not in AttachStateAttached.
//
// Lock contract: this is the advisory variant — uses AttachStateOf (which
// may promote to forced-under-lock via the K=5 escape hatch). Callers that
// already hold m.locks[volumeID] MUST use luksBackingDeviceUnderLock
// instead, otherwise the K-promotion path will deadlock on lock re-entry.
func (m *luksVolumeManager) LUKSBackingDevice(ctx context.Context, volumeID string) (string, error) {
	return m.luksBackingDeviceImpl(ctx, volumeID, false /* underLock */)
}

// luksBackingDeviceUnderLock is the lock-already-held variant. Used by
// transition callers (Attach reconciler, Detach, Resize, Destroy) that
// hold m.locks[volumeID]. Skips the K=5 promotion in the probe — the
// caller has already serialized us against transitions on this volume.
func (m *luksVolumeManager) luksBackingDeviceUnderLock(ctx context.Context, volumeID string) (string, error) {
	return m.luksBackingDeviceImpl(ctx, volumeID, true /* underLock */)
}

func (m *luksVolumeManager) luksBackingDeviceImpl(ctx context.Context, volumeID string, underLock bool) (string, error) {
	var state AttachState
	var err error
	if underLock {
		state, err = m.attachStateOfUnderLock(ctx, volumeID)
	} else {
		state, err = m.AttachStateOf(ctx, volumeID)
	}
	if err != nil {
		return "", err
	}
	if state != AttachStateAttached {
		return "", ErrNotAttached
	}
	meta, err := readVolumeMetaSummary(volumeID)
	if err != nil {
		return "", err
	}
	if meta == nil {
		return "", ErrNotAttached
	}
	switch meta.typ {
	case volumeTypeEphemeral:
		return "", ErrUnsupportedVolumeType
	case volumeTypeLUKSLoop:
		// Discover the loop device backing the loop file.
		if m.loopVol == nil {
			return "", ErrNotAttached
		}
		loopFile := paths.CoreJoin(meta.loopFile)
		dev, ferr := m.loopVol.findLoop(ctx, loopFile)
		if ferr != nil || dev == "" {
			return "", ErrNotAttached
		}
		return dev, nil
	default:
		// service-data / rootfs share the same convention: LUKS sits on the LV.
		if m.lvMgr == nil {
			return "", ErrNotAttached
		}
		return m.lvMgr.LVPath(meta.lvName), nil
	}
}

// resetVolume force-tears-down every kernel layer observed by LiveLayers
// (top-down). Internal primitive — not exposed via any user-facing surface;
// piccolod is a consumer appliance and end users are not expected to
// trigger reset. Refuses control (luks-loop): control's recovery is the
// crypto-reset path, a distinct lifecycle.
//
// Acquires the per-volume lock. Idempotent: already-detached volumes
// return nil. After reset, the next Attach starts from Detached.
//
// Reserved for future internal callers (e.g., automated recovery flows
// when a `ForeignMountAtPath` cluster is observed in fleet diagnostics).
// Tests cover the primitive; no HTTP route exposes it today.
func (m *luksVolumeManager) resetVolume(ctx context.Context, volumeID string) error {
	meta, err := readVolumeMetaSummary(volumeID)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if meta == nil {
		// No metadata — nothing to reset. Return success.
		return nil
	}
	if meta.typ == volumeTypeLUKSLoop {
		return fmt.Errorf("volume %s is type %q (control); reset not supported for control volumes — different recovery lifecycle", volumeID, meta.typ)
	}

	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()

	layers, err := m.LiveLayers(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("enumerate live layers: %w", err)
	}
	if errs := m.tearDownLayers(ctx, layers); len(errs) > 0 {
		return fmt.Errorf("reset volume %s: %v", volumeID, errs)
	}
	return nil
}

// tearDownLayers walks LiveLayers (top-down) and unmakes each. Used by
// detachRootfsLocked, detachAppVolume's LV deactivation, and resetVolume.
// Continues on per-layer errors and returns them collected.
func (m *luksVolumeManager) tearDownLayers(ctx context.Context, layers []LiveLayer) []error {
	var errs []error
	for _, layer := range layers {
		switch layer.Kind {
		case LiveLayerKindIDMapBind, LiveLayerKindFSMount:
			if err := m.run.Run(ctx, "umount", layer.Path); err != nil {
				if err2 := m.run.Run(ctx, "umount", "-l", layer.Path); err2 != nil {
					errs = append(errs, fmt.Errorf("umount %s: %w", layer.Path, err2))
				}
			}
		case LiveLayerKindLUKSMapper:
			if err := m.run.Run(ctx, "cryptsetup", "close", layer.Name); err != nil {
				errs = append(errs, fmt.Errorf("cryptsetup close %s: %w", layer.Name, err))
			}
		case LiveLayerKindThinLV:
			if m.lvMgr != nil && layer.Name != "" {
				_ = m.lvMgr.DeactivateLV(ctx, layer.Name)
			}
		case LiveLayerKindLoopDev:
			// Control volumes have their own lifecycle; loop layer is a
			// no-op here — callers that handle control (LUKSLoopVolume.Close)
			// detach the loop separately.
		}
	}
	return errs
}

// clearKernelStateCorrupted re-probes under the per-volume lock and acts on
// the result. Internal primitive — not exposed via any user-facing surface;
// piccolod is a consumer appliance and end users do not clear sticky state
// manually. Process restart already clears sticky (it lives in-memory), so
// the watchdog path covers the recovery case for free; this primitive is
// reserved for future internal callers (test harness, automated recovery).
//
//   - Clean re-probe (Detached / Attached): counter reset, sticky cleared,
//     returns the new state and nil error.
//   - PartialMapperOnly / StaleMountRecord: counter reset (state is
//     resolvable by reconciler); sticky cleared; returns the new state.
//   - Unknown: counter NOT reset, sticky NOT cleared, returns
//     ErrKernelStateAmbiguous.
//   - ForeignMountAtPath: counter reset (no longer corrupted, just foreign),
//     sticky cleared, returns the new state with no error — caller's
//     classification flips: ForeignMountAtPath is a separate failure mode.
func (m *luksVolumeManager) clearKernelStateCorrupted(ctx context.Context, volumeID string) (AttachState, error) {
	lock := m.lockFor(volumeID)
	lock.Lock()
	defer lock.Unlock()

	counter := m.unknownCounterFor(volumeID)
	state, perr := m.probeUnderLockClearPath(ctx, volumeID)
	switch state {
	case AttachStateUnknown:
		// Don't clear — the kernel is still ambiguous. Force-counter is
		// already incremented by our probe. Caller distinguishes from a
		// successful clear by inspecting the returned state and error.
		if perr == nil {
			perr = ErrKernelStateAmbiguous
		}
		return state, perr
	case AttachStateKernelStateCorrupted:
		// Sticky was already set and probe returned it — counter not reset.
		return state, ErrKernelStateCorrupted
	default:
		// All non-Unknown, non-Corrupted outcomes clear the sticky.
		counter.reset()
		return state, perr
	}
}

// probeUnderLockClearPath is a lock-already-held re-probe used by the
// admin clear-corrupted-state endpoint. It bypasses sticky-state checks
// (the whole point of the call is to clear sticky) and bypasses the
// promote-to-forced check (we're already under lock). Returns the raw
// probe result.
func (m *luksVolumeManager) probeUnderLockClearPath(ctx context.Context, volumeID string) (AttachState, error) {
	meta, err := readVolumeMetaSummary(volumeID)
	if err != nil {
		return AttachStateUnknown, err
	}
	if meta == nil {
		return AttachStateDetached, nil
	}
	state, ambiguous := m.probeAttachStateWithRetry(ctx, volumeID, meta)
	if state == AttachStateUnknown && ambiguous {
		return state, ErrKernelStateAmbiguous
	}
	return state, nil
}

// lockFor returns the per-volume lock, creating it on first reference.
// Used by transitions (Phase 2+) and by the K-counter forced-under-lock
// promotion in AttachStateOf.
func (m *luksVolumeManager) lockFor(volumeID string) *sync.Mutex {
	if v, ok := m.locks.Load(volumeID); ok {
		return v.(*sync.Mutex)
	}
	v, _ := m.locks.LoadOrStore(volumeID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// unknownCounterFor returns the per-volume Unknown-streak counter,
// creating it on first reference.
func (m *luksVolumeManager) unknownCounterFor(volumeID string) *unknownCounterEntry {
	if v, ok := m.unknownCounter.Load(volumeID); ok {
		return v.(*unknownCounterEntry)
	}
	v, _ := m.unknownCounter.LoadOrStore(volumeID, &unknownCounterEntry{})
	return v.(*unknownCounterEntry)
}
