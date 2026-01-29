# RFC: Multi-Container Apps (Compose-Style) for Piccolo OS

**Date:** 2026-01-02  
**Status:** Implemented

## 1. Summary
Piccolo apps are currently modeled as **one app instance = one Podman container**. This RFC evolves the runtime and manifest format to support **multiple containers per app instance** (Compose-style) for **`x-piccolo.mode: service` apps**, while standardizing on a `services`-first manifest for **all modes**.

Explicit product/platform decisions captured here:
- **No in-daemon image builds:** manifests must specify `image`; `build:` (containerfile/git) is rejected.
- **Workspaces stay single-container:** `x-piccolo.mode: workspace` remains a single container (VM-like) at runtime, but manifests still use `services` with exactly one service (see `docs/rfc/20260101-workspace-disk-container-independent.md` for workspace persistence direction).
- **No inter-app dependencies, ever:** the platform does not model “app A depends on app B”. Dependencies must be packaged inside an app as sidecars.
- **No backwards-compat requirement:** assume no existing deployments; manifest/runtime changes do not need a migration strategy.

This RFC’s initial scope focuses on the common “**one primary container + N sidecars**” pattern with:
- encrypted-at-rest guarantees preserved (per-app gocryptfs volume),
- leadership semantics applied to the whole group (leader runs, follower stops),
- per-service logs/exec for debugging and UI observability.

For multi-container apps, this RFC also introduces one internal **network anchor (infra/pause) container** per app instance to hold the shared network namespace and listener publish bindings.

### 1.1 Terminology (to avoid “service” ambiguity)
- **Listener:** an externally proxied endpoint defined under `listeners` (historically called a “service” in parts of the code/API today).
- **Service:** a container-level unit defined under `services` (Compose-style) when `x-piccolo.mode: service`.
- **Primary service:** the “main” container that typically serves user requests; listeners target this service by default.
- **Sidecar:** any non-primary service container in the app instance.
- **Network anchor:** an internal infra/pause container whose network namespace all services join (`--network container:<id>`); it publishes listener ports for the whole app group.

## 2. Current State (Baseline)

### 2.1 Manifest model (v1)
`docs/app-platform/specification.yaml` documents today’s manifest (`app.yaml`) as a single container with:
- `image` (today it also documents `build`, but this RFC adopts “no builds” and requires rejecting `build:`)
- `listeners` (required)
- `storage` volumes (mapped into the per-app encrypted volume)
- `x-piccolo.mode` = `service` or `workspace`

### 2.2 Runtime model (v1)
Key implementation touchpoints (non-exhaustive):
- `internal/app/app_manager.go`: installs, starts, stops, reconciles **one** container per instance.
- `internal/app/filesystem.go`: persists `metadata.json` with a single `container_id`.
- `internal/services/manager.go`: allocates listener ports and restores proxies by inspecting **one** container’s published ports.
- `internal/server/*`: logs endpoints assume one container per app (`GET /api/v1/apps/:name/logs` and `/logs/stream`).
- `ui/lib/shared/widgets/log_stream_viewer.dart`: streams logs for an app without selecting a container/service.

## 3. Goals / Non-Goals

### 3.1 Goals
1. Support **multi-container** app instances with an explicit **primary** container and additional **sidecars**.
2. Keep “single-service” apps simple (multi-container is opt-in).
3. Keep all durable app bytes **encrypted at rest** under the app’s gocryptfs mount:
   - container writable layers (“disk dataset” / Podman `--root`)
   - declared persistent volumes (“data dataset”)
   - logs and container metadata (where applicable)
4. Apply **leadership semantics** at the app level: if an app transitions to follower, *all* its containers stop.
5. Provide **logs per service/container** (API + UI).
6. Provide deterministic **intra-app** start/stop ordering for sidecars (but no cross-app dependencies).

### 3.2 Non-Goals (initial phase)
- Full Docker Compose parity (networks, readiness semantics, profiles, etc.).
- Multiple externally-addressable containers per app with independent port namespaces (we keep “listeners map to a single shared port space” initially).
- Cross-app service discovery and inter-app dependency graphs (explicitly not supported).

## 4. Proposed Manifest Evolution

### 4.1 Add `services` (required for service + workspace)
We introduce a required `services:` map for **all apps**. Each entry describes one container (“service”) belonging to the app instance.

Ergonomics rules:
- Service mode: `services` may define multiple containers.
- Workspace mode: `services` must define **exactly one** container (workspace remains single-container at runtime).
- The primary service is `primary_service` if specified, else defaults to `main`.
- All per-container fields must live under `services.<name>`; top-level container fields are rejected (e.g. `image`, `environment`, `storage`, `resources`).
- App-level fields remain at the root (e.g. `listeners`, `healthcheck`, `permissions`, `app_config`, `x-piccolo`).

Validation rules:
- `build:` is rejected (platform does not build images).
- Top-level `depends_on:` is rejected (no inter-app dependencies).
- Workspace mode requires exactly one service; service mode requires at least one service.
- Reject top-level container fields (no implicit “defaults for primary”).
- Every service must declare `bind_ports` (may be empty) so Piccolo can validate shared port namespace collisions at manifest time.
- The primary service’s `bind_ports` must include every `listeners[].guest_port` (v1 listeners target the primary service by default).

### 4.2 New fields

#### 4.2.1 `primary_service`
```yaml
primary_service: web
```
- Optional.
- Defaults to `main`.

#### 4.2.2 `services.<name>`
Each service supports a subset of the existing app-level fields:
- `image` (required)
- `after` (optional; ordering only)
- `bind_ports` (required; shared port namespace contract)
- `environment` (optional)
- `storage` (optional)
- `resources` (optional; per-service CPU/memory)

Permissions remain **app-level** in v1 (no per-service permission overrides).

```yaml
services:
  web:
    image: docker.io/library/nginx:alpine
    after: [db] # start order only (not readiness)
    bind_ports: [8080]
    resources:
      limits:
        memory: 512MB
        cpu: 1.0
    environment:
      FOO: bar
    storage:
      persistent:
        content:
          container: /usr/share/nginx/html
  db:
    image: docker.io/library/postgres:16
    bind_ports: [5432]
    resources:
      limits:
        memory: 2GB
        cpu: 2.0
    environment:
      POSTGRES_PASSWORD: example
    storage:
      persistent:
        pgdata:
          container: /var/lib/postgresql/data
```

Notes:
- `storage.persistent` keys (e.g., `pgdata`) are **global by name** within the app instance.
- To distinguish intentional sharing from copy/paste mistakes, if a volume name is referenced by more than one service, every reference must set `shared: true` (see below). Otherwise the manifest is rejected.
- Conflicts (e.g., same volume name but different `size_limit`) must be rejected at validation time.
- `after` defines deterministic start/stop ordering. It does **not** imply readiness; dependent services may observe connection failures until dependencies are accepting connections.
- When multiple valid start orders exist, Piccolo uses a stable topological sort with lexicographic service-name ordering as a tiebreaker.
- Because the network namespace is anchored by an infra container (§5.1), `after` can be used for the primary service as well (e.g., `web.after: [db]`).
- `bind_ports` is the manifest-time contract for the shared port namespace. Piccolo validates that no two services declare the same `bind_ports` entry, and that the primary service declares all `listeners[].guest_port` entries.
- Services must not bind to additional TCP ports outside `bind_ports`. If they do (or if they misdeclare), collisions may still occur at runtime and will surface as container start failures.

##### Explicit volume sharing (`shared: true`)
When multiple services mount the same volume name, require `shared: true` to make the intent explicit:
```yaml
services:
  web:
    storage:
      persistent:
        cache:
          container: /cache
          shared: true
  worker:
    storage:
      persistent:
        cache:
          container: /cache
          shared: true
```
If `cache` is referenced by more than one service and any reference omits `shared: true`, reject the manifest.

#### 4.2.3 Readiness gating (planned optional; not in v1)
We intentionally keep v1 minimal: `after` controls *ordering*, not *readiness*. Most modern apps already handle “dependency not ready yet” via retries/backoff and crash-restart.

We reserve an optional `wait_for` block per service (targeting phase 2 if needed). Until implemented, manifests containing `wait_for` should be rejected (to avoid “silently ignored” behavior).

Example shape:
```yaml
services:
  web:
    after: [db]
    wait_for:
      tcp:
        host: 127.0.0.1
        port: 5432
        timeout: 60s
```

`wait_for` is strictly optional and should remain intentionally small in scope (tcp connect / http GET).

#### 4.2.4 Listeners default to primary (optional future: listener targets)
Initially:
- `listeners` remain at the app root and target the primary service by default.
- In multi-container apps, publish bindings are attached to the network anchor container (§5.1), but the guest ports are still expected to be served by the primary process unless a future listener target is specified.

Future extension (not required for phase 1 but reserved in schema):
```yaml
listeners:
  - name: metrics
    service: sidecar-metrics
    guest_port: 9090
    flow: tcp
    protocol: http
```

## 5. Proposed Runtime Model (Phase 1: “Primary + Sidecars”)

### 5.1 Container grouping: shared network namespace
We model a multi-container app instance as a “pod-like” group by introducing an internal **network anchor** container (an infra/pause container) and running **all** services (primary + sidecars) with:
- `--network container:<network_anchor_id>`

This yields:
- All services share `localhost` and a single port namespace.
- The primary service can start **after** sidecars (because the network namespace is anchored by the infra container, not the primary).
- Because the port namespace is shared, two services cannot both bind the same TCP port (e.g. both trying to listen on `0.0.0.0:8080`). Piccolo validates this via the required `bind_ports` declarations and via listener `guest_port` uniqueness.

Port publishing note (Podman constraint):
- Containers using `--network container:<id>` cannot meaningfully own port publishes; the publish bindings must live on the container that owns the network namespace.
- Therefore, for multi-container apps, **listener port publishes attach to the network anchor container**. Listeners still *target the primary service by default*; the anchor container is just the stable network namespace + publish holder.
- The network anchor container runs a minimal “pause” process from a non-sensitive built-in image (stored in the shared imagestore, like other base images).

### 5.2 Container naming
To keep naming deterministic and enable safe reconciliation:
- Network anchor container name is deterministic:
  - `"<instanceID>__netns__"`
- Primary container name remains exactly the `instanceID` (existing behavior; it is still the default logs/terminal target).
- Sidecar container names are deterministic:
  - `"<instanceID>__<serviceName>"`

Rationale:
- `instanceID` and `serviceName` should both follow Piccolo’s standard name rules (lowercase letters, digits, hyphens).
- Using `__` as a delimiter avoids ambiguity/collisions even if names contain `-` or `--`.

### 5.3 State persistence changes
We extend app runtime metadata to track multiple containers:
- `metadata.json` gains `containers: { "<service>": "<container_id>" }`.
- Store `primary_service` explicitly (so runtime behavior does not depend on defaults drifting).
- Keep a `container_id` field for the primary container as a convenient shortcut (even if redundant with `containers[primary_service]`).
- Introduce `network_anchor_id` as the authoritative ID used to attach services (`--network container:<id>`) and to inspect published listener ports during proxy restore. For multi-container apps, this is the infra/pause container ID.

### 5.4 Lifecycle operations

#### Install
1. Allocate listener endpoints (unchanged).
2. Create and start the network anchor container (publishes all listener ports).
3. Create service containers (primary + sidecars), each with network mode `container:<network_anchor_id>` and **no published ports**.
4. Start service containers in topological order derived from `after` (stable; lexicographic service-name tiebreak, and lexicographic order when there are no edges).
5. Persist `network_anchor_id`, `containers` map, and primary `container_id`.

#### Start/Stop
- `Start(app)` starts the network anchor, then starts services in topological order derived from `after`.
- `Stop(app)` stops services in reverse order, then stops the network anchor.

#### Reconcile
- Desired state remains app-level.
- If the network anchor is missing/recreated: recreate it, then reap and recreate **all** services (they were attached to the old network namespace).
- If a service container is missing: recreate that service only (network anchor required).

Status reporting:
- Persist per-service container IDs and observed container state (exists/running).
- Consider introducing an app-level `"degraded"` status when the primary is running but one or more declared sidecars are not (UI can surface which sidecar is unhealthy).

#### Uninstall
- Stop and remove all containers for the instance.
- Purge behavior applies to the shared per-app encrypted volume as today.

### 5.5 Normalization & planning (mandatory)
To avoid scattering conditional logic across the codebase (and to avoid ad-hoc “diffing” of containers), introduce a single normalization + planning step:
- Parse + validate YAML into `internal/api.AppDefinition`.
- Normalize into an internal runtime plan (example shape):
  - `PrimaryService string`
  - `Services map[string]ServiceSpec` (including the primary)
  - `Listeners []api.AppListener` (still app-level in phase 1)
  - `NetworkAnchorID string` (infra/pause container ID for multi-container apps)
  - `StartOrder []string` (topologically sorted by `after`)

`AppManager` should operate on the normalized model only. This keeps:
- reconciliation logic reusable
- install/update/start/stop paths consistent
- future compose-generalization (networks, per-service listeners) localized to the normalizer + container planner

Reconciliation should be structured as a small state machine:
1. `reconcileNetworkAnchor()` (establish a stable `network_anchor_id`)
2. `reconcileServices()` (only runs if the anchor is stable/unchanged)

### 5.6 Container identity (labels) (mandatory)
All containers created by Piccolo must be labeled for:
- debugging (understanding which container belongs to which app/service),
- safe garbage collection (reaping zombies/orphans),
- future migrations where persisted state may change shape.

Required Podman labels on **every** container:
- `io.piccolo.instance=<instanceID>`
- `io.piccolo.service=<serviceName>` (for the network anchor container, use the reserved value `__netns__`)
- `io.piccolo.role=network_anchor|service` (explicitly distinguish the infra container from app services)

### 5.7 Zombie container cleanup (mandatory)
With N containers per app, “zombie” containers become more likely (partial failures, crashes during recreate, etc.).

The runtime must implement a best-effort pruning step during install/start/reconcile:
- list all containers matching `io.piccolo.instance=<instanceID>`
- compute the valid expected set (network anchor + primary + declared sidecars)
- remove any labeled container not in the expected set (or not in the persisted `containers` map, depending on the chosen reconciliation source-of-truth)

This keeps `podman ps` clean and prevents resource leaks. It also avoids “name already in use” issues when recreating containers.

## 6. Persistence & Encryption Considerations (Must-Fix)

### 6.1 Encrypted datasets already in place
The existing per-app encrypted volume layout (`internal/app/storage.go`) provides:
- `disk/podman` for Podman `--root` (encrypted)
- `data/` for user-declared persistent volumes (encrypted)

Multi-container services share the same volume and therefore inherit these guarantees.

### 6.2 Base image sensitivity policy
Podman base image layers may be stored outside the per-app encrypted volume (e.g., a shared imagestore for dedupe/bandwidth efficiency). The platform assumes:
- images are **non-sensitive artifacts** (they must not embed user secrets),
- secrets are injected at runtime (env/files) and therefore land inside the encrypted per-app `--root` and/or declared volumes.

Workspace persistence and “container-independent disk” concerns are handled separately by `docs/rfc/20260101-workspace-disk-container-independent.md`. Multi-container is not supported for workspace mode.

## 7. Leadership Semantics (Leader/Follower)
App leadership currently stops a single container on follower transition. With multi-container apps:
- Leadership applies to the **entire app instance**.
- On follower transition: stop **primary + all sidecars** and remove proxies.
- On leader transition: normal reconcile/restore starts the entire group.

Optional (future): inject role into containers (`PICCOLO_ROLE=leader|follower`) or a file under `/piccolo/`.

## 8. Logs (All Services)

### 8.1 API changes
Extend logs endpoints to support selecting a service/container:
- `GET /api/v1/apps/:name/logs?tail=200&service=<serviceName>`
- `GET /api/v1/apps/:name/logs/stream?tail=200&timestamps=1&service=<serviceName>`

Defaults:
- If `service` omitted, stream the primary service logs (existing behavior).

To support UI discoverability (service dropdown), `GET /api/v1/apps/:name` should also return container/service status alongside the existing listener endpoints. To reduce ambiguity with manifest `services`, prefer exposing listener endpoints under a `listeners` key (instead of `services`).

Example:
```json
{
  "app": { "...": "..." },
  "listeners": [ /* existing: listener endpoints */ ],
  "containers": [
    { "service": "main", "container_id": "…", "running": true },
    { "service": "db", "container_id": "…", "running": true }
  ]
}
```

### 8.2 UI changes
Update `ui/lib/shared/widgets/log_stream_viewer.dart` and app detail screens to allow:
- choosing a service (primary + sidecars)
- streaming logs for the selected service

### 8.3 Exec terminal (per service)
Piccolo supports a WebSocket terminal via:
- `GET /api/v1/apps/:name/terminal`

With sidecars, allow exec into *any* service container:
- `GET /api/v1/apps/:name/terminal?service=<serviceName>`

Defaults:
- If `service` omitted, exec into the primary service (existing behavior).

UI impact:
- `ui/lib/features/apps/workspace_terminal.dart` needs a service selector (or a separate “container” chooser in app details).

## 9. Dependency Model (No Inter-App Dependencies)
Piccolo does not support “app A depends on app B”, and we should not add this later:
- it becomes ambiguous with multi-instance IDs,
- it couples independent lifecycle domains (install/upgrade/rollback) unnecessarily,
- it is better modeled as **sidecars within a single app** when the dependency is required.

Therefore:
- top-level `depends_on:` is removed from the spec and rejected by validation.
- dependencies *within* an app use `services.<name>.after` for **start/stop ordering only**.

## 10. Decisions (for v1)
These are the recommended choices for the initial implementation:
1. **Networking:** shared network namespace (pod-like) anchored to an internal infra/pause “network anchor” container.
2. **Volumes:** per-service `storage` blocks with explicit sharing via `shared: true` on volumes mounted by more than one service.
3. **Readiness:** no readiness gating; `after` is ordering-only.
4. **Resources:** per-service CPU/memory limits supported; permissions remain app-level.

## 11. Implementation Plan (Phased)

### Phase 1: Manifest + validation
- Reject `build:` and any build-related fields (`containerfile`, `git`, etc.).
- Reject top-level `depends_on:`.
- Enforce: `services:` is only valid when `x-piccolo.mode: service`.
- If `services` is present, reject top-level container fields (`image`, `environment`, `storage`, `resources`, etc.).
- Implement `services.<name>.after` validation (unknown references, cycles).
- Enforce explicit volume sharing rules: if a volume name is referenced by more than one service, every reference must include `shared: true`, and volume attributes (e.g. `size_limit`) must match (mount path may differ).
- Require `services.<name>.bind_ports` for every service; validate collisions across services and require the primary service to declare all `listeners[].guest_port` values.
- Parse/validate per-service resource limits (CPU/memory). Keep permissions app-level.

### Phase 2: Runtime + State
- Add `services` support + normalization layer in `internal/app/parser.go` / `internal/api/types.go`.
- Update `FilesystemStateManager` (`internal/app/filesystem.go`) to persist/load `containers` map.
- Update `AppManager` (`internal/app/app_manager.go`) to install/start/stop/reconcile/uninstall groups.
- Add a per-app network anchor container for multi-container apps; attach listener publishes to the anchor and run all services with `--network container:<network_anchor_id>`.
- Treat `network_anchor_id` as authoritative: if it changes, reap/recreate all services.
- Split reconciliation: `reconcileNetworkAnchor()` then `reconcileServices()` (only when the anchor is stable).
- Make labels mandatory and prune zombie containers by label.
- Keep listeners app-level and default target the primary service (shared network namespace).
- Optional (if needed): implement `services.<name>.wait_for` (tcp/http) as an explicit readiness gate before starting a dependent service.

### Phase 3: Logs + UI
- Add `service` selection to log endpoints and the Flutter UI.
- Rename app detail payload field `services` → `listeners` to avoid collision with manifest `services` (update UI accordingly).

### Phase 4: Listener targets (optional)
- Allow listeners to target a non-primary service.
- Validate that `guest_port` uniqueness holds across the shared network namespace.

### Phase 5: Compose-generalization (future)
- Optional: podman network per app and per-container port namespaces.
- Optional: replace the explicit “network anchor container” implementation with a Podman pod abstraction (still stable netns + group publishes, but with a first-class Podman object).

### Testing & rollout gates
- Unit: normalization + validation (single-service and multi-service manifests).
- Unit: `AppManager` lifecycle + reconcile for multi-container using `MockContainerManager` (start/stop/recreate ordering, primary-missing vs sidecar-missing).
- Server: logs/stream + terminal handlers accept `service` param and default to primary.
- UI: add a minimal regression check that log stream URL construction includes `service=` when selected.

## 12. Open Questions
- None for v1; decisions above are intentional to keep scope tight.

## Implementation Notes & Status
- **Implemented (2026-01-03):** backend/runtime support for `services`-based multi-container apps in `x-piccolo.mode: service`.
- **Manifest:** `build:` and top-level `depends_on:` are rejected; `services` + `primary_service` + `bind_ports` validated; explicit persistent volume sharing via `shared: true` enforced.
- **Runtime:** a per-app **network anchor** container (`<instanceID>__netns__`) holds published listener ports; all services join its network namespace via `--network container:<anchorID>`.
- **State:** `metadata.json` now persists `primary_service`, `network_anchor_id`, and `containers{service:container_id}` for multi-container apps.
- **API:** app logs + log streaming + terminal exec accept `?service=<name>` (defaults to primary service).
- **Implemented (2026-01-05):** UI service selector for multi-container apps; logs + terminal include `?service=<name>` when selected; app detail consumes `listeners` + `containers`.
