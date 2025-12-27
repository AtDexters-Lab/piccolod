package persistence

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestModuleEnsureCoreVolumesIgnoresReconcileErrors(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")

	root := t.TempDir()
	mgr := newFileVolumeManager(root, nil, nil)

	// Seed a corrupted state file so reconcileAllVolumeStates() fails. ensureCoreVolumes
	// should still continue and bring up core volumes so piccolod can start.
	badDir := filepath.Join(root, "volumes", "app-code-server")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatalf("mkdir bad volume dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write bad volume state: %v", err)
	}

	mod := &Module{volumes: mgr}
	if err := mod.ensureCoreVolumes(context.Background()); err != nil {
		t.Fatalf("ensureCoreVolumes: %v", err)
	}
	if mod.bootstrapHandle.ID != "bootstrap" {
		t.Fatalf("expected bootstrap volume handle, got %q", mod.bootstrapHandle.ID)
	}
	if mod.controlHandle.ID != "control" {
		t.Fatalf("expected control volume handle, got %q", mod.controlHandle.ID)
	}
}
