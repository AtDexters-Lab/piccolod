//go:build linux

package persistence

import (
	"fmt"
	"os"
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

func isMountPoint(mountPoint string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	target := filepath.Clean(mountPoint)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 5 {
			continue
		}
		pathField := decodeMountPoint(fields[4])
		if pathField == target {
			return true, nil
		}
	}
	return false, nil
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
