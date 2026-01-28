//go:build linux

package persistence

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	fsImmutableFl = 0x00000010
)

func protectMountDir(path string) error {
	if !mountpointGuardEnabled() {
		return nil
	}
	return setMountDirImmutable(path, true)
}

func unprotectMountDir(path string) error {
	if !mountpointGuardEnabled() {
		return nil
	}
	return setMountDirImmutable(path, false)
}

func setMountDirImmutable(path string, immutable bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		log.Printf("DEBUG: mountpoint guard: skipping empty path")
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("mountpoint guard: open %s: %w", path, err)
	}
	defer f.Close()

	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil
		}
		return fmt.Errorf("mountpoint guard: get flags for %s: %w", path, err)
	}

	newFlags := flags
	if immutable {
		newFlags |= fsImmutableFl
	} else {
		newFlags &^= fsImmutableFl
	}
	if newFlags == flags {
		return nil
	}

	if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, newFlags); err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil
		}
		return fmt.Errorf("mountpoint guard: set immutable=%t for %s: %w", immutable, path, err)
	}
	return nil
}
