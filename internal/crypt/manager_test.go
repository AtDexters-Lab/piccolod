package crypt

import (
	"strings"
	"testing"
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
