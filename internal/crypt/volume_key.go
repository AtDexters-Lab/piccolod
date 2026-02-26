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

// SealVolumeKey wraps plaintext key material using AES-256-GCM with the given SDEK.
// Returns base64-encoded wrapped ciphertext and nonce.
func SealVolumeKey(sdek, plaintext []byte) (wrappedKey, nonce string, err error) {
	block, err := aes.NewCipher(sdek)
	if err != nil {
		return "", "", fmt.Errorf("seal volume key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
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
	block, err := aes.NewCipher(sdek)
	if err != nil {
		return nil, fmt.Errorf("unwrap volume key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
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
