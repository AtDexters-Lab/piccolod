# Plan: Lazy service-rootfs attach in container reconcile

**Date:** 2026-05-21
**Status:** Draft — pending review

## Scope block

**Problem:** `reconcileContainerGroup` runs every 30 s per running app and, at the top, eagerly calls `ensureAllServiceRootfsAttached` to build `blockNativeRootfsMap` — but that map is consumed only by the create/recreate branches. In steady state (containers already running) the map is built and discarded, costing 6×/tick idempotent attach probes (metadata read + mount-table scan) and an unconditional `INFO: attached service rootfs` log per service (~17k lines/day per app, evicting useful journal history faster).

**In scope:** make the service-rootfs attach in `reconcileContainerGroup` **lazy** — performed only when a create/recreate branch actually needs the handles.

**Out of scope:**
- The other callers of `ensureAllServiceRootfsAttached` (install, group lifecycle, catalog-sync) — they attach as part of legitimate install/start events; unchanged.
- `AttachRootfs` idempotency/logging internals, and the dedicated `ReconcileRootfsStates` unlock-time reconciler — unchanged.
- The reconcile cadence (30 s) and container-state inspection — unchanged.
- The app-log download feature (separate, already shipped).

## Validation of intent

The eager attach is an incidental create-precondition, not a deliberate per-tick self-heal (origin: `1a3b496`/`efc0ef4`, plumbing rootfs handles into the create path; a dedicated `ReconcileRootfsStates` already handles rootfs reconciliation at unlock). Healing therefore stays correct reactively — see the Q3 "Healing" bullet.

## Decision / shape

Replace the eager build with a memoized accessor and call it only where handles are needed:

```
rootfsMap := sync.OnceValues(func() (map[string]*rootfsMountInfo, error) {
    return m.ensureAllServiceRootfsAttached(ctx, appInst.InstanceID, mode, def, appInst)
})
```

`sync.OnceValues` (Go 1.25) computes once on first call, caches the result+error, and is a no-op if never called — so steady-state reconcile (no create/recreate branch reached) never attaches. Consumers (site list) resolve `bn` via the accessor before their (re)create.

**Error-memoization semantics (decision, not "identical to today"):** `ensureAllServiceRootfsAttached` is all-or-nothing — any single service's attach failure runs `rollbackAttached()` and returns `(nil, error)`. `sync.OnceValues` caches that error for the rest of the pass, so one service's transient attach failure aborts the whole reconcile pass. This is **inherited from the current eager early-return** (line 173 aborts the tick on any attach error today) — not newly introduced; the change just moves the abort from tick-start to first-recreate-branch. It self-corrects on the next 30 s tick (the accessor is re-created per `reconcileContainerGroup` call). Per-service attach independence would require changing `ensureAllServiceRootfsAttached`'s all-or-nothing contract — **out of scope**; we keep the per-pass abort.

**Error-ordering nuance:** with the eager build a transient attach error aborts the tick *before* container inspection; with the lazy accessor it surfaces only when the first recreate branch is reached. So steady-state ticks that previously failed-fast on a transient attach error now proceed through inspection. Benign — fail-closed is preserved (no container is (re)created against an unattached rootfs), just at a different point.

## Site list (Q1 — surface area), all in `internal/app/container_group_reconcile.go`

A `grep blockNativeRootfsMap` is the surface-area check: **six** consumer sites (corrected from an earlier hand-enumeration that missed 286 and 485).

| # | Site | Change |
|---|------|--------|
| 1 | `reconcileContainerGroup` ~168–175 (eager build, guarded by `desiredRunning`) | Replace eager `blockNativeRootfsMap` build + early error return with the `rootfsMap` memoized accessor (still gated on `desiredRunning`; for `!desiredRunning` never created/called). |
| 2 | ~257 `recreateMissingMultiContainer` (anchor missing) | `bn, err := rootfsMap(); if err != nil { handleStartupFailure; return err }` then pass `bn`. |
| 3 | ~286 `recoverStaleAnchor` (anchor exists but `StartContainer` **failed** → recover/recreate) | Resolve `bn` via accessor + error handling, pass `bn` to `recoverStaleAnchor`. |
| 4 | ~348 `createAndStartServiceContainer` (service container missing) | Resolve `bn` via accessor, then index `bn[svcName]`. |
| 5 | ~382 `recreateServiceContainer` (existing service `StartContainer` failed → recreate) | Same. |
| 6 | ~433 `recreateMissingMultiContainer` (per-service recreate path) | Same — **resolve + error-check `bn` *before* the branch's stop/remove/Deactivate**, so a transient accessor error returns without leaving the group half-torn-down (REDTEAM-2). |
| 7 | ~485 `recoverStaleAnchor` (stale-DNAT repair → recover/recreate) | Resolve `bn` via accessor **before `handleStartupFailure` (line 478) and before any teardown**, returning on accessor error first. This keeps a transient attach error from counting as a startup failure (which over ~5 ticks would escalate a *running* app to `StatusError`) and from tearing down before recreate — matching eager's pre-478 abort (REDTEAM-1/2). |

Genuinely-plain start paths — anchor `StartContainer` success (~281) and service `StartContainer` success (~368) — do **not** consume the map. But their **failure siblings do** (286, 382): a failed start escalates to recreate, which needs the handles. That distinction is the correction over the earlier draft.

## Q2 — named behavior at each site

- **Accessor error** at any consumer: wrap/return the error before the (re)create runs (see Decision for ordering). Fail-closed: no container (re)created without its rootfs.
- **Branch not reached** (steady state — anchor + all services running): accessor never invoked → no attach, no probe, no log. This is the fix.
- **Legacy/no-rootfs apps:** `ensureAllServiceRootfsAttached` returns `(nil, nil)`; consumers already treat a missing map entry as "image mode" (`if svcRootfs, ok := bn[svc]; ok`). A nil map from the accessor preserves this — `ok` is false, image-mode path taken. Unchanged.

## Q3 — invariants preserved (composition / sibling-shape audit)

- **Shared-utility refactor:** only the *reconcile* call site changes timing; other `ensureAllServiceRootfsAttached` callers are untouched (see Out of scope).
- **Healing — de-frequented, not lost (decision):** the common case (rootfs detaches → container dies → recreate branch → accessor attaches) is preserved. The red-team surfaced one residual case the "container holds its mount" claim does *not* cover: a service rootfs is a two-layer mount (raw + idmap bind); the raw mount can be lazy-unmounted while the idmapped container keeps running on pinned inodes, so the probe sees `StaleMountRecord`/`PartialMapperOnly` but `InspectContainerState` still reports Running → no recreate branch fires → the lazy accessor never re-attaches. Today's eager per-tick attach silently re-attaches this every 30 s. **Decision: accept.** The trigger (raw lazy-unmount under a *surviving* idmapped container) is rare on a single-node appliance (kernel-state churn that detaches a rootfs typically also kills the container, which hits the recreate path), the impact is latent (not data loss), and recovery still occurs via the unlock-time `ReconcileRootfsStates` + the next container restart/recreate. **Periodic rootfs self-heal, if ever wanted, belongs in `ReconcileRootfsStates` (the dedicated rootfs reconciler) — not bolted onto per-tick container reconcile.** Bolting a per-tick probe back into reconcile would re-introduce the cost this plan removes, at the wrong layer.
- **State mutations between old/new abort points (REDTEAM-1):** lazy proceeds past line 173 to the recreate branches, so two mutations now run on attach-failing ticks that eager suppressed: `markTupleHealthy` (line 409, one-way `EverHealthy` latch) and `handleStartupFailure` (line 478, DNAT path). `markTupleHealthy` is **accepted** — it latches "healthy" only when containers are confirmed Running, which is *more* correct than eager suppressing it on an unrelated attach hiccup. `handleStartupFailure` escalation is **prevented** by the Site-7 ordering (resolve `bn` before line 478), so a transient attach error never counts as a startup failure.
- **Per-attach side effects de-frequented (S1) — assessed benign:** `AttachRootfs`'s Attached fast-path also runs `verifyIDMapFingerprint` and `upgradeUndersizedWorkspace` each call. Losing per-tick cadence is acceptable: the fingerprint is pinned at startup (`backfillIDMapFingerprintsAtStartup`) and re-checked on every create/recreate (the fast-path check only catches out-of-band metadata hand-edits across a restart — out of the single-user threat model); `upgradeUndersizedWorkspace` is workspace-only (service rootfs is a read-only snapshot → it returns immediately), one-time, idempotent, and still runs at unlock + on any recreate. Neither heal is lost, only de-frequented from the wasteful 2,880×/day.
- **Concurrency:** `reconcileContainerGroup` runs single-goroutine per app from the reconcile loop; `sync.OnceValues` is goroutine-safe regardless. No shared-state change.
- **`desiredRunning == false`:** today the eager build is skipped under `if desiredRunning`; the accessor is likewise only constructed/called on the running path, so stopped/follower behavior is unchanged.

## Sequencing
1. Introduce the accessor + rewire all six consumer sites (single file).
2. Code review.
3. Alpha-VM check: app running → confirm steady-state reconcile no longer logs `attached service rootfs` every 30 s; then kill a service container → confirm reconcile recreates it (accessor attaches rootfs, container returns).

## Acknowledged / not addressed
- The per-tick `InspectContainerState` probes and the 30 s cadence remain (intended reconcile behavior). This plan removes only the redundant rootfs attach, not the container-state reconciliation.
