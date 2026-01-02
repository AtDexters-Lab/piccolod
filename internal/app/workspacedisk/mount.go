package workspacedisk

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
// This is more reliable than device ID comparison for FUSE mounts.
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
			// fields[1] is the mountpoint
			if fields[1] == mountpoint {
				return true, nil
			}
		}
	}
	return false, scanner.Err()
}

// MountOverlay mounts the overlay filesystem using fuse-overlayfs.
// It combines lowerDir (base image rootfs) with upperDir (workspace writable layer).
func MountOverlay(ctx context.Context, layout Layout, lowerDir string) error {
	// Verify lowerDir exists and is readable
	if _, err := os.Stat(lowerDir); err != nil {
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

	// Find fuse-overlayfs binary
	fuseOverlayfs, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return fmt.Errorf("fuse-overlayfs not found: %w", err)
	}

	// Build fuse-overlayfs command
	// Format: fuse-overlayfs -o lowerdir=...,upperdir=...,workdir=... mountpoint
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		lowerDir, layout.Upper, layout.Work)

	cmd := exec.CommandContext(ctx, fuseOverlayfs, "-o", opts, layout.Merged)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s: %s", ErrMountFailed, err, string(output))
	}

	// Verify mount succeeded
	mounted, err = isMountedFromMtab(layout.Merged)
	if err != nil {
		return fmt.Errorf("verify mount: %w", err)
	}
	if !mounted {
		return fmt.Errorf("%w: mount command succeeded but mountpoint not found in /proc/mounts", ErrMountFailed)
	}

	return nil
}

// UnmountOverlay unmounts the overlay filesystem.
// It attempts a normal unmount first, then falls back to lazy unmount if busy.
func UnmountOverlay(ctx context.Context, layout Layout) error {
	mounted, err := isMountedFromMtab(layout.Merged)
	if err != nil {
		return fmt.Errorf("check mount status: %w", err)
	}
	if !mounted {
		return nil // Already unmounted
	}

	// Try normal unmount first
	cmd := exec.CommandContext(ctx, "fusermount3", "-u", layout.Merged)
	if err := cmd.Run(); err != nil {
		// fusermount3 might not exist, try fusermount
		cmd = exec.CommandContext(ctx, "fusermount", "-u", layout.Merged)
		if err := cmd.Run(); err != nil {
			// Fall back to lazy unmount
			if err := unix.Unmount(layout.Merged, unix.MNT_DETACH); err != nil {
				return fmt.Errorf("%w: %v", ErrUnmountFailed, err)
			}
		}
	}

	return nil
}

// CleanupStaleMount attempts to clean up a stale mount from a previous crash.
// It uses lazy unmount to detach even if busy.
func CleanupStaleMount(ctx context.Context, layout Layout) error {
	mounted, err := isMountedFromMtab(layout.Merged)
	if err != nil {
		// Best effort - ignore read errors
		return nil
	}
	if !mounted {
		return nil
	}

	// Try fusermount first (more graceful)
	cmd := exec.CommandContext(ctx, "fusermount3", "-u", "-z", layout.Merged)
	if cmd.Run() == nil {
		return nil
	}
	cmd = exec.CommandContext(ctx, "fusermount", "-u", "-z", layout.Merged)
	if cmd.Run() == nil {
		return nil
	}

	// Fall back to lazy kernel unmount
	_ = unix.Unmount(layout.Merged, unix.MNT_DETACH)
	return nil
}
