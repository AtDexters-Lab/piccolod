package paths

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCoreRoot_Default(t *testing.T) {
	t.Setenv("PICCOLO_CORE_ROOT", "")
	t.Setenv("PICCOLO_STATE_DIR", "")
	once = &sync.Once{}
	once.Do(func() {}) // exhaust
	coreRoot = defaultCoreRoot
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

func TestPodmanRoot_Default(t *testing.T) {
	once = &sync.Once{}
	once.Do(func() {}) // exhaust
	coreRoot = defaultCoreRoot
	podmanRoot = filepath.Join(defaultCoreRoot, "podman")
	want := filepath.Join(defaultCoreRoot, "podman")
	if got := PodmanRoot(); got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestPodmanRoot_DerivedFromCoreRoot(t *testing.T) {
	t.Setenv("PICCOLO_CORE_ROOT", "/custom/root")
	t.Setenv("PICCOLO_PODMAN_ROOT", "")
	once = &sync.Once{}
	once.Do(resolveRoots)
	if got := PodmanRoot(); got != "/custom/root/podman" {
		t.Fatalf("expected /custom/root/podman, got %s", got)
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

func TestPodmanJoin(t *testing.T) {
	dir := t.TempDir()
	SetPodmanRootForTest(t, dir)
	got := PodmanJoin("imagestore")
	want := filepath.Join(dir, "imagestore")
	if got != want {
		t.Fatalf("PodmanJoin: got %s, want %s", got, want)
	}
}

func TestMountDir(t *testing.T) {
	dir := t.TempDir()
	SetCoreRootForTest(t, dir)
	got := MountDir("app-abc123")
	want := filepath.Join(dir, "mounts", "app-abc123")
	if got != want {
		t.Fatalf("MountDir: got %s, want %s", got, want)
	}
}

func TestVolumeMetaDir(t *testing.T) {
	dir := t.TempDir()
	SetCoreRootForTest(t, dir)
	got := VolumeMetaDir("control-plane")
	want := filepath.Join(dir, "volumes", "control-plane")
	if got != want {
		t.Fatalf("VolumeMetaDir: got %s, want %s", got, want)
	}
}

func TestSetRootsForTest(t *testing.T) {
	core, podman := SetRootsForTest(t)
	if CoreRoot() != core {
		t.Fatalf("CoreRoot mismatch: got %s, want %s", CoreRoot(), core)
	}
	if PodmanRoot() != podman {
		t.Fatalf("PodmanRoot mismatch: got %s, want %s", PodmanRoot(), podman)
	}
	if _, err := os.Stat(core); err != nil {
		t.Fatalf("core dir not created: %v", err)
	}
	if _, err := os.Stat(podman); err != nil {
		t.Fatalf("podman dir not created: %v", err)
	}
}
