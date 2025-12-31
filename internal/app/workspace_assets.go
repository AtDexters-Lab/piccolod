package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"piccolod/internal/app/assets"
	"piccolod/internal/state/paths"
)

var (
	bootShOnce sync.Once
	bootShErr  error
)

// BootShHostPath returns the host filesystem path where boot.sh is stored.
// This path is bind-mounted into workspace containers as /piccolo/boot.sh.
func BootShHostPath() string {
	return paths.Join("assets", "boot.sh")
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

// writeBootShAsset writes the embedded boot.sh to the host filesystem.
func writeBootShAsset() error {
	hostPath := BootShHostPath()
	dir := filepath.Dir(hostPath)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create assets directory: %w", err)
	}

	// Always overwrite to ensure the latest version is deployed
	if err := os.WriteFile(hostPath, assets.BootSh, 0o755); err != nil {
		return fmt.Errorf("write boot.sh asset: %w", err)
	}

	return nil
}

// ResetBootShAssetForTest resets the once guard so tests can re-run EnsureBootShAsset.
func ResetBootShAssetForTest() {
	bootShOnce = sync.Once{}
	bootShErr = nil
}
