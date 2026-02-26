package container

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	overlayOnce    sync.Once
	overlaySupport bool
	overlayErr     error
)

// SupportsNativeOverlay reports whether the running kernel supports native
// (non-FUSE) overlayfs. The result is cached after the first call.
//
// The probe creates a temporary directory, attempts an overlay mount with
// default options (no userxattr — piccolod runs as root), and cleans up
// immediately.
func SupportsNativeOverlay() bool {
	overlayOnce.Do(probeNativeOverlay)
	return overlaySupport
}

// RequireNativeOverlay is like SupportsNativeOverlay but returns an error
// if native overlay is not available. Called at startup to hard-fail early.
func RequireNativeOverlay() error {
	overlayOnce.Do(probeNativeOverlay)
	if !overlaySupport {
		return fmt.Errorf("native overlayfs not available: %v", overlayErr)
	}
	return nil
}

func probeNativeOverlay() {
	probeDir, err := os.MkdirTemp("", "piccolo-overlay-probe-*")
	if err != nil {
		overlayErr = fmt.Errorf("create probe dir: %w", err)
		return
	}
	defer os.RemoveAll(probeDir)

	// Create the required overlay subdirectories.
	lower := probeDir + "/lower"
	upper := probeDir + "/upper"
	work := probeDir + "/work"
	merged := probeDir + "/merged"
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.Mkdir(d, 0o700); err != nil {
			overlayErr = fmt.Errorf("create probe subdir: %w", err)
			return
		}
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	err = unix.Mount("overlay", merged, "overlay", 0, opts)
	if err != nil {
		overlayErr = fmt.Errorf("probe mount: %w", err)
		return
	}
	_ = unix.Unmount(merged, 0)
	overlaySupport = true
}
