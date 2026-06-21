# RFC: App Data Rollback Artifact Lifecycle

**Date:** 2026-06-20
**Status:** Draft

## Scope block

**Problem:** App image and service-app update flows can leave, or later encounter,
LVM rollback artifacts outside durable app metadata. A stale
`snap-app-<instance>--gen<N>` LV can survive without `generations.json`; a later
image refresh can then reuse the deterministic snapshot name, collide with the
old LV, and fail after rootfs staging and runtime interruption. Piccolo should
self-heal or route around this state instead of asking SDE users to repair LVM.

**In scope:** App data rollback artifacts for installed apps:
`snap-app-*--gen*`, `snap-app-*--manifest-*`, `vol-app-*--failed-gen*`,
`vol-app-*--failed-manifest-*`, their tuple metadata, manifest/update
transaction metadata, image-refresh snapshot allocation, app startup/reconcile,
orphan cleanup, crash/retry windows, diagnostics, and tests.

**Out of scope:** Modify App image freshness/digest behavior, workspace image
rebasing, a broad storage registry rewrite, operator-facing LVM repair UI,
rootfs/golden redesign, PCV export policy, and user-initiated data rollback
after a successful manifest/service-app update.

## Incident summary

The observed failing app had:

- an active data LV, `vol-app-piclu`;
- a stale `snap-app-piclu--gen1` LV;
- no `/piccolo-core/mounts/control-plane/apps/piclu/generations.json`;
- an image refresh failure while creating rollback snapshot
  `snap-app-piclu--gen1`.

Removing the stale snapshot unblocked the image refresh. That is useful
incident recovery evidence, but the product fix must be internal:

- stale rollback artifacts must not block future updates;
- avoidable rollback-storage failures must be detected before runtime
  interruption;
- crash windows must leave durable enough state for repair or safe routing;
- low-level LVM details should remain diagnostics, not normal user workflow.

## Existing intent

`docs/rfc/20260302-app-snapshot-tuples.md` defines an app tuple as the atomic
rollback unit: service rootfs pointers plus the app data volume. A pre-update
snapshot should be recorded in `generations.json`; rollback restores both
rootfs and data together.

`docs/rfc/20260611-service-app-update-v2.md` adds a different class of data
snapshot: transaction-private precommit snapshots for manifest/service-app
updates. These are not normal user rollback snapshots. They exist only to
restore app data if a candidate runtime mutates persistent storage before the
update commits.

The current code has both concepts, but the lifecycle ownership is split:

- image refresh creates user-visible tuple snapshots in
  `internal/app/app_manager.go`;
- manifest/service-app update creates transaction-private snapshots in
  `internal/app/installed_app_apply_transaction.go`;
- low-level orphan reconciliation in
  `internal/persistence/luks_volume_manager.go` skips rollback-looking LVs by
  name without consulting app metadata.

That split is the design smell. LVM can list and remove volumes, but only the
app layer knows whether a rollback artifact is a user rollback point, a private
transaction restore point, a failed rollback dependency, or an untracked
leftover.

## Audit

| Artifact | Current metadata owner | Current behavior | Issue | Target policy |
| --- | --- | --- | --- | --- |
| `vol-app-<id>` | volume metadata under `volumes/app-<id>` | Generic orphan reconciler removes when volume metadata is missing. | Correct for active data ownership; not enough for derived rollback artifacts. | Keep owned by persistence metadata. App rollback policy must never infer active app data from rollback names alone. |
| `snap-app-<id>--gen<N>` | `generations.json` `TupleGeneration.DataSnapshot` | Image refresh derives `N` from `NextGenNumber`, creates LV, then stores tuple metadata. Generic orphan reconcile skips all `snap-*`. | Missing or unwritten tuple metadata resets allocation to `gen1`, and skipped stale snapshots can permanently block updates. | Allocate against both tuple metadata and actual LVM state. Persist a planned/created operation marker before disruptive work. Unreferenced snapshots are cleanup candidates, but allocation must route around them even if cleanup fails. |
| `snap-app-<id>--manifest-<op>` | `ManifestUpdateTransaction.PrecommitDataSnapshotID` | Service-app update stores a planned marker before creating the snapshot, then uses it for failure rollback or cleanup. | Better crash shape, but generic orphan reconcile still cannot classify it after transaction loss. | Keep transaction-private. Cleanup requires readable transaction state or another proof that candidate-mutated data cannot need the snapshot. Unreadable or missing transaction state defaults to retained/quarantined, never user-visible rollback. |
| `vol-app-<id>--failed-gen<N>` | `TupleGeneration.FailedLVName` | Rollback records failed active data LV for GC. Generic orphan reconcile skips by pattern. | Some failed LVs may be required by CoW dependency after snapshot promotion; blind deletion is unsafe. Blind permanent skip leaks. | App-layer GC owns deletion only when tuple policy proves safe. Unknown failed LVs are retained but must not block future snapshot allocation. |
| `vol-app-<id>--failed-manifest-<op>` | tuple failed generation created by manifest transaction recovery | Manifest restore records failed LV for later tuple GC. | Same CoW-retention concern as failed-gen. | Same failed-LV policy: retain unless tuple state proves safe; diagnostics only. |
| service rootfs LVs | `AppInstance.ActiveRootfs`, rootfs metadata, tuple generations | Rootfs managers and tuple GC handle rootfs retention/cleanup. | Adjacent but not the incident class. | Out of this RFC except where image refresh transaction state must keep rootfs/data rollback boundaries consistent. |

## Design

### App layer owns rollback artifact meaning

Introduce an app-layer rollback artifact reconciler/helper. Its responsibility
is policy, not raw LVM mechanics:

- load app metadata, tuple metadata, and pending update transactions;
- list rollback-shaped LVs from the volume manager;
- classify them as referenced, planned, transaction-private, failed dependency,
  unreferenced cleanup candidate, or unknown retained artifact;
- allocate new snapshot names by consulting both metadata and actual LVM names;
- ask the persistence layer to create, destroy, or health-check LVs.

The persistence layer remains the low-level LVM executor. It should not use
name-only app policy such as "skip every `snap-*` forever." When it lacks app
context, it must be conservative. App-aware cleanup belongs above it.

This is intentionally narrower than a broad storage registry. We need one owner
for app data rollback lifecycle decisions, not a new universal storage control
plane.

### Snapshot names must be collision-safe

Image refresh must stop treating `NextGenNumber` as proof that the corresponding
LV name is available. Allocation rules:

1. load tuple state, creating an empty state only if missing;
2. build a reserved-name set from:
   - all tuple `DataSnapshot` names;
   - all tuple `FailedLVName` names;
   - all pending transaction snapshot/failed names;
   - all actual LVM names matching the app rollback patterns;
3. reserve generation numbers, not only snapshot names: generation `N` is
   unavailable if either `snap-app-<id>--gen<N>` or
   `vol-app-<id>--failed-gen<N>` is reserved;
4. choose the next `gen<N>` whose snapshot name and failed-data LV name are
   both free;
5. advance `NextGenNumber` past any skipped names before persisting.

If cleanup of a stale LV fails, allocation still proceeds with a later name.
Cleanup improves disk posture; it must not be required for forward progress
unless thin-pool safety itself is failing.

Rollback itself must also use a collision-safe failed-data LV name. The normal
case should use the reserved generation pair, but if an older tuple state did
not reserve a failed-LV name, rollback must allocate a non-conflicting failed LV
name before renaming the active data volume.

### Image refresh needs durable rollback operation state

The current image-refresh path stages rootfs, stops containers, creates the data
snapshot, and only then stores tuple metadata. That leaves two bad windows:

- create-before-record: a crash or metadata write failure can leave an
  untracked snapshot;
- runtime-touched-without-journal: a crash after candidate containers start can
  leave app data possibly mutated without enough state to decide whether to
  restore the snapshot.

Image refresh should gain a small durable operation record, shaped like the
manifest update transaction but scoped to image/rootfs refresh. The exact file
name can be local to implementation; the required phases are:

| Phase | Meaning | Recovery |
| --- | --- | --- |
| `snapshot_planned` | collision-safe snapshot name and pre-update rootfs map are persisted; no LV exists yet | if no runtime switch started and no LV exists, clear the plan |
| `snapshot_created` | data snapshot LV exists and is the rollback point for the pre-update tuple | if candidate data risk was not reached, either keep as normal rollback snapshot or clean it according to tuple policy; if candidate data risk was reached and commit intent was not, restore it |
| `runtime_switch_started` | old runtime has been quiesced/stopped | restart old runtime if candidate not created |
| `candidate_data_risk` | candidate containers may be created, started, or attached to existing persistent data | on failure or restart before durable commit intent, restore data snapshot before normal reconcile |
| `commit_intent` | durable forward-complete boundary; normally written after private candidate readiness, or before any candidate can become user-visible when private readiness is unavailable | recovery forward-completes commit; it must not restore the pre-update data snapshot under candidate metadata |
| `committed` | app metadata, active rootfs, and tuple active generation are durably stored | snapshot becomes normal tuple rollback state and operation record can be cleared |

This can be implemented as a dedicated image update transaction or as a
generalized app apply transaction record. The important invariant is that
rollback snapshot state crosses durable storage before runtime mutation.

The `candidate_data_risk` marker is intentionally conservative. It must be
persisted before any candidate container can mount existing persistent storage.
If a crash happens after this marker but before the candidate actually writes
data, recovery still follows the restore path. That may discard no-op candidate
state, but it avoids resuming old code against data that may have been migrated
by new code.

The `commit_intent` marker is the opposite boundary. It must contain or
reference the candidate manifest/rootfs/container state needed to finish writing
app metadata and tuple active generation. After this marker, recovery
forward-completes the update or keeps the operation pending for retry; it does
not restore the pre-update data snapshot.

Candidate readiness before `commit_intent` must be truly private. Candidate
containers must not receive user traffic, public listener traffic, remote relay
traffic, or direct user-facing host-bind traffic before the durable
forward-complete boundary. If the implementation cannot guarantee private
candidate execution for a given app shape, it must move `commit_intent` earlier:
before any candidate runtime can bind or receive user-facing traffic. Failures
after that point are treated as post-commit image update failures handled by the
normal tuple rollback/health path, not by transaction-private restore.

### Preflight before disruptive work

For persistent-data image refresh:

1. check snapshot support and thin-pool/origin viability;
2. reconcile or route around existing rollback-shaped LVs for the app;
3. allocate and persist the planned snapshot name;
4. only then pull/stage rootfs;
5. run a final rollback-storage viability check after all planned
   thin-pool-consuming staging and immediately before quiescing the app while
   holding AppManager's app-lifecycle serialization lock;
6. stop containers only after the planned rollback point is durable and the
   final viability check passes.

Snapshot creation still happens while the app is quiesced, preserving the tuple
consistency intent from the original snapshot RFC. The preflight is about
availability and durable planning, not taking a live data snapshot early.

If the final viability step fails, the old runtime is still running. The update
is rejected before interruption; staged rootfs is detached or retained for
normal rootfs cleanup according to existing rootfs policy. It is not converted
into a user-visible app failure.

This RFC does not introduce a general persistence-owned thin-pool reservation
primitive. In the current daemon, app lifecycle operations that create app data
snapshots, service rootfs snapshots, and manifest/config update snapshots are
serialized by AppManager's `reconcileMu`; the final viability check therefore
covers Piccolo's in-process app update work for this flow. A future storage-wide
reservation can further protect against out-of-band LVM consumers or new
non-AppManager thin-pool writers, but that is a broader storage primitive rather
than part of this rollback-artifact fix.

### Cutover and partial-commit recovery

The image refresh commit boundary must be explicit because app metadata and
tuple metadata are separate files today.

| Durable state observed on recovery | Required behavior |
| --- | --- |
| `snapshot_planned` with no LV and no runtime switch | clear the plan |
| `snapshot_planned` with planned LV present and no runtime switch | mark the operation as `snapshot_created` and continue recovery, or retain/quarantine and route future allocation around it; never allocate the same name again |
| `snapshot_created` or `candidate_data_risk`, no `commit_intent` | restore the data snapshot if candidate data risk was reached; restore/restart the previous runtime and previous app metadata; keep transaction until restore succeeds |
| `commit_intent`, app metadata still previous | forward-complete app metadata and tuple active generation from the operation record |
| `commit_intent`, app metadata candidate but tuple active generation missing or stale | forward-complete tuple active generation; keep snapshot as the normal pre-update rollback snapshot |
| `commit_intent`, tuple active generation present but operation not marked committed | mark committed, then run committed cleanup |
| `committed` with cleanup failure | keep retry marker; update remains applied |

This makes recovery choose one direction for every split-brain shape:

- before durable commit intent, old manifest/rootfs/data are restored together;
- after durable commit intent, candidate manifest/rootfs/data are completed
  together;
- no recovery path restores old data underneath candidate app metadata.

Rollback/data repair is state repair, not desired-state activation. It runs for
enabled and disabled apps. After data/rootfs/metadata recovery completes, normal
desired-state reconcile decides whether the runtime should be started or remain
stopped.

### Reconciliation policy

Startup and app reconcile must repair the states Piccolo itself can understand:

- planned snapshot with no LV and no runtime switch: clear the plan;
- planned/created snapshot with a pending operation whose candidate data risk
  was reached but commit intent was not: restore snapshot before normal app
  reconcile;
- created transaction-private manifest snapshot after committed cleanup failed:
  retry cleanup through the existing transaction cleanup path;
- tuple snapshot referenced by `generations.json`: keep until tuple GC expires
  it;
- unreferenced `snap-app-*--gen*`: retain/quarantine when image-refresh
  operation state is missing or unreadable, because it may be the only restore
  point for candidate-mutated data; cleanup requires a readable tuple/operation
  state, committed/aborted marker, app epoch, or uninstall evidence proving the
  snapshot is not needed; if cleanup fails, retain and route future allocations
  around it;
- a quarantined `snap-app-*--gen*` that may be transaction-critical is also an
  activation decision, not just a cleanup decision: the recovery barrier must
  restore from it when safe proof exists, fail closed with a human-level
  "cannot prove safe rollback state" error when proof is missing, or explicitly
  prove normal activation is safe before allowing `RestoreServices` or
  `ReconcileOnce` to publish/start the app;
- unreferenced `snap-app-*--manifest-*`: retain/quarantine when the matching
  transaction state is missing or unreadable, because it may be the only
  restore point for candidate-mutated data; cleanup only when a readable
  transaction, committed cleanup marker, or app epoch proves it is no longer
  needed;
- unknown failed LVs: retain unless tuple metadata proves CoW-safe deletion.

The reconciler should log diagnostic details, including LV name and
classification, but the app detail UI should not become an LVM repair console.
User-visible failures should be human-level:

- "Update blocked because Piccolo cannot create a safe rollback point."
- "Update restored the previous version, but cleanup will retry."
- "Update applied; access repair is pending." (existing access-repair surface)

### Generic orphan reconcile stops making app rollback policy by pattern

`ReconcileOrphanLVs` can still remove ordinary orphaned LVs with no metadata.
For rollback-shaped app artifacts it should either delegate to the app-aware
reconciler or leave them for that reconciler. It should not permanently skip all
`snap-*` and `--failed-gen` names while also being the only startup cleanup
path.

The app-aware reconciler may still choose retention for ambiguous artifacts.
That is different from silence: retained unknowns are diagnostics and excluded
from future allocation collisions.

### Startup run site

Rollback artifact discovery has two layers:

1. the existing low-level `ReconcileOrphanLVs` continues to run after pool
   activation, but it must leave rollback-shaped app artifacts to the app-aware
   policy instead of making final decisions by name;
2. the app manager owns a pre-runtime-activation recovery barrier. Every path
   that can restore, publish, start, or reconcile app runtimes must pass this
   barrier after the control plane is unlocked and app state is readable.

The barrier is not only a `gin_server.go` unlock hook. It must be enforced by
`AppManager` itself, either directly inside `RestoreServices` and
`ReconcileOnce` or via a single shared helper both call while holding the normal
runtime/reconcile lock. Current callers such as `ObserveRuntimeEvents`,
`StartBackground`, boot restore, and direct test/helper calls then inherit the
same recovery ordering instead of relying on call-site discipline.

The app-manager pass scans both installed apps and rollback-shaped LVs whose app
metadata is absent. Absent-app `snap-app-*--gen*` snapshots are retained and
routed around on first discovery unless stronger proof exists, such as a durable
uninstall tombstone, no active `vol-app-*`, and an age/quarantine threshold.
Absent-app `snap-app-*--manifest-*` snapshots are retained/quarantined without
readable transaction state. Absent-app failed LVs are retained unless the
app-aware policy can prove they are not CoW dependencies.

## Image refresh target flow

For service-mode image refresh with persistent storage:

1. Acquire the existing app update lock/reconcile lock.
2. Run rollback preflight and allocate a collision-safe planned tuple snapshot.
3. Pull images and stage changed rootfs while the current app continues running.
4. Run the final rollback-storage viability check after staging and before
   runtime interruption while holding the app lifecycle lock.
5. Quiesce/stop the current runtime.
6. Create the planned data snapshot and mark it created.
7. Before any candidate container can mount existing persistent storage, persist
   `candidate_data_risk`.
8. Remove old containers and create candidate containers without exposing them
   to user traffic.
9. Verify candidate readiness through the required private checks when private
   candidate execution is available.
10. Persist `commit_intent` with enough candidate state to forward-complete the
    commit after a crash. If private candidate execution is not available for
    this app shape, this step moves before candidate creation or user-facing
    bind/publication.
11. Publish/switch user-facing traffic only after `commit_intent` is durable.
12. Persist app metadata, active rootfs map, and tuple active generation.
13. Mark operation committed and clear it after committed cleanup.

If any step before `candidate_data_risk` fails, restart the old runtime and
clean or retain the unused planned snapshot according to policy.

If any step after `candidate_data_risk` but before `commit_intent` fails,
restore the data snapshot and old rootfs/runtime before normal reconcile can
run.

If any step after `commit_intent` fails, recovery forward-completes the update
or keeps the operation pending for retry. It does not restore the pre-update
data snapshot under candidate metadata.

For the early `commit_intent` fallback, forward completion must still create the
normal tuple snapshot and active generation. If the candidate active generation
never becomes healthy, the existing tuple-health auto-rollback path must be able
to roll back from that normal snapshot; the operation must not retry candidate
startup forever while a rollback target exists.

If cleanup fails after commit, keep a retry marker. Do not report a successful
update as failed solely because internal artifact cleanup is pending, but do
surface a warning in diagnostics.

## Site list

Expected implementation sites:

- `internal/app/app_manager.go`
  - image refresh preflight and transaction integration;
  - replace direct `snapshotTupleBeforeUpdate` allocation with planned
    collision-safe allocation;
  - ensure no persistent-data app is stopped before rollback planning succeeds.
- `internal/app/tuple.go`
  - add any required tuple status/fields for planned image-refresh snapshots, or
    route through a separate transaction type without overloading tuple status.
- `internal/app/filesystem.go`
  - durable load/store/clear helpers for image refresh transaction state, if a
    separate transaction is chosen.
- `internal/app/tuple_gc.go`
  - keep existing tuple GC semantics for referenced snapshots and failed LVs;
  - integrate app-aware cleanup candidates without deleting ambiguous failed LVs.
- `internal/app/tuple_health.go` and app reconcile startup paths
  - recover planned/created/candidate-data-risk image refresh operations before
    normal app reconcile;
  - keep user-visible rollback snapshots distinct from transaction-private
    restore points.
- `internal/app/app_manager.go:ObserveRuntimeEvents`,
  `internal/app/app_manager.go:StartBackground`,
  `internal/app/app_manager.go:RestoreServices`, and
  `internal/app/app_manager.go:ReconcileOnce`
  - enforce the app-manager-owned recovery barrier on every runtime activation
    path.
- `internal/server/gin_server.go`
  - keep low-level orphan cleanup before app reloaders, but do not own the sole
    app rollback recovery barrier.
- `internal/app/installed_app_apply_transaction.go`
  - reuse or share snapshot planning helpers where practical;
  - keep manifest/service-app snapshots transaction-private.
- `internal/app/custom_manifest_update.go`
  - ensure existing manifest snapshot recovery composes with the shared
    artifact classifier and diagnostics.
- `internal/persistence/luks_volume_manager.go`
  - expose low-level LV list/existence/destroy operations needed by the app
    reconciler;
  - expose observed rollback-shaped LV names so app-owned allocation can route
    around stale artifacts;
  - keep low-level orphan cleanup from deleting rollback-looking app LVs whose
    ownership cannot be proven locally.
- `internal/storage/lvm/{types.go,volume.go}`
  - extend LV listing if implementation needs origin, segtype, or creation time
    for safer diagnostics/classification.
- `internal/persistence/volume_diagnostics.go`
  - include retained rollback artifact classifications in diagnostics if the
    implementation exposes them beyond structured logs.
- Tests in `internal/app/*update*_test.go`,
  `internal/app/tuple*_test.go`, `internal/persistence/luks_volume_manager_test.go`,
  and `internal/storage/lvm/lvm_test.go`.

## Test plan

Backend tests must cover:

- stale `snap-app-<id>--gen1` exists while `generations.json` is absent:
  image refresh allocates `gen2` or later and does not fail on collision;
- stale snapshot cleanup failure does not block allocation when another name is
  available;
- metadata write failure after snapshot creation does not leave a future
  deterministic-name blocker;
- daemon restart with image refresh `snapshot_planned` and no LV clears the
  plan;
- daemon restart with image refresh `snapshot_planned`, planned LV present, and
  no runtime switch retains or marks the snapshot created without reallocating
  the same LV name;
- daemon restart with snapshot created and candidate data risk not reached
  preserves or cleans according to tuple policy without corrupting the app;
- daemon restart with candidate data risk before commit intent restores the data
  snapshot before normal reconcile;
- daemon restart with candidate data risk and missing/unreadable image operation
  metadata retains/quarantines the unreferenced `snap-app-*--gen*` snapshot
  rather than deleting the only possible restore point, and the recovery barrier
  blocks normal activation unless restore or safe activation can be proven;
- candidate runtime cannot receive user/public/remote traffic before
  `commit_intent` is durable, or the implementation takes the documented
  fallback of writing `commit_intent` before user-facing bind/publication;
- early `commit_intent` fallback with a candidate that never becomes healthy
  reaches normal tuple-health rollback instead of indefinite operation retry;
- daemon restart after durable commit intent but before tuple active generation
  forward-completes the update and does not restore old data under new metadata;
- crash/fault injection after each durable cutover write proves recovery chooses
  either full restore or forward completion, never mixed old data/new metadata;
- normal image refresh still produces a user-visible rollback snapshot and
  active generation;
- manifest/service-app transaction-private snapshots remain hidden from
  `snapshot_available` and user rollback;
- manifest/service-app snapshot with missing or unreadable transaction metadata
  is retained/quarantined rather than auto-deleted;
- unknown failed LVs are retained unless tuple metadata proves safe deletion;
- stale failed-LV names are included in generation allocation, so both snapshot
  and future failed-data LV names are collision-safe;
- generic orphan reconcile no longer codifies "all snap-app artifacts are
  skipped forever" as the only cleanup policy.

Diagnostics tests should also cover retained-artifact accumulation: cleanup
retry/backoff is observable in structured logs and, if implemented beyond logs
in v1, the existing volume diagnostics surface. This RFC does not require a new
user-facing storage health UI.

Validation should start with focused Go tests. Alpha validation through
`scripts/alpha/*` is required once the backend tests pass if the implementation
touches real update flow ordering, startup recovery, or LVM command shape.

## Review focus

Reviewers should challenge:

- whether the proposed operation phases are sufficient to decide restore vs
  cleanup after every crash window;
- whether any auto-delete rule can destroy a needed CoW dependency;
- whether transaction-private snapshots can accidentally become user-visible
  rollback snapshots;
- whether image refresh can still interrupt a running app before avoidable
  rollback-storage failure is known;
- whether the solution remains app data rollback lifecycle work rather than
  becoming a broad storage registry rewrite.
