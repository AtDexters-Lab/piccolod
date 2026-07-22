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

	"piccolod/internal/resources/pressure"
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
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
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
	totalSectors = totalSectors * 512 / int64(sectorSize) // blockdev --getsz always returns 512-byte sectors
	if totalSectors > lastEndSector {
		state.UnallocatedGB = int((totalSectors - lastEndSector) * int64(sectorSize) / (1 << 30))
	}

	// Look for data partition (LUKS type code 8309, LUKS header, or LVM PV)
	dataNode, dataSlot := p.findDataPartition(ctx, disk, sfdisk)
	if dataNode != "" {
		state.DataPartition = dataNode
		state.DataPartitionSlot = dataSlot
	}

	// Determine if data partition needs expansion: it must exist, be the last
	// partition on disk, and have unallocated space following it.
	if state.DataPartition != "" && state.UnallocatedGB > 0 {
		for _, part := range sfdisk.PartitionTable.Partitions {
			if part.Node == state.DataPartition {
				if part.Start+part.Size == lastEndSector {
					state.DataNeedsExpansion = true
				}
				break
			}
		}
	}

	return state, nil
}

// VerifyPiccoloCoreExists checks whether the core subvolume path exists.
func (p *Preparer) VerifyPiccoloCoreExists(ctx context.Context, corePath string) bool {
	info, err := os.Stat(corePath)
	return err == nil && info.IsDir()
}

// FindNextSlot finds the first unused partition slot from sfdisk output.
// GPT allows up to 128 slots; MBR allows 4 primary slots.
func FindNextSlot(sfdisk storage.SfdiskOutput) int {
	maxSlots := 128
	if sfdisk.IsMBR() {
		maxSlots = 4
	}
	used := make(map[int]bool)
	for _, part := range sfdisk.PartitionTable.Partitions {
		slot := extractSlotNumber(part.Node)
		if slot > 0 {
			used[slot] = true
		}
	}
	for slot := 1; slot <= maxSlots; slot++ {
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

// hasFilesystem checks if a device has any filesystem or volume signature via blkid.
// Returns true if blkid detects a signature (ext4, btrfs, xfs, swap, LVM2_member, etc.),
// false only if blkid explicitly reports no signature (exit code 2).
// On probe errors (I/O, missing binary) returns true as a fail-safe to avoid
// misidentifying an existing partition as raw.
func hasFilesystem(ctx context.Context, run runner.CommandRunner, device string) bool {
	err := run.Run(ctx, "blkid", "-p", device)
	if err == nil {
		return true // blkid found a signature
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		return false // exit code 2 = no signature found → raw partition
	}
	return true // probe error → fail-safe: assume filesystem exists
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
//
// Limitation: on LUKS/LVM systems findmnt returns the mapped device
// (e.g. /dev/mapper/root) which won't match physical partition nodes in
// sfdisk output. Piccolo OS does not use LUKS/LVM for root, so this is
// acceptable. If that changes, resolve the mapper device to its physical
// partition here (e.g. via lsblk -ndo PKNAME).
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

// findDataPartition looks for a data partition by LUKS type code, LUKS header,
// or LVM PV signature.
// On GPT, the LUKS type GUID provides a fast path. On MBR, there is no distinct
// LUKS type code (both root and data use 0x83), so we go straight to header probing.
func (p *Preparer) findDataPartition(ctx context.Context, disk string, sfdisk storage.SfdiskOutput) (node string, slot int) {
	// GPT fast path: Linux LUKS partition type GUID
	if sfdisk.IsGPT() {
		const luksTypeGUID = "CA7D7CCB-63ED-4C53-861C-1742536059CC"
		for _, part := range sfdisk.PartitionTable.Partitions {
			if part.Node == "" {
				continue
			}
			if strings.EqualFold(part.Type, luksTypeGUID) {
				return part.Node, extractSlotNumber(part.Node)
			}
		}
	}
	// MBR + GPT fallback: probe each non-root partition for LUKS header
	rootDev, _ := p.getRootDevice(ctx)
	for _, part := range sfdisk.PartitionTable.Partitions {
		if part.Node == "" || part.Node == rootDev {
			continue
		}
		if ok, _ := IsLUKS(ctx, p.run, part.Node); ok {
			return part.Node, extractSlotNumber(part.Node)
		}
	}
	// LVM PV: in block-native mode the data partition hosts LVM directly
	// (no pool-level LUKS). hasFilesystem() returns true for LVM2_member
	// signatures, so the MBR last-resort (raw type-83) cannot match — this
	// tier bridges that gap.
	for _, part := range sfdisk.PartitionTable.Partitions {
		if part.Node == "" || part.Node == rootDev {
			continue
		}
		if isLVMPV(ctx, p.run, part.Node) {
			return part.Node, extractSlotNumber(part.Node)
		}
	}
	// MBR last resort: accept a non-root Linux (type 83) partition that is raw
	// (no filesystem signature). On MBR there is no LUKS-specific type code, so
	// a data partition created in Phase 1 but not yet initialized appears as
	// plain type 83 with no signature. The hasFilesystem guard prevents
	// misidentifying a pre-existing ext4/xfs/btrfs partition as the data
	// partition. LVM PVs are caught by the tier above.
	if sfdisk.IsMBR() {
		for _, part := range sfdisk.PartitionTable.Partitions {
			if part.Node == "" || part.Node == rootDev {
				continue
			}
			if strings.EqualFold(part.Type, "83") && !hasFilesystem(ctx, p.run, part.Node) {
				return part.Node, extractSlotNumber(part.Node)
			}
		}
	}
	return "", 0
}

// isLVMPV checks if a device is an LVM physical volume.
func isLVMPV(ctx context.Context, run runner.CommandRunner, device string) bool {
	out, err := run.RunWithOutput(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "LVM2_member"
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

// BuildSfdiskJSON builds sfdisk -J compatible JSON for testing.
// If label is empty, defaults to "gpt" for backwards compatibility.
func BuildSfdiskJSON(sectorSize int, label string, partitions []storage.SfdiskPartition) []byte {
	if label == "" {
		label = "gpt"
	}
	type table struct {
		SectorSize int                        `json:"sectorsize"`
		Label      storage.PartitionTableType `json:"label"`
		Partitions []storage.SfdiskPartition  `json:"partitions"`
	}
	data, _ := json.Marshal(struct {
		PartitionTable table `json:"partitiontable"`
	}{
		PartitionTable: table{
			SectorSize: sectorSize,
			Label:      storage.PartitionTableType(label),
			Partitions: partitions,
		},
	})
	return data
}

// CreateDataPartition creates a new data partition for the LVM pool using the remaining disk space.
// The data partition is created BEFORE root expansion so it acts as a boundary for growpart.
// Dispatches to GPT (sgdisk) or MBR (sfdisk) based on the partition table label.
func (p *Preparer) CreateDataPartition(ctx context.Context, disk string) (string, int, error) {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return "", 0, err
	}
	sfdisk, err := p.readSfdisk(ctx, disk)
	if err != nil {
		return "", 0, fmt.Errorf("read partition table: %w", err)
	}

	slot := FindNextSlot(sfdisk)
	if slot == 0 {
		return "", 0, fmt.Errorf("no free partition slot on %s", disk)
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

	if sfdisk.IsMBR() {
		return p.createDataPartitionMBR(ctx, disk, sfdisk, layout, sectorSize, slot)
	}
	return p.createDataPartitionGPT(ctx, disk, sfdisk, layout, sectorSize, slot)
}

// computeDataStartSector determines where the data partition should begin.
// It finds the root partition's actual start from the partition table, computes
// where the root's target size ends, and ensures we never overlap any existing
// partition. The result is 1 MiB aligned.
func (p *Preparer) computeDataStartSector(ctx context.Context, sfdisk storage.SfdiskOutput, layout storage.PartitionLayout, sectorSize int) (int64, error) {
	rootDev, err := p.getRootDevice(ctx)
	if err != nil {
		return 0, fmt.Errorf("detect root device: %w", err)
	}

	var rootStart int64
	var foundRoot bool
	for _, part := range sfdisk.PartitionTable.Partitions {
		if part.Node == rootDev {
			rootStart = part.Start
			foundRoot = true
			break
		}
	}
	if !foundRoot {
		return 0, fmt.Errorf("root partition %s not found in partition table", rootDev)
	}

	// Data partition starts after root's TARGET size (not its current image size),
	// because growpart uses the data partition as a boundary.
	rootTargetEnd := rootStart + int64(layout.RootGB)*(1<<30)/int64(sectorSize)

	// Also consider actual end of all partitions (safety: never overlap).
	var lastEnd int64
	for _, part := range sfdisk.PartitionTable.Partitions {
		if end := part.Start + part.Size; end > lastEnd {
			lastEnd = end
		}
	}

	startSector := max(rootTargetEnd, lastEnd)

	// Align to 1 MiB boundary.
	sectorsPerMiB := int64(1<<20) / int64(sectorSize)
	startSector = ((startSector + sectorsPerMiB - 1) / sectorsPerMiB) * sectorsPerMiB

	return startSector, nil
}

// createDataPartitionGPT creates the data partition on a GPT disk using sgdisk.
func (p *Preparer) createDataPartitionGPT(ctx context.Context, disk string, sfdisk storage.SfdiskOutput, layout storage.PartitionLayout, sectorSize, slot int) (string, int, error) {
	// Repair GPT: when the OS image is written to a larger disk, the backup GPT
	// header remains at the old end-of-disk position. sgdisk refuses to operate
	// until this is fixed. sgdisk -e moves the backup structures to the correct
	// position and is safe to run idempotently.
	if err := p.run.Run(ctx, "sgdisk", "-e", disk); err != nil {
		log.Printf("WARN: sgdisk -e (GPT repair) failed: %v (continuing)", err)
	}

	startSector, err := p.computeDataStartSector(ctx, sfdisk, layout, sectorSize)
	if err != nil {
		return "", 0, err
	}

	slotStr := strconv.Itoa(slot)
	startStr := strconv.FormatInt(startSector, 10)

	// sgdisk: -n slot:start:0 means "start at startSector, extend to end of disk"
	// -t slot:8309 sets the LUKS type GUID (partition hosts LVM; per-volume LUKS in M3)
	// -c slot:piccolo-data sets a human-readable partition label
	if err := p.run.Run(ctx, "sgdisk",
		"-n", slotStr+":"+startStr+":0",
		"-t", slotStr+":8309",
		"-c", slotStr+":piccolo-data",
		disk); err != nil {
		return "", 0, fmt.Errorf("sgdisk create partition: %w", err)
	}

	return p.waitForPartition(ctx, disk, slot)
}

// createDataPartitionMBR creates the data partition on an MBR disk using sfdisk.
func (p *Preparer) createDataPartitionMBR(ctx context.Context, disk string, sfdisk storage.SfdiskOutput, layout storage.PartitionLayout, sectorSize, slot int) (string, int, error) {
	startSector, err := p.computeDataStartSector(ctx, sfdisk, layout, sectorSize)
	if err != nil {
		return "", 0, err
	}

	// sfdisk -N <slot> writes to a specific partition slot.
	// Type 83 = Linux. No size → uses remaining disk space.
	stdin := []byte(fmt.Sprintf("start=%d, type=83\n", startSector))
	slotStr := strconv.Itoa(slot)

	// --force: override "disk in use" check (root is mounted on this disk).
	// --no-reread: skip sfdisk's internal BLKRRPART (we handle kernel notification
	// ourselves via registerNewPartition, which uses targeted BLKPG_ADD_PARTITION).
	if err := p.run.RunWithStdin(ctx, stdin, "sfdisk", "--force", "--no-reread", "-N", slotStr, disk); err != nil {
		return "", 0, fmt.Errorf("sfdisk create partition: %w", err)
	}

	return p.waitForPartition(ctx, disk, slot)
}

// waitForPartition reloads the partition table and waits for the kernel to
// register the new partition device node (up to ~2 seconds).
func (p *Preparer) waitForPartition(ctx context.Context, disk string, slot int) (string, int, error) {
	if err := p.registerNewPartition(ctx, disk, slot); err != nil {
		return "", 0, fmt.Errorf("register new partition: %w", err)
	}

	partDev := storage.PartitionDevicePath(disk, slot)

	for attempt := range 10 {
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
	panic("unreachable")
}

// ExpandRootPartition expands the root partition and filesystem to fill
// available space up to the data partition boundary.
func (p *Preparer) ExpandRootPartition(ctx context.Context, disk string, rootPartition string) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	// Extract the root partition number for growpart.
	rootSlot := extractSlotNumber(rootPartition)
	if rootSlot == 0 {
		return fmt.Errorf("cannot determine root partition slot from %s", rootPartition)
	}

	// growpart expands the partition to fill available space (bounded by next partition).
	// growpart exits non-zero with NOCHANGE when partition is already at max size.
	// NOCHANGE may appear on stdout or stderr — check both.
	if out, err := p.run.RunWithOutput(ctx, "growpart", disk, strconv.Itoa(rootSlot)); err != nil {
		if strings.Contains(string(out), "NOCHANGE") || strings.Contains(err.Error(), "NOCHANGE") {
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

// ExpandDataPartition expands the data partition to fill remaining disk space.
// For GPT disks, repairs the backup header first (sgdisk -e) since the image
// may have been written to a larger disk. Uses growpart which is online-safe
// and idempotent (NOCHANGE = not an error). No filesystem resize is needed
// because the partition is a raw LVM PV; the LVM-level expansion (pvresize +
// lvextend) happens separately in Phase 2.
func (p *Preparer) ExpandDataPartition(ctx context.Context, disk string, dataPartition string) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	dataSlot := extractSlotNumber(dataPartition)
	if dataSlot == 0 {
		return fmt.Errorf("cannot determine data partition slot from %s", dataPartition)
	}

	// Repair GPT backup header if needed. On GPT disks with an image deployed
	// to a larger disk, the backup header sits at the old end-of-disk position.
	// sgdisk -e moves it. Safe to run idempotently; on MBR disks sgdisk fails
	// harmlessly.
	if _, err := p.run.RunWithOutput(ctx, "sgdisk", "-e", disk); err != nil {
		log.Printf("WARN: sgdisk -e (GPT repair) on %s: %v (continuing)", disk, err)
	}

	// growpart expands the partition to fill remaining space (to end of disk
	// for the last partition). NOCHANGE may appear on stdout or stderr.
	if out, err := p.run.RunWithOutput(ctx, "growpart", disk, strconv.Itoa(dataSlot)); err != nil {
		if strings.Contains(string(out), "NOCHANGE") || strings.Contains(err.Error(), "NOCHANGE") {
			log.Printf("data partition %s already at maximum size", dataPartition)
			return nil
		}
		return fmt.Errorf("growpart data partition: %w", err)
	}

	log.Printf("data partition %s expanded", dataPartition)
	return nil
}

// registerNewPartition notifies the kernel about a newly created partition.
// Uses partx --add --nr (BLKPG_ADD_PARTITION ioctl) which works even when
// other partitions on the disk are mounted. Falls back to full table reload.
func (p *Preparer) registerNewPartition(ctx context.Context, disk string, slot int) error {
	slotStr := fmt.Sprintf("%d:%d", slot, slot)
	if err := p.run.Run(ctx, "partx", "--add", "--nr", slotStr, disk); err != nil {
		log.Printf("partx --add --nr %s failed (%v), falling back to full table reload", slotStr, err)
		return p.reloadPartitionTable(ctx, disk)
	}
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

// EnsureDirectories creates the required directory tree under the core root.
// In the block-native architecture, the data partition no longer has a filesystem;
// all directories live under the core root or on per-volume mounts.
func (p *Preparer) EnsureDirectories(ctx context.Context) error {
	dirs := []string{
		paths.CoreJoin("volumes"),
		paths.CoreJoin("mounts"),
		paths.CoreJoin("cache"),
		paths.CoreJoin("cache", "catalog"),
		paths.CoreJoin("tiering"),
		paths.CoreJoin("drbd-meta"),
		paths.CoreJoin("recovery"),
		paths.CoreJoin("federation"),
		paths.CoreJoin("system-objects", "control-plane-backups"),
		paths.CoreJoin("system-objects", "volume-checkpoints"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}
