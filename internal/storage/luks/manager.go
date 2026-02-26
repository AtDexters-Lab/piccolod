package luks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"piccolod/internal/crypt"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
)

const (
	// tmpfsKeyfilePath is the tmpfs location for materializing the pool keyfile.
	tmpfsKeyfilePath = "/run/piccolo/piccolo_data_pool_key"
	tmpfsDir         = "/run/piccolo"
)

// PoolManager manages LUKS2 encrypted data pools.
type PoolManager struct {
	run    runner.CommandRunner
	crypto *crypt.Manager
}

// NewPoolManager creates a LUKS pool manager.
func NewPoolManager(run runner.CommandRunner, crypto *crypt.Manager) *PoolManager {
	return &PoolManager{run: run, crypto: crypto}
}

// InitializeLUKS formats a LUKS2 device and enrolls all three keyslots.
func (m *PoolManager) InitializeLUKS(ctx context.Context, device, adminPassword string, mnemonicKey []byte) error {
	// Step 1: Generate pool keyfile.
	kf, err := crypt.GeneratePoolKeyfile()
	if err != nil {
		return fmt.Errorf("generate pool keyfile: %w", err)
	}
	defer SecureZero(kf.KeyData)

	// Materialize keyfile to tmpfs for cryptsetup.
	if err := os.MkdirAll(tmpfsDir, 0o700); err != nil {
		return fmt.Errorf("create tmpfs dir: %w", err)
	}
	if err := os.WriteFile(tmpfsKeyfilePath, kf.KeyData, 0o600); err != nil {
		return fmt.Errorf("write pool keyfile to tmpfs: %w", err)
	}
	defer os.Remove(tmpfsKeyfilePath)

	// Step 2: luksFormat with pool keyfile (keyslot 0).
	if err := m.run.Run(ctx, "cryptsetup", "luksFormat",
		"--type", "luks2",
		"--batch-mode",
		"--label", "piccolo-data",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--hash", "sha256",
		"--pbkdf", "pbkdf2",
		"--pbkdf-force-iterations", "1000",
		"--key-file", tmpfsKeyfilePath,
		device,
	); err != nil {
		return fmt.Errorf("cryptsetup luksFormat: %w", err)
	}

	// Step 3: Store encrypted pool keyfile in control plane.
	if err := m.crypto.StorePoolKeyfile(kf.KeyData); err != nil {
		return fmt.Errorf("store pool keyfile: %w", err)
	}

	// Step 4: Read device UUID for KDF params.
	deviceUUID, err := m.getLUKSUUID(ctx, device)
	if err != nil {
		return fmt.Errorf("read LUKS UUID: %w", err)
	}

	// Step 5: Generate and persist KDF params.
	params, err := NewLUKSKDFParams(deviceUUID)
	if err != nil {
		return fmt.Errorf("generate KDF params: %w", err)
	}
	if err := WriteKDFParams(params); err != nil {
		return fmt.Errorf("write KDF params: %w", err)
	}

	// Step 6: Add admin password keyslot (slot 1).
	adminPassphrase := DeriveRecoveryPassphrase(adminPassword, params)
	defer SecureZero(adminPassphrase)
	if err := m.addKeyslot(ctx, device, tmpfsKeyfilePath, adminPassphrase, SlotAdminPassword); err != nil {
		return fmt.Errorf("add admin keyslot: %w", err)
	}

	// Step 7: Add mnemonic recovery keyslot (slot 2), if key available.
	// Key may be nil during initial setup; slot is added when recovery key is generated.
	if mnemonicKey != nil {
		// Mnemonic key is already high-entropy — use directly as LUKS passphrase (no KDF).
		if err := m.addKeyslot(ctx, device, tmpfsKeyfilePath, mnemonicKey, SlotRecoveryMnemonic); err != nil {
			return fmt.Errorf("add mnemonic keyslot: %w", err)
		}
	}

	// Step 8: Backup LUKS header.
	if err := BackupHeader(ctx, m.run, device, deviceUUID); err != nil {
		log.Printf("WARN: failed to backup LUKS header after init: %v", err)
	}

	log.Printf("LUKS initialized on %s (UUID: %s)", device, deviceUUID)
	return nil
}

// Unlock opens a LUKS device using the unlock cascade:
// pool keyfile → admin password → header restore + retry → error.
func (m *PoolManager) Unlock(ctx context.Context, device, adminPassword string) error {
	mapperName := MapperName(0)
	mapperPath := MapperPath(0)

	// Handle stale mapper from unclean shutdown.
	if _, err := os.Stat(mapperPath); err == nil {
		if m.isMapperActive(ctx, mapperName) {
			log.Printf("LUKS device already open: %s", mapperName)
			return nil
		}
		// Stale mapper — close before re-opening.
		_ = m.run.Run(ctx, "cryptsetup", "close", mapperName)
	}

	// Attempt 1: Pool keyfile (keyslot 0).
	poolKeyErr := m.unlockWithPoolKeyfile(ctx, device, mapperName)
	if poolKeyErr == nil {
		log.Printf("LUKS unlocked via pool keyfile (keyslot 0)")
		return nil
	}
	log.Printf("WARN: pool keyfile unlock failed: %v", poolKeyErr)

	// Attempt 2: Admin password (keyslot 1).
	if adminPassword != "" {
		adminErr := m.unlockWithAdminPassword(ctx, device, mapperName, adminPassword)
		if adminErr == nil {
			log.Printf("WARN: LUKS unlocked via admin password (keyslot 1) — pool keyfile may need repair")
			return nil
		}
		log.Printf("WARN: admin password unlock failed: %v", adminErr)
	}

	// Attempt 3: Header recovery.
	deviceUUID, uuidErr := m.getLUKSUUID(ctx, device)
	if uuidErr != nil {
		// Header unreadable — try restoring by device.
		if restoreErr := RestoreHeaderByDevice(ctx, m.run, device); restoreErr == nil {
			if retryErr := m.unlockWithPoolKeyfile(ctx, device, mapperName); retryErr == nil {
				log.Printf("LUKS unlocked after header restore via pool keyfile")
				return nil
			}
		}
	} else {
		if restoreErr := RestoreHeader(ctx, m.run, device, deviceUUID); restoreErr == nil {
			if retryErr := m.unlockWithPoolKeyfile(ctx, device, mapperName); retryErr == nil {
				log.Printf("LUKS unlocked after header restore via pool keyfile")
				return nil
			}
		}
	}

	return &UnlockError{
		Phase:   "all_keyslots_exhausted",
		Cause:   poolKeyErr,
		Message: "All unlock paths failed. Recovery mnemonic may be needed (keyslot 2).",
	}
}

// Lock closes a LUKS device: unmount → cryptsetup close.
// Deprecated: pool-level LUKS is no longer used. Per-volume LUKS handled in M3.
func (m *PoolManager) Lock(ctx context.Context, mountPoint string) error {
	mapperName := MapperName(0)

	if err := m.run.Run(ctx, "umount", mountPoint); err != nil {
		return fmt.Errorf("unmount %s: %w", mountPoint, err)
	}

	if err := m.run.Run(ctx, "cryptsetup", "close", mapperName); err != nil {
		return fmt.Errorf("cryptsetup close %s: %w", mapperName, err)
	}

	log.Printf("storage locked: %s", mapperName)
	return nil
}

// MountDataPool mounts a LUKS device at the given mount point.
// Deprecated: pool-level LUKS is no longer used. Per-volume LUKS handled in M3.
func (m *PoolManager) MountDataPool(ctx context.Context, mountPoint string) error {
	mapperPath := MapperPath(0)

	if err := os.MkdirAll(mountPoint, 0o711); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}

	if m.isMounted(ctx, mountPoint) {
		log.Printf("data pool already mounted at %s", mountPoint)
		return nil
	}

	if err := m.run.Run(ctx, "mount", mapperPath, mountPoint); err != nil {
		return fmt.Errorf("mount %s at %s: %w", mapperPath, mountPoint, err)
	}
	return nil
}

// unlockWithPoolKeyfile attempts to open the LUKS device using the pool keyfile.
func (m *PoolManager) unlockWithPoolKeyfile(ctx context.Context, device, mapperName string) error {
	keyfile, err := m.crypto.UnwrapPoolKeyfile()
	if err != nil {
		return fmt.Errorf("unwrap pool keyfile: %w", err)
	}
	defer SecureZero(keyfile)

	if err := os.MkdirAll(tmpfsDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmpfsKeyfilePath, keyfile, 0o600); err != nil {
		return fmt.Errorf("write keyfile to tmpfs: %w", err)
	}
	defer os.Remove(tmpfsKeyfilePath)

	return m.run.Run(ctx, "cryptsetup", "open",
		"--type", "luks2",
		"--key-file", tmpfsKeyfilePath,
		device, mapperName)
}

// unlockWithAdminPassword attempts to open the LUKS device using the admin password.
func (m *PoolManager) unlockWithAdminPassword(ctx context.Context, device, mapperName, adminPassword string) error {
	deviceUUID, err := m.getLUKSUUID(ctx, device)
	if err != nil {
		return fmt.Errorf("read LUKS UUID: %w", err)
	}

	params, err := ReadKDFParams(deviceUUID)
	if err != nil {
		return fmt.Errorf("read KDF params for %s: %w", deviceUUID, err)
	}

	passphrase := DeriveRecoveryPassphrase(adminPassword, params)
	defer SecureZero(passphrase)

	passPath := tmpfsDir + "/unlock-admin-passphrase"
	if err := os.WriteFile(passPath, passphrase, 0o600); err != nil {
		return err
	}
	defer os.Remove(passPath)

	return m.run.Run(ctx, "cryptsetup", "open",
		"--type", "luks2",
		"--key-file", passPath,
		device, mapperName)
}

// addKeyslot adds a new keyslot using the pool keyfile as authorization.
func (m *PoolManager) addKeyslot(ctx context.Context, device, keyfilePath string, passphrase []byte, slot int) error {
	passPath := fmt.Sprintf("%s/keyslot-%d-passphrase", tmpfsDir, slot)
	if err := os.WriteFile(passPath, passphrase, 0o600); err != nil {
		return fmt.Errorf("write keyslot passphrase: %w", err)
	}
	defer os.Remove(passPath)

	return m.run.Run(ctx, "cryptsetup", "luksAddKey",
		"--key-file", keyfilePath,
		"--key-slot", fmt.Sprintf("%d", slot),
		device,
		passPath,
	)
}

// getLUKSUUID reads the LUKS device UUID.
func (m *PoolManager) getLUKSUUID(ctx context.Context, device string) (string, error) {
	out, err := m.run.RunWithOutput(ctx, "cryptsetup", "luksUUID", device)
	if err != nil {
		return "", fmt.Errorf("cryptsetup luksUUID: %w", err)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", errors.New("empty LUKS UUID")
	}
	return uuid, nil
}

// isMapperActive checks if a device mapper is active via cryptsetup status.
func (m *PoolManager) isMapperActive(ctx context.Context, mapperName string) bool {
	return m.run.Run(ctx, "cryptsetup", "status", mapperName) == nil
}

// isMounted checks if a path has a mounted filesystem.
func (m *PoolManager) isMounted(ctx context.Context, mountPoint string) bool {
	return m.run.Run(ctx, "mountpoint", "-q", mountPoint) == nil
}

// TestPassphrase checks if a passphrase is valid for a given keyslot.
func (m *PoolManager) TestPassphrase(ctx context.Context, device string, slot int, passphrase []byte) bool {
	passPath := tmpfsDir + "/test-passphrase"
	if err := os.MkdirAll(tmpfsDir, 0o700); err != nil {
		return false
	}
	if err := os.WriteFile(passPath, passphrase, 0o600); err != nil {
		return false
	}
	defer os.Remove(passPath)

	return m.run.Run(ctx, "cryptsetup", "open", "--test-passphrase",
		"--type", "luks2",
		"--key-file", passPath,
		"--key-slot", fmt.Sprintf("%d", slot),
		device) == nil
}

// HasLUKSHeader checks if the device has a valid LUKS header.
// Returns (true, nil) if LUKS header exists, (false, nil) if not,
// or (false, err) if the check itself failed (missing binary, I/O error).
func (m *PoolManager) HasLUKSHeader(ctx context.Context, device string) (bool, error) {
	err := m.run.Run(ctx, "cryptsetup", "isLuks", device)
	if err == nil {
		return true, nil
	}
	// cryptsetup isLuks returns exit code 1 for non-LUKS devices.
	// Only treat that specific case as "no header". Any other error
	// (I/O, permission, missing binary) must be propagated to prevent
	// callers from mistakenly falling back to luksFormat.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("cryptsetup isLuks: %w", err)
}

// DetectOrphanedLUKSHeader checks if a device has a LUKS header but no pool keyfile.
// An orphan means a previous init wrote the LUKS header but crashed before persisting
// the pool keyfile. A fresh partition (no LUKS header) is NOT orphaned.
// Returns false on any error from the header check (fail-safe).
func (m *PoolManager) DetectOrphanedLUKSHeader(ctx context.Context, device string) bool {
	hasHeader, err := m.HasLUKSHeader(ctx, device)
	if err != nil {
		log.Printf("WARN: HasLUKSHeader check failed for %s: %v", device, err)
		return false // fail-safe: don't treat as orphaned on error
	}
	if !hasHeader {
		return false // no LUKS header → not orphaned
	}
	// LUKS header exists but no pool keyfile → orphaned.
	poolKeyPath := paths.CoreJoin("crypto", "piccolo_data_pool_key.enc")
	_, err = os.Stat(poolKeyPath)
	return errors.Is(err, os.ErrNotExist)
}

// WipeLUKSHeader erases the LUKS header on a device. Only safe when no data
// has been written (orphaned header from crashed init).
func (m *PoolManager) WipeLUKSHeader(ctx context.Context, device string) error {
	return m.run.Run(ctx, "cryptsetup", "luksErase", "--batch-mode", device)
}
