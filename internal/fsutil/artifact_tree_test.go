package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestValidateArtifactTreeFileTypes(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "directory", "model.bin"), []byte("model"), 0o644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	if err := os.Symlink("directory/model.bin", filepath.Join(root, "model-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ValidateArtifactTree(root); err != nil {
		t.Fatalf("regular artifact tree rejected: %v", err)
	}

	fifo := filepath.Join(root, "blocking-input")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if err := ValidateArtifactTree(root); err == nil {
		t.Fatal("artifact tree containing FIFO was accepted")
	}
}
