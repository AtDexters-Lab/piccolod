# RFC: Storage v2 Foundation (Core/Data roots + PCV export replication)

**Date:** 2026-02-02  
**Status:** Draft  
**Related:**
- `docs/rfc/20260201-storage-posture.md` (disk posture, partitioning, LUKS2 pool setup)
- `org-context/03_engineering/storage_architecture.md` (source architecture)

## 1) Summary
This RFC tracks the **storage v2 migration** for `piccolod` based on the updated Piccolo storage architecture.

Key decisions captured here:
- Replace `PICCOLO_STATE_DIR` with **two roots**: `PICCOLO_CORE_ROOT` (default `/piccolo-core`) and `PICCOLO_DATA_ROOT` (default `/piccolo-data`).
- Keep the **control plane encrypted store** on `/piccolo-core` using a **gocryptfs-like** mechanism (simple, boot-critical).
- Publish and replicate the control plane via **Piccolo Vault (PCV) exports** (`current.enc`) (not by replicating a live filesystem), leaving room for separate orchestrator backup flows.
- Treat “network-bootstrap” as the only pre-unlock bootstrap layer (remote access before admin unlock).
- Land a single release milestone called **Foundation** (implemented via layered PRs) that establishes the new roots, control-plane placement, PCV export publishing/replication, and `/piccolo-data` bring-up scaffolding.

### 1.1 Relationship to `20260201-storage-posture.md`
This RFC defines **logical storage contracts** that `piccolod` code must follow (paths, env vars, PCV export publishing/replication).

`docs/rfc/20260201-storage-posture.md` defines the **physical disk posture** and first-boot disk preparation needed to make this work on the target OS images (partitioning, LUKS2 pool creation, btrfs mount).

This RFC assumes:
- `/piccolo-core` exists and is writable on boot (btrfs subvolume on the root filesystem).
- `/piccolo-data` is mounted only **after admin unlock**, because its pool key is stored encrypted in the control plane.

## 2) Goals
- **Eliminate `PICCOLO_STATE_DIR`** and any implicit “single state root” assumption.
- Make `piccolod` conform to the **two-root layout**:
  - `/piccolo-core` for boot-critical state and mountpoints.
  - `/piccolo-data` for grow-with-time user and replication data.
- Implement a stable **control-plane persistence + PCV export** contract suitable for:
  - local operation,
  - cluster replication (via artifacts),
  - orchestrator backup/restore.
- Enable **remote reachability before admin unlock** via `network-bootstrap`.
- Define a phased plan that gets to **beta quickly**, with explicit points where compatibility becomes more rigid.

## 3) Non-goals (for Foundation milestone)
- Implementing full **app volume v2** (JuiceFS + per-volume BadgerDB + object store + replication gating).
- Implementing **etcd-based leader leases + epochs** and the “metadata must not get ahead of objects” mechanism.
- Implementing **PSFN federation** and `psfn-service`.
- Providing backward compatibility for pre-v2 installs (we currently have no active deployments).

## 4) Background (why change)
The storage architecture has been updated to:
- enforce a hard separation between “boot-critical” and “expandable user storage”,
- support single-node and cluster modes with well-defined recovery and replication invariants,
- enable future federation/cold-tier recall without compromising correctness.

OS images were updated to include a dedicated `piccolo-core` btrfs subvolume (see the Piccolo OS kiwi changes).

`piccolod` today anchors persistence under `PICCOLO_STATE_DIR` (default `/var/lib/piccolod`) and uses per-volume gocryptfs-style mounts under `mounts/<volume-id>`. That model must be refactored to match the new two-root contract.

Historically, persistence also treated a “bootstrap volume” as a special encrypted mount that was available early to host remote configuration and TLS material. In storage v2, we replace that concept with **`/piccolo-core/network-bootstrap/`** so “remote before unlock” does not imply a separate volume engine.

### 4.1 Terminology and naming

- **`PICCOLO_CORE_ROOT`**: root for boot-critical, fixed storage (default `/piccolo-core`).
- **`PICCOLO_DATA_ROOT`**: root for grow-with-time storage (default `/piccolo-data`).
- **`piccolo_data_pool_key`**: a pool-scoped keyfile (64 random bytes) used to unlock all `/piccolo-data` LUKS2 devices. It is stored encrypted with SDEK at `/piccolo-core/crypto/piccolo_data_pool_key.enc` — **outside** gocryptfs (always readable once SDEK is available). The keyfile is node-scoped (specific to the LUKS devices on this node) but **is included in PCV exports** so that restore workflows can unlock existing `/piccolo-data` partitions on the same hardware (see §8.2.2). The plaintext keyfile is materialized only transiently in tmpfs (e.g. under `/run/piccolo/`) during `cryptsetup` operations.
- **PCV export**: the portable encrypted export bundle produced from the control plane’s durable encrypted payload for replication/backup/restore workflows. On-device, the latest PCV export is stored at `/piccolo-core/recovery/current.enc` (with an accompanying metadata manifest at `/piccolo-core/recovery/current.json`).

## 5) Proposed directory layout (v2 contract)

### 5.1 `/piccolo-core` (fixed / internal)
Holds boot-critical state and mount plumbing:

```
/piccolo-core
  /crypto/                         # key material (outside gocryptfs, always readable)
    /keyset.json                   # SDEK sealed with KEK
    /piccolo_data_pool_key.enc     # LUKS pool keyfile wrapped with SDEK
    /luks-kdf-params/              # Argon2 derivation params per LUKS device (node-scoped)
    /luks-header-backups/          # LUKS header backups (device-specific, NOT in PCV)
  /ciphertext/
    /control-plane/                # btrfs subvolume: gocryptfs ciphertext (durable encrypted payload)
  /volumes/
    /control-plane/
      /piccolo.volume.json         # volume metadata incl. wrapped gocryptfs passphrase
  /mounts/                         # immutable mountpoints (one per volume)
    /control-plane/                # gocryptfs plaintext view (control.db + CP state)
    /<vol-id>/                     # app volume mountpoints
  /recovery/                       # PCV exports (portable, replicated)
    /current.enc                   # latest published PCV export
    /current.json                  # metadata manifest for current.enc
    /history/                      # bounded history
    /staging/                      # build area for atomic publish
  /network-bootstrap/              # pre-unlock remote/bootstrap state (TPM-sealed later)
  /clusterdb/etcd/                 # etcd data dir (cluster mode only; system plane)
```

**Mountpoint rule:** all volume mountpoints — including the control plane — are on `/piccolo-core/mounts/<vol-id>`. Mountpoints are immutable (`chattr +i`, mode `0555`) when unmounted.

**Directory permissions:** All directories under `/piccolo-core` and `/piccolo-data` are created with mode `0700` (owner-only) unless noted otherwise. `crypto/` and its children use `0700`. `recovery/` uses `0700`. `network-bootstrap/` uses `0700`. These permissions ensure that only `piccolod` (running as root) can access sensitive material.

**Directory classification:**
- **Portable (replicated to peers):** `recovery/`
- **Device-local (not replicated as live state):** `crypto/`, `ciphertext/`, `volumes/`, `network-bootstrap/`

**Note:** "Device-local" here means these directories are not replicated *as live state*. The PCV export under `recovery/` bundles the minimal subset needed for restore (see §8.2.2), including node-scoped crypto material (pool keyfile, KDF params) but excluding device-specific runtime state (LUKS header backups, rotation progress markers).

**Note on `/piccolo-core/bin/`:** The architecture baseline (`org-context/03_engineering/storage_architecture.md` §4.1) lists `/piccolo-core/bin/` for `piccolod` + helpers. This RFC intentionally omits it: `piccolod` is installed via RPM into the OS filesystem (e.g. `/usr/bin/piccolod`), not into the `/piccolo-core` subvolume. Keeping the binary in the OS filesystem ensures it participates in MicroOS transactional updates and snapshot rollbacks.

### 5.2 `/piccolo-data` (expandable pool)
Holds grow-with-time data and bulk runtime:

```
/piccolo-data
  /node/                         # runtime scratch, caches, images (churn-heavy)
  /user/
    /volumes/
      /<vol-id>/
        /meta/                   # per-volume metadata engine data dir (BadgerDB)
        /objects/                # per-volume local ciphertext object store
        /cache/                  # per-volume disposable cache (ciphertext only)
  /federation/                   # ciphertext shards stored for PSFN (future)
  /system-objects/
    /control-plane-backups/      # encrypted replicas of PCV exports
    /volume-checkpoints/         # metadata snapshots/checkpoints (future)
```

**CoW posture:** NOCOW should be set *before* files are created for:
- `/piccolo-data/federation/`
- `/piccolo-data/node/`
- `/piccolo-data/user/volumes/<vol-id>/cache/` (when the volume is created)
- `/piccolo-data/user/volumes/<vol-id>/meta/` (when the volume is created)

## 6) Configuration: environment variables

### 6.1 New variables
- `PICCOLO_CORE_ROOT` (default `/piccolo-core`)
- `PICCOLO_DATA_ROOT` (default `/piccolo-data`)

All path resolution must go through `internal/state/paths` (or the successor package if renamed).

#### Path package refactoring (PR 1)
The existing `paths.Root()` resolves to `PICCOLO_STATE_DIR`. In PR 1 ("Paths + env vars"), refactor to:
- `paths.CoreRoot()` → resolves `PICCOLO_CORE_ROOT` (default `/piccolo-core`)
- `paths.DataRoot()` → resolves `PICCOLO_DATA_ROOT` (default `/piccolo-data`)
- Deprecate and remove `paths.Root()` / `paths.Join()`
- Update all callers: e.g. `paths.NetworkBootstrapDir()` becomes `filepath.Join(paths.CoreRoot(), "network-bootstrap")`

**Migration note:** `NetworkBootstrapDir()` is already used by `internal/server/gin_server.go` for the internal CA. Ensure this path resolves correctly under `CoreRoot()` before removing `PICCOLO_STATE_DIR` to avoid breaking HTTPS pre-unlock.

### 6.2 Removal: `PICCOLO_STATE_DIR`
`PICCOLO_STATE_DIR` must be removed from:
- code, docs, Makefile/systemd units, tests, and UI references.

If `PICCOLO_STATE_DIR` is set, `piccolod` should fail fast with a clear error:
“`PICCOLO_STATE_DIR` is no longer supported; use `PICCOLO_CORE_ROOT`/`PICCOLO_DATA_ROOT`.”

## 7) Boot/unlock sequencing (Foundation)

### 7.1 Before admin unlock
- `piccolod` starts with `/piccolo-core` available.
- `network-bootstrap` is available for remote reachability and enrollment flows *prior to unlock*.
- The encrypted control plane remains sealed until the admin password is provided.

### 7.2 After admin unlock
- Unlock the control plane.
- Read the wrapped `/piccolo-data` pool key (`piccolo_data_pool_key`) from the control plane.
- Use it to unlock and mount `/piccolo-data` (LUKS2 + btrfs pool; see `docs/rfc/20260201-storage-posture.md`).
- Ensure the directory layout exists and apply NOCOW posture where required.
- Resume/start services that require `/piccolo-data`.

### 7.3 Network-bootstrap contract (pre-unlock)
`/piccolo-core/network-bootstrap/` is the only pre-unlock persistence root. It exists to support “remote reachability before admin unlock” and device-local onboarding state.

Properties:
- **Device-local:** it is not replicated as part of the portable control plane.
- **Pre-unlock readable/writable:** piccolod may read/write here before the admin password is provided.
- **Mixed sensitivity:** it may contain non-secret state (e.g. onboarding choice) and TPM-sealed (or software-sealed) enrollment credentials.

**TPM adapter model:** All code paths assume a TPM exists. On devices without hardware TPM, a software emulation layer (the "TPM adapter") provides the same sealing/unsealing API using a software-derived key. This means `network-bootstrap` credentials (Nexus credential, portal TLS key, ACME account key) are always "sealed" — the difference is only in the security properties of the seal:
- **Hardware TPM:** secrets are bound to the measured boot state (PCR values); physical device theft cannot extract them without the correct boot chain.
- **Software TPM adapter:** secrets are sealed with a software-derived key stored on the filesystem; physical device theft can extract them. The portal surfaces a warning in this case (see `tpm_remote_reachability.feature`).

Remote reachability before admin unlock is available on **all devices** via this adapter model — there is no "TPM-only" gate for remote access.

Minimum contents (v1):
- `onboarding.json` (device-local, non-secret): persisted onboarding state machine for USB boot flows.
- `remote/` (device-local): the minimal remote configuration and any pre-unlock TLS material needed to establish remote connectivity. Exact schema is owned by the remote subsystem.

Post-unlock behavior (v1):
- Once the control plane is unlocked, piccolod may **mirror** remote configuration into the control plane for portability and clustering, but `network-bootstrap` remains the source of truth for pre-unlock operation.

## 8) Control plane persistence + replication (PCV export-based)

### 8.1 Authoritative storage and unlock chain

The control plane is a gocryptfs-encrypted volume. Its plaintext view is accessed via the standard mount contract at `/piccolo-core/mounts/control-plane/`.

**Unlock chain (matches current `crypt.Manager` + `fileVolumeManager` pattern):**
1. Read `crypto/keyset.json` (always readable, outside gocryptfs).
2. Admin password + Argon2id → KEK → unseal SDEK from `keyset.json`.
3. SDEK → unwrap gocryptfs passphrase from `volumes/control-plane/piccolo.volume.json`.
4. Mount gocryptfs: `ciphertext/control-plane/` → `mounts/control-plane/`.
5. SDEK → unwrap `crypto/piccolo_data_pool_key.enc` → unlock LUKS `/piccolo-data`.

**Key locations:**
- **Outside gocryptfs (always readable):** `crypto/keyset.json`, `crypto/piccolo_data_pool_key.enc`, `volumes/control-plane/piccolo.volume.json`, `crypto/luks-header-backups/`.
- **Durable ciphertext:** `ciphertext/control-plane/` (gocryptfs data, including `gocryptfs.conf`). This MUST be a dedicated btrfs subvolume to support consistent snapshots for PCV export publishing.
- **Plaintext view (only when mounted):** `mounts/control-plane/` (control.db, app state, etc.).

PCV export publishing and replication should operate on the **durable ciphertext payload** (`ciphertext/control-plane/`) plus the minimal required *unlock material* (outside gocryptfs) so PCV exports can be produced while locked. Device-local-only material (e.g. LUKS header backups) must not be included in PCV exports.

### 8.2 Published PCV exports

#### Prerequisite: `ciphertext/control-plane/` as a btrfs subvolume

`ciphertext/control-plane/` **must** be created as a dedicated btrfs subvolume (not a regular directory) during first-run initialization, before gocryptfs is initialized:

```bash
btrfs subvolume create /piccolo-core/ciphertext/control-plane
```

This is performed by `piccolod` during control-plane setup (PR 2 in §11). Without a dedicated subvolume, `btrfs subvolume snapshot` will fail.

#### Consistency boundary
Since gocryptfs writes to the ciphertext directory are not atomic at the directory level, copying `ciphertext/control-plane/` while the mount is active could produce a corrupt snapshot. To ensure consistency:

- **Flush FUSE writes before snapshot:** When the control plane is mounted (unlocked), gocryptfs writes flow through FUSE and may be buffered in the kernel page cache before reaching the btrfs ciphertext directory. Call `syncfs()` on a file descriptor opened on `ciphertext/control-plane/` before taking the snapshot to ensure all in-flight FUSE writes are durable on the btrfs subvolume. When locked (no FUSE mount active), `syncfs()` is still safe (no-op for the subvolume) and keeps the code path unconditional.

- **Always use a btrfs snapshot** of `ciphertext/control-plane/` as the artifact source, regardless of whether the control plane is locked or unlocked. Since `/piccolo-core` is btrfs and `ciphertext/control-plane` is a dedicated subvolume, `btrfs subvolume snapshot -r` produces an atomic, read-only point-in-time copy. The snapshot is created in `recovery/staging/<unique-id>/`, archived into `current.enc`, then the staging snapshot is deleted via `btrfs subvolume delete`.

Using snapshots unconditionally (even when locked) keeps the publish pipeline simple — one code path, no conditional logic based on mount state.

**Concurrency:** A single-flight mechanism (mutex) ensures only one publish runs at a time. On startup, any leftover `recovery/staging/*` snapshots from a previous crash are cleaned up (via `btrfs subvolume delete`) before the first publish.

#### Archive format

`current.enc` is a **gzip-compressed tar archive** containing the directory tree defined in §8.2.2. The archive preserves relative paths rooted at `/piccolo-core/` so that extraction reconstructs the expected layout. The `.enc` extension is a naming convention — the archive contents are already encrypted (gocryptfs ciphertext + SDEK-wrapped keys); no additional encryption layer is applied.

**User-facing naming:** The portal surfaces PCV exports as `.pcv` files (e.g., for download or import). The on-disk name `current.enc` is an implementation detail. When a user exports or downloads a PCV, the portal renames it to `<device-name>-<timestamp>.pcv`.

`piccolod` produces a PCV export on control-plane mutations and periodically:
- build in `/piccolo-core/recovery/staging/<unique-id>/`
- create compressed tar archive
- atomic publish to `/piccolo-core/recovery/current.enc` (via write-to-temp + rename to prevent partial reads)
- publish manifest to `/piccolo-core/recovery/current.json` (via same atomic pattern)
- keep bounded history in `/piccolo-core/recovery/history/`

#### Trigger policy (Foundation defaults)
A new PCV export is published when **any** of the following conditions are met:
- **On startup (dirty-check):** When the PCV publisher starts, it compares the current `ciphertext/control-plane/` subvolume generation (via `btrfs subvolume show`) against the generation recorded in `current.json`. If the subvolume has changed since the last successful publish (or if no `current.enc` exists), an immediate publish is triggered. This catches mutations that occurred while the publisher was not running (e.g., crash between a control-plane write and the debounce-triggered publish).
- **On mutation:** a control-plane write occurs (e.g. app install/uninstall, volume creation, key rotation). Publishing uses **trailing-edge debounce**: the 30-second timer resets on each new `TopicControlStoreCommit` event, and the publish fires 30 seconds after the *last* write in a burst. This avoids publishing intermediate states during rapid sequences (e.g., bulk app install) while ensuring the final state is captured promptly.
- **Periodic:** every 6 hours if no mutation-triggered publish has occurred, to ensure a reasonably fresh artifact is always available.
- **On demand:** via `POST /api/v1/system/pcv/publish` (endpoint name TBD; for orchestrator-driven or manual workflows).

**Latch pattern:** The publisher maintains a boolean latch (`dirty`) that is set on every `TopicControlStoreCommit` event and cleared after a successful publish. The debounce timer only runs while the latch is set. This prevents redundant publishes when the periodic timer fires but no mutations have occurred since the last publish.

#### Retention policy (Foundation defaults)
- **`current.enc`:** always the latest published PCV export.
- **`history/`:** retain the most recent **5 generations**. Older exports are deleted after a successful publish. The retention count should be configurable via a control-plane setting in future milestones.

#### PCV size guard
Before finalizing a publish, the publisher checks the archive size against a configurable maximum (default: 100 MiB for Foundation). If the archive exceeds this limit, the publish is aborted, an error is logged, and `TopicPCVExportFailed` is emitted. This catches pathological growth (e.g., runaway log files accidentally written to the control plane) before it fills `/piccolo-core` with oversized PCV history.

#### Publish failure handling
- If staging or atomic rename fails, the previous `current.enc` remains intact (no partial overwrites).
- Publish failures are logged and surfaced via the health endpoint. They do not block normal operation.
- The next trigger (mutation or periodic) will retry.

To avoid implicit knowledge, the PCV export must be accompanied by a small metadata manifest that is replicated and backed up together with the payload.

#### 8.2.1 PCV export manifest (metadata; minimum schema)
Store alongside PCV exports:
- `/piccolo-core/recovery/current.json` (manifest for `current.enc`)
- `/piccolo-core/recovery/history/<gen>.json` (manifest for `<gen>.enc`)

Minimum JSON schema (v1):
```json
{
  "version": 1,
  "gen": "2026-02-02T18:04:05Z-000001",
  "created_at": "2026-02-02T18:04:05Z",
  "sha256": "<hex sha256 of the .enc payload>",
  "size_bytes": 1234567,
  "source_node_id": "node-abc123"
}
```

**Generation ID (`gen`) ordering:** The `gen` field is `<UTC timestamp>-<6-digit monotonic counter>` (e.g., `2026-02-02T18:04:05Z-000001`). The counter increments per publish and resets on daemon restart. Lexicographic ordering of `gen` values produces chronological ordering.

**Clock-skew handling:** On startup (and before each publish), the publisher reads the previous `current.json`'s `gen` and verifies the new `gen` is strictly greater (lexicographic). If the system clock has gone backward (e.g., NTP correction), the publisher reuses the *timestamp portion* from the previous `gen` and increments only the counter suffix. This ensures monotonicity without waiting for the clock to catch up. Example: if previous gen is `2026-02-02T18:04:05Z-000003` and current UTC is `2026-02-02T18:03:00Z` (clock went back), the new gen becomes `2026-02-02T18:04:05Z-000004`.

**`source_node_id`:** Identifies which node produced this PCV export. For Foundation (single-node), this is the device's stable node ID (generated at first boot and stored in the control plane). In cluster mode, this field enables peers to attribute PCV exports to their source.

**Resolution when locked:** The node ID is generated at first boot and stored in the control plane (inside gocryptfs). Since PCV exports can be produced while locked (from the ciphertext snapshot), the publisher must cache the `source_node_id` in memory after the first successful unlock. If the publisher starts while locked (e.g., periodic publish before admin unlock), `source_node_id` falls back to reading the previous `current.json`'s value. On a brand-new device (no previous PCV, still locked), the field is set to an empty string and updated on the first post-unlock publish.

Optional fields (reserved for future cluster/orchestrator workflows):
- `control_plane_schema_version`
- `min_piccolod_version`
- `notes`

#### 8.2.2 PCV export payload contract (what is inside `current.enc`)
This RFC standardizes `current.enc` as the **portable PCV export**. A restore on new hardware must be possible using:
- the PCV export (`current.enc` + `current.json`), and
- the user secret (admin password / recovery key).

Minimum required contents (v1):
- `ciphertext/control-plane/**` (the durable ciphertext payload; exported from a consistent snapshot)
- `volumes/control-plane/piccolo.volume.json` (wrapped gocryptfs passphrase)
- `crypto/keyset.json` (sealed SDEK)
- `crypto/piccolo_data_pool_key.enc` (wrapped `/piccolo-data` pool keyfile — node-scoped, see below)
- `crypto/luks-kdf-params/*.json` (Argon2 derivation parameters per LUKS device — node-scoped)

Explicitly excluded (MUST NOT be bundled into PCV):
- `crypto/luks-header-backups/**` (LUKS headers are device-specific and not portable)
- `network-bootstrap/**` (pre-unlock device-local remote/bootstrap state)
- any other device-specific runtime files (e.g. rotation progress markers)

**Per-node rooms within PCV:** Some PCV contents are inherently node-scoped (pool keyfiles, KDF params, LUKS header references). In cluster mode, each peer contributes its own node-scoped data to the shared control plane. The PCV export bundles all of it — 1 user account = 1 PCV bundle, with logical partitions ("rooms") per cluster peer inside the control plane database. This ensures:
- **Restore on same hardware:** pool keyfile from the node's room unlocks existing `/piccolo-data` disks.
- **Restore on new hardware:** the new node generates a fresh pool keyfile and enrolls it via LUKS keyslot 0 (existing disks unlocked via admin password keyslot 1 or mnemonic keyslot 2 during bootstrap).
- **Cluster join:** a new peer creates its own room; existing peers' rooms are read-only to it.

The per-node room schema is defined during the cluster RFC (Milestone C). For Foundation (single-node), all node-scoped data is implicitly "this node's room".

### 8.3 Replication to peers (cluster mode)
Rather than replicating a live control-plane filesystem, we replicate **published PCV exports**:
- replicate `current.enc` + the corresponding manifest (`current.json`)
- persist local redundant copies at:
  - `/piccolo-data/system-objects/control-plane-backups/<gen>.enc`
  - `/piccolo-data/system-objects/control-plane-backups/<gen>.json`

This keeps the replication surface small and decoupled from the app volume v2 replication design.

#### 8.3.1 Promotion gating (future, reserved contract)
Cluster mode is out of scope for Foundation, but we reserve the following invariant:
- A node must not promote itself to a “kernel/control-plane writer” role unless it has (or can fetch) the latest published PCV export generation (`gen`) and its manifest.

The exact consensus mechanism (etcd keys, ACK quorum rules) will be specified in the cluster RFC, but the manifest schema in §8.2.1 is intended to be stable and referenced by that future work.

### 8.4 PCV restore (recovery flow)

The restore algorithm is specified in `org-context/03_engineering/storage_architecture.md` (§14 Recovery & Disaster Recovery). At a high level:

1. Flash the base piccolo-os image to the target device.
2. Prepare `/piccolo-core` for import:
   - Ensure `ciphertext/control-plane/` is created as a **btrfs subvolume** (not a regular directory) before extracting PCV contents. Without this, subsequent PCV exports will fail (`btrfs subvolume snapshot` requires a subvolume source).
   - Create the subvolume: `btrfs subvolume create /piccolo-core/ciphertext/control-plane`
   - Extract PCV archive contents into the prepared layout.
3. The user provides their admin password or recovery mnemonic to unlock the control plane.
4. Once the control plane is available, the device can either:
   - Use an existing `/piccolo-data` partition (if the disk was preserved), or
   - Fetch data from a cluster peer or federation source (if available).

This RFC does not prescribe the full restore UX or automation — it only ensures the PCV export format (§8.2.2) contains the minimum required material for restore and that the import procedure reconstructs the required btrfs subvolume structure.

#### 8.4.1 PCV import contract

**Authenticity model:** PCV exports use AES-GCM authenticated encryption (via gocryptfs). A PCV that can be successfully decrypted with the user's admin password or recovery mnemonic is authentic — no separate signature or HMAC is needed. AES-GCM's authentication tag ensures both integrity (no tampering) and authenticity (only someone with the correct password could have produced valid ciphertext). A corrupted or tampered PCV will fail at the gocryptfs mount step with an authentication error.

**Import procedure (programmatic contract):**
1. Verify the PCV archive's SHA-256 matches the accompanying manifest (`current.json`).
2. Extract the archive into a staging area.
3. Ensure `ciphertext/control-plane/` is a btrfs subvolume (create if needed, or replace existing).
4. Move extracted contents into the `/piccolo-core` layout (atomic where possible).
5. Attempt unlock with the user-provided secret (admin password or recovery mnemonic).
6. If unlock succeeds → import is valid, proceed with normal startup.
7. If unlock fails → report error, do not modify existing state.

**Conflict handling:** If a control plane already exists on the device, the import replaces it entirely. The previous control plane's `ciphertext/control-plane/` subvolume is renamed (e.g., `ciphertext/control-plane.bak.<timestamp>`) before extraction, and deleted after successful unlock of the imported PCV. If unlock fails, the backup is restored.

**Implementation timeline:** The full PCV import implementation (portal UX, API endpoint, conflict handling) is deferred to post-Foundation. For Foundation, the contract and round-trip invariant (export → import → unlock succeeds) must be validated via integration tests, but the user-facing import flow is not required.

**Round-trip test requirement:** An integration test must verify: create control plane → write known state → export PCV → wipe `/piccolo-core` → import PCV → unlock → verify known state is intact. This test validates the export format, btrfs subvolume reconstruction, and the authenticity model end-to-end.

### 8.5 Backup to orchestrator
The orchestrator may store:
- the PCV export bundle(s) (`current.enc`, bounded history), and/or
- associated manifests/metadata for device recovery workflows.

This RFC does not prescribe orchestrator protocol details; it only standardizes the artifact format and on-device paths.

## 9) Migration and compatibility strategy

### 9.1 Pre-beta stance
We will take a **breaking change** stance for beta velocity:
- no `PICCOLO_STATE_DIR` support,
- no “legacy gocryptfs per-app volume” runtime dual-stack.

If legacy support becomes necessary later, prefer:
- a **one-shot importer** tool, or
- an explicitly **time-boxed** compatibility path behind a storage schema/version gate.

### 9.2 Post-beta stance
After beta, we should assume:
- compatibility requirements harden (schema/layout migrations must be explicit),
- “carry old behaviors forever” becomes expensive.

If needed, we can introduce a **one-shot import tool** for legacy layouts, but avoid keeping multiple runtime engines long-term.

## 10) Phased implementation plan (releases)

### 10.1 Milestone A — Foundation (this RFC)
Deliverable: `piccolod` runs with the new roots and PCV export publishing/replication, and has the `/piccolo-data` mount/unlock sequencing scaffolding.

Scope (high-level):
- Replace `PICCOLO_STATE_DIR` with `PICCOLO_CORE_ROOT`/`PICCOLO_DATA_ROOT` across code + docs + tests.
- Implement the directory layout and path resolution contract.
- Replace the "bootstrap volume" concept with `network-bootstrap` on `/piccolo-core`.
- Implement control-plane storage using the distributed layout under `/piccolo-core` (`ciphertext/control-plane/`, `crypto/`, `volumes/control-plane/`, `mounts/control-plane/` — see §5.1) and PCV exports under `/piccolo-core/recovery/`.
- Implement `/piccolo-data` pool mount orchestration (initially: mount detection + layout creation; then LUKS2 integration).

### 10.2 Milestone B — App volumes v2 (single-node)
Deliverable: app/VM volumes mount at `/piccolo-core/mounts/<vol-id>`, backed by per-volume engines under `/piccolo-data/user/volumes/<vol-id>/`.

Notes:
- JuiceFS volumes are backed by the local ciphertext object store first; supporting an optional S3-compatible backend is future work.

### 10.3 Milestone C — Cluster mode
Deliverable: etcd leases + epochs, replication streams, correctness gating, CP/RPO policies, safe promotion rules.

### 10.4 Milestone D — PSFN federation
Deliverable: federation tiers, `psfn-service`, cold-tier recall.

## 11) Foundation work breakdown (PR-sized plan)
This section intentionally outlines a layered implementation approach (reviewable PRs) while still landing as one release milestone.

1. **Paths + env vars**
   - Introduce `PICCOLO_CORE_ROOT` and `PICCOLO_DATA_ROOT`.
   - Remove `PICCOLO_STATE_DIR` usage and tests/guards for it.
   - **Migration sequencing:** The `PICCOLO_STATE_DIR` removal must be ordered carefully:
     1. First, add `paths.CoreRoot()` and `paths.DataRoot()` as **additive** changes (new functions, no removals). Callers can be migrated incrementally.
     2. Migrate all callers from `paths.Root()` / `paths.Join()` to `paths.CoreRoot()` / `paths.DataRoot()` (this is the bulk of the work — 40+ call sites across code, tests, Makefile, docs).
     3. **Last:** Add the fail-fast guard (`if PICCOLO_STATE_DIR is set → error`) and remove `paths.Root()`. This must be the final step to avoid breaking intermediate commits where some callers still use the old path.
2. **Control-plane path move**
   - Anchor control-plane material under the distributed layout in §5.1: `crypto/`, `ciphertext/control-plane/`, `volumes/control-plane/`, `mounts/control-plane/`.
   - Create `ciphertext/control-plane/` as a btrfs subvolume during first-run initialization (required for PCV export snapshots — see §8.2).
   - Ensure no control-plane writes occur outside `/piccolo-core`.
   - **Volume ID migration:** The existing codebase uses `"control"` as the volume ID (e.g., in `internal/persistence/service.go` `ensureCoreVolumes()`). This RFC standardizes on `"control-plane"` to match the architecture doc. PR 2 must update all volume ID references from `"control"` to `"control-plane"` (constants, path construction, volume metadata). Since there are no active deployments, no runtime migration logic is needed — this is a code-only rename.
3. **Network-bootstrap**
   - Replace the current “bootstrap volume” usage in remote config/certs with `/piccolo-core/network-bootstrap/`.
   - Preserve “remote before unlock” semantics via TPM-sealed state later.
4. **PCV exports**
   - Implement `recovery/current.enc` publish pipeline and bounded history.
   - Define the PCV export manifest schema and hashing.
5. **`/piccolo-data` bring-up scaffolding**
   - Mount detection + directory creation.
   - NOCOW posture enforcement.
   - Add LUKS2 unlock/mount sequencing once control plane can supply `piccolo_data_pool_key`.
   - Reuse existing filesystem utilities (e.g., `internal/fsutil/` if present, or the atomic-write helpers from persistence) for operations like atomic file writes, directory creation with permissions, and NOCOW attribute setting.
6. **Docs + ops**
   - Update docs to remove `PICCOLO_STATE_DIR` references.
   - Provide operator notes for core/data roots and “locked vs unlocked” behavior.

## 12) Foundation acceptance criteria
- `piccolod` starts with `PICCOLO_CORE_ROOT=/piccolo-core` and does not write to legacy state paths.
- `PICCOLO_STATE_DIR` is rejected with a clear startup error.
- Remote config/bootstrap state is readable pre-unlock via `/piccolo-core/network-bootstrap/`.
- Control plane material is stored under the §5.1 layout (`crypto/`, `ciphertext/control-plane/`, `volumes/control-plane/`, `mounts/control-plane/`) and can be unlocked using the admin password.
- `ciphertext/control-plane/` is a btrfs subvolume (required for consistent PCV snapshots).
- `piccolod` publishes PCV exports to `/piccolo-core/recovery/current.enc` atomically (readers never observe a partial file), with a valid `current.json` manifest (sha256 matches payload, size_bytes is correct), and maintains bounded history (≤ 5 generations in `history/`).
- After unlock, `/piccolo-data` is mounted (or a clear actionable error is surfaced), the directory layout is created, and NOCOW posture is enforced where required.

## 13) Risks and mitigations
- **Privilege boundaries:** unlocking LUKS and mounting filesystems may require elevated privileges depending on deployment; isolate these operations behind a storage service boundary with explicit error reporting.
- **Early coupling:** avoid tying control-plane availability to the app volume v2 engine; keep control plane simple until app volumes + cluster replication are stable.
- **Btrfs/NOCOW correctness:** ensure NOCOW is applied before creating DB files; add checks/tests to prevent regressions.

## 14) ExportManager replacement

The existing `ExportManager` interface (`RunControlPlane`, `RunFullData`, `ImportControlPlane`, `ImportFullData`) in `internal/persistence/interfaces.go` and its `fileExportManager` implementation are **removed entirely** and replaced by the new PCV export pipeline.

**Removal checklist:**
- Delete `ExportManager` interface from `internal/persistence/interfaces.go`.
- Delete `fileExportManager` implementation (and any related types/helpers).
- Remove all callers (API handlers, supervisor hooks) that reference `ExportManager`.
- Remove associated tests.
- The `Service` struct in `internal/persistence/service.go` drops its `ExportManager` field.
- Delete `BootstrapStore` and `BootstrapVolume` interfaces from `internal/persistence/interfaces.go` (replaced by direct filesystem access to `/piccolo-core/network-bootstrap/`).
- Remove the bootstrap volume concept from `VolumeManager` — the control plane is the only gocryptfs volume; network-bootstrap is a plain directory.
- Remove associated bootstrap volume initialization logic from `service.go`'s `ensureCoreVolumes()`.

**What replaces it:**
- `ExportManager.RunControlPlane` → PCV publisher (btrfs snapshot + compressed tar + atomic publish to `recovery/current.enc`).
- `ExportManager.RunFullData` → removed entirely for Foundation. Full data export will be redesigned as part of Milestone B (app volumes v2) since the data layout changes fundamentally.
- `ExportManager.ImportControlPlane` / `ImportFullData` → PCV import (extract compressed tar to reconstruct `/piccolo-core` layout, ensuring `ciphertext/control-plane/` is a btrfs subvolume — see §8.4). Import is required for the recovery flow (flash base image → import PCV → unlock).

**Integration point for mutation-triggered publishes:**
The PCV publisher subscribes to `TopicControlStoreCommit` events (from the existing event bus) with a trailing-edge 30-second debounce (see below). The periodic 6-hour publish runs on a separate timer. Both feed into the same single-flight publish pipeline.

`TopicControlStoreCommit` is emitted by the persistence layer (`internal/persistence/`) after any durable write to the control-plane store (e.g., app state change, volume metadata update, key rotation, settings change). It signals that the on-disk ciphertext has changed and a new PCV export should be queued. The event payload includes a monotonic sequence number for ordering.

**New event bus topics:**
- `TopicPCVExportPublished` — emitted after a successful publish (payload: generation ID, sha256).
- `TopicPCVExportFailed` — emitted on publish failure (payload: error details). The health endpoint aggregates these for the portal.

**Supervisor registration:** The PCV publisher registers as a supervisor component (`internal/runtime/supervisor/`) with a `Start`/`Stop` lifecycle. `Start` cleans up leftover staging snapshots and begins listening for `TopicControlStoreCommit` events and the periodic timer. `Stop` cancels pending debounce timers, waits for any in-flight publish to complete (with a timeout), and unsubscribes from the event bus.

## 15) Development environment

### 15.1 Btrfs requirement for PCV exports

PCV exports rely on `btrfs subvolume snapshot -r` for consistency. Developer machines must use a btrfs-backed directory for `PICCOLO_CORE_ROOT`. The recommended setup:

```bash
# Create a btrfs loopback filesystem for development
truncate -s 2G /tmp/piccolo-dev-core.img
mkfs.btrfs /tmp/piccolo-dev-core.img
mkdir -p .run-state/core
sudo mount -o loop /tmp/piccolo-dev-core.img .run-state/core
```

The `Makefile` targets (`make run`, `make run-fresh`) will be updated to:
- Create `PICCOLO_CORE_ROOT=.run-state/core` and `PICCOLO_DATA_ROOT=.run-state/data` instead of `PICCOLO_STATE_DIR=.run-state`.
- Optionally create a btrfs loopback for `.run-state/core` (with a `make setup-dev-btrfs` helper).
- If btrfs is not available (detected via `stat -f -c %T`), PCV export tests that require snapshots are skipped with a clear message.

**Dev/test fallback (non-btrfs):** For developers who cannot easily set up a btrfs loopback (e.g., macOS with Docker, CI without btrfs), the PCV publisher should support a **copy-based fallback** that uses `cp -a` instead of `btrfs subvolume snapshot`. This fallback is NOT crash-consistent and MUST NOT be used in production. It is gated behind an explicit env var (`PICCOLO_DEV_NO_BTRFS=1`) and logs a prominent warning on every publish. The fallback enables development and basic testing of the PCV pipeline without requiring btrfs infrastructure.

### 15.2 Test harness

The existing `SetRootForTest` in the paths package becomes two functions:
- `SetCoreRootForTest(t, dir)` — sets `PICCOLO_CORE_ROOT` for the test and restores on cleanup.
- `SetDataRootForTest(t, dir)` — sets `PICCOLO_DATA_ROOT` for the test and restores on cleanup.

Convenience helper for tests that need both roots:
- `SetRootsForTest(t) (coreDir, dataDir string)` — creates two `t.TempDir()` directories, sets both env vars, and returns the paths. This covers the common case where a test needs a clean two-root environment.

## 16) Non-goals and deferred decisions

**Pool keyfile rotation:** Rotating the `/piccolo-data` pool keyfile (LUKS keyslot 0) is explicitly a non-goal for Foundation and unlikely to be needed. The pool keyfile is 64 random bytes with full entropy — it does not degrade over time. A stolen PCV export already gives an attacker the encrypted pool keyfile (wrapped with SDEK), and the admin password or recovery mnemonic can unlock the same LUKS devices via keyslots 1 and 2. Rotating the pool keyfile would not meaningfully improve security given these parallel unlock paths. If rotation becomes necessary in the future (e.g., for compliance), it can be added as a LUKS keyslot 0 replacement operation.

## 17) Open questions
- What is the minimal PCV export manifest schema required for cluster promotion and orchestrator restore flows?
- Do we want a formal "storage schema version" marker (e.g. in `mounts/control-plane/` or the PCV manifest) to gate upgrades?
- Which components should be allowed to function pre-unlock (read-only APIs, discovery, remote enrollment), and which must hard-block?
- If/when we remove gocryptfs from the control plane, should we prefer a non-FUSE encrypted embedded DB (to reduce dependencies) rather than running the control plane on the v2 volume engine?

## Implementation Notes & Status
- 2026-02-02: Drafted. No code changes landed yet. Next step is to implement Foundation via the PR breakdown in §11.
- 2026-02-04: Review pass. Fixed: (1) resolved path inconsistency — sections 10/11/12 now reference the distributed §5.1 layout instead of the nonexistent `/piccolo-core/control-plane/` aggregate path; (2) specified PCV export format as gzip-compressed tar; (3) added `ciphertext/control-plane/` btrfs subvolume creation requirement; (4) specified ExportManager replacement strategy with event bus integration; (5) added dev environment section with btrfs loopback guidance; (6) corrected `piccolo_data_pool_key` terminology — stored outside gocryptfs, encrypted with SDEK, device-local; (7) added publish concurrency control (single-flight + staging cleanup); (8) refined acceptance criteria for testability.
- 2026-02-04: Parallel review fixes. Fixed: (9) `ciphertext/` directory uses no dot prefix (matches existing codebase convention); (10) added §8.4 PCV restore reference (delegates to architecture doc); (11) no org-context schema references to remove (RFC defines its own minimal manifest schema inline).
- 2026-02-06: Second parallel review fixes. Fixed: (12) resolved pool key PCV inclusion contradiction — pool key IS included in PCV (node-scoped data), updated §4.1 and directory classification; (13) added per-node room concept in §8.2.2 for cluster-mode PCV scoping; (14) added `crypto/luks-kdf-params/` to PCV payload and directory layout; (15) fixed stale open question path `/piccolo-core/control-plane/` → `mounts/control-plane/`.
- 2026-02-06: Third review pass. Blocking fixes: (16) PCV restore must create `ciphertext/control-plane/` as btrfs subvolume before extraction (§8.4); (17) `TopicControlStoreCommit` definition and payload clarified (§14); (18) always use btrfs snapshot for PCV export (no conditional locked/unlocked path). Significant fixes: (19) trailing-edge 30s debounce semantics specified; (20) staging snapshot cleanup uses `btrfs subvolume delete`; (21) gen ordering semantics documented (lexicographic = chronological, clock-skew handling); (22) PCV size guard added (100 MiB default); (23) ExportManager removal checklist expanded; (24) `source_node_id` added to manifest schema. Suggestions: (25) supervisor registration for PCV publisher; (26) fsutil reuse note; (27) directory permissions documented (0700); (28) `SetRootsForTest` convenience helper.
- 2026-02-06: Fourth review pass (combined assessment). Blocking/critical fixes: (29) added `syncfs()` flush before btrfs snapshot to capture in-flight FUSE writes; (30) PCV import contract specified (§8.4.1) with authenticity model (AES-GCM decryptability = authenticity), conflict handling, and round-trip test requirement; (31) generation ID clock-skew handling specified — reuse previous timestamp when clock goes backward. Significant fixes: (32) TPM adapter model documented in §7.3 — all devices get remote reachability via software emulation on non-TPM; (33) `control` → `control-plane` volume ID migration noted in PR 2; (34) `BootstrapStore`/`BootstrapVolume` added to ExportManager removal checklist; (35) `PICCOLO_STATE_DIR` migration sequencing specified (additive first, fail-fast guard last); (36) startup dirty-check + latch pattern for PCV publisher; (37) `source_node_id` resolution when locked (cache + fallback); (38) dev/test btrfs fallback (`PICCOLO_DEV_NO_BTRFS=1`, copy-based, not crash-consistent); (39) pool keyfile rotation documented as non-goal (§16).
