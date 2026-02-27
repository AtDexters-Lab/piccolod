# RFC: Workspace Block-Native Rootfs — Single-LV Architecture with dm-thin Cloning

**Date:** 2026-02-27
**Status:** Draft
**Supersedes:** `20260101-workspace-disk-container-independent.md` (overlay-based workspace persistence)

## 1. Summary

Replace the overlay-based workspace persistence model (overlayfs upper/lower on a data volume) with a single block device per workspace: a dm-thin LV containing the complete rootfs and user data. Containers mount this directly via `podman create --rootfs`. Workspace cloning is achieved via instant dm-thin snapshots.

This eliminates overlay filesystems, FUSE, and UID mapping from the workspace path entirely. The container becomes a thin wrapper around a mounted block device.

## 2. Motivation

### 2.1 Current Architecture Pain

The current workspace model (RFC `20260101`) uses overlayfs to layer a writable upper dir on top of shared base image layers:

```
Image layers (lowerdir, shared, owned by piccolo-runtime UID 470)
  + Upper dir (on per-app LUKS+ext4 data volume)
  + Work dir
  = Merged rootfs (overlay mount)
```

This works but requires solving cross-user UID ownership at the overlay layer:

1. **Rootless podman UID mismatch.** Image layers are stored with `piccolo-runtime` (UID 470) rootless-remapped UIDs. Per-app containers run as separate users (UID 468+). Kernel overlayfs cannot translate UIDs across these users.

2. **`mount_setattr(MOUNT_ATTR_IDMAP)` blocked by podman.** The kernel supports idmapped overlay mounts (5.19+), but `containers/storage` hardcodes `Supports shifting: false` when `euid != 0`. Per-app podman runs rootless — this path is unreachable regardless of kernel capability.

3. **fuse-overlayfs as workaround.** Both service and workspace containers use fuse-overlayfs to handle cross-user image access. This reintroduces FUSE to the rootfs path — the thing the block-native stack was designed to eliminate.

4. **Workspace persistence complexity.** The `workspacedisk/` package manages overlay layout, metadata, mount/unmount lifecycle, stale mount cleanup, base image layer resolution via `podman image inspect`, and fuse-overlayfs UID squashing. This is ~500 lines solving a problem that block devices don't have.

5. **No cloning capability.** Overlay-based workspaces cannot be cloned without copying the entire upper directory. With on-device AI agents, instant workspace forking (agent experiments in isolation, discards or merges) is a key capability.

### 2.2 Why Not VMs?

QEMU/KVM VMs provide persistent rootfs, own UID space, and qcow2 snapshots natively — seemingly a better fit for workspaces. We evaluated this and rejected it for one decisive reason:

**GPU sharing on consumer hardware.** Multiple containers can share a single NVIDIA GPU simultaneously via CUDA time-slicing (standard since Pascal architecture, no license required). VMs require VFIO passthrough (exclusive — one VM gets the GPU) or NVIDIA vGPU (requires enterprise GPUs + paid license). On consumer GeForce hardware, which is what piccolo users have, only one VM can access the GPU at a time.

For on-device AI agents — where multiple workspace clones need GPU access for inference or fine-tuning — this is a non-starter.

Additional VM disadvantages:
- ~100MB RAM overhead per VM kernel (significant at 10-20 workspaces on edge hardware)
- TAP/bridge networking complexity vs container port mapping
- Out-of-tree kernel modules for Intel iGPU SR-IOV sharing
- Separate runtime from service mode containers (two management paths)

### 2.3 Design Goals

1. **Zero overlay, zero FUSE** for workspace rootfs I/O
2. **Instant workspace cloning** via dm-thin snapshots
3. **Proper multi-UID semantics** preserved (root owns `/etc`, www-data owns `/var/www`)
4. **Compatible with existing storage stack** (thin LV → NBD → DRBD → LUKS → ext4)
5. **Service mode unaffected** — podman continues managing service container images

## 3. Proposed Architecture

### 3.1 Workspace Volume

Each workspace gets a single dm-thin LV containing the complete filesystem:

```
dm-thin LV (workspace)
  → LUKS2 (per-volume encryption, same key management as today)
  → ext4
  → idmapped mount (mount_setattr from piccolod, running as root)
  → podman create --rootfs /mnt/workspace-<id>
```

No overlay. No FUSE. No separate data volume. The rootfs IS the persistent state.

### 3.2 Idmapped Mount

The ext4 filesystem contains files with on-disk UIDs as stored during image flatten (typically UID 0 for root, 33 for www-data, etc.). The container runs in a user namespace where container UID 0 = host UID of the per-app user.

#### 3.2.1 User Namespace Creation

piccolod (running as root with `CAP_SYS_ADMIN` in the init namespace) creates a dedicated user namespace to hold the UID map:

1. `clone3(CLONE_NEWUSER)` — create a child process in a new user namespace
2. Write the UID/GID map to `/proc/<child>/uid_map` and `/proc/<child>/gid_map`
3. Open `/proc/<child>/ns/user` → obtain `userns_fd`
4. Child exits; the userns stays alive as long as the fd (or the idmapped mount referencing it) exists

Writing `uid_map`/`gid_map` from the parent process requires `CAP_SETUID`/`CAP_SETGID` in the parent namespace. piccolod (root) has these. No `newuidmap` binary is needed — direct write from the parent avoids the file-capability + setuid conflicts documented in the alpha testing journal.

**Concrete UID map for a per-app user `pa-code-server` (UID 468, GID 468, subuid range 200000-265535, subgid range 200000-265535):**

The per-app user's primary GID is sourced from `/etc/passwd` (the `Gid` field of `syscall.Credential`). In piccolo's per-app user isolation model (RFC 20260220), each `pa-<app>` user has a dedicated group with the same numeric ID as the UID. The subgid range mirrors the subuid range (allocated from `/etc/subgid`).

```
# /proc/<child>/uid_map: "inside_uid  outside_uid  count"
# Map on-disk UID 0      → host UID 468    (per-app user; container sees UID 0 = root)
# Map on-disk UID 1-65535 → host UIDs 200000-265534 (subuid range; container sees 1-65535)
0 468 1
1 200000 65535
```

```
# /proc/<child>/gid_map: same structure, using per-app GID and subgid range
0 468 1
1 200000 65535
```

The idmap fd lifetime: the `mount_setattr` call copies the idmap from the userns into the mount's internal state. After `mount_setattr` completes, the userns fd can be closed — the idmap is owned by the mount, not the namespace. This means: (a) no long-lived goroutine holding the fd, (b) piccolod restart does not invalidate active idmapped mounts, (c) the mount persists until explicitly unmounted.

#### 3.2.2 Applying the Idmap

```go
fd, _ := unix.OpenTree(unix.AT_FDCWD, mountpoint,
    unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_RECURSIVE)

unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH, &unix.MountAttr{
    AttrSet:  unix.MOUNT_ATTR_IDMAP,
    Userns_fd: uint64(usernsFd),
})

unix.MoveMount(fd, "", unix.AT_FDCWD, targetPath, unix.MOVE_MOUNT_F_EMPTY_PATH)
```

The sequence: `open_tree` (detach mount), `mount_setattr` (apply idmap), `move_mount` (attach at target path). This is the standard kernel-recommended pattern for idmapped mounts.

#### 3.2.3 Interaction with Rootless Podman User Namespace

**Critical question:** Does rootless podman's user namespace double-map UIDs on top of the kernel idmap?

**Answer: No.** `podman create --rootfs` uses the provided path as-is for the container rootfs. The kernel idmap translates on-disk UIDs to host UIDs at the VFS layer. Podman's user namespace then maps host UIDs to container UIDs. The two mappings compose correctly:

```
On-disk UID 0
  → idmap → host UID 468 (per-app user)
  → podman userns → container UID 0 (per-app user's uid_map: "0 468 1")
```

There is no double-mapping because the idmap operates at the VFS layer (before any userspace sees the mount) and the user namespace operates at the process layer (mapping the process's effective UID to kernel UIDs). They are orthogonal translations on different axes.

**Validation required before Phase 2 implementation:** Run `podman create --rootfs /tmp/test-idmap --userns=auto` against a known idmapped ext4 mount and verify file ownership inside the container. This is a Phase 1 deliverable — the `mount_setattr` integration test.

This preserves multi-UID semantics: inside the container, `/etc` is owned by root, `/var/www` by www-data, etc. No squashing.

ext4 idmapped mounts are stable since kernel 5.12. piccolod is root — no rootless podman limitation applies. The `containers/storage` `euid != 0` guard is irrelevant because podman never touches the storage layer for `--rootfs`.

#### 3.2.4 Kernel Fallback

If `mount_setattr` returns `ENOSYS` (kernel < 5.12) or `EINVAL` (unsupported filesystem), fall back to fuse-overlayfs with `squash_to_uid/gid` as implemented today. Detection happens once at startup; the result is cached. Log a clear warning: `"idmapped mount unavailable (kernel too old?), falling back to fuse-overlayfs for workspace rootfs"`.

### 3.3 Image Flatten

At workspace install time, the OCI image is flattened into the thin LV:

1. Check thin pool capacity — abort if data usage > 85% or metadata usage > 75% (configurable thresholds; metadata exhaustion is harder to recover from than data exhaustion, hence the lower threshold)
2. `lvcreate --thin` — new thin LV (default 10 GiB, thin-provisioned)
3. `cryptsetup luksFormat` + `luksOpen` — per-volume LUKS2
4. `mkfs.ext4` + mount
5. Write sentinel: `.piccolo_flatten_incomplete` to the mount root
6. Flatten image (as root, piped from piccolo-runtime's podman):
   ```
   cid=$(podman --root <runtime-root> create <image> true)
   podman --root <runtime-root> export $cid | tar x --numeric-owner -C /mnt/ws-<id>/
   podman --root <runtime-root> rm $cid
   ```
   `podman create` and `export` run under `piccolo-runtime` identity (via `SysProcAttr.Credential`, same as existing image operations) to access the shared imagestore. `tar x --numeric-owner` runs as root (piccolod) to preserve original on-disk UIDs (0 for root, 33 for www-data, etc.) without remapping.
7. `syncfs(mount_fd)` to flush all dirty buffers for the mounted filesystem
8. Write workspace metadata (`piccolo.volume.json`) via `fsutil.AtomicWriteFile`
9. Remove `.piccolo_flatten_incomplete` sentinel

**Ordering note:** metadata is written (step 8) before sentinel removal (step 9). If piccolod crashes between 8 and 9, the next startup sees the sentinel and destroys the volume — conservative but safe. The sentinel is the commit marker, not the metadata file.

The flatten writes the complete merged filesystem tree to ext4. All OCI layers are resolved into a single directory tree. This is a one-time cost during install (~30s for a typical workspace image, dominated by extraction).

**Identity for flatten:** `podman create` runs as `piccolo-runtime` (the shared image runtime user that owns the pulled images). `podman export` produces a tar stream with numeric UIDs preserved from the image. `tar x --numeric-owner` writes files with their original UIDs (0 for root, 33 for www-data, etc.) to the ext4 mount. Since piccolod runs as root, it has permission to create files with any UID.

**Partial flatten recovery:** On startup, `ReconcileAllVolumeStates` checks for workspace LVs with `.piccolo_flatten_incomplete` present. Such volumes are destroyed and their metadata removed. The sentinel is written before extraction begins and removed atomically after fsync, so any interruption (crash, OOM) leaves the sentinel in place.

**Storage deduplication across workspaces:** Each workspace from the same base image stores a full copy of the flattened rootfs. For piccolo's target scale (2-5 workspaces), this is acceptable — a typical workspace image is 1-3 GiB, and thin provisioning means unread blocks are not allocated. For higher workspace density, dm-vdo (§4.3) provides transparent cross-volume deduplication as a future optimization. Workspace cloning (§3.4) from an existing workspace, by contrast, deduplicates at the block level via dm-thin CoW — clones share all unchanged blocks with the origin.

### 3.4 Workspace Cloning

```
lvcreate --snapshot --name ws-<clone-id> <vg>/ws-<origin-id>
```

This is a dm-thin metadata operation — instantaneous regardless of workspace size. The clone shares all unchanged blocks with the origin via CoW. Only blocks written by the clone consume new space.

#### 3.4.1 Clone LUKS Handling

The snapshot copies the LUKS2 header verbatim (same UUID, same key slots). To avoid `cryptsetup` UUID collisions when both origin and clone are open simultaneously, the clone header must be re-keyed and re-UUIDed before first open:

1. `lvcreate --snapshot` — create clone LV
2. `lvchange -ay <vg>/ws-<clone-id>` — activate clone LV (but do NOT `cryptsetup open` yet)
3. `cryptsetup luksUUID --uuid $(uuidgen) /dev/<vg>/ws-<clone-id>` — assign a new UUID to the clone header. This modifies only the header on the raw block device; it does not require the LUKS device to be open.
4. Generate a new random volume key, wrap it with SDEK (same as other per-volume keys)
5. `cryptsetup luksAddKey /dev/<vg>/ws-<clone-id> <new-keyfile> --key-file <origin-keyfile>` — add new key slot using origin's key for authentication
6. `cryptsetup luksKillSlot /dev/<vg>/ws-<clone-id> 0` — remove the origin's key slot
7. Store new wrapped key and nonce in clone's `piccolo.volume.json` metadata

After this sequence, the clone has a distinct LUKS UUID and independent key material. If re-keying fails at any step after `lvcreate --snapshot`, the clone LV is immediately destroyed (`lvremove`) to prevent two LVs with identical LUKS headers from existing.

The origin's key material is retrieved from the origin's volume metadata (`wrapped_key` + `nonce`), unwrapped via the SDEK (crypto manager), and written to a tmpfs file for the `luksAddKey` call. The tmpfs file is securely zeroed after use (same pattern as existing `writeKeyToTmpfsDir`).

Clone mapper names use the clone's volume ID: `piccolo-vol-ws-<clone-id>`, distinct from the origin's `piccolo-vol-ws-<origin-id>`. No collision.

#### 3.4.2 Concurrent Origin + Clone

Both origin workspace and clone workspace can be mounted and running simultaneously. This is the primary use case for AI agent forking. After re-keying (§3.4.1), the origin and clone are independent LUKS devices with different UUIDs and different mapper names. dm-thin handles concurrent CoW correctly — writes to either device allocate new chunks from the pool without affecting the other.

**dm-thin chunk size consideration:** The pool uses 64k chunks (from `pool.go`). A single LUKS sector write (512 bytes) triggers CoW on a 64k chunk. For write-heavy workloads in a clone (compilation, model fine-tuning), this causes write amplification at the block layer. For read-heavy AI inference workloads, it's negligible. The 64k chunk size is a reasonable default; if write amplification becomes a measured problem, the pool can be recreated with larger chunks (at the cost of less granular space allocation).

#### 3.4.3 Clone Lifecycle

- **Fork:** check pool capacity (data < 85%, metadata < 75%) → `lvcreate --snapshot` + re-UUID + re-key LUKS + create metadata → instant clone
- **Discard:** stop container, `cryptsetup close`, `lvremove` → instant cleanup
- **Promote:** stop both containers, swap metadata volume references, archive or destroy the old origin
- **Merge:** application-level — rsync specific paths from clone to origin (conflict resolution is the caller's responsibility, not the storage layer's)
- **Origin uninstall with active clones:** dm-thin snapshots are independent after creation — the clone's data is self-contained (CoW divergence means shared blocks are retained as long as any snapshot references them). Uninstalling the origin while clones exist is permitted. The origin's metadata and LUKS mapper are removed; the clone continues to function. The `clone_of` field in clone metadata becomes a dangling reference (informational only, not load-bearing).

`lvm.LVManager` gains a new method: `CreateSnapshot(ctx, originLV, snapshotName) error`.

### 3.5 Replication

Workspace thin LVs use the same replication path as data volumes:

```
thin LV → NBD → DRBD → LUKS → ext4
```

Since there's now one volume per workspace (not rootfs + data), replication is simpler — one DRBD resource covers the entire workspace state.

### 3.6 Service Mode — No Change

Service mode containers continue using podman's overlay storage driver with the `additionalimagestore` + `mount_program=fuse-overlayfs` configuration from the block-native M4 implementation. Service container rootfs is ephemeral — the overlay approach is the right fit.

The fuse-overlayfs surface in service mode is read-mostly and page-cached. Eliminating it is a future optimization (idmapped overlay mounts from piccolod), not a current priority.

## 4. Alternatives Considered

### 4.1 Idmapped Overlay Mounts (Incremental Fix)

Use `mount_setattr(MOUNT_ATTR_IDMAP)` on the existing overlay mount to translate UIDs without FUSE. This would eliminate fuse-overlayfs while keeping the overlay architecture.

**Rejected because:**
- Solves only the UID problem, not the persistence complexity or cloning capability
- Overlay idmap support is newer (kernel 5.19) and less tested than ext4 idmap (5.12)
- Still requires the full `workspacedisk/` overlay machinery
- Cannot enable workspace cloning (no CoW snapshot of an overlay upper dir)

### 4.2 QEMU/KVM VMs for Workspaces

Replace container-based workspaces with lightweight VMs. Persistent rootfs, own UID space, and qcow2 snapshots are all native.

**Rejected because:**
- **GPU sharing on consumer hardware is impossible.** VFIO gives exclusive access to one VM. NVIDIA vGPU requires enterprise GPUs + license. Multiple containers share a GPU natively via CUDA time-slicing.
- ~100MB RAM overhead per VM kernel
- Requires TAP/bridge networking or vsock, replacing container port mapping
- Two runtime management paths (podman + QEMU) instead of one

### 4.3 dm-vdo Under Thin Pool

Add VDO (Virtual Data Optimizer) under the thin pool for block-level deduplication and compression across all volumes. This would deduplicate identical blocks across different workspace images.

**Deferred (not rejected):**
- ~1GB RAM per 1TB for dedup index — significant on edge hardware
- CPU overhead on every write (SHA-256 hash + compression)
- Can be added later as `physical → VDO → thin pool → thin LVs` without changing the workspace architecture
- Evaluate after measuring real workload patterns on target hardware

### 4.4 OverlayBD (Block-Level Image Layers)

Use OverlayBD to represent each OCI layer as a block device, stacked via device-mapper. Preserves per-layer sharing across different images.

**Rejected because:**
- containerd plugin, not podman-native — heavy integration effort
- Adds block-level layer stacking complexity (what overlayfs does but at a lower level)
- Piccolo runs a small number of apps — per-layer cross-image dedup has low ROI

## 5. Implementation Plan

### Phase 1: `mount_setattr` Syscall Support

Implement the raw `mount_setattr(2)` wrapper in Go:
- `internal/fsutil/idmap.go` — syscall wrapper, userns fd creation, UID map construction
- Unit tests with real mounts (requires root, integration test tag)
- ~100-150 lines of Go

### Phase 2: Workspace LV Lifecycle

Replace `workspacedisk/` overlay machinery with thin LV management:
- Image flatten pipeline (`podman create` + `podman export` + `tar x`)
- Workspace thin LV creation, LUKS, ext4, idmapped mount
- `podman create --rootfs` integration
- Metadata migration (reuse `meta.json` schema, drop overlay-specific fields)

### Phase 3: Workspace Cloning

- `lvcreate --snapshot` for instant clone
- Clone metadata and LUKS key management
- API endpoints for clone/discard/promote
- Integration with AI agent workspace forking

### Phase 4: Cleanup

- Remove `workspacedisk/` overlay machinery (mount.go, layout, stale cleanup)
- Remove `workspace_disk_integration.go` overlay-specific code
- Remove fuse-overlayfs dependency for workspace mode
- Update alpha test suite

## 6. Volume Metadata Schema

### 6.1 New Volume Type

Workspace rootfs LVs use a new metadata type to distinguish them from data volumes:

```json
{
  "version": 2,
  "type": "luks-workspace",
  "wrapped_key": "...",
  "nonce": "...",
  "lv_name": "ws-code-server-abc123",
  "vg_name": "piccolo-data-vg",
  "size_bytes": 10737418240,
  "fs_type": "ext4",
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

The `idmap` block stores the UID/GID map parameters used to construct the idmapped mount. These are captured at flatten time from the per-app user's `/etc/passwd` and `/etc/subuid`/`/etc/subgid` entries. Storing them in metadata ensures the idmapped mount is reconstructable across piccolod restarts and is stable even if the per-app user's subuid allocation were to change. The image-related fields (`base_image_digest`, `base_image_ref`) reuse the existing `workspacedisk.WorkspaceMeta` / `ImageConfig` types rather than defining parallel structs.

**LV naming:** Workspace LVs use `ws-<instanceID>` prefix (not `vol-<instanceID>` used by data volumes). Clones use `ws-<cloneID>`. The `clone_of` field references the origin LV name for clone → origin relationships (empty for non-clones).

**New volume class:** `VolumeClassWorkspace` is added to `VolumeRequest`. The `luksVolumeManager.EnsureVolume` dispatcher routes workspace requests to a new `ensureWorkspaceVolume` path that handles flatten, idmap, and workspace-specific metadata.

**Impact on existing code:** `DestroyVolume`, `ReconcileAllVolumeStates`, and `cleanupStaleAppState` gain a `case "luks-workspace"` branch. `cleanupStaleAppState` is updated to also check for `ws-<id>` LV names and `piccolo-vol-ws-<id>` LUKS mapper names (in addition to the existing `vol-<id>` and `piccolo-vol-<id>` patterns). `ReconcileAllVolumeStates` additionally scans for workspace LVs with the `.piccolo_flatten_incomplete` sentinel — these are destroyed even if no metadata file exists, using the `ws-` LV name prefix for identification.

### 6.2 Workspace Manager Interface

A new `WorkspaceBlockManager` interface in `internal/app/workspaceblock/` replaces `workspacedisk.Manager` for the block-native path:

```go
type WorkspaceBlockManager interface {
    Flatten(ctx context.Context, instanceID string, opts FlattenOptions) error
    Mount(ctx context.Context, instanceID string) (rootfsPath string, err error)
    Unmount(ctx context.Context, instanceID string) error
    Clone(ctx context.Context, originID, cloneID string) error
    DestroyClone(ctx context.Context, cloneID string) error
    Status(ctx context.Context, instanceID string) (Status, error)
}
```

The old `workspacedisk.Manager` remains as a fallback for the kernel version fallback path (§3.2.4). The `AppManager` selects the implementation at startup based on idmap capability detection.

## 7. Migration

Existing workspace installations (overlay-based) will be migrated on first piccolod upgrade. Migration requires workspace downtime.

### 7.1 Detection

On startup, `ReconcileAllVolumeStates` checks each workspace volume's metadata type. Volumes with `"type": "luks-thinlv"` that have a `workspacedisk/` layout (upper/, work/, merged/ subdirs) are candidates for migration.

### 7.2 Procedure

1. Stop workspace container
2. Check thin pool capacity — abort migration if data usage > 85% or metadata > 75%
3. Verify base image is locally cached. If not, `podman pull` the image digest from `meta.json`. If pull fails (no network, registry unavailable), defer migration to next startup — the workspace continues on the overlay path.
4. Create new thin LV (`ws-<instanceID>`), LUKS, ext4
5. Write `.piccolo_flatten_incomplete` sentinel
6. **Re-flatten from image** into the new ext4 (same pipeline as §3.3 — `podman export | tar x --numeric-owner`). This ensures on-disk UIDs are the original image UIDs (0, 33, etc.), not the squashed per-app UIDs from the old fuse-overlayfs overlay.
7. **Copy user modifications** from the old overlay upper dir on top: `rsync -aX --numeric-ids <upper>/ /mnt/ws-<id>/`. The upper dir contains only files the user modified (installed packages, config changes). These files have squashed UIDs (per-app user), so rsync them with `--chown=0:0` to restore root ownership, or more precisely, reverse the squash mapping for known UID ranges. **Simplification:** for most workspaces, user modifications are package installs (owned by root inside container = squashed to per-app UID on disk). Chowning to UID 0 on the new ext4 is correct for these. Files owned by other UIDs inside the container (rare in practice) are best-effort.
8. `syncfs(mount_fd)`, remove sentinel
9. Write new `"luks-workspace"` metadata via `fsutil.AtomicWriteFile`
10. Remove old overlay dirs (upper/, work/, merged/) and old metadata
11. Start workspace on new block-native path

### 7.3 Rollback

If migration fails at any step after LV creation (steps 4-8), the new LV is destroyed and the old overlay workspace remains functional. The migration is re-attempted on next startup. No data loss in any failure scenario — the old overlay is not touched until step 10, which runs only after the new volume is verified complete.

### 7.4 Phase 4 Cleanup

The old `workspacedisk/` overlay code and fuse-overlayfs dependency are removed only after migration support has shipped in at least one release cycle. This ensures users who upgrade have the migration path available. Phase 4 also removes the kernel fallback path (§3.2.4) — by this point, the minimum kernel requirement includes idmap support.

## 8. Risks

1. **`mount_setattr` kernel compatibility.** Requires kernel 5.12+ for ext4 idmap. MicroOS (Tumbleweed-based) ships 6.x — no risk for target platform. Fallback: fuse-overlayfs squash (§3.2.4), with detection at startup.

2. **`podman --rootfs` limitations.** podman doesn't apply OCI image config (ENTRYPOINT, CMD, ENV) when using `--rootfs`. We already handle this — `meta.json` stores image config, and container creation applies it explicitly. No new work needed.

3. **Flatten cost.** Image flatten adds ~30s to workspace install. Acceptable: image pull dominates install time, and this is a one-time cost.

4. **Thin snapshot LUKS interaction.** Mitigated by re-keying clone LUKS at snapshot time (§3.4.1). Integration test required: verify `cryptsetup luksChangeKey` on a thin snapshot with origin still open.

5. **Thin pool full during flatten.** Mitigated by pre-flatten capacity check (§3.3 step 1). If pool fills during extraction, ext4 returns ENOSPC — the sentinel-based recovery (§3.3) handles this as a partial flatten.

6. **Workspace volume resize.** Initial 10 GiB is thin-provisioned (only used blocks consume pool space). For workspaces needing more (AI model weights, large datasets), expose resize via API: `lvextend` + `resize2fs` (online, no downtime). Not in Phase 2 scope but the architecture supports it.

## 9. Operational Readiness

### 9.1 Observability

Log events for all workspace block operations:
- `INFO: workspace <id>: flatten started (image=<ref>, lv=ws-<id>)`
- `INFO: workspace <id>: flatten complete (<duration>, <bytes written>)`
- `INFO: workspace <id>: idmapped mount at <path> (uid_map: 0→<host_uid>, 1-65535→<subuid_start>)`
- `INFO: workspace <id>: clone <clone-id> created from <origin-id>`
- `INFO: workspace <clone-id>: re-keying clone LUKS (separating from origin <origin-id>)`
- `WARN: workspace <id>: mount_setattr unavailable, falling back to fuse-overlayfs`
- `ERROR: workspace <id>: flatten interrupted, marked for cleanup`

### 9.2 Error Translation

`mount_setattr` returns generic errno values. Translate to actionable messages:
- `ENOSYS` → "kernel too old for idmapped mounts (need 5.12+), using fuse-overlayfs fallback"
- `EINVAL` → "invalid UID map configuration — check subuid allocation for <username>"
- `EPERM` → "missing CAP_SYS_ADMIN — piccolod must run as root"

### 9.3 Teardown Sequence

On workspace stop:
1. Stop container (`podman stop --timeout 30`)
2. If container doesn't exit within timeout, force-kill (`podman kill`). The container must be terminated before unmount to avoid writes landing in a detached mount tree.
3. Unmount the idmapped bind mount (the `move_mount` target path that `--rootfs` sees). If EBUSY, use `MNT_DETACH` (lazy unmount) — same pattern as existing `UnmountOverlay`.
4. Unmount the underlying ext4 mount (the original mount point before `open_tree`). These are two separate mount records — the idmapped bind mount references the underlying ext4 mount. Both must be unmounted, idmapped first.
5. Close LUKS mapper (`cryptsetup close piccolo-vol-ws-<id>`)
6. Deactivate thin LV

This integrates into the existing `detachAppVolume` path in `luksVolumeManager` via the `"luks-workspace"` type branch.

### 9.4 Diagnostics

Expose workspace LV state in the existing storage inspection API:
- Which workspace LVs exist, their sizes, and clone relationships
- Active LUKS mappers and mount status
- Thin pool usage breakdown (workspace vs data volumes)
