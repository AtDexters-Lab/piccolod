package crypt

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func TestSealUnwrapVolumeKey_RoundTrip(t *testing.T) {
	sdek := make([]byte, 32) // AES-256
	if _, err := rand.Read(sdek); err != nil {
		t.Fatal(err)
	}
	plaintext := make([]byte, 64) // 512-bit volume key
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatal(err)
	}

	wrapped, nonce, err := SealVolumeKey(sdek, plaintext)
	if err != nil {
		t.Fatalf("SealVolumeKey: %v", err)
	}
	if wrapped == "" || nonce == "" {
		t.Fatal("expected non-empty wrapped key and nonce")
	}

	got, err := UnwrapVolumeKey(sdek, wrapped, nonce)
	if err != nil {
		t.Fatalf("UnwrapVolumeKey: %v", err)
	}
	if len(got) != len(plaintext) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(plaintext))
	}
	for i := range got {
		if got[i] != plaintext[i] {
			t.Fatalf("byte %d mismatch: got %x, want %x", i, got[i], plaintext[i])
		}
	}
}

func TestSealVolumeKey_UniqueNonces(t *testing.T) {
	sdek := make([]byte, 32)
	if _, err := rand.Read(sdek); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("test-key-material")

	_, nonce1, err := SealVolumeKey(sdek, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	_, nonce2, err := SealVolumeKey(sdek, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if nonce1 == nonce2 {
		t.Error("expected unique nonces across calls")
	}
}

func TestUnwrapVolumeKey_WrongSDEK(t *testing.T) {
	sdek1 := make([]byte, 32)
	sdek2 := make([]byte, 32)
	if _, err := rand.Read(sdek1); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(sdek2); err != nil {
		t.Fatal(err)
	}

	wrapped, nonce, err := SealVolumeKey(sdek1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = UnwrapVolumeKey(sdek2, wrapped, nonce)
	if err == nil {
		t.Fatal("expected error with wrong SDEK")
	}
	if !errors.Is(err, ErrKeyDataCorrupted) {
		t.Errorf("expected ErrKeyDataCorrupted, got %v", err)
	}
}

func TestUnwrapVolumeKey_InvalidNonce(t *testing.T) {
	sdek := make([]byte, 32)
	if _, err := rand.Read(sdek); err != nil {
		t.Fatal(err)
	}

	_, err := UnwrapVolumeKey(sdek, base64.StdEncoding.EncodeToString([]byte("data")), "not-base64!!!")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrKeyDataCorrupted) {
		t.Errorf("expected ErrKeyDataCorrupted, got %v", err)
	}
}

func TestUnwrapVolumeKey_InvalidWrappedKey(t *testing.T) {
	sdek := make([]byte, 32)
	if _, err := rand.Read(sdek); err != nil {
		t.Fatal(err)
	}
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 12)) // GCM nonce is 12 bytes

	_, err := UnwrapVolumeKey(sdek, "not-base64!!!", nonce)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrKeyDataCorrupted) {
		t.Errorf("expected ErrKeyDataCorrupted, got %v", err)
	}
}

func TestUnwrapVolumeKey_EmptyWrappedKey(t *testing.T) {
	sdek := make([]byte, 32)
	if _, err := rand.Read(sdek); err != nil {
		t.Fatal(err)
	}
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 12))

	_, err := UnwrapVolumeKey(sdek, base64.StdEncoding.EncodeToString(nil), nonce)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrKeyDataCorrupted) {
		t.Errorf("expected ErrKeyDataCorrupted, got %v", err)
	}
}

func TestUnwrapVolumeKey_WrongNonceLength(t *testing.T) {
	sdek := make([]byte, 32)
	if _, err := rand.Read(sdek); err != nil {
		t.Fatal(err)
	}
	// GCM nonce should be 12 bytes; use 8 instead.
	shortNonce := base64.StdEncoding.EncodeToString(make([]byte, 8))

	_, err := UnwrapVolumeKey(sdek, base64.StdEncoding.EncodeToString([]byte("data")), shortNonce)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrKeyDataCorrupted) {
		t.Errorf("expected ErrKeyDataCorrupted, got %v", err)
	}
}

func TestSealVolumeKey_InvalidSDEKSize(t *testing.T) {
	// AES requires 16, 24, or 32 byte keys. 17 bytes is invalid.
	badSDEK := make([]byte, 17)
	_, _, err := SealVolumeKey(badSDEK, []byte("data"))
	if err == nil {
		t.Fatal("expected error with invalid SDEK size")
	}
}
