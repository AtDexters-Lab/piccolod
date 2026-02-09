package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNoHardcodedCoreRoot(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("unable to determine caller path")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	allowed := map[string]struct{}{
		filepath.Clean(filepath.Join(root, "internal", "state", "paths", "paths.go")): {},
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if _, ok := allowed[filepath.Clean(path)]; ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), defaultCoreRoot) {
			t.Fatalf("hard-coded core root %q found in %s", defaultCoreRoot, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}
}
