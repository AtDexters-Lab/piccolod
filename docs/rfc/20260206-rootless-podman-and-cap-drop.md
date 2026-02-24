# RFC: Rootless Podman Execution and Capability Hardening

**Date:** 2026-02-06
**Status:** Draft

## 1. Problem Statement

piccolod runs as root (required for port 80, gocryptfs mounts, mDNS). All `podman` commands are executed via `exec.CommandContext(ctx, "podman", args...)` without any user switching, meaning containers run under **rootful Podman**. A container escape exploit would yield root on the host.

The PRD states _"rootless containers where possible; drop capabilities"_ (`docs/pre-beta-prd.md:27`), but neither is implemented today:
- No user namespace remapping — container UID 0 = host UID 0.
- No capability dropping — Podman rootful defaults (~14 capabilities) are granted unconstrained.
- No `--security-opt` flags are applied.

## 2. Goals

1. Run all Podman operations as a dedicated unprivileged system user, achieving **genuine rootless Podman** where container "root" maps to an unprivileged UID on the host.
2. Drop all Linux capabilities except a vetted allow-list that home-server apps need.
3. Enforce resource limits (`--memory`, `--cpus`, `--pids-limit`, `--ulimit nofile`) from the app manifest, with sensible defaults when unspecified.
4. Reject `privileged: true` in app manifests — make the security stance explicit.
5. Zero app breakage — changes must be transparent to all existing store apps.

## 3. Non-Goals

- `--security-opt no-new-privileges` — blocks the `PUID`/`PGID` setuid-based user switching pattern used by LinuxServer images. Requires per-app opt-in and is deferred.
- `--read-only` root filesystem — requires per-app tmpfs configuration. Deferred.
- Custom per-app seccomp profiles — Podman's default profile is adequate for now.
- VM-level isolation (Firecracker/gVisor) — different architecture, out of scope.

## 4. Current State

### 4.1 Podman execution (`internal/container/podman.go`)

All 20+ `PodmanCLI` methods call `exec.CommandContext(ctx, "podman", args...)` or `exec.Command("podman", args...)` with no `SysProcAttr` credential switching. Commands inherit piccolod's root UID.

Two methods independently set `cmd.Env` after command construction:
- `ExecShellCmd` (line 1477): `cmd.Env = os.Environ()`
- `PullImageWithProgress` (line 1766): `cmd.Env = append(os.Environ(), "LC_ALL=C")`

### 4.2 Storage layout (`internal/app/podman_runtime.go`)

Per-app isolation uses `--root`, `--runroot`, `--imagestore` flags pointing to directories under the encrypted gocryptfs volume (per-app) and a shared imagestore. All directories are created with `0700` permissions owned by root.

### 4.3 FUSE mounts (`internal/persistence/file_volume_manager.go`)

gocryptfs mounts already use `-allow_other` (line 452), and the codebase expects `user_allow_other` in `/etc/fuse.conf` (tested in `state_dir_audit_test.go:39-52`). FUSE mount points are already accessible to non-root users.

### 4.4 `buildCreateArgs()` (`internal/container/podman.go:341-447`)

Constructs `podman create` arguments. Currently passes `--memory` and `--cpus` from `ResourceLimits`, but not `--pids-limit` or `--ulimit`. No capability or security flags are included.

### 4.5 Container create spec (`internal/container/podman.go:268-306`)

`ContainerCreateSpec` has a `ResourceLimits` struct with `Memory` and `CPU` fields. No fields for capabilities, security options, or pids-limit.

### 4.6 Manifest permissions (`internal/api/types.go:315-340`)

`AppResourcePermissions` defines `MaxProcesses` and `MaxOpenFiles` that are validated by the parser but never passed through to container creation. A `Privileged bool` field exists but is silently ignored.

### 4.7 Networking (`internal/app/netavark_repair.go`)

`flushAndReloadNetavarkRules()` runs at startup to clean stale nftables DNAT rules left by netavark (rootful Podman's network backend). It calls `nft delete table` and `podman network reload`. This is rootful-specific — rootless Podman uses `pasta`/`slirp4netns` instead of netavark.

### 4.8 Workspace fuse-overlayfs (`internal/app/workspacedisk/mount.go:156`)

Workspace overlay mounts are run by piccolod as root via `exec.CommandContext(ctx, fuseOverlayfs, ...)`. These are FUSE mounts, not Podman commands — they remain under root execution.

### 4.9 Workspace image mounter (`internal/app/workspacedisk/manager.go`)

`PodmanImageMounter` contains three methods that invoke `podman` directly via `exec.CommandContext` — bypassing `PodmanCLI`:
- `MountImage` (line 396): `podman image mount`
- `UnmountImage` (line 436): `podman image unmount`
- `getImageMountPath` (line 466): `podman image mount` (query)

These use the shared image runtime with overlay storage driver. `podman image mount` with overlay **does not work in rootless mode** without `podman unshare`. See section 5.2 for how this is handled.

## 5. Design

### 5.1 Dedicated runtime user

Create a system user `piccolo-runtime` (no login shell) during OS image build:

```bash
useradd --system --shell /usr/sbin/nologin --create-home piccolo-runtime
# Allocate subordinate UID/GID ranges for rootless user namespaces
echo "piccolo-runtime:100000:65536" >> /etc/subuid
echo "piccolo-runtime:100000:65536" >> /etc/subgid
# Enable linger for cgroup delegation (required for resource limits)
loginctl enable-linger piccolo-runtime
```

### 5.2 Process boundary: what stays root vs. switches

Only Podman CLI commands switch to `piccolo-runtime`. Everything else remains root:

| Process | Runs as | Rationale |
|---------|---------|-----------|
| piccolod (HTTP server, proxy, mDNS) | root | Needs port 80, system management |
| gocryptfs mounts | root | FUSE mount operation (already uses `-allow_other`) |
| fuse-overlayfs (workspace) | root | FUSE mount operation (needs `-allow_other`, see 5.5) |
| `podman image mount/unmount` (workspace) | root | Overlay driver requires root for image mount (see 4.9); mounted paths accessible to `piccolo-runtime` via `-allow_other` on underlying FUSE mounts |
| `podman create/start/stop/rm/pull/...` | piccolo-runtime | Container lifecycle — the security boundary |
| `podman exec` (terminal sessions) | piccolo-runtime | Runs inside container namespace |
| `nft delete` (netavark repair) | root | nftables management (conditional, see 5.12) |

### 5.3 Credential resolution at initialization

Resolve the `piccolo-runtime` user **once** at `AppManager` initialization and store the credential for reuse. This avoids repeated `user.Lookup` calls and ensures consistency:

```go
// In AppManager initialization:
func resolveRuntimeCredential() *syscall.Credential {
    u, err := user.Lookup("piccolo-runtime")
    if err != nil {
        log.Printf("INFO: piccolo-runtime user not found, using rootful Podman fallback")
        return nil
    }
    uid, _ := strconv.ParseUint(u.Uid, 10, 32)
    gid, _ := strconv.ParseUint(u.Gid, 10, 32)
    log.Printf("INFO: Podman commands will execute as UID %d (piccolo-runtime)", uid)
    return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
}
```

The resolved `*syscall.Credential` is stored on `AppManager` and passed into every `PodmanRuntime` instance.

### 5.4 Credential switching in PodmanCLI

Add a `Credential` field to `PodmanRuntime`:

```go
type PodmanRuntime struct {
    Root          string
    RunRoot       string
    Imagestore    string
    StorageDriver string
    StorageOpts   []string
    Credential    *syscall.Credential // non-nil = run as this user
}
```

A single helper applies the credential and constructs a **minimal, explicit** environment. This function must be the **final** modifier of `cmd.Env` and `cmd.SysProcAttr` — no method may overwrite these fields after calling it:

```go
func applyRuntimeCredential(cmd *exec.Cmd, rt PodmanRuntime, extraEnv ...string) {
    if rt.Credential == nil {
        return
    }
    uid := rt.Credential.Uid

    // Minimal explicit environment — do NOT propagate os.Environ().
    // Leaking the root process environment undermines security isolation.
    env := []string{
        "HOME=/home/piccolo-runtime",
        fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
        fmt.Sprintf("PATH=%s", os.Getenv("PATH")),
        "LANG=C.UTF-8",
        "LC_ALL=C",
    }
    env = append(env, extraEnv...)
    cmd.Env = env

    if cmd.SysProcAttr == nil {
        cmd.SysProcAttr = &syscall.SysProcAttr{}
    }
    cmd.SysProcAttr.Credential = rt.Credential
}
```

Key design decisions:
- **Minimal env, not `os.Environ()`** — prevents leaking root-process variables (`PICCOLO_CORE_ROOT`, `DBUS_SESSION_BUS_ADDRESS`, etc.) into rootless subprocesses. `CONTAINERS_CONF`, `CONTAINERS_REGISTRIES_CONF`, and `CONTAINERS_STORAGE_CONF` are intentionally omitted — Podman's defaults for the rootless user (`$HOME/.config/containers/`) are correct.
- **`extraEnv` parameter** — allows methods to inject additional variables (e.g., `TERM=xterm-256color` for terminal sessions) without overwriting the credential-aware environment.
- **Merges into existing `SysProcAttr`** — preserves fields like `Setsid` set by PTY code. Sets `Credential` without replacing the struct.
- **Must be called last** — enforced by convention; no method may set `cmd.Env` or `cmd.SysProcAttr.Credential` after this call.

#### Affected PodmanCLI methods

There are two categories of changes across all ~21 `exec.Command("podman", ...)` call sites in `PodmanCLI`:

**Methods requiring refactoring** (currently set `cmd.Env`, must be changed to use `applyRuntimeCredential`):

- `ExecShellCmd` (line 1477): currently sets `cmd.Env = os.Environ()`. Replace with `applyRuntimeCredential(cmd, runtime, "TERM=xterm-256color")`. The `TERM` variable is needed for proper terminal handling in the podman process.
- `PullImageWithProgress` (line 1766): currently sets `cmd.Env = append(os.Environ(), "LC_ALL=C")`. Replace with `applyRuntimeCredential(cmd, runtime)` — `LC_ALL=C` is already in the base env.

For `PullImageWithProgress`, there is an additional concern: `pty.StartWithSize()` (from `creack/pty`) may set `SysProcAttr.Setsid = true`. The helper merges `Credential` into an existing `SysProcAttr` rather than replacing it, so the ordering is: (1) create `cmd`, (2) call `applyRuntimeCredential` which sets `Credential` on `SysProcAttr`, (3) `pty.StartWithSize` may set `Setsid` on the same struct — both fields are preserved. If `pty` replaces `SysProcAttr` entirely (version-dependent), the implementation must pre-set `Setsid: true` before calling `applyRuntimeCredential`, or merge after PTY setup. This must be verified against the pinned `creack/pty` version during implementation.

**Methods requiring addition** (no existing env handling, add `applyRuntimeCredential` call after `exec.CommandContext`):

All other `PodmanCLI` methods: `CreateContainer`, `StartContainer`, `StopContainer`, `RemoveContainer`, `ImageExists`, `RemoveImage`, `PullImage`, `Logs`, `LogsStream`, `containerExists`, `ResolveContainerIDByName`, `ListContainersByLabel`, `InspectContainerState`, `InspectPublishedPorts`, `NetworkReload`, `ResetStorage`, `ValidateAndRepairStorage`, `InspectImage`, `SearchRegistry`.

#### Methods that stay rootful

`PodmanImageMounter` in `internal/app/workspacedisk/manager.go` (`MountImage`, `UnmountImage`, `getImageMountPath`) — these call `podman image mount/unmount` which requires root with overlay driver. They do **not** get credential switching. The mounted filesystem is accessible to `piccolo-runtime` via `-allow_other` on the underlying FUSE mounts (gocryptfs and fuse-overlayfs). However, these methods should still use the minimal environment (not `os.Environ()`) for consistency. If `PodmanImageMounter` currently propagates `os.Environ()`, strip it to minimal env without credential switching.

### 5.5 Workspace fuse-overlayfs accessibility

Workspace overlay mounts (`internal/app/workspacedisk/mount.go:156`) are run by piccolod as root. The mounted filesystem must be accessible by `piccolo-runtime` (which runs the Podman container on top of it).

Add `-allow_other` to the fuse-overlayfs mount options:

```go
opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,allow_other",
    lowerDir, layout.Upper, layout.Work)
```

This is consistent with how gocryptfs already mounts (line 452 of `file_volume_manager.go`). The `user_allow_other` prerequisite in `/etc/fuse.conf` is already satisfied.

### 5.6 XDG_RUNTIME_DIR lifecycle

`/run` is tmpfs and cleared on every boot. piccolod must create the runtime directory at startup (not at OS image build time):

```go
// At AppManager initialization, after resolving piccolo-runtime UID:
xdgDir := fmt.Sprintf("/run/user/%d", runtimeUID)
os.MkdirAll(xdgDir, 0700)
os.Chown(xdgDir, int(runtimeUID), int(runtimeGID))
```

To prevent systemd `logind` from garbage-collecting the directory (since `piccolo-runtime` has no active login session), `loginctl enable-linger piccolo-runtime` is configured at OS image build time (see 5.1). This keeps the user's runtime directory alive without an active session.

### 5.7 Directory ownership (recursive)

piccolod (root) creates all Podman storage directories, then recursively chowns them to `piccolo-runtime`. This uses `filepath.WalkDir` for pure-Go recursive ownership transfer:

```go
func chownRecursive(root string, uid, gid int) error {
    return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
        if err != nil {
            return err
        }
        return os.Lchown(path, uid, gid)
    })
}
```

This runs in `podmanRuntimeForApp()` and `podmanImageRuntime()` after `ensureDir()`:

Affected directories:
- Per-app `--root` (inside encrypted volume)
- Per-app `--runroot` (under `$PICCOLO_CORE_ROOT/run/podman/`)
- Shared `--imagestore` (under `$PICCOLO_CORE_ROOT/podman/imagestore`)
- Image root (under `$PICCOLO_CORE_ROOT/podman/image-root`)

**Performance**: The recursive chown must be guarded by an ownership check to avoid traversing large directory trees on every startup. The implementation must stat the root directory first and skip if already owned by `piccolo-runtime`:

```go
func chownIfNeeded(root string, uid, gid int) error {
    info, err := os.Stat(root)
    if err != nil {
        return err
    }
    stat := info.Sys().(*syscall.Stat_t)
    if stat.Uid == uint32(uid) && stat.Gid == uint32(gid) {
        return nil // Already owned by target user
    }
    return chownRecursive(root, uid, gid)
}
```

For the initial migration from rootful, the shared imagestore may contain gigabytes of image layers — the recursive chown is a one-time cost on upgrade. For fresh installations, directories are empty when first created. On subsequent startups, the stat check makes this a constant-time no-op.

### 5.8 Capability dropping

Use the allowlist approach (`--cap-drop=ALL` + explicit `--cap-add`) for defense in depth. Any new capability added to Podman's default set in future versions is automatically dropped. These flags are added in `buildCreateArgs()` and apply unconditionally to **all** containers, including network anchor (pause) containers. The pause image is minimal and does not require any capabilities beyond the allowlist — this is verified by the dedicated network anchor test (section 8, item 5):

```go
// Unconditional security hardening — allowlist approach
args = append(args,
    "--cap-drop=ALL",
    "--cap-add=CHOWN",
    "--cap-add=DAC_OVERRIDE",
    "--cap-add=FOWNER",
    "--cap-add=FSETID",
    "--cap-add=SETUID",
    "--cap-add=SETGID",
    "--cap-add=NET_BIND_SERVICE",
    "--cap-add=KILL",
    "--cap-add=SYS_CHROOT",
    "--cap-add=SETFCAP",
    "--cap-add=SETPCAP",
    "--cap-add=AUDIT_WRITE",
)
```

Justification for each allowed capability:

| Capability | Why allowed |
|---|---|
| `CHOWN` | File ownership management, needed by most images during init |
| `DAC_OVERRIDE` | Bypass file permission checks, needed for container init as root |
| `FOWNER` | Bypass permission checks on file owner, needed for package managers |
| `FSETID` | Preserve setuid/setgid bits on file modification |
| `SETUID`, `SETGID` | User switching inside container (LinuxServer `PUID`/`PGID` pattern) |
| `NET_BIND_SERVICE` | Bind ports < 1024 inside container |
| `KILL` | Signal management inside container |
| `SYS_CHROOT` | Some init systems need this |
| `SETFCAP`, `SETPCAP` | Capability management (needed for some package managers) |
| `AUDIT_WRITE` | Write to audit log (standard default, low risk) |

Notable capabilities **excluded** (dropped via `--cap-drop=ALL`):

| Capability | Risk |
|---|---|
| `SYS_ADMIN` | Near-root: mount, BPF, namespace ops |
| `SYS_MODULE` | Load kernel modules |
| `SYS_RAWIO` | Raw device I/O |
| `SYS_PTRACE` | Trace/debug arbitrary processes |
| `SYS_BOOT` | Reboot the host |
| `NET_RAW` | Raw sockets, packet spoofing (`ping` will not work inside containers — expected and acceptable for a home server) |
| `MKNOD` | Create device nodes |
| `DAC_READ_SEARCH` | Bypass file read permissions |
| `SYS_TIME` | Modify system clock |
| `SYS_RESOURCE` | Override resource limits |

### 5.9 Reject `privileged: true` in manifest

The `Privileged` field in `AppResourcePermissions` (`internal/api/types.go:334`) is currently parsed and silently ignored. Change the parser to **reject** manifests that set `privileged: true`:

```go
// In validateResourcePermissions() (internal/app/parser.go):
if res.Privileged {
    return fmt.Errorf("privileged containers are not supported")
}
```

This makes the security stance explicit — app authors cannot assume the field works, and future accidental enabling is prevented.

### 5.10 Resource limit enforcement

Extend `ResourceLimits` and wire manifest permissions through:

```go
type ResourceLimits struct {
    Memory      string
    CPU         string
    PidsLimit   int // from manifest max_processes, or default
    NofileLimit int // from manifest max_open_files, or default
}
```

In `buildCreateArgs()`:

```go
if spec.Resources.PidsLimit > 0 {
    args = append(args, "--pids-limit", strconv.Itoa(spec.Resources.PidsLimit))
}
if spec.Resources.NofileLimit > 0 {
    args = append(args, "--ulimit",
        fmt.Sprintf("nofile=%d:%d", spec.Resources.NofileLimit, spec.Resources.NofileLimit))
}
```

**Default limits** when the manifest omits them — prevents a single compromised container from DoS'ing the host:

| Limit | Default | Rationale |
|-------|---------|-----------|
| `--pids-limit` | 4096 | Prevents fork bombs; generous for any legitimate app |
| `--ulimit nofile` | 65536 | Prevents fd exhaustion; standard server default |

The defaults are applied in `buildServiceContainerSpec()` when the manifest's `MaxProcesses`/`MaxOpenFiles` are zero.

### 5.11 Cgroup delegation for resource limits

Rootless Podman on cgroups v2 requires the unprivileged user to have a delegated cgroup scope. Without delegation, `--memory`, `--cpus`, and `--pids-limit` are **silently ignored** — Podman does not error.

The `loginctl enable-linger piccolo-runtime` in section 5.1 creates a persistent `user-<uid>.slice` in systemd, which provides cgroup delegation. This means:
- systemd creates `/sys/fs/cgroup/user.slice/user-<uid>.slice/` with proper delegation
- Podman can create sub-cgroups within this slice for resource enforcement

At piccolod startup, after resolving the runtime user, verify delegation is available:

```go
cgroupPath := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice", runtimeUID)
if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
    log.Printf("WARN: cgroup delegation not available for UID %d — "+
        "resource limits (--memory, --cpus, --pids-limit) will be silently ignored. "+
        "Run 'loginctl enable-linger piccolo-runtime' to fix.", runtimeUID)
}
```

This warning ensures silent failures are surfaced in logs rather than going undetected.

### 5.12 Rootless networking

Switching from rootful to rootless Podman changes the networking backend:

| | Rootful (before) | Rootless (after) |
|---|---|---|
| Backend | netavark + nftables | pasta (default on modern Podman) |
| Port forwarding | Kernel-level DNAT rules | Userspace namespace proxying |
| Latency | Kernel-native | Negligible overhead (pasta uses kernel namespaces, not TCP proxy) |

**Why `pasta` over `slirp4netns`**: `pasta` is the modern default (Podman 5+), uses kernel namespace plumbing rather than userspace TCP/UDP proxying (as `slirp4netns` does), and adds negligible latency. For a home server where the bottleneck is always the network or the app, the difference is immaterial.

**`127.0.0.1` port binding**: The existing pattern `--publish 127.0.0.1:<host>:<guest>` continues to work under rootless+pasta. The port is bound on the host's loopback interface by `pasta`, and piccolod's reverse proxy connects to it as before. No change needed in `buildCreateArgs()` or `internal/services/`.

**Netavark repair removed**: `flushAndReloadNetavarkRules()` and `internal/app/netavark_repair.go` are deleted entirely. Under rootless Podman with pasta networking, netavark is not used — there are no nftables DNAT rules to repair. Since this RFC targets new installations only (no rootful-to-rootless migration), the rootful code path is not needed.

## 6. Affected Modules

| Module | Change |
|--------|--------|
| `internal/container/podman.go` | Add `Credential` to `PodmanRuntime`, `applyRuntimeCredential()` helper, cap-drop/cap-add flags in `buildCreateArgs()`, resource limit args, apply credential in all methods |
| `internal/container/podman.go` (`ExecShellCmd`) | Remove `cmd.Env = os.Environ()`, use `applyRuntimeCredential()` instead |
| `internal/container/podman.go` (`PullImageWithProgress`) | Remove `cmd.Env = append(os.Environ(), "LC_ALL=C")`, use `applyRuntimeCredential()` instead (LC_ALL already in base env) |
| `internal/container/podman.go` (`ContainerCreateSpec`) | Add `PidsLimit`, `NofileLimit` to `ResourceLimits` |
| `internal/app/podman_runtime.go` | Store resolved `*syscall.Credential` on `AppManager`, populate in all `PodmanRuntime` instances, recursive chown of directories |
| `internal/app/multi_container.go` | Pass `MaxProcesses`/`MaxOpenFiles` through to `ResourceLimits`, apply defaults when zero |
| `internal/app/netavark_repair.go` | Removed (rootless uses pasta, no netavark) |
| `internal/app/workspacedisk/mount.go` | Add `allow_other` to fuse-overlayfs mount options |
| `internal/app/workspacedisk/manager.go` | `PodmanImageMounter` refactored to use `podman image inspect` (runs as piccolo-runtime, no root needed) |
| `internal/app/parser.go` | Reject `privileged: true` in `validateResourcePermissions()` |
| OS image build | Create `piccolo-runtime` user, configure `/etc/subuid`, `/etc/subgid`, `loginctl enable-linger` |

## 7. Deployment

This RFC targets **new installations only**. There is no migration path from rootful to rootless — the OS image must be built with all prerequisites in place.

### 7.1 OS image prerequisites

1. `piccolo-runtime` system user with subuid/subgid ranges (100000:65536).
2. `loginctl enable-linger piccolo-runtime` for cgroup delegation.
3. `user_allow_other` in `/etc/fuse.conf` for FUSE mount accessibility.
4. `fuse-overlayfs` installed.

### 7.2 Boot sequence

1. piccolod resolves `piccolo-runtime` at `AppManager` init — **fails hard** if user is missing.
2. Creates `/run/user/<uid>` (XDG_RUNTIME_DIR) and verifies cgroup delegation.
3. Ensures `piccolo-apps` group and cgroup v2 controller delegation.
4. `podmanRuntimeForApp()` provisions per-app users and ensures directory ownership.
5. App reconciliation loop creates containers under per-app users.

### 7.3 Testing

Unit tests use `PICCOLO_ALLOW_UNMOUNTED_TESTS=1` to bypass the runtime user requirement. This is the **only** context where `runtimeUser` is nil — production never runs rootful.

## 8. Testing

1. **Unit tests** for `applyRuntimeCredential()` — verify `SysProcAttr.Credential` is set and env is minimal when Credential is non-nil; verify no-op when nil; verify `extraEnv` merging; verify it does not overwrite existing `SysProcAttr` fields (e.g., `Setsid`).
2. **Unit tests** for `buildCreateArgs()` — verify `--cap-drop=ALL` and all `--cap-add` flags are present; verify `--pids-limit` and `--ulimit nofile` with explicit values and defaults.
3. **Unit test** for `validateResourcePermissions()` — verify `privileged: true` is rejected.
4. **Integration test** — create and start a container with rootless Podman, verify the container process runs as an unprivileged UID on the host (`podman top <id> huser`).
5. **Network anchor test** — explicitly test the `registry.k8s.io/pause:3.9` image under the new cap-drop set. This container underpins all multi-container apps; if it fails, every multi-container app breaks.
6. **Resource limit test** — verify `--pids-limit` is enforced (attempt fork bomb inside container, confirm it's killed).
7. **Cgroup delegation test** — verify resource limits are actually enforced, not silently ignored. Compare container's cgroup controllers before and after linger is enabled.
8. **Store app smoke tests** — install each store app (service and workspace mode), verify it starts and serves traffic.

## 9. Security Impact Summary

| Property | Before | After |
|---|---|---|
| Container root on host | UID 0 (real root) | Unprivileged UID (user namespace) |
| Container escape impact | Full root on host | Unprivileged user on host |
| Capabilities | ~14 granted (Podman rootful default) | Explicit allowlist of 12; all others dropped |
| Resource limits | Schema only (not enforced) | Enforced via cgroups (with delegation verification) |
| Podman process privilege | Root | Unprivileged |
| Environment isolation | Root env inherited | Minimal explicit env |
| `privileged: true` in manifest | Silently ignored | Rejected at parse time |
| Networking backend | netavark (kernel DNAT) | pasta (kernel namespaces) |

## 10. Future Work

These are deferred from this RFC but represent the next hardening steps:

- **Per-app Linux user isolation** — See [RFC 20260220](20260220-per-app-user-isolation.md) which extends this single-user design to provision a unique Linux user per app instance. This ensures container escapes are scoped to a per-app UID rather than the shared `piccolo-runtime` user.
- **`--security-opt no-new-privileges`** — per-app opt-in for images that don't use setuid-based user switching. Could be a manifest field `permissions.security.no_new_privileges: true`.
- **`--read-only` root filesystem** — for service-mode containers with explicit tmpfs for `/tmp`, `/run`. Workspace containers would opt out.
- **Custom seccomp profiles** — per-app or per-mode profiles that restrict the syscall surface beyond Podman's default.
- **Image signature verification** — verify container images are signed before pulling, reducing supply chain risk.

## 11. Implementation Notes & Status

| Change | Status | Location |
|--------|--------|----------|
| Create `piccolo-runtime` system user + linger | Done | OS image build |
| Resolve credential once at `AppManager` init | Done | `internal/app/app_manager.go` |
| Add `Credential` to `PodmanRuntime` | Done | `internal/container/podman.go` |
| Add `applyRuntimeCredential()` helper (minimal env) | Done | `internal/container/podman.go` |
| Apply credential in all PodmanCLI methods | Done | `internal/container/podman.go` (`podmanCmd`) |
| Refactor `ExecShellCmd` env handling | Done | `internal/container/podman.go` |
| Refactor `PullImageWithProgress` env handling | Done | `internal/container/podman.go` |
| Recursive chown of storage directories | Done | `internal/container/credential.go` (`ChownIfNeeded`) |
| Create XDG_RUNTIME_DIR at startup | Done | `internal/container/credential.go` (`EnsureXDGRuntimeDir`) |
| Verify cgroup delegation at startup | Done | `internal/container/credential.go` (`CheckCgroupDelegation`) |
| Cap-drop=ALL + cap-add in `buildCreateArgs()` | Done | `internal/container/podman.go` |
| Reject `privileged: true` | Done | `internal/app/parser.go` |
| Wire `PidsLimit`/`NofileLimit` with defaults | Done | `internal/container/podman.go`, `internal/app/multi_container.go` |
| Workspace fuse-overlayfs `allow_other` + `squash_to_uid` | Done | `internal/app/workspacedisk/mount.go` |
| `PodmanImageMounter` uses `podman image inspect` (rootless) | Done | `internal/app/workspacedisk/manager.go` |
| Remove netavark repair (rootless uses pasta) | Done | `internal/app/netavark_repair.go` deleted |
| Per-app user provisioning | Done | `internal/container/appuser.go` |
| Cgroup v2 delegation drop-in | Done | `internal/container/appuser.go` (`EnsureCgroupDelegation`) |
| Unit tests | Done | `internal/container/podman_test.go`, `internal/container/credential_test.go`, `internal/container/appuser_test.go` |
| E2E smoke tests (service + workspace) | Done | `scripts/dev-vm-test.sh` |
