package pcv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveNodeID_Deterministic(t *testing.T) {
	// Reads real /etc/machine-id — if missing (containers), skip.
	if _, err := os.ReadFile("/etc/machine-id"); err != nil {
		t.Skip("no /etc/machine-id available")
	}

	id1, err := DeriveNodeID()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	id2, err := DeriveNodeID()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("not deterministic: %q vs %q", id1, id2)
	}
	if len(id1) != nodeIDLength {
		t.Fatalf("expected %d hex chars, got %d: %q", nodeIDLength, len(id1), id1)
	}
}

func TestReadTrimmedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test")
	if err := os.WriteFile(path, []byte("  hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTrimmedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("expected %q, got %q", "hello", got)
	}
}
