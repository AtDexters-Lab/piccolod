# App Store UI Integration Guide

Backend POC: codex (piccolod). Updated 2025-12-10.

## Scope
- Surfaces the curated catalog, local installs, and lifecycle controls (start/stop/uninstall) for apps managed by piccolod.
- All routes live under `/api/v1`; OpenAPI source of truth: `GET /api/v1/openapi.yaml` (also `docs/api/openapi.yaml`).

## Auth, CSRF, CORS
- Session cookie `piccolo_session` required for all state-changing routes; obtain via `/auth/login`.
- CSRF: send header `X-CSRF-Token` on non-GET requests; fetch via `GET /auth/csrf`.
- Same-origin CORS; localhost cross-port allowed. Include `credentials: include` on fetch/XHR.
- Locking: when crypto storage is locked, install/start/stop/uninstall return **423 Locked** with guidance in the message.

## Key Endpoints (App Store)
| Method | Path | Auth | CSRF | Purpose |
| --- | --- | --- | --- | --- |
| GET | `/catalog` | session | no | Curated catalog list (name, image, description, template snippet).
| GET | `/catalog/{name}/template` | session | no | Fetch YAML template for a catalog item.
| POST | `/apps/validate` | session | yes (because POST) | Validate app.yaml without installing. Content-Type: `application/x-yaml` / `text/yaml` or JSON `{ "app_definition": "..." }`.
| POST | `/apps` | session | yes | Install/Upsert app from YAML or JSON wrapper; returns created app instance. (Uninstall + recreate if name exists.)
| GET | `/apps` | session | no | List installed apps with status.
| GET | `/apps/{name}` | session | no | App detail plus its services array.
| DELETE | `/apps/{name}?purge=true|false` | session | yes | Uninstall; `purge=true` also deletes app data volumes.
| POST | `/apps/{name}/start` | session | yes | Start container.
| POST | `/apps/{name}/stop` | session | yes | Stop container.
| GET | `/apps/{name}/services` | session | no | Services for a single app (same shape as `services` in detail).
| GET | `/services` | session | no | All services across apps (for dashboards/port list).

Notes
- Supported request bodies for install/validate: raw YAML or JSON wrapper. For YAML uploads set `Content-Type: application/x-yaml` and send the YAML directly.
- Search/filter is client-side: server only offers list endpoints (no query params for search/sort/paging).

## Response Shapes (backend contract)
- **Install /apps (201)**: `{ "data": {App}, "message": "App '<name>' installed successfully" }`
- **Validate /apps/validate (200)**: `{ "data": { "valid": true }, "message": "valid" }`
- **List /apps (200)**: `{ "data": [App], "message": "Found N apps" }`
- **Get /apps/{name} (200)**: `{ "data": { "app": App, "services": [ServiceEndpoint] }, "message": "" }`
- **Start/Stop/Uninstall (200)**: `{ "data": null, "message": "App '<name>' started|stopped|uninstalled ..." }`
- **Errors**: `{ "error": { "error": <status text>, "code": <http status>, "message": <detail> } }` for app routes; some legacy handlers return `{ "error": "text" }` (handle both).

Key object fields
- **App**: `id`, `name`, `image`, `type`, `status` (`created|running|stopped|error`), `volumes` (host/container), `environment`, `container_id` (present in detail, not guaranteed in list).
- **ServiceEndpoint**: `app`, `name`, `guest_port`, `host_port` (127.0.0.1 bind), `public_port`, `remote_ports`, `remote_host` (fqdn when remote enabled), `flow` (`tcp|tls`), `protocol` (`http|websocket|raw`), `middleware`, `scheme` (`http|https|ws|wss`).
- **Catalog app**: `name`, `image`, `description`, `template` (inline YAML snippet).

## Happy-Path Flows
### Load catalog
1) GET `/api/v1/catalog` → show cards. Optional: GET `/api/v1/catalog/{name}/template` when user clicks “View YAML / Customize”.

### Browse installed apps
1) GET `/api/v1/apps`
2) For each card row, the UI can show status and quick actions.
3) Optional: GET `/api/v1/apps/{name}` to display detail + services (for port/URL badges).

### Install from catalog/template
1) (Optional) GET template → prefill editor.
2) POST `/api/v1/apps/validate` with YAML; if 200 valid, proceed; on 400 show parser/validation error message.
3) POST `/api/v1/apps` with the same YAML (or JSON wrapper). On success, redirect to detail or refresh list. Expect 201.
4) Immediately read detail via GET `/apps/{name}` to render assigned ports and status.

### Start / Stop
- POST `/apps/{name}/start` or `/apps/{name}/stop`; then refresh `/apps/{name}` or `/apps`.
- In demo mode (`PICCOLO_DEMO=1` on server) these always succeed with canned messages (useful for UI dev; status may not reflect real Podman state).

### Uninstall (with purge)
- DELETE `/apps/{name}?purge=true` when user confirms data deletion; omit or `purge=false` for keeping volumes.

### Services & URLs
- Use `services` from app detail for per-app table, or `/services` for a global view.
- Show LAN URL as `http(s)://<host>:host_port` based on `scheme` and `host_port`.
- If remote is enabled, `remote_host` is an FQDN; combine with `scheme` and `remote_ports` (defaults to 80/443) for remote badges.

## Error Handling & Edge Cases
- **401 Unauthorized**: session missing/expired → route to login.
- **423 Locked**: crypto store locked; prompt user to unlock (via `/crypto/unlock`). Message text already mentions unlocking.
- **404 Not Found**: app name missing on get/start/stop/delete.
- **400 Bad Request**: YAML parse/validation errors; show message.
- **Port conflicts** during install surface as 500 with text about port in use; retry from UI is acceptable.
- **Status field** may lag Podman; initial install returns `status="created"` even though container is started; subsequent starts set `running`.

## Request Examples
```bash
# Validate YAML
curl -b cookies.txt -c cookies.txt -H "X-CSRF-Token: $CSRF" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @app.yaml \
  http://localhost:8080/api/v1/apps/validate

# Install
curl -b cookies.txt -c cookies.txt -H "X-CSRF-Token: $CSRF" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @app.yaml \
  http://localhost:8080/api/v1/apps

# Start
curl -b cookies.txt -c cookies.txt -H "X-CSRF-Token: $CSRF" \
  -X POST http://localhost:8080/api/v1/apps/myapp/start
```

## Feature Flags / Env that affect UX
- `PICCOLO_DEMO=1`: start/stop succeed without Podman; good for UI dev without containers.
- `PICCOLO_API_VALIDATE=1`: backend validates requests against OpenAPI (catches shape mismatches early).
- `PICCOLO_DISABLE_MDNS=1`: no effect on app store UI, but service discovery badges may be absent elsewhere.

## Suggested UI States
- **Catalog**: loading, list, empty, fetch-error.
- **Validation**: idle → validating → valid/invalid (surface message from 400).
- **Install**: idle → installing → success (show ports) / failed (show message).
- **App row**: states created/running/stopped/error; actions disabled when locked (423) or while pending.
- **Delete**: confirm dialog with optional “also delete data” mapped to `purge=true`.

## Out-of-Scope (not yet implemented server-side)
- App update/revert/logs endpoints; app update status list; backup/restore; SSO. Avoid wiring UI until backend ships them.

## Link Construction (“Open App” URLs)
Backend fields already exposed (via `/services` or `apps/{name}` detail):
- `scheme`: `http|https|ws|wss` derived from flow/protocol.
- `public_port`: listener port bound on `0.0.0.0` for LAN/local access.
- `remote_host`: FQDN when Nexus remote is enabled (blank otherwise).
- `remote_ports`: preferred remote ports (defaults `[80, 443]`).

Recommended client logic
1) Fetch `/api/v1/remote/status` once to know if remote is enabled and what `tld/portal_hostname` are.
2) For each service endpoint:
   - **Remote context**: if `remote_host` is non-empty, build `scheme://remote_host[:port]` where `port` is chosen from `remote_ports` (prefer 443/80 defaults; omit port when using 80 for http or 443 for https). Show as “Remote” link; still keep a LAN link as fallback.
   - **Local/LAN context**: use the current page host (`window.location.hostname`) with the endpoint’s `public_port`. URL = `scheme://{currentHost}:{public_port}` (omit port only if it matches the current page port and you’re intentionally reusing it—generally include the port because services bind in the 35k range).
   - Do **not** use `host_port` (loopback-only); `public_port` is the correct externally reachable port.
3) If the UI is accessed via `piccolo.local`, `piccolo-*.local`, LAN IP, or localhost, the above still works because `public_port` is always bound on 0.0.0.0.
4) If remote is enabled but `remote_host` is empty, fall back to local URL.

Possible backend improvement (optional): add `local_url` and `remote_url` precomputed fields to service responses. Today the UI can compute them with the above rules using existing fields.
