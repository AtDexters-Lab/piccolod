# RCA: Piccolod task exhaustion and appliance-wide app outage

- **Status:** Trigger and blast-radius amplifier confirmed; historical task
  accumulator only partially attributable
- **Date:** 2026-07-19
- **Severity:** SEV-1 — Piccolod, the appliance's only access path, could no
  longer create processes and every app appeared unavailable
- **Incident build:** Operator reports piccolod 0.2.34 before reboot
- **Recovery build:** piccolod 0.2.38 after reboot; 0.2.39 was released later
  for the independent per-app OOM/session-recovery incident
- **Corrective RFC:**
  `docs/rfc/20260719-piccolod-task-pressure-safety.md`

## Executive finding

The host did not run out of global PIDs and the kernel did not OOM-kill
piccolod. The `piccolod.service` cgroup reached its systemd task ceiling of
2311. The kernel then rejected forks inside that service cgroup:

```text
2026-07-19T11:43:02.361771+05:30 kernel: cgroup: fork rejected by pids controller in /system.slice/piccolod.service
```

The first captured Podman failure followed 225 ms later:

```text
2026-07-19T11:43:02.586783+05:30 piccolod[1173]: ... podman container exists failed: exit status 2, output: runtime/cgo: pthread_create failed: Resource temporarily unavailable
```

Once the ceiling was reached, every child-process-dependent Piccolod facility
could fail, including Podman inspection, `lvs`, and terminal shell creation.
The app reconciler then amplified the resource failure: it logged failed
container observations but left the zero `ContainerState` value in place. That
zero value means `Exists=false`, so an unknown observation was treated as proof
that containers were missing. Reconciliation stopped, removed, recreated, and
deactivated routes while the same resource condition prevented those effects
from succeeding. Apps consequently turned red together.

Rebooting cleared the service cgroup. Piccolod subsequently used approximately
35 tasks and the apps returned green. That proves the failure was task-lifetime
or task-accumulation related rather than a durable absence of all app
containers.

## User impact

- All app cards appeared red even though the initiating failure was in the
  Piccolod control plane.
- Existing app routes were removed or became unavailable during failed
  recovery attempts.
- The web terminal, which is the appliance's only interactive access path,
  could not start `/bin/bash`:

  ```text
  Failed to create terminal session: pty start: fork/exec /bin/bash: resource temporarily unavailable
  ```

- SSH is intentionally unavailable. A host reboot was the only remaining
  recovery available to the operator.

## Evidence

### The limiting boundary was `piccolod.service`

After reboot, the relevant values were:

```text
kernel.threads-max = 15408
kernel.pid_max     = 4194304
system.slice TasksMax = infinity
piccolod.service TasksMax = 2311
piccolod.service TasksCurrent ~= 35
```

The kernel named `/system.slice/piccolod.service` in the rejection. There is no
kernel OOM report or killed-process record in the incident window. The global
limits were substantially above the service limit.

`2311` is consistent with systemd's default task policy on this host: roughly
15 percent of the kernel's 15408 thread ceiling. The repository and production
unit did not declare an explicit `TasksMax`, so the service inherited the host
default.

### The failure preceded the application errors

The relevant sequence is:

| Time (IST) | Observation |
| --- | --- |
| 11:42:30–11:43:02 | Normal reconciliation launches short-lived rootless Podman commands and corresponding user scopes. |
| 11:43:02.361 | Kernel rejects a fork in `piccolod.service` through the cgroup pids controller. |
| 11:43:02.586 | Namek container inspection fails because Podman's Go runtime cannot create a thread. |
| 11:45 onward | The same failure appears for Git, Landing, Namek, and Piccolospace observations. |
| 11:48 onward | Reconcile enters stop/remove/recreate paths; Podman and `lvs` effects fail with the same resource error. |
| 11:54 onward | Multiple apps are in failed recovery and the UI access shell cannot fork. |
| After reboot | Task count returns to tens and all apps recover. |

The app failures are therefore consequences of the control-plane limit, not
independent simultaneous container failures.

### Reconciliation converts observation failure into absence

Current 0.2.39 source retains the incident behavior, although 0.2.39 fixed a
different OOM/session hierarchy:

- `containerGroupObservedRunning` returns `false` for either an unhealthy
  observation or any inspection error.
- `reconcileApp` treats that `false` as permission to consume an automatic
  startup attempt and enter recovery.
- `reconcileContainerGroup` logs an anchor or service inspection error but
  continues with a zero `ContainerState`.
- `!state.Exists` then authorizes stop/remove/recreate and route deactivation.

The same family of ambiguity exists at name-based resolution and manual-start
inspection sites. A typed not-found result is authoritative absence; a failed
Podman invocation is not.

### A concrete unbounded task-lifecycle defect exists

The legacy direct WebSocket terminal implementation starts a PTY command in
`internal/server/pty_session.go` but never calls `cmd.Wait()`. Closing the PTY
file descriptor and cancelling the request do not reap the direct child.
Exited children can therefore remain as zombies charged to
`piccolod.service`, with one unreaped direct child possible per legacy terminal
session.

The current bundled Flutter UI no longer uses those routes. It uses the
persistent session manager, which has a 16-session bound, a five-minute idle
reaper, cancellation, and explicit `cmd.Wait()`. Both legacy routes nevertheless
remain registered:

- `GET /api/v1/terminal`
- `GET /api/v1/apps/:name/terminal`

This is a code-proven accumulation mechanism and must be removed. It is not
possible to prove that it alone accounted for all 2311 incident tasks because
the process/task inventory was lost at reboot and the retained journal does not
contain a pre-reboot cgroup task census.

The other production sites that explicitly call `Cmd.Start` pair them with a
`Wait` owner. The review still needs to cover indirect helpers and future
callers because the service had no task high-water telemetry.

### Snapper and Btrfs messages are correlated, not the proven accumulator

The same period contains repeated Snapper XML `Document is empty` messages,
160 snapshots, and transient Btrfs qgroup inconsistency warnings. The qgroup
scan immediately cleared the inconsistency flag. `snapperd.service` is a
separate cgroup, so Snapperd's own threads cannot directly consume
`piccolod.service`'s 2311-task allowance.

Piccolod may spawn awaited Snapper clients as part of storage work, so the
activity can add short-lived pressure and latency. The available evidence does
not show it retaining 2311 tasks. Snapper/parser cleanup is therefore an
adjacent issue, not this RCA's root-cause claim.

## Causal chain

```text
unbounded or unexpectedly retained tasks in piccolod.service
        ↓
TasksCurrent reaches inherited TasksMax=2311
        ↓
kernel pids controller rejects fork/clone
        ↓
Podman, lvs, and terminal child processes cannot start threads/processes
        ↓
container observation returns an error
        ↓
reconciler collapses unknown observation into Exists=false
        ↓
stop/remove/recreate and route deactivation are attempted
        ↓
those effects also cannot fork; app status escalates and all apps appear red
        ↓
the only access terminal cannot fork; operator must reboot
```

There are therefore three corrective layers:

1. **Prevent known task leaks.** Remove the unreaped legacy PTY path and audit
   every started child for a single reaper.
2. **Make resource pressure observable and self-recovering.** Detect the
   Piccolod cgroup high-water mark before the hard limit, shed nonessential
   process creation, capture attribution, and let systemd restart the daemon at
   a critical threshold.
3. **Make observation failure safe.** Unknown runtime state must preserve the
   last-known app state and routes; only authoritative state or a controlled,
   quiescence-proven app recovery may mutate containers.

Addressing only one layer is insufficient. Raising `TasksMax` would delay the
same outage. Fixing only the legacy terminal would leave another leak capable
of recreating the blast radius. Fixing only reconciliation would preserve apps
longer but leave Piccolod's sole access path unable to fork.

## What is proven and what remains unknown

### Proven

- The cgroup pids controller for `piccolod.service` rejected forks.
- The effective service limit was 2311 while global task limits were much
  larger.
- Podman and other child-process operations then failed with EAGAIN-style
  resource errors.
- Reconciliation treated failed observations as missing state and initiated
  destructive recovery effects.
- The legacy direct PTY path starts children without reaping them.
- Reboot cleared the condition and restored apps.
- The independent 0.2.39 OOM/session fix does not correct these task-pressure
  and observation-authority defects.

### Not recoverable from this incident

- The exact PID/thread/zombie breakdown at `TasksCurrent=2311`.
- The exact rate and starting time of accumulation.
- Whether legacy PTY zombies supplied all, most, or only some retained tasks.
- Whether any second indirect child-lifecycle defect contributed.

The corrective design therefore captures a bounded `/proc` and cgroup census
before an automatic restart so a recurrence is attributable without requiring
SSH or a human console.

## Immediate recovery and disposition

The reboot was an appropriate emergency action after Piccolod could no longer
fork and no independent access path existed. Applications are green after
reboot. No evidence supports container-data deletion or app reinstall as an
incident remedy.

The incident is not closed by reboot or by the 0.2.39 OOM release. Closure
requires the linked RFC's observation-safety, task-lifecycle, pressure guard,
automatic-restart, UI, package-policy, and live fault-injection gates.
