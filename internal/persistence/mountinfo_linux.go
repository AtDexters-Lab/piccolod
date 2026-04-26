//go:build linux

package persistence

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

var mountPointReplacer = strings.NewReplacer(
	`\040`, " ",
	`\011`, "\t",
	`\012`, "\n",
	`\134`, `\`,
)

// isMountPoint reports whether mountPoint appears in /proc/self/mountinfo.
// Thin wrapper over readMountInfo (the kernel-state probe's parser) so the
// mountinfo column-format knowledge lives in exactly one place.
func isMountPoint(mountPoint string) (bool, error) {
	mounts, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	_, ok := mounts[filepath.Clean(mountPoint)]
	return ok, nil
}

// mountAtPath reports whether mountPoint is a mount point and, if so,
// what the mount entry contains. Used by transition idempotency shortcuts
// to verify ownership before treating an existing mount as ours.
func mountAtPath(mountPoint string) (bool, mountEntry, error) {
	mounts, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		return false, mountEntry{}, err
	}
	entry, ok := mounts[filepath.Clean(mountPoint)]
	return ok, entry, nil
}

func decodeMountPoint(raw string) string {
	return mountPointReplacer.Replace(raw)
}

func waitForMountReady(mountPoint string, timeout time.Duration) error {
	mountPoint = filepath.Clean(mountPoint)
	deadline := time.Now().Add(timeout)
	for {
		mounted, err := isMountPoint(mountPoint)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for mount %s", mountPoint)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
