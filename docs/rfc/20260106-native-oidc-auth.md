# RFC: Native OIDC Provider & Family User Model

- **Status:** Draft
- **Date:** 2026-01-06
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC proposes integrating a minimal OpenID Connect (OIDC) Provider directly into `piccolod` to enable seamless Single Sign-On (SSO) for installed applications. It introduces a "Family" user model (1 Admin + N Standard Users) and solves the "Split-Horizon" access problem (LAN vs. Remote) via a "Stable Issuer, Dynamic Discovery" strategy.

## 2. Motivation

- **Seamless SSO:** Users should log in once to the Piccolo dashboard and access apps (Immich, Nextcloud, Gitea) without re-authenticating.
- **Unified UX:** User management occurs in OS settings, not separate identity apps.
- **Family Mode:** Support standard users (peers/family) restricted to specific apps.
- **Hybrid Access:** Support authentication flows regardless of whether the user is on LAN (Offline) or WAN (Remote), despite strict OIDC issuer requirements.

## 3. Architecture

### 3.1 The "Stable Issuer" Strategy
To satisfy OIDC strictness while allowing variable access origins:

1.  **Issuer Identity:** The Issuer is **always** `piccolo.local` (scheme configurable).
    *   Apps are configured with `ISSUER_URL=https://piccolo.local` (preferred) or `http://piccolo.local` (insecure / best-effort compatibility).
    *   Tokens are signed with `iss: <configured_issuer>`.
    *   **Multi-NIC:** `piccolo.local` is advertised via mDNS on all NICs; v1 does not embed raw LAN IPs into discovery.

2.  **Internal Resolution (The "Back-Channel"):**
    *   `piccolod` exposes an internal issuer origin reachable **from app containers** (even when the user is remote).
        *   **HTTPS issuer:** listener bound on a host address reachable from containers via `<host-gateway-ip>:443` with a self-signed cert.
        *   **HTTP issuer:** uses the normal portal HTTP listener (same host/port as `http://piccolo.local`).
    *   **Host Gateway Discovery (no hard-coded IP):**
        *   `piccolod` determines the "host gateway" IP address as seen from containers (runtime-dependent).
        *   In rootless Podman, this is discovered by resolving `host.containers.internal` from a helper container.
    *   **Container Networking:** `piccolod` injects `--add-host piccolo.local:<host-gateway-ip>` into every app container.
    *   **Trust (HTTPS):** `piccolod` mounts its internal CA certificate to `/var/lib/piccolo/certs/internal-ca.crt` in every container.
        *   `app.yaml` templates are updated to set `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, or `REQUESTS_CA_BUNDLE` to this path.
    *   **Trust (HTTP):** If app allows, `issuer_protocol: http` can be used to bypass TLS complexity.

3.  **Dynamic Discovery (The "Front-Channel"):**
    `piccolod` serves `/.well-known/openid-configuration`. The content is generated dynamically based on the **Best Reachable Origin**:
    *   **Remote Active:** `authorization_endpoint` = `https://<portal-hostname>/oauth/authorize`
    *   **Remote Inactive (Local Mode):** `authorization_endpoint` = `http://piccolo.local/oauth/authorize`
    *   **All Other Endpoints:** (`token`, `jwks`, etc.) remain internal.

4.  **Cache Busting:**
    *   The Discovery Endpoint serves `Cache-Control: no-store`.
    *   `piccolod` monitors Remote Tunnel status. On state change (debounced), it **restarts** all OIDC-enabled containers to flush their startup discovery cache.
        *   **Leader-only:** Restarts run only when the node is `kernel=leader`. In `kernel=follower` mode, apps are typically stopped; the restart hook is a no-op to preserve leader/follower invariants.

### 3.2 Endpoints & Reachability Matrix (v1)
The key invariant is that the **issuer stays stable** for apps, while the **front-channel** endpoint used by the browser may switch between local and remote origins.

| Endpoint | Used By | Local (LAN) | Remote (WAN) | Notes |
|---|---|---|---|---|
| `GET /.well-known/openid-configuration` | App container | via issuer origin | via issuer origin | Returns a dynamic `authorization_endpoint`. |
| `GET /oauth/authorize` | Browser | `http://piccolo.local/oauth/authorize` | `https://<portal-hostname>/oauth/authorize` | Interactive login + consent; enforces `allowed_apps`. |
| `POST /oauth/token` | App container | via issuer origin | via issuer origin | Back-channel; exchanges code/refresh for tokens. |
| `GET /oauth/jwks` | App container | via issuer origin | via issuer origin | Publishes signing keys (`kid`-versioned). |
| `GET /oauth/userinfo` | App container (optional) | via issuer origin | via issuer origin | Optional; prefer ID token claims when possible. |

### 3.3 User Model
*   **Admin (Root):** Single user. Full system access. Can unlock device.
*   **Standard:** Created by Admin. Restricted to `allowed_apps`. Cannot unlock. Cannot access system settings.
*   **Storage:** Persisted in `control.db` (SQLite).

## 4. Application Integration (`app.yaml`)
Apps use the templating system to receive credentials. `piccolod` acts as the Dynamic Client Registrar.

```yaml
auth:
  strategy: oidc # or 'header'
  injection:
    # Protocol preference for internal back-channel (default: https)
    # Use 'http' if app allows insecure issuer to avoid CA trust issues.
    issuer_protocol: https 
    
    # Custom mount path for CA cert if standard env vars don't work
    ca_mount_path: "/etc/ssl/certs/piccolo-ca.crt"

    env:
      ISSUER_URL: "{{ .Auth.Issuer }}" # https://piccolo.local or http://piccolo.local
      CLIENT_ID: "{{ .Auth.ClientID }}"
      CLIENT_SECRET: "{{ .Auth.ClientSecret }}"
```

### 4.1 Redirect URI Validation (v1)
For simplicity and compatibility across apps, v1 does **not** store per-app redirect URIs in the database. Instead, `piccolod` validates redirect URIs dynamically during the authorization flow:

- `client_id` is mapped to `app_id` via `oidc_clients`.
- `redirect_uri` is accepted only if its **origin** matches a currently-registered listener for that app.
  - **Local (LAN):** `http://piccolo.local:<public-port>/...`
  - **Remote (WAN):** `https://<listener>.<portal-hostname>/...`
- Path and query are app-defined (v1 does not enforce exact-path matching).

## 5. Authentication Strategies

### 5.1 Native OIDC (Preferred)
*   **Target:** Modern apps (Immich, Nextcloud, Gitea).
*   **Mechanism:** Standard OIDC Authorization Code Flow.
*   **Enforcement:** `piccolod` enforces `allowed_apps` at both authorization and token issuance (including refresh).

### 5.2 Trusted Headers (Legacy)
*   **Target:** Apps without OIDC support (Vaultwarden, FileBrowser).
*   **Mechanism:** The `piccolod` reverse proxy validates the user session and injects:
    *   `X-Piccolo-User`: Username
    *   `X-Piccolo-Email`: Email
    *   `X-Piccolo-Name`: Display Name
*   **Security Requirements:**
    *   **Strip Headers:** The Proxy **MUST** strip these headers from all incoming traffic on WAN/LAN interfaces.
    *   **Loopback Binding:** The app container **MUST** bind its listener only to `127.0.0.1` or the internal bridge network to prevent bypassing the proxy.
    *   **Enforcement:** The Proxy Middleware checks `allowed_apps` before forwarding the request.

## 6. Token & Key Management (v1)
### 6.1 Token profile
- **Signing:** JWTs with a persisted signing key (`kid` published via JWKS).
- **Claims (minimum):** `iss`, `sub`, `aud`, `exp`, `iat`, plus standard user claims (`preferred_username`, `email`, `name`) where available.
- **Lifetimes:** Short-lived access/id tokens; refresh tokens optional (only if required by app).

### 6.2 Keys
- Signing keys are generated on first use and persisted in `control.db`.
- Rotation is supported by publishing multiple keys in JWKS (current + previous) for a defined overlap window.

## 7. Storage Schema

```sql
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT CHECK(role IN ('admin', 'standard')),
    allowed_apps TEXT, -- JSON array
    created_at TEXT,
    updated_at TEXT
);

CREATE TABLE oidc_clients (
    id TEXT PRIMARY KEY,
    secret TEXT NOT NULL,
    app_id TEXT NOT NULL,
    created_at TEXT
    -- Redirect URIs checked dynamically at runtime
);

CREATE TABLE oidc_keys (
    kid TEXT PRIMARY KEY,
    alg TEXT NOT NULL,
    private_key BLOB NOT NULL, -- stored inside encrypted control volume
    created_at TEXT NOT NULL,
    retired_at TEXT
);
```

## 8. Implementation Plan

1.  **Core Auth:** Refactor `internal/auth` to support Multi-User + SQLite Persistence.
2.  **OIDC Engine:** Implement `op.Storage` and Dynamic Discovery Handler.
3.  **Networking:**
    *   Implement host-gateway discovery + `--add-host piccolo.local:<host-gateway-ip>` injection.
    *   Implement Internal HTTPS Listener (host loopback `127.0.0.1:443`) and optional HTTP mode.
    *   Implement CA Generation & Volume Mounting.
4.  **Orchestration:** Implement Remote State Monitor & App Restart Logic.
5.  **UI:** Add User Management screens.

## 9. Risks & Compatibility
*   **Split Origins:** Some apps may strictly require `issuer` host == `authorization_endpoint` host.
*   **Persistence:** Apps that persist discovery config to disk are incompatible with Dynamic Remote switching.
*   **HTTP Local AuthZ:** Some clients may refuse an `http://` `authorization_endpoint` even when the issuer is `https://`.

## 10. Rollout & Migration (Breaking)
- This feature is introduced as a new auth surface; backward compatibility for existing auth/app configurations is **not** a goal for v1.
- Expect app template updates and restarts during rollout; existing user sessions may be invalidated during migration.

## 11. Implementation Notes & Status
- **Status:** Draft (not implemented)
- **Notes:** TBD (add PR/commit references and follow-ups as work lands)
