package persistence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/state/paths"
)

func writeTestMeta(t *testing.T, root, volID string, meta interface{}) {
	t.Helper()
	dir := filepath.Join(root, "volumes", volID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, metadataV2File), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanAllVolumes_Empty(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)

	m := &luksVolumeManager{}
	vols, err := m.ScanAllVolumes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vols) != 0 {
		t.Fatalf("expected empty, got %d", len(vols))
	}
}

func TestScanAllVolumes_V3Volumes(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)

	// Golden LV.
	writeTestMeta(t, root, "golden-abc123", &volumeMetaV3{
		Version:         3,
		Type:            "golden",
		LVName:          "golden-abc123",
		VGName:          "piccolo-data-vg",
		SizeBytes:       1073741824,
		FSType:          "btrfs",
		ReadOnly:        true,
		BaseImageDigest: "sha256:abc123",
		BaseImageRef:    "docker.io/library/nginx:alpine",
		FlattenComplete: "2026-02-28T10:30:00Z",
	})

	// Workspace snapshotted from golden, cloned from another workspace.
	writeTestMeta(t, root, "ws-myworkspace", &volumeMetaV3{
		Version:   3,
		Type:      "workspace",
		LVName:    "ws-myworkspace",
		VGName:    "piccolo-data-vg",
		SizeBytes: 10737418240,
		FSType:    "btrfs",
		GoldenLV:  "golden-abc123",
		CloneOf:   "ws-origin",
	})

	// Service rootfs.
	writeTestMeta(t, root, "svc-rootfs-app1", &volumeMetaV3{
		Version:   3,
		Type:      "service-rootfs",
		LVName:    "svc-rootfs-app1",
		VGName:    "piccolo-data-vg",
		SizeBytes: 2147483648,
		FSType:    "btrfs",
		GoldenLV:  "golden-abc123",
	})

	m := &luksVolumeManager{}
	vols, err := m.ScanAllVolumes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vols) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(vols))
	}

	byID := make(map[string]VolumeDiagnostic)
	for _, v := range vols {
		byID[v.VolumeID] = v
	}

	golden := byID["golden-abc123"]
	if golden.Type != "golden" || golden.ReadOnly != true || golden.FlattenComplete == "" {
		t.Errorf("golden LV fields incorrect: %+v", golden)
	}
	if golden.BaseImageRef != "docker.io/library/nginx:alpine" {
		t.Errorf("golden image ref: got %q", golden.BaseImageRef)
	}

	ws := byID["ws-myworkspace"]
	if ws.Type != "workspace" || ws.GoldenLV != "golden-abc123" || ws.CloneOf != "ws-origin" {
		t.Errorf("workspace fields incorrect: %+v", ws)
	}

	svc := byID["svc-rootfs-app1"]
	if svc.Type != "service-rootfs" || svc.GoldenLV != "golden-abc123" {
		t.Errorf("service rootfs fields incorrect: %+v", svc)
	}
}

func TestScanAllVolumes_V2Volume(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)

	writeTestMeta(t, root, "vol-mydata", &volumeMetaV2{
		Version:    2,
		Type:       "luks-thinlv",
		WrappedKey: "secret",
		Nonce:      "nonce",
		LVName:     "vol-mydata",
		VGName:     "piccolo-data-vg",
		SizeBytes:  5368709120,
		FSType:     "ext4",
	})

	m := &luksVolumeManager{}
	vols, err := m.ScanAllVolumes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("expected 1, got %d", len(vols))
	}

	v := vols[0]
	if v.VolumeID != "vol-mydata" {
		t.Errorf("volume ID: got %q", v.VolumeID)
	}
	if v.Type != "luks-thinlv" {
		t.Errorf("type: got %q", v.Type)
	}
	if v.FSType != "ext4" {
		t.Errorf("fstype: got %q", v.FSType)
	}
	if v.SizeBytes != 5368709120 {
		t.Errorf("size: got %d", v.SizeBytes)
	}
}

func TestScanAllVolumes_CorruptedSkipped(t *testing.T) {
	root := t.TempDir()
	paths.SetCoreRootForTest(t, root)

	// Write invalid JSON.
	dir := filepath.Join(root, "volumes", "bad-vol")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "piccolo.volume.json"), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write a valid volume alongside.
	writeTestMeta(t, root, "good-vol", &volumeMetaV3{
		Version:   3,
		Type:      "ephemeral",
		LVName:    "eph-good",
		VGName:    "piccolo-data-vg",
		SizeBytes: 1073741824,
		FSType:    "btrfs",
	})

	m := &luksVolumeManager{}
	vols, err := m.ScanAllVolumes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("expected 1 (corrupted skipped), got %d", len(vols))
	}
	if vols[0].VolumeID != "good-vol" {
		t.Errorf("expected good-vol, got %q", vols[0].VolumeID)
	}
}
