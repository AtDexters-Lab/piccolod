# Piccolod Task-Pressure Safety and Observation Authority RFC

**Problem:** Piccolod can exhaust its systemd service task allowance, lose the
ability to fork its own container and access helpers, and then turn an
incomplete Podman observation into false proof that every app container is
missing. Because Piccolod is the appliance's only access path, waiting for
human recovery is not a valid terminal state.
**In scope:** Remove known unreaped child paths; make container observation
authority explicit; preserve routes and last-known app state during incomplete
observation; add bounded persistent app-scoped recovery; observe and shed
Piccolod task pressure; capture recurrence attribution; automatically restart
Piccolod before hard exhaustion without silently losing configured unattended
unlock continuity; surface pressure through the existing dock health treatment;
declare and validate the production systemd task/watchdog policy; hand repeated
failed Piccolod activations to the existing PID-1/MicroOS recovery boundary;
and qualify the design on 2 GiB and 4 GiB appliances.
**Out of scope:** Reopening the released 0.2.39 memory/OOM hierarchy; general
RAM capacity optimization; inactive-app offloading; a promise that 50–100 apps
can all run simultaneously; changing application memory limits; fixing Snapper
XML or Btrfs qgroup behavior; hardware watchdog policy; Piccolod directly
selecting a snapshot, invoking rollback, or rebooting the host on the first
task-pressure event; SSH, serial, or any alternate human access channel; and a
new per-app health screen or operator recovery workflow.

This RFC does not keep a continuously renewed recovery factor outstanding only
to cover arbitrary SIGKILL, kernel crash, or watchdog death. Prepared graceful
and task-Critical restarts qualify unattended continuity; an unexpected death
with no existing handoff guarantees automatic core-access restart plus the
manual unlock path, and is measured separately rather than counted as
successful unattended service recovery. Extending escrow exposure across all
unlocked uptime requires a separate explicit security/product decision.

Status: The main implementation, GitHub v0.2.40 release, authoritative OBS
`0.2.40-1.1` package, corrected Piccolo OS revision-21 image matrix, and
compatible local validation are complete. The exact local candidate RPM passed
install-from-start-limit recovery, both terminal boot-health branches with real
reboots, and unattended locked-to-unlocked recovery. Release qualification is
not closed: strict mounted-root validation of the corrected final image,
direct final-package task-exhaustion interruption, the clean-cohort app/owner
matrix, 4 GiB profile, installed-record capacity baseline, repeated p95, soak,
and canary gates remain pending.
Date: 2026-07-19
Related:

- `docs/rca/20260719-piccolod-pids-controller-exhaustion.md`
- `docs/rfc/20260718-per-app-runtime-oom-session-recovery-amendment.md`
- `docs/runtime/resource-stewardship.md`

## Product and reliability contract

The following assumptions were confirmed by the product owner on 2026-07-19
and amended on 2026-07-22:

- Piccolod is the only supported way to access a Piccolo appliance. SSH,
  console, or another human rescue daemon is not a fallback and must not be
  introduced.
- Automatic `piccolod.service` restart is acceptable when the alternative is a
  dead access plane. If repeated activation failures exhaust the bounded
  service-restart budget, an eventual PID-1-owned host reboot is expected. That
  reboot does not itself choose a snapshot; the next boot composes with the
  existing MicroOS health-checker and rollback owner.
- The OS health checker treats Piccolod's loopback readiness HTTP status as the
  complete contract. HTTP 200 is a successful OS boot even when the device is
  locked; the OS layer must not independently infer unlock, provider, app, or
  external-connectivity health. If that classification is wrong, Piccolod must
  return a non-200 response rather than asking the OS layer to reinterpret 200.
- A prepared graceful or first-generation task-Critical restart is not a
  successful recovery merely because the replacement process reports Ready.
  When unattended unlock is configured and its provider is available, success
  means Piccolod access, device unlock, and an enabled service/route restoration.
  A bounded provider failure or unexpected death with no prepared handoff may
  still fall back to a reachable locked UI and manual unlock; neither may
  postpone automatic core restart without a hard deadline.
- Emergency restart integrates with a provider-neutral encryption recovery
  capability. Namek is the first recovery-factor provider, not a task-pressure
  dependency or the permanent protocol boundary; future unattended-capable
  providers may use another device or mechanism. Interactive-only credentials
  do not satisfy unattended restart continuity.
- A persistent app-scoped unknown may automatically quiesce and recreate the
  app runtime, provided PID 1 proves the dedicated app cgroup empty and
  persistent app data is retained.
- During an incomplete observation, the UI should retain the last-known app
  state and use the existing bottom-bar health pill treatment for degradation.
- Minimum supported memory is 2 GiB; recommended minimum is 4 GiB.
- The architecture must remain compatible with approximately 50–100 installed
  apps. That is not a current guarantee for 100 simultaneously active apps;
  inactive-app offloading and active-capacity policy are separate work.
- Five-nines is a Piccolod-owned access-plane design target. It excludes
  planned OS update/reboot windows, loss of power, hardware failure, and
  infrastructure outside Piccolod. It is not an achieved SLO until production
  telemetry and an observation window demonstrate it. The corresponding
  annual unavailability budget is approximately 5 minutes 15 seconds.
- The known bundled UI uses persistent terminal-session endpoints. The legacy
  direct WebSocket terminal endpoints may be removed without compatibility
  support for unknown clients.

## Context and evidence

The incident host's global PID ceiling was 4194304 and its kernel thread
ceiling was 15408. `system.slice` allowed unlimited tasks, while
`piccolod.service` inherited `TasksMax=2311`. At 11:43:02.361 IST the kernel
rejected a fork specifically in `/system.slice/piccolod.service`. Podman's Go
runtime reported `pthread_create failed: Resource temporarily unavailable` 225
ms later. The same error then affected Podman inspect/start/stop/remove, `lvs`,
and terminal creation. There was no OOM kill.

After reboot the service used approximately 35 tasks and all apps returned
green. The historical task census was lost. The current tree does contain one
unbounded lifecycle defect: the legacy direct PTY starts a child and never
waits for it. It also contains the observed-state amplifier: failed container
inspection leaves a zero state, which reconciliation interprets as absence.

Version 0.2.39 deliberately solves a different problem. It restores the OOM
victim hierarchy and repairs dead rootless user sessions. This RFC composes
with that release and does not weaken its OOM scores, systemd session
authority, quiescence proof, startup-attempt accounting, or transition replay.

## Existing-system boundary

| Responsibility | Owner retained or extended |
| --- | --- |
| Durable desire to run an app | `AppInstance.Enabled` |
| App lifecycle and serialization | `AppManager`, `reconcileMu`, and existing transition fences |
| Rootless user-session truth and empty-cgroup proof | PID 1 through the 0.2.39 session contract |
| Container truth | Podman results, but only after a complete successful observation |
| App routes and listeners | Existing `ServiceManager` and router registrations |
| Per-app PSI/OOM pressure | Existing `internal/resources/pressure` monitor |
| Piccolod self task pressure | New constant-size task guard in the same resources/pressure boundary |
| Piccolod process restart and task cgroup cleanup | systemd `piccolod.service` |
| Restart unlock continuity and local wrapped-SDEK handoff | Existing `internal/autounlock` orchestration, generalized at its provider boundary |
| Recovery-factor deposit, pickup, and expiry | Configured provider; Namek is v1 and receives only a fresh random recovery factor, never the SDEK or password |
| User-visible degradation | Existing event stream and desktop dock health indicator |
| Crash replay for destructive app effects | Existing per-app `TransitionRecord`, extended with one runtime-recovery operation kind |
| Production unit policy | Existing OBS piccolod package plus mounted/live image validation |

No independent access daemon, human rescue path, per-app watcher, permanent
per-app goroutine, or unbounded worker pool is introduced.

## Definitions and invariants

### Observation outcomes

Container-group observation has exactly five mutually exclusive outcomes:

| Outcome | Meaning | May authorize mutation? |
| --- | --- | --- |
| Known running | All expected containers exist, belong to their recorded cgroups, and are running | Normal route/status reconciliation only |
| Known stopped | The container exists and a successful inspect reports it stopped | Start when desired or remain stopped |
| Known missing | A successful existence check proves absence, including a typed not-found result | Recreate when desired |
| Known stale | Podman metadata exists but the recorded process is not in the container cgroup | Existing strict stale recovery |
| Unknown | Session, Podman, filesystem, cancellation, timeout, parse, or resource failure prevented a complete observation | No container, route, status, or storage mutation solely from this observation |

Unknown is a first-class process-local observation result, not a new persisted
app desire or a synonym for stopped/missing. A zero Go struct cannot represent
unknown.

### Task-pressure states

The task guard reads the service's cgroup-v2 `pids.current`, `pids.max`, and
`pids.events` files without spawning a subprocess. Percentages are calculated
against the effective finite `pids.max`:

| State | Entry | Exit | Policy |
| --- | --- | --- | --- |
| Normal | Below 50%, no new `max` event | N/A | Normal admission and reconcile |
| Warning | At or above 50% for two samples | Below 40% for two samples | Degraded health; stop new nonessential child work; reap detached terminals; preserve app routes |
| Critical | At or above 75% in one sample, `pids.events:max` increments, or Warning remains at/above 50% for 60 seconds after admission shedding | Process restart | Fence new effects, request emergency Piccolod restart, and capture bounded best-effort attribution |

The host sample interval is five seconds. The existing per-app PSI sampling
remains 30 seconds; the D8 installed-record baseline must not turn task
guarding into per-app five-second polling.

At startup the guard records the current `pids.events:max` value as its
baseline; only a later positive delta is a Critical event. This prevents a
historical counter retained across a service-cgroup restart from causing a
restart loop. A validated cgroup-path change establishes a new baseline.
`pids.current` thresholds remain active immediately.

If `pids.max` is `max`, zero, malformed, or unreadable, or the service cgroup
cannot be resolved safely, the guard reports `task-pressure` Error with reason
`monitor_unavailable`, emits no false Critical transition, and leaves Piccolod
running. The production image/live policy gate must reject that configuration;
development and unit-test processes outside the production unit remain
supported through an explicitly disabled/injected guard. A malformed
`pids.events` disables only max-event-delta detection while current/limit
thresholds continue and health remains degraded.

The thresholds are deliberately far above normal observed Piccolod use
(approximately 35 tasks) while leaving at least 25 percent of the cgroup
allowance available for diagnosis and shutdown. They are percentages because
the systemd limit scales with the target host's kernel task capacity.
The sustained-Warning entry prevents a stable leak between 50 and 75 percent
from fencing app recovery forever without ever reaching Critical. Its timer
starts only after Warning admission has closed and resets after two samples
below 40 percent or a process restart. The corresponding Critical reason is
`sustained_high_water`.

### Safety invariants

1. Failed observation cannot manufacture container absence.
2. Unknown alone cannot deactivate an app route or turn a last-known running
   app red.
3. Destructive app-runtime recovery requires both app-scoped attribution and
   PID 1 proof that the dedicated app user cgroup is empty.
4. Persistent application data, rootfs generations, snapshots, desired state,
   and published listener intent are not part of resettable Podman metadata.
5. Every successfully started child has exactly one eventual `Wait` owner,
   including cancellation and client-disconnect paths.
6. Task-pressure recovery has constant daemon overhead independent of installed
   app count.
7. The service task limit remains finite. Raising or disabling it is not a
   substitute for lifecycle correctness.
8. An emergency control-plane restart must not claim an app lifecycle commit;
   existing durable transaction owners replay or compensate after restart.
9. Task recovery must not persist a password, plaintext SDEK, or plaintext
   recovery factor. The local handoff contains only the compatible raw wrapped
   SDEK plus optional non-secret metadata; the fresh random factor is held only
   by the configured provider.
10. Persisted desired aliases do not authorize remote publication. An app
    alias is publishable only while the authoritative runtime projection retains
    an actively published route; portal aliases remain available during core
    bootstrap.

## Decisions

### D1. Make observation authority explicit at the AppManager boundary

AppManager gains one typed container-group observation model containing the
outcome, the successfully observed container states, and a causal error when
the outcome is unknown. It is process-local and contains no desired-state or
recovery authority.

All app control paths branch on that model before effects. The production audit
includes:

- enabled and disabled `reconcileApp` paths;
- anchor and service handling in `reconcileContainerGroup`;
- `containerGroupObservedRunning` or its replacement;
- manual group start in `container_group_lifecycle.go`;
- name-based container resolution;
- service/published-port restoration;
- per-service status projection; and
- stale-container pruning when enumeration fails.

The Podman boundary retains the existing distinction: a successful
`container exists` exit-one result is known absent, while another exit code,
execution failure, timeout, cancellation, invalid output, or failed inspect is
unknown. `ContainerNotFoundError` remains the typed name-resolution absence.
Callers must use `errors.As`/`errors.Is` semantics rather than matching
human-oriented error strings.

A partial group observation is unknown for group-wide mutation. Successfully
observed individual states may be logged, but they do not authorize teardown
of siblings after another observation fails.

### D2. Preserve routes and last-known state while observation is unknown

When an enabled app was last known running and its runtime observation becomes
unknown, AppManager:

- retains the existing observed app status and transient message;
- retains existing routes, listener registrations, and container IDs;
- does not call stop, remove, create, start, route deactivation, storage repair,
  or startup-failure escalation merely because of the unknown;
- records a process-local observation-failure window and emits task/resource
  health evidence; and
- retries through the existing serialized reconcile cadence.

An unknown is not exposed as `StatusStopped` or `StatusError`. The dock's
system degradation communicates uncertainty without falsifying each app's
state. If Piccolod itself restarts, the WebSocket disconnect already makes the
dock Offline until the new process becomes ready; recovery-mode startup or the
normal startup/reconcile projection then resumes as applicable.

For an app with no prior running observation in the current process, the
existing starting projection may remain, but unknown does not consume another
automatic attempt or claim that its containers are absent.

The 0.2.39 user-session ladder remains first: an enabled-app reconcile may
consume one existing startup attempt to repair an inactive/failed dedicated
user session, then re-observe. A caller cancellation or daemon shutdown never
becomes a shared app failure.

### D3. Bound persistent app-scoped unknown and recover it without data loss

Unknown cannot mean permanent inaction. AppManager maintains a small
process-local failure window per app using the existing reconcile owner, not a
new goroutine. Three consecutive complete-group observation failures spanning
at least 60 seconds qualify as persistent only when:

- the Piccolod task guard is Normal;
- the app's systemd user session is ready after the 0.2.39 bounded repair;
- the app volume and shared image/rootfs dependencies are accessible;
- no manual lifecycle, update, rollback, manifest, leadership, shutdown, or
  other transition owns the app; and
- the failure remains isolated to this app's dedicated Podman runtime, with no
  equivalent typed cause observed for another app in the same or immediately
  preceding reconcile pass.

Observation failures carry one canonical cause category: task pressure,
dedicated user session, cancellation/timeout, Podman executable/control-plane,
shared image/rootfs/storage, dedicated runtime storage, or invalid
output/unknown. These categories come from typed boundaries and validated
paths, never stderr text. Before metadata quarantine, AppManager runs one
bounded no-mutation sentinel against an ephemeral Podman runtime using the
same executable and shared dependencies but none of the app's runtime
metadata. If the sentinel fails, or another app reports the equivalent cause,
the failure is shared/global: per-app quarantine is vetoed, all affected apps
retain last-known state/routes, and Piccolod remains accessible and degraded
while the shared owner is re-observed. A later successful sentinel can make a
still-persistent single-app failure eligible again.

Persistent app-scoped recovery consumes one existing automatic startup attempt
before its first effect. It then applies this ladder under `reconcileMu` and the
existing transition fence:

1. Ask PID 1 to stop the dedicated app user unit and prove its cgroup empty.
2. Restart/reacquire the user session and re-observe without mutation.
3. If state becomes known, use the corresponding existing known-state path.
4. If Podman remains unknown, run the existing bounded validation/repair path
   against the dedicated runtime and re-observe.
5. If failure remains isolated to resettable per-app Podman metadata, quarantine
   that metadata atomically, create a clean runtime root, and recreate the
   network anchor and service containers from the installed definition and
   retained rootfs/data volumes.

The final metadata quarantine is crash-replayable by extending the existing
single per-app `TransitionRecord` owner with
`TransitionOperationRuntimeRecovery`, an automatic-recovery source kind, the
minimum phases needed to distinguish original runtime, quarantined runtime,
clean runtime created, and container group committed, and resource fields for
the original/quarantine paths. A second record file or parallel transition
owner is not introduced. Existing transition fencing means runtime recovery
cannot begin while update, rollback, manifest, catalog, cleanup, or another
operation owns that record.

The existing transition planner currently accepts service-mode installed-app
updates. The runtime-recovery operation is the narrow exception that accepts
both service and workspace modes because it changes only the dedicated Podman
runtime. Its normalized plan requires `Enabled=true`, automatic source,
unchanged manifest/data/rootfs/access/cleanup policies, and validated original
and quarantine path keys. Its review action is disabled; it cannot appear as a
catalog/config review. Every other operation retains the current service-mode
and review constraints.

The runtime-recovery plan cannot change `Enabled`, app data, rootfs
generations, volume snapshots, listener intent, or another operation's plan.
Startup recovery dispatch recognizes this operation kind before ordinary
reconciliation, forward-completes or restores it idempotently, and keeps the
app fenced if the record is unreadable or its path/plan validation fails.
An older binary that does not recognize the operation/source or phase must
leave the strict record and quarantine untouched and keep the app fenced; a
coordinated rollback therefore completes or explicitly restores active runtime
recovery records before installing that older binary.

At most one quarantine is retained per app. A failed recreate keeps the record
and quarantine for idempotent forward retry or restoration. A successful
container-group and route commit deletes the known-bad quarantine and clears
the record; deletion failure uses the existing committed-cleanup-pending shape
and cannot roll back the running app. The ordinary process-local ten-minute
startup probation continues after the record clears and governs later churn.

Before PID 1 proves the dedicated cgroup empty, failure to prove exclusive
attribution, dependency access, sentinel health, transition ownership, or
quiescence stops recovery with retained routes/last-known state and a degraded
system signal. Once PID 1 proves the cgroup empty, the old backend is
authoritatively absent: publication is withdrawn before runtime repair, and a
later dependency, sentinel, or filesystem failure remains degraded with data
and recovery records retained but cannot keep advertising that absent backend.
Both cases retry only within the existing five-attempt/ten-minute startup
budget. Manual Start remains the existing explicit retry after escalation.

This fallback addresses a genuinely invalid per-app container runtime without
treating every transient Podman error as corruption.

### D4. Remove the unreaped legacy terminal path and audit child ownership

The following direct WebSocket endpoints and their private PTY implementation
are removed:

- `GET /api/v1/terminal`
- `GET /api/v1/apps/:name/terminal`
- `internal/server/gin_terminal.go`
- `internal/server/gin_workspace_terminal.go`
- `internal/server/pty_session.go`

The supported persistent session endpoints remain unchanged. The bundled
Flutter UI already uses them. Their manager retains the 16-session cap,
five-minute detached idle reaper, process cancellation, and explicit child
wait.

The implementation audit covers every production `exec.Cmd.Start`,
`pty.Start`, pipe-backed stream, and helper that returns a started process.
Each site documents exactly one wait owner and identifies which of start
failure, normal exit, context cancellation, consumer disconnect, and daemon
shutdown actually apply. Focused event tests are required for the supported
terminal shared helpers, changed child-lifecycle code, and any audited site
whose wait ownership is not already proven by an existing test. Already-correct
unchanged helpers may cite existing proof and mark inapplicable events rather
than gain bespoke matrix tests. Helpers may centralize this contract where it
removes duplication, but this RFC does not require a repository-wide process
abstraction.

The audit must not accept only goroutine cleanup as proof; the direct operating
system child must be waited. Tests repeatedly create and close the supported
host and workspace terminals, then assert session count, goroutine count, and
child/zombie count return to a stable baseline.

### D5. Add one constant-size Piccolod task guard

`internal/resources/pressure` gains a task guard registered with the existing
supervisor. It owns one goroutine and reads cgroup files directly. Production
discovers the Piccolod service cgroup from `/proc/self/cgroup` and validates
that the resolved path remains below the mounted cgroup-v2 root. Tests inject a
temporary filesystem and clock.

On Warning the guard:

- sets the existing health tracker component `task-pressure` to Warn;
- publishes a global `ResourcePressureEvent` with resource `tasks`;
- closes detached terminal sessions and rejects new terminal/log-stream
  sessions and other nonessential child-producing diagnostics;
- pauses automatic Podman observation/recovery effects before they spawn; and
- asks the restart-unlock continuity owner to prepare one short-lived handoff
  in the background while task headroom and the current in-memory SDEK still
  exist; and
- continues the in-process HTTP/event access plane and explicit no-fork cleanup
  operations.

Preparation is conditional: it is a no-op when the device is already locked,
unattended recovery is disabled, or no unattended-capable provider is ready.
The guard never performs provider I/O or waits for preparation. The continuity
owner coalesces repeated Warning samples into one attempt and serializes it
with ordinary shutdown ceremony, startup pickup, settings changes, and tests.
The prepared handoff is scoped to the current process generation and expires
through the provider's existing short window. Preparation is accepted only when
the provider reports at least 120 seconds of remaining effective lifetime,
covering sustained Warning, restart, and recovery margin; a shorter response is
treated as unprepared, the local blob is deleted, and the inert remote factor
is left to expire.

The guard-to-continuity boundary retains the latest pressure state and
generation even before the continuity owner is attached. Attachment atomically
reconciles that snapshot: current Warning requests preparation, current Normal
cancels any older Warning intent, and current Critical never starts background
preparation because the emergency owner already owns the bounded last chance.
This closes the constructor interval without moving provider work into the
sampler.

Manual requests that would spawn a child receive a typed 503 response with a
retryable task-pressure code. Deleting/closing a terminal remains available.
The gate lives at shared child-admission seams so adding an HTTP-only check
cannot leave background work unbounded.

The admission decision surface is explicit:

| Work class | Warning behavior | Primary sites/owners |
| --- | --- | --- |
| Rootless Podman commands | Reject before command construction unless they belong to an already-active explicit transition reaching its next recorded boundary | `internal/container/podman.go` command factory; AppManager reconcile, restore, install, lifecycle, and transition callers |
| Background app convergence | Do not begin a new observe/repair pass; retain current routes/status and resume without queuing every missed tick | `internal/app/app_manager.go`: background reconcile and service restore owners |
| Host/workspace terminals | Reject new sessions; keep delete/close; close detached sessions; active sessions continue until Critical or their normal owner ends them | `internal/server/gin_terminal_sessions.go`, `internal/terminal/manager.go` |
| App and journal log streams | Reject new child-backed streams; existing streams keep their existing cancellation owner | `internal/container/podman.go`, `internal/server/gin_logstream.go` |
| Diagnostic child probes/downloads | Reject new child-backed probes with the typed retry response; no-fork health/detail and event access remain available | `internal/server/gin_diagnostic.go` and diagnostic routes in `internal/server/gin_server.go` |
| New app/install/update/rollback/catalog effects | Reject before a durable operation/transition begins; an already-recorded transition is not abandoned at Warning and may reach its next crash-safe record boundary | AppManager transition entry points, `internal/update/manager.go`, catalog/app handlers |
| Onboarding image install and disk preparation | Reject a new user-initiated install; an active pipeline retains its owner and is not re-entered; boot preparation is not started by a new retry tick while fenced | `internal/onboarding/installer.go`, `internal/storage/manager.go`, `internal/storage/diskprep/preparer.go` |
| Network/firewall/mDNS helpers | Suppress only new nonessential child probes; do not fence in-process HTTP, event, proxy, or already-established network access | `internal/runner/runner.go`, `internal/firewall/manager.go`, mDNS/network owners |

All direct production `exec.Command*` sites are assigned to one row during
implementation review. `internal/runner`, the Podman command factory, and the
terminal manager carry the shared gate for their callers; direct command sites
listed above check the same gate at their owning operation boundary. No
caller-specific default is left to implementation.

Returning below 40 percent for two samples restores admission, clears the
health warning, publishes a Normal task snapshot, and lets ordinary reconcile
continue. It also asks the continuity owner to cancel an unused Warning
handoff by deleting its local wrapped blob and metadata. Namek v1 exposes one
device-wide slot and an unkeyed revoke, so this path must not issue a remote
revoke that could complete late and erase a newer deposit. The now-inert remote
factor expires through the provider's existing short window. Cleanup is
bounded, idempotent, asynchronous, and accumulates no queued work while
fenced.

On Critical the guard first closes task admission atomically and signals the
one-shot emergency process owner. If the Warning handoff is already prepared,
the owner proceeds directly to exit. If preparation is in flight, it waits only
within the same bounded emergency budget. A Critical transition with no
prepared handoff may request one last preparation attempt with a two-second
inner budget, provided an unlocked manager and registered continuity owner are
already available. Critical during early construction or while locked skips
that attempt.

The emergency owner arms a hard three-second outer deadline before consulting the
continuity owner. Once preparation succeeds, fails, or reaches its inner
budget, the existing final one-second exit sub-deadline is latched before
marker/census work. No provider, diagnostic, logger, event subscriber, shutdown
hook, or watchdog keepalive may extend the outer deadline or final exit
sub-deadline. Census collection then proceeds best effort and without forking:

- `pids.current`, `pids.max`, and `pids.events`;
- Piccolod Go goroutine count;
- for up to the highest-contributing 64 processes in the service cgroup and
  any descendant cgroups: PID, PPID, comm, state, and thread count from
  `/proc`;
- aggregate counts by comm and process state, including zombies; and
- active supported terminal/session counts and current lifecycle-operation
  kind, without request arguments, environment, terminal content, credentials,
  or user data.

The guard walks only the resolved service-cgroup subtree, deduplicates PIDs,
bounds retained/output records to 64 contributors, and uses aggregate counters
for the rest. The aggregate and bounded top contributors are offered to a
bounded/non-blocking journal sink so the existing redacted diagnostic download
normally preserves the evidence. At the applicable deadline the process exits
even if recovery preparation, `/proc`, event publication, journald, or another
output sink is blocked.
The exit owner does not wait for an acknowledgement from the guard, and the
ordinary watchdog loop is cancelled or ignored once the emergency signal is
latched.

A PCV snapshot may already hold the control-plane filesystem frozen when this
boundary is entered. Before issuing `FIFREEZE`, the publisher records a
conservative intent under `/run/piccolo`; ordinary `FITHAW` clears it only
after the ioctl and descriptor close succeed. The fatal owner atomically
fences future freezes but never waits for the publisher mutex, `FIFREEZE`, or
`FITHAW`. The existing systemd `ExecStopPost` invocation runs after the old
service cgroup is stopped, consumes the intent by attempting `FITHAW` before
recording the service exit, and retains the intent on any non-benign error. A
replacement process retries the same intent before reading or writing normal
control-plane state. `EINVAL` is benign because the conservative intent may
outlive an ordinary thaw that completed just before process exit.

This transfers cleanup to an independent process without forking from the
task-Critical cgroup and keeps the three-second process-exit boundary absolute.
It does not claim progress if the kernel itself leaves `FIFREEZE` or `FITHAW`
permanently uninterruptible: without a separate host-reboot recovery domain,
guaranteed thaw and guaranteed service restart cannot both be proven under
that host/kernel failure. Qualification therefore covers interruption at each
freeze lifecycle boundary with responsive real ioctls; a permanently hung
filesystem ioctl is outside this process-recovery fault model and must fail
closed with the intent retained.

### D6. Use systemd for bounded emergency restart and hang recovery

One process-level emergency owner in `cmd/piccolod` consumes task Critical,
unlock execution-liveness fatal, serialized-owner liveness fatal, and progress-
clear status 76 requests. It cancels new work and terminates Piccolod without
running normal app cleanup or the full graceful-shutdown ceremony. Every source
arms the same three-second absolute exit before other work. If an outstanding
handoff already exists it is reused. If lifecycle is Ready/SDEK-loaded and no
handoff exists, the owner gives the bounded restart-unlock continuity capability
at most two seconds to prepare one, then reserves the final second for marker
and exit. A locked process with no usable SDEK cannot manufacture a handoff.
Preparation failure still exits and truthfully falls back to manual unlock.
Existing durable app/update/rollback records remain authoritative and replay
after restart. This is a distinct emergency boundary; ordinary SIGTERM still
uses the existing graceful shutdown.

Task recovery depends on a small capability, not on Namek transport types. At
plan altitude the boundary is:

```text
RestartUnlockContinuity:
  Prepare(trigger, ttl) -> prepared | not_needed | unavailable
  Recover(complete_unlock_chain) -> unlocked | manual_unlock_required
  Cancel() -> best_effort_cleanup

RecoveryFactorProvider:
  Deposit(random_factor, ttl) -> effective_expiry
  Pickup()
```

The continuity owner, not the provider, generates the random factor, wraps the
in-memory SDEK, enforces expiry, zeroizes temporary factor bytes, and runs the
existing complete-unlock chain. The provider sees only the random factor.
Namek implements this provider contract for v1. Its current escrow API is a
single device-wide slot without a lease id; v1 therefore relies on expiry and
does not use the legacy unkeyed revoke in continuity, Test, disable, or cleanup
paths. A future provider may offer lease-scoped revocation, but that is not a
v1 abstraction or release obligation.

Rolling compatibility preserves the released cryptographic representation.
`auto_unlock_blob` remains the exact raw AEAD bytes with the existing v1 AAD,
so an old binary can consume a blob written by the new binary and a new binary
can consume one written by the old binary. Optional non-secret handoff metadata
(`schema_version`, `provider_id`, preparation phase, expiry, and SHA-256 digest
of the raw blob) is stored as a nested field in the existing
`auto_unlock.json`; old readers ignore it and old writers may drop it. Pickup
reconciles the pair in this order, so dispatch does not depend on which branch
happens to decode first:

1. With no raw blob, any metadata is orphaned and cleared; there is no automatic
   pickup.
2. With a raw blob and no metadata, the handoff is legacy `namek-v1`.
3. With both present, the raw-blob digest is compared before provider dispatch.
   A digest mismatch means the metadata describes an older blob: the metadata
   is cleared and the current raw blob is treated as legacy `namek-v1`, whether
   the stale schema/provider name was recognized or unknown.
4. With a matching digest, a recognized schema/provider is dispatched. A
   finalized record is subject to its recorded expiry; a `preparing` record has
   no trustworthy local expiry but still owns provider dispatch across the
   raw-write, deposit, and final-metadata crash boundaries. An unknown
   schema/provider, invalid phase, or present metadata that cannot supply the
   required schema/provider/digest fields fails closed to manual unlock and is
   never downgraded to legacy pickup. Expired finalized metadata removes the
   local handoff and requires manual unlock.

This preserves an old-writer handoff without binding it to prior metadata while
preventing a matching future format from being interpreted as Namek v1. The
new-writer order is preparing metadata with provider and blob digest, raw blob,
provider deposit, then finalized expiry metadata. A crash before the raw write
leaves orphan metadata that reconciliation clears. Once the matching raw blob
exists, even a lost final metadata write retains the recorded provider and can
never downgrade a custom handoff to Namek. A raw-only old-writer blob remains
the explicit legacy Namek v1 path. Neither file stores a password, plaintext
SDEK, or plaintext factor. A future non-Namek provider still requires its own
old-binary migration contract rather than weakening this v1 fallback.

The singleton slot has one exclusive prepared-handoff invariant. While the
local blob/metadata identify an outstanding handoff, a graceful shutdown reuses
it, repeated pressure preparation is idempotent, Test returns a typed busy
result without touching the provider, and settings disable/Normal cancel only
delete the local handoff and let the remote factor expire. Ceremony, prepare,
pickup, Test, settings, and cleanup share one context-aware process gate; a
Critical caller that cannot acquire it within its budget fails continuity and
still exits. No sequential operation may overwrite the slot while a prepared
handoff still depends on its factor.

Before exit the owner atomically writes a small volatile recovery marker at
`/run/piccolo/task-recovery.json`. `/run` deliberately survives a service
restart but not a host reboot. The marker contains only schema version,
timestamp, reason code, task high-water/limit, restart generation, non-secret
`last_failed_invocation_id`, active owner class/app name plus
`active_owner_invocation_id` when known, a bounded eight-entry owner-strike
ring, and one unattributed/global strike. Both ids are systemd's
`INVOCATION_ID`. It
contains no arguments, environment beyond that invocation id, terminal
content, or credentials. More than eight distinct suspects in one marker
window advances the global strike rather than growing the file or forgetting
containment.

Failure advancement is invocation-idempotent. An emergency write derives from
the inherited marker, increments generation/strike once, and records the
current `INVOCATION_ID` as the last failed invocation. Recording the owner
currently being reacquired is a separate progress field and does not advance
failure state. That progress write must commit before its optional owner starts;
failure keeps the owner unstarted, leaves core access degraded, and retries
marker I/O no faster than the existing 30-second recovery cadence. After an
owner returns, its clear must commit before a new
owner may start. If that clear cannot commit within a one-second bounded retry, main
exits with dedicated status 76 (`progress_state_uncertain`). Post-stop treats
that status as unattributed, ignores the possibly stale active-owner field, and
advances the global strike. It never assigns stale evidence to the prior owner.

The PID-1 post-stop helper receives the same invocation id: it
preserves a marker whose last-failed id is the just-exited invocation, and
otherwise advances the inherited marker exactly once, using a current progress
owner as attribution when available. Thus a successful emergency write is not
double-counted, while a failed write over an older marker is not silently
preserved. Missing starts at generation one with owner strike one when
attribution is available and global strike one otherwise; malformed or partial
state is normalized to generation two/global strike two and therefore
enters bounded recovery containment rather than being ignored. The marker write
is bounded by the same one-second deadline; an absent
marker cannot suppress the cgroup high-water guard on the replacement process.

On startup `cmd/piccolod` reads the marker before constructing the normal
restore pipeline and places the process in task-recovery admission. Recovery
startup brings up the task guard, authentication, in-process HTTP/API, event
stream, UI shell, configured in-process relay clients, and the portal-only TLS
route. It then attempts `Recover` before app restore and other owners that
require decrypted state. Automatic pickup and manual password unlock share one
joinable complete-unlock-chain owner: the winner runs the body and every loser
waits for the same terminal result instead of re-running storage/persistence
effects. The handoff is consumed locally only after lifecycle Ready. A timeout
after SDEK unwrap bounds the caller but does not release execution ownership or
declare the body terminated. Manual requests join it and receive a bounded
`recovery_in_progress` result while it still runs; no second body may start. A
conclusively returned failure retains the local handoff while its provider lease
is valid, leaves lifecycle non-Ready, and permits a later manual action to retry
the chain using the already-installed SDEK where applicable.

The execution owner also arms a non-resettable 30-second liveness deadline from
the instant the complete-unlock body starts. This is separate from the
20-second automatic caller cap. If the body has not returned, the coordinator
signals a pre-registered process-level fatal-recovery owner in `cmd/piccolod`;
that owner advances the reserved `unlock-chain` suspect in the recovery marker
and exits within its own
three-second absolute bound. It neither waits for the blocked body nor relies
on the independent systemd watchdog goroutine to stop sending keepalives.
Manual joins and caller timeouts cannot reset this deadline. The replacement
may retry the still-valid handoff, while no two bodies ever overlap in one
process.

Completion and liveness expiry have one atomic winner. The coordinator owns a
`running -> completion_committed | fatal_committed` state; the lifecycle-unaware
body may not publish Ready/Failed or consume the handoff itself. A returned body
must win `completion_committed` before the coordinator applies the in-process
lifecycle terminal transition and, after Ready, permits best-effort local
handoff consumption. The terminal transition contains no external I/O. If the
timer wins `fatal_committed`, its signal cannot be withdrawn: any later body
return is discarded and cannot publish Ready, delete the handoff, or start
optional owners before the process exits. Boundary tests force both winner
orders, including a body return during the fatal owner's three-second exit
window.

`TopicLockStateChanged` by itself does not authorize app restore. AppManager and
other decrypted owners wait for the lifecycle Ready barrier, and that Ready
transition supplies the wake-up that begins restore. Missing, expired,
unavailable, or failed continuity leaves the reachable unlock UI available,
records a non-secret diagnostic reason, and keeps those owners and app aliases
held; it does not enter an automatic restart loop. The normal eager
`RestoreServices` ordering in `NewGinServer` is therefore split: core access
can report ready while app state is truthfully retained/degraded, and automatic
owners are then reacquired one owner/app at a time through the shared admission
gate. No app is reported Running until lifecycle Ready plus its ordinary
observation or durable transition recovery proves it.

The core-before-optional ordering applies to normal startup as well as marked
recovery startup, so an unmarked boot cannot exhaust tasks inside
`RestoreServices` before the guard or access listener exists. The marker adds
serialized reacquisition and bounded suspect/global recurrence backoff; it does
not create the only safe startup path.

Remote alias publication uses one authoritative runtime projection. Persisted
aliases remain desired configuration only. The bootstrap projection includes
the portal base and starts the TLS mux/relay path for Piccolod itself without
advertising app aliases. An app alias enters both local routing surfaces and
the Nexus adapter only after typed app observation/transition recovery supplies
an actively published route. Registry presence is insufficient: an endpoint
retained by `SuspendAppPublication` is ineligible while proxy/firewall
publication is deactivated. Suspension, stop, and removal synchronously
removes the alias from the in-process resolver/TLS-mux acceptance projection
before route closure and bypasses addition-oriented debounce. Adapter
reconfiguration/cancellation is then attempted with a bounded context. Adapter
Stop completion is the local acknowledgement that the old relay client no
longer advertises through that connection. If the adapter ignores the bound,
route closure may proceed only after local acceptance is denied and degradation
is recorded; upstream may retain stale advertisement until adapter/upstream
state converges, but requests fail closed instead of reaching a missing or
different route. The RFC does not claim a bounded acknowledgement from external
relay infrastructure.

The withdrawal path cannot call the adapter/filter while holding
`ServiceManager.mu`, because the filter re-enters the active-publication
projection. Suspension first marks the app inactive and captures its endpoints
under the existing lifecycle serialization, releases the manager lock, then
synchronously refreshes local resolver/TLS-mux denial and attempts the bounded
adapter withdrawal before closing captured proxy/firewall routes. Every inbound
resolution also rechecks active publication, so a stale alias-map entry is a
deny rather than an authority bypass. Addition events may retain their 300 ms
coalescing; removal/suspension bypasses it. Resume activates all required
endpoints under its lifecycle fence, commits the active projection only after
successful activation, then refreshes/advertises outside the manager lock.

Adapter serialization also rejects stale prepared snapshots. The runtime
publication projection carries a process-local monotonic generation. Every
configure/refresh/reconnect path checks that generation under the adapter-apply
owner immediately before configuring; a snapshot prepared before suspension is
discarded/recomputed and cannot update the adapter fingerprint. If the
generation changes during a blocking adapter call, the owner applies the newest
projection before releasing. A bounded withdrawal caller need not wait for an
adapter that ignores cancellation—local denial and route closure still
proceed—but a late return observes the newer generation and cannot leave the
old alias as the final local adapter state. Tests pause an active snapshot
before apply, suspend/withdraw, then release the stale apply and require the
inactive projection to remain final.

Resume starts every required endpoint first and advertises only after successful
activation. A failed or rolled-back resume leaves the alias withdrawn. Unknown
retains it exactly when the last-known route remains actively published.
`SetNexusAdapter`, persisted-config save/reload, port-claim refresh, and network
reconnection consume this same projection and may not widen it from raw
persisted aliases. Successful external relay connection is
not a systemd Ready prerequisite because upstream infrastructure is outside
Piccolod's availability boundary, but initiating configured relay clients and
installing the local portal route are prerequisites.

Core Ready has an exact proof: the task guard and health owners are registered,
authentication/routes/event snapshots are constructed, and the external HTTP
listener is successfully bound. `SdNotifyReady` moves after bind and before
serving the accepted listener; the current pre-`ListenAndServe` notification is
not retained. Constructors and pre-Ready startup are audited so recovery mode
cannot spawn optional restore/probe children before that proof. App restore is
not a Ready prerequisite, and a locked recovery may truthfully expose the
unlock UI as core Ready. Neither state alone satisfies end-to-end recovery.
The existing `persistence`, `app-manager`, and
`service-manager` readiness components describe initialized control-plane
owners, while task/app recovery remains visibly degraded in health detail and
the dock.

Attribution is bounded evidence, not permission for permanent suppression. A
first strike receives an immediate serialized retry after non-suspects. A
recurrent owner uses a
continuous-Normal-plus-lifecycle-Ready automatic backoff of ten minutes at
strike two, thirty minutes at strike three, two hours at strike four, and six
hours at strike five or later. The process reacquires non-suspect desired
owners promptly, one at a time, without making them repeat the suspect's
backoff. It records the active owner in the marker before each attempt and
clears that progress field after return. A suspect is retried automatically at
the end of its backoff; a successful reacquisition that then remains active
while the appliance is Normal and Ready for ten minutes clears its strike. A
returned ordinary owner failure remains truthfully degraded under existing
retry/manual semantics and does not prevent the rest of the pass.

An unattributed failure, a shared owner class, or overflow beyond the bounded
suspect ring increments the global strike instead. Its first strike still gets
one immediate serialized pass; recurrent global strikes apply the same
10-minute/30-minute/2-hour/6-hour schedule to all optional automatic owners.
After the global delay they are retried one at a time so a later failure can be
attributed when possible. A full desired-owner pass followed by ten minutes
continuously Normal and Ready clears the global strike. Warning pauses new
acquisitions and resets only the current stability/backoff interval; Critical
or a fatal service exit advances the relevant strike once for that invocation.
Thus same-owner and alternating A/B failures isolate only their suspects,
whereas genuinely unattributed/shared recurrence receives the equivalent
global breaker. All paths retry automatically; authenticated manual actions
remain available through Piccolod and use existing explicit retry semantics.

Optional-owner and global recurrence backoff never suppresses pickup of an
existing restart handoff. The reserved `unlock-chain` suspect is the one
exception: it is advanced only by the 30-second execution-liveness fatal path,
not by an ordinary returned provider/chain failure. Its replacement serves the
locked core UI and delays the next automatic pickup by 30 seconds at strike
one, two minutes at strike two, and five minutes at strike three or later.
Manual unlock remains available during that delay. Automatic retry occurs only
if the recorded provider expiry leaves at least another 30 seconds; otherwise
the expired handoff is cleared and manual unlock remains. Lifecycle Ready
clears this suspect. This bounds a deterministic unlock hang without turning a
transient provider failure into a human-only terminal state.

The marker remains until every suspect/global strike has cleared and a fresh
serialized desired-owner pass has completed under Normal/Ready. A disabled or
removed durable owner is no longer desired and its suspect entry is removed.
Durable desire is never mutated by the volatile breaker. A host reboot
naturally clears the marker and reconstructs truth from durable owners.

Desired-owner enumeration and every serialized automatic recovery attempt have
the same independent liveness bridge to the process fatal owner. Enumeration
must acquire lifecycle serialization and return its durable snapshot within a
finite bound before it may expose successor owners or advance marker stability.
Each owner invocation must likewise have a finite owner-specific operation
deadline and committed active-owner progress. Deadline cancellation gets at
most five additional seconds for the operation to return; a return and grace
expiry contend on one `returned | fatal_committed` decision.
If grace wins, admission remains fenced, the active-owner marker is retained,
and main exits within the fatal owner's three-second bound even when the task
guard and systemd-watchdog goroutine remain healthy. A late return cannot start
another owner or withdraw the exit. If return wins, progress clear follows the
acknowledged rule above and ordinary convergence continues. The first-route
candidate therefore has its five-second qualification/operation context plus
at most five seconds of cancellation grace; longer storage/update owners use
their existing finite operation bounds plus the same grace. An automatic owner
without a finite bound may not enter the recovery pass. On replacement,
non-suspects are attempted before any suspect retry, including strike one, so a
non-returning candidate cannot repeatedly stand ahead of later healthy apps.

Critical interruption is specified for every retained child-producing owner:

| Interrupted owner | Post-restart authority and outcome |
| --- | --- |
| Steady-state observe/start/stop/recreate without a transition record | Durable `Enabled`, PID 1 session truth, Podman state, and ordinary reconcile re-observe; no pre-exit success is emitted |
| Installed-app install/update/manifest/catalog/snapshot/rollback or runtime quarantine | Existing `TransitionRecord` operation kind is recovered before normal reconcile; one-sided effects forward-complete or compensate according to that record |
| Rootfs export or image pull before transition commit | Context/process termination leaves no committed candidate; the owning transition cleans staged resources or retries |
| Onboarding install-to-disk stream | `InstallDone=false` remains authoritative; the partial target image is never reported complete and the existing onboarding recovery returns to a retryable pending state |
| Boot disk preparation | `internal/storage/manager.go` and `diskprep.Preparer` re-probe durable disk facts; completion health is published only after the existing phase proofs succeed |
| Transactional OS update command | The update manager re-reads transactional-update state; Piccolod does not manufacture staged/success state from the interrupted request |
| Terminal or log stream | Its client disconnects; systemd kills the old service-cgroup child; no durable session is promised across daemon restart |
| Diagnostic, firewall, network, or mDNS helper | Ephemeral result is discarded; the new process re-observes through the existing owner |

Implementation begins with fault tests for these existing recovery contracts.
If any owner cannot satisfy its stated post-restart outcome, the release blocks
for correction at that owner; the task guard cannot defer Critical exit and
cannot falsely claim crash safety.

The production unit explicitly declares:

```ini
[Unit]
StartLimitIntervalSec=15min
StartLimitBurst=3
OnFailure=piccolod-start-limit-recovery.service

[Service]
TasksMax=15%
WatchdogSec=60s
Restart=always
RestartSec=5s
KillMode=control-group
ExecStopPost=/usr/bin/piccolod --record-service-exit
```

`StartLimitAction` remains unset. The companion start-limit recovery unit is a
PID-1 integration shim, not a second health checker or rollback engine. It acts
only when systemd supplies `MONITOR_UNIT=piccolod.service` for a failed daemon
invocation and, after a bounded five-second scheduling probe, the unit remains
in the terminal `failed` state. systemd preserves the process result (such as
`exit-code`) when its following restart is rejected by the start limiter; it
does not expose `start-limit-hit` through `MONITOR_SERVICE_RESULT`. During an
ordinary `RestartSec` retry the unit is already `activating`, so the companion
returns without recovery action. An unobservable or unrecognized unit state is
retried for the same bounded probe, then enters the existing boot-health wait
as an explicit last-resort recovery path rather than being silently classified
as `failed`. Its unit is ordered `After=health-checker.service`,
so a current-boot health-check job that is already queued must reach a terminal
state before the companion can run:

- while the current boot's `health-checker.service` is pending, systemd leaves
  the companion queued so the existing loopback check can return non-200 and
  the upstream MicroOS owner can select retry, rollback, or reboot;
- after the current boot's health checker has accepted HTTP 200, the companion
  asks PID 1 for a normal host reboot. The upstream checker is a oneshot, so a
  completed successful invocation may be `inactive`; non-empty `InvocationID`
  plus `Result=success` is the terminal-success observation. Active state,
  invocation, and result are read in one systemd property snapshot so a checker
  starting between separate reads cannot be misclassified as complete; and
- after failed boot health becomes terminal, the companion preserves the
  already-written upstream snapshot/marker decision and makes an idempotent
  normal reboot request. The checker's own `FailureAction=reboot` remains the
  primary owner; the duplicate request cannot replace its selected snapshot.

The ordered current-boot wait is bounded by health-checker's existing
five-minute start deadline and manager-owned failure action. If no checker job
is present and its state remains non-terminal, the companion independently
waits at most five minutes before asking PID 1 for a normal reboot rather than
leaving the sole access plane start-limited. Failure of the already-authorized
normal reboot request, or of the bounded helper itself, falls back to the same
PID-1 `FailureAction=reboot` policy used by boot-health recovery.

The handler does not write snapshot state, rerun health-checker, interpret
unlock/app/provider state, or call the rollback engine. A normal reboot retains
the existing shutdown transaction and its bounded force fallback. On the next
boot, upstream health-checker evaluates the selected snapshot again. HTTP 200
is accepted exactly as Piccolod reported it; non-200 remains boot-health
failure.

An explicit service install or package upgrade supersedes an in-flight
start-limit recovery: it stops the companion, reloads the installed units,
resets both units' failed/rate-limit state, and only then starts the replacement
Piccolod binary. Without that reset, systemd would reject the repaired binary's
manual start until the old fifteen-minute window expired.

`TasksMax=15%` makes the existing effective finite policy intentional rather
than dependent on the host's `DefaultTasksMax`. The guard's percentage
thresholds scale with it. `KillMode=control-group` ensures old direct children
cannot survive a service restart; app workloads remain in their dedicated user
slices and are reconciled from durable desire.

The existing `runWatchdogLoop` supplies service-level systemd keepalives once
`WatchdogSec` is present. This is not the hardware `RuntimeWatchdogSec` that was
disabled on affected Intel systems. Live validation must prove both watchdog
restart and task-guard restart independently.

The post-stop mode is a no-secret PID-1-owned recurrence bridge. It reads
systemd's service-result/exit metadata after the old cgroup has been killed and
uses the unit's `INVOCATION_ID` to apply the invocation-idempotent marker rule
above. A clean stop writes nothing. If the task/fatal emergency owner already
recorded that invocation as failed, the helper preserves it. Otherwise
watchdog, timeout, signal, or unexpected non-zero exit advances the inherited
marker once, attributing the active progress owner only when that progress
record belongs to the same invocation and using generic `service_failure`
otherwise. Exit status 76 always ignores active progress and advances global
because main could not durably clear a completed attempt. This makes repeated watchdog crashes enter suspect or global
backoff instead of ordinary eager startup forever, while a failed emergency
marker write cannot lose a generation. It cannot create an unlock handoff
after process death; when none was already prepared, the replacement serves
core access in locked manual-fallback state. The helper itself is bounded and
failure to write the marker cannot prevent systemd restart.

The finite start budget is a terminal escape from repeated core activation
failure, not the first response to task pressure. A first-generation Critical
event still gets an ordinary five-second service replacement. More than three
starts in fifteen minutes means the process-local recovery domain has failed;
the branch above then composes with the current boot-health state instead of
unconditionally pre-empting it with `StartLimitAction=reboot-force`.

Before release, fault injection must prove that the previous service cgroup is
empty before the replacement process reports ready, that persisted Enabled
apps and routes converge, and that a task-critical interruption at each
durable lifecycle phase replays or compensates through its existing owner.

### D7. Reuse the existing pressure event and dock health treatment

The resource-pressure vocabulary gains `tasks` plus exact current/limit fields.
The unified event stream gains an authenticated `resource_pressure` topic and
sends an initial global task-pressure snapshot on connect. The current event
contract remains the source for listener and portal health; task pressure is
combined only in the dock presentation, not forged into a listener record.

For `resource=tasks`, the serialized pressure contract is:

| Field | Contract |
| --- | --- |
| `severity` | `ok` for Normal, `warn` for Warning or monitor unavailable, `urgent` for Critical |
| `task_current` | Last successfully read `pids.current`; omitted only when unavailable |
| `task_limit` | Valid finite `pids.max`; omitted only when unavailable |
| `reason_code` | `normal`, `high_water`, `sustained_high_water`, `max_event`, or `monitor_unavailable` |
| `action_taken` | Empty, `admission_fenced`, or `piccolod_restart_requested` |

`ok` becomes a valid resource-pressure severity so recovery and initial
snapshot do not depend on absence of an event. Other resource kinds retain
their existing severity behavior. Human messages are display context;
reason-code and typed fields drive UI behavior.

Initial connection uses an explicit snapshot barrier: while connected, the
dock remains in a muted `Checking` state until the initial listener-health
snapshot and the initial task-pressure snapshot have both completed, plus
remote state when that topic is relevant. `Offline` is reserved for an actual
event-stream disconnect. Receiving the first event from one topic cannot clear
the barrier for another. A disconnect clears all topic snapshots, and live
events received during snapshot assembly are ordered after their topic's
snapshot or coalesced into it. This prevents a healthy task snapshot from
producing a brief Healthy pill while listener health is still unknown without
contradicting the known connection state.

The existing bottom-bar pill renders:

| Task/connection state | Pill treatment |
| --- | --- |
| Connected, required snapshots pending | Checking |
| Normal | No additional degradation |
| Warning | Degraded |
| Monitor unavailable | Degraded; automatic restart is not inferred |
| Critical before disconnect | Recovering |
| Daemon restart/disconnected stream | Offline |
| Reconnected and below threshold | Aggregate listener/portal state resumes |

The pill uses non-technical, non-secret state-and-expectation copy:

- Critical: “Piccolo is recovering.”
- Warning: “Piccolo is under heavy load. Some actions may be temporarily
  unavailable.”
- Monitor unavailable: “Piccolo cannot confirm system health right now. It
  will keep trying.”
- Automatic recurrence backoff: “Piccolo is recovering. It will retry
  automatically.”
- Unknown app observation: “Piccolo cannot confirm some app statuses right
  now. Last known status is shown.”

If unattended unlock cannot complete, the existing locked surface may say
“Piccolo needs to be unlocked to finish recovery.” Manual Start may remain an
optional existing action; copy cannot imply it is the sole recovery path. The
UI does not say “task exhaustion” or “task pressure,” expose an internal reason
code, or name a suspected culprit. Per-app cards retain their last-known state
during unknown observation. No task counter, process list, recovery button, or
new per-app status workflow is required in the initial UI. Widget tests cover
Warning → Critical → disconnected Offline → connected Checking → hydrated
aggregate state, the neutral strings above, retained app state, and forbidden
internal terminology.

`task-pressure` is present in `/health/detail`. It affects aggregate health but
is not added to the required local-component list for `/health/ready`; a Warn
must not provoke OS rollback or stop Piccolod. Critical recovery is owned by
the task guard and systemd, not readiness polling.

### D8. Keep capacity overhead fixed and qualify the supported floor

This RFC does not add one monitor, timer, process, or goroutine per installed
app. Reconcile remains serialized and the task guard is constant-size. New
child-producing work uses explicit bounded admission; existing terminal
sessions remain capped at 16.

Qualification includes:

- a 2 GiB reference appliance with the production cgroup policy;
- a 4 GiB recommended appliance;
- 100 installed app metadata records with only a bounded active subset, proving
  no permanent task growth proportional to installed count;
- a 72-hour terminal/reconcile/install/log-stream soak with stable task,
  goroutine, and zombie baselines.

No implementation may simply increase `TasksMax` to pass the tests. If normal
supported flows approach Warning on 2 GiB after lifecycle defects are fixed,
the release blocks for an explicit capacity decision rather than silently
moving the limit.

### D9. Treat five-nines as a measured release target, not a claim

The design target for a first-generation injected task-Critical event is:

- detection within one five-second sample;
- emergency continuity preparation and process exit bounded by three seconds,
  with at most two seconds spent on a direct-Critical last-chance handoff;
- systemd restart delay no greater than the configured five seconds;
- the Piccolod shell/UI access plane reachable after restart;
- when unattended unlock is configured and the provider is healthy, pickup plus
  the complete-unlock chain has a 20-second failure cap, while the measured
  end-to-end path reaches lifecycle Ready and the first enabled route-bearing
  app route within
  30 seconds of critical detection at p95 on both supported memory profiles,
  provided the deterministic first-route candidate has no active transition
  and it plus its runtime dependencies are healthy; and
- when continuity is unavailable, a reachable locked UI truthfully requests
  manual unlock without being counted as successful end-to-end recovery.

The three-, five-, and twenty-second values are independent safety/failure caps,
not an arithmetic p95 allocation: consuming every cap would leave insufficient
room for construction, listener bind, and first-route activation. Qualification
therefore measures detection-to-exit, restart delay, construction-to-core-Ready,
pickup-to-lifecycle-Ready, and Ready-to-first-enabled-route-bearing-app
separately. The release blocks unless their observed p95 composition is at
most 30 seconds; the
pickup cap is not lengthened to hide slow provider or startup behavior.

The volatile marker preserves the guard's exact Critical sample time as
`detection_at` separately from the later marker/exit-record time. Correlated
stage records also carry continuity disposition and the known task
current/limit from that incident; a later failure with no task sample clears
those values rather than inheriting stale high-water data. Unlock telemetry
distinguishes a real pickup attempt from an already-Ready fast path.

The post-Ready restore has two explicit phases. The first-route qualification
phase takes one durable selector snapshot and freezes exactly one
reconstructible candidate: the lexicographically lowest persisted
`AppInstance.InstanceID` among durable `Enabled` records whose definition in
that snapshot contains at least one listener, using the repository's existing
normalized instance-id representation. It gives that app a non-resettable
five-second route-attempt slice and does not depend on process-local
`last-known Running` state. At qualification completion, the same candidate
must still be Enabled, transition-free, route-bearing under a fresh durable
definition read, and backed by active publication. Losing any of those proofs
invalidates the healthy qualification cohort as
`mixed_health_selector_changed`; ordinary serialized recovery re-enumerates
fresh truth and continues, but a later/easier route cannot retroactively make
that run a healthy first-route sample. The qualification identity and its
single five-second slice are process-recovery-run authority: neither transfers
nor re-arms when owner enumeration refreshes. A candidate that became
listenerless may still complete ordinary listenerless recovery, but that
success is not a route qualification result.

A listenerless-only selector snapshot has no first-route qualification cohort
and is classified `listenerless_no_cohort`; its enabled owners still receive
ordinary serialized recovery and stability proof without manufacturing
publication. The 30-second release cohort requires earlier stages to leave the
full five-second slice and requires the frozen route-bearing app, any active
transition recovery, and its dependencies to be healthy; otherwise the run is
classified as mixed-health rather than silently selecting an easier app.

Expiry of the five-second slice closes only the qualification phase. Ordinary
serialized convergence then continues over every durable desired owner/app in
stable identifier order with fresh existing per-owner operation bounds. A
returned failure or deadline marks that owner degraded and continues, so the
qualification deadline can neither strand later healthy apps nor become their
operation context. An unsuccessful qualification clears only its durable
in-progress marker; it does not mark the ordinary owner attempted or start its
30-second returned-owner retry delay, so that same app is immediately eligible
for its fresh ordinary bound. A successful qualification already supplies the
stronger ordinary stability proof and is not repeated. An owner that ignores deadline cancellation crosses the
five-second grace into the fatal-recovery path; the replacement restores later
non-suspects before retrying it. Mixed-health runs record time-to-each-route and eventual
convergence but are not falsely evaluated against the healthy-first-route gate.

The runner emits one correlated route-complete stage for each route-bearing app
that successfully becomes active in the recovery run, including a candidate
that succeeds only in its later ordinary attempt. Listenerless recovery never
emits that stage. Marker removal emits the terminal eventual-convergence stage.

Recurrent suspect/global backoff is a containment cohort, not a 30-second app
route cohort: core access and unattended unlock retain their normal timing
gates, non-suspects recover promptly when the breaker is owner-scoped, and
delayed owners record backoff-to-route/eventual-convergence separately. An
unexpected watchdog/crash restart with no pre-existing handoff is a third
cohort: core-access restart and loop-breaker behavior are gated, manual unlock
latency is observed, and it is not counted as unattended unlock/service success.

These are fault-recovery gates, not proof of annual five-nines availability.
Production telemetry must record anonymous restart reason, the stage durations
above, continuity outcome, recurrence, and task high-water marks before an
achieved SLO can be stated.

## Implementation sites

### Observation and app recovery

- `internal/app/types.go`
- `internal/app/app_manager.go`
- `internal/app/container_group_reconcile.go`
- `internal/app/container_group_lifecycle.go`
- `internal/app/container_status.go`
- `internal/app/installed_app_transition.go` for the runtime-recovery operation,
  automatic source, service/workspace planning exception, phases, resource
  paths, strict decode, fencing, and recovery dispatch
- `internal/app/filesystem.go` for the existing atomic transition persistence
- focused app reconciliation, lifecycle, transition replay, and status tests

`internal/container/podman.go` and its tests remain the typed Podman
not-found/error boundary. They change only where needed to preserve enough
typed cause for D1 and the exclusive-runtime repair proof in D3.

### Task guard, admission, and diagnostics

- `internal/resources/pressure/` for the constant-size guard and tests
- `internal/events/bus.go` for task pressure resource/fields
- the existing supervisor component registration; the fatal callback terminates
  at `cmd/piccolod` and no restart policy moves into the supervisor
- `cmd/piccolod/main.go` for the emergency process owner, unlock-liveness fatal
  callback, invocation-idempotent volatile
  `/run/piccolo/task-recovery.json` marker, hard exit deadline, and
  recovery-mode startup selection, plus the no-secret `--record-service-exit`
  post-stop mode used by systemd
- `internal/server/gin_server.go` for wiring, health reporting, and splitting
  core access startup from optional child-producing service restore, including
  binding the external listener before `SdNotifyReady` and splitting portal
  bootstrap routing from post-observation app aliases
- `internal/autounlock/orchestrator.go`, `ceremony.go`, and `pickup.go` for one
  serialized provider-neutral prepare/recover/cancel contract shared by graceful
  shutdown, task pressure, startup pickup, settings, and Test
- `internal/autounlock/blob.go` for the unchanged raw wrapped-SDEK format;
  `state.go` for optional compatible handoff metadata; `update.go` for local
  disable cleanup; `testaction.go` for the prepared-handoff busy rule; and new
  bounded continuity-controller and Namek v1 provider-adapter files
- existing `internal/crypt/manager.go` wrap/unwrap and secure-zero contracts;
  there is no password/SDEK persistence change
- `internal/server/gin_crypto_handlers.go` for the joinable unlock-chain owner;
  a focused `internal/server/recovery_execution.go` coordinator for atomic
  return/fatal ownership, progress acknowledgement, and optional-owner liveness;
  `internal/app/app_manager.go` for lifecycle-Ready restore gating and
  one-app-at-a-time recovery entry
- `internal/services/manager.go` for active-publication proof and local-deny/
  bounded-adapter-withdraw-before-close plus activate-before-advertise ordering;
  `internal/remote/manager.go`
  for applying that projection to every adapter configure/refresh/network-restart
  path without mutating desired aliases
- `internal/server/gin_security_auto_unlock_handlers.go` and
  `gin_boot_handler.go` retain outstanding-envelope and in-flight/manual-fallback
  behavior without a new workflow
- `internal/server/gin_diagnostic.go` for Warning admission and proof that the
  redacted journal download contains the bounded task census
- `internal/runner/runner.go`, `internal/container/podman.go`,
  `internal/server/gin_logstream.go`, `internal/onboarding/installer.go`,
  `internal/storage/manager.go`, `internal/storage/diskprep/preparer.go`,
  `internal/update/manager.go`, and `internal/firewall/manager.go` for the D5/D6
  admission and interruption matrix

### Terminal lifecycle and UI

- remove `internal/server/gin_terminal.go`
- remove `internal/server/gin_workspace_terminal.go`
- remove `internal/server/pty_session.go`
- remove the two legacy route registrations from `internal/server/gin_server.go`
- retain and test `internal/terminal/`
- `internal/server/gin_event_stream.go`
- `ui/lib/core/services/event_stream_client.dart`
- a small resource-pressure model under `ui/lib/core/models/`
- `ui/lib/shells/desktop/widgets/dock.dart` and its tests for neutral recovery
  copy with no internal task-pressure terminology

### Service policy, validation, and documentation

- development-unit generation in `Makefile`, including the start-limit recovery
  companion used by alpha testing
- OBS piccolod package `home:atdexterslab/piccolod/piccolod.service`, a focused
  `piccolod-start-limit-recovery.service`, and the corresponding spec install /
  lifecycle entries
- OBS package/spec tests for the service-result gate, current-boot
  health-checker ordering, normal reboot request, and upgrade-time
  stop/reset/start sequence
- `scripts/alpha/dev-vm-alpha-test.sh`
- Piccolo OS mounted-image and post-boot policy validators that already verify
  coordinated Piccolod policy, extended to require the bounded start budget and
  companion recovery unit without changing health-checker's snapshot authority
- `docs/runtime/resource-stewardship.md`
- the RCA and this RFC

The production OBS service is authoritative; the Makefile unit must not be the
only changed service definition.

## Temporal composition

### Canonical lifecycle events

| Event | Authority | State/effect ordering | Durable record | Retry or recovery |
| --- | --- | --- | --- | --- |
| Normal observation | AppManager under `reconcileMu` | Session ready, complete Podman observation, then any allowed effect | Existing app metadata only | Existing reconcile cadence |
| Transient unknown | Observation model | Record unknown; retain status/routes/IDs; perform no app mutation | None | Reobserve after 30 seconds |
| System-wide unknown under task Warning/Critical | Task guard then AppManager | Admission fence precedes skipped Podman work | Journal/telemetry only | Warning may clear; Critical restarts Piccolod |
| Pause or suspension at Warning | Task guard/admission gate, then continuity owner | Fence new child work, publish degraded state, request one coalesced restart handoff, and let already-owned operations keep their existing cancellation/record boundary | Non-secret preparing provider/digest metadata before the legacy-compatible raw wrapped-SDEK blob; finalized expiry metadata after provider deposit succeeds | Remain paused while at/above Warning; missed ticks are coalesced, not queued |
| Resume or reacquisition | Task guard/admission gate, then continuity owner | Two below-40-percent samples publish Normal and reopen admission before one ordinary reconcile is triggered; asynchronously delete the local unused Warning handoff without unkeyed remote revoke | None after local cleanup; provider expiry removes the inert remote factor | Normal callers retry through their existing owner |
| Persistent app-scoped unknown | AppManager and PID 1 | Consume startup attempt; retain publication until empty-cgroup proof; after proof withdraw the authoritatively absent backend, then repair/reobserve and quarantine only after exclusive failure proof | Existing `TransitionRecord` with runtime-recovery operation only if quarantine begins | Existing five-attempt/ten-minute budget and manual Start |
| Shared/global observation failure | AppManager typed-cause aggregator and sentinel | Retain routes/state and veto per-app quarantine | None | Reobserve shared owner; a later successful sentinel may reclassify one remaining app-scoped failure |
| Successful app recovery | Existing AppManager | Recreate, restore routes, publish Running, enter ten-minute probation | Existing container metadata plus recovery record until probation completes | Ordinary probation observation |
| Recovery failure | Existing AppManager | Retain data/quarantine and truthful degraded signal; no false success | Existing startup fields and any active recovery record | Bounded automatic retry; manual retry after error |
| Manual Stop during unknown | Manual lifecycle owner | Persist `Enabled=false`; serialize; quiesce via PID 1; unknown cannot preserve a route against explicit stop | Existing app metadata | Disabled reconcile completes cleanup |
| Manual Start during unknown | Manual lifecycle owner | Persist `Enabled=true`; use same typed observation and one explicit recovery attempt | Existing app metadata and transition record if quarantine is required | Existing manual recovery semantics |
| Follower transition | Cluster owner | Transition fence supersedes local running desire; mutation-free setup/proof failures retain publication; once a grouped graceful stop may have partially changed the backend, publication fails closed before PID 1 fallback, while `Stopped` still requires quiescence proof | Existing cluster/app state | Leader reconcile may restore later |
| Update/rollback overlap | Existing transition owner | Task/app recovery observes the fence and cannot quarantine or recreate; critical process exit relies on existing crash replay | Existing update/rollback record | Existing forward replay or compensation |
| Client cancellation | Request owner | Cancel only its work; never convert to shared missing/error state | None | A later independent owner may retry |
| Daemon graceful shutdown | Main/supervisor | Stop background work, preserve Enabled, use existing app quiescence | Existing metadata/transition records | systemd/manual start later |
| Daemon emergency task restart | Task guard, main, continuity owner, and systemd | Fence admission; arm three-second outer deadline; reuse/finish/last-chance-prepare one handoff; latch final one-second exit; write volatile marker and attempt no-fork census; exit without normal child cleanup; kill old cgroup; start replacement | Compatible raw handoff plus optional metadata when prepared; volatile recovery marker; journal reason; existing app/transaction records | `Restart=always`; provider failure or deadline still exits and falls back to manual unlock |
| Recovery-mode startup | Main, server core, continuity owner, unlock-chain owner, then existing durable owners | Read marker; start guard/auth/API/events/UI and portal-only relay routing; recover with a 20-second caller cap and a separate 30-second execution-liveness fatal bound; reach lifecycle Ready through one joinable complete-unlock owner; bound desired-owner enumeration before exposing successors; promptly reacquire non-suspects, automatically retry suspects/global failures after their bounded recurrence backoff, and publish app aliases only after active route proof | Continuity handoff until lifecycle Ready/expiry plus bounded volatile recovery marker and existing durable owner state | Unlock UI remains reachable on returned failure; Warning pauses recovery intervals; clear marker only after suspect/global stability and a fresh bounded desired-owner pass under Normal/Ready |
| Serialized automatic recovery attempt | Recovery coordinator, existing owner, then main fatal owner | Commit active progress; invoke with finite owner deadline; allow at most five seconds cancellation grace; atomically accept return or commit fatal exit; durably clear before the next owner; a committed post-Ready fatal reuses/prepares continuity within the common three-second exit bound | Volatile active-owner/invocation progress, bounded continuity handoff when prepared, plus existing owner-specific durable state | Returned failure continues truthfully; clear failure exits 76/global; non-returning owner exits through attributed fatal recovery and is retried after non-suspects/backoff; bounded continuity failure exposes manual unlock |
| Watchdog/unexpected service failure | systemd then bounded post-stop marker helper | Missing keepalive or unexpected death triggers old-cgroup cleanup; compare the just-exited `INVOCATION_ID`, preserve a failure already advanced by that invocation, or advance inherited state once using same-invocation progress attribution; restart core; use an existing handoff if present, otherwise expose manual unlock | Volatile recovery marker plus any already-existing continuity handoff; systemd journal | Never manufacture a factor after death; recurrent suspects/global failures use bounded automatic backoff; unexpected-no-handoff cohort is not unattended-recovery success |
| Piccolod start limit reached | systemd, start-limit recovery companion, and existing OS health-checker | Stop process-local retries; queue the companion behind a current health-checker job. After either terminal result, request a normal PID-1 reboot while preserving the failed-boot snapshot decision | No new snapshot or persistent recovery record; existing health-checker marker remains solely upstream-owned | Next boot reruns the ordinary MicroOS health decision; successful HTTP 200 is trusted, non-200 retains upstream retry/rollback/reboot behavior |
| Partial runtime-metadata quarantine | Runtime-recovery record | Phase record precedes each rename/create/commit effect | App-scoped recovery record | Idempotent forward completion or restoration of quarantine |
| Interrupted non-app child owner | Existing onboarding, storage, update, stream, or network owner | No completion is emitted; replacement process re-probes its durable authority according to D6 | Existing owner-specific state, or none for ephemeral work | Retry/reprobe through that owner; no generic task-guard replay |
| Process restart during task Warning | systemd/new AppManager | Old process-local windows vanish; durable desire and transition records remain | Existing durable state | Fresh bounded reconcile window, consistent with 0.2.39 |

### Effect ordering

1. A successful complete observation precedes any normal container or route
   effect. Unknown publication precedes no app mutation.
2. Warning admission closes before degraded health is published and before
   background owners observe the fence. Continuity preparation is requested
   only after the fence and never blocks the guard. On recovery, Normal is
   recorded before continuity cleanup and one reconcile wake-up; missed
   periodic work is never replayed as a backlog.
3. Persistent app recovery consumes one existing startup attempt and persists
   its `TransitionRecord` before metadata quarantine or path rename. PID 1
   empty-cgroup proof precedes every resettable-runtime filesystem effect.
   A follower graceful-stop error is conservatively treated as a possibly
   partial backend mutation: publication is withdrawn immediately, but stopped
   state is not committed unless PID 1 subsequently proves quiescence.
4. Each quarantine phase record precedes its corresponding rename/create
   effect; container metadata and routes commit before known-bad quarantine
   cleanup and record clearing. Cleanup failure retains the existing
   committed-cleanup-pending shape. A failed record write authorizes no next
   effect.
5. Critical admission closes and the three-second outer deadline is armed
   before any continuity, marker, or census work. Continuity gets no more than
   the remaining budget; the final one-second exit is then latched before
   marker/census work. The PCV freeze fence is atomic and non-blocking; a
   pre-freeze `/run` intent transfers any in-flight thaw obligation to
   `ExecStopPost`. None of those completions or blockages can delay exit.
6. Systemd kills/empties the previous service cgroup before the replacement
   process can notify Ready. `ExecStopPost` attempts any pending control-plane
   thaw before service-exit recording, and replacement startup retries a
   retained intent before normal control-plane access. Recovery startup then
   exposes core access, consumes an
   existing continuity handoff with a 20-second cap, and reaches lifecycle
   Ready through the single unlock-chain owner before optional owners requiring
   decrypted state. It then recovers durable transitions and re-observes apps
   before outward Running events and aliases.
7. A start-limit handler cannot pre-empt a pending MicroOS boot-health decision
   and cannot leave an already boot-healthy appliance with Piccolod permanently
   start-limited. Its activation is ordered after the current checker job. Once
   boot health is terminal it requests a normal PID-1 reboot, idempotently with
   the checker's failed-boot action, and never selects or mutates a snapshot.
8. App, onboarding, storage, and update owners never publish completion merely
   because the old Piccolod process exited; their D6 durable authority is
   re-probed first.
9. App alias removal from local acceptance commits before proxy/firewall route
   closure and is not delayed by the addition debounce. Bounded adapter stop is
   attempted before closure; a non-acknowledging upstream may be briefly stale
   but can only reach the fail-closed resolver. Route activation commits before
   alias advertisement; failed activation authorizes no advertisement.

### Execution ownership and cancellation

- The task guard owns one process-lifetime goroutine and its pressure state.
  Supervisor shutdown cancels it. It owns admission state and the one-shot
  Critical signal, but not process restart, app recovery, or systemd policy.
- `cmd/piccolod` alone consumes the one-shot Critical signal, owns the volatile
  recovery marker and absolute emergency deadline, and chooses the emergency
  exit. Server recovery coordinators receive a bounded acknowledgement API into
  this single marker owner; they never write the file independently. It may call
  only the registered continuity capability. Repeated Critical
  samples cannot launch parallel preparation/exit paths or reset the deadline.
- systemd owns watchdog/unexpected-death restart. Its bounded post-stop helper
  may preserve or advance only the non-secret volatile marker for its exact
  `INVOCATION_ID` after old-cgroup cleanup; it cannot access
  SDEK/provider-factor material or claim unattended unlock success.
- systemd also owns the finite start counter and host-reboot transaction. The
  start-limit companion consumes only a systemd-reported Piccolod failure that
  remains terminal after ordinary automatic-restart scheduling, plus the
  terminal current-boot health-checker state. Systemd ordering prevents it from
  running ahead of an already queued checker job. It creates no second health
  result, snapshot decision, rollback state, or restart counter.
- The continuity orchestrator owns one serialized preparation/pickup/cancel
  operation, the local handoff, and exclusive use of the Namek v1 singleton
  slot. The provider owns only factor deposit, pickup, and expiry. Warning/Normal callbacks express desired state; a
  completion must re-check the latest state before retaining or deleting a
  handoff so Warning-to-Normal and Normal-to-Critical races cannot resurrect it.
- The server unlock-chain coordinator owns exactly one storage/persistence/
  lifecycle-Ready execution at a time. Automatic and manual contenders join its
  terminal result; app restore observes lifecycle Ready, not the earlier raw
  lock-state event. It owns the non-resettable 30-second execution-liveness
  timer but only signals the process-level fatal owner; it never calls
  `os.Exit`, starts a replacement, or relies on the service-watchdog loop.
- The initial automatic-recovery coordinator invokes one optional owner at a
  time with committed progress, its finite operation context, five seconds of
  cancellation grace, and atomic return-versus-fatal arbitration. It cannot
  abandon an in-process mutation and continue concurrently; a non-returning
  owner transfers resolution to process exit and the replacement process.
- AppManager remains the sole automatic owner of observation windows,
  persistent-unknown escalation, and runtime-recovery transition dispatch.
  Manual lifecycle and cluster owners supersede it through existing fences.
- Each started child retains its existing command/session/transition owner and
  exactly one wait owner. Warning does not steal cancellation ownership from an
  in-flight operation. Critical process termination supersedes all in-service
  owners; D6 names the durable owner that resumes or discards each result.
- Event-stream clients own only their connection. Disconnect cannot clear
  server pressure state, and reconnect reconstructs it from the guard's initial
  snapshot.

### Concurrency and lock-order constraints

- The task guard never acquires `reconcileMu`, a per-app transition lock,
  FilesystemStateManager `fsMu`, terminal-manager mutex, or network/service
  manager locks while publishing Critical or waiting for process exit.
- Admission state is read without calling back into the guard while an app,
  terminal, runner, or server owner holds a mutually blocking lifecycle lock.
- App runtime recovery holds the existing app lifecycle serialization/fence;
  it cannot overlap another `TransitionRecord` operation for the same app.
  PID 1 and filesystem effects occur in the existing lifecycle order rather
  than while holding pressure/event locks.
- Health/event publication is best effort and cannot block the guard on a slow
  subscriber. The event bus cannot become the restart acknowledgement path.
- The guard invokes Warning/Normal continuity callbacks asynchronously and
  contains a blocked callback. The emergency owner never waits on the guard's
  callback goroutine; `EnsurePrepared` joins/coalesces only through the
  continuity owner and the absolute deadline.
- Continuity attachment atomically replays the guard's latest generation and
  pressure state. A stale pre-attachment Warning cannot prepare after a newer
  Normal or Critical intent.
- Normal resume produces at most one reconcile wake-up regardless of the
  number of periodic ticks skipped during Warning.
- Systemd is the sole service-restart authority. Neither the task guard,
  AppManager, health endpoint, nor UI may invoke host reboot or directly start
  a second Piccolod process. Only the start-limit recovery integration may ask
  PID 1 for a normal reboot, and only after the current boot-health result is
  composed as specified in D6.

### Required adversarial compositions

Implementation tests and release fault injection cover:

1. Anchor inspect succeeds while a service inspect fails.
2. Name resolution fails from EAGAIN, timeout, cancellation, invalid output, or
   a typed not-found result.
3. Task pressure becomes Warning between observation and effect.
4. Task pressure becomes Critical during container recreate, install, image
   update, manifest update, snapshot rollback, terminal creation, diagnostic
   download, and daemon shutdown.
5. Manual Stop or cluster demotion arrives during the 60-second app-unknown
   window.
6. Piccolod restarts after metadata quarantine but before clean-root creation,
   after clean-root creation, and after containers start but before record
   cleanup.
7. The replacement daemon starts while an old direct child is still alive; it
   must not report ready until the old service cgroup has been killed/emptied.
8. A task-warning event is missed by a disconnected UI and the initial event
   snapshot restores the correct dock state on reconnect.
9. The D8 installed-record baseline adds no permanent per-app guard worker.
10. The provider, journald, and event bus block during Critical; Piccolod still
    exits within three seconds and systemd restores core access.
11. Task usage plateaus between 50 and 75 percent after Warning admission;
    sustained high water becomes Critical instead of an infinite fence.
12. The same app/owner, alternating owners A/B, and an unattributed shared leak
    trigger repeated Critical exits within one marker window. Same/A-B
    attribution increments only each bounded suspect entry: non-suspects restore
    promptly and each suspect retries automatically on its escalating schedule.
    Unattributed/shared recurrence applies the equivalent global schedule.
    Another Critical advances exactly one strike for that service invocation
    rather than requiring operator intervention or resetting healthy owners to
    the suspect's full delay.
13. Two apps fail with the same Podman executable/shared-storage cause while
    task pressure is Normal; the sentinel vetoes both metadata quarantines.
14. External listener bind fails during normal or recovery startup; Piccolod
    never sends `SdNotifyReady` and systemd applies the declared restart policy.
15. App restore is suppressed in recovery mode while configured upstream relay
    infrastructure is healthy; the Piccolod portal route reconnects without
    publishing stale app aliases, and aliases appear only after active route
    publication.
16. Warning preparation completes after pressure has returned to Normal; the
    local handoff is deleted, no unkeyed remote revoke is sent, and the inert
    factor expires rather than threatening a later deposit.
17. Warning precedes continuity registration, Warning then Normal precede it,
    and Critical precedes it; attachment replays only the latest generation.
    Critical also overlaps in-flight preparation, locked state, and a blocking
    provider. Exactly one bounded attempt is used and exit always meets the
    three-second outer deadline.
18. A prepared task restart recovers after replacement and races a manual
    password submission. Exactly one complete-unlock body runs; failure after
    unwrap, data-volume activation, and persistence unlock leaves lifecycle
    non-Ready, optional owners/aliases held, and a bounded retryable manual path.
    A stage that ignores cancellation keeps execution ownership after the
    20-second caller cap; manual requests return `recovery_in_progress`, no
    second body starts, and the independent service-watchdog goroutine remains
    healthy. The non-resettable 30-second execution deadline nevertheless
    signals the process fatal owner, which advances the reserved unlock-chain
    suspect and exits within its own bound. The replacement serves locked core
    access during the bounded 30-second/2-minute/5-minute automatic-pickup
    backoff, permits manual unlock, and retries only within the provider expiry.
    Only lifecycle Ready consumes the local handoff.
19. Adapter injection, persisted-config save/reload, port-claim refresh,
    network restart, suspension, failed resume, route removal, and
    Unknown-with-retained-active-route all preserve the single alias-publication
    rule without changing desired aliases. A delayed/blocking adapter cannot
    delay local denial indefinitely or route stale traffic; activation precedes
    advertisement, and failed activation remains withdrawn. An active snapshot
    paused before adapter serialization cannot re-advertise after a newer
    suspension generation; late apply must discard/recompute to inactive.
20. An old writer/new reader and new writer/old reader exchange the raw v1 blob.
    The matrix covers blob absent/present; metadata absent/orphaned/malformed;
    preparing/finalized phase; crashes before raw write, before deposit, and
    before final metadata; digest match/mismatch; recognized/unknown
    schema/provider; and unexpired/expired recognized leases. Digest mismatch
    discards metadata before legacy Namek v1 pickup regardless of recognition,
    whereas matching unknown/malformed metadata fails closed. A matching
    preparing record dispatches only its recorded provider. No plaintext
    sentinel appears in either file.
21. A prepared handoff overlaps graceful Stop, Test, disable, and repeated
    Warning. Stop/repeated Warning reuse it, Test returns busy without provider
    I/O, disable deletes it locally, and no operation overwrites its singleton
    provider slot.
22. Watchdog kills an unlocked process with no handoff and after a recovery
    chain stops responding. A successful emergency/fatal marker write followed
    by post-stop preserves one generation for that `INVOCATION_ID`; an injected
    failed write over an inherited marker makes post-stop advance it once.
    Core access restarts, any existing handoff remains eligible, and the
    no-handoff case truthfully requires manual unlock rather than claiming
    unattended success.
23. The same or alternating owner hangs ordinary recovery starts without a
    task-Critical callback while the task guard and watchdog keepalive remain
    healthy. Its finite operation deadline plus five-second grace atomically
    commits fatal exit; while unlocked, the common fatal owner first
    reuses/prepares restart continuity within its absolute bound.
    Same-invocation active-owner progress lets post-stop
    advance the correct suspect. Non-suspects restore promptly, suspect retries
    use escalating automatic backoff, and unattributed/shared hangs use the
    bounded global schedule.
24. After lifecycle Ready, durable enabled route-bearing app A is the
    deterministic first candidate and later app C is healthy. A's five-second
    qualification slice cannot consume C's context: expiry closes only
    qualification, then ordinary convergence gives C a fresh bound when A
    returns; if A ignores cancellation, fatal recovery attributes it and the
    replacement restores C before retrying A. If A becomes disabled,
    listenerless, or transition-owned between selector snapshot and attempt
    completion, the frozen qualification fails as
    `mixed_health_selector_changed` even if A's new listenerless shape or C
    later converges. The 30-second cohort passes only when A itself remains the
    declared healthy route-bearing candidate, proves active publication, and
    receives the full slice. A lower-ID listenerless app cannot take A's slot;
    a listenerless-only fleet records `listenerless_no_cohort` and still runs
    ordinary recovery.
25. Active progress write fails before an owner starts, and clear fails after a
    successful return. The first owner remains unstarted while core access stays
    live. The second path reuses/prepares continuity within the common fatal
    bound and exits status 76; post-stop ignores stale attribution,
    advances global exactly once, and no later unrelated crash strikes the
    completed owner.
26. Desired-owner enumeration contends on app lifecycle serialization beyond
    its finite bound. Deadline plus cancellation grace uses the shared
    first-fatal latch; no stale successor starts, marker stability does not
    advance, and the volatile marker is not cleared. A returned enumeration
    error remains retryable without falsely requesting fatal exit, while an
    already-committed fatal source still terminates this runner path.

## Alternatives considered

### Raise `TasksMax` or set it to infinity

Rejected. It delays a lifecycle leak, weakens host protection, and leaves the
observation-collapse blast radius unchanged.

### Fix only the legacy PTY

Rejected as incomplete. The PTY is one proven leak mechanism, but the exact
incident accumulator is unavailable and future leaks must be detected before
the sole access path dies.

### Treat every Podman failure as missing or corrupted

Rejected. Resource pressure, cancellation, timeouts, session loss, shared
storage failure, and actual runtime corruption require different authority.
Destructive recovery without empty-cgroup and exclusive-failure proof risks
data and creates an outage amplifier.

### Never act on unknown

Rejected. It is safe for a transient but leaves a genuinely invalid dedicated
runtime permanently unavailable. D3 supplies a bounded escalation with proof.

### Immediately restart Piccolod on the first fork error

Rejected. A typed cgroup high-water signal is earlier and more reliable than
parsing child stderr. Warning load shedding may recover without disruption;
Critical still restarts automatically.

### Reboot the host on the first fault or add SSH/rescue access

Rejected. Host reboot is excessive for a first service-cgroup fault, and an
alternate human access path violates the appliance security/product wall.
After the finite process-restart budget is exhausted, D6 deliberately asks PID
1 for a normal reboot or lets the still-pending OS health checker own failed-
boot recovery.

### Add a separate rescue daemon or per-app watchdogs

Rejected. PID 1 already owns process restart, AppManager owns app recovery, and
per-app workers would scale permanent tasks with installed app count.

### Keep a recovery-factor handoff outstanding for all unlocked uptime

Deferred outside this RFC. It would make arbitrary SIGKILL/watchdog death
unattended-recoverable, but also extends remote-factor exposure from a bounded
restart window to normal operation and adds continual renewal/provider
dependency. This incident amendment prepares at Warning and reuses existing
graceful ceremony; unexpected death without a handoff restores core locked
access and is reported as manual fallback. A standing lease requires its own
explicit security/product decision.

### Persist every unknown observation and task sample

Rejected. Only destructive metadata quarantine needs durable app crash-replay
state, and only the last emergency cause/generation needs volatile
service-restart state. Observation windows and samples remain process-local;
bounded journal/telemetry is sufficient for pressure evidence.

## Implementation sequence

1. Add regression tests demonstrating that inspect/name-resolution errors do
   not authorize mutation, route deactivation, or startup-attempt consumption.
2. Introduce the typed group-observation model and migrate the audited app
   sites before changing recovery behavior.
3. Remove legacy direct terminal routes/files; complete the child-start/wait
   audit and terminal soak test.
4. Add the task guard, shared child-admission gate, health/event snapshots, and
   injectable emergency callback; validate without enabling production exit.
5. Implement the persistent app-scoped unknown window and PID 1
   quiesce/reobserve ladder, shared-cause/sentinel veto, then the
   crash-replayable metadata quarantine.
6. Split core access startup from optional child-producing restore; implement
   the invocation-idempotent volatile recovery marker plus bounded systemd
   post-stop recurrence bridge, bounded suspect/global strike schedules,
   sequential owner/app reacquisition with acknowledged progress and fatal
   liveness arbitration, deterministic first-route phase,
   active-publication alias proof/effect ordering, and marker-clear proof.
7. Generalize the existing auto-unlock transport seam into one provider-neutral
   continuity owner; preserve the raw v1 blob with optional compatible metadata,
   enforce singleton-handoff exclusivity, add the Namek v1 adapter, latest-state
   attachment replay, joinable unlock-chain/Ready gating and fatal execution-
   liveness bound, Warning prepare/Normal cancel races, and bounded recovery
   pickup before wiring it to task pressure.
8. Wire every deliberate fatal source through the common three-second boundary,
   bounded continuity reuse/prepare, final one-second exit, service-level
   watchdog, dock treatment, and production systemd policy, including the
   start-limit/current-boot-health composition.
9. Update runtime documentation and mounted/live image validators.
10. Run focused, repository-wide, race, Flutter, fault-injection, 2/4 GiB,
   owner-interruption matrix, D8 constant-overhead, and 72-hour soak validation.
11. Run scoped code review and RFC implementation closure before release.

## Acceptance criteria

- A failed anchor, service, name, or port observation cannot be represented as
  known missing and cannot trigger stop/remove/create/start, route
  deactivation, status red, or startup-attempt consumption.
- A typed successful absence and known stale state retain the existing bounded
  recreate behavior.
- During transient unknown, existing routes and last-known app state remain;
  the dock shows system degradation through a resource-pressure signal.
- Three persistent app-scoped observations can recover a genuinely invalid
  dedicated runtime only after task-normal, transition-free, session-ready,
  accessible-dependency, successful shared sentinel, exclusive-failure, and
  PID 1 empty-cgroup proofs; an equivalent multi-app/shared failure cannot
  quarantine either runtime. Failed empty-cgroup proof retains the last-known
  route; successful proof withdraws the now-authoritatively-absent backend
  before any later repair/quarantine failure is reported.
- Runtime metadata quarantine uses the existing single `TransitionRecord`,
  survives process interruption at every phase,
  retains persistent data/rootfs/listener desire, and keeps at most one bounded
  quarantine per app until recreate commits or cleanup retry completes.
- Every production child-start site identifies exactly one wait owner and its
  applicable completion/cancellation/disconnect/shutdown events. Changed or
  previously unproven sites have focused tests; unchanged sites may rely on
  existing proof instead of receiving an artificial universal event matrix.
- The legacy direct terminal endpoints and files are absent; known Flutter host
  and workspace terminals continue through persistent session endpoints.
- Repeated supported terminal create/attach/detach/delete/idle-reap cycles do
  not increase the stable zombie, task, or goroutine baseline.
- Warning admission prevents new nonessential child work without disabling the
  in-process HTTP/event access plane or explicit terminal deletion.
- Warning that remains at/above 50 percent for 60 seconds after shedding
  escalates to Critical rather than fencing convergence indefinitely.
- Warning asynchronously prepares at most one short-lived restart handoff when
  unattended unlock is configured and an SDEK/provider are available; returning
  to Normal deletes the local unused handoff, sends no unkeyed Namek v1 revoke,
  and relies on bounded provider expiry without blocking the task sampler.
- At Critical, Piccolod arms a three-second absolute exit boundary, gives an
  unprepared handoff at most two seconds, then latches the final one-second exit
  before attempting a bounded no-secret census. With a responsive journal the
  census is retained even if some `/proc` reads fail; with a blocked provider or
  diagnostics Piccolod still exits by the deadline and requests systemd restart.
- A PCV freeze intent commits under `/run/piccolo` before `FIFREEZE`; the fatal
  owner fences new freezes without waiting for filesystem ioctls. Ordinary
  thaw, systemd post-stop recovery, and earliest replacement startup form the
  ordered cleanup chain. Injected interruption before/inside/after freeze,
  during copy, and during thaw cannot strand an unowned freeze or extend the
  Piccolod exit deadline; non-benign recovery errors retain the intent.
- Recovery-marker failure state is explicit: active progress must commit before
  an automatic owner starts and must clear before the next owner; a pre-start
  write failure keeps the owner unstarted/core reachable, while a bounded clear
  failure exits 76 and makes post-stop advance global rather than stale owner
  attribution. Emergency/post-stop advancement remains once per invocation.
- `auto_unlock_blob` remains byte/AAD compatible in both upgrade directions;
  optional schema/provider/phase/expiry/blob-digest metadata lives in
  `auto_unlock.json`. Preparing provider/digest authority commits before the
  raw blob and final expiry commits after deposit. No blob clears orphan
  metadata; no metadata means legacy Namek v1; any digest mismatch clears
  metadata and treats the current blob as legacy; a matching unknown or
  malformed format fails closed; matching preparing or recognized unexpired
  finalized metadata dispatches only its recorded provider. Namek v1 sees only
  the fresh random factor. No password, plaintext SDEK, or plaintext factor is
  persisted.
- While that local handoff is outstanding, graceful Stop and repeated prepare
  reuse it, Test returns busy without provider I/O, disable/Normal delete only
  local material, and no unkeyed revoke or sequential deposit can clobber its
  singleton Namek v1 slot.
- The old service cgroup is empty before the replacement process reports ready;
  Piccolod binds the external listener before `SdNotifyReady`; core
  authenticated API/event/UI and configured portal-only relay routing start
  before optional child work. Marked recovery attempts envelope pickup and the
  joinable complete-unlock chain with a 20-second cap; automatic and manual
  contenders run one body, a caller timeout never releases a still-running
  owner, lifecycle Ready gates decrypted owners, and only Ready consumes the
  local handoff. A manual contender receives bounded `recovery_in_progress`
  while a context-ignoring body remains active. A separate non-resettable
  30-second execution timer signals a process-level fatal owner and forces a
  bounded marker-advancing exit even while normal watchdog keepalives continue.
  That exit advances only the reserved unlock-chain suspect; the replacement
  exposes locked core access and uses the bounded 30-second/2-minute/5-minute
  automatic retry schedule while the provider lease remains usable, with manual
  unlock available throughout.
  Completion-versus-fatal expiry has one atomic winner; a late successful body
  after fatal commitment cannot publish Ready, consume the handoff, advertise
  aliases, or begin restore.
  Failure after any partial stage keeps optional owners/aliases held and leaves
  a reachable unlock UI with a bounded retry path; it is not counted as
  end-to-end recovery success. After Ready, enabled apps/routes and their
  aliases converge through existing durable desire and typed observation.
- Every serialized automatic recovery owner has a finite operation deadline,
  at most five seconds of cancellation grace, and atomic return-versus-fatal
  arbitration. A context-ignoring owner causes attributed process exit even
  while watchdog/task loops are healthy; the replacement restores non-suspects
  before retrying it. An owner with no finite bound is not admitted.
- Every deliberate fatal request shares the task-Critical three-second owner.
  Post-Ready owner-liveness and status-76 exits reuse an outstanding handoff or
  give continuity at most two seconds to prepare one before the final-second
  exit; the locked unlock-chain fatal preserves its existing handoff. With a
  configured healthy provider this is a release-gated automatic-unlock path;
  bounded failure still restarts core and requires manual unlock.
- Persisted desired aliases survive unchanged; portal aliases publish at core
  bootstrap, while every adapter configure/refresh/reconnect path publishes an
  app alias only with active-publication proof. Suspension/removal withdraws
  local resolver/TLS-mux acceptance before route closure without debounce and
  performs bounded adapter cancellation; a stale upstream registration can only
  fail closed. Resume advertises after successful activation; failed resume
  remains withdrawn; Unknown retains only an actively published last-known
  route. Every adapter apply revalidates a monotonic runtime-projection
  generation under its serialization owner, so an older configure/reconnect
  snapshot cannot become final after a newer withdrawal.
- The marker advances at most once per systemd invocation. Same or alternating
  attributed recurrence applies the 10-minute/30-minute/2-hour/6-hour schedule
  only to bounded suspects while non-suspects restore promptly; unattributed,
  shared, or overflow recurrence applies the equivalent global schedule.
  Every hold ends in automatic serialized retry, Warning pauses its interval,
  and durable desire/data remain unchanged. The marker clears only after every
  strike has cleared through successful stability and a fresh desired-owner
  pass, or after host reboot.
- Every D6 non-app child owner with externally visible or durable effects
  suppresses premature completion and reaches its stated retry/reprobe state
  after critical interruption.
- `TasksMax=15%`, `WatchdogSec=60s`, `Restart=always`, `RestartSec=5s`,
  `KillMode=control-group`, the bounded invocation-idempotent post-stop marker
  helper, and the three-start/fifteen-minute budget are proven in the
  authoritative package, mounted image, and live boot. A start-limit hit before
  boot-health completion queues behind health-checker; after either terminal
  result it requests a normal PID-1 reboot without changing the upstream
  snapshot decision. Both successful emergency writes
  and failed writes over inherited markers advance exactly once for the exited
  invocation. Watchdog/unexpected death without a prepared handoff restores core
  locked access and advances recurrence state but is never counted as successful
  unattended unlock.
- Task Warning does not make `/health/ready` fail boot health or trigger the OS
  rollback owner. The OS accepts HTTP 200 without independently inspecting
  locked state, apps, the recovery provider, or external connectivity.
- The task-pressure state is present in the initial event snapshot and combines
  with listener/portal health using the existing dock pill treatment. Actual
  disconnect is Offline; connected snapshot assembly is Checking; hydrated
  states use the neutral D7 copy and never expose internal task-pressure or
  suspected-culprit terminology.
- On 2 GiB and 4 GiB, first-generation task-Critical injections with a healthy
  provider and a healthy lowest-id durable Enabled route-bearing candidate
  leave and use its full five-second route slice within the 30-second p95
  unlocked first-route target. The candidate remains Enabled, transition-free,
  route-bearing, and actively published at qualification completion. Slice
  expiry closes only qualification; ordinary convergence continues with fresh
  bounds. Recurrence containment, mixed-health convergence, and
  unexpected-no-handoff/manual fallback are measured as separate cohorts and
  are not counted as that success. The result is qualification evidence, not a
  five-nines achievement claim. Stage telemetry proves detection-to-exit,
  restart delay, construction-to-core-Ready, pickup-to-lifecycle-Ready, and
  Ready-to-the-deterministic-first-enabled-route-bearing-app rather than
  treating the independent safety caps as an arithmetic allocation.
- The first-route acceptance matrix includes a lower-ID listenerless app before
  the selected route-bearing app, a listenerless-only fleet, and Enabled,
  listener-shape, or transition-owner change between selector snapshot and
  qualification completion. The first case still qualifies the selected route
  app; the second records `listenerless_no_cohort`; every selector-drift case
  records `mixed_health_selector_changed`, cannot enter the healthy p95
  denominator, and still permits ordinary recovery from fresh durable truth.
  A runner-level refresh case proves that a later route-bearing app receives an
  ordinary fresh bound without inheriting qualification authority or another
  five-second slice; a listenerless-only initial snapshot likewise cannot gain
  a qualification cohort later in that recovery run.
- The D8 constant-overhead baseline passes with 100 installed metadata records,
  a bounded active subset, and no permanent per-app task-guard worker.
- Existing 0.2.39 OOM score, session repair, quiescence, startup probation, and
  transition-replay tests remain green.
- `go test ./...`, relevant `go test -race` packages, `go vet ./...`, Flutter
  tests/analyze, package validation, mounted-image validation, live watchdog
  injection, live task-pressure injection, and `git diff --check` pass.

## Rollout and observability

The first release is canaried on a 2 GiB appliance and a 4 GiB appliance before
general alpha publication. Canary telemetry records only:

- task high-water percentage and configured limit;
- Warning/Critical transition counts;
- emergency restart reason, continuity prepare outcome, detection-to-core-ready,
  detection-to-unlocked, per-route restoration, and eventual-convergence
  durations, with Critical detection time kept distinct from marker/exit time;
- qualification cohort (`task_first_generation`, `recurrence_containment`, or
  `unexpected_no_handoff`) so probation/manual fallback cannot be mixed into the
  30-second unattended-recovery statistic;
- non-secret first-route qualification outcome (`eligible_pass`,
  `listenerless_no_cohort`, or `mixed_health_selector_changed`) so definition
  or transition drift and no-route fleets cannot enter the healthy p95
  denominator by omission;
- aggregate top contributor comm/state/thread counts captured locally in the
  diagnostic journal; and
- app recovery outcome categories without arguments, environment, terminal
  content, or app data.

The release blocks if normal idle/reconcile behavior enters Warning, if any
child/zombie baseline grows during soak, if a critical injection can strand the
unit in failed/start-limited or repeated eager-restore state, if a blocked
provider/journal can extend the three-second emergency deadline, if a configured
healthy provider cannot restore unlock and enabled services within the p95
budget, if locked core Ready is miscounted as successful recovery, if alias
publication outruns active route proof, if readiness causes an OS rollback at
Warn, if start-limit recovery can pre-empt pending boot-health or leave a
boot-healthy device permanently start-limited, or if a transaction cannot
replay after emergency interruption.

Rollback is package/image rollback. The app observation model adds no durable
migration. A runtime-recovery record is forward/backward handled by the
coordinated release until its quarantine is resolved; rollback cannot discard
an active record or delete its quarantine. If automatic OS rollback crosses an
active runtime-recovery record, the older strict decoder may fence that one app
rather than guess; it must preserve the quarantine and leave core access and
other apps available. The release artifact must retain a forward-update path
that can resume or explicitly restore the record. Canary fault injection
measures the active-record window; this bounded fail-closed compatibility risk
is accepted over making an older binary mutate unknown recovery state.

## Review and closure gates

- Structured RFC review must verify scope, typed authority, lifecycle
  ownership, systemd semantics, package ownership, and validation altitude.
- Adversarial review must attempt to produce data loss, route loss, restart
  loops, readiness/rollback coupling, unbounded per-app work, or a permanent
  unknown state through temporal compositions.
- Subtractive review must remove any new state, site, event, or abstraction
  that is not required by the product and safety invariants above.
- Implementation review must use this RFC as behavior context.
- RFC implementation closure must prove every decision and acceptance gate
  against the final code, package, image, and live fault-injection artifact.

## Structured review ledger

### Amendment checkpoint — 2026-07-21

- **Trigger:** post-alpha review proved that adapter alias publication bypassed
  route proof and that emergency exit bypassed configured auto-unlock.
- **Classification:** both are local in-scope effect-authority defects, not new
  product scope. Persisted desired state cannot itself authorize either remote
  alias publication or a claim of recovered service availability.
- **Authorization:** the product owner explicitly authorized autonomous handling
  of the alias defect and confirmed the provider-neutral unlock-continuity
  contract, bounded Namek v1 recovery, manual-unlock fallback, and non-technical
  UI copy.
- **Action:** the initial alias filter separates desired aliases from adapter
  snapshots, but review found that retained/deactivated registry endpoints still
  passed proof and removal was debounced. The amended contract now requires
  active-publication proof and withdraw-before-close ordering without new
  durable state. Restart-unlock continuity implementation and the fresh review
  epoch remain open.
- **Review obligation:** focused verification closes neither discovery nor
  convergence. After the continuity implementation, restart the standard code,
  security, UX, gating, and RFC-closure cadence from a fresh discovery pass.

### Amendment review pass 1 — 2026-07-21 (superseded by pass 2)

Fresh structured and adversarial review returned **RED** with four blocking
continuity findings and significant alias/timing findings. This revision closes
them at RFC altitude as follows:

1. Namek v1 is explicitly one unkeyed singleton slot. An exclusive prepared
   handoff blocks Test/overwrite, graceful Stop reuses it, and cleanup deletes
   only local material while the remote factor expires; no late unkeyed revoke
   can erase a newer handoff.
2. Upgrade/rollback preserve the released raw blob and AAD. Optional metadata
   lives in the backward-compatible state JSON; missing metadata is legacy
   Namek v1 and unknown metadata fails closed.
3. Automatic/manual unlock share one joinable chain owner, lifecycle Ready is
   the decrypted-owner barrier, and partial-chain failure retains a bounded
   retry path without publishing apps or aliases.
4. Pressure state/generation is latched before attachment and atomically replayed.
5. Marker generation two introduces a global optional-owner safe-start breaker,
   independent of culprit attribution; ten minutes continuously Normal/Ready
   then automatically resumes serialized reacquisition of every desired owner,
   including the attributed culprit, so the hold is bounded.
6. Alias authority is active publication, not registry presence; local
   acceptance is denied before closure, adapter cancellation is bounded, stale
   upstream traffic fails closed, and additions follow successful activation.
7. The 20-second pickup value is a failure cap, while the 30-second p95 gate is
   measured from explicit stage telemetry rather than inferred by addition.

Verification review remains required before implementation proceeds.

### Amendment review pass 2 — 2026-07-21

Fresh whole-RFC structured and adversarial review remained **RED**. It proved
that pass 1's global probation merely rate-limited a deterministic culprit,
that a hung unlock body could coexist with healthy watchdog keepalives, and
that marker/first-route wording depended on state the replacement could not
reconstruct. This revision closes those findings at RFC altitude:

1. Complete-unlock execution has a separate non-resettable 30-second liveness
   deadline and a process-level fatal owner; it does not depend on the
   independent watchdog goroutine becoming unhealthy. A reserved continuity
   suspect then provides bounded automatic retry while locked core/manual
   access remains available, instead of repeating a 30-second crash loop.
2. Marker advancement is idempotent per systemd `INVOCATION_ID`, distinguishes
   active-owner progress from an already-recorded failure, and specifies both
   successful and failed emergency-write composition with post-stop.
3. A bounded suspect ring gives recurring attributed owners escalating
   automatic backoff while restoring non-suspects promptly. Unattributed,
   shared, or overflow recurrence receives the equivalent bounded global
   schedule; durable desire is never changed.
4. First-route qualification selects the lowest-id durable Enabled record whose
   current definition is route-bearing, reserves one five-second slice, and
   then hands ordinary convergence fresh per-owner bounds. Listenerless-only
   fleets skip the qualification cohort but still converge under ordinary
   per-owner bounds. Selection makes no claim based on process-local last-known
   state.
5. Blob reconciliation now orders absence, digest comparison, format dispatch,
   and expiry for every recognized/unknown/malformed combination.
6. Unlock success and fatal expiry now have one atomic commit point; a late
   result cannot publish Ready or consume the handoff after fatal wins.
7. Active progress is acknowledged before start and after return. Pre-start
   write failure starts no owner; clear failure exits with a distinct status so
   post-stop advances global instead of stale attribution.
8. Every automatic recovery owner has a finite operation deadline, five-second
   cancellation grace, and return-versus-fatal arbitration, closing hangs that
   leave the independent watchdog healthy.
9. Every deliberate post-Ready fatal exit shares bounded continuity prepare,
   and adapter apply revalidates a monotonic active-publication generation so a
   pre-suspension snapshot cannot overwrite a newer withdrawal.

Verification review remains required before implementation proceeds.

### Amendment phase-1 convergence — 2026-07-21

After the pass-2 closures, fresh whole-artifact structured and adversarial
review both returned **GREEN**. The final verification explicitly covered
unlock completion-versus-fatal arbitration, healthy-watchdog hangs, marker
write/clear failure, exactly-once post-stop advancement, deliberate post-Ready
continuity, suspect/non-suspect ordering, deterministic first-route
continuation, blob compatibility, and alias suspension/apply-generation races.
No remaining product decision was identified; alias publication remained a
technical active-route authority invariant. Subtractive review is the next
gate; implementation remains paused until phase-3 regression also converges.

### Amendment subtractive pass — 2026-07-21

The independent minimality review returned **EXCESS** with no blocking finding
and two significant reductions. Both were applied: the child-start audit still
requires a complete inventory and exactly-one-`Wait` proof, but bespoke
five-event tests are limited to changed or previously unproven ownership; and
unused handoff `generation`/`purpose` metadata was removed, leaving only
schema/provider/expiry/blob-digest fields consumed by compatibility behavior.
An acknowledged suggestion to compress the historical review ledger was not an
implementation obligation; the ledger remains explicitly superseded/current-
status labelled as the audit trail for this incident. Phase-3 structured and
adversarial regression is pending.

### Amendment UX pass — 2026-07-21

The independent UX review found three significant presentation gaps in the
dirty implementation: internal task-pressure terminology, a connected/pending
state labelled Offline despite a contradictory tooltip, and model-only tests
that did not cover the visible sequence. D7 now reserves Offline for disconnect,
uses muted Checking during connected snapshot assembly, specifies neutral
wait-versus-act copy, retains last-known app cards, and requires widget/forbidden-
term coverage. It adds no new recovery screen, counter, culprit UI, or workflow.
The existing neutral Unlocking/manual-unlock surfaces remain unchanged.

### Amendment final RFC convergence — 2026-07-21

After applying the subtractive and UX findings, phase-3 whole-artifact
structured and adversarial review both returned **GREEN**, followed by a
**GREEN** UX verification of the amended D7 flow. No blocking or significant
RFC finding remains. The amendment is ready for implementation; this verdict
does not satisfy the later code, security, dirty-tree gating, RFC-to-code
closure, package/image, live fault-injection, capacity, soak, or canary gates.

### Review pass 1 — 2026-07-19

Root-cause verdict: **ROOT-LEVEL**. The RFC addresses the code-proven child
lifecycle defect, the absent service-level pressure boundary, and the
observation-authority amplifier rather than raising the task ceiling alone.

Resolved findings:

1. **blocking × in-scope — parallel recovery ownership:** the first draft
   proposed a separate runtime-recovery record despite the existing single
   per-app `TransitionRecord`. D3 now reuses that owner with a new operation
   kind and explicit fencing/recovery dispatch.
2. **blocking × in-scope — incomplete admission surface:** the first draft
   named a shared Warning gate without assigning all child-producing work
   classes. D5 now contains an owner/site/fail-direction matrix and the site
   list names the direct seams.
3. **blocking × in-scope — emergency restart composition:** the first draft
   relied generally on app transition replay while omitting onboarding,
   storage preparation, OS update, streams, and network helpers. D6 now states
   the post-restart authority/outcome for every retained owner and blocks
   release if fault tests disprove an existing contract.
4. **significant × in-scope — partial event snapshot:** the first draft did not
   prevent one resource event from clearing the dock's pending state before
   listener health arrived. D7 now requires a per-topic snapshot barrier and
   defines disconnect/live-event ordering.

Pass-1 verdict after resolution: pending verification review.

### Review pass 2 — 2026-07-19 verification

- Root cause: **ROOT-LEVEL**
- Reuse: **APPROPRIATE** — PID 1, AppManager, the existing transition record,
  pressure events, health tracker, event stream, terminal manager, and systemd
  remain the relevant owners.
- Approach: **OPTIMAL within scope** — a finite limit plus early guard,
  observation safety, bounded app recovery, and automatic service restart
  closes all three causal layers without an alternate access daemon.
- Novel pattern: **SOUND** — the task guard is constant-size and has no restart
  or app authority; runtime recovery extends the existing exclusive transition
  owner.
- Failure modes: **THOROUGH**
- Decision surface: **EXPLICIT**
- Temporal composition: **EXPLICIT**
- Assumptions: **WELL-GROUNDED**, with threshold/kernel and service-policy facts
  held as live release gates rather than inferred guarantees.
- Operational readiness: **PRODUCTION-READY at RFC altitude**

No blocking or significant structured-review findings remain. Final structured
verdict: **GREEN**.

### Review pass 3 — post-adversarial verification

The structured checklist was repeated after adding the emergency deadline,
volatile recovery marker, safe startup ordering, sustained-Warning escalation,
shared-failure veto, listener Ready proof, and portal-bootstrap split.

- Scope/reuse: **PASS** — the only new cross-restart state is a volatile global
  marker owned by main; durable app effects remain in the existing transition
  owner.
- Dependency/order model: **PASS** — guard and authenticated portal access
  precede optional child work; app aliases and Running events follow typed route
  proof.
- Recovery ownership: **PASS** — main owns exit/marker, systemd owns restart,
  AppManager owns app recovery, and existing durable owners retain replay.
- Failure/rollback behavior: **PASS** — diagnostic blockage, repeated culprit,
  shared failure, listener bind failure, and older-binary fencing are explicit.
- Acceptance/site alignment: **PASS** — every new decision has an
  implementation site and fault-injection gate at RFC altitude.

Post-adversarial structured verdict: **GREEN**.

## Adversarial review ledger

### Red-team pass 1 — 2026-07-19

The pass constructed the following concrete failure chains. All are in the
incident/access-plane scope.

1. **blocking × in-scope — diagnostics can defeat emergency restart**
   - Location: D5/D6 Critical boundary.
   - Scenario: task guard enters Critical -> synchronous census journal write
     blocks on journald -> the independent watchdog loop continues keepalives
     -> systemd never restarts the only access plane.
   - Likelihood: plausible under the same resource pressure or filesystem/log
     backpressure that motivates the guard.
   - Suggested resolution: latch exit before diagnostics and enforce a deadline
     independent of log/event/watchdog acknowledgements.
   - Resolution: Critical now signals a pre-existing emergency owner first;
     marker/census output is best effort and a one-second deadline cannot be
     extended.

2. **blocking × in-scope — eager restore creates an infinite restart loop**
   - Location: D6 startup composition and current `NewGinServer`
     `RestoreServices` ordering.
   - Scenario: one restore owner/app drives task growth -> guard restarts ->
     process-local budget is lost -> eager restore runs before access is served
     -> same owner drives Critical again -> unlimited systemd retries never
     yield user access.
   - Likelihood: high for a deterministic lifecycle leak or broken helper.
   - Suggested resolution: carry a volatile restart cause across service
     restarts, expose core access first, serialize recovery, and suppress a
     repeated culprit.
   - Resolution: `/run/piccolo/task-recovery.json`, recovery-mode core startup,
     one-at-a-time owner/app reacquisition, and the repeated-culprit circuit
     breaker now define that path.

3. **blocking × in-scope — stable Warning becomes permanent outage**
   - Location: task-pressure state machine and D5 admission.
   - Scenario: a leak plateaus at 55 percent -> Warning fences Podman and
     restore work -> usage never falls below 40 percent and never reaches 75
     percent -> apps needing convergence remain unavailable forever.
   - Likelihood: plausible for leaked zombies/threads with no active producer.
   - Suggested resolution: escalate sustained post-shedding high water.
   - Resolution: 60 seconds continuously at/above 50 percent after Warning
     admission is now Critical with reason `sustained_high_water`.

4. **blocking × in-scope — shared failure causes sequential app destruction**
   - Location: D3 exclusive-failure proof.
   - Scenario: the Podman executable or shared storage fails identically while
     the task guard is Normal -> every app reaches the three-observation window
     -> each is misclassified as a bad dedicated runtime -> runtimes are
     quarantined one by one and all apps go down.
   - Likelihood: plausible because shared control-plane failures present at an
     app-scoped call site.
   - Suggested resolution: canonical typed causes, a cross-app window, and a
     shared-dependency sentinel that vetoes quarantine.
   - Resolution: D3 now requires both cross-app isolation and a successful
     scratch-runtime sentinel before any app metadata effect.

5. **blocking × in-scope — systemd Ready can precede a usable listener**
   - Location: D6 and current `GinServer.Start` ordering.
   - Scenario: Piccolod sends `READY=1` -> external listener bind fails -> boot
     health treats the sole access plane as ready although no client can
     connect.
   - Likelihood: uncommon but deterministic under a port/listener fault.
   - Suggested resolution: bind first and make successful bind part of the
     control-plane Ready proof.
   - Resolution: D6, sites, tests, and acceptance now move `SdNotifyReady` after
     external listener bind and keep optional app restore outside that proof.

6. **blocking × in-scope — local core Ready still leaves the only configured
   remote path behind app restore**
   - Location: D6 and current `refreshRemoteRuntime` ordering.
   - Scenario: recovery mode correctly defers app restore -> persisted portal
     base/TLS mux setup is deferred with it -> local listener is healthy but the
     user cannot reach it through the configured relay -> the nominally safe
     restart still violates the no-alternate-access contract.
   - Likelihood: high on appliances operated through their remote portal.
   - Suggested resolution: bootstrap the portal-only relay route as core access
     and defer only app aliases until their routes are proven.
   - Resolution: D6 now splits remote runtime refresh into portal bootstrap and
     post-observation app-alias phases; relay connection is tested in recovery
     mode without making external infrastructure a systemd Ready dependency.

7. **significant × in-scope — rollback crosses an active new recovery record**
   - Location: D3 and rollout.
   - Scenario: metadata is quarantined -> OS rolls back to 0.2.39 -> the older
     binary cannot interpret the runtime-recovery phase -> that app remains
     fenced with no SSH fallback.
   - Likelihood: narrow but credible during canary/update rollback.
   - Suggested resolution: define older-binary fail-closed behavior, preserve
     core access/quarantine, retain a forward repair path, and measure the
     active-record window.
   - Resolution: rollout now explicitly accepts one-app fail-closed fencing
     instead of unsafe mutation, requires core/other-app availability and a
     forward-update recovery path, and adds canary fault injection.

Pass-1 verification after these changes found no remaining chain that can
produce data loss, all-app false teardown, permanent task fencing, or an
unbounded automatic restart loop within the declared scope. Adversarial
verdict: **GREEN**, subject to the stated live fault-injection gates.

## Minimality review ledger

### Subtractive pass — 2026-07-19

1. **acknowledged × adjacent — future active-capacity measurement:** D8 asked
   this release to collect active-app scaling data for the separate
   capacity/offloading RFC. That work was not needed to prove constant task
   overhead for 100 installed records. Suggested resolution: remove it from
   this RFC and leave active-capacity/offloading qualification to its own
   scope. **Resolved by removal.**
2. **strength × in-scope — bounded recovery state:** observation windows remain
   process-local, task recurrence uses one volatile global marker, and only
   destructive app metadata quarantine extends the existing durable per-app
   transition record.
3. **strength × in-scope — defensive surfaces trace to demonstrated chains:**
   the owner interruption matrix, safe-start mode, shared-failure veto,
   snapshot barrier, and portal-bootstrap split each close a documented
   blocking/significant review scenario rather than adding generic framework
   work.

No blocking or significant subtractive finding remains. Minimality verdict:
**MINIMAL**.

## Original RFC final review verification — superseded by 2026-07-21 amendment

After the subtractive removal, structured and adversarial checks were repeated:

- Structured regression: **GREEN** — the removal changes no incident
  obligation, owner, decision, site, temporal edge, or acceptance gate.
- Adversarial regression: **GREEN** — all seven documented failure chains
  remain closed; the removed future capacity measurement protected none of
  them.
- Review execution note: the specialized role contracts were executed
  serially in this session because independent reviewer delegation was not
  authorized. The process retained role/checklist separation, but reviewer
  independence was therefore degraded.

Original RFC verdict: **READY FOR IMPLEMENTATION AUTHORIZATION**. The
2026-07-21 amendment reopens this verdict until its own structured,
adversarial, and subtractive reviews converge.

## Repeated-start / boot-health composition review — 2026-07-22

**blocking × in-scope — unconditional service policy can bypass or strand OS
recovery**

- **Location:** D6 production service policy and MicroOS boot-health boundary.
- **Scenario:** a new Piccolod build repeatedly fails before stable access ->
  unlimited restart attempts leave the only access plane unavailable forever;
  simply restoring unconditional `StartLimitAction=reboot-force` instead lets
  a fast start loop reboot before health-checker finishes its current-boot
  snapshot decision -> the same snapshot can reboot repeatedly without the
  established rollback owner getting a terminal result.
- **Likelihood:** plausible for a deterministic startup regression or repeated
  service-watchdog failure; impact is an extended appliance outage.
- **Suggested resolution:** retain a finite start budget, but route its terminal
  action through a companion that waits for the current boot-health decision.
  Pending/failed boot health remains owned by MicroOS; an already accepted boot
  requests normal PID-1 reboot; an unobservable result is bounded by the
  existing health-checker timeout.
- **Resolution:** incorporated into the scope, product contract, D6, site list,
  temporal composition, acceptance criteria, and release gates above. The
  implementation review additionally replaced a free-running concurrent wait
  with `After=health-checker.service`, closing the timer-skew race in which a
  companion started first could otherwise reach its own bound before the
  checker's five-minute deadline.

Structured verification finds the ownership split explicit: Piccolod owns the
meaning of HTTP readiness; health-checker owns snapshot/rollback state; systemd
owns service starts and host reboot. The companion introduces no new persistent
counter or rollback authority. Adversarial verification finds no remaining
terminal combination in which pending boot-health is pre-empted or accepted
boot-health leaves Piccolod permanently start-limited. Minimality verification
retains the companion because a static unconditional start-limit action cannot
satisfy both obligations. Plan verdict: **GREEN**. Strict mounted-image and the
remaining live release-qualification proof remain blockers.

## Implementation Notes & Status

The original RFC was implemented on 2026-07-19 across the Piccolod tree and its
local package/alpha seams in commit `49ee693`; the unlock-before-enumeration
correction is commit `12b85d3`. The authoritative package integration was
published as OBS source revision 78 and built as `piccolod-0.2.40-1.1` for both
architectures. Piccolo OS commit `965711b8` enforces the mounted-image policy,
and OBS image revision 21 synchronizes the already-committed final bootstrap-
DNS seed before rebuilding all image profiles. Strict mounted-root validation
and the remaining live qualification are not implied by these publication
facts.

Post-alpha review on 2026-07-21 found two incomplete effect boundaries. The
self-hosted Nexus adapter derived aliases directly from persisted desired
configuration, and the emergency one-second exit bypassed the existing graceful
auto-unlock ceremony, so the alpha helper's automatic password submission had
masked a locked replacement. The local implementation now closes both: alias
publication is derived from active runtime projection with apply-generation
ordering and fail-closed withdrawal, while deliberate restart paths use the
provider-neutral bounded handoff and replacement pickup flow. The prior alpha
run by itself remained insufficient evidence for unattended unlocked service
recovery; the exact candidate-RPM qualification recorded below later closed
that live gate.

Code, security, and UX review were repeated after implementation. Their local
blocking/significant findings were resolved: live runtime-pressure events now
apply per-app authorization; emergency attribution retains the active owner
when the bounded census is late; lifecycle-owner claims cover actual work
rather than idle watchers; startup, Warning, and Critical use independent
admission fences; update markers survive admission/probe uncertainty; optional
child-producing owners begin only after core Ready; and runtime-quarantine
tests now cross every durable crash boundary through final cleanup. Final
dirty-tree gating also found and fixed stale resource-pressure state across UI
event-stream reconnects; the client and dock now clear disconnected state,
hydrate current task pressure from the guard, and hydrate mutable app-scoped
suppression from AppManager snapshots.
The final temporal pass found and fixed three further composition defects:
follower grouped stop could partially quiesce a backend while retaining its
route; the five-second qualification could consume and delay the same app's
ordinary recovery pass; and recovery telemetry mislabeled marker-write time as
Critical detection while omitting per-route/final convergence truth and then
initially derived route completion from stale enumeration-time shape. The
implementation now fails publication closed after an uncertain partial stop,
keeps failed qualification accounting separate from ordinary recovery, and
emits exact detection, continuity/task, unlock, per-route, and terminal stages;
route completion comes only from the fresh attempt's route-bearing plus active
publication result.
Exact package qualification then exposed one more temporal dependency: desired
owner enumeration requires decrypted lifecycle state, but the recovery runner
had selected enumeration before the unlock owner on a locked replacement. The
runner now exposes only the existing unlock owner until Ready, then exposes
only enumeration before the remaining owners. Focused/race tests and repeated
exact-RPM boots prove the corrected order without adding a second unlock or
enumeration authority.
Code, security, UX, minimality, and temporal-convergence roles were exercised by
independent local reviewer agents. No external review CLI was used. There are
no product or scope deviations. The accepted correctness fix schedules the
provider-neutral `unlock-chain` before decrypted desired-owner enumeration on
locked recovery starts; all other core owners remain Ready-gated.

### RFC-to-code trace

| RFC reference | Expected behavior | Implementation evidence | Status | Notes |
| --- | --- | --- | --- | --- |
| Product contract and invariants 1–4 | Piccolod remains the only access plane; unknown observation retains app truth/routes; destructive recovery needs exclusive attribution and PID 1 quiescence proof | `internal/app/container_group_observation.go`; `internal/app/container_group_reconcile.go`; `internal/app/runtime_unknown_recovery.go`; `TestReconcileUnknownObservationPreservesRunningProjectionRoutesAndAttemptBudget`; `TestRestoreServicesPreservesExistingRouteWhenObservationIsUnknown` | satisfied | No alternate rescue daemon or access path was added. |
| D1 — observation authority | Exactly five typed outcomes; only typed successful absence is missing; partial/error results are unknown | `internal/app/container_group_observation.go`; strict Podman parse/not-found boundaries in `internal/container/podman.go`; Podman and reconciliation tests | satisfied | Invalid output is typed and never converted to absence. |
| D2 — last-known state and routes | Unknown cannot mutate containers/routes/status or consume an attempt; retries use existing serialization | `internal/app/app_manager.go`; `internal/app/container_status.go`; `internal/app/container_group_reconcile_test.go` | satisfied | Runtime uncertainty is projected through resource pressure, not false per-app red state. |
| D3 — persistent app-scoped recovery | Three/60-second isolated failure window; Normal/session/dependency/transition/shared-sentinel proofs; retain publication until PID 1 empty-cgroup proof, then fail closed for the absent backend; single crash-replayable transition; at most one quarantine; persistent data retained | `internal/app/runtime_unknown_recovery.go`; runtime-recovery additions in `internal/app/installed_app_transition.go`; quiescence/publication, follower fallback, and durable-phase fault tests | satisfied | Live phase-interruption fault injection remains a release gate below. |
| D4 and invariant 5 — child lifecycle | Remove legacy direct PTY endpoints; supported sessions retain caps/reaper and exactly one `Wait` owner | Deleted `gin_terminal.go`, `gin_workspace_terminal.go`, and `pty_session.go`; `internal/terminal/session.go`; `internal/terminal/manager.go`; `TestManager_RepeatedCreateDeleteReapsChildren`; child-start audit in touched seams | satisfied | The 72-hour task/goroutine/zombie soak remains pending. |
| D5 — task guard and Warning admission | One constant-size direct cgroup-v2 sampler; exact thresholds/hysteresis; no-fork bounded census; shared child admission; retryable 503; sustained Warning becomes Critical; Warning prepare and Normal cancel callbacks never block sampling | `internal/resources/pressure/task_guard.go`; `internal/resources/pressure/admission.go`; `internal/autounlock/task_pressure.go`; `cmd/piccolod/task_fatal_owner.go`; guard/admission/callback/first-fatal/race tests | satisfied | Sampler pressure, monitor health, startup admission, and process-fatal admission have separate release authority. Critical commits its exact snapshot to the shared producer-side first-fatal latch before callbacks or census. Live renewed qualification remains a release gate. |
| D6 — emergency restart marker | Three-second outer deadline, at-most-two-second last-chance continuity, final one-second exit; invocation-idempotent atomic marker; PCV pre-freeze intent and independent post-stop/earliest-startup thaw recovery; acknowledged active progress with exit-76 global fallback; bounded suspect/global schedules; prompt non-suspect restore and automatic retry/clear proof | `cmd/piccolod/task_recovery.go`; `task_fatal_owner.go`; `internal/pcv/control_plane_thaw.go`; recovery controller/runner/server and PCV freeze-interruption tests; marker, backoff, continuity, and timing tests | partial | Local process-exit and cross-systemd thaw ownership are implemented. A direct real-filesystem/systemd task-exhaustion interruption on the final package remains required; the package start-limit reboots below prove the outer replacement boundary but not this exact fault. Permanently uninterruptible filesystem ioctls remain outside the process-recovery fault model. |
| D6 — restart unlock continuity | Provider-neutral prepare/recover/cancel; compatible raw v1 blob plus ordered digest/format reconciliation; singleton-handoff exclusivity; Namek v1 expiry cleanup; joinable unlock chain and Ready barrier; caller timeout retains execution ownership; atomic completion-versus-fatal liveness bound and reserved-suspect retry; no plaintext secret persistence; 20-second pickup failure cap and manual fallback | `internal/autounlock/`; `internal/server/gin_crypto_handlers.go`; `internal/server/recovery_execution.go`; `internal/app/app_manager.go`; `internal/server/gin_server.go`; `cmd/piccolod`; `TestTaskRecoveryRunnerUnlocksBeforeDesiredOwnerEnumeration`; unit and race matrices; exact 0.2.40 candidate-RPM alpha boots | satisfied | The exact candidate RPM moved locked-to-unlocked without password input in two seconds, repeated after install-from-start-limit recovery, and reached unlocked Ready after both real-reboot branches. Standing all-uptime escrow, Linux keyring, keyed revocation, and provider registry/UI remain out of scope. |
| D6 — core-before-optional startup and alias authority | Bind listener before `READY=1`; portal bootstrap first; reach lifecycle Ready before decrypted owners; publish app aliases only after active-publication proof; deny local acceptance and attempt bounded adapter withdrawal before closure; stale upstream fails closed; apply-time projection generation prevents stale snapshot replay | `internal/server/gin_server.go`; `internal/remote/manager.go`; `internal/services/publication_state.go`; startup, publication, suspension, withdrawal, projection-generation, and stale-apply tests | satisfied | Local acceptance and relay projection use the same active-publication authority; delayed and stale applies cannot republish a withdrawn alias. Live package/canary validation remains pending. |
| D6 — interrupted-owner matrix | Durable transitions replay; storage/onboarding/update suppress premature success; ephemeral streams/probes are discarded; bounded per-suspect/global recurrence automatically retries while non-suspects recover independently | Existing transition tests plus admission/lifecycle ownership in app, persistence, onboarding, storage, update, terminal, network, and firewall owners; bounded desired-owner enumeration lock-stall/fatal tests | partial | Local enumeration and owner execution share the finite deadline, cancellation grace, and first-fatal boundary. The amended live interruption matrix remains a release gate. |
| D7 — event and dock treatment | Authenticated initial task snapshot; per-topic barrier; connected pending Checking versus disconnected Offline; Warning/Unavailable degraded and Critical recovering; no per-app false status or new workflow; neutral expectation-setting copy | `internal/events/bus.go`; `internal/server/gin_event_stream.go`; `gin_event_stream_pressure_test.go`; Dart resource model/client/dock; `ui/test/resource_pressure_test.dart`; `ui/test/terminal_task_pressure_test.dart` | satisfied | Mounted reconnect/hydration transitions, exact neutral copy, and forbidden internal terminology are covered. `EventStreamClient` is the single pressure-state owner. |
| D8 — fixed overhead and capacity floor | No per-app guard workers; 16 terminal cap; qualify 2/4 GiB, the canonical constant-overhead installed-record baseline, and 72-hour stability | Single-loop guard unit test, terminal cap/soak unit test, and 2 GiB alpha profile (2,044,694,528 bytes RAM, one CPU, no swap) | partial | The 4 GiB profile, D8 constant-overhead baseline, and 72-hour mixed-operation soak remain pending. |
| D9 — measured reliability target | Treat five-nines as a target; capture stage timing; prove 30-second p95 unlocked route recovery for the healthy deterministic lowest-id Enabled route-bearing candidate with its reserved five-second slice; skip that qualification cohort for listenerless-only fleets while continuing their ordinary recovery; continue ordinary convergence with fresh bounds; measure recurrence and unexpected-no-handoff cohorts separately | `cmd/piccolod/task_recovery.go`, `task_fatal_owner.go`, `task_recovery_controller.go`, `task_recovery_runner.go`, and `internal/server/task_recovery_capabilities.go` preserve exact detection separately from marker time, keep failed qualification separate from the ordinary pass, carry fresh route/publication truth out of each app attempt, and emit correlated core-Ready, truthful unlock pickup/skip, lifecycle-Ready, qualification, per-route, and eventual-convergence stages with continuity/task/cohort labels; repeated 2/4 GiB derivation remains pending | partial | Local source telemetry is sufficient for the canary harness to derive stage durations without stale high-water or route-shape carry-over. Earlier sub-30-second core Ready runs are not unlocked-service evidence, and prepared/unknown handoffs, backoff, or manual unlock cannot enter the unexpected-no-handoff/healthy denominator. |
| Production unit and validators | Finite 15% task limit, 60-second service watchdog, always/5-second restart, control-group kill, bounded `ExecStopPost` recurrence marker, three-start/fifteen-minute limit, conditional PID-1 reboot composition, and upgrade-time stop/reset/start recovery in authoritative package, mounted image, and live boot | Published OBS `home:atdexterslab/piccolod` revision 78, companion recovery unit/helper/test, spec, and `0.2.40-1.1` binaries; `Makefile`; `scripts/systemd/piccolod-start-limit-recovery-test.sh`; `scripts/alpha/dev-vm-alpha-test.sh`; Piccolo OS `scripts/validate-image-policy.sh` at commit `965711b8`; corrected OBS image revision 21 and Build21.1 evidence; local RPM `%check`; exact candidate-RPM alpha lifecycle and real reboot logs | partial | The authoritative package builds and all corrected image profiles are green. The exact local RPM proved the effective policy, install-from-start-limit reset/restart, both boot-health branches with automatic real reboots, and unattended unlocked Ready. Strict mounted-root validation of Build21.1 and direct authoritative-package fault injection remain pending. |
| Local validation acceptance | Repository Go tests/races/vet, focused Flutter validation, shell syntax, policy diffs, and diff hygiene | `go test ./...`, scoped `go test -race`, `go vet ./...`, compatible native/browser Flutter test partitions, `flutter analyze --no-fatal-infos`, `bash -n`, `osc diff`, and `git diff --check` | satisfied | All compatible test partitions are green. A single all-platform Flutter invocation remains impossible because the existing native suite includes a web-only test and the browser suite includes a `dart:io` source-reading test; analyze reports nine pre-existing info-only lints. |
| Out-of-scope memory/capacity work | Do not reopen 0.2.39 OOM hierarchy, memory limits, inactive-app offloading, Snapper/Btrfs, or hardware-watchdog policy | No changes to those owners; resource documentation preserves the separate OOM contract and distinguishes service from hardware watchdog | satisfied | Existing 0.2.39 Go tests remain green. |
| Out-of-scope rescue/product surfaces | Do not add SSH/serial/rescue daemon, Piccolod-owned snapshot/rollback selection, per-app health screen, or operator recovery workflow | No such API, process, service, UI screen, or rollback path in the diff | satisfied | The bounded start-limit companion only composes with existing PID-1/MicroOS owners; safe-start and manual Start semantics are unchanged. |

### 2 GiB alpha qualification evidence — 2026-07-20

The server candidate used for fault injection (`sha256
bf3642976e78b1a48080ad8be20f258f57b2f0fcc5deef180d3af103672b8409`)
was installed with the authoritative OBS unit on a 2 GiB, one-vCPU, no-swap
alpha VM. A final gating correction changed only Flutter reconnect-state
handling; the exact rebuilt tree (`sha256
0aaa05dcd7589712d2b3d49142a01b48f9279f4992e1d3fece652da967c5582f`)
was then deployed with the same unit and passed the post-setup smoke 6/6 with
live/ready health OK and task pressure Normal at 11/2294. This is
source-candidate evidence, not an OBS package or final image artifact.

- Warning admission at 1261/2294 tasks retained `/health/ready` HTTP 200,
  reported task-pressure Warn, rejected a new diagnostic child with retryable
  HTTP 503, and did not replace Piccolod.
- Sustained high water emitted the bounded task census and recovery marker,
  exited with status 75 at 09:55:58 IST, and the replacement notified Ready at
  09:56:04. The injected 1250-thread contributor and old Piccolod PID were
  absent; the replacement cgroup returned to nine tasks. A startup ordering
  fault found by the first injection was fixed before this successful repeat.
- With the main process stopped, systemd recorded `Watchdog timeout` at
  09:58:47.843 and the replacement notified Ready at 09:58:53.473. The old PID
  was absent, `NRestarts=1`, and the cgroup again held nine tasks. The independent
  rescue timer was disarmed before it could alter this result.
- After unlock, the post-setup functional smoke passed 6/6. Lifecycle was
  `ready`, live health was `ok`, task pressure was Normal at 10/2294, and both
  fault-injection rescue timers were inactive. The alpha harness now follows
  the product's two-door order by unlocking a locked appliance before login.

These runs prove the live mechanism on the minimum-memory profile, but they do
not satisfy the repeated-sample p95, 4 GiB, installed-record, soak, package,
mounted-image, or canary-telemetry gates.

### Amended start-limit alpha evidence — 2026-07-22

The final source candidate (`v0.2.40-rc.local`, binary `sha256
6f9bd4840ff4afaa1951364c5c96193176a78f84c2ba76e17ded0a1ec6d00488`)
was packaged as the exact local RPM used for qualification (`sha256
6833865b437677467cb02ef9ec50b995a5df6bef3fe30a51c5c77494b82d1495`)
and installed on the same 2 GiB, one-vCPU, no-swap alpha profile. Effective
systemd state reported `StartLimitIntervalUSec=15min`, `StartLimitBurst=3`,
`RestartUSec=5s`, `WatchdogUSec=1min`, `TasksMax=2294`,
`KillMode=control-group`, the companion `OnFailure`, and the no-secret
`ExecStopPost`. The RPM was built locally with real Tumbleweed RPM macros and
passed `%check`; the unavailable privileged OBS chroot means this is not an
authoritative remote OBS build.

The first exact-RPM start exposed the locked-enumeration ordering defect above.
After the two-file correction and RPM rebuild, an existing volatile handoff
moved the device from locked to unlocked in two seconds without a password,
emitting `unlock_pickup_started`, `unlock_pickup_complete outcome=active`, and
`lifecycle_ready`. The same RPM was then installed while Piccolod was terminally
start-limited: its package lifecycle cleared the failed/rate-limit state,
restarted the daemon, and again reached unlocked Ready in two seconds.

The terminal boot-health branches used an isolated `Type=oneshot`
`health-checker.service` fixture while retaining the packaged companion and
real `/usr/bin/systemctl --no-block reboot` effect. For completed successful
health, systemd reported `ActiveState=inactive`, a non-empty `InvocationID`, and
`Result=success`; repeated Piccolod failure caused the companion to request a
normal reboot, after which the boot ID changed and Piccolod reached unattended
unlocked Ready. For failed health, systemd reported `ActiveState=failed`, a
non-empty `InvocationID`, and `Result=exit-code`; four automatic companion
invocations observed Piccolod's restart transition, and the terminal invocation
logged `preserving its recovery decision and requesting normal reboot`.
systemd-logind recorded `System is rebooting`; the boot ID changed from
`0b031a0e-bb8a-47f5-a55a-3c6020e2bfc4` to
`a6c039e0-466f-4ce7-8e41-486c99dd3e5e`, and the replacement reached HTTP 200
with `ready:true`, control storage unlocked, and `NRestarts=0`. The temporary
fixture and runtime override were removed after qualification.

These runs prove the exact local package lifecycle, both terminal boot-health
classifiers, automatic real reboot, and unattended unlock continuity. The
subsequent publication evidence below proves the remote artifact identity but
not its direct fault injection, strict mounted image, a clean-image app
lifecycle, the named interruption matrix, 4 GiB/installed-record baselines,
repeated p95, soak, or canary gates. Earlier app-install observations on this
reused VM remain excluded because prior test state contaminated the cohort.

### Published package and corrected image evidence — 2026-07-22

Tag `v0.2.40` points at commit `12b85d3`. GitHub Actions run `29902274844`
completed its UI, x86_64, aarch64, and release jobs successfully. The published
server assets report SHA-256
`a5348d382b13e2d5125a3d90ce6f718cab10a140b2c75a5e2555a827d910181b`
for x86_64 and
`2772049feb53cb328c725ab9c81b1a74d2ad89539dd1a2f237d9c37e5c4b8715`
for aarch64.

OBS package source revision 78 consumes those assets and produced
`piccolod-0.2.40-1.1` successfully for x86_64 and aarch64. The downloaded
x86_64 RPM has SHA-256
`adfd196d3766bc3c2a5ff501cf3c0bdc6a13ea2e62e222597df37ead9c6e35d1`;
its header and payload digests and RSA/SHA256 signatures verify after importing
the repository's public key into an isolated temporary RPM database.

The first dependent image rebuild, VirtualBox Build20.25, was excluded after
release-path review found authoritative image source revision 20 missing the
already-committed late bootstrap-DNS seed from `disk.sh`. Publishing only the
byte-identical Git file as OBS image revision 21 rebuilt all five x86_64 and
aarch64 profiles successfully. Corrected VirtualBox Build21.1 has official and
downloaded SHA-256
`275dfc2c63019469c128061965611b6cff29b7f30e87396c74dfb53827cece4a`.
Its CycloneDX SBOM records `piccolod-0.2.40-1.1`,
`piccolo-os-support-0.3.14-1.1`, and health-checker; the authoritative build log
records the late `bootstrap-dns.sh apply /etc/resolv.conf` execution. These are
artifact identity and build-composition proofs. The strict read-only mounted-
root validator still requires an operator-authorized local mount and remains a
release gate.

### Closure result

Closure verdict: `blocked-missing-requirement` (high confidence).

The local code obligations have no known open blocking requirement. Release
closure remains blocked because the accepted RFC deliberately makes coordinated
package/image/device and sustained-runtime evidence part of the contract.

- **blocking × in-scope — strict mounted-image integration**
  - **Location:** Production unit and validators.
  - **Statement:** GitHub v0.2.40, OBS package revision 78, both authoritative
    package architectures, corrected OBS image revision 21, and all image
    profiles are published and green. Build21.1's checksum, SBOM, and build log
    prove the intended source/package composition, but the strict validator has
    not yet inspected its mounted root and effective systemd configuration.
  - **Suggested resolution:** from the Piccolo OS repository, run
    `./scripts/validate-obs-image.sh --profile VirtualBox --arch x86_64
    /tmp/piccolo-os.x86_64-0.2.0-VirtualBox-Build21.1.vdi.xz` and append the
    exact result here.
- **blocking × in-scope — release qualification evidence**
  - **Location:** D6/D8/D9, Acceptance criteria, and Rollout and observability.
  - **Statement:** live 2 GiB unit policy, watchdog/task-critical recovery,
    old-process cleanup, exact local RPM lifecycle, both terminal boot-health
    branches with real reboots, and unattended unlock recovery are proven.
    Release still lacks the mounted-image result, direct task-exhaustion
    interruption of the authoritative `0.2.40-1.1` package, a clean-cohort app
    lifecycle, the named owner/transition interruption matrix, repeated 2 GiB
    and 4 GiB p95
    timing, the D8 constant-overhead baseline, 72-hour mixed-operation soak, and
    canary telemetry. The monolithic Flutter gate remains unavailable because
    of pre-existing cross-platform harness incompatibility, although every
    compatible partition is green.
  - **Suggested resolution:** validate the coordinated mounted image; run the
    remaining live fault matrix on clean 2 GiB and 4 GiB canaries;
    capture the installed-record, soak, and canary evidence; append exact
    commands/results here; then repeat RFC implementation closure.
