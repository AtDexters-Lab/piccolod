package persistence

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/crypt"
)

func TestModuleAttachVolumesIgnoresRequestCancellation(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")

	cryptoMgr, err := crypt.NewManager(tempDir)
	if err != nil {
		t.Fatalf("crypto manager init: %v", err)
	}
	if err := cryptoMgr.Setup("passphrase"); err != nil {
		t.Fatalf("crypto setup: %v", err)
	}
	if err := cryptoMgr.Unlock("passphrase"); err != nil {
		t.Fatalf("crypto unlock: %v", err)
	}

	handles := make(map[string]VolumeHandle)
	volumes := &stubVolumeManager{}
	volumes.onEnsure = func(_ context.Context, req VolumeRequest) (VolumeHandle, error) {
		if handle, ok := handles[req.ID]; ok {
			return handle, nil
		}
		handle := VolumeHandle{ID: req.ID, MountDir: filepath.Join(tempDir, "mounts", req.ID)}
		handles[req.ID] = handle
		if err := os.MkdirAll(handle.MountDir, 0o700); err != nil {
			t.Fatalf("mkdir mount dir: %v", err)
		}
		return handle, nil
	}
	volumes.onAttach = func(ctx context.Context, handle VolumeHandle, _ AttachOptions) error {
		if ctx.Done() != nil {
			t.Fatalf("expected attach context without cancellation, got Done channel")
		}
		return nil
	}

	mod, err := NewService(Options{
		Crypto:   cryptoMgr,
		StateDir: tempDir,
		Volumes:  volumes,
	})
	if err != nil {
		t.Fatalf("service init: %v", err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	prepareControlCipherDir(t, tempDir)
	if err := mod.setLockState(reqCtx, false); err != nil {
		t.Fatalf("unlock setLockState: %v", err)
	}
}
