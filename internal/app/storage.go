package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"piccolod/internal/persistence"
)

type appVolumeLayout struct {
	VolumeID   string
	MountDir   string
	DiskDir    string
	PodmanRoot string
	DataDir    string
}

func appVolumeID(appName string) string {
	return fmt.Sprintf("app-%s", appName)
}

func ensureDir(path string, mode os.FileMode) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", path)
		}
		return os.Chmod(path, mode)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (m *AppManager) currentVolumeManager() persistence.VolumeManager {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.volumeManager
}

func (m *AppManager) ensureAppVolumeLayout(ctx context.Context, appName string) (appVolumeLayout, error) {
	volumes := m.currentVolumeManager()
	if volumes == nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: volume manager not configured")
	}

	volID := appVolumeID(appName)
	req := persistence.VolumeRequest{
		ID:          volID,
		Class:       persistence.VolumeClassApplication,
		ClusterMode: persistence.ClusterModeStateful,
	}

	handle, err := volumes.EnsureVolume(ctx, req)
	if err != nil {
		return appVolumeLayout{}, err
	}
	if handle.MountDir == "" {
		return appVolumeLayout{}, fmt.Errorf("app manager: volume %s mount dir unavailable", volID)
	}

	// In production we must never write to the expected mount directory unless it is
	// actually mounted (encrypted). Tests can bypass this with PICCOLO_ALLOW_UNMOUNTED_TESTS=1.
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") != "1" {
		mounted, err := persistence.IsMountPoint(handle.MountDir)
		if err != nil {
			return appVolumeLayout{}, fmt.Errorf("app manager: check mountpoint %s: %w", handle.MountDir, err)
		}
		if !mounted {
			if err := volumes.Attach(ctx, handle, persistence.AttachOptions{Role: persistence.VolumeRoleLeader}); err != nil {
				if errors.Is(err, persistence.ErrNotImplemented) {
					return appVolumeLayout{}, fmt.Errorf("app manager: volume attachment not supported")
				}
				return appVolumeLayout{}, err
			}
			mounted, err = persistence.IsMountPoint(handle.MountDir)
			if err != nil {
				return appVolumeLayout{}, fmt.Errorf("app manager: recheck mountpoint %s: %w", handle.MountDir, err)
			}
			if !mounted {
				return appVolumeLayout{}, ErrVolumeUnavailable
			}
		}
	}

	diskDir := filepath.Join(handle.MountDir, "disk")
	podmanRoot := filepath.Join(diskDir, "podman")
	dataDir := filepath.Join(handle.MountDir, "data")

	if err := ensureDir(podmanRoot, 0o700); err != nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: ensure disk dataset for %s: %w", appName, err)
	}
	if err := ensureDir(dataDir, 0o755); err != nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: ensure data dataset for %s: %w", appName, err)
	}

	return appVolumeLayout{
		VolumeID:   volID,
		MountDir:   handle.MountDir,
		DiskDir:    diskDir,
		PodmanRoot: podmanRoot,
		DataDir:    dataDir,
	}, nil
}
