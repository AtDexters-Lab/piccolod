package persistence

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"piccolod/internal/runner"
)

// cryptsetupExitDeviceExists is the exit code returned by cryptsetup when the
// mapper device already exists (e.g. from a prior crash or TOCTOU race).
const cryptsetupExitDeviceExists = 5

// LUKSLoopVolume manages a LUKS2-encrypted loop file. Used for the control
// plane volume (SQLite + state) which doesn't need the full block device
// stack (no thin LV, no NBD, no DRBD).
type LUKSLoopVolume struct {
	run            runner.CommandRunner
	tmpfsDir       string                              // directory for ephemeral key material (default: /run/piccolo)
	mapperExists func(mapper string) bool          // checks if /dev/mapper/<mapper> exists
	isMountPoint func(path string) (bool, error)  // checks if path is a mount point
}

func defaultMapperExists(mapper string) bool {
	_, err := os.Stat("/dev/mapper/" + mapper)
	return err == nil
}

// NewLUKSLoopVolume creates a LUKS loop volume manager.
func NewLUKSLoopVolume(run runner.CommandRunner) *LUKSLoopVolume {
	return &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       "/run/piccolo",
		mapperExists:   defaultMapperExists,
		isMountPoint: isMountPoint,
	}
}

// NewLUKSLoopVolumeWithTmpfs creates a LUKS loop volume manager with a custom
// tmpfs directory. Used in tests where /run is not writable.
func NewLUKSLoopVolumeWithTmpfs(run runner.CommandRunner, tmpfsDir string) *LUKSLoopVolume {
	return &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       tmpfsDir,
		mapperExists:   defaultMapperExists,
		isMountPoint: isMountPoint,
	}
}

// mapperName derives a device mapper name from the loop file path.
// e.g., /path/to/control-plane.luks → piccolo-loop-control-plane
func mapperName(loopFile string) string {
	base := filepath.Base(loopFile)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return "piccolo-loop-" + name
}

// Init creates a new LUKS2-encrypted loop volume.
// Creates a sparse file, formats it with LUKS2, and creates an ext4 filesystem.
// keyMaterial is a raw key (typically 64 bytes) used as the LUKS passphrase.
func (v *LUKSLoopVolume) Init(ctx context.Context, loopFile string, sizeBytes int64, keyMaterial []byte) error {
	mapper := mapperName(loopFile)

	// Clean up stale mapper from prior crash — hard error if it can't be removed,
	// because the loop device cannot be safely reformatted while a mapper holds it.
	if v.mapperExists(mapper) {
		log.Printf("stale mapper %s detected during init, closing", mapper)
		if err := v.run.Run(ctx, "cryptsetup", "close", mapper); err != nil {
			return fmt.Errorf("close stale mapper %s before reinit: %w", mapper, err)
		}
	}

	// Clean up stale loop device from prior crash (best-effort).
	// Safe to proceed even if detach fails: the mapper (most dangerous holder) is
	// already closed above, and attachLoop will reuse the existing device.
	if existing, err := v.findLoop(ctx, loopFile); err == nil && existing != "" {
		if err := v.detachLoop(ctx, existing); err != nil {
			log.Printf("stale loop %s detach failed (will reuse): %v", existing, err)
		}
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(loopFile), 0o700); err != nil {
		return fmt.Errorf("create loop file dir: %w", err)
	}

	// Create sparse file.
	if err := v.run.Run(ctx, "truncate", "-s", fmt.Sprintf("%d", sizeBytes), loopFile); err != nil {
		return fmt.Errorf("create sparse loop file: %w", err)
	}

	// Attach loop device.
	loopDev, err := v.attachLoop(ctx, loopFile)
	if err != nil {
		return err
	}
	defer v.detachLoop(ctx, loopDev)

	// Write key to tmpfs for cryptsetup.
	keyPath, cleanup, err := v.writeKeyToTmpfs(keyMaterial)
	if err != nil {
		return err
	}
	defer cleanup()

	// LUKS format.
	if err := v.run.Run(ctx, "cryptsetup", "luksFormat",
		"--type", "luks2",
		"--batch-mode",
		"--label", mapper,
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--hash", "sha256",
		"--pbkdf", "pbkdf2",
		"--pbkdf-force-iterations", "1000",
		"--key-file", keyPath,
		loopDev,
	); err != nil {
		return fmt.Errorf("cryptsetup luksFormat: %w", err)
	}

	// Open, mkfs, close.
	if err := v.run.Run(ctx, "cryptsetup", "open",
		"--type", "luks2",
		"--allow-discards",
		"--key-file", keyPath,
		loopDev, mapper,
	); err != nil {
		return fmt.Errorf("cryptsetup open for mkfs: %w", err)
	}

	mapperPath := "/dev/mapper/" + mapper
	mkfsErr := v.run.Run(ctx, "mkfs.ext4", "-F", "-m", "1", mapperPath)

	// Always close after mkfs, regardless of error.
	if err := v.run.Run(ctx, "cryptsetup", "close", mapper); err != nil && mkfsErr == nil {
		return fmt.Errorf("cryptsetup close after mkfs: %w", err)
	}
	if mkfsErr != nil {
		return fmt.Errorf("mkfs.ext4: %w", mkfsErr)
	}

	return nil
}

// Open attaches, unlocks, and mounts a LUKS loop volume.
// Idempotent: if the volume is already open and mounted, returns nil.
// Recovers from stale state left by a prior crash.
func (v *LUKSLoopVolume) Open(ctx context.Context, loopFile string, keyMaterial []byte, mountDir string) error {
	mapper := mapperName(loopFile)
	mapperPath := "/dev/mapper/" + mapper

	// Fast path: mapper exists and already mounted — idempotent no-op.
	if v.mapperExists(mapper) {
		mounted, err := v.isMountPoint(mountDir)
		if err == nil && mounted {
			return nil
		}

		// Mapper exists but not mounted — try to recover by mounting directly.
		log.Printf("stale mapper %s detected, attempting recovery mount", mapper)
		if mkdirErr := os.MkdirAll(mountDir, 0o700); mkdirErr == nil {
			if mountErr := v.run.Run(ctx, "mount", "-t", "ext4", mapperPath, mountDir); mountErr == nil {
				return nil
			} else {
				log.Printf("recovery mount %s on %s failed: %v", mapperPath, mountDir, mountErr)
			}
		}

		// Stale mapper — close it and fall through to normal path.
		log.Printf("stale mapper %s recovery mount failed, closing for fresh open", mapper)
		if err := v.run.Run(ctx, "cryptsetup", "close", mapper); err != nil {
			log.Printf("cryptsetup close stale mapper %s failed: %v", mapper, err)
		}
	}

	// Normal path: attach loop (idempotent), unlock, mount.
	loopDev, err := v.attachLoop(ctx, loopFile)
	if err != nil {
		return err
	}

	keyPath, cleanup, err := v.writeKeyToTmpfs(keyMaterial)
	if err != nil {
		v.detachLoop(ctx, loopDev)
		return err
	}
	defer cleanup()

	if err := v.run.Run(ctx, "cryptsetup", "open",
		"--type", "luks2",
		"--allow-discards",
		"--key-file", keyPath,
		loopDev, mapper,
	); err != nil {
		// Tolerate exit code 5 (device already exists) — TOCTOU race with mapper check.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != cryptsetupExitDeviceExists {
			v.detachLoop(ctx, loopDev)
			return fmt.Errorf("cryptsetup open: %w", err)
		}
	}

	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		v.run.Run(ctx, "cryptsetup", "close", mapper)
		v.detachLoop(ctx, loopDev)
		return fmt.Errorf("create mount dir: %w", err)
	}

	if err := v.run.Run(ctx, "mount", "-t", "ext4", mapperPath, mountDir); err != nil {
		v.run.Run(ctx, "cryptsetup", "close", mapper)
		v.detachLoop(ctx, loopDev)
		return fmt.Errorf("mount: %w", err)
	}

	return nil
}

// Close unmounts, locks, and detaches a LUKS loop volume.
// Continues cleanup on individual step failures and returns all errors joined.
func (v *LUKSLoopVolume) Close(ctx context.Context, loopFile, mountDir string) error {
	mapper := mapperName(loopFile)
	loopDev, _ := v.findLoop(ctx, loopFile)
	var errs []error

	// Unmount with lazy fallback.
	if err := v.run.Run(ctx, "umount", mountDir); err != nil {
		log.Printf("umount %s failed, trying lazy unmount", mountDir)
		if lazyErr := v.run.Run(ctx, "umount", "-l", mountDir); lazyErr != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", mountDir, lazyErr))
		}
	}

	// Close LUKS — continue even if umount failed.
	if err := v.run.Run(ctx, "cryptsetup", "close", mapper); err != nil {
		errs = append(errs, fmt.Errorf("cryptsetup close %s: %w", mapper, err))
	}

	// Detach loop — continue even if close failed.
	if loopDev != "" {
		if err := v.detachLoop(ctx, loopDev); err != nil {
			errs = append(errs, fmt.Errorf("detach loop %s: %w", loopDev, err))
		}
	}

	return errors.Join(errs...)
}

// MapperName returns the device mapper name that would be used for a given loop file.
func MapperNameForLoop(loopFile string) string {
	return mapperName(loopFile)
}

// attachLoop attaches a file to a loop device and returns the device path.
// Idempotent: reuses an existing loop device if one is already attached.
func (v *LUKSLoopVolume) attachLoop(ctx context.Context, loopFile string) (string, error) {
	if existing, err := v.findLoop(ctx, loopFile); err == nil && existing != "" {
		return existing, nil
	}
	out, err := v.run.RunWithOutput(ctx, "losetup", "--find", "--show", loopFile)
	if err != nil {
		return "", fmt.Errorf("losetup attach %s: %w", loopFile, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// detachLoop detaches a loop device.
func (v *LUKSLoopVolume) detachLoop(ctx context.Context, loopDev string) error {
	return v.run.Run(ctx, "losetup", "-d", loopDev)
}

// findLoop finds the loop device associated with a file.
func (v *LUKSLoopVolume) findLoop(ctx context.Context, loopFile string) (string, error) {
	out, err := v.run.RunWithOutput(ctx, "losetup", "-j", loopFile)
	if err != nil {
		return "", fmt.Errorf("losetup find %s: %w", loopFile, err)
	}
	// Output format: "/dev/loop0: [65025]:131104 (/path/to/file)"
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("no loop device for %s", loopFile)
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) < 1 {
		return "", fmt.Errorf("unexpected losetup output: %s", line)
	}
	return strings.TrimSpace(parts[0]), nil
}

func (v *LUKSLoopVolume) writeKeyToTmpfs(keyMaterial []byte) (string, func(), error) {
	return writeKeyToTmpfsDir(v.tmpfsDir, keyMaterial)
}

// writeKeyToTmpfsDir writes key material to a unique tmpfs file and returns
// the path and a cleanup function. Uses os.CreateTemp to avoid races when
// multiple volumes are operated on concurrently.
func writeKeyToTmpfsDir(tmpfsDir string, keyMaterial []byte) (string, func(), error) {
	if err := os.MkdirAll(tmpfsDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create tmpfs dir: %w", err)
	}
	f, err := os.CreateTemp(tmpfsDir, "volume-key-*")
	if err != nil {
		return "", nil, fmt.Errorf("create key tmpfs file: %w", err)
	}
	keyPath := f.Name()
	if _, err := f.Write(keyMaterial); err != nil {
		f.Close()
		os.Remove(keyPath)
		return "", nil, fmt.Errorf("write key to tmpfs: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(keyPath)
		return "", nil, fmt.Errorf("close key tmpfs file: %w", err)
	}
	return keyPath, func() { os.Remove(keyPath) }, nil
}
