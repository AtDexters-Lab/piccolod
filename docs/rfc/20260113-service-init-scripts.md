# RFC: Service Init Scripts

- **Status:** Draft
- **Date:** 2026-01-13
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC introduces service-level init scripts: inline shell commands that run once inside a container on first install. Init scripts enable one-time setup tasks (database migrations, admin user creation, config generation) without custom entrypoint wrappers.

## 2. Motivation

**One-time setup:** Many apps require initialization steps that should run exactly once:
- Database schema migrations
- Admin user/password creation
- API key generation
- Initial configuration file creation

**No wrapper images:** Without init scripts, developers must build custom images that wrap the upstream entrypoint. This creates maintenance burden and blocks auto-updates.

**Template access:** Init scripts can use Piccolo template variables (inputs, system values) to configure the app with user-provided values.

## 3. Proposed Changes

### 3.1 Service-Level `init` Block

```yaml
services:
  main:
    image: nextcloud:latest
    bind_ports: [8080]
    init:
      env:
        ADMIN_USER: "{{ .Inputs.admin_user }}"
        ADMIN_PASS: "{{ .Inputs.admin_password }}"
      script: |
        php occ maintenance:install \
          --database sqlite \
          --admin-user "$ADMIN_USER" \
          --admin-pass "$ADMIN_PASS"
      timeout: 300s
```

### 3.2 Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `script` | Yes | - | Inline shell script |
| `env` | No | `{}` | Environment variables (template-evaluated) |
| `timeout` | No | `120s` | Maximum execution time |
| `ready_timeout` | No | `30s` | Time to wait after container starts before running script |
| `shell` | No | `/bin/sh` | Shell interpreter path |
| `user` | No | (image USER) | User to run script as |
| `workdir` | No | (image WORKDIR) | Working directory |

### 3.3 Execution Semantics

**When it runs:**
1. Container starts with its normal entrypoint
2. If healthcheck configured, wait for health check to pass (up to `ready_timeout`)
3. If no healthcheck, wait `ready_timeout` seconds
4. Init script executes via `podman exec` with `env` variables injected
5. On success, init is marked complete in app metadata
6. On failure, install fails

**Exactly-once guarantee:**
- Init completion is recorded in `AppMetadata.Init` field (see §7)
- Re-install (after uninstall) runs init again
- App updates do NOT re-run init
- Manual re-run available via API: `POST /api/v1/apps/{id}/reinit`

**Failure handling:**
- Non-zero exit code = init failure
- Timeout exceeded = init failure (SIGTERM, then SIGKILL after 10s grace period)
- Init failure = entire app install fails
- On failure, container is stopped and removed, volumes preserved for debugging

**Output capture:**
- stdout and stderr are captured to `$STATE_DIR/apps/{instanceID}/init-{service}.log`
- Logs are available via API: `GET /api/v1/apps/{id}/init-logs?service={name}`
- Logs are preserved on failure for debugging

### 3.4 Environment Variable Injection

Template values are injected as environment variables, NOT inline text substitution. This prevents shell injection vulnerabilities.

```yaml
init:
  env:
    DB_NAME: "{{ .Inputs.db_name }}"
    ADMIN_PASS: "{{ .Inputs.admin_password }}"
    API_KEY: "{{ .Secrets.api_key }}"
  script: |
    # Template values available as environment variables
    psql -c "CREATE DATABASE $DB_NAME;"
    set-admin-pass "$ADMIN_PASS"
    set-api-key "$API_KEY"
```

**How it works:**
1. At install time, manifest is parsed and `init.env` templates are evaluated
2. Values are stored in `AppMetadata.Init.Env` (encrypted in control volume)
3. At exec time, values are passed via `podman exec -e KEY=VALUE`
4. The `script` content is NOT template-evaluated

**Available template contexts in `init.env`:**
- `{{ .Inputs.<name> }}` — User-provided input values
- `{{ .System.Domain }}` — Portal domain
- `{{ .System.Architecture }}` — CPU architecture (amd64, arm64)
- `{{ .System.Auth.* }}` — OIDC values (if `oidc_client` declared)
- `{{ .Secrets.<name> }}` — Generated secrets (see §3.5)

### 3.5 Generated Secrets

For apps that need random credentials, declare `secrets` at app level:

```yaml
secrets:
  api_key:
    type: random
    length: 32
  db_password:
    type: password
    length: 24

services:
  main:
    environment:
      API_KEY: "{{ .Secrets.api_key }}"
    init:
      env:
        DB_PASS: "{{ .Secrets.db_password }}"
      script: |
        configure-db --password "$DB_PASS"
```

| Type | Description |
|------|-------------|
| `random` | Alphanumeric string (a-zA-Z0-9) |
| `password` | Alphanumeric + symbols (!@#$%^&*) |
| `hex` | Hexadecimal string (0-9a-f) |
| `base64` | Base64-encoded random bytes |

**Secrets vs inputs:**
- `secrets[]` — system-generated, NOT shown in UI, for internal use
- `inputs[].generate: true` — system-generated default, shown in UI, user can modify

**Storage:** Generated secrets are stored in `AppMetadata.Secrets` within the encrypted control volume (`$STATE_DIR/apps/{instanceID}/metadata.json`). They persist across updates.

### 3.6 Multi-Service Apps

Each service can have its own init script. Execution order follows `after` dependencies:

```yaml
services:
  db:
    image: postgres:16
    bind_ports: [5432]
    init:
      script: |
        until pg_isready; do sleep 1; done
        psql -c "CREATE DATABASE app;"
      timeout: 60s
      user: postgres

  web:
    image: myapp:latest
    bind_ports: [8080]
    after: [db]
    init:
      env:
        ADMIN_EMAIL: "{{ .Inputs.admin_email }}"
      script: |
        ./migrate up
        ./seed-admin "$ADMIN_EMAIL"
      timeout: 180s
```

**Execution order:**
1. All containers start (following `after` ordering)
2. Init scripts run in dependency order (db init before web init)
3. Any init failure stops remaining inits and fails install

## 4. Examples

### 4.1 Nextcloud Initial Setup

```yaml
name: nextcloud

inputs:
  admin_user:
    type: string
    label: "Admin Username"
    default: "admin"
  admin_password:
    type: password
    label: "Admin Password"
    generate: true

services:
  main:
    image: nextcloud:latest
    bind_ports: [80]
    init:
      env:
        ADMIN_USER: "{{ .Inputs.admin_user }}"
        ADMIN_PASS: "{{ .Inputs.admin_password }}"
      script: |
        php occ maintenance:install \
          --database sqlite \
          --admin-user "$ADMIN_USER" \
          --admin-pass "$ADMIN_PASS" \
          --admin-email "admin@localhost"
        php occ app:enable files_external
      timeout: 300s
      ready_timeout: 60s
      user: www-data
      workdir: /var/www/html
```

### 4.2 PostgreSQL Database Creation

```yaml
name: postgres

inputs:
  db_name:
    type: string
    label: "Database Name"
    default: "app"

secrets:
  db_password:
    type: password
    length: 24

services:
  main:
    image: postgres:16
    bind_ports: [5432]
    environment:
      POSTGRES_PASSWORD: "{{ .Secrets.db_password }}"
    init:
      env:
        DB_NAME: "{{ .Inputs.db_name }}"
      script: |
        until pg_isready; do sleep 1; done
        psql -U postgres -c "CREATE DATABASE $DB_NAME;"
      timeout: 60s
      user: postgres
```

### 4.3 Multi-Service with Dependencies

```yaml
name: myapp

secrets:
  jwt_secret:
    type: base64
    length: 32
  db_password:
    type: password
    length: 24

services:
  db:
    image: postgres:16
    bind_ports: [5432]
    environment:
      POSTGRES_PASSWORD: "{{ .Secrets.db_password }}"
    init:
      script: |
        until pg_isready; do sleep 1; done
        psql -U postgres -c "CREATE DATABASE myapp;"
      timeout: 60s
      user: postgres

  backend:
    image: myapp-backend:latest
    bind_ports: [8080]
    after: [db]
    environment:
      DATABASE_URL: "postgres://postgres:{{ .Secrets.db_password }}@db:5432/myapp"
      JWT_SECRET: "{{ .Secrets.jwt_secret }}"
    init:
      script: |
        ./manage.py migrate
        ./manage.py createsuperuser --noinput \
          --username admin \
          --email admin@localhost
      timeout: 180s
```

## 5. Schema

```yaml
# Service-level init block
services.<name>.init:
  script: string              # Required: inline shell script (NOT template-evaluated)
  env: map[string]string      # Optional: environment variables (template-evaluated)
  timeout: duration           # Optional: max execution time (default: 120s)
  ready_timeout: duration     # Optional: wait before running (default: 30s)
  shell: string               # Optional: shell path (default: /bin/sh)
  user: string                # Optional: run as user (default: image USER)
  workdir: string             # Optional: working directory (default: image WORKDIR)

# App-level secrets
secrets.<name>:
  type: random | password | hex | base64    # Required
  length: int                                # Required (8-256)
```

## 6. Validation Rules

1. `init.script` must not be empty if `init` block is declared
2. `init.script` must not exceed 64KB and must be valid UTF-8
3. `init.timeout` must be between `1s` and `3600s` (1 hour max)
4. `init.ready_timeout` must be between `0s` and `600s` (10 min max)
5. `init.shell` must be an absolute path
6. `init.env` keys must be valid environment variable names (`[A-Z_][A-Z0-9_]*`)
7. `secrets[].type` must be one of: `random`, `password`, `hex`, `base64`
8. `secrets[].length` must be between 8 and 256

## 7. State Management

Init status is recorded in `AppMetadata` (extends existing `internal/app/filesystem.go`):

```go
type AppMetadata struct {
    // ... existing fields ...

    Init    *InitState           `json:"init,omitempty"`
    Secrets map[string]string    `json:"secrets,omitempty"`
}

type InitState struct {
    Services map[string]ServiceInitState `json:"services"`
}

type ServiceInitState struct {
    Status      string    `json:"status"`  // pending | running | completed | failed
    StartedAt   time.Time `json:"started_at,omitempty"`
    CompletedAt time.Time `json:"completed_at,omitempty"`
    ExitCode    int       `json:"exit_code,omitempty"`
    Error       string    `json:"error,omitempty"`
}
```

**Status transitions:**
```
pending → running → completed
              ↓
           failed
```

### 7.1 Re-initialization

Manual re-init via API:

```
POST /api/v1/apps/{id}/reinit
Content-Type: application/json

{
  "services": ["db", "web"]  // Optional: specific services (default: all)
}
```

Response:
```json
{
  "status": "started",
  "services": ["db", "web"]
}
```

- Clears init status for specified services (or all if omitted)
- Restarts affected containers
- Re-runs init scripts
- Useful for recovery after init script bugs are fixed

### 7.2 Init Logs API

```
GET /api/v1/apps/{id}/init-logs?service={name}
```

Returns raw log content from `$STATE_DIR/apps/{instanceID}/init-{service}.log`.

## 8. Implementation Plan

1. **Parser:** Add `init` and `secrets` schema validation
2. **Secret Generation:** Implement generators for random/password/hex/base64
3. **Template Context:** Extend template evaluation to include `{{ .Secrets.* }}`
4. **Exec Runner:** Implement non-interactive `ExecScript()` in `podman.go`:
   ```go
   func (p *PodmanCLI) ExecScript(ctx context.Context, runtime PodmanRuntime,
       containerID string, opts ExecScriptOptions) (exitCode int, stdout, stderr string, err error)
   ```
5. **State Tracking:** Extend `AppMetadata` with `Init` and `Secrets` fields
6. **Install Flow:** Modify `installContainerGroup()` to run init after container start
7. **API:** Add `/reinit` and `/init-logs` endpoints
8. **Tests:** Template evaluation, exec behavior, failure handling, ordering

## 9. Security Considerations

- **No shell injection:** Template values are passed as environment variables via `podman exec -e`, NOT inline substituted into scripts
- **Secret storage:** Generated secrets stored in encrypted control volume only
- **Execution context:** Scripts run inside container with container's permissions
- **Timeout enforcement:** SIGTERM followed by SIGKILL after 10s grace period
- **Script validation:** Size limit (64KB) and UTF-8 validation prevent malformed input
- **Environment key validation:** Only valid env var names accepted

## 10. Future Enhancements

### 10.1 Pre/Post Hooks

Extend to support lifecycle hooks beyond init:

```yaml
services:
  main:
    hooks:
      pre_start: |
        echo "Before container starts"
      post_start: |
        echo "After container starts (current init)"
      pre_stop: |
        echo "Before container stops (graceful shutdown)"
```

### 10.2 Update Migrations

Scripts that run on app updates (not just first install):

```yaml
services:
  main:
    migrations:
      - version: "2.0"
        script: |
          ./migrate-v2.sh
```

## 11. Implementation Notes & Status

- **Status:** Draft
- **Depends on:** Multi-Container Apps (RFC 20260102), app.yaml template system
