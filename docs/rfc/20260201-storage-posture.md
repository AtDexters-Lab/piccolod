# RFC: Storage Posture - Two-Root Architecture with LUKS2 Encryption

- **Status:** Draft
- **Date:** 2026-02-01
- **Authors:** Engineering Team
- **Related:** `org-context/03_engineering/storage_architecture.md`

## 1. Summary

This RFC describes how piccolod will adopt the new two-root storage architecture. The OS image is now **minimal** - piccolod is responsible for all disk partitioning, filesystem creation, and encryption setup.

Piccolod must:
1. **Detect boot mode** (internal disk vs USB boot)
2. **Expand root partition** to ~20GB
3. **Verify `/piccolo-core`** btrfs subvolume exists (created by OS)
4. **Create `/piccolo-data` partition** from remaining disk space
5. **LUKS2 encrypt** `/piccolo-data` with pool keyfile
6. **Create btrfs** on `/piccolo-data`
7. **Handle USB boot** with onboarding flow (Install to Disk / Try Live)

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

### 2.4 USB Boot Scenario

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
│               └── Shows onboarding choice:                          │
│                       │                                             │
│                       ├── "Install to Disk"                         │
│                       │       └── (Future RFC: btrfs send to disk)  │
│                       │                                             │
│                       └── "Try Live"                                │
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
- **USB boot support**: Show onboarding flow, support "Try Live" mode
- **Persistent USB storage**: "Try Live" creates real partitions on USB
- **Degraded mode**: System operates if USB storage (expansion) fails

### 3.2 Non-Goals

- **"Install to Disk" implementation**: Deferred to future RFC (btrfs send)
- **Migration from existing systems**: Fresh installs only
- **JuiceFS/BadgerDB integration**: Future work
- **Cluster mode**: Future work

## 4. Boot Mode Detection

### 4.1 Detection Logic

```go
type BootMode string

const (
    BootModeInternal BootMode = "internal"  // Booted from internal disk (sata, nvme, ata)
    BootModeUSB      BootMode = "usb"       // Booted from USB drive
)

func DetectBootMode(ctx context.Context) (BootMode, error) {
    // Get root device
    rootDev, err := getRootDevice(ctx)  // e.g., /dev/sda2
    if err != nil {
        return "", err
    }

    // Get parent disk
    disk := getParentDisk(rootDev)  // e.g., /dev/sda

    // Use lsblk to get transport type (more reliable than /sys/block/*/removable)
    // TRAN values: usb, sata, nvme, ata, etc.
    transport, err := getTransportType(ctx, disk)
    if err != nil {
        return "", err
    }

    if transport == "usb" {
        return BootModeUSB, nil
    }
    return BootModeInternal, nil
}

func getTransportType(ctx context.Context, disk string) (string, error) {
    // lsblk -ndo TRAN /dev/sda → "usb" or "sata" or "nvme" etc.
    output, err := exec.CommandContext(ctx, "lsblk", "-ndo", "TRAN", disk).Output()
    if err != nil {
        return "", fmt.Errorf("failed to get transport type: %w", err)
    }
    transport := strings.TrimSpace(string(output))

    // Fallback: empty TRAN can happen with virtio (VMs) or some RAID controllers
    if transport == "" {
        // Log warning and assume internal boot (safer default)
        slog.Warn("lsblk returned empty transport type, assuming internal boot",
            "disk", disk)
        return "internal", nil  // Treat as internal, not USB
    }

    return transport, nil
}
```

**Why `lsblk -o TRAN` instead of `/sys/block/*/removable`:**

| Method | Limitation |
|--------|------------|
| `/sys/block/*/removable` | USB HDDs/SSDs report `0` (non-removable) |
| `lsblk -o TRAN` | Reliably shows `usb` for all USB-connected devices |

**Fallback behavior:** If `lsblk` returns an empty transport type (can happen with virtio in VMs or some RAID controllers), assume internal boot. This is the safer default - incorrectly treating a USB boot as internal just skips the onboarding UI, whereas incorrectly treating internal as USB would show unnecessary prompts.

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
│       └── USB BOOT ─────────────────────────────────────────────→   │
│               │                                                     │
│               └── IsFirstBoot()?                                    │
│                       ├── YES → ShowOnboardingFlow()                │
│                       │           ├── "Install to Disk" → Defer     │
│                       │           └── "Try Live" → RunDiskPrep()    │
│                       └── NO  → Continue (already set up)           │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## 5. Disk Preparation Sequence

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

#### Phase 1: Boot-time Partitioning (Before Server Starts)

Runs synchronously during `NewGinServer()`, before HTTP server is available.

```
┌─────────────────────────────────────────────────────────────────────┐
│                 PHASE 1: BOOT-TIME PARTITIONING                     │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  1. Verify /piccolo-core btrfs subvolume exists                     │
│       │                                                             │
│       ├── btrfs subvolume show /piccolo-core                        │
│       └── If missing → enter Emergency Mode (see Section 12.3)      │
│                                                                     │
│  2. Create /piccolo-data partition (at 20GB offset)                 │
│       │   (MUST happen before root expansion to bound growpart)     │
│       │                                                             │
│       ├── Detect next free partition slot (see 5.5)                 │
│       ├── Calculate: start = 20GB (in sectors), end = disk_end      │
│       ├── sgdisk -n $slot:$start:0 -t $slot:8309 /dev/sdX           │
│       └── partprobe /dev/sdX                                        │
│                                                                     │
│  3. Expand root partition (bounded by data partition)               │
│       │                                                             │
│       ├── growpart /dev/sdX 2  (expands up to data partition)       │
│       └── btrfs filesystem resize max /var                          │
│           (use /var - writable mount on same btrfs filesystem)      │
│                                                                     │
│  Result: Server starts, portal available                            │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Why this order?**
- `growpart` expands a partition to fill all contiguous free space
- By creating `/piccolo-data` at the 20GB mark first, it acts as a boundary
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
│       ├── mkfs.btrfs /dev/mapper/piccolo_data_pool_0                │
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

### 5.3 Subsequent Boot Sequence

On subsequent boots, both phases are still executed but operations are idempotent:

```
Phase 1 (Boot-time):
  1. Check disk state
  2. Verify /piccolo-core exists → Emergency Mode if missing
  3. If /piccolo-data partition missing → create at 20GB offset
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
    RootTargetSizeGB   = 20   // Target size for root partition
    MinDataPartitionGB = 5    // Minimum size for /piccolo-data
)

func calculatePartitionLayout(diskSizeGB int) (*PartitionLayout, error) {
    if diskSizeGB < RootTargetSizeGB + MinDataPartitionGB {
        // Small disk (e.g., 16GB USB) - use proportional split
        rootSize := diskSizeGB * 70 / 100  // 70% for root
        dataSize := diskSizeGB - rootSize
        return &PartitionLayout{RootGB: rootSize, DataGB: dataSize}, nil
    }

    // Normal disk - root gets 20GB, data gets the rest
    return &PartitionLayout{
        RootGB: RootTargetSizeGB,
        DataGB: diskSizeGB - RootTargetSizeGB,
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

### 5.6 Idempotent Operations

All disk prep operations must be idempotent to support repeated execution on every boot.

**Important:** Data partition MUST be created before root expansion (see Section 5.2).

```go
const (
    RootTargetSizeGB   = 20
    MinDataPartitionGB = 5
)

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

// CreateDataPartition creates /piccolo-data at the 20GB offset
// MUST be called BEFORE ExpandRootPartition to bound growpart
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

    // Query actual sector size (512 for SATA, possibly 4096 for NVMe)
    sectorSize, _ := p.getSectorSize(ctx, disk)

    // Calculate start sector at 20GB mark
    // This leaves room for ~20GB root and puts data partition at end
    startSector := (RootTargetSizeGB * 1024 * 1024 * 1024) / sectorSize

    slot, err := p.findNextPartitionSlot(ctx, disk)
    if err != nil {
        return err
    }

    // Create partition: start at 20GB, extend to end of disk (0 = end)
    // Type 8309 = Linux LUKS
    if err := p.runner.Run(ctx, "sgdisk",
        "-n", fmt.Sprintf("%d:%d:0", slot, startSector),
        "-t", fmt.Sprintf("%d:8309", slot),
        disk,
    ); err != nil {
        return fmt.Errorf("sgdisk failed: %w", err)
    }

    // Reload partition table
    if err := p.runner.Run(ctx, "partprobe", disk); err != nil {
        p.logger.Warn("partprobe failed, may need reboot", "error", err)
    }

    p.logger.Info("data partition created",
        "disk", disk,
        "slot", slot,
        "start_sector", startSector)
    return nil
}

// ExpandRootPartition expands root up to the data partition boundary
// MUST be called AFTER CreateDataPartition
func (p *Preparer) ExpandRootPartition(ctx context.Context) error {
    rootDev, _ := getRootDevice(ctx)
    disk := getParentDisk(rootDev)
    partNum := getPartitionNumber(rootDev)  // e.g., 2

    // Check current partition size
    currentSizeBytes, err := p.getPartitionSize(ctx, rootDev)
    if err != nil {
        return err
    }
    currentSizeGB := currentSizeBytes / (1024 * 1024 * 1024)

    // Skip if already at or above target size
    if currentSizeGB >= RootTargetSizeGB {
        p.logger.Info("root partition already at target size, skipping expansion",
            "current_gb", currentSizeGB,
            "target_gb", RootTargetSizeGB)
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

    // Notify kernel of partition table change after growpart
    if err := p.runner.Run(ctx, "partprobe", disk); err != nil {
        p.logger.Warn("partprobe failed after root expansion", "error", err)
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

## 6. LUKS2 Encryption

### 6.1 Key Hierarchy

```
┌─────────────────────────────────────────────────────────────────────┐
│                     LUKS2 KEY HIERARCHY                             │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Admin Password                                                     │
│       │                                                             │
│       ├──→ Derives KEK (Argon2id)                                   │
│       │         │                                                   │
│       │         └──→ Decrypts control-plane                         │
│       │                    │                                        │
│       │                    └──→ Contains pool-keyfile.enc           │
│       │                              │                              │
│       │                              └──→ Unwraps pool_key          │
│       │                                          │                  │
│       │                                          └──→ LUKS Keyslot 0│
│       │                                                             │
│       └──→ LUKS Keyslot 1 (Recovery)                                │
│            Admin password → device-specific passphrase              │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 6.2 Pool Keyfile Management

```go
// Stored in control plane: /piccolo-core/control-plane/luks/pool-keyfile.enc
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

The **encrypted** pool keyfile is stored persistently at `/piccolo-core/control-plane/luks/pool-keyfile.enc`. The `/run/piccolo/` path is only used transiently during `cryptsetup` operations.

```go
func (m *StorageManager) InitializeLUKS(ctx context.Context, device, adminPassword string) error {
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
    keyfilePath := "/run/piccolo/pool-keyfile"
    if err := os.WriteFile(keyfilePath, keyfile, 0600); err != nil {
        return err
    }
    defer os.Remove(keyfilePath)
    defer secureZero(keyfile)

    // 3. LUKS format with keyfile (keyslot 0)
    // --batch-mode prevents interactive confirmation prompts
    if err := m.runner.Run(ctx, "cryptsetup", "luksFormat",
        "--type", "luks2",
        "--batch-mode",
        "--key-file", keyfilePath,
        device); err != nil {
        return err
    }

    // 4. Add recovery keyslot (keyslot 1)
    deviceUUID, _ := m.getLUKSUUID(ctx, device)
    recoveryPass := DeriveRecoveryPassphrase(adminPassword, deviceUUID)
    if err := m.addRecoveryKeyslot(ctx, device, keyfilePath, recoveryPass); err != nil {
        return err
    }

    // 5. Backup LUKS header to control plane (for disaster recovery)
    if err := m.backupLUKSHeader(ctx, device, deviceUUID); err != nil {
        m.logger.Warn("failed to backup LUKS header", "error", err)
        // Non-fatal: system can operate without header backup
    }

    // 6. Store encrypted keyfile in control plane
    if err := m.crypto.StorePoolKeyfile(ctx, keyfile); err != nil {
        return err
    }

    return nil
}
```

### 6.4 Recovery Keyslot

```go
func DeriveRecoveryPassphrase(adminPassword, deviceUUID string) []byte {
    salt := []byte("piccolo-luks-recovery:" + deviceUUID)
    return argon2.IDKey(
        []byte(adminPassword),
        salt,
        3,        // time
        64*1024,  // memory (64MB)
        4,        // threads
        32,       // key length
    )
}

func (m *StorageManager) addRecoveryKeyslot(ctx context.Context, device string, keyfilePath string, recoveryPass []byte) error {
    // Write recovery passphrase to temp file (tmpfs)
    recoveryPath := "/run/piccolo/recovery-passphrase"
    if err := os.WriteFile(recoveryPath, recoveryPass, 0600); err != nil {
        return fmt.Errorf("failed to write recovery passphrase: %w", err)
    }
    defer os.Remove(recoveryPath)
    defer secureZero(recoveryPass)

    // Add recovery keyslot (keyslot 1) using pool keyfile as existing key
    // cryptsetup luksAddKey --key-file <pool-keyfile> --key-slot 1 <device> <recovery-passphrase-file>
    if err := m.runner.Run(ctx, "cryptsetup", "luksAddKey",
        "--key-file", keyfilePath,
        "--key-slot", "1",
        device,
        recoveryPath,
    ); err != nil {
        return fmt.Errorf("failed to add recovery keyslot: %w", err)
    }

    return nil
}

func secureZero(b []byte) {
    for i := range b {
        b[i] = 0
    }
}
```

### 6.5 Admin Password Rotation Hook

```go
func (m *StorageManager) OnAdminPasswordRotated(ctx context.Context, oldPass, newPass string) error {
    devices, err := m.listDataPoolDevices(ctx)
    if err != nil {
        return err
    }

    for _, dev := range devices {
        oldRecovery := DeriveRecoveryPassphrase(oldPass, dev.UUID)
        newRecovery := DeriveRecoveryPassphrase(newPass, dev.UUID)
        if err := m.updateLUKSKeyslot(ctx, dev.Path, 1, oldRecovery, newRecovery); err != nil {
            return fmt.Errorf("failed to update keyslot for %s: %w", dev.UUID, err)
        }
    }
    return nil
}
```

### 6.6 LUKS Header Backup and Recovery

The LUKS header contains critical metadata (keyslots, encryption parameters). If corrupted, data is unrecoverable. We backup headers to the control plane for disaster recovery.

```go
// Stored at: /piccolo-core/control-plane/luks/headers/<device-uuid>.bin
func (m *StorageManager) backupLUKSHeader(ctx context.Context, device, deviceUUID string) error {
    backupDir := "/piccolo-core/control-plane/luks/headers"
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
    backupPath := filepath.Join("/piccolo-core/control-plane/luks/headers", deviceUUID+".bin")

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
1. If LUKS header corrupts but control plane is intact → restore header from backup → unlock with pool keyfile
2. If control plane is unavailable but LUKS header intact → unlock with admin password via recovery keyslot
3. If both header AND control plane are lost → data is unrecoverable (by design - encryption works)

**Header Backup Updates:**
- Initial backup after `luksFormat`
- Re-backup after any keyslot changes (password rotation)

## 7. Directory Structure

### 7.1 `/piccolo-core` (Btrfs Subvolume on Root)

```
/piccolo-core/
├── control-plane/            # encrypted control plane store
│   ├── crypto/keyset.json    # SDEK (encrypted)
│   └── luks/
│       ├── pool-keyfile.enc  # LUKS pool keyfile (encrypted with SDEK)
│       └── headers/          # LUKS header backups for disaster recovery
│           └── <device-uuid>.bin
├── recovery/                 # control-plane snapshots
│   ├── current.enc
│   ├── history/
│   └── staging/
├── mounts/                   # volume mountpoints
│   └── <vol-id>/
├── network-bootstrap/        # TPM-sealed enrollment (future)
└── clusterdb/etcd/           # etcd data (future, cluster mode)
```

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
    OnboardingTryLive     OnboardingState = "try_live"     // User chose try live
    OnboardingComplete    OnboardingState = "complete"     // Setup complete
)

// Stored in /piccolo-core/control-plane/onboarding.json
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
    Options    []string        `json:"options,omitempty"` // ["install_disk", "try_live"]
}

// POST /api/v1/system/onboarding
type OnboardingChoice struct {
    Choice string `json:"choice"` // "install_disk" or "try_live"
}
```

### 9.3 "Try Live" Flow

When user selects "Try Live":

1. Mark onboarding state as `try_live`
2. Run full disk prep on USB drive
3. Continue with normal admin password setup
4. Mark onboarding as `complete`

**USB Partitioning Safety:**

The "Try Live" flow partitions the active boot USB device. This is safe because:

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

### 9.4 "Install to Disk" Flow (Deferred)

This RFC does NOT implement "Install to Disk". When selected:

1. Return error indicating feature not yet available
2. Or: Show message directing user to use piccolo-os installer

Future RFC will implement:
- Detect internal disk
- btrfs send/receive from USB to internal
- Reboot into internal disk

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

// Phase 1: Boot-time operations (called before server starts)
func (m *Manager) DetectBootMode(ctx context.Context) (BootMode, error)
func (m *Manager) GetDiskState(ctx context.Context) (*DiskState, error)
func (m *Manager) PreparePartitioning(ctx context.Context) error  // Phase 1: partition prep
func (m *Manager) IsEmergencyMode() bool
func (m *Manager) EmergencyError() error

// Phase 2: Post-auth operations (called from API handlers)
func (m *Manager) InitializeDataVolume(ctx context.Context, adminPassword string) error  // Phase 2: LUKS + mount
func (m *Manager) Unlock(ctx context.Context, adminPassword string) error  // Subsequent boots
func (m *Manager) Lock(ctx context.Context) error

// Lifecycle hooks
func (m *Manager) OnAdminPasswordRotated(ctx context.Context, oldPass, newPass string) error
```

### 10.2 New Package: `internal/storage/diskprep`

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

    if bootMode == storage.BootModeUSB && !diskState.SetupComplete {
        // USB boot, first time - need onboarding
        // Register onboarding endpoints, wait for user choice
    }

    // PHASE 1: Boot-time partitioning (non-blocking, no password needed)
    if needsDiskPrep(diskState) {
        if err := storageMgr.PreparePartitioning(ctx); err != nil {
            // Don't crash - enter emergency mode
            storageMgr.logger.Error("partitioning failed, entering emergency mode", "error", err)
            // Server will start in emergency mode (see Section 12.3)
        }
    }

    // Server starts here - portal becomes available
    // ...

    // PHASE 2 happens later, triggered by API:
    // - POST /api/v1/crypto/setup (first boot) → storageMgr.InitializeDataVolume(ctx, password)
    // - POST /api/v1/crypto/unlock (subsequent) → storageMgr.Unlock(ctx, password)
}

// In crypto handlers:
func (s *GinServer) handleCryptoSetup(c *gin.Context) {
    // ... validate password ...

    // Initialize control plane encryption (existing)
    if err := s.crypto.Setup(ctx, password); err != nil {
        // handle error
    }

    // PHASE 2: Initialize /piccolo-data (LUKS + btrfs + mount)
    if err := s.storageMgr.InitializeDataVolume(ctx, password); err != nil {
        // handle error - could be retried
    }
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

    // Check root size
    state.RootNeedsExpansion = state.RootPartition.SizeGB < RootTargetSizeGB

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
    // Check if /piccolo-core is a btrfs subvolume
    _, err := p.runner.Run(ctx, "btrfs", "subvolume", "show", "/piccolo-core")
    if err != nil {
        return fmt.Errorf("/piccolo-core subvolume missing - OS image is broken")
    }

    // Verify it's mounted and writable
    if err := unix.Access("/piccolo-core", unix.W_OK); err != nil {
        return fmt.Errorf("/piccolo-core is not writable: %w", err)
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

```go
func (m *StorageManager) InitializeLUKS(ctx context.Context, device, adminPassword string) error {
    // LUKS format is atomic - either succeeds or device is unchanged

    // Generate keyfile first
    keyfile, err := GeneratePoolKeyfile()
    if err != nil {
        return err
    }

    // Format LUKS
    if err := m.luksFormat(ctx, device, keyfile); err != nil {
        // Device unchanged, can retry
        return fmt.Errorf("LUKS format failed: %w", err)
    }

    // Add recovery keyslot
    if err := m.addRecoveryKeyslot(ctx, device, keyfile, adminPassword); err != nil {
        // LUKS formatted but no recovery keyslot
        // This is recoverable - can add keyslot later
        m.logger.Error("failed to add recovery keyslot", "error", err)
    }

    // Store keyfile in control plane
    if err := m.crypto.StorePoolKeyfile(ctx, keyfile); err != nil {
        // Critical failure - LUKS exists but keyfile not stored
        // Recovery: user must use admin password via recovery keyslot
        return fmt.Errorf("failed to store keyfile: %w (use admin password to recover)", err)
    }

    return nil
}
```

### 12.3 Emergency Mode

If Phase 1 (boot-time partitioning) fails, the system enters Emergency Mode instead of crash-looping:

```go
type EmergencyState struct {
    Active  bool   `json:"active"`
    Reason  string `json:"reason"`
    Details string `json:"details"`
}

func (m *Manager) PreparePartitioning(ctx context.Context) error {
    // 1. Verify /piccolo-core exists
    if err := m.diskPrep.VerifyPiccoloCoreExists(ctx); err != nil {
        m.enterEmergencyMode(err, "OS image is broken: /piccolo-core subvolume missing")
        return err
    }

    // 2. Create data partition FIRST (bounds growpart)
    if err := m.diskPrep.CreateDataPartition(ctx); err != nil {
        m.enterEmergencyMode(err, "Failed to create /piccolo-data partition")
        return err
    }

    // 3. Expand root partition (bounded by data partition created above)
    if err := m.diskPrep.ExpandRootPartition(ctx); err != nil {
        m.enterEmergencyMode(err, "Failed to expand root partition")
        return err
    }

    return nil
}

func (m *Manager) enterEmergencyMode(err error, reason string) {
    m.emergency = true
    m.emergencyErr = err
    m.emergencyReason = reason
    m.logger.Error("entering emergency mode", "reason", reason, "error", err)
}
```

**Emergency Mode Behavior:**

1. **Server still starts** - HTTP server becomes available
2. **Portal shows error page** - User sees "Storage Initialization Failed" with details
3. **Most APIs disabled** - Only diagnostic and recovery endpoints work
4. **No crash loop** - System stays up for debugging

```go
// Emergency mode middleware
func (s *GinServer) emergencyModeMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        if s.storageMgr.IsEmergencyMode() {
            // Allow only diagnostic endpoints
            allowed := []string{
                "/api/v1/system/health",
                "/api/v1/system/emergency",
                "/",  // Portal (shows error UI)
            }
            for _, prefix := range allowed {
                if strings.HasPrefix(c.Request.URL.Path, prefix) {
                    c.Next()
                    return
                }
            }
            c.JSON(http.StatusServiceUnavailable, gin.H{
                "error":   "storage_emergency",
                "message": s.storageMgr.EmergencyError().Error(),
            })
            c.Abort()
        }
    }
}

// GET /api/v1/system/emergency
type EmergencyStatus struct {
    Active  bool   `json:"active"`
    Reason  string `json:"reason"`
    Error   string `json:"error"`
    Actions []string `json:"actions"`  // Suggested recovery actions
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
3. Select "Try Live" - verify USB gets partitioned
4. Reboot - verify state persists

## 14. Security Considerations

- Pool keyfile only in memory after unlock
- Keyfile encrypted with SDEK in control plane
- Recovery keyslot uses Argon2id with device-specific salt
- LUKS2 provides at-rest encryption (AES-XTS-plain64 by default)
  - Note: Default mode provides confidentiality only, not integrity/authentication
  - Integrity protection would require dm-integrity or authenticated modes (future consideration)
- Temp keyfile written to tmpfs (/run), never to disk
- LUKS header backed up to control plane for disaster recovery

## 15. Future Work

Out of scope for this RFC:

1. **"Install to Disk"** - btrfs send from USB to internal disk
2. **USB device hotplug** - detecting and adding USB storage expansion
3. **JuiceFS integration** - per-volume filesystems
4. **Cluster mode** - etcd placement
5. **Network bootstrap** - TPM enrollment
6. **Degraded mode UI** - surfacing pool status

## 16. Required System Tools

The following CLI tools are required and must be declared as **RPM dependencies** in the piccolod spec file:

| Tool | RPM Package | Purpose |
|------|-------------|---------|
| `lsblk` | util-linux | Disk/partition discovery, transport type detection |
| `sgdisk` | gptfdisk | GPT partition table manipulation |
| `sfdisk` | util-linux | Partition boundary queries (JSON output) |
| `growpart` | growpart (or cloud-utils-growpart) | Expanding root partition |
| `partprobe` | parted | Reloading partition table |
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

## 17. References

- `org-context/03_engineering/storage_architecture.md`
- `piccolo-os/kiwi/microos-ots/piccolo-os.kiwi`
- `internal/persistence/file_volume_manager.go`
- `internal/crypt/manager.go`
