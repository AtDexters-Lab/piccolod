# RFC: Unified Block-Native Rootfs — Golden LV, btrfs+zstd, Single LUKS Key

**Date:** 2026-02-27
**Updated:** 2026-03-01
**Status:** Draft
**Supersedes:** `20260101-workspace-disk-container-independent.md` (overlay-based workspace persistence)
**Breaks:** This is a clean break from the current overlay-based rootfs and per-volume LUKS key model. No migration code, no dual-path logic. Existing volumes must be destroyed and recreated.

## 1. Summary

Replace overlay-based container rootfs management (both workspace and service) with a unified golden LV architecture: a flattened OCI image on a dm-thin LV serves as a template, dm-thin snapshots provide per-container rootfs instances, and idmapped mounts handle UID translation. btrfs with zstd compression provides 2-3x storage reduction on all rootfs volumes. A single LUKS master key encrypts all volumes.

This eliminates overlay filesystems, FUSE, fuse-overlayfs, `additionalimagestore`, per-app `storage.conf`, and dm-vdo from the entire stack. Containers become thin wrappers around mounted block devices via `podman create --rootfs`.

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

**The golden LV approach** eliminates the problem entirely: flatten the image via `podman export | tar x --numeric-owner` to produce files with real UIDs (0, 33, etc.) on btrfs. One idmap. No overlay. No UID range conflicts.

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
6. **Service rootfs always read-only** — unconditional correctness invariant for cluster (no divergence without DRBD, not configurable per-app)
7. **DRBD + NBD always present** for replicated volumes (standalone on single-node)
8. **Transparent compression** — btrfs+zstd on all rootfs volumes (A, B, C) for 2-3x storage reduction
9. **No migration, no fallbacks** — clean break, hard fail if kernel features unavailable
10. **Self-healing integrity** — NBD hash verification + cold tier recall detects and corrects block corruption transparently

## 3. Volume Types

### 3.1 Golden LV — Template (A)

One golden LV per unique OCI image **digest**. Contains the flattened image on btrfs+zstd. Right-sized to actual image content (not a fixed size).

```
dm-thin LV → LUKS → btrfs+zstd → flattened image
```

- Created at first install of an image, shared by all containers using that image digest
- Multi-container service apps create multiple golden LVs (one per container image). Sidecar images (postgres, redis) shared across apps using the same digest.
- Never mounted during normal operation (only during creation and image updates)
- Not replicated via DRBD (no NBD, no DRBD in normal stack). Cold-tiered as an opaque binary blob after creation (§4.5). Reconstructable from cold-tiered copy; re-pull + re-flatten is the last-resort fallback but produces a non-byte-identical LV.
- Snapshots provide service rootfs (C) and workspace (B) instances

### 3.2 Workspace (B)

Snapshot of a golden LV. Contains the complete rootfs + user data. Replicated.

```
dm-thin snapshot of A → NBD → DRBD → LUKS → btrfs+zstd (rw) → idmapped mount → podman --rootfs
```

- Single LV = rootfs + user data. No separate data volume.
- Replicated via DRBD (user data on rootfs). NBD always present.
- Idmapped mount for UID translation (piccolod creates as root)
- DRBD standalone on single-node, connected in cluster mode

### 3.3 Workspace Clone (B-clone)

Snapshot of an existing workspace B at its current state. For AI agent forking.

```
dm-thin snapshot of B → NBD → DRBD → LUKS → btrfs+zstd (rw) → idmapped mount → podman --rootfs
```

- Instant creation (dm-thin metadata operation)
- Origin and clone run concurrently
- LUKS UUID change required to avoid collision (§5.4)

### 3.4 Service Rootfs (C)

Read-only snapshot of a golden LV. Provides the immutable base filesystem for service containers.

```
dm-thin snapshot of A → LUKS → btrfs+zstd (ro) → idmapped mount → podman --rootfs --read-only
```

- **Always read-only** — unconditional correctness invariant, not configurable per-app. Without DRBD, cluster nodes mount independent snapshots. Read-write would allow silent divergence between nodes.
- Not replicated (no NBD, no DRBD) — reconstructable from golden LV
- Writable paths provided via:
  - **tmpfs:** `/tmp`, `/run`, `/var/run`, `/var/tmp`, `/dev/shm` (podman defaults + app manifest `x-piccolo.tmpfs`)
  - **Per-app tmpfs:** `~/.cache`, app-specific temp dirs (via `storage.temporary` in app manifest)
  - **Persistent bind mounts:** from service data volume (D) at app-defined paths (via `storage.persistent` in app manifest)
  - **Environment:** `PYTHONDONTWRITEBYTECODE=1` for Python apps (via `environment` in app manifest)
- Apps that cannot function within this model (read-only rootfs + tmpfs + persistent bind mounts) are unsupported in service mode. Analysis of the top 50 self-hosted apps shows 53% work cleanly, 34% work with per-app writable mount configuration, and 13% are fundamentally incompatible (see §10 Risk 5).

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
| A (golden) | LV | — | — | yes | btrfs+zstd | template | No |
| B (workspace) | snapshot of A | yes | yes | yes | btrfs+zstd rw | live | Yes |
| B-clone | snapshot of B | yes | yes | yes | btrfs+zstd rw | live | Yes |
| C (svc rootfs) | snapshot of A | — | — | yes | btrfs+zstd ro | live | No |
| D (svc data) | LV | yes | yes | yes | ext4 rw | live | Yes |
| Ephemeral | LV | — | — | — | btrfs+zstd | live | No |

## 4. Block Device Stack

### 4.1 Compression Strategy: btrfs+zstd (replaces dm-vdo)

dm-vdo was originally planned above LUKS for transparent compression. **Removed** because the stacking order required by LUKS (dm-vdo above LUKS, above dm-thin) causes compound write amplification: VDO's allocate-on-write CoW compounds with dm-thin snapshot CoW. Red Hat recommends dm-thin above VDO, but our LUKS requirement forces the opposite order. The compression benefit doesn't justify the 4-5x write amplification cost on workspaces, especially given SSD wear on edge hardware.

**Replacement: btrfs with zstd compression.** Filesystem-level compression without a separate dm target. Analysis of the top 50 self-hosted container images shows cross-image OCI layer deduplication saves only 2.7% (380 MB across 52 images, 581 layers), while filesystem compression saves 2-3x. Compression is the far more valuable optimization, and btrfs delivers it transparently:

- **Read-only volumes (A, C):** zero CoW penalty. Compression is pure storage savings with no write amplification.
- **Read-write volumes (B):** btrfs CoW on random writes adds ~1.3x write amplification — far less than VDO's 4-5x. Acceptable trade-off for 2-3x storage reduction. Per-file opt-out available via `chattr +C` for write-heavy paths if needed.
- **Service data (D):** stays ext4. Application databases and data files don't benefit predictably from compression, and btrfs CoW is undesirable for database workloads.

**Compression level: `zstd:1` everywhere.** Level 1 is ~2x faster than level 3 on writes with only ~5% less compression ratio. On NVMe-backed edge hardware, CPU overhead from higher compression levels can bottleneck I/O. Since all volume types (A, B, C) share the same golden LV as origin, a single compression level avoids mixed-level snapshots. Level 1 is the right trade-off: fast enough for workspace interactive writes, sufficient compression for storage reduction.

**TRIM chain:** btrfs `mount -o discard=async` → LUKS `--allow-discards` → DRBD `rs-discard-granularity` → NBD `BLKDISCARD` → dm-thin deallocates. btrfs `discard=async` batches discards for better performance than synchronous discard. **Critical:** All `cryptsetup open` invocations MUST include `--allow-discards`. Without it, TRIM stops at LUKS and the thin pool fills monotonically — fatal on edge hardware with small storage.

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

**Cipher mode: `aes-xts-plain64` (encryption only, no AEAD).** LUKS AEAD (`--integrity`) adds dm-integrity underneath for per-sector authentication tags. This is removed because:

- **btrfs volumes (A, B, C):** btrfs already checksums every data and metadata block (crc32c). dm-integrity is redundant — double integrity checking with ~10-20% write overhead and ~3% storage overhead for tags.
- **ext4 volumes (D):** dm-integrity would detect corruption but cannot correct it — there's no local redundant copy. The only recovery path (DRBD peer or cold tier) is the same whether corruption is detected by dm-integrity or by application-level checksums (PostgreSQL page checksums, SQLite WAL checksums).
- **NBD integrity verification (§4.6)** replaces LUKS AEAD with a strictly better model: detection via hash verification + automatic correction via cold tier recall. LUKS AEAD only detects (`EIO`); NBD self-heals.

All `cryptsetup luksFormat` commands use `--cipher aes-xts-plain64 --key-size 512` (256-bit AES in XTS mode). No `--integrity` flag.

**Volume creation with shared key:**
- Golden LV (A): `cryptsetup luksFormat --cipher aes-xts-plain64 --key-size 512 --master-key-file <shared-key>`, add keyslots 1 and 2
- Snapshots (B, C): inherit LUKS header from A. Change UUID only (`cryptsetup luksUUID`)
- Fresh volumes (D): `cryptsetup luksFormat --cipher aes-xts-plain64 --key-size 512 --master-key-file <shared-key>`, add keyslots

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

piccolod (root, `CAP_SYS_ADMIN`) creates a user namespace to hold the UID map and applies it to the btrfs mount via `mount_setattr(MOUNT_ATTR_IDMAP)`.

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

If `mount_setattr` returns `ENOSYS` or `EINVAL`, piccolod fails hard. No fuse-overlayfs fallback. MicroOS ships kernel 6.x (btrfs idmap stable since 5.15, overlay idmap since 5.19). Hard dependency is acceptable for the target platform.

### 4.5 Golden LV Cold Tiering

Golden LVs are **non-reproducible binary artifacts**. Two independent `mkfs.btrfs` + `tar x` operations on the same OCI image will NOT produce byte-identical LVs — btrfs generates random filesystem UUIDs, non-deterministic metadata block allocation, and compression-dependent extent placement (see §5.7.1). This has two consequences:

1. **Disaster recovery requires the original golden LV bytes.** Workspace snapshots (B) are dm-thin snapshots that share unmodified blocks with their golden LV origin. Recovering a workspace from cold tier requires applying the cold-tiered delta onto a byte-identical golden LV base. A re-pulled and re-flattened golden LV has different block layout — applying the old delta on a new base corrupts the filesystem.

2. **Self-sovereignty.** If an upstream registry goes down, rate-limits, or removes an image, the cold-tiered golden LV is the sovereign copy of the app's rootfs. Without it, a disk failure + registry outage = unrecoverable app with no fallback.

**Mechanism:** After golden LV creation (§5.1), the golden LV is cold-tiered as a one-time opaque blob upload, keyed by image digest. This is NOT via NBD/DRBD — golden LVs have no NBD or DRBD in their stack. It is a separate fire-and-forget block-copy to cold storage:

1. Golden LV creation completes (§5.1 steps 1-10)
2. Reactivate LV, stream raw LUKS ciphertext blocks to cold storage
3. Cold storage stores blob keyed by `<image-digest>:<golden-lv-size>`
4. Deactivate LV. Done — no ongoing replication.

**Properties:**
- Immutable after creation — write once, never modified. Ideal cold tier candidate.
- Small — compressed image size, typically 0.3-1.5 GiB per unique image digest.
- Shared — one golden LV per unique digest serves all workspaces and services using that image.
- Recall is rare — only needed on disk failure recovery or peer node provisioning (§5.7.1).

**Interaction with §5.7.1 (DRBD skip-sync):** The golden LV block-copy to the peer node and the golden LV cold-tier upload are the same operation conceptually — shipping the opaque blob to a different destination. In a two-node cluster, the golden LV is shipped to both the peer and cold storage.

### 4.6 NBD Integrity Verification

NBD provides read-time block integrity verification with automatic self-healing via cold tier recall. This replaces LUKS AEAD (§4.2) with a strictly better model: detection + correction instead of detection-only.

**How it works:**

```
Write path:
  App writes block X → NBD stores locally (dm-thin)
                      → NBD computes hash(X), stores in hash index
                      → NBD async-ships block X to cold tier

Read path:
  App reads block X → NBD reads from local dm-thin
                    → NBD verifies hash(X) against stored hash
                    → Match: serve block
                    → Mismatch: recall block from cold tier, repair local copy, serve correct data
```

**Hash function:** xxhash64 — ~10 GB/s on modern CPUs, negligible compared to NVMe latency. 8 bytes per 4K block = ~20 MiB hash index per 10 GiB volume. Not cryptographic (not adversarial threat model — this detects bit rot, not tampering; LUKS encryption handles confidentiality).

**Why NBD is the right layer:**

| Layer | Detects corruption | Corrects corruption |
|-------|-------------------|-------------------|
| btrfs checksums | Yes | No (single disk, no RAID) |
| LUKS AEAD | Yes (returns `EIO`) | No |
| NBD hash + cold recall | Yes | **Yes** (transparent recall) |

NBD is the only layer with access to both the local data AND an alternative source (cold tier). btrfs detects but can't fix. LUKS AEAD detects but can't fix. NBD detects AND fixes.

**Verification modes:**
- **Every read** — safest, catches corruption immediately. Hash verification adds ~100ns per 4K block (xxhash64). Acceptable for interactive workloads.
- **Background scrub** — periodic full scan of all local blocks against hash index. Catches latent corruption before it's accessed. Runs during idle periods.

**Correction window:** Between write and cold-tier-ship, a corrupted block is detectable (hash mismatch) but not correctable — the cold tier doesn't have a copy yet. During this window, NBD behavior matches LUKS AEAD (returns `EIO`). Once the block is cold-tiered, self-healing activates.

**Applies to:** All volumes with NBD in the stack — B (workspace), D (service data). This fills the ext4 integrity gap on D volumes: ext4 has no native data checksumming, and LUKS AEAD is removed (§4.2). NBD integrity verification provides the safety net.

**Does not apply to:** A (golden), C (service rootfs), Ephemeral — these have no NBD. A and C rely on btrfs checksums for detection; correction is via re-creation from cold-tiered golden LV (A) or re-snapshot from golden LV (C).

## 5. Lifecycle

### 5.1 Golden LV Creation

When a workspace or service is installed and no golden LV exists for the image digest:

1. Check thin pool capacity — abort if data > 85% or metadata > 75%
2. `lvcreate --thin` — new thin LV, right-sized to `max(image_size × 1.5, image_size + 1 GiB)`, thin-provisioned. Image size determined from `podman image inspect --format '{{.Size}}'` after pull (uncompressed virtual size). This intentionally over-allocates virtual size relative to on-disk usage after btrfs+zstd compression — the formula provides a safe upper bound. Since the LV is thin-provisioned, virtual over-allocation does not consume pool data space; only actual written blocks count against the thin pool. Right-sizing reduces DRBD sync bandwidth and thin pool waste compared to a fixed 10 GiB allocation.
3. `cryptsetup luksFormat --master-key-file <shared-key>` → `luksOpen`
4. `mkfs.btrfs -f` on LUKS device, mount with `-o compress=zstd:1,discard=async,noatime`
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
10. Unmount btrfs, close LUKS, deactivate LV

**Partial flatten recovery:** On startup, `ReconcileAllVolumeStates` destroys golden LVs with `.piccolo_flatten_incomplete` present. Sentinel written before extraction, removed after fsync.

### 5.2 Golden LV Image Update

When a pulled image has a new digest for the same ref:

1. Create a NEW golden LV for the new digest (§5.1) — happens in background while existing containers continue running
2. Stop affected containers, destroy old C snapshots, create new C snapshots from new golden LV, restart
3. Workspaces are NOT affected — B is a diverged snapshot with user data. Users create new workspaces from the updated image if desired.
4. Once no snapshots reference the old golden LV, garbage collect it (§5.3)

**Multi-container service apps:** When multiple container images update in a multi-container service (e.g., app + postgres sidecar), new golden LVs are created in the background for all updated images while the pod runs. The pod stops once for snapshot rotation: each container's C snapshot is destroyed and re-created from its new golden LV, then the pod restarts. If a snapshot re-creation fails mid-way, the pod restarts with a mix of old and new snapshots — manual rollback available but not automatic. Atomic all-or-nothing across multiple images is not provided.

### 5.3 Golden LV Garbage Collection

On startup and after service/workspace uninstalls, scan for golden LVs with zero active snapshots (no B or C volumes reference it). Destroy: close LUKS + `lvremove` + remove metadata.

### 5.4 Workspace Creation

1. Ensure golden LV exists for the image (create if not — §5.1)
2. `lvcreate --snapshot --name ws-<id> <vg>/golden-<image-id>` — instant
3. `lvchange -ay <vg>/ws-<id>` — activate snapshot
4. `cryptsetup luksUUID --uuid $(uuidgen) /dev/<vg>/ws-<id>` — unique UUID (avoids collision with golden LV and other snapshots). If UUID assignment fails, destroy the snapshot LV immediately (`lvremove`) to prevent two LVs with identical LUKS UUIDs.
5. Write workspace metadata
6. Attach: NBD → DRBD (standalone) → `cryptsetup open --allow-discards` → `mount -o compress=zstd:1,discard=async,noatime` → idmapped mount
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
5. Attach: `cryptsetup open --allow-discards` → `mount -o ro,compress=zstd:1,discard=async,noatime` → idmapped mount
6. `podman create --rootfs <idmapped-path> --read-only`

**Host-level read-only mount:** The btrfs mount is truly ro at the host level (not just container-level `--read-only`). This works because the flattened image from `podman export` already contains all standard OCI runtime bind mount targets (`/etc/resolv.conf`, `/etc/hostname`, `/etc/hosts`, `/etc/mtab`). Runc/crun bind-mounts over these existing paths — it does not need to create them. The `/piccolo` bind mount directory (for `boot.sh`, config) is workspace-only; workspaces (B) mount rw.

**Writable paths:** Podman provides tmpfs for `/tmp`, `/var/run`, `/dev/shm`. Additional writable mounts configured per-app via the app manifest: `x-piccolo.tmpfs` for additional tmpfs paths, `storage.temporary` for ephemeral writable dirs, `storage.persistent` for durable bind mounts from data volume (D), and `environment` for runtime flags like `PYTHONDONTWRITEBYTECODE=1`. This is standard read-only root filesystem behavior (same as Kubernetes `readOnlyRootFilesystem: true`).

### 5.7 Replication

Workspace (B) and service data (D) volumes use the full replication stack:

```
thin LV → NBD → DRBD → LUKS → btrfs+zstd (B) or ext4 (D)
```

One DRBD resource per volume. Since workspaces are now a single LV (not rootfs + data), replication is simpler — one resource covers the entire workspace state.

#### 5.7.1 DRBD Skip-Sync for Workspaces

When a workspace is created on a two-node cluster, the peer must have an identical starting point to avoid a full initial sync of the entire LV.

**Optimization:** Block-copy the golden LV's thin LV data to the peer once per unique image digest. When creating a workspace snapshot on both nodes from byte-identical golden LVs, the initial snapshot content is byte-identical. DRBD can skip the initial sync:

1. Block-copy the golden LV to the peer (once per unique image digest). The primary node streams the golden LV's LUKS ciphertext via a temporary NBD export; the peer writes it to a local thin LV of the same size. This ensures byte-identical golden LVs on both nodes.
2. Create workspace snapshot on both nodes from their respective local golden LVs
3. `drbdadm new-current-uuid --clear-bitmap <resource>` — tells DRBD both sides are identical, skip initial sync

**Why block-copy, not independent reconstruction:** Two independent `mkfs.btrfs` + `tar x` operations on separate nodes will NOT produce byte-identical LVs. btrfs generates random filesystem UUIDs, non-deterministic metadata block allocation, and compression-dependent extent placement. `--clear-bitmap` with diverged content causes silent data corruption — DRBD will not detect the mismatch. Block-copy is the only safe path for skip-sync.

**Bandwidth reduction:** Without skip-sync, every workspace creation triggers a full DRBD sync of `LV_size` bytes. With skip-sync, only the golden LV needs to be shipped once per unique image. For N workspaces from the same image: sync cost drops from `N × LV_size` to `1 × image_size`. At typical scale (3-5 workspaces per image, 2-5 unique images), this is ~8x bandwidth reduction.

### 5.8 Teardown Sequence

**Workspace / service data (B, D):**
1. Stop container (`podman stop --timeout 30`, force-kill if needed)
2. Unmount idmapped bind mount (if applicable). EBUSY → `MNT_DETACH`.
3. Unmount btrfs (B) or ext4 (D)
4. Close LUKS mapper
5. DRBD down (if cluster)
6. NBD disconnect
7. Deactivate thin LV

**Service rootfs (C):**
1. Stop container
2. Unmount idmapped bind mount
3. Unmount btrfs (ro)
4. Close LUKS mapper
5. Deactivate thin LV

**Crash recovery and mount discovery:** Idmapped mounts survive piccolod crashes (they are kernel mount state, not process state). On restart, `ReconcileAllVolumeStates` discovers existing mounts by scanning `/proc/self/mountinfo` for active mount entries at known paths (`/piccolo-core/mounts/<vol-id>`). If a mount is already active, the attachment sequence skips the mount steps and reuses it. For partially-attached stacks (e.g., LUKS open but btrfs not mounted), reconciliation tears down to a known-clean state (close all layers) and re-attaches from scratch. The golden LV sentinel check (`.piccolo_flatten_incomplete`) also performs a full teardown of any partially-attached golden LV stack before `lvremove`.

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

### 6.5 Podman Native Storage for Services

Considered using standard podman overlay storage for service containers, keeping the golden LV architecture only for workspaces. This would preserve OCI layer deduplication across images.

**Not chosen because:** Analysis of the top 50 self-hosted container images (52 images, 581 layers) shows cross-image OCI layer deduplication saves only 2.7% — 380 MB out of 13.63 GB. Meanwhile, podman stores pulled images uncompressed on disk, while btrfs+zstd compresses 2-3x. The unified golden LV + btrfs architecture saves far more storage than layer dedup, and eliminates a separate code path for service rootfs management.

### 6.6 dm-vdo for Compression

See §4.1. VDO's allocate-on-write CoW compounds with dm-thin snapshot CoW, causing 4-5x write amplification on workspaces. btrfs+zstd delivers the same compression benefit at the filesystem level with ~1.3x write amplification (and zero for read-only volumes).

### 6.7 LUKS AEAD for Integrity

Considered using LUKS2 with `--integrity` (dm-integrity) for per-sector authentication tags on all volumes.

**Not chosen because:**
- **Redundant with btrfs (A/B/C):** btrfs checksums every data and metadata block natively. dm-integrity adds a second integrity layer with ~10-20% write overhead and ~3% storage overhead for authentication tags. No additional detection capability.
- **Detection without correction (D):** On ext4 volumes, dm-integrity detects corruption (`EIO`) but cannot correct it — no local redundant copy. Recovery requires DRBD peer resync or cold tier recall regardless.
- **NBD integrity is strictly better (§4.6):** Provides the same detection (hash mismatch) plus automatic correction (cold tier recall). Self-healing instead of `EIO`. Applies to all NBD-backed volumes (B, D).
- **Overhead:** dm-integrity requires a journal region per volume, adds a dm layer to the stack, and imposes write amplification from journaled tag updates.

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
  "size_bytes": 2147483648,
  "fs_type": "btrfs",

  "base_image_digest": "docker.io/library/ubuntu@sha256:...",
  "base_image_ref": "ubuntu:22.04"
}
```

`size_bytes` is right-sized per §5.1 step 2. Reflects actual image size + headroom, not a fixed allocation.

### 7.3 Workspace Metadata

```json
{
  "version": 3,
  "type": "workspace",
  "lv_name": "ws-code-server-abc123",
  "vg_name": "piccolo-data-vg",
  "size_bytes": 2147483648,
  "fs_type": "btrfs",

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
  "fs_type": "btrfs",

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
- Integration tests (root required): idmapped btrfs mount
- ~150 lines of Go

### Phase 2: Golden LV and Workspace Lifecycle

- Migrate `rootfs_volume_manager.go` from ext4 to btrfs+zstd: `mkfs.ext4` → `mkfs.btrfs`, mount options → `compress=zstd:1,discard=async,noatime`, service rootfs mount → host-level ro
- Update `idmap.go` kernel version comment: 5.12 → 5.15 (btrfs idmap)
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
- Remove dm-vdo package (`internal/storage/vdo/`)
- Update alpha test suite

## 9. What This Eliminates

| Removed | Replaced by |
|---------|-------------|
| fuse-overlayfs (both modes) | btrfs + idmapped mount |
| overlayfs entirely | dm-thin snapshot = writable layer |
| dm-vdo (compression) | btrfs+zstd (filesystem-level compression) |
| `additionalimagestore` config | Golden LV (shared image template) |
| Per-app `storage.conf` with `mount_program` | `podman --rootfs` (bypasses storage driver) |
| `workspacedisk/` overlay machinery (~500 lines) | dm-thin snapshot + mount (~150 lines) |
| `workspace_disk_integration.go` overlay code | Golden LV flatten pipeline |
| UID squashing (`squash_to_uid`) | Kernel idmapped mount (multi-UID preserved) |
| Separate rootfs + data volume for workspaces | Single LV per workspace |
| Per-volume LUKS key management | Single shared key |
| LUKS AEAD / dm-integrity | btrfs checksums (A/B/C) + NBD integrity verification (B/D) |

## 10. Risks

1. **`mount_setattr` hard dependency.** Kernel 5.15+ for btrfs idmap. MicroOS ships 6.x — no risk for target platform. No fallback by design.

2. **`podman --rootfs` limitations.** OCI image config (ENTRYPOINT, CMD, ENV) not applied by `--rootfs`. Already handled — `meta.json` stores image config, container creation applies it explicitly.

3. **Flatten cost.** ~30s per unique image (one-time). Image pull dominates install time. Subsequent containers from the same image are instant snapshots.

4. **Single LUKS key blast radius.** Key leak from RAM exposes all volumes. Mitigated: if attacker has RAM access, they have process access → SDEK → all keys anyway. See §4.2.

5. **Service rootfs read-only compatibility.** Read-only rootfs is unconditional — no per-app opt-out. Analysis of the top 50 self-hosted container images: 25 apps (53%) work cleanly with read-only rootfs + tmpfs + data volume. 16 apps (34%) need per-app writable mount configuration in the app manifest (additional tmpfs paths, persistent bind mounts from D at specific rootfs locations, environment variables). 6 apps (13%) are fundamentally incompatible (Nextcloud, BookStack, Home Assistant, Pi-hole, Dockge, Hoarder) — these are either replaceable (AdGuard Home over Pi-hole), solvable by bind-mounting writable subtrees from D (Nextcloud's entrypoint populates an empty `/var/www/html` from `/usr/src/nextcloud/`), or can run as workspaces if users require them. This is a deliberate trade-off: unconditional read-only rootfs eliminates a code branch and preserves the cluster correctness invariant (§2.4 goal 6).

6. **Golden LV storage.** One golden LV per unique image digest. With btrfs+zstd compression, typical images occupy 0.3-1.5 GiB (compressed, thin-provisioned). Right-sizing (§5.1) avoids over-allocation. Acceptable for piccolo's app count (5-20 apps, fewer unique image digests due to sidecar sharing).

7. **Image update coordination.** Updating a golden LV requires stopping all service containers using it, re-creating snapshots, and restarting. More involved than `podman pull` + restart. Acceptable because image updates are infrequent and the coordination is automatable.

8. **btrfs CoW write amplification on workspaces.** btrfs CoW on random writes adds ~1.3x write amplification compared to ext4. This is a deliberate trade-off: 2-3x storage reduction from compression outweighs the modest write amplification. VDO's alternative was 4-5x — btrfs is far better. For write-heavy paths (databases, build caches), per-file opt-out is available via `chattr +C` (disables CoW and compression for that file). Read-only volumes (A, C) have zero CoW penalty.

9. **NBD integrity verification cold tier dependency.** Self-healing only works for blocks that have been cold-tiered. Between write and cold-tier-ship, corruption is detectable (hash mismatch → `EIO`) but not correctable — same as LUKS AEAD behavior. The correction window depends on cold-tier upload latency and bandwidth. Hash index adds ~20 MiB memory per 10 GiB volume and ~100ns read-path latency (xxhash64). Acceptable for piccolo's scale.

10. **Golden LV cold tier as bootstrap dependency.** Workspace disaster recovery depends on recalling the golden LV from cold storage before workspace deltas can be applied. If cold storage is unreachable, workspace recovery is blocked. Mitigated: in two-node clusters, the peer has a block-copied golden LV (§5.7.1) as an alternative source. Single-node without cold tier access falls back to re-pull + re-flatten (creates new workspaces, does not recover existing workspace data).

## 11. Operational Readiness

### 11.1 Observability

```
INFO: golden-lv <id>: flatten started (image=<ref>, digest=<digest>)
INFO: golden-lv <id>: flatten complete (<duration>, <bytes> raw, <bytes> compressed)
INFO: workspace <id>: created from golden-lv <golden-id> (snapshot, instant)
INFO: workspace <id>: idmapped mount at <path> (uid_map: 0→<host_uid>, 1-65535→<subuid_start>)
INFO: workspace <id>: clone <clone-id> created (snapshot, instant)
INFO: workspace <id>: DRBD skip-sync — golden LV already on peer
INFO: service-rootfs <id>: created from golden-lv <golden-id> (snapshot, read-only)
INFO: service-data <id>: created (fresh thin LV, <size>)
INFO: nbd <vol-id>: integrity check passed (block <offset>)
WARN: nbd <vol-id>: hash mismatch at block <offset> — recalling from cold tier
INFO: nbd <vol-id>: block <offset> recalled and repaired from cold tier
WARN: nbd <vol-id>: hash mismatch at block <offset> — cold tier unavailable, returning EIO
INFO: nbd <vol-id>: background scrub complete (<n> blocks verified, <m> repaired)
INFO: golden-lv <id>: cold-tier upload started (<size>)
INFO: golden-lv <id>: cold-tier upload complete (<duration>)
ERROR: golden-lv <id>: flatten interrupted, marked for cleanup
ERROR: mount_setattr failed: <errno translation> — piccolod cannot start
```

### 11.2 Error Translation

- `ENOSYS` → "kernel does not support idmapped mounts (need 5.15+) — cannot proceed"
- `EINVAL` → "invalid UID map — check subuid allocation for <username>"
- `EPERM` → "missing CAP_SYS_ADMIN — piccolod must run as root"

### 11.3 Diagnostics

Expose via storage inspection API:
- Golden LV inventory: image ref, digest, compressed size, snapshot count
- Per-volume: type, LV name, LUKS mapper, mount status, fs type
- Thin pool: data%, metadata%, per-type breakdown (golden, workspace, service-rootfs, service-data, ephemeral)
- Clone relationships: origin → clone graph
- Compression ratio: per-volume btrfs compression stats (`btrfs filesystem df`)
- NBD integrity: hash index size, last scrub timestamp, blocks repaired since boot
- Golden LV cold tier: per-digest upload status (pending, uploaded, recalled), cold tier size
