package luks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
)

// BackupHeader creates a LUKS header backup for the given device.
func BackupHeader(ctx context.Context, run runner.CommandRunner, device, deviceUUID string) error {
	backupDir := paths.CoreJoin("crypto", "luks-header-backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("create header backup dir: %w", err)
	}

	backupPath := filepath.Join(backupDir, deviceUUID+".bin")
	if err := run.Run(ctx, "cryptsetup", "luksHeaderBackup",
		device,
		"--header-backup-file", backupPath,
	); err != nil {
		return fmt.Errorf("cryptsetup luksHeaderBackup: %w", err)
	}
	return nil
}

// RestoreHeader restores a LUKS header from backup.
func RestoreHeader(ctx context.Context, run runner.CommandRunner, device, deviceUUID string) error {
	backupPath := filepath.Join(paths.CoreJoin("crypto", "luks-header-backups"), deviceUUID+".bin")
	return run.Run(ctx, "cryptsetup", "luksHeaderRestore",
		device,
		"--header-backup-file", backupPath,
		"--batch-mode",
	)
}

// RestoreHeaderByDevice attempts to restore a LUKS header when the device UUID
// is unreadable. Picks the first .bin file in the header backup directory.
func RestoreHeaderByDevice(ctx context.Context, run runner.CommandRunner, device string) error {
	backupDir := paths.CoreJoin("crypto", "luks-header-backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return fmt.Errorf("no header backup directory: %w", err)
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bin") {
			uuid := strings.TrimSuffix(entry.Name(), ".bin")
			return RestoreHeader(ctx, run, device, uuid)
		}
	}
	return fmt.Errorf("no .bin header backup found in %s", backupDir)
}
