# RFC: Minimal Post-Update Unlock Recovery

**Problem:** A newly updated Piccolo can serve the unlock screen and accept the
correct password but fail before the system becomes Ready. The user then cannot
reach the existing update, reboot, rollback, or diagnostic controls.

**In scope:** Provide password-authenticated recovery controls while Piccolo is
Locked or Failed; distinguish password rejection from post-password startup
failure; record the exact pre-update root and existing rollback-guard result
before a Piccolod-controlled reboot into a staged root; and reuse the existing
OS update manager for update, exact-fallback rollback, and reboot.

**Out of scope:** Changing MicroOS boot-health policy; automatic rollback from
unlock failures; fleet or Namek commands; routine diagnostic upload; fallback
pinning or Snapper cleanup changes; hard-power-loss coverage before the
protected reboot call; bootloader or process-down recovery; arbitrary shell,
app, or snapshot access; general snapshot lifecycle or cross-version
persistence compatibility; recovery support in pre-feature snapshots; durable
classification of an unlock attempt after Piccolod exits; password recovery in
hard storage emergency; and strengthening ordinary rollback's existing
app-mutation concurrency semantics.

## Context And Decision

The `0.2.43` incident fell between two existing recovery surfaces:

- MicroOS health-checker protects boots where Piccolod does not start or boot
  health reports failure.
- Piccolod normally exposes update, reboot, and rollback only after portal
  login and, for mutations, lifecycle Ready.

A manually locked appliance is a valid boot, so boot health cannot wait for a
user or classify lack of unlock as failure. A failure after password acceptance
therefore remains a Piccolod concern and is recovered from the unlock screen.

The design adds only:

1. one marker for an unconfirmed staged boot and its exact fallback;
2. one recovery-only authenticated session and panel.

There is no automatic classifier, rollback daemon, fleet control plane,
snapshot pin, or second durable operation workflow.

## Design

### 1. Protect One Controlled Staged Reboot

Before a Piccolod-controlled reboot consumes a staged default root, the update
manager atomically writes:

```json
{
  "schema_version": 1,
  "source_snapshot": "1016",
  "candidate_snapshot": "1017",
  "fallback_snapshot": "1016",
  "rollback_guard": "allowed",
  "phase": "pending"
}
```

The marker lives at `/piccolo-core/update/unconfirmed-boot.json`.

- `source_snapshot` is active when reboot is requested.
- `candidate_snapshot` is the staged default root.
- `fallback_snapshot` is the exact pre-update root.
- `rollback_guard` is `allowed`, `blocked`, or `unknown`.
- `pending` can authorize exact fallback; `cleanup` cannot.

Before initial creation on a Ready source, Piccolod evaluates the existing
`connection_auth_mtls_v1` enabled-app rollback guard. It records `allowed`,
`blocked`, or `unknown`; recovery rollback is available only for `allowed`.
This preserves the existing narrow guard without claiming general compatibility
across OS versions.

The update manager owns marker and snapshot decisions under its existing
serialization:

- marker-write failure blocks staged reboot;
- plain reboot with equal active/default roots creates no marker;
- `issued_or_unknown` preserves the marker because reboot effect is ambiguous;
- definite `not_issued` from Ready durably retires authorization before mutable
  Ready state resumes, and the next attempt evaluates the guard again;
- definite `not_issued` while Locked/Failed may retain the marker because app
  mutation remains unavailable and recovery can retry;
- inability to prove required retirement leaves marker/reboot state fail-closed;
- marker replacement and confirmation compare the complete tuple and phase.

If recovery stages a hotfix while a valid marker is pending, it preserves the
original fallback and guard only while the active root remains the recorded
source, candidate, or fallback. From an unrelated root, replacement uses the
current root as source/fallback and records guard `unknown`; old authority is
not carried forward.

Piccolo OS masks the native `transactional-update.timer`; Piccolod's existing
systemd operation is the supported update producer. This RFC adds no timer
fence or second producer lease.

Authenticated force reboot may skip existing candidate validation, but cannot
skip marker protection. Every Piccolod-controlled path capable of consuming a
staged root—including ordinary reboot, force reboot, auto-reboot, and the
network supervisor—uses the protected update-manager path. Physical and
process-down reboots remain outside this guarantee.

`Reboot(context.Context) error` retains its signature and can expose:

- `not_issued`: PID 1 was not asked to reboot;
- `issued_or_unknown`: PID 1 was asked and the effect is ambiguous.

The v1 guarantee begins only when recovery-capable Piccolod performs that
protected call. A hard reboot before marker creation still leaves hotfix,
plain reboot, and diagnostics available, but exact fallback is not promised.
An ordinary caller-selected rollback into pre-v1 code remains the existing
cross-version downgrade and is not presented as protected recovery.

#### Marker Interpretation

| Observed state | Recovery behavior |
|---|---|
| No marker or `cleanup` | No rollback authority |
| Valid `pending`, active candidate, fallback exists, guard `allowed` | Exact rollback available |
| Active/default roots differ | Block normal unlock; offer the appropriate staged-root restart |
| Active fallback and roots match | Rollback unnecessary; unlock may proceed |
| Missing fallback, blocked/unknown guard, or unrelated root | Disable rollback; do not guess |
| Read failure | State unknown; allow bounded retry/diagnostics and update only if producer idle is independently proven |
| Malformed marker | Quarantine before Ready; never authorize rollback |

After the complete post-decrypt chain succeeds, the existing liveness-owned
unlock execution performs one bounded final probe. A matching `pending` marker
is durably changed to non-authorizing `cleanup` before lifecycle Ready; physical
deletion is best-effort. A fatal winner or failed confirmation cannot publish
Ready or later clean the marker from a losing execution.

### 2. Add One Recovery-Only Session

The unlock screen gains `Advanced recovery`. Its endpoint verifies the device
password without advancing unlock:

- normal unlock and verify-only recovery share one password-unwrap primitive
  and process-local admission gate;
- verify-only retains no SDEK, notifies no persistence owner, and changes no
  lifecycle state;
- concurrent unwrap returns typed busy state rather than queueing;
- rejected password, rate-limited response, unavailable crypto state, and
  ambiguous valid-shape ciphertext failure remain distinct and do not blame
  the password when that is unproven;
- user `/auth/login` keeps its independent limiting;
- there is no global pre-unwrap cooldown that lets one caller prevent the next
  correct password from being evaluated.

Successful verification creates an in-memory, origin-bound, non-sliding
15-minute session with audience `recovery`. It uses existing host-only cookie
and CSRF mechanics, contains neither password nor SDEK, is accepted only by
recovery routes while Locked/Failed, and is invalidated on Ready or process
restart. Session insertion and Ready invalidation share one serialization
boundary, so recovery authority cannot be inserted after Ready.

The panel is available wherever the normal unlock screen is available,
including the configured remote domain. Soft storage emergency may admit only
the bounded recovery route group to its own auth and dependency checks;
unrelated APIs remain blocked. Hard emergency retains its existing
diagnostic/retry screen.

Recovery diagnostics reuse the existing redacted journal producer. The current
LAN-only unhealthy diagnostic endpoint remains unchanged.

### 3. Reuse Existing Recovery Capabilities

The route group contains only:

| Method | Route | Behavior |
|---|---|---|
| `POST` | `/api/v1/recovery/session` | Verify password and create recovery session |
| `GET` | `/api/v1/recovery/status` | Read bounded producer/root/marker state |
| `POST` | `/api/v1/recovery/update` | Start the existing latest-update operation |
| `POST` | `/api/v1/recovery/rollback` | Stage the marker's eligible exact fallback |
| `POST` | `/api/v1/recovery/reboot` | Run protected staged or plain reboot |
| `GET` | `/api/v1/recovery/diagnostic-log` | Download existing redacted diagnostics |

The client cannot supply a snapshot ID, force flag, command, power action, app
operation, or user-management request.

Status reports only the current facts needed to select an action:

- producer idle/running and its bounded current result;
- active/default root equality or staged-root presence;
- marker validity and rollback availability;
- staged action `update`, `exact_fallback`, `unclassified`, or `unknown`.

`unclassified` means a root is definitely staged but its provenance is unknown;
`unknown` means safety state could not be read. Historical update information
may be context but never authorizes an action. Existing systemd update work
survives request/session loss; re-authentication resumes status rather than
duplicating work.

One admission owner prevents overlap among:

- normal unlock;
- recovery update, rollback, or reboot;
- locked password reset or equivalent device-credential/keyslot mutation.

First admission wins. Reads remain independent. Producer launch, rollback
staging, marker work, and reboot serialize in the update manager. Unknown state
fails closed for unlock, rollback, and reboot; latest update is permitted only
when existing producer admission independently proves idle.

### 4. Keep Unlock And Recovery UI Honest

Normal unlock returns stable typed outcomes for password rejection,
busy/rate-limited verification, unavailable crypto state, recovery conflict,
unknown update state, and terminal `post_unlock_failed`. UI copy never claims
“wrong password” when the server cannot distinguish password rejection from
valid-shape wrapped-key corruption.

`Advanced recovery` is always secondary on the unlock card. It opens
automatically only when the current client witnesses `post_unlock_failed` or
the current process reports lifecycle Failed:

- witnessed failure may say the password was accepted;
- process-local Failed uses generic startup-failure copy;
- after a fatal process restart, the router may truthfully return only Locked,
  so manual Advanced recovery remains available without a password-acceptance
  claim.

The panel shows one primary action:

| Current state | Primary action |
|---|---|
| Producer running or ownership unknown | Show progress or retry status; disable mutations/unlock |
| Safety state unreadable | Explain unavailable actions; offer update only when producer idle is proven |
| Update root staged | `Restart to finish update` |
| Exact fallback staged | `Restart to return to the pre-update system` |
| Readable but unclassified staged root | `Restart into staged system`, without exact-return promise |
| Roots match after no-change/failed update | Try unlock or retry update according to the observed result |
| Confirmed post-password failure, roots match | `Check for and install latest update` |
| No confirmed post-password failure, roots match | `Try unlocking again` |

When roots match, `Restart Piccolo` is a secondary action. When a root is
staged, only its contextual restart is shown. Eligible exact rollback is a
destructive secondary action with explicit confirmation that later system
changes may be discarded while user data is preserved. Diagnostics are
tertiary.

After reboot invocation, connection loss is expected. When Piccolo is reachable
again, the client re-reads boot and recovery status and renders current state.
Disconnect followed by liveness alone never proves or claims reboot completion;
a staged root transition may provide stronger evidence. Timeout offers
`Check again` and manual refresh without claiming success or failure.

Session loss re-reads `/api/v1/system/boot`; a `401` alone never implies Locked.
If work may still be active, re-authentication resumes status without
relaunching it.

## Invariants

1. Invalid password never grants recovery authority or advances unlock.
2. Recovery authority never grants portal/app access and ends on Ready.
3. Exact rollback uses only a valid pending marker's existing fallback with
   recorded guard `allowed`.
4. A complete unlock durably revokes marker authorization before Ready.
5. Unlock, recovery mutation, and locked credential/keyslot mutation do not
   overlap; launched update work retains its existing owner.
6. Every Piccolod-controlled staged-root reboot is protected; physical,
   process-down, and pre-v1 paths retain their declared limits.
7. Health-checker, ordinary rollback, Snapper cleanup, origin/CSRF, and
   diagnostic-redaction semantics otherwise remain unchanged.

## Temporal Composition

| Event | Durable/result state | Recovery |
|---|---|---|
| Protected staged reboot | Pending marker precedes reboot request | Write failure blocks reboot |
| Ready reboot definitively not issued | Authorization retired before mutable Ready resumes | Next attempt re-evaluates guard |
| Reboot effect ambiguous | Marker remains pending | Poll boundedly; display freshly read state without completion claim |
| Candidate unlock succeeds | Matching marker becomes `cleanup` before Ready | Physical deletion is best-effort |
| Post-password failure or process-local Failed | Pending marker remains | Re-authenticate into recovery |
| Fatal process restart | Failure classification may be lost | Truthful Locked UI retains manual recovery |
| Related-root hotfix | New source/candidate; original fallback/guard | Same protected reboot rules |
| Unrelated-root hotfix | Current source/fallback; guard `unknown` | Old authority is dropped |
| Concurrent mutation | First admission wins | Loser receives typed retryable conflict |
| Request/session loss | Existing update owner continues | Re-authenticate and resume status |

Required composition tests cover:

- marker write, retirement, replacement, confirmation, malformed/read-failure,
  missing-fallback, and effect-ambiguous cases;
- ordinary, force, auto, network-supervisor, physical, and process-down reboot
  boundaries;
- guard allowed/blocked/unknown and related/unrelated hotfixes;
- unlock, password reset, update, rollback, and reboot in competing orders;
- failure response, process-local Failed, fatal restart, and session-loss UI;
- staged, unclassified, unknown, no-change, failed-update, and timeout states;
- recovery audience, CSRF/origin, password-verification, and diagnostic access;
- soft versus hard storage-emergency boundaries;
- pre-v1 rollback and pre-marker hard-reboot limits.

## Site List

Backend:

- `internal/crypt/manager.go`, `internal/auth/manager.go`
  - shared verify-only unwrap and recovery-audience session.
- `internal/update/manager.go`
  - marker, guard evidence, status, exact fallback, confirmation, rollback, and
    protected reboot effect classification.
- `internal/server/gin_server.go`, `internal/server/gin_middleware.go`,
  `internal/server/gin_recovery_handlers.go` (new),
  `internal/server/recovery_execution.go` (new)
  - routes, recovery auth, shared mutation admission, and manager wiring.
- `internal/server/gin_boot_handler.go`,
  `internal/server/gin_crypto_handlers.go`,
  `internal/server/gin_phase2_handlers.go`,
  `internal/server/gin_emergency_handlers.go`,
  `internal/server/gin_auth_handlers.go`
  - honest boot/unlock outcomes, password-reset admission, force protection,
    bounded soft-emergency access, and audience-aware generic auth.
- `internal/server/gin_diagnostic.go`
  - reuse the redacted producer without changing existing LAN diagnostics.
- `internal/network/manager.go`, `internal/network/actuator.go`,
  `internal/network/supervisor.go`
  - route the current direct reboot through protected update-manager reboot.

UI:

- `ui/lib/core/services/api_client.dart`
- `ui/lib/shells/desktop/features/setup/controllers/auth_controller.dart`
- `ui/lib/shells/desktop/features/setup/steps/unlock_step.dart`
- `ui/lib/shells/desktop/features/setup/setup_router.dart`

The UI sites add recovery auth/status/actions, honest unlock errors, Advanced
recovery, and current-state routing.

Tests and contract:

- focused `internal/crypt`, `internal/auth`, `internal/update`,
  `internal/network`, `internal/server`, controller, and widget tests;
- `docs/api/os-updates-integration.md`, `docs/api/openapi.yaml`, and OpenAPI
  validation tests.

## Rollout And Validation

Land backend and recovery UI in one Piccolod release. Validate on MicroOS:

- controlled update records fallback/guard and successful unlock confirms it;
- forced post-password failure reaches authenticated recovery;
- hotfix, plain reboot, exact fallback, diagnostics, and re-auth resume work;
- blocked/unknown guard or missing fallback disables only rollback;
- force, auto-reboot, and network-supervisor staged reboots are protected;
- definite non-issuance retires Ready-created authority; ambiguity retains it;
- password reset cannot overlap recovery mutation;
- recovery session cannot cross portal/app boundaries;
- soft emergency admits bounded recovery while hard emergency remains unchanged;
- hard reboot before marker creation and ordinary pre-v1 rollback demonstrate
  the accepted limits;
- existing health-checker behavior remains unchanged.

## Implementation Status

Proposed on 2026-07-29. No implementation has landed.
