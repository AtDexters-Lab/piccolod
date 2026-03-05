package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrKeyDataCorrupted indicates stored key material is corrupted or tampered with.
var ErrKeyDataCorrupted = errors.New("crypt: key data corrupted")

// newAES256GCM constructs an AES-256-GCM AEAD from a 32-byte key.
//
// This is a shared construction helper only. It does NOT imply that call sites
// use the same wire format — two distinct crypto envelopes exist in this package:
//
//   - SealVolumeKey/UnwrapVolumeKey: per-volume keys stored as separate base64 fields
//     (base64.StdEncoding, nonce and ciphertext as independent strings).
//   - Manager.Encrypt/Decrypt: pool keyfile and master key stored as nonce-prepended
//     ciphertext (raw bytes, base64.RawStdEncoding in the outer JSON envelope).
//
// Callers are responsible for their own encoding and nonce management.
func newAES256GCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealVolumeKey wraps plaintext key material using AES-256-GCM with the given SDEK.
// Returns base64-encoded wrapped ciphertext and nonce.
func SealVolumeKey(sdek, plaintext []byte) (wrappedKey, nonce string, err error) {
	aead, err := newAES256GCM(sdek)
	if err != nil {
		return "", "", fmt.Errorf("seal volume key: %w", err)
	}
	nonceBytes := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", "", fmt.Errorf("seal volume key: %w", err)
	}
	sealed := aead.Seal(nil, nonceBytes, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed),
		base64.StdEncoding.EncodeToString(nonceBytes),
		nil
}

// UnwrapVolumeKey decrypts a wrapped key using AES-256-GCM with the given SDEK.
// wrappedKey and nonce are base64-encoded strings as returned by SealVolumeKey.
func UnwrapVolumeKey(sdek []byte, wrappedKey, nonce string) ([]byte, error) {
	aead, err := newAES256GCM(sdek)
	if err != nil {
		return nil, fmt.Errorf("unwrap volume key: %w", err)
	}
	nonceBytes, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return nil, fmt.Errorf("%w: decode nonce: %v", ErrKeyDataCorrupted, err)
	}
	if len(nonceBytes) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: invalid nonce length %d (expected %d)",
			ErrKeyDataCorrupted, len(nonceBytes), aead.NonceSize())
	}
	sealed, err := base64.StdEncoding.DecodeString(wrappedKey)
	if err != nil {
		return nil, fmt.Errorf("%w: decode wrapped key: %v", ErrKeyDataCorrupted, err)
	}
	if len(sealed) == 0 {
		return nil, fmt.Errorf("%w: empty wrapped key", ErrKeyDataCorrupted)
	}
	key, err := aead.Open(nil, nonceBytes, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: unwrap failed: %v", ErrKeyDataCorrupted, err)
	}
	return key, nil
}
