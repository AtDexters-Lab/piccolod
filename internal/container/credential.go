package container

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// RuntimeUser holds the resolved credentials and home directory for the
// runtime user used in rootless Podman execution.
type RuntimeUser struct {
	Credential *syscall.Credential
	HomeDir    string
}

// ResolveRuntimeCredential looks up the given system user and returns a
// RuntimeUser with the syscall.Credential and HomeDir. Returns an error if
// the user does not exist or cannot be resolved — the daemon requires
// rootless Podman execution and must not start without a valid runtime user.
func ResolveRuntimeCredential(username string) (*RuntimeUser, error) {
	u, err := defaultResolver.LookupUser(username)
	if err != nil {
		return nil, fmt.Errorf("runtime user %q not found (rootless Podman requires this user): %w", username, err)
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid UID for runtime user %q: %w", username, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid GID for runtime user %q: %w", username, err)
	}

	// Look up supplementary groups for shared imagestore access (piccolo-apps group).
	// Supplementary groups are required for per-app users to read the shared imagestore.
	// Hard error: piccolo-runtime is a local system user — GroupIds should never fail
	// except on corrupted /etc/group. Failing loudly here is preferable to silent
	// imagestore permission failures at container start.
	groupIDs, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("resolve supplementary groups for %q: %w", username, err)
	}
	var groups []uint32
	for _, gidStr := range groupIDs {
		g, err := strconv.ParseUint(gidStr, 10, 32)
		if err != nil {
			continue
		}
		groups = append(groups, uint32(g))
	}

	log.Printf("INFO: resolved runtime user %q (uid=%d gid=%d groups=%v home=%s)", username, uid, gid, groups, u.HomeDir)
	return &RuntimeUser{
		Credential: &syscall.Credential{
			Uid:    uint32(uid),
			Gid:    uint32(gid),
			Groups: groups,
		},
		HomeDir: u.HomeDir,
	}, nil
}

// ChownIfNeeded recursively ensures all files under root are owned by uid:gid.
// Fast path: if the root directory is already owned by uid:gid, the entire
// tree is assumed correct (skips the walk). This makes reconciliation O(1) for
// already-correct ownership. The full walk only runs on first chown or after
// ownership changes.
//
// Concurrency safety: this fast-path is safe because lifecycle operations
// (install, start, stop, uninstall, reconcile) are serialized per-app via
// reconcileMu in app_manager.go. No concurrent caller can change ownership
// between the root stat and the decision to skip the walk.
func ChownIfNeeded(root string, uid, gid int) error {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to chown
		}
		return fmt.Errorf("stat %s: %w", root, err)
	}

	// Fast path: root already has correct ownership → assume children are correct.
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(stat.Uid) == uid && int(stat.Gid) == gid {
			return nil
		}
	}

	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok && int(stat.Uid) == uid && int(stat.Gid) == gid {
			return nil // Already correct
		}
		return os.Lchown(path, uid, gid)
	})
}

// EnsureXDGRuntimeDir creates /run/user/<uid> with mode 0700 and correct ownership.
// This directory is required by rootless Podman for runtime state.
//
// Also fixes ownership of the libpod subdirectory if it exists and is root-owned.
// Podman creates /run/user/<uid>/libpod/tmp for alive files and lock state.
// If a root-level podman process (e.g., overlay compat check) creates this tree
// before the rootless user runs podman, the rootless process gets EPERM on chmod
// (sticky bit). Chowning the subtree on startup prevents this race.
func EnsureXDGRuntimeDir(uid, gid uint32) error {
	dir := fmt.Sprintf("/run/user/%d", uid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create XDG_RUNTIME_DIR %s: %w", dir, err)
	}
	// Ensure correct mode even if directory pre-existed with broader permissions.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod XDG_RUNTIME_DIR %s: %w", dir, err)
	}
	if err := os.Chown(dir, int(uid), int(gid)); err != nil {
		return fmt.Errorf("chown XDG_RUNTIME_DIR %s: %w", dir, err)
	}

	// Fix root-owned libpod subtree if present.
	libpodDir := filepath.Join(dir, "libpod")
	if err := ChownIfNeeded(libpodDir, int(uid), int(gid)); err != nil {
		log.Printf("WARN: chown libpod dir %s: %v", libpodDir, err)
	}
	return nil
}

// CheckCgroupDelegation logs a warning if the expected cgroup slice for the
// runtime user does not exist. This is required for rootless Podman resource limits.
// Non-fatal: the caller should continue even if delegation is missing.
func CheckCgroupDelegation(uid uint32) {
	path := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice", uid)
	if _, err := os.Stat(path); err != nil {
		log.Printf("ERROR: cgroup delegation not found at %s — rootless Podman resource limits may not work. Ensure loginctl linger is enabled for the runtime user.", path)
	}
}
