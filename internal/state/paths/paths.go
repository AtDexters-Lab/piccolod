package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	defaultCoreRoot = "/piccolo-core"
	defaultDataRoot = "/piccolo-data"
)

var (
	coreRoot string
	dataRoot string
	once     sync.Once
)

func resolveRoots() {
	if v := os.Getenv("PICCOLO_STATE_DIR"); v != "" {
		fmt.Fprintf(os.Stderr, "FATAL: PICCOLO_STATE_DIR is no longer supported; use PICCOLO_CORE_ROOT / PICCOLO_DATA_ROOT.\n")
		os.Exit(1)
	}
	coreRoot = filepath.Clean(envOr("PICCOLO_CORE_ROOT", defaultCoreRoot))
	dataRoot = filepath.Clean(envOr("PICCOLO_DATA_ROOT", defaultDataRoot))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// CoreRoot returns the root of the piccolo-core subvolume.
func CoreRoot() string {
	once.Do(resolveRoots)
	return coreRoot
}

// DataRoot returns the root of the piccolo-data partition.
func DataRoot() string {
	once.Do(resolveRoots)
	return dataRoot
}

// CoreJoin resolves a path relative to the core root.
func CoreJoin(parts ...string) string {
	all := append([]string{CoreRoot()}, parts...)
	return filepath.Join(all...)
}

// DataJoin resolves a path relative to the data root.
func DataJoin(parts ...string) string {
	all := append([]string{DataRoot()}, parts...)
	return filepath.Join(all...)
}

// SetCoreRootForTest overrides the core root for the duration of a test.
func SetCoreRootForTest(t *testing.T, dir string) {
	t.Helper()
	prev := coreRoot
	prevOnce := once
	coreRoot = dir
	once = sync.Once{} // prevent re-resolve from clobbering
	once.Do(func() {}) // exhaust the once
	t.Cleanup(func() {
		coreRoot = prev
		once = prevOnce
	})
}

// SetDataRootForTest overrides the data root for the duration of a test.
func SetDataRootForTest(t *testing.T, dir string) {
	t.Helper()
	prev := dataRoot
	prevOnce := once
	dataRoot = dir
	once = sync.Once{}
	once.Do(func() {})
	t.Cleanup(func() {
		dataRoot = prev
		once = prevOnce
	})
}

// SetRootsForTest creates temp directories for core and data roots and returns their paths.
func SetRootsForTest(t *testing.T) (core, data string) {
	t.Helper()
	core = filepath.Join(t.TempDir(), "core")
	data = filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(core, 0o755); err != nil {
		t.Fatalf("create test core root: %v", err)
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatalf("create test data root: %v", err)
	}
	SetCoreRootForTest(t, core)
	SetDataRootForTest(t, data)
	return core, data
}
