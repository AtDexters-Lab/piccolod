// Package fsutil provides filesystem utilities for safe and atomic file operations.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWriteFile writes data to a file atomically using the rename pattern.
// This prevents partial writes from corrupting the file if the process is
// interrupted (e.g., during VM save state or unexpected shutdown).
//
// The function:
//  1. Creates a temp file in the same directory (required for atomic rename)
//  2. Writes data to the temp file
//  3. Syncs the temp file to disk (fsync)
//  4. Sets the requested permissions
//  5. Atomically renames temp file to target path
//  6. Syncs the parent directory for full durability
//
// If any step fails, the temp file is cleaned up and the original file
// (if it exists) remains unchanged.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Create temp file in the same directory (required for atomic rename)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Cleanup on failure
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	// Write data
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	// Sync to disk to ensure data is persisted before rename
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	// Close before rename (required on some systems)
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Set permissions
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to final: %w", err)
	}

	// Mark success immediately after rename - the file is now safely written.
	// This must happen before syncDir to prevent the defer from trying to
	// clean up the (now non-existent) temp file if syncDir fails.
	success = true

	// Sync parent directory to ensure rename is durable across power loss.
	// This is defense-in-depth: without this, the directory entry pointing
	// to the new file might not be persisted on some filesystems (ext4/xfs).
	// Best-effort: failure here doesn't affect correctness, only durability.
	_ = SyncDir(dir)

	return nil
}

// SyncDir syncs a directory to ensure all metadata changes are durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
