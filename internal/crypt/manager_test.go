package crypt

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"piccolod/internal/state/paths"
)

func TestManager_RewrapUnlocked(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("old-secret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("old-secret"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := m.RewrapUnlocked("new-secret"); err != nil {
		t.Fatalf("RewrapUnlocked: %v", err)
	}
	m.Lock()
	if err := m.Unlock("old-secret"); err == nil {
		t.Fatalf("expected old password to fail after rewrap")
	}
	if err := m.Unlock("new-secret"); err != nil {
		t.Fatalf("Unlock new password: %v", err)
	}
}

func TestManager_GenerateRecoveryKeyRotation(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("admin-pass"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("admin-pass"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	words1, _, err := m.GenerateRecoveryKey(false)
	if err != nil {
		t.Fatalf("GenerateRecoveryKey first: %v", err)
	}
	if len(words1) != 24 {
		t.Fatalf("expected 24 words, got %d", len(words1))
	}
	if _, _, err := m.GenerateRecoveryKey(false); err == nil {
		t.Fatalf("expected error when regenerating without force")
	}
	words2, _, err := m.GenerateRecoveryKey(true)
	if err != nil {
		t.Fatalf("GenerateRecoveryKey force: %v", err)
	}
	if len(words2) != 24 {
		t.Fatalf("expected 24 words on rotation, got %d", len(words2))
	}
	if strings.Join(words1, " ") == strings.Join(words2, " ") {
		t.Fatalf("expected different mnemonic after rotation")
	}
	if !m.HasRecoveryKey() {
		t.Fatalf("expected HasRecoveryKey to remain true after rotation")
	}
	// Verify the new recovery key actually works (round-trip test)
	m.Lock()
	if err := m.UnlockWithRecoveryKey(words2); err != nil {
		t.Fatalf("failed to unlock with rotated recovery key: %v", err)
	}
}

// TestManager_GenerateRecoveryKey_AtomicWordsKeyIDPairing pins the REC-4
// race-closure contract: under concurrent rotation, each caller's returned
// (words, keyID) pair MUST be self-consistent — keyID is the fingerprint of
// the SDEKRK that wraps THIS caller's words. Computing keyID via a separate
// post-write RecoveryKeyID() read allows another caller's rotation to
// interleave, returning words from generation A but keyID of generation B.
//
// This test would fail on the pre-fix /generate handler that called
// RecoveryKeyID() after GenerateRecoveryKey returned (codex Phase 3 finding,
// 2026-04-28). Fix: GenerateRecoveryKey computes keyID inside its write
// lock from the just-constructed st.SDEKRK.
func TestManager_GenerateRecoveryKey_AtomicWordsKeyIDPairing(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("admin-pass"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("admin-pass"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// First generation establishes a key so subsequent rotations exercise the
	// force=true branch.
	if _, _, err := m.GenerateRecoveryKey(false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Run N concurrent rotations and verify each returned (words, keyID) is
	// internally consistent. Specifically: keyID must be the fingerprint of
	// the SDEKRK that wraps the caller's words. We verify this by re-deriving
	// the wrapping key from the words and comparing the resulting ciphertext's
	// fingerprint — but a simpler proof is: at least one caller must observe
	// a keyID equal to RecoveryKeyID()-at-some-point — which is satisfied if
	// the function preserves the "words and keyID came from the same write"
	// invariant. Stronger property: any two concurrent callers must NOT return
	// the same keyID with different words (would mean one fingerprint paired
	// with two distinct mnemonics).
	const N = 8
	type pair struct {
		words []string
		keyID string
	}
	pairs := make([]pair, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			words, keyID, err := m.GenerateRecoveryKey(true)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			pairs[idx] = pair{words: words, keyID: keyID}
		}(i)
	}
	wg.Wait()

	// Each (words, keyID) must be unique together. Two different callers
	// returning the SAME keyID for DIFFERENT words is the race signature.
	for i := 0; i < N; i++ {
		for j := i + 1; j < N; j++ {
			if pairs[i].keyID == pairs[j].keyID {
				wi := strings.Join(pairs[i].words, " ")
				wj := strings.Join(pairs[j].words, " ")
				if wi != wj {
					t.Fatalf("REC-4 race: callers %d and %d returned same keyID %q with different words — keyID was computed from on-disk state after another caller rotated past us",
						i, j, pairs[i].keyID)
				}
				// Same keyID + same words is impossible (each call generates
				// fresh entropy), but the comparison is for safety.
				t.Fatalf("unexpected: callers %d and %d returned identical (words, keyID) — entropy generation collapsed?", i, j)
			}
		}
	}
}

func TestManager_UnlockWithRecoveryKey_Validation(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("admin-pass"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Test: unlock without recovery key set
	if err := m.UnlockWithRecoveryKey([]string{"abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "art"}); err == nil {
		t.Fatalf("expected error when no recovery key is set")
	}

	// Generate a recovery key
	if err := m.Unlock("admin-pass"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	words, _, err := m.GenerateRecoveryKey(false)
	if err != nil {
		t.Fatalf("GenerateRecoveryKey: %v", err)
	}
	m.Lock()

	// Test: truncated mnemonic (invalid BIP39)
	if err := m.UnlockWithRecoveryKey(words[:12]); err == nil {
		t.Fatalf("expected error for truncated mnemonic")
	}

	// Test: invalid BIP39 checksum (flip last word)
	badWords := make([]string, len(words))
	copy(badWords, words)
	badWords[23] = "zoo" // unlikely to match checksum
	if err := m.UnlockWithRecoveryKey(badWords); err == nil {
		t.Fatalf("expected error for invalid BIP39 checksum")
	}

	// Test: valid BIP39 but wrong mnemonic
	wrongMnemonic := []string{"abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "abandon", "art"}
	if err := m.UnlockWithRecoveryKey(wrongMnemonic); err == nil {
		t.Fatalf("expected error for wrong mnemonic")
	}

	// Test: correct mnemonic works
	if err := m.UnlockWithRecoveryKey(words); err != nil {
		t.Fatalf("expected correct mnemonic to unlock: %v", err)
	}
}

func TestManager_EncryptDecrypt(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("secret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("secret"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	plaintext := []byte("hello, pool keyfile data!")
	ct, err := m.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext should differ from plaintext")
	}

	pt, err := m.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("decrypted = %q, want %q", pt, plaintext)
	}
}

func TestManager_EncryptDecrypt_Locked(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("secret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Don't unlock — should fail.
	if _, err := m.Encrypt([]byte("test")); err == nil {
		t.Fatal("expected error when locked")
	}
	if _, err := m.Decrypt([]byte("test")); err == nil {
		t.Fatal("expected error when locked")
	}
}

func TestManager_PoolKeyfile_Roundtrip(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	cryptoDir := filepath.Join(core, "crypto")
	if err := os.MkdirAll(cryptoDir, 0o700); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(core)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("secret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("secret"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// Generate and store a pool keyfile.
	rawKey := make([]byte, poolKeyfileSize)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	if err := m.StorePoolKeyfile(rawKey); err != nil {
		t.Fatalf("StorePoolKeyfile: %v", err)
	}

	// Unwrap and verify.
	got, err := m.UnwrapPoolKeyfile()
	if err != nil {
		t.Fatalf("UnwrapPoolKeyfile: %v", err)
	}
	if !bytes.Equal(got, rawKey) {
		t.Fatal("unwrapped keyfile does not match original")
	}
}

func TestManager_PoolKeyfileAt_CustomPath(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("secret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("secret"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	rawKey := make([]byte, poolKeyfileSize)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatal(err)
	}

	customPath := filepath.Join(dir, "custom_key.enc")
	if err := m.StorePoolKeyfileAt(rawKey, customPath); err != nil {
		t.Fatalf("StorePoolKeyfileAt: %v", err)
	}

	got, err := m.UnwrapPoolKeyfileFrom(customPath)
	if err != nil {
		t.Fatalf("UnwrapPoolKeyfileFrom: %v", err)
	}
	if !bytes.Equal(got, rawKey) {
		t.Fatal("unwrapped keyfile does not match original")
	}
}

func TestManager_MnemonicKeyCallbacks(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Without callback set, should error.
	if err := m.WithMnemonicKey(func(key []byte) error { return nil }); err == nil {
		t.Fatal("expected error when callback not set")
	}
	if err := m.WithOldMnemonicKey(func(key []byte) error { return nil }); err == nil {
		t.Fatal("expected error when callback not set")
	}

	// Set callbacks.
	testKey := []byte("test-mnemonic-derived-key")
	m.SetMnemonicKeyCallback(func(fn func([]byte) error) error {
		return fn(testKey)
	})
	m.SetOldMnemonicKeyCallback(func(fn func([]byte) error) error {
		return fn([]byte("old-key"))
	})

	var received []byte
	err = m.WithMnemonicKey(func(key []byte) error {
		received = make([]byte, len(key))
		copy(received, key)
		return nil
	})
	if err != nil {
		t.Fatalf("WithMnemonicKey: %v", err)
	}
	if !bytes.Equal(received, testKey) {
		t.Fatalf("received key = %q, want %q", received, testKey)
	}
}

func TestManager_OnKeyMaterialChanged(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}

	called := false
	m.OnKeyMaterialChanged = func() { called = true }
	m.notifyKeyMaterialChanged()
	if !called {
		t.Error("OnKeyMaterialChanged callback not called")
	}
}

func TestRandomKeyMaterial_UniqueKeys(t *testing.T) {
	k1 := make([]byte, poolKeyfileSize)
	k2 := make([]byte, poolKeyfileSize)
	if _, err := rand.Read(k1); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(k2); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Error("expected different keys from two rand.Read calls")
	}
}

func setupUnlocked(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Setup("admin-pass"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := m.Unlock("admin-pass"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	return m
}

func TestManager_WrapUnwrapEscrow_RoundTrip(t *testing.T) {
	m := setupUnlocked(t)
	F := make([]byte, 32)
	if _, err := rand.Read(F); err != nil {
		t.Fatal(err)
	}
	aad := []byte("device-abc123|auto_unlock_v1")

	blob, err := m.WrapSDEKForEscrow(F, aad)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	// Snapshot SDEK, lock, then unwrap and verify it matches.
	var original []byte
	if err := m.WithSDEK(func(sdek []byte) error { original = bytes.Clone(sdek); return nil }); err != nil {
		t.Fatalf("WithSDEK: %v", err)
	}
	m.Lock()

	if err := m.UnwrapSDEKWithEscrow(blob, F, aad); err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if err := m.WithSDEK(func(sdek []byte) error {
		if !bytes.Equal(sdek, original) {
			t.Fatalf("SDEK after unwrap differs from original")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithSDEK after unwrap: %v", err)
	}
}

func TestManager_WrapForEscrow_LockedFails(t *testing.T) {
	m := setupUnlocked(t)
	m.Lock()
	F := make([]byte, 32)
	if _, err := m.WrapSDEKForEscrow(F, nil); err != ErrAutoUnlockManagerLocked {
		t.Fatalf("expected ErrAutoUnlockManagerLocked, got %v", err)
	}
}

func TestManager_UnwrapWithEscrow_AlreadyUnlocked(t *testing.T) {
	m := setupUnlocked(t)
	F := make([]byte, 32)
	rand.Read(F)
	blob, err := m.WrapSDEKForEscrow(F, nil)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if err := m.UnwrapSDEKWithEscrow(blob, F, nil); err != ErrAutoUnlockAlreadyUnlocked {
		t.Fatalf("expected ErrAutoUnlockAlreadyUnlocked, got %v", err)
	}
}

func TestManager_UnwrapWithEscrow_WrongF(t *testing.T) {
	m := setupUnlocked(t)
	F := make([]byte, 32)
	rand.Read(F)
	blob, err := m.WrapSDEKForEscrow(F, nil)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	m.Lock()
	wrongF := make([]byte, 32)
	rand.Read(wrongF)
	if err := m.UnwrapSDEKWithEscrow(blob, wrongF, nil); err != ErrAutoUnlockBlobCorrupt {
		t.Fatalf("expected ErrAutoUnlockBlobCorrupt for wrong F, got %v", err)
	}
}

func TestManager_UnwrapWithEscrow_WrongAAD(t *testing.T) {
	m := setupUnlocked(t)
	F := make([]byte, 32)
	rand.Read(F)
	blob, err := m.WrapSDEKForEscrow(F, []byte("device-abc"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	m.Lock()
	if err := m.UnwrapSDEKWithEscrow(blob, F, []byte("device-xyz")); err != ErrAutoUnlockBlobCorrupt {
		t.Fatalf("expected ErrAutoUnlockBlobCorrupt for wrong AAD, got %v", err)
	}
}

func TestManager_UnwrapWithEscrow_TruncatedBlob(t *testing.T) {
	m := setupUnlocked(t)
	m.Lock()
	F := make([]byte, 32)
	rand.Read(F)
	if err := m.UnwrapSDEKWithEscrow([]byte{1, 2, 3}, F, nil); err != ErrAutoUnlockBlobCorrupt {
		t.Fatalf("expected ErrAutoUnlockBlobCorrupt for truncated blob, got %v", err)
	}
}

func TestManager_EscrowF_LengthValidation(t *testing.T) {
	m := setupUnlocked(t)
	if _, err := m.WrapSDEKForEscrow(make([]byte, 16), nil); err != ErrAutoUnlockBadFLen {
		t.Fatalf("expected ErrAutoUnlockBadFLen on Wrap, got %v", err)
	}
	m.Lock()
	if err := m.UnwrapSDEKWithEscrow([]byte{}, make([]byte, 16), nil); err != ErrAutoUnlockBadFLen {
		t.Fatalf("expected ErrAutoUnlockBadFLen on Unwrap, got %v", err)
	}
}
