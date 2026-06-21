# RFC: Fast Snapshot Gate for OS Update Status and Auto-Reboot

**Problem:** Auto-reboot and OS update status currently depend on the full
status path, which includes snapper-backed snapshot listing. When
`snapper --json list` is slow or wedged, Piccolo can fail to notice that a
transactional update is already staged and can keep re-probing the slow path.

**In scope:** Introduce an authoritative fast snapshot-state path for staged
update detection, route auto-reboot and required-reboot reporting through it,
and demote snapper/zypper/RPM metadata to bounded best-effort enrichment with
shared backoff.

**Out of scope:** Changing snapper/qgroup cleanup policy, deleting old
snapshots, changing transactional-update timers, redesigning the System Updates
UI, or changing reboot/snapshot validation semantics beyond consuming the fast
staged-update signal.

## Context

On 2026-06-16, one device had already staged `piccolod-0.2.29` in the default
snapshot while the running root still reported `piccolod-0.2.28`.
`transactional-update.service` had succeeded, but Piccolo did not auto-reboot
inside the configured 03:00-05:00 maintenance window. Manual reboot moved the
device to `0.2.29`.

After reboot, the device repeatedly recreated a snapper failure mode:

- `snapper --json list` could take long enough to time out.
- `snapper-cleanup.service` failed with `Config is locked`.
- `snapperd` accumulated many threads blocked in `btrfs_ioctl`.
- Btrfs qgroup rescans repeated roughly every 19 seconds.
- Foreground and background status probes could continue to invoke snapper.

The important product invariant is narrower than the full status payload:
Piccolo must know, cheaply and reliably, whether the booted root differs from
the bootloader-default root. Auto-reboot only needs that staged-update signal;
version and cleanup metadata are useful, but must not be load-bearing for
reboot eligibility.

## Current Shape

`internal/autounlock/scheduler.go` calls `UpdateManager.HasStagedUpdate(ctx)`
before firing auto-reboot.

`internal/server/autounlock_adapters.go` currently implements that method by
calling `Manager.Status(ctx)` and returning `Status.RequiresReboot`.

`internal/update/manager.go` builds `Status` through a broad read path. The
path compares active and default snapshots, but it also calls snapper, reads
transactional-update metadata, asks zypper for available updates, and queries
RPM state. The same status machinery feeds the API, cache refresh, watcher, and
auto-reboot gate.

This couples a safety-critical, cheap question to several slow or fragile
enrichment sources.

## Proposal

### 1. Add A Fast Snapshot-State Primitive

Add a first-class update-manager primitive that answers the staged-root
question without entering the full status enrichment path.

Proposed API shape:

```go
type SnapshotReadiness string

type SnapshotState struct {
    ActiveSnapshot  string
    DefaultSnapshot string
    Readiness       SnapshotReadiness
    RequiresReboot  bool
    Source          string
}

func (m *Manager) SnapshotState(ctx context.Context) (SnapshotState, error)
```

`SnapshotReadiness` has four semantic values:

- `staged`: active and default snapshots are normalized to the same namespace,
  differ, and no transactional-update producer is currently active;
- `absent`: active and default snapshots are normalized to the same namespace,
  match, and no transactional-update producer is currently active;
- `in_progress`: a transactional-update or Piccolo transactional-update unit is
  active, queued, or represented by a live in-progress marker;
- `unknown`: active/default state could not be probed or normalized within the
  fast gate's timeout.

The primitive should:

- first check whether a transactional-update producer is still running;
- read the currently booted root from the mounted root source;
- read the bootloader-default subvolume from btrfs;
- normalize active and default snapshots into the same namespace before
  comparison;
- derive `RequiresReboot` only from `Readiness == staged`;
- avoid `snapper`, `zypper`, and full RPM listing;
- use a short timeout suitable for the maintenance-window scheduler;
- return `Readiness == unknown` plus an error for unknown state instead of
  converting unknown into `RequiresReboot=false`.

The implementation can reuse existing active/default snapshot helpers where
they already stay on the fast path. Any fallback must remain bounded and must
not call `snapper --json list`.

Normalization is part of the contract. If one side is a snapper snapshot number
and the other side is a raw btrfs subvolume ID, the gate must prove they refer
to the same namespace before comparing. If it cannot prove that relationship
within the fast timeout, the result is `unknown`, not a staged or absent
decision.

### 2. Route Auto-Reboot Through The Fast Primitive

Change the auto-unlock adapter so the scheduler consumes snapshot readiness
directly instead of deriving a boolean from full update status.

Package boundary decision:

- `internal/update` owns `SnapshotState` and the rich probe semantics.
- `internal/server.osUpdateManager` adds `SnapshotState(ctx)`.
- `internal/server.osUpdateManagerAdapter` maps update readiness into a small
  scheduler-facing enum.
- `internal/autounlock.UpdateManager` replaces boolean `HasStagedUpdate(ctx)`
  with a readiness-shaped method plus `Reboot(ctx)`.

The scheduler-facing readiness must preserve these states:

- staged update present;
- no staged update;
- update in progress / not ready;
- unknown probe failure.

Scheduler behavior should distinguish all four readiness states:

- staged update present: deposit unlock material, persist `last_fired_at`, and
  reboot;
- no staged update: skip the window without persisting `last_fired_at`;
- update in progress / not ready: skip the window without persisting
  `last_fired_at`;
- snapshot state unknown: log a bounded warning and skip without persisting
  `last_fired_at`.

This preserves the existing safety posture. Piccolo should not reboot if it
cannot prove a staged and quiescent transactional root exists, but it also
should not mark the window as fired on an inconclusive or in-progress probe.

Nil update-manager wiring remains a scheduler failure: it should not be
collapsed into "no staged update". Adapter and scheduler tests should cover nil
manager, probe error, in-progress, no staged update, and staged update.

### 3. Split Status Into Core State Plus Enrichment

Refactor `Manager.Status(ctx)` so the core status fields are sourced from the
fast snapshot state first:

- `Pending`
- `RequiresReboot`
- active/default snapshot identifiers in `Meta`

Snapper, zypper, transactional-update run details, and RPM version comparisons
become enrichment for the status API. Enrichment can improve labels and
diagnostics, but failure or timeout must not erase a true `RequiresReboot`
signal from the fast path.

Preserve the existing API behavior for active update production:

- if `SnapshotState` is `in_progress`, `Manager.Status(ctx)` continues to
  return `ErrInProgress`;
- `handleOSUpdateStatus` continues to map `ErrInProgress` to HTTP `429` with
  `Retry-After`;
- the Dart controller's existing 429 busy/poll behavior remains valid.

Returning `200` with `snapshot_readiness=in_progress` would be a UI/API
contract change and is out of scope for this fix.

If enrichment is unavailable, the API should still return a useful degraded
status:

- current known installed version if available;
- staged/reboot-required state from snapshot comparison;
- metadata indicating which enrichment source is degraded or in backoff.

### 4. Share Single-Flight And Backoff Across Status Callers

The existing background refresh guard is not enough if foreground requests can
start their own full status reads. Introduce one shared enrichment coordinator
inside the update manager.

The coordinator should ensure:

- at most one enrichment probe is running at a time;
- foreground API calls do not spawn unbounded snapper work;
- background `Watch` refreshes the fast snapshot state on its normal cadence;
- slow enrichment failures enter a cooldown before snapper/zypper are retried;
- callers receive fast core state plus the newest cached enrichment available.

Every full-status or enrichment entry point must route through either the fast
snapshot primitive or the coordinator. Direct `readStatus` calls are part of
the behavior being removed, not an acceptable internal shortcut.

Required caller decisions:

- `Status`: preserves `ErrInProgress` for active update production; otherwise
  returns fast core state and attaches cached or freshly coordinated enrichment
  when available;
- `refreshStatusCache`: asks the coordinator for enrichment and publishes only
  samples that are still fresh relative to invalidation;
- `Watch`: refreshes fast snapshot state on the existing cadence, but enters
  enrichment only through the coordinator and respects backoff;
- `checkAndRecover`: uses fast snapshot readiness for recovery gating and a
  bounded fresh transactional-update last-run probe for fallback decisions;
- `runTransactionalUpdate`: after a successful launch, captures the target
  snapshot through a bounded fast snapshot probe or caller-provided target hint,
  not `readStatus(context.Background())`.

The initial enrichment cooldown is five minutes after a snapper/zypper/RPM
enrichment timeout or failure. Fast snapshot readiness still runs on the normal
status/watch cadence; only enrichment backs off.

### 5. Preserve Reboot And Recovery Safety Checks

This RFC does not weaken transactional reboot safety.

`Reboot(ctx)` should continue to validate the staged snapshot before rebooting.
Snapshot validation, rollback watch behavior, and critical-file checks should
remain on their existing safety-oriented path.

Recovery code should not treat enrichment failure as proof that no staged
update exists. Unknown enrichment state should be reported as degraded and
should not trigger agent-package fallback behavior by itself.

Recovery decision matrix:

- readiness `staged`: do not run fallback; a reboot candidate already exists;
- readiness `absent` plus fresh failed last transactional-update run: keep
  existing fallback rules and circuit breaker;
- readiness `in_progress`: do not run fallback; report degraded/in-progress
  state only;
- readiness `unknown`: do not run fallback; report degraded snapshot-probe
  state only.

Auto-fallback requires a proven `absent` snapshot state. This prevents recovery
from stacking new transactional updates on top of an unresolved reboot
candidate or unknown probe failure.

Recovery must not use stale cached status enrichment as proof of a current
transactional-update failure. Last-run evidence used for fallback must come from
a bounded fresh probe, or from a cache entry that is explicitly fresh enough for
recovery. Snapper/zypper enrichment cooldown must not force recovery to reuse an
old failed `last_run`.

### 6. Make The Failure Mode Observable

Expose enough metadata to make this incident diagnosable without shell access:

- fast snapshot probe source and active/default IDs;
- snapshot readiness: staged, absent, in-progress, or unknown;
- whether enrichment is fresh, stale, running, timed out, or in backoff;
- last enrichment error class, without leaking excessive command output;
- whether `requires_reboot` came from the fast snapshot gate.

Scheduler logs should include bounded messages for:

- maintenance window skipped because no staged update exists;
- maintenance window skipped because update production is still in progress;
- maintenance window skipped because snapshot state is unknown;
- reboot fired because fast snapshot state proved a staged root exists.

## Site List

Primary backend sites:

- `internal/update/manager.go`
  - `Manager`
  - `osBackend`
  - `microOSBackend.Status`
  - `readStatus`
  - `statusFallback`
  - `refreshStatusCache`
  - `scheduleStatusRefresh`
  - `Watch`
  - `isInProgress`
  - `activeSnapshot`
  - `defaultSnapshot`
  - `snapperNumberFromID`
  - `snapperSnapshots`
  - `rpmUpdateCount`
  - `queryRPM`
  - `lastRunInfo`
  - `runTransactionalUpdate`
  - `checkAndRecover`
  - `watchSnapshots`
  - `validateStagedSnapshot`
  - `Reboot`

Auto-reboot and API sites:

- `internal/autounlock/scheduler.go`
- `internal/server/autounlock_adapters.go`
- `internal/server/gin_server.go`
- `internal/server/gin_phase2_handlers.go`

UI compatibility sites:

- `ui/lib/core/models/os_update.dart`
- `ui/lib/shells/desktop/features/settings/settings_controller.dart`
- `ui/lib/shells/desktop/features/settings/tabs/system_tab.dart`

Test sites:

- `internal/update/manager_test.go`
- `internal/update/manager_recovery_test.go`
- `internal/server/autounlock_adapters_test.go`
- `internal/server/gin_phase2_handlers_test.go`
- `internal/autounlock/scheduler_test.go`
- `ui/test/os_update_test.dart`

Documentation sites:

- `docs/rca/20260616-auto-reboot-missed-staged-update-snapper-qgroup.md`
- `docs/api/os-updates-integration.md`

## Test Plan

Backend unit coverage:

- fast snapshot state returns `RequiresReboot=true` when active/default roots
  differ in the same namespace and no update is in progress;
- fast snapshot state returns `RequiresReboot=false` when active/default roots
  match;
- fast snapshot state returns `in_progress` when transactional-update is active
  even if active/default snapshots differ;
- fast snapshot state returns an error for unknown active/default state;
- fast snapshot state returns unknown when active/default identifiers cannot be
  normalized into the same namespace;
- `Status` preserves `ErrInProgress` while update production is active;
- auto-reboot fires only on proven staged state;
- auto-reboot does not persist `last_fired_at` on unknown snapshot state;
- auto-reboot does not persist `last_fired_at` while update production is still
  in progress;
- `Status` preserves `RequiresReboot=true` when enrichment times out;
- foreground status calls share one enrichment probe;
- background watch does not repeatedly reenter enrichment while backoff is
  active;
- `runTransactionalUpdate` target capture does not call the full status path;
- recovery logic requires proven absent snapshot state before fallback and
  treats enrichment failure, in-progress state, and fast snapshot unknown as
  degraded rather than no-update proof;
- recovery fallback uses fresh last-run evidence and does not act on stale
  cached failed `last_run` while enrichment is in backoff.

Handler/API coverage:

- `/api/v1/updates/os` returns fast pending/reboot fields even when enrichment
  is degraded;
- `/api/v1/updates/os` preserves HTTP `429` plus `Retry-After` while update
  production is in progress;
- degraded/backoff metadata is present and parsed without breaking existing UI
  consumers.

Manual device validation:

- stage an update and confirm active/default snapshot mismatch is detected
  without `snapper --json list`;
- start or simulate an in-progress transactional update and confirm
  auto-reboot does not fire on a temporary active/default mismatch;
- confirm auto-reboot fires in the maintenance window when the fast gate is
  true;
- simulate snapper timeout and confirm status remains responsive;
- confirm snapper enrichment enters cooldown rather than running every watcher
  tick;
- confirm manual reboot still validates and boots into the staged snapshot.

## Implementation Decisions

- Enrichment cooldown starts at five minutes for device builds.
- Degraded/backoff fields remain best-effort `Meta` for now. Stable top-level
  API behavior remains `pending`, `requires_reboot`, and HTTP `429` while
  update production is active.
