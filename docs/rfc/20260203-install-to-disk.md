# RFC: Install to Disk (x86_64 Live USB → Internal Disk)

- **Status:** Draft
- **Date:** 2026-02-07
- **Authors:** Engineering Team
- **Related:**
  - `docs/rfc/20260201-storage-posture.md` (disk posture, partitioning, LUKS2, USB boot onboarding)
  - `docs/rfc/20260202-storage-v2-foundation.md` (PCV exports, control-plane layout, path contracts)
  - `org-context/02_product/acceptance_features/install_to_disk_x86.feature` (product acceptance)
  - `org-context/02_product/piccolo_os_prd.md` §Distribution & install (product requirements)

## 1. Summary

This RFC specifies the **Install to Disk** flow for x86_64 systems booted from a live USB image. It is the companion RFC referenced by Posture RFC §9.4, which defers detailed specification of the installation pipeline to this document.

Install to Disk copies a running Piccolo OS from a live USB to an internal disk, producing a system in the production two-root posture (`/piccolo-core` on root btrfs, `/piccolo-data` on LUKS2 + btrfs). Two modes are supported:

1. **Fresh start** — wipe the target disk, sync the OS, reboot into first-run setup.
2. **Carry over current state** — sync the OS plus control-plane state, apps, and configuration from the USB "Try Piccolo" session.

A **dry-run simulation** mode computes and displays the full plan without writing to disk.

### 1.1 Relationship to Parent RFCs

- **Posture RFC §9.4** defines the storage contracts the installer must satisfy (target disk ends in the two-root posture, USB is never modified). This RFC specifies *how*.
- **Foundation RFC §5.1/§5.2** defines the directory layouts on `/piccolo-core` and `/piccolo-data` that the installer must create on the target.
- **Posture RFC §6** defines the LUKS2 key hierarchy and keyslot enrollment that the installer replicates on the target's `/piccolo-data` partition.

## 2. Prerequisites

Install to Disk is available only when **all** of the following are true:

| Condition | Check | Source |
|---|---|---|
| Boot mode is USB (or Unknown) | `DetectBootMode()` returns `BootModeUSB` or `BootModeUnknown` | Posture RFC §4.1 |
| At least one non-boot internal disk exists | `discoverInternalDisks(ctx, bootDisk)` returns ≥ 1 result | Posture RFC §9.2 |
| Phase 1 disk prep on USB is complete | `IsPhase1Complete() == true` | Posture RFC §5.2 |
| User has not already installed to disk | `OnboardingState != "complete"` with install target | Onboarding state machine |

If the user chose "Try Piccolo" first, Install to Disk remains accessible via the Settings page (per PRD and acceptance feature `install_to_disk_x86.feature` scenario 2). If the user chose "Install to Disk" from the landing page, the installation wizard launches directly.

## 3. Disk Discovery and Selection

### 3.1 Target Disk Discovery

Reuse `discoverInternalDisks()` from Posture RFC §9.2. For each candidate disk, collect:

```go
type TargetDisk struct {
    // Identifiers
    DevPath    string `json:"dev_path"`     // e.g., /dev/sda
    ByIDPath   string `json:"by_id_path"`   // e.g., /dev/disk/by-id/ata-Samsung_SSD_850_EVO_250GB_S21PNXAG...
    Serial     string `json:"serial"`       // Drive serial number

    // Display info
    Model      string `json:"model"`        // e.g., "Samsung SSD 850 EVO 250GB"
    SizeBytes  int64  `json:"size_bytes"`
    SizeHuman  string `json:"size_human"`   // e.g., "250 GB"
    Transport  string `json:"transport"`    // e.g., "sata", "nvme"

    // Existing contents (for erase warning)
    Partitions []ExistingPartition `json:"partitions"`
    HasData    bool                `json:"has_data"` // True if any recognized filesystem or LUKS header found
}

type ExistingPartition struct {
    Number     int    `json:"number"`
    SizeBytes  int64  `json:"size_bytes"`
    TypeCode   string `json:"type_code"`    // GPT type code
    Label      string `json:"label"`        // Partition label (if any)
    FSType     string `json:"fs_type"`      // Detected filesystem (ext4, btrfs, ntfs, etc.)
    FSLabel    string `json:"fs_label"`     // Filesystem label (if any)
    LUKSHeader bool   `json:"luks_header"`  // True if LUKS header detected
}
```

**Discovery implementation:** Use `lsblk -JdO` for disk-level metadata (model, size, serial, transport) and `sfdisk -J` + `blkid` for partition and filesystem detection. Filter out:
- The boot disk (identified by the root device's parent disk)
- USB-connected devices (`TRAN == "usb"`)
- Loop devices, CD-ROMs, and other non-disk block devices

### 3.2 User-Facing Identifier

Per PRD requirement, target disks are presented by their `/dev/disk/by-id/*` path. This path is stable across reboots (unlike `/dev/sdX` which depends on probe order) and includes the drive model and serial for human identification.

```go
// resolveByIDPath finds the /dev/disk/by-id/ symlink for a block device.
// Prefers ata-* or nvme-* links over generic wwn-* links.
func resolveByIDPath(devPath string) (string, error) {
    entries, err := os.ReadDir("/dev/disk/by-id")
    if err != nil {
        return "", err
    }
    var best string
    for _, e := range entries {
        target, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-id", e.Name()))
        if err != nil {
            continue
        }
        if target == devPath {
            // Prefer ata-/nvme- prefixed links (most readable)
            if strings.HasPrefix(e.Name(), "ata-") || strings.HasPrefix(e.Name(), "nvme-") {
                return filepath.Join("/dev/disk/by-id", e.Name()), nil
            }
            if best == "" {
                best = filepath.Join("/dev/disk/by-id", e.Name())
            }
        }
    }
    if best == "" {
        return "", fmt.Errorf("no /dev/disk/by-id link for %s", devPath)
    }
    return best, nil
}
```

### 3.3 Confirmation UX

Per PRD: "type the exact disk id to confirm." The portal requires the user to type the full `/dev/disk/by-id/*` identifier (or a unique suffix) into a confirmation field. This prevents accidental selection of the wrong disk.

**Minimum disk size:** The target disk must satisfy `calculatePartitionLayout()` from Posture RFC §5.4 — at minimum, enough space for the ESP (~512MB) + root (~20GB or proportional) + `/piccolo-data` (≥5GB).

## 4. Dry-Run Simulation

Before any writes, the user can request a simulation that computes and displays the full installation plan.

**Simulation output:**

```go
type InstallPlan struct {
    TargetDisk     TargetDisk        `json:"target_disk"`
    Mode           InstallMode       `json:"mode"`           // "fresh" or "carry_over"
    WillErase      bool              `json:"will_erase"`     // Always true (full disk wipe)
    Partitions     []PlannedPartition `json:"partitions"`
    SourceSnapshot string            `json:"source_snapshot"` // btrfs snapshot path on USB
    EstimatedSize  int64             `json:"estimated_size"`  // Bytes to transfer
    Warnings       []string          `json:"warnings"`        // e.g., "Target disk contains existing data"
}

type PlannedPartition struct {
    Number   int    `json:"number"`
    Label    string `json:"label"`     // "ESP", "piccolo-root", "piccolo-data"
    TypeCode string `json:"type_code"` // GPT type code
    SizeGB   int    `json:"size_gb"`
    FSType   string `json:"fs_type"`   // "vfat", "btrfs", "luks2+btrfs"
}
```

The simulation:
1. Validates the target disk (size, not the boot disk, accessible, **not currently mounted**). The installer verifies no partitions on the target disk are mounted (via `findmnt --source <dev>*` or `/proc/mounts` scan) and no dm-crypt mappers are open on the target. Running `sgdisk --zap-all` on a mounted disk would cause data corruption. If any partition is in use, the endpoint returns an error instructing the user to unmount first.
2. Computes the partition layout using `calculatePartitionLayout()` from Posture RFC §5.4.
3. Identifies the active btrfs snapshot on the USB to determine transfer size.
4. Lists all existing partitions on the target (for the erase warning).
5. Returns the plan without writing anything.

## 5. Installation Pipeline (Fresh Start)

A fresh start wipes the target disk and installs a clean Piccolo OS. After reboot, the user goes through first-run setup (create admin, explore dashboard).

### Phase 1: Target Disk Preparation

Wipe the target disk and create the production partition layout matching Posture RFC §2.2.

```
Target disk (e.g., 256GB):
├── Partition 1: ESP (~512MB, FAT32, type EF00)
├── Partition 2: Root btrfs (~20GB, type 8300)
└── Partition 3: /piccolo-data (remaining space, type 8309 — Linux LUKS)
```

```go
func (inst *Installer) prepareTargetDisk(ctx context.Context, target string) error {
    // 1. Wipe existing partition table
    if err := inst.runner.Run(ctx, "sgdisk", "--zap-all", target); err != nil {
        return fmt.Errorf("failed to wipe target disk: %w", err)
    }

    // 2. Compute partition layout
    diskSizeGB, err := inst.getDiskSizeGB(ctx, target)
    if err != nil {
        return err
    }
    layout, err := calculatePartitionLayout(diskSizeGB)
    if err != nil {
        return fmt.Errorf("target disk too small: %w", err)
    }

    sectorSize, sectorErr := inst.getSectorSize(ctx, target)
    if sectorErr != nil {
        // All three partition boundaries depend on sector size; a wrong default
        // would silently misalign the layout. Fail hard rather than guess.
        return fmt.Errorf("cannot determine sector size for %s: %w", target, sectorErr)
    }

    // 3. Create ESP (partition 1)
    // ESP starts at 1MiB (standard alignment), size ~512MB
    espSizeSectors := (512 * 1024 * 1024) / sectorSize
    espStart := int64(1024 * 1024 / sectorSize) // 1MiB aligned
    espEnd := espStart + espSizeSectors - 1

    // 4. Create root partition (partition 2)
    rootStart := espEnd + 1
    // Align to 1MiB boundary
    alignSectors := int64(1024 * 1024 / sectorSize)
    rootStart = ((rootStart + alignSectors - 1) / alignSectors) * alignSectors
    rootSizeSectors := (int64(layout.RootGB) * 1024 * 1024 * 1024) / sectorSize
    rootEnd := rootStart + rootSizeSectors - 1

    // 5. Create data partition (partition 3) — from root end to disk end
    dataStart := rootEnd + 1
    dataStart = ((dataStart + alignSectors - 1) / alignSectors) * alignSectors

    if err := inst.runner.Run(ctx, "sgdisk",
        // ESP
        "-n", fmt.Sprintf("1:%d:%d", espStart, espEnd),
        "-t", "1:EF00",
        "-c", "1:EFI",
        // Root
        "-n", fmt.Sprintf("2:%d:%d", rootStart, rootEnd),
        "-t", "2:8300",
        "-c", "2:piccolo-root",
        // Data (extends to end of disk)
        "-n", fmt.Sprintf("3:%d:0", dataStart),
        "-t", "3:8309",
        "-c", "3:piccolo-data",
        target,
    ); err != nil {
        return fmt.Errorf("sgdisk partition creation failed: %w", err)
    }

    // 6. Reload partition table
    if err := inst.runner.Run(ctx, "partprobe", target); err != nil {
        return fmt.Errorf("partprobe failed: %w", err)
    }

    // 7. Verify all partition device nodes exist
    for _, slot := range []int{1, 2, 3} {
        partDev := partitionDevicePath(target, slot)
        if err := waitForDeviceNode(ctx, partDev, 10, 200*time.Millisecond); err != nil {
            return fmt.Errorf("kernel did not register partition %s: %w", partDev, err)
        }
    }

    return nil
}
```

### Phase 2: ESP Setup

Format the target ESP and copy boot files from the USB ESP.

```go
func (inst *Installer) setupESP(ctx context.Context, target string) error {
    espDev := partitionDevicePath(target, 1)

    // 1. Format as FAT32
    if err := inst.runner.Run(ctx, "mkfs.vfat", "-F", "32", "-n", "EFI", espDev); err != nil {
        return fmt.Errorf("mkfs.vfat failed: %w", err)
    }

    // 2. Mount target ESP
    targetESPMount := "/run/piccolo/install/esp"
    if err := os.MkdirAll(targetESPMount, 0700); err != nil {
        return err
    }
    if err := inst.runner.Run(ctx, "mount", espDev, targetESPMount); err != nil {
        return fmt.Errorf("failed to mount target ESP: %w", err)
    }
    defer inst.runner.Run(ctx, "umount", targetESPMount)

    // 3. Find and mount source ESP (partition 1 on the boot disk)
    sourceESPMount := "/run/piccolo/install/source-esp"
    if err := os.MkdirAll(sourceESPMount, 0700); err != nil {
        return err
    }
    sourceESP := partitionDevicePath(inst.bootDisk, 1)
    if err := inst.runner.Run(ctx, "mount", "-o", "ro", sourceESP, sourceESPMount); err != nil {
        return fmt.Errorf("failed to mount source ESP: %w", err)
    }
    defer inst.runner.Run(ctx, "umount", sourceESPMount)

    // 4. Copy all ESP contents (EFI directory, kernel, initrd)
    // cp -a preserves directory structure, permissions, and timestamps
    if err := inst.runner.Run(ctx, "cp", "-a",
        sourceESPMount+"/.", targetESPMount+"/"); err != nil {
        return fmt.Errorf("failed to copy ESP contents: %w", err)
    }

    return nil
}
```

**ESP contents (copied from USB):**
- `EFI/BOOT/BOOTX64.EFI` — UEFI shim (Secure Boot signed)
- `EFI/opensuse/grubx64.efi` — GRUB2 (signed by SUSE shim)
- `EFI/opensuse/grub.cfg` — GRUB configuration (updated in Phase 4)
- Kernel (`vmlinuz`) and initrd images

### Phase 3: Root Filesystem Sync

Use `btrfs send | btrfs receive` to sync the active root snapshot from USB to the target disk.

```go
func (inst *Installer) syncRootFilesystem(ctx context.Context, target string) error {
    rootDev := partitionDevicePath(target, 2)

    // 1. Create btrfs on target root partition
    if err := inst.runner.Run(ctx, "mkfs.btrfs", "-f", "-L", "piccolo-root", rootDev); err != nil {
        return fmt.Errorf("mkfs.btrfs failed: %w", err)
    }

    // 2. Mount target root
    targetRootMount := "/run/piccolo/install/target-root"
    if err := os.MkdirAll(targetRootMount, 0700); err != nil {
        return err
    }
    if err := inst.runner.Run(ctx, "mount", rootDev, targetRootMount); err != nil {
        return err
    }
    defer inst.runner.Run(ctx, "umount", "-R", targetRootMount)

    // 3. Find active snapshot on USB
    // MicroOS active snapshot is typically /.snapshots/<N>/snapshot
    activeSnap, err := inst.findActiveSnapshot(ctx)
    if err != nil {
        return fmt.Errorf("failed to find active snapshot: %w", err)
    }

    // 4. Clean up any leftover send snapshot from a previous failed attempt
    sendSnap := activeSnap + "-install-send"
    _ = inst.runner.Run(ctx, "btrfs", "subvolume", "delete", sendSnap) // ignore error if absent

    // 5. Create read-only snapshot for send
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "snapshot", "-r",
        activeSnap, sendSnap); err != nil {
        return fmt.Errorf("failed to create send snapshot: %w", err)
    }
    defer inst.runner.Run(ctx, "btrfs", "subvolume", "delete", sendSnap)

    // 6. btrfs send | btrfs receive
    // Pipe send output directly to receive for streaming transfer
    if err := inst.runPiped(ctx,
        []string{"btrfs", "send", sendSnap},
        []string{"btrfs", "receive", targetRootMount},
    ); err != nil {
        return fmt.Errorf("btrfs send/receive failed: %w", err)
    }

    // 7. The received snapshot is read-only; create a writable snapshot
    // as the new default subvolume
    receivedName := filepath.Base(sendSnap)
    receivedPath := filepath.Join(targetRootMount, receivedName)
    defaultSubvol := filepath.Join(targetRootMount, "@")
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "snapshot",
        receivedPath, defaultSubvol); err != nil {
        return fmt.Errorf("failed to create writable snapshot: %w", err)
    }

    // 8. Set the writable snapshot as default
    subvolID, err := inst.getSubvolumeID(ctx, defaultSubvol)
    if err != nil {
        return err
    }
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "set-default",
        subvolID, targetRootMount); err != nil {
        return fmt.Errorf("failed to set default subvolume: %w", err)
    }

    // 9. Clean up the read-only received snapshot
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "delete", receivedPath); err != nil {
        inst.logger.Warn("failed to clean up received snapshot", "error", err)
    }

    // 10. Recreate nested subvolumes that btrfs send does NOT include.
    // CRITICAL: btrfs send only transfers the content of the sent subvolume;
    // nested subvolumes appear as empty directories in the receive stream.
    // /piccolo-core and /.snapshots are nested subvolumes in the source OS
    // image and must be explicitly recreated on the target.
    nestedSubvols := []string{"piccolo-core", ".snapshots"}
    for _, name := range nestedSubvols {
        subvolPath := filepath.Join(defaultSubvol, name)
        // Remove the empty directory left by btrfs receive
        if err := os.Remove(subvolPath); err != nil && !os.IsNotExist(err) {
            return fmt.Errorf("failed to remove placeholder directory %s: %w", name, err)
        }
        // Create as a proper btrfs subvolume
        if err := inst.runner.Run(ctx, "btrfs", "subvolume", "create", subvolPath); err != nil {
            return fmt.Errorf("failed to create %s subvolume: %w", name, err)
        }
    }

    return nil
}
```

**Snapshot scope:** Only the active snapshot is sent — the full snapshot history is not transferred. This keeps transfer time proportional to actual used data, not to historical state.

**Nested subvolume limitation:** `btrfs send` does **not** recursively include nested subvolumes in the send stream. Nested subvolumes (such as `/piccolo-core` and `/.snapshots`) appear as **empty directories** on the receiving side. Step 10 in the pseudocode above explicitly removes these empty placeholder directories and recreates them as proper btrfs subvolumes. Any files that existed inside these nested subvolumes on the source (e.g., snapper metadata under `/.snapshots/`) are **not** transferred by `btrfs send` — they are either recreated by the system on first boot (snapper) or populated by later phases of the installer (Phase 5 for `/piccolo-core`).

**MicroOS/snapper compatibility:** After creating the writable `@` subvolume on the target, the installer must:
1. Reset `grubenv` in the target ESP to clear `saved_entry` (which may reference a USB-specific snapshot number): `grub2-editenv <target-esp>/EFI/opensuse/grubenv unset saved_entry`.
2. Run `snapper --root <target-root>/@/ setup-quota` to initialize snapper's quota tracking on the target.
3. The `/.snapshots/` subvolume was recreated in step 10 above (empty); snapper will populate it on first boot when `transactional-update` creates the first snapshot.

On first boot, MicroOS's `transactional-update` creates a new read-only snapshot from the current state. As long as the `@` subvolume is the default and `/.snapshots/` is a valid subvolume, snapper continues numbering from the highest existing snapshot number + 1. No explicit snapper re-configuration is needed beyond the `grubenv` reset.

### Phase 4: Post-Sync Fixups

After the filesystem sync, update configurations to reference the new disk's UUIDs.

```go
func (inst *Installer) applyPostSyncFixups(ctx context.Context, target string) error {
    rootDev := partitionDevicePath(target, 2)
    espDev := partitionDevicePath(target, 1)
    dataDev := partitionDevicePath(target, 3)

    // Get new UUIDs from target partitions
    rootUUID, err := inst.getBlkidUUID(ctx, rootDev)
    if err != nil {
        return fmt.Errorf("failed to get root UUID: %w", err)
    }
    espUUID, err := inst.getBlkidUUID(ctx, espDev)
    if err != nil {
        return fmt.Errorf("failed to get ESP UUID: %w", err)
    }

    targetRootMount := "/run/piccolo/install/target-root"

    // 1. Update /etc/fstab on the target
    fstabPath := filepath.Join(targetRootMount, "@/etc/fstab")
    if err := inst.updateFstab(ctx, fstabPath, rootUUID, espUUID); err != nil {
        return fmt.Errorf("failed to update fstab: %w", err)
    }

    // 2. Update GRUB configuration to point to new partition UUIDs
    // grub.cfg may contain: search --fs-uuid <root-uuid>, root=UUID=<root-uuid>,
    // and search --fs-uuid <esp-uuid>. All must be updated.
    targetESPMount := "/run/piccolo/install/esp"
    if err := inst.runner.Run(ctx, "mount", espDev, targetESPMount); err != nil {
        return err
    }
    defer inst.runner.Run(ctx, "umount", targetESPMount)

    // Get source UUIDs to find-and-replace
    sourceRootUUID, _ := inst.getBlkidUUID(ctx, partitionDevicePath(inst.bootDisk, 2))
    sourceESPUUID, _ := inst.getBlkidUUID(ctx, partitionDevicePath(inst.bootDisk, 1))

    grubCfgPath := filepath.Join(targetESPMount, "EFI/opensuse/grub.cfg")
    if err := inst.replaceUUIDsInFile(ctx, grubCfgPath,
        sourceRootUUID, rootUUID,
        sourceESPUUID, espUUID); err != nil {
        return fmt.Errorf("failed to update GRUB config: %w", err)
    }

    // 3. Reset grubenv saved_entry (may reference USB-specific snapshot IDs)
    grubenvPath := filepath.Join(targetESPMount, "EFI/opensuse/grubenv")
    if _, err := os.Stat(grubenvPath); err == nil {
        inst.runner.Run(ctx, "grub2-editenv", grubenvPath, "unset", "saved_entry")
    }

    // 4. Regenerate initrd to embed the target's root UUID.
    // The copied initrd was built by dracut with the USB's root partition UUID.
    // On MicroOS, dracut may bake the root device UUID into the initrd (depending
    // on the dracut modules used). The kernel command line `root=UUID=` in grub.cfg
    // (updated above) should take precedence, but some dracut configurations also
    // embed a compiled-in default. Regenerating via chroot + dracut --force ensures
    // the initrd references the correct target root UUID regardless of dracut config.
    chrootTarget := filepath.Join(targetRootMount, "@")

    // Bind-mount /dev, /proc, /sys into chroot for dracut.
    // NOTE: /boot must be accessible inside chrootTarget. On MicroOS, /boot is
    // part of the root subvolume (@), so it is already present. If a future
    // layout uses a separate /boot partition, bind-mount it here as well.
    for _, mount := range []string{"/dev", "/proc", "/sys"} {
        dst := filepath.Join(chrootTarget, mount)
        if err := inst.runner.Run(ctx, "mount", "--bind", mount, dst); err != nil {
            return fmt.Errorf("failed to bind-mount %s for chroot: %w", mount, err)
        }
        defer inst.runner.Run(ctx, "umount", "-l", dst)
    }

    // Regenerate initrd for all installed kernels
    if err := inst.runner.Run(ctx, "chroot", chrootTarget,
        "dracut", "--force", "--regenerate-all"); err != nil {
        return fmt.Errorf("failed to regenerate initrd on target: %w", err)
    }

    // Copy the regenerated initrd to the target ESP (MicroOS stores kernel+initrd
    // in both /boot and the ESP; dracut updates /boot, we sync to ESP)
    if err := inst.syncKernelToESP(ctx, chrootTarget, targetESPMount); err != nil {
        inst.logger.Warn("failed to sync regenerated initrd to ESP — "+
            "target may boot with stale initrd if ESP copy is used", "error", err)
    }

    return nil
}

// updateFstab rewrites the target's fstab with new UUIDs.
// Only updates UUID= entries for the root and ESP partitions.
// /piccolo-data is NOT added to fstab — it is managed by piccolod (Posture RFC §5.2).
func (inst *Installer) updateFstab(ctx context.Context, fstabPath, rootUUID, espUUID string) error {
    content, err := os.ReadFile(fstabPath)
    if err != nil {
        return err
    }

    // Replace root UUID and ESP UUID in existing entries
    lines := strings.Split(string(content), "\n")
    var result []string
    for _, line := range lines {
        updated := inst.replaceFstabUUID(line, "/", rootUUID)
        updated = inst.replaceFstabUUID(updated, "/boot/efi", espUUID)
        result = append(result, updated)
    }

    return os.WriteFile(fstabPath, []byte(strings.Join(result, "\n")), 0644)
}
```

### Phase 5: `/piccolo-core` Subvolume Verification

Verify the `/piccolo-core` btrfs subvolume exists on the target. It was recreated as an empty subvolume in Phase 3 step 10 (since `btrfs send` does not transfer nested subvolumes). For fresh start, the empty subvolume is sufficient — it will be populated by piccolod on first boot.

```go
func (inst *Installer) verifyPiccoloCore(ctx context.Context) error {
    targetRootMount := "/run/piccolo/install/target-root"
    corePath := filepath.Join(targetRootMount, "@/piccolo-core")

    // Verify subvolume exists (created in Phase 3 step 10)
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "show", corePath); err != nil {
        return fmt.Errorf("/piccolo-core subvolume not found on target: %w", err)
    }
    return nil
}
```

### Phase 6: UEFI Boot Entry

Register a UEFI boot entry so the system boots from the internal disk after reboot.

```go
func (inst *Installer) registerUEFIBootEntry(ctx context.Context, target string) error {
    espDev := partitionDevicePath(target, 1)

    // Determine the disk and ESP partition number for efibootmgr
    disk, partNum := splitDeviceAndPartition(espDev)

    // Create boot entry pointing to the shim on the target ESP
    if err := inst.runner.Run(ctx, "efibootmgr",
        "--create",
        "--disk", disk,
        "--part", fmt.Sprintf("%d", partNum),
        "--label", "Piccolo OS",
        "--loader", `\EFI\BOOT\BOOTX64.EFI`,
    ); err != nil {
        return fmt.Errorf("efibootmgr failed: %w", err)
    }

    // Set the new entry as the first in the boot order
    // efibootmgr --create already adds it, but ensure it's first
    if err := inst.setBootOrderFirst(ctx, "Piccolo OS"); err != nil {
        inst.logger.Warn("failed to set boot order — user may need to select boot device manually", "error", err)
    }

    return nil
}
```

### Phase 7: Reboot Prompt

The installation is complete. The portal prompts the user to reboot. Reboot is NOT automatic — the user must confirm.

After reboot:
- The system boots from the internal disk.
- The portal shows first-run setup within ≤ 60 seconds (PRD requirement).
- The USB drive can be removed.

## 6. Installation Pipeline (Carry Over State)

Carry-over mode transfers the live USB session's state (admin account, apps, configuration, crypto material) to the internal disk. After reboot, the portal resumes with the migrated state.

**Prerequisite:** The user must have completed first-run setup on the USB ("Try Piccolo" → create admin → optional app deployment).

### 6.0 Admin Password and Mnemonic Key Availability

Carry-over mode requires the admin password (for LUKS keyslot 1 enrollment) and the mnemonic-derived key (for keyslot 2 enrollment). Neither is stored in plaintext — the admin password is used once during setup to derive the KEK, then discarded; the mnemonic words are shown once and never persisted.

**Admin password:** The `POST /api/v1/system/install/start` endpoint requires the admin password in the request body for carry-over mode:

```json
{
  "target_disk": "...",
  "mode": "carry_over",
  "confirm_disk_id": "...",
  "admin_password": "..."
}
```

The password is validated by attempting a control-plane unlock verification (if not already unlocked) or by verifying it derives the correct KEK. This ensures the password is correct before starting irreversible disk operations. The password is held in memory only for the duration of the installation pipeline and zeroed after LUKS keyslot enrollment.

**Mnemonic-derived key:** `crypt.Manager` retains the mnemonic-derived key in memory after initial setup (it is needed for `OnRecoveryMnemonicRotated` callbacks per Posture RFC §6.5). The installer accesses it via `inst.crypto.WithMnemonicKey()`. If the mnemonic key is not available in memory (e.g., daemon restarted after setup), keyslot 2 enrollment is skipped with a warning — the user can re-enroll it later via the recovery mnemonic rotation flow. Keyslot 0 (pool keyfile) and keyslot 1 (admin password) provide sufficient unlock paths.

### 6.0a Service Quiescence Before Sync

Before starting the carry-over pipeline, the installer quiesces running services to ensure consistent state:

1. **Stop running app containers** via `internal/app.Manager.StopAll()`. This ensures container images and runtime state in `/piccolo-data/node/` are not being actively written during sync.
2. **Flush the control-plane store** to ensure all pending writes are committed to the gocryptfs ciphertext. The PCV publisher's two-step `syncfs` flush (Foundation RFC §8.2) is called: first on the plaintext mount, then on the ciphertext subvolume.
3. **Note:** The HTTP server remains running during installation (the progress endpoint must be accessible). Only app workloads are stopped.

### Phases 1–4: Same as Fresh Start

Target disk preparation (§5 Phase 1), ESP setup (§5 Phase 2), root filesystem sync (§5 Phase 3), and post-sync fixups (§5 Phase 4) are identical to the fresh start pipeline.

### Phase 5: Copy `/piccolo-core` State

Copy the USB's `/piccolo-core` contents to the target. This includes all control-plane material.

```go
func (inst *Installer) copyPiccoloCoreState(ctx context.Context) error {
    targetRootMount := "/run/piccolo/install/target-root"
    targetCorePath := filepath.Join(targetRootMount, "@/piccolo-core")

    // Copy specific files and directories from USB's /piccolo-core to target.
    // We selectively copy rather than bulk-copying to avoid carrying over
    // USB-specific artifacts (KDF params, LUKS header backups) that would
    // pollute the target's /piccolo-core/crypto/ with stale entries.

    srcCore := paths.CoreRoot()

    // 1. crypto/ — selective copy (keyset.json, pool key only)
    // USB-specific files (luks-kdf-params/, luks-header-backups/) are excluded;
    // target-specific versions are created in Phase 10.
    cryptoDst := filepath.Join(targetCorePath, "crypto")
    if err := os.MkdirAll(cryptoDst, 0700); err != nil {
        return err
    }
    cryptoFiles := []string{"keyset.json", "piccolo_data_pool_key.enc"}
    for _, f := range cryptoFiles {
        src := filepath.Join(srcCore, "crypto", f)
        if _, err := os.Stat(src); os.IsNotExist(err) {
            continue
        }
        if err := inst.runner.Run(ctx, "cp", "-a", src, filepath.Join(cryptoDst, f)); err != nil {
            return fmt.Errorf("failed to copy crypto/%s: %w", f, err)
        }
    }

    // 2. ciphertext/ — special handling (must be btrfs subvolume)
    src := filepath.Join(srcCore, "ciphertext")
    if _, err := os.Stat(src); err == nil {
        dst := filepath.Join(targetCorePath, "ciphertext")
        if err := inst.copyCiphertextSubvolume(ctx, src, dst); err != nil {
            return fmt.Errorf("failed to copy ciphertext: %w", err)
        }
    }

    // 3. Remaining directories — bulk copy
    bulkDirs := []string{
        "volumes",          // control-plane volume metadata
        "recovery",         // PCV exports
        "network-bootstrap", // onboarding state, remote config
    }
    for _, dir := range bulkDirs {
        src := filepath.Join(srcCore, dir)
        dst := filepath.Join(targetCorePath, dir)
        if _, err := os.Stat(src); os.IsNotExist(err) {
            continue
        }
        if err := inst.runner.Run(ctx, "cp", "-a", src, dst); err != nil {
            return fmt.Errorf("failed to copy %s: %w", dir, err)
        }
    }

    // Note: mounts/ directory is intentionally omitted — piccolod recreates
    // mountpoint structure on boot (Foundation RFC §5.1 mountpoint rule).

    return nil
}

// copyCiphertextSubvolume handles the special case of ciphertext/control-plane/
// which must be a btrfs subvolume on the target (required for PCV snapshots).
func (inst *Installer) copyCiphertextSubvolume(ctx context.Context, src, dst string) error {
    if err := os.MkdirAll(dst, 0700); err != nil {
        return err
    }

    srcCP := filepath.Join(src, "control-plane")
    dstCP := filepath.Join(dst, "control-plane")

    // Create as btrfs subvolume (Foundation RFC §8.2 requirement)
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "create", dstCP); err != nil {
        return fmt.Errorf("failed to create ciphertext subvolume: %w", err)
    }

    // Copy contents into the subvolume
    if err := inst.runner.Run(ctx, "cp", "-a", srcCP+"/.", dstCP+"/"); err != nil {
        return fmt.Errorf("failed to copy ciphertext contents: %w", err)
    }

    return nil
}
```

**What is copied:**
- `crypto/keyset.json` — SDEK sealed with KEK (same admin password works on target)
- `crypto/piccolo_data_pool_key.enc` — overwritten in Phase 10 with the target's new pool keyfile. **Partial failure note:** If the pipeline fails between Phase 5 and Phase 10 (e.g., LUKS init fails in Phase 7), the target's `/piccolo-core/crypto/piccolo_data_pool_key.enc` contains the USB's pool keyfile, which cannot unlock the target's LUKS device. Recovery: the error handler offers "wipe and restart" (§11) which clears the target's `/piccolo-core` and retries from scratch
- `ciphertext/control-plane/` — full gocryptfs ciphertext (as btrfs subvolume on target)
- `volumes/control-plane/piccolo.volume.json` — wrapped gocryptfs passphrase
- `recovery/` — PCV exports (will be re-published post-install)
- `network-bootstrap/` — onboarding state, remote config, TLS material

**What is NOT copied (USB-specific, excluded intentionally):**
- `crypto/luks-kdf-params/` — USB device KDF params; target gets new ones in Phase 10
- `crypto/luks-header-backups/` — USB LUKS header backups; target gets its own in Phase 10
- `mounts/` — recreated by `piccolod` on boot

### Phase 6: Generate New Pool Keyfile for Target

The target disk gets a **new** LUKS pool keyfile. The USB's pool keyfile is specific to the USB's LUKS device and must not be reused — the target is a different physical device with a different LUKS header.

```go
func (inst *Installer) generateTargetPoolKeyfile(ctx context.Context) ([]byte, error) {
    // Generate a fresh 64-byte random keyfile (Posture RFC §6.2.1)
    keyfile, err := GeneratePoolKeyfile()
    if err != nil {
        return nil, fmt.Errorf("failed to generate target pool keyfile: %w", err)
    }

    // The new keyfile will be stored on the target's /piccolo-core after
    // the control plane is operational. For now, hold in memory.
    return keyfile, nil
}
```

### Phase 7: Initialize LUKS2 on Target `/piccolo-data`

Create the LUKS2 volume on the target's data partition with three keyslots, matching Posture RFC §6.3.

```go
func (inst *Installer) initializeTargetLUKS(ctx context.Context, target string, poolKeyfile []byte, adminPassword string) error {
    dataDev := partitionDevicePath(target, 3)

    // Ensure ephemeral secrets directory
    if err := os.MkdirAll("/run/piccolo", 0700); err != nil {
        return err
    }

    // Write keyfile to tmpfs
    keyfilePath := "/run/piccolo/target_pool_key"
    if err := os.WriteFile(keyfilePath, poolKeyfile, 0600); err != nil {
        return err
    }
    defer os.Remove(keyfilePath)
    // NOTE: Do NOT secureZero(poolKeyfile) here — the same slice is reused
    // by Phase 8 (setupTargetDataFilesystem). The caller zeroes it after
    // all phases complete.

    // LUKS format with keyfile (keyslot 0)
    // Parameters match Posture RFC §6.3
    if err := inst.runner.Run(ctx, "cryptsetup", "luksFormat",
        "--type", "luks2",
        "--batch-mode",
        "--cipher", "aes-xts-plain64",
        "--key-size", "512",
        "--hash", "sha512",
        "--pbkdf", "pbkdf2",
        "--pbkdf-force-iterations", "1000",
        "--label", "piccolo-data",
        "--key-slot", "0",
        "--key-file", keyfilePath,
        dataDev); err != nil {
        return fmt.Errorf("LUKS format failed: %w", err)
    }

    // Read device UUID and generate KDF params.
    // IMPORTANT: kdfParams is generated ONCE here and passed to Phase 10
    // (updatePoolKeyfileInControlPlane) for persistence. Phase 10 must NOT
    // regenerate params — the persisted params must match the actual keyslots.
    deviceUUID, err := inst.getLUKSUUID(ctx, dataDev)
    if err != nil {
        return fmt.Errorf("failed to read LUKS UUID: %w", err)
    }
    kdfParams, err := NewLUKSKDFParams(deviceUUID)
    if err != nil {
        return err
    }
    // Store kdfParams on the installer for Phase 10 to consume
    inst.targetKDFParams = kdfParams
    inst.targetDeviceUUID = deviceUUID

    // Add admin password recovery keyslot (keyslot 1)
    adminPass := DeriveRecoveryPassphrase(adminPassword, kdfParams)
    if err := inst.addKeyslot(ctx, dataDev, keyfilePath, adminPass, 1); err != nil {
        inst.logger.Warn("failed to add admin recovery keyslot on target", "error", err)
    }

    // Add mnemonic recovery keyslot (keyslot 2)
    // Use callback-based access to mnemonic key material (§6.0).
    // If the mnemonic key is not available in memory (daemon restarted after setup),
    // this is skipped — keyslot 0 and 1 provide sufficient unlock paths.
    var mnemonicErr error
    if err := inst.crypto.WithMnemonicKey(func(mnemonicKey []byte) error {
        mnemonicPass := DeriveMnemonicRecoveryPassphrase(mnemonicKey, kdfParams)
        mnemonicErr = inst.addKeyslot(ctx, dataDev, keyfilePath, mnemonicPass, 2)
        return nil
    }); err != nil || mnemonicErr != nil {
        inst.logger.Warn("failed to add mnemonic recovery keyslot on target",
            "callback_error", err, "keyslot_error", mnemonicErr)
    }

    return nil
}
```

### Phase 8: Create Btrfs and Directory Layout on Target Data

```go
func (inst *Installer) setupTargetDataFilesystem(ctx context.Context, target string, poolKeyfile []byte) error {
    dataDev := partitionDevicePath(target, 3)
    mapperName := "piccolo_install_data"
    mapperPath := "/dev/mapper/" + mapperName

    // Open LUKS device
    keyfilePath := "/run/piccolo/target_pool_key"
    if err := os.WriteFile(keyfilePath, poolKeyfile, 0600); err != nil {
        return err
    }
    defer os.Remove(keyfilePath)

    if err := inst.runner.Run(ctx, "cryptsetup", "open",
        "--type", "luks2",
        "--key-file", keyfilePath,
        dataDev, mapperName); err != nil {
        return fmt.Errorf("failed to open target LUKS: %w", err)
    }
    defer inst.runner.Run(ctx, "cryptsetup", "close", mapperName)

    // Create btrfs
    if err := inst.runner.Run(ctx, "mkfs.btrfs", "-f", "-L", "piccolo-data", mapperPath); err != nil {
        return fmt.Errorf("mkfs.btrfs on target data failed: %w", err)
    }

    // Mount and create directory layout (Foundation RFC §5.2)
    targetDataMount := "/run/piccolo/install/target-data"
    if err := os.MkdirAll(targetDataMount, 0700); err != nil {
        return err
    }
    if err := inst.runner.Run(ctx, "mount", mapperPath, targetDataMount); err != nil {
        return fmt.Errorf("failed to mount target data: %w", err)
    }
    defer inst.runner.Run(ctx, "umount", targetDataMount)

    // Create directory structure per Foundation RFC §5.2
    dirs := []string{
        "node",
        "user/volumes",
        "federation",
        "system-objects/control-plane-backups",
        "system-objects/volume-checkpoints",
    }
    for _, dir := range dirs {
        if err := os.MkdirAll(filepath.Join(targetDataMount, dir), 0700); err != nil {
            return fmt.Errorf("failed to create directory %s: %w", dir, err)
        }
    }

    // Set NOCOW on churn-heavy directories (Posture RFC §8)
    nocowDirs := []string{"node", "federation"}
    for _, dir := range nocowDirs {
        path := filepath.Join(targetDataMount, dir)
        if err := inst.runner.Run(ctx, "chattr", "+C", path); err != nil {
            inst.logger.Warn("failed to set NOCOW", "path", dir, "error", err)
        }
    }

    return nil
}
```

### Phase 9: Sync User Data Volumes

If the USB's `/piccolo-data` contains user data (app volumes, container images), sync them to the target.

```go
func (inst *Installer) syncUserData(ctx context.Context) error {
    sourceDataMount := paths.DataRoot() // USB's /piccolo-data (already mounted)
    targetDataMount := "/run/piccolo/install/target-data"

    // Copy user data directories
    // Only copy if they exist and contain data
    dataDirs := []string{
        "node",                                // Container images, runtime state
        "user",                                // App volumes
        "system-objects/control-plane-backups", // PCV backups
    }

    for _, dir := range dataDirs {
        src := filepath.Join(sourceDataMount, dir)
        if _, err := os.Stat(src); os.IsNotExist(err) {
            continue
        }
        dst := filepath.Join(targetDataMount, dir)
        // Use cp -a for regular directories
        if err := inst.runner.Run(ctx, "cp", "-a", src+"/.", dst+"/"); err != nil {
            inst.logger.Warn("failed to sync data directory", "dir", dir, "error", err)
            // Non-fatal: apps can be re-pulled
        }
    }

    return nil
}
```

### Phase 10: Update Pool Keyfile in Control Plane

Store the new target pool keyfile in the copied control plane (encrypted with the existing SDEK).

```go
func (inst *Installer) updatePoolKeyfileInControlPlane(ctx context.Context, poolKeyfile []byte) error {
    targetRootMount := "/run/piccolo/install/target-root"
    targetCorePath := filepath.Join(targetRootMount, "@/piccolo-core")

    // Store the new pool keyfile encrypted with the existing SDEK.
    // The SDEK is already available (we're carrying over the control plane).
    // Write the encrypted keyfile to the target's /piccolo-core/crypto/
    if err := inst.crypto.StorePoolKeyfileAt(ctx, poolKeyfile,
        filepath.Join(targetCorePath, "crypto", "piccolo_data_pool_key.enc")); err != nil {
        return fmt.Errorf("failed to store target pool keyfile: %w", err)
    }

    // Write KDF params for the target LUKS device.
    // CRITICAL: Use the SAME kdfParams that were generated in Phase 7
    // (initializeTargetLUKS) and used for actual keyslot enrollment.
    // DO NOT regenerate — new random salts would not match the enrolled keyslots,
    // silently breaking admin password and mnemonic recovery.
    kdfParams := inst.targetKDFParams
    deviceUUID := inst.targetDeviceUUID
    if kdfParams == nil {
        return fmt.Errorf("target KDF params not set — Phase 7 must run before Phase 10")
    }

    kdfPath := filepath.Join(targetCorePath, "crypto", "luks-kdf-params", deviceUUID+".json")
    if err := os.MkdirAll(filepath.Dir(kdfPath), 0700); err != nil {
        return err
    }
    if err := writeJSON(kdfPath, kdfParams); err != nil {
        return fmt.Errorf("failed to write target KDF params: %w", err)
    }

    // Backup LUKS header for the target device
    dataDev := partitionDevicePath(inst.targetDisk, 3)
    headerBackupPath := filepath.Join(targetCorePath, "crypto", "luks-header-backups", deviceUUID+".bin")
    if err := os.MkdirAll(filepath.Dir(headerBackupPath), 0700); err != nil {
        return err
    }
    if err := inst.runner.Run(ctx, "cryptsetup", "luksHeaderBackup",
        dataDev, "--header-backup-file", headerBackupPath); err != nil {
        inst.logger.Warn("failed to backup target LUKS header", "error", err)
    }

    return nil
}
```

### Phase 11: UEFI Boot Entry + Reboot Prompt

Same as fresh start Phase 6 (§5 Phase 6) and Phase 7 (§5 Phase 7).

After reboot with carry-over:
- The system boots from the internal disk.
- The portal resumes with the migrated admin, apps, and configuration.
- The admin password unlocks the carried-over control plane.
- `/piccolo-data` unlocks with the new pool keyfile (which was stored encrypted in the carried-over control plane).

**Onboarding state on target:** The carried-over `network-bootstrap/onboarding.json` has `state: "complete"` (from the USB's "Try Piccolo" setup). On the target's first boot, `DetectBootMode()` returns `BootModeInternal`, so the onboarding flow is skipped entirely (Posture RFC §4.2) — the system proceeds directly to the unlock flow. The "Install to Disk" option is hidden in Settings since the system is no longer USB-booted.

**Fresh start onboarding state:** The synced filesystem includes the USB's `onboarding.json` (state: "complete" or "install_disk"). However, the target's `onboarding.json` is irrelevant because `DetectBootMode()` returns `BootModeInternal` — internal boot skips onboarding regardless of the state file. The target proceeds to first-run setup (create admin) via the normal crypto/setup flow.

## 7. Target Disk Partition Layout

The target disk layout **must** match Posture RFC §2.2 exactly:

```
Target Disk (e.g., 256GB):
├── Partition 1: ESP (~512MB)
│   ├── Type: EF00 (EFI System)
│   ├── Filesystem: FAT32
│   └── Contents: UEFI bootloader chain
├── Partition 2: Root btrfs (~20GB)
│   ├── Type: 8300 (Linux filesystem)
│   ├── Filesystem: btrfs (label: piccolo-root)
│   ├── @/.snapshots/         (MicroOS transactional updates)
│   └── @/piccolo-core/       (btrfs subvolume)
└── Partition 3: /piccolo-data (remaining space)
    ├── Type: 8309 (Linux LUKS)
    ├── Encryption: LUKS2 (AES-XTS-plain64, 512-bit)
    ├── Filesystem: btrfs (label: piccolo-data)
    └── Directory layout per Foundation RFC §5.2
```

Partition sizing uses `calculatePartitionLayout()` from Posture RFC §5.4. On normal disks (≥26GB), root gets 20GB and data gets the remainder. On small disks, the 70/30 proportional split applies.

## 8. Bootloader Installation

### 8.1 ESP Contents

The target ESP is a complete copy of the USB ESP. This ensures the same kernel, initrd, and bootloader binaries are available.

### 8.2 Secure Boot Chain

Piccolo OS uses the standard SUSE Secure Boot chain:
1. UEFI firmware verifies the shim (`BOOTX64.EFI`) against the Microsoft UEFI CA.
2. The shim verifies GRUB (`grubx64.efi`) against the SUSE signing key.
3. GRUB verifies the kernel against the SUSE signing key.

This chain is preserved during Install to Disk because the same signed binaries are copied from USB to the target ESP. No custom key enrollment is required.

### 8.3 efibootmgr Invocation

```bash
efibootmgr --create \
    --disk /dev/sda \
    --part 1 \
    --label "Piccolo OS" \
    --loader '\EFI\BOOT\BOOTX64.EFI'
```

The `--create` flag adds a new boot entry and appends it to the boot order. The installer then reorders so the new entry is first, ensuring the internal disk boots by default.

**Fallback:** If `efibootmgr` fails (some firmware implementations are buggy), the user is instructed to select the boot device manually via their BIOS/UEFI setup. The `BOOTX64.EFI` path is the UEFI removable media fallback, so many systems will find it automatically.

## 9. LUKS2 Setup on Target

### 9.1 Fresh Start

No LUKS setup during installation. The target disk's partition 3 is left as raw (no filesystem, no LUKS header). After reboot, `piccolod` runs its normal first-boot flow:
1. Phase 1 detects the data partition exists but has no LUKS header.
2. Phase 2 (after admin password setup) runs `InitializeLUKS()` per Posture RFC §6.3.

This avoids duplicating LUKS initialization logic in the installer.

**Posture RFC state machine trace (fresh-start first boot):**

The target's first boot proceeds through the Posture RFC's state machine as follows:

| Step | Posture RFC Function | State Input | Result |
|------|---------------------|-------------|--------|
| 1 | `DetectBootMode()` | Boot disk transport = `sata`/`nvme` | `BootModeInternal` — skip onboarding |
| 2 | `GetPartitionState()` | Partition 3 exists, type `8309`, label `piccolo-data` | `{DataPartitionExists: true, DataPartitionLUKS: false}` |
| 3 | `ExpandRootPartition()` | Root already at target size (installer-created) | No-op |
| 4 | `CreateDataPartition()` | Partition 3 already exists | No-op — `findNextPartitionSlot()` sees existing partition |
| 5 | Phase 1 complete | No writes performed | Channel signaled for Phase 2 |
| 6 | `detectOrphanedLUKSHeader()` | No LUKS header, no pool keyfile | Returns `false` — not an orphan |
| 7 | Phase 2 (after admin password) | `!state.DataPartitionLUKS` | Calls `InitializeLUKS()` per §6.3 |

The `{DataPartitionExists: true, DataPartitionLUKS: false}` state is explicitly handled by the Posture RFC Phase 2 logic: a data partition without a LUKS header triggers `InitializeLUKS()` (which is the normal first-boot LUKS setup path). The `findDataPartitionDevice()` function (Posture RFC §6.3a) scans the boot disk for GPT type code `8309` + label `piccolo-data`, which matches the installer-created partition 3.

### 9.2 Carry Over

The installer creates the LUKS2 volume during installation (Phases 7–8 in §6) because:
- The admin password is already known (user completed setup on USB).
- The carried-over control plane expects a functional `/piccolo-data` on first boot.
- The pool keyfile stored in the control plane must match the target LUKS device.

**Three-keyslot enrollment on target:**

| Keyslot | Key Source | Enrolled During |
|---------|-----------|----------------|
| 0 | New pool keyfile (generated in Phase 6) | Phase 7 |
| 1 | Argon2id(admin password, new target KDF params) | Phase 7 |
| 2 | Argon2id(mnemonic-derived key, new target KDF params) | Phase 7 |

**KDF params:** New random salts and params are generated for the target device (Posture RFC §6.4.1). The USB's KDF params are not reused — they are bound to the USB's LUKS device UUID.

## 10. Progress Reporting and Cancellation

### 10.1 Progress Model

The installation pipeline reports progress as a series of phase completions.

```go
type InstallProgress struct {
    State       InstallState `json:"state"`       // "preparing", "running", "completed", "failed", "cancelled"
    Phase       int          `json:"phase"`        // Current phase number (1-based)
    TotalPhases int          `json:"total_phases"` // Total phases in the pipeline
    PhaseName   string       `json:"phase_name"`   // e.g., "Syncing root filesystem"
    Percent     int          `json:"percent"`       // 0-100 (within current phase)
    BytesSent   int64        `json:"bytes_sent"`    // For btrfs send/receive phases
    BytesTotal  int64        `json:"bytes_total"`   // Estimated total bytes
    Error       string       `json:"error,omitempty"`
    StartedAt   time.Time    `json:"started_at"`
    UpdatedAt   time.Time    `json:"updated_at"`
}

type InstallState string

const (
    InstallStatePreparing InstallState = "preparing"
    InstallStateRunning   InstallState = "running"
    InstallStateCompleted InstallState = "completed"
    InstallStateFailed    InstallState = "failed"
    InstallStateCancelled InstallState = "cancelled"
)
```

### 10.2 Progress Delivery

Progress is delivered via **polling** (`GET /api/v1/system/install/progress`). SSE is deferred to a future enhancement.

Phase milestones for fresh start:
1. Preparing target disk
2. Setting up ESP
3. Syncing root filesystem (btrfs send/receive)
4. Applying configuration fixups
5. Verifying /piccolo-core
6. Registering UEFI boot entry
7. Ready to reboot

Carry-over milestones (branches after milestone 4):
5. Copying control plane state
6. Generating target encryption keys
7. Initializing target encryption
8. Creating data filesystem
9. Syncing user data
10. Updating crypto material
11. Registering UEFI boot entry
12. Ready to reboot

### 10.3 Cancellation

```go
type InstallCancellation struct {
    // Safe cancellation: before Phase 3 write begins (no data on target yet)
    Safe bool
    // Best-effort cancellation: after Phase 3 begins (target may have partial data)
    BestEffort bool
}
```

**Cancellation semantics:**
- **Before root sync (Phase 3) begins:** Safe cancellation. The target disk has only been wiped and partitioned — no meaningful data. Cancellation leaves the target in a wiped state. The USB boot environment is unaffected.
- **After root sync begins:** Best-effort cancellation. The in-progress `btrfs send/receive` is terminated (SIGTERM → SIGKILL if needed). The target disk is left in an indeterminate state (partial btrfs). A subsequent install attempt will detect this and offer to wipe-and-restart (see §11).
- **After the final phase (reboot prompt):** Cancellation is meaningless — the install is complete. This is Phase 7 for fresh start (§5) or Phase 11 for carry-over (§6).

### 10.4 Timeouts

All subprocess invocations respect the pipeline's `context.Context` for cancellation.

| Scope | Timeout | Rationale |
|---|---|---|
| Global pipeline | 60 minutes | Upper bound for the entire installation. Must exceed worst-case sum of sequential phases (root sync 20m + data sync 15m + LUKS init 5m + overhead). |
| Disk preparation (Phase 1) | 2 minutes | `sgdisk` and `partprobe` should complete in seconds. |
| ESP setup (Phase 2) | 2 minutes | Small filesystem format + copy. |
| Root sync (Phase 3) | 20 minutes | `btrfs send/receive` for a full USB image (2-10GB used data). |
| Post-sync fixups (Phase 4) | 1 minute | Text file updates. |
| LUKS initialization (Phase 7) | 5 minutes | Argon2id KDF for keyslots 1 and 2 is the bottleneck. |
| Data sync (Phase 9) | 15 minutes | Depends on user data volume; container images can be large. |

Per-phase timeouts are implemented as `context.WithTimeout` wrappers. If a phase exceeds its timeout, the pipeline transitions to the `failed` state with a descriptive error. The global timeout is a hard upper bound that overrides per-phase timeouts.

**USB disconnect during installation:** If the USB is physically disconnected, all reads from the USB will fail with I/O errors. The affected subprocess (e.g., `btrfs send`) will terminate, and the pipeline reports the failure. The target disk is left in a partial state — the user must reconnect the USB and use wipe-and-restart.

## 11. Error Handling and Recovery

### 11.1 Partial Write Detection

On retry, the installer must detect partial writes on the target from a previous failed attempt:

| Target State | Detection | Recovery Action |
|---|---|---|
| Wiped GPT, no partitions | `sgdisk -p` shows empty table | Normal install (no conflict) |
| Partitions created, no btrfs | Partitions exist but `blkid` shows no filesystem | Offer wipe-and-restart |
| Partial btrfs (incomplete send) | btrfs mount fails or shows corruption | Offer wipe-and-restart |
| Complete btrfs, no LUKS on data | Root looks valid, data partition raw | Offer wipe-and-restart or resume |
| Complete install (all valid) | Root mounts, LUKS header on data, boot entry exists | Warn: "Target appears to have a complete installation" |

### 11.2 USB Boot Environment Safety

**Invariant:** Install to Disk NEVER modifies the USB boot environment. All writes target the internal disk exclusively.

If the install fails:
- The user can reboot from USB and retry.
- The USB session continues to function normally.
- "Try Piccolo" state on the USB is preserved.

### 11.3 Pipeline Error Strategy

Each phase is a discrete unit. On error:
1. Log the error with full context (phase, device, command output).
2. Attempt cleanup of the current phase's resources (unmount, close mapper).
3. Report the error to the user via the progress endpoint.
4. Do not attempt to resume from a mid-phase failure — offer wipe-and-restart.

## 12. Pre-Existing Data on Target

### 12.1 Detection

Before installation, the portal displays all existing partitions on the target disk (§3.1 `ExistingPartition`). This includes:
- Partition type codes and labels
- Filesystem types and labels (ext4, NTFS, btrfs, etc.)
- LUKS headers
- Estimated data usage (if the filesystem can be queried)

### 12.2 Erase Warning

The portal shows a clear erase warning:

> **All data on [disk model] ([size]) will be permanently erased.**
>
> Existing partitions:
> - Partition 1: Windows Recovery (NTFS, 500 MB)
> - Partition 2: EFI System (FAT32, 100 MB)
> - Partition 3: Microsoft basic data (NTFS, 237 GB)
>
> Type the disk identifier to confirm: `/dev/disk/by-id/ata-Samsung_SSD_850_...`

### 12.3 Previous Piccolo Installation

If the target disk has an existing Piccolo installation (btrfs with `piccolo-root` label, LUKS with `piccolo-data` label), the portal warns:

> This disk appears to contain an existing Piccolo installation.
> Installing will replace it completely. State from the existing installation will NOT be carried over — use PCV export/import for that purpose.

## 13. API Endpoints

### 13.1 List Target Disks

```
GET /api/v1/system/install/targets
```

**Response:**
```json
{
  "targets": [
    {
      "dev_path": "/dev/sda",
      "by_id_path": "/dev/disk/by-id/ata-Samsung_SSD_850_EVO_250GB_S21PNXAG...",
      "model": "Samsung SSD 850 EVO 250GB",
      "size_bytes": 250059350016,
      "size_human": "250 GB",
      "transport": "sata",
      "partitions": [...],
      "has_data": true
    }
  ],
  "boot_disk": "/dev/sdb"
}
```

**Availability:** Only when `BootMode == USB || BootMode == Unknown`. Returns `404 Not Found` when booted from internal disk.

### 13.2 Simulate Installation

```
POST /api/v1/system/install/simulate
```

**Request:**
```json
{
  "target_disk": "/dev/disk/by-id/ata-Samsung_SSD_850_EVO_250GB_S21PNXAG...",
  "mode": "fresh"
}
```

**Response:** `InstallPlan` (§4).

### 13.3 Start Installation

```
POST /api/v1/system/install/start
```

**Request:**
```json
{
  "target_disk": "/dev/disk/by-id/ata-Samsung_SSD_850_EVO_250GB_S21PNXAG...",
  "mode": "carry_over",
  "confirm_disk_id": "ata-Samsung_SSD_850_EVO_250GB_S21PNXAG...",
  "admin_password": "..."
}
```

The `confirm_disk_id` must match the `by_id_path` (stripped of the `/dev/disk/by-id/` prefix). This is the "type the exact disk identifier" confirmation from the PRD.

The `admin_password` field is **required for carry-over mode** (used for LUKS keyslot 1 enrollment on the target disk — see §6.0) and **ignored for fresh mode**. The password is validated before starting the pipeline (see preconditions below). It is held in memory only for the duration of the installation and zeroed after use.

**Response:**
```json
{
  "install_id": "inst-abc123",
  "status": "running"
}
```

**Preconditions checked:**
- Boot mode is USB/Unknown
- Target disk exists and is not the boot disk
- `confirm_disk_id` matches the target
- No other installation is in progress
- Target disk meets minimum size requirements

For carry-over mode additionally:
- Control plane is unlocked (SDEK available for pool keyfile encryption)
- `/piccolo-data` is mounted (user data accessible for sync)
- `admin_password` is present and validates against the control plane (derives correct KEK)

### 13.4 Poll Progress

```
GET /api/v1/system/install/progress
```

**Response:** `InstallProgress` (§10.1).

Returns `404` if no installation has been started.

### 13.5 Cancel Installation

```
POST /api/v1/system/install/cancel
```

**Response:**
```json
{
  "cancelled": true,
  "safe": true,
  "message": "Installation cancelled before any data was written to the target disk."
}
```

Or if after Phase 3:
```json
{
  "cancelled": true,
  "safe": false,
  "message": "Installation cancelled. The target disk may contain partial data. A fresh install will wipe it."
}
```

## 14. Device Identity

Per PRD: "Preserve/emit device identity so the installed system is recognized post-reboot."

### 14.1 `/etc/machine-id` Handling

The `machine-id` is synced from USB to the installed system as part of the `btrfs send/receive` root sync. It is **intentionally kept identical** — regenerating it would change the node ID derivation (`SHA-256(machine-id || DMI board serial)`) and create a mismatch with any carried-over `node-id.json`.

**Implication:** After Install to Disk, the USB and internal disk share the same `machine-id`. The USB **should not be booted on the same hardware** after installation. Booting both environments with the same `machine-id` on the same machine can cause systemd journal corruption and mDNS/Avahi identity conflicts. This is acceptable because:
- The Install to Disk flow sets the UEFI boot order to prefer the internal disk.
- The USB's primary purpose is evaluation ("Try Piccolo"); after installing to disk, it is no longer needed on that hardware.
- The USB can still be used on different hardware (different DMI serial produces a different node ID even with the same `machine-id`).

### 14.2 Fresh Start

The installed system generates a new node ID on first boot (Foundation RFC §8.2.1 — `SHA-256(machine-id || DMI board serial)`). Since the `machine-id` is part of the synced OS and the DMI serial comes from the hardware, the node ID is deterministic for the physical device.

### 14.3 Carry Over

The carried-over control plane contains the `node-id.json` (generated during the USB "Try Piccolo" session). This node ID was derived from the USB boot's `machine-id` and the physical hardware's DMI serial.

After install and reboot:
- The `machine-id` on the internal disk is the same (synced from USB).
- The DMI serial is the same (same physical hardware).
- Therefore, a freshly derived node ID matches the one in `node-id.json`.

If for any reason the node ID does not match (e.g., `machine-id` was regenerated), `piccolod` detects the mismatch on first boot and updates `node-id.json`. The PCV publisher emits a new PCV with the corrected `source_node_id`.

## 15. Post-Install Verification

Before presenting the reboot prompt, the installer performs verification checks:

```go
func (inst *Installer) verifyInstallation(ctx context.Context, mode InstallMode) error {
    target := inst.targetDisk

    // 1. Root filesystem integrity
    rootDev := partitionDevicePath(target, 2)
    rootMount := "/run/piccolo/install/target-root"
    if err := inst.runner.Run(ctx, "mount", "-o", "ro", rootDev, rootMount); err != nil {
        return fmt.Errorf("target root filesystem failed to mount: %w", err)
    }
    defer inst.runner.Run(ctx, "umount", rootMount)

    // 2. /piccolo-core subvolume exists
    corePath := filepath.Join(rootMount, "@/piccolo-core")
    if err := inst.runner.Run(ctx, "btrfs", "subvolume", "show", corePath); err != nil {
        return fmt.Errorf("/piccolo-core subvolume missing on target: %w", err)
    }

    // 3. UEFI boot entry registered
    entries, err := inst.listBootEntries(ctx)
    if err != nil {
        return fmt.Errorf("failed to list boot entries: %w", err)
    }
    found := false
    for _, entry := range entries {
        if strings.Contains(entry, "Piccolo OS") {
            found = true
            break
        }
    }
    if !found {
        return fmt.Errorf("UEFI boot entry 'Piccolo OS' not found")
    }

    // 4. ESP contains required boot files
    espDev := partitionDevicePath(target, 1)
    espMount := "/run/piccolo/install/verify-esp"
    if err := os.MkdirAll(espMount, 0700); err != nil {
        return err
    }
    if err := inst.runner.Run(ctx, "mount", "-o", "ro", espDev, espMount); err != nil {
        return fmt.Errorf("target ESP failed to mount: %w", err)
    }
    defer inst.runner.Run(ctx, "umount", espMount)

    requiredFiles := []string{
        "EFI/BOOT/BOOTX64.EFI",
        "EFI/opensuse/grubx64.efi",
    }
    for _, f := range requiredFiles {
        if _, err := os.Stat(filepath.Join(espMount, f)); err != nil {
            return fmt.Errorf("required boot file missing: %s", f)
        }
    }

    // 5. Carry-over specific checks
    if mode == InstallModeCarryOver {
        // Verify ciphertext/control-plane/ is a btrfs subvolume
        cpPath := filepath.Join(corePath, "ciphertext", "control-plane")
        if err := inst.runner.Run(ctx, "btrfs", "subvolume", "show", cpPath); err != nil {
            return fmt.Errorf("ciphertext/control-plane is not a btrfs subvolume: %w", err)
        }

        // Verify keyset.json exists
        keysetPath := filepath.Join(corePath, "crypto", "keyset.json")
        if _, err := os.Stat(keysetPath); err != nil {
            return fmt.Errorf("keyset.json missing on target: %w", err)
        }

        // Verify target pool keyfile exists
        poolKeyPath := filepath.Join(corePath, "crypto", "piccolo_data_pool_key.enc")
        if _, err := os.Stat(poolKeyPath); err != nil {
            return fmt.Errorf("target pool keyfile missing: %w", err)
        }

        // Verify LUKS header on data partition
        dataDev := partitionDevicePath(target, 3)
        isLuks, err := isLUKS(ctx, dataDev)
        if err != nil || !isLuks {
            return fmt.Errorf("target data partition does not have a valid LUKS header")
        }
    }

    return nil
}
```

## 16. Security Considerations

### 16.1 Erase Warning

All existing data on the target disk is permanently erased. The confirmation UX (type exact disk identifier) mitigates accidental data loss.

### 16.2 Secure Boot Chain

The Secure Boot chain is preserved by copying the signed EFI binaries. No unsigned code is introduced. The GRUB configuration update (Phase 4) modifies a non-signed text file; GRUB's Secure Boot verification applies to the kernel and initrd, not the configuration.

### 16.3 Crypto Material Handling

**Fresh start:** No crypto material is transferred. The installed system generates everything fresh during first-run setup.

**Carry over:**
- The control plane ciphertext and sealed keyset are copied to the target. The admin password remains the unlock secret — it is never written to the target in plaintext.
- A **new** pool keyfile is generated for the target disk's LUKS volume. The USB's pool keyfile is copied to the target (as part of `/piccolo-core/crypto/`) but is immediately overwritten with the new target-specific keyfile (Phase 10).
- KDF params for the target LUKS device use fresh random salts.
- Ephemeral key material (plaintext keyfiles, derived passphrases) exists only in tmpfs (`/run/piccolo/`) during installation and is cleaned up immediately.

### 16.4 No Residual Crypto on USB

After carry-over installation, the USB retains its own crypto material (this is expected — the USB continues to function as a bootable "Try Piccolo" session). The USB's crypto material does NOT grant access to the target disk's `/piccolo-data` because the target uses a new pool keyfile.

### 16.5 PCV Import Authentication

PCV import (Foundation RFC §8.4.1) MUST be rejected for requests arriving over the Nexus tunnel. This applies to the Install to Disk flow as well — installation is a local-only operation requiring physical access.

## 17. Acceptance Criteria

Mapping to `install_to_disk_x86.feature` scenarios:

| Scenario | Acceptance Criteria | Covered By |
|---|---|---|
| Landing page presents Try or Install options | Portal shows both options when USB boot + internal disk detected | §2, §3, Posture RFC §9.2 |
| Try mode allows deferred installation | "Install to Disk" available in Settings while USB boot; hidden on internal boot | §2 |
| Install to Disk with fresh start | Syncs via btrfs send/receive; reboots to installed system; portal in ≤ 60s | §5 (all phases) |
| Install to Disk with state migration | Syncs including user state; resumes with migrated admin, apps, config | §6 (all phases) |
| Dry run simulation | Shows plan without writing to disk | §4, §13.2 |

**Additional acceptance criteria (not in feature file but required by this RFC):**
- Target disk ends in production two-root posture after installation.
- USB boot environment is never modified.
- `confirm_disk_id` match is enforced before writing.
- Partial installation is detected on retry with wipe-and-restart option.
- Post-install verification passes before reboot prompt.
- Carry-over: admin password unlocks the installed system; `/piccolo-data` unlocks with the new pool keyfile on first boot.

## 18. Component Changes

### 18.1 New Package: `internal/storage/installer`

```go
package installer

type Installer struct {
    runner     CommandRunner       // Shared with diskprep/luks packages
    crypto     *crypt.Manager
    storage    *storage.Manager
    logger     *slog.Logger
    bootDisk   string             // The USB boot disk (excluded from targets)
    targetDisk string             // Selected target disk
    progress   *InstallProgress
    cancel     context.CancelFunc // For cancellation
    mu         sync.Mutex         // Protects progress and cancel

    // Carry-over state (set during pipeline execution)
    targetKDFParams  *LUKSKDFParams // Generated in Phase 7, consumed in Phase 10
    targetDeviceUUID string          // Target LUKS device UUID
}

// Discovery
func (inst *Installer) DiscoverTargets(ctx context.Context) ([]TargetDisk, error)

// Planning
func (inst *Installer) Simulate(ctx context.Context, target string, mode InstallMode) (*InstallPlan, error)

// Execution
// adminPassword is required for carry-over mode (LUKS keyslot enrollment); empty for fresh mode.
func (inst *Installer) Start(ctx context.Context, target string, mode InstallMode, confirmID, adminPassword string) error
func (inst *Installer) Progress() *InstallProgress
func (inst *Installer) Cancel() error

// Internal pipeline
func (inst *Installer) runFreshPipeline(ctx context.Context) error
func (inst *Installer) runCarryOverPipeline(ctx context.Context, adminPassword string) error
```

### 18.2 Key Helpers

**`runPiped`** — Streams stdout of one subprocess into stdin of another, used for `btrfs send | btrfs receive`:

```go
// runPiped starts two commands, piping cmd1's stdout into cmd2's stdin.
// Both commands run concurrently. Returns error if either command fails.
// Respects ctx cancellation — on cancel, both processes receive SIGTERM.
func (inst *Installer) runPiped(ctx context.Context, cmd1Args, cmd2Args []string) error
```

Implementation must: (a) create an `io.Pipe` or use `cmd1.StdoutPipe()` → `cmd2.Stdin`, (b) start both commands, (c) wait for both, (d) on error in either, kill the other via process signal, (e) collect and return the first error. For progress tracking, a `CountingReader` can be interposed on the pipe to report `BytesSent`.

**`splitDeviceAndPartition`** — Parses a partition device path into its parent disk and partition number for `efibootmgr`:

```go
// splitDeviceAndPartition("/dev/sda1")      → ("/dev/sda", 1)
// splitDeviceAndPartition("/dev/nvme0n1p1") → ("/dev/nvme0n1", 1)
func splitDeviceAndPartition(partDev string) (disk string, partNum int, err error)
```

### 18.3 Integration Points

- **`internal/storage.Manager`**: The installer reuses `calculatePartitionLayout()`, `partitionDevicePath()`, and disk sizing helpers from the storage package.
- **`internal/crypt.Manager`**: Used for pool keyfile generation/encryption and mnemonic key access in carry-over mode.
- **`internal/server/gin_server.go`**: Registers the install API endpoints (§13). Endpoints are gated on USB/Unknown boot mode.
- **`internal/events/`**: Emits audit events for install start, progress, completion, failure, and cancellation.

## 19. Required System Tools

All tools listed in Posture RFC §17, plus:

| Tool | RPM Package | Purpose |
|------|-------------|---------|
| `efibootmgr` | efibootmgr | UEFI boot entry management |
| `mkfs.vfat` | dosfstools | ESP filesystem creation |
| `blkid` | util-linux | Filesystem and UUID detection |
| `grub2-editenv` | grub2 | Reset GRUB environment variables |
| `cp` | coreutils | File/directory copying |

**RPM Spec addition:**
```spec
Requires: efibootmgr
Requires: dosfstools
```

Note: `grub2`, `coreutils`, `dracut`, and `snapper` are already present in the base MicroOS image and do not need explicit RPM dependencies. `dracut` is used in Phase 4 (`dracut --force --regenerate-all` inside chroot) and `snapper` in Phase 3 (`snapper setup-quota`).

## 20. Open Questions

1. ~~**MicroOS snapshot numbering:**~~ Resolved — see §5 Phase 3 "MicroOS/snapper compatibility" note. `grubenv` is reset; snapper resumes numbering automatically.
2. ~~**GRUB environment block:**~~ Resolved — `grubenv` `saved_entry` is cleared in Phase 4 post-sync fixups.
3. **systemd-boot vs GRUB:** If Piccolo OS migrates to systemd-boot in the future, the ESP setup and GRUB config update logic will need revision. No action needed for v1.

## Implementation Notes & Status
- 2026-02-07: Initial draft. Covers all areas deferred by Posture RFC §9.4.
- 2026-02-08: Second review pass. Blocking fixes: (10) Phase 4 now regenerates initrd via `chroot` + `dracut --force --regenerate-all` — the USB's initrd may have the USB root UUID baked in by dracut; without regeneration the target would fail to boot; (11) `/etc/machine-id` intentionally kept identical across USB and installed system — regenerating would break node ID derivation for carry-over; documented that USB should not be booted on same hardware after install; (12) added explicit Posture RFC state machine trace for fresh-start first boot — confirms `{DataPartitionExists: true, DataPartitionLUKS: false}` is handled correctly by Phase 2 `InitializeLUKS()`; (13) documented stale USB pool keyfile on carry-over partial failure (Phase 5-10 gap) — recovery is wipe-and-restart. Significant fixes: (14) target disk validation now checks for mounted partitions and open dm-crypt mappers before `sgdisk --zap-all`.
- 2026-02-07: First review pass fixes. Blocking fixes: (1) admin password and mnemonic key availability for carry-over mode specified (§6.0, §13.3); (2) KDF params double-generation bug fixed — params generated once in Phase 7, passed to Phase 10 via installer state; (3) USB-specific crypto artifacts (KDF params, header backups) excluded from Phase 5 copy; (4) fresh-start partition detection compatibility with Posture RFC documented (§9.1). Significant fixes: (5) timeout handling added (§10.4) with per-phase and global timeouts; (6) MicroOS/snapper compatibility resolved — grubenv reset, snapper resumes numbering automatically; (7) GRUB config fixup expanded to replace both root and ESP UUIDs; (8) service quiescence before carry-over sync (§6.0a); (9) onboarding state transitions documented (§6 Phase 11 note).
