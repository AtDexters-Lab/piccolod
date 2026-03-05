package crypt

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
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
	words1, err := m.GenerateRecoveryKey(false)
	if err != nil {
		t.Fatalf("GenerateRecoveryKey first: %v", err)
	}
	if len(words1) != 24 {
		t.Fatalf("expected 24 words, got %d", len(words1))
	}
	if _, err := m.GenerateRecoveryKey(false); err == nil {
		t.Fatalf("expected error when regenerating without force")
	}
	words2, err := m.GenerateRecoveryKey(true)
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
	words, err := m.GenerateRecoveryKey(false)
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
