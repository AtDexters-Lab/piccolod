# RFC: Installed App Transition Boundary

**Date:** 2026-06-21
**Status:** RFC reviewed; first implementation slice in validation

## Scope block

**Problem:** Installed-app mutation flows can independently change manifest, ledger, rootfs, containers, listeners, and recovery state, so lifecycle invariants are easy to preserve in one flow while accidentally violating them in a sibling flow.
**In scope:** A shared installed-app transition boundary for service-mode app mutations, the domain plans that must sit behind it, the invariants each domain owns, the first release-ready slice for Modify App stale image/rootfs prevention, and the site list that must compose with the boundary.
**Out of scope:** A full rewrite of every installed-app operation in the first implementation, workspace app rebasing, storage migration/removal semantics, the stale/orphaned LV cleanup incident, automatic high-risk catalog application, user-initiated rollback after a successful broad app update, and UI redesign beyond the behavior needed to present transition plans truthfully.

## Background

Piccolo now has several installed-app mutation paths:

- install and reinstall;
- exact-tag image/rootfs refresh through Update Image;
- custom full-source replacement through Modify App;
- installed config updates;
- catalog sync and catalog-pending review;
- start, stop, reconcile, recovery, rollback, and uninstall cleanup.

Each path was reasonable when viewed locally. The unsafe pattern is that several
of them are vertical flows: they can touch app source, rendered manifest,
stored values, image/rootfs state, persistent-data safety, containers,
listeners, access publication, task progress, and recovery metadata without
going through a single lifecycle contract.

The Piclu stale-image incident is the current concrete failure:

- Update Image already treats a mutable tag as a reference whose digest can move
  without the image ref string changing.
- install/rootfs cache lookup also knows that a same tag can require a registry
  freshness check.
- Modify App currently stages rootfs only for new services or changed image ref
  strings, so it can recreate an app from a changed source while reusing stale
  active rootfs volumes for existing services whose mutable tags moved.

That is not only a missing `if` in Modify App. It means image identity was not
owned by an installed-app transition domain. A feature flow was allowed to
decide image/rootfs reuse from a local manifest diff.

The recent review feedback on service app update work showed the same class of
bug in other domains:

- data snapshot timing and cleanup depended on flow-local ordering;
- listener reservation, prepared publication, and access repair had state
  ordering holes;
- catalog pending state could be routed through the wrong flow;
- active rootfs metadata cleanup could drift from the committed app definition;
- UI operation settlement could disagree with backend commit state.

The common root is ownership: Piccolo has domain invariants, but not yet a
mandatory boundary that all generation-changing operations must pass through.

## Decision summary

Introduce an installed-app transition boundary as the architectural owner for
service-mode runtime generation changes.

Feature flows submit an operation intent. The transition boundary renders or
accepts the candidate app state, asks each domain planner for a plan, produces a
single redacted transition plan for review/dry-run, persists a durable
transaction for apply, and executes or recovers from that plan.

Callers do not choose lifecycle safety policy directly. They supply intent and
source provenance. The transition boundary derives the legal source,
image/rootfs, data, access, and UI policy from the operation kind plus the
candidate source. If a caller tries to submit a source-changing candidate under
a preserve-rootfs/config-only policy, the boundary rejects the request before
planning. This rule prevents another flow-mixing bug where a high-risk source
update is accidentally routed through a lower-risk config path.

The boundary does not make every operation identical. It makes operation
differences explicit policy:

| Operation | Source policy | Image/rootfs policy | Runtime policy |
| --- | --- | --- | --- |
| Update Image | current installed source | refresh mutable image refs, preserve manifest refs | recreate runtime from current source when rootfs changes |
| Modify App | pasted full replacement source | verify and plan all candidate service rootfs identities before runtime commit | recreate runtime from candidate source |
| Edit Config | current installed source plus ledger changes | preserve active rootfs; no image pull by policy | recreate only when config requires runtime materialization |
| Catalog low-risk sync | catalog source | preserve or reject according to classifier | automatic only for low-risk classes |
| Catalog high-risk review | catalog source | same semantics as Modify App when the source changes runtime image/rootfs identity | operator-reviewed transition |
| Reconcile/recovery | committed source and transaction state | repair from committed active rootfs or retained transaction plan | restore or forward-complete according to transaction phase |

The first implementation slice is not a full rewrite. It establishes the
boundary where the current production bug lives: image/rootfs planning for
runtime-changing Modify App, shared with or made congruent with Update Image.

## Core invariants

### I1. Runtime generation changes need a complete transition plan

Any operation that commits a new service-mode runtime generation must have a
plan for:

- rendered source and config ledger state;
- active rootfs identities for every candidate service;
- persistent-data snapshot requirement and viability;
- container recreation or preservation;
- listener/access preparation and publication;
- durable transaction and recovery behavior;
- user-facing review and required confirmations.

An operation can set a domain to "preserve" only as an explicit plan decision.
It must not get preserve behavior by failing to mention the domain.

### I2. Image ref equality is not image identity

For mutable refs, `old image ref == new image ref` does not prove that the
runtime image is unchanged. The image/rootfs domain owns the identity rule:

- digest-pinned refs are immutable unless the ref string changes;
- mutable refs require registry digest verification when an operation will
  create or commit a runtime generation that depends on image freshness;
- a registry verification failure in a freshness-required operation fails
  closed before manifest or runtime commit;
- dry run and apply must bind to the same resolved digest and expected rootfs
  identity.

### I3. Persistent-data rollback is a pre-commit runtime boundary

When candidate containers can mutate existing persistent data, snapshot
viability, quiesce timing, snapshot creation, runtime touch markers, and
snapshot restore/cleanup are one ordered domain plan. Other domains may depend
on that boundary, but they do not define it locally.

### I4. Listener/access changes have prepare, commit, and publish phases

Listener endpoint allocation, public/remote port reservations, TLS/proxy
routing, auth/middleware routing, and access repair state are not just side
effects of container recreation. They have a prepared intent before commit and
a published state after commit. Recovery must know which one is authoritative.

### I5. Recovery reads the transaction before ordinary reconcile

Boot recovery and periodic reconcile must not infer a candidate truth merely
because files, LVs, containers, or service endpoints happen to exist. Pending
transition transactions are interpreted first. Only after the transaction is
resolved does normal reconcile operate on committed app state.

### I6. UI review is a projection of the plan

The UI does not decide risk from raw diffs. It presents the redacted transition
plan: what will change, what will be preserved, what is blocked, what requires
operator review, and what Piccolo will do if apply fails.

## Transition model

### Intent

`AppTransitionIntent` is the caller-owned request. It names the operation and
the candidate source of truth.

Plan-level shape:

| Field | Meaning |
| --- | --- |
| operation kind | `update_image`, `modify_app`, `edit_config`, `catalog_sync`, `catalog_review`, `recovery`, or future explicit kinds |
| instance ID | installed app identity |
| candidate source | current installed manifest, pasted YAML, catalog source, or config ledger mutation |
| requested action context | caller-visible action and source provenance, never authoritative lifecycle safety policy |
| caller context | task type, UI source provenance, sync/manual flag, dry-run token context |

Boundary-owned policy:

| Operation kind | Boundary-derived policy rule |
| --- | --- |
| `update_image` | source must be the committed installed manifest; image/rootfs domain may refresh mutable refs; source/ledger domain must preserve manifest and ledger |
| `modify_app` | source must be pasted full replacement YAML; image/rootfs domain must plan candidate service identities; data/access domains apply according to candidate diff |
| `edit_config` | source must be current installed source plus value changes; image/rootfs domain must preserve active rootfs and reject source-changing candidates |
| `catalog_sync` | automatic apply is allowed only for low-risk classifier output; high-risk output records pending review instead of applying |
| `catalog_review` | source must be pending catalog manifest-review source; image/rootfs/data/access policies match source replacement semantics |

The first slice must include rejection tests for a manifest source submitted
through config policy, a catalog manifest-review source submitted as config,
and any forged or mismatched policy hints sent by API/UI callers.

### Plan

`AppTransitionPlan` is the transition boundary output. It is redacted for UI,
but complete enough for apply and recovery to avoid rediscovering policy.

Plan-level shape:

| Domain | Required decision |
| --- | --- |
| source/ledger | previous and candidate source hashes, rendered manifest hash, ledger revision/source behavior, stored-value reuse/regeneration/replace decisions |
| image/rootfs | per-service action: preserve active rootfs, refresh same ref, stage changed ref, create new service rootfs, remove/retain old service rootfs, or reject |
| storage/data | persistent storage stability, snapshot required/not required, viability result, private snapshot lifecycle |
| runtime | metadata-only, recreate from preserved rootfs, recreate from staged rootfs, or reject |
| listener/access | preserve, prepare new endpoints, publish after commit, repair after commit, or reject |
| confirmations | stable confirmation IDs and the redacted items they acknowledge |
| transaction | durable fields needed before each irreversible boundary |
| progress/readiness | task phases and post-apply readiness/access-repair expectations |
| operation policy summary | compact UI projection of source policy, image/runtime policy, config policy, and apply behavior |
| outcome states | blocked, ready, applying, rolled back before commit, committed, committed with access repair, restore failed |

Every domain returns one of:

- `preserve`: domain state is intentionally unchanged;
- `stage`: domain state is prepared before commit;
- `commit`: domain state becomes authoritative at commit;
- `publish_after_commit`: domain state is activated after runtime commit;
- `repair_after_commit`: candidate committed, domain needs forward repair;
- `reject`: candidate cannot be applied by this operation.

### Execution

Execution consumes the plan in a conservative order:

1. preflight source, ledger, image, storage, and access prerequisites;
2. stage rootfs and other candidate resources while current runtime is still
   authoritative;
3. prepare listener/access state without exposing candidate traffic;
4. quiesce runtime and take any required private data snapshot;
5. recreate candidate containers from planned rootfs state;
6. verify candidate runtime privately;
7. commit manifest, ledger, active rootfs, containers, and prepared access
   intent;
8. publish access and verify public/remote/local routes after commit;
9. clean up superseded resources according to transaction phase.

The exact ordering can be optimized only when the plan proves the same
invariants. For example, metadata-only config updates can skip rootfs and
runtime domains, while image-only refresh can keep source/ledger unchanged.

## Domain ownership

### Source and ledger

Owner responsibilities:

- pin app identity for rendered candidates;
- normalize and reuse stored values according to the operation policy;
- classify changed value meaning and usage for review;
- bind dry-run token to source hashes, rendered manifest digest, ledger
  revision, and relevant candidate state;
- prevent catalog pending config and catalog pending manifest review flows from
  crossing into each other by omission.

The source/ledger domain may say "preserve active source" for Update Image, or
"candidate source replaces current source" for Modify App. Both are explicit.

### Image and rootfs

Owner responsibilities:

- resolve candidate service image identity;
- decide per-service rootfs action;
- prove whether the current active rootfs identity is known, fresh, and
  preservable;
- compute expected versioned rootfs volume IDs;
- bind dry-run and apply to the same image digest and rootfs identity;
- reject if registry freshness is required and cannot be verified;
- update candidate `ActiveRootfs` for every candidate service;
- name removed/superseded rootfs volumes for bounded cleanup;
- keep digest-pinned refs immutable by policy.

Rootfs identity proof:

- the expected versioned rootfs volume ID is a target identity, not proof that
  the active rootfs contains the expected image;
- preservation is allowed only when authoritative rootfs metadata proves the
  active volume's base image digest matches the resolved candidate digest;
- digest comparison uses a canonical digest identity key, not raw string
  equality, because existing paths can observe both repo-qualified digests
  such as `image@sha256:...` and bare digests such as `sha256:...`;
- the adapter preserves raw metadata, remote, and inspect digests as evidence,
  but equality and drift checks compare canonical digest keys;
- the canonical digest key is also the input to transition identity
  derivation: expected service rootfs volume IDs, `__netns__` rootfs IDs,
  golden LV IDs, dry-run/apply transaction bindings, superseded-rootfs sets,
  and cleanup references must not be derived from raw digest formatting;
- compatibility rule: canonical keys are used for newly derived target
  identities and digest-bound transaction bindings; observed persisted IDs from
  `ActiveRootfs`, existing transaction records, rootfs metadata, and previous
  runtime state remain exact references and are never silently recomputed;
- superseded and removed cleanup sets are built from observed previous and
  candidate volume IDs, not by recomputing names from digest;
- storage-backed metadata such as `piccolo.volume.json` `base_image_digest` and
  `base_image_ref` is authoritative for this comparison; volume naming alone is
  not authoritative;
- missing `ActiveRootfs`, legacy/unversioned service rootfs, missing rootfs
  metadata, corrupt metadata, unreadable metadata, or digest mismatch makes
  active identity unverifiable;
- unverifiable identity does not silently preserve: if the image can be staged
  from a verified or pinned digest, the plan refreshes the rootfs; if staging
  cannot be made safe, the plan rejects before commit;
- the rootfs identity query or domain adapter exposes this proof, so app flows
  do not parse metadata files or volume names themselves.

The first slice requires a typed rootfs identity query or an equivalent
image/rootfs domain adapter. The result shape must include:

| Field | Meaning |
| --- | --- |
| volume ID | active rootfs volume that was checked |
| base image ref | image ref recorded in rootfs metadata, when present |
| base image digest | image digest recorded in rootfs metadata, when present |
| canonical digest key | normalized digest identity used for equality, drift checks, and derived storage/transaction identities |
| metadata status | present, missing, corrupt, unreadable, unsupported, or mismatch |
| failure reason | redacted explanation for reject/refresh decisions and tests |

For runtime-changing Modify App, every candidate service with an image ref must
have an image/rootfs plan:

| Service shape | Required image/rootfs decision |
| --- | --- |
| new service | pull/inspect image, create staged rootfs, bind digest |
| existing service, image ref changed | pull/inspect image, create staged rootfs, bind digest |
| existing service, same digest-pinned ref | preserve only when active rootfs metadata proves the pinned digest; otherwise stage rootfs from the pinned ref |
| existing service, same mutable ref, remote digest matches proven active rootfs metadata | preserve active rootfs |
| existing service, same mutable ref, remote digest differs from active rootfs | refresh rootfs under same image ref |
| existing service, same mutable ref, remote digest cannot be checked | reject before commit |
| existing service, same mutable ref, active rootfs identity unverifiable | refresh rootfs if remote digest can be checked and staged; otherwise reject |
| removed service | remove from candidate `ActiveRootfs`; retain/cleanup according to transaction and storage policy |
| synthetic runtime entry such as `__netns__` network anchor | plan through the anchor image/rootfs policy below; never infer removal from absence in YAML services |

Network anchor policy:

- `__netns__` is a Piccolo runtime support entry, not an app manifest service;
- its image source is the current daemon's configured network-anchor image,
  such as `networkAnchorImage()`, not the candidate app YAML;
- when a runtime-changing transition recreates the container group, the
  image/rootfs domain must prove the active anchor rootfs matches the current
  anchor image digest, stage a refreshed anchor rootfs, or reject before runtime
  switch;
- missing, corrupt, unreadable, unsupported, or mismatched anchor metadata is
  handled like other unverifiable rootfs identity: refresh if the current anchor
  image can be resolved and staged, otherwise reject;
- dry run and apply bind the anchor source image, resolved digest, and expected
  rootfs identity the same way they bind app-service image/rootfs decisions;
- refreshed or superseded anchor rootfs cleanup is transaction-owned and must
  not be counted as a service addition/removal;
- the UI projection groups the anchor as Piccolo runtime support or keeps it in
  technical details; it never displays `__netns__` as a user app service.

Update Image uses the same image/rootfs identity semantics, but with source
policy set to "current manifest unchanged."

The preferred implementation is a shared image/rootfs planner. A thin adapter is
acceptable only if it is covered by the same conformance tests as Modify App:
mutable digest drift, registry failure, digest-pinned no-op, expected rootfs ID,
active-rootfs identity proof, and fail-closed behavior.

Edit Config uses image/rootfs policy "preserve active rootfs, no registry
check." This is intentional because Edit Config changes values for the current
source rather than advancing image identity. If product requirements change,
that policy must change in the transition boundary, not in the wizard.

### Storage and data

Owner responsibilities:

- reject existing persistent storage mutation/removal unless a future storage
  migration RFC owns it;
- decide whether candidate runtime can touch existing persistent data;
- run snapshot viability before runtime interruption;
- ensure snapshots are taken after quiesce and before candidate writes;
- restore private snapshots only before the runtime commit boundary;
- hide transaction-private snapshots from user rollback after successful commit;
- retain enough transaction metadata for cleanup and recovery.

Any planned rootfs refresh or creation is a runtime/data-affecting transition,
even when the manifest image ref string did not change. If the candidate
runtime can mount and write existing persistent data, the storage/data domain
must require the same transaction-private snapshot safety as an explicit image
ref change. A same-ref mutable digest refresh must therefore reject before
runtime interruption when snapshot viability fails, and must restore the private
snapshot if the candidate touches data and then fails before commit.

The stale/orphaned LV cleanup incident is out of this RFC's first slice, but it
is an example of why storage artifacts must be owned by a transaction domain
rather than by flow-local cleanup guesses.

### Runtime and containers

Owner responsibilities:

- decide metadata-only versus runtime recreation;
- recreate from the candidate active rootfs map, not from ad hoc image refs;
- mark when old runtime is stopped and when candidate runtime can mutate data;
- record container IDs only after candidate runtime reaches the appropriate
  phase;
- keep observed status and task progress aligned with the transaction.

### Listener and access

Owner responsibilities:

- compare listener topology, routing, auth, middleware, and public/remote
  exposure as a domain decision;
- prepare endpoints and reservations before runtime commit where required;
- publish after runtime commit;
- preserve prepared reservations on post-commit publish failure;
- forward-complete or mark access repair after commit;
- rollback prepared-but-uncommitted state before commit.

### UI, task progress, and readiness

Owner responsibilities:

- present operation vocabulary from source provenance and operation kind;
- show an operation policy summary above confirmations: source, image/runtime
  policy, config policy, and apply behavior;
- present each image/rootfs decision as a structured review item with service
  name or runtime entry ID, entry kind, display name/group, display action,
  operator-facing reason, source policy, blocked reason when applicable, and
  optional technical evidence;
- distinguish `app_service` entries from `runtime_anchor` entries; synthetic
  runtime entries such as `__netns__` must not be counted or displayed as app
  service additions/removals;
- show plan groups before confirmations;
- keep Apply disabled until required confirmation IDs are acknowledged;
- report terminal and near-terminal states as structured UI outcomes: blocked
  before apply, rolled back before commit, committed, committed with access
  repair pending, and restore failed/operator action needed;
- refresh app detail after successful mutation or access repair state changes.

## First implementation slice

The first slice fixes the Piclu stale-image/rootfs issue while establishing the
image/rootfs domain boundary.

### Slice scope

In scope for the first implementation:

- introduce an image/rootfs transition plan shape used by runtime-changing
  Modify App;
- make Update Image use the same identity rule or a shared adapter with the
  same semantics;
- make Modify App plan all candidate service image/rootfs identities, not only
  changed image ref strings;
- fail closed when Modify App needs mutable-tag freshness and the registry
  digest cannot be verified;
- bind dry-run and apply to the same resolved digest and expected rootfs volume
  identity;
- require same-ref mutable digest refresh to participate in persistent-data
  snapshot safety whenever candidate runtime can touch existing persistent data;
- show a redacted summary that distinguishes refreshed, staged, reused, and
  removed rootfs decisions;
- add tests for same-ref digest drift, registry verification failure, digest
  drift after dry run, digest-pinned reuse, active-rootfs identity proof, mixed
  multi-service plans, persistent-data snapshot coupling, and Edit Config
  preserving rootfs by explicit policy.

Out of scope for the first implementation:

- migrating all catalog sync and installed config execution to the full
  transition boundary;
- storage migration/removal support;
- stale/orphaned LV discovery and cleanup;
- changing user-initiated rollback semantics;
- adding a new access repair command.

### Required behavior

For runtime-changing Modify App:

- dry run resolves mutable refs for all candidate services;
- dry run compares each resolved digest to authoritative active-rootfs metadata,
  not only the active rootfs volume ID;
- the candidate active-rootfs map includes candidate image services plus
  transition-owned synthetic runtime entries such as the `__netns__` network
  anchor;
- the network anchor rootfs is planned from the current daemon anchor image,
  with preserve/refresh/reject, dry-run/apply binding, and cleanup semantics
  matching the anchor policy above;
- if an existing same-ref mutable tag moved, the plan refreshes that service
  rootfs even though the image ref string is unchanged;
- if active rootfs identity is missing, legacy, corrupt, unreadable, or
  otherwise unverifiable, the plan refreshes from a verified digest or rejects
  before commit;
- if registry resolution fails, dry run rejects and apply cannot start;
- apply recomputes or consumes a bound image/rootfs plan and rejects on digest
  or rootfs identity drift;
- any rootfs refresh for an app whose candidate runtime can touch existing
  persistent data requires snapshot viability before runtime interruption and
  pre-commit data restore on candidate failure;
- app metadata's `ActiveRootfs` after commit contains exactly the planned
  candidate image services plus transition-owned synthetic runtime entries and
  their candidate rootfs IDs;
- removed service rootfs cleanup remains transaction-owned and bounded by
  rollback needs.

For Update Image:

- mutable refs continue to refresh by digest, not by ref-string changes;
- digest-pinned refs remain no-op;
- registry or pull failure fails before runtime switch;
- persistent-data snapshot safety remains a pre-runtime-switch gate.

For Edit Config:

- image/rootfs preservation is explicit and covered by tests;
- no registry check or image pull is added in this slice.

## Site list

### RFCs and docs

- `docs/rfc/20260530-custom-app-manifest-update.md`
  - Previous narrow v1 assumptions are historical context. The new boundary
    supersedes local image-ref equality as the rootfs decision rule for Modify
    App.
- `docs/rfc/20260611-service-app-update-v2.md`
  - Broad service-app update remains the product target. This RFC supplies the
    lifecycle boundary that v2 should route through.
- `docs/rfc/20260616-app-maintenance-ux-overhaul.md`
  - UX vocabulary remains valid. The UI must present transition-plan groups
    instead of backend-local summaries.
- `docs/rfc/20260302-app-snapshot-tuples.md`
  - Tuple/rootfs rollback semantics remain authoritative for existing image
    rollback; transition-private data snapshots must not be exposed as normal
    tuple rollback.

### Backend transition and manifest update

- `internal/app/custom_manifest_update.go`
  - `DryRunCustomManifestUpdate` must obtain image/rootfs decisions from the
    transition image/rootfs domain after candidate render and policy
    classification.
  - `ApplyCustomManifestUpdate` must bind apply to the reviewed image/rootfs
    plan and reject drift.
  - `ManifestUpdateImagePlanItem` or its successor must include enough
    structured information to explain `new`, `changed ref`, `same ref
    refreshed`, `reused`, and `removed` decisions.
  - `stageManifestUpdateRootfs` must consume a plan rather than recomputing a
    local image-ref-diff rule.
  - manifest update transaction fields must continue to record previous and
    candidate active rootfs maps, staged rootfs, created rootfs, removed rootfs,
    resolved image identities, and recovery phases.

- `internal/app/installed_app_apply_transaction.go`
  - The transaction executor remains the first execution boundary. It should
    consume planned rootfs, data snapshot, listener/access, and runtime
    decisions instead of making policy decisions from raw diffs.
  - Recovery markers for runtime switch, runtime touched, access suspended,
    prepared endpoints, and private data snapshots remain authoritative.

- `internal/app/app_manager.go`
  - `UpdateImage` should call the shared image/rootfs planner. If the first
    implementation uses an adapter, it must pass the same conformance suite for
    mutable tag freshness, digest-pinned refs, expected rootfs identity,
    active-rootfs identity proof, persistent-data snapshot coupling, and
    fail-closed errors.
  - `ImageUpdateBlockedReason` remains an app-detail projection of the data
    snapshot preflight, not a separate image identity policy.

- `internal/app/installed_config_update.go`
  - Config update intentionally preserves active rootfs. The plan must name
    this as operation policy so future maintainers do not infer omission.
  - Any runtime recreation caused by config update must use the committed
    active rootfs map, not image ref freshness.

- `internal/app/catalog_sync_apply.go`
  - Low-risk sync can keep its current path initially, but high-risk pending
    review must not bypass the transition boundary when it becomes applyable.
  - Catalog image-only and structural-with-image paths should eventually route
    to the same image/rootfs domain instead of telling the operator to manually
    sequence unrelated operations.

### Install, rootfs, and persistence

- `internal/app/container_group_install.go`
  - Existing install-time cache freshness semantics are evidence for the shared
    image identity rule. Do not duplicate divergent mutable-tag behavior in
    transition code.
  - Digest normalization such as the current `extractDigestHash` behavior must
    be reused or mirrored by the image/rootfs adapter so repo-qualified and bare
    digest strings compare by image identity, not formatting, and derive the
    same rootfs/golden identities.
- `internal/app/rootfs_integration.go`
  - Container creation must continue to prefer `ActiveRootfs` for service-mode
    apps, including the `__netns__` network-anchor entry. Candidate rootfs
    planning must therefore update `ActiveRootfs` before committed runtime
    recreation depends on it.
- `internal/persistence/interfaces.go`
  - The first slice must expose a typed query surface, or an equivalent
    image/rootfs domain adapter, for mapping active rootfs volume ID to
    authoritative image ref/digest metadata without forcing each flow to parse
    volume IDs or metadata files.
- `internal/persistence/rootfs_volume_manager.go`
  - Golden LV and rootfs metadata remain the storage-backed authority for
    flattened image materialization. Any new helper should expose metadata
    rather than duplicating naming assumptions in app flows.

### Listener/access and services

- `internal/services/manager.go`
  - Prepared listener reconcile, reservation, publication, release, and restore
    are transition-domain resources. The full boundary must preserve their
    before/after-commit semantics.
- `internal/app/catalog_sync_apply.go` and `internal/app/custom_manifest_update.go`
  - Existing direct calls to listener reconcile or container recreate must be
    audited as transition-boundary consumers.

### UI/API surfaces

- `internal/server/gin_app_handlers.go`
  - Manifest dry-run/apply responses must carry transition-plan review fields
    and stable confirmation IDs.
- `docs/api/openapi.yaml`
  - Any new dry-run/apply fields must be documented.
- `ui/lib/core/models/app_models.dart`
  - Typed models must parse transition-plan fields, operation policy summary,
    image/rootfs review items including entry kind/display group, terminal
    outcome fields, dry-run token, base hash, runtime fingerprint, and stable
    confirmation IDs.
- `ui/lib/core/services/app_service.dart`
  - Service calls must preserve dry-run token/base hash/runtime fingerprint and
    submit only acknowledged confirmation IDs; the service layer must not drop
    new review fields before the wizard can render them.
- `ui/lib/features/apps/manifest_update_wizard.dart`
  - Modify App must show image/rootfs decisions from the plan, including same
    ref refresh, registry verification failures, active-rootfs unverifiable
    refresh/reject outcomes, per-item operator-facing reasons, and runtime
    anchor decisions grouped away from user app services.
- `ui/lib/features/apps/installed_config_wizard.dart`
  - Edit Config must not imply image freshness; it presents preserve-rootfs
    semantics for config-only operation policy.
- `ui/lib/features/apps/app_detail_view.dart`
  - Update/Modify/Edit entry points must route to the operation whose
    transition policy matches the user's intent.
- `ui/lib/features/apps/app_operation_lifecycle.dart`
  - Progress and readiness copy must distinguish update commit success from
    access repair follow-up.

### Tests and validation

- `internal/app/custom_manifest_update_test.go`
  - Add Modify App image/rootfs planning tests for same-ref digest drift,
    registry failure, apply-time digest drift, digest-pinned reuse, missing
    `ActiveRootfs`, legacy/unversioned rootfs, missing/corrupt rootfs metadata,
    mixed preserve-plus-refresh plans, refresh-plus-removed-service cleanup, and
    same-ref refresh with persistent-data snapshot viability/restore.
  - Add digest-normalization tests proving bare and repo-qualified forms compare
    equal for app-service rootfs preservation and drift detection, and produce
    the same expected rootfs/golden identities, transaction bindings, and
    cleanup references.
  - Add compatibility tests for an existing raw-derived app-service rootfs whose
    metadata canonical key matches: preserve keeps the observed ID, refresh
    creates the canonical target ID, and cleanup references the actual
    superseded observed ID.
  - Add tests proving `__netns__` network-anchor rootfs is preserved as a
    synthetic runtime entry and is not treated as a removed YAML service.
  - Add tests for refreshed anchor rootfs when the daemon anchor image digest
    changes, unverifiable anchor metadata, apply-time anchor digest drift, and
    transaction-owned cleanup of superseded anchor rootfs.
  - Add anchor digest-normalization tests proving bare and repo-qualified forms
    compare equal for `__netns__` and produce the same expected anchor rootfs
    identity and cleanup references.
  - Add compatibility tests for an existing raw-derived `__netns__` rootfs whose
    metadata canonical key matches: preserve keeps the observed ID, refresh
    creates the canonical target ID, and cleanup references the actual
    superseded observed ID.
- `internal/app/update_image_test.go`
  - Preserve Update Image mutable-tag refresh and digest-pinned no-op behavior,
    and share the conformance matrix for registry failure, rootfs identity
    proof, expected rootfs identity, and persistent-data snapshot coupling.
- `internal/app/installed_config_update_test.go`
  - Cover explicit active-rootfs preserve policy for config update.
- Backend/API tests
  - Reject manifest source submitted through config policy, catalog
    manifest-review source submitted as config, and forged/mismatched policy
    hints from API/UI callers.
- `internal/services/*_test.go`
  - Full transition work must keep listener prepare/publish repair tests green;
    first slice should not alter listener behavior except through rootfs plan
    inputs.
- `ui/test/*`
  - Add or update tests only if the first slice changes visible plan copy or
    confirmation gating.
- `scripts/alpha/*`
  - Use alpha validation for the first slice once unit tests pass, preferably
    with an app whose mutable tag can be republished or simulated, and record
    image digest/rootfs evidence before and after Modify App.

## Completeness audit

### Q1. Surface area

The site list above is the Q1 surface. The first implementation slice must
touch only the image/rootfs subset unless tests prove a coupled site needs an
adapter or explicit preserve policy.

### Q2. Required behavior at each site

- Manifest update sites: consume image/rootfs plan, do not use image-ref string
  equality as a rootfs decision.
- Update Image sites: share or conform to the same mutable-tag identity rule.
- Config update sites: explicitly preserve active rootfs and avoid registry
  access by operation policy.
- Boundary entry sites: derive legal lifecycle policy from operation kind and
  candidate source; caller-supplied policy hints are non-authoritative.
- Catalog sites: do not silently apply high-risk source changes through a
  lower-risk config path.
- Rootfs/persistence sites: expose authoritative image/rootfs metadata instead
  of forcing feature flows to guess from naming conventions where avoidable.
- Digest comparison sites: compare canonical digest identity keys while
  preserving raw digest strings for review/debug evidence; every derived
  new target rootfs/golden/transaction identity uses the canonical key, while
  observed persisted IDs remain exact references for preservation and cleanup.
- Synthetic runtime entries: `ActiveRootfs` includes YAML image services plus
  transition-owned entries such as `__netns__`; absence from YAML service keys
  does not by itself make such entries removed.
- UI/API sites: display plan semantics and confirmation requirements without
  leaking secrets or raw rendered YAML.
- UI/API image/rootfs entries: carry entry kind and display grouping so
  synthetic runtime entries such as `__netns__` cannot be presented as user app
  services.
- Recovery sites: prefer durable transaction plan state over inferred partial
  resources.

### Q3. Invariants preserved

- App identity and primary listener identity remain pinned by source rendering.
- Stored secrets are reused by backend policy without browser display.
- Existing persistent storage is not mutated or removed by this RFC.
- Candidate runtime does not reach public traffic before the runtime/data commit
  boundary.
- Digest-pinned image refs do not get refreshed accidentally.
- Mutable image refs cannot be treated as fresh solely because their string did
  not change.
- Active rootfs cannot be preserved from naming convention alone; preservation
  requires authoritative metadata proof.
- Rootfs digest equality cannot depend on raw string formatting; bare and
  repo-qualified forms of the same digest are the same identity and must produce
  the same newly derived storage and transaction identities.
- Existing persisted volume IDs are authoritative references. Preservation and
  cleanup use observed IDs, not recomputed IDs.
- Synthetic runtime rootfs entries such as `__netns__` remain planned runtime
  state, not service-removal cleanup candidates.
- Network-anchor rootfs freshness is planned against the current daemon anchor
  image and bound through dry run/apply when runtime recreation depends on it.
- A same-ref mutable digest refresh is data-safety equivalent to an explicit
  image/rootfs refresh.
- Dry-run review binds to apply; drift rejects rather than silently changing
  the reviewed operation.
- Unknown or unsupported pending flow fails closed.
- Normal reconcile does not overrule a pending transaction.

## Review and implementation plan

1. Review this RFC with the RFC cadence:
   - architecture/RFC review;
   - red-team review;
   - UX review only for plan fields that surface to operators;
   - minimizer after convergence.
2. If review converges without a new architectural fork, implement the first
   image/rootfs slice.
3. Run targeted Go tests for manifest update, image update, config update, and
   any rootfs planner helpers.
4. Run UI tests only if visible review fields or confirmation behavior change.
5. Run alpha validation through `scripts/alpha/*` when the local tests prove
   the code path and an alpha app can exercise same-ref mutable tag refresh.
6. Run the full code review cadence before commit or publish.

Stop and ask for a product/architecture decision if review or implementation
reveals any of these forks:

- Edit Config should refresh mutable images when it recreates runtime;
- catalog sync should auto-apply a high-risk source update;
- storage mutation/removal is required for the stale-image fix;
- registry-unavailable fallback should be allowed for Modify App;
- the first slice requires replacing the whole manifest update transaction
  instead of introducing the image/rootfs domain boundary.

## Implementation Notes & Status

RFC review cadence converged without a new architectural fork. The current
branch implements the first image/rootfs slice for runtime-changing Modify App:
same-ref mutable digest drift is planned and refreshed, registry freshness
failure rejects before commit, dry-run/apply bind to canonical digest and
rootfs identity, digest-pinned refs use the same identity proof, and the
synthetic `__netns__` network-anchor rootfs is planned as Piccolo runtime
support. Validation and code-review cadence are still in progress.
