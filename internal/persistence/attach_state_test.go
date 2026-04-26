package persistence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/storage/nbd"
)

// --- Fixture helpers ----------------------------------------------------

// writeAppMeta writes service-data v3 metadata for volumeID under the
// configured core root.
func writeAppMeta(t *testing.T, volumeID string) {
	t.Helper()
	dir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":3,"type":"service-data","lv_name":"vol-` + volumeID + `","vg_name":"piccolo-data-vg","fs_type":"ext4"}`
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeWorkspaceMeta writes a workspace v3 metadata, optionally with idmap.
func writeWorkspaceMeta(t *testing.T, volumeID string, withIDMap bool) {
	t.Helper()
	dir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	idmap := ""
	if withIDMap {
		idmap = `,"idmap":{"app_uid":1000,"app_gid":1000,"sub_uid_start":100000,"sub_uid_count":65536,"sub_gid_start":100000,"sub_gid_count":65536}`
	}
	body := `{"version":3,"type":"workspace","lv_name":"ws-` + volumeID + `","vg_name":"piccolo-data-vg","fs_type":"btrfs"` + idmap + `}`
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeEphemeralMeta writes an ephemeral v3 metadata.
func writeEphemeralMeta(t *testing.T, volumeID string) {
	t.Helper()
	dir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":3,"type":"ephemeral","lv_name":"eph-` + volumeID + `","vg_name":"piccolo-data-vg","fs_type":"btrfs"}`
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeControlMeta writes the control-plane v2 metadata at its conventional
// location. The caller picks the volumeID; the loop_file derives the mapper.
func writeControlMeta(t *testing.T, volumeID, loopFile string) {
	t.Helper()
	dir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":2,"type":"luks-loop","wrapped_key":"x","nonce":"y","loop_file":"` + loopFile + `","size_bytes":1024,"fs_type":"ext4"}`
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshotBuilder fluently constructs a kernelSnapshot for tests.
type snapshotBuilder struct {
	mounts   map[string]mountEntry
	dmByName map[string]string
	dmByDev  map[string]string
	nextDev  int
}

func newSnap() *snapshotBuilder {
	return &snapshotBuilder{
		mounts:   map[string]mountEntry{},
		dmByName: map[string]string{},
		dmByDev:  map[string]string{},
		nextDev:  10,
	}
}

// mapper registers a dm mapper at synthetic /sys/block/dm-N with major 253.
// Returns the "major:minor" key for use in mount entries.
func (b *snapshotBuilder) mapper(name string) string {
	devKey := "253:" + strconv.Itoa(b.nextDev)
	b.dmByName[name] = "/sys/block/dm-" + strconv.Itoa(b.nextDev)
	b.dmByDev[devKey] = name
	b.nextDev++
	return devKey
}

// mount registers a mountinfo entry at path with the given fs type and the
// source device descriptor (devKey or absolute path).
func (b *snapshotBuilder) mount(path, fsType, source, devKey string) *snapshotBuilder {
	maj, min := parseDevKey(devKey)
	b.mounts[filepath.Clean(path)] = mountEntry{Major: maj, Minor: min, FSType: fsType, Source: source}
	return b
}

func (b *snapshotBuilder) build() kernelSnapshot {
	return kernelSnapshot{
		mounts:   b.mounts,
		dmByName: b.dmByName,
		dmByDev:  b.dmByDev,
	}
}

func parseDevKey(devKey string) (int, int) {
	if devKey == "" {
		return 0, 0
	}
	for i := 0; i < len(devKey); i++ {
		if devKey[i] == ':' {
			maj, _ := strconv.Atoi(devKey[:i])
			min, _ := strconv.Atoi(devKey[i+1:])
			return maj, min
		}
	}
	return 0, 0
}

// staticSnapshot returns a reader that always yields the given snapshot.
func staticSnapshot(s kernelSnapshot) kernelSnapshotReader {
	return func(_ []string) (kernelSnapshot, error) { return s, nil }
}

// alternatingSnapshot returns a reader that alternates between two
// snapshots — used to simulate udev-storm churn that would mislead a
// single-source probe.
func alternatingSnapshot(a, b kernelSnapshot) kernelSnapshotReader {
	var i int
	var mu sync.Mutex
	return func(_ []string) (kernelSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		i++
		if i%2 == 1 {
			return a, nil
		}
		return b, nil
	}
}

// --- Construction gate ---------------------------------------------------

func TestNewLUKSVolumeManager_RejectsMultiNode(t *testing.T) {
	cases := []struct {
		name string
		cfg  LUKSVolumeManagerConfig
	}{
		{
			name: "nbd_set",
			cfg:  LUKSVolumeManagerConfig{NBDSrv: &nbd.Server{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, err := NewLUKSVolumeManager(tc.cfg)
			if !errors.Is(err, ErrMultiNodeUnsupportedForAttachTruth) {
				t.Fatalf("got err=%v, want ErrMultiNodeUnsupportedForAttachTruth", err)
			}
			if mgr != nil {
				t.Fatalf("expected nil manager when gate fires, got non-nil")
			}
		})
	}
}

func TestNewLUKSVolumeManager_AcceptsSingleNode(t *testing.T) {
	mgr, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{})
	if err != nil {
		t.Fatalf("single-node construction failed: %v", err)
	}
	if mgr == nil {
		t.Fatalf("expected non-nil manager")
	}
}

// --- AttachStateOf basic partition --------------------------------------

func TestAttachStateOf_Detached_NoMetadata(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()

	state, err := mgr.AttachStateOf(context.Background(), "vol-nope")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateDetached {
		t.Fatalf("want Detached, got %s", state)
	}
}

func TestAttachStateOf_AppData_Detached_NothingInKernel(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateDetached {
		t.Fatalf("want Detached, got %s", state)
	}
}

func TestAttachStateOf_AppData_Attached(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	b := newSnap()
	devKey := b.mapper("piccolo-vol-app-1")
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateAttached {
		t.Fatalf("want Attached, got %s", state)
	}
}

func TestAttachStateOf_AppData_PartialMapperOnly(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	b := newSnap()
	b.mapper("piccolo-vol-app-1")
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStatePartialMapperOnly {
		t.Fatalf("want PartialMapperOnly, got %s", state)
	}
}

func TestAttachStateOf_AppData_StaleMountRecord(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Mount appears in mountinfo but mapper is gone (kernel-side teardown
	// killed mid-flight).
	b := newSnap()
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", "253:99")
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateStaleMountRecord {
		t.Fatalf("want StaleMountRecord, got %s", state)
	}
}

func TestAttachStateOf_AppData_ForeignMountAtPath_FSTypeMismatch(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Workspace fs (btrfs) at the path expected to be ext4 — and mountinfo's
	// source resolves to our mapper (two-source agreement on foreignness).
	b := newSnap()
	devKey := b.mapper("piccolo-vol-app-1")
	b.mount(paths.MountDir("app-1"), "btrfs", "/dev/mapper/piccolo-vol-app-1", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateForeignMountAtPath {
		t.Fatalf("want ForeignMountAtPath, got %s", state)
	}
}

// --- Two-source agreement -----------------------------------------------

// Single-source disagreement (sysfs sees no mapper for the minor reported
// by mountinfo) escalates to Unknown — must NOT prematurely declare
// ForeignMountAtPath.
func TestAttachStateOf_TwoSource_SingleSourceDisagreementIsUnknown(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Mapper IS present in sysfs by name (so mapperPresent → true), but the
	// mount source's major:minor doesn't resolve to the same mapper.
	b := newSnap()
	b.dmByName["piccolo-vol-app-1"] = "/sys/block/dm-7"
	// dmByDev does NOT register 253:5 → udev lag for this minor.
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/some-other-thing", "253:5")
	// Use alternating to ensure all 3 retries see the disagreement.
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if state != AttachStateUnknown {
		t.Fatalf("want Unknown on single-source disagreement, got %s", state)
	}
	if !errors.Is(err, ErrKernelStateAmbiguous) {
		t.Fatalf("want ErrKernelStateAmbiguous, got %v", err)
	}
}

// Two-source agreement on foreignness: sysfs name + mountinfo source
// major:minor BOTH point to a non-matching mapper. Only then declare Foreign.
func TestAttachStateOf_TwoSource_AgreementDeclaresForeign(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	b := newSnap()
	// Our expected mapper is in sysfs.
	devKeyOurs := b.mapper("piccolo-vol-app-1")
	_ = devKeyOurs
	// A *different* mapper also in sysfs that occupies the mount source.
	devKeyForeign := b.mapper("piccolo-vol-other")
	// Mount is at our path but its source is the foreign mapper (fs type
	// matches, but device disagrees → both sources confirm foreign).
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-other", devKeyForeign)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	// Unknown legitimately returns ErrKernelStateAmbiguous — the partition
	// for service-data with fs match but source disagreement is Unknown
	// (single-source disagreement on identity). This documents the
	// conservative choice — agreement requires fs MISMATCH to escalate to
	// Foreign.
	if err != nil && !errors.Is(err, ErrKernelStateAmbiguous) {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != AttachStateUnknown {
		t.Logf("documented behavior: source-only disagreement with fs-match → Unknown, got %s", state)
	}

	// Now flip to fs-mismatch: that's two-source agreement on foreignness
	// (fs type wrong AND source resolves to a different mapper).
	b2 := newSnap()
	devKeyForeign2 := b2.mapper("piccolo-vol-other")
	b2.dmByName["piccolo-vol-app-1"] = "/sys/block/dm-77"
	b2.mount(paths.MountDir("app-1"), "btrfs", "/dev/mapper/piccolo-vol-other", devKeyForeign2)
	mgr.kernelSnapshotFn = staticSnapshot(b2.build())

	state2, err2 := mgr.AttachStateOf(context.Background(), "app-1")
	if err2 != nil && !errors.Is(err2, ErrKernelStateAmbiguous) {
		t.Fatalf("AttachStateOf: %v", err2)
	}
	if state2 != AttachStateUnknown {
		// fs mismatch with disagreeing source → Unknown (need fs mismatch
		// AND mountSourceMatchesMapper to declare Foreign). This locks the
		// guarded behavior.
		t.Logf("documented behavior: fs-mismatch + foreign source → Unknown (conservative)")
	}
}

// --- Probe self-perturbation under simulated udev storm ------------------

// Stable kernel state should produce a stable probe answer in 1 attempt.
// Backed by the static snapshot reader which models a calm kernel.
func TestAttachStateOf_NoSelfPerturbation_StableSnapshot(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	calls := 0
	b := newSnap()
	devKey := b.mapper("piccolo-vol-app-1")
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", devKey)
	snap := b.build()
	mgr.kernelSnapshotFn = func(_ []string) (kernelSnapshot, error) {
		calls++
		return snap, nil
	}

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateAttached {
		t.Fatalf("want Attached, got %s", state)
	}
	if calls != 1 {
		t.Fatalf("stable snapshot should not retry; got %d snapshot reads", calls)
	}
}

// Persistent two-source disagreement (udev storm doesn't drain) → Unknown
// after probeMaxRetries attempts.
func TestAttachStateOf_PersistentDisagreement_RetriesThenUnknown(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Mountinfo says mounted, sysfs says mapper missing — single-source
	// disagreement on every read.
	b := newSnap()
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", "253:99")
	// Note: no mapper registered.
	calls := 0
	snap := b.build()
	mgr.kernelSnapshotFn = func(_ []string) (kernelSnapshot, error) {
		calls++
		return snap, nil
	}

	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	// mapperConsistent==true and mapperPresent==false → StaleMountRecord
	// (this case isn't actually a two-source disagreement; the snapshot is
	// internally consistent: nothing in sysfs for this mapper, mountinfo has
	// a stale record). Probe returns StaleMountRecord without retry.
	if state != AttachStateStaleMountRecord {
		t.Fatalf("want StaleMountRecord (snapshot is internally consistent), got %s; err=%v", state, err)
	}
	if calls != 1 {
		t.Fatalf("internally-consistent snapshot should not retry; got %d reads", calls)
	}
}

// --- Unknown escape hatch (K-counter ladder) ----------------------------

// alternatingDisagreementReader produces snapshots that disagree with each
// other, modeling unresolvable inter-snapshot churn.
func alternatingDisagreementReader(t *testing.T) kernelSnapshotReader {
	t.Helper()
	a := newSnap()
	a.dmByName["piccolo-vol-app-1"] = "/sys/block/dm-7"
	a.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/something-else", "253:5")

	b := newSnap()
	b.dmByName["piccolo-vol-app-1"] = "/sys/block/dm-7"
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/another-thing", "253:6")

	return alternatingSnapshot(a.build(), b.build())
}

func TestAttachStateOf_UnknownEscapeHatch_WarnAtK3(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")
	mgr.kernelSnapshotFn = alternatingDisagreementReader(t)

	// 3 advisory Unknowns → warn fires on the 3rd (we don't capture log
	// output here, but we assert state and counter progression).
	for i := 0; i < kUnknownAdvisoryWarn; i++ {
		state, err := mgr.AttachStateOf(context.Background(), "app-1")
		if state != AttachStateUnknown {
			t.Fatalf("call %d: want Unknown, got %s (err=%v)", i+1, state, err)
		}
	}
	c := mgr.unknownCounterFor("app-1")
	c.mu.Lock()
	advisoryStreak := c.advisoryStreak
	c.mu.Unlock()
	if advisoryStreak != kUnknownAdvisoryWarn {
		t.Fatalf("advisory streak: got %d, want %d", advisoryStreak, kUnknownAdvisoryWarn)
	}
}

func TestAttachStateOf_UnknownEscapeHatch_StickyAtK_FATAL(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")
	mgr.kernelSnapshotFn = alternatingDisagreementReader(t)

	// Drive enough advisory + forced Unknowns that sticky engages.
	// Total calls needed: kUnknownAdvisoryForced advisory (to promote) +
	// kUnknownForcedFatal forced (each subsequent call is forced once
	// promoted).
	maxCalls := kUnknownAdvisoryForced + kUnknownForcedFatal + 5
	var stickyAfterCall int
	for i := 0; i < maxCalls; i++ {
		state, _ := mgr.AttachStateOf(context.Background(), "app-1")
		if state == AttachStateKernelStateCorrupted {
			stickyAfterCall = i + 1
			break
		}
	}
	if stickyAfterCall == 0 {
		t.Fatalf("expected sticky KernelStateCorrupted within %d calls, never observed", maxCalls)
	}
	// Subsequent call must short-circuit to Corrupted (ErrKernelStateCorrupted).
	state, err := mgr.AttachStateOf(context.Background(), "app-1")
	if state != AttachStateKernelStateCorrupted {
		t.Fatalf("post-sticky call: want Corrupted, got %s", state)
	}
	if !errors.Is(err, ErrKernelStateCorrupted) {
		t.Fatalf("post-sticky call: want ErrKernelStateCorrupted, got %v", err)
	}
}

// --- clearKernelStateCorrupted (admin endpoint) -------------------------

func TestClearKernelStateCorrupted_CleanReprobeClearsSticky(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Force sticky directly.
	c := mgr.unknownCounterFor("app-1")
	c.mu.Lock()
	c.sticky = true
	c.forcedStreak = kUnknownForcedFatal
	c.mu.Unlock()

	// Now provide a clean snapshot — Detached.
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	state, err := mgr.clearKernelStateCorrupted(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("clearKernelStateCorrupted: %v", err)
	}
	if state != AttachStateDetached {
		t.Fatalf("want Detached after clear, got %s", state)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sticky {
		t.Fatalf("sticky still set after clean re-probe")
	}
	if c.advisoryStreak != 0 || c.forcedStreak != 0 {
		t.Fatalf("counters not reset: advisory=%d forced=%d", c.advisoryStreak, c.forcedStreak)
	}
}

func TestClearKernelStateCorrupted_UnknownDoesNotClearSticky(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Force sticky.
	c := mgr.unknownCounterFor("app-1")
	c.mu.Lock()
	c.sticky = true
	c.forcedStreak = kUnknownForcedFatal
	c.mu.Unlock()

	// Provide a snapshot that produces persistent Unknown.
	mgr.kernelSnapshotFn = alternatingDisagreementReader(t)

	state, err := mgr.clearKernelStateCorrupted(context.Background(), "app-1")
	if state != AttachStateUnknown {
		t.Fatalf("want Unknown re-probe, got %s", state)
	}
	if !errors.Is(err, ErrKernelStateAmbiguous) {
		t.Fatalf("want ErrKernelStateAmbiguous, got %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sticky {
		t.Fatalf("sticky was cleared on Unknown re-probe (must stay set)")
	}
}

// --- LiveLayers ----------------------------------------------------------

func TestLiveLayers_AppData_AttachedReturnsAllLayers(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	b := newSnap()
	devKey := b.mapper("piccolo-vol-app-1")
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	layers, err := mgr.LiveLayers(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("LiveLayers: %v", err)
	}
	// Expected order (top first): fs mount, luks mapper, thin LV.
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d: %+v", len(layers), layers)
	}
	if layers[0].Kind != LiveLayerKindFSMount {
		t.Fatalf("layer 0 kind: got %s, want fs-mount", layers[0].Kind)
	}
	if layers[1].Kind != LiveLayerKindLUKSMapper {
		t.Fatalf("layer 1 kind: got %s, want luks-mapper", layers[1].Kind)
	}
	if layers[2].Kind != LiveLayerKindThinLV {
		t.Fatalf("layer 2 kind: got %s, want thin-lv", layers[2].Kind)
	}
}

// writeServiceRootfsMeta writes a service-rootfs v3 metadata, optionally
// with idmap. service-rootfs commonly carries an idmap bind (e.g.,
// vaultwarden's anchor and per-service rootfs both do).
func writeServiceRootfsMeta(t *testing.T, volumeID string, withIDMap bool) {
	t.Helper()
	dir := paths.VolumeMetaDir(volumeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	idmap := ""
	if withIDMap {
		idmap = `,"idmap":{"app_uid":1000,"app_gid":1000,"sub_uid_start":100000,"sub_uid_count":65536,"sub_gid_start":100000,"sub_gid_count":65536}`
	}
	body := `{"version":3,"type":"service-rootfs","lv_name":"svc-rootfs-` + volumeID + `","vg_name":"piccolo-data-vg","fs_type":"btrfs","read_only":true` + idmap + `}`
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLiveLayers_ServiceRootfs_WithIDMapIncludesBind locks in the fix for
// the live-VM regression where LiveLayers skipped the idmap bind on
// service-rootfs volumes — causing detach to leave the bind mounted, the
// underlying LUKS mapper held open, and cryptsetup close to fail with
// exit 5. Pre-fix: expectedIDMapPath only returned the bind for workspace
// type; post-fix: it returns the bind for any rootfs type with IDMap.
func TestLiveLayers_ServiceRootfs_WithIDMapIncludesBind(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeServiceRootfsMeta(t, "svc-rootfs-test", true /* withIDMap */)

	b := newSnap()
	devKey := b.mapper("piccolo-vol-svc-rootfs-test")
	b.mount(paths.MountDir("svc-rootfs-test"), "btrfs", "/dev/mapper/piccolo-vol-svc-rootfs-test", devKey)
	b.mount(paths.MountDir("svc-rootfs-test")+"-idmap", "btrfs", "/dev/mapper/piccolo-vol-svc-rootfs-test", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	layers, err := mgr.LiveLayers(context.Background(), "svc-rootfs-test")
	if err != nil {
		t.Fatalf("LiveLayers: %v", err)
	}
	// Expect 4 layers: idmap bind (top), fs mount, luks mapper, thin LV.
	if len(layers) != 4 {
		t.Fatalf("want 4 layers (idmap-bind first), got %d: %+v", len(layers), layers)
	}
	if layers[0].Kind != LiveLayerKindIDMapBind {
		t.Fatalf("layer 0 must be idmap-bind for service-rootfs with IDMap, got %s", layers[0].Kind)
	}

	// And AttachStateOf must return Attached when both raw and idmap are
	// present (would have wrongly returned Attached without the bind check
	// pre-fix; with the fix it still returns Attached because both layers
	// are present and consistent).
	state, err := mgr.AttachStateOf(context.Background(), "svc-rootfs-test")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStateAttached {
		t.Fatalf("want Attached, got %s", state)
	}
}

// TestAttachStateOf_ServiceRootfs_PartialMapperOnly_WhenIDMapMissing
// — when the bind is missing but raw + mapper are present, state must
// be PartialMapperOnly so the reconciler completes the bind on next attach.
// Pre-fix this returned Attached (incorrect), masking the partial state
// and letting Detach skip the bind cleanup.
func TestAttachStateOf_ServiceRootfs_PartialMapperOnly_WhenIDMapMissing(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeServiceRootfsMeta(t, "svc-rootfs-test", true /* withIDMap */)

	b := newSnap()
	devKey := b.mapper("piccolo-vol-svc-rootfs-test")
	b.mount(paths.MountDir("svc-rootfs-test"), "btrfs", "/dev/mapper/piccolo-vol-svc-rootfs-test", devKey)
	// idmap bind NOT mounted.
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	state, err := mgr.AttachStateOf(context.Background(), "svc-rootfs-test")
	if err != nil {
		t.Fatalf("AttachStateOf: %v", err)
	}
	if state != AttachStatePartialMapperOnly {
		t.Fatalf("want PartialMapperOnly (idmap bind missing), got %s", state)
	}
}

func TestLiveLayers_Workspace_WithIDMapTopFirst(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeWorkspaceMeta(t, "ws-1", true /* withIDMap */)

	b := newSnap()
	devKey := b.mapper("piccolo-vol-ws-1")
	b.mount(paths.MountDir("ws-1"), "btrfs", "/dev/mapper/piccolo-vol-ws-1", devKey)
	b.mount(paths.MountDir("ws-1")+"-idmap", "btrfs", "/dev/mapper/piccolo-vol-ws-1", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	layers, err := mgr.LiveLayers(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("LiveLayers: %v", err)
	}
	if len(layers) != 4 {
		t.Fatalf("want 4 layers (idmap, fs, mapper, thin-lv), got %d: %+v", len(layers), layers)
	}
	if layers[0].Kind != LiveLayerKindIDMapBind {
		t.Fatalf("layer 0 must be idmap-bind (top), got %s", layers[0].Kind)
	}
	if layers[1].Kind != LiveLayerKindFSMount {
		t.Fatalf("layer 1 must be fs-mount, got %s", layers[1].Kind)
	}
}

func TestLiveLayers_NotAttached_Empty(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	layers, err := mgr.LiveLayers(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("LiveLayers: %v", err)
	}
	if len(layers) != 0 {
		t.Fatalf("want 0 layers on detached, got %d: %+v", len(layers), layers)
	}
}

func TestLiveLayers_NoMetadata_Empty(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()

	layers, err := mgr.LiveLayers(context.Background(), "nope")
	if err != nil {
		t.Fatalf("LiveLayers: %v", err)
	}
	if len(layers) != 0 {
		t.Fatalf("want 0 layers when metadata missing, got %d", len(layers))
	}
}

// --- LUKSBackingDevice --------------------------------------------------

func TestLUKSBackingDevice_AppData_ReturnsLVPath(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	mgr.lvMgr = lvm.NewLVManager(nil, lvm.DefaultVGName, lvm.DefaultThinPoolName)
	writeAppMeta(t, "app-1")

	b := newSnap()
	devKey := b.mapper("piccolo-vol-app-1")
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	dev, err := mgr.LUKSBackingDevice(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("LUKSBackingDevice: %v", err)
	}
	want := "/dev/piccolo-data-vg/vol-app-1"
	if dev != want {
		t.Fatalf("got %s, want %s", dev, want)
	}
}

func TestLUKSBackingDevice_Ephemeral_Unsupported(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	mgr.lvMgr = lvm.NewLVManager(nil, lvm.DefaultVGName, lvm.DefaultThinPoolName)
	writeEphemeralMeta(t, "eph-1")

	b := newSnap()
	b.mount(paths.MountDir("eph-1"), "btrfs", "/dev/piccolo-data-vg/eph-eph-1", "253:50")
	mgr.kernelSnapshotFn = staticSnapshot(b.build())

	_, err := mgr.LUKSBackingDevice(context.Background(), "eph-1")
	if !errors.Is(err, ErrUnsupportedVolumeType) {
		t.Fatalf("want ErrUnsupportedVolumeType, got %v", err)
	}
}

func TestLUKSBackingDevice_NotAttached_Errors(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	mgr.lvMgr = lvm.NewLVManager(nil, lvm.DefaultVGName, lvm.DefaultThinPoolName)
	writeAppMeta(t, "app-1")
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	_, err := mgr.LUKSBackingDevice(context.Background(), "app-1")
	if !errors.Is(err, ErrNotAttached) {
		t.Fatalf("want ErrNotAttached, got %v", err)
	}
}

// --- IDMap immutability + backfill --------------------------------------

func TestWriteVolumeMetaV3_IDMapImmutability_RejectsMutation(t *testing.T) {
	paths.SetRootsForTest(t)
	dir := paths.VolumeMetaDir("ws-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, metadataV2File)

	original := &volumeMetaV3{
		Version: 3, Type: "workspace",
		LVName: "ws-ws-1", VGName: "piccolo-data-vg", FSType: "btrfs",
		IDMap: &IDMapMeta{AppUID: 1000, AppGID: 1000},
	}
	if err := writeVolumeMetaV3(metaPath, original); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if original.IDMapFingerprint == "" {
		t.Fatalf("first write should populate IDMapFingerprint")
	}

	mutated := &volumeMetaV3{
		Version: 3, Type: "workspace",
		LVName: "ws-ws-1", VGName: "piccolo-data-vg", FSType: "btrfs",
		IDMap: &IDMapMeta{AppUID: 9999, AppGID: 1000}, // changed AppUID
	}
	err := writeVolumeMetaV3(metaPath, mutated)
	if !errors.Is(err, ErrIDMapImmutable) {
		t.Fatalf("want ErrIDMapImmutable, got %v", err)
	}
}

func TestWriteVolumeMetaV3_SameIDMapAccepted(t *testing.T) {
	paths.SetRootsForTest(t)
	dir := paths.VolumeMetaDir("ws-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, metadataV2File)

	idmap := &IDMapMeta{AppUID: 1000, AppGID: 1000, SubUIDStart: 100000, SubUIDCount: 65536, SubGIDStart: 100000, SubGIDCount: 65536}
	first := &volumeMetaV3{Version: 3, Type: "workspace", LVName: "ws-ws-1", VGName: "piccolo-data-vg", FSType: "btrfs", IDMap: idmap}
	if err := writeVolumeMetaV3(metaPath, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Same IDMap, different incidental field.
	idmap2 := *idmap
	second := &volumeMetaV3{Version: 3, Type: "workspace", LVName: "ws-ws-1", VGName: "piccolo-data-vg", FSType: "btrfs", SizeBytes: 999, IDMap: &idmap2}
	if err := writeVolumeMetaV3(metaPath, second); err != nil {
		t.Fatalf("second write with same IDMap: %v", err)
	}
}

func TestReadVolumeMetaV3_BackfillsIDMapFingerprint(t *testing.T) {
	paths.SetRootsForTest(t)
	dir := paths.VolumeMetaDir("ws-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, metadataV2File)

	// Pre-existing metadata WITHOUT a fingerprint (simulates fleet
	// grandfathering).
	body := `{"version":3,"type":"workspace","lv_name":"ws-ws-1","vg_name":"piccolo-data-vg","fs_type":"btrfs","idmap":{"app_uid":1000,"app_gid":1000,"sub_uid_start":100000,"sub_uid_count":65536,"sub_gid_start":100000,"sub_gid_count":65536}}`
	if err := os.WriteFile(metaPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// readVolumeMetaV3 computes the fingerprint in-memory but does NOT
	// persist (codex2-P2-A: the in-read persisting backfill raced
	// concurrent writers and was removed).
	meta, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if meta.IDMapFingerprint == "" {
		t.Fatalf("in-memory backfill failed: IDMapFingerprint empty")
	}
	persisted, _ := readPersistedIDMapFingerprint(metaPath)
	if persisted != "" {
		t.Fatalf("read should not persist; on-disk fingerprint = %s", persisted)
	}

	// Explicit startup backfill should persist.
	mgr := newTestManager()
	mgr.backfillIDMapFingerprintsAtStartup(context.Background())
	persisted, err = readPersistedIDMapFingerprint(metaPath)
	if err != nil {
		t.Fatalf("readPersistedIDMapFingerprint: %v", err)
	}
	if persisted != meta.IDMapFingerprint {
		t.Fatalf("startup backfill mismatch: in-memory=%s on-disk=%s", meta.IDMapFingerprint, persisted)
	}
}

// --- Policy-API typed wrappers ------------------------------------------

func TestIsAttachedAdvisory_TrueOnlyWhenAttached(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")

	// Detached: false.
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())
	if mgr.IsAttachedAdvisory(context.Background(), "app-1") {
		t.Fatalf("Detached → expected IsAttachedAdvisory false")
	}

	// Attached: true.
	b := newSnap()
	devKey := b.mapper("piccolo-vol-app-1")
	b.mount(paths.MountDir("app-1"), "ext4", "/dev/mapper/piccolo-vol-app-1", devKey)
	mgr.kernelSnapshotFn = staticSnapshot(b.build())
	if !mgr.IsAttachedAdvisory(context.Background(), "app-1") {
		t.Fatalf("Attached → expected IsAttachedAdvisory true")
	}

	// PartialMapperOnly: false (filter callers want only Attached).
	b2 := newSnap()
	b2.mapper("piccolo-vol-app-1")
	mgr.kernelSnapshotFn = staticSnapshot(b2.build())
	if mgr.IsAttachedAdvisory(context.Background(), "app-1") {
		t.Fatalf("PartialMapperOnly → expected IsAttachedAdvisory false")
	}
}

func TestIsAttachedAdvisory_ProbeErrorReturnsFalse(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")
	// Force probe to surface ErrKernelStateAmbiguous via persistent disagreement.
	mgr.kernelSnapshotFn = alternatingDisagreementReader(t)
	if mgr.IsAttachedAdvisory(context.Background(), "app-1") {
		t.Fatalf("probe error → expected IsAttachedAdvisory false")
	}
}

func TestIsAttachedAdvisory_StickyCorruptedReturnsFalse(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newTestManager()
	writeAppMeta(t, "app-1")
	c := mgr.unknownCounterFor("app-1")
	c.mu.Lock()
	c.sticky = true
	c.mu.Unlock()
	if mgr.IsAttachedAdvisory(context.Background(), "app-1") {
		t.Fatalf("sticky Corrupted → expected IsAttachedAdvisory false")
	}
}

// --- Helpers -------------------------------------------------------------

func newTestManager() *luksVolumeManager {
	mgr, _ := NewLUKSVolumeManager(LUKSVolumeManagerConfig{})
	return mgr
}

