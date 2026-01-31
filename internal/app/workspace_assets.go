package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"piccolod/internal/app/assets"
	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

var (
	bootShOnce         sync.Once
	bootShErr          error
	piccoloStartupOnce sync.Once
	piccoloStartupErr  error
)

// BootShHostPath returns the host filesystem path where boot.sh is stored.
// This path is bind-mounted into workspace containers as /piccolo/boot.sh.
func BootShHostPath() string {
	return paths.Join("assets", "boot.sh")
}

// PiccoloStartupHostPath returns the host filesystem path where piccolo-startup is stored.
// This path is bind-mounted into workspace containers as /piccolo/bin/piccolo-startup.
func PiccoloStartupHostPath() string {
	return paths.Join("assets", "piccolo-startup")
}

// EnsureBootShAsset ensures the boot.sh wrapper script exists on the host filesystem.
// It writes the embedded script to the state directory, creating directories as needed.
// This function is safe to call concurrently and will only write the file once.
func EnsureBootShAsset() error {
	bootShOnce.Do(func() {
		bootShErr = writeBootShAsset()
	})
	return bootShErr
}

// EnsurePiccoloStartupAsset ensures the piccolo-startup helper exists on the host filesystem.
// It writes the embedded script to the state directory, creating directories as needed.
// This function is safe to call concurrently and will only write the file once.
func EnsurePiccoloStartupAsset() error {
	piccoloStartupOnce.Do(func() {
		piccoloStartupErr = writePiccoloStartupAsset()
	})
	return piccoloStartupErr
}

// EnsureWorkspaceAssets ensures all workspace assets exist on the host filesystem.
func EnsureWorkspaceAssets() error {
	if err := EnsureBootShAsset(); err != nil {
		return err
	}
	if err := EnsurePiccoloStartupAsset(); err != nil {
		return err
	}
	return nil
}

// writeBootShAsset writes the embedded boot.sh to the host filesystem.
func writeBootShAsset() error {
	hostPath := BootShHostPath()
	dir := filepath.Dir(hostPath)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create assets directory: %w", err)
	}

	// Always overwrite to ensure the latest version is deployed
	if err := fsutil.AtomicWriteFile(hostPath, assets.BootSh, 0o755); err != nil {
		return fmt.Errorf("write boot.sh asset: %w", err)
	}

	return nil
}

// writePiccoloStartupAsset writes the embedded piccolo-startup to the host filesystem.
func writePiccoloStartupAsset() error {
	hostPath := PiccoloStartupHostPath()
	dir := filepath.Dir(hostPath)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create assets directory: %w", err)
	}

	// Always overwrite to ensure the latest version is deployed
	if err := fsutil.AtomicWriteFile(hostPath, assets.PiccoloStartup, 0o755); err != nil {
		return fmt.Errorf("write piccolo-startup asset: %w", err)
	}

	return nil
}

// ResetBootShAssetForTest resets the once guard so tests can re-run EnsureBootShAsset.
func ResetBootShAssetForTest() {
	bootShOnce = sync.Once{}
	bootShErr = nil
}

// ResetPiccoloStartupAssetForTest resets the once guard so tests can re-run EnsurePiccoloStartupAsset.
func ResetPiccoloStartupAssetForTest() {
	piccoloStartupOnce = sync.Once{}
	piccoloStartupErr = nil
}
