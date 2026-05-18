package persistence

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"piccolod/internal/crypt"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

// blobCrypto is the AEAD surface the blob primitives need. Defined
// locally so the helpers can be tested without constructing a full
// crypt.Manager — and so the reconciler can substitute a stub.
type blobCrypto interface {
	EncryptWithAAD(plaintext, aad []byte) ([]byte, error)
	DecryptWithAAD(ciphertext, aad []byte) ([]byte, error)
}

// Keyslot pending-blob primitives — RFC 20260510 §Architecture.
//
// The reconciler is async, but the synchronous handler (/generate, password
// change) needs to hand the passphrase off durably so the reconciler can
// finish provisioning across the per-process / reboot boundary. Blobs live
// at <core>/mounts/control-plane/recovery/keyslot-pending-<slot>-<key_id>.blob
// and survive process restart and system reboot (the encrypted control
// plane survives reboot; only the SDEK in RAM is wiped).
//
// Format (D13, S8 fix):
//   version_byte(1) || nonce(12) || AEAD(plaintext, AAD = slot||key_id||version_byte)
//
// The AAD binds the ciphertext to (slot, key_id, version) so an attacker
// who can write blob files cannot trick the reconciler by swapping a
// slot-1 blob for slot-2 or replaying an older generation's blob.

const (
	keyslotPendingPrefix     = "keyslot-pending-"
	keyslotPendingSuffix     = ".blob"
	keyslotBlobVersionV1     = byte(0x01)
	keyslotRecoverySubdir    = "recovery"
	keyslotControlPlaneMount = "control-plane"
)

// KeyslotSlot enumerates the LUKS keyslot identifiers the reconciler
// manages. Slot 0 is the LUKS master-key slot (managed by piccolod's
// internal master-key path); slots 1 and 2 are the operator-facing slots.
type KeyslotSlot int

const (
	KeyslotPassword KeyslotSlot = 1
	KeyslotRecovery KeyslotSlot = 2
)

// keyslotBlob is the in-memory representation of a pending-passphrase blob
// after decoding. Fields are intentionally minimal — only what the
// reconciler needs to capture the (slot, key_id, passphrase) triplet
// atomically per pass (D9).
type keyslotBlob struct {
	Slot       KeyslotSlot
	KeyID      string
	Passphrase []byte
	Path       string // filesystem path for cleanup
}

// keyslotRecoveryDir returns the on-disk directory pending blobs live in.
// Computed each call from paths.CoreJoin so the tests can override the
// core root mid-process.
func keyslotRecoveryDir() string {
	return paths.CoreJoin("mounts", keyslotControlPlaneMount, keyslotRecoverySubdir)
}

// keyslotBlobName builds the filesystem name for a pending blob keyed by
// (slot, key_id). Key_id is the generation fingerprint — non-empty and
// hex-only by construction (sha256 prefix). Caller enforces non-empty.
func keyslotBlobName(slot KeyslotSlot, keyID string) string {
	return fmt.Sprintf("%s%d-%s%s", keyslotPendingPrefix, int(slot), keyID, keyslotPendingSuffix)
}

// keyslotBlobPath returns the absolute path for a pending blob.
func keyslotBlobPath(slot KeyslotSlot, keyID string) string {
	return filepath.Join(keyslotRecoveryDir(), keyslotBlobName(slot, keyID))
}

// keyslotBlobAAD constructs the AAD bound into the AEAD ciphertext per
// D13: slot || uint16(len(key_id)) || key_id || version_byte. The length
// prefix on key_id makes the AAD canonical even if future callers pass a
// different fingerprint shape — without the prefix, `slot=1 key_id="ab"`
// and `slot=1 key_id="ab\x01"` would produce indistinguishable AAD prefixes.
// Today's call sites always pass 16 lowercase hex chars (sha256[:8]) but
// the primitive defends the invariant rather than assuming it.
func keyslotBlobAAD(slot KeyslotSlot, keyID string, version byte) []byte {
	aad := make([]byte, 0, 1+2+len(keyID)+1)
	aad = append(aad, byte(slot))
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(keyID)))
	aad = append(aad, lenBuf[:]...)
	aad = append(aad, []byte(keyID)...)
	aad = append(aad, version)
	return aad
}

// writeKeyslotBlob seals the passphrase under SDEK-AES-GCM with the D13
// AAD and atomically writes the resulting blob at the (slot, key_id)
// location. RFC F-iter3-B1: uses fsutil.AtomicWriteFile so power loss
// between write and the subsequent persistent state commit (keyset.json /
// userManager.ChangePassword) cannot leave the device with a stale blob
// referenced by a committed key_id.
func writeKeyslotBlob(crypto blobCrypto, slot KeyslotSlot, keyID string, passphrase []byte) error {
	if crypto == nil {
		return errors.New("keyslot blob: crypto manager unavailable")
	}
	if keyID == "" {
		return errors.New("keyslot blob: key_id required")
	}
	aad := keyslotBlobAAD(slot, keyID, keyslotBlobVersionV1)
	ct, err := crypto.EncryptWithAAD(passphrase, aad)
	if err != nil {
		return fmt.Errorf("keyslot blob: seal: %w", err)
	}
	return writeKeyslotBlobBytes(slot, keyID, ct)
}

// writeKeyslotBlobWithKey writes a slot blob using an explicit SDEK
// (RFC 20260510 D11 hook path — the caller holds the crypt manager's
// write lock so it cannot re-enter EncryptWithAAD's RLock without
// deadlocking). Identical wire format to writeKeyslotBlob.
func writeKeyslotBlobWithKey(sdek []byte, slot KeyslotSlot, keyID string, passphrase []byte) error {
	if len(sdek) == 0 {
		return errors.New("keyslot blob: sdek required")
	}
	if keyID == "" {
		return errors.New("keyslot blob: key_id required")
	}
	aad := keyslotBlobAAD(slot, keyID, keyslotBlobVersionV1)
	ct, err := crypt.EncryptWithExplicitKey(sdek, passphrase, aad)
	if err != nil {
		return fmt.Errorf("keyslot blob: seal: %w", err)
	}
	return writeKeyslotBlobBytes(slot, keyID, ct)
}

// writeKeyslotBlobBytes is the shared write tail: prefix version, mkdir,
// atomic write. The ciphertext is already AAD-bound at this point.
func writeKeyslotBlobBytes(slot KeyslotSlot, keyID string, ct []byte) error {
	blob := make([]byte, 0, 1+len(ct))
	blob = append(blob, keyslotBlobVersionV1)
	blob = append(blob, ct...)
	dir := keyslotRecoveryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keyslot blob: mkdir: %w", err)
	}
	if err := fsutil.AtomicWriteFile(keyslotBlobPath(slot, keyID), blob, 0o600); err != nil {
		return fmt.Errorf("keyslot blob: write: %w", err)
	}
	return nil
}

// readKeyslotBlob opens the blob at (slot, key_id), validates the version
// byte (S8 hazard: unknown versions are treated as orphans and refused),
// and returns the plaintext passphrase. Caller is responsible for zeroing
// the returned bytes after use.
func readKeyslotBlob(crypto blobCrypto, slot KeyslotSlot, keyID string) (*keyslotBlob, error) {
	if crypto == nil {
		return nil, errors.New("keyslot blob: crypto manager unavailable")
	}
	path := keyslotBlobPath(slot, keyID)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 1 {
		return nil, fmt.Errorf("keyslot blob: %s: truncated", path)
	}
	version := raw[0]
	if version != keyslotBlobVersionV1 {
		return nil, fmt.Errorf("keyslot blob: %s: unknown version 0x%02x", path, version)
	}
	aad := keyslotBlobAAD(slot, keyID, version)
	pt, err := crypto.DecryptWithAAD(raw[1:], aad)
	if err != nil {
		return nil, fmt.Errorf("keyslot blob: %s: open: %w", path, err)
	}
	return &keyslotBlob{
		Slot:       slot,
		KeyID:      keyID,
		Passphrase: pt,
		Path:       path,
	}, nil
}

// deleteKeyslotBlob removes a pending blob. Idempotent — absent files
// return nil so end-of-pass cleanup can run unconditionally without
// gating on whether the file ever existed.
func deleteKeyslotBlob(slot KeyslotSlot, keyID string) error {
	err := os.Remove(keyslotBlobPath(slot, keyID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// listKeyslotBlobs enumerates all pending blobs for a given slot and
// returns them sorted by key_id. Slot scoping is enforced by the filename
// prefix — there is no temporal ordering implied by the sort (RFC F-A2:
// sha256-prefix fingerprints have no ordering by construction). Caller
// uses the live key_id from cryptoManager to identify the matching blob
// and treats every other entry as an orphan eligible for cleanup.
func listKeyslotBlobs(slot KeyslotSlot) ([]string, error) {
	dir := keyslotRecoveryDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	prefix := fmt.Sprintf("%s%d-", keyslotPendingPrefix, int(slot))
	var keyIDs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, keyslotPendingSuffix) {
			continue
		}
		idPart := strings.TrimSuffix(strings.TrimPrefix(name, prefix), keyslotPendingSuffix)
		if !isHexLower(idPart) {
			// Junk file in the recovery dir — ignore. Cleanup belongs to a
			// separate pass that the reconciler can run; refusing to parse
			// here keeps listKeyslotBlobs side-effect-free.
			continue
		}
		keyIDs = append(keyIDs, idPart)
	}
	sort.Strings(keyIDs)
	return keyIDs, nil
}

// isHexLower reports whether s is non-empty and contains only lowercase
// hex digits. fingerprintSDEKRK / FingerprintPasswordHash both emit this
// shape; rejecting other shapes prevents path-traversal style filenames
// from being treated as live blobs.
func isHexLower(s string) bool {
	if s == "" {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// cleanupOrphanKeyslotBlobs deletes every pending blob for the given slot
// whose key_id does not match liveKeyID. RFC F-A2 cleanup rule — runs at
// process startup and at end-of-pass-success. liveKeyID == "" means "no
// live recovery/password key for this slot" → all blobs are orphans.
func cleanupOrphanKeyslotBlobs(slot KeyslotSlot, liveKeyID string) error {
	return cleanupOrphanKeyslotBlobsPreserve(slot, []string{liveKeyID})
}

// cleanupOrphanKeyslotBlobsPreserve deletes every pending blob whose
// key_id is NOT in the preserve set. Used by the reconciler's pass-exit
// defer to preserve both the captured blob AND any newer blob written
// by a concurrent /generate or password change — the prior single-id
// version would delete the newer blob and lose the rotation's
// passphrase (codex P1).
func cleanupOrphanKeyslotBlobsPreserve(slot KeyslotSlot, preserve []string) error {
	ids, err := listKeyslotBlobs(slot)
	if err != nil {
		return err
	}
	preserveSet := make(map[string]struct{}, len(preserve))
	for _, p := range preserve {
		if p != "" {
			preserveSet[p] = struct{}{}
		}
	}
	var errs []error
	for _, id := range ids {
		if _, keep := preserveSet[id]; keep {
			continue
		}
		if err := deleteKeyslotBlob(slot, id); err != nil {
			errs = append(errs, fmt.Errorf("delete orphan slot=%d key_id=%s: %w", slot, id, err))
		}
	}
	return errors.Join(errs...)
}
