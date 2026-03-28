# RFC: Piccolo-Core Rollback Guard

- **Status:** Draft
- **Date:** 2026-03-28
- **Amends:** RFC 20260210 (Unbrickable Piccolo OS — this RFC fills the data-side rollback gap identified in that RFC's defense layer architecture)

## 1. Summary

A pair of systemd services that automatically pair btrfs snapshots of `/piccolo-core` with OS snapshot IDs, so that an OS transactional rollback also restores the correct control plane data state. Fully external to piccolod — zero daemon code changes.

## 2. Motivation

Piccolo OS uses MicroOS with btrfs transactional updates. The OS root is snapshotted by snapper, but `/piccolo-core` — a btrfs subvolume (`@/piccolo-core`) containing all control plane state — is **not** included in OS snapshots. This creates a version skew risk:

1. OS update installs new `piccolod` binary (in new OS snapshot)
2. New `piccolod` boots, runs migrations, mutates `/piccolo-core`
3. Something goes catastrophically wrong (buggy migration, corruption, etc.)
4. User rolls back OS → old binary restored, but `/piccolo-core` still mutated
5. Old binary can't read the mutated state → **system bricked**

The defense must be **structurally external to piccolod** — we cannot rely on any particular piccolod version to take the correct safety steps (e.g., snapshotting before migration). A forgotten safety step in one release would defeat the entire mechanism.

### 2.1 What lives in `/piccolo-core`

All regular files — no nested btrfs subvolumes in the current implementation:

```
/piccolo-core/                        (btrfs subvolume @/piccolo-core, KIWI-created)
├── control-plane.luks                (256 MiB sparse LUKS2+ext4 — the control plane data)
├── crypto/                           (keyset, pool key, KDF params, LUKS header backups)
├── volumes/control-plane/            (volume metadata JSON)
├── mounts/                           (runtime mount points — empty at boot)
├── recovery/                         (PCV exports)
├── update/state.json                 (update tracking)
└── ... (cache, network-bootstrap, tiering, drbd-meta, federation, system-objects)
```

### 2.2 Mount setup

`/piccolo-core` is declared in the KIWI image config (`<volume name="piccolo-core" />`) across all profiles (VirtualBox, RaspberryPi, Rock64, SelfInstall). With `btrfs_root_is_readonly_snapshot="true"`, KIWI creates it as `@/piccolo-core` on the btrfs tree and generates a separate fstab entry to mount it writable over the read-only root snapshot.

This means it cannot be renamed in-place (it's a mount point) but CAN be unmounted, deleted, and replaced via btrfs operations on the top-level tree.

## 3. Design

### 3.1 Two-layer architecture

| Layer | What | When | Mechanism |
|-------|------|------|-----------|
| **Snapshot creation** (primary) | Pre-reboot snapshot | Before every orderly reboot | systemd `Before=reboot.target` oneshot |
| **Snapshot creation** (safety net) | Boot-time snapshot | On first boot of a new OS version | systemd `Before=piccolod.service` oneshot |
| **Rollback detection + restore** | Boot-time guard | Every boot, before piccolod | Same boot-time oneshot |

**Why two layers for creation:**
- The pre-reboot service fires on **every** orderly reboot regardless of source: piccolod's `Reboot()`, `ForceReboot()`, health-checker rollback, manual `systemctl reboot`. No piccolod integration needed.
- Unclean shutdowns (crash, watchdog reset, power loss) bypass `reboot.target`. The boot-time guard catches these: if the OS version changed, it snapshots the current (pre-piccolod) state before piccolod starts.
- Both services use a single shell script (`rollback-guard.sh`) with `--pre-reboot` and `--boot` flags.

**Why fully external — no piccolod code changes:**
- Keeps everything in one package (`piccolo-os-support`), one repo (`piccolo-os`), one language (shell).
- A buggy piccolod release cannot break the snapshot mechanism.
- A `Before=reboot.target` systemd service has broader coverage than a hook in piccolod's `Reboot()` — it fires on manual reboots, health-checker-triggered reboots, etc.

### 3.2 Snapshot storage

Snapshots live in `/var/lib/piccolo-rollback-guard/`:

- `/var` is a separate btrfs subvolume (`@/var`) on the same btrfs filesystem
- Survives OS rollbacks (not part of root snapshot)
- No confusion about storing snapshots inside the subvolume being snapshotted
- btrfs can snapshot across subvolumes on the same filesystem

```
/var/lib/piccolo-rollback-guard/
├── boot-marker             (text: "v1:<os-snapshot-id>")
├── restore-count           (text: consecutive restore count for loop prevention)
└── snapshots/
    ├── 5/                  (read-only btrfs snapshot of @/piccolo-core)
    ├── 7/
    └── 9/
```

### 3.3 Restore mechanism: btrfs native (snapshot-first, delete-second)

Restore uses btrfs operations on the top-level tree. The sequence is **snapshot-first, delete-second** — at no point does `/piccolo-core` not exist:

```bash
# 1. Get root block device (from / — survives if /piccolo-core is unmounted)
root_dev=$(findmnt -no SOURCE / | sed 's/\[.*\]//')

# 2. Unmount (safe — nothing uses it before piccolod)
umount /piccolo-core

# 3. Mount top-level btrfs tree
mount -o subvolid=5 "$root_dev" /mnt/btrfs-root

# 4. SAFE RESTORE:
#    4a fails → old piccolo-core untouched, abort
#    4b fails → both exist (wasted space, not a brick)
#    4c fails → piccolo-core-restoring exists, retry next boot

# 4a. Create writable snapshot at temporary name
btrfs subvolume snapshot \
    /mnt/btrfs-root/@/var/lib/piccolo-rollback-guard/snapshots/{os_id} \
    /mnt/btrfs-root/@/piccolo-core-restoring

# 4b. Delete old subvolume
btrfs subvolume delete /mnt/btrfs-root/@/piccolo-core

# 4c. Rename to canonical path
mv /mnt/btrfs-root/@/piccolo-core-restoring /mnt/btrfs-root/@/piccolo-core

# 5. Cleanup and remount
umount /mnt/btrfs-root
mount /piccolo-core    # fstab: subvol=@/piccolo-core
```

## 4. Algorithm

### 4.1 Pre-reboot snapshot

Fires on every orderly reboot/shutdown via `Before=reboot.target`:

1. Parse active OS snapshot ID from `findmnt -no SOURCE /`. No match → exit 0.
2. Snapshot (safe order):
   - `btrfs subvolume snapshot -r /piccolo-core → snapshots/{os_id}.new`
   - Delete old snapshot for this OS if exists
   - Rename `.new` → `{os_id}`
3. Log success/failure. **Never block reboot** — exit 0 always.

### 4.2 Boot-time guard

Fires on every boot via `Before=piccolod.service`:

1. **DETECT**: Parse active OS snapshot ID. Regex: `/\.snapshots/(\d+)/snapshot/` (same as `internal/update/manager.go:868`). No match → exit 0.
2. **VALIDATE**: Check `/piccolo-core` and `/var` are on the same btrfs filesystem. Diverge → log error, exit 1.
3. **SETUP**: `mkdir -p` guard directory. Clean up stale `@/piccolo-core-restoring` if present.
4. **CHECK**: Read boot marker. If `current_os == prev_os` → reset restore counter, exit 0.
5. **FIRST-BOOT**: If no marker → take snapshot tagged with `current_os`, write marker, exit 0.
6. **OS VERSION CHANGED**:
   - **6a. SAVE** current state tagged with `prev_os` (skip if already saved): `btrfs subvolume snapshot -r /piccolo-core → snapshots/{prev_os}`
   - **6b. RESTORE** (if snapshot exists for `current_os`):
     - Verify sanity: `control-plane.luks` and `crypto/keyset.json` exist in snapshot
     - Check restore counter (skip if ≥ 3)
     - Execute snapshot-first, delete-second sequence (§3.3)
     - Increment restore counter
7. **MARKER**: Write `current_os` to boot-marker **last** (crash-safety).
8. **PRUNE**: Keep max 5 snapshots. Never prune `current_os` or `prev_os`. Sort numerically.

## 5. Systemd Units

### 5.1 Pre-reboot snapshot

```ini
[Unit]
Description=Piccolo Core Pre-Reboot Snapshot
DefaultDependencies=no
Before=reboot.target halt.target poweroff.target
ConditionPathIsDirectory=/piccolo-core
ConditionPathIsDirectory=/.snapshots

[Service]
Type=oneshot
ExecStart=/usr/libexec/piccolo/rollback-guard.sh --pre-reboot
TimeoutStartSec=60

[Install]
WantedBy=reboot.target halt.target poweroff.target
```

### 5.2 Boot-time guard

```ini
[Unit]
Description=Piccolo Core Rollback Guard
After=local-fs.target
Before=piccolod.service
ConditionPathIsDirectory=/piccolo-core
ConditionPathIsDirectory=/.snapshots

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/libexec/piccolo/rollback-guard.sh --boot
TimeoutStartSec=120
StandardOutput=journal
StandardError=journal
SyslogIdentifier=piccolo-rollback-guard

[Install]
WantedBy=multi-user.target
```

- **No `Requires=piccolod.service`**: Guard failure does not block piccolod
- **`ConditionPath` guards**: No-op on non-Piccolo or non-MicroOS systems
- **`DefaultDependencies=no`** on pre-reboot: ensures it runs during the shutdown sequence

## 6. Error Handling

| Failure | Handling | Marker written? |
|---------|----------|-----------------|
| Not a snapshotted system | exit 0 | N/A |
| `/piccolo-core` and `/var` on different filesystems | exit 1 | N/A |
| Snapshot save fails | Log warning, continue | Yes |
| Disk space < 500 MiB | Skip snapshot, log warning | Yes |
| Restore sanity check fails | Skip restore, log error | Yes |
| Restore counter ≥ 3 | Skip restore, log warning | Yes |
| Unmount `/piccolo-core` fails | exit 1 | **No** (retry next boot) |
| Snapshot to `@/piccolo-core-restoring` fails | Remount old, exit 1 | **No** (old data intact) |
| Delete `@/piccolo-core` fails | Both exist (wasted space), exit 1 | **No** (retry) |
| Rename fails | `@/piccolo-core-restoring` exists, exit 1 | **No** (cleaned up on retry) |
| Pre-reboot snapshot fails | Log warning, exit 0 (never block reboot) | N/A |

**Marker-last invariant**: Crash before marker write = automatic retry on next boot.

**Restore counter semantics**: Increment on every restore attempt. Reset on stable boot (`current_os == prev_os`). Skip restore at ≥ 3 — device is in a state that repeated restore can't fix.

## 7. Walkthrough

| Boot | active_os | marker | Action | Snapshots |
|------|-----------|--------|--------|-----------|
| First ever | 5 | (none) | Boot guard: first-boot snapshot for 5, marker→5 | {5} |
| *Running for weeks...* | | | | |
| Update triggered, reboot | 5 | 5 | Pre-reboot: refreshes snapshot for 5 (latest state) | {5} |
| Boot into v7 | 7 | 5 | Boot guard: save for 5 (exists→skip), no snap for 7→skip restore, marker→7 | {5} |
| *piccolod v7 runs migration...* | | | | |
| Rollback, reboot | 7 | 7 | Pre-reboot: saves snapshot for 7 (post-migration state) | {5, 7} |
| Boot into v5 | 5 | 7 | Boot guard: save for 7 (exists→skip), snap for 5→**btrfs restore**, marker→5 | {5, 7} |
| *piccolod v5 boots with original state* | | | | |

## 8. Edge Cases

### 8.1 Guard + health-checker interaction
If restored state causes failure: health-checker triggers `snapper rollback` → same OS ID → no-op. Boot stabilizes. Restore counter (≥ 3 = skip) prevents infinite loops.

### 8.2 Unclean shutdown
Pre-reboot service doesn't fire. Boot-time guard catches it: if the OS version changed, step 6a snapshots the current (pre-piccolod) state before piccolod starts. This is the correct state for the previous OS version.

### 8.3 LUKS loop file crash consistency
- **Boot-time guard**: LUKS loop not open. Snapshot captures quiescent file.
- **Pre-reboot service**: LUKS loop IS open. ext4 journal inside LUKS guarantees consistency — btrfs COW snapshots the outer file atomically, ext4 journal replay recovers the inner filesystem on restore.

### 8.4 Disk space
btrfs snapshots are COW — cost is only changed blocks. Guard checks `btrfs filesystem usage` before creating. Max 5 snapshots with numeric-sort pruning.

### 8.5 Guard script versioning
Boot marker format: `v1:<os_id>`. Unknown versions → skip.

### 8.6 Nested subvolume guard
Current `/piccolo-core` has no nested btrfs subvolumes. If a future change adds one, `btrfs subvolume delete` fails (can't delete with children). Guard detects and logs — requires design update.

### 8.7 Stale restoring subvolume
If `@/piccolo-core-restoring` exists at boot (crash during previous restore), delete it before proceeding.

## 9. Packaging

Everything in `piccolo-os` repo, `piccolo-os-support` RPM. **Zero piccolod changes.**

| File | Description |
|------|-------------|
| `/usr/libexec/piccolo/rollback-guard.sh` | Single script (~200 lines), `--pre-reboot` and `--boot` modes |
| `/usr/lib/systemd/system/piccolo-core-pre-reboot-snapshot.service` | Pre-reboot trigger |
| `/usr/lib/systemd/system/piccolo-core-rollback-guard.service` | Boot-time guard + restore |

RPM `%post`:
```bash
systemctl --root=/ --no-reload enable piccolo-core-rollback-guard.service
systemctl --root=/ --no-reload enable piccolo-core-pre-reboot-snapshot.service
```

## 10. Interaction with Existing Systems

| System | Interaction |
|--------|------------|
| **PCV exports** | Orthogonal — PCV is cross-device recovery; guard is same-device rollback |
| **Health checker** | Composes correctly — OS rollback triggers guard on next boot |
| **Update manager** | No integration needed — pre-reboot systemd unit covers all reboot paths |
| **App tuples** | Operate on `/piccolo-data` (LVM), not `/piccolo-core`. No interaction |

## 11. Future Enhancements

- **Routine snapshots**: Daily timer for general recovery beyond OS updates
- **PCV trigger sharing**: Take btrfs snapshot alongside PCV publish
- **Portal visibility**: piccolod reads guard status on startup, surfaces in system health
- **Diagnostic mode**: `rollback-guard.sh --status` for operator debugging
