# v0.2.43 lifecycle remediation: Slice 3 retrospective and abandonment record

Status: **Historical decision record. The unshipped Slice 3 implementation is
abandoned as a release candidate as of 2026-08-13. Do not resume or merge its
complete dirty diff.** Released Slices 1 and 2 remain supported and are not
part of this abandonment.

This document preserves why the work started, what was proven, what shipped,
what was attempted, why the later implementation did not converge, which
technical and product decisions remain useful, and how a fresh effort must
avoid repeating the same patchwork. It is intentionally a retrospective, not
a new implementation RFC.

## Executive conclusion

Piccolod is not proven unfixable, and the original incidents were real. The
failure was the shape of the unshipped Slice 3 effort.

The effort started as a bounded unlock-latency repair plus a stuck-uninstall
repair. Slices 1 and 2 successfully shipped those bounded foundations in
`v0.2.44`. Slice 3 then attempted to make install publication, uninstall,
resource cleanup, process containment, all external authority, exact app
identity, API behavior, and UI presentation converge as one atomic lifecycle
contract across the existing codebase.

That contract crossed too many existing procedural owners at once. Each local
review correction was usually rational, but the cumulative result became a
partial lifecycle rewrite: 123 tracked files and 33 untracked files, with
18,970 tracked insertions and 3,505 tracked deletions in the final recovered
worktree. Reviews stopped validating a stable design and repeatedly discovered
new parts of the design through missed composition sites.

The correct action is therefore:

1. keep released Slices 1 and 2;
2. preserve the Slice 3 experiment's evidence and lessons in this record;
3. do not merge or separately archive its complete implementation;
4. return to current `main` for a fresh architectural assessment; and
5. permit a deeper app-lifecycle refactor if that is smaller and more coherent
   than another compatibility patch across the present owners.

## Provenance and preservation boundary

### Released history retained

- `35e0e23` — `v0.2.43`, incident release baseline.
- `d59bbf6` — Slice 1, `fix: move rootfs maintenance out of unlock`.
- `6fea2e3` — Slice 2 and `v0.2.44`, `fix: enforce bounded app lifecycle
  recovery`.
- Main at the time of this record: `4334f47`.

Slices 1 and 2 are independently landed commits. They are not experimental
debris and must not be reverted merely because Slice 3 failed to converge.

### Historical planning and evidence

- Accepted remediation RFC:
  `docs/rfc/20260729-v043-runtime-lifecycle-remediation.md`.
- Accepted incident/product decision ledger:
  `docs/incidents/2026-07-v043-runtime-lifecycle-context.md`.
- Separate durable-event architecture placeholder:
  `docs/rfc/20260729-durable-event-delivery-and-domain-reconciliation.md`.
- Separate post-update recovery plan:
  `docs/rfc/20260728-post-update-unlock-recovery.md`.
- Root Codex transcript:
  `/home/abhishek-borar/.codex/sessions/2026/07/28/`
  `rollout-2026-07-28T15-31-11-019fa82b-d0b7-7622-9d44-c408d13d7f97.jsonl`.

The RFC and context ledger remain historical sources of constraints and
decisions. They are no longer authorization to resume the abandoned Slice 3
implementation.

### Experimental implementation snapshots

The first broad experiment was recorded in:

- worktree: `/tmp/piccolod-v043-unlock-runtime-lifecycle`;
- branch: `fix/v043-unlock-runtime-lifecycle`;
- recorded pause size: 77 changed files, 9,614 insertions, 1,203 deletions.

The later desired-absence experiment was rebased on:

- commit: `ce8593c` (`docs: rebase durable uninstall on desired app state`);
- parent: `6fea2e3` (`v0.2.44`);
- recovered clone: `/tmp/piccolod-v044-slice3-clean-impl-recovered`;
- branch: `fix/v0244-slice3-clean-impl-recovered`;
- final observed dirty size: 123 tracked modified files, 33 untracked files,
  18,970 tracked insertions, 3,505 tracked deletions.
- tracked binary-diff SHA-256 at the abandonment checkpoint:
  `14977deea412ce9a33a01bc9b930ea23fa807d6f5666060873b7e47fafd259b3`;
- sorted untracked-file content-manifest SHA-256 at the same checkpoint:
  `aa83328d9e59b7a9a092fdfe9e58b1436e3bf8383106c31f90aaf3b911fbb74c`.

Other preserved worktrees/branches existed under
`/media/abhishekborar/Aux/tmp/piccolod/`, including the original Slice 3,
clean-implementation, and desired-absence variants.

The final recovered implementation is not committed, and that is intentional.
The user decided on 2026-08-13 that preserving the exact failed source is not
necessary and could anchor a fresh effort back to the same patchwork. This
retrospective preserves the design and failure history, not every experimental
source line. After this repository record is safely committed, the temporary
clone may be deleted without creating an archival WIP commit or patch bundle.

### Runtime diagnostic sources

- `~/Downloads/piccolod-diagnostic (5).log`, SHA-256
  `f441a02e655cee57a6039db6d14aafe70656f5e53cd6648d01fb0833c21b7594`.
- `~/Downloads/piccolod-diagnostic (6).log`, SHA-256
  `d406dedbd0ae0e7d2487f41aeea4cfd8deb99d82f345e4f0525e05ca2f052e33`.

These paths are local evidence locations, not repository artifacts. Their
hashes identify the sources used by this investigation.

## The original problems

### Incident U1: correct-password unlock failed after `v0.2.43`

The affected device rendered the unlock screen and cryptographically accepted
the correct password. Failure occurred later in the post-decrypt
`completeUnlockChain` path. The synchronous chain exceeded the existing
30-second liveness boundary, recorded `unlock_chain_liveness`, and restarted
Piccolod. Locked-state health still returned HTTP 200/Warn, so boot health did
not reject the deployment. Rolling back restored unlock.

The code-supported working cause was reconstructible golden/rootfs settlement
inside mandatory unlock, amplified by repeated physical inventory on a device
with approximately 350 LVs and 165 observed rootfs records. Exact per-stage
timing was not captured, so this remains a supported working cause rather than
an overclaimed complete RCA.

### Incident U2: one stuck uninstall blocked unrelated lifecycle work

A custom AI-provider app would not uninstall, and a new provider app remained
stuck at Installing. Runtime evidence showed:

- `podman stop --time 30` alive for hours;
- multiple `runc kill <container> 15` processes consuming sustained CPU;
- the app user manager, containers, and app-data mount still present;
- Piccolod itself still running; and
- one globally serialized app-lifecycle owner preventing unrelated progress.

A reboot was an acceptable one-time host recovery. Rebooting Piccolod or the
device was explicitly rejected as the product response to one app-specific
failure.

The evidence did not prove that Podman, runc, conmon, or passt package
replacement was the correct remedy.

## What shipped successfully

### Slice 1: unlock and reconstructible maintenance

Commit `d59bbf6`, later included in `v0.2.44`, established useful and retained
boundaries:

- core unlock no longer waits for physical rootfs/golden maintenance;
- physical maintenance runs after `Ready`;
- one broad inventory can be shared across the pass;
- unknown LVs are not automatically deleted;
- exact foreground reuse remains possible after strict proof; and
- the public manual-lock path was removed in favor of power-off/reboot for
  intentional offline operation.

### Slice 2: finite runtime ownership

Commit `6fea2e3` / `v0.2.44` retained global lifecycle serialization but made
the boundary finite and joinable:

- queued admission observes request cancellation;
- Podman/runtime control receives Piccolod-owned hard bounds;
- process groups are canceled and reaped;
- app-user cgroup plus numeric-UID process absence form the containment proof;
- background owners are canceled/joined on shutdown; and
- an app-specific failure does not intentionally restart Piccolod or reboot the
  host.

These two slices addressed the most direct incident mechanisms and remain the
stable starting point for future work.

## Product decisions that remain useful

These decisions survived multiple design attempts. A fresh architecture may
choose different mechanisms, but it should not silently change the behavior
without returning to the user.

1. Core unlock remains bounded; reconstructible or app-specific work is not a
   prerequisite for device `Ready`.
2. Post-Ready maintenance retries indefinitely, but attempts are finite,
   cancellable, non-overlapping, and visible as degraded/Warn when necessary.
3. Keep the 30-second core-unlock liveness guard.
4. An app-specific failure must not restart Piccolod or reboot the host.
5. Keep global app lifecycle serialization until a separate design justifies
   per-app parallelism.
6. Confirmed uninstall has no undo. It must survive request loss and restart
   until the system either converges or reports durable unresolved state.
7. Retry is infinite only when something can change between attempts; every
   individual retry remains finite and yields the global owner.
8. A failed uninstall stays visible and non-actionable except for retry or
   inspection. It is never misrepresented as an ordinary stopped app.
9. An uninstalling app must not remain an eligible capability provider.
10. App-specific failure must not block unrelated app lifecycle indefinitely.
11. Do not change retention or automatically delete unknown LVs without an
    exact read-only ownership audit and a separate product decision.
12. Keep the current 30-second graceful app-stop setting unless a later feature
    makes it manifest-configurable.
13. Events are wake notifications unless durable delivery is separately
    designed; authoritative state must survive a missed event.
14. The public manual-lock feature remains removed.
15. It is acceptable that an already-loaded app A iframe can briefly send
    ordinary non-OIDC name-routed traffic to same-name replacement B before
    Piccolo's shell refreshes. Do not build a browser/session revocation system
    for this incident.

## Attempt history

### Attempt A: phase-heavy uninstall transition

The first implementation placed uninstall intent in the existing transition
journal and grew a multi-phase lifecycle around it. It introduced or proposed:

- uninstall pending, containing, finalizing, and identity-retiring phases;
- a committed/completed removal representation;
- captured identity tuples and removal receipts;
- per-resource progress and cleanup classification;
- retry arbitration and new process-local owners;
- broad lifecycle callbacks into terminal, capability, OIDC, and UI; and
- increasingly detailed destructive-cleanup proofs.

Why it failed:

- uninstall desired state and transition progress became overlapping
  authorities;
- each new phase created interruption, restart, retry, and UI composition
  obligations;
- review fixes expanded the protocol faster than they reduced uncertainty;
- the diff grew to 77 files before a stable vertical uninstall path existed;
  and
- mechanisms were being defended because they had been introduced, not because
  the original incidents required them.

The key learning was that a transition journal should not become authoritative
for a fact that belongs to desired state.

### Attempt B: reduced slice plan

The implementation was restarted around smaller slices and existing owners.
The plan tried to retain:

- one global capacity-one lifecycle gate;
- existing per-app transition replay;
- terminal/OIDC/capability managers as their own resource owners;
- list/get as authoritative UI projections; and
- no new cleanup queue, scheduler, or durable event system.

This produced the two successful shipped slices. It did not provide a small
enough Slice 3 because durable uninstall still composed almost every app
subsystem.

### Attempt C: desired-absence architecture rebase

On 2026-08-02 the user accepted a subtractive root cut: removal intent moved
from transition phases to `present|absent` in existing app desired state.
There would be no uninstall phases, completion bit, cleanup cursor, or durable
retry scheduler. The existing reconciler would repeatedly perform one finite,
idempotent deletion-only attempt, and the active app directory would reserve
the app ID until atomic retirement.

This was a real architectural improvement. It removed duplicated uninstall
state. It nevertheless expanded into a larger app-lifecycle contract because
existing installation and resource owners were not already organized around
one authoritative aggregate. To make desired absence safe, the effort then had
to define all of the following together:

- exact publication/incarnation identity;
- conditional metadata and sidecar writes;
- durable publication and directory-sync ambiguity;
- post-publication install convergence without rollback;
- disposable-generation reset and exact init replay;
- cancellation/join ownership for long preparation;
- strict deletion-only owners for runtime, users, volumes, rootfs, artifacts,
  logs, slices, services, OIDC, capability, accelerator, and terminal state;
- final durable namespace retirement; and
- UI/API behavior for pending, unavailable, ambiguous, replaced, and retired
  installations.

The desired-absence choice was not itself the failure. The failure was trying
to retrofit its full implications across all existing owners in one release
slice.

### Attempt D: exact-incarnation and external-authority closure

Reviews found that display timestamp `CreatedAt` was not a safe authority
token. The experiment introduced a random opaque `i2:` incarnation for new
publications and stable disjoint `l1:` compatibility identity for legacy
publications. That identity propagated through:

- API mutation expectations;
- uninstall confirmation;
- conditional persistence and retirement;
- install task/UI handoff;
- terminal and log admission;
- OIDC client, code, token, proxy-state, and app-session lineage; and
- focused desktop/detail windows.

This closed real same-name A-to-B races. It also made the migration
composition-wide: every action, reader, serializer, compatibility path, and
test fixture had to choose exact or name-compatible semantics. Missed call
sites repeatedly appeared as new blockers.

The user explicitly narrowed one overreach: ordinary app iframe traffic stayed
name-routed. This was a useful product-level subtraction and should be
remembered.

### Attempt E: strict owner and process-boundary hardening

The experiment added or hardened:

- process-group-aware external command cancellation/reaping;
- finite child budgets under a two-minute aggregate attempt;
- direct local passwd/group/subuid/subgid identity proof;
- exact descriptor-relative filesystem deletion;
- storage parent-directory sync before retirement;
- strict service listener and accepted-connection drain;
- runtime-observation reservations and cancellation/join; and
- independent authority withdrawal before destructive storage cleanup.

Many of these are good low-level primitives. They did not make the cumulative
Slice 3 independently releasable because their caller composition was still
being discovered during review.

### Attempt F: UI ambiguity and background ownership

The Flutter work attempted to make install/uninstall truthful across request
loss, daemon restart, same-name replacement, view disposal, and missed events.
It introduced process-local background install ownership, exact view binding,
cross-view fences, authoritative refetch generation checks, unavailable
projections, stale-confirmation deltas, and explicit retired detail state.

The useful learning is that a widget cannot be the sole owner of an operation
which outlives the widget. The failure was allowing UI repair to become another
large lifecycle subsystem before the backend aggregate and wire contract were
stable.

## Review history and what it means

The cumulative temporary review ledger recorded 26 numbered review epochs,
64 P1-labelled findings, 31 P2-labelled findings, and at least 12 explicit
cross-phase convergence checkpoints.

These numbers must not be interpreted as 95 independent production defects:

- some findings were sibling call sites of one missing invariant;
- some were test/fixture or schema propagation misses;
- some were theoretical candidates later rejected;
- some were defects created by an earlier attempted remedy; and
- severity was intentionally conservative at destructive and authority
  boundaries.

However, the recurrence itself is decisive evidence that the implementation
was not converging. The principal clusters were:

| Cluster | Repeated lesson |
| --- | --- |
| State authority | Desired, observed, transition, cache, and presentation state must not become competing authorities. |
| Exact identity | A name is not an incarnation, but introducing an incarnation is a whole-contract migration, not a local field addition. |
| Cleanup ownership | Every destructive effect needs an existing exact owner, idempotent absence proof, and retry anchor. A generic cleanup coordinator cannot manufacture ownership. |
| Execution ownership | Context cancellation is not process completion. Child processes, streams, accepted connections, and background preparation need cancel-and-join ownership. |
| Observation versus mutation | GET/status/log/readiness paths must not silently create, attach, backfill, or repair state. |
| External authority | Routes, OIDC, capabilities, terminals, logs, and app sessions can outlive metadata unless admission and withdrawal share a final authoritative boundary. |
| Identity teardown | Passwd identity, UID receipts, subid ranges, home ownership, cgroups, and slice policies have partial states and cannot be inferred from one another after deletion. |
| Durability | A visible rename/write is not necessarily a durable commit; ambiguity must not authorize destruction or premature success. |
| UI lifetime | Request, task, app incarnation, widget, and desktop session have different lifetimes. A widget-local state machine cannot own backend convergence. |
| Review process | A holistic reviewer repeatedly finding sibling sites means the architecture/site census is incomplete; it is not a reason for unlimited local patching. |

### Latest unresolved findings at abandonment

The final fresh Phase-1 discovery still found:

1. **Accelerator uninstall non-convergence.** Initial withdrawal could remove
   the ACL and durable accelerator grant, then app-user deletion occurred, but
   historical `AcceleratorDevices` metadata remained. Final reproof treated
   it as live authority and required the now-absent user. The app could remain
   permanently `Uninstalling`.
2. **Unavailable projection schema mismatch.** Runtime GET/list responses and
   OpenAPI disagreed over identity-only data, required status, and
   `unavailable` casing.
3. **Incomplete LV audit.** Retained diagnostics prove a 350-LV inventory and
   only a partial owner set. The exact incident device was not reachable for a
   complete read-only recapture, so no deletion or retention conclusion is
   authorized.

The first finding repeated the strict-owner/reproof cluster after the
explicitly capped architecture restart. That was the final stop condition.

## Why the effort is abandoned

The following facts together make another local patch iteration the wrong
choice:

- Slice 3 was no longer independently reviewable or releasable.
- Its dirty diff touched most app lifecycle, persistence, service, auth, API,
  and UI surfaces.
- Review was still discovering missing ownership and serialization rules after
  broad tests passed.
- Several fixes created new state or ownership that then required further
  fixes at sibling sites.
- Parallel implementation in one shared dirty tree caused substantial
  signature/fixture integration churn and made semantic regressions harder to
  distinguish from migration fallout.
- Alpha/device validation was deferred until too late; the architecture grew
  primarily under static and unit-level adversarial review.
- Important artifacts and the cumulative review ledger lived under `/tmp`,
  making recovery and context continuity part of the engineering burden.
- The agreed convergence cap was reached repeatedly.

Passing unit, race, vet, analyzer, or broad package tests did not discharge
this. Those tests proved many individual primitives; they did not prove that
the complete lifecycle aggregate was coherent.

## What not to conclude

This retrospective does **not** establish that:

- Piccolod must be rewritten from scratch;
- the existing reconciler is useless;
- every P1-labelled review finding was a practical incident;
- Podman/runc package versions caused both original incidents;
- approximately 350 LVs are leaks or safe deletion candidates;
- a durable event bus would automatically solve lifecycle ownership;
- distributed transactions, consensus, or event sourcing are required; or
- every experimental primitive is wrong.

It establishes only that the current system boundaries plus the attempted
cross-cutting Slice 3 migration were not a tractable release unit.

## Reusable material from the experiments

The lessons and regression scenarios recorded here may be re-derived after a
fresh design review; the temporary worktree does not need to survive. Candidate
reusable material includes:

- concrete failure scenarios and focused regression tests;
- bounded external-command process-group cancellation and reaping;
- direct app-user partial-state tests;
- exact descriptor-relative child-tree deletion;
- strict LUKS/rootfs/artifact ownership checks;
- endpoint accepted-connection drain tests;
- runtime-observation stream cancellation/join tests;
- authoritative-unavailable readiness invalidation tests;
- uninstall confirmation and same-name replacement UX cases; and
- the read-only LV audit procedure.

Reuse is not approval. Each item must be re-derived from the fresh design's
owner boundary. Do not copy coordinator code, state machines, or the complete
site list merely because tests exist for them.

## Material that must not be ported by default

- The complete dirty Slice 3 diff.
- Uninstall phases, committed cleanup records, tombstones, or resource cursors.
- A general lifecycle scheduler, waiter registry, or durable client operation
  journal.
- Exact-incarnation propagation into every surface without a product and API
  compatibility decision.
- A general desktop window lifecycle framework.
- Per-window iframe proxy identity or browser revocation.
- A helper process whose only purpose is manufacturing a timeout around
  uninterruptible local filesystem I/O.
- Automatic deletion or retention reclassification for unknown LVs.
- Review-generated mechanisms whose only obligation was introduced by an
  earlier optional mechanism.

## Fresh-start charter

A new effort begins from current `main`, not from the dirty Slice 3 worktree.
Before implementation it should perform a fresh codebase-level architecture
assessment and explicitly consider whether a deeper refactor is smaller than
compatibility patching.

### Questions to answer before a new RFC

1. What is the single authoritative app aggregate: definition, desired
   presence, desired runtime intent, immutable identity, and observed state?
2. Which component alone commits desired-state changes?
3. Which reconciler alone owns convergence, and which existing procedural
   handlers must stop performing lifecycle effects directly?
4. What is the minimal resource-owner interface shared by runtime, storage,
   service publication, identity, OIDC, capability, terminal, and log owners?
5. Which resources are reconstructible and may degrade locally, and which are
   security or deletion authorities that must fail closed?
6. Can install and uninstall use one symmetric desired-state/reconcile model,
   rather than separate procedural pipelines?
7. Which exact-identity guarantees are product requirements, and which were
   defensive review expansion?
8. Is the existing global gate still the right aggregate boundary, or should a
   refactor first make app ownership explicit and only then revisit
   concurrency?
9. Which event consumers need durable delivery, if any, after desired state is
   authoritative?
10. What is the smallest end-to-end vertical slice that can run on the alpha VM
    before touching UI/OIDC compatibility breadth?

### Execution rules for the fresh effort

1. Start with executable incident acceptance tests, not with a broad schema
   migration.
2. Prove one vertical path on the alpha VM early: install, injected stuck
   teardown, finite release, unrelated app progress, reboot, convergence.
3. Keep code-review slices genuinely independently reviewable. A slice that is
   not safe to commit or exercise is not a slice.
4. Set a small changed-surface budget. Crossing it requires an explicit
   architecture checkpoint before more implementation.
5. Do not run another whole-diff fix loop after the same invariant appears in
   two sibling surfaces. Return to architecture immediately.
6. Separate practical runtime failures from hardening opportunities and label
   review severity accordingly.
7. Keep one durable repository handoff current. Do not make `/tmp` ledgers or
   session compaction the only source of truth.
8. Land low-level primitives only when their production caller and one
   end-to-end behavior are part of the same reviewed slice.
9. Treat UI, OIDC, logs, and terminals as consumers of an already-stable app
   lifecycle contract, not as co-designers of that contract during the first
   backend slice.
10. Stop before implementation if the new plan again requires simultaneous
    migration of most app writers, resource owners, APIs, and UI surfaces.

## Suggested first fresh milestone

The first milestone should be diagnostic and architectural, not a code port:

1. map every current app lifecycle writer and resource owner on clean main;
2. trace the exact present-day install and uninstall paths without reference to
   the abandoned mechanisms;
3. run the original stuck-runtime shape on the alpha VM or a controlled test;
4. propose at most two architectures: a minimal repair and a deeper aggregate
   refactor;
5. compare their changed surfaces and independently releasable milestones; and
6. ask the user to choose before implementation.

The abandoned branch supplies adversarial cases and lessons to that assessment.
It does not supply the default architecture.

## Final disposition

- Released Slices 1 and 2: **retain**.
- Unshipped Slice 3 implementation: **abandon as release candidate**.
- Experimental worktrees: **do not merge or archive; they may be deleted after
  this repository record is safely committed**.
- Existing RFC/context: **historical input, not resume authorization**.
- LV deletion/retention changes: **not authorized**.
- Next session: **fresh architecture assessment from main, with full refactor
  explicitly available as an option**.
