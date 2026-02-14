package onboarding

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mockRunner is a test CommandRunner that returns canned responses.
type mockRunner struct {
	outputs map[string]string
	errors  map[string]error
}

func newMockRunner() *mockRunner {
	return &mockRunner{
		outputs: make(map[string]string),
		errors:  make(map[string]error),
	}
}

func (m *mockRunner) key(name string, args ...string) string {
	return name + " " + strings.Join(args, " ")
}

func (m *mockRunner) Set(output string, name string, args ...string) {
	m.outputs[m.key(name, args...)] = output
}

func (m *mockRunner) SetError(err error, name string, args ...string) {
	m.errors[m.key(name, args...)] = err
}

func (m *mockRunner) Run(ctx context.Context, name string, args ...string) error {
	k := m.key(name, args...)
	if err, ok := m.errors[k]; ok {
		return err
	}
	return nil
}

func (m *mockRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	k := m.key(name, args...)
	if err, ok := m.errors[k]; ok {
		return nil, err
	}
	if out, ok := m.outputs[k]; ok {
		return []byte(out), nil
	}
	return []byte(""), nil
}

func (m *mockRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	k := m.key(name, args...)
	if err, ok := m.errors[k]; ok {
		return err
	}
	return nil
}

func TestDiscoverInternalDisks(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	// lsblk output with three disks: sda (sata), sdb (usb boot), sdc (nvme)
	lsblkJSON := `{
		"blockdevices": [
			{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung SSD 870","type":"disk"},
			{"name":"sdb","size":32212254720,"tran":"usb","model":"USB Flash","type":"disk"},
			{"name":"sdb1","size":536870912,"tran":"","model":"","type":"part"},
			{"name":"sdc","size":512110190592,"tran":"nvme","model":"WD Blue SN570","type":"disk"}
		]
	}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")

	// Boot disk is sdb (USB).
	r.Set("/dev/sdb2\n", "findmnt", "-nro", "SOURCE", "/")

	// HasData probes: sda has filesystem, sdc does not.
	r.Set("ext4\n", "lsblk", "-ndo", "FSTYPE", "/dev/sda")
	r.Set("\n", "lsblk", "-ndo", "FSTYPE", "/dev/sdc")

	disks, err := DiscoverInternalDisks(ctx, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(disks) != 2 {
		t.Fatalf("expected 2 disks, got %d", len(disks))
	}

	// sda should be present.
	if disks[0].Device != "/dev/sda" {
		t.Errorf("disk[0].Device = %s, want /dev/sda", disks[0].Device)
	}
	if disks[0].Model != "Samsung SSD 870" {
		t.Errorf("disk[0].Model = %s, want Samsung SSD 870", disks[0].Model)
	}
	if disks[0].SizeGB != 238 { // 256060514304 / 1073741824
		t.Errorf("disk[0].SizeGB = %d, want 238", disks[0].SizeGB)
	}
	if disks[0].Transport != "sata" {
		t.Errorf("disk[0].Transport = %s, want sata", disks[0].Transport)
	}
	if !disks[0].HasData {
		t.Error("disk[0].HasData should be true")
	}

	// sdc should be present.
	if disks[1].Device != "/dev/sdc" {
		t.Errorf("disk[1].Device = %s, want /dev/sdc", disks[1].Device)
	}
	if disks[1].HasData {
		t.Error("disk[1].HasData should be false")
	}
}

func TestDiscoverInternalDisks_ExcludesBootDisk(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	// Only one disk, which is also the boot disk.
	lsblkJSON := `{
		"blockdevices": [
			{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung SSD","type":"disk"}
		]
	}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	r.Set("/dev/sda2\n", "findmnt", "-nro", "SOURCE", "/")

	disks, err := DiscoverInternalDisks(ctx, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("expected 0 disks (boot excluded), got %d", len(disks))
	}
}

func TestDiscoverInternalDisks_ExcludesUSB(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	// Two disks: sda is internal boot, sdb is USB external.
	lsblkJSON := `{
		"blockdevices": [
			{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung SSD","type":"disk"},
			{"name":"sdb","size":128000000000,"tran":"usb","model":"USB Drive","type":"disk"}
		]
	}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	r.Set("/dev/sda2\n", "findmnt", "-nro", "SOURCE", "/")

	disks, err := DiscoverInternalDisks(ctx, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// sda is boot (excluded), sdb is USB (excluded).
	if len(disks) != 0 {
		t.Errorf("expected 0 disks, got %d", len(disks))
	}
}

func TestDiscoverInternalDisks_NvmeBootDisk(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	// NVMe boot disk + SATA target.
	lsblkJSON := `{
		"blockdevices": [
			{"name":"nvme0n1","size":512110190592,"tran":"nvme","model":"WD Blue SN570","type":"disk"},
			{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung SSD 870","type":"disk"}
		]
	}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	r.Set("/dev/nvme0n1p2\n", "findmnt", "-nro", "SOURCE", "/")
	r.Set("\n", "lsblk", "-ndo", "FSTYPE", "/dev/sda")

	disks, err := DiscoverInternalDisks(ctx, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(disks))
	}
	if disks[0].Device != "/dev/sda" {
		t.Errorf("expected /dev/sda, got %s", disks[0].Device)
	}
}

func TestValidateTargetDisk_NotFound(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	lsblkJSON := `{"blockdevices": [{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung","type":"disk"}]}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	r.Set("/dev/sda2\n", "findmnt", "-nro", "SOURCE", "/")

	// sda is boot disk, so no internal disks. /dev/sdb is not found.
	err := ValidateTargetDisk(ctx, r, "/dev/sdb")
	if err == nil {
		t.Error("expected error for non-existent disk")
	}
}

func TestValidateTargetDisk_Mounted(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	lsblkJSON := `{
		"blockdevices": [
			{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung","type":"disk"},
			{"name":"sdb","size":128000000000,"tran":"sata","model":"Target","type":"disk"}
		]
	}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	r.Set("/dev/sda2\n", "findmnt", "-nro", "SOURCE", "/")
	r.Set("\n", "lsblk", "-ndo", "FSTYPE", "/dev/sdb")
	r.Set("disk\n", "lsblk", "-ndo", "TYPE", "/dev/sdb")
	r.Set("/mnt/data\n", "lsblk", "-nro", "MOUNTPOINT", "/dev/sdb")
	r.Set("disk\npart\n", "lsblk", "-nro", "TYPE", "/dev/sdb")

	err := ValidateTargetDisk(ctx, r, "/dev/sdb")
	if err == nil {
		t.Error("expected error for mounted disk")
	}
	if !strings.Contains(err.Error(), "mounted") {
		t.Errorf("error should mention mounted, got: %v", err)
	}
}

func TestValidateTargetDisk_DeviceMapper(t *testing.T) {
	ctx := context.Background()
	r := newMockRunner()

	lsblkJSON := `{
		"blockdevices": [
			{"name":"sda","size":256060514304,"tran":"sata","model":"Samsung","type":"disk"},
			{"name":"sdb","size":128000000000,"tran":"sata","model":"Target","type":"disk"}
		]
	}`
	r.Set(lsblkJSON, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	r.Set("/dev/sda2\n", "findmnt", "-nro", "SOURCE", "/")
	r.Set("\n", "lsblk", "-ndo", "FSTYPE", "/dev/sdb")
	r.Set("disk\n", "lsblk", "-ndo", "TYPE", "/dev/sdb")
	r.Set("\n", "lsblk", "-nro", "MOUNTPOINT", "/dev/sdb")
	r.Set("disk\ncrypt\n", "lsblk", "-nro", "TYPE", "/dev/sdb")

	err := ValidateTargetDisk(ctx, r, "/dev/sdb")
	if err == nil {
		t.Error("expected error for device-mapper disk")
	}
	if !strings.Contains(err.Error(), "device-mapper") {
		t.Errorf("error should mention device-mapper, got: %v", err)
	}
}

func TestFormatDiskSize(t *testing.T) {
	tests := []struct {
		sizeGB int
		want   string
	}{
		{256, "256 GB"},
		{1000, "1.0 TB"},
		{2000, "2.0 TB"},
		{500, "500 GB"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%dGB", tt.sizeGB), func(t *testing.T) {
			if got := FormatDiskSize(tt.sizeGB); got != tt.want {
				t.Errorf("FormatDiskSize(%d) = %q, want %q", tt.sizeGB, got, tt.want)
			}
		})
	}
}
