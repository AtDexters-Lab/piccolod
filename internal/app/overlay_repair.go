package app

import (
	"bufio"
	"io"
	"log"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"piccolod/internal/state/paths"
)

// staleMount represents a stale overlay mount entry from /proc/mounts.
type staleMount struct {
	path   string
	fstype string
}

// cleanupStaleOverlayMounts unmounts all overlay mounts under piccolo-managed
// directories (core root, podman runroot, and image graphroot).
// Called once at startup before the first reconcile tick.
//
// After a service restart, kernel overlay mounts from the previous session
// remain in the mount table. These must be cleaned up before the new session
// can mount volumes at the same paths.
//
// Strategy: scan /proc/mounts for overlay entries under the state dir and
// lazy-unmount them in reverse order (submounts before parents). Safe because
// no containers are running at startup.
func cleanupStaleOverlayMounts() {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		log.Printf("WARN: overlay repair: cannot read /proc/mounts: %v", err)
		return
	}
	defer f.Close()

	mounts := parseOverlayMounts(f,
		paths.CoreRoot(),            // per-app volume overlays, workspace overlays
		podmanRunRootBase(),         // ALL runRoot overlays (image-root + per-app vol-ids)
		paths.PodmanJoin("image-root"), // image graphroot overlays
	)
	if len(mounts) == 0 {
		return
	}

	// Reverse so submounts (which appear later in /proc/mounts) are unmounted
	// before their parent mounts.
	slices.Reverse(mounts)

	log.Printf("INFO: overlay repair: found %d stale overlay mount(s), cleaning up", len(mounts))
	for _, mp := range mounts {
		if err := unix.Unmount(mp.path, unix.MNT_DETACH); err != nil {
			log.Printf("WARN: overlay repair: failed to unmount %s at %s: %v", mp.fstype, mp.path, err)
		} else {
			log.Printf("INFO: overlay repair: unmounted %s at %s", mp.fstype, mp.path)
		}
	}
}

// parseOverlayMounts reads mount entries and returns all overlay mountpoints
// whose path falls under any of the given prefixes.
func parseOverlayMounts(r io.Reader, prefixes ...string) []staleMount {
	var mounts []staleMount
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mountpoint := fields[1]
		fstype := fields[2]

		if fstype != "overlay" {
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
		mounts = append(mounts, staleMount{path: mountpoint, fstype: fstype})
	}
	if err := scanner.Err(); err != nil {
		log.Printf("WARN: overlay repair: error reading mount entries: %v", err)
	}
	return mounts
}
