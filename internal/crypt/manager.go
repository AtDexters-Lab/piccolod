package crypt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"piccolod/internal/cryptoutil"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"

	"github.com/cosmos/go-bip39"
	"golang.org/x/crypto/argon2"
)

type kdfParams struct {
	Alg     string `json:"alg"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

type fileState struct {
	SDEK  string    `json:"sdek"`  // base64 ciphertext
	Salt  string    `json:"salt"`  // base64
	Nonce string    `json:"nonce"` // base64
	KDF   kdfParams `json:"kdf"`
	// Optional recovery-key wrapped SDEK
	RKSalt  string `json:"rk_salt,omitempty"`
	RKNonce string `json:"rk_nonce,omitempty"`
	SDEKRK  string `json:"sdek_rk,omitempty"`
}

// Manager controls encryption key setup and unlock lifecycle.
// It intentionally does not manage any mounts; it only keeps the SDEK in memory when unlocked.
type Manager struct {
	path   string
	mu     sync.RWMutex
	sdek   []byte // plaintext SDEK when unlocked
	inited bool

	// OnKeyMaterialChanged is called when any key material (password, recovery,
	// pool keyfile) changes. Used by server integration to emit events.
	OnKeyMaterialChanged func()

	// mnemonicKeyFn supplies the current recovery mnemonic key for LUKS
	// keyslot rotation. Set by server after recovery key generation.
	mnemonicKeyFn func(fn func([]byte) error) error

	// oldMnemonicKeyFn supplies the previous recovery mnemonic key during
	// mnemonic rotation (used to remove old LUKS keyslot).
	oldMnemonicKeyFn func(fn func([]byte) error) error
}

var (
	ErrNotInitialized = errors.New("crypt: not initialized")
	ErrLocked         = errors.New("crypt: locked")
)

func NewManager(stateDir string) (*Manager, error) {
	if stateDir == "" {
		stateDir = paths.CoreRoot()
	}
	dir := filepath.Join(stateDir, "crypto")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{path: filepath.Join(dir, "keyset.json")}
	if _, err := os.Stat(m.path); err == nil {
		m.inited = true
	} else {
		m.inited = false
	}
	return m, nil
}

func (m *Manager) IsInitialized() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inited
}

func (m *Manager) IsLocked() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.inited {
		return false
	}
	return len(m.sdek) == 0
}

// WithSDEK invokes fn with a copy of the unlocked SDEK. Returns ErrNotInitialized
// if setup has not run, or ErrLocked when the manager is locked.
func (m *Manager) WithSDEK(fn func([]byte) error) error {
	if fn == nil {
		return errors.New("callback required")
	}
	m.mu.RLock()
	if !m.inited {
		m.mu.RUnlock()
		return ErrNotInitialized
	}
	if len(m.sdek) == 0 {
		m.mu.RUnlock()
		return ErrLocked
	}
	buf := make([]byte, len(m.sdek))
	copy(buf, m.sdek)
	m.mu.RUnlock()
	defer cryptoutil.SecureZero(buf)
	return fn(buf)
}

func (m *Manager) deriveKey(pw string, salt []byte, k kdfParams) []byte {
	return argon2.IDKey([]byte(pw), salt, k.Time, k.Memory, k.Threads, 32)
}

func (m *Manager) Setup(password string) error {
	if password == "" {
		return errors.New("password required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inited {
		return errors.New("already initialized")
	}

	// KDF defaults — memory is capped dynamically based on system RAM to
	// prevent OOM when cryptsetup spawns its own Argon2id shortly after.
	params := kdfParams{Alg: "argon2id", Time: 3, Memory: cryptoutil.SelectKDFMemoryKiB(), Threads: uint8(selectCryptoParallelism())}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := m.deriveKey(password, salt, params)

	// Generate SDEK and seal with AES-GCM
	sdek := make([]byte, 32)
	if _, err := rand.Read(sdek); err != nil {
		return err
	}
	aead, err := newAES256GCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := aead.Seal(nil, nonce, sdek, nil)

	st := fileState{
		SDEK:  base64.RawStdEncoding.EncodeToString(ct),
		Salt:  base64.RawStdEncoding.EncodeToString(salt),
		Nonce: base64.RawStdEncoding.EncodeToString(nonce),
		KDF:   params,
	}
	b, _ := json.MarshalIndent(&st, "", "  ")
	if err := fsutil.AtomicWriteFile(m.path, b, 0o600); err != nil {
		return err
	}
	m.inited = true
	m.sdek = nil // locked by default after setup
	return nil
}

func (m *Manager) Unlock(password string) error {
	if password == "" {
		return errors.New("password required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.inited {
		return errors.New("not initialized")
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.KDF.Alg != "argon2id" {
		return fmt.Errorf("unsupported kdf: %s", st.KDF.Alg)
	}
	salt, err := base64.RawStdEncoding.DecodeString(st.Salt)
	if err != nil {
		return err
	}
	key := m.deriveKey(password, salt, st.KDF)
	aead, err := newAES256GCM(key)
	if err != nil {
		return err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(st.Nonce)
	if err != nil {
		return err
	}
	ct, err := base64.RawStdEncoding.DecodeString(st.SDEK)
	if err != nil {
		return err
	}
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return errors.New("invalid password")
	}
	m.sdek = pt
	return nil
}

func (m *Manager) Lock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cryptoutil.SecureZero(m.sdek)
	m.sdek = nil
}

func selectCryptoParallelism() int {
	cores := runtime.NumCPU()
	if cores <= 1 {
		return 1
	}
	p := cores - 1
	if p < 1 {
		p = 1
	}
	if p > 8 {
		p = 8
	}
	return p
}

// Rewrap decrypts SDEK with old password and re-seals it with new password.
func (m *Manager) Rewrap(oldPassword, newPassword string) error {
	if oldPassword == "" || newPassword == "" {
		return errors.New("passwords required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.inited {
		return errors.New("not initialized")
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.KDF.Alg != "argon2id" {
		return fmt.Errorf("unsupported kdf: %s", st.KDF.Alg)
	}
	salt, _ := base64.RawStdEncoding.DecodeString(st.Salt)
	keyOld := m.deriveKey(oldPassword, salt, st.KDF)
	aead, _ := newAES256GCM(keyOld)
	nonce, _ := base64.RawStdEncoding.DecodeString(st.Nonce)
	ct, _ := base64.RawStdEncoding.DecodeString(st.SDEK)
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return errors.New("invalid old password")
	}
	// Derive new key with new salt
	newSalt := make([]byte, 16)
	if _, err := rand.Read(newSalt); err != nil {
		return err
	}
	keyNew := m.deriveKey(newPassword, newSalt, st.KDF)
	aead2, _ := newAES256GCM(keyNew)
	newNonce := make([]byte, aead2.NonceSize())
	if _, err := rand.Read(newNonce); err != nil {
		return err
	}
	newCT := aead2.Seal(nil, newNonce, pt, nil)
	st.SDEK = base64.RawStdEncoding.EncodeToString(newCT)
	st.Salt = base64.RawStdEncoding.EncodeToString(newSalt)
	st.Nonce = base64.RawStdEncoding.EncodeToString(newNonce)
	// Save
	nb, _ := json.MarshalIndent(&st, "", "  ")
	if err := fsutil.AtomicWriteFile(m.path, nb, 0o600); err != nil {
		return err
	}
	return nil
}

// RewrapUnlocked reseals the in-memory SDEK with a new password without
// requiring the previous password, assuming the manager is currently unlocked.
func (m *Manager) RewrapUnlocked(newPassword string) error {
	if newPassword == "" {
		return errors.New("password required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.inited {
		return errors.New("not initialized")
	}
	if len(m.sdek) == 0 {
		return ErrLocked
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.KDF.Alg != "argon2id" {
		return fmt.Errorf("unsupported kdf: %s", st.KDF.Alg)
	}
	newSalt := make([]byte, 16)
	if _, err := rand.Read(newSalt); err != nil {
		return err
	}
	keyNew := m.deriveKey(newPassword, newSalt, st.KDF)
	aead, err := newAES256GCM(keyNew)
	if err != nil {
		return err
	}
	newNonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(newNonce); err != nil {
		return err
	}
	newCT := aead.Seal(nil, newNonce, m.sdek, nil)
	st.SDEK = base64.RawStdEncoding.EncodeToString(newCT)
	st.Salt = base64.RawStdEncoding.EncodeToString(newSalt)
	st.Nonce = base64.RawStdEncoding.EncodeToString(newNonce)
	nb, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(m.path, nb, 0o600)
}

// Recovery key management using BIP39 (256-bit entropy, 24 words).

// GenerateRecoveryKey generates fresh recovery-mnemonic words, wraps the
// SDEK with a key derived from them, and persists keyset.json. Returns the
// words AND the key fingerprint (REC-4 anchor). The fingerprint is computed
// inside the write lock from the just-constructed SDEKRK ciphertext: this
// closes the race where a concurrent /generate could rotate past us between
// release and a separate fingerprint read, mismatching words against id.
func (m *Manager) GenerateRecoveryKey(force bool) ([]string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.inited {
		return nil, "", errors.New("not initialized")
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return nil, "", err
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, "", err
	}
	if st.SDEKRK != "" && !force {
		return nil, "", errors.New("recovery key already set")
	}
	// Use plaintext SDEK if already unlocked; otherwise require password via helper
	var sdek []byte
	if len(m.sdek) > 0 {
		sdek = make([]byte, len(m.sdek))
		copy(sdek, m.sdek)
	} else {
		return nil, "", errors.New("unlock required")
	}
	// Generate BIP39 mnemonic (256-bit entropy → 24 words)
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, "", fmt.Errorf("entropy generation failed: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, "", fmt.Errorf("mnemonic generation failed: %w", err)
	}
	words := strings.Split(mnemonic, " ")
	// Derive RK key from mnemonic with new salt
	rkSalt := make([]byte, 16)
	if _, err := rand.Read(rkSalt); err != nil {
		return nil, "", err
	}
	rkParams := st.KDF
	rkKey := m.deriveKey(mnemonic, rkSalt, rkParams)
	aead, _ := newAES256GCM(rkKey)
	rkNonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(rkNonce); err != nil {
		return nil, "", err
	}
	rkCT := aead.Seal(nil, rkNonce, sdek, nil)
	st.RKSalt = base64.RawStdEncoding.EncodeToString(rkSalt)
	st.RKNonce = base64.RawStdEncoding.EncodeToString(rkNonce)
	st.SDEKRK = base64.RawStdEncoding.EncodeToString(rkCT)
	// Save
	nb, _ := json.MarshalIndent(&st, "", "  ")
	if err := fsutil.AtomicWriteFile(m.path, nb, 0o600); err != nil {
		return nil, "", err
	}
	keyID := fingerprintSDEKRK(st.SDEKRK)
	return words, keyID, nil
}

// fingerprintSDEKRK derives the REC-4 key_id anchor from a base64-encoded
// SDEKRK ciphertext. Single source of truth so /generate (computed inside
// the write lock) and RecoveryKeyID() (read-only fingerprint of on-disk
// state) cannot drift.
func fingerprintSDEKRK(sdekrkB64 string) string {
	if sdekrkB64 == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sdekrkB64))
	return hex.EncodeToString(sum[:8])
}

// GenerateRecoveryKeyWithPassword unlocks SDEK using provided password and
// sets recovery wrapper. Returns words + REC-4 key fingerprint (computed
// inside the write lock — same race-closure rationale as GenerateRecoveryKey).
func (m *Manager) GenerateRecoveryKeyWithPassword(password string, force bool) ([]string, string, error) {
	if password == "" {
		return nil, "", errors.New("password required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.inited {
		return nil, "", errors.New("not initialized")
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return nil, "", err
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, "", err
	}
	if st.SDEKRK != "" && !force {
		return nil, "", errors.New("recovery key already set")
	}
	salt, _ := base64.RawStdEncoding.DecodeString(st.Salt)
	key := m.deriveKey(password, salt, st.KDF)
	aead, _ := newAES256GCM(key)
	nonce, _ := base64.RawStdEncoding.DecodeString(st.Nonce)
	ct, _ := base64.RawStdEncoding.DecodeString(st.SDEK)
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, "", errors.New("invalid password")
	}
	// Generate BIP39 mnemonic (256-bit entropy → 24 words)
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, "", fmt.Errorf("entropy generation failed: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, "", fmt.Errorf("mnemonic generation failed: %w", err)
	}
	words := strings.Split(mnemonic, " ")
	rkSalt := make([]byte, 16)
	if _, err := rand.Read(rkSalt); err != nil {
		return nil, "", err
	}
	rkKey := m.deriveKey(mnemonic, rkSalt, st.KDF)
	aead2, _ := newAES256GCM(rkKey)
	rkNonce := make([]byte, aead2.NonceSize())
	if _, err := rand.Read(rkNonce); err != nil {
		return nil, "", err
	}
	rkCT := aead2.Seal(nil, rkNonce, pt, nil)
	st.RKSalt = base64.RawStdEncoding.EncodeToString(rkSalt)
	st.RKNonce = base64.RawStdEncoding.EncodeToString(rkNonce)
	st.SDEKRK = base64.RawStdEncoding.EncodeToString(rkCT)
	nb, _ := json.MarshalIndent(&st, "", "  ")
	if err := fsutil.AtomicWriteFile(m.path, nb, 0o600); err != nil {
		return nil, "", err
	}
	keyID := fingerprintSDEKRK(st.SDEKRK)
	return words, keyID, nil
}

func (m *Manager) HasRecoveryKey() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, err := os.ReadFile(m.path)
	if err != nil {
		return false
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return false
	}
	return st.SDEKRK != ""
}

// RecoveryKeyID returns a stable fingerprint of the current recovery-key
// material — the first 16 hex chars of SHA-256(SDEKRK ciphertext). It changes
// whenever the recovery key rotates (new RKSalt + RKNonce produce new
// SDEKRK ciphertext under the same SDEK plaintext). Empty string when no
// recovery key is set or the keyset file cannot be read.
//
// Used by /recovery-key/generate (returned alongside words) and
// /recovery-key/ack (required in request body) to bind the operator's
// acknowledgement to the specific words they were shown — closes REC-4
// (multi-tab rotation race).
func (m *Manager) RecoveryKeyID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, err := os.ReadFile(m.path)
	if err != nil {
		// Corruption + permanent-empty-fingerprint mode: every /ack will 409.
		// Operator stays in re-prompt loop. Surface at WARN so operators
		// triaging "I cannot ack my recovery key" see the underlying I/O
		// fault rather than chasing the symptom. Distinct from the
		// no-recovery-key-yet case (read succeeds, SDEKRK empty), which is
		// a normal pre-generation state.
		if !os.IsNotExist(err) {
			log.Printf("WARN: RecoveryKeyID: keyset read failed: %v", err)
		}
		return ""
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("WARN: RecoveryKeyID: keyset unmarshal failed: %v", err)
		return ""
	}
	return fingerprintSDEKRK(st.SDEKRK)
}

func (m *Manager) UnlockWithRecoveryKey(words []string) error {
	mn := strings.Join(words, " ")
	if !bip39.IsMnemonicValid(mn) {
		return errors.New("invalid recovery key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.inited {
		return errors.New("not initialized")
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.SDEKRK == "" {
		return errors.New("recovery key not set")
	}
	rkSalt, _ := base64.RawStdEncoding.DecodeString(st.RKSalt)
	rkKey := m.deriveKey(mn, rkSalt, st.KDF)
	aead, _ := newAES256GCM(rkKey)
	rkNonce, _ := base64.RawStdEncoding.DecodeString(st.RKNonce)
	rkCT, _ := base64.RawStdEncoding.DecodeString(st.SDEKRK)
	pt, err := aead.Open(nil, rkNonce, rkCT, nil)
	if err != nil {
		return errors.New("invalid recovery key")
	}
	m.sdek = pt
	return nil
}

// Encrypt encrypts plaintext using AES-GCM with the SDEK.
// Returns ErrLocked if the manager is locked.
func (m *Manager) Encrypt(plaintext []byte) ([]byte, error) {
	var ct []byte
	err := m.WithSDEK(func(sdek []byte) error {
		aead, err := newAES256GCM(sdek)
		if err != nil {
			return err
		}
		nonce := make([]byte, aead.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return err
		}
		// Prepend nonce to ciphertext.
		ct = aead.Seal(nonce, nonce, plaintext, nil)
		return nil
	})
	return ct, err
}

// Decrypt decrypts ciphertext that was encrypted by Encrypt.
// Returns ErrLocked if the manager is locked.
func (m *Manager) Decrypt(ciphertext []byte) ([]byte, error) {
	var pt []byte
	err := m.WithSDEK(func(sdek []byte) error {
		aead, err := newAES256GCM(sdek)
		if err != nil {
			return err
		}
		if len(ciphertext) < aead.NonceSize() {
			return errors.New("ciphertext too short")
		}
		nonce := ciphertext[:aead.NonceSize()]
		ct := ciphertext[aead.NonceSize():]
		var decErr error
		pt, decErr = aead.Open(nil, nonce, ct, nil)
		return decErr
	})
	return pt, err
}

// SetMnemonicKeyCallback sets the callback used to access the current recovery
// mnemonic key material for LUKS keyslot operations.
func (m *Manager) SetMnemonicKeyCallback(fn func(func([]byte) error) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mnemonicKeyFn = fn
}

// SetOldMnemonicKeyCallback sets the callback used to access the previous recovery
// mnemonic key material during mnemonic rotation.
func (m *Manager) SetOldMnemonicKeyCallback(fn func(func([]byte) error) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.oldMnemonicKeyFn = fn
}

// WithMnemonicKey invokes fn with the current recovery mnemonic key material.
func (m *Manager) WithMnemonicKey(fn func([]byte) error) error {
	m.mu.RLock()
	cb := m.mnemonicKeyFn
	m.mu.RUnlock()
	if cb == nil {
		return errors.New("mnemonic key callback not set")
	}
	return cb(fn)
}

// WithOldMnemonicKey invokes fn with the previous recovery mnemonic key material.
func (m *Manager) WithOldMnemonicKey(fn func([]byte) error) error {
	m.mu.RLock()
	cb := m.oldMnemonicKeyFn
	m.mu.RUnlock()
	if cb == nil {
		return errors.New("old mnemonic key callback not set")
	}
	return cb(fn)
}

// notifyKeyMaterialChanged fires the OnKeyMaterialChanged callback if set.
func (m *Manager) notifyKeyMaterialChanged() {
	if m.OnKeyMaterialChanged != nil {
		m.OnKeyMaterialChanged()
	}
}

// readFileBytes reads an entire file into memory.
func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
