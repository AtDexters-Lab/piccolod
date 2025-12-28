package app

import (
	"fmt"
	"os/exec"

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

	fuseOverlayfs, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: fuse-overlayfs not found: %w", err)
	}

	// Use a shared imagestore for base layer deduplication.
	// Note: Base images stored here are NOT encrypted. User data (container RW layer)
	// remains encrypted in the per-app --root.
	// Future: Support per-app private imagestore for custom apps requiring full encryption.
	imagestore := paths.Join("podman", "imagestore")
	if err := ensureDir(imagestore, 0o700); err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: ensure podman imagestore: %w", err)
	}

	return container.PodmanRuntime{
		Root:          layout.PodmanRoot,
		RunRoot:       runRoot,
		Imagestore:    imagestore,
		StorageDriver: "overlay",
		StorageOpts:   []string{fmt.Sprintf("mount_program=%s", fuseOverlayfs)},
	}, nil
}
