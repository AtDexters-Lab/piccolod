package crypt

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"piccolod/internal/cryptoutil"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

const (
	poolKeyfileSize  = 64 // 512-bit keyfile for LUKS
	luksMasterKeySize = 64 // 512-bit master key for LUKS (key-size 512 / 8)
)

// StorePoolKeyfile encrypts a pool keyfile with the SDEK and writes it to the
// default location under the core root.
func (m *Manager) StorePoolKeyfile(rawKey []byte) error {
	dest := paths.CoreJoin("crypto", "piccolo_data_pool_key.enc")
	return m.StorePoolKeyfileAt(rawKey, dest)
}

// StorePoolKeyfileAt encrypts a pool keyfile and writes it to the given path.
func (m *Manager) StorePoolKeyfileAt(rawKey []byte, destPath string) error {
	ct, err := m.Encrypt(rawKey)
	if err != nil {
		return fmt.Errorf("encrypt pool keyfile: %w", err)
	}

	envelope := struct {
		Version    int    `json:"version"`
		Ciphertext []byte `json:"ciphertext"`
	}{
		Version:    1,
		Ciphertext: ct,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal pool keyfile envelope: %w", err)
	}

	return fsutil.AtomicWriteFile(destPath, data, 0o600)
}

// UnwrapPoolKeyfile reads and decrypts the pool keyfile from the default location.
func (m *Manager) UnwrapPoolKeyfile() ([]byte, error) {
	return m.UnwrapPoolKeyfileFrom(paths.CoreJoin("crypto", "piccolo_data_pool_key.enc"))
}

// StoreLUKSMasterKey encrypts a LUKS master key with the SDEK and writes it
// to the default location under the core root.
func (m *Manager) StoreLUKSMasterKey(masterKey []byte) error {
	dest := paths.CoreJoin("crypto", "luks_master_key.enc")
	return m.StorePoolKeyfileAt(masterKey, dest)
}

// UnwrapLUKSMasterKey reads and decrypts the LUKS master key from the default location.
func (m *Manager) UnwrapLUKSMasterKey() ([]byte, error) {
	return m.UnwrapPoolKeyfileFrom(paths.CoreJoin("crypto", "luks_master_key.enc"))
}

// ensureKeyMaterial returns key material of the given size, generating and persisting
// a new key if it doesn't exist. Only generates when the keyfile is missing. Other
// errors (permission, corruption, decrypt failure) are returned immediately to prevent
// silent key rotation that would make existing LUKS volumes inaccessible.
func (m *Manager) ensureKeyMaterial(size int, unwrap func() ([]byte, error), store func([]byte) error, label string) ([]byte, error) {
	key, err := unwrap()
	if err == nil {
		if len(key) != size {
			cryptoutil.SecureZero(key)
			return nil, fmt.Errorf("%s size %d != expected %d", label, len(key), size)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("unwrap %s: %w", label, err)
	}

	key = make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate %s: %w", label, err)
	}
	if err := store(key); err != nil {
		cryptoutil.SecureZero(key)
		return nil, fmt.Errorf("store %s: %w", label, err)
	}
	return key, nil
}

// EnsureLUKSMasterKey returns the LUKS master key, generating and persisting a new
// one if it doesn't exist.
func (m *Manager) EnsureLUKSMasterKey() ([]byte, error) {
	return m.ensureKeyMaterial(luksMasterKeySize, m.UnwrapLUKSMasterKey, m.StoreLUKSMasterKey, "LUKS master key")
}

// EnsurePoolKeyfile returns the pool keyfile, generating and persisting a new
// one if it doesn't exist.
func (m *Manager) EnsurePoolKeyfile() ([]byte, error) {
	return m.ensureKeyMaterial(poolKeyfileSize, m.UnwrapPoolKeyfile, m.StorePoolKeyfile, "pool keyfile")
}

// UnwrapPoolKeyfileFrom reads and decrypts a pool keyfile from the given path.
func (m *Manager) UnwrapPoolKeyfileFrom(path string) ([]byte, error) {
	data, err := readFileBytes(path)
	if err != nil {
		return nil, fmt.Errorf("read pool keyfile: %w", err)
	}

	var envelope struct {
		Version    int    `json:"version"`
		Ciphertext []byte `json:"ciphertext"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse pool keyfile envelope: %w", err)
	}

	pt, err := m.Decrypt(envelope.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt pool keyfile: %w", err)
	}

	// Return a copy; caller is responsible for zeroing.
	result := make([]byte, len(pt))
	copy(result, pt)
	cryptoutil.SecureZero(pt)
	return result, nil
}
