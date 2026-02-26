# Piccolo-OS: Block-Native Storage Alignment

**Date:** 2026-02-26
**Status:** Planned (not yet implemented)
**Depends on:** `20260226-block-native-implementation.md` (piccolod M1–M4, committed)
**Repo:** `piccolo-os` (kiwi image config + `piccolo-os-support` RPM)

## Summary

The piccolod block-native storage implementation (M1–M4) replaces FUSE-based storage
with kernel-native components: LVM thin pools, DRBD, NBD, per-volume LUKS2, and
kernel overlayfs. The OS image must be updated to ship the required userspace tools,
kernel modules, and remove the now-dead FUSE packages.

## Current State (piccolo-os)

| Component | Package | Version | Used by |
|---|---|---|---|
| FUSE overlay | `fuse-overlayfs` | 1.15 | Rootless Podman container overlay |
| FUSE encryption | `gocryptfs` | 2.6.1 | Per-directory volume encryption |
| FUSE framework | `fuse3` | 3.17.4 | Dependency of above two |
| LUKS | `cryptsetup` | 2.8.1 | Pool-level LUKS (to become per-volume) |
| Device mapper | `device-mapper` | 2.03.29 | Explicit in .kiwi ("easier to add encryption later") |
| LVM | `lvm2` | 2.03.29 | Pulled as dependency, not explicit in .kiwi |

Additionally:
- `config.sh:260-262` creates `/piccolo-data` mount point for the old pool-level LUKS partition
- `piccolo-os-support.spec:206-216` configures `fuse.conf` `user_allow_other` for gocryptfs/fuse-overlayfs
- `.kiwi` declares a `piccolo-core` btrfs subvolume (still valid — loop file lives here)

## Changes Required

### 1. Package Additions — `piccolo-os.kiwi`

Add to the `<packages>` section:

```xml
<!-- Block-native storage stack (M1–M4) -->
<package name="lvm2" />                     <!-- Explicit: thin pool management -->
<package name="thin-provisioning-tools" />   <!-- thin_check, thin_dump, thin_restore -->
<package name="drbd-utils" />                <!-- drbdadm, drbdsetup, drbdmeta -->
<package name="nbd" />                       <!-- nbd-client for kernel NBD -->
```

**Kernel modules required** (verify availability in `kernel-default`):
- `dm-thin-pool` — LVM thin provisioning (usually built-in with device-mapper)
- `overlay` — kernel overlayfs (almost always built-in on modern kernels)
- `drbd` — DRBD replication. May need `drbd-kmp-default` on openSUSE if not in `kernel-default`
- `nbd` — Network Block Device. Usually a loadable module in `kernel-default`

If any module is not in `kernel-default`, add the corresponding `-kmp-default` package.

**Open question:** Check whether `drbd-utils` and `nbd` are available in the openSUSE
MicroOS package repos. If not in the standard repos, we may need to add the LINBIT
or OBS repo for DRBD.

### 2. Package Removals — `piccolo-os.kiwi`

Remove from the `<packages>` section:

```xml
<!-- REMOVED: zero-FUSE architecture, no longer needed -->
<!-- <package name="gocryptfs" /> -->        <!-- Replaced by per-volume LUKS2 -->
<!-- <package name="fuse-overlayfs" /> -->    <!-- Replaced by kernel-native overlayfs -->
```

**Note:** `gocryptfs` and `fuse-overlayfs` are not explicitly listed in the current
`.kiwi` — they are pulled as dependencies of `patterns-containers-container_runtime`
or installed by `piccolo-os-support`. Verify how they enter the image:
- If via pattern dependency: they'll remain unless explicitly excluded with `<package name="..." replaces=""/>` or the pattern is trimmed
- If via `piccolo-os-support` Requires: remove the RPM dependency

`fuse3` may still be needed by other MicroOS components — check reverse dependencies
before removing. If nothing else needs it, remove.

### 3. fuse.conf Cleanup — `piccolo-os-support.spec`

**Current** (`%post`, lines 206-216):
```bash
# Enable user_allow_other in fuse.conf so rootless users can access FUSE mounts
# (gocryptfs, fuse-overlayfs) created by root with -allow_other.
```

**Action:** Remove this entire block. With zero FUSE, no rootless user needs FUSE
mount access. If `fuse3` is retained for other reasons, the default `fuse.conf`
(without `user_allow_other`) is fine.

### 4. `/piccolo-data` Mount Point — `config.sh`

**Current** (lines 259-262):
```bash
# Create /piccolo-data mount point for LUKS data partition
mkdir -p /piccolo-data
```

**Action:** Remove. In the block-native architecture:
- `/piccolo-data` was a btrfs-on-LUKS filesystem mount. Now it's an LVM VG — no filesystem, no mount point.
- Volume mounts live under `/piccolo-core/mounts/<vol-id>/`
- The `piccolo-core` btrfs subvolume (already in `.kiwi`) is the control plane root

### 5. Kernel Module Loading — `piccolo-os-support.spec` or systemd config

Ensure modules load at boot. Add a modules-load.d config:

```bash
# /etc/modules-load.d/piccolo-storage.conf
dm-thin-pool
nbd
drbd
overlay
```

This can be added in `piccolo-os-support.spec` `%post` or as a file in the RPM.

### 6. DRBD Configuration Directory

DRBD expects `/etc/drbd.d/` for resource configs. piccolod generates per-volume
resource configs at runtime, but the base directory should exist:

```bash
# In piccolo-os-support.spec %post or config.sh
mkdir -p /etc/drbd.d
```

### 7. NBD Device Nodes

The kernel `nbd` module creates `/dev/nbdN` device nodes. Verify that:
- `nbd.ko` creates enough devices (default is 16, may need `nbds_max=64` module parameter)
- udev rules are in place (usually automatic)

If needed, add module parameter:
```bash
# /etc/modprobe.d/piccolo-nbd.conf
options nbd nbds_max=64
```

### 8. Documentation — README.md

**Current** (lines 116, 143):
```
Strong data protection: Per-directory encryption (gocryptfs-style)...
Encrypted volumes: Per-directory encryption with gated unlock...
```

**Update to:**
```
Strong data protection: Per-volume LUKS2 encryption, password-derived keys,
optional TPM assist, and a recovery key.
...
Encrypted volumes: Per-volume LUKS2 encryption with gated unlock and recovery key support.
```

## File Inventory

| File | Repo | Action |
|---|---|---|
| `kiwi/microos-ots/piccolo-os.kiwi` | piccolo-os | Add lvm2, thin-provisioning-tools, drbd-utils, nbd; verify gocryptfs/fuse-overlayfs removal |
| `kiwi/microos-ots/config.sh` | piccolo-os | Remove `/piccolo-data` mkdir |
| `packages/piccolo-os-support/piccolo-os-support.spec` | piccolo-os | Remove fuse.conf block; add modules-load.d config; add /etc/drbd.d |
| `README.md` | piccolo-os | Update encryption description |

## Verification

1. Build kiwi image: `kiwi-ng system build` completes without errors
2. Boot image in VirtualBox: all kernel modules load (`lsmod | grep -E 'drbd|nbd|dm_thin|overlay'`)
3. `which drbdadm drbdsetup thin_check nbd-client cryptsetup` — all present
4. `rpm -q gocryptfs fuse-overlayfs` — not installed
5. `/piccolo-data` does not exist as a directory
6. `/etc/drbd.d/` exists
7. `cat /etc/modules-load.d/piccolo-storage.conf` — lists all 4 modules
8. `piccolod` starts and passes `RequireNativeOverlay()` check
9. Full app lifecycle: install → start → stop → uninstall works with block-native stack

## Open Questions

1. **DRBD package availability**: Is `drbd-utils` in the standard openSUSE MicroOS repos,
   or do we need the LINBIT repo? MicroOS may not ship DRBD by default.
2. **NBD package**: Same question — is `nbd` (nbd-client) in the standard repos?
3. **fuse3 reverse dependencies**: Does anything in the MicroOS base pattern still need fuse3?
   If so, we can leave it but remove `user_allow_other`.
4. **Kernel module availability**: Are `drbd.ko` and `nbd.ko` in `kernel-default` or do
   they require separate `-kmp-default` packages?
5. **Image size impact**: Adding DRBD + NBD adds ~5-10 MB. Removing gocryptfs + fuse-overlayfs
   saves ~10-15 MB. Net impact should be roughly neutral.
