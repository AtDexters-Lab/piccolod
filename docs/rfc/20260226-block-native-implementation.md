# Block-Native Storage Implementation Plan

**Date:** 2026-02-26
**Status:** Implementation
**Target RFC:** `org-context/03_engineering/storage_architecture_block_native.md`

## Summary

Replace FUSE-based storage (gocryptfs for encryption, fuse-overlayfs for container overlay) with a zero-FUSE block-native stack: per-volume LUKS2 on LVM thin LVs (ext4), kernel-native overlayfs, a LUKS loop file for the control plane, and DRBD+NBD as the cluster/tiering foundation.

## Design Decisions

1. ~~**Strict no-FUSE**: Hard fail if native overlay unavailable. No `mount_program` fallback.~~ **Revised:** Volume I/O is zero-FUSE (LUKS+ext4). Container rootfs overlay uses fuse-overlayfs for cross-user additionalimagestore access (rootless limitation — see Findings below).
2. **No backward compatibility**: No migration code, no dual-path logic.
3. **No pool-level LUKS**: LVM VG on raw partition. Per-volume LUKS2 encryption.
4. **Remove Argon2id on recovery mnemonic**: Mnemonic key used directly as LUKS passphrase.
5. **Always full stack**: ALL app volumes use `thin LV → NBD → DRBD → LUKS → ext4`.

## Block Device Stack

```
dm-thin LV         ← thin provisioning (M1)
  ↑
NBD server/client  ← userspace block I/O, PSFN hooks paused (M2)
  ↑
DRBD               ← replication, standalone/paused in single-node (M2)
  ↑
LUKS2              ← per-volume encryption (M3)
  ↑
ext4               ← mounted filesystem, visible to containers (M3)
```

Control plane: `loop file → LUKS → ext4` (no thin LV, no NBD, no DRBD).

## Milestones

### M1: LVM Thin Pool
### M2: NBD + DRBD
### M3: Per-Volume LUKS2
### M4: Zero FUSE

See implementation plan for full details.

## Testing Journal

### 2026-02-26 — Alpha VM Testing (Tumbleweed)

**Setup:** Cloned `piccolo-dev-template` (Tumbleweed + VBox shared folder at `/piccolod`).
Test script: `scripts/alpha/dev-vm-alpha-test.sh`.

#### Session 1: Initial Boot & Storage Stack

Stages 0-6 pass clean. Block device stack verified:
- LVM VG `piccolo-data-vg` with thin pool
- Control plane LUKS loop at `/piccolo-core/control-plane.luks`
- Per-volume LUKS2 encryption on thin LVs
- ext4 mounts at `/piccolo-core/mounts/app-*/`

**Bugs found and fixed:**

1. **`DestroyVolume` race** — detach LUKS before LV destroy (was trying to destroy busy LV).
2. **`ensureAppVolume` stale state** — cleanup-and-recreate when LUKS mapper exists but volume handle is stale.
3. **`newuidmap` capability conflict** — file capabilities + setuid bit on `/usr/bin/newuidmap` cause EPERM. Test script strips file caps.
4. **Pre-setup startup crash** — `fileVolumeManager` wiring still referenced; fixed to `luksVolumeManager`.
5. **`persistence: locked` error** — ensureUnlocked gate needed control volume attachment.

#### Session 2: Service App Install (stage 7)

**Bug: `statfs` permission denied on per-app data dir**

Root cause: `/piccolo-core/mounts/` directory was `drwx------` (0700, root:root). Per-app users couldn't traverse it to reach their own mount points. The old `fileVolumeManager` handled this (making `mounts/` 0711), but `luksVolumeManager` didn't.

Fix: Added `MkdirAll(mountsParent, 0o711)` + `Chmod(mountsParent, 0o711)` in `luksVolumeManager.attachAppVolume`.

**Bug: `creating /etc/mtab symlink: permission denied` (service mode)**

Root cause: Cross-user overlay with additionalimagestore. Shared imagestore layers owned by `piccolo-runtime` (UID 470); per-app user (UID 468) couldn't use them via kernel overlay because `mount_setattr(MOUNT_ATTR_IDMAP)` requires `CAP_SYS_ADMIN` in the init user namespace — rootless podman doesn't have this.

Investigation confirmed:
- Root podman: `"Supports shifting": "true"` — kernel 6.18 has full idmap support
- Rootless podman: `"Supports shifting": "false"` — unconditionally, checked in `containers/storage` via `os.Geteuid() != 0` guard

Fix: Added `mount_program = "/usr/bin/fuse-overlayfs"` + `force_mask = "shared"` to per-app storage.conf in `ensureAdditionalStoresConf`.

**FUSE surface analysis:** Only container rootfs overlay is FUSE (read-mostly, page-cached). Volume I/O is fully kernel-native (LUKS+ext4). The FUSE surface is dramatically smaller than the old gocryptfs approach where ALL data I/O was FUSE.

**Stage 7 result:** All 4 checks passed (install, running, uninstall, cleanup).

#### Session 3: Workspace App Install (stage 8)

**Bug: `creating /etc/mtab symlink: permission denied` (workspace mode)**

Root cause: Kernel overlay cannot remap UIDs across users. Image layers in the lower dir are owned by the image runtime user (piccolo-runtime UID 470, rootless pull extracts with remapped UIDs). The container runs as the per-app user (UID 468) in a user namespace where container UID 0 = host UID 468. When the container's init process (host UID 468) tries to create `/etc/mtab`, it needs write access to `/etc/` — but `/etc/` in the lower layer is owned by host UID 470 with mode 755.

Initial fix attempt (chowning workspace upper dir) was insufficient: copy-up preserves lower layer UIDs, so copied-up directories retain the runtime user's ownership. The container's user namespace capabilities (CAP_DAC_OVERRIDE) don't apply to init-namespace mounts.

Fix: Switched workspace overlay from kernel overlayfs to fuse-overlayfs with `squash_to_uid/gid`. All files in the merged overlay appear owned by the per-app user, enabling the container to write through the overlay. fuse-overlayfs also requires `allow_other` since the mount is created by root but accessed by the per-app user.

Changes:
- `workspacedisk/mount.go`: Added `mountFuseOverlay` — uses fuse-overlayfs when UID mapping needed
- `workspace_disk_integration.go`: Simplified `buildWorkspaceMountOpts` to use squash instead of per-range UID mapping

**Architecture summary:** Both service and workspace containers use fuse-overlayfs for their rootfs overlay layer (cross-user UID handling). Volume I/O remains fully kernel-native (LUKS+ext4). The FUSE surface is read-mostly and page-cached — minimal performance impact.

**Stage 8 result:** All 5 checks passed (install, running, fuse-overlayfs mount, uninstall, cleanup).

#### Session 3 cont'd: Reboot & Post-Reboot (stages 9-10)

**Stage 9 (reboot):** All 4 checks passed — VBox reset, crypto initialized+locked after reboot, unlock succeeds, crypto unlocked.

**Stage 10 (post-reboot):** All 3 checks passed — LVM VG active, control plane mounted, no stale FUSE mounts.

**Test script fix:** `getfattr` returns exit 1 when attribute doesn't exist, triggering `set -e`. Added `|| true` to the `vssh` calls in stage 0.

**Full suite result (50/50):**
- Stage 0 (prereq): 17/17
- Stage 1 (boot): 3/3
- Stage 4 (post-setup): 2/2
- Stage 5 (storage inspect): 7/7
- Stage 6 (storage verify): 5/5
- Stage 7 (service app): 4/4
- Stage 8 (workspace app): 5/5
- Stage 9 (reboot): 4/4
- Stage 10 (post-reboot): 3/3
