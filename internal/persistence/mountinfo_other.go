//go:build !linux

package persistence

import (
	"fmt"
	"runtime"
	"time"
)

func isMountPoint(string) (bool, error) {
	return false, fmt.Errorf("isMountPoint not supported on %s", runtime.GOOS)
}

func waitForMountReady(string, time.Duration) error {
	return fmt.Errorf("waitForMountReady not supported on %s", runtime.GOOS)
}
