# v0.2.43 runtime-lifecycle remediation context

Status: historical accepted-decision ledger. Released Slices 1 and 2 remain
authoritative, but the unshipped Slice 3 implementation was abandoned as a
release candidate on 2026-08-13. Do not use this file alone as authorization to
resume that implementation. See
`docs/incidents/2026-08-v043-slice3-remediation-retrospective.md`.

This file preserves the incident evidence, user-approved product decisions,
review lessons, and scope guardrails that must survive context compaction and
the fresh implementation branch. A later implementation plan may choose
mechanisms, but it must not silently rewrite the facts or decisions recorded
here.

## Scope

### Problem

Preserve the proven `v0.2.43` unlock-latency and stuck-app-lifecycle incident
evidence, along with the product decisions needed to remediate them without
repeating the superseded implementation's review-driven expansion.

### In scope

- Proven runtime observations and code-supported working causes.
- User-locked product behavior and reduced-design decisions.
- Anti-derail, slice, validation, and evidence guardrails for the accepted
  remediation plan.

### Out of scope

- Runtime implementation or implementation approval by this ledger alone.
- The separate post-update recovery/fallback UI implementation.
- Retention-policy, package-version, per-app parallelism, durable event
  delivery, or other work explicitly deferred below.

## Provenance

- Runtime release baseline: `35e0e23` (`v0.2.43`).
- Current reduced-branch base: `dd58fd3`; this adds only the separate
  post-update recovery RFC to the runtime baseline.
- Superseded implementation worktree:
  `/tmp/piccolod-v043-unlock-runtime-lifecycle`.
- Superseded implementation branch:
  `fix/v043-unlock-runtime-lifecycle`.
- Superseded implementation size at the pause checkpoint:
  77 changed files, 9,614 insertions, and 1,203 deletions.
- Root Codex transcript:
  `/home/abhishek-borar/.codex/sessions/2026/07/28/`
  `rollout-2026-07-28T15-31-11-019fa82b-d0b7-7622-9d44-c408d13d7f97.jsonl`.
- Superseded planning and review artifacts:
  `/tmp/piccolod-v043-lifecycle-plan.md`,
  `/tmp/piccolod-v043-review-ledger.md`, and
  `/tmp/piccolod-v043-code-review-ledger.md`.
- Separate recovery RFC:
  `docs/rfc/20260728-post-update-unlock-recovery.md`.
- Deferred durable-delivery architecture placeholder:
  `docs/rfc/20260729-durable-event-delivery-and-domain-reconciliation.md`.
- Accepted reduced implementation plan:
  `docs/rfc/20260729-v043-runtime-lifecycle-remediation.md`.

The superseded worktree is evidence and a regression-test quarry. Its
mechanisms are not the default design for the reduced implementation.

## Original runtime observations

### U1: post-update unlock latency regression

- The device was updated overnight to Piccolod `v0.2.43`.
- The unlock screen rendered normally.
- The correct password was cryptographically accepted.
- Failure occurred later in the post-decrypt `completeUnlockChain` path.
- The synchronous unlock chain crossed the existing 30-second liveness
  boundary.
- Fatal recovery recorded `unlock_chain_liveness` and restarted Piccolod.
- Locked-state health still returned HTTP 200/Warn, so boot health considered
  the deployment healthy.
- Rolling back to the previous release restored normal unlock.
- The affected device had a large retained storage population: approximately
  350 LVs and 165 rootfs records.

Working cause supported by code and runtime evidence: reconstructible
golden/rootfs settlement was made synchronous in the mandatory unlock path,
and repeated storage discovery amplified its cost.

### U2: stuck app uninstall blocks unrelated app lifecycle

- A custom AI-provider app could not be uninstalled.
- A new provider app remained stuck at Installing.
- The affected app's `podman stop --time 30` remained live for hours.
- Multiple `runc kill <container> 15` processes consumed sustained CPU instead
  of completing.
- The app user manager and mount remained active.
- Piccolod remained running while the global app lifecycle owner was occupied,
  preventing unrelated install/uninstall progress.
- A simple reboot was considered adequate host recovery; the product fix is
  preventing one app-specific runtime failure from blocking the daemon or
  rebooting the device.
- Observed packages were Podman 5.8.3, runc 1.4.3, conmon 2.2.1, and
  passt 20260612. Package replacement was not proven to be the remedy.

## Locked product decisions

These decisions came from the user and are requirements, not review
suggestions.

1. Core unlock remains bounded and synchronous only for password validation,
   core persistence, and control-plane readiness.
2. Reconstructible golden/rootfs maintenance runs after `Ready`.
3. Post-Ready maintenance retries indefinitely, but every attempt is finite
   and cancellation-aware, with capped backoff and visible Warn state.
4. Maintenance failure must not reverse `Ready`, relock the device, restart
   Piccolod, or reboot the host.
5. Retain the existing 30-second core-unlock liveness guard.
6. A foreground exact local-golden reuse path remains available after strict
   identity/content proof.
7. Collect broad LVM/VG inventory at most once per relevant reconciliation
   pass; do not perform a full rescan per artifact.
8. A Podman/runc command needs a Piccolod-enforced hard deadline independent
   of the container's graceful stop period.
9. When rootless teardown is ambiguous, use the dedicated per-app user/session
   containment boundary and prove both cgroup and numeric-UID process absence
   before destructive cleanup.
10. An app-specific lifecycle failure must never restart Piccolod or reboot the
    host.
11. Keep global app lifecycle serialization for this release. Per-app
    lifecycle parallelism is deferred.
12. A confirmed uninstall intent is durable and retries indefinitely across
    reconciliation and reboot.
13. A failed uninstall remains visible and disabled. It exposes `Retry now`;
    it is not presented as an ordinary stopped app.
14. User data is guaranteed retained before the durable purge fence.
15. After purge begins, the UI must say finalizing and that data may already be
    gone; recovery is no longer promised.
16. The app disappears only after physical cleanup succeeds.
17. `RemovalPending` immediately makes a capability provider ineligible.
    Select a deterministic eligible replacement or report the capability as
    unavailable/reconciling.
18. Retain the current capability-provider disclosure semantics; provider data
    is not migrated automatically.
19. Remove the public manual-lock API/UI. Intentional offline operation is
    power-off/reboot; internal shutdown/storage-lock primitives remain.
20. Do not change the 30-day retention or rollback policy until the 350-LV
    ownership audit supplies evidence.
21. Keep the current 30-second graceful app-stop period for now.
22. Manifest-declared stop grace is a future improvement, not part of this
    repair.
23. The post-update recovery RFC and its fallback UI are a separate
    implementation stream.
24. Active Stage, app-detail, and capability screens revalidate their
    authoritative backend projection every 30 seconds in addition to SSE,
    reconnect, activation, and trailing-edge refetches.
25. Every external runtime command executed while a lifecycle owner holds the
    global gate has a Piccolod-enforced finite deadline. The 45-second
    runtime-control bound also covers ordinary reconciliation, not only
    uninstall.

## Load-bearing obligations

The reduced design must preserve these outcomes even if it rejects every
mechanism from the superseded implementation.

- `Ready` is independent of reconstructible physical maintenance.
- Golden reuse remains identity-safe while maintenance and install overlap.
- No unbounded external runtime command can hold the global lifecycle owner.
- Teardown ambiguity is contained at the dedicated app identity boundary.
- Removal intent, irreversible-purge state, exact destructive ownership, and
  final completion survive process restart.
- Failed cleanup never publishes success or silently loses its retry owner.
- App identity cannot be reused while removal finalization is unproven.
- A removal-pending app cannot regain runtime, ingress, terminal, OIDC, or
  capability authority through a stale concurrent path.
- Background recovery is cancellable/joinable during daemon shutdown.
- The UI derives its recoverable/finalizing claims from durable backend truth.

## Mechanisms that are not approved for automatic porting

The independent subtractive audit found that the superseded branch repeatedly
turned a valid obligation into a new protocol, then fixed the new protocol's
composition failures. Do not port the following mechanisms unless a new
accepted plan proves that omitting that exact mechanism violates a locked
outcome:

- removal intent in app metadata plus a second global cleanup-record namespace
  plus a tombstone namespace;
- two removal attempt counters, a per-resource cleanup cursor, and many
  effect-complete booleans;
- registration of every queued lifecycle caller in a daemon admission
  registry with timer-polled mutex acquisition;
- RestoreServices whole-pass budgets, per-app quanta, and rotating cursor;
- AppManager-owned terminal PTY-start snapshots and global lifecycle callbacks;
- durable capability `old/new` pending-reconciliation deltas;
- capability UI polling or client event epochs as a substitute for one
  authoritative server projection;
- permanent per-app OIDC mutation fences when desired state can be derived;
- a global weighted golden handoff semaphore when identity-local ownership can
  carry the invariant;
- a permanent LV diagnostics API or an unapproved retention-policy change.

This list is not a claim that every listed line of code is incorrect. It is a
burden-of-proof rule for the fresh design.

## Locked reduced-design decisions

The following shapes replace the superseded branch's broader mechanisms:

1. Hydrate a metadata-only golden index before `Ready`; run physical
   settlement and GC post-Ready; preserve foreground exact-golden reuse through
   the existing identity-local ownership boundary.
2. Add `uninstall` as an operation in the existing per-app
   `app_transition_v2.json` authority. Do not create a second removal journal,
   global cleanup-record namespace, or tombstone state machine. The active
   uninstall transition itself is the durable removal intent and app-ID
   reservation until physical cleanup completes.
3. Replace direct blocking acquisition of the global app lifecycle mutex with
   one context-aware capacity-one gate. Request cancellation applies while
   waiting for admission; once an uninstall intent commits, its durable
   transition—not the HTTP request—owns finite retry attempts.
4. Keep global lifecycle serialization in this release. The gate is admission,
   not a waiter registry or scheduler; there is no per-app lifecycle
   parallelism, polling acquisition, or queued-caller bookkeeping.
5. The terminal manager owns app-terminal admission and drain. Uninstall blocks
   new sessions before committing intent, then closes and joins existing or
   in-flight sessions before runtime containment. AppManager does not own PTY
   snapshots or a terminal callback protocol.
6. Capability eligibility is derived from current durable app state. An active
   uninstall transition immediately excludes that provider; reconciliation
   chooses the deterministic eligible replacement or reports the capability
   unavailable/reconciling. No durable `old -> new` capability delta is added,
   and correctness does not require remembering the previous default.
7. OIDC uninstall cleanup is owned by one retained OIDC client manager.
   Uninstall requests an idempotent ensure-absent operation and cannot complete
   while app-scoped clients remain. OIDC install/pre-registration is not
   rewritten in this incident slice.
8. One backend-derived app/removal projection is authoritative for list/get and
   action eligibility. Events are wake notifications; the UI coalesces a
   refetch instead of maintaining a second removal or capability state machine.
9. The current in-process event bus remains non-authoritative and unchanged for
   this remediation. Durable outbox/event delivery and broader domain
   reconciliation are explicit follow-up work in
   `docs/rfc/20260729-durable-event-delivery-and-domain-reconciliation.md`.

## Anti-derail execution contract

The reduced plan and its site list were accepted by the user on 2026-07-29.
Runtime implementation may proceed only through its separately reviewable
slices.

For every substantive review finding:

1. Prove the baseline delta: pre-existing, introduced/worsened by this change,
   or adjacent.
2. Record the behavioral obligation separately from the reviewer's suggested
   remedy.
3. Evaluate the smallest resolution that preserves the obligation.
4. Pause for a repair-altitude decision before adding or expanding durable
   state, authority, lifecycle, protocol, operator surface, or a
   general-purpose abstraction.
5. Challenge an optional mechanism before implementing secondary findings
   created only by that mechanism.

Mechanical stop conditions:

- A change needs a subsystem or site outside the accepted slice site list.
- A remedy introduces a durable record, execution owner, lifecycle phase,
  cross-layer callback, or polling protocol absent from the accepted plan.
- A previously clean slice is substantively reopened more than once.
- Two consecutive findings share a remedy-created root or expand the same
  mechanism.
- Full diff or test growth cannot be explained by a locked obligation.
- A review proposes changing a locked product decision or an explicit
  out-of-scope item.

When any stop condition triggers, update the decision ledger and return to the
user. Do not open another automatic review/fix epoch.

## Slice discipline

The reduced implementation is expected to proceed only through separately
reviewable slices:

1. core unlock and golden indexing/maintenance;
2. bounded runtime teardown and containment;
3. durable removal;
4. narrowly approved terminal/capability/OIDC integration;
5. minimal UI/API and the read-only LV audit.

Later slices may use earlier contracts, but they may not retroactively enlarge
an earlier slice without a user-approved plan change. Existing superseded tests
are adversarial scenarios to evaluate, not mechanisms that must be ported.

## Deferred and untouched

- post-update recovery RFC implementation and fallback UI;
- automatic update rollback;
- retention-policy changes and destructive orphan-LV cleanup;
- runc/Podman/conmon/passt package changes without new evidence;
- per-app parallel lifecycle architecture;
- manifest-configurable stop grace;
- removal or extension of the 30-second unlock watchdog;
- generic artifact supervisors and permanent storage-diagnostics APIs.
- durable outbox/event delivery or a broad OIDC desired-state controller.

## 2026-07-30 review checkpoint

Fresh holistic review found that the reduced RFC and dirty Slice 1
implementation had moved and hardened the existing destructive generic
orphan-LV cleanup after `Ready`. That conflicts with this ledger's explicit
deferral of destructive orphan-LV cleanup and the read-only 350-LV audit
boundary.

The required root cut is subtractive: remove automatic generic orphan deletion
and its allocation-versus-deletion protocol from this remediation. Post-Ready
maintenance may retain one strict inventory for scoped golden/rootfs
settlement and GC; unknown LV ownership remains read-only evidence for the
later audit.

The same review found two plan clarifications that preserve already-locked
behavior without new authority:

- containment uses a child context derived from, and capped by, the remaining
  two-minute uninstall attempt rather than a new independent owner;
- one automatic lifecycle-gate admission dispatches at most one uninstall
  phase attempt before releasing.

The user selected a simple 30-second authoritative revalidation while Stage,
app-detail, or capability screens remain active. This bounds a dropped SSE
wake without adding durable event delivery or a removal-specific client state
machine.

At the final Plan Review iteration, the user also confirmed that ordinary
reconciliation cannot retain the global lifecycle gate through an unbounded
external runtime command. Runtime observation/control commands use the same
local hard-bound policy as uninstall teardown; longer transfer/materialization
work remains subordinate to its explicit finite operation budget.

## Remaining evidence gates

- classify the 350-LV population by owner, age, reference, and retention reason;
- alpha/device unlock timing with representative golden/rootfs inventory;
- timeout and process-absence proof for the observed runc/Podman failure class;
- crash/restart replay at each accepted removal phase;
- immediate capability handoff with and without a replacement;
- broad race, Go, Flutter, independent review, RFC-alignment, and alpha-VM
  gates after the reduced implementation converges.
