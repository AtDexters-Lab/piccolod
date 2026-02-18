# RCA: USB "Try Piccolo" setup failure — Phase 1 never started, non-resumable half-init

- **Status:** Open
- **Date:** 2026-02-18
- **Severity:** Critical (setup flow bricked without reboot)
- **Environment:** HP Laptop 15q-bu1xx, 8GB USB stick (slow), piccolod v0.1.23
- **Related:**
  - `docs/rfc/20260201-storage-posture.md` (LUKS lifecycle)
  - `docs/rfc/20260203-install-to-disk.md` (install pipeline)
  - `docs/rca/20260212-gocryptfs-password-mismatch-on-reboot.md` (prior LUKS ordering fix)

## 1. Summary

During a USB "Try Piccolo" boot on an HP laptop with an 8GB USB stick, the setup flow failed and left the system in an unrecoverable state (short of rebooting). Two distinct failures occurred:

1. **Install-to-Disk failed** due to insufficient staging space (8GB USB, 1306 MB needed). The onboarding state machine transitioned to `install_disk` and stayed there.
2. **Crypto setup blocked indefinitely** on `WaitForPhase1()` because Phase 1 (disk partitioning) was never started — the UI bypassed the "Try Piccolo" onboarding choice after the install failure, so `StartPartitioningAsync()` was never called. The user refreshed the page after ~2 minutes, which canceled the HTTP request context. The handler continued server-side but auth/user creation failed on the canceled context, leaving the system half-initialized with no recovery path except reboot.

## 2. Timeline

All timestamps from `artifacts/logs/hp-usb-try`. Server timestamps are UTC-5:30 offset from journal.

| Time | Event |
|---|---|
| 22:43:08 | piccolod starts. Boot disk is 8GB — below 18GB minimum. |
| 22:43:10 | `boot mode is usb, onboarding state is pending; deferring disk preparation to onboarding` — **Phase 1 not started.** |
| 22:44:17 | User opens UI, pages load. `GET /api/v1/system/onboarding` → onboarding wizard displayed. |
| 22:44:22 | `GET /api/v1/storage/disks` — user on disk selection page. |
| 22:44:28 | User picks "Install to Disk" → `POST /api/v1/system/install-to-disk` → 202. **State transitions: `pending → install_disk`.** |
| 22:44:30 | `ERROR: install to disk failed: download failed: insufficient disk space: need 1306 MB, neither /tmp nor /piccolo-core has enough room` |
| 22:45:11 | User refreshes page. UI calls `_checkStatus()`. |
| 22:45:12 | `GET /api/v1/system/onboarding` → `{state: "install_disk", required: false, install_done: false}`. UI falls through to crypto check → `initialized: false` → shows **welcome/credentials screen** (not onboarding choice). |
| ~22:45:26 | User sets password → `POST /api/v1/crypto/setup` begins. Crypto keys created, crypto unlocked. Handler blocks on `WaitForPhase1(ctx)`. |
| 22:47:19 | **User refreshes page** (`GET "/"` at exact same timestamp). Browser aborts in-flight POST. Go cancels `r.Context()`. `WaitForPhase1` returns `context.Canceled`. |
| 22:47:27 | Handler continues server-side: persistence unlock succeeds, but `authManager.Setup` logs `WARN: admin user migration failed: count users: context canceled`. `userManager.Create` fails: `ERROR: admin user creation failed: context canceled`. Handler returns early before session creation. `POST /api/v1/crypto/setup` → **500** (2m1s). |
| 22:47:19 | Refreshed page: UI sees crypto initialized + unlocked, session not authenticated → shows **login screen**. |
| ~22:47:24 | User enters credentials → `POST /api/v1/auth/login` begins. Persistence still locked (crypto/setup's unlock hasn't fired yet). `userManager.Verify` → `ErrLocked` → enters crypto unlock path → `UnlockDataVolume` → **blocks on `WaitForPhase1(ctx)`**. |
| 22:50:31 | **User refreshes page again.** Browser aborts login POST. `ERROR: auth login data volume unlock failed: wait for phase 1: context canceled`. `POST /api/v1/auth/login` → **401** (3m7s). |
| 22:50:34 | User retries login → immediate **401** (persistence now unlocked, but admin user doesn't exist). |
| 22:50:44 | User retries login again → immediate **401** (same reason). |
| 22:53:24 | Service terminated (SIGTERM). User gave up. |

## 3. Root Cause

### 3.1 Phase 1 never started — missing onboarding state transition

`StartPartitioningAsync()` is called from three places:

| Call site | Condition | Triggered? |
|---|---|---|
| `gin_server.go:797` | `BootModeInternal` | No — USB boot |
| `gin_server.go:801` | `state == try_piccolo \|\| state == complete` | No — state was `pending` at startup |
| `gin_onboarding_handlers.go:69` | User chooses `try_piccolo` via `POST /api/v1/system/onboarding` | **No — never called** |

Phase 1 was deferred to onboarding at startup (`gin_server.go:814`). The onboarding choice handler (`handleOnboardingChoice`) is the only runtime path that calls `StartPartitioningAsync()`, and it was never invoked because the UI bypassed the onboarding screen (see §3.2).

With Phase 1 never started, `phase1Done` channel (`storage/manager.go:58`) was never closed. `WaitForPhase1()` (`storage/manager.go:108-116`) blocks indefinitely:

```go
select {
case <-m.phase1Done:   // never closed
    return m.phase1Err
case <-ctx.Done():     // only exit — when HTTP context is canceled
    return ctx.Err()
}
```

### 3.2 UI bypasses onboarding choice after failed install

After the install-to-disk failure, the onboarding state was `install_disk`. When the user refreshed the page, the UI's `_checkStatus()` (`setup_controller.dart:135-194`) evaluated:

1. `onboarding['required']` → **false** (`isRequiredLocked()` returns `state == StatePending`, which is false for `install_disk`) — **skips onboarding screen**
2. `state == 'install_disk' && install_done == true` → false
3. `state == 'install_disk'` with active task → false (installer finished, `ActiveTaskID()` returns empty)
4. Falls through to `crypto/status` → `initialized: false` → **`_state = SetupState.welcome`**

The user was shown the welcome → credentials screen with no opportunity to choose "Try Piccolo". The `chooseTryPiccolo()` method (`setup_controller.dart:244`) and its `POST /api/v1/system/onboarding` with `choice: try_piccolo` were never invoked.

### 3.3 Onboarding state machine has no `install_disk → try_piccolo` transition

Even if the UI had offered the choice, the backend state machine would reject it. `validateTransition()` (`onboarding/state.go:237-251`) allows:

| From | To | Purpose |
|---|---|---|
| `pending` | `try_piccolo` | Normal USB flow |
| `pending` | `install_disk` | User chooses install |
| `try_piccolo` | `complete` | Setup done |
| `try_piccolo` | `install_disk` | Settings: change mind |
| `complete` | `install_disk` | Settings: re-install |

**`install_disk → try_piccolo` is not valid.** The only recovery is boot-time reset: `install_disk` + `InstallDone == false` → `pending` (`onboarding/state.go:74-79`). This requires a reboot.

### 3.4 HTTP request context used for long-running operations

`handleCryptoSetup` (`gin_crypto_handlers.go:68`) uses `ctx := c.Request.Context()` for `InitializeDataVolume`, which blocks on `WaitForPhase1`. The HTTP server on port 80 has **no configured timeouts** (`gin_server.go:842-845` — no `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`). The context is solely controlled by client connection lifetime.

When the user refreshed the page at 22:47:19, the browser aborted the pending `fetch()` for the POST. Go's `net/http` detected the TCP close and canceled `r.Context()`. Confirmed by exact timestamp correlation:

- `22:47:19` — `GET "/"` (page load) **and** `ERROR: data volume initialization failed: wait for phase 1: context canceled`
- `22:50:31` — `GET "/"` (page load) **and** `ERROR: auth login data volume unlock failed: wait for phase 1: context canceled`

The handler goroutine continued executing server-side (Go does not kill goroutines on context cancellation). Persistence unlock succeeded, but `authManager.Setup` and `userManager.Create` failed because SQLite operations respect context cancellation.

### 3.5 Setup is not idempotent — half-init leaves unrecoverable state

After the first `crypto/setup` failure, the system was:

| Component | Status | Recoverable? |
|---|---|---|
| Crypto keys | Created (`Setup` + `Unlock` succeeded at steps 2-3) | `Setup()` will reject — keys exist |
| Data volume (LUKS) | Not initialized (Phase 1 never ran) | No runtime path to start Phase 1 |
| Persistence (gocryptfs) | Unlocked (notify fired at 22:47:27) | OK |
| Auth manager | Partially set up (migration failed) | Possibly re-entrant |
| Admin user | **Not created** (context canceled at step 7) | `Create` might fail on duplicate if partially written |
| Session | **Not created** (handler returned early at step 7, before session creation at step 9) | N/A |
| Onboarding state | `install_disk` (`Complete()` only accepts `try_piccolo`/`pending`) | Stuck |

The user had no viable path forward:
- **Can't re-run crypto/setup**: `cryptoManager.Setup()` rejects — keys already exist.
- **Can't login**: admin user doesn't exist. After persistence re-locks, login enters crypto unlock → `UnlockDataVolume` → `WaitForPhase1` → blocks indefinitely again.
- **Can't choose "Try Piccolo"**: state machine rejects `install_disk → try_piccolo`.
- **Only recovery**: reboot (boot recovery resets `install_disk` + `!InstallDone` → `pending`).

## 4. Impact

- **Blast radius:** Any USB "Try Piccolo" boot where the user first attempts "Install to Disk" and it fails. The 8GB USB stick is the trigger, but the state machine bug affects any failed-install-then-try-piccolo path.
- **User experience:** System appears to hang for minutes during setup. After refresh, login fails silently. No error message explains the situation or suggests a remedy. The only recovery requires hardware knowledge (reboot the device).
- **Scope:** Affects `handleCryptoSetup` (blocks on Phase 1), `handleCryptoUnlock` (same), and `handleAuthLogin` (same via `UnlockDataVolume`). All three are gated by `WaitForPhase1` with no guard against Phase 1 not being started.

## 5. Contributing Factors

1. **Onboarding state machine assumes linear flow.** The `install_disk` state has no backward transition to `try_piccolo` or `pending`. A failed install is a dead end at runtime.
2. **UI `_checkStatus()` falls through silently.** When state is `install_disk` with no active task and `install_done: false`, the UI skips the onboarding screen entirely instead of returning the user to the choice screen.
3. **`WaitForPhase1` has no "not started" guard.** It blocks on a channel that may never close, with the caller's context as the only escape hatch.
4. **`handleCryptoSetup` uses HTTP request context for a potentially minutes-long operation.** Browser navigation or timeout cancels the context and cascades failure through all downstream steps.
5. **No idempotency in the setup pipeline.** `cryptoManager.Setup()` is one-shot. There is no "resume from where we left off" path after a partial failure.
6. **Install-to-disk has no upfront disk space validation.** The handler returns 202 before checking staging space, wasting user time and triggering the state transition to `install_disk` before discovering the failure.

## 6. Remediation

### 6.1 Add `install_disk → pending` (or `→ try_piccolo`) state transition

Allow the user to back out of a failed install without rebooting. The simplest option is `install_disk → pending` (when `InstallDone == false`), which mirrors the boot recovery logic but at runtime.

### 6.2 UI: return to onboarding choice after failed install

When `_checkStatus()` sees `state == 'install_disk'`, `install_done: false`, and no active install task, the UI should show the onboarding choice screen (or an error screen with a "back" option), not fall through to the credentials screen.

### 6.3 Guard `WaitForPhase1` against "not started"

Either:
- (a) Return an immediate error from `WaitForPhase1` if Phase 1 was never started (e.g., track a `phase1Started` flag), or
- (b) Have `handleCryptoSetup` start Phase 1 itself if it hasn't been started yet (lazy start).

Option (b) is more resilient — it makes crypto/setup self-sufficient rather than depending on a prior onboarding step.

### 6.4 Decouple `handleCryptoSetup` from HTTP request context

Use `context.Background()` (with a generous timeout) for `InitializeDataVolume`, similar to how `handleInstallToDisk` already uses `context.Background()` for the installer (`gin_onboarding_handlers.go:170`). A browser refresh should not abort the setup pipeline.

### 6.5 Make setup idempotent / resumable

Allow `handleCryptoSetup` to detect and resume from a partial state:
- If crypto keys already exist, skip `Setup()` and proceed to `Unlock()`.
- If admin user already exists, skip creation.
- If onboarding is not in `try_piccolo`/`pending`, handle gracefully.

### 6.6 Upfront disk space validation for install-to-disk

Check staging space (HEAD request for image size + `statfs`) before returning 202 and transitioning the state machine. Reject the request early with a clear error if space is insufficient.

## Remediation Status

- [ ] State machine: add `install_disk → pending` transition (when `!InstallDone`)
- [ ] UI: return to onboarding choice on failed install with no active task
- [ ] `WaitForPhase1`: guard against "not started" / lazy-start Phase 1 from crypto/setup
- [ ] `handleCryptoSetup`: decouple from HTTP request context
- [ ] Setup idempotency: detect and resume from partial state
- [ ] Install-to-disk: upfront disk space validation
