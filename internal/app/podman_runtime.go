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
// and the directory itself gets setgid + 0750 (piccolo-apps group inherits).
//
// Also pre-creates containers/storage metadata directories and seed files
// so per-app users don't fail when containers/storage initializes
// additionalImageStores — without this, the first per-app podman command
// triggers mkdir/create as the per-app user, which lacks write permission.
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

	if err := ensureDir(imagestore, os.ModeSetgid|modeGroupShared); err != nil {
		return "", fmt.Errorf("ensure imagestore: %w", err)
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

	imagestore, err := ensureImagestoreDir()
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	// Resolve per-app user for rootless execution isolation.
	// Provisioning failure is a hard error — silent fallback to the shared user
	// would violate the zero-fallback promise (RFC 20260206).
	cred, homeDir, err := m.resolveAppCredential(instanceID, layout, runRoot)
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	additionalStores := []string{imagestore}
	if cred != nil {
		log.Printf("INFO: podmanRuntimeForApp %s: uid=%d gid=%d root=%s additionalStores=%v",
			instanceID, cred.Uid, cred.Gid, layout.PodmanRoot, additionalStores)
	}

	// Clean stale VFS storage from previous runs.
	cleanStaleDriverStorage(layout.PodmanRoot, staleDriverPrefix)
	cleanStaleDriverStorage(runRoot, staleDriverPrefix)

	// Both service and workspace modes use the kernel overlay driver.
	// For workspace mode, --rootfs bypasses Podman storage; the overlay driver
	// here is only for the network anchor and reading the shared imagestore
	// (whose overlay metadata would be invisible to a VFS primary driver).
	serviceRoot, err := m.ensureServiceRoot(instanceID, cred)
	if err != nil {
		return container.PodmanRuntime{}, fmt.Errorf("app manager: %w", err)
	}

	// Fix imagestore permissions before any per-app podman command accesses it.
	if m.runtimeUser != nil {
		m.fixImagestoreAccess()
	}

	return container.PodmanRuntime{
		Root:                  serviceRoot,
		RunRoot:               runRoot,
		AdditionalImageStores: additionalStores,
		StorageDriver:         "overlay",
		Credential:            cred,
		HomeDir:               homeDir,
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
// Uses the kernel overlay driver, matching the per-app service-mode runtimes.
// additionalimagestores requires the same storage driver across all stores.
// Overlay provides layer deduplication — 10 apps using the same base image
// store only 1 copy.
//
// The shared imagestore directory IS the graphRoot (--root) of this runtime.
// This creates a self-contained containers/storage store that per-app runtimes
// reference via additionalimagestores for read-only access.
// Container operations (create, start, stop) continue to use per-app runtimes.
// Result is cached via sync.Once.
func (m *AppManager) podmanImageRuntime() (container.PodmanRuntime, error) {
	m.imageRuntimeOnce.Do(func() {
		runRoot, err := ensurePodmanPreamble("imagestore")
		if err != nil {
			m.imageRuntimeErr = fmt.Errorf("app manager: image runtime: %w", err)
			return
		}

		// The imagestore IS the graphRoot — a single self-contained store that
		// per-app runtimes access via additionalimagestores.
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

// ensureImagestoreGroupAccess sets the shared imagestore ownership to
// piccolo-runtime:piccolo-apps with setgid, and ensures group-readable
// permissions on containers/storage metadata so per-app users can read
// the store via additionalimagestores.
//
// containers/storage creates lock files (images.lock, layers.lock) with
// mode 0600. Without group-read on these files, per-app users in the
// piccolo-apps group cannot acquire the shared locks that
// additionalimagestores requires, causing "image not known" errors.
//
// Layer content directories (overlay/<hash>/diff/) are skipped — their
// permissions must be preserved as-is because they represent the container
// filesystem (e.g., /usr/bin must remain world-readable for non-root
// container processes).
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

	info, err := os.Stat(imagestorePath)
	if err != nil {
		return fmt.Errorf("stat imagestore: %w", err)
	}

	// Set setgid on root so new files/dirs inherit the piccolo-apps group.
	if info.Mode()&os.ModeSetgid == 0 {
		if err := os.Chmod(imagestorePath, info.Mode().Perm()|os.ModeSetgid); err != nil {
			return fmt.Errorf("chmod setgid on imagestore root: %w", err)
		}
	}

	// Walk and fix ownership + group permissions on metadata entries.
	// diff/ subtrees are skipped (layer content — preserve original modes).
	var fixedOwnership, fixedPerms, totalEntries int
	walkErr := filepath.WalkDir(imagestorePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip layer content trees — their permissions are part of the
		// container filesystem and must not be altered.
		if d.IsDir() && filepath.Base(path) == "diff" {
			return filepath.SkipDir
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
		// and JSON manifests that additionalimagestores needs to read.
		perm := fi.Mode().Perm()
		if d.IsDir() {
			if perm&0o050 != 0o050 {
				_ = os.Chmod(path, perm|0o050)
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
// group permissions so per-app users can read it via additionalimagestores.
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
	if err := m.containerManager.PullImageWithProgress(ctx, rt, image, onProgress); err != nil {
		return err
	}
	m.fixImagestoreAccess()
	return nil
}
