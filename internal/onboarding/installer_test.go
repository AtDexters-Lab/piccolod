package onboarding

import (
	"fmt"
	"strings"
	"testing"

	"piccolod/internal/events"
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

func TestDDProgressParser(t *testing.T) {
	// Just ensure it doesn't panic and accepts dd-style output.
	parser := &ddProgressParser{
		taskID:        "install-test",
		reporter:      &Installer{},
		estimatedSize: 4 * 1024 * 1024 * 1024, // 4 GiB estimated
	}

	lines := []string{
		"4194304 bytes (4.2 MB, 4.0 MiB) copied, 0.01 s, 419 MB/s\r",
		"1073741824 bytes (1.1 GB, 1.0 GiB) copied, 5.0 s, 215 MB/s\r",
		"some other output\n",
	}
	for _, line := range lines {
		n, err := parser.Write([]byte(line))
		if err != nil {
			t.Errorf("Write error: %v", err)
		}
		if n != len(line) {
			t.Errorf("Write returned %d, expected %d", n, len(line))
		}
	}
}

func TestDDProgressParser_Interpolation(t *testing.T) {
	// Verify progress interpolation within 71-91% range.
	var lastPct int
	reporter := &mockProgressReporter{}
	inst := &Installer{reporter: reporter}

	estimated := int64(4 * 1024 * 1024 * 1024) // 4 GiB
	parser := &ddProgressParser{
		taskID:        "install-test",
		reporter:      inst,
		estimatedSize: estimated,
	}

	tests := []struct {
		bytes   int64
		wantMin int
		wantMax int
	}{
		{0, 71, 71},                        // 0% → 71
		{1 * 1024 * 1024 * 1024, 75, 76},   // 25% → ~76
		{2 * 1024 * 1024 * 1024, 80, 82},   // 50% → ~81
		{3 * 1024 * 1024 * 1024, 85, 87},   // 75% → ~86
		{4 * 1024 * 1024 * 1024, 91, 91},   // 100% → 91
		{5 * 1024 * 1024 * 1024, 91, 91},   // >100% capped → 91
	}

	for _, tt := range tests {
		reporter.lastProgress = -1
		line := fmt.Sprintf("%d bytes (0 B) copied, 1.0 s, 0 B/s\r", tt.bytes)
		parser.Write([]byte(line))
		lastPct = reporter.lastProgress
		if tt.bytes == 0 {
			// 0 bytes line doesn't match since fields[0] would be "0"
			// which parses fine, so check it
			if lastPct < tt.wantMin || lastPct > tt.wantMax {
				t.Errorf("bytes=%d: progress=%d, want [%d, %d]", tt.bytes, lastPct, tt.wantMin, tt.wantMax)
			}
		} else if lastPct < tt.wantMin || lastPct > tt.wantMax {
			t.Errorf("bytes=%d: progress=%d, want [%d, %d]", tt.bytes, lastPct, tt.wantMin, tt.wantMax)
		}
	}
}

func TestDDProgressParser_NoEstimate(t *testing.T) {
	// Without estimatedSize, progress should stay at 71.
	reporter := &mockProgressReporter{}
	inst := &Installer{reporter: reporter}

	parser := &ddProgressParser{
		taskID:        "install-test",
		reporter:      inst,
		estimatedSize: 0,
	}

	parser.Write([]byte("1073741824 bytes (1.1 GB, 1.0 GiB) copied, 5.0 s, 215 MB/s\r"))
	if reporter.lastProgress != 71 {
		t.Errorf("expected progress 71 with no estimate, got %d", reporter.lastProgress)
	}
}

// mockProgressReporter captures the last reported progress.
type mockProgressReporter struct {
	lastProgress int
	lastPhase    string
	events       []events.TaskProgressEvent
}

func (m *mockProgressReporter) Report(evt events.TaskProgressEvent) {
	m.lastProgress = evt.Progress
	m.lastPhase = evt.Phase
	m.events = append(m.events, evt)
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
