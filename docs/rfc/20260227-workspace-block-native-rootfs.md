# RFC: Unified Block-Native Rootfs — Golden LV, Single LUKS Key

**Date:** 2026-02-27
**Status:** Draft
**Supersedes:** `20260101-workspace-disk-container-independent.md` (overlay-based workspace persistence)
**Breaks:** This is a clean break from the current overlay-based rootfs and per-volume LUKS key model. No migration code, no dual-path logic. Existing volumes must be destroyed and recreated.

## 1. Summary

Replace overlay-based container rootfs management (both workspace and service) with a unified golden LV architecture: a flattened OCI image on a dm-thin LV serves as a template, dm-thin snapshots provide per-container rootfs instances, and idmapped mounts handle UID translation. A single LUKS master key encrypts all volumes.

This eliminates overlay filesystems, FUSE, fuse-overlayfs, `additionalimagestore`, and per-app `storage.conf` from the entire stack. Containers become thin wrappers around mounted block devices via `podman create --rootfs`.

## 2. Motivation

### 2.1 Current Architecture Pain

Both service and workspace containers use fuse-overlayfs to layer image content with per-app writable state. This was implemented as a workaround during block-native M4 (see testing journal in `20260226-block-native-implementation.md`).

```
Image layers (lowerdir, owned by piccolo-runtime UID 470)
  + Upper dir (per-app writable layer)
  + Work dir
  = fuse-overlayfs merged rootfs
```

### 2.2 The Cross-User UID Problem

The FUSE workaround exists because of a fundamental UID range incompatibility in overlayfs, not merely a rootless podman limitation.

**The setup:** `piccolo-runtime` (UID 470, subuid 200000-265535) pulls images. On disk, image layers have piccolo-runtime's remapped UIDs: UID 470 for container root, UID 200033 for www-data, etc. Per-app containers run as separate users (e.g., UID 468, subuid 300000-365535).

**The constraint:** A kernel overlay with lower layers from piccolo-runtime (UIDs 470, 200000+) and upper from the per-app user (UIDs 468, 300000+) cannot be reconciled through a single `mount_setattr` idmap. The idmap requires non-overlapping source→target ranges. Mapping both `470→468` (from lower) and `468→468` (from upper) produces overlapping targets from different sources. Unmapped UIDs become `nobody`.

**Two-idmap workaround exists but is complex:** Pre-idmap each lower layer (piccolo-runtime→per-app) before creating the overlay, then both layers share the same UID space. This works on kernel 5.19+ (idmapped overlay lower layers) but requires N per-layer idmapped bind mounts, layer path discovery via `podman image inspect`, and overlay mount lifecycle management.

**fuse-overlayfs** solves this in userspace via `squash_to_uid` — arbitrary UID remapping unconstrained by kernel idmap bijectivity. But it's FUSE on the rootfs I/O path.

**The golden LV approach** eliminates the problem entirely: flatten the image via `podman export | tar x --numeric-owner` to produce files with real UIDs (0, 33, etc.) on ext4. One idmap. No overlay. No UID range conflicts.

### 2.3 Why Not VMs?

QEMU/KVM VMs provide persistent rootfs, own UID space, and qcow2 snapshots natively — seemingly a better fit for workspaces. Rejected for one decisive reason:

**GPU sharing on consumer hardware.** Containers share the host kernel's GPU driver — multiple containers access the same GPU simultaneously via device passthrough (`/dev/dri/renderD128`, `/dev/nvidia0`, `/dev/kfd`), with no special hardware, license, or driver required. This is vendor-agnostic:

- **NVIDIA:** Multiple containers share via CUDA scheduler. Works on consumer GeForce. No license.
- **AMD:** Multiple containers share via ROCm/GPU scheduler (`/dev/dri/renderD128` + `/dev/kfd`). Works on consumer RX. No license.
- **Intel iGPU/Arc:** Multiple containers share via `/dev/dri/renderD128`. No special driver needed.

VMs cannot share GPUs this way because each VM runs its own kernel. VM GPU access requires:
- **VFIO passthrough:** exclusive — one VM gets the GPU, others get nothing
- **NVIDIA vGPU:** requires enterprise GPUs (A-series/H-series) + paid license. Not available on consumer GeForce.
- **AMD SR-IOV:** datacenter MI-series only, not consumer RX GPUs
- **Intel SR-IOV:** requires out-of-tree `i915-sriov-dkms` kernel module (12th gen+), not mainline

For on-device AI agents — where multiple workspace clones need GPU access for inference or fine-tuning — VMs on consumer hardware cannot share a single GPU. This is the decisive factor.

Additional: ~100MB RAM per VM kernel, TAP/bridge networking complexity, two runtime management paths (podman + QEMU).

### 2.4 Design Goals

1. **Zero overlay, zero FUSE** for all container rootfs I/O (workspace and service)
2. **Unified architecture** — one rootfs model for both container modes
3. **Instant workspace cloning** via dm-thin snapshots for AI agent forking
4. **Multi-UID semantics preserved** via kernel idmapped mounts (no UID squashing)
5. **Single LUKS key** — simplified key management, enables golden LV sharing
6. **Service rootfs read-only** — correctness invariant for cluster (no divergence without DRBD)
7. **DRBD + NBD always present** for replicated volumes (standalone on single-node)
8. **No migration, no fallbacks** — clean break, hard fail if kernel features unavailable

## 3. Volume Types

### 3.1 Golden LV — Template (A)

One golden LV per unique OCI image. Contains the flattened image on ext4.

```
dm-thin LV → LUKS → ext4 → flattened image
```

- Created at first install of an image, shared by all containers using that image
- Never mounted during normal operation (only during creation and image updates)
- Not replicated (no NBD, no DRBD) — reconstructable from the OCI image at any time
- Snapshots provide service rootfs (C) and workspace (B) instances

### 3.2 Workspace (B)

Snapshot of a golden LV. Contains the complete rootfs + user data. Replicated.

```
dm-thin snapshot of A → NBD → DRBD → LUKS → ext4 (rw) → idmapped mount → podman --rootfs
```

- Single LV = rootfs + user data. No separate data volume.
- Replicated via DRBD (user data on rootfs). NBD always present.
- Idmapped mount for UID translation (piccolod creates as root)
- DRBD standalone on single-node, connected in cluster mode

### 3.3 Workspace Clone (B-clone)

Snapshot of an existing workspace B at its current state. For AI agent forking.

```
dm-thin snapshot of B → NBD → DRBD → LUKS → ext4 (rw) → idmapped mount → podman --rootfs
```

- Instant creation (dm-thin metadata operation)
- Origin and clone run concurrently
- LUKS UUID change required to avoid collision (§5.4)

### 3.4 Service Rootfs (C)

Read-only snapshot of a golden LV. Provides the immutable base filesystem for service containers.

```
dm-thin snapshot of A → LUKS → ext4 (ro) → idmapped mount → podman --rootfs --read-only
```

- **ext4 mounted read-only** — correctness invariant. Without DRBD, cluster nodes mount independent snapshots. Read-write would allow silent divergence between nodes.
- Not replicated (no NBD, no DRBD) — reconstructable from golden LV
- Writable paths (`/tmp`, `/var/run`, `/dev/shm`) provided as tmpfs by podman
- Persistent state lives on service data volume (D), bind-mounted into the container

### 3.5 Service Data (D)

Fresh dm-thin LV for service persistent state. Standard full-stack app data volume.

```
dm-thin LV → NBD → DRBD → LUKS → ext4 (rw)
```

- Independent volume per service. Replicated via DRBD.
- Bind-mounted into the service container at app-defined paths

### 3.6 Stack Summary

| Volume | dm-thin | NBD | DRBD | LUKS | FS | Mode | Replicated |
|--------|---------|-----|------|------|----|------|------------|
| A (golden) | LV | — | — | yes | ext4 | template | No |
| B (workspace) | snapshot of A | yes | yes | yes | ext4 rw | live | Yes |
| B-clone | snapshot of B | yes | yes | yes | ext4 rw | live | Yes |
| C (svc rootfs) | snapshot of A | — | — | yes | ext4 ro | live | No |
| D (svc data) | LV | yes | yes | yes | ext4 rw | live | Yes |
| Ephemeral | LV | — | — | — | btrfs+zstd | live | No |

## 4. Block Device Stack

### 4.1 dm-vdo Removed

dm-vdo was originally planned above LUKS for transparent compression. **Removed** because the stacking order required by LUKS (dm-vdo above LUKS, above dm-thin) causes compound write amplification: VDO's allocate-on-write CoW compounds with dm-thin snapshot CoW. Red Hat recommends dm-thin above VDO, but our LUKS requirement forces the opposite order. The compression benefit (5-30 GB at piccolo's scale) doesn't justify the write amplification cost on workspaces, especially given SSD wear on edge hardware.

**TRIM chain:** ext4 `mount -o discard` → LUKS `--allow-discards` → DRBD `rs-discard-granularity` → NBD `BLKDISCARD` → dm-thin deallocates. **Critical:** All `cryptsetup open` invocations MUST include `--allow-discards`. Without it, TRIM stops at LUKS and the thin pool fills monotonically — fatal on edge hardware with small storage.

### 4.2 Single LUKS Key

All volumes share a single LUKS2 master key. This is a deliberate simplification from the org-context §5.1 per-volume key model.

**Key hierarchy:**
```
Admin password → KEK (Argon2id) → SDEK → single LUKS master key (wraps all volumes)
```

The master key is stored once in the control plane, wrapped by SDEK. Every volume's LUKS header contains the same master key, encrypted by the same keyslot passphrases.

**Keyslots (per LUKS header):**
- Keyslot 0: master key encrypted by SDEK-derived passphrase (normal boot path)
- Keyslot 1: master key encrypted by admin-password-derived passphrase (offline recovery)
- Keyslot 2: master key encrypted by recovery-mnemonic-derived passphrase (password lost)

Since the master key is the same, all keyslots are functionally identical across volumes.

**Volume creation with shared key:**
- Golden LV (A): `cryptsetup luksFormat --master-key-file <shared-key>`, add keyslots 1 and 2
- Snapshots (B, C): inherit LUKS header from A. Change UUID only (`cryptsetup luksUUID`)
- Fresh volumes (D): `cryptsetup luksFormat --master-key-file <shared-key>`, add keyslots

**Threat model analysis:**

| Scenario | Per-volume keys | Single key | Difference |
|----------|----------------|------------|------------|
| Physical disk theft | Need admin password | Need admin password | None |
| Admin password compromised | SDEK unwraps ALL volume keys | Same | None |
| piccolod process compromised | All mounted plaintext accessible | Same | None |
| Single key leaked from RAM | One volume exposed | All volumes exposed | Worse |
| Crypto-shredding on uninstall | Destroy volume key → ciphertext dead | Can't — key alive in other volumes | Worse |

The only practical losses are per-volume crypto-shredding and single-key-leak blast radius. For piccolo's threat model (home device, no SSH/console, physical extraction is primary attack vector):
- The admin password gates everything regardless of key model
- If an attacker can extract a LUKS key from RAM, they likely have full process access (→ SDEK → all keys anyway)
- Crypto-shredding of individual volumes is a nice-to-have, not a hard requirement

**What single key enables:**
- Golden LV snapshots work for both workspace and service (no master key isolation concern)
- One key to wrap, store, unwrap, rotate
- Simpler boot sequence
- Clone LUKS handling is just UUID change (no re-keying)

**Admin password rotation:** Update keyslot 1 on all volumes. Same effort as per-volume keys (each volume's keyslot 1 must be updated). With single key, could optimize by skipping keyslot updates on reconstructable volumes (A, C) — their keyslots are only needed for offline recovery, and they can be recreated.

### 4.3 DRBD and NBD

**Always present** for replicated volumes (B, D). This aligns with org-context §6.6: "Full stack always built for app data volumes. Idle DRBD + idle NBD: negligible overhead."

- **Single-node:** DRBD runs standalone (`drbdadm disconnect`), NBD serves the volume. One code path.
- **Cluster mode:** DRBD connected, replicates ciphertext. `drbdadm connect` to enable.
- **Runtime toggleable:** No stack reconstruction. Replication on/off is `drbdadm connect/disconnect`.

**Not present** for non-replicated volumes (A, C, Ephemeral). These are reconstructable — replication adds overhead with no benefit.

### 4.4 Idmapped Mounts

piccolod (root, `CAP_SYS_ADMIN`) creates a user namespace to hold the UID map and applies it to the ext4 mount via `mount_setattr(MOUNT_ATTR_IDMAP)`.

#### 4.4.1 User Namespace Creation

1. `clone3(CLONE_NEWUSER)` — child process in new user namespace
2. Write UID/GID maps to `/proc/<child>/uid_map` and `/proc/<child>/gid_map`
3. Open `/proc/<child>/ns/user` → `userns_fd`
4. Child exits; userns alive via fd

No `newuidmap` binary needed — piccolod writes maps directly (avoids file-capability + setuid conflicts from alpha testing).

**UID map for per-app user `pa-code-server` (UID 468, subuid 200000-265535):**
```
# /proc/<child>/uid_map
0 468 1           # on-disk root → host per-app user
1 200000 65535    # on-disk 1-65535 → host subuid range

# /proc/<child>/gid_map (same structure)
0 468 1
1 200000 65535
```

#### 4.4.2 Applying the Idmap

```go
fd, _ := unix.OpenTree(unix.AT_FDCWD, mountpoint,
    unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_RECURSIVE)

unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH, &unix.MountAttr{
    AttrSet:   unix.MOUNT_ATTR_IDMAP,
    Userns_fd: uint64(usernsFd),
})

unix.MoveMount(fd, "", unix.AT_FDCWD, targetPath, unix.MOVE_MOUNT_F_EMPTY_PATH)
```

The `mount_setattr` call copies the idmap into the mount's internal state. After completion, the userns fd can be closed — the idmap is owned by the mount. piccolod restart does not invalidate active idmapped mounts.

#### 4.4.3 Interaction with Podman User Namespace

`podman create --rootfs` uses the provided path as-is. The kernel idmap translates on-disk UIDs to host UIDs at the VFS layer. Podman's user namespace maps host UIDs to container UIDs. The two compose correctly:

```
On-disk UID 0 → idmap → host UID 468 → podman userns → container UID 0
```

No double-mapping — idmap operates at VFS (before userspace), userns at process layer. Orthogonal translations.

This preserves multi-UID semantics: inside the container, `/etc` is owned by root, `/var/www` by www-data. No squashing.

#### 4.4.4 No Kernel Fallback

If `mount_setattr` returns `ENOSYS` or `EINVAL`, piccolod fails hard. No fuse-overlayfs fallback. MicroOS ships kernel 6.x (ext4 idmap stable since 5.12, overlay idmap since 5.19). Hard dependency is acceptable for the target platform.

## 5. Lifecycle

### 5.1 Golden LV Creation

When a workspace or service is installed and no golden LV exists for the image:

1. Check thin pool capacity — abort if data > 85% or metadata > 75%
2. `lvcreate --thin` — new thin LV (default 10 GiB virtual, thin-provisioned)
3. `cryptsetup luksFormat --master-key-file <shared-key>` → `luksOpen`
4. `mkfs.ext4` on LUKS device, mount
5. Write sentinel: `.piccolo_flatten_incomplete`
6. Flatten image:
   ```
   cid=$(podman --root <runtime-root> create <image> true)
   podman --root <runtime-root> export $cid | tar x --numeric-owner --xattrs --xattrs-include='*' -C /mnt/golden-<id>/
   podman --root <runtime-root> rm $cid
   ```
   `podman create/export` run as `piccolo-runtime` to access the shared imagestore. `tar x --numeric-owner` runs as root to preserve original on-disk UIDs (0, 33, etc.). `--xattrs --xattrs-include='*'` preserves extended attributes including `security.capability` (needed for binaries like `ping` with `cap_net_raw`) and SELinux labels.
7. `syncfs(mount_fd)`
8. Write golden LV metadata (`piccolo.volume.json`) via `fsutil.AtomicWriteFile`
9. Remove `.piccolo_flatten_incomplete` sentinel
10. Unmount ext4, close LUKS, deactivate LV

**Partial flatten recovery:** On startup, `ReconcileAllVolumeStates` destroys golden LVs with `.piccolo_flatten_incomplete` present. Sentinel written before extraction, removed after fsync.

### 5.2 Golden LV Image Update

When a pulled image has a new digest for the same ref:

1. Create a NEW golden LV for the new digest (§5.1)
2. For each service using the old golden LV: stop container → destroy old C snapshot → create new C snapshot from new golden LV → restart
3. Workspaces are NOT affected — B is a diverged snapshot with user data. Users create new workspaces from the updated image if desired.
4. Once no snapshots reference the old golden LV, garbage collect it (§5.3)

### 5.3 Golden LV Garbage Collection

On startup and after service/workspace uninstalls, scan for golden LVs with zero active snapshots (no B or C volumes reference it). Destroy: close LUKS + `lvremove` + remove metadata.

### 5.4 Workspace Creation

1. Ensure golden LV exists for the image (create if not — §5.1)
2. `lvcreate --snapshot --name ws-<id> <vg>/golden-<image-id>` — instant
3. `lvchange -ay <vg>/ws-<id>` — activate snapshot
4. `cryptsetup luksUUID --uuid $(uuidgen) /dev/<vg>/ws-<id>` — unique UUID (avoids collision with golden LV and other snapshots). If UUID assignment fails, destroy the snapshot LV immediately (`lvremove`) to prevent two LVs with identical LUKS UUIDs.
5. Write workspace metadata
6. Attach: NBD → DRBD (standalone) → `cryptsetup open --allow-discards` → `mount -o discard ext4` → idmapped mount
7. `podman create --rootfs <idmapped-path>`

### 5.5 Workspace Cloning

```
lvcreate --snapshot --name ws-<clone-id> <vg>/ws-<origin-id>
```

Instant dm-thin metadata operation. Clone shares all unchanged blocks with origin via CoW.

**Clone LUKS handling (simplified with single key):**

1. `lvcreate --snapshot` — create clone LV
2. `lvchange -ay <vg>/ws-<clone-id>`
3. `cryptsetup luksUUID --uuid $(uuidgen) /dev/<vg>/ws-<clone-id>` — unique UUID

No re-keying needed — single master key, all volumes share it. Just prevent UUID collision.

If UUID assignment fails, destroy the clone LV immediately.

**Concurrent operation:** Origin and clone run simultaneously. Different LUKS UUIDs → different mapper names (`piccolo-vol-ws-<origin>` vs `piccolo-vol-ws-<clone>`). dm-thin handles concurrent CoW correctly.

**Clone lifecycle:**
- **Fork:** pool capacity check → `lvcreate --snapshot` → UUID change → metadata → attach → start
- **Discard:** stop → detach → `lvremove`
- **Promote:** stop both → swap metadata references → archive/destroy old origin
- **Origin uninstall with active clones:** permitted — dm-thin snapshots are independent after creation. Clone continues to function. `clone_of` becomes dangling reference (informational only).

### 5.6 Service Rootfs Creation

1. Ensure golden LV exists for the image (§5.1)
2. `lvcreate --snapshot --name svc-rootfs-<id> <vg>/golden-<image-id>`
3. `cryptsetup luksUUID --uuid $(uuidgen) /dev/<vg>/svc-rootfs-<id>` — if UUID assignment fails, destroy the snapshot LV immediately (`lvremove`)
4. Write service rootfs metadata
5. Attach: `cryptsetup open --allow-discards` → `mount -o ro,discard ext4` → idmapped mount
6. `podman create --rootfs <idmapped-path> --read-only`

**Writable paths:** Podman provides tmpfs for `/tmp`, `/var/run`, `/dev/shm`. Additional writable mounts configured per-app (bind mounts from data volume D). This is standard read-only root filesystem behavior (same as Kubernetes `readOnlyRootFilesystem: true`).

### 5.7 Replication

Workspace (B) and service data (D) volumes use the full replication stack:

```
thin LV → NBD → DRBD → LUKS → ext4
```

One DRBD resource per volume. Since workspaces are now a single LV (not rootfs + data), replication is simpler — one resource covers the entire workspace state.

### 5.8 Teardown Sequence

**Workspace / service data (B, D):**
1. Stop container (`podman stop --timeout 30`, force-kill if needed)
2. Unmount idmapped bind mount (if applicable). EBUSY → `MNT_DETACH`.
3. Unmount ext4
4. Close LUKS mapper
5. DRBD down (if cluster)
6. NBD disconnect
7. Deactivate thin LV

**Service rootfs (C):**
1. Stop container
2. Unmount idmapped bind mount
3. Unmount ext4 (ro)
4. Close LUKS mapper
5. Deactivate thin LV

**Crash recovery and mount discovery:** Idmapped mounts survive piccolod crashes (they are kernel mount state, not process state). On restart, `ReconcileAllVolumeStates` discovers existing mounts by scanning `/proc/self/mountinfo` for active mount entries at known paths (`/piccolo-core/mounts/<vol-id>`). If a mount is already active, the attachment sequence skips the mount steps and reuses it. For partially-attached stacks (e.g., LUKS open but ext4 not mounted), reconciliation tears down to a known-clean state (close all layers) and re-attaches from scratch. The golden LV sentinel check (`.piccolo_flatten_incomplete`) also performs a full teardown of any partially-attached golden LV stack before `lvremove`.

## 6. Alternatives Considered

### 6.1 Two-Idmap Kernel Overlayfs

Pre-idmap each image lower layer (piccolo-runtime UIDs → per-app UIDs) via `mount_setattr`, then create kernel overlayfs with aligned UID spaces. Technically correct on kernel 5.19+.

**Not chosen because:**
- N per-layer idmapped bind mounts per container (5-20 layers typical)
- Layer path discovery depends on podman storage internals
- Overlay mount lifecycle management
- Cannot enable workspace cloning (no CoW snapshot of overlay upper)
- Golden LV is architecturally cleaner: one flatten, one mount, no overlay

### 6.2 QEMU/KVM VMs for Workspaces

See §2.3. Containers share the host GPU driver across all vendors (NVIDIA, AMD, Intel). VMs require hardware-level partitioning (VFIO exclusive, vendor-specific SR-IOV/vGPU) not available on consumer GPUs.

### 6.3 Per-Volume LUKS Keys

See §4.2 threat model analysis. Per-volume keys provide crypto-shredding and reduced blast radius on key leak, but both scenarios are marginal for piccolo's threat model. The simplification of a single key (enabling golden LV sharing, simpler key management, simpler boot) outweighs the marginal security benefit.

### 6.4 OverlayBD

containerd plugin for block-level OCI layers. Rejected: not podman-native, heavy integration, low ROI at piccolo's app count.

## 7. Volume Metadata Schema

### 7.1 Global LUKS Key (in control plane)

```json
{
  "luks_master_key": {
    "wrapped_key": "base64...",
    "nonce": "base64...",
    "key_version": 1
  }
}
```

One entry in the control plane. All volumes reference this key.

### 7.2 Golden LV Metadata

```json
{
  "version": 3,
  "type": "golden",
  "lv_name": "golden-ubuntu-22.04-abc123",
  "vg_name": "piccolo-data-vg",
  "size_bytes": 10737418240,
  "fs_type": "ext4",

  "base_image_digest": "docker.io/library/ubuntu@sha256:...",
  "base_image_ref": "ubuntu:22.04"
}
```

### 7.3 Workspace Metadata

```json
{
  "version": 3,
  "type": "workspace",
  "lv_name": "ws-code-server-abc123",
  "vg_name": "piccolo-data-vg",
  "size_bytes": 10737418240,
  "fs_type": "ext4",

  "golden_lv": "golden-ubuntu-22.04-abc123",
  "base_image_digest": "docker.io/library/ubuntu@sha256:...",
  "base_image_ref": "ubuntu:22.04",
  "clone_of": "",
  "idmap": {
    "app_uid": 468,
    "app_gid": 468,
    "subuid_start": 200000,
    "subuid_count": 65535,
    "subgid_start": 200000,
    "subgid_count": 65535
  }
}
```

### 7.4 Service Rootfs Metadata

```json
{
  "version": 3,
  "type": "service-rootfs",
  "lv_name": "svc-rootfs-nextcloud-abc123",
  "vg_name": "piccolo-data-vg",
  "fs_type": "ext4",

  "golden_lv": "golden-nextcloud-abc123",
  "read_only": true,
  "idmap": {
    "app_uid": 467,
    "app_gid": 467,
    "subuid_start": 265536,
    "subuid_count": 65535,
    "subgid_start": 265536,
    "subgid_count": 65535
  }
}
```

### 7.5 Service Data Metadata

```json
{
  "version": 3,
  "type": "service-data",
  "lv_name": "vol-nextcloud-abc123",
  "vg_name": "piccolo-data-vg",
  "size_bytes": 53687091200,
  "fs_type": "ext4",

}
```

### 7.6 LV Naming Conventions

| Volume type | LV name pattern | LUKS mapper pattern |
|-------------|----------------|---------------------|
| Golden | `golden-<image-short-id>` | `piccolo-vol-golden-<id>` |
| Workspace | `ws-<instance-id>` | `piccolo-vol-ws-<id>` |
| Service rootfs | `svc-rootfs-<instance-id>` | `piccolo-vol-svc-rootfs-<id>` |
| Service data | `vol-<instance-id>` | `piccolo-vol-<id>` |
| Ephemeral | `eph-<instance-id>` | N/A (no LUKS) |

## 8. Implementation Plan

### Phase 1: Syscall Integration

- `internal/fsutil/idmap.go` — `mount_setattr` wrapper, userns creation, UID map construction
- Integration tests (root required): idmapped ext4 mount
- ~150 lines of Go

### Phase 2: Golden LV and Workspace Lifecycle

- Golden LV manager: create, flatten, update, garbage collect
- Workspace creation from golden LV snapshot
- `podman create --rootfs` integration with idmapped mounts
- Service rootfs creation from golden LV snapshot (read-only mount)
- Volume metadata schema v3
- Integration tests: golden LV → snapshot → mount → podman rootfs

### Phase 3: Workspace Cloning

- `lvcreate --snapshot` of existing workspace
- Clone UUID handling
- API endpoints: clone, discard, promote
- Integration with AI agent workspace forking

### Phase 4: Cleanup

- Remove `workspacedisk/` overlay machinery (mount.go, layout, stale cleanup)
- Remove `workspace_disk_integration.go` overlay-specific code
- Remove fuse-overlayfs dependency entirely
- Remove `additionalimagestore` configuration
- Remove per-app `storage.conf` mount_program/force_mask
- Update alpha test suite

## 9. What This Eliminates

| Removed | Replaced by |
|---------|-------------|
| fuse-overlayfs (both modes) | ext4 + idmapped mount |
| overlayfs entirely | dm-thin snapshot = writable layer |
| `additionalimagestore` config | Golden LV (shared image template) |
| Per-app `storage.conf` with `mount_program` | `podman --rootfs` (bypasses storage driver) |
| `workspacedisk/` overlay machinery (~500 lines) | dm-thin snapshot + mount (~150 lines) |
| `workspace_disk_integration.go` overlay code | Golden LV flatten pipeline |
| UID squashing (`squash_to_uid`) | Kernel idmapped mount (multi-UID preserved) |
| Separate rootfs + data volume for workspaces | Single LV per workspace |
| Per-volume LUKS key management | Single shared key |

## 10. Risks

1. **`mount_setattr` hard dependency.** Kernel 5.12+ for ext4 idmap. MicroOS ships 6.x — no risk for target platform. No fallback by design.

2. **`podman --rootfs` limitations.** OCI image config (ENTRYPOINT, CMD, ENV) not applied by `--rootfs`. Already handled — `meta.json` stores image config, container creation applies it explicitly.

3. **Flatten cost.** ~30s per unique image (one-time). Image pull dominates install time. Subsequent containers from the same image are instant snapshots.

4. **Single LUKS key blast radius.** Key leak from RAM exposes all volumes. Mitigated: if attacker has RAM access, they have process access → SDEK → all keys anyway. See §4.2.

5. **Service rootfs writable paths.** Containers expecting to write to rootfs paths not covered by tmpfs/bind mounts will get EROFS. Requires per-image analysis of writable path needs. Standard practice for read-only root filesystem containers.

6. **Golden LV storage.** One golden LV per unique image, even if only one container uses it. At ~1-3 GiB per image (thin-provisioned), this is acceptable for piccolo's app count (5-20 apps).

7. **Image update coordination.** Updating a golden LV requires stopping all service containers using it, re-creating snapshots, and restarting. More involved than `podman pull` + restart. Acceptable because image updates are infrequent and the coordination is automatable.

## 11. Operational Readiness

### 11.1 Observability

```
INFO: golden-lv <id>: flatten started (image=<ref>)
INFO: golden-lv <id>: flatten complete (<duration>, <bytes>)
INFO: workspace <id>: created from golden-lv <golden-id> (snapshot, instant)
INFO: workspace <id>: idmapped mount at <path> (uid_map: 0→<host_uid>, 1-65535→<subuid_start>)
INFO: workspace <id>: clone <clone-id> created (snapshot, instant)
INFO: service-rootfs <id>: created from golden-lv <golden-id> (snapshot, read-only)
INFO: service-data <id>: created (fresh thin LV, <size>)
ERROR: golden-lv <id>: flatten interrupted, marked for cleanup
ERROR: mount_setattr failed: <errno translation> — piccolod cannot start
```

### 11.2 Error Translation

- `ENOSYS` → "kernel does not support idmapped mounts (need 5.12+) — cannot proceed"
- `EINVAL` → "invalid UID map — check subuid allocation for <username>"
- `EPERM` → "missing CAP_SYS_ADMIN — piccolod must run as root"

### 11.3 Diagnostics

Expose via storage inspection API:
- Golden LV inventory: image ref, digest, compressed size, snapshot count
- Per-volume: type, LV name, LUKS mapper, mount status
- Thin pool: data%, metadata%, per-type breakdown (golden, workspace, service-rootfs, service-data, ephemeral)
- Clone relationships: origin → clone graph
