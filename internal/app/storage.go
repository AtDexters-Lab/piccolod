package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/persistence"
	"piccolod/internal/resources/pressure"
	statepaths "piccolod/internal/state/paths"
)

var errAppVolumeObservationUnavailable = errors.New("app volume observation prerequisites unavailable")

type appVolumeLayout struct {
	VolumeID     string
	MountDir     string
	DiskDir      string
	PodmanRoot   string
	DataDir      string
	WorkspaceDir string // Workspace disk directory ({MountDir}/disk/workspace)
}

type StorageResizeResult struct {
	VolumeID   string
	VolumeKind string
	SizeBytes  int64
}

// appVolumeID returns the volume ID for an app instance.
// The instanceID is the unique instance identifier.
func appVolumeID(instanceID string) string {
	return fmt.Sprintf("app-%s", instanceID)
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

// copyFileWithOwner copies src to dst, preserving the executable bit, and chowns to uid:gid.
// The destination is written atomically (write to tmp + rename).
func copyFileWithOwner(src, dst string, uid, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	srcInfo, err := in.Stat()
	if err != nil {
		return err
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chown(tmp, uid, gid); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (m *AppManager) currentVolumeManager() persistence.VolumeManager {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.volumeManager
}

func (m *AppManager) ResizeStorage(ctx context.Context, instanceID string, sizeBytes int64) (StorageResizeResult, error) {
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
	if sizeBytes <= 0 {
		return StorageResizeResult{}, fmt.Errorf("storage size must be positive")
	}
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return StorageResizeResult{}, err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	state, err := m.ensureStateManager()
	if err != nil {
		return StorageResizeResult{}, err
	}
	if err := m.ensureUnlocked(); err != nil {
		return StorageResizeResult{}, err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return StorageResizeResult{}, err
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceResizeStorage); err != nil {
		return StorageResizeResult{}, err
	}
	inst, exists := state.GetApp(instanceID)
	if !exists {
		return StorageResizeResult{}, errAppNotFound(instanceID)
	}
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return StorageResizeResult{}, fmt.Errorf("%w: rootfs manager not available", ErrVolumeUnavailable)
	}

	workspaceVolumeID := rootfs.RootfsVolumeID("workspace", inst.InstanceID)
	if rootfs.RootfsExists(workspaceVolumeID) {
		if err := rootfs.ResizeWorkspace(ctx, workspaceVolumeID, sizeBytes); err != nil {
			return StorageResizeResult{}, fmt.Errorf("resize workspace %s: %w", workspaceVolumeID, err)
		}
		return StorageResizeResult{VolumeID: workspaceVolumeID, VolumeKind: "workspace", SizeBytes: sizeBytes}, nil
	}

	applicationVolumeID := appVolumeID(inst.InstanceID)
	if err := rootfs.ResizeApplication(ctx, applicationVolumeID, sizeBytes); err != nil {
		return StorageResizeResult{}, fmt.Errorf("resize application volume %s: %w", applicationVolumeID, err)
	}
	return StorageResizeResult{VolumeID: applicationVolumeID, VolumeKind: "application", SizeBytes: sizeBytes}, nil
}

// ensureAppVolumeLayout ensures the per-app encrypted volume is mounted and returns its layout.
// The instanceID parameter is the unique instance identifier.
func (m *AppManager) ensureAppVolumeLayout(ctx context.Context, instanceID string) (appVolumeLayout, error) {
	volumes := m.currentVolumeManager()
	if volumes == nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: volume manager not configured")
	}

	volID := appVolumeID(instanceID)
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

	// In production we must never write to the expected mount directory unless
	// the volume is actually attached (kernel-state truth, beyond just the
	// mount-point bit). Tests bypass via PICCOLO_ALLOW_UNMOUNTED_TESTS=1.
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") != "1" {
		state, err := volumes.AttachStateOf(ctx, volID)
		if err != nil {
			if errors.Is(err, persistence.ErrKernelStateAmbiguous) {
				// Probe couldn't get a consistent snapshot — treat as
				// transient unavailability so the caller retries.
				return appVolumeLayout{}, ErrVolumeUnavailable
			}
			if errors.Is(err, persistence.ErrKernelStateCorrupted) {
				// Operator action required; surface distinctly.
				return appVolumeLayout{}, fmt.Errorf("app manager: volume %s requires operator action: %w", volID, err)
			}
			return appVolumeLayout{}, fmt.Errorf("app manager: probe attach state for %s: %w", volID, err)
		}
		if state != persistence.AttachStateAttached {
			if err := volumes.Attach(ctx, handle, persistence.AttachOptions{Role: persistence.VolumeRoleLeader}); err != nil {
				if errors.Is(err, persistence.ErrNotImplemented) {
					return appVolumeLayout{}, fmt.Errorf("app manager: volume attachment not supported")
				}
				return appVolumeLayout{}, err
			}
			// Attach contract: returns nil only when state is Attached. No
			// re-check needed.
		}
	}

	diskDir := filepath.Join(handle.MountDir, "disk")
	podmanRoot := filepath.Join(diskDir, "podman")
	dataDir := filepath.Join(handle.MountDir, "data")
	workspaceDir := filepath.Join(diskDir, "workspace")

	if err := ensureDir(podmanRoot, 0o700); err != nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: ensure disk dataset for %s: %w", instanceID, err)
	}
	if err := ensureDir(dataDir, 0o750); err != nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: ensure data dataset for %s: %w", instanceID, err)
	}

	return appVolumeLayout{
		VolumeID:     volID,
		MountDir:     handle.MountDir,
		DiskDir:      diskDir,
		PodmanRoot:   podmanRoot,
		DataDir:      dataDir,
		WorkspaceDir: workspaceDir,
	}, nil
}

// observeAppVolumeLayout returns the layout only when the existing volume is
// already attached and its datasets exist. It never creates, attaches, or
// chmods storage; reconciliation must cross the explicit bounded readiness
// repair boundary before calling ensureAppVolumeLayout.
func (m *AppManager) observeAppVolumeLayout(ctx context.Context, instanceID string) (appVolumeLayout, error) {
	volumes := m.currentVolumeManager()
	if volumes == nil {
		return appVolumeLayout{}, fmt.Errorf("app manager: volume manager not configured")
	}

	volID := appVolumeID(instanceID)
	mountDir := statepaths.MountDir(volID)
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") == "1" {
		// Test volume managers may intentionally use a noncanonical temp mount.
		handle, err := volumes.EnsureVolume(ctx, persistence.VolumeRequest{
			ID:          volID,
			Class:       persistence.VolumeClassApplication,
			ClusterMode: persistence.ClusterModeStateful,
		})
		if err != nil {
			return appVolumeLayout{}, err
		}
		mountDir = handle.MountDir
	} else if !volumes.IsAttachedAdvisory(ctx, volID) {
		return appVolumeLayout{}, fmt.Errorf("%w: volume %s is not attached", errAppVolumeObservationUnavailable, volID)
	}
	if strings.TrimSpace(mountDir) == "" {
		return appVolumeLayout{}, fmt.Errorf("%w: volume %s mount dir unavailable", errAppVolumeObservationUnavailable, volID)
	}

	diskDir := filepath.Join(mountDir, "disk")
	podmanRoot := filepath.Join(diskDir, "podman")
	dataDir := filepath.Join(mountDir, "data")
	workspaceDir := filepath.Join(diskDir, "workspace")
	for label, path := range map[string]string{
		"mount":       mountDir,
		"podman root": podmanRoot,
		"data":        dataDir,
	} {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return appVolumeLayout{}, fmt.Errorf("%w: %s path %s does not exist", errAppVolumeObservationUnavailable, label, path)
			}
			return appVolumeLayout{}, fmt.Errorf("observe %s path %s: %w", label, path, err)
		}
		if !info.IsDir() {
			return appVolumeLayout{}, fmt.Errorf("%w: %s path %s is not a directory", errAppVolumeObservationUnavailable, label, path)
		}
	}

	return appVolumeLayout{
		VolumeID:     volID,
		MountDir:     mountDir,
		DiskDir:      diskDir,
		PodmanRoot:   podmanRoot,
		DataDir:      dataDir,
		WorkspaceDir: workspaceDir,
	}, nil
}
