package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	defaultCoreRoot = "/piccolo-core"
)

var (
	coreRoot   string
	podmanRoot string
	once       = &sync.Once{}
)

func resolveRoots() {
	if v := os.Getenv("PICCOLO_STATE_DIR"); v != "" {
		fmt.Fprintf(os.Stderr, "FATAL: PICCOLO_STATE_DIR is no longer supported; use PICCOLO_CORE_ROOT.\n")
		os.Exit(1)
	}
	coreRoot = filepath.Clean(envOr("PICCOLO_CORE_ROOT", defaultCoreRoot))
	// Derive podman root from core root when not explicitly set,
	// so that overriding PICCOLO_CORE_ROOT automatically relocates podman storage.
	podmanRoot = filepath.Clean(envOr("PICCOLO_PODMAN_ROOT", filepath.Join(coreRoot, "podman")))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// CoreRoot returns the root of the piccolo-core subvolume.
func CoreRoot() string {
	once.Do(resolveRoots)
	return coreRoot
}

// CoreJoin resolves a path relative to the core root.
func CoreJoin(parts ...string) string {
	all := append([]string{CoreRoot()}, parts...)
	return filepath.Join(all...)
}

// PodmanRoot returns the mount point of the ephemeral podman shared thin LV.
// This replaces the old DataJoin("node", "podman", ...) paths.
func PodmanRoot() string {
	once.Do(resolveRoots)
	return podmanRoot
}

// PodmanJoin resolves a path relative to the podman root.
func PodmanJoin(parts ...string) string {
	all := append([]string{PodmanRoot()}, parts...)
	return filepath.Join(all...)
}

// MountDir returns the mount point for a volume.
// All volumes mount under /piccolo-core/mounts/<vol-id>/.
func MountDir(volumeID string) string {
	return CoreJoin("mounts", volumeID)
}

// VolumeMetaDir returns the metadata directory for a volume.
// All volume metadata lives under /piccolo-core/volumes/<vol-id>/.
func VolumeMetaDir(volumeID string) string {
	return CoreJoin("volumes", volumeID)
}

// SetCoreRootForTest overrides the core root for the duration of a test.
// Also re-derives podmanRoot so PodmanJoin stays consistent with the new core root.
func SetCoreRootForTest(t *testing.T, dir string) {
	t.Helper()
	prevCore := coreRoot
	prevPodman := podmanRoot
	prevOnce := once
	coreRoot = dir
	podmanRoot = filepath.Join(dir, "podman")
	once = &sync.Once{} // prevent re-resolve from clobbering
	once.Do(func() {})  // exhaust the once
	t.Cleanup(func() {
		coreRoot = prevCore
		podmanRoot = prevPodman
		once = prevOnce
	})
}

// SetPodmanRootForTest overrides the podman root for the duration of a test.
func SetPodmanRootForTest(t *testing.T, dir string) {
	t.Helper()
	prev := podmanRoot
	prevOnce := once
	podmanRoot = dir
	once = &sync.Once{}
	once.Do(func() {})
	t.Cleanup(func() {
		podmanRoot = prev
		once = prevOnce
	})
}

// SetRootsForTest creates temp directories for core and podman roots and returns their paths.
func SetRootsForTest(t *testing.T) (core, podman string) {
	t.Helper()
	core = filepath.Join(t.TempDir(), "core")
	podman = filepath.Join(t.TempDir(), "podman")
	if err := os.MkdirAll(core, 0o755); err != nil {
		t.Fatalf("create test core root: %v", err)
	}
	if err := os.MkdirAll(podman, 0o755); err != nil {
		t.Fatalf("create test podman root: %v", err)
	}
	SetCoreRootForTest(t, core)
	SetPodmanRootForTest(t, podman)
	return core, podman
}
