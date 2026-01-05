package workspacedisk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewLayout(t *testing.T) {
	baseDir := "/var/lib/piccolo/mounts/app-test/disk/workspace"
	layout := NewLayout(baseDir)

	if layout.Base != baseDir {
		t.Errorf("Base = %v, want %v", layout.Base, baseDir)
	}
	if layout.Upper != filepath.Join(baseDir, UpperDir) {
		t.Errorf("Upper = %v, want %v", layout.Upper, filepath.Join(baseDir, UpperDir))
	}
	if layout.Work != filepath.Join(baseDir, WorkDir) {
		t.Errorf("Work = %v, want %v", layout.Work, filepath.Join(baseDir, WorkDir))
	}
	if layout.Merged != filepath.Join(baseDir, MergedDir) {
		t.Errorf("Merged = %v, want %v", layout.Merged, filepath.Join(baseDir, MergedDir))
	}
}

func TestLayout_EnsureDirs(t *testing.T) {
	tmpDir := t.TempDir()
	layout := NewLayout(tmpDir)

	if err := layout.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}

	// Verify directories exist
	for _, dir := range []string{layout.Upper, layout.Work, layout.Merged} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("directory %s not created: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}
}

func TestLayout_CleanWorkDir(t *testing.T) {
	tmpDir := t.TempDir()
	layout := NewLayout(tmpDir)

	// First ensure dirs exist
	if err := layout.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}

	// Create some files in work dir
	testFile := filepath.Join(layout.Work, "test-file")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(testFile); err != nil {
		t.Fatalf("test file not created: %v", err)
	}

	// Clean work dir
	if err := layout.CleanWorkDir(); err != nil {
		t.Fatalf("CleanWorkDir() error = %v", err)
	}

	// Verify work dir exists but is empty
	info, err := os.Stat(layout.Work)
	if err != nil {
		t.Errorf("work dir not recreated: %v", err)
	} else if !info.IsDir() {
		t.Error("work dir is not a directory")
	}

	// Verify file is gone
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("test file still exists after CleanWorkDir()")
	}
}

func TestLayout_IsMounted_NotMounted(t *testing.T) {
	tmpDir := t.TempDir()
	layout := NewLayout(tmpDir)

	if err := layout.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}

	mounted, err := layout.IsMounted()
	if err != nil {
		t.Fatalf("IsMounted() error = %v", err)
	}
	if mounted {
		t.Error("IsMounted() = true, want false for unmounted directory")
	}
}

func TestLayout_IsMounted_NonexistentDir(t *testing.T) {
	tmpDir := t.TempDir()
	layout := NewLayout(tmpDir)
	// Don't create dirs

	mounted, err := layout.IsMounted()
	if err != nil {
		t.Fatalf("IsMounted() error = %v", err)
	}
	if mounted {
		t.Error("IsMounted() = true, want false for nonexistent directory")
	}
}
