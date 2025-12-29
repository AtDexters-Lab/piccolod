package app

import (
	"fmt"
	"os/exec"

	"piccolod/internal/container"
	"piccolod/internal/state/paths"
)

// podmanRuntimeForApp returns a runtime configured for a specific app instance.
// Each app instance has fully isolated podman storage within its encrypted volume.
// This avoids cross-reference issues with shared imagestores.
// The instanceID parameter is the unique instance identifier.
func (m *AppManager) podmanRuntimeForApp(instanceID string, layout appVolumeLayout) (container.PodmanRuntime, error) {
	volID := layout.VolumeID
	if volID == "" {
		volID = appVolumeID(instanceID)
	}

	runRoot := paths.Join("run", "podman", volID)
	if err := ensureDir(runRoot, 0o700); err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: ensure podman runroot: %w", err)
	}

	if layout.PodmanRoot == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: podman root missing for %s", instanceID)
	}

	fuseOverlayfs, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: fuse-overlayfs not found: %w", err)
	}

	// Fully isolated per-app storage:
	// - Root: per-app encrypted volume (images + container layers)
	// - No shared imagestore (avoids overlay path resolution issues)
	// Trade-off: Images are duplicated per-app, but storage is fully isolated and encrypted
	return container.PodmanRuntime{
		Root:          layout.PodmanRoot,
		RunRoot:       runRoot,
		StorageDriver: "overlay",
		StorageOpts:   []string{fmt.Sprintf("mount_program=%s", fuseOverlayfs)},
	}, nil
}
