# RFC: Storage Posture - Two-Root Architecture with LUKS2 Encryption

- **Status:** Draft
- **Date:** 2026-02-01
- **Authors:** Engineering Team
- **Related:**
  - `org-context/03_engineering/storage_architecture.md`
  - `docs/rfc/20260202-storage-v2-foundation.md`

## 1. Summary

This RFC describes how piccolod will adopt the new two-root storage architecture. The OS image is now **minimal** - piccolod is responsible for all disk partitioning, filesystem creation, and encryption setup.

Piccolod must:
1. **Detect boot mode** (internal disk vs USB boot)
2. **Expand root partition** to ~20GB
3. **Verify `/piccolo-core`** btrfs subvolume exists (created by OS)
4. **Create `/piccolo-data` partition** from remaining disk space
5. **LUKS2 encrypt** `/piccolo-data` with pool keyfile
6. **Create btrfs** on `/piccolo-data`
7. **Handle USB boot** with onboarding flow (Install to Disk / Try Piccolo)

### 1.1 Relationship to `20260202-storage-v2-foundation.md`
This RFC specifies the **physical disk posture** and first-boot disk preparation required to make the two-root model real on target OS images:
- partition and filesystem sizing,
- creation and mounting of `/piccolo-data` as a LUKS2 + btrfs pool,
- NOCOW posture on churn-heavy directories.

`docs/rfc/20260202-storage-v2-foundation.md` specifies the **daemon-level storage contracts** that consume this posture:
- `PICCOLO_CORE_ROOT`/`PICCOLO_DATA_ROOT` env vars (defaults `/piccolo-core`/`/piccolo-data`),
- logical directory layout and mountpoint contracts,
- control-plane persistence placement and PCV export publishing/replication.

## 2. Context

### 2.1 Disk Layout After dd (Minimal Image)

The piccolo-os image is now minimal (~2-3GB) to allow short dd images:

```
Disk (any size):
├── ESP (~512MB)
└── Root btrfs (minimal, ~2GB used)
    └── OS snapshots (MicroOS)
```

**KIWI settings:**
- No `spare_part` - piccolod creates `/piccolo-data` partition
- `<volume name="piccolo-core" />` - OS creates the btrfs subvolume
- No fixed `size` - all profiles produce minimal images (~2-3GB)
- `oem-resize="false"` - piccolod handles all expansion

### 2.2 Disk Layout After Piccolod Disk Prep

```
Disk (e.g., 256GB):
├── ESP (~512MB)
├── Root btrfs (~20GB)
│   ├── @/.snapshots/         (MicroOS transactional updates)
│   └── @/piccolo-core/       (btrfs subvolume, created by KIWI)
└── /piccolo-data             (remaining space, LUKS2 + btrfs, created by piccolod)
```

### 2.3 Design Rationale

Per the storage architecture document:

- **`/piccolo-core`** on root partition (internal) - ensures boot reliability
- **`/piccolo-data`** as separate partition - can be expanded with USB devices
- **LUKS2 encryption** for `/piccolo-data` - at-rest encryption for user data
- **Minimal image** - enables fast dd to any size disk, including USB drives

### 2.4 Configuration and path roots
`piccolod` uses two path roots:
- `PICCOLO_CORE_ROOT` (default `/piccolo-core`)
- `PICCOLO_DATA_ROOT` (default `/piccolo-data`)

On production images, these defaults are used and should be treated as the canonical mountpoints. The environment variables primarily exist to support development/test harnesses and non-standard environments.

> **Note on code examples:** Pseudocode throughout this RFC uses the default paths (`/piccolo-core`, `/piccolo-data`) for brevity. Implementations **must** resolve all paths through `paths.CoreRoot()` / `paths.DataRoot()` (see Foundation RFC §6.1) to support non-default environments.

> **Note on CLI calls:** Some pseudocode uses `exec.CommandContext` directly for brevity. All implementations **must** route CLI calls through a `CommandRunner` interface (see §10.2) for testability — this allows unit tests to mock all disk/LUKS operations without real devices.

### 2.5 USB Boot Scenario

> **Contract note:** "Try Piccolo" is an **evaluation-only** mode. When booting from USB, both `/piccolo-core` and `/piccolo-data` reside on the USB boot device itself. This is an explicit exception to the production storage contract (architecture doc §3.1) where `/piccolo-core` lives on internal storage and USB devices are added only to `/piccolo-data`. V1 includes an **Install to Disk** flow that can either start fresh or **carry over current state** from "Try Piccolo"; however, "Try Piccolo" remains an evaluation posture (USB is not a supported long-term storage medium).

The minimal image can be dd'd to a USB drive and booted:

```
┌─────────────────────────────────────────────────────────────────────┐
│                     USB BOOT ONBOARDING                             │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  User boots from USB                                                │
│       │                                                             │
│       └── Piccolod detects USB boot                                 │
│               │                                                     │
│               └── Scan for internal (non-USB) disks                 │
│                       │                                             │
│                       ├── Internal disk found:                      │
│                       │       └── Shows two options:                │
│                       │               ├── "Install to Disk"         │
│                       │               │     └── (V1: Fresh → Carry over → Dry run) │
│                       │               └── "Try Piccolo"                │
│                       │                                             │
│                       └── No internal disk (e.g., RPi, USB-only):  │
│                               └── "Try Piccolo" only                   │
│                               └── Expand USB partitions             │
│                               └── Create persistent /piccolo-data   │
│                               └── Continue with normal setup        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## 3. Goals & Non-Goals

### 3.1 Goals

- **Boot mode detection**: Detect USB vs internal disk boot
- **Disk partitioning**: Expand root to ~20GB, create `/piccolo-data` partition
- **Subvolume verification**: Verify `/piccolo-core` subvolume exists (OS creates it)
- **LUKS2 encryption**: Pool keyfile for `/piccolo-data`
- **Install to Disk (v1, phased)**: Install from live USB onto internal disk (fresh start and carry-over paths)
- **USB boot support**: Show onboarding flow, support "Try Piccolo" mode
- **Persistent USB storage**: "Try Piccolo" creates real partitions on USB
- **Degraded mode**: System operates if USB storage (expansion) fails
- **Two-root contract**: Treat `/piccolo-core` (fixed) and `/piccolo-data` (expandable) as distinct roots, matching `docs/rfc/20260202-storage-v2-foundation.md`.

### 3.2 Non-Goals

- **Migration from pre-v2 installs**: Fresh installs only
- **JuiceFS/BadgerDB integration**: Future work
- **Cluster mode**: Future work

## 4. Boot Mode Detection

### 4.1 Detection Logic

```go
type BootMode string

const (
    BootModeInternal BootMode = "internal"  // Booted from internal disk (sata, nvme, ata)
    BootModeUSB      BootMode = "usb"       // Booted from USB drive
    BootModeUnknown  BootMode = "unknown"   // Ambiguous transport (virtio, iSCSI, some RAID)
)

func DetectBootMode(ctx context.Context) (BootMode, error) {
    // CI/QA override: allows unattended provisioning in VM/container environments
    // where lsblk TRAN is empty (virtio) and the onboarding UI cannot be clicked.
    // Not for production use — the env var is only respected in test/CI images.
    // Sentinel file gate: the override is only honored on test/CI images.
    // The sentinel file is created by the test image build, never by production images.
    if override := os.Getenv("PICCOLO_BOOT_MODE_OVERRIDE"); override != "" {
        if _, err := os.Stat("/etc/piccolo-test-image"); err != nil {
            return "", fmt.Errorf("PICCOLO_BOOT_MODE_OVERRIDE is set but /etc/piccolo-test-image does not exist; " +
                "this override is only supported on test/CI images")
        }
        switch BootMode(override) {
        case BootModeInternal, BootModeUSB, BootModeUnknown:
            return BootMode(override), nil
        default:
            return "", fmt.Errorf("invalid PICCOLO_BOOT_MODE_OVERRIDE value: %q", override)
        }
    }

    // Get root device
    rootDev, err := getRootDevice(ctx)  // e.g., /dev/sda2
    if err != nil {
        return "", err
    }

    // Get parent disk
    disk := getParentDisk(rootDev)  // e.g., /dev/sda

    // Use lsblk to get transport type (more reliable than /sys/block/*/removable)
    // TRAN values: usb, sata, nvme, ata, mmc, etc.
    transport, err := getTransportType(ctx, disk)
    if err != nil {
        return "", err
    }

    switch transport {
    case "usb":
        return BootModeUSB, nil
    case "sata", "nvme", "ata", "mmc":
        // mmc = eMMC / SD card (e.g., Raspberry Pi) — treated as internal storage
        // because on devices where eMMC/SD is the primary boot medium, it is
        // functionally equivalent to internal storage.
        return BootModeInternal, nil
    default:
        // Empty or unrecognized transport (virtio, iSCSI, some RAID controllers).
        // Treated as unknown — follows the onboarding flow so the user confirms
        // before any partition writes occur. This is safer than auto-running disk
        // prep on an ambiguous device.
        return BootModeUnknown, nil
    }
}

func getTransportType(ctx context.Context, disk string) (string, error) {
    // lsblk -ndo TRAN /dev/sda → "usb" or "sata" or "nvme" etc.
    output, err := exec.CommandContext(ctx, "lsblk", "-ndo", "TRAN", disk).Output()
    if err != nil {
        return "", fmt.Errorf("failed to get transport type: %w", err)
    }
    return strings.TrimSpace(string(output)), nil
}
```

**Why `lsblk -o TRAN` instead of `/sys/block/*/removable`:**

| Method | Limitation |
|--------|------------|
| `/sys/block/*/removable` | USB HDDs/SSDs report `0` (non-removable) |
| `lsblk -o TRAN` | Reliably shows `usb` for all USB-connected devices |

**Transport classification:**

| TRAN value | Boot mode | Rationale |
|------------|-----------|-----------|
| `usb` | USB | USB-connected device; show onboarding flow |
| `sata`, `nvme`, `ata` | Internal | Standard internal storage transports |
| `mmc` | Internal | eMMC/SD (RPi primary boot medium); functionally internal |
| Empty or unrecognized | Unknown | Ambiguous (virtio, iSCSI, some RAID); show onboarding flow so user confirms before partition writes |

**Unknown mode behavior:** `BootModeUnknown` follows the same flow as `BootModeUSB` — it shows the onboarding UI and requires explicit user confirmation before any disk prep runs. This is safer than auto-running partition writes on ambiguous hardware. VM users (the primary case for empty TRAN) are expected to be hands-on and will simply click "Try Piccolo" to proceed.

**CI/QA override:** Set `PICCOLO_BOOT_MODE_OVERRIDE=internal` (or `usb`/`unknown`) to bypass `lsblk` transport detection. This enables unattended provisioning in VM/container CI environments where `TRAN` is empty and the onboarding UI cannot be clicked. The override is **gated on a sentinel file** (`/etc/piccolo-test-image`) that is created only by test/CI image builds — if the sentinel file does not exist, the override is rejected with a startup error. This prevents accidental or malicious use on production hardware.

### 4.2 Boot Flow by Mode

```
┌─────────────────────────────────────────────────────────────────────┐
│                     BOOT MODE FLOW                                  │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  piccolod starts                                                    │
│       │                                                             │
│       ├── DetectBootMode()                                          │
│       │                                                             │
│       ├── INTERNAL BOOT ────────────────────────────────────────→   │
│       │       │                                                     │
│       │       └── NeedsDiskPrep()?                                  │
│       │               ├── YES → RunDiskPrep() → Continue            │
│       │               └── NO  → Continue                            │
│       │                                                             │
│       ├── USB BOOT ─────────────────────────────────────────────→   │
│       │       │                                                     │
│       │       └── IsFirstBoot()?                                    │
│       │               ├── YES → ShowOnboardingFlow()                │
│       │               │           ├── "Install to Disk" → InstallToDisk() │
│       │               │           └── "Try Piccolo" → RunDiskPrep()    │
│       │               └── NO  → Continue (already set up)           │
│       │                                                             │
│       └── UNKNOWN BOOT ─────────────────────────────────────────→   │
│               │                                                     │
│               └── Same as USB BOOT (onboarding flow required)       │
│                   No disk prep until user explicitly confirms        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## 5. Disk Preparation Sequence

### 5.0 Partition Table Preconditions

Phase 1 disk preparation assumes the following preconditions are met by the OS image (`piccolo-os`):

| Precondition | Expected | Checked by |
|---|---|---|
| Partition table type | GPT | `sgdisk -p` (fails on MBR) |
| ESP partition | Slot 1, FAT32, ~512MB | KIWI image build |
| Root partition | Slot 2, btrfs, MicroOS snapshots | KIWI image build |
| Root filesystem | btrfs with `/piccolo-core` subvolume | `btrfs subvolume show /piccolo-core` |
| No stacking layers | No dm-crypt, LVM, or MD-RAID on the boot disk | `lsblk -ndo TYPE` (expect `disk`/`part` only) |

If any precondition is violated, `PreparePartitioning` enters Emergency Mode (§12.3) with a diagnostic message identifying which check failed.

**Why GPT:** `sgdisk` operates exclusively on GPT tables. MBR disks are not supported and would fail at the partition creation step. All piccolo-os images use GPT.

### 5.1 Overview

Disk prep runs on first boot (or every boot for expansion). The sequence differs slightly based on what's already done:

```go
type DiskState struct {
    RootExpanded         bool  // Root partition at target size (~20GB)
    PiccoloCoreExists    bool  // /piccolo-core subvolume exists (fatal if false)
    DataPartitionExists  bool  // /piccolo-data partition exists
    DataPartitionSlot    int   // Partition slot number (0 if not exists)
    DataPartitionLUKS    bool  // /piccolo-data is LUKS formatted
    DataPartitionMounted bool  // /piccolo-data is mounted
    SetupComplete        bool  // Full setup done (partition + LUKS + mounted)
}

// SetupComplete is true when all conditions are met:
// DataPartitionExists && DataPartitionLUKS && DataPartitionMounted

func (m *StorageManager) GetDiskState(ctx context.Context) (*DiskState, error)
```

### 5.2 Two-Phase Disk Preparation

Disk preparation is split into two phases to avoid deadlock (server must start before user can provide admin password):

#### Phase 1: Boot-time Partitioning (Background, Non-Blocking)

Phase 1 runs **in the background** after the HTTP server starts. The portal is available immediately and shows a "Preparing storage..." indicator while disk prep is in progress. This ensures the PRD time-to-portal target (≤60 seconds) is met regardless of disk prep duration.

```
┌─────────────────────────────────────────────────────────────────────┐
│                 PHASE 1: BOOT-TIME PARTITIONING                     │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Server starts → portal available (shows "Preparing storage..."     │
│  if disk prep is in progress)                                       │
│                                                                     │
│  Background goroutine:                                              │
│                                                                     │
│  1. Verify /piccolo-core btrfs subvolume exists                     │
│       │                                                             │
│       ├── btrfs subvolume show /piccolo-core                        │
│       └── If missing → enter Emergency Mode (see Section 12.3)      │
│                                                                     │
│  2. Create /piccolo-data partition (at root target offset)          │
│       │   (MUST happen before root expansion to bound growpart)     │
│       │                                                             │
│       ├── Determine root target size via §5.4 sizing rules          │
│       │   (20GB on normal disks; proportional 70% on small disks)   │
│       ├── Detect next free partition slot (see 5.5)                 │
│       ├── Calculate: start = rootTargetGB (in sectors), end = disk  │
│       ├── sgdisk -n $slot:$start:0 -t $slot:8309 /dev/sdX           │
│       └── partprobe /dev/sdX                                        │
│                                                                     │
│  3. Expand root partition (bounded by data partition)               │
│       │                                                             │
│       ├── growpart /dev/sdX 2  (expands up to data partition)       │
│       └── btrfs filesystem resize max /var                          │
│           (use /var - writable mount on same btrfs filesystem)      │
│                                                                     │
│  Result: Phase 1 complete → portal transitions to normal state      │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Phase 1 ↔ Phase 2 sequencing:** Phase 2 (LUKS init / unlock) cannot proceed until Phase 1 completes. If the user provides their admin password before Phase 1 finishes, the Phase 2 handler blocks on the Phase 1 completion signal (a `sync.WaitGroup` or channel) and the portal shows "Finalizing storage preparation...". This is expected to be rare — disk prep is typically fast (seconds) and the user must navigate the onboarding UI first.

**Why this order?**
- `growpart` expands a partition to fill all contiguous free space
- By creating `/piccolo-data` at the root target offset first (20GB on normal disks, proportional on small disks per §5.4), it acts as a boundary
- `growpart` on root will expand only up to where `/piccolo-data` starts

#### Phase 2: Post-Auth Initialization (After Admin Password Setup)

Runs when user provides admin password via `POST /api/v1/crypto/setup`.

```
┌─────────────────────────────────────────────────────────────────────┐
│              PHASE 2: POST-AUTH INITIALIZATION                      │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Triggered by: POST /api/v1/crypto/setup (admin password)           │
│                                                                     │
│  4. Initialize LUKS2 on /piccolo-data                               │
│       │                                                             │
│       ├── Generate pool keyfile (64 bytes)                          │
│       ├── cryptsetup luksFormat --type luks2 /dev/sdX$slot          │
│       ├── Store encrypted keyfile in control plane                  │
│       ├── cryptsetup luksAddKey (recovery keyslot)                  │
│       └── cryptsetup open /dev/sdX$slot piccolo_data_pool_0         │
│                                                                     │
│  5. Create btrfs on LUKS device                                     │
│       │                                                             │
│       ├── mkfs.btrfs -L piccolo-data /dev/mapper/piccolo_data_pool_0 │
│       ├── mkdir -p /piccolo-data                                    │
│       └── mount /dev/mapper/piccolo_data_pool_0 /piccolo-data       │
│                                                                     │
│  6. Create directory structure                                      │
│       │                                                             │
│       └── (See Section 7)                                           │
│                                                                     │
│  7. Set NOCOW attributes                                            │
│       │                                                             │
│       └── (See Section 8)                                           │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Mapper naming and collision handling:** The dm-crypt mapper name for each pool device follows the pattern `piccolo_data_pool_<index>` (e.g., `piccolo_data_pool_0`). Before calling `cryptsetup open`, the implementation must check whether `/dev/mapper/piccolo_data_pool_<index>` already exists (stale mapper from a crash or unclean shutdown). If it does, attempt `cryptsetup close` first; if that fails (device busy), the volume is already open and can be mounted directly.

**No `/etc/fstab` entry for `/piccolo-data`:** The data volume is intentionally **not** added to fstab. Mounting is orchestrated by piccolod after admin unlock (Phase 2). Adding an fstab entry would cause systemd to attempt mounting at boot before the LUKS device is open, producing confusing mount failures. piccolod's state machine manages the full lifecycle (detect → partition → format → open → mount → unmount → close).

### 5.3 Subsequent Boot Sequence

On subsequent boots, both phases are still executed but operations are idempotent:

```
Phase 1 (Boot-time):
  1. Check disk state
  2. Verify /piccolo-core exists → Emergency Mode if missing
  3. If /piccolo-data partition missing → create at root target offset (§5.4)
  4. If root not expanded → expand (bounded by data partition)
  → Server starts

Phase 2 (After unlock via portal):
  5. Check if /piccolo-data has LUKS header
     - If no header → InitializeLUKS (first-time setup or resuming after crash)
     - If header exists → cryptsetup open (normal unlock)
  6. Mount /piccolo-data
  7. Expand LUKS + btrfs if unallocated space on pool devices (every boot)
  8. Ensure directories exist
```

### 5.4 Partition Sizing

```go
const (
    // RootTargetSizeGB is the target root partition size. openSUSE MicroOS
    // recommends a maximum of 20GB for the root filesystem (server variant);
    // this leaves room for OS snapshots while bounding growpart so the
    // remainder of the disk is available for /piccolo-data.
    RootTargetSizeGB   = 20   // MicroOS recommended max for root (server)
    MinDataPartitionGB = 5    // Minimum size for /piccolo-data
    ESPSizeGB          = 1    // Conservative estimate for ESP (~512MB, rounded up)
)

func calculatePartitionLayout(diskSizeGB int) (*PartitionLayout, error) {
    // Usable space excludes the ESP partition
    usableGB := diskSizeGB - ESPSizeGB

    if usableGB < RootTargetSizeGB + MinDataPartitionGB {
        // Small disk (e.g., 16GB USB) - use proportional split of usable space
        rootSize := usableGB * 70 / 100  // 70% for root
        dataSize := usableGB - rootSize
        if dataSize < MinDataPartitionGB {
            // Minimum: ESP (1GB) + enough usable space so 30% >= MinDataPartitionGB
            // With 70/30 split, need ceil(MinDataPartitionGB / 0.3) usable ≈ 17GB, so ~18GB total.
            minDisk := ESPSizeGB + (MinDataPartitionGB * 100 / 30) + 1
            return nil, fmt.Errorf("disk too small: %dGB total, need at least %dGB",
                diskSizeGB, minDisk)
        }
        return &PartitionLayout{RootGB: rootSize, DataGB: dataSize}, nil
    }

    // Normal disk - root gets 20GB, data gets the rest
    return &PartitionLayout{
        RootGB: RootTargetSizeGB,
        DataGB: usableGB - RootTargetSizeGB,
    }, nil
}
```

### 5.5 Partition Slot Detection

Instead of hardcoding partition number 3, detect the next free slot:

```go
func (p *Preparer) findNextPartitionSlot(ctx context.Context, disk string) (int, error) {
    // Use sgdisk to list existing partitions
    // sgdisk -p /dev/sdX shows partition table
    output, err := exec.CommandContext(ctx, "sgdisk", "-p", disk).Output()
    if err != nil {
        return 0, err
    }

    // Parse output to find used partition numbers
    usedSlots := parsePartitionSlots(output)  // e.g., {1, 2} for ESP + root

    // Find first free slot (GPT supports 1-128)
    for slot := 1; slot <= 128; slot++ {
        if !usedSlots[slot] {
            return slot, nil
        }
    }
    return 0, fmt.Errorf("no free partition slots")
}

func (p *Preparer) getLastPartitionEnd(ctx context.Context, disk string) (sectorOffset int64, err error) {
    // Use sfdisk to get partition boundaries in JSON
    // sfdisk -J /dev/sdX
    output, err := exec.CommandContext(ctx, "sfdisk", "-J", disk).Output()
    if err != nil {
        return 0, err
    }

    var table struct {
        PartitionTable struct {
            Partitions []struct {
                Start int64 `json:"start"`
                Size  int64 `json:"size"`
            } `json:"partitions"`
        } `json:"partitiontable"`
    }
    if err := json.Unmarshal(output, &table); err != nil {
        return 0, err
    }

    // Find the end of the last partition
    var maxEnd int64
    for _, p := range table.PartitionTable.Partitions {
        end := p.Start + p.Size
        if end > maxEnd {
            maxEnd = end
        }
    }
    return maxEnd, nil
}
```

**Expected partition layout:**
| Slot | Content | Created By |
|------|---------|------------|
| 1 | ESP (~512MB) | KIWI |
| 2 | Root btrfs | KIWI |
| 3+ | `/piccolo-data` | piccolod (detected dynamically) |

### 5.6 Partition Device Path Helper

Linux block device naming varies by transport: `/dev/sda3` (SCSI/SATA), `/dev/nvme0n1p3` (NVMe), `/dev/mmcblk0p3` (eMMC/SD). The rule is: if the disk path ends with a digit, a `p` separator is inserted before the partition number.

```go
// partitionDevicePath returns the device node for a partition on a disk.
// Handles all naming conventions: sda→sda3, nvme0n1→nvme0n1p3, mmcblk0→mmcblk0p3.
func partitionDevicePath(disk string, slot int) string {
    if disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
        return fmt.Sprintf("%sp%d", disk, slot) // nvme, mmcblk, loop
    }
    return fmt.Sprintf("%s%d", disk, slot) // sda, vda
}
```

### 5.7 Idempotent Operations

All disk prep operations must be idempotent to support repeated execution on every boot.

**Important:** Data partition MUST be created before root expansion (see Section 5.2). Both operations use `calculatePartitionLayout` (§5.4) for disk-size-aware sizing.

```go
const (
    RootTargetSizeGB   = 20
    MinDataPartitionGB = 5
)

// getRootPartitionStart returns the start sector of the root partition
// using sfdisk JSON output. This is needed to compute the data partition
// start relative to where root actually begins (after the ESP).
func (p *Preparer) getRootPartitionStart(ctx context.Context, disk, rootDev string) (int64, error) {
    output, err := exec.CommandContext(ctx, "sfdisk", "-J", disk).Output()
    if err != nil {
        return 0, fmt.Errorf("sfdisk failed: %w", err)
    }

    var table struct {
        PartitionTable struct {
            Partitions []struct {
                Node  string `json:"node"`
                Start int64  `json:"start"`
            } `json:"partitions"`
        } `json:"partitiontable"`
    }
    if err := json.Unmarshal(output, &table); err != nil {
        return 0, fmt.Errorf("failed to parse sfdisk output: %w", err)
    }

    for _, p := range table.PartitionTable.Partitions {
        if p.Node == rootDev {
            return p.Start, nil
        }
    }
    return 0, fmt.Errorf("root partition %s not found in partition table", rootDev)
}

// reloadPartitionTable attempts partprobe, falling back to partx --add
// for kernels that refuse a full partition table re-read on a busy root disk.
// The slot parameter narrows the partx fallback to the specific partition that
// was just created, avoiding unnecessary re-reads of other partition entries.
func (p *Preparer) reloadPartitionTable(ctx context.Context, disk string, slot int) error {
    // Try partprobe first (standard, reloads entire table)
    if err := p.runner.Run(ctx, "partprobe", disk); err == nil {
        return nil
    }

    p.logger.Warn("partprobe failed on busy disk, trying partx --add fallback",
        "disk", disk, "slot", slot)

    // Fallback: partx --add scans for new partitions without a full re-read.
    // Narrow to the exact slot that was just created (--nr N:N) rather than
    // scanning all partitions (--nr :-1), which risks confusing the kernel
    // if other partition entries are in flux.
    slotStr := fmt.Sprintf("%d:%d", slot, slot)
    if err := p.runner.Run(ctx, "partx", "--add", "--nr", slotStr, disk); err != nil {
        return fmt.Errorf("both partprobe and partx --add failed for slot %d: %w", slot, err)
    }

    return nil
}

// getDiskSizeGB returns the total disk size in GB
func (p *Preparer) getDiskSizeGB(ctx context.Context, disk string) (int, error) {
    // lsblk -ndo SIZE -b /dev/sdX → size in bytes
    output, err := exec.CommandContext(ctx, "lsblk", "-ndo", "SIZE", "-b", disk).Output()
    if err != nil {
        return 0, fmt.Errorf("failed to get disk size: %w", err)
    }
    sizeBytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
    if err != nil {
        return 0, fmt.Errorf("failed to parse disk size: %w", err)
    }
    // Ceiling division: a 20.1 GB disk must report 21 GB, not 20 GB, to ensure
    // calculatePartitionLayout sees enough space for both root and data partitions.
    // Floor division would silently lose the fractional GB and could push a
    // borderline disk below the MinDataPartitionGB threshold.
    const gib = int64(1024 * 1024 * 1024)
    return int((sizeBytes + gib - 1) / gib), nil
}

// getSectorSize queries the logical sector size of a disk
// Modern NVMe drives may use 4096-byte sectors instead of 512
func (p *Preparer) getSectorSize(ctx context.Context, disk string) (int64, error) {
    // lsblk -ndo LOG-SEC /dev/sdX
    output, err := exec.CommandContext(ctx, "lsblk", "-ndo", "LOG-SEC", disk).Output()
    if err != nil {
        return 512, nil  // Default to 512 if query fails
    }
    size, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
    if err != nil {
        return 512, nil
    }
    return size, nil
}

// CreateDataPartition creates /piccolo-data partition using the sizing rules from §5.4.
// MUST be called BEFORE ExpandRootPartition to bound growpart.
func (p *Preparer) CreateDataPartition(ctx context.Context) error {
    state, err := p.GetPartitionState(ctx)
    if err != nil {
        return err
    }

    if state.DataPartitionExists {
        p.logger.Info("data partition already exists, skipping creation",
            "slot", state.DataPartitionSlot)
        return nil
    }

    rootDev, _ := getRootDevice(ctx)
    disk := getParentDisk(rootDev)

    // Determine disk size and apply sizing rules (§5.4)
    diskSizeGB, err := p.getDiskSizeGB(ctx, disk)
    if err != nil {
        return fmt.Errorf("failed to get disk size: %w", err)
    }

    layout, err := calculatePartitionLayout(diskSizeGB)
    if err != nil {
        return fmt.Errorf("failed to calculate partition layout: %w", err)
    }

    // Query actual sector size (512 for SATA, possibly 4096 for NVMe)
    sectorSize, _ := p.getSectorSize(ctx, disk)

    // Compute data partition start relative to root partition's actual start.
    // The root partition starts after the ESP, so we use its real start sector
    // plus the target root size to position the data partition correctly.
    rootStartSector, err := p.getRootPartitionStart(ctx, disk, rootDev)
    if err != nil {
        return fmt.Errorf("failed to get root partition start: %w", err)
    }
    rootTargetSectors := (int64(layout.RootGB) * 1024 * 1024 * 1024) / sectorSize
    startSector := rootStartSector + rootTargetSectors

    // Align to 1 MiB boundary (2048 sectors for 512-byte, 256 for 4096-byte).
    // sgdisk aligns partitions by default; we must match to avoid the boundary
    // shifting silently and weakening the growpart bound.
    alignSectors := int64(1024 * 1024 / sectorSize) // 1 MiB in sectors
    startSector = ((startSector + alignSectors - 1) / alignSectors) * alignSectors

    slot, err := p.findNextPartitionSlot(ctx, disk)
    if err != nil {
        return err
    }

    // Create partition: start after root allocation, extend to end of disk (0 = end)
    // Type 8309 = Linux LUKS; label "piccolo-data" for identification by tools/humans.
    if err := p.runner.Run(ctx, "sgdisk",
        "-n", fmt.Sprintf("%d:%d:0", slot, startSector),
        "-t", fmt.Sprintf("%d:8309", slot),
        "-c", fmt.Sprintf("%d:piccolo-data", slot),
        disk,
    ); err != nil {
        return fmt.Errorf("sgdisk failed: %w", err)
    }

    // Reload partition table — MUST succeed.
    // If the kernel doesn't see the new data partition, growpart on root
    // will expand into unbounded free space, defeating the boundary mechanism.
    if err := p.reloadPartitionTable(ctx, disk, slot); err != nil {
        return fmt.Errorf("kernel cannot see new data partition (boundary unsafe): %w", err)
    }

    // CRITICAL: Verify the kernel actually registered the new partition.
    // partprobe/partx returning success does not guarantee the device node exists.
    // If we proceed to growpart without this check, root could expand unbounded.
    partDev := partitionDevicePath(disk, slot)
    for attempt := 0; attempt < 10; attempt++ {
        if _, err := os.Stat(partDev); err == nil {
            break // Kernel sees the partition
        }
        if attempt == 9 {
            return fmt.Errorf("kernel did not register partition %s after reload (boundary unsafe)", partDev)
        }
        time.Sleep(200 * time.Millisecond)
    }

    p.logger.Info("data partition created",
        "disk", disk,
        "slot", slot,
        "start_sector", startSector,
        "root_target_gb", layout.RootGB,
        "data_target_gb", layout.DataGB,
        "disk_size_gb", diskSizeGB)
    return nil
}

// ExpandRootPartition expands root up to the data partition boundary.
// MUST be called AFTER CreateDataPartition.
// The target size depends on disk size (see §5.4): 20GB on normal disks,
// proportional (70%) on small disks.
func (p *Preparer) ExpandRootPartition(ctx context.Context) error {
    rootDev, _ := getRootDevice(ctx)
    disk := getParentDisk(rootDev)
    partNum := getPartitionNumber(rootDev)  // e.g., 2

    // Determine target root size using same sizing rules as CreateDataPartition
    diskSizeGB, err := p.getDiskSizeGB(ctx, disk)
    if err != nil {
        return fmt.Errorf("failed to get disk size: %w", err)
    }
    layout, err := calculatePartitionLayout(diskSizeGB)
    if err != nil {
        return fmt.Errorf("failed to calculate partition layout: %w", err)
    }

    // Check current partition size
    currentSizeBytes, err := p.getPartitionSize(ctx, rootDev)
    if err != nil {
        return err
    }
    currentSizeGB := currentSizeBytes / (1024 * 1024 * 1024)

    // Skip if already at or above target size
    if currentSizeGB >= int64(layout.RootGB) {
        p.logger.Info("root partition already at target size, skipping expansion",
            "current_gb", currentSizeGB,
            "target_gb", layout.RootGB)
        return nil
    }

    // growpart expands partition to fill free space up to next partition
    // Since data partition exists at 20GB mark, this is naturally bounded
    if err := p.runner.Run(ctx, "growpart", disk, fmt.Sprintf("%d", partNum)); err != nil {
        // growpart exits non-zero if partition is already at max size
        if strings.Contains(err.Error(), "NOCHANGE") {
            p.logger.Info("root partition already at maximum size")
            return nil
        }
        return fmt.Errorf("growpart failed: %w", err)
    }

    // Notify kernel of partition table change after growpart.
    // Non-fatal: growpart already triggers BLKRRPART ioctl internally to
    // re-read the partition table. partprobe is a belt-and-suspenders call;
    // if it fails, the kernel has already seen the expanded partition via growpart.
    if err := p.runner.Run(ctx, "partprobe", disk); err != nil {
        p.logger.Warn("partprobe failed after root expansion (growpart already updated kernel)", "error", err)
    }

    // Expand btrfs filesystem to use new partition space
    // Use /var (writable subvolume) since / may be read-only on MicroOS
    if err := p.runner.Run(ctx, "btrfs", "filesystem", "resize", "max", "/var"); err != nil {
        return fmt.Errorf("btrfs resize failed: %w", err)
    }

    p.logger.Info("root partition expanded successfully")
    return nil
}
```

### 5.7 Power Failure Recovery (Phase 1)

All Phase 1 operations are designed to be crash-safe and self-healing on the next boot:

| Failure Point | State After Crash | Recovery on Next Boot |
|---|---|---|
| During `sgdisk` (GPT write) | GPT may have inconsistent primary/secondary tables. `sgdisk` writes the secondary GPT first, then the primary. Most tools (including the kernel) prefer the primary table. | `sgdisk` and `sfdisk` can recover from primary/secondary mismatch. On next boot, `GetPartitionState` re-reads the table. If the data partition was not created, it is retried. |
| After `sgdisk`, before `partprobe` | GPT is valid on disk but kernel has not re-read it. | Next boot re-reads the full partition table from scratch — no `partprobe` needed. |
| After data partition created, before `growpart` | Data partition exists, root is still minimal (~2GB). | `ExpandRootPartition` detects root is below target size and runs `growpart`, bounded by the existing data partition. |
| After `growpart`, before `btrfs resize` | Root partition is expanded but btrfs filesystem is smaller than the partition. | `btrfs filesystem resize max /var` detects the gap and extends the filesystem. This is a safe, idempotent online operation. |
| After `btrfs resize` | Phase 1 complete. | No action needed — state checks confirm everything is done. |

**Key invariant:** Because each step checks current state before acting (idempotent), any partial completion is automatically resumed on the next boot. No rollback is required or attempted.

## 6. LUKS2 Encryption

### 6.1 Key Hierarchy

```
┌─────────────────────────────────────────────────────────────────────┐
│                     LUKS2 KEY HIERARCHY                             │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Admin Password                                                     │
│       │                                                             │
│       ├──→ Derives KEK (Argon2id + salt from keyset.json)           │
│       │         │                                                   │
│       │         └──→ Unseals SDEK from keyset.json                  │
│       │                    │                                        │
│       │                    ├──→ Unwraps gocryptfs passphrase        │
│       │                    │    (from volumes/control-plane/        │
│       │                    │     piccolo.volume.json)               │
│       │                    │         └──→ Mounts control-plane      │
│       │                    │                                        │
│       │                    └──→ Unwraps piccolo_data_pool_key.enc   │
│       │                         (from crypto/)                      │
│       │                              └──→ LUKS Keyslot 0            │
│       │                                                             │
│       ├──→ LUKS Keyslot 1 (Admin Password Recovery)                 │
│       │    Admin password → Argon2id(password, persisted salt+params)│
│       │                                                             │
│  Recovery Mnemonic (24-word)                                        │
│       │                                                             │
│       └──→ Derives KEK → Unseals SDEK → Unwraps pool keyfile       │
│            → LUKS Keyslot 0 (same keyfile, same path as above)     │
│                                                                     │
│       └──→ LUKS Keyslot 2 (Mnemonic Recovery)                      │
│            Mnemonic → Argon2id(mnemonic-derived, persisted salt+params)│
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Three LUKS keyslots per device:**
| Keyslot | Key Source | Purpose |
|---------|-----------|---------|
| 0 | Pool keyfile (unwrapped from SDEK via admin password or recovery mnemonic) | Primary unlock path — used on every boot |
| 1 | Argon2id(admin password, persisted salt + params) | Offline recovery when control plane is unavailable |
| 2 | Argon2id(mnemonic-derived key, persisted salt + params) | Recovery when admin password is lost — user unlocks with 24-word mnemonic |

**Why three keyslots:** The recovery mnemonic can already unlock the control plane (via `crypt.Manager.UnlockWithRecoveryKey`), which provides the pool keyfile for keyslot 0. Keyslot 2 provides a direct unlock path for `/piccolo-data` when the control plane itself is damaged or unavailable, ensuring the mnemonic is a complete recovery mechanism independent of `/piccolo-core` state.

### 6.2 Pool Keyfile Management

```go
// Stored at /piccolo-core/crypto/piccolo_data_pool_key.enc
// This is OUTSIDE gocryptfs (always readable once SDEK is available).
// The pool keyfile is node-scoped (specific to the LUKS devices on this node)
// but IS included in PCV exports so that restore workflows can unlock
// existing /piccolo-data partitions. See Foundation RFC §8.2.2 for the
// per-node room concept within PCV.
type PoolKeyfile struct {
    Version   int       `json:"version"`
    KeyData   []byte    `json:"key_data"`   // 64-byte random keyfile (encrypted with SDEK)
    CreatedAt time.Time `json:"created_at"`
}

// Generate new keyfile
func GeneratePoolKeyfile() ([]byte, error) {
    key := make([]byte, 64)
    if _, err := rand.Read(key); err != nil {
        return nil, err
    }
    return key, nil
}
```

### 6.3 LUKS Initialization

**Ephemeral Secrets Directory:** `/run/piccolo/` is a tmpfs-backed directory for temporary secrets during cryptographic operations. It is:
- Cleared automatically on every reboot (standard Linux `/run` behavior)
- Never persisted to disk - plaintext secrets exist only in RAM
- Created by piccolod at startup: `os.MkdirAll("/run/piccolo", 0700)`

The **encrypted** pool keyfile is stored persistently at `/piccolo-core/crypto/piccolo_data_pool_key.enc`. The `/run/piccolo/` path is only used transiently during `cryptsetup` operations.

```go
// InitializeLUKS formats a LUKS2 device and enrolls all three keyslots.
// The recovery mnemonic is generated during POST /api/v1/crypto/setup (the same
// endpoint that triggers this call), so mnemonicKey is always available at init time.
// For recovery key rotation after setup, see AddMnemonicKeyslot below.
func (m *StorageManager) InitializeLUKS(ctx context.Context, device, adminPassword string, mnemonicKey []byte) error {
    // 0. Ensure ephemeral secrets directory exists (tmpfs, cleared on reboot)
    if err := os.MkdirAll("/run/piccolo", 0700); err != nil {
        return fmt.Errorf("failed to create ephemeral secrets dir: %w", err)
    }

    // 1. Generate pool keyfile
    keyfile, err := GeneratePoolKeyfile()
    if err != nil {
        return err
    }

    // 2. Write keyfile to temp location (memory-backed tmpfs, never persisted)
    keyfilePath := "/run/piccolo/piccolo_data_pool_key"
    if err := os.WriteFile(keyfilePath, keyfile, 0600); err != nil {
        return err
    }
    keyfileCleanedUp := false
    cleanupKeyfile := func() {
        if !keyfileCleanedUp {
            os.Remove(keyfilePath)
            secureZero(keyfile)
            keyfileCleanedUp = true
        }
    }
    defer cleanupKeyfile()

    // 3. LUKS format with keyfile (keyslot 0)
    // Pin cipher parameters explicitly for reproducibility across OS versions.
    // Keyslot 0 uses a 64-byte random keyfile (max entropy) — pbkdf2 with
    // minimal iterations is sufficient. Memory-hard KDF adds no security value
    // for high-entropy key material and would add ~1-2s to every boot unlock.
    // Keyslots 1 and 2 (password/mnemonic-derived) use argon2id via addKeyslot.
    if err := m.runner.Run(ctx, "cryptsetup", "luksFormat",
        "--type", "luks2",
        "--batch-mode",
        "--cipher", "aes-xts-plain64",
        "--key-size", "512",
        "--hash", "sha512",
        "--pbkdf", "pbkdf2",
        "--pbkdf-force-iterations", "1000",
        "--key-file", keyfilePath,
        device); err != nil {
        return err
    }

    // At this point the ONLY unlock path is the ephemeral keyfile in memory.
    // We MUST persist at least one durable unlock path before allowing cleanup.
    // Order: store keyfile first (primary path), then add recovery keyslots (secondary).
    keyfileStored := false
    adminRecoveryOK := false
    mnemonicRecoveryOK := false

    // 4. Store pool keyfile in control plane (crypto/) — PRIMARY unlock path
    if err := m.crypto.StorePoolKeyfile(ctx, keyfile); err != nil {
        m.logger.Error("failed to store pool keyfile", "error", err)
    } else {
        keyfileStored = true
    }

    // 5. Generate and persist KDF params for this device.
    // getLUKSUUID MUST succeed — the UUID is used as the salt anchor for all
    // recovery keyslot derivations. If it fails, the device was not actually
    // formatted (or the header is unreadable), and continuing would produce
    // non-recoverable keyslots.
    deviceUUID, err := m.getLUKSUUID(ctx, device)
    if err != nil {
        return fmt.Errorf("failed to read LUKS UUID after format: %w", err)
    }
    kdfParams, err := NewLUKSKDFParams(deviceUUID)
    if err != nil {
        return fmt.Errorf("failed to generate KDF params: %w", err)
    }
    if err := os.MkdirAll(filepath.Dir(kdfParamsPath(deviceUUID)), 0700); err != nil {
        return fmt.Errorf("failed to create KDF params dir: %w", err)
    }
    if err := writeJSON(kdfParamsPath(deviceUUID), kdfParams); err != nil {
        return fmt.Errorf("failed to persist KDF params: %w", err)
    }

    // 6. Add admin-password recovery keyslot (keyslot 1)
    recoveryPass := DeriveRecoveryPassphrase(adminPassword, kdfParams)
    if err := m.addKeyslot(ctx, device, keyfilePath, recoveryPass, 1); err != nil {
        m.logger.Error("failed to add admin recovery keyslot", "error", err)
    } else {
        adminRecoveryOK = true
    }

    // 7. Add mnemonic recovery keyslot (keyslot 2)
    // mnemonicKey is always provided — the recovery mnemonic is generated during
    // the same crypto/setup call that triggers InitializeLUKS.
    mnemonicPass := DeriveMnemonicRecoveryPassphrase(mnemonicKey, kdfParams)
    if err := m.addKeyslot(ctx, device, keyfilePath, mnemonicPass, 2); err != nil {
        m.logger.Error("failed to add mnemonic recovery keyslot", "error", err)
    } else {
        mnemonicRecoveryOK = true
    }

    // SAFETY: At least one persistent unlock path must exist.
    // If none succeeded, the ephemeral keyfile is about to be zeroed
    // and the LUKS device would become permanently locked.
    if !keyfileStored && !adminRecoveryOK && !mnemonicRecoveryOK {
        // All three persistent unlock paths failed. The LUKS device has been
        // formatted but has no durable keyslot — it will become permanently
        // locked after reboot (the ephemeral keyfile lives in tmpfs).
        //
        // Recovery: this is a REFLASH scenario. The user must re-flash the
        // OS image and start fresh. No user data has been written to the LUKS
        // device at this point (it was just formatted), so nothing is lost.
        //
        // We still preserve the ephemeral keyfile so the current boot session
        // can continue diagnostics, but do NOT guide the user toward manual
        // cryptsetup commands (Piccolo has no SSH access by design).
        keyfileCleanedUp = true // prevent deferred cleanup
        return fmt.Errorf("LUKS formatted but no persistent unlock path: " +
            "keyfile storage, admin recovery, and mnemonic recovery all failed; " +
            "recovery requires re-flashing the OS image and restarting setup")
    }

    if !keyfileStored {
        m.logger.Warn("pool keyfile not stored; recovery keyslots are the only unlock path")
    }
    if !adminRecoveryOK {
        m.logger.Warn("admin recovery keyslot not added", "device", device)
    }
    if !mnemonicRecoveryOK {
        m.logger.Warn("mnemonic recovery keyslot not added", "device", device)
    }

    // 8. Backup LUKS header to control plane (for disaster recovery)
    if err := m.backupLUKSHeader(ctx, device, deviceUUID); err != nil {
        m.logger.Warn("failed to backup LUKS header", "error", err)
        // Non-fatal: system can operate without header backup
    }

    return nil
}
```

### 6.4 Recovery Keyslot

#### 6.4.1 KDF Parameter Persistence

Argon2id output is a function of all parameters including parallelism (`threads`). To allow dynamic, CPU-appropriate thread counts without risking unlock failures if the CPU count changes (cgroups, VM resize, BIOS settings), all derivation parameters and salts are persisted per LUKS device.

```go
// Stored at /piccolo-core/crypto/luks-kdf-params/<device-uuid>.json
// This file is OUTSIDE gocryptfs (always readable), included in PCV exports
// (node-scoped data), and required for re-deriving recovery passphrases.
type LUKSKDFParams struct {
    Version       int    `json:"version"`        // Schema version (1)
    DeviceUUID    string `json:"device_uuid"`     // LUKS device UUID
    Argon2Time    uint32 `json:"argon2_time"`     // Argon2id time parameter
    Argon2Memory  uint32 `json:"argon2_memory"`   // Argon2id memory (KiB)
    Argon2Threads uint8  `json:"argon2_threads"`  // Argon2id parallelism
    KeyLength     uint32 `json:"key_length"`      // Derived key length (bytes)
    SaltAdmin     []byte `json:"salt_admin"`      // Random 32-byte salt for admin passphrase
    SaltMnemonic  []byte `json:"salt_mnemonic"`   // Random 32-byte salt for mnemonic passphrase
    CreatedAt     string `json:"created_at"`      // ISO 8601
}

// NewLUKSKDFParams generates params for a new device. Called once during
// InitializeLUKS; the result is persisted and re-read on every derivation.
func NewLUKSKDFParams(deviceUUID string) (*LUKSKDFParams, error) {
    // 32 bytes = 256 bits of entropy, matching Argon2 recommendation (RFC 9106 §4)
    // and LUKS2's internal salt length.
    saltAdmin := make([]byte, 32)
    saltMnemonic := make([]byte, 32)
    if _, err := rand.Read(saltAdmin); err != nil {
        return nil, fmt.Errorf("failed to generate admin salt: %w", err)
    }
    if _, err := rand.Read(saltMnemonic); err != nil {
        return nil, fmt.Errorf("failed to generate mnemonic salt: %w", err)
    }

    return &LUKSKDFParams{
        Version:       1,
        DeviceUUID:    deviceUUID,
        Argon2Time:    3,
        Argon2Memory:  512 * 1024, // 512 MiB — matches crypt.Manager's SDEK derivation hardness
        Argon2Threads: uint8(min(8, max(1, runtime.NumCPU()-1))), // Cap at 8 for portability (PCV restore to smaller hardware)
        KeyLength:     32,
        SaltAdmin:     saltAdmin,
        SaltMnemonic:  saltMnemonic,
        CreatedAt:     time.Now().UTC().Format(time.RFC3339),
    }, nil
}

func kdfParamsPath(deviceUUID string) string {
    return filepath.Join(paths.CoreRoot(), "crypto/luks-kdf-params", deviceUUID+".json")
}
```

#### 6.4.2 Passphrase Derivation

```go
// DeriveRecoveryPassphrase derives the admin-password LUKS recovery passphrase
// using persisted KDF params. The params file MUST exist (created during InitializeLUKS).
func DeriveRecoveryPassphrase(adminPassword string, params *LUKSKDFParams) []byte {
    return argon2.IDKey(
        []byte(adminPassword),
        params.SaltAdmin,
        params.Argon2Time,
        params.Argon2Memory,
        params.Argon2Threads,
        params.KeyLength,
    )
}

// DeriveMnemonicRecoveryPassphrase derives a LUKS passphrase from the
// mnemonic-derived key material. This provides a direct unlock path for
// /piccolo-data when the admin password is lost — the user enters their
// 24-word recovery mnemonic, which yields mnemonicKey via crypt.Manager,
// and this function derives the device-specific LUKS passphrase.
func DeriveMnemonicRecoveryPassphrase(mnemonicKey []byte, params *LUKSKDFParams) []byte {
    return argon2.IDKey(
        mnemonicKey,
        params.SaltMnemonic,
        params.Argon2Time,
        params.Argon2Memory,
        params.Argon2Threads,
        params.KeyLength,
    )
}

// addKeyslot adds a new LUKS keyslot using the pool keyfile as the existing key.
func (m *StorageManager) addKeyslot(ctx context.Context, device string, keyfilePath string, passphrase []byte, slot int) error {
    // Write passphrase to temp file (tmpfs)
    passPath := fmt.Sprintf("/run/piccolo/keyslot-%d-passphrase", slot)
    if err := os.WriteFile(passPath, passphrase, 0600); err != nil {
        return fmt.Errorf("failed to write keyslot passphrase: %w", err)
    }
    defer os.Remove(passPath)
    defer secureZero(passphrase)

    if err := m.runner.Run(ctx, "cryptsetup", "luksAddKey",
        "--key-file", keyfilePath,
        "--key-slot", fmt.Sprintf("%d", slot),
        device,
        passPath,
    ); err != nil {
        return fmt.Errorf("failed to add keyslot %d: %w", slot, err)
    }

    return nil
}

// secureZero overwrites a byte slice with zeros. runtime.KeepAlive prevents
// the Go compiler from optimizing away the dead stores.
func secureZero(b []byte) {
    for i := range b {
        b[i] = 0
    }
    runtime.KeepAlive(b)
}
```

### 6.5 Recovery Mnemonic Rotation Hook

When the user rotates their recovery mnemonic (via `POST /api/v1/crypto/recovery-key/generate`), keyslot 2 must be updated on all pool devices. This uses `luksChangeKey` for atomic replacement, matching the password rotation pattern.

```go
// OnRecoveryMnemonicRotated updates keyslot 2 on all /piccolo-data LUKS devices
// when the user generates a new recovery mnemonic.
// Uses crypt.Manager.WithMnemonicKey() callbacks (old + new) so raw key material
// stays inside the crypto module scope — matching the WithSDEK pattern.
//
// Crash recovery: tracks progress via mnemonic-rotation-progress.json, matching
// the password rotation pattern in §6.6.1. On crash, resumeMnemonicRotationIfNeeded
// uses the pool keyfile (keyslot 0) to re-create keyslot 2.
func (m *StorageManager) OnRecoveryMnemonicRotated(ctx context.Context) error {
    devices, err := m.listDataPoolDevices(ctx)
    if err != nil {
        return err
    }

    // Track rotation progress for crash recovery (matching §6.6 pattern)
    progressPath := paths.CoreJoin("crypto", "mnemonic-rotation-progress.json")
    progress := &RotationProgress{
        StartedAt: time.Now(),
        Total:     len(devices),
        Completed: []string{},
    }
    if err := writeJSON(progressPath, progress); err != nil {
        return fmt.Errorf("failed to write mnemonic rotation progress: %w", err)
    }

    for _, dev := range devices {
        params, err := readJSON[LUKSKDFParams](kdfParamsPath(dev.UUID))
        if err != nil {
            return fmt.Errorf("failed to read KDF params for %s: %w", dev.UUID, err)
        }

        // Derive old and new passphrases via callbacks
        var oldPass, newPass []byte
        if err := m.crypto.WithOldMnemonicKey(func(oldKey []byte) error {
            oldPass = DeriveMnemonicRecoveryPassphrase(oldKey, params)
            return nil
        }); err != nil {
            return fmt.Errorf("failed to derive old mnemonic passphrase: %w", err)
        }
        if err := m.crypto.WithMnemonicKey(func(newKey []byte) error {
            newPass = DeriveMnemonicRecoveryPassphrase(newKey, params)
            return nil
        }); err != nil {
            return fmt.Errorf("failed to derive new mnemonic passphrase: %w", err)
        }

        if err := m.changeLUKSKeyslot(ctx, dev.Path, 2, oldPass, newPass); err != nil {
            return fmt.Errorf("failed to rotate keyslot 2 for %s: %w", dev.UUID, err)
        }

        if err := m.backupLUKSHeader(ctx, dev.Path, dev.UUID); err != nil {
            m.logger.Warn("failed to re-backup LUKS header after mnemonic rotation",
                "device", dev.UUID, "error", err)
        }

        progress.Completed = append(progress.Completed, dev.UUID)
        _ = writeJSON(progressPath, progress)
    }

    os.Remove(progressPath)
    return nil
}
```

#### 6.5.1 Crash Recovery for Mnemonic Rotation

If `piccolod` crashes during mnemonic rotation, a `mnemonic-rotation-progress.json` file will exist on next boot. Recovery uses the pool keyfile (keyslot 0) to re-create keyslot 2, matching the password rotation crash recovery pattern in §6.6.1.

```go
func (m *StorageManager) resumeMnemonicRotationIfNeeded(ctx context.Context) error {
    progressPath := paths.CoreJoin("crypto", "mnemonic-rotation-progress.json")
    progress, err := readJSON[RotationProgress](progressPath)
    if errors.Is(err, os.ErrNotExist) {
        return nil  // No rotation in progress
    }
    if err != nil {
        return fmt.Errorf("failed to read mnemonic rotation progress: %w", err)
    }

    m.logger.Warn("resuming interrupted mnemonic keyslot rotation",
        "completed", len(progress.Completed), "total", progress.Total)

    devices, _ := m.listDataPoolDevices(ctx)
    completed := toSet(progress.Completed)

    for _, dev := range devices {
        if completed[dev.UUID] {
            continue  // Already rotated
        }

        // Re-create keyslot 2 using the pool keyfile (always available after unlock)
        // and the current (new) mnemonic key via callback.
        params, _ := readJSON[LUKSKDFParams](kdfParamsPath(dev.UUID))
        var newPass []byte
        if err := m.crypto.WithMnemonicKey(func(key []byte) error {
            newPass = DeriveMnemonicRecoveryPassphrase(key, params)
            return nil
        }); err != nil {
            return fmt.Errorf("failed to derive mnemonic passphrase for recovery: %w", err)
        }

        m.logger.Warn("re-creating keyslot 2 via pool keyfile", "device", dev.UUID)
        if err := m.rekeySlotViaPoolKeyfile(ctx, dev.Path, 2, newPass); err != nil {
            return fmt.Errorf("failed to recover keyslot 2 for %s: %w", dev.UUID, err)
        }

        progress.Completed = append(progress.Completed, dev.UUID)
        _ = writeJSON(progressPath, progress)
    }

    os.Remove(progressPath)
    m.logger.Info("mnemonic keyslot rotation recovery complete")
    return nil
}
```

### 6.6 Admin Password Rotation Hook

Password rotation must update LUKS keyslot 1 (admin-derived) on all pool devices. Keyslot 2 (mnemonic-derived) is unaffected by password rotation since it is derived from the recovery mnemonic, which does not change when the password changes.

**Atomicity requirement:** Keyslot updates MUST use `cryptsetup luksChangeKey` (not `luksRemoveKey` + `luksAddKey`) to perform an atomic in-place replacement. This ensures there is never a window where the recovery keyslot is absent.

```go
func (m *StorageManager) OnAdminPasswordRotated(ctx context.Context, oldPass, newPass string) error {
    devices, err := m.listDataPoolDevices(ctx)
    if err != nil {
        return err
    }

    // Track rotation progress so we can resume after a crash.
    progressPath := paths.CoreJoin("crypto", "luks-rotation-progress.json")
    progress := &RotationProgress{
        StartedAt: time.Now(),
        Total:     len(devices),
        Completed: []string{},
    }
    if err := writeJSON(progressPath, progress); err != nil {
        return fmt.Errorf("failed to write rotation progress: %w", err)
    }

    for _, dev := range devices {
        params, err := readJSON[LUKSKDFParams](kdfParamsPath(dev.UUID))
        if err != nil {
            return fmt.Errorf("failed to read KDF params for %s: %w", dev.UUID, err)
        }
        oldRecovery := DeriveRecoveryPassphrase(oldPass, params)
        newRecovery := DeriveRecoveryPassphrase(newPass, params)

        // cryptsetup luksChangeKey atomically replaces keyslot 1
        if err := m.changeLUKSKeyslot(ctx, dev.Path, 1, oldRecovery, newRecovery); err != nil {
            return fmt.Errorf("failed to rotate keyslot 1 for %s: %w", dev.UUID, err)
        }

        // Re-backup LUKS header after keyslot change
        if err := m.backupLUKSHeader(ctx, dev.Path, dev.UUID); err != nil {
            m.logger.Warn("failed to re-backup LUKS header after rotation", "device", dev.UUID, "error", err)
        }

        progress.Completed = append(progress.Completed, dev.UUID)
        _ = writeJSON(progressPath, progress)
    }

    // Rotation complete — remove progress file
    os.Remove(progressPath)
    return nil
}

// changeLUKSKeyslot atomically replaces a keyslot using cryptsetup luksChangeKey.
func (m *StorageManager) changeLUKSKeyslot(ctx context.Context, device string, slot int, oldPass, newPass []byte) error {
    oldPath := fmt.Sprintf("/run/piccolo/rotation-old-%d", slot)
    newPath := fmt.Sprintf("/run/piccolo/rotation-new-%d", slot)

    if err := os.WriteFile(oldPath, oldPass, 0600); err != nil {
        return err
    }
    defer os.Remove(oldPath)
    if err := os.WriteFile(newPath, newPass, 0600); err != nil {
        return err
    }
    defer os.Remove(newPath)

    // luksChangeKey atomically replaces the passphrase for the given keyslot
    return m.runner.Run(ctx, "cryptsetup", "luksChangeKey",
        "--key-file", oldPath,
        "--key-slot", fmt.Sprintf("%d", slot),
        device,
        newPath,
    )
}
```

#### 6.6.1 Crash Recovery for Partial Rotation

If `piccolod` crashes during password rotation, a `luks-rotation-progress.json` file will exist on next boot. Recovery is triggered during Phase 2 (after the user provides the new password to unlock):

```go
func (m *StorageManager) resumeRotationIfNeeded(ctx context.Context, currentPass string) error {
    progressPath := paths.CoreJoin("crypto", "luks-rotation-progress.json")
    progress, err := readJSON[RotationProgress](progressPath)
    if errors.Is(err, os.ErrNotExist) {
        return nil  // No rotation in progress
    }
    if err != nil {
        return fmt.Errorf("failed to read rotation progress: %w", err)
    }

    m.logger.Warn("resuming interrupted LUKS keyslot rotation",
        "completed", len(progress.Completed), "total", progress.Total)

    devices, _ := m.listDataPoolDevices(ctx)
    completed := toSet(progress.Completed)

    for _, dev := range devices {
        if completed[dev.UUID] {
            continue  // Already rotated
        }

        // The device may have the old or new passphrase. Try both:
        // - Derive "new" from currentPass (the password the user just provided)
        // - Try luksChangeKey with currentPass-derived as both old and new (no-op if already rotated)
        // - If that fails, the device still has an older passphrase — try common
        //   recovery strategies (the pool keyfile via keyslot 0 is always available
        //   since it is unaffected by password rotation).
        params, _ := readJSON[LUKSKDFParams](kdfParamsPath(dev.UUID))
        newRecovery := DeriveRecoveryPassphrase(currentPass, params)
        err := m.changeLUKSKeyslot(ctx, dev.Path, 1, newRecovery, newRecovery)
        if err != nil {
            // Keyslot 1 still has the old passphrase — we don't have the old password,
            // but we can kill and re-add the keyslot using the pool keyfile (keyslot 0).
            m.logger.Warn("re-creating keyslot 1 via pool keyfile", "device", dev.UUID)
            if err := m.rekeySlotViaPoolKeyfile(ctx, dev.Path, 1, newRecovery); err != nil {
                return fmt.Errorf("failed to recover keyslot 1 for %s: %w", dev.UUID, err)
            }
        }

        progress.Completed = append(progress.Completed, dev.UUID)
        _ = writeJSON(progressPath, progress)
    }

    os.Remove(progressPath)
    m.logger.Info("LUKS keyslot rotation recovery complete")
    return nil
}
```

**Note:** `resumeRotationIfNeeded` is called during Phase 2 unlock, after the user has provided the (current/new) admin password and the pool keyfile is available in memory. The pool keyfile (keyslot 0) serves as the stable "escape hatch" for re-creating any recovery keyslot.

```go
// rekeySlotViaPoolKeyfile replaces a keyslot using the pool keyfile (keyslot 0)
// as the existing key. Used during crash recovery when the old passphrase is
// unknown — the pool keyfile is always available after unlock.
func (m *StorageManager) rekeySlotViaPoolKeyfile(ctx context.Context, device string, slot int, newPass []byte) error {
    keyfilePath := "/run/piccolo/piccolo_data_pool_key"
    // Pool keyfile is already materialized in tmpfs during Phase 2 unlock.

    // Remove old keyslot
    if err := m.runner.Run(ctx, "cryptsetup", "luksKillSlot",
        "--key-file", keyfilePath,
        "--batch-mode",
        device,
        fmt.Sprintf("%d", slot),
    ); err != nil {
        return fmt.Errorf("failed to kill keyslot %d: %w", slot, err)
    }

    // Re-add with new passphrase
    if err := m.addKeyslot(ctx, device, keyfilePath, newPass, slot); err != nil {
        return fmt.Errorf("failed to re-add keyslot %d: %w", slot, err)
    }

    return nil
}
```

### 6.7 LUKS Header Backup and Recovery

The LUKS header contains critical metadata (keyslots, encryption parameters). If corrupted, data is unrecoverable. We backup headers to the control plane for disaster recovery.

```go
// Stored at: /piccolo-core/crypto/luks-header-backups/<device-uuid>.bin
// Header backups live under crypto/ alongside other device-local key material.
// This path is always writable (outside gocryptfs) and not replicated to peers.
func (m *StorageManager) backupLUKSHeader(ctx context.Context, device, deviceUUID string) error {
    backupDir := "/piccolo-core/crypto/luks-header-backups"
    if err := os.MkdirAll(backupDir, 0700); err != nil {
        return err
    }

    backupPath := filepath.Join(backupDir, deviceUUID+".bin")
    if err := m.runner.Run(ctx, "cryptsetup", "luksHeaderBackup",
        device,
        "--header-backup-file", backupPath,
    ); err != nil {
        return fmt.Errorf("header backup failed: %w", err)
    }

    m.logger.Info("LUKS header backed up", "device", device, "backup", backupPath)
    return nil
}

// Recovery: restore header then unlock with admin password
func (m *StorageManager) restoreLUKSHeader(ctx context.Context, device, deviceUUID string) error {
    backupPath := filepath.Join("/piccolo-core/crypto/luks-header-backups", deviceUUID+".bin")

    if err := m.runner.Run(ctx, "cryptsetup", "luksHeaderRestore",
        device,
        "--header-backup-file", backupPath,
        "--batch-mode",
    ); err != nil {
        return fmt.Errorf("header restore failed: %w", err)
    }

    return nil
}
```

**Recovery Model:**
1. If LUKS header corrupts but control plane is intact → restore header from backup → unlock with pool keyfile (keyslot 0)
2. If control plane is unavailable but LUKS header intact → unlock with admin password via keyslot 1, or recovery mnemonic via keyslot 2
3. If admin password is lost → recovery mnemonic unlocks control plane (via `crypt.Manager`) to get pool keyfile for keyslot 0, or directly via keyslot 2
4. If both header AND control plane are lost → data is unrecoverable (by design — encryption works)

**Header Backup Updates:**
- Initial backup after `luksFormat`
- Re-backup after any keyslot changes (password rotation)

## 7. Directory Structure

### 7.1 `/piccolo-core` (Btrfs Subvolume on Root)

The control plane is a gocryptfs-encrypted volume that follows the **same mount contract** as all other volumes (architecture doc §13): its mountpoint lives inside `/piccolo-core/mounts/`, is immutable (`chattr +i`, mode `0555`) when unmounted, and is only writable when the gocryptfs FUSE mount is active.

Key material and volume metadata required for the unlock chain must live **outside** the gocryptfs mount (always readable). See Foundation RFC §8.1 for the full unlock chain.

**Unlock chain:**
1. `crypto/keyset.json` (always readable) → admin password + Argon2id → KEK → unseal SDEK
2. SDEK → unwrap gocryptfs passphrase from `volumes/control-plane/piccolo.volume.json` → mount gocryptfs at `mounts/control-plane/`
3. SDEK → unwrap `crypto/piccolo_data_pool_key.enc` → unlock LUKS `/piccolo-data`

```
/piccolo-core/
├── crypto/                   # key material (OUTSIDE gocryptfs, always readable)
│   ├── keyset.json           # SDEK sealed with KEK (needed to start unlock)
│   ├── piccolo_data_pool_key.enc  # LUKS pool keyfile wrapped with SDEK
│   ├── luks-kdf-params/      # Argon2 derivation params per LUKS device (node-scoped, in PCV)
│   │   └── <device-uuid>.json
│   └── luks-header-backups/  # LUKS header backups (device-specific, NOT in PCV)
│       └── <device-uuid>.bin
├── ciphertext/
│   └── control-plane/        # btrfs subvolume: gocryptfs ciphertext (durable encrypted payload; PCV export source)
│       ├── gocryptfs.conf    # gocryptfs master key (encrypted with volume passphrase)
│       └── ...               # encrypted file data
├── volumes/
│   └── control-plane/
│       └── piccolo.volume.json  # volume metadata incl. wrapped gocryptfs passphrase
├── mounts/                   # volume mountpoints (immutable when unmounted)
│   ├── control-plane/        # gocryptfs plaintext view (control.db + CP state)
│   └── <vol-id>/             # app volume mountpoints
├── recovery/                 # PCV exports (portable, replicated to peers)
│   ├── current.enc
│   ├── current.json
│   ├── history/
│   └── staging/
├── network-bootstrap/        # pre-unlock remote/bootstrap state (TPM-sealed later)
└── clusterdb/etcd/           # etcd data (future, cluster mode)
```

**Directory classification:**
- **Portable (replicated):** `recovery/` — PCV exports for peer replication and orchestrator backup.
- **Device-local (not replicated as live state):** `crypto/`, `network-bootstrap/`, `ciphertext/`, `volumes/`. Note: some `crypto/` contents (keyset, pool keyfile, KDF params) are included in PCV exports as node-scoped data for restore workflows; LUKS header backups are excluded.

**Note:** “Device-local” here means these directories are not replicated *as live state*. The portable PCV export under `recovery/` is the mechanism for portability/replication.

### 7.2 `/piccolo-data` (LUKS2 + Btrfs Partition)

```
/piccolo-data/
├── node/                     # runtime scratch, caches (NOCOW)
├── user/
│   └── volumes/
│       └── <vol-id>/
│           ├── meta/         # per-volume metadata (NOCOW)
│           ├── objects/      # ciphertext object store
│           └── cache/        # disposable cache (NOCOW)
├── federation/               # PSFN shards (NOCOW)
└── system-objects/
    ├── control-plane-backups/
    └── volume-checkpoints/
```

## 8. NOCOW Attributes

Set `chattr +C` **before files are created**:

```go
func (d *DiskPreparer) SetNOCOWAttributes(ctx context.Context) error {
    nocowDirs := []string{
        "/piccolo-data/node",
        "/piccolo-data/federation",
        // Per-volume dirs set when volume is created
    }

    for _, dir := range nocowDirs {
        if err := os.MkdirAll(dir, 0700); err != nil {
            return err
        }
        if err := d.runner.Run(ctx, "chattr", "+C", dir); err != nil {
            d.logger.Warn("failed to set NOCOW", "path", dir, "error", err)
        }
    }
    return nil
}
```

## 9. USB Boot: Onboarding Flow

### 9.1 Onboarding State Machine

```go
type OnboardingState string

const (
    OnboardingPending     OnboardingState = "pending"      // Waiting for user choice
    OnboardingInstallDisk OnboardingState = "install_disk" // User chose install
    OnboardingTryPiccolo     OnboardingState = "try_piccolo"     // User chose try live
    OnboardingComplete    OnboardingState = "complete"     // Setup complete
)

// Stored in /piccolo-core/network-bootstrap/onboarding.json
type OnboardingConfig struct {
    BootMode    BootMode        `json:"boot_mode"`
    State       OnboardingState `json:"state"`
    UserChoice  string          `json:"user_choice,omitempty"`
    CompletedAt *time.Time      `json:"completed_at,omitempty"`
}
```

### 9.2 API for Onboarding

```go
// GET /api/v1/system/onboarding
type OnboardingStatus struct {
    Required   bool            `json:"required"`
    BootMode   BootMode        `json:"boot_mode"`
    State      OnboardingState `json:"state"`
    Options    []string        `json:"options,omitempty"` // dynamic: see below
}

// POST /api/v1/system/onboarding
type OnboardingChoice struct {
    Choice string `json:"choice"` // "install_disk" or "try_piccolo"
}

// buildOnboardingOptions determines available options based on hardware.
// "Install to Disk" is only offered when a non-boot internal disk is detected.
// On devices with no internal disk (e.g., Raspberry Pi booting from SD/USB),
// only "Try Piccolo" is available.
func buildOnboardingOptions(ctx context.Context, bootDisk string) []string {
    internalDisks := discoverInternalDisks(ctx, bootDisk)
    if len(internalDisks) > 0 {
        return []string{"install_disk", "try_piccolo"}
    }
    return []string{"try_piccolo"}
}

// discoverInternalDisks finds non-USB block devices that are not the boot disk.
func discoverInternalDisks(ctx context.Context, excludeDisk string) []string {
    // lsblk -ndo NAME,TRAN,TYPE — filter for type "disk", TRAN != "usb", NAME != excludeDisk
    // ...
}
```

### 9.3 "Try Piccolo" Flow (Evaluation-Only)

> **Note:** "Try Piccolo" runs the full storage posture on the boot USB. This is an evaluation-only mode — see §2.5 for contract implications.

When user selects "Try Piccolo":

1. Mark onboarding state as `try_piccolo`
2. Run full disk prep on USB drive
3. Continue with normal admin password setup
4. Mark onboarding as `complete`

**USB Partitioning Safety:**

The "Try Piccolo" flow partitions the active boot USB device. This is safe because:

1. **Data partition created first**: `/piccolo-data` is created at the 20GB offset before root expansion, which bounds `growpart` (see Section 5.2).

2. **GPT table update is atomic**: `sgdisk` writes the new partition table atomically. The root filesystem content remains intact.

3. **Kernel partition re-read**: After `partprobe`, the kernel re-reads the partition table. This works reliably for adding partitions and expanding existing ones - no reboot required.

4. **Root expansion is safe**: `growpart` only extends the partition boundary into unallocated space (doesn't touch filesystem content). `btrfs filesystem resize` is an online operation that extends the filesystem to use the new partition space. Both are standard operations that work safely on mounted filesystems.

5. **Small USB handling**: On small USB drives (< 25GB), Section 5.4 proportional sizing applies - root gets 70%, data gets 30%, ensuring both partitions fit.

```go
// Safety checks before USB partitioning
func (p *Preparer) ValidateUSBPartitioning(ctx context.Context, disk string) error {
    // 1. Verify we're only adding partitions (not modifying existing)
    existingParts, _ := p.listPartitions(ctx, disk)
    if len(existingParts) < 2 {
        return fmt.Errorf("unexpected partition layout: expected at least ESP + root")
    }

    // 2. Verify there's unallocated space at the end
    unallocated, _ := p.calculateUnallocatedSpace(ctx, disk)
    if unallocated < MinDataPartitionGB*1024*1024*1024 {
        return fmt.Errorf("insufficient unallocated space on USB: %d bytes", unallocated)
    }

    return nil
}
```

### 9.4 "Install to Disk" Flow (v1, phased)

Install to Disk is a **v1 requirement** (see product acceptance criteria in `org-context/02_product/acceptance_features/install_to_disk_x86.feature`). This RFC does not fully specify UI/UX, but it defines the storage posture contracts the installer must satisfy.

**Core contract (always true):**
- Target disk ends up in the production two-root posture:
  - `/piccolo-core` on internal root btrfs (fixed)
  - `/piccolo-data` on an internal LUKS2 + btrfs partition (expandable pool)
- After reboot, the installed system must boot from the internal disk and continue with normal setup/unlock flows.

**Phased implementation plan (all within v1 scope):**
1. **Fresh start**
   - Wipe + install to internal disk.
   - Reboot into the installed system and run first-run setup.
2. **Carry over current state**
   - Copy the live system state (admin, apps, configuration) from USB to the internal install target.
   - Reboot and resume with the migrated state.
3. **Dry-run simulation**
   - Compute and present the full plan (target disk, partitions/filesystems, estimated time, and what will be erased) without writing to disk.

**Implementation note (expected approach):**
- Use `btrfs send | btrfs receive` to sync the live root filesystem to the target disk (matching the PRD), then apply the same disk-prep posture on the target disk (root sizing + `/piccolo-data` creation + LUKS2 init).

**Failure and retry:** Install to Disk writes to the internal disk only — it does not modify the boot USB. If the install fails (power loss, I/O error), the USB boot environment remains fully functional. On retry, the installer must detect partial writes on the target disk (e.g., GPT present but no valid btrfs, or btrfs present but incomplete snapshot) and offer to wipe and restart rather than attempting to resume from an unknown state.

## 10. Component Changes

### 10.1 New Package: `internal/storage`

```go
package storage

type Manager struct {
    crypto      *crypt.Manager
    diskPrep    *diskprep.Preparer
    luks        *luks.PoolManager
    logger      *slog.Logger
    state       *DiskState
    emergency   bool          // True if in emergency mode
    emergencyErr error        // Error that caused emergency mode
}

// Phase 1: Boot-time operations (runs in background after server starts)
func (m *Manager) DetectBootMode(ctx context.Context) (BootMode, error)
func (m *Manager) GetDiskState(ctx context.Context) (*DiskState, error)
func (m *Manager) StartPartitioningAsync(ctx context.Context)     // Phase 1: launches background goroutine
func (m *Manager) WaitForPhase1(ctx context.Context) error        // Blocks until Phase 1 completes
func (m *Manager) IsPhase1Complete() bool                         // Non-blocking check
func (m *Manager) IsEmergencyMode() bool
func (m *Manager) EmergencyError() error

// Phase 2: Post-auth operations (called from API handlers)
func (m *Manager) InitializeDataVolume(ctx context.Context, adminPassword string) error  // Phase 2: LUKS + mount
func (m *Manager) Unlock(ctx context.Context, adminPassword string) error  // Subsequent boots
func (m *Manager) Lock(ctx context.Context) error

// Lifecycle hooks
func (m *Manager) OnAdminPasswordRotated(ctx context.Context, oldPass, newPass string) error
func (m *Manager) OnRecoveryMnemonicRotated(ctx context.Context) error  // Uses crypt.Manager.WithMnemonicKey callback
```

### 10.2 New Package: `internal/storage/diskprep`

**`CommandRunner` interface (shared across storage packages):**

All CLI operations (`sgdisk`, `cryptsetup`, `btrfs`, etc.) are routed through a `CommandRunner` interface to enable unit testing with mock commands.

**Consolidation note:** The existing `commandRunner` in `internal/persistence/file_volume_manager.go` has signature `Run(ctx, name, []string, []byte)` (args as slice, optional stdin). This new interface uses variadic args and splits stdin into a separate method (`RunWithStdin`). During implementation, the legacy interface should be migrated to the new one so both storage and persistence share a single `CommandRunner` definition (likely in a shared `internal/exec` or `internal/runner` package).

**`secureZero` consolidation:** The `secureZero` helper in §6.3 duplicates `zeroBytes` in `internal/crypt/manager.go`. During implementation, consolidate into a single shared utility (e.g., `internal/crypto/zero.go`) used by both packages.

```go
// CommandRunner abstracts CLI execution for testability.
// Production: wraps exec.CommandContext.
// Tests: returns canned outputs / errors per command.
type CommandRunner interface {
    // Run executes a command with the given arguments and optional stdin.
    // Returns the combined stdout/stderr output and any error.
    Run(ctx context.Context, name string, args ...string) error

    // RunWithOutput executes a command and returns stdout.
    RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error)

    // RunWithStdin executes a command with stdin data (e.g., keyfile piping).
    RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error
}
```

```go
package diskprep

type Preparer struct {
    runner CommandRunner
    logger *slog.Logger
}

func (p *Preparer) ExpandRootPartition(ctx context.Context) error
func (p *Preparer) VerifyPiccoloCoreExists(ctx context.Context) error  // Fatal if missing
func (p *Preparer) CreateDataPartition(ctx context.Context) error      // Detects free slot
func (p *Preparer) EnsureDirectories(ctx context.Context) error
func (p *Preparer) SetNOCOWAttributes(ctx context.Context) error

// Partition detection helpers
func (p *Preparer) findNextPartitionSlot(ctx context.Context, disk string) (int, error)
func (p *Preparer) getLastPartitionEnd(ctx context.Context, disk string) (int64, error)
```

### 10.3 New Package: `internal/storage/luks`

```go
package luks

type PoolManager struct {
    runner  CommandRunner
    keyfile []byte
    logger  *slog.Logger
}

func (p *PoolManager) Initialize(ctx context.Context, device, adminPassword string) error
func (p *PoolManager) Open(ctx context.Context) error
func (p *PoolManager) Close(ctx context.Context) error
func (p *PoolManager) AddDevice(ctx context.Context, device, adminPassword string) error
func (p *PoolManager) UpdateRecoveryKeyslot(ctx context.Context, oldPass, newPass string) error
```

### 10.4 Modified: `internal/server/gin_server.go`

```go
func NewGinServer() (*GinServer, error) {
    storageMgr := storage.NewManager(...)

    // Detect boot mode
    bootMode, _ := storageMgr.DetectBootMode(ctx)

    // Check disk state
    diskState, _ := storageMgr.GetDiskState(ctx)

    if (bootMode == storage.BootModeUSB || bootMode == storage.BootModeUnknown) && !diskState.SetupComplete {
        // USB or unknown boot, first time — need onboarding.
        // BootModeUnknown follows the same flow as USB: show the onboarding UI
        // and require explicit user confirmation before any partition writes.
        // Register onboarding endpoints, wait for user choice.
        // Do NOT start disk prep until user chooses "Try Piccolo"
        // ("Install to Disk" targets internal disk, not the boot device)
    }

    // PHASE 1: Boot-time partitioning (background, non-blocking)
    // The server starts immediately — the portal shows "Preparing storage..."
    // while disk prep runs in a background goroutine.
    // IMPORTANT: For USB first-boot, disk prep is deferred until after the user
    // selects "Try Piccolo" via POST /api/v1/system/onboarding. The onboarding
    // handler calls StartPartitioningAsync after recording the user's choice.
    if needsDiskPrep(diskState) && !pendingUSBOnboarding {
        storageMgr.StartPartitioningAsync(ctx) // sets storageMgr.phase1Done channel on completion
    }

    // Server starts here - portal becomes available immediately
    // Portal shows "Preparing storage..." until Phase 1 completes
    // ...

    // PHASE 2 happens later, triggered by API:
    // - POST /api/v1/crypto/setup (first boot) → storageMgr.InitializeDataVolume(ctx, password)
    // - POST /api/v1/crypto/unlock (subsequent) → storageMgr.Unlock(ctx, password)
    // Phase 2 handlers block on Phase 1 completion before proceeding.
}

// In crypto handlers:
func (s *GinServer) handleCryptoSetup(c *gin.Context) {
    // ... validate password ...

    // Initialize control plane encryption (existing)
    if err := s.crypto.Setup(ctx, password); err != nil {
        // handle error
    }

    // Generate recovery mnemonic at setup time so all three LUKS keyslots
    // can be enrolled during InitializeDataVolume. The mnemonic words are
    // returned to the user in the setup response for safekeeping.
    //
    // NOTE: Raw mnemonic key material must NOT cross API handler boundaries.
    // Instead of returning raw bytes, crypt.Manager exposes a callback-based
    // API matching the existing WithSDEK pattern: the crypto module controls
    // the key's lifetime and zeroing.
    mnemonicWords, err := s.crypto.GenerateRecoveryKey(false)
    if err != nil {
        // handle error
    }

    // PHASE 2: Initialize /piccolo-data (LUKS + btrfs + mount)
    // Mnemonic keyslot enrollment uses crypt.Manager.WithMnemonicKey() callback
    // so raw key material stays inside the crypto module scope.
    if err := s.storageMgr.InitializeDataVolume(ctx, password); err != nil {
        // handle error - could be retried
    }

    // Return mnemonicWords in response for user to save
}

func (s *GinServer) handleCryptoUnlock(c *gin.Context) {
    // ... validate password ...

    // Unlock control plane (existing)
    if err := s.crypto.Unlock(ctx, password); err != nil {
        // handle error
    }

    // PHASE 2: Unlock and mount /piccolo-data
    if err := s.storageMgr.Unlock(ctx, password); err != nil {
        // handle error
    }
}
```

## 11. State Detection Logic

### 11.1 Detecting Partition State

```go
func (p *Preparer) GetPartitionState(ctx context.Context) (*PartitionState, error) {
    rootDev, _ := getRootDevice(ctx)
    disk := getParentDisk(rootDev)

    // Parse partition table using sfdisk JSON output
    partitions, _ := p.listPartitions(ctx, disk)

    state := &PartitionState{
        Disk:           disk,
        RootPartition:  findPartition(partitions, "root"),
        UnallocatedGB:  calculateUnallocated(disk, partitions),
    }

    // Check root size using the same sizing rules as CreateDataPartition (§5.4).
    // Using calculatePartitionLayout instead of the fixed RootTargetSizeGB constant
    // ensures consistency on small disks where root gets a proportional 70% share.
    diskSizeGB, err := p.getDiskSizeGB(ctx, disk)
    if err != nil {
        return nil, fmt.Errorf("failed to get disk size: %w", err)
    }
    layout, err := calculatePartitionLayout(diskSizeGB)
    if err != nil {
        return nil, fmt.Errorf("failed to calculate partition layout: %w", err)
    }
    state.RootNeedsExpansion = state.RootPartition.SizeGB < layout.RootGB

    // Find data partition by LUKS type code (8309) or label
    state.DataPartition = findPartitionByTypeCode(partitions, "8309")
    if state.DataPartition == nil {
        // Also check by filesystem detection (existing LUKS header)
        state.DataPartition = findLUKSPartition(ctx, disk, partitions)
    }

    state.DataPartitionExists = state.DataPartition != nil
    if state.DataPartitionExists {
        state.DataPartitionSlot = state.DataPartition.Slot
        state.DataPartitionLUKS, _ = isLUKS(ctx, state.DataPartition.Device)
    }

    return state, nil
}

func isLUKS(ctx context.Context, device string) (bool, error) {
    err := exec.CommandContext(ctx, "cryptsetup", "isLuks", device).Run()
    if err == nil {
        return true, nil  // Is LUKS
    }

    // Distinguish "not LUKS" (exit code 1) from execution failures
    var exitErr *exec.ExitError
    if errors.As(err, &exitErr) {
        // cryptsetup isLuks returns exit code 1 for non-LUKS devices
        if exitErr.ExitCode() == 1 {
            return false, nil  // Not LUKS, but not an error
        }
    }

    // Actual execution failure (missing binary, permission denied, I/O error)
    return false, fmt.Errorf("cryptsetup isLuks failed: %w", err)
}
```

### 11.2 Verifying /piccolo-core Subvolume

The `/piccolo-core` subvolume is created by the OS image (KIWI `<volume name="piccolo-core" />`).
Piccolod verifies it exists; if missing, enters Emergency Mode (see Section 12.3).

```go
func (p *Preparer) VerifyPiccoloCoreExists(ctx context.Context) error {
    coreRoot := paths.CoreRoot()

    // Check if /piccolo-core is a btrfs subvolume
    if err := p.runner.Run(ctx, "btrfs", "subvolume", "show", coreRoot); err != nil {
        return fmt.Errorf("%s subvolume missing - OS image is broken", coreRoot)
    }

    // Verify it's mounted and writable
    if err := unix.Access(coreRoot, unix.W_OK); err != nil {
        return fmt.Errorf("%s is not writable: %w", coreRoot, err)
    }

    return nil
}
```

## 12. Error Handling and Rollback

### 12.1 Partition Creation Failures

```go
func (p *Preparer) CreateDataPartition(ctx context.Context) error {
    // This is not easily rollback-able
    // If partition creation fails mid-way, system may be in inconsistent state

    // Strategy: Validate heavily before starting
    state, err := p.GetPartitionState(ctx)
    if err != nil {
        return err
    }

    if state.UnallocatedGB < MinDataPartitionGB {
        return fmt.Errorf("insufficient space: need %dGB, have %dGB",
            MinDataPartitionGB, state.UnallocatedGB)
    }

    // Create partition
    if err := p.runner.Run(ctx, "sgdisk", ...); err != nil {
        return fmt.Errorf("partition creation failed: %w (manual intervention may be required)", err)
    }

    return nil
}
```

### 12.2 LUKS Initialization Failures

The canonical `InitializeLUKS` in §6.3 is the authoritative implementation. Key error handling properties:

1. **`luksFormat` is atomic** — either succeeds or the device is unchanged. If it fails, the caller can retry safely.
2. **Keyfile is persisted first** (before recovery keyslots) — this is the primary unlock path and has the highest success probability.
3. **"At least one persistent unlock path" invariant** — if the pool keyfile, admin recovery keyslot, and mnemonic recovery keyslot all fail to persist, the function skips ephemeral keyfile cleanup and returns an error. This is a **reflash scenario**: the user must re-flash the OS image and start fresh. No user data has been written at this point (the LUKS device was just formatted). The ephemeral keyfile remains in tmpfs for the current boot session's diagnostics only.
4. **Partial keyslot failure is non-fatal** — if only one or two of the three unlock paths succeed, the system continues with degraded recovery options and logs warnings.

### 12.3 Emergency Mode

If Phase 1 (boot-time partitioning) fails, the system enters Emergency Mode instead of crash-looping. Emergency mode is **differentiated** based on the failure cause and whether the system was previously set up:

```go
type EmergencyLevel string

const (
    // EmergencyHard: fatal — crypto endpoints blocked, system cannot operate.
    // Caused by: missing /piccolo-core subvolume, or first-boot partition failure.
    EmergencyHard EmergencyLevel = "hard"

    // EmergencySoft: degraded — crypto endpoints allowed, system may still unlock.
    // Caused by: transient partition/expansion failure on a previously-set-up device.
    EmergencySoft EmergencyLevel = "soft"
)

type EmergencyState struct {
    Active  bool           `json:"active"`
    Level   EmergencyLevel `json:"level"`
    Reason  string         `json:"reason"`
    Details string         `json:"details"`
}

const phase1MaxRetries = 3
const phase1RetryBackoff = 2 * time.Second

func (m *Manager) PreparePartitioning(ctx context.Context) error {
    // Phase 1 timeout: disk prep should complete in seconds on healthy hardware.
    // A 5-minute timeout catches hung I/O (dead disk, stuck partition table lock)
    // rather than blocking the phase1Done channel indefinitely while the user
    // waits on the portal.
    ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
    defer cancel()

    // 1. Verify /piccolo-core exists — no retries (deterministic: image is broken or not)
    if err := m.diskPrep.VerifyPiccoloCoreExists(ctx); err != nil {
        m.enterEmergencyMode(EmergencyHard, err, "OS image is broken: /piccolo-core subvolume missing")
        return err
    }

    // Determine if the system was previously set up (onboarding completed).
    // This controls whether partition failures are hard (first boot — can't proceed)
    // or soft (subsequent boot — may still unlock existing storage).
    previouslySetUp := m.isOnboardingComplete()

    // 2. Create data partition FIRST (bounds growpart) — retry up to 3 times
    if err := m.retryPhase1Op(ctx, "CreateDataPartition", m.diskPrep.CreateDataPartition); err != nil {
        level := EmergencyHard
        if previouslySetUp {
            level = EmergencySoft // Existing LUKS may still be unlockable
        }
        m.enterEmergencyMode(level, err, "Failed to create /piccolo-data partition")
        return err
    }

    // 3. Expand root partition (bounded by data partition created above) — retry up to 3 times
    if err := m.retryPhase1Op(ctx, "ExpandRootPartition", m.diskPrep.ExpandRootPartition); err != nil {
        // Root expansion failure is always soft — system can function with smaller root
        m.enterEmergencyMode(EmergencySoft, err, "Failed to expand root partition")
        return err
    }

    return nil
}

// retryPhase1Op retries a Phase 1 operation up to phase1MaxRetries times with
// backoff. Transient failures (flaky USB probe, busy partition table lock) often
// succeed on retry.
func (m *Manager) retryPhase1Op(ctx context.Context, name string, op func(context.Context) error) error {
    var lastErr error
    for attempt := 1; attempt <= phase1MaxRetries; attempt++ {
        lastErr = op(ctx)
        if lastErr == nil {
            return nil
        }
        if attempt < phase1MaxRetries {
            m.logger.Warn("phase 1 operation failed, retrying",
                "op", name, "attempt", attempt, "error", lastErr)
            select {
            case <-time.After(phase1RetryBackoff):
            case <-ctx.Done():
                return ctx.Err()
            }
        }
    }
    return fmt.Errorf("%s failed after %d attempts: %w", name, phase1MaxRetries, lastErr)
}

func (m *Manager) enterEmergencyMode(level EmergencyLevel, err error, reason string) {
    m.emergency = true
    m.emergencyLevel = level
    m.emergencyErr = err
    m.emergencyReason = reason
    m.logger.Error("entering emergency mode", "level", level, "reason", reason, "error", err)
}

// isOnboardingComplete checks whether the system was previously set up
// by reading the persisted onboarding state in /piccolo-core/network-bootstrap/onboarding.json.
func (m *Manager) isOnboardingComplete() bool {
    cfg, err := readJSON[OnboardingConfig](paths.CoreJoin("network-bootstrap", "onboarding.json"))
    if err != nil {
        return false // No onboarding file = never set up
    }
    return cfg.State == OnboardingComplete
}
```

**Emergency Mode Behavior:**

1. **Server still starts** - HTTP server becomes available
2. **Portal shows error state** - User sees degraded status with details
3. **Endpoint availability depends on emergency level** (see below)
4. **No crash loop** - System stays up for debugging

**Endpoint availability by system state:**

Pre-unlock, hard emergency, and soft emergency are distinct states with different endpoint allowlists.

**Pre-unlock (normal — before admin password):**

| Endpoint | Purpose |
|---|---|
| `/` (portal shell) | Serves the Flutter Web UI (locked/onboarding states) |
| `/api/v1/system/health` | Health check for load balancers and monitoring |
| `/api/v1/system/onboarding` | USB boot onboarding flow |
| `/api/v1/crypto/setup` | First-run admin password setup |
| `/api/v1/crypto/unlock` | Unlock control plane + `/piccolo-data` |

**Hard emergency (fatal storage failure — first boot or missing `/piccolo-core`):**

| Endpoint | Purpose |
|---|---|
| `/` (portal shell) | Shows "Storage Initialization Failed" error UI |
| `/api/v1/system/health` | Health check (reports degraded) |
| `/api/v1/system/emergency` | Emergency mode diagnostics and recovery actions |

Crypto endpoints are **blocked in hard emergency** because disk prep must succeed on first boot before LUKS initialization or unlock can proceed.

**Soft emergency (transient failure — previously set up device):**

| Endpoint | Purpose |
|---|---|
| `/` (portal shell) | Shows degraded storage warning with unlock available |
| `/api/v1/system/health` | Health check (reports degraded) |
| `/api/v1/system/emergency` | Emergency mode diagnostics and recovery actions |
| `/api/v1/crypto/unlock` | Unlock control plane + existing `/piccolo-data` |

Crypto **unlock** is **allowed in soft emergency** because the system was previously set up and the existing LUKS device may still be unlockable. Crypto **setup** is blocked (setup requires successful disk prep). The portal prominently warns about the storage degradation while allowing the user to unlock and access their data.

**Note:** Device discovery (mDNS/`piccolo.local`) is handled at the OS level by Avahi, not by piccolod endpoints.

```go
// Emergency mode middleware — blocks endpoints based on emergency level.
func (s *GinServer) emergencyModeMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        if !s.storageMgr.IsEmergencyMode() {
            c.Next()
            return
        }

        path := c.Request.URL.Path

        // Allow API diagnostics endpoints (all emergency levels)
        if strings.HasPrefix(path, "/api/v1/system/health") ||
            strings.HasPrefix(path, "/api/v1/system/emergency") {
            c.Next()
            return
        }

        // Allow portal static assets (non-API paths serve the Flutter Web UI)
        if !strings.HasPrefix(path, "/api/") {
            c.Next()
            return
        }

        // Soft emergency: allow crypto/unlock (existing storage may be unlockable)
        if s.storageMgr.EmergencyLevel() == EmergencySoft {
            if strings.HasPrefix(path, "/api/v1/crypto/unlock") {
                c.Next()
                return
            }
        }

        // Block all other API endpoints
        c.JSON(http.StatusServiceUnavailable, gin.H{
            "error":   "storage_emergency",
            "level":   string(s.storageMgr.EmergencyLevel()),
            "message": s.storageMgr.EmergencyError().Error(),
        })
        c.Abort()
    }
}

// GET /api/v1/system/emergency
type EmergencyStatus struct {
    Active  bool           `json:"active"`
    Level   EmergencyLevel `json:"level"`   // "hard" or "soft"
    Reason  string         `json:"reason"`
    Error   string         `json:"error"`
    Actions []string       `json:"actions"` // Suggested recovery actions
}
```

## 13. Testing Strategy

### 13.1 Unit Tests

- Mock command runner for all CLI operations
- Test partition state detection logic
- Test LUKS keyslot derivation

### 13.2 Integration Tests

Using loopback devices in privileged container:

```go
func TestDiskPrepFullSequence(t *testing.T) {
    // Create 30GB loopback device
    // Run full disk prep
    // Verify partition layout
    // Verify LUKS setup
    // Verify btrfs mount
}
```

### 13.3 VM Testing

On fresh piccolo-os image:

1. Boot from internal - verify disk prep
2. Boot from USB - verify onboarding flow
3. Select "Try Piccolo" - verify USB gets partitioned
4. Reboot - verify state persists

## 14. Audit Events

Key storage operations should emit events to the audit log (via `internal/events/`):
- `storage.phase1.completed` — Phase 1 disk prep finished (partitioning + root expansion)
- `storage.phase1.emergency` — Phase 1 entered emergency mode (includes reason)
- `storage.luks.initialized` — LUKS2 device formatted and keyslots enrolled
- `storage.luks.unlocked` / `storage.luks.locked` — data volume unlock/lock
- `storage.luks.keyslot_rotated` — password or mnemonic keyslot updated (slot number, device UUID)
- `storage.data.mounted` / `storage.data.unmounted` — data volume mount lifecycle

## 15. Security Considerations

- Pool keyfile only in memory after unlock
- Keyfile encrypted with SDEK in control plane
- Recovery keyslots use Argon2id with random per-device salts (persisted in `luks-kdf-params/`)
- LUKS2 provides at-rest encryption (AES-XTS-plain64 by default)
  - Note: Default mode provides confidentiality only, not integrity/authentication
  - Integrity protection would require dm-integrity or authenticated modes (future consideration)
- Temp keyfile written to tmpfs (/run), never to disk
- LUKS header backed up to control plane for disaster recovery (device-specific; excluded from PCV exports)
- KDF params and pool keyfile included in PCV exports (node-scoped data for restore workflows)

## 16. Future Work

Out of scope for this RFC:

1. **USB storage expansion management** - hotplug detection and adding/removing devices to the `/piccolo-data` pool
2. **JuiceFS integration** - per-volume filesystems
3. **Cluster mode** - etcd placement
4. **Network-bootstrap hardening** - TPM enrollment and sealing
5. **Degraded mode UI** - surfacing pool status
6. **LUKS2 authenticated encryption** - `dm-integrity` or AEAD modes for tamper detection on physically accessible devices
7. **In-place encryption of existing data** - the `storage_and_encryption.feature` "Encrypt existing data in place" scenario is not applicable to the two-root architecture: `/piccolo-data` is LUKS2-encrypted from first boot, so there is no unencrypted user data to migrate. If a future migration path from pre-v2 installs is needed, it would be handled by a one-shot importer (see Foundation RFC §9).
8. **Stolen lock / remote kill switch** - the `storage_and_encryption.feature` "Stolen lock prevents access" scenario requires orchestrator integration (a remote flag that blocks local unlock). This depends on the Piccolo Orchestrator and will be specified in the cluster/orchestrator RFC.

## 17. Required System Tools

The following CLI tools are required and must be declared as **RPM dependencies** in the piccolod spec file:

| Tool | RPM Package | Purpose |
|------|-------------|---------|
| `lsblk` | util-linux | Disk/partition discovery, transport type detection |
| `sgdisk` | gptfdisk | GPT partition table manipulation |
| `sfdisk` | util-linux | Partition boundary queries (JSON output) |
| `growpart` | growpart (or cloud-utils-growpart) | Expanding root partition |
| `partprobe` | parted | Reloading partition table |
| `partx` | util-linux | Reloading/adding partition entries when full re-read is refused |
| `cryptsetup` | cryptsetup | LUKS2 formatting, key management |
| `mkfs.btrfs` | btrfs-progs | Filesystem creation |
| `btrfs` | btrfs-progs | Subvolume operations, filesystem resize |
| `chattr` | e2fsprogs | Setting NOCOW attributes |

**RPM Spec Requirements:**
```spec
# In piccolod.spec
Requires: util-linux
Requires: gptfdisk
Requires: growpart
Requires: parted
Requires: cryptsetup
Requires: btrfs-progs
Requires: e2fsprogs
```

This ensures tools are installed as dependencies of piccolod rather than relying on base image contents.

## 18. References

- `org-context/03_engineering/storage_architecture.md`
- `piccolo-os/kiwi/microos-ots/piccolo-os.kiwi`
- `docs/rfc/20260202-storage-v2-foundation.md`
- `internal/persistence/file_volume_manager.go` (legacy persistence v1; will be replaced in v2)
- `internal/crypt/manager.go`

## Implementation Notes & Status
- 2026-02-01: Drafted. No code changes landed yet.
- 2026-02-04: Review pass. Fixed: (1) canonical InitializeLUKS with defensive "at least one unlock path" check and keyfile-first ordering; (2) added LUKS keyslot 2 for 24-word recovery mnemonic; (3) pinned LUKS cipher parameters; (4) added power failure recovery table for Phase 1; (5) fixed secureZero with runtime.KeepAlive; (6) added password rotation crash recovery logic; (7) specified luksChangeKey for atomic keyslot replacement; (8) fixed small disk error message formula; (9) "Install to Disk" hidden when no internal disk detected; (10) clarified pool key placement as outside gocryptfs, device-local.
- 2026-02-04: Parallel review fixes. Fixed: (11) `.ciphertext/` → `ciphertext/` (removed dot prefix to match existing codebase); (12) Argon2 thread count now dynamic (`max(1, runtime.NumCPU()-1)`) to align with crypt.Manager; (13) Phase 1 disk prep now runs in background after server starts (portal available immediately, shows "Preparing storage..."); (14) added §5.0 partition table preconditions; (15) added 20GB root rationale (MicroOS recommended max); (16) added pre-unlock endpoint allowlist table.
- 2026-02-06: Second parallel review fixes. Fixed: (17) pool keyfile now included in PCV exports (node-scoped data for restore workflows); updated §6.2 and §7.1 classification; (18) Argon2 derivation params + random salts now persisted per device at `crypto/luks-kdf-params/<uuid>.json` — fixes correctness risk where CPU count changes would produce different passphrases; added §6.4.1 KDF parameter persistence; (19) split endpoint allowlist into pre-unlock vs emergency mode tables — crypto endpoints blocked in emergency since disk prep must succeed first; (20) per-node room concept documented for cluster-mode PCV.
- 2026-02-06: Third review pass. Blocking fixes: (21) Argon2 thread cap at 8 for portability; (22) Argon2 memory 512 MiB to match crypt.Manager; (23) hardcoded path replaced with `paths.CoreRoot()`; (24) keyslot 0 uses pbkdf2 (high-entropy keyfile, argon2 adds no value); (25) kernel partition verification loop after partprobe; (26) mnemonic always enrolled at setup, added `OnRecoveryMnemonicRotated` hook. Significant fixes: (27) emergency middleware path matching rewritten (old `/` prefix matched all paths); (28) specified `rekeySlotViaPoolKeyfile` implementation; (29) mapper collision check before `cryptsetup open`; (30) `CommandRunner` interface note for testability; (31) no-fstab design rationale documented; (32) 32-byte salt rationale (RFC 9106 §4). Suggestions: (33) GPT partition label `piccolo-data`; (34) btrfs filesystem label; (35) audit events section added (§14).
- 2026-02-06: Cross-review validation. Fixed: (36) ephemeral keyfile cleanup is now conditional — skipped on all-three-failed path so operator can recover; (37) USB onboarding guard prevents async partitioning before user chooses "Try Piccolo"; (38) renamed "Try Live" → "Try Piccolo" throughout to match PRD and acceptance features.
- 2026-02-06: Fourth review pass (combined assessment). Fixes: (39) three-mode boot detection — added `BootModeUnknown` for ambiguous transports (virtio, iSCSI), follows USB onboarding flow; (40) `partitionDevicePath()` helper for mmcblk/nvme/loop naming; (41) 1 MiB sector alignment for data partition start; (42) LUKS all-paths-failed is now a reflash scenario (no SSH, no manual cryptsetup); (43) `reloadPartitionTable` narrowed to specific slot (`partx --nr N:N`); (44) `CommandRunner` interface definition added to §10.2; (45) gin_server pseudocode handles `BootModeUnknown`; (46) `GenerateRecoveryKey` API change noted (returns key material); (47) Install to Disk failure/retry handling documented.
- 2026-02-07: Sixth review pass (amendment decisions). Fixes: (56) `PICCOLO_BOOT_MODE_OVERRIDE` gated behind sentinel file `/etc/piccolo-test-image` — prevents misuse on production images; (57) emergency mode rewritten as differentiated hard/soft levels — soft emergency allows crypto unlock on previously-set-up devices where LUKS may still work; (58) Phase 1 retries added (3 attempts, 2s backoff) before entering emergency; (59) hardcoded paths fixed: `"/piccolo-core/crypto/luks-rotation-progress.json"` → `paths.CoreJoin(...)` in `OnAdminPasswordRotated` and `resumeRotationIfNeeded`; `"/piccolo-core"` → `paths.CoreRoot()` in `VerifyPiccoloCoreExists`; (60) future work items added: in-place encryption not applicable (two-root design), stolen lock deferred to orchestrator RFC.
- 2026-02-07: Fifth review pass (combined assessment). Fixes: (48) `GetPartitionState` root expansion check now uses `calculatePartitionLayout` instead of fixed `RootTargetSizeGB` — ensures consistency on small disks with proportional 70% split; (49) `PICCOLO_BOOT_MODE_OVERRIDE` env var for CI/QA unattended provisioning in VMs; (50) `getLUKSUUID` error handling changed from silent discard to fail-fast — UUID is the KDF salt anchor; (51) `getDiskSizeGB` uses ceiling division to avoid under-counting fractional GBs; (52) Phase 1 `PreparePartitioning` now wraps context with 5-minute timeout; (53) mnemonic rotation (`OnRecoveryMnemonicRotated`) updated to callback-based pattern matching `WithSDEK`/`WithMnemonicKey`, with crash recovery progress tracking (§6.5.1) matching password rotation pattern; (54) `CommandRunner` interface consolidation note — legacy `commandRunner` in `file_volume_manager.go` should be migrated to shared definition; (55) `secureZero`/`zeroBytes` consolidation note added to §10.2.
