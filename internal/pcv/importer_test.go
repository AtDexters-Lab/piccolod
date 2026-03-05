package pcv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
)

func createTestPCVArchive(t *testing.T, destPath string, files map[string][]byte) string {
	t.Helper()
	f, err := os.Create(destPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(data)),
			Mode: 0o600,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}

	tw.Close()
	gw.Close()

	return destPath
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func TestImporter_ImportSuccess(t *testing.T) {
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)

	// Create minimal core structure (no existing control plane).
	os.MkdirAll(filepath.Join(coreRoot, "recovery", "import-staging"), 0o700)

	// Create archive with LUKS loop file and crypto files.
	archivePath := filepath.Join(t.TempDir(), "test.pcv")
	createTestPCVArchive(t, archivePath, map[string][]byte{
		"control-plane.luks":                        []byte("LUKS-encrypted-data"),
		"crypto/keyset.json":                        []byte(`{"sdek":"sealed"}`),
		"volumes/control-plane/piccolo.volume.json": []byte(`{"pass":"wrapped"}`),
	})

	hash := hashFile(t, archivePath)
	imp := NewImporter(&stubRunner{})
	err := imp.Import(context.Background(), archivePath, hash)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Verify LUKS loop file was extracted.
	data, err := os.ReadFile(filepath.Join(coreRoot, "control-plane.luks"))
	if err != nil {
		t.Fatalf("loop file missing: %v", err)
	}
	if string(data) != "LUKS-encrypted-data" {
		t.Errorf("loop file content = %q", data)
	}

	data, err = os.ReadFile(filepath.Join(coreRoot, "crypto", "keyset.json"))
	if err != nil {
		t.Fatalf("keyset.json missing: %v", err)
	}
	if string(data) != `{"sdek":"sealed"}` {
		t.Errorf("keyset.json content = %q", data)
	}
}

func TestImporter_RejectsFunctionalControlPlane(t *testing.T) {
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)

	// Create a functional control plane (keyset + loop file).
	os.MkdirAll(filepath.Join(coreRoot, "crypto"), 0o700)
	os.WriteFile(filepath.Join(coreRoot, "crypto", "keyset.json"), []byte("key"), 0o600)
	os.WriteFile(filepath.Join(coreRoot, "control-plane.luks"), []byte("encrypted"), 0o600)

	archivePath := filepath.Join(t.TempDir(), "test.pcv")
	createTestPCVArchive(t, archivePath, map[string][]byte{
		"crypto/keyset.json": []byte("new-key"),
	})

	imp := NewImporter(&stubRunner{})
	err := imp.Import(context.Background(), archivePath, "")
	if err != ErrControlPlaneExists {
		t.Fatalf("expected ErrControlPlaneExists, got %v", err)
	}
}

func TestImporter_HashMismatch(t *testing.T) {
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)

	archivePath := filepath.Join(t.TempDir(), "test.pcv")
	createTestPCVArchive(t, archivePath, map[string][]byte{
		"crypto/keyset.json": []byte("data"),
	})

	imp := NewImporter(&stubRunner{})
	err := imp.Import(context.Background(), archivePath, "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected hash mismatch error")
	}
}

func TestImporter_PathTraversalRejected(t *testing.T) {
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)
	os.MkdirAll(filepath.Join(coreRoot, "recovery", "import-staging"), 0o700)

	// Create an archive with a path traversal entry.
	archivePath := filepath.Join(t.TempDir(), "malicious.pcv")
	f, _ := os.Create(archivePath)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	tw.WriteHeader(&tar.Header{
		Name: "../../../etc/passwd",
		Size: 4,
		Mode: 0o644,
	})
	tw.Write([]byte("evil"))
	tw.Close()
	gw.Close()
	f.Close()

	imp := NewImporter(&stubRunner{})
	err := imp.Import(context.Background(), archivePath, "")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestImporter_BackupsCorruptLoopFile(t *testing.T) {
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)

	// Create a corrupt control plane (keyset missing but loop file exists).
	os.WriteFile(filepath.Join(coreRoot, "control-plane.luks"), []byte("corrupt"), 0o600)
	os.MkdirAll(filepath.Join(coreRoot, "recovery", "import-staging"), 0o700)

	archivePath := filepath.Join(t.TempDir(), "test.pcv")
	createTestPCVArchive(t, archivePath, map[string][]byte{
		"control-plane.luks": []byte("fresh-encrypted-data"),
		"crypto/keyset.json": []byte("key"),
	})

	imp := NewImporter(&stubRunner{})
	err := imp.Import(context.Background(), archivePath, "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Verify old loop file was backed up.
	entries, _ := os.ReadDir(coreRoot)
	bakFound := false
	for _, e := range entries {
		if len(e.Name()) > len("control-plane.luks.bak.") && e.Name()[:len("control-plane.luks.bak.")] == "control-plane.luks.bak." {
			bakFound = true
		}
	}
	if !bakFound {
		t.Error("expected backup of corrupt loop file")
	}

	// Verify new content applied.
	data, _ := os.ReadFile(filepath.Join(coreRoot, "control-plane.luks"))
	if string(data) != "fresh-encrypted-data" {
		t.Errorf("expected fresh content, got %q", data)
	}
}
