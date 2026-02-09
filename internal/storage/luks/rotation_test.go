package luks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

func TestWriteReadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	progress := &RotationProgress{
		StartedAt: time.Now().UTC(),
		Total:     3,
		Completed: []string{"uuid-1", "uuid-2"},
	}

	if err := writeJSON(path, progress); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	read, err := readJSON[RotationProgress](path)
	if err != nil {
		t.Fatalf("readJSON: %v", err)
	}

	if read.Total != 3 {
		t.Errorf("Total = %d, want 3", read.Total)
	}
	if len(read.Completed) != 2 {
		t.Errorf("Completed len = %d, want 2", len(read.Completed))
	}
}

func TestReadJSON_Missing(t *testing.T) {
	_, err := readJSON[RotationProgress]("/nonexistent/file.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestRotationProgressPath(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	progressPath := paths.CoreJoin("crypto", "luks-rotation-progress.json")
	if filepath.Dir(progressPath) != filepath.Join(core, "crypto") {
		t.Errorf("unexpected progress path: %s", progressPath)
	}
}

func TestResumePasswordRotation_NoProgress(t *testing.T) {
	paths.SetRootsForTest(t)

	run := &fakeRunner{
		outputs: map[string]string{
			"cryptsetup luksUUID /dev/sda3": "test-uuid\n",
		},
	}
	mgr := NewPoolManager(run, nil)

	if err := mgr.ResumePasswordRotationIfNeeded(context.Background(), "/dev/sda3", "password"); err != nil {
		t.Fatalf("expected no-op: %v", err)
	}
}

func TestResumePasswordRotation_AlreadyCompleted(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	cryptoDir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(cryptoDir, 0o700); err != nil {
		t.Fatal(err)
	}

	progress := RotationProgress{
		StartedAt: time.Now(),
		Total:     1,
		Completed: []string{"test-uuid"},
	}
	data, _ := json.MarshalIndent(progress, "", "  ")
	progressPath := filepath.Join(cryptoDir, "luks-rotation-progress.json")
	if err := fsutil.AtomicWriteFile(progressPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	run := &fakeRunner{
		outputs: map[string]string{
			"cryptsetup luksUUID /dev/sda3": "test-uuid\n",
		},
	}
	mgr := NewPoolManager(run, nil)

	if err := mgr.ResumePasswordRotationIfNeeded(context.Background(), "/dev/sda3", "password"); err != nil {
		t.Fatalf("expected no-op for completed device: %v", err)
	}

	if _, err := os.Stat(progressPath); !os.IsNotExist(err) {
		t.Error("expected progress file to be removed")
	}
}

func TestResumeMnemonicRotation_NoProgress(t *testing.T) {
	paths.SetRootsForTest(t)

	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil)

	if err := mgr.ResumeMnemonicRotationIfNeeded(context.Background(), "/dev/sda3", nil); err != nil {
		t.Fatalf("expected no-op: %v", err)
	}
}

func TestResumeMnemonicRotation_NilKey_Deferred(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	cryptoDir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(cryptoDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Write incomplete progress.
	progress := RotationProgress{
		StartedAt: time.Now(),
		Total:     1,
		Completed: []string{},
	}
	data, _ := json.MarshalIndent(progress, "", "  ")
	progressPath := filepath.Join(cryptoDir, "mnemonic-rotation-progress.json")
	if err := fsutil.AtomicWriteFile(progressPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	run := &fakeRunner{}
	mgr := NewPoolManager(run, nil)

	// With nil key, rotation should be deferred (no error).
	if err := mgr.ResumeMnemonicRotationIfNeeded(context.Background(), "/dev/sda3", nil); err != nil {
		t.Fatalf("expected deferred no-op: %v", err)
	}

	// Progress file should still exist (rotation deferred).
	if _, err := os.Stat(progressPath); os.IsNotExist(err) {
		t.Error("expected progress file to still exist (rotation deferred)")
	}
}
