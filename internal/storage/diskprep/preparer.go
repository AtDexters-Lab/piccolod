package diskprep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage"
)

// Preparer performs disk probes and mutating partition operations.
type Preparer struct {
	run runner.CommandRunner
}

// NewPreparer creates a preparer with the given command runner.
func NewPreparer(run runner.CommandRunner) *Preparer {
	return &Preparer{run: run}
}

// GetPartitionState surveys the boot disk and returns partition information.
func (p *Preparer) GetPartitionState(ctx context.Context) (*storage.PartitionState, error) {
	rootDev, err := p.getRootDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect root device: %w", err)
	}
	disk := storage.GetParentDisk(rootDev)

	sfdisk, err := p.readSfdisk(ctx, disk)
	if err != nil {
		return nil, fmt.Errorf("read partition table: %w", err)
	}

	diskSizeGB, err := p.getDiskSizeGB(ctx, disk)
	if err != nil {
		return nil, fmt.Errorf("disk size: %w", err)
	}

	sectorSize := sfdisk.PartitionTable.SectorSize
	if sectorSize <= 0 {
		sectorSize = 512
	}

	state := &storage.PartitionState{
		Disk:          disk,
		RootPartition: rootDev,
	}

	// Find root partition size
	for _, part := range sfdisk.PartitionTable.Partitions {
		if part.Node == rootDev {
			state.RootSizeGB = int(part.Size * int64(sectorSize) / (1 << 30))
			break
		}
	}

	// Determine if root needs expansion
	layout, err := storage.CalculatePartitionLayout(diskSizeGB)
	if err != nil {
		log.Printf("WARN: cannot compute partition layout for %dGB disk: %v", diskSizeGB, err)
	} else {
		state.RootNeedsExpansion = state.RootSizeGB < layout.RootGB
	}

	// Calculate unallocated space
	var lastEndSector int64
	for _, part := range sfdisk.PartitionTable.Partitions {
		end := part.Start + part.Size
		if end > lastEndSector {
			lastEndSector = end
		}
	}
	totalSectors, _ := p.getDiskSectors(ctx, disk)
	if totalSectors > lastEndSector {
		state.UnallocatedGB = int((totalSectors - lastEndSector) * int64(sectorSize) / (1 << 30))
	}

	// Look for data partition (LUKS type code 8309 or LUKS header)
	dataNode, dataSlot := p.findDataPartition(ctx, disk, sfdisk)
	if dataNode != "" {
		state.DataPartition = dataNode
		state.DataPartitionSlot = dataSlot
		state.DataPartitionLUKS, _ = IsLUKS(ctx, p.run, dataNode)
	}

	return state, nil
}

// VerifyPiccoloCoreExists checks whether the core subvolume path exists.
func (p *Preparer) VerifyPiccoloCoreExists(ctx context.Context, corePath string) bool {
	info, err := os.Stat(corePath)
	return err == nil && info.IsDir()
}

// findNextPartitionSlot finds the first unused GPT slot (1-128).
func (p *Preparer) findNextPartitionSlot(ctx context.Context, disk string) (int, error) {
	sfdisk, err := p.readSfdisk(ctx, disk)
	if err != nil {
		return 0, err
	}
	return FindNextSlot(sfdisk), nil
}

// FindNextSlot finds the first unused GPT partition slot from sfdisk output.
func FindNextSlot(sfdisk storage.SfdiskOutput) int {
	used := make(map[int]bool)
	for _, part := range sfdisk.PartitionTable.Partitions {
		slot := extractSlotNumber(part.Node)
		if slot > 0 {
			used[slot] = true
		}
	}
	for slot := 1; slot <= 128; slot++ {
		if !used[slot] {
			return slot
		}
	}
	return 0
}

// getDiskSizeGB returns the disk size in GB (ceiling division).
// Ceiling ensures a 20.1 GB disk reports 21 GB, preventing borderline disks
// from falling below the MinDataPartitionGB threshold in calculatePartitionLayout.
func (p *Preparer) getDiskSizeGB(ctx context.Context, disk string) (int, error) {
	out, err := p.run.RunWithOutput(ctx, "lsblk", "-bndo", "SIZE", disk)
	if err != nil {
		return 0, fmt.Errorf("lsblk SIZE: %w", err)
	}
	sizeBytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse disk size: %w", err)
	}
	const gib = int64(1 << 30)
	return int((sizeBytes + gib - 1) / gib), nil
}

// getDiskSectors returns the total number of sectors on the disk.
func (p *Preparer) getDiskSectors(ctx context.Context, disk string) (int64, error) {
	out, err := p.run.RunWithOutput(ctx, "blockdev", "--getsz", disk)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// getSectorSize returns the logical sector size.
func (p *Preparer) getSectorSize(ctx context.Context, disk string) (int, error) {
	out, err := p.run.RunWithOutput(ctx, "blockdev", "--getss", disk)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// IsLUKS checks if a device has a LUKS header using cryptsetup.
func IsLUKS(ctx context.Context, run runner.CommandRunner, device string) (bool, error) {
	err := run.Run(ctx, "cryptsetup", "isLuks", device)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, fmt.Errorf("cryptsetup isLuks: %w", err)
}

// getRootDevice returns the block device backing /.
func (p *Preparer) getRootDevice(ctx context.Context) (string, error) {
	out, err := p.run.RunWithOutput(ctx, "findmnt", "-nro", "SOURCE", "/")
	if err != nil {
		return "", fmt.Errorf("findmnt: %w", err)
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", fmt.Errorf("findmnt returned empty root device")
	}
	if idx := strings.Index(dev, "["); idx != -1 {
		dev = dev[:idx]
	}
	return dev, nil
}

// readSfdisk parses the partition table of a disk via sfdisk -J.
func (p *Preparer) readSfdisk(ctx context.Context, disk string) (storage.SfdiskOutput, error) {
	out, err := p.run.RunWithOutput(ctx, "sfdisk", "-J", disk)
	if err != nil {
		return storage.SfdiskOutput{}, fmt.Errorf("sfdisk -J %s: %w", disk, err)
	}
	return storage.ParseSfdiskJSON(out)
}

// findDataPartition looks for a data partition by LUKS type code or LUKS header.
func (p *Preparer) findDataPartition(ctx context.Context, disk string, sfdisk storage.SfdiskOutput) (node string, slot int) {
	// Linux LUKS partition type GUID
	const luksTypeGUID = "CA7D7CCB-63ED-4C53-861C-1742536059CC"

	for _, part := range sfdisk.PartitionTable.Partitions {
		if part.Node == "" {
			continue
		}
		if strings.EqualFold(part.Type, luksTypeGUID) {
			return part.Node, extractSlotNumber(part.Node)
		}
	}
	// Fallback: probe each non-root partition for LUKS header
	rootDev, _ := p.getRootDevice(ctx)
	for _, part := range sfdisk.PartitionTable.Partitions {
		if part.Node == "" || part.Node == rootDev {
			continue
		}
		if ok, _ := IsLUKS(ctx, p.run, part.Node); ok {
			return part.Node, extractSlotNumber(part.Node)
		}
	}
	return "", 0
}

// extractSlotNumber extracts the partition number from a device node.
// e.g., /dev/sda3 → 3, /dev/nvme0n1p2 → 2
func extractSlotNumber(node string) int {
	// Work backwards from end to find the trailing digits
	i := len(node) - 1
	for i >= 0 && node[i] >= '0' && node[i] <= '9' {
		i--
	}
	if i == len(node)-1 {
		return 0
	}
	n, err := strconv.Atoi(node[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// sfdiskPartitionJSON is used for JSON marshaling in tests.
type sfdiskPartitionJSON struct {
	Node  string `json:"node"`
	Start int64  `json:"start"`
	Size  int64  `json:"size"`
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
}

// BuildSfdiskJSON builds sfdisk -J compatible JSON for testing.
func BuildSfdiskJSON(sectorSize int, partitions []storage.SfdiskPartition) []byte {
	type table struct {
		SectorSize int                      `json:"sectorsize"`
		Partitions []storage.SfdiskPartition `json:"partitions"`
	}
	data, _ := json.Marshal(struct {
		PartitionTable table `json:"partitiontable"`
	}{
		PartitionTable: table{
			SectorSize: sectorSize,
			Partitions: partitions,
		},
	})
	return data
}

// CreateDataPartition creates a new Linux LUKS partition using the remaining disk space.
// The data partition is created BEFORE root expansion so it acts as a boundary for growpart.
func (p *Preparer) CreateDataPartition(ctx context.Context, disk string) (string, int, error) {
	// Repair GPT: when the OS image is written to a larger disk, the backup GPT
	// header remains at the old end-of-disk position. sgdisk refuses to operate
	// until this is fixed. sgdisk -e moves the backup structures to the correct
	// position and is safe to run idempotently.
	if err := p.run.Run(ctx, "sgdisk", "-e", disk); err != nil {
		log.Printf("WARN: sgdisk -e (GPT repair) failed: %v (continuing)", err)
	}

	slot, err := p.findNextPartitionSlot(ctx, disk)
	if err != nil {
		return "", 0, fmt.Errorf("find free slot: %w", err)
	}
	if slot == 0 {
		return "", 0, fmt.Errorf("no free GPT partition slot on %s", disk)
	}

	// Compute the start sector: after the root partition's target end.
	sfdisk, err := p.readSfdisk(ctx, disk)
	if err != nil {
		return "", 0, fmt.Errorf("read partition table: %w", err)
	}
	diskSizeGB, err := p.getDiskSizeGB(ctx, disk)
	if err != nil {
		return "", 0, fmt.Errorf("disk size: %w", err)
	}
	layout, err := storage.CalculatePartitionLayout(diskSizeGB)
	if err != nil {
		return "", 0, fmt.Errorf("partition layout: %w", err)
	}

	sectorSize := sfdisk.PartitionTable.SectorSize
	if sectorSize <= 0 {
		sectorSize = 512
	}

	// Start sector: root target size (in sectors) from the ESP end.
	// We use the root target in bytes / sector size.
	startSector := int64(layout.RootGB+storage.ESPSizeGB) * (1 << 30) / int64(sectorSize)

	slotStr := strconv.Itoa(slot)
	startStr := strconv.FormatInt(startSector, 10)

	// sgdisk: -n slot:start:0 means "start at startSector, extend to end of disk"
	// -t slot:8309 sets the Linux LUKS partition type GUID
	// -c slot:piccolo-data sets a human-readable partition label
	if err := p.run.Run(ctx, "sgdisk",
		"-n", slotStr+":"+startStr+":0",
		"-t", slotStr+":8309",
		"-c", slotStr+":piccolo-data",
		disk); err != nil {
		return "", 0, fmt.Errorf("sgdisk create partition: %w", err)
	}

	if err := p.reloadPartitionTable(ctx, disk); err != nil {
		return "", 0, fmt.Errorf("reload partition table: %w", err)
	}

	partDev := storage.PartitionDevicePath(disk, slot)

	// Verify kernel registered the new partition (up to ~2 seconds).
	for attempt := 0; attempt < 10; attempt++ {
		if _, err := os.Stat(partDev); err == nil {
			log.Printf("data partition created: %s (slot %d)", partDev, slot)
			return partDev, slot, nil
		}
		if attempt == 9 {
			return "", 0, fmt.Errorf("kernel did not register partition %s after reload", partDev)
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}
	return partDev, slot, nil
}

// ExpandRootPartition expands the root partition and filesystem to fill
// available space up to the data partition boundary.
func (p *Preparer) ExpandRootPartition(ctx context.Context, disk string, rootPartition string) error {
	// Extract the root partition number for growpart.
	rootSlot := extractSlotNumber(rootPartition)
	if rootSlot == 0 {
		return fmt.Errorf("cannot determine root partition slot from %s", rootPartition)
	}

	// growpart expands the partition to fill available space (bounded by next partition).
	// Use RunWithOutput because growpart emits "NOCHANGE" on stdout/stderr, but
	// Run() pipes output to os.Stdout — err.Error() only contains "exit status N".
	if out, err := p.run.RunWithOutput(ctx, "growpart", disk, strconv.Itoa(rootSlot)); err != nil {
		// growpart exits non-zero with NOCHANGE when partition is already at max size.
		if strings.Contains(string(out), "NOCHANGE") {
			log.Printf("root partition %s already at maximum size", rootPartition)
			return nil
		}
		return fmt.Errorf("growpart: %w", err)
	}

	// Resize the btrfs filesystem. Use /var as the mount point since root
	// may be read-only, but /var is a writable subvolume on MicroOS.
	if err := p.run.Run(ctx, "btrfs", "filesystem", "resize", "max", "/var"); err != nil {
		return fmt.Errorf("btrfs filesystem resize: %w", err)
	}

	log.Printf("root partition %s expanded", rootPartition)
	return nil
}

// reloadPartitionTable asks the kernel to re-read the partition table.
// Uses partprobe with partx as fallback.
func (p *Preparer) reloadPartitionTable(ctx context.Context, disk string) error {
	if err := p.run.Run(ctx, "partprobe", disk); err != nil {
		log.Printf("partprobe failed, trying partx: %v", err)
		if err2 := p.run.Run(ctx, "partx", "-u", disk); err2 != nil {
			return fmt.Errorf("partprobe and partx both failed: partprobe=%v, partx=%v", err, err2)
		}
	}
	return nil
}

// EnsureDirectories creates the required directory tree on the data root.
func (p *Preparer) EnsureDirectories(ctx context.Context) error {
	dirs := []string{
		paths.DataJoin("node"),
		paths.DataJoin("user", "volumes"),
		paths.DataJoin("federation"),
		paths.DataJoin("system-objects", "control-plane-backups"),
		paths.DataJoin("system-objects", "volume-checkpoints"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}

// SetNOCOWAttributes sets the NOCOW attribute on directories that benefit
// from disabling btrfs copy-on-write (high-churn, disposable data).
// Failures are logged but not fatal.
func (p *Preparer) SetNOCOWAttributes(ctx context.Context) {
	nocowDirs := []string{
		paths.DataJoin("node"),
		paths.DataJoin("federation"),
	}
	for _, dir := range nocowDirs {
		if err := p.run.Run(ctx, "chattr", "+C", dir); err != nil {
			log.Printf("WARN: failed to set NOCOW on %s: %v", dir, err)
		}
	}
}
