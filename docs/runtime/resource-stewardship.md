# Resource Stewardship

Architectural reference for Piccolo's resource-stewardship subsystem:
how memory, CPU, and disk are declared, derived, and enforced across
installed apps on consumer-grade hardware (4–8 GB RAM, 2–4 cores,
128–512 GB SSD).

See also: `.claude/plans/resource-stewardship.md` (full design history),
`runtime/resource-stewardship-rollout.md` (operator migration).

## The model in one sentence

Catalog authors declare *shape* (priority tier, memory profile + floor,
optional storage max); the runtime derives kernel knobs against actual
system state; enforcement lives at the per-app-user systemd slice; a
unified pressure signal surfaces to the UI when things get tight.

## Manifest: what catalog authors declare

```yaml
resources:
  # CPU priority tier. Controls cgroup cpu.weight at the slice.
  priority: normal   # high | normal | background (default: normal)

  # Memory declaration. Both fields are honest about what authors can know:
  # the functional minimum, and whether the app is stay-in-its-lane or
  # grow-to-fill.
  memory:
    min_required: 4GB     # REQUIRED when memory block is declared. Size.
    profile: bounded      # bounded | elastic (default: bounded)

  # Optional storage ceiling. Hidden/advanced — set only for apps known to
  # be data-heavy (immich, nextcloud). When absent, runtime uses the
  # default cap min(100 GiB, pool_total × 0.4).
  storage:
    max: 500GiB

# Reserved namespace for the future stop-on-idle RFC. No fields consumed
# today; catalog authors can anticipate the namespace.
lifecycle: {}
```

**No numeric memory target, no numeric memory ceiling, no numeric CPU
cap** are accepted from catalog manifests. Authors cannot know the
deploy-site hardware; asking them to pick a target just moves the
failure from one field to another.

## Priority tier → CPU weight

| Tier          | CPUWeight | Use for |
|---------------|-----------|---------|
| `high`        | 400       | Interactive servers the user is actively waiting on (API endpoints, web UI) |
| `normal`      | 100       | Default. Most apps. |
| `background`  | 25        | Batch workloads that should yield to interactive (thumbnail generation, indexing, backup) |

Values are compressed (400/100/25) for 2–4 core target hardware. Larger
ratios (e.g. 1000:100:10 on server-class machines) starve `background`
for multiple seconds at a time on small-core CPUs.

## Memory profile + min_required → MemoryHigh / MemoryMax

Definitions:
- `usable_RAM = system_RAM × 0.8` — reserves 20% for OS, kernel, piccolod.
- `slice_ceiling_cap = usable_RAM − 256 MiB` — the hard cap for MemoryMax
  regardless of author declaration. Keeps slice-scoped reclaim as the
  first line of defence *before* host-level OOM.
- `active_elastic_plus_self = max(1, running-elastic-apps + 1)`.

### `bounded` profile

Predictable relative to the declared floor. For apps that stay in their
lane (DBs with fixed shared_buffers, config-only services, standard API
servers):
- `MemoryHigh = min(min_required × 1.25, slice_ceiling_cap × 0.9)`
- `MemoryMax  = min(min_required × 2, slice_ceiling_cap)`

### `elastic` profile

Grows to fill available memory when the box has headroom. For apps that
benefit from more RAM (ML workers loading large models, photo
thumbnailers, ZFS-like caching behaviour):
- `MemoryHigh = min(max(min_required × 1.25, usable_RAM × 0.5 / N), slice_ceiling_cap × 0.9)`
- `MemoryMax  = min(max(min_required × 1.5, usable_RAM × 0.7), slice_ceiling_cap)`

where N = active_elastic_plus_self.

**Invariant:** `MemoryMax ≤ slice_ceiling_cap` always, regardless of
`min_required`. If `min_required × 1.25 > slice_ceiling_cap`, the clamp
wins. The app may still fail to function, but it fails *contained within
its own slice* — the OOM scorer picks its tasks, not piccolod or a
sibling app.

## Install-time gate (D-11)

Two tiers:

- **Tier 2 (hard block):** `min_required > system_RAM`. The app cannot
  be contained within available memory; any runtime state would force
  host-scope OOM. Install refused. Single known way around it: uninstall
  a sibling app or upgrade hardware.
- **Tier 1 (warn):** `Σ min_required(installed + new) > usable_RAM`.
  Installing is permitted with a warning — the slice clamp keeps failure
  contained if the user proceeds.

On `sysinfo(2)` failure the gate fails closed (Tier 2 cannot be
verified). Retry on a healthy system.

## Disk: per-app LV auto-grow

Application volumes (ext4 on LUKS on thin-LV) auto-grow in two stages
(D-5a):

1. **70% early-schedule:** when usage crosses this threshold, the monitor
   schedules a grow but defers execution.
2. **Idle-window wait (up to 5 min):** samples writes-completed from
   `/sys/block/dm-N/stat` for the volume's mapper. When the counter is
   unchanged for ≥30 s, executes the grow.
3. **80% hard fallback:** if usage crosses this before an idle window
   appears, grow immediately. Keeps the deferral bounded.

Deferral is specifically to avoid the 1–5 s `lvresize + cryptsetup
resize + resize2fs` stall tripping client-side timeouts
(e.g. postgres `statement_timeout`, HTTP `statement_timeout`) during
active writes like an immich mobile-photo backup.

### Pool admission + ordering (D-8)

Per-grow admission:
- Refuses the grow when `projected_pool_pct_after_grow > 95%`. Replaces
  the previous set-once 95% "disable all grows" flag.

When multiple volumes cross threshold in the same monitor tick, they
are sorted by current usage ratio descending. The app closest to its
own ceiling grows first — beats Go-map iteration randomness when pool
headroom is constrained.

### Pool guard (90% / 95%)

Unchanged from pre-RFC:
- 90% → user-visible warning via `TopicResourcePressure`
  (`severity=warn`) + `TopicStorageAlert` (legacy).
- 95% → stops running workspace apps (service apps left alone; they
  take bounded writes). Publishes `urgent` pressure event.

## Pressure attribution

The `resource-pressure` supervisor component polls per-app-user slices
every 30 s:

- `memory.pressure` + `cpu.pressure` (cgroup v2 PSI) — sustained-
  threshold detection with debounce.
- `memory.events.oom_kill` — increments bypass sustained-pressure
  debounce (OOM is a strong-enough signal to notify immediately).

Severity levels derived from PSI `some avg60`:

| Threshold | Sustain | Severity |
|-----------|---------|----------|
| ≥ 10 %    | 30 s    | info     |
| ≥ 20 %    | 60 s    | warn     |
| ≥ 40 %    | 60 s    | urgent   |

Debounce: one emission per `(app, resource, severity)` tuple per 60 s.
Reset: after 2 minutes below threshold, severity state resets so the
next excursion re-notifies at the correct level.

All events land on `TopicResourcePressure` with unified payload
(`ResourcePressureEvent`). `TopicStorageAlert` is dual-published by
the pool guard during the transition window; deprecated.

## OOM recovery hierarchy

The appliance uses one existing systemd hierarchy with three explicit
OOM priorities:

1. `piccolod.service` runs at `OOMScoreAdjust=-500`. It owns pressure
   attribution, desired-state reconciliation, the UI, and app recovery.
2. The global `user@.service` template runs user managers at `-250`.
   This makes the rootless control boundary less attractive than app
   workloads without equating it to piccolod.
3. Every rootless Podman command passes through
   `/usr/bin/choom -n 0 -- ...` at the existing runtime-credential
   boundary. Podman helpers and container workloads therefore run at the
   neutral value instead of inheriting either protected control plane.

Under pathological misdeclaration, the kernel can kill a workload while
leaving both recovery layers available. If host-scope OOM still kills an
app's user manager, ordinary app reconciliation repairs the same
`user@UID.service` and then reuses the existing container-group recovery.
Status and read-only paths only observe session readiness; automatic or
manual start paths may ensure it; stop, shutdown, uninstall, and storage
transitions quiesce it.

Automatic repair remains inside the existing five-attempt/ten-minute
startup budget. A successful repair begins a ten-minute process-local
probation window rather than resetting history immediately, so rapid
running-then-OOM churn cannot open a fresh retry budget every reconcile
tick. See
[`20260718-per-app-runtime-oom-session-recovery-amendment.md`](../rfc/20260718-per-app-runtime-oom-session-recovery-amendment.md)
for the lifecycle contract and release requirements.

## Catalog-author guide: picking priority + profile

**Priority:**
- Pick `high` only if the app's latency directly affects user experience
  and the app would be measurably worse running at normal weight under
  contention. In practice: primary API/web servers.
- Pick `background` for anything that can wait — indexing, transcoding,
  backups, cleanup.
- Everything else: `normal`.

**Memory profile:**
- `bounded`: the app's peak working set is close to its cold-start
  working set. DBs with configured shared_buffers, redis, most
  config-only or lightweight services.
- `elastic`: the app benefits from more RAM than it strictly needs.
  ML workers loading models, photo thumbnailers, anything with an
  internal cache that grows opportunistically.

**min_required:** honest estimate of the functional minimum — below
which the app cannot complete its primary operation without OOM. Upstream
docs, benchmark results, or measured steady-state peaks. Setting it
higher than needed blocks installs on tight-RAM boxes; setting it lower
means the app may still OOM on boxes where it would otherwise have
worked.

**storage.max:** leave unset unless the app is data-heavy (immich,
nextcloud). Default ceiling (min(100 GiB, 40% of pool)) is adequate for
most workloads.

## What's explicitly deferred

- Per-app `storage.max` wired through `volumeMetaV3` at provisioning
  (currently the monitor uses the global 500 GiB ceiling).
- UI subscriber for `TopicResourcePressure` — backend publishes the
  event topic; UI consumption is a separate frontend task.
- Per-container walk for intra-app attribution (multi-service apps).
- Stop-on-idle / cgroup.freeze lifecycle (its own RFC; manifest
  `lifecycle:` namespace reserved).
- IO weights (`io.weight`). Same pattern extends cleanly; not a
  presenting pain point.
