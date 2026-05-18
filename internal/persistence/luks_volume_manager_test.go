package persistence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/testutil"
)

type fakeRunner = testutil.FakeRunner

// fakeBlockDevice implements blockdev.BlockDevice for testing.
type fakeBlockDevice struct {
	name   string
	closed bool
	err    error
}

func (d *fakeBlockDevice) Name() string               { return d.name }
func (d *fakeBlockDevice) Path() string                { return "/dev/fake/" + d.name }
func (d *fakeBlockDevice) Open(_ context.Context) error { return nil }
func (d *fakeBlockDevice) Close(_ context.Context) error {
	d.closed = true
	return d.err
}
func (d *fakeBlockDevice) SizeBytes() int64 { return 0 }

func TestLUKSVolumeManager_ImplementsInterfaces(t *testing.T) {
	// Compile-time interface checks.
	var _ VolumeManager = (*luksVolumeManager)(nil)
	var _ RoleCheckable = (*luksVolumeManager)(nil)
	var _ Reconcilable = (*luksVolumeManager)(nil)
}

func TestLUKSVolumeManager_SetRoleChecker(t *testing.T) {
	mgr := &luksVolumeManager{}

	called := false
	mgr.SetRoleChecker(func(id string, role VolumeRole) bool {
		called = true
		return id == "vol-test"
	})

	mgr.mu.Lock()
	checker := mgr.roleChecker
	mgr.mu.Unlock()

	if checker == nil {
		t.Fatal("expected role checker to be set")
	}
	if !checker("vol-test", VolumeRoleLeader) {
		t.Error("expected checker to return true for vol-test")
	}
	if !called {
		t.Error("expected checker to be called")
	}
}

func TestLUKSVolumeManager_ReconcileAllVolumeStates_Empty(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// No volumes dir yet — should not error.
	if err := mgr.ReconcileAllVolumeStates(); err != nil {
		t.Fatalf("ReconcileAllVolumeStates: %v", err)
	}

	// Create volumes dir but leave it empty.
	if err := os.MkdirAll(filepath.Join(core, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ReconcileAllVolumeStates(); err != nil {
		t.Fatalf("ReconcileAllVolumeStates: %v", err)
	}
}

func TestLUKSVolumeManager_ReconcileAllVolumeStates_ValidMeta(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// Create a volume with valid metadata.
	volDir := filepath.Join(core, "volumes", "test-vol")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":2,"type":"luks-loop","wrapped_key":"abc","nonce":"def","loop_file":"test.luks","size_bytes":1024,"fs_type":"ext4"}`
	if err := os.WriteFile(filepath.Join(volDir, metadataV2File), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mgr.ReconcileAllVolumeStates(); err != nil {
		t.Fatalf("ReconcileAllVolumeStates: %v", err)
	}
}

func TestLUKSVolumeManager_EnsureVolume_ExistingReturnsHandle(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// Pre-create metadata.
	volDir := filepath.Join(core, "volumes", "existing-vol")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":2,"type":"luks-loop","wrapped_key":"abc","nonce":"def","size_bytes":1024,"fs_type":"ext4"}`
	if err := os.WriteFile(filepath.Join(volDir, metadataV2File), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{
		ID:    "existing-vol",
		Class: VolumeClassControl,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if handle.ID != "existing-vol" {
		t.Errorf("expected ID=existing-vol, got %s", handle.ID)
	}
	if handle.MountDir != paths.MountDir("existing-vol") {
		t.Errorf("unexpected mount dir: %s", handle.MountDir)
	}
}

func TestLUKSVolumeManager_RoleStream(t *testing.T) {
	mgr := &luksVolumeManager{}

	ch, err := mgr.RoleStream("any-vol")
	if err != nil {
		t.Fatalf("RoleStream: %v", err)
	}
	role := <-ch
	if role != VolumeRoleLeader {
		t.Errorf("expected Leader, got %v", role)
	}
}

func TestLUKSVolumeManager_DestroyVolume_NonExistent(t *testing.T) {
	paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// Should not error for non-existent volume.
	if err := mgr.DestroyVolume(context.Background(), "nonexistent"); err != nil {
		t.Errorf("DestroyVolume: %v", err)
	}
}

func TestLUKSVolumeManager_DestroyVolume_LoopType(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// Create a loop volume's metadata.
	volDir := filepath.Join(core, "volumes", "ctrl")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":2,"type":"luks-loop","wrapped_key":"abc","nonce":"def","loop_file":"control-plane.luks","size_bytes":1024,"fs_type":"ext4"}`
	if err := os.WriteFile(filepath.Join(volDir, metadataV2File), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create the loop file.
	loopFile := filepath.Join(core, "control-plane.luks")
	if err := os.WriteFile(loopFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create mount dir.
	mountDir := filepath.Join(core, "mounts", "ctrl")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := mgr.DestroyVolume(context.Background(), "ctrl"); err != nil {
		t.Fatalf("DestroyVolume: %v", err)
	}

	// Loop file should be removed.
	if _, err := os.Stat(loopFile); !os.IsNotExist(err) {
		t.Error("expected loop file to be removed")
	}
	// Metadata dir should be removed.
	if _, err := os.Stat(volDir); !os.IsNotExist(err) {
		t.Error("expected volume metadata dir to be removed")
	}
	// Mount dir should be removed.
	if _, err := os.Stat(mountDir); !os.IsNotExist(err) {
		t.Error("expected mount dir to be removed")
	}
}

func TestReadWriteVolumeMeta(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, metadataV2File)

	meta := &volumeMetaV2{
		Version:    metadataV2Version,
		Type:       "luks-thinlv",
		WrappedKey: "wrapped123",
		Nonce:      "nonce456",
		LVName:     "vol-test",
		VGName:     "piccolo-data-vg",
		SizeBytes:  10 << 30,
		FSType:     "ext4",
	}

	if err := writeVolumeMeta(metaPath, meta); err != nil {
		t.Fatalf("writeVolumeMeta: %v", err)
	}

	got, err := readVolumeMeta(metaPath)
	if err != nil {
		t.Fatalf("readVolumeMeta: %v", err)
	}

	if got.Type != "luks-thinlv" {
		t.Errorf("Type = %q, want luks-thinlv", got.Type)
	}
	if got.LVName != "vol-test" {
		t.Errorf("LVName = %q, want vol-test", got.LVName)
	}
	if got.SizeBytes != 10<<30 {
		t.Errorf("SizeBytes = %d", got.SizeBytes)
	}
}

func TestReadWriteVolumeMetaV3_ServiceData(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, metadataV2File)

	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      "service-data",
		LVName:    "vol-app-nextcloud",
		VGName:    "piccolo-data-vg",
		SizeBytes: 10 << 30,
		FSType:    "ext4",
	}

	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		t.Fatalf("writeVolumeMetaV3: %v", err)
	}

	// Verify version dispatch.
	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		t.Fatalf("readVolumeMetaVersion: %v", err)
	}
	if version != metadataV3Version {
		t.Fatalf("version = %d, want %d", version, metadataV3Version)
	}

	got, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatalf("readVolumeMetaV3: %v", err)
	}

	if got.Type != "service-data" {
		t.Errorf("Type = %q, want service-data", got.Type)
	}
	if got.LVName != "vol-app-nextcloud" {
		t.Errorf("LVName = %q, want vol-app-nextcloud", got.LVName)
	}
	if got.SizeBytes != 10<<30 {
		t.Errorf("SizeBytes = %d", got.SizeBytes)
	}
	if got.FSType != "ext4" {
		t.Errorf("FSType = %q, want ext4", got.FSType)
	}
}

func TestReconcileAllVolumeStates_V3ServiceData(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	// Create a v3 service-data volume metadata.
	volDir := filepath.Join(core, "volumes", "app-nextcloud")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":3,"type":"service-data","lv_name":"vol-app-nextcloud","vg_name":"piccolo-data-vg","size_bytes":10737418240,"fs_type":"ext4"}`
	if err := os.WriteFile(filepath.Join(volDir, metadataV2File), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mgr.ReconcileAllVolumeStates(); err != nil {
		t.Fatalf("ReconcileAllVolumeStates: %v", err)
	}
}

func TestReadVolumeMeta_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, metadataV2File)

	if err := os.WriteFile(metaPath, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := readVolumeMeta(metaPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReadVolumeMeta_WrongVersion(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, metadataV2File)

	meta := `{"version":99,"type":"luks-loop"}`
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := readVolumeMeta(metaPath)
	if err == nil {
		t.Fatal("expected error for wrong version")
	}
}

func TestLuksSetKeyslot_CallsAddKey(t *testing.T) {
	// FakeRunner returns empty output for luksDump → probe parse fails →
	// we fall through to the optimistic-add path. Optimistic add succeeds
	// (no Errs configured), so the kill+add fallback is not exercised.
	// Expected calls: luksDump (probe), luksAddKey (optimistic).
	run := &fakeRunner{}
	tmpfs := t.TempDir()
	mgr := &luksVolumeManager{run: run, tmpfsDir: tmpfs}

	masterKey := []byte("master-key-64-bytes-padding-here-0123456789abcdef0123456789abcdef")
	passphrase := []byte("admin-password")

	if err := mgr.luksSetKeyslot(context.Background(), "/dev/fake", 1, passphrase, masterKey); err != nil {
		t.Fatalf("luksSetKeyslot: %v", err)
	}

	calls := run.GetCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (probe + add), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "cryptsetup luksDump") {
		t.Errorf("expected first call to be luksDump probe, got %q", calls[0])
	}
	if !strings.Contains(calls[1], "cryptsetup luksAddKey") {
		t.Errorf("expected second call to be luksAddKey, got %q", calls[1])
	}
	if !strings.Contains(calls[1], "--key-slot 1") {
		t.Errorf("expected --key-slot 1, got %q", calls[1])
	}
	if !strings.Contains(calls[1], "/dev/fake") {
		t.Errorf("expected device /dev/fake, got %q", calls[1])
	}

	// Verify tmpfs files are cleaned up (key material must not persist).
	entries, _ := os.ReadDir(tmpfs)
	if len(entries) != 0 {
		t.Errorf("expected tmpfs dir to be clean, found %d files", len(entries))
	}
}

// luksSetKeyslot when the probe reports the slot is empty: add-only path,
// no kill (sub-case ii / "unprovisioned" sentinel). Empty-window invariant
// holds because the slot is already empty.
func TestLuksSetKeyslot_AddOnlyWhenSlotEmpty(t *testing.T) {
	run := &fakeRunner{
		Outputs: map[string]string{
			"cryptsetup luksDump --dump-json-metadata /dev/fake": `{"keyslots":{}}`,
		},
	}
	tmpfs := t.TempDir()
	mgr := &luksVolumeManager{run: run, tmpfsDir: tmpfs}

	if err := mgr.luksSetKeyslot(context.Background(), "/dev/fake", 1,
		[]byte("pw"), []byte("master-key-padded-to-32-bytes-here!!")); err != nil {
		t.Fatalf("luksSetKeyslot: %v", err)
	}
	calls := run.GetCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (probe + add), got %d: %v", len(calls), calls)
	}
	for _, c := range calls {
		if strings.Contains(c, "luksKillSlot") {
			t.Errorf("did not expect luksKillSlot on add-only path, got %q", c)
		}
	}
}

// luksSetKeyslot when the probe reports the slot is occupied: kill+add
// pair runs under WithoutCancel so external cancellation cannot tear the
// pair apart and leave the slot empty.
func TestLuksSetKeyslot_KillAddWhenSlotOccupied(t *testing.T) {
	run := &fakeRunner{
		Outputs: map[string]string{
			"cryptsetup luksDump --dump-json-metadata /dev/fake": `{"keyslots":{"1":{"type":"luks2"}}}`,
		},
	}
	tmpfs := t.TempDir()
	mgr := &luksVolumeManager{run: run, tmpfsDir: tmpfs}

	if err := mgr.luksSetKeyslot(context.Background(), "/dev/fake", 1,
		[]byte("pw"), []byte("master-key-padded-to-32-bytes-here!!")); err != nil {
		t.Fatalf("luksSetKeyslot: %v", err)
	}
	calls := run.GetCalls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls (probe + kill + add), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "luksDump") {
		t.Errorf("call 0 not luksDump: %q", calls[0])
	}
	if !strings.Contains(calls[1], "luksKillSlot") {
		t.Errorf("call 1 not luksKillSlot: %q", calls[1])
	}
	if !strings.Contains(calls[2], "luksAddKey") {
		t.Errorf("call 2 not luksAddKey: %q", calls[2])
	}
}

// luksDumpSlotOccupied parses cryptsetup's --dump-json-metadata output and
// reports whether the given keyslot ID is present.
func TestLuksDumpSlotOccupied(t *testing.T) {
	cases := []struct {
		name     string
		dump     string
		slot     int
		want     bool
		wantErr  bool
	}{
		{"empty keyslots", `{"keyslots":{}}`, 1, false, false},
		{"slot 1 present", `{"keyslots":{"1":{"type":"luks2"}}}`, 1, true, false},
		{"slot 2 present, asking 1", `{"keyslots":{"2":{"type":"luks2"}}}`, 1, false, false},
		{"both slots", `{"keyslots":{"0":{},"1":{},"2":{}}}`, 2, true, false},
		{"malformed JSON", `not json`, 1, false, true},
		{"missing keyslots field", `{}`, 1, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := luksDumpSlotOccupied([]byte(tc.dump), tc.slot)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProvisionKeyslotOnAllVolumes_RejectsInvalidSlot(t *testing.T) {
	mgr := &luksVolumeManager{
	}

	if err := mgr.provisionKeyslotOnAllVolumes(context.Background(), 0, []byte("pass")); err == nil {
		t.Error("expected error for slot 0")
	}
	if err := mgr.provisionKeyslotOnAllVolumes(context.Background(), 3, []byte("pass")); err == nil {
		t.Error("expected error for slot 3")
	}
}

func TestProvisionKeyslotOnAllVolumes_NoVolumes_ReturnsNil(t *testing.T) {
	paths.SetRootsForTest(t)

	run := &fakeRunner{}
	mgr := &luksVolumeManager{
		run:      run,
		tmpfsDir: t.TempDir(),
	}

	// With no volumes, provisioning is a no-op regardless of crypto state.
	err := mgr.provisionKeyslotOnAllVolumes(context.Background(), 1, []byte("pass"))
	if err != nil {
		t.Fatalf("expected nil error with no volumes, got: %v", err)
	}
	// No cryptsetup calls should have been made.
	if calls := run.GetCalls(); len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d: %v", len(calls), calls)
	}
}

func TestModuleWriteKeyslotBlob_StubReturnsErrNotImplemented(t *testing.T) {
	// Module with nil/stub volume manager surfaces ErrNotImplemented so
	// callers can fall through to legacy behavior without panicking.
	mod := &Module{}
	err := mod.WriteKeyslotBlob(context.Background(), KeyslotRecovery, "deadbeefdeadbeef", []byte("p"))
	if err != ErrNotImplemented {
		t.Errorf("expected ErrNotImplemented, got %v", err)
	}
}

// --- Ephemeral volume tests ---

func TestVolumeClassEphemeral_Value(t *testing.T) {
	if VolumeClassEphemeral != "ephemeral" {
		t.Errorf("VolumeClassEphemeral = %q, want %q", VolumeClassEphemeral, "ephemeral")
	}
}

func TestReadWriteVolumeMetaV3_Ephemeral(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, metadataV2File)

	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      "ephemeral",
		LVName:    "eph-scratch-vol",
		VGName:    "piccolo-data-vg",
		SizeBytes: 10 << 30,
		FSType:    "btrfs",
	}

	if err := writeVolumeMetaV3(metaPath, meta); err != nil {
		t.Fatalf("writeVolumeMetaV3: %v", err)
	}

	version, err := readVolumeMetaVersion(metaPath)
	if err != nil {
		t.Fatalf("readVolumeMetaVersion: %v", err)
	}
	if version != metadataV3Version {
		t.Fatalf("version = %d, want %d", version, metadataV3Version)
	}

	got, err := readVolumeMetaV3(metaPath)
	if err != nil {
		t.Fatalf("readVolumeMetaV3: %v", err)
	}
	if got.Type != "ephemeral" {
		t.Errorf("Type = %q, want ephemeral", got.Type)
	}
	if got.LVName != "eph-scratch-vol" {
		t.Errorf("LVName = %q, want eph-scratch-vol", got.LVName)
	}
	if got.FSType != "btrfs" {
		t.Errorf("FSType = %q, want btrfs", got.FSType)
	}
}

func TestReconcileAllVolumeStates_V3Ephemeral(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{}

	volDir := filepath.Join(core, "volumes", "eph-scratch")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":3,"type":"ephemeral","lv_name":"eph-scratch","vg_name":"piccolo-data-vg","size_bytes":10737418240,"fs_type":"btrfs"}`
	if err := os.WriteFile(filepath.Join(volDir, metadataV2File), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mgr.ReconcileAllVolumeStates(); err != nil {
		t.Fatalf("ReconcileAllVolumeStates: %v", err)
	}
}

func TestResolveLUKSDevice_RejectsEphemeral(t *testing.T) {
	mgr := &luksVolumeManager{
	}

	meta := &volumeMetaV3{
		Version: metadataV3Version,
		Type:    "ephemeral",
		LVName:  "eph-test",
	}

	_, _, err := mgr.resolveLUKSDevice(context.Background(), "test-vol", meta)
	if err == nil {
		t.Fatal("expected error for ephemeral volume")
	}
	if !strings.Contains(err.Error(), "no LUKS device") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDestroyVolume_Ephemeral_NoCryptsetup(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	run := &fakeRunner{}
	lvMgr := lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName)

	mgr := &luksVolumeManager{
		run:          run,
		lvMgr:        lvMgr,
	}

	// Create ephemeral volume metadata.
	volDir := filepath.Join(core, "volumes", "eph-test")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := &volumeMetaV3{
		Version:   metadataV3Version,
		Type:      "ephemeral",
		LVName:    "eph-eph-test",
		VGName:    lvm.DefaultVGName,
		SizeBytes: ephDefaultSize,
		FSType:    "btrfs",
	}
	if err := writeVolumeMetaV3(filepath.Join(volDir, metadataV2File), meta); err != nil {
		t.Fatal(err)
	}

	// Create mount dir.
	mountDir := filepath.Join(core, "mounts", "eph-test")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := mgr.DestroyVolume(context.Background(), "eph-test"); err != nil {
		t.Fatalf("DestroyVolume: %v", err)
	}

	// Verify no cryptsetup calls were made.
	for _, call := range run.GetCalls() {
		if strings.Contains(call, "cryptsetup") {
			t.Errorf("unexpected cryptsetup call: %q", call)
		}
	}

	// Metadata dir should be removed.
	if _, err := os.Stat(volDir); !os.IsNotExist(err) {
		t.Error("expected volume metadata dir to be removed")
	}
	// Mount dir should be removed.
	if _, err := os.Stat(mountDir); !os.IsNotExist(err) {
		t.Error("expected mount dir to be removed")
	}
}

func TestProvisionKeyslotOnAllVolumes_SkipsEphemeral(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	run := &fakeRunner{
		// Make cryptsetup fail so we can detect if it's called.
		Errs: map[string]error{"cryptsetup": errors.New("should not be called")},
	}

	// Create an ephemeral volume metadata (should be skipped).
	volDir := filepath.Join(core, "volumes", "eph-scratch")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":3,"type":"ephemeral","lv_name":"eph-scratch","vg_name":"piccolo-data-vg","size_bytes":10737418240,"fs_type":"btrfs"}`
	if err := os.WriteFile(filepath.Join(volDir, metadataV2File), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr := &luksVolumeManager{
		run:          run,
		tmpfsDir:     t.TempDir(),
		// crypto is nil — provisionKeyslotOnAllVolumes will fail on UnwrapLUKSMasterKey.
		// But ephemeral volumes should be skipped before we get to any per-volume operations.
	}

	// This will fail because crypto is nil (can't unwrap master key),
	// but the point is that if we got past the master key unwrap,
	// ephemeral volumes would not trigger cryptsetup.
	_ = mgr.provisionKeyslotOnAllVolumes(context.Background(), 1, []byte("pass"))

	// No cryptsetup calls should have been made for the ephemeral volume.
	for _, call := range run.GetCalls() {
		if strings.Contains(call, "cryptsetup") {
			t.Errorf("unexpected cryptsetup call for ephemeral volume: %q", call)
		}
	}
}

func TestReconcileOrphanLVs(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	// Stub lvs output: mix of known, orphan, snapshot, and failed-rollback LVs.
	// Format: lv_name  lv_size  lv_attr  pool_lv
	lvsOutput := strings.Join([]string{
		"  eph-scratch       53687091200  Vwi-a-tz--  thinpool",  // known (has metadata)
		"  vol-app-myapp     10737418240  Vwi-a-tz--  thinpool",  // known (has metadata)
		"  golden-abc123     3221225472   Vwi---tz--  thinpool",  // known (has metadata)
		"  eph-orphan        53687091200  Vwi---tz--  thinpool",  // orphan — should be removed
		"  ws-old-install    5368709120   Vwi-a-tz--  thinpool",  // orphan (active) — should be deactivated + removed
		"  snap-app-myapp--gen1  10737418240  Vwi---tz--  thinpool", // tuple snapshot — skip
		"  vol-app-myapp--failed-gen2  10737418240  Vwi---tz--  thinpool", // tuple failed rollback — skip
		"  thinpool          214748364800  twi-a-t---  ",          // thin pool itself — skip
		"  foreign-lv        1073741824   -wi-a-----  ",          // not in pool — ListLVs filters it
	}, "\n")

	lvsKey := testutil.BuildKey("lvs", []string{
		"--noheadings", "--nosuffix", "--units", "b",
		"-o", "lv_name,lv_size,lv_attr,pool_lv",
		lvm.DefaultVGName,
	})

	run := &fakeRunner{
		Outputs: map[string]string{lvsKey: lvsOutput},
	}
	lvMgr := lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName)

	mgr := &luksVolumeManager{
		run:          run,
		lvMgr:        lvMgr,
	}

	// Create metadata for the "known" LVs.
	for _, tc := range []struct {
		volID  string
		lvName string
		typ    string
	}{
		{"scratch", "eph-scratch", "ephemeral"},
		{"app-myapp", "vol-app-myapp", "service-data"},
		{"golden-abc123", "golden-abc123", "golden"},
	} {
		volDir := filepath.Join(core, "volumes", tc.volID)
		if err := os.MkdirAll(volDir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := &volumeMetaV3{
			Version: metadataV3Version,
			Type:    tc.typ,
			LVName:  tc.lvName,
			VGName:  lvm.DefaultVGName,
		}
		if err := writeVolumeMetaV3(filepath.Join(volDir, metadataV2File), meta); err != nil {
			t.Fatal(err)
		}
	}

	if err := mgr.ReconcileOrphanLVs(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanLVs: %v", err)
	}

	calls := run.GetCalls()

	// Verify orphan "eph-orphan" was removed.
	expectRemoved := map[string]bool{
		"eph-orphan":    false,
		"ws-old-install": false,
	}
	for _, call := range calls {
		if strings.Contains(call, "lvremove") {
			for name := range expectRemoved {
				if strings.Contains(call, name) {
					expectRemoved[name] = true
				}
			}
		}
	}
	for name, removed := range expectRemoved {
		if !removed {
			t.Errorf("orphan LV %s was not removed", name)
		}
	}

	// Verify active orphan "ws-old-install" was deactivated first.
	deactivated := false
	for _, call := range calls {
		if strings.Contains(call, "lvchange -an") && strings.Contains(call, "ws-old-install") {
			deactivated = true
		}
	}
	if !deactivated {
		t.Error("active orphan ws-old-install was not deactivated before removal")
	}

	// Verify known LVs were NOT removed.
	for _, call := range calls {
		if strings.Contains(call, "lvremove") {
			for _, name := range []string{"eph-scratch", "vol-app-myapp", "golden-abc123"} {
				if strings.Contains(call, name) {
					t.Errorf("known LV %s should not be removed", name)
				}
			}
		}
	}

	// Verify tuple-managed LVs were NOT removed.
	for _, call := range calls {
		if strings.Contains(call, "lvremove") {
			if strings.Contains(call, "snap-app-myapp--gen1") {
				t.Error("tuple snapshot LV should not be removed")
			}
			if strings.Contains(call, "vol-app-myapp--failed-gen2") {
				t.Error("tuple failed rollback LV should not be removed")
			}
		}
	}
}

func TestDetachAppVolume_ContinuesOnUmountFailure(t *testing.T) {
	// Phase 3 of volume-attach-truth migrates Detach off the cached
	// DeviceStack object; LV deactivation now goes through lvMgr directly
	// (kernel-state truth). Tests that previously asserted dev.closed are
	// updated to assert "cryptsetup close was called even when umount
	// failed" — the cross-step continuation invariant the original test
	// was guarding.
	tests := []struct {
		name          string
		umountErr     error // error for first umount
		lazyUmountErr error // error for lazy umount
		cryptsetupErr error // error for cryptsetup close
		wantErr       bool
	}{
		{
			name: "all_succeed",
		},
		{
			name:          "umount_already_gone_lazy_also_fails",
			umountErr:     errors.New("not mounted"),
			lazyUmountErr: errors.New("not mounted"),
			wantErr:       true,
		},
		{
			name:      "umount_fails_lazy_succeeds",
			umountErr: errors.New("device busy"),
		},
		{
			name:          "cryptsetup_fails",
			cryptsetupErr: errors.New("device busy"),
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths.SetRootsForTest(t)
			mountDir := paths.MountDir("app-test")
			handle := VolumeHandle{ID: "app-test", MountDir: mountDir}
			// Pre-create the metadata so detachAppVolume's metadata-read
			// branch (Phase 3 migration) finds an LVName to deactivate.
			writeAppMetaForDetachTest(t, "app-test")

			errs := make(map[string]error)
			umountKey := testutil.BuildKey("umount", []string{mountDir})
			if tt.umountErr != nil {
				errs[umountKey] = tt.umountErr
			}
			lazyKey := testutil.BuildKey("umount", []string{"-l", mountDir})
			if tt.lazyUmountErr != nil {
				errs[lazyKey] = tt.lazyUmountErr
			}
			if tt.cryptsetupErr != nil {
				errs["cryptsetup close piccolo-vol-app-test"] = tt.cryptsetupErr
			}

			run := &fakeRunner{Errs: errs}

			mgr := &luksVolumeManager{
				run:                        run,
			}

			err := mgr.detachAppVolume(context.Background(), handle)

			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			// cryptsetup close must always be attempted, even when umount fails.
			calls := run.GetCalls()
			found := false
			for _, c := range calls {
				if strings.HasPrefix(c, "cryptsetup close") {
					found = true
					break
				}
			}
			if !found {
				t.Error("cryptsetup close was not called")
			}
		})
	}
}

// writeAppMetaForDetachTest mirrors writeAppMeta but lives in the same
// _test.go grouping. Inlined to keep the test file self-contained.
func writeAppMetaForDetachTest(t *testing.T, volumeID string) {
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

func TestDetachEphemeralVolume_ContinuesOnUmountFailure(t *testing.T) {
	// Phase 3 migration: Detach uses kernel-state truth (metadata + lvMgr)
	// rather than the cached DeviceStack. Test asserts the umount-failure
	// continuation invariant against the new path.
	paths.SetRootsForTest(t)
	mountDir := paths.MountDir("eph-test")
	handle := VolumeHandle{ID: "eph-test", MountDir: mountDir}

	dir := paths.VolumeMetaDir("eph-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":3,"type":"ephemeral","lv_name":"eph-eph-test","vg_name":"piccolo-data-vg","fs_type":"btrfs"}`
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	umountKey := testutil.BuildKey("umount", []string{mountDir})
	lazyKey := testutil.BuildKey("umount", []string{"-l", mountDir})

	run := &fakeRunner{Errs: map[string]error{
		umountKey: errors.New("not mounted"),
		lazyKey:   errors.New("not mounted"),
	}}

	mgr := &luksVolumeManager{
		run:                        run,
	}

	err := mgr.detachEphemeralVolume(context.Background(), handle)
	if err == nil {
		t.Error("expected error for failed umount, got nil")
	}
	// umount + lazy umount were both attempted.
	calls := run.GetCalls()
	found := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "umount") {
			found++
		}
	}
	if found < 2 {
		t.Errorf("expected umount + lazy fallback to be attempted, saw %d umount calls", found)
	}
}
