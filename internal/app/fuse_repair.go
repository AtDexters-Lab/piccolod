package app

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"piccolod/internal/state/paths"
)

// fuseMount represents a FUSE mount entry from /proc/mounts.
type fuseMount struct {
	path   string
	fstype string
}

// cleanupStaleFUSEMounts unmounts all FUSE mounts under piccolo-managed
// directories (core root, podman runroot, and image graphroot).
// Called once at startup before the first reconcile tick.
//
// After a service restart, systemd kills FUSE daemon processes (fuse-overlayfs,
// gocryptfs, etc.) in the unit's cgroup, leaving stale FUSE mount entries in the
// kernel mount table. Accessing these stale mounts returns ENOTCONN ("Transport
// endpoint is not connected"). Critically, stat(2)/fstatat(2) succeeds on stale
// FUSE mountpoints (kernel returns cached VFS attributes), so os.Stat is not a
// reliable liveness probe — only statx(2) and actual I/O (readdir, open) fail.
//
// Strategy: scan /proc/mounts for all fuse.* entries under the state dir and
// lazy-unmount them in reverse order (submounts before parents). Safe because
// no containers are running at startup.
func cleanupStaleFUSEMounts(ctx context.Context) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		log.Printf("WARN: fuse repair: cannot read /proc/mounts: %v", err)
		return
	}
	defer f.Close()

	mounts := parseFUSEMounts(f,
		paths.CoreRoot(),                               // gocryptfs, per-app graphroot overlays, workspace overlays
		podmanRunRootBase(),                             // ALL runRoot overlays (image-root + per-app vol-ids)
		paths.DataJoin("node", "podman", "image-root"),  // image graphroot overlays
	)
	if len(mounts) == 0 {
		return
	}

	// Reverse so submounts (which appear later in /proc/mounts) are unmounted
	// before their parent mounts.
	slices.Reverse(mounts)

	log.Printf("INFO: fuse repair: found %d stale FUSE mount(s), cleaning up", len(mounts))
	for _, mp := range mounts {
		if lazyUnmount(ctx, mp.path) {
			log.Printf("INFO: fuse repair: unmounted %s at %s", mp.fstype, mp.path)
		} else {
			log.Printf("WARN: fuse repair: failed to unmount %s at %s", mp.fstype, mp.path)
		}
	}
}

// parseFUSEMounts reads mount entries and returns all fuse.* mountpoints
// whose path falls under any of the given prefixes.
func parseFUSEMounts(r io.Reader, prefixes ...string) []fuseMount {
	var mounts []fuseMount
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mountpoint := fields[1]
		fstype := fields[2]

		if !strings.HasPrefix(fstype, "fuse.") {
			continue
		}
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(mountpoint, prefix+"/") || mountpoint == prefix {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		mounts = append(mounts, fuseMount{path: mountpoint, fstype: fstype})
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
