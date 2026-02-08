package luks

import (
	"os"
	"testing"

	"piccolod/internal/state/paths"
)

func TestNewLUKSKDFParams(t *testing.T) {
	params, err := NewLUKSKDFParams("test-uuid-1234")
	if err != nil {
		t.Fatalf("NewLUKSKDFParams: %v", err)
	}

	if params.Version != 1 {
		t.Errorf("Version = %d, want 1", params.Version)
	}
	if params.DeviceUUID != "test-uuid-1234" {
		t.Errorf("DeviceUUID = %q, want test-uuid-1234", params.DeviceUUID)
	}
	if params.Argon2Time != 3 {
		t.Errorf("Argon2Time = %d, want 3", params.Argon2Time)
	}
	if params.Argon2Memory != 512*1024 {
		t.Errorf("Argon2Memory = %d, want %d", params.Argon2Memory, 512*1024)
	}
	if params.Argon2Threads < 1 || params.Argon2Threads > 8 {
		t.Errorf("Argon2Threads = %d, want 1-8", params.Argon2Threads)
	}
	if params.KeyLength != 32 {
		t.Errorf("KeyLength = %d, want 32", params.KeyLength)
	}
	if len(params.SaltAdmin) != 32 {
		t.Errorf("SaltAdmin length = %d, want 32", len(params.SaltAdmin))
	}
	if len(params.SaltMnemonic) != 32 {
		t.Errorf("SaltMnemonic length = %d, want 32", len(params.SaltMnemonic))
	}
}

func TestNewLUKSKDFParams_UniqueSalts(t *testing.T) {
	p1, _ := NewLUKSKDFParams("uuid-1")
	p2, _ := NewLUKSKDFParams("uuid-2")

	if string(p1.SaltAdmin) == string(p2.SaltAdmin) {
		t.Error("expected different admin salts for different UUIDs")
	}
}

func TestDeriveRecoveryPassphrase_Deterministic(t *testing.T) {
	params := &LUKSKDFParams{
		Argon2Time:    1, // fast for testing
		Argon2Memory:  64 * 1024,
		Argon2Threads: 1,
		KeyLength:     32,
		SaltAdmin:     make([]byte, 32),
	}

	p1 := DeriveRecoveryPassphrase("password", params)
	p2 := DeriveRecoveryPassphrase("password", params)

	if string(p1) != string(p2) {
		t.Error("same input should produce same output")
	}
	if len(p1) != 32 {
		t.Errorf("passphrase length = %d, want 32", len(p1))
	}
}

func TestDeriveRecoveryPassphrase_DifferentPasswords(t *testing.T) {
	params := &LUKSKDFParams{
		Argon2Time:    1,
		Argon2Memory:  64 * 1024,
		Argon2Threads: 1,
		KeyLength:     32,
		SaltAdmin:     make([]byte, 32),
	}

	p1 := DeriveRecoveryPassphrase("password1", params)
	p2 := DeriveRecoveryPassphrase("password2", params)

	if string(p1) == string(p2) {
		t.Error("different passwords should produce different outputs")
	}
}

func TestDeriveMnemonicRecoveryPassphrase(t *testing.T) {
	params := &LUKSKDFParams{
		Argon2Time:    1,
		Argon2Memory:  64 * 1024,
		Argon2Threads: 1,
		KeyLength:     32,
		SaltMnemonic:  make([]byte, 32),
	}

	p1 := DeriveMnemonicRecoveryPassphrase([]byte("mnemonic-key"), params)
	if len(p1) != 32 {
		t.Errorf("passphrase length = %d, want 32", len(p1))
	}
}

func TestKDFParamsPath(t *testing.T) {
	paths.SetCoreRootForTest(t, "/test-core")
	got := KDFParamsPath("abc-123")
	want := "/test-core/crypto/luks-kdf-params/abc-123.json"
	if got != want {
		t.Errorf("KDFParamsPath = %q, want %q", got, want)
	}
}

func TestWriteReadKDFParams(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)
	_ = core

	params, _ := NewLUKSKDFParams("test-uuid")
	if err := WriteKDFParams(params); err != nil {
		t.Fatalf("WriteKDFParams: %v", err)
	}

	read, err := ReadKDFParams("test-uuid")
	if err != nil {
		t.Fatalf("ReadKDFParams: %v", err)
	}

	if read.DeviceUUID != params.DeviceUUID {
		t.Errorf("DeviceUUID = %q, want %q", read.DeviceUUID, params.DeviceUUID)
	}
	if read.Argon2Time != params.Argon2Time {
		t.Errorf("Argon2Time mismatch")
	}
}

func TestReadKDFParams_Missing(t *testing.T) {
	paths.SetRootsForTest(t)
	_, err := ReadKDFParams("nonexistent-uuid")
	if err == nil {
		t.Error("expected error for missing KDF params")
	}
}

func TestMapperName(t *testing.T) {
	if got := MapperName(0); got != "piccolo_data_pool_0" {
		t.Errorf("MapperName(0) = %q", got)
	}
	if got := MapperPath(0); got != "/dev/mapper/piccolo_data_pool_0" {
		t.Errorf("MapperPath(0) = %q", got)
	}
}

func TestUnlockError(t *testing.T) {
	err := &UnlockError{
		Phase:   "pool_keyfile",
		Cause:   os.ErrNotExist,
		Message: "pool keyfile missing",
	}
	if err.Error() == "" {
		t.Error("expected non-empty error string")
	}
	if err.Unwrap() != os.ErrNotExist {
		t.Error("expected Unwrap to return cause")
	}
}

func TestSecureZero(t *testing.T) {
	data := []byte{1, 2, 3, 4, 5}
	SecureZero(data)
	for i, b := range data {
		if b != 0 {
			t.Errorf("data[%d] = %d, want 0", i, b)
		}
	}
}
