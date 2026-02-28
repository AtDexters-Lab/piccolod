//go:build integration

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCreateIDMappedMount_Integration(t *testing.T) {
	// This test requires CAP_SYS_ADMIN and kernel >= 5.12.
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	// Create a source tmpfs mount with a test file owned by UID 0.
	srcDir := t.TempDir()
	if err := unix.Mount("tmpfs", srcDir, "tmpfs", 0, "size=1M"); err != nil {
		t.Fatalf("mount tmpfs: %v", err)
	}
	t.Cleanup(func() { unix.Unmount(srcDir, 0) })

	testFile := filepath.Join(srcDir, "testfile")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("create test file: %v", err)
	}
	// Chown to UID 0 (root).
	if err := os.Chown(testFile, 0, 0); err != nil {
		t.Fatalf("chown: %v", err)
	}

	// Create idmapped mount that maps UID 0 → UID 1000.
	targetDir := filepath.Join(t.TempDir(), "idmapped")
	config := IDMapConfig{
		AppUID:      1000,
		AppGID:      1000,
		SubUIDStart: 100000,
		SubUIDCount: 65536,
		SubGIDStart: 100000,
		SubGIDCount: 65536,
	}

	if err := CreateIDMappedMount(srcDir, targetDir, config); err != nil {
		t.Fatalf("CreateIDMappedMount: %v", err)
	}
	t.Cleanup(func() { unix.Unmount(targetDir, 0) })

	// Verify the file appears as owned by UID 1000 in the idmapped view.
	mappedFile := filepath.Join(targetDir, "testfile")
	var stat syscall.Stat_t
	if err := syscall.Stat(mappedFile, &stat); err != nil {
		t.Fatalf("stat mapped file: %v", err)
	}
	if stat.Uid != 1000 {
		t.Errorf("expected UID 1000, got %d", stat.Uid)
	}
	if stat.Gid != 1000 {
		t.Errorf("expected GID 1000, got %d", stat.Gid)
	}
}
