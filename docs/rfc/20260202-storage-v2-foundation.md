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
- **`piccolo_data_pool_key`**: a pool-scoped keyfile (64 random bytes) used to unlock all `/piccolo-data` LUKS2 devices. It is stored encrypted with SDEK at `/piccolo-core/crypto/piccolo_data_pool_key.enc` — **outside** gocryptfs (always readable once SDEK is available), device-local, and not included in PCV exports. The plaintext keyfile is materialized only transiently in tmpfs (e.g. under `/run/piccolo/`) during `cryptsetup` operations.
- **PCV export**: the portable encrypted export bundle produced from the control plane’s durable encrypted payload for replication/backup/restore workflows. On-device, the latest PCV export is stored at `/piccolo-core/recovery/current.enc` (with an accompanying metadata manifest at `/piccolo-core/recovery/current.json`).

## 5) Proposed directory layout (v2 contract)

### 5.1 `/piccolo-core` (fixed / internal)
Holds boot-critical state and mount plumbing:

```
/piccolo-core
  /crypto/                         # key material (outside gocryptfs, always readable)
    /keyset.json                   # SDEK sealed with KEK
    /piccolo_data_pool_key.enc     # LUKS pool keyfile wrapped with SDEK
    /luks-header-backups/          # LUKS header backups (device-specific)
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

**Directory classification:**
- **Portable (replicated to peers):** `recovery/`
- **Device-local (not replicated):** `crypto/`, `ciphertext/`, `volumes/`, `network-bootstrap/`

**Note:** “Device-local” here means these directories are not replicated *as live state*. The portable PCV export under `recovery/` MUST bundle the minimal required subset to restore on new hardware (see §8.2.2).

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
- **Mixed sensitivity:** it may contain non-secret state (e.g. onboarding choice) and, later, TPM-sealed enrollment credentials (future work).

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

- **Use a btrfs snapshot** of `ciphertext/control-plane/` as the artifact source. Since `/piccolo-core` is btrfs and `ciphertext/control-plane` is a dedicated subvolume, `btrfs subvolume snapshot -r` produces an atomic, read-only point-in-time copy. The snapshot is created in `recovery/staging/<unique-id>/`, archived into `current.enc`, then deleted.
- If the control plane is **locked** (gocryptfs unmounted), the ciphertext directory is quiescent and can be copied directly without a snapshot.

**Concurrency:** A single-flight mechanism (mutex) ensures only one publish runs at a time. On startup, any leftover `recovery/staging/*` artifacts from a previous crash are cleaned up before the first publish.

#### Archive format

`current.enc` is a **gzip-compressed tar archive** containing the directory tree defined in §8.2.2. The archive preserves relative paths rooted at `/piccolo-core/` so that extraction reconstructs the expected layout. The `.enc` extension is a naming convention — the archive contents are already encrypted (gocryptfs ciphertext + SDEK-wrapped keys); no additional encryption layer is applied.

`piccolod` produces a PCV export on control-plane mutations and periodically:
- build in `/piccolo-core/recovery/staging/<unique-id>/`
- create compressed tar archive
- atomic publish to `/piccolo-core/recovery/current.enc` (via write-to-temp + rename to prevent partial reads)
- publish manifest to `/piccolo-core/recovery/current.json` (via same atomic pattern)
- keep bounded history in `/piccolo-core/recovery/history/`

#### Trigger policy (Foundation defaults)
A new PCV export is published when **any** of the following conditions are met:
- **On mutation:** a control-plane write occurs (e.g. app install/uninstall, volume creation, key rotation). Publishing is debounced — at most once per 30 seconds after a burst of writes.
- **Periodic:** every 6 hours if no mutation-triggered publish has occurred, to ensure a reasonably fresh artifact is always available.
- **On demand:** via `POST /api/v1/system/pcv/publish` (endpoint name TBD; for orchestrator-driven or manual workflows).

#### Retention policy (Foundation defaults)
- **`current.enc`:** always the latest published PCV export.
- **`history/`:** retain the most recent **5 generations**. Older exports are deleted after a successful publish. The retention count should be configurable via a control-plane setting in future milestones.

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
  "size_bytes": 1234567
}
```

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
- `crypto/piccolo_data_pool_key.enc` (wrapped `/piccolo-data` pool keyfile)

Explicitly excluded (device-local only; MUST NOT be bundled into PCV):
- `crypto/luks-header-backups/**` (LUKS headers are not portable backups)
- `network-bootstrap/**` (pre-unlock device-local remote/bootstrap state)
- any other device-specific runtime files (e.g. rotation progress markers)

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
2. Import the PCV export (`current.enc`) into the fresh `/piccolo-core` layout.
3. The user provides their admin password or recovery mnemonic to unlock the control plane.
4. Once the control plane is available, the device can either:
   - Use an existing `/piccolo-data` partition (if the disk was preserved), or
   - Fetch data from a cluster peer or federation source (if available).

This RFC does not prescribe the full restore UX or automation — it only ensures the PCV export format (§8.2.2) contains the minimum required material for restore.

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
2. **Control-plane path move**
   - Anchor control-plane material under the distributed layout in §5.1: `crypto/`, `ciphertext/control-plane/`, `volumes/control-plane/`, `mounts/control-plane/`.
   - Create `ciphertext/control-plane/` as a btrfs subvolume during first-run initialization (required for PCV export snapshots — see §8.2).
   - Ensure no control-plane writes occur outside `/piccolo-core`.
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

The existing `ExportManager` interface (`RunControlPlane`, `RunFullData`, `ImportControlPlane`, `ImportFullData`) in `internal/persistence/interfaces.go` and its `fileExportManager` implementation are **replaced entirely** by the new PCV export pipeline.

**What changes:**
- `ExportManager.RunControlPlane` → replaced by the PCV publisher (btrfs snapshot + compressed tar + atomic publish to `recovery/current.enc`).
- `ExportManager.RunFullData` → removed for Foundation. Full data export will be redesigned as part of Milestone B (app volumes v2) since the data layout changes fundamentally.
- `ExportManager.ImportControlPlane` / `ImportFullData` → replaced by PCV import (extract compressed tar to reconstruct `/piccolo-core` layout). Import is required for the recovery flow (flash base image → import PCV → unlock).

**Integration point for mutation-triggered publishes:**
The PCV publisher subscribes to `TopicControlStoreCommit` events (from the existing event bus) with a 30-second debounce. The periodic 6-hour publish runs on a separate timer. Both feed into the same single-flight publish pipeline.

**New event bus topics:**
- `TopicPCVExportPublished` — emitted after a successful publish (payload: generation ID, sha256).
- `TopicPCVExportFailed` — emitted on publish failure (payload: error details). The health endpoint aggregates these for the portal.

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

### 15.2 Test harness

The existing `SetRootForTest` in the paths package becomes two functions:
- `SetCoreRootForTest(t, dir)` — sets `PICCOLO_CORE_ROOT` for the test and restores on cleanup.
- `SetDataRootForTest(t, dir)` — sets `PICCOLO_DATA_ROOT` for the test and restores on cleanup.

## 16) Open questions
- What is the minimal PCV export manifest schema required for cluster promotion and orchestrator restore flows?
- Do we want a formal “storage schema version” marker under `/piccolo-core/control-plane/` to gate upgrades?
- Which components should be allowed to function pre-unlock (read-only APIs, discovery, remote enrollment), and which must hard-block?
- If/when we remove gocryptfs from the control plane, should we prefer a non-FUSE encrypted embedded DB (to reduce dependencies) rather than running the control plane on the v2 volume engine?

## Implementation Notes & Status
- 2026-02-02: Drafted. No code changes landed yet. Next step is to implement Foundation via the PR breakdown in §11.
- 2026-02-04: Review pass. Fixed: (1) resolved path inconsistency — sections 10/11/12 now reference the distributed §5.1 layout instead of the nonexistent `/piccolo-core/control-plane/` aggregate path; (2) specified PCV export format as gzip-compressed tar; (3) added `ciphertext/control-plane/` btrfs subvolume creation requirement; (4) specified ExportManager replacement strategy with event bus integration; (5) added dev environment section with btrfs loopback guidance; (6) corrected `piccolo_data_pool_key` terminology — stored outside gocryptfs, encrypted with SDEK, device-local; (7) added publish concurrency control (single-flight + staging cleanup); (8) refined acceptance criteria for testability.
- 2026-02-04: Parallel review fixes. Fixed: (9) `ciphertext/` directory uses no dot prefix (matches existing codebase convention); (10) added §8.4 PCV restore reference (delegates to architecture doc); (11) no org-context schema references to remove (RFC defines its own minimal manifest schema inline).
