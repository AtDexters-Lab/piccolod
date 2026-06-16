# RCA: Auto-reboot missed staged update due to snapper/qgroup status timeout

**Date:** 2026-06-16  
**Device:** Piccolo device reachable as `d111.abhishekborar.com`  
**Observed versions:** running `piccolod v0.2.28`; staged snapshot contained `piccolod 0.2.29-1.1`; manual reboot successfully booted `v0.2.29`

---

## Summary

The device staged the `piccolod v0.2.29` update successfully, but the 03:00-05:00 auto-reboot window did not reboot into it. This was not a transactional-update failure and not a failed reboot attempt: the auto-reboot scheduler never fired.

The scheduler's staged-update gate depended on the full OS update status path. That status path shells out to `snapper --json list` as enrichment. On the affected device, `snapper --json list` was taking longer than Piccolo's 5s foreground status budget and sometimes long enough to overlap with later probes. The device also had 447 snapshots despite snapper's configured `NUMBER_LIMIT="50"`, Btrfs qgroup churn, and snapper cleanup repeatedly failing with `Config is locked`.

The incident therefore has two linked causes:

1. **Piccolod robustness bug:** auto-reboot used the slow, enrichment-heavy update status path to decide whether a staged update existed.
2. **System state issue:** snapper/Btrfs qgroup cleanup was unhealthy, leaving too many snapshots and making `snapper --json list` slow or stuck.

---

## Impact

- The device stayed on `piccolod v0.2.28` after the overnight maintenance window even though `v0.2.29` was already staged.
- The System Updates UI/API degraded: `/api/v1/updates/os` calls took exactly 5s and returned degraded/fallback status.
- Snapper cleanup did not drain old snapshots, keeping the device in a self-reinforcing slow-status state.
- The user had to manually reboot to apply the staged update.

---

## Timeline

### 2026-06-15

- `transactional-update.service` completed successfully at `2026-06-15 00:09:54 IST`.
- Service logs reported `Minimally required reboot level: reboot` and `Reboots are disabled`.
- Running system still had:

```text
piccolod-0.2.28-1.1.x86_64
```

- Btrfs default snapshot was already set to snapshot `817`:

```text
ID 2716 gen 158761 top level 257 path @/.snapshots/817/snapshot
```

- Staged snapshot `817` contained the update:

```text
rpm --root /.snapshots/817/snapshot -q piccolod
piccolod-0.2.29-1.1.x86_64
```

### 2026-06-16 morning

- At around 10:00 IST, the live version endpoint still reported:

```json
{"service":"piccolod","version":"v0.2.28"}
```

- `auto_unlock.json` still showed the previous fire from June 12:

```json
{
  "enabled": true,
  "auto_reboot": {
    "enabled": true,
    "window_start_hour": 3,
    "window_end_hour": 5,
    "last_fired_at": "2026-06-12T03:00:18.903115256+05:30"
  },
  "last_deposit_at": "2026-06-12T03:00:19.170352712+05:30",
  "last_pickup_at": "2026-06-12T03:01:00.954276321+05:30"
}
```

This proves the June 16 scheduler did not reach its fire step. `last_fired_at` is persisted before `Reboot()` is called.

- Diagnostic log `~/Downloads/piccolod-diagnostic (1).log` was capped at exactly 50,000 lines and began at `2026-06-16T03:23:02+05:30`, so the first 23 minutes of the maintenance window were missing.
- From `03:23` through `09:58`, the retained log showed no shutdown/reboot/SIGTERM markers.
- The same log showed repeated update status requests taking exactly 5s:

```text
2026-06-16T09:57:53+05:30 ... [GIN] ... | 200 |       5s | 127.0.0.1 | GET "/api/v1/updates/os"
2026-06-16T09:58:03+05:30 ... [GIN] ... | 200 |       5s | 127.0.0.1 | GET "/api/v1/updates/os"
2026-06-16T09:58:13+05:30 ... [GIN] ... | 200 |       5s | 127.0.0.1 | GET "/api/v1/updates/os"
```

### Manual reboot

- Manual reboot applied the staged snapshot successfully.
- Device then reported `v0.2.29`, confirming the staged snapshot was healthy and the update itself was not the failing part.

---

## Investigation

### Auto-reboot path

The auto-reboot scheduler only fires when all gates pass:

- auto-unlock enabled
- auto-reboot enabled
- timezone considered safe
- current local hour within `[window_start_hour, window_end_hour)`
- not already tried this window
- `UpdateManager.HasStagedUpdate(ctx)` returns true

The observed `last_fired_at` remaining on June 12 means one of the gates returned false before the fire point. The strongest candidate was `HasStagedUpdate`.

Current code path:

- `internal/autounlock/scheduler.go`: scheduler calls `UpdateManager.HasStagedUpdate(ctx)`.
- `internal/server/autounlock_adapters.go`: adapter implements that by calling `updateManager.Status(ctx)` and returning `st.RequiresReboot`.
- `internal/update/manager.go`: `Status()` has a 5s request timeout. On timeout with no good cached status, it returns a degraded fallback with `Pending=false` and `RequiresReboot=false`.

That means a slow status probe turns a real staged update into "no staged update" for auto-reboot purposes.

### Snapper/Btrfs symptoms

Snapper cleanup was failing repeatedly:

```text
snapper-cleanup.service:
Running timeline cleanup for 'root'.
Deleting snapshot from root:
422
Config is locked.
timeline cleanup for 'root' failed.
```

Snapper timeline also failed in the diagnostic log:

```text
IO Error (open failed path://.snapshots errno:24 (Too many open files)).
```

The device had far more snapshots than policy intended:

```text
find /.snapshots -mindepth 1 -maxdepth 1 -type d | wc -l
447
```

Relevant snapper config:

```text
QGROUP="1/0"
SPACE_LIMIT="0.5"
NUMBER_CLEANUP="yes"
NUMBER_LIMIT="50"
TIMELINE_CREATE="yes"
TIMELINE_CLEANUP="yes"
```

Bare `snapper --json list` exceeded 20 seconds:

```text
time -p timeout 20s snapper --json list >/tmp/snapper-list-after-reset.json
real 20.00
exit=124
```

When observed live, a `snapper --json list` client could still be alive after 17 seconds.

### Snapperd state before reset

Before restarting snapperd, the daemon had accumulated many threads and file descriptors:

```text
snapperd=1877
FDSize:512
Threads:303
305 /.snapshots
```

Thread wait-channel sampling showed:

```text
430 btrfs_ioctl
```

After `systemctl restart snapperd`, the daemon returned to a small idle shape:

```text
FDSize:256
Threads:2
```

The reset cleared the accumulated daemon state, but `snapper --json list` remained too slow for Piccolo's status budget.

### Producer cadence

After rebooting into `v0.2.29`, new `snapper --json list` processes continued to appear with parent `/usr/bin/piccolod`:

```text
PPID 1773 = /usr/bin/piccolod
```

The cadence was about 30 seconds:

```text
2026-06-16T12:45:33 ... snapper --json list age 00:06
2026-06-16T12:46:03 ... new snapper --json list age 00:06
```

This matches `internal/update/manager.go`'s background `statusRefreshTicker`, not only the browser UI. The UI can add extra requests when the System/Updates view is open, but the background watchdog alone can reproduce recurring snapper probes.

---

## Root Cause

### Immediate root cause

Auto-reboot missed the staged update because its "has staged update" gate depended on the full OS update status path. On this device, the status path timed out because `snapper --json list` was slow or stuck. The timeout fallback reported `RequiresReboot=false`, so the scheduler skipped the reboot window and did not update `last_fired_at`.

### Underlying system cause

Snapper cleanup was not draining old snapshots. The device had 447 snapshots with qgroup accounting enabled. `snapper --json list` took longer than 20 seconds, and snapperd had previously accumulated hundreds of threads blocked in `btrfs_ioctl`. Cleanup repeatedly failed with `Config is locked`, likely because snapper operations from Piccolo's periodic status refresh overlapped cleanup and held snapper's config path busy.

### Self-reinforcing loop

```text
447 snapshots + qgroup enabled
→ snapper list becomes slow
→ piccolod runs snapper --json list every 30s for update status refresh
→ snapper operations overlap cleanup
→ snapper-cleanup fails with "Config is locked"
→ old snapshots do not drain
→ snapper list remains slow
→ update status keeps timing out
→ auto-reboot cannot detect staged updates reliably
```

---

## Contributing Factors

1. **Critical gate depended on non-critical enrichment.** Active-vs-default snapshot mismatch is enough to determine whether a reboot is required, but the current status path also gathers snapper metadata, zypper update counts, journal/run info, and RPM version details.
2. **Timeout fallback was unsafe for auto-reboot.** For UI reads, degraded fallback is acceptable. For auto-reboot, returning `RequiresReboot=false` on status uncertainty suppresses the maintenance action silently.
3. **Background status refresh uses the same heavy path.** `updateManager.Watch()` refreshes status every 30 seconds and invokes `readStatus()`, which calls `snapper --json list`.
4. **No strong backoff around slow snapper.** Repeated snapper probes continue even when snapper is known to be slow or timing out.
5. **Diagnostic log cap hid the first 23 minutes of the maintenance window.** The retained diagnostic started at `03:23:02`, so exact scheduler behavior near 03:00 was not captured.

---

## Immediate Remediation

- Manual reboot applied the staged `v0.2.29` snapshot successfully.
- Restarting `snapperd` cleared the accumulated daemon thread/fd state.
- Avoiding the System/Updates UI reduces extra `/api/v1/updates/os` polling, but it does not stop the background 30s status refresh.

---

## Preventive Remediation

1. **Decouple auto-reboot from full update status.** Auto-reboot should use a fast, narrow staged-update check based on active snapshot ID vs default snapshot ID (`findmnt` + `btrfs subvolume get-default /`). It should not call `snapper --json list`.
2. **Make `requires_reboot` cheap and authoritative.** The foreground status API should compute `Pending` / `RequiresReboot` before any slow enrichment. Snapper metadata should be optional best-effort data.
3. **Add hard backoff/single-flight for snapper enrichment.** If `snapper --json list` times out, avoid spawning another one every 30 seconds. Keep serving stale/degraded metadata while backing off.
4. **Improve auto-reboot observability.** Log once per window for skip reasons that matter operationally, especially `staged_update_unknown` or `status_degraded`.
5. **Improve diagnostics.** Include current `auto_unlock.json`, active/default snapshot IDs, snapper process/thread/fd summary, and recent update-status degradation in support bundles.
6. **Investigate snapper/qgroup cleanup policy.** 447 snapshots with `NUMBER_LIMIT=50` indicates cleanup is not keeping up. Evaluate qgroup settings, cleanup scheduling, and whether Piccolo should avoid qgroup-backed enrichment on appliance devices.

---

## Open Questions

1. Does cleanup succeed if Piccolo stops probing snapper for long enough, or is snapper cleanup independently broken by qgroup state?
2. Is snapshot `422` special/corrupt, or merely the oldest candidate cleanup repeatedly reaches before hitting the config lock?
3. Is qgroup accounting needed for the root snapshot set on Piccolo OS, or can it be disabled/tuned to avoid repeated qgroup rescans?
4. Should the update status background watcher run at all when the UI is not open, or should it be demand-driven after boot/apply/rollback events?

---

## Remediation Status

| Item | Status |
|------|--------|
| Manual reboot applied staged `v0.2.29` | Done |
| Evidence captured for snapperd thread/fd pileup and slow `snapper --json list` | Done |
| Patch auto-reboot to use fast active/default snapshot check | Open |
| Patch update status to compute reboot-required before snapper enrichment | Open |
| Add snapper enrichment backoff/single-flight | Open |
| Add scheduler skip observability | Open |
| Investigate snapper cleanup/qgroup policy and snapshot backlog | Open |
