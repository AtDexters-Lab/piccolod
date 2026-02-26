package luks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

// OnAdminPasswordRotated rotates keyslot 1 on all data pool devices after
// an admin password change.
func (m *PoolManager) OnAdminPasswordRotated(ctx context.Context, device, oldPass, newPass string) error {
	deviceUUID, err := m.getLUKSUUID(ctx, device)
	if err != nil {
		return fmt.Errorf("read LUKS UUID: %w", err)
	}

	params, err := ReadKDFParams(deviceUUID)
	if err != nil {
		return fmt.Errorf("read KDF params for %s: %w", deviceUUID, err)
	}

	// Write rotation progress for crash recovery.
	progressPath := paths.CoreJoin("crypto", "luks-rotation-progress.json")
	progress := &RotationProgress{
		StartedAt: time.Now().UTC(),
		Total:     1,
		Completed: []string{},
	}
	if err := writeJSON(progressPath, progress); err != nil {
		return fmt.Errorf("write rotation progress: %w", err)
	}

	oldPassphrase := DeriveRecoveryPassphrase(oldPass, params)
	newPassphrase := DeriveRecoveryPassphrase(newPass, params)
	defer SecureZero(oldPassphrase)
	defer SecureZero(newPassphrase)

	if err := m.changeLUKSKeyslot(ctx, device, SlotAdminPassword, oldPassphrase, newPassphrase); err != nil {
		return fmt.Errorf("rotate keyslot %d: %w", SlotAdminPassword, err)
	}

	// Re-backup LUKS header after keyslot change.
	if err := BackupHeader(ctx, m.run, device, deviceUUID); err != nil {
		log.Printf("WARN: failed to re-backup LUKS header after rotation: %v", err)
	}

	progress.Completed = append(progress.Completed, deviceUUID)
	_ = writeJSON(progressPath, progress)
	os.Remove(progressPath)
	return nil
}

// OnRecoveryMnemonicRotated rotates keyslot 2 on all data pool devices after
// a recovery mnemonic change.
func (m *PoolManager) OnRecoveryMnemonicRotated(ctx context.Context, device string, oldMnemonicKey, newMnemonicKey []byte) error {
	deviceUUID, err := m.getLUKSUUID(ctx, device)
	if err != nil {
		return fmt.Errorf("read LUKS UUID: %w", err)
	}

	progressPath := paths.CoreJoin("crypto", "mnemonic-rotation-progress.json")
	progress := &RotationProgress{
		StartedAt: time.Now().UTC(),
		Total:     1,
		Completed: []string{},
	}
	if err := writeJSON(progressPath, progress); err != nil {
		return fmt.Errorf("write mnemonic rotation progress: %w", err)
	}

	// Mnemonic keys are already high-entropy — use directly as LUKS passphrases (no KDF).
	if err := m.changeLUKSKeyslot(ctx, device, SlotRecoveryMnemonic, oldMnemonicKey, newMnemonicKey); err != nil {
		return fmt.Errorf("rotate keyslot %d: %w", SlotRecoveryMnemonic, err)
	}

	if err := BackupHeader(ctx, m.run, device, deviceUUID); err != nil {
		log.Printf("WARN: failed to re-backup LUKS header after mnemonic rotation: %v", err)
	}

	progress.Completed = append(progress.Completed, deviceUUID)
	_ = writeJSON(progressPath, progress)
	os.Remove(progressPath)
	return nil
}

// ResumePasswordRotationIfNeeded checks for and resumes an interrupted admin
// password keyslot rotation.
func (m *PoolManager) ResumePasswordRotationIfNeeded(ctx context.Context, device, currentPass string) error {
	progressPath := paths.CoreJoin("crypto", "luks-rotation-progress.json")
	progress, err := readJSON[RotationProgress](progressPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // No rotation in progress.
	}
	if err != nil {
		return fmt.Errorf("read rotation progress: %w", err)
	}

	log.Printf("WARN: resuming interrupted LUKS keyslot rotation (completed %d/%d)",
		len(progress.Completed), progress.Total)

	deviceUUID, err := m.getLUKSUUID(ctx, device)
	if err != nil {
		return fmt.Errorf("read device UUID: %w", err)
	}

	if contains(progress.Completed, deviceUUID) {
		os.Remove(progressPath)
		return nil
	}

	params, err := ReadKDFParams(deviceUUID)
	if err != nil {
		return fmt.Errorf("read KDF params: %w", err)
	}

	newPassphrase := DeriveRecoveryPassphrase(currentPass, params)
	defer SecureZero(newPassphrase)

	// Test if keyslot already has new passphrase.
	if m.TestPassphrase(ctx, device, SlotAdminPassword, newPassphrase) {
		os.Remove(progressPath)
		return nil
	}

	// Re-create keyslot via pool keyfile.
	log.Printf("WARN: re-creating keyslot %d via pool keyfile", SlotAdminPassword)
	if err := m.rekeySlotViaPoolKeyfile(ctx, device, SlotAdminPassword, newPassphrase); err != nil {
		return fmt.Errorf("rekey keyslot %d: %w", SlotAdminPassword, err)
	}

	os.Remove(progressPath)
	log.Printf("LUKS keyslot rotation recovery complete")
	return nil
}

// ResumeMnemonicRotationIfNeeded checks for and resumes an interrupted mnemonic
// keyslot rotation.
func (m *PoolManager) ResumeMnemonicRotationIfNeeded(ctx context.Context, device string, mnemonicKey []byte) error {
	progressPath := paths.CoreJoin("crypto", "mnemonic-rotation-progress.json")
	progress, err := readJSON[RotationProgress](progressPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read mnemonic rotation progress: %w", err)
	}

	log.Printf("WARN: resuming interrupted mnemonic keyslot rotation (completed %d/%d)",
		len(progress.Completed), progress.Total)

	if mnemonicKey == nil {
		log.Printf("WARN: mnemonic key not available — deferring keyslot 2 recovery")
		return nil
	}

	deviceUUID, err := m.getLUKSUUID(ctx, device)
	if err != nil {
		return fmt.Errorf("read device UUID: %w", err)
	}

	if contains(progress.Completed, deviceUUID) {
		os.Remove(progressPath)
		return nil
	}

	// Mnemonic key is already high-entropy — use directly as LUKS passphrase (no KDF).
	log.Printf("WARN: re-creating keyslot %d via pool keyfile", SlotRecoveryMnemonic)
	if err := m.rekeySlotViaPoolKeyfile(ctx, device, SlotRecoveryMnemonic, mnemonicKey); err != nil {
		return fmt.Errorf("rekey keyslot %d: %w", SlotRecoveryMnemonic, err)
	}

	os.Remove(progressPath)
	log.Printf("mnemonic keyslot rotation recovery complete")
	return nil
}

// changeLUKSKeyslot atomically replaces a keyslot passphrase.
func (m *PoolManager) changeLUKSKeyslot(ctx context.Context, device string, slot int, oldPass, newPass []byte) error {
	oldPath := fmt.Sprintf("%s/rotation-old-%d", tmpfsDir, slot)
	newPath := fmt.Sprintf("%s/rotation-new-%d", tmpfsDir, slot)

	if err := os.MkdirAll(tmpfsDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(oldPath, oldPass, 0o600); err != nil {
		return fmt.Errorf("write old passphrase: %w", err)
	}
	defer os.Remove(oldPath)

	if err := os.WriteFile(newPath, newPass, 0o600); err != nil {
		return fmt.Errorf("write new passphrase: %w", err)
	}
	defer os.Remove(newPath)

	return m.run.Run(ctx, "cryptsetup", "luksChangeKey",
		"--key-file", oldPath,
		"--key-slot", fmt.Sprintf("%d", slot),
		"--new-key-file", newPath,
		device)
}

// rekeySlotViaPoolKeyfile kills a keyslot and re-adds it with a new passphrase,
// using the pool keyfile for authorization.
func (m *PoolManager) rekeySlotViaPoolKeyfile(ctx context.Context, device string, slot int, newPass []byte) error {
	keyfile, err := m.crypto.UnwrapPoolKeyfile()
	if err != nil {
		return fmt.Errorf("unwrap pool keyfile: %w", err)
	}
	defer SecureZero(keyfile)

	if err := os.MkdirAll(tmpfsDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmpfsKeyfilePath, keyfile, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmpfsKeyfilePath)

	// Kill old keyslot.
	if err := m.run.Run(ctx, "cryptsetup", "luksKillSlot",
		"--key-file", tmpfsKeyfilePath,
		"--batch-mode",
		device,
		fmt.Sprintf("%d", slot),
	); err != nil {
		return fmt.Errorf("kill keyslot %d: %w", slot, err)
	}

	// Re-add with new passphrase.
	if err := m.addKeyslot(ctx, device, tmpfsKeyfilePath, newPass, slot); err != nil {
		return fmt.Errorf("re-add keyslot %d: %w", slot, err)
	}

	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

func readJSON[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
