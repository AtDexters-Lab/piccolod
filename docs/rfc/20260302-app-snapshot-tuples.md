# RFC: App Snapshot Tuples — Atomic Snapshots, Updates, and Replication

**Date:** 2026-03-02
**Status:** Superseded — folded into `org-context/03_engineering/storage_architecture_block_native.md`
**Depends on:** `20260227-workspace-block-native-rootfs.md` (golden LV architecture)

## 1. Summary

An app's state is a **tuple of LVM thin volumes** — per-service rootfs snapshots and a shared data volume. This tuple is the atomic unit of snapshotting, updating, rolling back, and replicating. No operation acts on a single volume in isolation; operations always act on the complete tuple to guarantee consistency.

This RFC formalizes the tuple concept, defines snapshot/update/rollback semantics, and establishes the tuple as the replication unit for cluster sync.

## 2. Motivation

Current app lifecycle operations (install, update, uninstall) treat volumes independently. `UpdateImage` is blocked for service-mode apps because there's no mechanism to atomically snapshot the current state before making changes. If an update breaks an app, there's no rollback path.

MicroOS solves this for OS updates with btrfs transactional snapshots — the OS root is snapshotted before each update, and rollback switches to the previous snapshot. Apps need the same guarantee: a pre-update snapshot of the complete state, verified health before deprecation, and instant rollback if anything goes wrong.

Additionally, cluster replication (DRBD) currently operates per-volume. Replicating a single data volume without its corresponding rootfs (or vice versa) can produce an inconsistent state on the peer. The tuple defines the consistency boundary for replication.

## 3. The App Tuple

### 3.1 Definition

An app tuple is the set of LVM thin volumes that constitute the app's complete state:

```
AppTuple(instanceID) = {
    rootfs:  { svc-rootfs-{id}--{svcName} for each service with an image },
    data:    vol-app-{id}
}
```

**Concrete example** — multi-container app `myapp` with services web, worker, db:
```
AppTuple("myapp") = {
    rootfs: {
        svc-rootfs-myapp--web,       // snapshot of golden-a1b2c3 (nginx:latest)
        svc-rootfs-myapp--worker,    // snapshot of golden-d4e5f6 (python:3.12)
        svc-rootfs-myapp--db,        // snapshot of golden-789abc (postgres:16)
    },
    data: vol-app-myapp              // contains /data/pgdata, /data/uploads, /disk/podman
}
```

**Key properties:**
- **Rootfs volumes** are read-only thin snapshots from golden LVs (service mode). The golden LVs are shared across apps — they are NOT part of the tuple.
- **The data volume** is a single encrypted thin LV containing all persistent storage subdirectories. All per-service persistent volumes (`storage.persistent.*`) are directories within this single LV.
- **Workspace mode** substitutes `ws-{id}` for the rootfs set. Workspace rootfs is writable (overlay on golden LV), so it carries user data and must be snapshotted like a data volume.

### 3.2 Tuple Identity

Each tuple state is identified by a **generation**, which captures the exact set of volume versions:

```go
type TupleGeneration struct {
    GenerationID string              // monotonic ID (e.g., "gen-1", "gen-2")
    RootfsVolIDs map[string]string   // svcName → rootfs volume ID
    DataSnapshot string              // LV snapshot name for data volume (empty = live)
    CreatedAt    time.Time
    DeprecatedAt *time.Time          // set when a newer generation is verified healthy
    Status       string              // "active" | "snapshot" | "deprecated"
}
```

The **active generation** is the one currently mounted and running. Previous generations are retained as snapshots until GC.

## 4. Snapshot Operation

A snapshot captures the entire tuple at a consistent point in time.

### 4.1 Steps

1. **Quiesce** — stop all app containers (services + network anchor). This ensures filesystem consistency (no in-flight writes).
2. **Snapshot data volume** — `lvcreate --snapshot --name snap-app-{id}--gen{N} vol-app-{id}`. This is an LVM thin snapshot — instant, copy-on-write.
3. **Record rootfs state** — rootfs volumes are read-only (service mode), so they don't need separate LVM snapshots. Just record which rootfs volume IDs are active.
4. **Persist tuple generation** — write generation metadata linking the data snapshot to the rootfs volume IDs.

### 4.2 Workspace Mode Addendum

Workspace rootfs is writable. Step 2 must also snapshot the workspace rootfs LV:
```
lvcreate --snapshot --name snap-ws-{id}--gen{N} ws-{id}
```

### 4.3 Consistency Guarantee

All snapshots are taken while containers are stopped. This provides crash-consistency at minimum. For apps with journaling filesystems (ext4 on data, btrfs on rootfs), recovery is guaranteed.

For stronger consistency, an optional `fsfreeze` can be applied before snapshotting (not required for initial implementation — stop is sufficient).

## 5. Image Update Flow

The image update operates on the tuple as an atomic unit.

### 5.1 Pre-Update Snapshot

Before any changes, snapshot the current tuple (§4). This is the rollback target.

### 5.2 Update Steps

1. **Pull new image** to shared imagestore
2. **Create new golden LV** (if image digest differs) via `EnsureGoldenLV`
3. **Snapshot the current tuple** (§4) — creates generation N
4. **Create new rootfs** for each updated service — new rootfs with versioned volume ID (`svc-rootfs-{id}--{svcName}--{shortDigest}`). LVM name limit is 128 chars; with `svc-rootfs-` (11) + instance (max ~40) + `--` (2) + service (max ~30) + `--` (2) + digest (12) = ~97 chars — well within limits.
5. **Start app** with new rootfs volumes + existing data volume (the pre-update data snapshot preserves the point-in-time state for rollback)
6. **Update tuple generation** — create generation N+1 with new rootfs volume IDs, mark generation N as "snapshot"
7. **Monitor health** — containers must be running and healthy for a configurable duration (default: 24 hours)

### 5.3 Multi-Service Image Update

For multi-service apps, ALL service images can be updated together:
```
UpdateImages(instanceID, {
    "web":    "nginx:1.26",
    "worker": "python:3.13",
    // "db" unchanged — keeps current rootfs
})
```

Services with unchanged images keep their existing rootfs volume IDs in the new generation.

### 5.4 Single-Service Shortcut

For single-service apps, the existing `UpdateImage(instanceID, tag)` API updates the single service's image. Internally, it follows the same tuple snapshot → update → verify flow.

## 6. Health Verification and Deprecation

### 6.1 Automatic Verification

After update, the system monitors app health:
- All containers must be running (not in `StatusError` or crash-looping)
- Health checks (if defined) must pass
- Duration threshold: configurable, default 24 hours

Once the health threshold is met, the pre-update snapshot generation is marked `"deprecated"`.

### 6.2 Manual Override

Users can:
- **Force-deprecate** — mark the old snapshot as deprecated immediately (confident the update is good)
- **Force-rollback** — trigger rollback to the pre-update snapshot at any time (even after auto-deprecation, if the snapshot hasn't been GC'd yet)

### 6.3 Auto-Rollback on Failure

If the updated app enters `StatusError` (containers fail to start or crash-loop within a threshold), automatic rollback is triggered:
1. Stop all containers
2. Detach new rootfs volumes
3. Re-attach old rootfs volumes (from the snapshot generation)
4. Restore data volume from snapshot (LV rename swap — §7.3)
5. Restart containers with old generation
6. Mark new generation as "failed", old generation as "active"

**Data rollback is always performed alongside rootfs rollback.** The tuple is atomic — rolling back rootfs without data risks logical inconsistency (e.g., old code encountering a migrated schema). The cost is that any data changes since the update are lost, but this is the correct trade-off for consistency.

## 7. Rollback Operation

Rollback restores the entire tuple to a previous generation.

### 7.1 Service Mode Rollback

Rollback always restores the entire tuple — both rootfs and data — to guarantee consistency:
1. Stop all containers
2. Swap active rootfs pointers to the snapshot generation's rootfs volume IDs
3. Restore data volume from snapshot via LV rename swap (§7.3)
4. Start containers

### 7.2 Workspace Mode Rollback

Workspace rootfs is writable, so both rootfs and data need restoration:
1. Stop container
2. Promote workspace rootfs snapshot LV
3. Promote data volume snapshot LV
4. Start container

### 7.3 Data Volume Rollback via LV Rename

LVM thin snapshot rollback uses rename:
```bash
# 1. Deactivate current
lvchange -an piccolo-data-vg/vol-app-myapp

# 2. Rename current to failed
lvrename piccolo-data-vg vol-app-myapp vol-app-myapp--failed-gen2

# 3. Promote snapshot to active name
lvrename piccolo-data-vg snap-app-myapp--gen1 vol-app-myapp

# 4. Activate promoted
lvchange -ay piccolo-data-vg/vol-app-myapp
```

**CoW chain preservation:** LVM thin snapshots retain their copy-on-write relationship with the origin LV after rename. The promoted snapshot (now `vol-app-myapp`) still depends on shared thin pool extents from its origin (`vol-app-myapp--failed-gen2`). The failed generation's LV **must not** be removed while the promoted snapshot references shared extents — removal would corrupt the promoted volume. GC (§8.3) must verify that no active or snapshot generation depends on a failed LV before destroying it. In practice, the promoted snapshot becomes fully self-sufficient once all shared blocks have been overwritten via CoW, but tracking this is complex — the simpler approach is to retain failed LVs until the promoted volume itself is snapshotted (creating a new independent baseline) and the old chain can be safely severed.

## 8. Garbage Collection

### 8.1 Inactive Rootfs

Rootfs volumes from deprecated generations are GC'd after a configurable retention period (default: 30 days). Once rootfs volumes are removed, their golden LVs may become unreferenced — existing `GarbageCollectGoldenLVs` handles this automatically.

### 8.2 Data Volume Snapshots

Data volume snapshots from deprecated generations are GC'd after the same retention period. These consume thin pool space (copy-on-write deltas), so timely GC is important.

### 8.3 Failed Generation Artifacts

LVs from failed generations (rollback targets that were rolled back from) exist for debugging and CoW chain integrity. A failed data LV can only be removed when no active or snapshot generation's data volume was promoted from it (i.e., the CoW dependency chain has been severed by a subsequent snapshot creating a new baseline). Failed rootfs volumes have no CoW dependency and can be removed after 7 days.

### 8.4 GC Implementation

A periodic GC task (runs daily or on admin trigger):
1. Scan all tuple generation metadata
2. Find generations with status `"deprecated"` older than retention period
3. Destroy rootfs volumes + data snapshots for those generations
4. Run `GarbageCollectGoldenLVs` to clean up unreferenced golden LVs
5. Find and remove failed-generation LVs older than 7 days

## 9. Replication (Cluster Sync)

### 9.1 Tuple as Replication Unit

When replicating to a peer (via DRBD or future mechanism), the tuple is the unit. All volumes in the tuple are replicated together or not at all. Partial replication (e.g., data without rootfs) produces an inconsistent state.

### 9.2 Implications for DRBD

Current DRBD operates per-volume. To replicate a tuple:
- Each volume in the tuple has its own DRBD resource
- A tuple-level coordination layer ensures all DRBD resources for an app reach consistent state before the peer activates the app
- On failover, the peer activates the complete tuple (all rootfs + data) atomically

### 9.3 Snapshot-Based Initial Sync

For initial sync of a new peer, the tuple snapshot mechanism provides a consistent baseline:
1. Snapshot the tuple on the primary
2. Stream each snapshot LV to the peer (block-level copy or DRBD initial sync)
3. Peer activates the snapshot as its local copy
4. DRBD switches to live replication for ongoing changes

## 10. Metadata Schema

### 10.1 Tuple Generation File

Stored at `{AppStateDir}/{instanceID}/generations.json` (alongside `app.yaml` and other app metadata). Written atomically via temp-file + rename. The initial install does **not** create a generation — generations are introduced on the first update (no rollback target exists before any update). If `generations.json` is missing or corrupted, the app continues running with the current rootfs/data (degraded mode — no rollback capability).

```json
{
  "instance_id": "myapp",
  "current_generation": "gen-2",
  "generations": [
    {
      "id": "gen-1",
      "rootfs": {
        "web": "svc-rootfs-myapp--web",
        "worker": "svc-rootfs-myapp--worker"
      },
      "data_snapshot": "snap-app-myapp--gen1",
      "created_at": "2026-03-01T10:00:00Z",
      "deprecated_at": "2026-03-02T10:00:00Z",
      "status": "deprecated"
    },
    {
      "id": "gen-2",
      "rootfs": {
        "web": "svc-rootfs-myapp--web--a1b2c3",
        "worker": "svc-rootfs-myapp--worker--d4e5f6"
      },
      "data_snapshot": "",
      "created_at": "2026-03-02T10:00:00Z",
      "deprecated_at": null,
      "status": "active"
    }
  ]
}
```

### 10.2 Integration with AppInstance

`AppInstance` gains a field pointing to the active generation:
```go
ActiveRootfs map[string]string `json:"active_rootfs,omitempty"`
```

This is the runtime pointer derived from the active generation's rootfs map. The `generations.json` file is the source of truth for tuple history; `ActiveRootfs` is a denormalized cache for fast attach-path lookups.

## 11. Non-Goals

- **Per-service granular update** — updating a single service in a multi-service app without touching others. While the tuple model supports this (only changed services get new rootfs, others keep their existing IDs), the API for this is deferred.
- **Live migration** — snapshotting without stopping containers. Requires fsfreeze or application-level quiesce hooks. Deferred.
- **Cross-node rollback** — rolling back on a peer node. Requires tuple-aware failover. Deferred to cluster replication work.
- **Automated update scheduling** — scheduling periodic image updates. This RFC defines the mechanism, not the policy.

## 12. Implementation Phases

### Phase 1: Foundation (Task #13)
- `ActiveRootfs` field in `AppInstance`
- Versioned rootfs volume IDs (`VersionedServiceRootfsVolumeID`)
- `VolumeID` override in `ServiceRootfsRequest`
- Enable `UpdateImage` for single-service service-mode apps
- Create new rootfs alongside old (no destruction)
- Attach path reads `ActiveRootfs` with legacy fallback
- Uninstall destroys all rootfs versions (scan by prefix)

### Phase 2: Tuple Snapshots
- `TupleGeneration` metadata schema
- Pre-update data volume snapshot (LVM thin snapshot)
- Generation tracking (create, deprecate, GC)

### Phase 3: Rollback
- Add `RenameLV` to `LVManager` interface and implementation
- Manual rollback API endpoint
- LV rename swap for data volume rollback
- Auto-rollback on container failure
- Startup reconciliation for partially-renamed LVs

### Phase 4: Health Verification
- Post-update health monitoring (container uptime + healthchecks)
- Auto-deprecation after health threshold
- Manual force-deprecate / force-rollback

### Phase 5: Replication
- Tuple-aware DRBD resource grouping
- Consistent tuple sync to peer
- Tuple-atomic failover
