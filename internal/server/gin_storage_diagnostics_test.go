package server

import (
	"testing"

	"piccolod/internal/persistence"
)

func TestBuildGoldenInventory(t *testing.T) {
	volumes := []persistence.VolumeDiagnostic{
		{VolumeID: "golden-abc", Type: "golden", LVName: "golden-abc", BaseImageRef: "nginx:alpine", BaseImageDigest: "sha256:abc", SizeBytes: 1000, FlattenComplete: "2026-01-01T00:00:00Z"},
		{VolumeID: "golden-def", Type: "golden", LVName: "golden-def", BaseImageRef: "redis:7", BaseImageDigest: "sha256:def", SizeBytes: 2000},
		{VolumeID: "ws-app1", Type: "workspace", GoldenLV: "golden-abc", SizeBytes: 5000},
		{VolumeID: "ws-app2", Type: "workspace", GoldenLV: "golden-abc", SizeBytes: 5000},
		{VolumeID: "svc-rootfs-svc1", Type: "service-rootfs", GoldenLV: "golden-def", SizeBytes: 3000},
	}

	inventory := buildGoldenInventory(volumes)
	if len(inventory) != 2 {
		t.Fatalf("expected 2 golden LVs, got %d", len(inventory))
	}

	byID := make(map[string]goldenLVDiagnostic)
	for _, g := range inventory {
		byID[g.VolumeID] = g
	}

	abc := byID["golden-abc"]
	if abc.SnapshotCount != 2 {
		t.Errorf("golden-abc snapshot count: got %d, want 2", abc.SnapshotCount)
	}
	if abc.ImageRef != "nginx:alpine" {
		t.Errorf("golden-abc image ref: got %q", abc.ImageRef)
	}
	if abc.FlattenComplete != "2026-01-01T00:00:00Z" {
		t.Errorf("golden-abc flatten_complete: got %q", abc.FlattenComplete)
	}

	def := byID["golden-def"]
	if def.SnapshotCount != 1 {
		t.Errorf("golden-def snapshot count: got %d, want 1", def.SnapshotCount)
	}
}

func TestBuildTypeSummary(t *testing.T) {
	volumes := []persistence.VolumeDiagnostic{
		{Type: "golden", SizeBytes: 1000},
		{Type: "golden", SizeBytes: 2000},
		{Type: "workspace", SizeBytes: 5000},
		{Type: "service-rootfs", SizeBytes: 3000},
		{Type: "service-rootfs", SizeBytes: 4000},
		{Type: "service-rootfs", SizeBytes: 1000},
	}

	summary := buildTypeSummary(volumes)

	if s := summary["golden"]; s.Count != 2 || s.TotalBytes != 3000 {
		t.Errorf("golden: count=%d total=%d", s.Count, s.TotalBytes)
	}
	if s := summary["workspace"]; s.Count != 1 || s.TotalBytes != 5000 {
		t.Errorf("workspace: count=%d total=%d", s.Count, s.TotalBytes)
	}
	if s := summary["service-rootfs"]; s.Count != 3 || s.TotalBytes != 8000 {
		t.Errorf("service-rootfs: count=%d total=%d", s.Count, s.TotalBytes)
	}
}

func TestBuildCloneGraph(t *testing.T) {
	volumes := []persistence.VolumeDiagnostic{
		{VolumeID: "ws-origin", Type: "workspace"},
		{VolumeID: "ws-clone1", Type: "workspace", CloneOf: "ws-origin"},
		{VolumeID: "ws-clone2", Type: "workspace", CloneOf: "ws-origin"},
		{VolumeID: "ws-other", Type: "workspace"},
	}

	graph := buildCloneGraph(volumes)

	clones, ok := graph["ws-origin"]
	if !ok {
		t.Fatal("expected ws-origin in clone graph")
	}
	if len(clones) != 2 {
		t.Fatalf("expected 2 clones, got %d", len(clones))
	}

	// ws-other should not appear (no CloneOf).
	if _, exists := graph["ws-other"]; exists {
		t.Error("ws-other should not be in clone graph")
	}

	// No self-references.
	if _, exists := graph["ws-clone1"]; exists {
		t.Error("ws-clone1 should not be an origin in clone graph")
	}
}
