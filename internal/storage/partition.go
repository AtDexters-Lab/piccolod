package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"piccolod/internal/runner"
)

const (
	// RootTargetSizeGB is the target root partition size. openSUSE MicroOS
	// recommends a maximum of 20GB for the root filesystem (server variant).
	RootTargetSizeGB = 20
	// MinDataPartitionGB is the minimum acceptable size for /piccolo-data.
	MinDataPartitionGB = 5
	// ESPSizeGB is a conservative estimate for the EFI System Partition.
	ESPSizeGB = 1
)

// PartitionLayout captures the computed root and data sizes for a given disk.
type PartitionLayout struct {
	RootGB int
	DataGB int
}

// DiskState summarises the partition-level view of the boot disk.
type DiskState struct {
	RootExpanded        bool // Root partition at target size
	PiccoloCoreExists   bool // Core subvolume present
	DataPartitionExists bool // piccolo-data partition present
	DataPartitionSlot   int  // Partition slot number (0 if absent)
	DataPartitionLUKS   bool // Partition has a LUKS header
	DataPartitionMounted bool // /piccolo-data is mounted
	SetupComplete       bool // All of above are true
}

// PartitionState captures the current state of the boot disk's partitions.
// Used by diskprep.Preparer and storage.Manager.
type PartitionState struct {
	Disk              string // Parent disk (e.g., /dev/sda)
	RootPartition     string // Root partition device (e.g., /dev/sda2)
	RootSizeGB        int    // Current root partition size in GB
	RootNeedsExpansion bool  // Root is smaller than the calculated target
	DataPartition     string // Data partition device (empty if absent)
	DataPartitionSlot int    // Partition slot (0 if absent)
	DataPartitionLUKS bool   // Data partition has LUKS header
	UnallocatedGB     int    // Approximate unallocated space on disk
}

// calculatePartitionLayout computes the root/data split for a disk of the given size.
func calculatePartitionLayout(diskSizeGB int) (PartitionLayout, error) {
	usableGB := diskSizeGB - ESPSizeGB
	if usableGB < RootTargetSizeGB+MinDataPartitionGB {
		// Small disk — proportional 70/30 split.
		rootSize := usableGB * 70 / 100
		dataSize := usableGB - rootSize
		if dataSize < MinDataPartitionGB {
			minDisk := ESPSizeGB + (MinDataPartitionGB*100/30) + 1
			return PartitionLayout{}, fmt.Errorf("disk too small: %dGB total, need at least %dGB",
				diskSizeGB, minDisk)
		}
		return PartitionLayout{RootGB: rootSize, DataGB: dataSize}, nil
	}
	// Normal disk — root at target, data gets the rest.
	return PartitionLayout{
		RootGB: RootTargetSizeGB,
		DataGB: usableGB - RootTargetSizeGB,
	}, nil
}

// partitionDevicePath returns the device node for a partition on a disk.
// Handles naming conventions: sda→sda3, nvme0n1→nvme0n1p3, mmcblk0→mmcblk0p3.
func partitionDevicePath(disk string, slot int) string {
	if len(disk) > 0 && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, slot)
	}
	return fmt.Sprintf("%s%d", disk, slot)
}

// getRootDevice returns the block device backing the root filesystem.
func getRootDevice(ctx context.Context, run runner.CommandRunner) (string, error) {
	out, err := run.RunWithOutput(ctx, "findmnt", "-nro", "SOURCE", "/")
	if err != nil {
		return "", fmt.Errorf("findmnt: %w", err)
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", fmt.Errorf("findmnt returned empty root device")
	}
	// Strip btrfs subvolume brackets: /dev/sda2[/subvol] → /dev/sda2
	if idx := strings.Index(dev, "["); idx != -1 {
		dev = dev[:idx]
	}
	return dev, nil
}

// getParentDisk derives the parent disk device from a partition device path.
// e.g., /dev/sda2 → /dev/sda, /dev/nvme0n1p2 → /dev/nvme0n1
func getParentDisk(partitionDev string) string {
	// NVMe/eMMC: /dev/nvme0n1p2 → /dev/nvme0n1
	if strings.Contains(partitionDev, "nvme") || strings.Contains(partitionDev, "mmcblk") || strings.Contains(partitionDev, "loop") {
		if idx := strings.LastIndex(partitionDev, "p"); idx > 0 {
			candidate := partitionDev[:idx]
			// Ensure the suffix after 'p' is purely numeric
			rest := partitionDev[idx+1:]
			if rest != "" && isDigits(rest) {
				return candidate
			}
		}
	}
	// SCSI/SATA/virtio: /dev/sda2 → /dev/sda, /dev/vda3 → /dev/vda
	return strings.TrimRight(partitionDev, "0123456789")
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// SfdiskOutput represents the JSON output of sfdisk -J.
type SfdiskOutput struct {
	PartitionTable struct {
		SectorSize int              `json:"sectorsize"`
		Partitions []SfdiskPartition `json:"partitions"`
	} `json:"partitiontable"`
}

// SfdiskPartition is a single partition entry from sfdisk -J.
type SfdiskPartition struct {
	Node  string `json:"node"`
	Start int64  `json:"start"`
	Size  int64  `json:"size"`
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
}

// ParseSfdiskJSON parses sfdisk -J output into structured data.
// sfdisk may emit warning lines (e.g. GPT PMBR size mismatch) before the JSON
// object, so we skip to the first '{' character.
func ParseSfdiskJSON(data []byte) (SfdiskOutput, error) {
	idx := bytes.IndexByte(data, '{')
	if idx < 0 {
		return SfdiskOutput{}, fmt.Errorf("parse sfdisk JSON: no JSON object found in output")
	}
	var out SfdiskOutput
	if err := json.Unmarshal(data[idx:], &out); err != nil {
		return SfdiskOutput{}, fmt.Errorf("parse sfdisk JSON: %w", err)
	}
	return out, nil
}

// GetParentDisk derives the parent disk device from a partition device path.
// Exported for use by the diskprep sub-package.
func GetParentDisk(partitionDev string) string {
	return getParentDisk(partitionDev)
}

// CalculatePartitionLayout is the exported version of calculatePartitionLayout.
func CalculatePartitionLayout(diskSizeGB int) (PartitionLayout, error) {
	return calculatePartitionLayout(diskSizeGB)
}

// PartitionDevicePath returns the device node for a partition on a disk.
// Exported for use by the diskprep sub-package.
func PartitionDevicePath(disk string, slot int) string {
	return partitionDevicePath(disk, slot)
}
