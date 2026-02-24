# RFC: Per-App Linux User Isolation for Rootless Podman

**Date:** 2026-02-20
**Status:** Draft
**Depends on:** RFC 20260206 (Rootless Podman Execution and Capability Hardening)

## 1. Problem Statement

The current rootless Podman implementation (RFC 20260206) runs ALL app containers as a single shared `piccolo-runtime` user. After a container escape (CVE-2024-21626, CVE-2019-5736, CVE-2022-0185), the attacker lands as `piccolo-runtime` on the host and can read/write every other app's Podman storage, runtime state, and data. Since apps are untrusted, per-app user isolation is needed so that an escape from app A (UID X) cannot access app B's data (owned by UID Y).

### Why not `--userns=auto`?

Podman's `--userns=auto` gives each container different UID mappings *inside* the user namespace. But the most common escape vectors (runc/conmon vulnerabilities like CVE-2024-21626) give the attacker control of the container manager process, which runs as `piccolo-runtime` in the parent namespace — NOT as the mapped subuid. So the attacker lands as `piccolo-runtime` regardless of `--userns=auto`. Per-app Linux users ensure even the runc/conmon process runs under a different UID per app, so escape from app A yields UID X (not Y).

## 2. Goals

1. Run each app's Podman lifecycle (create/start/stop/rm/exec/logs) under a unique per-app Linux user (`pa-{instanceID}`).
2. Ensure container escape from app A yields UID X, which cannot access app B's storage (owned by UID Y).
3. Share base images via POSIX group permissions (`piccolo-apps` group) without duplicating pulls.
4. Be fully self-healing: no persistent metadata needed, users re-provisioned from deterministic naming.
5. Handle orphan cleanup at startup for users whose apps no longer exist.

## 3. Non-Goals

- `--userns=auto` — useful defense-in-depth but not a replacement; may be layered on later.
- Per-app network namespaces — all apps share the host network via pasta.
- Encrypted per-app imagestore — base images stay unencrypted in shared store.

## 4. Design

### 4.1 Username Scheme

Per-app usernames: `pa-{instanceID}` (prefix "piccolo app"). If the resulting username exceeds 32 characters (Linux limit), truncate and append a 4-character hash to avoid collisions.

### 4.2 Dynamic User Provisioning

Users created at app install, destroyed at app uninstall. `ProvisionAppUser` is fully idempotent — always checks if user exists first. MicroOS's /etc is writable (overlay).

**At install** (all steps idempotent):

1. Check if user exists (`user.Lookup`) — if yes, verify subuid range + group, return.
2. `groupadd -f piccolo-apps`
3. `useradd --system --shell /usr/sbin/nologin --create-home --groups piccolo-apps pa-{instanceID}`
4. Allocate subuid/subgid range via `usermod --add-subuids START-END --add-subgids START-END`.
5. `loginctl enable-linger pa-{instanceID}`
6. On failure after useradd, rollback: `userdel --remove pa-{instanceID}`.

**At uninstall:**

1. `loginctl disable-linger pa-{instanceID}`
2. `userdel --remove pa-{instanceID}`

### 4.3 subuid/subgid Allocation

Each per-app user needs a 65536-entry non-overlapping range. Base offset: 200000 (above `piccolo-runtime`'s 100000:65536). Strategy: scan `/etc/subuid` for all existing entries, find first non-overlapping slot.

### 4.4 Shared Imagestore Access

POSIX group `piccolo-apps` with group-read permissions. All per-app users and `piccolo-runtime` are members of the group. Imagestore owned by `piccolo-runtime:piccolo-apps` with `0750` dirs / `0640` files. Setgid bit on imagestore dir.

### 4.5 Self-Healing Design

Usernames derived deterministically from instanceID — no persistent metadata needed. If /etc is wiped (transactional-update rollback), users re-provisioned on next lifecycle operation.

### 4.6 Orphan Cleanup

At startup, scan `/etc/passwd` for `pa-*` users not matching any known app. Clean up via `DestroyAppUser`.

### 4.7 Migration

Existing apps get per-app users on next lifecycle operation. `PodmanRoot` and `RunRoot` re-chowned from `piccolo-runtime` to the new per-app user.

### 4.8 What Stays Shared

- **piccolod daemon:** root
- **gocryptfs/fuse-overlayfs mounts:** root
- **podman image mount/unmount:** piccolo-runtime
- **podman pull/inspect/search:** piccolo-runtime

### 4.9 What Changes

- **podman create/start/stop/rm:** `pa-{instanceID}`
- **podman exec (terminal):** `pa-{instanceID}`
- **podman logs:** `pa-{instanceID}`

## 5. Security Analysis

| Property | Before (single user) | After (per-app user) |
|---|---|---|
| Container escape from app A | Access ALL app data (`piccolo-runtime`) | Access ONLY app A data (`pa-A`) |
| runc/conmon exploit | Lands as `piccolo-runtime` | Lands as per-app UID |
| Shared imagestore | Writable by `piccolo-runtime` | Read-only group access for per-app users |
| Cross-app data access | Unrestricted (same UID) | Blocked (different UIDs, `0700` dirs) |

## 6. Implementation

See implementation in:

- `internal/container/appuser.go` — user provisioning/cleanup
- `internal/app/podman_runtime.go` — per-app user resolution in runtime
- `internal/app/app_manager.go` — lifecycle wiring and orphan cleanup

## 7. Testing

1. Unit tests for username derivation (deterministic, truncation).
2. Unit tests for subuid allocation (gap-filling, no overlaps).
3. Unit tests for idempotent provisioning.
4. Integration: verify different UIDs per app in `ps aux`.
5. Integration: verify cross-app isolation (app A cannot read app B's podman root).

## 8. Future Work

- Layer `--userns=auto` on top for defense-in-depth.
- Per-app seccomp profiles.
- Per-app network namespaces.
