package persistence

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
)

func TestModuleEnsureCoreVolumesIgnoresReconcileErrors(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")

	core, _ := paths.SetRootsForTest(t)

	mgr := &luksVolumeManager{stacks: nil}

	// Seed a corrupted volume metadata file so ReconcileAllVolumeStates logs
	// a warning. ensureCoreVolumes should still continue and bring up the
	// control-plane volume handle so piccolod can start.
	badDir := filepath.Join(core, "volumes", "app-code-server")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatalf("mkdir bad volume dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, metadataV2File), []byte("{"), 0o600); err != nil {
		t.Fatalf("write bad volume metadata: %v", err)
	}

	mod := &Module{volumes: mgr}
	if err := mod.ensureCoreVolumes(context.Background()); err != nil {
		t.Fatalf("ensureCoreVolumes: %v", err)
	}
	if mod.controlHandle.ID != "control-plane" {
		t.Fatalf("expected control-plane volume handle, got %q", mod.controlHandle.ID)
	}
}
