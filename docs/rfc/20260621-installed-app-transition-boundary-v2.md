# RFC: Installed App Transition Boundary v2

**Problem:** Installed app mutation flows can change source, rootfs, data, containers, listeners, access, and UI state through separate vertical implementations, so correctness depends on each flow independently rediscovering the same transition rules.
**In scope:** Define and implement one service-mode installed-app transition boundary for Modify App, Update Image, config-driven runtime updates, catalog-review updates, rollback/recovery, uninstall/cleanup composition, and the operator-facing update UI projection.
**Out of scope:** Workspace app mutation semantics, general storage schema migration/removal support, cleanup of stale LVs created before this transition system unless they are referenced by transition-owned metadata, user-initiated rollback product design after a successful update, install-path rewrite beyond shared planning helpers, and daemon/self-update behavior.

## Status

RFC review converged; ready for implementation.

The previous RFC `20260621-installed-app-transition-boundary.md` is the
first-slice record: it fixed Modify App image/rootfs freshness and named the
broader boundary. This RFC is the migration plan for the remaining roadmap.

## Background

The first slice fixed the immediate stale-image bug by teaching Modify App that
`image ref unchanged` is not the same as `runtime image unchanged`. That was
necessary, but it left the larger architectural issue in place:

- Modify App and Edit Config use `ManifestUpdateTransaction` through
  `installedAppApplyTransaction`.
- Update Image uses `ImageUpdateTransaction` and a separate rootfs/snapshot/
  tuple executor.
- Catalog sync decides between auto-apply, config pending, and manifest-review
  pending before any shared transition contract sees the source.
- Recovery has separate manifest-update and image-update dispatchers.
- UI settlement and review copy are assembled from operation-specific response
  fields rather than a stable transition plan.

The review feedback during the first slice repeatedly landed on transaction
edges: data snapshot timing, listener reservation lifetime, catalog pending
flow routing, rootfs cleanup, completed-operation settlement, and recovery
state. Those are not unrelated bugs; they are symptoms of the same missing
boundary.

## Design Goal

All service-mode installed-app mutations that can affect runtime state must be
represented as:

`Plan -> Prepare -> Commit Intent -> Switch -> Recover/Cleanup`

Feature entry points choose an operation policy and source, but they do not own
container/rootfs/listener/data transition semantics.

## Non-Goals

- Do not add storage volume mutation/removal support. Existing add-only storage
  behavior remains policy-gated; unsupported storage mutations keep failing
  closed.
- Do not support workspace image/base mutation through this boundary.
- Do not turn catalog sync into an automatic high-risk updater. High-risk
  catalog changes remain operator-reviewed.
- Do not rewrite install. Install may share image/rootfs helpers, but it is not
  a transition from an existing runtime.
- Do not solve historical LVM orphan discovery as part of this RFC. Transition
  cleanup must be correct for artifacts it records.

## Core Decisions

### D1. Introduce A Single Transition Plan

Add a service-mode installed-app transition plan with these domains:

- `Operation`: operation kind, source kind, risk class, UI intent, and whether
  the operation is automatic or operator-reviewed.
- `Source`: current manifest hash, candidate manifest hash, ledger revision,
  source hash, pending catalog flow, and input schema identity.
- `ImageRootfs`: per runtime entry decisions for app services and synthetic
  entries such as `__netns__`, plus normalized image identity requirements.
- `Data`: whether persistent data can be exposed to candidate runtime, whether
  snapshot viability is required, deterministic snapshot/failed-LV naming
  policy, and rollback behavior.
- `Runtime`: recreate policy, previous active rootfs fingerprint, candidate
  active-rootfs plan, primary service, candidate runtime naming policy,
  readiness requirements, and disabled app behavior.
- `Access`: listener prepare policy, reservation keys, publication strategy,
  proxy OIDC delta policy, and access-repair behavior.
- `Ledger`: whether to write install state, catalog metadata, or only runtime
  metadata.
- `Cleanup`: cleanup policy for staged rootfs, superseded rootfs, removed
  rootfs, data snapshots, failed data LVs, generated OIDC clients, and
  retained listener reservations.
- `Review`: stable confirmation IDs and operator-facing summaries projected
  from the plan.

The immutable `TransitionPlan` is produced during dry run or preflight and is
bound to apply by a plan hash, base manifest hash, ledger fingerprint, runtime
fingerprint, and image/rootfs resolved identities. Mutable facts discovered or
created during prepare/switch are recorded in a `TransitionRecord`, not added
back into the plan hash.

The plan hash is canonical executor state, not UI display state:

- include operation kind, source kind, pending catalog flow, base/candidate
  manifest hashes, ledger revision/source hash, runtime fingerprint,
  image/rootfs resolved identities or resolution requirements, data policy,
  access policy, cleanup policy, reserved resource keys, and required
  confirmation IDs;
- encode with stable map-key ordering and list ordering defined by the plan
  schema, not by Go/Dart map iteration;
- normalize image identities with canonical digest keys before hashing;
- normalize confirmation IDs as a sorted unique set;
- distinguish absent fields from intentional empty lists/maps only where the
  schema declares that distinction; otherwise canonicalize to one empty form;
- exclude timestamps, progress labels, localized/display text, raw secret
  values, and redacted UI-only summaries;
- include a plan schema version so future fields can fail closed instead of
  comparing unlike payloads;
- reject unknown required fields or unknown schema versions instead of dropping
  them during hash verification;
- reject apply when the submitted plan hash, base hashes, runtime fingerprint,
  or resolved image/rootfs identities differ from the dry-run/preflight plan.

The boundary between immutable plan and mutable record is explicit:

| Domain | Hash-bound `TransitionPlan` | Mutable `TransitionRecord` |
| --- | --- | --- |
| source/ledger | current/candidate hashes, source hash, pending flow, input schema identity | commit proof observations, metadata retry state, last errors |
| image/rootfs | image identity requirements, digest decisions, planned active-rootfs roles, deterministic rootfs keys | actual staged rootfs volume IDs, created golden/rootfs metadata, activation result |
| data | snapshot requirement, viability result, deterministic snapshot/failed-LV names | actual snapshot IDs, health result, rollback-created failed LV IDs |
| runtime | recreate/readiness policy, candidate runtime naming policy | created container IDs, candidate readiness result, `candidate_touched` predicate |
| access | listener reservation keys, publication/OIDC delta policy | prepared endpoints, retained reservations, generated OIDC client IDs/secrets handles, publication errors |
| cleanup | cleanup policy and reserved artifact keys | concrete cleanup inventory and per-artifact retry/complete state |

Apply verifies the immutable plan. Prepare appends resource inventory to the
transition record under that plan hash. A later record append never changes the
plan hash; it only gives recovery stronger facts.

### D2. Operation Policy Is Authoritative

Callers provide intent, not lifecycle rules. The transition planner derives the
legal policy from operation kind and current/candidate state:

| Operation | Source policy | Image/rootfs policy | Data policy | Access policy |
| --- | --- | --- | --- | --- |
| Modify App | new custom raw template or manifest-review catalog source | stage new/changed/same-ref-refreshed entries; preserve only with metadata proof | snapshot if candidate runtime can touch persistent data after source/rootfs change | prepare before switch, publish after readiness |
| Update Image | current committed definition | refresh mutable image refs by digest; skip digest-pinned refs; no manifest source change | snapshot if app has persistent data | preserve listener topology, resume/publish after runtime switch |
| Edit Config | current or config-pending catalog source | preserve committed active rootfs; no registry access | snapshot only when rendered config policy requires runtime/data protection | prepare only if rendered listener/access changes are allowed |
| Catalog Manifest Review | manifest-review pending catalog source | same as Modify App | same as Modify App | same as Modify App |
| Catalog Config Review | config-pending catalog source | same as Edit Config | same as Edit Config | same as Edit Config |
| Catalog Auto Apply | rendered catalog source that policy proves low-risk | preserve rootfs; image-only catalog diffs are not current-source Update Image | no new high-risk data exposure | no operator-only access changes |

API/UI hints about the desired flow are non-authoritative. A mismatched pending
catalog flow fails closed.

Current catalog classifier behavior maps into v2 operation policy as follows:

| Current condition | v2 policy |
| --- | --- |
| `DiffKindNone` | Mark catalog source committed; no runtime transition. |
| `DiffKindOIDCLibraryOnly` | Auto-apply live OIDC/library update; no runtime switch. |
| `DiffKindStructuralNoImage` and policy allowed | Auto-apply through v2 transition with preserve-rootfs policy. |
| `DiffKindStructuralNoImage` and policy requires review | Store manifest-review or config-pending source according to policy reason/input requirement. |
| `DiffKindImageOnly` | Store manifest-review source transition that commits catalog raw template and stages rootfs; do not route to current-source Update Image. |
| `DiffKindStructuralWithImage` | Store manifest-review source transition when stageable; otherwise fail closed with manual sequencing/reinstall reason. |
| init-script drift or unverifiable init-script hash | Fail closed; manual reinstall remains required. |
| legacy install allowlisted OIDC/library fields | Preserve existing legacy patch behavior until v2 ledger is complete. |
| legacy install non-allowlisted source, service set, or listener set change | Fail closed; manual reinstall remains required. |

The planner must preserve these classifications until an RFC explicitly changes
catalog policy.

No current catalog image-only path may use current-source Update Image. A
future automatic image-only catalog route would need its own policy decision
and must still commit the catalog source and metadata through the transition.

Runtime-changing transitions require an enabled app in the initial v2
implementation. Disabled/stopped apps can receive metadata-only updates, but
Modify App, Update Image, catalog manifest review, and runtime-changing Edit
Config fail closed with "start app before applying runtime update." A future
no-publication/no-start staging mode would be a separate RFC because it trades
away readiness proof and changes operator expectations.

### D3. Use One Durable Transaction Shape

Add one v2 transaction record for transition-owned app mutation state. It
records the plan domains above plus phase and recovery predicates.

Required phase vocabulary:

- `prepared`: transaction created with previous-state anchors.
- `resources_prepared`: rootfs/listeners/snapshot viability and cleanup records
  are durable.
- `commit_intent`: the executor has all resources needed to either restore or
  forward-complete; recovery must use the transaction, not inference.
- `switching_runtime`: access quiesce/runtime switch is in progress.
- `candidate_touched`: candidate runtime may have touched persistent data.
- `source_committing`: manifest/ledger/metadata commit is in progress.
- `source_committed`: durable source of truth points at the candidate.
- `publishing_access`: runtime/source are committed but access may need repair.
- `committed_metadata_pending`: source/runtime/access commit is complete, but
  catalog metadata or sync bookkeeping needs retry.
- `committed_cleanup_pending`: user-visible commit succeeded; cleanup must be
  retried.
- `committed`: transition and cleanup are complete.
- `restoring_previous`: recovery is restoring the previous source/runtime/data.
- `restore_failed`: recovery needs operator/developer intervention.

Legacy `manifest_update_transaction.json` and `image_update_transaction.json`
remain readable until drained. New writes use the v2 transaction.

Transition-created external resources require durable intent before creation:

| Resource domain | Durable-before-create rule | Discovery/cleanup fallback |
| --- | --- | --- |
| rootfs/golden volumes | deterministic staged rootfs key and cleanup role are in the record before create/pull begins | recovery destroys or resumes by recorded key; unrecorded creates are not allowed |
| data snapshots/failed LVs | snapshot and failed-LV names are recorded before snapshot/rollback can create them | recovery restores/destroys by recorded name and treats missing artifacts as retryable cleanup facts |
| listener reservations/endpoints | reservation key and listener identity are recorded before allocate/start/publish | repair/release uses recorded key; failed publish keeps the reservation retained |
| generated OIDC clients | service/purpose/redirect fingerprint is recorded before client creation; created client ID/secret handle is appended before use | cleanup deletes by recorded client ID or reconciles by fingerprint if creation result is uncertain |
| candidate runtime | candidate container names/rootfs attachments are recorded before create; `candidate_touched` is written before data can be mounted | recovery treats uncertain candidate data access as touched and restores snapshot when source commit is not proven |

If a storage failure happens after an external allocator succeeds but before
the append that records the concrete result, recovery must still be able to
find the artifact from the previously recorded deterministic key or fingerprint.
Allocators that cannot provide such a key are not eligible for use inside the
transition boundary.

Per-app transaction exclusivity is mandatory:

- startup recovery checks v2 transition records first;
- if no v2 record exists, startup drains one legacy manifest/image transaction
  for the app before ordinary reconcile can start;
- no new v2 transition may begin while any legacy transaction file exists for
  that app;
- if both v2 and legacy records are found, v2 wins and recovery marks the
  legacy record as blocked-for-manual-inspection rather than executing two
  independent recoveries;
- normal planning fails with `transition already in progress` until recovery
  clears or marks the older record.

Legacy compatibility is a required migration contract:

| Legacy record | Legacy phase/predicate | v2 interpretation | Recovery action |
| --- | --- | --- | --- |
| manifest update | `committed` or `committed_cleanup_pending` | source committed, cleanup may be pending | run committed cleanup through v2 cleanup adapter, then clear legacy record |
| manifest update | ledger commit proven by manifest/install-state fingerprints | source committed, access may be unpublished | forward-complete catalog metadata/access repair, then cleanup |
| manifest update | `RuntimeTouched` or phase implies runtime touched before source commit | candidate may have touched data | restore precommit data snapshot, restore previous source/rootfs, recreate previous runtime only if enabled |
| manifest update | runtime switch started but runtime not touched | previous source still authoritative | restore previous source/rootfs/access, cleanup staged artifacts |
| manifest update | prepared/listener/access fields present after commit | access repair pending | restore prepared endpoints when present; otherwise repair from runtime or resume preserved publication |
| image update | `CommitIntent` or `forward_repair_failed` | current source committed to candidate rootfs/runtime | forward-complete active rootfs, tuple generation, containers, status, and cleanup |
| image update | `CandidateDataRisk` without commit intent | candidate may have touched data | restore data snapshot and previous active rootfs |
| image update | `RuntimeSwitchStarted` before candidate data risk | previous source/rootfs authoritative | restart previous runtime and clear staged rootfs when safe |
| image update | snapshot planned/created before switch | resources prepared only | cleanup or preserve snapshot according to existing image-update rollback artifact rules |
| unknown/corrupt legacy phase | no trustworthy recovery state | blocked | fail closed, surface repair diagnostics, and prevent normal reconcile |

The legacy adapter must enumerate concrete phase strings, not only predicate
classes:

| Legacy type | Concrete phase strings | Adapter bucket |
| --- | --- | --- |
| manifest | `prepared`, `candidate_persisted`, `rootfs_staging`, `rootfs_staged`, `listeners_prepared`, `data_snapshot_planned`, `data_snapshot_failed`, `data_snapshot_created`, `access_suspending`, `access_suspended`, `runtime_switch_started`, `recreating_runtime` | pre-source-commit manifest transition; restore previous source/rootfs/access, restoring data snapshot when `RuntimeTouched`/`candidate_touched` is set or cannot be disproven |
| manifest | `ledger_committing`, `publishing_access`, `access_published`, `committed_metadata_pending` | source commit may have crossed; prove commit from manifest/install-state/catalog fingerprints, then forward-complete or restore according to proof |
| manifest | `committed`, `committed_cleanup_pending` | committed cleanup path |
| manifest | `restoring_previous`, `restore_failed` | preserve transaction and surface blocked repair unless adapter can complete the existing restore semantics exactly |
| image | `snapshot_planned`, `snapshot_created`, `runtime_switch_started` | pre-candidate-data image transition; cleanup or restore previous runtime/rootfs according to existing image recovery |
| image | `candidate_data_risk`, `restoring_previous`, `restore_failed` | candidate may have touched data; restore data snapshot/previous active rootfs |
| image | `commit_intent`, `forward_repair_failed` | forward-complete candidate rootfs/tuple/runtime metadata |
| image | `committed`, `committed_cleanup_pending` | committed cleanup path |

Compatibility tests seed each concrete legacy phase above, plus the legacy
boolean combinations (`RuntimeSwitchStarted`, `RuntimeTouched`,
`AccessSuspended`, `AccessPublished`, `CandidateDataRisk`, `CommitIntent`),
and prove the v2 dispatcher blocks normal reconcile, then preserves the
existing forward/rollback result.

For ambiguous legacy phases, the adapter matrix is phase plus predicate
booleans, not phase alone. In particular, image `restoring_previous` and
`restore_failed` are tested both with and without `CandidateDataRisk`; manifest
restore phases are tested with and without `RuntimeTouched` and
`AccessSuspended`. The adapter must pick the data-restore bucket whenever a
legacy predicate says candidate data may have been touched or cannot be
disproven.

### D4. Recovery Is A Single Dispatcher

Startup and explicit repair first load v2 transition transactions. Legacy
manifest/image transactions are handled by compatibility adapters until no such
records remain.

Recovery decisions come from durable predicates:

- If source/ledger/runtime commit is proven, forward-complete access/catalog
  metadata and cleanup according to the remaining proof layers.
- `candidate_touched` means "candidate runtime may touch persistent data." It
  must be written durably before any candidate process can mount or write app
  data. If the executor cannot prove no candidate process reached data, recovery
  treats the barrier as crossed.
- If candidate runtime may have touched persistent data before source commit,
  restore the precommit data snapshot even when the app is disabled.
- If runtime switch started but candidate did not touch data, restore previous
  manifest/ledger/rootfs and recreate previous runtime only when enabled.
- If listener publication failed after commit, keep prepared endpoints or
  retained reservations until repair or cleanup explicitly resolves them.
- If cleanup fails after commit, keep the transaction as
  `committed_cleanup_pending` and expose repair state without declaring the app
  rolled back.

Normal reconcile must not overrule a pending transition.

Recovery proof is layered:

| Proof layer | Required facts | Missing/partial behavior |
| --- | --- | --- |
| source/ledger/runtime | operation-specific source hash, install-state revision, active rootfs/runtime, and tuple facts prove candidate became authoritative | restore previous state unless `candidate_touched` requires data restore first; never treat missing source proof as metadata-only |
| pending-source ownership | transaction-owned catalog source snapshot hash matches candidate source; pending slot is either still frozen to that source or already cleared by candidate ledger | do not use a later mutable pending slot to disprove the transaction; retain/restore the transaction source snapshot |
| catalog metadata/sync | committed catalog hash, sync error/hash, and pending-clear state reflect the candidate source | enter or retain `committed_metadata_pending`; do not reapply source |
| access publication | `AccessPublished` and proxy/OIDC publication facts match prepared endpoints | stay in `publishing_access`; do not downgrade to metadata pending |
| cleanup | all transition-recorded artifacts are destroyed, retained, or transferred to tuple/app metadata | enter or retain `committed_cleanup_pending` |

Source commit proof is operation-specific. Recovery may forward-complete only
when the source/ledger/runtime proof layer for the operation is present:

| Operation | Commit proof | If proof is partial |
| --- | --- | --- |
| Modify App | app manifest hash equals candidate hash and install-state revision/source hash equals candidate ledger plan | restore previous manifest/install-state unless `candidate_touched` requires data restore first |
| Edit Config | app manifest hash equals candidate hash and install-state revision/input/source hash equals candidate plan | restore previous manifest/install-state unless `candidate_touched` requires data restore first |
| Catalog Manifest Review | Modify App proof plus transaction-owned manifest-review source snapshot hash matches candidate source | if source proof exists but pending-clear/catalog metadata proof is missing, enter `committed_metadata_pending`; otherwise restore previous source |
| Catalog Config Review | Edit Config proof plus transaction-owned config source snapshot hash matches candidate source | if source proof exists but pending-clear/catalog metadata proof is missing, enter `committed_metadata_pending`; otherwise restore previous source |
| Catalog Auto Apply | operation-specific manifest/ledger proof plus transaction-owned rendered catalog source snapshot hash matches candidate source | if source proof exists but catalog metadata proof is missing, enter `committed_metadata_pending`; do not reapply source |
| Update Image | source hash/manifest unchanged, active rootfs equals candidate rootfs plan, and tuple active generation matches candidate rootfs/data snapshot plan when persistent data exists | forward-complete rootfs/tuple metadata if candidate data committed; otherwise restore snapshot/rootfs |

Catalog source ownership is also explicit:

| Catalog state | Raw template owner | Pending behavior | Metadata behavior | Recovery behavior |
| --- | --- | --- | --- | --- |
| no pending source | install-state `RawTemplate` | none | `CatalogManifestHash` tracks committed catalog source | no special recovery |
| config pending | install-state `PendingRawTemplate` with flow `config` | only Edit Config/catalog config review may consume; frozen once a transition record snapshots it | unchanged until config transition commits | retain pending on rollback; clear only when candidate ledger commits |
| manifest-review pending | install-state `PendingRawTemplate` with flow `manifest_review` | only Modify App/catalog manifest review may consume; frozen once a transition record snapshots it | unchanged until manifest transition commits | retain pending on rollback; clear only when candidate ledger commits |
| legacy inferred pending | old source without flow plus reason heuristic | planner resolves to config or manifest flow and writes normalized flow before review | unchanged until commit | if normalization cannot prove flow, fail closed |
| auto apply | fetched catalog raw rendered by sync | no pending source | write committed source hash and clear sync error only after transition/source commit | metadata retry uses transaction state, not a second apply |
| image-only pending source | fetched catalog raw with image-only diff | manifest-review transition consumes it; frozen once snapshotted by transition | committed source hash advances only after reviewed source/rootfs transition commits | rollback retains pending source; commit clears pending source and updates catalog metadata |
| review apply committed but metadata write failed | candidate ledger/source committed | pending source already cleared by candidate ledger | `CatalogManifestHash`/sync error update retried from transaction | show committed-with-metadata-repair state, not failed update |

Only the transition executor clears pending catalog source. Catalog sync may
create or refresh pending source records only when no transition has snapshotted
that pending source. Once a review or auto-apply transition records a
transaction-owned catalog source snapshot, catalog sync must not overwrite that
pending slot until the transition reaches rollback or committed metadata
completion. A later catalog fetch is ignored until the next sync; it is not
consumable by Edit Config, Modify App, recovery, or UI routing while the active
transition owns the current pending source.

### D5. Transition Owns Rootfs And Data Artifact Cleanup

Rootfs cleanup may not be derived from the current YAML service list alone.
Cleanup uses transition records plus current `ActiveRootfs`:

- staged rootfs not made active are destroyed or retried;
- superseded active rootfs are destroyed after commit cleanup;
- removed service rootfs are destroyed even when the service no longer appears
  in the candidate manifest;
- synthetic runtime entries are preserved or cleaned by entry kind, not YAML
  service membership;
- failed data LVs created by rollback are recorded in tuple metadata for later
  GC;
- uninstall and tuple GC consult transition metadata before deciding what can
  be destroyed.

Uninstall and tuple GC use the v2 transaction as the artifact inventory:

| v2 phase | Uninstall behavior | Tuple/cleanup behavior |
| --- | --- | --- |
| no transition | existing uninstall/GC behavior | existing tuple/rootfs GC behavior |
| `prepared` / `resources_prepared` | reject uninstall until transition rolls back or clears | cleanup only staged resources explicitly recorded by the transaction |
| `commit_intent` / `switching_runtime` / `candidate_touched` | reject uninstall; recovery must restore or forward-complete first | do not GC previous/candidate rootfs, snapshots, failed LVs, or prepared listener reservations |
| `source_committing` / `source_committed` | reject uninstall until recovery proves commit direction | preserve all previous/candidate artifacts until recovery reaches committed or restored state |
| `publishing_access` | allow explicit access repair; reject uninstall by default to avoid destroying retained reservations before repair state is written | preserve prepared/retained listener reservations and candidate runtime artifacts |
| `committed_metadata_pending` | uninstall may proceed only through transition-aware metadata/cleanup finalization first | retry catalog metadata/sync bookkeeping, then cleanup inventory |
| `committed_cleanup_pending` | uninstall may proceed only through transition-aware cleanup that first consumes the transaction inventory | retry staged/superseded/removed rootfs, data snapshot, failed-LV tracking, and generated credential cleanup before or during uninstall |
| `committed` | ordinary uninstall allowed | transition record should already be cleared; lingering record is cleanup-pending |
| `restoring_previous` / `restore_failed` | reject uninstall until operator/developer repairs or explicitly force-purges with diagnostics | preserve all artifacts referenced by the failed transaction |

Force-purge of a failed transition is outside normal UI update flow. If added
later, it needs its own operator confirmation and support diagnostics.

### D6. Runtime Mutation Goes Through The Executor

Feature paths should stop directly calling container recreation as a lifecycle
operation. The shared executor owns:

- listener prepare before runtime switch;
- access suspension before candidate runtime can receive traffic;
- snapshot viability before downtime;
- quiesce before data snapshot creation;
- precommit snapshot creation before candidate data touch;
- rootfs attachment and active-rootfs update;
- candidate runtime creation;
- readiness check;
- source/ledger commit;
- access publication and repair markers.

Low-level container helpers remain, but become executor internals or
executor-only dependencies.

### D7. Access Repair Is A First-Class Operation

Access repair is a transition operation, not an incidental reconcile side
effect:

- automatic startup recovery retries access repair for `publishing_access`
  before ordinary reconcile;
- the app detail API exposes repair state, last error, retained/prepared
  endpoint summary, and whether app runtime is otherwise committed;
- the UI exposes an explicit retry action when automatic repair fails;
- a successful repair marks `AccessPublished=true`, releases no-longer-needed
  retained reservations, refreshes proxy OIDC state, and proceeds to committed
  cleanup;
- a failed repair keeps reservations retained and prevents another transition
  from stealing or releasing them;
- starting a new update is not a repair action unless the planner explicitly
  proves it will consume the pending access-repair transaction first.

Every legacy/runtime-mutating entry point must also respect the v2 transition
fence before doing work. This includes Update Image, Start, Stop, Rollback,
Uninstall, service-mode storage resize, catalog sync apply/review, catalog sync
control writes, listener update, normal reconcile, access repair, metadata
retry, and cleanup retry. A v2 transition record means the entry point must
either join the transition through an allowed follow-up operation or fail closed
with `transition already in progress`.

The fence sits before the first observable or durable side effect: status
mutation, image pull, rootfs create/destroy, container start/stop for user
operations, storage volume resize, listener/proxy/OIDC/cert queue changes,
slice policy changes, tuple metadata writes, install-state writes, app metadata
writes, sync enable/disable/trigger/refresh-context writes, and app YAML
writes. Allowed exceptions are read-only detail/progress/log/sync-status
access, startup recovery, explicit transition follow-up operations that
consume the active record, and daemon shutdown/follower quiesce that stops
processes without mutating source, rootfs/data metadata, listener reservations,
or transition state except for recording the quiesce result.

### D8. UI Projects The Transition Plan

The app UI should present one calm update model:

- `Update` chooses the correct operation. If review is unnecessary, it runs; if
  review is required, the same action opens review.
- `Modify App` is the YAML/template source editor.
- `Edit Config` is the installed input/config editor.
- Review screens show plan-derived groups: source, credentials/current-value
  reuse, image/rootfs, storage/data, services/runtime, exposure/access, and
  interruption.
- Sensitive current values are never shown, but the UI states whether the
  backend will keep them, require re-entry, regenerate them, or reject reuse.
- Progress/readiness settlement distinguishes committed update success from
  access-repair or cleanup-pending follow-up.

The app detail projection must tell the operator what `Update` will do before
they click:

| State | Primary label | Pre-click meaning |
| --- | --- | --- |
| mutable current images, no catalog pending, no review required | `Refresh image now` | immediately refresh current-source mutable images and show consequence copy |
| mutable current images, no catalog pending, review required by data/interruption policy | `Review image refresh` | open current-source image refresh plan before apply |
| catalog manifest review pending | `Review update` | open manifest-review plan from catalog source |
| catalog config pending | `Continue update` | open Edit Config with pending catalog source |
| registry/data preflight blocked | disabled `Update` | show blocker and retry guidance |
| access repair pending | `Retry access repair` | repair committed runtime access, not rerun the update |
| cleanup pending | `Finish cleanup` | finish transition cleanup, not rerun the update |
| metadata pending | `Finish catalog update` | retry catalog metadata/sync bookkeeping for an already committed update |
| no refreshable image and no pending source | disabled/no-op state | show "Up to date" or hide action according to existing app detail pattern |

Primary action priority is follow-up before new work: restore failure/support
state, access repair, cleanup, metadata retry, catalog pending review/config,
then current-source image refresh. A lower-priority image refresh or catalog
review must not hide an active follow-up action.

Pending catalog UI must show source, flow, and routing:

- banner title: `Update needs review`, `Update needs config values`, or
  state-specific follow-up text such as `Access repair needed`,
  `Cleanup pending`, or `Catalog status needs retry`;
- source label: app catalog plus source hash/version if available;
- primary CTA routes to the only valid consumer for the pending flow;
- incompatible entry points are hidden or disabled with copy explaining that
  the catalog update must be handled through the primary CTA;
- unknown or stale pending flow shows a fail-closed refresh/repair message
  rather than opening the wrong wizard.

Sensitive field projection is field-level:

| Field state | Default action | Apply behavior |
| --- | --- | --- |
| reusable non-sensitive current value | keep and display current value | apply allowed unless separate review is required |
| reusable sensitive current value | keep without display | apply allowed only after any required kept-value review is acknowledged |
| required missing sensitive value | re-enter | apply disabled until entered |
| generated value with missing/invalid stored value | regenerate | apply disabled until regenerate/enter decision is made |
| reuse rejected by structural/sensitive render policy | blocked | apply rejected with blocking reason |

Each field item carries label, provenance, allowed actions, chosen/default
action, blocking reason, confirmation ID when needed, and whether non-sensitive
edits can be preserved after stale-plan refresh.

Current-value reuse also carries a semantic-delta review item when the same
input key is reused but the candidate changes its meaning, sensitivity,
generation behavior, description, or manifest usage path. The review item
includes old/new usage paths, old/new meaning where known, risk kind,
confirmation ID, and a blocking reason if the backend cannot explain reuse
safely enough for an operator to approve it.

Settlement states are explicit:

| Settlement | Title/severity | App usability | Primary action |
| --- | --- | --- | --- |
| committed | success | candidate runtime/access committed | refresh detail |
| access repair pending | warning | runtime/source committed, access may be unavailable | retry access repair |
| cleanup pending | warning | update committed, cleanup still retrying | finish cleanup / view details |
| metadata pending | warning | update committed, catalog metadata will retry | finish catalog update |
| restore failed | error | previous/candidate state uncertain | view diagnostics / support repair |
| stale plan rejected | neutral warning | no mutation occurred | refresh preview; preserve safe edits; re-request sensitive values as needed |

Stale plan/fingerprint mismatches are preview invalidation, not failed updates.
The UI must explain that app state changed since preview, refresh the plan, and
avoid implying runtime rollback. After refresh, the UI states which
non-sensitive edits were preserved, which confirmations were reset, and which
sensitive values or generate/keep choices must be re-entered.

### D9. API Contract

Each operation exposes either a plan/apply pair or an explicit compatibility
wrapper over the same transition plan:

| Operation | API shape | Apply binding | Result semantics |
| --- | --- | --- | --- |
| Modify App | configure/dry-run/apply | dry-run token, plan hash, base manifest hash, ledger fingerprint, runtime fingerprint | transition result with review groups and settlement |
| Edit Config | dry-run/apply | dry-run token, plan hash, source/input hashes, ledger revision, runtime fingerprint | transition result or config projection of it |
| Catalog Manifest Review | same as Modify App with `catalog_pending=true` | pending source hash/flow plus normal binding | clears pending only after source commit proof |
| Catalog Config Review | same as Edit Config with pending source | pending source hash/flow plus normal binding | clears pending only after source commit proof |
| Update Image | minimal plan/apply or direct POST returning transition result | plan hash or current-source preflight hash; current manifest/source/runtime fingerprint | no-op, blocked, committed, access repair pending, cleanup pending, or failed |
| Access Repair / Cleanup Retry / Metadata Retry | explicit follow-up endpoint | transition operation ID and phase | committed/follow-up settlement |

If the existing direct Update Image endpoint remains during migration, it must
still execute through the v2 planner internally and return the v2 settlement
shape. It may omit operator confirmations only when the policy proves current
source image refresh requires no review beyond existing button intent.

The API/UI projection exposes a deterministic action kind, separate from label:
`refresh_now`, `preview_refresh`, `review_catalog_update`,
`continue_config_update`, `access_repair`, `finish_cleanup`, `metadata_retry`,
or `disabled`. The app detail view must not use one action kind for both
immediate mutation and preview.

## Site List

### Backend Planning And Policy

- `internal/app/custom_manifest_update.go`
  - Keep source rendering, input reuse, and operator confirmation policy.
  - Replace manifest-specific image/rootfs/data/access transaction decisions
    with v2 transition plan/executor calls.
  - Continue to fail closed for unsupported OIDC lifecycle and storage
    mutations unless explicitly accepted by this RFC's policy table.
- `internal/app/installed_config_update.go`
  - Treat Edit Config as a transition-plan consumer.
  - Preserve active rootfs by policy and record that preserve decision in the
    plan.
  - Route config-pending catalog sources only through config operation policy.
- `internal/app/catalog_sync_apply.go`
  - Use operation policy to choose auto apply, config pending, manifest-review
    pending, or failure.
  - Do not expose manifest-review pending sources to Edit Config or config
    pending sources to Modify App.
  - Freeze pending catalog source records once a transition snapshots them; a
    later catalog fetch cannot overwrite the source used by active recovery.
  - Route any runtime-changing reviewed catalog apply through the v2 executor.
- `internal/app/catalog_sync.go`
  - Fence sync trigger, enable/disable, and refresh-context writes before
    `StoreInstallState` or `StoreAppMetadata` when a v2 transition is active.
  - Allow read-only sync status during active transitions.
- `internal/app/app_manager.go`
  - Move Update Image planning/execution onto the shared transition boundary.
  - Keep `ImageUpdateBlockedReason` as a projection of transition/data
    preflight, not independent policy.
  - Keep low-level container/rootfs helpers only as executor dependencies.
  - Gate Start, Stop, Rollback, Uninstall, listener update, and Update Image
    against active v2 transitions before mutating runtime state.
- `internal/app/container_group_install.go`
  - Provide install-time image/rootfs behavior as shared evidence/helper for
    rootfs identity, digest normalization, and golden metadata.

### Durable State, Recovery, And Cleanup

- v2 transition state files
  - Persist hash-bound `TransitionPlan` separately from mutable
    `TransitionRecord` resource inventory.
  - Append concrete resource facts under the existing plan hash without
    changing the plan hash.
- `internal/app/installed_app_apply_transaction.go`
  - Retire as a manifest/config-specific executor after v2 equivalent exists.
  - During migration, keep behavior-equivalent wrappers for existing callers.
- `internal/app/image_update_transaction.go`
  - Stop new writes after Update Image migrates.
  - Keep legacy recovery adapter until old records are drained.
- `internal/app/*reconcile*` / restore-services startup paths
  - Run v2 recovery before ordinary reconcile and make ordinary reconcile
    skip/repair only through allowed transition follow-up operations.
- `internal/app/tuple.go`, `internal/app/tuple_gc.go`
  - Include transition-owned failed data LVs and active/superseded rootfs
    references in cleanup decisions.
- `internal/app/rootfs_integration.go`
  - Keep `ActiveRootfs` as runtime truth for service and synthetic entries.
  - Ensure executor updates candidate `ActiveRootfs` before committed runtime
    recreate depends on it.
- `internal/persistence/interfaces.go`
  - Expose typed rootfs metadata lookup needed by the transition planner.
- `internal/persistence/rootfs_volume_manager.go`
  - Remain storage-backed authority for golden/rootfs metadata.
- `internal/persistence/luks_volume_manager.go`
  - Preserve snapshot viability, snapshot health, rollback, destroy, and failed
    LV semantics required by transition data domain.

### Listener And Access

- `internal/services/manager.go`
  - Prepared reconcile, reservation retention, restore, publish, and release
    are transition resources.
  - Recovery must use prepared endpoints when present.
  - Auto/explicit TCP/UDP reservation keying must remain consistent across
    allocate, restore, publish, and release.
- `internal/app/catalog_sync_apply.go`,
  `internal/app/custom_manifest_update.go`,
  `internal/app/installed_config_update.go`,
  `internal/app/app_manager.go`
  - Remove direct ownership of listener lifecycle decisions once migrated.

### API And UI

- `internal/server/gin_app_handlers.go`
  - Return transition plan/review fields consistently for Modify App, Update,
    catalog review, and config update endpoints.
  - Gate service-mode `resize-storage` before persistence/rootfs lookup or
    `ResizeApplication` while a v2 transition is active.
- `internal/server/gin_app_sync_handlers.go`
  - Gate sync enable/disable/trigger/refresh-context mutations while a v2
    transition is active; keep sync status read-only.
- `docs/api/openapi.yaml`
  - Document the v2 transition plan fields and confirmation IDs.
- `ui/lib/core/models/app_models.dart`
  - Parse plan domains and pending catalog flow.
- `ui/lib/core/services/app_service.dart`
  - Preserve dry-run token, plan hash, base hashes, runtime fingerprint, and
    accepted confirmation IDs.
- `ui/lib/features/apps/app_detail_view.dart`
  - Route `Update`, `Modify App`, and `Edit Config` to the operation policy
    that matches user intent and pending catalog flow.
  - Prioritize restore/access-repair/cleanup/metadata follow-up actions before
    fresh update or image refresh actions.
  - Refresh detail after committed success, access repair, metadata retry, or
    cleanup-pending settlement.
- `ui/lib/features/apps/manifest_update_wizard.dart`
  - Render Modify App/catalog manifest review from the transition plan.
- `ui/lib/features/apps/installed_config_wizard.dart`
  - Render Edit Config/catalog config review from the transition plan or its
    config projection.
- `ui/lib/features/apps/app_operation_lifecycle.dart`
  - Treat access repair, cleanup pending, and metadata pending as
    committed-with-follow-up states, not failed updates.

### Tests And Validation

- `internal/app/custom_manifest_update_test.go`
  - Keep first-slice image/rootfs tests and add v2 executor conformance for
    manifest-review transitions.
- `internal/app/installed_config_update_test.go`
  - Cover config rootfs-preserve policy, runtime-changing config transitions,
    and config-pending catalog routing.
- `internal/app/update_image_test.go`
  - Move Update Image tests onto the shared transition conformance matrix.
- `internal/app/image_update_transaction_test.go`
  - Keep only legacy recovery compatibility tests after migration.
- `internal/app/tuple_gc_test.go`
  - Cover transition-recorded failed LVs, superseded rootfs, and uninstall
    cleanup.
- `internal/services/*_test.go`
  - Preserve prepared listener repair, UDP/TCP reservation, and publication
    failure behavior.
- `ui/test/*`
  - Cover the unified Update action, pending catalog flow routing, hidden
    sensitive value reuse, semantic kept-value review, review confirmation
    gating, metadata retry, and committed-with-repair settlement.
- transition planner hash tests
  - Cover stable map/list ordering, nil/empty canonicalization, normalized
    image identity, confirmation ordering, redaction exclusions, unknown-field
    rejection, and dry-run/apply reload stability.
- transition recovery/resource tests
  - Cover store failure immediately after each external allocation class, source
    proof vs metadata proof layering, frozen pending catalog source recovery,
    active-transition fence placement before side effects, service-mode
    resize-storage blocking before `ResizeApplication`, and sync control
    blocking before install-state or metadata writes.
- `scripts/alpha/*`
  - Exercise Modify App same-ref refresh, Update Image mutable-tag refresh,
    catalog manifest review, config-pending catalog review where feasible, and
    rollback/recovery after forced failure.

## Migration Plan

### Phase 1. Contract And Planner

Add v2 transition types and a pure planner that can produce a plan for current
Modify App, Edit Config, Catalog Review, and Update Image scenarios. Existing
flows may still execute through legacy paths, but tests assert that the v2 plan
matches current accepted behavior.

Exit criteria:

- plan fields cover source, image/rootfs, data, runtime, access, ledger,
  cleanup, and review domains;
- `TransitionPlan` hash-bound fields and `TransitionRecord` mutable resource
  fields are separated and tested;
- plan hash canonicalization is versioned and excludes UI-only/redacted fields;
- plan hash canonicalization has stable ordering, nil/empty, normalized image
  identity, confirmation-ordering, unknown-field, and reload-stability tests;
- unsupported storage/workspace cases fail closed in the planner;
- disabled runtime-changing transitions fail closed with an operator-facing
  "start app before applying runtime update" reason;
- pending catalog flow mismatches fail closed;
- current catalog diff/legacy classifications map to explicit v2 operation
  policy;
- catalog pending source freeze behavior is defined for active transitions;
- no caller-supplied lifecycle hint can override operation policy;
- legacy transaction coexistence fails closed before any new v2 transition;
- every legacy/runtime-mutating entry point checks for active v2 transition
  before doing work and before any side effect, including service-mode storage
  resize and sync control writes.

### Phase 2. Executor For Manifest/Config Transitions

Replace `installedAppApplyTransaction` behavior with the v2 executor while
keeping public API response shapes compatible. Modify App, Edit Config, and
catalog review applies use the same executor.

Exit criteria:

- rootfs staging and current-value/secret reuse remain behavior-compatible;
- snapshot viability happens before downtime;
- snapshot creation happens after quiesce and before candidate data touch;
- durable intent is recorded before rootfs, snapshot/failed-LV, listener, OIDC,
  or candidate runtime resources can be created;
- `candidate_touched` is durable before any candidate process can mount or
  write persistent data;
- recovery restores data snapshot whenever candidate runtime touched data and
  source commit did not complete;
- committed access failure is represented as repair pending.

### Phase 3. Update Image Migration

Move Update Image onto the same planner/executor and stop writing new
`ImageUpdateTransaction` records. The executor treats Update Image as a
current-source rootfs refresh transition. The existing direct Update Image API
may remain as a compatibility entry point only if it returns v2 transition
settlement and obeys pending-catalog/access-repair precedence.

Exit criteria:

- mutable tags refresh by digest and fail fast when registry/digest resolution
  is unavailable;
- digest-pinned refs no-op;
- persistent-data snapshot behavior matches Modify App same-ref refresh;
- tuple generation recording is preserved;
- minimal Update Image plan/settlement API behavior is defined and tested;
- legacy image-update recovery still drains old transaction records;
- no new Update Image starts while legacy/v2 transition repair is pending.

### Phase 4. Recovery, Cleanup, And Uninstall Composition

Make the v2 recovery dispatcher authoritative and ensure cleanup consumers use
transition metadata.

Exit criteria:

- v2 transaction recovery runs before normal reconcile;
- legacy manifest/image recovery remains compatible;
- committed cleanup retry survives restart;
- committed metadata retry survives restart as `committed_metadata_pending`;
- recovery distinguishes source/ledger/runtime proof from pending-source and
  catalog metadata proof;
- failed data LVs are tracked for GC;
- access repair retry is automatic on startup and available as an explicit
  follow-up operation;
- uninstall and tuple GC cannot orphan transition-recorded rootfs or data
  artifacts.

### Phase 5. UI/API Projection

Unify the operator surfaces around transition plans.

Exit criteria:

- Update action either runs directly or opens required review;
- app detail states what Update will do before click;
- Modify App is clearly source/YAML modification;
- Edit Config remains installed-input editing;
- pending catalog banners expose source, flow, and the only valid CTA;
- app detail exposes metadata retry and state-specific access repair/cleanup
  copy as follow-up actions;
- review copy states exactly which current values are kept, re-entered,
  regenerated, unavailable, or semantically changed;
- stale preview rejection refreshes the plan without presenting a failed
  runtime update;
- HTTP success and progress-stream success settle to the same detail refresh
  and follow-up state.

### Phase 6. Release Validation

Run the full review cadence and alpha validation after focused tests pass.

Required gates:

- focused Go tests for app transition planner/executor, update image, manifest
  update, installed config update, tuple cleanup, and service publication;
- UI tests for changed review/settlement behavior;
- `go test ./...` or documented environment-only failures;
- `flutter analyze` and relevant Flutter tests when UI changes land;
- `scripts/alpha/*` coverage for same-ref Modify App refresh, Update Image
  refresh, and at least one reviewed catalog/config transition;
- full code review cadence green before commit/release.

## Fork Points Requiring User Decision

Pause implementation and return to design if any of these occur:

- supporting a real user case requires storage volume removal, rename, or
  mount-path mutation rather than rejection;
- workspace image/base mutation becomes necessary;
- catalog auto-apply needs to apply a high-risk service/rootfs/listener change
  without operator review;
- the v2 executor cannot coexist with legacy transactions without a one-time
  migration step;
- user-facing rollback-after-success semantics are needed to complete the
  boundary;
- UI simplification requires removing or renaming a public API already consumed
  elsewhere.
