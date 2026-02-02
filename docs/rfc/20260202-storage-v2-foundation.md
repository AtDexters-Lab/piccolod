# RFC: Storage v2 Foundation (Core/Data roots + control-plane recovery replication)

**Date:** 2026-02-02  
**Status:** Draft

## 1) Summary
This RFC tracks the **storage v2 migration** for `piccolod` based on the updated Piccolo storage architecture.

Key decisions captured here:
- Replace `PICCOLO_STATE_DIR` with **two roots**: `PICCOLO_CORE_ROOT` (default `/piccolo-core`) and `PICCOLO_DATA_ROOT` (default `/piccolo-data`).
- Keep the **control plane encrypted store** on `/piccolo-core` using a **gocryptfs-like** mechanism (simple, boot-critical).
- Replicate the control plane via **published encrypted recovery artifacts** (not by replicating a live filesystem), leaving room for separate orchestrator backup flows.
- Treat “network-bootstrap” as the only pre-unlock bootstrap layer (remote access before admin unlock).
- Merge the previous “phase 1/2/3” work into a single release milestone called **Foundation**, but still implement it via layered PRs.

## 2) Goals
- **Eliminate `PICCOLO_STATE_DIR`** and any implicit “single state root” assumption.
- Make `piccolod` conform to the **two-root layout**:
  - `/piccolo-core` for boot-critical state and mountpoints.
  - `/piccolo-data` for grow-with-time user and replication data.
- Implement a stable **control-plane persistence + recovery artifact** contract suitable for:
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

## 5) Proposed directory layout (v2 contract)

### 5.1 `/piccolo-core` (fixed / internal)
Holds boot-critical state and mount plumbing:

```
/piccolo-core
  /network-bootstrap/            # pre-unlock remote/bootstrap state (TPM-sealed later)
  /clusterdb/etcd/               # etcd data dir (cluster mode only; system plane)
  /control-plane/                # authoritative encrypted control plane store
  /recovery/                     # control-plane snapshots (encrypted artifacts)
    /current.enc                 # latest published snapshot
    /history/                    # bounded history
    /staging/                    # build area for atomic publish
  /mounts/
    /<vol-id>/                   # immutable mountpoints (one per volume)
```

**Mountpoint rule:** all volume mountpoints are on `/piccolo-core/mounts/<vol-id>`.

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
    /control-plane-backups/      # encrypted replicas of control-plane artifacts
    /volume-checkpoints/         # metadata snapshots/checkpoints (future)
```

**CoW posture:** NOCOW should be set *before* files are created for:
- `/piccolo-data/federation/`
- `/piccolo-data/node/`
- `/piccolo-data/user/volumes/<vol-id>/cache/`
- `/piccolo-data/user/volumes/<vol-id>/meta/`

## 6) Configuration: environment variables

### 6.1 New variables
- `PICCOLO_CORE_ROOT` (default `/piccolo-core`)
- `PICCOLO_DATA_ROOT` (default `/piccolo-data`)

All path resolution must go through `internal/state/paths` (or the successor package if renamed).

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
- Read the wrapped `piccolo_data_pool_key` from the control plane.
- Use it to unlock and mount `/piccolo-data` (LUKS2 + btrfs pool).
- Ensure the directory layout exists and apply NOCOW posture where required.
- Resume/start services that require `/piccolo-data`.

## 8) Control plane persistence + replication (artifact-based)

### 8.1 Authoritative storage
- Control plane encrypted store lives at: `/piccolo-core/control-plane/`.

### 8.2 Published recovery artifacts
`piccolod` periodically produces an encrypted snapshot artifact:
- build in `/piccolo-core/recovery/staging/`
- atomic publish to `/piccolo-core/recovery/current.enc`
- keep bounded history in `/piccolo-core/recovery/history/`

### 8.3 Replication to peers (cluster mode)
Rather than replicating a live control-plane filesystem, we replicate **published artifacts**:
- replicate `current.enc` + a small manifest (generation ID, hash, timestamps)
- persist local redundant copies at:
  - `/piccolo-data/system-objects/control-plane-backups/<gen>.enc`

This keeps the replication surface small and decoupled from the app volume v2 replication design.

### 8.4 Backup to orchestrator
The orchestrator may store:
- the encrypted recovery bundle(s) (`current.enc`, bounded history), and/or
- associated manifests/metadata for device recovery workflows.

This RFC does not prescribe orchestrator protocol details; it only standardizes the artifact format and on-device paths.

## 9) Migration and compatibility strategy

### 9.1 Pre-beta stance
We will take a **breaking change** stance for beta velocity:
- no `PICCOLO_STATE_DIR` support,
- no “legacy gocryptfs per-app volume” runtime dual-stack.

### 9.2 Post-beta stance
After beta, we should assume:
- compatibility requirements harden (schema/layout migrations must be explicit),
- “carry old behaviors forever” becomes expensive.

If needed, we can introduce a **one-shot import tool** for legacy layouts, but avoid keeping multiple runtime engines long-term.

## 10) Phased implementation plan (releases)

### 10.1 Milestone A — Foundation (this RFC)
Deliverable: `piccolod` runs with the new roots and control-plane recovery artifacts, and has the `/piccolo-data` mount/unlock sequencing scaffolding.

Scope (high-level):
- Replace `PICCOLO_STATE_DIR` with `PICCOLO_CORE_ROOT`/`PICCOLO_DATA_ROOT` across code + docs + tests.
- Implement the directory layout and path resolution contract.
- Replace the “bootstrap volume” concept with `network-bootstrap` on `/piccolo-core`.
- Implement control-plane store under `/piccolo-core/control-plane/` and recovery artifacts under `/piccolo-core/recovery/`.
- Implement `/piccolo-data` pool mount orchestration (initially: mount detection + layout creation; then LUKS2 integration).

### 10.2 Milestone B — App volumes v2 (single-node)
Deliverable: app/VM volumes mount at `/piccolo-core/mounts/<vol-id>`, backed by per-volume engines under `/piccolo-data/user/volumes/<vol-id>/`.

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
   - Anchor encrypted control plane at `/piccolo-core/control-plane/`.
   - Ensure no control-plane writes occur outside `/piccolo-core`.
3. **Network-bootstrap**
   - Replace the current “bootstrap volume” usage in remote config/certs with `/piccolo-core/network-bootstrap/`.
   - Preserve “remote before unlock” semantics via TPM-sealed state later.
4. **Recovery artifacts**
   - Implement `recovery/current.enc` publish pipeline and bounded history.
   - Define artifact manifest schema and hashing.
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
- Control plane is stored under `/piccolo-core/control-plane/` and can be unlocked using the admin password.
- `piccolod` can publish `/piccolo-core/recovery/current.enc` atomically and maintain bounded history.
- After unlock, `/piccolo-data` is mounted (or a clear actionable error is surfaced), the directory layout is created, and NOCOW posture is enforced where required.

## 13) Risks and mitigations
- **Privilege boundaries:** unlocking LUKS and mounting filesystems may require elevated privileges depending on deployment; isolate these operations behind a storage service boundary with explicit error reporting.
- **Early coupling:** avoid tying control-plane availability to the app volume v2 engine; keep control plane simple until app volumes + cluster replication are stable.
- **Btrfs/NOCOW correctness:** ensure NOCOW is applied before creating DB files; add checks/tests to prevent regressions.

## 14) Open questions
- What is the minimal artifact manifest schema required for cluster promotion and orchestrator restore flows?
- Do we want a formal “storage schema version” marker under `/piccolo-core/control-plane/` to gate upgrades?
- Which components should be allowed to function pre-unlock (read-only APIs, discovery, remote enrollment), and which must hard-block?

## Implementation Notes & Status
- 2026-02-02: Drafted. No code changes landed yet. Next step is to implement Foundation via the PR breakdown in §11.

