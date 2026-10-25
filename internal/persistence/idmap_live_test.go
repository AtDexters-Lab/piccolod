package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
)

// The recorded maps written by writeServiceRootfsMeta/writeWorkspaceMeta.
var (
	recordedTestIDMap = IDMapConfig{AppUID: 1000, AppGID: 1000, SubUIDStart: 100000, SubUIDCount: 65536, SubGIDStart: 100000, SubGIDCount: 65536}
	// The same app after its user was re-created with new outer IDs, as on
	// the node where pa-piccolospace moved from 474/472 to 468/468.
	recreatedTestIDMap = IDMapConfig{AppUID: 2000, AppGID: 2001, SubUIDStart: 100000, SubUIDCount: 65536, SubGIDStart: 100000, SubGIDCount: 65536}
)

// failingRunner fails every command so an attach stops at its first
// physical step (activating the thin LV).
type failingRunner struct {
	calls []string
}

func (r *failingRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return errors.New("injected failure: " + name)
}

func (r *failingRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return nil, r.Run(ctx, name, args...)
}

func (r *failingRunner) RunWithStdin(ctx context.Context, _ []byte, name string, args ...string) error {
	return r.Run(ctx, name, args...)
}

func newFailingAttachManager() (*luksVolumeManager, *failingRunner) {
	run := &failingRunner{}
	mgr := newTestManager()
	mgr.run = run
	mgr.lvMgr = lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName)
	return mgr, run
}

func readTestMeta(t *testing.T, volumeID string) *volumeMetaV3 {
	t.Helper()
	meta, err := readVolumeMetaV3(filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	persisted, err := readPersistedIDMapFingerprint(filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File))
	if err != nil {
		t.Fatalf("read persisted fingerprint: %v", err)
	}
	if persisted != "" && persisted != meta.IDMapFingerprint {
		t.Fatalf("persisted fingerprint %s disagrees with its record %s", persisted, meta.IDMapFingerprint)
	}
	return meta
}

func attachedServiceRootfsSnapshot(volumeID string, withRaw, withBind bool) kernelSnapshot {
	b := newSnap()
	mapper := volMapperName(volumeID)
	devKey := b.mapper(mapper)
	if withRaw {
		b.mount(paths.MountDir(volumeID), "btrfs", "/dev/mapper/"+mapper, devKey)
	}
	if withBind {
		b.mount(paths.MountDir(volumeID)+"-idmap", "btrfs", "/dev/mapper/"+mapper, devKey)
	}
	return b.build()
}

func TestLiveIDMapChanged(t *testing.T) {
	recorded := idMapMeta(recordedTestIDMap)
	tests := []struct {
		name    string
		meta    volumeMetaV3
		live    *IDMapConfig
		changed bool
		wantErr bool
	}{
		{name: "matching record", meta: volumeMetaV3{Type: volumeTypeServiceRootfs, IDMap: &recorded}, live: &recordedTestIDMap},
		{name: "user re-created", meta: volumeMetaV3{Type: volumeTypeWorkspace, IDMap: &recorded}, live: &recreatedTestIDMap, changed: true},
		{name: "no record yet", meta: volumeMetaV3{Type: volumeTypeWorkspace}, live: &recreatedTestIDMap, changed: true},
		{name: "app volume without live map", meta: volumeMetaV3{Type: volumeTypeWorkspace, IDMap: &recorded}, wantErr: true},
		{name: "volume without any map", meta: volumeMetaV3{Type: volumeTypeServiceRootfs}},
		{name: "golden content", meta: volumeMetaV3{Type: volumeTypeGolden}},
		{name: "golden content with app map", meta: volumeMetaV3{Type: volumeTypeGolden}, live: &recordedTestIDMap, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, err := liveIDMapChanged("vol", &tt.meta, tt.live)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if changed != tt.changed {
				t.Fatalf("changed = %v, want %v", changed, tt.changed)
			}
		})
	}
}

// An attached bind built from the old record may back running containers.
// It must be refused, never returned as if it matched the app user.
func TestAttachRootfsRefusesBindMountedWithStaleIDMap(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "svc-rootfs-site--__netns__"
	writeServiceRootfsMeta(t, volumeID, true)
	mgr := newTestManager()
	mgr.kernelSnapshotFn = staticSnapshot(attachedServiceRootfsSnapshot(volumeID, true, true))

	_, err := mgr.AttachRootfs(context.Background(), volumeID, &recreatedTestIDMap)
	if !errors.Is(err, ErrIDMapStaleMount) {
		t.Fatalf("AttachRootfs error = %v, want ErrIDMapStaleMount", err)
	}
	for _, want := range []string{"uid 1000 gid 1000", "uid 2000 gid 2001"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should name %q", err, want)
		}
	}
	if got := readTestMeta(t, volumeID).IDMap; *got != idMapMeta(recordedTestIDMap) {
		t.Fatalf("record changed under a live bind: %+v", got)
	}
}

func TestAttachRootfsReturnsBindMountedWithLiveIDMap(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "svc-rootfs-site--__netns__"
	writeServiceRootfsMeta(t, volumeID, true)
	mgr := newTestManager()
	mgr.kernelSnapshotFn = staticSnapshot(attachedServiceRootfsSnapshot(volumeID, true, true))

	handle, err := mgr.AttachRootfs(context.Background(), volumeID, &recordedTestIDMap)
	if err != nil {
		t.Fatalf("AttachRootfs: %v", err)
	}
	if handle.MountPath != paths.MountDir(volumeID)+"-idmap" {
		t.Fatalf("mount path = %s, want the idmapped bind", handle.MountPath)
	}
}

// A bind left behind without its raw mount was still built from the old
// record, so the partial state is refused as well.
func TestAttachRootfsRefusesBindLeftWithoutRawMount(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "svc-rootfs-site--__netns__"
	writeServiceRootfsMeta(t, volumeID, true)
	mgr, run := newFailingAttachManager()
	mgr.kernelSnapshotFn = staticSnapshot(attachedServiceRootfsSnapshot(volumeID, false, true))

	_, err := mgr.AttachRootfs(context.Background(), volumeID, &recreatedTestIDMap)
	if !errors.Is(err, ErrIDMapStaleMount) {
		t.Fatalf("AttachRootfs error = %v, want ErrIDMapStaleMount", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("refused attach ran commands: %v", run.calls)
	}
	if got := readTestMeta(t, volumeID).IDMap; *got != idMapMeta(recordedTestIDMap) {
		t.Fatalf("record changed under a live bind: %+v", got)
	}
}

// With nothing mounted, the record follows the live map before any bind is
// built from it, so the bind the attach goes on to create uses the app
// user's current IDs.
func TestAttachRootfsRecordsLiveIDMapBeforeBuildingBind(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "svc-rootfs-site--__netns__"
	writeServiceRootfsMeta(t, volumeID, true)
	mgr, run := newFailingAttachManager()
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	_, err := mgr.AttachRootfs(context.Background(), volumeID, &recreatedTestIDMap)
	if err == nil || errors.Is(err, ErrIDMapStaleMount) {
		t.Fatalf("AttachRootfs error = %v, want the injected activation failure", err)
	}
	if len(run.calls) == 0 || !strings.HasPrefix(run.calls[0], "lvchange -ay") {
		t.Fatalf("attach should proceed to activation after recording the map, calls=%v", run.calls)
	}
	meta := readTestMeta(t, volumeID)
	if *meta.IDMap != idMapMeta(recreatedTestIDMap) {
		t.Fatalf("record = %+v, want the live map %+v", *meta.IDMap, recreatedTestIDMap)
	}
	if meta.IDMapFingerprint != computeIDMapFingerprint(meta.IDMap) {
		t.Fatalf("fingerprint %s does not cover the live map", meta.IDMapFingerprint)
	}
}

// End to end: the bind the attach builds uses the live map, not the record
// it read, so the container sees its own files as root-owned again.
func TestAttachRootfsBuildsBindFromLiveIDMap(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	const volumeID = "svc-rootfs-site--__netns__"
	writeServiceRootfsMeta(t, volumeID, true)
	run := &fakeRunner{}
	mgr := newTestManager()
	mgr.run = run
	mgr.crypto = newResizeTestCryptManager(t, core)
	mgr.tmpfsDir = t.TempDir()
	mgr.lvMgr = lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName)
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())
	var built []fsutil.IDMapConfig
	mgr.idmappedMountFn = func(_, target string, config fsutil.IDMapConfig) error {
		if target != paths.MountDir(volumeID)+"-idmap" {
			t.Fatalf("bind target = %s", target)
		}
		built = append(built, config)
		return nil
	}

	handle, err := mgr.AttachRootfs(context.Background(), volumeID, &recreatedTestIDMap)
	if err != nil {
		t.Fatalf("AttachRootfs: %v", err)
	}
	if len(built) != 1 || built[0] != recreatedTestIDMap {
		t.Fatalf("binds built with %+v, want one with the live map %+v", built, recreatedTestIDMap)
	}
	if handle.MountPath != paths.MountDir(volumeID)+"-idmap" {
		t.Fatalf("mount path = %s, want the idmapped bind", handle.MountPath)
	}
}

// Writers that do not take the per-volume lock (keyslot reconciliation) may
// stamp metadata after the attach read it; following the user keeps them.
func TestFollowLiveIDMapKeepsFieldsStampedAfterAttachRead(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "ws-site"
	writeWorkspaceMeta(t, volumeID, true)
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	readByAttach := readTestMeta(t, volumeID)
	stamped := readTestMeta(t, volumeID)
	stamped.PasswordKeyslotKeyID = "3c20941cab36bb83"
	if err := writeVolumeMetaV3(metaPath, stamped); err != nil {
		t.Fatal(err)
	}
	mgr := newTestManager()
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	if err := mgr.followLiveIDMap(context.Background(), volumeID, metaPath, readByAttach, recreatedTestIDMap); err != nil {
		t.Fatalf("followLiveIDMap: %v", err)
	}
	onDisk := readTestMeta(t, volumeID)
	if onDisk.PasswordKeyslotKeyID != stamped.PasswordKeyslotKeyID {
		t.Fatalf("keyslot stamp = %q, want %q kept", onDisk.PasswordKeyslotKeyID, stamped.PasswordKeyslotKeyID)
	}
	if *onDisk.IDMap != idMapMeta(recreatedTestIDMap) || *readByAttach.IDMap != idMapMeta(recreatedTestIDMap) {
		t.Fatalf("record on disk %+v / for the bind %+v, want the live map", *onDisk.IDMap, *readByAttach.IDMap)
	}
}

func TestAttachRootfsRequiresLiveIDMapForAppVolumes(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "svc-rootfs-site--main"
	writeServiceRootfsMeta(t, volumeID, true)
	mgr, run := newFailingAttachManager()
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())

	if _, err := mgr.AttachRootfs(context.Background(), volumeID, nil); err == nil ||
		!strings.Contains(err.Error(), "live ID map") {
		t.Fatalf("AttachRootfs error = %v, want a missing live map refusal", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("refused attach ran commands: %v", run.calls)
	}
}

// Only the attach path may change a recorded map. Generic metadata writers
// (resize, keyslot rotation) still cannot move it, including back to the
// value they read before the record followed the app user.
func TestRecordedIDMapStaysImmutableForOtherWriters(t *testing.T) {
	paths.SetRootsForTest(t)
	const volumeID = "ws-site"
	writeWorkspaceMeta(t, volumeID, true)
	metaPath := filepath.Join(paths.VolumeMetaDir(volumeID), metadataV2File)
	stale := readTestMeta(t, volumeID)
	if err := writeVolumeMetaV3(metaPath, stale); err != nil {
		t.Fatalf("persist starting record: %v", err)
	}

	followed := readTestMeta(t, volumeID)
	if err := writeLiveIDMap(metaPath, volumeID, followed, recreatedTestIDMap); err != nil {
		t.Fatalf("writeLiveIDMap: %v", err)
	}
	if err := writeVolumeMetaV3(metaPath, stale); !errors.Is(err, ErrIDMapImmutable) {
		t.Fatalf("writing the pre-follow record = %v, want ErrIDMapImmutable", err)
	}
	current := readTestMeta(t, volumeID)
	current.SizeBytes = 4096
	if err := writeVolumeMetaV3(metaPath, current); err != nil {
		t.Fatalf("unrelated write after follow: %v", err)
	}
}

func TestCloneWorkspaceRequiresCloneOwnerIDMap(t *testing.T) {
	paths.SetRootsForTest(t)
	writeWorkspaceMeta(t, "ws-origin", true)
	mgr, run := newFailingAttachManager()

	if _, err := mgr.CloneWorkspace(context.Background(), "origin", "copy", nil); err == nil ||
		!strings.Contains(err.Error(), "live ID map") {
		t.Fatalf("CloneWorkspace error = %v, want a missing clone map refusal", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("refused clone ran commands: %v", run.calls)
	}
}

func TestRootfsVolumeInstanceIDs(t *testing.T) {
	tests := map[string][]string{
		"ws-site":                         {"site"},
		"svc-rootfs-site":                 {"site"},
		"svc-rootfs-site--__netns__":      {"site--__netns__", "site"},
		"svc-rootfs-site--main--0123abcd": {"site--main--0123abcd", "site", "site--main"},
		"golden-0123abcd":                 nil,
		"app-site":                        nil,
		"ws-":                             nil,
		"svc-rootfs-":                     nil,
	}
	for volumeID, want := range tests {
		if got := RootfsVolumeInstanceIDs(volumeID); !reflect.DeepEqual(got, want) {
			t.Errorf("RootfsVolumeInstanceIDs(%q) = %v, want %v", volumeID, got, want)
		}
	}
}

func writeArtifactReferenceForTest(t *testing.T, record artifactReferenceRecord) {
	t.Helper()
	if err := os.MkdirAll(artifactReferencesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactReferencePath(record.ReferenceID), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAttachArtifactReferenceRecordFollowsLiveIDMap(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: "site--artifact--model",
		GoldenID:    "golden-model",
		Identity: GoldenContentIdentity{
			SourceKind:       GoldenSourceHuggingFace,
			ResolvedIdentity: strings.Repeat("a", 40),
			Projection:       GoldenProjectionHuggingFace + ":model.gguf",
		},
		IDMap: idMapMeta(recordedTestIDMap),
	}
	writeArtifactReferenceForTest(t, record)
	manager := &luksVolumeManager{goldenMu: make(map[string]*sync.Mutex)}

	// The golden content is absent, so the attach stops after the record
	// has followed the app user.
	_, err := manager.AttachArtifactReference(context.Background(), record.ReferenceID, recreatedTestIDMap)
	var missing *ArtifactContentMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("AttachArtifactReference error = %v, want missing golden content", err)
	}
	loaded, err := loadArtifactReference(record.ReferenceID)
	if err != nil {
		t.Fatalf("load reference: %v", err)
	}
	if loaded.IDMap != idMapMeta(recreatedTestIDMap) {
		t.Fatalf("reference map = %+v, want the live map", loaded.IDMap)
	}
}

// A projection still mounted with the old map may back running containers,
// so the record must not follow until the app's artifacts detach.
func TestAttachArtifactReferenceRefusesProjectionMountedWithStaleIDMap(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: "site--artifact--model",
		GoldenID:    "golden-model",
		Identity: GoldenContentIdentity{
			SourceKind:       GoldenSourceHuggingFace,
			ResolvedIdentity: strings.Repeat("a", 40),
			Projection:       GoldenProjectionHuggingFace + ":model.gguf",
		},
		IDMap: idMapMeta(recordedTestIDMap),
	}
	writeArtifactReferenceForTest(t, record)
	b := newSnap()
	devKey := b.mapper(volMapperName(record.GoldenID))
	b.mount(artifactMountPath(record.ReferenceID), "btrfs", "/dev/mapper/"+volMapperName(record.GoldenID), devKey)
	manager := &luksVolumeManager{goldenMu: make(map[string]*sync.Mutex), kernelSnapshotFn: staticSnapshot(b.build())}

	_, err := manager.AttachArtifactReference(context.Background(), record.ReferenceID, recreatedTestIDMap)
	if !errors.Is(err, ErrIDMapStaleMount) {
		t.Fatalf("AttachArtifactReference error = %v, want ErrIDMapStaleMount", err)
	}
	loaded, err := loadArtifactReference(record.ReferenceID)
	if err != nil {
		t.Fatalf("load reference: %v", err)
	}
	if loaded.IDMap != record.IDMap {
		t.Fatalf("reference map changed under a mounted projection: %+v", loaded.IDMap)
	}
}

// End to end: the projection an artifact attach builds uses the live map,
// not the record it loaded.
func TestAttachArtifactReferenceBuildsProjectionFromLiveIDMap(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("f", 40),
		Projection:       GoldenProjectionHuggingFace + ":model.gguf",
	}
	const goldenID = "golden-model"
	if err := os.MkdirAll(paths.VolumeMetaDir(goldenID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File), &volumeMetaV3{
		Version:             metadataV3Version,
		Type:                volumeTypeGolden,
		LVName:              goldenID,
		VGName:              lvm.DefaultVGName,
		FSType:              "btrfs",
		GoldenIdentity:      &identity,
		MaterializeComplete: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: "site--artifact--model",
		GoldenID:    goldenID,
		Identity:    identity,
		IDMap:       idMapMeta(recordedTestIDMap),
	}
	writeArtifactReferenceForTest(t, record)
	run := &fakeRunner{}
	mgr := newTestManager()
	mgr.run = run
	mgr.crypto = newResizeTestCryptManager(t, core)
	mgr.tmpfsDir = t.TempDir()
	mgr.lvMgr = lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName)
	mgr.kernelSnapshotFn = staticSnapshot(newSnap().build())
	var built []fsutil.IDMapConfig
	mgr.idmappedMountFn = func(_, target string, config fsutil.IDMapConfig) error {
		if target != artifactMountPath(record.ReferenceID) {
			t.Fatalf("projection target = %s", target)
		}
		built = append(built, config)
		return nil
	}

	if _, err := mgr.AttachArtifactReference(context.Background(), record.ReferenceID, recreatedTestIDMap); err != nil {
		t.Fatalf("AttachArtifactReference: %v", err)
	}
	if len(built) != 1 || built[0] != recreatedTestIDMap {
		t.Fatalf("projections built with %+v, want one with the live map %+v", built, recreatedTestIDMap)
	}
}

func TestCreateArtifactReferenceFollowsLiveIDMapOfExistingRecord(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("e", 40),
		Projection:       GoldenProjectionHuggingFace + ":model.gguf",
	}
	candidates, err := candidateGoldenIDs(identity, "")
	if err != nil {
		t.Fatalf("candidateGoldenIDs: %v", err)
	}
	goldenID := candidates[0]
	metaDir := paths.VolumeMetaDir(goldenID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMetaV3(filepath.Join(metaDir, metadataV2File), &volumeMetaV3{
		Version:             metadataV3Version,
		Type:                volumeTypeGolden,
		LVName:              goldenID,
		VGName:              lvm.DefaultVGName,
		GoldenIdentity:      &identity,
		MaterializeComplete: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: "site--artifact--model",
		GoldenID:    goldenID,
		Identity:    identity,
		IDMap:       idMapMeta(recordedTestIDMap),
	}
	writeArtifactReferenceForTest(t, record)
	// The failing runner reports the golden LV absent, so the attach stops
	// after the existing record has followed the app user.
	manager, _ := newFailingAttachManager()

	_, err = manager.CreateArtifactReference(context.Background(), ArtifactReferenceRequest{
		ReferenceID: record.ReferenceID,
		GoldenID:    goldenID,
		Identity:    identity,
		IDMap:       recreatedTestIDMap,
	})
	var missing *ArtifactContentMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("CreateArtifactReference error = %v, want missing golden content, not a durable-content conflict", err)
	}
	loaded, err := loadArtifactReference(record.ReferenceID)
	if err != nil {
		t.Fatalf("an existing reference must survive a failed attach: %v", err)
	}
	if loaded.IDMap != idMapMeta(recreatedTestIDMap) {
		t.Fatalf("reference map = %+v, want the live map", loaded.IDMap)
	}
}
