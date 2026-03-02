package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	modeGroupShared = os.FileMode(0o750) // owner rwx, group rx
	modeGroupRead   = os.FileMode(0o640) // owner rw, group r
)

// imagestorePath returns the shared imagestore directory path.
func imagestorePath() string {
	return paths.PodmanJoin("imagestore")
}

// ensurePodmanPreamble creates the podman runroot base (world-traversable)
// and creates a private runroot subdir.
// Used by both podmanRuntimeForApp and podmanImageRuntime.
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

// ensureImagestoreDir creates the shared imagestore directory with correct
// permissions for per-app user access. Parent directories are made
// world-traversable (0o711) so per-app users can reach the imagestore,
// and the directory itself gets 0750 (piccolo-apps group readable).
//
// IMPORTANT: setgid must NOT be used on the imagestore or its subdirectories.
// Rootless podman overlay storage fails with "permission denied" on overlay
// metacopy check when the graphroot has setgid set. The group permission
// fixup after each pull (ensureImagestoreGroupAccess) handles group ownership.
//
// Also pre-creates containers/storage metadata directories and seed files
// so per-app users don't fail when containers/storage initializes the shared
// imagestore — without this, the first per-app podman command triggers
// mkdir/create as the per-app user, which lacks write permission.
func ensureImagestoreDir() (string, error) {
	imagestore := imagestorePath()

	imagestoreParent := filepath.Dir(imagestore)
	if err := os.MkdirAll(imagestoreParent, modeTraversable); err != nil {
		return "", fmt.Errorf("ensure imagestore parent: %w", err)
	}
	// Best-effort: make ancestor dirs traversable so per-app users can reach imagestore.
	// Walk from core root down to the imagestore parent so rootless podman can traverse
	// the full path. Errors intentionally ignored — downstream operations will surface
	// access failures with more actionable context than a stray chmod error here.
	for _, dir := range []string{filepath.Dir(paths.PodmanRoot()), paths.PodmanRoot()} {
		_ = os.Chmod(dir, modeTraversable)
	}

	if err := ensureDir(imagestore, modeGroupShared); err != nil {
		return "", fmt.Errorf("ensure imagestore: %w", err)
	}

	// Strip setgid from existing directories. Templates/clones from
	// older versions may have setgid set, which breaks rootless overlay.
	stripSetgidRecursive(imagestore)

	// Remove stale .has-mount-program markers left by prior fuse-overlayfs
	// configurations. This marker tells podman to require mount_program,
	// which is no longer used in the block-native architecture.
	hasMountProgram := filepath.Join(imagestore, "overlay", ".has-mount-program")
	if _, err := os.Stat(hasMountProgram); err == nil {
		_ = os.Remove(hasMountProgram)
		log.Printf("INFO: ensureImagestoreDir: removed stale .has-mount-program marker")
	}

	// Pre-create containers/storage metadata directories.
	for _, sub := range []string{"overlay-images", "overlay-layers", "overlay-containers"} {
		subDir := filepath.Join(imagestore, sub)
		if err := os.MkdirAll(subDir, modeGroupShared); err != nil {
			log.Printf("WARN: ensureImagestoreDir: create %s: %v", sub, err)
		}
	}

	// Pre-create lock files and empty JSON manifests that containers/storage
	// expects. Per-app users need to open lock files for shared locking and
	// read JSON manifests.
	seedFiles := map[string][]byte{
		"overlay-images/images.lock": nil,
		"overlay-images/images.json": []byte("[]"),
		"overlay-layers/layers.lock": nil,
		"overlay-layers/layers.json": []byte("[]"),
	}
	for rel, content := range seedFiles {
		p := filepath.Join(imagestore, rel)
		if _, err := os.Stat(p); err != nil {
			if content == nil {
				content = []byte{}
			}
			if wErr := os.WriteFile(p, content, modeGroupRead); wErr != nil {
				log.Printf("WARN: ensureImagestoreDir: seed file %s: %v", rel, wErr)
			}
		}
	}

	return imagestore, nil
}

// stripSetgidRecursive removes the setgid bit from all directories under root.
// Rootless podman overlay mounts fail with EPERM when directories in the
// graphroot have setgid set — the kernel overlay metacopy check cannot
// create temporary overlay mounts inside a user namespace on setgid dirs.
func stripSetgidRecursive(root string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		fi, fiErr := d.Info()
		if fiErr != nil {
			return nil
		}
		if fi.Mode()&os.ModeSetgid != 0 {
			_ = os.Chmod(path, fi.Mode().Perm())
		}
		return nil
	})
}

// podmanRunRootBase returns the base directory for podman runtime state.
// Per-app and image runtimes create subdirectories under this path.
func podmanRunRootBase() string {
	if base := os.Getenv("PICCOLO_PODMAN_RUNROOT_BASE"); base != "" {
		return filepath.Clean(base)
	}
	return "/run/piccolo/podman"
}

// resolveAppCredential provisions a per-app Linux user and sets up the
// environment (XDG_RUNTIME_DIR, cgroup delegation, directory ownership).
// Returns nil credential when per-app isolation is disabled (m.runtimeUser == nil).
func (m *AppManager) resolveAppCredential(instanceID string, layout appVolumeLayout, runRoot string) (*syscall.Credential, string, error) {
	if m.runtimeUser == nil {
		return nil, "", nil
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
	if cred != nil {
		// Wipe stale podman state if the graphroot was created by a different UID.
		// After daemon restarts the per-app user may be recreated with a different
		// UID. The bolt DB inside the graphroot has hardcoded paths referencing the
		// old UID's /run/user/<old-uid>/libpod/tmp — partial fixup is fragile.
		cleanStaleUIDStorage(serviceRoot, int(cred.Uid))
		if err := container.ChownIfNeeded(serviceRoot, int(cred.Uid), int(cred.Gid)); err != nil {
			return "", fmt.Errorf("chown service root: %w", err)
		}
		// Best-effort: make apps parent dir traversable for other per-app users.
		// Errors intentionally ignored — same rationale as ensureImagestoreDir.
		_ = os.Chmod(paths.PodmanJoin("apps"), modeTraversable)
	}
	cleanStaleDriverStorage(serviceRoot, staleDriverPrefix)
	return serviceRoot, nil
}

// podmanRuntimeForApp returns a runtime configured for a specific app instance.
// Each app instance has an isolated podman Root (container metadata, RW layers) within
// its encrypted volume, while images are stored in the shared imagestore for deduplication.
// Both service and workspace modes use the kernel overlay driver.
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

	// Resolve per-app user for rootless execution isolation.
	// Provisioning failure is a hard error — silent fallback to the shared user
	// would violate the zero-fallback promise (RFC 20260206).
	cred, homeDir, err := m.resolveAppCredential(instanceID, layout, runRoot)
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	if cred != nil {
		log.Printf("INFO: podmanRuntimeForApp %s: uid=%d gid=%d root=%s",
			instanceID, cred.Uid, cred.Gid, layout.PodmanRoot)
	}

	// Clean stale VFS storage from previous runs.
	cleanStaleDriverStorage(layout.PodmanRoot, staleDriverPrefix)
	cleanStaleDriverStorage(runRoot, staleDriverPrefix)

	// Per-app users store images (network anchor) in their own graphroot.
	// Service containers use --rootfs from golden LV snapshots, bypassing podman storage.
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

// cleanStaleDriverStorage wipes all contents of a podman graphroot/runroot when
// metadata from a different storage driver is detected. containers/storage
// auto-detects the driver from <driver>-images/ directories, ignoring the config
// if existing metadata disagrees. The entire directory must be reset to switch drivers.
// The stalePrefix is the driver whose metadata should trigger cleanup (e.g., "vfs" or "overlay").
func cleanStaleDriverStorage(dir, stalePrefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // doesn't exist yet — nothing to clean
	}
	hasStale := false
	for _, e := range entries {
		// Only detect driver-specific directories (e.g. vfs-images/, vfs-layers/) as
		// stale indicators. Do NOT use "libpod" as a trigger — libpod/ exists in all
		// graphroots regardless of driver and would cause every non-empty graphroot to
		// be wiped on every lifecycle operation.
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
// was created by a different Linux UID. After daemon restarts, per-app users may
// be recreated with a different UID (e.g., orphan cleanup + re-provisioning).
// The bolt DB inside the graphroot stores the old user's /run/user/<uid>/libpod/tmp
// path — reusing the graphroot with a new UID causes "permission denied" on the
// old user's runtime dir. Wiping is safe because the graphroot only contains
// podman metadata for the network anchor; actual app data lives on the encrypted volume.
func cleanStaleUIDStorage(dir string, expectedUID int) {
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == expectedUID {
		return // Owned by the expected user, nothing to do.
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

// podmanImageRuntime returns a shared PodmanRuntime for base image operations across
// all app types (pull, inspect, exists, mount, unmount, remove).
//
// Uses the kernel overlay driver. Overlay provides layer deduplication —
// 10 apps using the same base image store only 1 copy.
//
// The shared imagestore directory IS the graphRoot (--root) of this runtime.
// Golden LV images are prepared here and then snapshotted per-app via LVM.
// Container operations (create, start, stop) use per-app runtimes.
// Result is cached via sync.Once.
func (m *AppManager) podmanImageRuntime() (container.PodmanRuntime, error) {
	m.imageRuntimeOnce.Do(func() {
		runRoot, err := ensurePodmanPreamble("imagestore")
		if err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: image runtime: %w", err)
			return
		}

		// The imagestore IS the graphRoot — a single self-contained store used
		// for golden LV image preparation.
		imagestore, err := ensureImagestoreDir()
		if err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: image runtime: %w", err)
			return
		}

		// Clean stale VFS storage from previous runs.
		cleanStaleDriverStorage(imagestore, staleDriverPrefix)
		cleanStaleDriverStorage(runRoot, staleDriverPrefix)

		rt := container.PodmanRuntime{
			Root:          imagestore,
			RunRoot:       runRoot,
			StorageDriver: "overlay",
		}

		if m.runtimeUser != nil {
			rt.Credential = m.runtimeUser.Credential
			rt.HomeDir = m.runtimeUser.HomeDir
			uid := int(m.runtimeUser.Credential.Uid)
			gid := int(m.runtimeUser.Credential.Gid)

			// Re-ensure XDG runtime dir: the libpod subtree may have been
			// created root-owned between daemon init and first imagestore use
			// (e.g., by overlay compat checks during reconciliation).
			if err := container.EnsureXDGRuntimeDir(m.runtimeUser.Credential.Uid, m.runtimeUser.Credential.Gid); err != nil {
				log.Printf("WARN: imagestore runtime: failed to ensure XDG_RUNTIME_DIR: %v", err)
			}

			if err := container.ChownIfNeeded(runRoot, uid, gid); err != nil {
				m.imageRuntimeErr = fmt.Errorf("app manager: chown image runtime runroot: %w", err)
				return
			}
			// Imagestore uses group permissions (piccolo-apps) so per-app users can read it.
			if err := ensureImagestoreGroupAccess(imagestore, uid); err != nil {
				log.Printf("WARN: imagestore group access setup failed: %v", err)
			}
		}

		m.imageRuntimeVal = rt
	})
	return m.imageRuntimeVal, m.imageRuntimeErr
}

// PodmanImageRuntime exposes the image runtime for diagnostics.
func (m *AppManager) PodmanImageRuntime() (container.PodmanRuntime, error) {
	return m.podmanImageRuntime()
}

// inDiffSubtree returns true if path is a diff/ directory or any descendant of one.
// The imagestore layout is: <root>/overlay/<hash>/diff/...
// This detects paths where the relative path from imagestorePath contains "/diff"
// as a path component after the overlay/<hash>/ prefix.
func inDiffSubtree(imagestorePath, path string) bool {
	rel, err := filepath.Rel(imagestorePath, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// Layout: overlay/<hash>/diff/... → parts[2] == "diff"
	return len(parts) >= 3 && parts[0] == "overlay" && parts[2] == "diff"
}

// ensureImagestoreGroupAccess sets the shared imagestore ownership to
// piccolo-runtime:piccolo-apps and ensures group-readable permissions on
// containers/storage metadata so per-app users can read the store.
//
// containers/storage creates lock files (images.lock, layers.lock) with
// mode 0600. Without group-read on these files, podman commands fail.
//
// For diff/ subtrees (container filesystem content): ownership is fixed
// but permissions are preserved. Native overlay stores layer contents with
// original UIDs (root:root); chowning to piccolo-runtime allows podman's
// storage initialization to traverse layers. Container semantics are preserved
// because idmapped mounts remap ownership at runtime.
//
// IMPORTANT: setgid is NOT used. Rootless podman overlay mounts fail when
// directories have setgid set (overlay metacopy check EPERM in userns).
// This function explicitly strips setgid from any directory that has it,
// and relies on the post-pull walk to fix group ownership on new files.
//
// This function must be called after every image pull, not just at startup,
// because each pull creates new metadata files with restrictive permissions.
func ensureImagestoreGroupAccess(imagestorePath string, ownerUID int) error {
	grp, err := user.LookupGroup(container.AppsGroupName)
	if err != nil {
		return fmt.Errorf("lookup %s group: %w", container.AppsGroupName, err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return fmt.Errorf("parse %s GID: %w", container.AppsGroupName, err)
	}

	// Walk and fix ownership + group permissions on metadata entries.
	// diff/ subtrees are skipped (layer content — preserve original modes).
	var fixedOwnership, fixedPerms, totalEntries int
	walkErr := filepath.WalkDir(imagestorePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// For diff/ subtrees: fix ownership but NOT permissions.
		// Native overlay stores layer contents with original UIDs (root:root);
		// idmapped mounts remap them at container runtime. Podman's storage
		// initialization needs to traverse these trees as uid=470 (piccolo-runtime),
		// so ownership must match. Permissions are container filesystem semantics
		// and must not be changed (e.g., etc/ssl/private stays 0700).
		if inDiffSubtree(imagestorePath, path) {
			fi, fiErr := d.Info()
			if fiErr == nil {
				st, ok := fi.Sys().(*syscall.Stat_t)
				if ok && (int(st.Uid) != ownerUID || int(st.Gid) != gid) {
					if err := os.Lchown(path, ownerUID, gid); err != nil {
						log.Printf("WARN: imagestore diff chown %s: %v", path, err)
					} else {
						fixedOwnership++
					}
				}
				totalEntries++
			}
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		totalEntries++

		// Symlinks: fix ownership only (permission bits are meaningless).
		if fi.Mode().Type() == os.ModeSymlink {
			st, ok := fi.Sys().(*syscall.Stat_t)
			if ok && (int(st.Uid) != ownerUID || int(st.Gid) != gid) {
				_ = os.Lchown(path, ownerUID, gid)
				fixedOwnership++
			}
			return nil
		}

		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}

		// Fix ownership.
		if int(st.Uid) != ownerUID || int(st.Gid) != gid {
			if err := os.Lchown(path, ownerUID, gid); err != nil {
				return err
			}
			fixedOwnership++
		}

		// Ensure group can access metadata: group-rx on dirs, group-r on files.
		// This is critical for lock files (created 0600 by containers/storage)
		// and JSON manifests that per-app users need to read.
		// Also ensure owner write on dirs: containers/storage creates read-only
		// diff dirs (mode 555) inside temp dirs; the owner must be able to
		// clean these up. Strip setgid — rootless overlay fails with it.
		perm := fi.Mode().Perm() // Perm() strips special bits (setgid etc.)
		if d.IsDir() {
			needsGroupRX := perm&0o050 != 0o050
			needsOwnerW := perm&0o200 == 0
			hasSetgid := fi.Mode()&os.ModeSetgid != 0
			if needsGroupRX || needsOwnerW || hasSetgid {
				_ = os.Chmod(path, perm|0o250)
				fixedPerms++
			}
		} else {
			if perm&0o040 == 0 {
				_ = os.Chmod(path, perm|0o040)
				fixedPerms++
			}
		}

		return nil
	})
	if walkErr == nil {
		log.Printf("INFO: ensureImagestoreGroupAccess: scanned %d entries, fixed %d ownership + %d permissions (target gid=%d)",
			totalEntries, fixedOwnership, fixedPerms, gid)
	}
	return walkErr
}

// fixImagestoreAccess ensures the shared imagestore has correct group
// permissions after an image pull. No-op when per-app isolation is disabled.
func (m *AppManager) fixImagestoreAccess() {
	if m.runtimeUser == nil {
		return
	}
	imagestore := imagestorePath()
	uid := int(m.runtimeUser.Credential.Uid)
	if err := ensureImagestoreGroupAccess(imagestore, uid); err != nil {
		log.Printf("WARN: post-pull imagestore permission fix failed: %v", err)
	}
}

// pullToImagestore pulls an image to the shared imagestore and fixes
// group permissions for golden LV preparation.
// If onProgress is non-nil, it receives real-time pull progress reports;
// otherwise a default logging callback is used.
func (m *AppManager) pullToImagestore(ctx context.Context, image string, onProgress func(container.ImagePullReport)) error {
	rt, err := m.podmanImageRuntime()
	if err != nil {
		return fmt.Errorf("image runtime unavailable: %w", err)
	}
	if onProgress == nil {
		onProgress = func(report container.ImagePullReport) {
			log.Printf("INFO: pullToImagestore %s: phase=%s overall=%d%% downloaded=%d/%d layers=%d",
				image, report.Phase, report.OverallPercent, report.DownloadedBytes, report.TotalBytes, len(report.Layers))
		}
	}
	// Fix ownership before pull: previous operations (container create/export
	// during flatten) may have left stale temp dirs with root-owned layer content.
	// Podman running as uid=470 can't remove them without ownership fix first.
	m.fixImagestoreAccess()
	if err := m.containerManager.PullImageWithProgress(ctx, rt, image, onProgress); err != nil {
		return err
	}
	m.fixImagestoreAccess()
	return nil
}
