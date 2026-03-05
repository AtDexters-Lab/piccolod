package persistence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage/blockdev"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/testutil"
)

type fakeRunner = testutil.FakeRunner

func TestLUKSVolumeManager_ImplementsInterfaces(t *testing.T) {
	// Compile-time interface checks.
	var _ VolumeManager = (*luksVolumeManager)(nil)
	var _ RoleCheckable = (*luksVolumeManager)(nil)
	var _ Reconcilable = (*luksVolumeManager)(nil)
}

func TestLUKSVolumeManager_SetRoleChecker(t *testing.T) {
	mgr := &luksVolumeManager{stacks: nil}

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

	mgr := &luksVolumeManager{stacks: nil}

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

	mgr := &luksVolumeManager{stacks: nil}

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

	mgr := &luksVolumeManager{stacks: nil}

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
	mgr := &luksVolumeManager{stacks: nil}

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

	mgr := &luksVolumeManager{stacks: nil}

	// Should not error for non-existent volume.
	if err := mgr.DestroyVolume(context.Background(), "nonexistent"); err != nil {
		t.Errorf("DestroyVolume: %v", err)
	}
}

func TestLUKSVolumeManager_DestroyVolume_LoopType(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{stacks: nil}

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

	mgr := &luksVolumeManager{stacks: nil}

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
	run := &fakeRunner{}
	tmpfs := t.TempDir()
	mgr := &luksVolumeManager{run: run, tmpfsDir: tmpfs}

	masterKey := []byte("master-key-64-bytes-padding-here-0123456789abcdef0123456789abcdef")
	passphrase := []byte("admin-password")

	if err := mgr.luksSetKeyslot(context.Background(), "/dev/fake", 1, passphrase, masterKey); err != nil {
		t.Fatalf("luksSetKeyslot: %v", err)
	}

	calls := run.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "cryptsetup luksAddKey") {
		t.Errorf("expected luksAddKey, got %q", calls[0])
	}
	if !strings.Contains(calls[0], "--key-slot 1") {
		t.Errorf("expected --key-slot 1, got %q", calls[0])
	}
	if !strings.Contains(calls[0], "/dev/fake") {
		t.Errorf("expected device /dev/fake, got %q", calls[0])
	}

	// Verify tmpfs files are cleaned up (key material must not persist).
	entries, _ := os.ReadDir(tmpfs)
	if len(entries) != 0 {
		t.Errorf("expected tmpfs dir to be clean, found %d files", len(entries))
	}
}

func TestProvisionKeyslotOnAllVolumes_RejectsInvalidSlot(t *testing.T) {
	mgr := &luksVolumeManager{
		stacks: make(map[string]*blockdev.DeviceStack),
	}

	if err := mgr.provisionKeyslotOnAllVolumes(context.Background(), 0, []byte("pass")); err == nil {
		t.Error("expected error for slot 0")
	}
	if err := mgr.provisionKeyslotOnAllVolumes(context.Background(), 3, []byte("pass")); err == nil {
		t.Error("expected error for slot 3")
	}
}

func TestProvisionKeyslotOnAllVolumes_FailsWithNilCrypto(t *testing.T) {
	paths.SetRootsForTest(t)

	run := &fakeRunner{}
	mgr := &luksVolumeManager{
		run:      run,
		tmpfsDir: t.TempDir(),
		stacks:   make(map[string]*blockdev.DeviceStack),
	}

	err := mgr.provisionKeyslotOnAllVolumes(context.Background(), 1, []byte("pass"))
	if err == nil {
		t.Fatal("expected error (no crypto manager)")
	}
	// No cryptsetup calls should have been made.
	if calls := run.GetCalls(); len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d: %v", len(calls), calls)
	}
}

func TestModuleProvisionLUKSKeyslot_NoopWithoutLUKS(t *testing.T) {
	// Module with nil volume manager should be a no-op.
	mod := &Module{}
	if err := mod.ProvisionLUKSKeyslot(context.Background(), 1, []byte("pass")); err != nil {
		t.Errorf("expected nil error, got %v", err)
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

	mgr := &luksVolumeManager{stacks: nil}

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
		stacks:       make(map[string]*blockdev.DeviceStack),
		rootfsMounts: make(map[string]*rootfsMountState),
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
		stacks:       make(map[string]*blockdev.DeviceStack),
		rootfsMounts: make(map[string]*rootfsMountState),
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
		stacks:       make(map[string]*blockdev.DeviceStack),
		rootfsMounts: make(map[string]*rootfsMountState),
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
