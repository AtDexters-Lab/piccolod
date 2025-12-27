package app

import (
	"fmt"

	"piccolod/internal/container"
	"piccolod/internal/state/paths"
)

func (m *AppManager) podmanRuntimeForApp(appName string, layout appVolumeLayout) (container.PodmanRuntime, error) {
	volID := layout.VolumeID
	if volID == "" {
		volID = appVolumeID(appName)
	}

	runRoot := paths.Join("run", "podman", volID)
	if err := ensureDir(runRoot, 0o700); err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: ensure podman runroot: %w", err)
	}

	if layout.PodmanRoot == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: podman root missing for %s", appName)
	}

	// We cannot use a shared imagestore with the 'vfs' driver because vfs does not support
	// the split-store feature (read-only images separate from read-write container layers).
	// To ensure container data (the RW layer) is encrypted, we must store everything 
	// (including images) inside the per-app encrypted volume (--root).
	// imagestore := paths.Join("podman", "imagestore") 
	
	return container.PodmanRuntime{
		Root:          layout.PodmanRoot,
		RunRoot:       runRoot,
		// Imagestore:    imagestore, // Disabled for vfs security
		StorageDriver: "vfs",
	}, nil
}
