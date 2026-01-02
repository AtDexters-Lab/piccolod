# RFC: Workspace Disk — Container-Independent Persistence

**Date:** 2026-01-01  
**Status:** Draft

## 1. Summary
This RFC proposes a persistence architecture for `x-piccolo.mode: workspace` that makes the workspace’s mutable bytes (the “instance disk”) **independent of Podman container objects**. The core idea is to persist a per-workspace writable filesystem layer (`upperdir`) inside the per-app encrypted volume, while reusing immutable base image layers as a shared read-only layer (`lowerdir`). Containers become disposable wrappers around the workspace disk: they can be stopped/removed/recreated (e.g., for listener CRUD) without risking data loss and without relying on `podman commit`.

This RFC also recommends an opinionated workflow direction: **Piccolo does not build images** as part of the core app platform. Workspaces are created from registry/imported OCI images pinned by digest; “productizing” a workspace happens via explicit export/publish steps, not via implicit snapshot-commit flows.

## 2. Goals
- **Workspace persistence is seamless:** users should not have to think about whether a change “stuck”.
- **Crash-safe by design:** container crash, piccolod restart, and host reboot must preserve workspace bytes (crash-consistent semantics).
- **Listener CRUD is safe:** exposing/unexposing ports may restart/recreate the container, but must not risk losing workspace bytes.
- **Efficient storage:** reuse base image layers; store only per-workspace diffs in the app volume.
- **Future-ready for leader/follower + federation:** the durable workspace bytes must live in the encrypted volume so ciphertext replication can provide RPO=0 later.

## 3. Non-goals (for this RFC)
- RPO=0 across devices today (planned later at ciphertext replication layer).
- Live/no-restart listener updates (restart is acceptable for listener CRUD in v1).
- In-cluster distributed filesystems or multi-writer volumes for workspaces.
- Full “image rebase” semantics for mutable workspaces (explicit workflows only).
- Implementing in-daemon image builds (`build:`) as a first-class experience.

## 4. Background: Current Workspace Persistence Model (Problem)
Today’s intended model for `x-piccolo.mode: workspace` is “container as a roaming instance”: the writable layer and container metadata live in the app’s encrypted `disk/` dataset.

However, several operations require container **recreation** (notably listener CRUD) because Podman does not support dynamic updates of `--publish` bindings on running containers. Current codepaths attempt to preserve the workspace filesystem across recreation by taking an on-demand snapshot via `podman commit` before removing the container.

This has two fundamental issues:
1. **Correctness depends on a best-effort snapshot step.** If the container/piccolod/host fails before the snapshot is taken (or if commit fails), the recreate path can lose user changes.
2. **The container object is treated as the persistence boundary.** Any operation that needs to delete/recreate the container becomes a data-loss risk unless snapshotting is perfect and transactional.

## 5. Proposed Solution: Workspace Disk as a First-Class Persistent Artifact

### 5.1 Model
Define a per-workspace **Workspace Disk** that holds all mutable root filesystem bytes.

At runtime, the container root filesystem is assembled as:
- `lowerdir` = immutable base image rootfs (shared, deduped, typically non-sensitive)
- `upperdir` = workspace’s persistent writable layer (inside the app encrypted volume)
- `merged` = runtime mountpoint used as the container’s rootfs

The container object becomes a replaceable wrapper around this assembled rootfs.

### 5.2 Storage layout (inside the per-app encrypted volume)
Within the app volume mount (e.g. `$PICCOLO_STATE_DIR/mounts/app-<id>/`), store:

```
disk/
  workspace/
    meta.json              # base image digest + config + versioning
    upper/                 # persistent writable layer (the “disk”)
    work/                  # overlay workdir (must be empty at mount; see §5.6)
    merged/                # mountpoint (ephemeral contents; recreated on start)
```

`meta.json` should minimally include:
- `format_version`
- `base_image_digest` (canonical)
- `base_image_ref` (for UI/auditing)
- `image_config` (the image config required to run with `--rootfs`; see §5.2.1)
- timestamps

#### 5.2.1 `meta.json` schema (v1)
When running containers with `podman ... --rootfs`, there is no image context, so Podman will not apply image configuration automatically (ENTRYPOINT/CMD/ENV/etc). Therefore, piccolod must persist the relevant image config at install time and explicitly apply it when creating the container wrapper.

To keep the schema future-proof, `image_config` should be treated as the **OCI image config object** (or equivalently `podman image inspect`’s `.Config` JSON) and stored largely verbatim. piccolod can extract the fields it needs today, while preserving additional config fields for future container features without a schema rewrite.

**Required fields (v1):**
- `format_version` (string, e.g. `"1"`)
- `base_image_digest` (string, e.g. `"docker.io/library/ubuntu@sha256:..."`)
- `base_image_ref` (string, user-facing reference, e.g. `"ubuntu:22.04"`)
- `image_config` (object; stored as the OCI image config JSON)
  - `image_config.entrypoint` (`[]string`, may be empty)
  - `image_config.cmd` (`[]string`, may be empty)
  - `image_config.env` (`[]string`, may be empty; OCI-style `"KEY=VALUE"` entries)
- `created_at` (RFC3339 string)

**Optional fields (v1):**
- `image_config.workdir` (string)
- `image_config.user` (string)

**Example:**
```json
{
  "format_version": "1",
  "base_image_digest": "docker.io/library/ubuntu@sha256:…",
  "base_image_ref": "ubuntu:22.04",
  "image_config": {
    "entrypoint": [],
    "cmd": ["/bin/bash"],
    "env": [
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    ],
    "workdir": "/",
    "user": "root"
  },
  "created_at": "2026-01-01T00:00:00Z"
}
```

Validation rules (v1):
- `base_image_digest` MUST be present and MUST be the digest used to mount the base rootfs.
- `entrypoint`/`cmd` MAY be empty, but piccolod MUST ensure the workspace can start (e.g., via `boot.sh` wrapper semantics).
- `env` is stored in OCI array form for fidelity; at runtime piccolod may normalize it (last entry wins) before applying additional environment overlays from the app manifest.

### 5.3 Runtime strategy (rootless-friendly)
On the leader node:
1. Ensure the base image (by digest) exists locally.
2. Obtain a read-only mountpoint for the base image rootfs (e.g. `podman image mount`).
3. Mount an overlay filesystem using `fuse-overlayfs`:
   - `lowerdir=<base_mount>`
   - `upperdir=<app_volume>/disk/workspace/upper`
   - `workdir=<app_volume>/disk/workspace/work`
   - mountpoint: `<app_volume>/disk/workspace/merged`
4. Run the workspace container using the assembled rootfs:
   - `podman run --rootfs <merged> ...`

This preserves base-layer dedupe while keeping mutable bytes inside the encrypted volume.

**Important:** when using `--rootfs`, piccolod must explicitly apply `ENTRYPOINT`, `CMD`, and `ENV` values from `meta.json` (plus any manifest overrides) when creating the container wrapper.

### 5.4 Container recreation is always safe
Because the durable bytes are in `upper/` and `meta.json`:
- Listener CRUD (or any other change that requires recreation) becomes:
  1) stop container → 2) remove container object → 3) create new container wrapper → 4) start
- No `podman commit` step is required.
- If recreation fails, the workspace disk remains intact and can be retried.

### 5.5 Listener CRUD semantics (restart acceptable)
In v1, listener CRUD may restart the workspace. The key guarantee is persistence:
- Stop/remove/recreate/start is permitted.
- All user changes persist because they are on the workspace disk, not on the container object.

Future enhancement (not required for v1): move port exposure out of Podman publishes so listener CRUD can be applied without container recreation.

### 5.6 Mount lifecycle and crash recovery
Overlay mounts must be treated as a runtime artifact:
- On clean stop/uninstall, unmount `merged/`.
- On daemon crash or host reboot, `merged/` may be left mounted or stale. On next start, piccolod should:
  - detect and unmount any stale mount at `merged/` (best-effort),
  - remount overlay idempotently.

The `upper/` directory is the durable source of truth; `merged/` should never contain unique durable bytes.

#### 5.6.1 `work/` semantics (why it exists, and how we manage it)
`work/` is not the “merged overlay”; it is an OverlayFS/fuse-overlayfs scratch directory required for copy-up and atomic rename operations. Requirements:
- `work/` MUST be on the same filesystem as `upper/`.
- `work/` MUST be empty at mount time.

In v1, piccolod should treat `work/` as **ephemeral runtime state** stored *inside* the encrypted volume only because it must be colocated with `upper/`. To avoid stale state after crashes, piccolod should empty or recreate `work/` before each mount attempt.

## 6. Efficient Storage: Reusing Base Image Layers
This design is efficient because only per-workspace diffs are stored in `upper/`. Base image reuse comes from:
- pinning base images by digest,
- storing base layers in a shared imagestore (dedupe across workspaces),
- mounting base rootfs read-only and using it as `lowerdir`.

### 6.1 Opinionated decision: remove “custom base image” as a core workflow
To reduce architectural friction:
- Piccolo should treat base images as **registry/imported OCI artifacts**, not as “built inside Piccolo”.
- Users who want custom images use external tooling (CI/CD or local build) and push to a registry, then deploy via manifest.
- “Workspace” mode is the supported path for iterative mutation and tinkering.

For v1, we assume workspace base images do not embed sensitive secrets and are pullable/available on all nodes that may become leader (see §7.2 and §11). Secrets should be injected via Piccolo’s secret mechanisms (env/files), not baked into images.

## 7. Leader/Follower + Federation Integration

### 7.1 Leader/follower
The per-app encrypted volume remains the roaming unit of state.
- **Leader** mounts the app volume RW, mounts the workspace overlay, runs the container, and serves traffic locally.
- **Follower** must not run the container for a `stateful` workspace app. It attaches the volume RO (warm standby) or detaches (cold standby) per policy.

Failover sequence (conceptual):
1. Old leader stops container and detaches volume.
2. New leader attaches volume RW.
3. New leader ensures base image digest is present locally.
4. New leader mounts overlay and starts a new container wrapper.

Workspace persistence is preserved because it is entirely in the volume ciphertext.

### 7.2 Federation storage network
Federation replication targets the encrypted ciphertext tree for the app volume.
- Replicating ciphertext captures `upper/` + `meta.json` → sufficient to preserve the workspace disk.
- Base image layers are intentionally **not** part of the replicated payload for catalog/public images to keep replication efficient.

**Availability note:** to run the workspace on a new leader, the base image digest must be obtainable (cached or pullable). If the registry is unavailable and the digest is not cached, the workspace disk is safe but the app may not be runnable until the base image is fetched.

Mitigations (policy-driven; can be phased):
- background prefetch of base digests on follower nodes,
- a periodic “base image warm-cache” cadence: when a workspace is installed/updated, followers automatically pull the pinned digest (best-effort) so failover is not blocked on registry availability.

## 8. Operations & State Machine (v1)

### 8.1 Install workspace
1. Validate manifest and select base image digest (prefer digest pinning).
2. Ensure app volume layout exists.
3. Pull base image (or ensure present).
4. Initialize `disk/workspace/` directories and write `meta.json`.
5. Mount overlay at `merged/`.
6. Create container wrapper using `--rootfs merged` and the wrapper entrypoint (`boot.sh`).
7. Start container.
8. Persist app state (including service allocations, container name, and workspace metadata pointers).

### 8.2 Start/Stop
- Stop: stop container; optionally unmount overlay (or leave mounted and manage on next start).
- Start: ensure base image present; mount overlay; create/start container if missing.

### 8.3 Listener CRUD
- Apply new listener config.
- Stop container.
- Remove container object.
- Recreate container wrapper with updated publishes.
- Start container.
- Persist new listener configuration and service allocations.

### 8.4 Uninstall
- Without purge: stop/remove container wrapper; keep the app volume (workspace disk remains).
- With purge: stop/remove container wrapper; destroy app volume.

## 9. Security Considerations
- **Durable bytes encryption:** workspace diffs live inside the per-app encrypted volume (`upper/`), satisfying “no plaintext user changes on host disk”.
- **Base image content:** in v1 we assume images do not embed secrets and treat base layers as non-sensitive artifacts. Secrets must be injected at runtime via Piccolo secret mechanisms (env/files).
- **No implicit snapshots:** removing `podman commit` from correctness-critical paths reduces the risk of snapshotting leaking data into unexpected places and avoids snapshot failure modes becoming persistence failure modes.

## 10. Rollout Plan
1. **Phase 0 (Design):** finalize this RFC and define exact `meta.json` + mount-manager contracts.
2. **Phase 1 (Risk validation):** validate `gocryptfs` + `fuse-overlayfs` stacking and `podman --rootfs` viability with targeted stress tests (see §11).
3. **Phase 2 (Foundations):** implement workspace disk layout + `meta.json` + overlay mount/unmount manager (leader-only).
4. **Phase 3 (Runtime wiring):** run workspace containers via `--rootfs merged` and remove commit-based snapshotting from listener CRUD.

## 11. Open Questions
- `gocryptfs` + `fuse-overlayfs` stacking: performance and crash-consistency under stress; recommended mount options.
- `podman --rootfs` compatibility across Podman upgrades: ensure our “rootfs container wrapper” path stays supported and covered by tests.
- Handling “image update” for mutable workspaces:
  - explicit “rebase” workflow vs “clone new workspace from new base and copy data”.

## 12. Workspace Disk Mount Manager (proposed interface)
The workspace disk requires a dedicated “mount manager” responsible for idempotent mount/unmount, cleanup of stale mounts, and safe handling of crashes.

Proposed responsibilities:
- Ensure base image mount exists (and is unmounted on cleanup when appropriate).
- Ensure `upper/`, `work/`, and `merged/` exist.
- Ensure `work/` is empty before each mount.
- Mount overlay (via `fuse-overlayfs`) and verify it is active.
- Unmount overlay robustly (best-effort) on stop/uninstall, including stale mount cleanup on next start.
- On partial failures, perform best-effort cleanup so subsequent calls can retry safely.
- Be defensive: assume the prior run may have crashed. On startup (and/or at the start of `Mount`), aggressively attempt stale mount/lock cleanup (best-effort) so workspaces can self-heal.

Proposed Go-ish surface (illustrative):
```go
type WorkspaceDiskManager interface {
  EnsureInitialized(ctx context.Context, instanceID string, meta WorkspaceMeta) error
  Mount(ctx context.Context, instanceID string) (mergedPath string, err error)
  Unmount(ctx context.Context, instanceID string) error
  CleanupStale(ctx context.Context, instanceID string) error
  Status(ctx context.Context, instanceID string) (WorkspaceDiskStatus, error)
}
```

Idempotency expectations:
- `Mount` is safe to call repeatedly; if already mounted, it returns success.
- `Unmount` is safe to call repeatedly; if not mounted, it returns success.
- `CleanupStale` is safe on startup and best-effort (uses lazy detach when needed).

Partial failure expectations:
- If `Mount` fails after mounting the base image rootfs but before mounting the overlay, the manager should unmount the base image mount (best-effort) before returning.
- If `Mount` fails after a partially-created overlay mount, the manager should attempt to unmount `merged/` (lazy detach if needed) and reset `work/` before returning.

`Status` is intended for debugging/observability and should report (at minimum): whether `merged/` is currently mounted, whether base image is mounted, and the last observed digest/config applied.

## 13. Observability (v1 requirements)
We should make workspace disk health debuggable from logs and metrics:
- Log the base digest, overlay mount parameters, and mount/unmount outcomes (without leaking sensitive paths).
- Emit structured events for:
  - overlay mount failures
  - stale mount cleanup performed
  - base image missing / pull failures (which can block failover)
- Add a periodic lightweight health check on leader:
  - verify merged mount is present when container is running
  - optionally verify basic read/write within the merged view
- Track counters for:
  - `workspace_mount_success_total`, `workspace_mount_fail_total`
  - `workspace_unmount_success_total`, `workspace_unmount_fail_total`
  - `workspace_stale_mount_cleanup_total`
  - `workspace_base_image_missing_total`

## 14. Interaction with `x-piccolo.mode: service`
This RFC is scoped to `x-piccolo.mode: workspace` and does not change the persistence contract for `service` apps:
- `service` apps remain “replaceable rootfs”: correctness depends on explicitly declared `storage.persistent` mounts, and Piccolo may recreate containers for updates/reconfiguration.
- The workspace disk mount manager is only used for `workspace` apps.

Commonalities / intersections:
- Base image layer reuse can (and should) be shared across both modes via a shared imagestore/cache, keyed by digest.
- Digest pinning and follower “warm-cache” prefetch (see §7.2) benefits both modes, but is more critical for workspaces because failover must be able to reassemble the rootfs from `meta.json` + base digest.

## Implementation Notes & Status

**Status:** Core implementation complete (2026-01-03). Manual testing in progress.

### Implemented

#### Workspace Disk Mount Manager (`internal/app/workspacedisk/`)
- `manager.go` — `WorkspaceDiskManager` with `EnsureInitialized`, `Mount`, `Unmount`, `CleanupStale`, `Status`
- `mount.go` — `fuse-overlayfs` mounting/unmounting, stale mount detection and cleanup
- `meta.go` — `meta.json` schema (v1) with `WorkspaceMeta` and `ImageConfig` types
- `errors.go` — Typed errors (`ErrNotInitialized`, `ErrAlreadyMounted`, etc.)

#### App Manager Integration (`internal/app/`)
- `workspace_disk_integration.go` — Glue between `AppManager` and `WorkspaceDiskManager`
- **Install flow** (`installLocked`):
  - New install: pulls image, initializes workspace disk, mounts overlay, creates container with `--rootfs merged`
  - Reinstall: mounts existing workspace disk, preserves `upper/` data
- **Start/Stop** (`startLocked`, `stopInternal`):
  - Start ensures overlay is mounted before starting container
  - Stop unmounts overlay on clean shutdown
- **Container recreation** (`recreateMissingContainer`):
  - Properly mounts workspace disk and uses `--rootfs` mode
  - Fixes bug where stop/start lost overlay data
- **UpdateImage blocked** for workspace apps — returns error explaining persistence is tied to base image

#### Container Runtime (`internal/container/podman.go`)
- `ContainerSpec.Rootfs` field for `--rootfs` mode
- `MountImage` / `UnmountImage` for base layer access via `podman image mount`
- Terminal exec uses bash with login shell (`-l`) for proper completion/colors
- TERM propagated into container via `-e TERM=xterm-256color`

#### UX Improvements
- `piccolo-startup` helper command (`internal/app/assets/piccolo-startup`)
  - Discoverable via `/usr/local/bin/piccolo-startup` in workspace containers
  - Commands: `edit`, `show`, `enable`, `disable`
- `boot.sh` auto-chmod — makes `start.sh` executable if it exists but isn't
- Terminal window closes on Ctrl+D (WebSocket close code 1000 handling)

### Not Yet Implemented
- Stress tests for `gocryptfs` + `fuse-overlayfs` + `--rootfs` runtime (§11)
- Metrics/events for mount health (§13)
- Periodic health checks for mounted overlays

### Known Limitations
- Image rebase not supported — users must uninstall/reinstall to change base image
- No automatic stale mount cleanup on daemon startup (cleanup happens on next `Mount` call per-workspace)
