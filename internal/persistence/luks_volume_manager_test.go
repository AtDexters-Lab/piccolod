package persistence

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
)

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
