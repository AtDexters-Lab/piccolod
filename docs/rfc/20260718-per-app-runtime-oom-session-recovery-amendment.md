# Per-App Runtime OOM Hierarchy and User-Session Recovery Amendment

**Problem:** Under global memory pressure, Piccolo currently makes rootless app
workloads as OOM-resistant as `piccolod` while leaving the systemd user manager
and D-Bus session that those workloads require easier to kill. After such a
kill, a stale D-Bus socket can be mistaken for a live session, so an enabled app
does not converge back to running until an operator restarts the user service.
**In scope:** Make systemd the authority for per-app user-session liveness;
repair failed sessions through the existing app reconciler; account session
repair failures through the existing startup-escalation policy; restore a
deliberate OOM victim hierarchy at the rootless execution boundary; package the
user-manager policy in Piccolo OS; and add focused unit, package, image, and live
recovery validation.
**Out of scope:** Per-app memory-limit policy or resource-class correction;
adding swap, RAM, zram, load shedding, admission control, or host-capacity
policy; a new pressure monitor, session supervisor, retry ledger, circuit
breaker, event topic, or UI workflow; choosing which individual app should be
killed; and changes to Namek or any other app definition.

Status: Implemented; current-tree Tumbleweed alpha validation passed; coordinated-image validation pending
Date: 2026-07-18
Amends:

- docs/rfc/20260206-rootless-podman-and-cap-drop.md
- docs/rfc/20260220-per-app-user-isolation.md

Corrects the OOM-score inheritance assumption in:

- docs/runtime/resource-stewardship.md
- .claude/plans/resource-stewardship.md

## Context and evidence

On the incident host, a global OOM event killed the user-session control plane
for the Namek app. The relevant observed sequence was:

1. The machine had approximately 1.9 GiB RAM and no swap.
2. The kernel reported `constraint=CONSTRAINT_NONE` and `global_oom`, not a
   per-app cgroup maximum breach. The affected app slice reported
   `memory.events max=0`.
3. The OOM task table showed `piccolod`, Gitea, Postgres, PowerDNS, Pasta, and
   Namek at `oom_score_adj=-500`. Rootless processes had inherited the daemon's
   protection.
4. The per-app systemd user manager was at `oom_score_adj=100`, and its D-Bus
   broker was at `200`.
5. The kernel killed D-Bus processes and then the Namek user's systemd manager.
   systemd consequently killed the app's remaining Podman and container
   processes, and `user@475.service` settled in `failed/result=signal`.
6. `/run/user/475/bus` still existed. The current socket-only check therefore
   treated the failed session as healthy.
7. A later UI start reached rootless Podman but failed during runc cgroup
   creation with `Access denied` and the interactive-authentication message.
8. Explicitly restarting the user service and app restored service.

The global memory shortage is a host-capacity concern outside this amendment.
It exposed two Piccolo-owned amplifiers that are in scope:

- the workload/control-plane OOM ordering is inverted; and
- enabled-app reconciliation cannot distinguish a live session from stale
  filesystem residue or repair a failed session reliably.

This is therefore not only a one-off dead-session liveness defect. The immediate
trigger may recur until capacity policy changes, but Piccolo should both choose
a workload before its session plumbing and recover the desired app state after
the selected workload is lost.

## Existing-system boundary

This amendment deliberately strengthens existing owners instead of adding a
parallel resilience subsystem.

| Responsibility | Existing owner retained by this amendment |
| --- | --- |
| User-session lifecycle and truth | systemd `user@UID.service` |
| Durable app desire | `AppInstance.Enabled` |
| Automatic desired-state convergence | `AppManager.ReconcileOnce` |
| Reconcile serialization and lifecycle exclusion | `reconcileMu` and existing transition fences |
| Repeated startup-failure escalation | existing attempt/time-window tracking and `handleStartupFailure` |
| User-visible app state | existing status/message/progress/event paths |
| Rootless command identity boundary | `container.ApplyRuntimeCredential` |
| App resource-pressure observation | existing per-app pressure monitor |
| Host unit and user-manager policy | existing OBS piccolod package and `piccolo-os-support` package |

No new long-running goroutine, supervisor, monitor, standalone recovery record,
event kind, daemon, or operator control is introduced. Existing tuple and
transition records remain the only durable phase owners. Kernel OOM journal
parsing remains diagnostic evidence and is not a control-loop input.

## Decisions

### D1. Treat systemd as the sole user-session liveness authority

`user@UID.service` state from PID 1 is authoritative. A D-Bus socket path is
only evidence after systemd reports the service active; its mere existence
never proves liveness or D-Bus usability. Ready means that the unit is active
and a bounded, non-mutating connection to the app user's bus succeeds.

Runtime acquisition carries an intent through the existing app-user and
`podmanRuntimeForApp` seam. This is a call contract, not a new lifecycle owner:

| Intent | Existing callers | Session behavior |
| --- | --- | --- |
| Observe only | Status, logs, exec preparation, diagnostics, and service restoration | Inspect readiness; never start or restart a unit; return unavailable when the session is not usable |
| Ensure ready | Enabled-app reconcile, manual Start, and an existing install/update/transition owner that must perform Podman work | May start or repair the dedicated app user unit within the existing operation and retry budget |
| Quiesce | Manual Stop, follower demotion, daemon shutdown, and uninstall | Never start a unit merely to stop an app; use healthy Podman for graceful cleanup, otherwise ask PID 1 to stop the dedicated unit and prove its cgroup empty |

An explicit mutating install, clone, update, or recovery transaction may ensure
the session as a prerequisite without acquiring authority over `Enabled`; its
existing transition record and compensation path remain the operation owner.
`ReconcileOnce` remains the only background owner that repairs a session for
the purpose of restoring an enabled app. Read-only paths and disabled-app
reconciliation cannot restore memory consumption after an OOM by starting a
user manager.

The existing per-app user provisioning seam obtains the unit's `ActiveState`,
`SubState`, and `Result` and applies this state machine:

| Observed state | Action |
| --- | --- |
| Active and user-bus probe succeeds | Return the runtime credential as ready |
| Inactive or failed | Ensure-ready may idempotently start the unit; observe-only returns unavailable; quiesce proves the cgroup empty without starting it |
| Activating, deactivating, or reloading | Wait only as allowed by the caller intent within the same operation deadline, then re-evaluate |
| Active but user-bus probe fails | Observe-only returns unavailable; ensure-ready performs one bounded restart of this dedicated app unit; quiesce stops the unit through PID 1 |
| Maintenance, unknown, query failure, start/restart failure, or terminal non-active result | Return a contextual readiness error unless quiesce can prove the unit cgroup empty |

Linger remains the provisioning mechanism that asks systemd to keep the user
manager available. It is not a health signal. Existing-user and newly-created
user paths both use the same readiness contract. Neither a failed `systemctl
start` nor an unusable D-Bus endpoint is swallowed.

The query, start or restart, state wait, and user-bus probe share one
cancellation-aware deadline. A command that hangs cannot escape that bound.
Restarting an active-but-unusable unit is permitted only to an ensure-ready
owner because the unit is dedicated to one app and the operation is already
recovering that app; ordinary observers cannot cause that disruption.

Readiness errors include the app instance, UID, unit, pre-action state,
post-action state, and repair result in structured logs. Control flow does not
depend on human-oriented `systemctl status` or journal text.

### D2. Recover enabled apps through the existing reconciler and escalation

`AppInstance.Enabled == true` remains the only durable statement that an app
should run. The existing 30-second reconcile loop remains the only automatic
recovery owner. A separate session watcher or OOM-triggered restart path is not
added.

For an enabled app, normal reconciliation first enforces the user-session
readiness contract, then runs the existing container-group reconciliation. If
an OOM event killed the user manager and its descendants, the next reconcile
starts the failed user unit and the existing stale/missing container recovery
recreates the network anchor and services.

A session-readiness failure occurs before container-group reconciliation today.
It must nevertheless enter the existing startup-failure accounting, status,
and retry budget. Automatic reconciliation observes the existing five-attempt
or ten-minute escalation rule before performing another session repair or
container start. Once escalated to `StatusError`, it stops automatic start work
until the existing manual-start recovery path is invoked.

A successful start does not immediately erase nonzero recovery history. The
existing AppManager owns one process-local probation timestamp per recovering
app alongside the existing attempt and first-failure fields. Its transitions
are:

| Observation | Existing-state transition |
| --- | --- |
| Healthy desired-running app is first observed absent or unusable | Enter recovery; before repair, consume one existing attempt, set first failure if absent, and clear probation |
| Later automatic repair or recreate | Check the existing guard, then consume one attempt before the effect; success and failure use that same attempt rather than double-counting |
| Failed manual recovery attempt | Retain or increment the existing failure history and clear probation |
| Successful manual or automatic recovery | Set probation start to now, retain attempts and first failure, and publish Running |
| Running observation during probation | Use observe-only acquisition; retain all recovery state until ten continuous minutes elapse |
| Ten continuous running minutes | Clear attempts, first failure, and probation |
| Workload/session loss during probation | Clear probation; the next automatic repair is guard-checked and consumes the next attempt before any effect |
| piccolod restart | Rebuild with no probation or startup history, granting a fresh in-process retry window |

Manual Start bypasses the automatic error guard for its one requested attempt,
but success enters the same probation rather than clearing an escalated
history. Ordinary reconcile remains allowed to observe a probationary Running
app without re-enabling repair; a loss re-enters the normal guard as described
above.

A rapid running-then-OOM loss therefore stays in the same existing failure
budget rather than opening a fresh retry window every 30 seconds. No new health
monitor, timer goroutine, or persisted recovery record is added. The probation
timestamp is an in-process field of the existing AppManager and is evaluated by
ordinary reconciliation. A piccolod restart grants a fresh retry window; that
bounded limitation is accepted because the daemon itself remains the
more-protected `-500` control plane.

Manual Start uses the same readiness and container paths, retains its existing
ability to retry an app in error, and does not create a second recovery policy.
Its durable commit point is `Enabled=true`: the metadata write succeeds and the
in-memory cache reflects that same value before session repair or container
effects begin. Manual Stop similarly commits `Enabled=false` before fallible
runtime cleanup. If either metadata write fails, cache and disk retain the
prior coherent value, no runtime effect begins, the call returns an error, and
no outward success is emitted.

If cleanup fails after a successful Stop commit, the explicit stop intent
remains durable and later reconciliation continues toward stopped rather than
resurrecting the app. The original call returns the cleanup error and does not
falsely report successful quiescence. Runtime helpers cannot perform a
best-effort Enabled write and continue; lifecycle desire has one checked commit
boundary.

Graceful daemon shutdown remains distinct: it stops background reconciliation,
preserves `Enabled`, and does not start a dead user session merely to stop
processes that systemd has already proven absent. A volume may be detached only
after graceful Podman stop succeeds or PID 1 proves that the dedicated user unit
and its cgroup have no processes. If neither proof is available, shutdown keeps
the volume attached and returns the existing shutdown error rather than risking
live-write corruption.

Uninstall remains serialized with reconciliation. It first attempts normal
Podman teardown when the session is usable. If runtime control is unavailable,
the existing systemd unit is the fallback quiescence boundary; destructive
data cleanup may continue only after PID 1 proves the app user's cgroup empty.
Otherwise the app remains installed and retryable rather than claiming that
its container state and data were removed.

The same no-live-writer invariant applies to existing install, update,
snapshot, and rollback transactions. A function that both quiesces and later
recreates runtime is treated as two phases: graceful stop or PID 1 empty-cgroup
proof must commit before any data/rootfs snapshot, LV rename, rollback, detach,
or destructive cleanup; only after that storage effect completes may the
transaction use ensure-ready to recreate runtime. Existing transaction records
remain the sole forward-repair and compensation authority.

Follower demotion, update transitions, install, clone, status, exec, logs, and
diagnostic callers continue to use the same runtime-acquisition seam with the
intent assigned in D1. They may receive the same clear session-readiness error,
but they do not gain authority to change `Enabled` or bypass their existing
transition and rollback owners.

### D3. Restore an explicit OOM victim hierarchy

The target hierarchy is:

| Process class | Target `oom_score_adj` | Ownership and rationale |
| --- | ---: | --- |
| `piccolod` | -500 | Existing production unit protection; the appliance control plane remains last-resort infrastructure |
| Per-app `user@UID.service` manager | -250 | Piccolo OS systemd drop-in; session plumbing survives ordinary workload selection |
| Services launched by that user manager | -150 by systemd manager default | Existing systemd relative default, verified on the target image |
| Credentialed rootless Podman commands and descendants | 0 | Reset at the central execution boundary so app workloads are ordinary OOM candidates |

The exact live values, rather than only file contents, are an acceptance test.
The design relies on the target systemd version's documented relative
user-service default; image validation and the live test prove that assumption
for the shipped artifact.

`ApplyRuntimeCredential` remains the canonical rootless execution boundary. For
credentialed commands it executes the original command through the packaged
absolute path `/usr/bin/choom` with an adjustment of `0`, under the same per-app
UID, minimal environment, file descriptors, terminal wiring, context, and
signal semantics. `choom` applies the score before it replaces itself with the
target process, so no post-start PID race and no interval with a protected
workload is introduced.

The wrapper does not mutate `piccolod`'s own score and does not temporarily
change shared parent-process state. It covers the central Podman command path,
interactive exec, image pulls, rootfs export, and ephemeral diagnostic Podman
calls that already use `ApplyRuntimeCredential`. Rootful maintenance commands
whose runtime has no credential remain unchanged.

If the neutral score cannot be applied, the credentialed command fails closed
with a clear error; it is not launched with inherited `-500` protection. The
production piccolod package declares util-linux as a runtime dependency. A
lifecycle owner that must quiesce an already-running app does not bypass this
rule with unwrapped Podman; it uses the D1 systemd fallback and proceeds only
after the dedicated user cgroup is empty.

Piccolo OS installs a systemd drop-in for `user@.service` with
`OOMScoreAdjust=-250`. This is a template policy, not a per-app generated
drop-in, so existing and future Piccolo app users receive the same control-plane
protection without adding resource-policy reconciliation to piccolod.

The template intentionally applies to every `user@UID.service` on the
appliance, not only `pa-*` accounts. Piccolo OS currently treats persistent user
managers as trusted appliance/session infrastructure, so the simpler boot-time
policy is preferred over generated per-instance drop-ins and their provisioning,
migration, cleanup, and linger-order lifecycle. If Piccolo OS later hosts
general-purpose untrusted user workloads, that changed appliance invariant is
the trigger to reconsider a narrower policy.

### D4. Ship the hierarchy as a coordinated cross-repository change

The complete ordering requires both sides:

- piccolod resets rootless workload commands to `0`; and
- Piccolo OS protects each user manager at `-250`.

Either half is useful but incomplete. Deploying the OS drop-in first makes the
user manager more resistant than it is today, while old rootless workloads
remain at `-500`. Deploying the daemon first makes workloads selectable at `0`,
while the old user manager remains at `100`. Neither partial state is accepted
as proof of the final hierarchy.

The release therefore coordinates the piccolod binary/service package and the
`piccolo-os-support` package in one validated image. The first support-package
version carrying the drop-in declares a minimum piccolod version that contains
the neutral reset; OBS publishes that piccolod build first. The existing final
image-policy gate rejects a snapshot whose installed package versions or
effective unit contents do not contain both halves.

Installing the drop-in and reloading PID 1 does not alter an already-running
user manager. Package scriptlets do not restart active user units and kill
apps. Activation and live-score acceptance occur at the next normal image boot,
after which lingering users start under the new template policy.

No persisted application state or data migration is added. Rollback restores
both packages together through the existing operating-system image/snapshot
mechanism. The minimum-version dependency prevents a rollback to an old daemon
while the new support package remains installed. A new daemon with an old
support package is compatible but incomplete and cannot pass the final
hierarchy gate. Session-recovery code is compatible with the old systemd unit
policy, but full OOM-order acceptance is claimed only after both artifacts are
live following boot.

## Implementation site list

### Piccolod user-session readiness and lifecycle composition

- `internal/container/appuser.go`
- `internal/container/appuser_test.go`
- `internal/app/podman_runtime.go`
- `internal/app/app_manager.go`
- `internal/app/app_manager_test.go`
- `internal/app/filesystem.go`
- `internal/app/filesystem_test.go`
- `internal/app/container_group_reconcile.go`
- `internal/app/container_group_reconcile_test.go`
- existing lifecycle-specific app-manager tests for stop, shutdown, follower
  transition, uninstall, update, and transition recovery

`ProvisionAppUser` remains the single app-user identity/provisioning seam, and
the runtime-acquisition contract carries the D1 intent. `podmanRuntimeForApp`
propagates its readiness failure. `FilesystemStateManager.UpdateAppEnabled`
provides the coherent disk/cache commit required by D2. Existing locking and
transition ownership remain unchanged.

The production call-site audit is explicit:

| Intent | Files and functions | Failure and ownership contract |
| --- | --- | --- |
| Ensure ready for desired running | `app_manager.go`: `reconcileApp` only when Enabled and locally led; `startLocked` | Existing startup accounting and container-group recovery own failure; manual Start owns its checked Enabled commit |
| Observe or quiesce desired stopped | `app_manager.go`: `reconcileApp` when disabled or follower | Never starts a session; healthy runtime stops normally, otherwise PID 1 quiesces the dedicated unit |
| Observe only | `app_manager.go`: `RestoreServices`, `LogsForService`, `LogsStreamForService`, `ExecShellCmdForService`; `container_status.go`: `ContainerStatuses` | Return/deactivate as unavailable without changing Enabled or repairing a session; later reconcile owns recovery |
| Quiesce | `app_manager.go`: `stopAppForShutdown`, `stopForFollowerTransition`, `stopInternal`, `uninstallLocked` | Never starts solely for teardown; PID 1 empty-cgroup proof is the fallback and the prerequisite for detach/destruction |
| Ensure ready under an existing explicit transaction | `app_manager.go`: `installWithRetries`, `cloneWorkspaceLocked`, and the recreate phase of `updateImageLocked`, `rollbackToSnapshotLocked`, and `updateListenersLocked` | Existing install/update transaction owns rollback and may not change Enabled except at its already-defined lifecycle commit |
| Quiesce before transactional storage effects | `app_manager.go`: quiesce phases of image update, snapshot rollback, and listener-driven recreate; `custom_manifest_update.go`: `restorePrecommitDataSnapshot`; `image_update_transaction.go`: abort and pre-commit data-restore phases; `installed_app_apply_transaction.go`: `quiesceRuntimeForPrecommitDataSnapshotIfNeeded` | Snapshot, LV rename/restore, detach, or destruction cannot begin before graceful stop or PID 1 empty-cgroup proof |
| Ensure ready under manifest-update recovery | `custom_manifest_update.go`: `restoreManifestUpdateAccessFromRuntime`, `stageManifestUpdateRootfs`, and both container-recreate phases | Existing manifest transition record owns forward repair or compensation; readiness failure is recorded in that path |
| Ensure ready under image-update recovery | `image_update_transaction.go`: forward-complete and post-restore recreate phases | Existing image transition phases own failure and replay after quiescence and storage effects commit |
| Quiesce then ensure under catalog apply | `catalog_sync_apply.go`: container-recreate transaction | The existing apply transaction proves quiescence before destructive replacement, then may ensure-ready for recreate; compensation remains its owner |

Focused tests exercise every intent/authority class and the protected
quiesce-before-storage composition. The table is the current production
call-site audit; it does not introduce a permanent exhaustive-call-site test
mechanism.

### Piccolod rootless OOM boundary

- `internal/container/podman.go`
- `internal/container/podman_test.go`
- `internal/app/rootfs_integration.go`
- `internal/server/gin_diagnostic.go`

The last two files are explicit composition-audit sites because they call
`ApplyRuntimeCredential` directly. They require code changes only if their
current `exec.Cmd` construction cannot preserve the wrapper contract.

### Production package and Piccolo OS policy

- OBS piccolod package `home:atdexterslab/piccolod/piccolod.service`
- OBS piccolod package `home:atdexterslab/piccolod/piccolod.spec`
- `piccolo-os/packages/piccolo-os-support/piccolo-os-support.spec`
- new packaged `user@.service.d` OOM policy source under
  `piccolo-os/packages/piccolo-os-support/`
- `piccolo-os/scripts/validate-image-policy.sh`
- `piccolo-os/scripts/validate-obs-image.sh`

The existing production `piccolod.service` value of `OOMScoreAdjust=-500` is
retained and asserted. The package spec adds the `choom` runtime dependency.
The OS support package owns installation of the user-manager template drop-in,
daemon reload/reboot integration, package file inventory, and final-image
validation.

### Documentation

- `docs/runtime/resource-stewardship.md`
- `docs/rfc/20260206-rootless-podman-and-cap-drop.md`
- `docs/rfc/20260220-per-app-user-isolation.md`
- `.claude/plans/resource-stewardship.md`

Historical documents remain readable but point to this amendment wherever they
state or imply that a credential switch alone makes per-app processes ordinary
OOM candidates.

## Temporal composition

The shared invariant is: persisted app desire is authoritative over automatic
recovery, while process killability is ordered so an app workload is selectable
before the session and daemon required to reconcile that desire.

### Canonical lifecycle events

| Event | Authority | State transition | Observable effect | Durable record | Retry | Cleanup or compensation |
| --- | --- | --- | --- | --- | --- | --- |
| Start or activation | Manual Start or existing reconciler | Enabled app with absent/failed session to ready session, then running container group | Existing progress and status paths report start/recovery | Existing `Enabled` and app metadata only | Existing startup-attempt/time policy | Existing container-group recreate removes stale runtime state |
| Normal completion or commit | App manager | Session ready and container group running | Status becomes running and services reactivate | Existing container IDs/status metadata; nonzero recovery history remains process-local until the stability window completes | Stability is re-observed by ordinary reconcile | Tracking resets after ten continuous running minutes |
| Abnormal workload loss | Kernel/systemd, then reconciler | Workload exits while session remains or the whole user unit fails | App becomes non-running; next reconcile enters recovery and consumes an attempt before repair | `Enabled` remains true; attempt/probation state is process-local | Reconcile within existing escalation budget | Existing stale/missing container recovery |
| Abnormal session-repair failure | PID 1 and app manager | User unit remains non-ready | Contextual error and existing starting/error status | Existing process-local startup-attempt fields; no new durable record | Existing five-attempt or ten-minute budget | Manual Start may reacquire after operator/environment correction |
| Pause or suspension | None | No new paused recovery state exists | None | None | None | Stop, shutdown, or process termination are the supported boundaries |
| Resume or reacquisition | Existing reconciler | A later eligible reconcile reacquires systemd readiness | Ordinary status transition | Existing app metadata plus process-local recovery history | Existing budget | Continuous running through the existing window clears tracking |
| Cancellation, interruption, or abort | Request context or daemon shutdown | In-flight wait/command terminates | Caller receives cancellation; no success event | No new record | Owning lifecycle may be invoked again | Graceful shutdown preserves Enabled and stops reconciliation first |
| Supersession or owner change | Manual lifecycle or cluster role transition | Manual Stop or follower state supersedes local running desire | Reconcile cannot race past the existing lock/fence | Manual Stop persists `Enabled=false`; cluster role remains separately owned | Normal reconcile converges stopped | Existing stop/container cleanup paths |
| Retry or replay | Existing reconciler or manual Start | Same desired state is evaluated idempotently | Repeated attempts are visible through existing status/message paths | Existing process-local startup tracking | Bounded automatically within the daemon lifetime; manual retry remains available | No separate replay log |
| Process restart or recovery | systemd and app manager | piccolod rebuilds observed state, then evaluates persisted desire | Enabled apps reacquire sessions and containers | Existing persisted app state; startup budget begins fresh | Normal reconcile | Orphan/user/container cleanup remains existing behavior |
| Rollback or compensation | Release/image owner | Both package halves return to the prior image | Prior hierarchy and behavior resume | Existing OS snapshot/package state | Redeploy coordinated artifact if needed | No application-data migration to undo |
| Partial completion or one-sided effect | App manager or package manager | Session starts but containers fail, or only one OOM-policy half is deployed | Existing startup error, or explicitly incomplete live hierarchy | No new durable recovery state | Existing startup budget; release completion for policy skew | Reconcile cleans stale runtime; package rollback restores both halves |
| Concurrent overlap or reordering | `reconcileMu`, transition fences, PID 1 | Manual Stop/Start, reconcile, transition recovery, and session failure serialize at the existing boundary | Explicit lifecycle intent wins; no second owner races it | Existing desired and transition state | Existing owners retry | Existing compensation paths remain authoritative |

### Effect ordering

- For credentialed commands, the neutral OOM adjustment is applied before the
  target process executes.
- For runtime operations, systemd-active state and a responsive user bus are
  proven before Podman is invoked.
- Manual Start durably commits `Enabled=true`, and manual Stop durably commits
  `Enabled=false`, before either operation begins fallible runtime effects. A
  failed commit leaves both cache and disk at the prior value.
- For automatic start, the existing escalation guard is checked before another
  repair or recreate attempt.
- Nonzero recovery history is cleared only after the existing ten-minute window
  has been observed continuously running.
- For graceful shutdown, background reconciliation stops before enabled apps
  are quiesced without changing their persisted desire, and volume detach
  follows proven process absence.
- Transactional snapshot, LV rename/restore, detach, or destructive cleanup
  follows graceful-stop or PID 1 empty-cgroup proof; ensure-ready recreation is
  a later phase owned by the same existing transaction.
- The coordinated image is not accepted until both live user-manager and
  workload scores match D3 after boot.

### Ownership and concurrency

- PID 1 owns the user-unit state transition; piccolod requests and verifies it.
- `AppManager.ReconcileOnce` owns automatic desired-state convergence.
- Manual lifecycle calls and reconcile continue to serialize through
  `reconcileMu`; existing transition fences remain authoritative.
- `provisionMu` continues to serialize user creation and subuid/subgid
  allocation. Session readiness adds no second user-lifecycle lock.
- Each state query, repair, and bus probe is cancellation-aware and bounded.
  Reconciliation remains serial, so one pass has a finite worst case of the
  sum of those per-app bounds; it checks the existing context between apps and
  does not add a parallel recovery herd.
- `ApplyRuntimeCredential` configures each command independently and never
  mutates the daemon or other concurrent commands.

### Adversarial scenarios

1. `user@UID.service` is failed while `/run/user/UID/bus` still exists.
2. The unit is active and the socket exists, but the D-Bus broker is dead or
   unresponsive.
3. Unit start succeeds, but readiness crosses the bounded deadline.
4. Unit start fails repeatedly and the app crosses the existing escalation
   threshold.
5. A later manual Start succeeds, enters probation without clearing prior
   tracking, and clears it only after ten continuous running minutes.
6. Manual Stop overlaps the reconcile that would repair a killed session.
7. piccolod shuts down while an enabled app's user unit is already failed.
8. Uninstall begins while the user unit is failed or becomes failed during
   Podman cleanup.
9. Several app user sessions are killed in one global OOM episode and reconcile
   repairs them serially.
10. `choom` is missing, cannot set the score, or cannot execute the target.
11. Interactive exec and progress-streaming Podman calls are wrapped without
    losing terminal, pipe, cancellation, or signal behavior.
12. The OS policy is present without the daemon reset, and the daemon reset is
    present without the OS policy.
13. The final image boots with lingering app users and proves live scores for
    piccolod, the user manager, a user service, Podman/conmon/Pasta, and the app
    workload.
14. A workload is OOM-killed while its session survives, and the enabled app
    returns to running without operator action.
15. The entire user unit is killed, stale socket residue remains, and the
    enabled app returns to running without operator action.
16. A recovered app reaches Running briefly and is OOM-killed again before the
    ten-minute stability window.
17. Persisting `Enabled=true` or `Enabled=false` fails after the in-memory app
    object has been read but before any runtime effect begins.
18. Shutdown loses Podman or `choom` control while a database process still has
    the encrypted volume open.
19. Several dead sessions make one reconcile pass slow while manual Stop or
    shutdown cancellation waits on the existing global lock.
20. A support-package update is installed on a running image whose existing
    user managers still have the old live OOM score.

## Alternatives considered

### Treat the stale socket as the only bug

Rejected as incomplete. It repairs this incident after the control plane is
killed but preserves the inverted victim hierarchy that preferentially kills
that control plane again.

### Add a dedicated session supervisor or OOM watcher

Rejected. systemd already owns session liveness and the app reconciler already
owns automatic recovery. A second loop would introduce duplicate authority,
retry policy, concurrency, and shutdown behavior.

### Let every runtime caller repair the user service

Rejected. A status read or disabled-app reconcile could recreate memory usage
after OOM, bypass startup accounting, or disrupt a healthy workload. Only the
D1 ensure-ready owners may start or restart the dedicated unit after a real
bus probe; observers never repair and teardown owners only quiesce.

### Reset startup history as soon as Running is observed

Rejected. An app can become Running briefly and then be killed again by the
same pressure episode. Immediate reset creates an unbounded 30-second recovery
loop. The existing ten-minute window doubles as the stability confirmation.

### Detach volumes after a best-effort shutdown stop

Rejected. A rootless control failure can coexist with a still-running database.
Detach requires either successful graceful stop or PID 1 proof that the
dedicated user cgroup is empty.

### Change OOM scores after child process start

Rejected. PID lookup and mutation after `Start` creates a race in which the
workload runs with inherited protection and complicates exec, PTY, and
cancellation ownership.

### Temporarily change piccolod's score around process creation

Rejected. The daemon is shared across concurrent app operations; changing its
score would be racy and could expose the appliance control plane itself.

### Set only Podman container `--oom-score-adj`

Rejected. Rootless runtime support processes such as Podman, conmon, and Pasta
also inherit the daemon's score and are part of the observed inversion.

### Protect only the user manager

Rejected as incomplete. If rootless workloads remain at `-500`, the user
manager is still a more attractive victim even after moderate protection.

## Implementation and verification sequence

1. Add an injectable, cancellation-aware systemd user-session state and
   user-bus readiness seam. Test stale sockets, responsive/unresponsive buses,
   every state class, bounded commands, cancellation, and start/restart errors.
2. Carry observe/ensure/quiesce intent through runtime acquisition, audit the
   current callers in the site table, and add focused behavior tests for each
   intent class. Integrate enabled reconcile with existing startup accounting
   and ten-minute stability clearing.
3. Make the existing Enabled metadata update a coherent disk/cache commit and
   compose manual Start, Stop, shutdown detach, uninstall, and transition paths
   around that commit and PID 1 quiescence proof.
4. Extend `ApplyRuntimeCredential` with the pre-exec neutral OOM wrapper and
   test argument, environment, credential, PTY/pipe, cancellation, direct-call,
   rootless, and rootful behavior.
5. Package the user-manager template drop-in in `piccolo-os-support`, add the
   minimum compatible piccolod dependency and its util-linux requirement, and
   extend package/image-policy and post-boot live-score validation.
6. Correct the runtime resource-stewardship documentation and mark the amended
   assumptions in the earlier RFCs.
7. Run focused Go tests, repository-wide Go tests, race-sensitive lifecycle
   tests where practical, package checks, final-image policy validation, and
   live incident reproductions.
8. Run scoped code review and RFC implementation closure before release.

## Acceptance criteria

- A stale `/run/user/UID/bus`, including one paired with a live manager but dead
  broker, cannot make the user session appear ready.
- A failed or inactive per-app user unit is started idempotently and becomes
  usable only after both systemd-active and D-Bus-ready conditions hold.
- Session start/readiness errors are returned to callers and enter the existing
  enabled-app startup escalation; they are not logged and discarded.
- An enabled app whose workload or entire user unit is OOM-killed returns to
  running through ordinary reconciliation without operator action, provided
  capacity is again available.
- Rapid running-then-OOM churn remains inside the existing five-attempt or
  ten-minute budget because every automatic recovery attempt, including one
  that succeeds, consumes an attempt before its effects; recovery history
  resets only after ten continuous running minutes. A daemon restart explicitly
  begins a fresh in-process window.
- Observe-only and disabled-app paths cannot start or restart a user manager;
  repair and quiescence occur only under the D1 owners.
- A failed Start/Stop Enabled write leaves cache, disk, runtime, and outward
  completion consistent with the prior desired state.
- Manual Stop remains durable even when runtime cleanup encounters a dead
  session; graceful daemon shutdown does not change persisted Enabled state and
  never detaches a volume until graceful stop or empty-cgroup proof succeeds.
- Existing update/snapshot/rollback paths cannot perform a data or rootfs
  storage effect until the same graceful-stop or empty-cgroup proof succeeds;
  runtime recreation begins only afterward.
- Rootless Podman commands and runtime descendants execute at
  `oom_score_adj=0` without mutating piccolod's `-500` score.
- The per-app user manager executes at `-250`, and the target image proves the
  expected relative user-service score.
- Failure to apply the neutral rootless score prevents the workload from
  launching with inherited daemon protection.
- The production package declares the utility providing `choom`; the support
  package requires the first compatible daemon version; and the final image
  contains both halves. Mounted-image validation must fail closed unless the
  RPM database proves the coordinated versions and `systemd-analyze --root`
  proves the merged `user@.service=-250` and `piccolod.service=-500` values;
  live values are validated separately after boot.
- No new monitor, supervisor, retry ledger, standalone recovery record, event
  topic, or UI workflow is introduced. The existing tuple record may carry the
  minimum rollback phase markers required to make its own LV/app commit
  crash-replayable.

## Validated assumptions and external dependencies

- The owner confirmed on 2026-07-18 that automatic recovery is desired for apps
  whose persisted state is Enabled.
- The owner confirmed on 2026-07-18 that the solution may change both piccolod
  and Piccolo OS.
- The owner confirmed on 2026-07-18 that the appliance-wide `user@.service`
  template is preferred over per-app generated drop-ins at the current system
  boundary.
- The owner requires reuse of existing resilience and robustness systems rather
  than bolt-on recovery infrastructure.
- The target Piccolo OS image supplies util-linux and systemd; the package and
  live-image tests verify the exact binaries and score behavior rather than
  relying on the development host.
- The production piccolod unit remains protected at `OOMScoreAdjust=-500`.

## Implementation Notes & Status

Source implementation is complete as of 2026-07-19, and the final current tree
passed pre-release live qualification on a fresh Tumbleweed alpha VM on
2026-07-19. Release acceptance remains open until the coordinated OBS image is
built, booted, and the live gates below are repeated against that exact
artifact; source/unit and development-VM success are not being used as a
substitute for final-image proof.

Implemented through existing owners:

- `user@UID.service` state plus a real bounded user-bus transaction is the
  readiness proof; ensure-ready repairs failed/inactive units and observe-only
  exits immediately on an unusable bus.
- lifecycle quiescence first uses strict Podman stop across stored,
  deterministic-name, and `io.piccolo.instance` label ownership, then falls
  back to PID 1 and requires a non-active empty user cgroup.
- enabled reconciliation uses the existing five-attempt/ten-minute startup
  budget, including continuous-running probation, without a new monitor or
  durable retry ledger.
- rollback uses the existing tuple as authority. It persists the intended LV
  names before the first rename, then an explicit
  `rollback_app_state_committed` phase; recovery can therefore replay a
  partial/complete physical promotion and split `app.yaml`/`metadata.json`
  commits before volume layout or runtime acquisition.
- credentialed rootless commands pass through `/usr/bin/choom -n 0`; the
  appliance-wide `user@.service` drop-in supplies `-250` while the existing
  piccolod unit remains `-500`.

Coordinated package versions are piccolod `0.2.39` and
piccolo-os-support `0.3.14`. The support package requires
`piccolod >= 0.2.39`; piccolod requires util-linux.

Completed source validation:

- `go test ./...`
- `go test -race ./internal/app ./internal/container`
- `go vet ./...`
- shell syntax checks for both Piccolo OS image validators
- `rpmspec -P` for the changed Piccolo OS support-package spec
- `git diff --check` in piccolod and Piccolo OS

The mounted final-image path invokes `validate-image-policy.sh
--strict-artifact`; missing RPM proof, an old piccolod, a missing helper, or an
effective systemd override mismatch is fatal.

Completed current-tree Tumbleweed alpha live validation:

- A fresh `piccolo-oom-alpha-20260719` openSUSE Tumbleweed VM was created from
  the alpha template, initialized through the existing setup stage, and passed
  the existing post-setup auth/storage smoke with six passes and no failures or
  skips. It reported 2,044,694,528 bytes of RAM and no swap.
- Early runs were rejected after independent audits found two teardown gaps:
  an app-UID `catatonit -P` outside the dedicated user cgroup, followed by a
  `/run/user/UID` directory that survived account deletion and was immediately
  inherited when the UID was reused. The existing app-user lifecycle now
  terminates and proves absence of all processes matching the numeric UID,
  stops `user-runtime-dir@UID.service`, and removes its fallback runtime path.
  The harness also treats cleanup as successful only after exact absence proof.
- Review of the precursor 28/0/0 run then found two remaining fail-open edges:
  runtime-path removal did not prove the systemd runtime-directory owner had
  actually stopped, and uninstall discarded its app record after treating user
  deletion failure as non-fatal. Final cleanup now proves linger-marker
  absence and `user-runtime-dir@UID.service` inactivity before and after path
  removal. A cleanup failure retains the existing app record with
  `Enabled=false`, using that record as the durable retry owner without adding
  a cleanup ledger or controller.
- `scripts/alpha/dev-vm-alpha-test.sh 192.168.0.140 oom-recovery` then ran the
  accepted explicit destructive stage against the final current-tree build
  with invocation-unique fixtures `oomr9980923226` and `oomp9980923226`. It
  completed with 28 passes, zero failures, and zero skips.
- The live hierarchy was PiccoloD `-500`, the per-app user manager `-250`, and
  the rootless workload `0`; a real transaction on the app user bus succeeded.
- Loss injection required a full 64-hex container ID, proved exact
  `/proc/PID/cgroup` membership, and targeted its named libpod user scope.
  Negative checks proved that a mismatched ID and missing proc entry fail
  closed.
- Workload-only loss recovered through ordinary reconciliation on attempt one.
  Whole-user-slice loss reproduced `user@UID.service` in failed/signal state
  while its bus socket remained present, then recovered the unit and workload
  with a replacement manager PID on attempt two. Structured logs recorded the
  owned start/restart repair action.
- Rapid loss retained attempts through five, the sixth recovery was blocked,
  ten continuous running minutes cleared the recovery history, and the next
  injected loss started a fresh attempt-one budget.
- Both in-stage cleanup gates proved the exact fixture users, all processes
  matching their real/effective/saved/filesystem UIDs, subuid/subgid entries,
  linger/home/runtime roots, Podman runroots/graphroots, volume mounts, and app
  paths absent after uninstall.
- A separate post-run SSH audit resolved both fixtures to UID 465 from their
  structured session logs and independently proved their identities, UID-owned
  processes, and paths absent; both `user@465.service` and
  `user-runtime-dir@465.service` were inactive/dead with successful results,
  PiccoloD remained active, and `/version` returned
  `v0.2.38-1-g1f6eea5-dirty`.
- The hardened harness kept its cookie and request bodies under a UID-owned
  `0700` directory with `0600` files and removed the prior current-UID-owned
  legacy cookie jar before the accepted run.
- This qualifies the current source-tree build and policy on the development
  VM. It does not qualify RPM provenance, image composition, or the exact
  coordinated OBS artifact, so all final-image gates below remain required.

Pending coordinated-image release gates:

1. Boot the image containing both package versions and record effective/live
   values for piccolod (`-500`), an app `user@UID.service` manager (`-250`), its
   normal user-session plumbing (the target image's relative default), and the
   app's rootless workload (`0`).
2. Kill only an enabled app workload, leave capacity available, and record that
   ordinary reconciliation returns it to running without operator action.
3. Kill the whole app `user@UID.service`, confirm stale bus residue cannot pass
   readiness, and record automatic unit repair plus app recovery.
4. Exercise repeated rapid loss through the fifth owned attempt and the
   ten-minute continuous-running probation boundary, recording that a sixth
   attempt is blocked until the documented reset owner acts.

Plan review on 2026-07-18 converged after delegated structured and adversarial
review, a subtractive minimality pass, and final structured/adversarial
verification. The plan retains no separate session supervisor, OOM watcher,
retry ledger, or host-capacity controller.

## Implementation closure ledger

Delegated implementation review was repeated after closing the alpha cleanup
findings:

- the prior scoped code review found the escaped process and runtime-root leaks
  described above; both now have focused tests and accepted live proof;
- scoped code and security review both returned READY with no open blocking or
  significant defect;
- RFC implementation verification returned closed after the current-tree
  28/0/0 run and independent audit supplied its only missing evidence;
- the final current-tree validation repeated the repository-wide test suite,
  race detection for app and container plus focused server lifecycle seams,
  vet, validator syntax and shell checks, the support-package RPM parse,
  formatting, and diff hygiene; the separate full-server race result is
  recorded below.

The Gin container-manager double now serializes its mutable runtime state, and
the focused leadership race test passes. A full server-package race run also
reported a separate pre-existing `TlsMux.Start`/`TlsMux.Stop` race in
`TestRemote_TlsMuxRestartsAfterReload`; neither side is touched by this
amendment, so it remains an adjacent repository issue rather than a release
claim for this implementation.

Current-tree Tumbleweed alpha qualification and scoped source implementation
closure are accepted. Release acceptance is not accepted: the strict
mounted-image result and all four post-boot live gates above must still be
appended here after the coordinated OBS image is built and exercised. The
locally corrected ignored
`.claude/plans/resource-stewardship.md` is a planning workspace artifact, not a
release input; tracked runtime/RFC documentation is the durable source record.
