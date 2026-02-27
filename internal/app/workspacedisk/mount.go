package workspacedisk

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Directory names within the workspace directory.
const (
	UpperDir  = "upper"
	WorkDir   = "work"
	MergedDir = "merged"
)

// Layout holds the paths for a workspace disk.
type Layout struct {
	// Base is the workspace directory ({volume}/disk/workspace)
	Base string
	// Upper is the persistent writable layer
	Upper string
	// Work is the overlay workdir (ephemeral)
	Work string
	// Merged is the overlay mountpoint (container rootfs)
	Merged string
}

// NewLayout creates a Layout from a base workspace directory.
func NewLayout(workspaceDir string) Layout {
	return Layout{
		Base:   workspaceDir,
		Upper:  filepath.Join(workspaceDir, UpperDir),
		Work:   filepath.Join(workspaceDir, WorkDir),
		Merged: filepath.Join(workspaceDir, MergedDir),
	}
}

// EnsureDirs creates the workspace directories if they don't exist.
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.Upper, l.Work, l.Merged} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// CleanWorkDir removes and recreates the work directory.
// OverlayFS requires work/ to be empty at mount time.
func (l Layout) CleanWorkDir() error {
	if err := os.RemoveAll(l.Work); err != nil {
		return fmt.Errorf("remove work directory: %w", err)
	}
	if err := os.MkdirAll(l.Work, 0o755); err != nil {
		return fmt.Errorf("create work directory: %w", err)
	}
	return nil
}

// IsMounted checks if the merged directory is an active mountpoint.
func (l Layout) IsMounted() (bool, error) {
	return isMountPoint(l.Merged)
}

// isMountPoint checks if a path is a mountpoint by comparing device IDs.
func isMountPoint(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}

	parent := filepath.Dir(path)
	if parent == path {
		return true, nil // Root is always a mountpoint
	}

	var st, pst unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false, err
	}
	if err := unix.Stat(parent, &pst); err != nil {
		return false, err
	}

	// Different device IDs mean path is a mountpoint
	return st.Dev != pst.Dev, nil
}

// isMountedFromMtab checks /proc/mounts for an overlay mount at the given path.
func isMountedFromMtab(mountpoint string) (bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, fmt.Errorf("open /proc/mounts: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			if fields[1] == mountpoint {
				return true, nil
			}
		}
	}
	return false, scanner.Err()
}

// needsFuseOverlay returns true when UID/GID mapping is required, which
// kernel overlayfs cannot do — fuse-overlayfs must be used instead.
func needsFuseOverlay(opts MountOptions) bool {
	return opts.UIDMapping != "" || opts.GIDMapping != "" || opts.SquashUID >= 0 || opts.SquashGID >= 0
}

// MountOverlay mounts the overlay filesystem combining lowerDir (base image rootfs)
// with upperDir (workspace writable layer). lowerDir may be colon-separated
// (multiple overlay layers from podman image inspect).
//
// When MountOptions includes UID/GID mapping, fuse-overlayfs is used because
// kernel overlayfs cannot remap UIDs across users. The image layers in lowerDir
// are owned by the image runtime user (rootless pull), while the container runs
// as the per-app user — fuse-overlayfs translates between these UID spaces.
// When no mapping is needed, kernel-native overlayfs is used.
func MountOverlay(_ context.Context, layout Layout, lowerDir string, mountOpts MountOptions) error {
	// Verify first lowerDir path is accessible (lowerDir may be colon-separated)
	firstPath := strings.SplitN(lowerDir, ":", 2)[0]
	if _, err := os.Stat(firstPath); err != nil {
		return fmt.Errorf("lowerdir not accessible: %w", err)
	}

	// Ensure work directory is empty (required by overlayfs)
	if err := layout.CleanWorkDir(); err != nil {
		return fmt.Errorf("clean work directory: %w", err)
	}

	// Check if already mounted
	mounted, err := isMountedFromMtab(layout.Merged)
	if err != nil {
		return fmt.Errorf("check mount status: %w", err)
	}
	if mounted {
		return ErrAlreadyMounted
	}

	if needsFuseOverlay(mountOpts) {
		return mountFuseOverlay(layout, lowerDir, mountOpts)
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		lowerDir, layout.Upper, layout.Work)

	if err := unix.Mount("overlay", layout.Merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("%w: mount -t overlay: %v", ErrMountFailed, err)
	}

	return nil
}

// mountFuseOverlay mounts the overlay using fuse-overlayfs with UID/GID mapping.
// This enables cross-user access: lower layers owned by the image runtime user
// are remapped to the per-app user's UID space so the container can write
// through the overlay.
func mountFuseOverlay(layout Layout, lowerDir string, mountOpts MountOptions) error {
	fuseOverlayfs, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return fmt.Errorf("%w: fuse-overlayfs not found (required for cross-user workspace overlay): %v", ErrMountFailed, err)
	}

	// Build fuse-overlayfs mount options.
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		lowerDir, layout.Upper, layout.Work)

	// UID/GID mapping: our internal format is colon-separated triplets
	// "from:to:count:from:to:count:..." — fuse-overlayfs expects the same
	// format but as separate -o uidmapping= entries.
	if mountOpts.UIDMapping != "" {
		for _, m := range parseMappingTriplets(mountOpts.UIDMapping) {
			opts += ",uidmapping=" + m
		}
	}
	if mountOpts.GIDMapping != "" {
		for _, m := range parseMappingTriplets(mountOpts.GIDMapping) {
			opts += ",gidmapping=" + m
		}
	}
	if mountOpts.SquashUID >= 0 {
		opts += fmt.Sprintf(",squash_to_uid=%d", mountOpts.SquashUID)
	}
	if mountOpts.SquashGID >= 0 {
		opts += fmt.Sprintf(",squash_to_gid=%d", mountOpts.SquashGID)
	}

	// allow_other: root-mounted FUSE must allow access by the per-app user
	// (container process runs as a different host UID).
	opts += ",allow_other"

	log.Printf("INFO: workspace overlay %s: using fuse-overlayfs for cross-user UID mapping", layout.Merged)

	cmd := exec.Command(fuseOverlayfs, "-o", opts, layout.Merged)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: fuse-overlayfs: %v, output: %s", ErrMountFailed, err, strings.TrimSpace(string(out)))
	}

	// fuse-overlayfs daemonizes; verify the mount appeared.
	for i := 0; i < 10; i++ {
		if ok, _ := isMountedFromMtab(layout.Merged); ok {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("%w: fuse-overlayfs exited but mount not visible in /proc/mounts", ErrMountFailed)
}

// parseMappingTriplets splits a colon-separated mapping string like
// "0:468:1:470:468:1:200000:200200:65536" into triplets like
// ["0:468:1", "470:468:1", "200000:200200:65536"].
func parseMappingTriplets(mapping string) []string {
	parts := strings.Split(mapping, ":")
	var triplets []string
	for i := 0; i+2 < len(parts); i += 3 {
		triplets = append(triplets, parts[i]+":"+parts[i+1]+":"+parts[i+2])
	}
	return triplets
}

// UnmountOverlay unmounts the overlay filesystem.
// It attempts a normal unmount first, then falls back to lazy unmount if busy.
func UnmountOverlay(_ context.Context, layout Layout) error {
	mounted, err := isMountedFromMtab(layout.Merged)
	if err != nil {
		return fmt.Errorf("check mount status: %w", err)
	}
	if !mounted {
		return nil
	}

	if err := unix.Unmount(layout.Merged, 0); err != nil {
		// Fall back to lazy unmount on any failure (e.g. EBUSY from active processes)
		if err := unix.Unmount(layout.Merged, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("%w: %v", ErrUnmountFailed, err)
		}
	}
	return nil
}

// CleanupStaleMount attempts to clean up a stale mount from a previous crash.
// It uses lazy unmount to detach even if busy.
func CleanupStaleMount(_ context.Context, layout Layout) error {
	mounted, err := isMountedFromMtab(layout.Merged)
	if err != nil {
		return nil // Best effort
	}
	if !mounted {
		return nil
	}
	_ = unix.Unmount(layout.Merged, unix.MNT_DETACH)
	return nil
}
