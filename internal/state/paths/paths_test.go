package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCoreRoot_Default(t *testing.T) {
	t.Setenv("PICCOLO_CORE_ROOT", "")
	t.Setenv("PICCOLO_DATA_ROOT", "")
	t.Setenv("PICCOLO_STATE_DIR", "")
	once.Do(func() {}) // exhaust
	coreRoot = defaultCoreRoot
	dataRoot = defaultDataRoot
	if got := CoreRoot(); got != defaultCoreRoot {
		t.Fatalf("expected %s, got %s", defaultCoreRoot, got)
	}
}

func TestCoreRoot_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	coreDir := filepath.Join(dir, "core")
	SetCoreRootForTest(t, coreDir)
	if got := CoreRoot(); got != coreDir {
		t.Fatalf("expected %s, got %s", coreDir, got)
	}
}

func TestDataRoot_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	SetDataRootForTest(t, dataDir)
	if got := DataRoot(); got != dataDir {
		t.Fatalf("expected %s, got %s", dataDir, got)
	}
}

func TestCoreJoin(t *testing.T) {
	dir := t.TempDir()
	SetCoreRootForTest(t, dir)
	got := CoreJoin("crypto", "keyset.json")
	want := filepath.Join(dir, "crypto", "keyset.json")
	if got != want {
		t.Fatalf("CoreJoin: got %s, want %s", got, want)
	}
}

func TestDataJoin(t *testing.T) {
	dir := t.TempDir()
	SetDataRootForTest(t, dir)
	got := DataJoin("pool", "data")
	want := filepath.Join(dir, "pool", "data")
	if got != want {
		t.Fatalf("DataJoin: got %s, want %s", got, want)
	}
}

func TestSetRootsForTest(t *testing.T) {
	core, data := SetRootsForTest(t)
	if CoreRoot() != core {
		t.Fatalf("CoreRoot mismatch: got %s, want %s", CoreRoot(), core)
	}
	if DataRoot() != data {
		t.Fatalf("DataRoot mismatch: got %s, want %s", DataRoot(), data)
	}
	if _, err := os.Stat(core); err != nil {
		t.Fatalf("core dir not created: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
}
