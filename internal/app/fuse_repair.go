package app

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"

	"piccolod/internal/state/paths"
)

// cleanupStaleFUSEMounts unmounts all fuse-overlayfs mounts under the state directory.
// Called once at startup before the first reconcile tick.
//
// After a service restart, systemd kills fuse-overlayfs daemon processes in the
// unit's cgroup, leaving stale FUSE mount entries in the kernel mount table.
// Accessing these stale mounts returns ENOTCONN ("Transport endpoint is not connected").
// Critically, stat(2)/fstatat(2) succeeds on stale FUSE mountpoints (kernel returns
// cached VFS attributes), so os.Stat is not a reliable liveness probe — only statx(2)
// and actual I/O (readdir, open) fail.
//
// This affects the podman image-root overlay driver's merged directories, which are
// used as lowerdir for workspace disk overlays. The stale mounts cause all workspace
// apps to fail with "cannot read lower dirs: Transport endpoint is not connected".
//
// Strategy: scan /proc/mounts for fuse-overlayfs entries under the state dir and
// lazy-unmount them all. Safe because no containers are running at startup.
func cleanupStaleFUSEMounts(ctx context.Context) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		log.Printf("WARN: fuse repair: cannot read /proc/mounts: %v", err)
		return
	}
	defer f.Close()

	mounts := parseFUSEMounts(f, paths.Root())
	if len(mounts) == 0 {
		return
	}

	log.Printf("INFO: fuse repair: found %d stale fuse-overlayfs mount(s), cleaning up", len(mounts))
	for _, mp := range mounts {
		if lazyUnmount(ctx, mp) {
			log.Printf("INFO: fuse repair: unmounted %s", mp)
		} else {
			log.Printf("WARN: fuse repair: failed to unmount %s", mp)
		}
	}
}

// parseFUSEMounts reads mount entries and returns all fuse.fuse-overlayfs
// mountpoints whose path falls under the given prefix.
func parseFUSEMounts(r io.Reader, prefix string) []string {
	var mounts []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mountpoint := fields[1]
		fstype := fields[2]

		if fstype != "fuse.fuse-overlayfs" {
			continue
		}
		if !strings.HasPrefix(mountpoint, prefix+"/") && mountpoint != prefix {
			continue
		}
		mounts = append(mounts, mountpoint)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("WARN: fuse repair: error reading mount entries: %v", err)
	}
	return mounts
}

// lazyUnmount attempts a lazy unmount of a mountpoint.
// Tries fusermount3, fusermount, then kernel MNT_DETACH.
func lazyUnmount(ctx context.Context, mountpoint string) bool {
	if cmd := exec.CommandContext(ctx, "fusermount3", "-u", "-z", mountpoint); cmd.Run() == nil {
		return true
	}
	if cmd := exec.CommandContext(ctx, "fusermount", "-u", "-z", mountpoint); cmd.Run() == nil {
		return true
	}
	if err := unix.Unmount(mountpoint, unix.MNT_DETACH); err != nil {
		log.Printf("WARN: fuse repair: kernel MNT_DETACH %s: %v", mountpoint, err)
		return false
	}
	return true
}
