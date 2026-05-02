package autounlock

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
)

func TestReadBlob_Missing(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	_, err := ReadBlob()
	if err != ErrBlobMissing {
		t.Fatalf("expected ErrBlobMissing; got %v", err)
	}
}

func TestWriteReadBlob_RoundTrip(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	want := []byte{0x01, 0x02, 0x03, 0xff}
	if err := WriteBlob(want); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	got, err := ReadBlob()
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("blob mismatch: got %x, want %x", got, want)
	}
}

func TestWriteBlob_Permissions(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := WriteBlob([]byte("test")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	info, err := os.Stat(filepath.Join(stateDir(), "auto_unlock_blob"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("blob mode = %o; want 0600", mode)
	}
}

func TestDeleteBlob_Idempotent(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := DeleteBlob(); err != nil {
		t.Fatalf("DeleteBlob (missing): %v", err)
	}
	if err := WriteBlob([]byte("x")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if err := DeleteBlob(); err != nil {
		t.Fatalf("DeleteBlob (present): %v", err)
	}
	if BlobExists() {
		t.Errorf("BlobExists true after DeleteBlob")
	}
	if err := DeleteBlob(); err != nil {
		t.Fatalf("DeleteBlob (already gone): %v", err)
	}
}

func TestBlobExists(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if BlobExists() {
		t.Error("BlobExists true before write")
	}
	if err := WriteBlob([]byte("x")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if !BlobExists() {
		t.Error("BlobExists false after write")
	}
}

func TestWriteBlob_OverwritesAtomically(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	if err := WriteBlob([]byte("first")); err != nil {
		t.Fatalf("first WriteBlob: %v", err)
	}
	if err := WriteBlob([]byte("second")); err != nil {
		t.Fatalf("second WriteBlob: %v", err)
	}
	got, err := ReadBlob()
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("expected 'second' after overwrite; got %q", got)
	}
}
