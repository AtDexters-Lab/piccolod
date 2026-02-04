package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFile(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("writes file successfully", func(t *testing.T) {
		path := filepath.Join(tmpDir, "test1.txt")
		content := []byte("hello world")

		err := AtomicWriteFile(path, content, 0644)
		if err != nil {
			t.Fatalf("AtomicWriteFile failed: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if string(data) != string(content) {
			t.Errorf("content mismatch: got %q, want %q", data, content)
		}
	})

	t.Run("overwrites existing file atomically", func(t *testing.T) {
		path := filepath.Join(tmpDir, "test2.txt")
		original := []byte("original content")
		updated := []byte("updated content")

		if err := os.WriteFile(path, original, 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		err := AtomicWriteFile(path, updated, 0644)
		if err != nil {
			t.Fatalf("AtomicWriteFile failed: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if string(data) != string(updated) {
			t.Errorf("content mismatch: got %q, want %q", data, updated)
		}
	})

	t.Run("sets correct permissions", func(t *testing.T) {
		path := filepath.Join(tmpDir, "test3.txt")
		content := []byte("test")

		err := AtomicWriteFile(path, content, 0600)
		if err != nil {
			t.Fatalf("AtomicWriteFile failed: %v", err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}
		// On most systems, umask may affect permissions, so check the owner bits
		perm := info.Mode().Perm()
		if perm&0600 != 0600 {
			t.Errorf("unexpected permissions: got %o, want at least 0600", perm)
		}
	})

	t.Run("no temp file left on success", func(t *testing.T) {
		subDir := filepath.Join(tmpDir, "subdir")
		if err := os.Mkdir(subDir, 0755); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		path := filepath.Join(subDir, "test4.txt")
		content := []byte("test")

		if err := AtomicWriteFile(path, content, 0644); err != nil {
			t.Fatalf("AtomicWriteFile failed: %v", err)
		}

		entries, err := os.ReadDir(subDir)
		if err != nil {
			t.Fatalf("ReadDir failed: %v", err)
		}

		if len(entries) != 1 {
			t.Errorf("expected 1 file, got %d", len(entries))
		}
		if entries[0].Name() != "test4.txt" {
			t.Errorf("unexpected file: %s", entries[0].Name())
		}
	})

	t.Run("fails on non-existent directory", func(t *testing.T) {
		path := filepath.Join(tmpDir, "nonexistent", "test.txt")
		content := []byte("test")

		err := AtomicWriteFile(path, content, 0644)
		if err == nil {
			t.Error("expected error for non-existent directory, got nil")
		}
	})

	t.Run("handles empty content", func(t *testing.T) {
		path := filepath.Join(tmpDir, "empty.txt")
		content := []byte{}

		err := AtomicWriteFile(path, content, 0644)
		if err != nil {
			t.Fatalf("AtomicWriteFile failed: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if len(data) != 0 {
			t.Errorf("expected empty file, got %d bytes", len(data))
		}
	})

	t.Run("handles large content", func(t *testing.T) {
		path := filepath.Join(tmpDir, "large.txt")
		// 1MB of data
		content := make([]byte, 1024*1024)
		for i := range content {
			content[i] = byte(i % 256)
		}

		err := AtomicWriteFile(path, content, 0644)
		if err != nil {
			t.Fatalf("AtomicWriteFile failed: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if len(data) != len(content) {
			t.Errorf("content length mismatch: got %d, want %d", len(data), len(content))
		}
	})
}
