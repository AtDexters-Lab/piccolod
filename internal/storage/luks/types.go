package luks

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"piccolod/internal/cryptoutil"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"

	"golang.org/x/crypto/argon2"
)

const (
	// Keyslot assignments.
	SlotPoolKeyfile     = 0
	SlotAdminPassword   = 1
	SlotRecoveryMnemonic = 2

	// MapperPrefix is the device mapper name prefix.
	MapperPrefix = "piccolo_data_pool"
)

// MapperName returns the device mapper name for a data pool index.
func MapperName(index int) string {
	return fmt.Sprintf("%s_%d", MapperPrefix, index)
}

// MapperPath returns the /dev/mapper/ path for a data pool index.
func MapperPath(index int) string {
	return "/dev/mapper/" + MapperName(index)
}

// LUKSKDFParams stores the Argon2id parameters used for deriving LUKS keyslot
// passphrases. Persisted outside gocryptfs so they're always readable.
type LUKSKDFParams struct {
	Version       int    `json:"version"`
	DeviceUUID    string `json:"device_uuid"`
	Argon2Time    uint32 `json:"argon2_time"`
	Argon2Memory  uint32 `json:"argon2_memory"`  // KiB
	Argon2Threads uint8  `json:"argon2_threads"`
	KeyLength     uint32 `json:"key_length"`      // bytes
	SaltAdmin     []byte `json:"salt_admin"`      // 32 bytes
	SaltMnemonic  []byte `json:"salt_mnemonic"`   // 32 bytes
	CreatedAt     string `json:"created_at"`
}

// NewLUKSKDFParams creates KDF params with sensible defaults for the given device.
func NewLUKSKDFParams(deviceUUID string) (*LUKSKDFParams, error) {
	saltAdmin := make([]byte, 32)
	if _, err := rand.Read(saltAdmin); err != nil {
		return nil, fmt.Errorf("generate admin salt: %w", err)
	}
	saltMnemonic := make([]byte, 32)
	if _, err := rand.Read(saltMnemonic); err != nil {
		return nil, fmt.Errorf("generate mnemonic salt: %w", err)
	}

	threads := runtime.NumCPU() - 1
	if threads < 1 {
		threads = 1
	}
	if threads > 8 {
		threads = 8
	}

	return &LUKSKDFParams{
		Version:       1,
		DeviceUUID:    deviceUUID,
		Argon2Time:    3,
		Argon2Memory:  cryptoutil.SelectKDFMemoryKiB(),
		Argon2Threads: uint8(threads),
		KeyLength:     32,
		SaltAdmin:     saltAdmin,
		SaltMnemonic:  saltMnemonic,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// DeriveRecoveryPassphrase derives the LUKS keyslot 1 passphrase from an admin password.
func DeriveRecoveryPassphrase(adminPassword string, params *LUKSKDFParams) []byte {
	return argon2.IDKey(
		[]byte(adminPassword),
		params.SaltAdmin,
		params.Argon2Time,
		params.Argon2Memory,
		params.Argon2Threads,
		params.KeyLength,
	)
}

// DeriveMnemonicRecoveryPassphrase derives the LUKS keyslot 2 passphrase from
// a mnemonic-derived key.
func DeriveMnemonicRecoveryPassphrase(mnemonicKey []byte, params *LUKSKDFParams) []byte {
	return argon2.IDKey(
		mnemonicKey,
		params.SaltMnemonic,
		params.Argon2Time,
		params.Argon2Memory,
		params.Argon2Threads,
		params.KeyLength,
	)
}

// KDFParamsPath returns the file path for persisted KDF params.
func KDFParamsPath(deviceUUID string) string {
	return paths.CoreJoin("crypto", "luks-kdf-params", deviceUUID+".json")
}

// ReadKDFParams loads KDF params from disk for the given device UUID.
func ReadKDFParams(deviceUUID string) (*LUKSKDFParams, error) {
	data, err := os.ReadFile(KDFParamsPath(deviceUUID))
	if err != nil {
		return nil, err
	}
	var params LUKSKDFParams
	if err := json.Unmarshal(data, &params); err != nil {
		return nil, fmt.Errorf("parse KDF params: %w", err)
	}
	return &params, nil
}

// WriteKDFParams persists KDF params to disk.
func WriteKDFParams(params *LUKSKDFParams) error {
	dir := filepath.Dir(KDFParamsPath(params.DeviceUUID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(KDFParamsPath(params.DeviceUUID), data, 0o600)
}

// RotationProgress tracks multi-device keyslot rotation for crash recovery.
type RotationProgress struct {
	StartedAt time.Time `json:"started_at"`
	Total     int       `json:"total"`
	Completed []string  `json:"completed"` // device UUIDs
}

// UnlockError provides structured unlock failure information.
type UnlockError struct {
	Phase   string // "pool_keyfile", "admin_password", "header_recovery", "all_keyslots_exhausted"
	Cause   error
	Message string
}

func (e *UnlockError) Error() string {
	return fmt.Sprintf("unlock failed at %s: %s (%v)", e.Phase, e.Message, e.Cause)
}

func (e *UnlockError) Unwrap() error {
	return e.Cause
}

// PoolDevice represents a LUKS data pool device.
type PoolDevice struct {
	Path string // e.g., /dev/sda3
	UUID string // LUKS device UUID
}

// SecureZero overwrites a byte slice with zeroes.
func SecureZero(b []byte) {
	cryptoutil.SecureZero(b)
}
