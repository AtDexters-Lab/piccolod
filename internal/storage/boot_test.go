package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyTransport(t *testing.T) {
	tests := []struct {
		transport string
		want      BootMode
	}{
		{"usb", BootModeUSB},
		{"USB", BootModeUSB},
		{"sata", BootModeInternal},
		{"nvme", BootModeInternal},
		{"ata", BootModeInternal},
		{"mmc", BootModeInternal},
		{"", BootModeUnknown},
		{"virtio", BootModeUnknown},
		{"iscsi", BootModeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.transport, func(t *testing.T) {
			got := classifyTransport(tt.transport)
			if got != tt.want {
				t.Errorf("classifyTransport(%q) = %q, want %q", tt.transport, got, tt.want)
			}
		})
	}
}

func TestDetectBootMode_OverrideWithoutSentinel(t *testing.T) {
	t.Setenv("PICCOLO_BOOT_MODE_OVERRIDE", "internal")
	// No sentinel file exists, should error
	_, err := DetectBootMode(context.Background(), &fakeRunner{})
	if err == nil {
		t.Fatal("expected error when sentinel file is missing")
	}
}

func TestDetectBootMode_OverrideWithSentinel(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "piccolo-test-image")
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	origSentinel := testImageSentinel
	setTestImageSentinelForTest(sentinel)
	t.Cleanup(func() { setTestImageSentinelForTest(origSentinel) })

	t.Setenv("PICCOLO_BOOT_MODE_OVERRIDE", "usb")
	mode, err := DetectBootMode(context.Background(), &fakeRunner{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != BootModeUSB {
		t.Fatalf("expected usb, got %s", mode)
	}
}

func TestDetectBootMode_InvalidOverride(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "piccolo-test-image")
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	origSentinel := testImageSentinel
	setTestImageSentinelForTest(sentinel)
	t.Cleanup(func() { setTestImageSentinelForTest(origSentinel) })

	t.Setenv("PICCOLO_BOOT_MODE_OVERRIDE", "invalid-mode")
	_, err := DetectBootMode(context.Background(), &fakeRunner{})
	if err == nil {
		t.Fatal("expected error for invalid override value")
	}
}

func TestDetectBootMode_WithFakeRunner(t *testing.T) {
	t.Setenv("PICCOLO_BOOT_MODE_OVERRIDE", "")
	run := &fakeRunner{
		Outputs: map[string]string{
			"findmnt -nro SOURCE /": "/dev/sda2",
			"lsblk -ndo TRAN /dev/sda": "sata",
		},
	}
	mode, err := DetectBootMode(context.Background(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != BootModeInternal {
		t.Fatalf("expected internal, got %s", mode)
	}
}

func TestDetectBootMode_USBTransport(t *testing.T) {
	t.Setenv("PICCOLO_BOOT_MODE_OVERRIDE", "")
	run := &fakeRunner{
		Outputs: map[string]string{
			"findmnt -nro SOURCE /": "/dev/sdb1",
			"lsblk -ndo TRAN /dev/sdb": "usb",
		},
	}
	mode, err := DetectBootMode(context.Background(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != BootModeUSB {
		t.Fatalf("expected usb, got %s", mode)
	}
}

func TestDetectBootMode_NVMe(t *testing.T) {
	t.Setenv("PICCOLO_BOOT_MODE_OVERRIDE", "")
	run := &fakeRunner{
		Outputs: map[string]string{
			"findmnt -nro SOURCE /":       "/dev/nvme0n1p2",
			"lsblk -ndo TRAN /dev/nvme0n1": "nvme",
		},
	}
	mode, err := DetectBootMode(context.Background(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != BootModeInternal {
		t.Fatalf("expected internal, got %s", mode)
	}
}
