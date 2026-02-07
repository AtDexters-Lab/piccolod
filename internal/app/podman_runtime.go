package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"piccolod/internal/container"
	"piccolod/internal/state/paths"
)

// podmanRuntimeForApp returns a runtime configured for a specific app instance.
// Each app instance has an isolated podman Root (container metadata, RW layers) within
// its encrypted volume, while images are stored in the shared imagestore for deduplication.
// The instanceID parameter is the unique instance identifier.
// The mode parameter controls storage driver selection:
//   - ModeService: uses overlay driver with fuse-overlayfs (image-based containers)
//   - ModeWorkspace: uses vfs driver (for --rootfs mode with our own fuse-overlayfs mount)
func (m *AppManager) podmanRuntimeForApp(instanceID string, layout appVolumeLayout, mode PiccoloMode) (container.PodmanRuntime, error) {
	volID := layout.VolumeID
	if volID == "" {
		volID = appVolumeID(instanceID)
	}

	runRootBase := os.Getenv("PICCOLO_PODMAN_RUNROOT_BASE")
	runRoot := ""
	if runRootBase != "" {
		runRoot = filepath.Join(filepath.Clean(runRootBase), volID)
	} else {
		runRoot = paths.Join("run", "podman", volID)
	}
	if err := ensureDir(runRoot, 0o700); err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: ensure podman runroot: %w", err)
	}

	if layout.PodmanRoot == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: podman root missing for %s", instanceID)
	}

	// Use a shared imagestore for base layer deduplication.
	// Note: Base images stored here are NOT encrypted. User data (container RW layer)
	// remains encrypted in the per-app --root.
	// Future: Support per-app private imagestore for custom apps requiring full encryption.
	imagestore := paths.Join("podman", "imagestore")
	if err := ensureDir(imagestore, 0o700); err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: ensure podman imagestore: %w", err)
	}

	// Workspace mode uses vfs driver to avoid podman creating its own overlay layer
	// on top of our fuse-overlayfs workspace disk. With vfs, podman uses the --rootfs
	// filesystem directly without additional layering.
	if mode == ModeWorkspace {
		return container.PodmanRuntime{
			Root:          layout.PodmanRoot,
			RunRoot:       runRoot,
			Imagestore:    imagestore,
			StorageDriver: "vfs",
		}, nil
	}

	// Service mode uses overlay driver with fuse-overlayfs for image-based containers.
	fuseOverlayfs, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: fuse-overlayfs not found: %w", err)
	}

	return container.PodmanRuntime{
		Root:          layout.PodmanRoot,
		RunRoot:       runRoot,
		Imagestore:    imagestore,
		StorageDriver: "overlay",
		StorageOpts:   []string{fmt.Sprintf("mount_program=%s", fuseOverlayfs)},
	}, nil
}

// podmanImageRuntime returns a shared PodmanRuntime for base image operations across
// all app types (pull, inspect, exists, mount, unmount, remove). Unlike podmanRuntimeForApp,
// this uses a shared root with overlay driver, providing:
//   - Layer deduplication: overlay stores thin diffs, not VFS cumulative copies
//   - Persistent metadata: shared root survives app install/uninstall cycles
//   - Cross-app sharing: 10 apps with the same Debian base = 1 copy of each layer
//
// Images are stored in the shared imagestore (same store used by per-app runtimes).
// The root (image-root) is a lightweight podman metadata directory (c/storage db, locks);
// actual image layers and metadata live in the imagestore.
// Container operations (create, start, stop) continue to use per-app runtimes.
// Result is cached via sync.Once.
func (m *AppManager) podmanImageRuntime() (container.PodmanRuntime, error) {
	m.imageRuntimeOnce.Do(func() {
		root := paths.Join("podman", "image-root")
		if err := ensureDir(root, 0o700); err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: ensure image runtime root: %w", err)
			return
		}

		runRootBase := os.Getenv("PICCOLO_PODMAN_RUNROOT_BASE")
		var runRoot string
		if runRootBase != "" {
			runRoot = filepath.Join(filepath.Clean(runRootBase), "image-root")
		} else {
			runRoot = paths.Join("run", "podman", "image-root")
		}
		if err := ensureDir(runRoot, 0o700); err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: ensure image runtime runroot: %w", err)
			return
		}

		imagestore := paths.Join("podman", "imagestore")
		if err := ensureDir(imagestore, 0o700); err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: ensure image runtime imagestore: %w", err)
			return
		}

		fuseOverlayfs, err := exec.LookPath("fuse-overlayfs")
		if err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: fuse-overlayfs not found: %w", err)
			return
		}

		m.imageRuntimeVal = container.PodmanRuntime{
			Root:          root,
			RunRoot:       runRoot,
			Imagestore:    imagestore,
			StorageDriver: "overlay",
			StorageOpts:   []string{fmt.Sprintf("mount_program=%s", fuseOverlayfs)},
		}
	})
	return m.imageRuntimeVal, m.imageRuntimeErr
}
