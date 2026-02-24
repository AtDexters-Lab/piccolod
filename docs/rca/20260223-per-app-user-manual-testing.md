# RCA: Per-app user isolation — manual testing observations

- **Status:** Open
- **Date:** 2026-02-23
- **Severity:** Mixed (see individual bugs)
- **Environment:** VirtualBox VM (1GB RAM), piccolod dev branch, per-app user isolation + rootless Podman
- **Related:**
  - `docs/rfc/20260206-rootless-podman-and-cap-drop.md`
  - `docs/rfc/20260220-per-app-user-isolation.md`

## Context

Manual testing of the per-app user isolation feature on a fresh dev-vm. Flow: boot VM, visit piccolo.local, set password, install vaultwarden, install uptime kuma, open both apps.

All 49 automated e2e checks (`dev-vm-test.sh`) pass. These bugs surfaced only during interactive browser-based testing.

---

## Bug 1: ERR_CONNECTION_RESET during password setup

- **Severity:** Medium (self-healing — daemon auto-restarts)
- **Relation to per-app users:** None (pre-existing)

### Symptom

After entering the password on the setup screen, the browser shows `ERR_CONNECTION_RESET`. Refreshing shows the login screen (password was set successfully).

### Root Cause

The daemon was **OOM-killed** during initial disk preparation. Timeline:

| Time | Event |
|---|---|
| 07:29:14 | `GET /api/v1/auth/csrf` — user about to set password |
| 07:29:19 | LUKS initialized (cryptsetup + btrfs mkfs) |
| 07:29:20 | "data volume initialized and mounted" |
| 07:29:20 | **OOM kill** — `piccolod.service: Failed with result 'oom-kill'` |
| 07:29:21 | systemd restarts daemon (restart counter = 1) |

The concurrent disk preparation (cryptsetup luksFormat, mkfs.btrfs, gocryptfs init) plus serving web UI assets exceeded the VM's memory limit. The daemon crash dropped the TCP connection → browser sees ERR_CONNECTION_RESET.

### Remediation

- **Status:** Open
- The VirtualBox dev-VM has 1GB RAM. Crypto setup spawns memory-heavy child processes (cryptsetup, mkfs.btrfs). Options:
  - Increase dev-VM RAM to 2GB
  - Add `MemoryMax=` to the systemd unit to make the limit explicit and tunable
  - Sequence disk operations to avoid concurrent memory spikes

---

## Bug 2: No download progress during app install

- **Severity:** Low (cosmetic — install succeeds)
- **Relation to per-app users:** None (pre-existing UI issue)

### Symptom

When installing vaultwarden, the UI showed no image download progress bar. The install completed successfully after ~28 seconds.

### Root Cause

The SSE progress stream was active for the full 27.9s duration (logs confirm `GET /api/v1/events/progress/stream` returned 200 after 27.9s). Progress events were emitted server-side during `pullToImagestore`. This is likely a UI rendering issue — the Flutter progress listener may not be handling the `pulling_image` phase events correctly.

### Remediation

- **Status:** Open (UI investigation needed)
- Verify SSE events contain progress percentage during image pull
- Check Flutter `InstallProgressController` handles `pulling_image` subtask events

---

## Bug 3: OIDC `interaction_required` when opening app

- **Severity:** High (app access broken until daemon restart)
- **Relation to per-app users:** None (pre-existing OIDC bug, triggered by Bug 1's OOM crash)

### Symptom

Opening vaultwarden (via proxy) redirects to OIDC authorize, which immediately returns `interaction_required` error. Repeated attempts fail the same way.

### Root Cause

The OIDC fast-path logs show `user_id=""`:

```
INFO OIDC authorize: user already authenticated, fast-path user_id="" session_id=mmdpsKhAKZBX6lDPM75Wf
WARN auth request oidc_error.type=interaction_required
```

The session is authenticated (session cookie valid, API calls work), but the OIDC subsystem has **no user linked to the session**. This is because the OOM crash (Bug 1) interrupted the first-boot flow — the OIDC user registration never completed. After the daemon restarted, the session was restored from disk but the OIDC user record was never created.

The OIDC provider correctly rejects the auth request because it can't issue tokens for a non-existent user — returning `interaction_required`.

### Remediation

- **Status:** Open
- The OIDC subsystem should handle missing user records gracefully:
  - Option A: Auto-create the OIDC user record on first authorize if session is valid
  - Option B: Redirect to login page instead of returning a cryptic OIDC error
- Root fix: prevent Bug 1 (OOM crash) so the first-boot flow completes atomically

---

## Bug 4: 502 on Uptime Kuma immediately after install

- **Severity:** Low (transient — app works after startup delay)
- **Relation to per-app users:** None (startup timing)

### Symptom

Opening Uptime Kuma immediately after install shows 502 Bad Gateway.

### Root Cause

The container was created and started successfully (install returned HTTP 201 at 07:32:01). The first 502 occurred at 07:32:05 — **4 seconds after install**. Proxy error: `connection reset by peer` at 127.0.0.1:15001.

Uptime Kuma is a Node.js app that needs several seconds to initialize its SQLite database and start the HTTP server on port 3001. The proxy forwarded the request before the app was ready.

Subsequent reconcile cycles (07:32:22, 07:32:52) show no errors — the containers were running normally.

### Remediation

- **Status:** Acceptable (expected behavior)
- The UI could show a "Starting..." banner while the app boots, using the existing startup status mechanism
- A health check probe before marking the proxy as ready would prevent premature 502s, but adds complexity

---

## Per-App User Specific: Imagestore permission race

- **Severity:** Low (self-healing within seconds)
- **Relation to per-app users:** Direct

### Symptom

First reconcile after app install fails:
```
Error: configure storage: open /piccolo-data/node/podman/imagestore/overlay-images/images.lock: permission denied
```

### Root Cause

The imagestore group permissions (`piccolo-apps`) are applied by `ensureImagestoreGroupAccess()`, which runs inside `podmanImageRuntime()` during image pulls. But the first reconcile for a newly installed app runs BEFORE the image pull completes, and the per-app user (`pa-vaultwarden`) can't read the imagestore lock file yet.

The issue self-heals: `ensureImagestoreGroupAccess` runs during the pull (line 349: "scanned 1 entries, fixed 1 ownership"), and subsequent reconciles succeed.

### Remediation

- **Status:** Fix planned
- Call `ensureImagestoreGroupAccess()` at daemon startup (in `NewAppManagerWithServices`) so imagestore permissions are correct before any per-app operation
- This eliminates the transient error window on first install
