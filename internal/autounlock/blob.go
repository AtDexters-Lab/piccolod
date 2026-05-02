package autounlock

import (
	"errors"
	"os"
	"path/filepath"

	"piccolod/internal/fsutil"
)

// blob lifecycle: write-temp-fsync-rename via fsutil.AtomicWriteFile, read
// pre-unlock, delete after pickup-success or posture-disable. The blob lives
// at {coreRoot}/network-bootstrap/security/auto_unlock_blob — alongside the
// posture state file, outside the encrypted control volume so pickup can
// read it pre-unlock. Format is opaque to this package: nonce + AEAD output
// produced by crypt.Manager.WrapSDEKForEscrow.

func blobPath() string {
	return filepath.Join(stateDir(), "auto_unlock_blob")
}

// WriteBlob atomically writes the AEAD blob produced by ceremony's
// WrapSDEKForEscrow. Caller has already verified the manager was unlocked.
func WriteBlob(blob []byte) error {
	if err := EnsureStateDir(); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(blobPath(), blob, 0o600)
}

// ReadBlob returns the on-disk blob. Returns ErrBlobMissing if the file does
// not exist (typical post-crash boot path). Other I/O errors propagate.
func ReadBlob() ([]byte, error) {
	b, err := os.ReadFile(blobPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrBlobMissing
		}
		return nil, err
	}
	return b, nil
}

// DeleteBlob removes the on-disk blob. Idempotent — absent file returns nil.
// Called after successful pickup, on posture-disable, and on pickup failure
// when retry is no longer meaningful (e.g. ErrEscrowNotFound from namek).
func DeleteBlob() error {
	err := os.Remove(blobPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// BlobExists returns true if the on-disk blob is present. Used by the
// posture HTTP GET handler to surface `has_outstanding_blob` to the UI.
func BlobExists() bool {
	_, err := os.Stat(blobPath())
	return err == nil
}
