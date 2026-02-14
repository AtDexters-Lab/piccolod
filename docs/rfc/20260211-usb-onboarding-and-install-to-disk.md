# RFC: USB Onboarding and Install to Disk

- **Status:** Draft
- **Date:** 2026-02-11
- **Authors:** Engineering Team
- **Related:**
  - `docs/rfc/20260201-storage-posture.md` (disk posture, partitioning, LUKS2, USB boot onboarding)
  - `docs/rfc/20260202-storage-v2-foundation.md` (PCV exports, control-plane layout, path contracts)
  - `docs/rfc/20260203-install-to-disk.md` (SUPERSEDED — btrfs send/receive approach)
  - `org-context/02_product/acceptance_features/install_to_disk_x86.feature` (product acceptance)
  - `org-context/02_product/piccolo_os_prd.md` §Distribution & install (product requirements)

## 1. Summary

When Piccolo OS boots from USB, the user is presented with an onboarding choice screen:

- **"Try Piccolo"** — Run Piccolo from the USB drive with full functionality.
- **"Install to Disk"** — Download the official piccolo-os `.raw.xz` image from OBS and write it to an internal disk via `xzcat | dd`.

After "Try Piccolo" setup, "Install to Disk" also remains accessible from Settings.

The dd approach is intentionally simple: the OBS image already contains the correct partition table, ESP, bootloader, btrfs subvolume layout, and fstab — eliminating all subvolume/bootloader/fstab complexity. After reboot from internal disk, the existing Phase 1 disk prep handles root expansion and `/piccolo-data` creation automatically.

### 1.1 Relationship to Parent RFCs

- **Posture RFC §9** defines the storage contracts the installer must satisfy: the target disk ends in the two-root posture after first-boot disk prep.
- **Foundation RFC §5** defines the directory layouts on `/piccolo-core` and `/piccolo-data`.
- This RFC specifies the onboarding UX and install pipeline.

**Explicit note:** Unlike the superseded RFC (`20260203`), this approach does NOT perform LUKS setup during installation. LUKS is handled by the standard first-boot flow (Posture RFC §6) after reboot from internal disk.

### 1.2 Changes from Superseded RFC

The superseded RFC (`20260203-install-to-disk.md`) used a btrfs send/receive approach with 11 phases, carry-over mode, and dry-run simulation. This RFC simplifies to:

- **Approach**: OBS image download + dd (~7 pipeline steps) instead of btrfs send/receive (11 phases)
- **Carry-over mode**: Deferred (was 11 phases, now best-effort PCV export before dd)
- **Dry-run simulation**: Deferred
- **LUKS setup**: Moved to post-reboot (standard first-boot flow)
- **API surface**: Simplified (fewer endpoints, simpler state machine)

## 2. Onboarding State Machine

### 2.1 States and Transitions

```
States: pending → try_piccolo → complete
                → install_disk → pending (on boot recovery)
```

```go
type OnboardingState string
const (
    StatePending     OnboardingState = "pending"
    StateTryPiccolo  OnboardingState = "try_piccolo"
    StateInstallDisk OnboardingState = "install_disk"
    StateComplete    OnboardingState = "complete"
)

type OnboardingConfig struct {
    State          OnboardingState `json:"state"`
    BootMode       string          `json:"boot_mode,omitempty"`
    InstallDone    bool            `json:"install_done,omitempty"`
    UpdatedAt      string          `json:"updated_at,omitempty"`
}
```

### 2.2 Transition Rules

| From | To | Condition |
|------|----|-----------|
| `pending` | `try_piccolo` | User chooses "Try Piccolo" |
| `pending` | `install_disk` | User chooses "Install to Disk" |
| `try_piccolo` | `complete` | Setup wizard completes |
| `install_disk` | `pending` | Automatic on boot if `InstallDone == false` (recovery) |

All other transitions are rejected with an error.

### 2.3 Persistence

State is persisted atomically (write temp + rename) to `paths.CoreJoin("network-bootstrap", "onboarding.json")`. The existing `storage.Manager.isPreviouslySetUp()` already reads this file to check for `state: "complete"`.

### 2.4 Boot Recovery

If the loaded state is `install_disk` and `InstallDone == false` on boot, the state is reset to `pending`. This handles the case where an install was interrupted (power loss, dd failure) — the user can retry.

### 2.5 Manager

```go
type Manager struct {
    mu       sync.RWMutex
    config   OnboardingConfig
    bootMode storage.BootMode
    filePath string
}
```

Key methods:
- `NewManager(bootMode BootMode) *Manager` — loads existing state or creates pending
- `State() OnboardingState`
- `BootMode() BootMode`
- `IsRequired() bool` — true when USB/Unknown boot AND state is `pending`
- `IsUSBBoot() bool` — true when boot mode is USB (for Settings visibility)
- `Choose(choice OnboardingState) error` — validates transition, persists
- `MarkInstallDone() error` — sets `InstallDone = true`, persists
- `Complete() error` — transitions `try_piccolo → complete`

## 3. Internal Disk Discovery

### 3.1 DiskInfo

```go
type DiskInfo struct {
    Device    string `json:"device"`
    Model     string `json:"model"`
    SizeGB    int    `json:"size_gb"`
    Transport string `json:"transport"`
    HasData   bool   `json:"has_data"`
}
```

### 3.2 Discovery Algorithm

1. `lsblk -Jbndo NAME,SIZE,TRAN,MODEL,TYPE` — all block devices
2. Filter to `TYPE == "disk"` only
3. Get boot disk via `findmnt -nro SOURCE /` → `storage.GetParentDisk()`
4. Exclude boot disk
5. Exclude USB transport (`TRAN == "usb"`)
6. For each: probe `has_data` via `lsblk -ndo FSTYPE` on child partitions

### 3.3 Target Disk Validation (at install time)

- Must be in current discovery results (prevents path traversal)
- Must be a whole disk device (not a partition)
- No partitions of the disk may be mounted
- No device-mapper users (LUKS, LVM) on any partition
- Not in use as swap (`/proc/swaps`)

## 4. Install to Disk Pipeline

### 4.1 Image URL Resolution

- OBS base: `https://download.opensuse.org/repositories/home:/atdexterslab:/piccolo-os/home_atdexterslab_atdexterslab_tumbleweed/`
- Detect arch: `runtime.GOARCH` → `amd64` maps to `x86_64`, `arm64` maps to `aarch64`
- Detect board: check `/sys/firmware/devicetree/base/model` for "Raspberry Pi"
- URL patterns:
  - x86_64: `piccolo-os.x86_64-SelfInstall.raw.xz`
  - aarch64 generic: `piccolo-os.aarch64-SelfInstall.raw.xz`
  - Raspberry Pi: `piccolo-os.aarch64-RaspberryPi.raw.xz`
- Override: `PICCOLO_INSTALL_IMAGE_URL` env var only (no user-supplied URL in API — eliminates SSRF risk)

### 4.2 Image Integrity

Download the companion `.raw.xz.sha256` file from OBS. Verify SHA-256 of downloaded image before writing. If checksum file unavailable, log warning and proceed.

### 4.3 Pipeline Phases

| Phase | Progress | Action |
|-------|----------|--------|
| Validate | 0-2% | Verify target disk, check `/sys/firmware/efi` |
| Download | 2-65% | Parallel 16-connection download of `.raw.xz` from OBS + fetch `.sha256` checksum |
| Verify | 65-70% | SHA-256 verify downloaded image |
| Write | 70-92% | `xzcat <file> \| dd of=<disk> bs=4M conv=fsync` with progress parsing |
| Sync | 92-95% | `sync` to flush all writes |
| Boot config | 95-98% | Best-effort `efibootmgr` to set internal disk as first boot entry |
| Complete | 98-100% | Clean up temp files, call `onboardingMgr.MarkInstallDone()` |

### 4.4 Parallel Download (16 connections)

Uses HTTP Range requests with 16 parallel goroutines:

1. HTTP HEAD to get Content-Length and confirm Range support
2. If server doesn't support Range → fall back to single-stream
3. Create output file, preallocate to Content-Length
4. Split into 16 chunks, each goroutine writes to correct offset via `file.WriteAt()`
5. `errgroup.Group` with context for coordinated cancellation
6. Retry 3x with backoff per chunk on failure
7. Progress via shared `atomic.Int64`

### 4.5 xzcat | dd Orchestration

```go
xzCmd := exec.CommandContext(ctx, "xzcat", imagePath)
pipe, _ := xzCmd.StdoutPipe()

ddCmd := exec.CommandContext(ctx, "dd", "of="+targetDisk, "bs=4M", "conv=fsync", "status=progress")
ddCmd.Stdin = pipe
ddCmd.Stderr = &progressParser

xzCmd.Start()
ddCmd.Start()

ddErr := ddCmd.Wait()
xzErr := xzCmd.Wait()
```

`conv=fsync` ensures data reaches disk before dd exits. `bs=4M` aligned to common sector sizes. `status=progress` parsed for UI updates.

### 4.6 Download Staging

Temp file: `/tmp/piccolo-install/<unique-id>.raw.xz`. Pre-download space check via `syscall.Statfs`. If tmpfs insufficient, fall back to `/piccolo-core/recovery/install-staging/`.

### 4.7 Concurrency

`sync.Mutex` ensures only one install at a time. Returns 409 if already running.

## 5. API Endpoints

### `GET /api/v1/system/onboarding` (public, no auth)

Returns current onboarding state. For internal boot, always returns `required: false`.

```json
{
  "state": "pending",
  "boot_mode": "usb",
  "required": true,
  "install_done": false
}
```

### `POST /api/v1/system/onboarding` (public, no auth)

```json
{ "choice": "try_piccolo" }
```

Side effects: persists state; for `try_piccolo`, calls `storageMgr.StartPartitioningAsync()`.

### `GET /api/v1/storage/disks` (public, no auth)

```json
{
  "disks": [
    { "device": "/dev/sda", "model": "Samsung SSD 870", "size_gb": 256, "transport": "sata", "has_data": true }
  ]
}
```

### `POST /api/v1/system/install-to-disk` (LAN-only)

Public during onboarding (`state == pending || state == install_disk`). Requires admin auth from Settings (`state == try_piccolo || state == complete`).

```json
{
  "target_disk": "/dev/sda",
  "confirm_data_loss": true,
  "task_id": "install-1707000000"
}
```

No `image_url` field (SSRF prevention). `confirm_data_loss` must be `true`.

### `GET /api/v1/system/install-progress/stream` (WebSocket, public)

Unauthenticated progress stream scoped to `install-` prefixed task IDs.

### `POST /api/v1/system/reboot` (public, LAN-only)

Only available when `state == install_disk` AND `InstallDone == true`. Returns 409 if install not complete.

## 6. Server Wiring

### Construction

```go
bootMode, _ := storage.DetectBootMode(ctx, runner.ExecRunner{})
onboardingMgr := onboarding.NewManager(bootMode)
installer := onboarding.NewInstaller(execRunner, events.NewBusProgressReporter(eventsBus))
```

### Boot Logic

```go
switch {
case onboardingMgr.BootMode() == storage.BootModeInternal:
    storageMgr.StartPartitioningAsync(ctx)
case onboardingMgr.State() == onboarding.StateTryPiccolo ||
     onboardingMgr.State() == onboarding.StateComplete:
    storageMgr.StartPartitioningAsync(ctx)
default:
    // USB/Unknown, pending onboarding — wait for user choice
}
```

### Emergency Middleware

Add read-only endpoints to emergency allowlist:
- `/api/v1/system/onboarding` (GET + POST)
- `/api/v1/storage/disks` (GET)

Do NOT add destructive endpoints (`install-to-disk`, `reboot`, `install-progress`) to emergency allowlist.

### Event Topic

```go
TopicOnboardingStateChanged Topic = "onboarding_state_changed"
```

## 7. Frontend Changes

### 7.1 Setup States

New states added to `SetupState` enum: `onboarding`, `installDisk`, `installComplete`.

### 7.2 Onboarding Step

Two-card choice screen:
- **"Try Piccolo"**: POST to onboarding endpoint, transition to welcome
- **"Install to Disk"**: Fetch disks, transition to install flow. Greyed out if no internal disks.

### 7.3 Install Disk Step

Multi-phase flow:
1. Disk selection with data loss warnings for disks with `has_data: true`
2. Confirmation dialog
3. Progress via `TaskProgressPanel` with custom `urlPath` for unauthenticated endpoint
4. Completion with reboot prompt

### 7.4 Pre-Existing Data Warnings

Erase warning for disks with `has_data: true`. Previous Piccolo installation detection (btrfs label `piccolo-root` or LUKS label `piccolo-data` on target).

### 7.5 Settings Integration

"Install to Disk" visible in Settings when `boot_mode == "usb"` and state is `try_piccolo` or `complete`. Same install flow, requires admin auth.

## 8. Carry-Over via PCV

**Before admin setup (fresh USB boot → Install to Disk):**
No control plane exists. After reboot on internal disk, normal fresh first-boot.

**After Try Piccolo + full setup → Install to Disk (from Settings):**
1. Install handler calls `pcvPublisher.PublishNow()` before starting dd (best-effort)
2. dd writes fresh image to internal disk, reboot
3. User can import PCV manually via existing import API or "Restore from backup" on setup screen

## 9. Error Handling

| Failure | Behavior |
|---------|----------|
| No internal disks found | "Install to Disk" greyed out with explanation |
| Network unreachable | Download fails → retry 3x with backoff → report error |
| Download corrupted | SHA-256 mismatch → report error, delete temp file |
| Download interrupted | Temp file deleted. Fresh download on retry. |
| dd fails / power loss | USB still boots. State auto-resets to `pending`. |
| efibootmgr fails | Warning only: "Change boot order in BIOS manually." |
| Disk removed during install | dd fails with I/O error → reported via progress |
| Target disk is mounted | Rejected at validation with clear error |
| Insufficient staging space | Check before download, abort with error |

## 10. Security Considerations

- **SSRF prevention**: No `image_url` field in API. Image URL derived from arch/board or `PICCOLO_INSTALL_IMAGE_URL` env var.
- **LAN-only**: Destructive operations restricted to LAN.
- **Data loss gate**: `confirm_data_loss` must be `true` in request.
- **SHA-256 integrity**: Downloaded image verified against checksum.
- **Conditional auth**: No auth during pre-setup onboarding; admin auth required for Settings path.

## 11. Post-Install Boot Sequence

After dd, the freshly written internal disk boots through:

1. **Phase 1 disk prep**: Root expansion to ~20GB + `/piccolo-data` partition creation
2. **First-run setup**: Admin password creation
3. **Phase 2 LUKS init**: LUKS2 encryption of `/piccolo-data` (after admin password)

**Device identity:** The OBS image generates a fresh `machine-id` on first boot via `systemd-firstboot`. No shared identity concern (unlike superseded RFC's btrfs sync which copied the USB's `machine-id`).

**Post-install verification:** `dd` + `conv=fsync` ensures data reaches disk. No post-write filesystem mount/verify (scope reduction from superseded RFC — acceptable since dd is an atomic write, not multi-phase assembly).

## 12. Required System Tools

| Tool | RPM Package | Purpose |
|------|-------------|---------|
| `xzcat` | xz | Decompress `.raw.xz` image |
| `dd` | coreutils | Write decompressed image to disk |
| `efibootmgr` | efibootmgr | UEFI boot entry management |
| `lsblk` / `findmnt` | util-linux | Disk discovery and boot disk detection |

Note: `dosfstools`, `grub2`, `dracut` (required by superseded RFC) are no longer needed by the installer.

## 13. Testing

**Unit tests:**
- State machine: valid/invalid transitions, persistence, `IsRequired()` per boot mode, boot recovery
- Disk discovery: mock lsblk output, boot disk exclusion, USB exclusion, mounted disk detection
- Installer: mock runner for pipeline phases, xzcat/dd start ordering, dual-wait error handling, progress events, retry logic, SHA-256 verification
- Handlers: onboarding status per state, install validation, reboot gating, conditional auth

**Integration tests:**
- Full onboarding flow with `PICCOLO_BOOT_MODE_OVERRIDE=usb`
- Install pipeline with loopback device

**Manual/E2E:**
- Boot from USB → onboarding screen → Try Piccolo path
- Boot from USB → Install to Disk → verify reboot to internal disk
- Try Piccolo → Settings → Install to Disk (admin auth required)

## 14. Acceptance Criteria

Mapping to `install_to_disk_x86.feature` scenarios:

| Scenario | Status |
|----------|--------|
| Landing page presents Try or Install options | v1 scope |
| Try mode allows deferred installation | v1 scope |
| Install to Disk with fresh start | v1 scope |
| Install to Disk with state migration | Deferred from v1 |
| Dry run simulation | Deferred from v1 |

## Implementation Notes & Status
- 2026-02-11: Initial draft. Covers USB onboarding flow and OBS image download + dd approach.
