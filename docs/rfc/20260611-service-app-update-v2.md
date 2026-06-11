# RFC: Service App Update v2

**Date:** 2026-06-11
**Status:** Draft

## Scope block

**Problem:** Installed service apps can hit reinstall-only walls when a new manifest changes image references, listener/auth shape, service shape, or other runtime wiring that v1 Apply Manifest YAML deliberately rejects.
**In scope:** A task-backed operator flow for installed service-mode apps that can apply a broader rendered manifest update, including changed image references, listener/auth/topology changes, service additions, service removals with explicit operator review, service environment changes, add-only storage, transaction-private failure-path data snapshot/restore before candidate runtime touches existing persistent storage, and immutable existing persistent storage declarations.
**Out of scope:** Existing persistent storage mutation or removal, service removals that detach or orphan persistent storage, workspace image rebasing, app data migration, hot-reload contracts, automatic background application of high-risk catalog changes, and user-initiated data rollback after a technically successful update.

## Background

`docs/rfc/20260530-custom-app-manifest-update.md` shipped a deliberately narrow
manual YAML update path. It is useful for topology-neutral wiring changes:
existing service environment changes and add-only storage against the same app
identity, service set, image references, listeners, and persistent storage
mounts.

Real operator updates are already hitting the v1 boundary:

- a service image reference changes, and the dry run says to run image update or
  reinstall;
- listener topology or auth changes, and the dry run says reinstall or a future
  v2 flow is required.

Those failures are not independent bugs. They are symptoms of the same missing
operation: a broader installed service app update that can compose manifest
source, image/rootfs state, listener routing/auth state, config ledger state,
and container recreation under one operator-reviewed transaction.

This RFC intentionally does not make existing persistent storage mutable. That
is a different product and data-safety problem: a storage mount change can make
old app data invisible, partially migrated, or semantically wrong even when the
bytes are preserved. Service update v2 must preserve every existing persistent
storage entry exactly. It may add new storage. It does not remove service
references to persistent storage in this track, because detaching an existing
mount from the runtime can make preserved bytes semantically invisible to the
app.

## Decision summary

Extend the installed service app update model from "manifest-only v1" and
"image-only Update Image" into one operator-reviewed **service app update**
transaction.

The operator supplies a candidate manifest source. Piccolo renders it with the
installed app identity and declared input/config context, computes a redacted
diff plan, stages any required image/rootfs changes, applies listener/routing
changes and container recreation in a controlled order, and commits the manifest
plus config ledger only after runtime materialization succeeds.

The current **Update Image** operation remains useful for the common exact-tag
republish path. It stays as a shortcut for "refresh rootfs behind the same
manifest image refs." The broader v2 flow is for cases where the manifest source
itself changes and therefore the image refs, listeners, auth, or services need
to change with it.

The current **Apply Manifest YAML** action becomes the manual entry point for
custom service apps, but the user-facing operation should be labeled **Review
App Update** or **Apply App Update from YAML**. "Manifest YAML" is the input
format, not the perceived operation, because the flow can change images,
listeners, auth, services, and runtime state.

Catalog sync may reuse the same classifier and apply machinery, but high-risk
catalog changes are not silently applied in the background. Catalog sync should
record a pending update that the operator reviews through the same dry-run and
apply surface when the candidate changes image refs, listeners/auth, service
shape, or any other high-risk runtime field.

## Goals

- Let an installed service app accept normal upstream manifest evolution without
  uninstall/reinstall when Piccolo can preserve identity and data safely.
- Keep the operator's app address, primary identity, and persistent storage
  entries stable unless the operator chooses a separate reinstall or future
  storage migration flow.
- Compose image/rootfs changes with manifest and runtime changes instead of
  forcing operators to manually sequence exact-tag image updates and YAML
  updates.
- Make listener/auth exposure changes explicit in dry run before apply.
- Preserve truthful rollback guarantees: either the new runtime reaches the
  commit point, or Piccolo restores the previous manifest/runtime/routing state
  and any pre-commit data snapshot taken before candidate startup.

## Non-goals

- Do not rename, remove, remap, or migrate existing persistent storage entries.
- Do not support workspace-mode base image rebasing.
- Do not invent app-specific data migration hooks.
- Do not promise rollback of app-level data mutations after a successful update.
- Do not make catalog sync automatically apply high-risk updates without
  operator review.
- Do not redesign app detail operation tracking beyond what the new operation
  needs to report progress and readiness.

## Product behavior

### Operator flow

1. The operator opens an installed service app and chooses **Review App Update**
   for a custom app, with manifest YAML as the source input, or opens a pending
   catalog update review for a catalog app.
2. Piccolo renders the candidate manifest with the app's existing identity,
   stored config ledger, and any operator-entered or regenerated values.
3. Piccolo returns a redacted dry-run plan grouped by change class:
   - image/rootfs changes;
   - listener, routing, and auth changes;
   - service additions/removals;
   - environment and app config changes;
   - storage additions;
   - storage rejects;
   - preserved state;
   - expected interruption.
4. The operator reviews exposure-sensitive changes before apply. Public listener
   additions, auth weakening, middleware removal, new remote ports, protocol
   changes, and primary listener changes require a hard confirmation gate before
   Apply is enabled.
5. If applicable, the operator confirms apply using an opaque dry-run token for
   the exact rendered candidate and staged update plan.
6. Piccolo applies the update as a task-backed operation. App detail remains the
   authoritative operation surface and observes readiness after completion.

### Supported update classes

The v2 flow supports these classes when all safety checks pass:

- image reference changes for existing service names;
- service additions that use new rootfs materialization and either no persistent
  storage or only newly added persistent storage;
- service removals only after explicit operator review, only when the removed
  service has no persistent storage references, and with removed rootfs/container
  state retained long enough for rollback or explicit cleanup;
- listener topology changes, including listener additions, removals, port/claim
  changes, protocol/flow changes, primary-listener marker changes, remote/public
  exposure changes, routing changes, and auth/middleware changes;
- service environment changes, with extra operator review when the app already
  has existing persistent storage;
- rendered `app_config` changes that can be materialized by the existing runtime
  config mechanism, with extra operator review when the app already has existing
  persistent storage;
- add-only persistent or temporary storage declarations.

The v2 flow still rejects these classes:

- changes to an existing persistent storage entry's name, type, mount path, or
  backing semantics;
- adding a new service attachment to an existing persistent storage entry;
- removing a service that references existing persistent storage;
- deletion of persistent storage bytes;
- storage migration or copy semantics;
- service-level one-shot init or init-script drift that would require replaying
  install-time behavior;
- workspace-mode updates;
- app identity changes that cannot be explained by the existing app instance ID
  and the candidate listener model.

### Storage boundary

Storage remains the hard boundary.

For every existing persistent storage declaration, v2 requires exact semantic
stability: same logical name, same mount path, same volume type, same backing
relationship, and same service attachment relationship. Additions are allowed.
Persistent storage removals from the manifest are rejected.

The first implementation must reject persistent storage removals rather than
creating orphan semantics implicitly.

Temporary storage can be more permissive, but the dry run must still name
changed temporary mount paths because they can affect app behavior.

### Service and storage matrix

The first implementation uses this storage matrix:

| Candidate change | Persistent storage outcome |
| --- | --- |
| Existing service keeps existing storage reference unchanged | supported |
| Existing service adds newly declared storage | supported |
| Existing service adds reference to pre-existing storage it did not already mount | rejected |
| New service mounts no persistent storage | operator_review |
| New service mounts newly declared persistent storage | operator_review |
| New service mounts pre-existing persistent storage | rejected |
| Removed service has no persistent storage reference | operator_review |
| Removed service has any persistent storage reference | rejected |
| Existing storage declaration name/type/mount/backing changes | rejected |
| Existing storage declaration is removed | rejected |

Temporary storage changes are `operator_review` when they affect existing
services because they can change app behavior, but they do not carry the same
data-retention risk as persistent storage.

### Persistent data touch boundary

Any v2 update that starts candidate containers with existing persistent storage
mounted must take a data snapshot before candidate startup. If the update fails
before the commit point, recovery restores that snapshot along with the previous
manifest, ledger, rootfs map, container state, and listener/routing/auth state.

That snapshot is transaction-private. It is recorded as
`precommit_data_snapshot_id` on the service app update transaction, hidden from
normal `snapshot_available` or app rollback UI, and never exposed through
`RollbackToSnapshot`. A successful commit marks it for cleanup or bounded
internal retention only for crash recovery diagnostics. After commit, rollback
endpoints must fail closed rather than rolling back data/rootfs without the
matching manifest, listener, and ledger history.

If Piccolo cannot take the required snapshot, the candidate is rejected before
runtime switch. The operator may still use reinstall or a future storage/data
migration flow, but v2 must not silently downgrade to manifest/rootfs-only
rollback after candidate containers have had a chance to mutate existing
persistent data.

Snapshot presence is not enough. Before candidate startup, Piccolo must perform
a snapshot viability gate: enough backing-store headroom for rootfs staging plus
expected copy-on-write pressure, a healthy snapshot state, and a fail-closed
rule when rollback storage cannot be kept viable. During private candidate
verification, Piccolo monitors snapshot health; a failing snapshot aborts before
public traffic is exposed. If restoration itself fails, the app enters
`rollback failed, operator action needed` and the transaction is retained for
operator recovery.

Apps with no existing persistent storage mounted by candidate containers do not
require a data snapshot for this guarantee.

## Backend design

### Render source and identity

The candidate render uses the same install pipeline and installed config ledger
rules as the current custom manifest/config update flows:

- raw candidate source is retained in the ledger only after commit;
- `__app_address__` remains pinned to the installed instance ID;
- declared inputs and generated values are resolved through the same
  secret-safe dry-run token model.

Identity preservation is stricter than "same app name." The update must preserve
the installed app instance ID and produce an explicit primary listener outcome:
unchanged, renamed by supported marker semantics, or rejected. A primary browser
surface may move ports/protocol/auth only when the dry-run plan names the change
and the routing transaction can preserve a rollback path.

### Diff classification

Replace the v1 allow/reject policy with a richer service update classifier.

The classifier produces a normalized update plan with a complete outcome for
every flag. `supported` means the transaction can apply it without special
confirmation. `operator_review` means the transaction can apply it only after
the dry-run confirmation names the risk. `rejected` means the candidate requires
reinstall or a future flow.

| Diff flag | Outcome |
| --- | --- |
| image refs changed for existing services | operator_review |
| image refs changed for digest-pinned refs | operator_review, with immutable digest before/after |
| services added with no persistent storage | operator_review |
| services added with newly declared persistent storage | operator_review |
| services added with pre-existing persistent storage | rejected |
| services removed with no persistent storage | operator_review |
| services removed with persistent storage | rejected |
| existing persistent storage mutated | rejected |
| existing persistent storage removed | rejected |
| existing service attaches pre-existing persistent storage it did not already mount | rejected |
| persistent storage added | supported |
| temporary storage changed | operator_review |
| listener topology changed | operator_review |
| listener exposure changed | operator_review with exposure confirmation gate |
| listener auth or middleware policy changed without credential lifecycle changes | operator_review with exposure confirmation gate |
| primary listener identity changed | operator_review if instance ID remains stable, otherwise rejected |
| service environment changed when the app has no existing persistent storage | supported |
| service environment changed when the app has any existing persistent storage | operator_review with data-impact confirmation |
| rendered app config changed when the app has no existing persistent storage | supported |
| rendered app config changed when the app has any existing persistent storage | operator_review with data-impact confirmation |
| init or one-shot install behavior changed | rejected |
| init-script content/config changed | rejected |
| service-level OIDC client add/remove/update or credential regeneration | rejected |
| proxy OIDC client create/delete/update or credential regeneration | rejected |
| proxy OIDC authorize path routing changed without credential material changes | supported with routing/auth fingerprint |
| resources changed | rejected |
| permissions changed | rejected |
| healthcheck changed | rejected |
| lifecycle changed | rejected |
| `x-piccolo` extension changed | rejected unless explicitly listed by a future RFC |
| top-level auth changed | rejected unless represented as listener auth/middleware change |
| app type or workspace identity changed | rejected |

The dry-run result is redacted. It names services, listeners, ports, storage
keys, and change classes, but never secret values, generated credentials, raw
environment values, or rendered YAML.

For image changes, dry run resolves every changed image reference to an
immutable digest and computes the expected rootfs volume identity. The dry-run
token binds the source image ref, resolved digest, expected rootfs identity, and
service name. Apply rejects if the registry digest or staged rootfs plan no
longer matches the reviewed plan.

For environment and rendered app config changes in any app that already has
persistent storage, the dry-run plan shows a redacted data-impact item. It names
the service and key path, not the value, and requires operator review because
config can encode durable data semantics such as storage backend, migration
mode, data directory, provider endpoint, database URL, or migration flag. The
first implementation defaults to review because Piccolo cannot prove that a
stateless service cannot drive writes to another data-bearing service.

### Transaction shape

The transaction must cover the states that currently live in separate flows:

- previous and candidate rendered manifest;
- previous and candidate config ledger;
- previous and candidate active rootfs map;
- previous and candidate container IDs;
- previous and candidate listener endpoint/routing/auth state;
- proxy OIDC routing deltas that do not create, delete, or regenerate
  credentials;
- staged rootfs volumes for changed or added services;
- preserved removed-service rootfs/container metadata when service removal is
  supported without touching persistent storage attachments;
- transaction-private `precommit_data_snapshot_id` when candidate containers
  mount existing persistent storage.

The service app update transaction can evolve from the existing manifest update
transaction file, but its serialized shape must make image/rootfs and
listener/routing rollback explicit.

The commit point remains conservative for data and runtime state: do not commit
the candidate ledger and manifest as authoritative until rootfs staging,
container recreation, private runtime verification, and listener routing/auth
preparation have succeeded.

Public and remote access readiness is a separate post-commit health gate. Relay
connection, alias inventory, certificate availability, public port claims, and
other exposure prerequisites that do not depend on the active candidate runtime
must be prepared or preflighted before commit. Publishing those prepared routes
to user traffic happens after commit. A post-commit access failure is repaired
forward or reported as access repair work; it does not make the data/runtime
commit unsafe and it does not expose the pre-commit data snapshot as rollback.

After the commit point, cleanup and public route activation are best-effort but
observable. A reboot after commit but before cleanup must recover as committed
when the manifest, ledger, rootfs, and prepared listener/routing fingerprints
match the candidate transaction record. If the active published routing
fingerprint is missing or unhealthy, boot recovery forward-completes activation
from the prepared intent or marks the app `update applied, access needs repair`.

The transaction owns a service-update fingerprint written before runtime switch
and read by boot recovery before normal reconcile:

| Field | Writer | Reader | Partial-match fail direction |
| --- | --- | --- | --- |
| previous/candidate manifest hash | dry run and transaction begin | apply and boot recovery | restore previous before commit; error after commit |
| previous/candidate ledger revision and source hash | transaction begin and ledger commit | apply and boot recovery | restore previous before commit; error after commit |
| previous/candidate active rootfs map | rootfs staging | apply and boot recovery | detach candidate before commit; error after commit |
| resolved image digest per changed service | dry run | apply and boot recovery | reject apply on drift; error if post-commit mismatch |
| previous/candidate listener endpoint fingerprint | listener prepare | apply and boot recovery | restore previous before commit; repair access after commit |
| previous/candidate prepared routing/auth fingerprint | listener prepare | apply and boot recovery | restore previous before commit; forward-complete activation after commit |
| active published routing/auth fingerprint | route activation | boot recovery and health observation | not present before commit; repair access after commit |
| proxy OIDC routing delta without credential lifecycle changes | routing prepare | rollback and boot recovery | rollback before commit; error after commit |
| transaction-private precommit data snapshot ID when required | pre-switch snapshot | rollback and boot recovery | reject switch if missing or unhealthy; restore before commit; hide from user rollback after commit |

### Runtime apply order

The apply order should minimize externally visible split-brain:

1. stage image pulls and rootfs volumes for changed or added services while the
   current app continues running;
2. resolve and bind immutable image digests for the reviewed plan;
3. prepare listener endpoint, local proxy, TLS mux, remote relay, alias/cert,
   public port claims, and proxy OIDC routing changes without publishing a
   partial route or creating new credential material;
4. take the required data snapshot when candidate containers will mount existing
   persistent storage, and pass the snapshot viability gate;
5. stop and remove old containers when the runtime change is ready to switch;
6. create containers from the candidate manifest and staged rootfs set;
7. verify candidate readiness through private or synthetic probes only; external
   user traffic must not reach candidate state that may still be restored from
   the pre-commit snapshot;
8. commit manifest, ledger, active rootfs, container state, and prepared routing
   intent; this crosses the data/runtime rollback boundary and the pre-commit
   data snapshot is no longer a user-visible rollback option;
9. activate prepared public listener routing/auth changes to point at the
   candidate runtime and run post-commit listener verification.

If a failure occurs before commit, restore the previous manifest and ledger,
recreate containers from the previous active rootfs set, restore previous
listener routing/auth state, restore the pre-switch data snapshot when one was
required, remove or detach staged candidate rootfs state, and keep enough
transaction data for boot recovery if restoration fails.

Alias, certificate, and remote relay prerequisites needed for a required public
or remote exposure change are pre-commit gates. If they cannot be prepared, the
update fails before candidate startup or rolls back before public traffic is
exposed. After commit, propagation or health failures are handled as forward
repair, `update applied, access needs repair`, or `ready with warnings`; they
do not restore the transaction-private data snapshot.

After public traffic has been exposed to the candidate runtime, failure recovery
must not restore the pre-commit data snapshot. Post-commit listener or remote
health failures are access failures, not runtime rollback failures. They are
repaired forward when possible, otherwise app detail reports `update applied,
access needs repair` for required access surfaces or `ready with warnings` for
optional degraded surfaces.

### Rollback and recovery

Failure-path rollback is part of v2. User-initiated rollback after a successful
update remains limited to existing image/rootfs tuple rollback unless a separate
manifest-history and data-snapshot model is designed.

Failure-path data snapshots are not normal app snapshots. Boot recovery may use
`precommit_data_snapshot_id` only while resolving a pre-commit service app update
transaction. Successful commit hides the snapshot from user rollback, schedules
internal cleanup or bounded diagnostic retention, and records that rollback
endpoints must reject rather than exposing a partial data/rootfs rollback.

Boot recovery must run before normal app reconcile for any pending service app
update transaction. Normal reconcile must not promote a candidate manifest,
candidate listener state, or candidate rootfs map merely because it exists on
disk.

Recovery outcomes:

- committed and consistent: clean up transaction;
- committed with prepared route not yet active: forward-complete activation or
  mark `update applied, access needs repair`;
- pre-commit with previous state restorable: restore previous state and clean up;
- pre-commit with restore failure: mark `rollback failed, operator action
  needed` and retain transaction;
- post-commit access unhealthy: mark `update applied, access needs repair` and
  retain enough access-state detail for operator repair.

The operation uses a new app operation type and task type:

- task type: `update_service_app`;
- expected timeout class: app-install/update scale, initially 45 minutes because
  the operation may pull images and materialize rootfs volumes;
- app detail readiness: completion waits for runtime commit, then app detail
  enters the same readiness observation model as image update, with extra states
  for listener/auth verification and rollback.

## API shape

Prefer extending the existing custom manifest update endpoints instead of
creating a parallel API family:

- `POST /api/v1/apps/:name/manifest/configure`
- `POST /api/v1/apps/:name/manifest/dry-run`
- `POST /api/v1/apps/:name/manifest/update`

The dry-run response should gain:

- update class;
- supported/review/rejected decision per diff class;
- exposure review items;
- staged image/rootfs summary;
- listener/routing/auth summary;
- storage boundary summary;
- operation risk flags;
- runtime readiness expectations;
- required explicit confirmations;
- data safety summary, including whether a transaction-private snapshot is
  required, whether snapshot capacity/headroom checks passed, and the limit that
  successful updates do not create user-initiated data rollback.

The apply request should continue to use an opaque dry-run token, base manifest
hash, runtime fingerprint, ledger revision/source hash, and a candidate digest.
If the staged update plan can expire, apply must reject with a conflict and ask
the operator to rerun dry run.

Catalog pending update review uses the same dry-run/apply schema with a catalog
source reference instead of pasted raw YAML. Automatic catalog sync must not
call the apply path for high-risk update classes.

## UI behavior

App detail remains the authoritative surface for the operation.

The user-facing action is **Review App Update**. The wizard may include a
Manifest YAML source step for custom apps, but the operation copy should treat
the flow as an app update, not a YAML submit. It must clearly distinguish:

- changes Piccolo will apply;
- changes requiring explicit exposure review;
- preserved state;
- rejected changes;
- expected interruption and readiness checks.

When listener exposure or auth changes are present, the confirmation step must
show a dedicated review section before the final apply action. Apply remains
disabled until the operator explicitly confirms every exposure-sensitive item.
Each item must show old -> new listener name, URL/port/remote exposure, protocol
and flow, auth strategy, middleware added/removed, and primary-listener status.

The dry-run review must also include a **Data safety** group whenever existing
persistent storage will be mounted by candidate containers. It says whether a
transaction-private snapshot is required, whether capacity/headroom preflight
passed, what happens if the snapshot cannot be kept viable, and that Piccolo
does not offer user-initiated data rollback after a successful update.

When image refs change, the summary must explain that Piccolo will pull or
materialize new rootfs state and that this is different from the exact-tag
**Update Image** shortcut.

When service additions/removals are present, the summary must name the services
and explain what happens to rootfs/container state. If a removed service has any
persistent storage reference, the update is rejected with guidance to keep the
service, remove the storage relationship through a future storage migration
flow, or reinstall.

Service removals without persistent storage still require explicit operator
review because Piccolo cannot prove semantic safety for background workers or
sidecars. The confirmation text must say that removed services will no longer
run and that app-specific behavior may change even if the remaining listener is
healthy.

When storage mutation is rejected, the UI should keep the current clear v1
message but add the next action: preserve the old storage entry exactly, add a
new storage entry, or reinstall/use a future storage migration flow.

When environment or app config changes occur in an app that already has
persistent storage, the UI shows a redacted data-impact confirmation naming the
service and key path. The value remains hidden, but the operator must confirm
that the config change can affect durable app behavior, including through
network writes to another service's persistent data.

Every rejected dry-run item must name the offending service/listener/storage or
config path, the reason it is rejected, and the next valid move: edit YAML and
rerun, keep the old shape, use reinstall, or wait for a future migration flow.
The wizard preserves pasted YAML and entered values after rejection, conflict,
or token expiry unless the operator explicitly cancels.

App detail operation labels/states:

- staging images/rootfs;
- preparing listener/auth changes;
- snapshotting data;
- switching runtime;
- verifying runtime privately;
- publishing access;
- verifying access;
- rolling back;
- rolled back to previous version;
- rollback failed, operator action needed;
- update applied, access needs repair;
- ready;
- ready with warnings.

The terminal copy must name the data/runtime boundary. `Rolled back to previous
version` means the commit point was not crossed and previous data/runtime state
was restored. `Rollback failed, operator action needed` means pre-commit
restoration did not complete. `Update applied, access needs repair` means the
manifest, ledger, runtime, and data boundary were committed; Piccolo must repair
public/remote/local access forward and must not offer the transaction-private
data snapshot as rollback.

Mutating actions remain paused from submission through readiness handoff.

## Catalog behavior

Catalog sync should continue to apply low-risk metadata and safe structural
updates automatically only when the existing policy permits it.

When the catalog candidate requires v2 review, sync records a pending
high-risk-update state instead of repeatedly reporting a generic sync failure.
The state includes:

- catalog source ref and candidate source hash;
- current installed manifest/source hash;
- high-risk reason classes;
- missing or changed declared input schema hash when relevant;
- first-seen and last-checked timestamps;
- last render/classification error, if any;
- conflict/expiry behavior when a newer catalog source supersedes it.

App detail must show "Catalog update requires review" as a persistent state with
the candidate source/ref, high-risk change classes, last sync time, and actions:
Review update, Defer, and View rendered plan.

Defer never clears the pending update and never authorizes apply. It marks the
current candidate source hash as operator-deferred, records the deferred
timestamp, lowers the app-detail urgency for that exact candidate, and keeps a
visible Review update action. The state shows the re-escalation rule, for
example "deferred until a newer catalog update appears or 7 days pass." It
re-escalates when a newer catalog source hash appears or when the product's
defer TTL expires.

Operator-edited config values remain authoritative. Catalog updates must render
against the installed config ledger and must not replace operator values with
catalog defaults unless the field provenance permits that behavior.

## Validation plan

- Unit tests for diff classification across image refs, listener topology,
  auth/middleware, service additions/removals, storage add-only, storage
  mutation rejects, persistent-storage attachment removal rejects, app config,
  OIDC, and init-script drift.
- App manager tests for changed image refs on existing services, listener/auth
  update rollback, service addition with staged rootfs, service removal without
  persistent storage, service removal with persistent storage rejected, and
  failed apply recovery before and after the commit point.
- Data snapshot tests for candidate startup against existing persistent storage,
  including capacity/headroom preflight, snapshot health monitoring, failure
  before commit, hidden post-commit snapshot behavior, and reboot recovery.
- Digest drift tests proving apply rejects when a mutable tag changes after dry
  run.
- Listener prepare/publish rollback tests across local proxy, TLS mux, remote
  relay state, alias/cert prerequisites, and proxy OIDC routing deltas.
- Boot recovery tests for committed-but-not-yet-published access state, proving
  recovery forward-completes access activation or marks `update applied, access
  needs repair` without data restore.
- Public traffic boundary tests proving candidate user traffic is not exposed
  before the data-rollback boundary is crossed.
- OIDC classifier tests proving service-level and proxy credential lifecycle
  changes are rejected in the first implementation.
- Catalog sync tests for pending high-risk review instead of automatic apply.
- API/OpenAPI validation for extended dry-run/apply schemas.
- Flutter tests for grouped dry-run summaries, exposure review confirmation,
  storage rejection guidance, active-operation adoption, and readiness handoff.
- Runtime validation on a real service app that exercises image ref change,
  listener/auth change, and additive storage without existing storage mutation.

## Site list

- `docs/app-platform/README.md`: operator-facing update guidance.
- `docs/api/openapi.yaml`: extended dry-run/apply schemas and responses.
- `internal/app/custom_manifest_update.go`: render, dry run, token, policy, and
  apply entry point.
- `internal/app/installed_config_update.go`: ledger render/apply composition.
- `internal/app/installed_app_apply_transaction.go`: transaction record,
  rollback, ledger commit, and boot recovery shape.
- `internal/app/catalog_sync_apply.go`: diff classifier and catalog sync pending
  review behavior.
- `internal/app/app_manager.go`: `UpdateImage`, rootfs staging, container
  recreation, rollback snapshot integration, and app operation locking.
- `internal/app/container_group_install.go`: creating candidate containers with
  staged rootfs for added/changed services.
- `internal/app/container_group_reconcile.go`: reconcile behavior around pending
  transactions and service shape changes.
- `internal/app/rootfs_integration.go`: rootfs creation, attach/detach, and
  image config handling.
- `internal/app/tuple.go`, `internal/app/tuple_gc.go`, and
  `internal/app/tuple_health.go`: ensure service-update failure snapshots are
  transaction-private and hidden from user rollback surfaces after commit.
- `internal/app/multi_container.go`: service container specs, app config
  materialization, and runtime creation.
- `internal/app/install_pipeline.go`: candidate render and identity/system
  context handling.
- `internal/app/install_state.go`: ledger source, revision, and secret-bearing
  state.
- `internal/app/task_progress_constants.go`: new or reused task phases for the
  broader operation.
- `internal/persistence/luks_volume_manager.go`: data snapshot/restore behavior
  used before candidate runtime touches existing persistent storage.
- `internal/persistence/rootfs_volume_manager.go`: staged rootfs identity and
  cleanup behavior.
- `internal/server/gin_app_handlers.go`: configure/dry-run/apply handlers,
  update image handler, update listeners handler, and error mapping.
- `internal/server/gin_server.go`: route registration, task ID propagation,
  remote runtime application, alias resolver updates, remote cert queueing, and
  endpoint/cert observers.
- `internal/events/task_progress.go`: active/recent task semantics.
- `internal/services/manager.go`: listener endpoint allocation, route updates,
  and listener health refresh.
- `internal/services/proxy.go`: HTTP listener routing, auth, and OIDC authorize
  path updates.
- `internal/services/tlsmux.go`: TCP/TLS listener routing when topology changes
  affect muxed listeners.
- `internal/remote/manager.go`: relay connection state, port claims, alias
  inventory, certificate inventory, and certificate issuance status.
- `internal/remote/certstore.go`: certificate lookup behavior for remote
  hostnames during activation and health checks.
- `internal/remote/nexusclient/types.go` and
  `internal/remote/nexusclient/adapter.go`: relay alias and route publication
  payloads.
- `internal/services/middleware/registry.go`: middleware validation and
  operator-listed auth/middleware diffs.
- `ui/lib/core/models/app_models.dart`: extended manifest update result models.
- `ui/lib/core/services/app_service.dart`: API calls and task ID headers.
- `ui/lib/features/apps/manifest_update_wizard.dart`: grouped dry-run review,
  exposure confirmation, and apply progress.
- `ui/lib/features/apps/app_detail_view.dart`: action availability,
  active-operation display, readiness handoff, and pending catalog review entry.
- `ui/lib/features/apps/app_operation_lifecycle.dart`: operation policy for the
  broader service update.

## Deferred

- Existing persistent storage mutation, migration, and deletion.
- Service removals that detach or orphan persistent storage.
- Workspace image rebasing.
- User-initiated rollback of successful manifest updates with app-data
  snapshots.
- App-specific hot-reload hooks.
- Fully automatic catalog application of high-risk updates.
