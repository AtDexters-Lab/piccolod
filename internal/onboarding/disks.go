package onboarding

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"piccolod/internal/runner"
	"piccolod/internal/storage"
)

// DiskInfo describes an internal (non-USB, non-boot) disk candidate.
type DiskInfo struct {
	Device    string `json:"device"`
	Model     string `json:"model"`
	SizeGB    int    `json:"size_gb"`
	Transport string `json:"transport"`
	HasData   bool   `json:"has_data"`
}

// lsblkDevice mirrors lsblk -Jbndo output for a single block device.
type lsblkDevice struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`  // bytes when -b is used
	Tran  string `json:"tran"`
	Model string `json:"model"`
	Type  string `json:"type"`
}

type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

// DiscoverInternalDisks finds non-boot, non-USB whole disks.
func DiscoverInternalDisks(ctx context.Context, run runner.CommandRunner) ([]DiskInfo, error) {
	// Get all block devices.
	out, err := run.RunWithOutput(ctx, "lsblk", "-Jbndo", "NAME,SIZE,TRAN,MODEL,TYPE")
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}

	var parsed lsblkOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}

	// Determine boot disk.
	bootDisk, err := getBootDisk(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("boot disk: %w", err)
	}

	var disks []DiskInfo
	for _, dev := range parsed.Blockdevices {
		if dev.Type != "disk" {
			continue
		}
		devPath := "/dev/" + dev.Name

		// Exclude boot disk.
		if devPath == bootDisk {
			continue
		}

		// Exclude USB-connected devices.
		if strings.ToLower(dev.Tran) == "usb" {
			continue
		}

		sizeGB := int(dev.Size / (1024 * 1024 * 1024))
		hasData, _ := probeHasData(ctx, run, dev.Name)

		disks = append(disks, DiskInfo{
			Device:    devPath,
			Model:     strings.TrimSpace(dev.Model),
			SizeGB:    sizeGB,
			Transport: strings.ToLower(dev.Tran),
			HasData:   hasData,
		})
	}
	return disks, nil
}

// getBootDisk returns the parent disk of the root filesystem.
func getBootDisk(ctx context.Context, run runner.CommandRunner) (string, error) {
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
	return storage.GetParentDisk(dev), nil
}

// probeHasData checks if a disk has any child partitions with filesystem signatures.
func probeHasData(ctx context.Context, run runner.CommandRunner, diskName string) (bool, error) {
	out, err := run.RunWithOutput(ctx, "lsblk", "-ndo", "FSTYPE", "/dev/"+diskName)
	if err != nil {
		// If the disk itself has no partitions, check children.
		out, err = run.RunWithOutput(ctx, "lsblk", "-nro", "FSTYPE", "/dev/"+diskName)
		if err != nil {
			return false, err
		}
	}
	// Any non-empty FSTYPE line means data exists.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			return true, nil
		}
	}
	return false, nil
}

// ValidateTargetDisk performs install-time validation on a target disk.
func ValidateTargetDisk(ctx context.Context, run runner.CommandRunner, device string) error {
	// Must be in the current discovery results.
	disks, err := DiscoverInternalDisks(ctx, run)
	if err != nil {
		return fmt.Errorf("disk discovery: %w", err)
	}
	found := false
	for _, d := range disks {
		if d.Device == device {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("device %s not found among internal disks", device)
	}

	// Must be a whole disk (not a partition).
	out, err := run.RunWithOutput(ctx, "lsblk", "-ndo", "TYPE", device)
	if err != nil {
		return fmt.Errorf("check device type: %w", err)
	}
	if strings.TrimSpace(string(out)) != "disk" {
		return fmt.Errorf("%s is not a whole disk device", device)
	}

	// No partitions may be mounted.
	mountOut, err := run.RunWithOutput(ctx, "lsblk", "-nro", "MOUNTPOINT", device)
	if err != nil {
		return fmt.Errorf("check mounts: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(mountOut)), "\n") {
		if strings.TrimSpace(line) != "" {
			return fmt.Errorf("%s has mounted partitions", device)
		}
	}

	// No device-mapper users.
	dmOut, err := run.RunWithOutput(ctx, "lsblk", "-nro", "TYPE", device)
	if err != nil {
		return fmt.Errorf("check dm: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(dmOut)), "\n") {
		typ := strings.TrimSpace(line)
		if typ == "crypt" || typ == "lvm" {
			return fmt.Errorf("%s has active device-mapper users (%s)", device, typ)
		}
	}

	// Not in use as swap.
	if isSwapDevice(device) {
		return fmt.Errorf("%s is in use as swap", device)
	}

	return nil
}

// isSwapDevice checks /proc/swaps for the given device or its partitions.
func isSwapDevice(device string) bool {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 {
			swapDev := fields[0]
			// Check exact match (whole disk used as swap)
			if swapDev == device {
				return true
			}
			// Check if swap is a partition of device (e.g. /dev/sda1 for /dev/sda)
			if strings.HasPrefix(swapDev, device) {
				suffix := strings.TrimPrefix(swapDev, device)
				// Valid partition suffixes: "1", "p1"
				if len(suffix) > 0 {
					// Check for NVMe style "p1"
					if suffix[0] == 'p' {
						suffix = suffix[1:]
					}
					// Remaining suffix must be numeric
					if _, err := strconv.Atoi(suffix); err == nil {
						return true
					}
				}
			}
		}
	}
	return false
}

// FormatDiskSize returns a human-readable disk size string.
func FormatDiskSize(sizeGB int) string {
	if sizeGB >= 1000 {
		tb := float64(sizeGB) / 1000.0
		return strconv.FormatFloat(tb, 'f', 1, 64) + " TB"
	}
	return strconv.Itoa(sizeGB) + " GB"
}
