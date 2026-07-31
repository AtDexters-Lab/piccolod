# v0.2.43 runtime-lifecycle remediation plan

**Status:** Accepted on 2026-07-29 for slice-by-slice implementation; Slice 1
implementation and review are complete, with alpha/device validation pending.

This plan is governed by
`docs/incidents/2026-07-v043-runtime-lifecycle-context.md`. If this plan and
that decision ledger disagree, the ledger wins.

## Scope

### Problem

Piccolod `v0.2.43` put reconstructible rootfs maintenance in the mandatory
unlock path and allowed an unbounded Podman/runc teardown to hold the global
app-lifecycle owner. On the affected device this caused unlock liveness
restarts and left one uninstall blocking unrelated app operations.

### In scope

- Bound core unlock and move reconstructible rootfs settlement/GC after
  `Ready`.
- Bound external runtime teardown and prove per-app process containment before
  destructive cleanup.
- Make confirmed uninstall intent durable in the existing per-app transition
  record and retry it across daemon restart.
- Keep removal-pending apps, terminal admission, capabilities, OIDC clients,
  API responses, and UI behavior consistent with that transition.
- Remove the public manual-lock endpoint.
- Produce a read-only ownership audit for the observed approximately 350 LVs.

### Out of scope

- Post-update fallback recovery UI, automatic rollback, or changes to the
  separate post-update recovery RFC.
- Retention-policy changes, automatic orphan-LV deletion, or a permanent
  storage-diagnostics API.
- Podman, runc, conmon, or passt package replacement without new evidence.
- Per-app lifecycle parallelism, a lifecycle scheduler, or a waiter registry.
- Durable event delivery, an outbox, full event sourcing, or a broad OIDC
  desired-state rewrite.
- Manifest-configurable stop grace, changes to the 30-second graceful stop, or
  changes to the 30-second core-unlock guard.

## Required outcomes

1. A correct password reaches `Ready` without waiting for reconstructible
   rootfs settlement or GC.
2. Rootfs maintenance retries indefinitely after `Ready`, but each attempt is
   finite, cancellable, and performs at most one broad LV inventory.
3. No external runtime command can hold the global lifecycle owner without a
   Piccolod-enforced deadline.
4. A confirmed uninstall survives cancellation, failure, daemon restart, and
   reboot until physical cleanup is proven.
5. A failed uninstall remains visible, disabled, and retryable; it never
   restarts Piccolod or the host.
6. User data is guaranteed retained before the durable purge fence. After that
   fence the backend and UI call the operation finalizing and do not promise
   recovery.
7. A removal-pending app cannot regain runtime, ingress, terminal, OIDC, or
   capability authority through a concurrent or replayed path.
8. The app ID remains reserved and the app remains visible until physical
   cleanup completes.
9. Shutdown cancels and joins background maintenance and the active lifecycle
   owner before persistence is detached.

## Existing authorities to reuse

The repair adds no second removal journal or global cleanup queue.

- `app_transition_v2.json` remains the per-app durable multi-step authority.
- `AppManager`'s existing 30-second reconciliation loop owns automatic
  transition replay. The existing finish-cleanup action supplies `Retry now`.
- The existing global `reconcileMu` boundary remains global; only its
  acquisition contract changes.
- Existing golden identity locks serialize physical settlement and foreground
  exact-golden reuse.
- The per-app user manager, cgroup, and numeric UID remain the runtime
  containment boundary.
- `TerminalManager`, the retained OIDC client manager, and the capability
  manager continue to own their respective resources.
- List/get responses are authoritative. SSE events remain wake notifications
  that cause clients to refetch.

## Design

### 1. Unlock and golden/rootfs maintenance

Split current rootfs reconciliation into two contracts:

- **Metadata hydration:** load and validate the golden image-reference,
  digest, and identity mapping needed by `FindGoldenByImageRef`. It performs no
  mount, snapshot, LV settlement, GC, or broad physical discovery and remains
  in the bounded unlock chain.
- **Physical maintenance:** after lifecycle `Ready`, collect one strict LV
  inventory for the pass, supply that snapshot to all golden records, settle
  ready golden records under the existing identity-local lock, and run scoped
  golden/rootfs GC. Generic orphan-LV deletion does not run automatically;
  unknown LVs remain read-only audit evidence.

`onUnlockChainReady` nudges one GinServer-owned maintenance worker. The worker
uses a finite context per attempt, records a Warn health detail on failure,
and retries forever with capped backoff while the server remains unlocked.
Another unlock may nudge the same worker; it does not create a second
concurrent pass. Lock or shutdown cancels and joins the worker before
persistence teardown.

Foreground install may still reuse an exact local golden. Metadata cache
membership is only a candidate: the existing identity lock and a strict
targeted content/LV proof must succeed before physical reuse. Foreground reuse
and background settlement of the same identity therefore serialize without a
global handoff semaphore. Maintenance does not wait indefinitely for a busy
identity lock: it skips that identity and retries it in a later pass.

The LVM inventory parser fails closed on malformed rows and duplicate exact
identities. A pass supplies its single snapshot to all golden records rather
than calling `LVExistsExact` per record. That snapshot is physical discovery,
never deletion authority: scoped golden GC re-reads current rootfs/artifact
references under the exact golden identity lock immediately before
destruction. A busy identity is retained for a later pass.

### 2. Global lifecycle admission and shutdown

Replace direct use of `sync.Mutex` with a capacity-one gate exposing
context-aware acquire, non-blocking try-acquire, and release. Every current
`reconcileMu` owner migrates to that gate; global serialization is unchanged.

An HTTP mutation supplies its request context only for gate admission. A
disconnect cancels the mutation while it is queued. Once admitted, the gate
consumes that request-cancellation marker and the operation, including bounded
compensation and tail work, continues under finite server-owned contexts.
Operation deadlines and server shutdown still cancel that work. This contract
applies uniformly to uninstall and non-uninstall mutations.

For uninstall, once durable intent is stored, an operation deadline or server
shutdown can stop the current finite attempt but cannot cancel the durable
operation. The existing transition reconciler retries it later.

Automatic reconciliation uses a timer measured from completion of the prior
pass, not a catch-up ticker. A failed transition attempt releases the gate and
waits at least the existing 30-second interval before automatic retry. This
gives queued foreground and shutdown work a real admission window without a
scheduler, waiter registry, or durable retry counter. `Retry now` may bypass
that wait only through an explicit user request. One automatic gate admission
dispatches at most one uninstall phase attempt and returns before another
transition or ordinary reconciliation. Ordinary reconciliation uses a
separate admission and yields when foreground or shutdown work owns the gate.

On shutdown, GinServer first fences new HTTP work and cancels normal operation
and background roots. `StopAllApps` then acquires the same gate using the
bounded drain context. That acquisition joins any admitted owner before app
containment or persistence detach. No registry of callers or queued waiters is
introduced. If the join deadline expires, shutdown must not run `StopAllApps`,
detach persistence, unmount/lock its volumes, or report a clean drain. It
leaves persistence mounted, records and returns an explicit shutdown failure,
and lets the service manager apply its outer termination policy. The same
no-detach result applies after successful gate acquisition if `StopAllApps`
returns any error, an app lacks a complete quiescence proof, or the drain
deadline expires.

Shutdown is also removal-phase-aware. An app in `uninstall_pending` or
`uninstall_containing` receives containment-only handling. An app in
`uninstall_finalizing` or `uninstall_identity_retiring` must never pass through
ordinary ensure, attach, restore, or publication paths that can recreate an
already-removed resource; shutdown only proves containment and detaches
resources that are still observed present.

### 3. Bounded runtime teardown

Keep the existing container grace request of 30 seconds, but give the Podman
command a fixed Piccolod hard deadline of 45 seconds. Run it in its own process
group. When that deadline expires, terminate the command process group, reap
it, and return a typed timeout; the timeout must not target Piccolod's process
group.

The same command-ownership rule applies to every admitted lifecycle owner, not
only uninstall. Runtime observation/control commands such as inspect, start,
stop, remove, and storage reset use a Piccolod-enforced hard deadline no longer
than 45 seconds or the caller's remaining finite budget, whichever is sooner.
Long-running transfer or materialization commands keep their existing
operation-specific finite budget. No external runtime command may replace its
owner with a background or otherwise unbounded context while the global gate
is held.

Every uninstall entry path—initial HTTP work, startup recovery, automatic
reconcile, and `Retry now`—runs one phase-dispatch attempt with a two-minute
overall deadline. Runtime observation/removal, Podman storage reset,
containment, ingress/accelerator/OIDC cleanup, rootfs/artifact cleanup, volume
cleanup, slice-policy removal, and identity retirement all consume that same
finite attempt context. Each spawned command also has its own hard bound no
longer than the remaining attempt budget. Deadline or cancellation records the
latest error when persistence remains available, releases the global gate,
and leaves the durable phase for a later attempt.

The existing service-publication owner exposes a strict context-aware
app-removal operation for uninstall. Firewall, proxy, publication-withdrawal,
and endpoint cleanup use the attempt context or a shorter child deadline and
return failure to the transition; they must not replace it with a background
context. Existing best-effort callers may retain their current wrapper, but
uninstall cannot advance while publication absence remains unproven.

After a Podman subcommand returns or times out, the caller enters the existing
per-app user/session containment path using a new child context derived from
the admitted attempt context. Its deadline is no later than the attempt's
remaining two-minute deadline; “new” does not create an independent execution
owner. Destructive volume, rootfs, artifact, or identity cleanup proceeds only
after both of these are proven:

- the app's delegated cgroup contains no processes; and
- no process runs under the app's recorded numeric UID.

The uninstall transition persists its containing phase before the first
graceful Podman stop. The initially admitted attempt may try that graceful
stop once. Replay of the containing phase skips Podman and goes directly to
the containment proof, preventing repeated creation of stuck `runc kill`
processes.

### 4. Durable uninstall in the existing transition

Add `uninstall` to `TransitionOperationKind`. Store the app's trusted numeric
UID in `TransitionResources` before any destructive effect. The record is also
the app-ID reservation; no second cleanup record or tombstone is added.

Accepted phases and fences:

| Durable phase | Meaning | Data promise | Permitted next work |
| --- | --- | --- | --- |
| `uninstall_pending` | Intent accepted; app disabled and all new authority fenced | Retained | Provider handoff, terminal drain |
| `uninstall_containing` | Terminal drain completed; runtime containment has begun | Retained | One initial graceful stop, or replay directly to containment proof |
| `uninstall_finalizing` | Runtime and non-data resources are absent; purge fence crossed | May already be gone | App data/rootless storage cleanup and strict absence proof |
| `uninstall_identity_retiring` | Data and owned resources are absent; recorded identity is being retired | Gone | Delete/prove account absent and atomically retire app publication |

There is no durable completed record. Completion atomically renames the app
directory out of the active namespace and removes it from the in-memory app
publication; best-effort deletion of that already-retired directory is not a
second product state.

The transition advances only after the effect owned by the current phase is
confirmed. `LastError` records the latest finite attempt failure. Automatic
reconcile and `Retry now` both invoke the same phase dispatcher.

Before entering `uninstall_pending`, the handler validates any required
capability-provider acknowledgement, resolves and stores a trusted app UID,
and asks TerminalManager to block new sessions. If the transition write fails,
terminal admission is unblocked and no removal intent is reported. If trusted
identity cannot be established, the app remains installed and no privileged
destruction starts.

During unlock, a metadata-only scan reconstructs active uninstall fences
before lifecycle `Ready`, app restoration, background reconciliation, or
app-scoped terminal/OIDC/capability admission. An active uninstall transition
is sufficient to make the app disabled/ineligible even if a crash occurred
before `Enabled=false` was persisted. Physical recovery remains post-Ready.

The transition filename itself is a conservative app-local fence before its
contents are parsed. If a present record cannot be read or validated, the app
directory and ID remain reserved, all runtime/ingress/terminal/OIDC/capability
authority stays denied, and no destructive cleanup runs. The device may still
reach `Ready`; list/get derive a disabled transition-recovery error from the
app directory rather than inventing another durable record. Reconciliation
retries the authoritative record indefinitely and resumes normal phase
dispatch only after it becomes valid.

After intent commits:

1. persist `Enabled=false`, remove runtime/ingress eligibility, and reconcile
   capability replacement or unavailability;
2. close and join terminals;
3. persist `uninstall_containing`, attempt bounded runtime stop once, and prove
   per-app containment;
4. strictly and idempotently remove app-owned ingress, accelerator, OIDC,
   service rootfs, artifact references, and systemd slice policy while user
   data is retained;
5. persist `uninstall_finalizing`, then remove the data volume and rootless
   storage with absence proof;
6. persist `uninstall_identity_retiring`, delete or prove absent the recorded
   app identity without acting on a reused UID, then atomically retire the app
   publication.

Every destructive identifier is derived from or checked against the current
app definition and recorded app identity. A foreign rootfs, artifact, volume,
or UID fails closed. Rootfs teardown returns errors to the transition; global
golden GC is left to post-Ready maintenance. Volume metadata is removed only
after detach and LV absence are proven.

Slice-policy reconciliation treats any active uninstall transition as
ineligible. Strict policy removal uses the recorded UID, shares the existing
slice-policy serialization with the periodic pass, and must succeed before
identity retirement. A periodic pass that started first may finish, but
uninstall then removes its result under the same serialization; later passes
skip the removal-pending app.

### 5. Terminal, capability, and OIDC ownership

#### Terminal

TerminalManager keeps an in-memory blocked-app set and tracks in-flight
session creation. `BlockApp` closes the same-process start-before-intent gap.
`DrainApp(ctx, appID)` cancels in-flight creation, closes existing sessions,
and joins them. An unfinished drain remains retryable; AppManager does not
snapshot PTYs or receive terminal lifecycle callbacks.

Startup transition recovery re-establishes the block before replaying cleanup.
Terminal events may prompt cleanup, but correctness does not depend on event
delivery. The pre-Ready transition-fence scan establishes this block before
any terminal create can be admitted after restart.

#### Capability

Provider eligibility is derived from current durable app state. Any active
uninstall transition excludes that app from provider lists, selection,
ingress, and binding. Reconciliation persists the deterministic eligible
replacement or reports unavailable/reconciling. It does not persist an
old-to-new delta and never restores the removing provider because a side
effect failed.

#### OIDC

GinServer retains one OIDC `ClientManager` rather than constructing a
lightweight manager per handler call. App-scoped client mutations use that
owner. Its idempotent ensure-absent operation serializes with same-app client
creation and rechecks uninstall eligibility before a create.

Uninstall cannot advance beyond non-data resource cleanup until all app-scoped
clients are confirmed absent. Install-time pre-registration remains explicit;
this plan does not move it behind events or redesign OIDC desired state.

### 6. Backend/UI contract and manual lock

App list/get decorates the existing transition projection with one
backend-derived removal view:

- `pending` for the pending and containing phases;
- `finalizing` for finalizing and identity-retiring;
- latest attempt error;
- whether an attempt is currently active;
- whether `Retry now` is currently available;
- the phase-derived data-retention statement.

The projection is not persisted separately. A failed removal app stays on the
Stage, disabled, with its removal state and retry action. It is not displayed
as normally stopped. The Stage tile remains selectable into read-only removal
details, while Open, Start, Stop, Update, Rollback, Terminal, and Uninstall are
suppressed. An active attempt shows `Removing...` or `Retrying...` and no retry
action. An idle failed attempt shows the latest error and one `Retry now`;
the same state says, “Removal will retry automatically. Retry now starts
another attempt immediately.” Pressing it immediately disables the action
until the authoritative refetch.
Only final publication retirement removes the tile/detail.

The Stage app list, detail view, and capability view refetch their
authoritative endpoints after coalesced app/capability SSE wakes, on stream
connect/reconnect, and when the screen becomes active. This closes missed-wake
gaps without a removal-specific poller, client epoch, durable event, or
client-side transition state machine. Coalescing has a trailing-edge guarantee:
a wake received while a refetch is in flight schedules exactly one additional
authoritative refetch after that request settles. While one of those screens
remains active, one lightweight 30-second timer revalidates its authoritative
projection so a dropped wake cannot leave it indefinitely stale.

Confirmed uninstall returns `202 Accepted` once durable intent commits. The
request may drive one finite attempt, but HTTP lifetime is not the operation
owner. The existing finish-cleanup endpoint becomes the `Retry now` action for
uninstall phases. The app disappears only after atomic publication retirement.

The confirmation leads with the permanent consequence: uninstall permanently
removes the app and deletes all of its data. It then states that the operation
is non-cancellable, continues across reboot, keeps data present only while the
status is `pending`, and has begun irreversible deletion once `finalizing`
appears. Pending retention is explicitly not a cancellation or recovery
promise.

If the app is the selected capability provider, the dialog also renders the
existing server-provided provider-change disclosure: dependent requests may be
interrupted and provider-owned configuration, models, indexes, history, and
other state are not migrated. Missing disclosure fails closed. Confirmation
submits the existing acknowledgement; a selection race that returns
confirmation-required causes a capability refetch and a fresh confirmation,
never silent acknowledgement. After acceptance the UI refetches and shows the
authoritative replacement or unavailable/reconciling result.

Remove `POST /crypto/lock` from routing, handler, OpenAPI, and public endpoint
tests. Keep lifecycle/storage lock primitives used by shutdown and recovery.
The release/migration note names this as an intentional breaking API change and
directs intentional offline operation to power-off/reboot rather than a
replacement lock API.

### 7. Read-only LV ownership audit

Add an incident runbook, not an API. It captures one read-only LV inventory and
classifies each LV by exact app/rootfs/golden/artifact/snapshot reference,
record age, and the existing retention reason. Unknown and duplicate ownership
remain findings; the audit performs no deletion and makes no retention-policy
change.

## Temporal composition

### Execution ownership

| Work | Durable authority | Execution owner | Retry trigger |
| --- | --- | --- | --- |
| Core unlock | lifecycle/persistence state | unlock coordinator | explicit/automatic unlock |
| Golden physical maintenance | golden metadata | one GinServer worker | Ready nudge and capped timer |
| App uninstall | per-app transition record | admitted AppManager attempt | 30 seconds after prior pass completes, startup recovery, Retry now |
| Terminal drain | active uninstall transition | TerminalManager called by AppManager | same uninstall attempt |
| Capability handoff | app transition plus stored current default | capability owner | same attempt and existing reconcile |
| OIDC absence | active uninstall transition | retained OIDC client owner | same uninstall attempt |

Events in this table are optional latency optimizations; durable state and
resource observation decide correctness.

### Adversarial sequences

- Disconnect before lifecycle admission: queued request cancels and no durable
  effect occurs.
- Disconnect after uninstall intent commit: current attempt may stop; the
  visible transition remains and automatic replay resumes it.
- Shutdown while an app mutation owns the gate: new work is fenced, owner
  context is canceled, drain acquisition joins its release, then containment
  starts.
- Shutdown join times out: app containment and persistence detach are skipped,
  mounted state is preserved, and shutdown returns failure.
- A failed automatic uninstall attempt outlasts the normal interval: the next
  interval starts only after release, so queued foreground/shutdown work can
  acquire the gate.
- Crash after graceful stop begins: replay sees `uninstall_containing` and
  does not invoke Podman stop again.
- Crash after uninstall intent but before `Enabled=false`: the pre-Ready fence
  scan excludes the app from restoration and all app-scoped admission.
- A present uninstall transition is unreadable: its directory/ID remains
  reserved and app authority stays fenced, while unrelated apps and device
  `Ready` continue; later reconciliation retries the read without destructive
  inference.
- Crash immediately before or after the purge fence: the stored phase is the
  sole source of the UI data promise; work before finalizing cannot purge data.
- Crash after account deletion: `uninstall_identity_retiring` verifies the
  recorded account/cgroup state without signaling a newly reused UID.
- Capability replacement effect fails: the removing provider remains
  ineligible; backend reports unavailable/reconciling and retries.
- OIDC create races uninstall: same-app OIDC serialization plus a final
  transition eligibility check makes ensure-absent win before uninstall can
  advance.
- Service-publication withdrawal stalls: the same finite uninstall attempt
  expires, records the error without advancing phase, and releases the global
  lifecycle owner.
- Shutdown overlaps a finalizing uninstall: containment is proven without
  ensuring, attaching, restoring, or republishing already-removed resources.
- Foreground golden reuse races maintenance: both serialize on exact golden
  identity; GC refreshes references under that lock and cache/inventory
  snapshots alone never authorize deletion.
- Maintenance encounters a busy golden identity: it skips that identity and
  remains cancellable; a later pass retries.
- Maintenance fails or is canceled: lifecycle remains `Ready`; Warn persists
  and the next finite attempt retries.
- UI misses an SSE wake: connect/reconnect or screen activation refetches the
  authoritative removal and capability projection. A wake during an in-flight
  fetch causes one trailing refetch, so coalescing cannot indefinitely preserve
  the earlier response; the 30-second active-screen revalidation repairs a wake
  dropped before it reaches the client.

## Implementation slices and affected sites

The following is the final affected-site list for review. Discovering a
required runtime site outside it is a stop condition requiring plan update
before editing. Adjacent tests may be added beside an accepted production
site.

### Slice 1: bounded unlock, physical maintenance, and manual-lock removal

- `internal/persistence/interfaces.go`
- `internal/persistence/rootfs_volume_manager.go`
- `internal/persistence/rootfs_volume_manager_test.go`
- `internal/persistence/golden_content.go`
- `internal/persistence/golden_content_test.go`
- `internal/storage/lvm/volume.go`
- `internal/storage/lvm/lvm_test.go`
- `internal/app/container_group_install.go`
- `internal/app/container_group_install_test.go`
- `internal/server/gin_server.go`
- `internal/server/recovery_execution_test.go`
- `internal/server/gin_crypto_handlers.go`
- `internal/server/gin_emergency_handlers_test.go`
- `docs/api/openapi.yaml`
- `docs/api/openapi_validation_test.go`
- `docs/api/manual-lock-removal.md`

### Slice 2: lifecycle gate, bounded runtime, and shutdown join

- `internal/app/app_manager.go`
- `internal/app/capabilities.go`
- `internal/app/catalog_sync.go`
- `internal/app/catalog_sync_apply.go`
- `internal/app/custom_manifest_update.go`
- `internal/app/installed_config_update.go`
- `internal/app/installed_config_update_test.go`
- `internal/app/recovery_owner.go`
- `internal/app/recovery_owner_test.go`
- `internal/app/storage.go`
- `internal/app/container_group_lifecycle.go`
- `internal/app/podman_runtime.go`
- `internal/container/podman.go`
- `internal/container/podman_test.go`
- `internal/container/appuser.go`
- `internal/container/appuser_test.go`
- `internal/server/gin_server.go`
- `internal/server/gin_app_handlers.go`
- `internal/server/gin_app_handlers_test.go`

### Slice 3: durable uninstall and strict physical cleanup

- `internal/app/installed_app_transition.go`
- `internal/app/installed_app_transition_test.go`
- `internal/app/app_manager.go`
- `internal/app/app_manager_test.go`
- `internal/app/recovery_owner.go`
- `internal/app/recovery_owner_test.go`
- `internal/app/filesystem.go`
- `internal/app/filesystem_test.go`
- `internal/app/rootfs_integration.go`
- `internal/app/rootfs_integration_test.go`
- `internal/app/artifact_materialization.go`
- `internal/app/slice_policy.go`
- `internal/app/slice_policy_test.go`
- `internal/app/container_group_lifecycle.go`
- `internal/app/podman_runtime.go`
- `internal/persistence/luks_volume_manager.go`
- `internal/persistence/luks_volume_manager_test.go`
- `internal/services/manager.go`
- `internal/services/manager_publication_test.go`

### Slice 4: narrow resource-owner integration

- `internal/terminal/manager.go`
- `internal/terminal/manager_test.go`
- `internal/server/gin_terminal_sessions.go`
- `internal/app/capabilities.go`
- `internal/app/capabilities_test.go`
- `internal/app/capability_ingress.go`
- `internal/app/capability_ingress_test.go`
- `internal/server/gin_capability_handlers.go`
- `internal/server/gin_capability_handlers_test.go`
- `internal/oidc/client.go`
- `internal/server/gin_oidc_handlers.go`
- `internal/server/gin_oidc_handlers_test.go`
- `internal/server/gin_app_handlers.go`
- `internal/server/catalog_sync_host.go`
- `internal/server/gin_server.go`

### Slice 5: uninstall API, UI, and audit

- `internal/app/types.go`
- `internal/app/app_manager.go`
- `internal/server/gin_app_handlers.go`
- `internal/server/gin_app_handlers_test.go`
- `internal/server/gin_server.go`
- `docs/api/openapi.yaml`
- `docs/api/openapi_validation_test.go`
- `ui/lib/core/models/app_models.dart`
- `ui/lib/core/models/app_status_event.dart`
- `ui/lib/core/services/app_service.dart`
- `ui/lib/shells/desktop/widgets/stage.dart`
- `ui/lib/features/apps/app_detail_view.dart`
- `ui/lib/shared/widgets/uninstall_confirmation_dialog.dart`
- `ui/lib/features/apps/widgets/capability_provider_card.dart`
- `ui/test/app_models_test.dart`
- `ui/test/app_detail_removal_state_test.dart`
- `ui/test/capability_provider_card_test.dart`
- `ui/test/stage_removal_state_test.dart`
- `ui/test/uninstall_confirmation_dialog_test.dart`
- `docs/incidents/2026-07-v043-lv-ownership-audit.md`

## Verification gates

Each slice is reviewed and landed separately. Later slices may consume only
the accepted contracts above.

1. **Go correctness:** focused unit tests per slice, `go test -race` for
   lifecycle/transition/resource-owner packages, then `go test ./...`.
2. **Flutter correctness:** focused model/widget tests, `flutter analyze`, then
   the relevant UI suite.
3. **Unlock regression:** representative large golden metadata and LV
   inventory reaches `Ready` comfortably inside 30 seconds; physical
   golden/rootfs maintenance is observed only after Ready; command count proves
   one broad inventory per pass shared by all golden consumers. A stale
   inventory cannot delete a newly referenced golden, and unknown/generic
   orphan LVs are never deleted by the pass. Cancellation while an identity is
   busy joins promptly. The public manual-lock route, handler, and OpenAPI
   operation are absent, the authenticated route returns 404, and the migration
   note directs intentional offline operation to power-off or reboot.
4. **Runtime failure injection:** a Podman/runc stop that never returns reaches
   the local deadline, releases the global gate, creates no repeated stop
   processes on replay, and never restarts Piccolod or the host. The same test
   applies to every external teardown command and proves the complete
   two-minute attempt bound. A stuck inspect/start/stop command during ordinary
   reconciliation also reaches its local deadline and releases the gate.
5. **Crash matrix:** restart immediately before and after every uninstall phase
   write and destructive effect; the app remains visible until completion,
   data claims stay truthful, and app ID/UID ownership cannot be reused
   unsafely. A crash between intent and `Enabled=false` cannot restore runtime,
   terminal, OIDC, or capability authority. A present but unreadable transition
   record keeps that app locally fenced and visible as recovery-blocked without
   preventing lifecycle `Ready` or authorizing destructive inference.
6. **Authority races:** terminal create versus uninstall, OIDC create versus
   ensure-absent, capability replacement failure, client disconnect while
   queued, shutdown while admitted, slice-policy reconcile versus removal, and
   golden reuse versus maintenance. A non-releasing admitted owner must prevent
   `StopAllApps` and persistence shutdown rather than permitting unsafe detach;
   after successful gate acquisition, one app's failed quiescence proof must
   likewise prevent persistence shutdown. Publication teardown must obey the
   finite attempt context, and shutdown overlapping finalizing removal must not
   recreate, attach, restore, or republish an already-removed resource.
7. **Alpha VM/device:** install two unrelated apps, inject a stuck teardown in
   one, confirm the other is usable after the finite attempt releases, reboot
   mid-removal, observe convergence, and repeat unlock with representative
   storage population. A retry attempt longer than 30 seconds must still yield
   a full interval and admit unrelated foreground work before retry. With
   multiple pending transitions, one automatic admission performs at most one
   attempt and releases before another transition or ordinary reconciliation.
8. **LV gate:** review the read-only ownership classification before proposing
   any retention or deletion change.
9. **Review closure:** independent code-quality review against this RFC and the
   incident ledger after every slice, with security review for slices changing
   privileged cleanup, UID ownership, terminal admission, or OIDC authority.
   No review finding may expand a locked product decision without returning to
   the user.
10. **Removal UX:** widget tests cover permanent-delete and selected-provider
    disclosures, normal-action suppression, active versus failed retry states,
    automatic-retry disclosure beside the latest error and `Retry now`,
    missed-wake reconnect refetch on Stage/detail/capability views, Stage
    disappearance after authoritative list refetch, and disappearance only
    after authoritative publication retirement. They also prove one trailing
    refetch after a wake arrives during an in-flight fetch and convergence
    within one 30-second active-screen revalidation after a wake is dropped.
    OpenAPI validation covers the `202` response, acknowledgement input,
    removal fields, retry availability, and phase-derived retention contract.

## Implementation Notes & Status

- 2026-07-29: Reduced plan reviewed against the locked incident ledger.
- 2026-07-29: User accepted the plan, including the proposed 45-second Podman
  hard deadline, two-minute uninstall-attempt budget, and completion-relative
  30-second automatic retry interval.
- 2026-07-30: During Slice 1 code review, the manual-lock handler was found to
  race concurrent unlock while joining post-Ready maintenance. The user
  approved moving the already-accepted complete public manual-lock removal
  from Slice 5 into Slice 1 instead of adding temporary lock/unlock
  serialization for an endpoint being removed.
- 2026-07-30: Holistic re-review found that moving destructive generic
  orphan-LV cleanup post-Ready contradicted the governing read-only LV-audit
  boundary. It was removed from the plan together with its deletion-race
  protocol. The user selected a 30-second active-screen authoritative refetch
  to repair dropped SSE wakes without durable event delivery.
- 2026-07-30: The user confirmed that Piccolod command deadlines cover every
  external runtime command executed while a lifecycle owner holds the global
  gate, including ordinary reconciliation; the 45-second runtime-control cap
  is not limited to uninstall.
- 2026-07-30: Slice 1 passed focused normal/race validation, affected-package
  suites, targeted security review, a clean final Codex gate, and RFC
  implementation closure. Alpha/device validation remains pending.
