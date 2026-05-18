package persistence

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"piccolod/internal/crypt"
	"piccolod/internal/state/paths"
)

// newUnlockedCryptManager constructs a fully-initialized crypt.Manager
// holding a fresh SDEK so EncryptWithAAD / DecryptWithAAD succeed.
func newUnlockedCryptManager(t *testing.T) *crypt.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := crypt.NewManager(dir)
	if err != nil {
		t.Fatalf("crypt.NewManager: %v", err)
	}
	if err := mgr.Setup("test-password"); err != nil {
		t.Fatalf("crypt.Setup: %v", err)
	}
	if err := mgr.Unlock("test-password"); err != nil {
		t.Fatalf("crypt.Unlock: %v", err)
	}
	return mgr
}

func TestKeyslotBlob_RoundTrip(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	passphrase := []byte("recovery-mnemonic-24-words")
	keyID := "deadbeefcafe1234"
	if err := writeKeyslotBlob(mgr, KeyslotRecovery, keyID, passphrase); err != nil {
		t.Fatalf("writeKeyslotBlob: %v", err)
	}
	got, err := readKeyslotBlob(mgr, KeyslotRecovery, keyID)
	if err != nil {
		t.Fatalf("readKeyslotBlob: %v", err)
	}
	if string(got.Passphrase) != string(passphrase) {
		t.Errorf("passphrase mismatch: got %q want %q", got.Passphrase, passphrase)
	}
	if got.Slot != KeyslotRecovery || got.KeyID != keyID {
		t.Errorf("identity mismatch: got slot=%d key_id=%s", got.Slot, got.KeyID)
	}
}

// AAD binding: a blob written for (slot=1, key_id=A) must NOT decrypt as
// (slot=2, key_id=A) or (slot=1, key_id=B) — D13 invariant.
func TestKeyslotBlob_AADBindingPreventsSlotSwap(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	keyID := "feedface12345678"
	if err := writeKeyslotBlob(mgr, KeyslotPassword, keyID, []byte("admin-pw")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Rename to look like a slot-2 blob — same ciphertext, different
	// AAD binding. Read at slot 2 must fail.
	src := keyslotBlobPath(KeyslotPassword, keyID)
	dst := keyslotBlobPath(KeyslotRecovery, keyID)
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	_, err := readKeyslotBlob(mgr, KeyslotRecovery, keyID)
	if err == nil {
		t.Fatalf("expected AEAD-Open failure on slot swap, got success")
	}
}

func TestKeyslotBlob_AADBindingPreventsKeyIDSwap(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	keyIDA := "aaaaaaaaaaaaaaaa"
	keyIDB := "bbbbbbbbbbbbbbbb"
	if err := writeKeyslotBlob(mgr, KeyslotRecovery, keyIDA, []byte("words-A")); err != nil {
		t.Fatalf("write: %v", err)
	}
	src := keyslotBlobPath(KeyslotRecovery, keyIDA)
	dst := keyslotBlobPath(KeyslotRecovery, keyIDB)
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	_, err := readKeyslotBlob(mgr, KeyslotRecovery, keyIDB)
	if err == nil {
		t.Fatalf("expected AEAD-Open failure on key_id swap, got success")
	}
}

// Version-byte mismatch is rejected so future format bumps don't silently
// fall back to v1 decryption (S8 hazard).
func TestKeyslotBlob_UnknownVersionRejected(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	dir := keyslotRecoveryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a blob with version byte 0x02 (unknown). Body bytes don't
	// matter; the version check runs before AEAD open.
	path := keyslotBlobPath(KeyslotRecovery, "1234567890abcdef")
	if err := os.WriteFile(path, []byte{0x02, 0x00, 0x00, 0x00}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readKeyslotBlob(mgr, KeyslotRecovery, "1234567890abcdef")
	if err == nil || !contains(err.Error(), "unknown version") {
		t.Fatalf("expected unknown-version error, got %v", err)
	}
}

func TestKeyslotBlob_DeleteIdempotent(t *testing.T) {
	paths.SetRootsForTest(t)
	// Deleting a non-existent blob is a no-op (end-of-pass cleanup
	// should not gate on prior existence).
	if err := deleteKeyslotBlob(KeyslotPassword, "abcd1234"); err != nil {
		t.Fatalf("delete on missing blob: %v", err)
	}
}

func TestListKeyslotBlobs_ScopedBySlot(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	// Three blobs for slot 1, two for slot 2.
	want1 := []string{"aaaa1111aaaa1111", "bbbb2222bbbb2222", "cccc3333cccc3333"}
	want2 := []string{"dddd4444dddd4444", "eeee5555eeee5555"}
	for _, id := range want1 {
		if err := writeKeyslotBlob(mgr, KeyslotPassword, id, []byte("p")); err != nil {
			t.Fatalf("write slot=1 id=%s: %v", id, err)
		}
	}
	for _, id := range want2 {
		if err := writeKeyslotBlob(mgr, KeyslotRecovery, id, []byte("p")); err != nil {
			t.Fatalf("write slot=2 id=%s: %v", id, err)
		}
	}

	got1, err := listKeyslotBlobs(KeyslotPassword)
	if err != nil {
		t.Fatalf("list slot 1: %v", err)
	}
	sort.Strings(want1)
	if !equalStringSlice(got1, want1) {
		t.Errorf("slot=1 got %v want %v", got1, want1)
	}
	got2, err := listKeyslotBlobs(KeyslotRecovery)
	if err != nil {
		t.Fatalf("list slot 2: %v", err)
	}
	sort.Strings(want2)
	if !equalStringSlice(got2, want2) {
		t.Errorf("slot=2 got %v want %v", got2, want2)
	}
}

func TestListKeyslotBlobs_RejectsJunkFilenames(t *testing.T) {
	paths.SetRootsForTest(t)
	dir := keyslotRecoveryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Plant junk files mimicking the prefix but with non-hex id. The
	// real path-traversal concern is enforced upstream by listKeyslotBlobs
	// only honoring lowercase-hex ids; the test below validates that.
	for _, name := range []string{
		"keyslot-pending-1-..%2Ffoo.blob",
		"keyslot-pending-1-NOTHEX.blob",
		"keyslot-pending-1-UPPERHEX.blob",
		"keyslot-pending-1-AAAA.blob", // upper-case hex still rejected
		"keyslot-pending-1-.blob",
		"unrelated.blob",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := listKeyslotBlobs(KeyslotPassword)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected junk filenames rejected, got %v", got)
	}
}

func TestCleanupOrphanKeyslotBlobs(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	live := "feedface00112233"
	stale := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"}
	for _, id := range append([]string{live}, stale...) {
		if err := writeKeyslotBlob(mgr, KeyslotRecovery, id, []byte("p")); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}
	if err := cleanupOrphanKeyslotBlobs(KeyslotRecovery, live); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	remaining, err := listKeyslotBlobs(KeyslotRecovery)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != live {
		t.Errorf("expected only live blob to remain, got %v", remaining)
	}
}

// Cleanup with no live key_id deletes everything (post-rotation rollback /
// initial setup state).
func TestCleanupOrphanKeyslotBlobs_AllOrphansWhenNoLiveKey(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)

	for _, id := range []string{"abcd1234abcd1234", "5678efef5678efef"} {
		if err := writeKeyslotBlob(mgr, KeyslotPassword, id, []byte("p")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := cleanupOrphanKeyslotBlobs(KeyslotPassword, ""); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	got, _ := listKeyslotBlobs(KeyslotPassword)
	if len(got) != 0 {
		t.Errorf("expected all blobs cleaned, got %v", got)
	}
}

// Round-trip with a locked manager: write requires SDEK, so a locked
// manager surfaces ErrLocked. Reconciler honors this (D8).
func TestKeyslotBlob_WriteFailsWhenLocked(t *testing.T) {
	paths.SetRootsForTest(t)
	mgr := newUnlockedCryptManager(t)
	mgr.Lock()
	err := writeKeyslotBlob(mgr, KeyslotRecovery, "1234567812345678", []byte("p"))
	if err == nil || !errors.Is(err, crypt.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
