package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"piccolod/internal/container"
	"piccolod/internal/state/paths"
)

const (
	// staleDriverPrefix is the storage driver whose metadata triggers cleanup
	// when found in a graphroot. After switching from vfs to overlay, stale
	// vfs-images/ and vfs-layers/ directories cause driver mismatch errors.
	staleDriverPrefix = "vfs"

	// Named permission modes for clarity at call sites.
	modeTraversable = os.FileMode(0o711) // owner rwx, group+others x only
	modeReadableDir = os.FileMode(0o755) // owner rwx, group+others rx
	modePrivate     = os.FileMode(0o700) // owner rwx, no group/others
)

// ensurePodmanPreamble creates the podman runroot base (world-traversable)
// and creates a private runroot subdir.
func ensurePodmanPreamble(name string) (string, error) {
	if err := ensureDir(podmanRunRootBase(), modeReadableDir); err != nil {
		return "", fmt.Errorf("ensure podman runroot base: %w", err)
	}
	runRoot := filepath.Join(podmanRunRootBase(), name)
	if err := ensureDir(runRoot, modePrivate); err != nil {
		return "", fmt.Errorf("ensure podman runroot %s: %w", name, err)
	}
	return runRoot, nil
}

// podmanRunRootBase returns the base directory for podman runtime state.
// Per-app and ephemeral runtimes create subdirectories under this path.
func podmanRunRootBase() string {
	if base := os.Getenv("PICCOLO_PODMAN_RUNROOT_BASE"); base != "" {
		return filepath.Clean(base)
	}
	return "/run/piccolo/podman"
}

// resolveAppCredential provisions a per-app Linux user and sets up the
// environment (XDG_RUNTIME_DIR, cgroup delegation, directory ownership).
func (m *AppManager) resolveAppCredential(instanceID string, layout appVolumeLayout, runRoot string) (*syscall.Credential, string, error) {
	// Test hook: use injected resolver instead of real user provisioning.
	if m.credentialResolver != nil {
		return m.credentialResolver(instanceID)
	}

	appUser, err := container.ProvisionAppUser(instanceID)
	if err != nil {
		return nil, "", fmt.Errorf("provision per-app user for %s: %w", instanceID, err)
	}

	cred := appUser.Credential
	homeDir := appUser.HomeDir
	if err := container.EnsureXDGRuntimeDir(cred.Uid, cred.Gid); err != nil {
		log.Printf("WARN: %s: failed to ensure XDG_RUNTIME_DIR for per-app user %s (uid=%d): %v", instanceID, appUser.Username, cred.Uid, err)
	}
	container.CheckCgroupDelegation(cred.Uid)

	uid := int(cred.Uid)
	gid := int(cred.Gid)

	// Chown the mount root and disk/ so the per-app user can traverse
	// the path from the encrypted volume root to PodmanRoot.
	// Also chown PodmanRoot, dataDir, and runRoot for per-app isolation.
	chownDirs := []string{layout.PodmanRoot, runRoot}
	if layout.MountDir != "" {
		chownDirs = append(chownDirs, layout.MountDir, layout.DiskDir, layout.DataDir)
	}
	for _, dir := range chownDirs {
		if dir == "" {
			continue
		}
		if err := container.ChownIfNeeded(dir, uid, gid); err != nil {
			return nil, "", fmt.Errorf("chown %s: %w", dir, err)
		}
	}

	return cred, homeDir, nil
}

// ensureServiceRoot creates the per-app podman service root directory,
// cleans stale UID storage, chowns to the per-app user, and removes stale
// driver metadata.
func (m *AppManager) ensureServiceRoot(instanceID string, cred *syscall.Credential) (string, error) {
	serviceRoot := paths.PodmanJoin("apps", instanceID)
	if err := os.MkdirAll(serviceRoot, modePrivate); err != nil {
		return "", fmt.Errorf("ensure service podman root: %w", err)
	}
	// Wipe stale podman state if the graphroot was created by a different UID.
	cleanStaleUIDStorage(serviceRoot, int(cred.Uid))
	if err := container.ChownIfNeeded(serviceRoot, int(cred.Uid), int(cred.Gid)); err != nil {
		return "", fmt.Errorf("chown service root: %w", err)
	}
	// Best-effort: make apps parent dir traversable for other per-app users.
	_ = os.Chmod(paths.PodmanJoin("apps"), modeTraversable)
	cleanStaleDriverStorage(serviceRoot, staleDriverPrefix)
	return serviceRoot, nil
}

// podmanRuntimeForApp returns a runtime configured for a specific app instance.
// Each app instance has an isolated podman Root (container metadata, RW layers).
// Service containers use --rootfs from golden LV snapshots, bypassing podman image storage.
func (m *AppManager) podmanRuntimeForApp(instanceID string, layout appVolumeLayout, mode PiccoloMode) (container.PodmanRuntime, error) {
	volID := layout.VolumeID
	if volID == "" {
		volID = appVolumeID(instanceID)
	}

	runRoot, err := ensurePodmanPreamble(volID)
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	if layout.PodmanRoot == "" {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: podman root missing for %s", instanceID)
	}

	cred, homeDir, err := m.resolveAppCredential(instanceID, layout, runRoot)
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	log.Printf("INFO: podmanRuntimeForApp %s: uid=%d gid=%d root=%s",
		instanceID, cred.Uid, cred.Gid, layout.PodmanRoot)

	// Clean stale VFS storage from previous runs.
	cleanStaleDriverStorage(layout.PodmanRoot, staleDriverPrefix)
	cleanStaleDriverStorage(runRoot, staleDriverPrefix)

	serviceRoot, err := m.ensureServiceRoot(instanceID, cred)
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	return container.PodmanRuntime{
		Root:          serviceRoot,
		RunRoot:       runRoot,
		StorageDriver: "overlay",
		Credential:    cred,
		HomeDir:       homeDir,
	}, nil
}

// newEphemeralFlattenRuntime creates a temporary podman root for a single
// flatten operation. The caller must invoke cleanup() when done.
// Uses CoreJoin("tmp") as base — avoids /tmp tmpfs space constraints on
// edge devices with large images. Orphaned tmpdirs from crashes are
// cleaned up on daemon start via cleanStaleFlattenDirs.
func newEphemeralFlattenRuntime(ru container.RuntimeUser) (container.PodmanRuntime, func(), error) {
	flattenBase := paths.CoreJoin("tmp")
	if err := os.MkdirAll(flattenBase, 0o711); err != nil {
		return container.PodmanRuntime{}, nil, fmt.Errorf("ensure flatten base: %w", err)
	}
	base, err := os.MkdirTemp(flattenBase, "flatten-*")
	if err != nil {
		return container.PodmanRuntime{}, nil, fmt.Errorf("create flatten tmpdir: %w", err)
	}
	root := filepath.Join(base, "root")
	runRoot := filepath.Join(base, "run")
	if err := os.MkdirAll(root, 0o700); err != nil {
		os.RemoveAll(base)
		return container.PodmanRuntime{}, nil, err
	}
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		os.RemoveAll(base)
		return container.PodmanRuntime{}, nil, err
	}
	uid, gid := int(ru.Credential.Uid), int(ru.Credential.Gid)
	// Chown base dir so runtime user can traverse it to reach root/ and run/.
	if err := container.ChownIfNeeded(base, uid, gid); err != nil {
		os.RemoveAll(base)
		return container.PodmanRuntime{}, nil, err
	}
	if err := container.ChownIfNeeded(root, uid, gid); err != nil {
		os.RemoveAll(base)
		return container.PodmanRuntime{}, nil, err
	}
	if err := container.ChownIfNeeded(runRoot, uid, gid); err != nil {
		os.RemoveAll(base)
		return container.PodmanRuntime{}, nil, err
	}
	rt := container.PodmanRuntime{
		Root:          root,
		RunRoot:       runRoot,
		StorageDriver: "overlay",
		Credential:    ru.Credential,
		HomeDir:       ru.HomeDir,
	}
	cleanup := func() { os.RemoveAll(base) }
	return rt, cleanup, nil
}

// newRuntimeFromDir constructs a PodmanRuntime from an existing flatten directory.
// Used to reuse a pre-pulled runtime directory for flatten — avoids a redundant image pull.
func newRuntimeFromDir(baseDir string, ru container.RuntimeUser) container.PodmanRuntime {
	return container.PodmanRuntime{
		Root:          filepath.Join(baseDir, "root"),
		RunRoot:       filepath.Join(baseDir, "run"),
		StorageDriver: "overlay",
		Credential:    ru.Credential,
		HomeDir:       ru.HomeDir,
	}
}

// cleanStaleFlattenDirs removes orphaned flatten-* tmpdirs from prior crashes.
// Called on daemon startup.
func cleanStaleFlattenDirs() {
	flattenBase := paths.CoreJoin("tmp")
	entries, err := os.ReadDir(flattenBase)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "flatten-") {
			target := filepath.Join(flattenBase, e.Name())
			log.Printf("INFO: cleaning stale flatten tmpdir: %s", target)
			if err := os.RemoveAll(target); err != nil {
				log.Printf("WARN: failed to clean stale flatten tmpdir %s: %v", target, err)
			}
		}
	}
}

// cleanStaleDriverStorage wipes all contents of a podman graphroot/runroot when
// metadata from a different storage driver is detected.
func cleanStaleDriverStorage(dir, stalePrefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // doesn't exist yet — nothing to clean
	}
	hasStale := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stalePrefix+"-") || e.Name() == stalePrefix {
			hasStale = true
			break
		}
	}
	if !hasStale {
		return
	}
	log.Printf("INFO: cleanStaleDriverStorage: wiping stale %s state from %s", stalePrefix, dir)
	for _, e := range entries {
		target := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(target); err != nil {
			log.Printf("WARN: failed to remove %s: %v", target, err)
		}
	}
}

// cleanStaleUIDStorage wipes the contents of a per-app podman graphroot when it
// was created by a different Linux UID.
func cleanStaleUIDStorage(dir string, expectedUID int) {
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == expectedUID {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return
	}
	log.Printf("INFO: cleanStaleUIDStorage: wiping %s (owned by uid=%d, expected uid=%d)", dir, st.Uid, expectedUID)
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			log.Printf("WARN: cleanStaleUIDStorage: failed to remove %s: %v", e.Name(), err)
		}
	}
}
