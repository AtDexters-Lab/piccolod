package onboarding

import (
	"strings"
	"testing"
)

func TestResolveImageURL_Default(t *testing.T) {
	// Clear any override.
	t.Setenv("PICCOLO_INSTALL_IMAGE_URL", "")
	url := resolveImageURL()
	if url == "" {
		t.Fatal("expected non-empty URL")
	}
	if !strings.Contains(url, "piccolo-os.") {
		t.Errorf("URL should contain piccolo-os., got %s", url)
	}
	if !strings.Contains(url, ".raw.xz") {
		t.Errorf("URL should end with .raw.xz, got %s", url)
	}
}

func TestResolveImageURL_Override(t *testing.T) {
	t.Setenv("PICCOLO_INSTALL_IMAGE_URL", "https://example.com/custom.raw.xz")
	url := resolveImageURL()
	if url != "https://example.com/custom.raw.xz" {
		t.Errorf("expected override URL, got %s", url)
	}
}

func TestPartitionDevPath(t *testing.T) {
	tests := []struct {
		disk string
		slot int
		want string
	}{
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/sda", 3, "/dev/sda3"},
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/mmcblk0", 2, "/dev/mmcblk0p2"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := partitionDevPath(tt.disk, tt.slot)
			if got != tt.want {
				t.Errorf("partitionDevPath(%q, %d) = %q, want %q", tt.disk, tt.slot, got, tt.want)
			}
		})
	}
}

func TestSplitDiskAndPart(t *testing.T) {
	tests := []struct {
		partDev  string
		wantDisk string
		wantNum  int
	}{
		{"/dev/sda1", "/dev/sda", 1},
		{"/dev/sda3", "/dev/sda", 3},
		{"/dev/nvme0n1p1", "/dev/nvme0n1", 1},
		{"/dev/nvme0n1p3", "/dev/nvme0n1", 3},
		{"/dev/mmcblk0p1", "/dev/mmcblk0", 1},
	}
	for _, tt := range tests {
		t.Run(tt.partDev, func(t *testing.T) {
			disk, num := splitDiskAndPart(tt.partDev)
			if disk != tt.wantDisk || num != tt.wantNum {
				t.Errorf("splitDiskAndPart(%q) = (%q, %d), want (%q, %d)",
					tt.partDev, disk, num, tt.wantDisk, tt.wantNum)
			}
		})
	}
}

func TestDDStderrSink(t *testing.T) {
	sink := ddStderrSink{}
	data := []byte("1073741824 bytes (1.1 GB, 1.0 GiB) copied, 5.0 s, 215 MB/s\r")
	n, err := sink.Write(data)
	if err != nil {
		t.Errorf("Write error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, expected %d", n, len(data))
	}
}


func TestNewInstaller(t *testing.T) {
	inst := NewInstaller(nil, nil, nil)
	if inst == nil {
		t.Fatal("expected non-nil installer")
	}
}

func TestInstall_AlreadyRunning(t *testing.T) {
	inst := NewInstaller(nil, nil, nil)
	inst.mu.Lock()
	inst.running = true
	inst.mu.Unlock()

	err := inst.Install(nil, "/dev/sda", "", "install-test")
	if err == nil {
		t.Error("expected error for concurrent install")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected 'already in progress' error, got: %v", err)
	}
}
