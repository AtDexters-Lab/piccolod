package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCalculatePartitionLayout_NormalDisk(t *testing.T) {
	layout, err := calculatePartitionLayout(120) // 120GB disk
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if layout.RootGB != 20 {
		t.Errorf("RootGB = %d, want 20", layout.RootGB)
	}
	// usable = 120 - 1(ESP) = 119; data = 119 - 20 = 99
	if layout.DataGB != 99 {
		t.Errorf("DataGB = %d, want 99", layout.DataGB)
	}
}

func TestCalculatePartitionLayout_SmallDisk(t *testing.T) {
	// 24GB disk: usable = 23, below 25 threshold
	layout, err := calculatePartitionLayout(24)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 70% of 23 = 16; 30% = 7
	if layout.RootGB != 16 {
		t.Errorf("RootGB = %d, want 16", layout.RootGB)
	}
	if layout.DataGB != 7 {
		t.Errorf("DataGB = %d, want 7", layout.DataGB)
	}
}

func TestCalculatePartitionLayout_MinimumViableDisk(t *testing.T) {
	// Exactly 26GB: usable = 25 = RootTargetSizeGB + MinDataPartitionGB
	layout, err := calculatePartitionLayout(26)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if layout.RootGB != 20 {
		t.Errorf("RootGB = %d, want 20", layout.RootGB)
	}
	if layout.DataGB != 5 {
		t.Errorf("DataGB = %d, want 5", layout.DataGB)
	}
}

func TestCalculatePartitionLayout_TooSmallDisk(t *testing.T) {
	_, err := calculatePartitionLayout(10)
	if err == nil {
		t.Fatal("expected error for 10GB disk")
	}
	if !strings.Contains(err.Error(), "disk too small") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPartitionDevicePath(t *testing.T) {
	tests := []struct {
		disk string
		slot int
		want string
	}{
		{"/dev/sda", 3, "/dev/sda3"},
		{"/dev/vda", 2, "/dev/vda2"},
		{"/dev/nvme0n1", 3, "/dev/nvme0n1p3"},
		{"/dev/mmcblk0", 1, "/dev/mmcblk0p1"},
		{"/dev/loop0", 2, "/dev/loop0p2"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%d", tt.disk, tt.slot), func(t *testing.T) {
			got := partitionDevicePath(tt.disk, tt.slot)
			if got != tt.want {
				t.Errorf("partitionDevicePath(%q, %d) = %q, want %q", tt.disk, tt.slot, got, tt.want)
			}
		})
	}
}

func TestGetParentDisk(t *testing.T) {
	tests := []struct {
		partition string
		want      string
	}{
		{"/dev/sda2", "/dev/sda"},
		{"/dev/sdb1", "/dev/sdb"},
		{"/dev/vda3", "/dev/vda"},
		{"/dev/nvme0n1p2", "/dev/nvme0n1"},
		{"/dev/nvme1n1p3", "/dev/nvme1n1"},
		{"/dev/mmcblk0p1", "/dev/mmcblk0"},
		{"/dev/loop0p2", "/dev/loop0"},
	}
	for _, tt := range tests {
		t.Run(tt.partition, func(t *testing.T) {
			got := getParentDisk(tt.partition)
			if got != tt.want {
				t.Errorf("getParentDisk(%q) = %q, want %q", tt.partition, got, tt.want)
			}
		})
	}
}

func TestGetRootDevice_StripsBtrfsSubvolume(t *testing.T) {
	run := &fakeRunner{
		Outputs: map[string]string{
			"findmnt -nro SOURCE /": "/dev/sda2[/rootfs]",
		},
	}
	dev, err := getRootDevice(context.Background(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dev != "/dev/sda2" {
		t.Errorf("getRootDevice = %q, want /dev/sda2", dev)
	}
}

func TestParseSfdiskJSON_GPT(t *testing.T) {
	data := SfdiskOutput{}
	data.PartitionTable.SectorSize = 512
	data.PartitionTable.Label = PartitionTableGPT
	data.PartitionTable.Partitions = []SfdiskPartition{
		{Node: "/dev/sda1", Start: 2048, Size: 1048576, Type: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
		{Node: "/dev/sda2", Start: 1050624, Size: 41943040, Type: "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"},
	}
	raw, _ := json.Marshal(data)

	got, err := ParseSfdiskJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.PartitionTable.Partitions) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(got.PartitionTable.Partitions))
	}
	if got.PartitionTable.SectorSize != 512 {
		t.Errorf("SectorSize = %d, want 512", got.PartitionTable.SectorSize)
	}
	if got.PartitionTable.Label != PartitionTableGPT {
		t.Errorf("Label = %q, want %q", got.PartitionTable.Label, PartitionTableGPT)
	}
	if !got.IsGPT() {
		t.Error("IsGPT() = false, want true")
	}
	if got.IsMBR() {
		t.Error("IsMBR() = true, want false")
	}
	if got.PartitionTable.Partitions[1].Node != "/dev/sda2" {
		t.Errorf("second partition node = %q, want /dev/sda2", got.PartitionTable.Partitions[1].Node)
	}
}

func TestParseSfdiskJSON_MBR(t *testing.T) {
	// Real sfdisk -J output from an RPi MBR disk
	raw := []byte(`{
		"partitiontable": {
			"label": "dos",
			"sectorsize": 512,
			"partitions": [
				{"node": "/dev/mmcblk0p1", "start": 8192, "size": 524288, "type": "c"},
				{"node": "/dev/mmcblk0p2", "start": 532480, "size": 4194304, "type": "83"}
			]
		}
	}`)

	got, err := ParseSfdiskJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PartitionTable.Label != PartitionTableMBR {
		t.Errorf("Label = %q, want %q", got.PartitionTable.Label, PartitionTableMBR)
	}
	if !got.IsMBR() {
		t.Error("IsMBR() = false, want true")
	}
	if got.IsGPT() {
		t.Error("IsGPT() = true, want false")
	}
	if len(got.PartitionTable.Partitions) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(got.PartitionTable.Partitions))
	}
	// MBR type codes are hex strings, not GUIDs
	if got.PartitionTable.Partitions[0].Type != "c" {
		t.Errorf("partition 1 type = %q, want \"c\" (FAT32 LBA)", got.PartitionTable.Partitions[0].Type)
	}
	if got.PartitionTable.Partitions[1].Type != "83" {
		t.Errorf("partition 2 type = %q, want \"83\" (Linux)", got.PartitionTable.Partitions[1].Type)
	}
}

func TestParseSfdiskJSON_WarningPrefix(t *testing.T) {
	// sfdisk emits warnings (e.g. GPT PMBR mismatch) before JSON on resized disks.
	t.Run("single_warning_line", func(t *testing.T) {
		raw := []byte("GPT PMBR size mismatch (2504703 != 50331647) will be corrected by write.\n" +
			`{"partitiontable":{"sectorsize":512,"partitions":[{"node":"/dev/sda1","start":2048,"size":4096,"type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"}]}}`)

		got, err := ParseSfdiskJSON(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.PartitionTable.Partitions) != 1 {
			t.Fatalf("expected 1 partition, got %d", len(got.PartitionTable.Partitions))
		}
		if got.PartitionTable.Partitions[0].Node != "/dev/sda1" {
			t.Errorf("partition node = %q, want /dev/sda1", got.PartitionTable.Partitions[0].Node)
		}
	})

	t.Run("multiple_warning_lines", func(t *testing.T) {
		raw := []byte("GPT PMBR size mismatch (2504703 != 50331647) will be corrected by write.\n" +
			"The backup GPT table is not on the end of the device.\n" +
			`{"partitiontable":{"sectorsize":512,"partitions":[{"node":"/dev/sda1","start":2048,"size":4096,"type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"}]}}`)

		got, err := ParseSfdiskJSON(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.PartitionTable.Partitions) != 1 {
			t.Fatalf("expected 1 partition, got %d", len(got.PartitionTable.Partitions))
		}
	})
}
