# RFC: Listener Auth Rules & Service-Level OIDC Client

- **Status:** Draft
- **Date:** 2026-01-12
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC introduces:
1. **Listener-level auth rules:** Path-based access control with four strategies (`oidc_passthrough`, `headers`, `protected`, `public`).
2. **Service-level `oidc_client`:** OIDC credential injection scoped to individual services.

These changes improve security (least privilege), provide flexibility (per-path policies), and clarify the relationship between Piccolo authentication and app authorization.

**Related RFC:** LAN/Remote hostname scheme and optional LAN host-based routing are proposed in `docs/rfc/20260114-hostname-scheme-routing.md`. That RFC can eliminate the need for LAN cookie rewriting by giving each app its own hostname. This RFC specifies auth behavior independent of routing mode.

## 2. Motivation

**Path-level granularity:** Apps need mixed access policies — webhooks public, admin areas protected. Without path rules, developers lose either Piccolo's access control or app functionality.

**Clear auth model:** Separate concerns between:
- **Piccolo:** Authentication (who are you?) + access control (can you use this app?)
- **App:** Authorization (what can you do inside?)

**Service-scoped credentials:** Only inject OIDC secrets into services that need them.

## 3. Auth Model

### 3.1 Piccolo's Role

Piccolo authenticates **only Piccolo users** (Admin + Standard users in database):

| User Type | Can Authenticate? | `allowed_apps` Applies? |
|-----------|------------------|------------------------|
| Admin | Yes | No (full access) |
| Standard | Yes | Yes |
| External/Guest | No | N/A |

External sharing (friends, public links) is handled by apps, not Piccolo OIDC.

### 3.2 Strategies

| Strategy | Proxy Behavior | Identity | `allowed_apps` |
|----------|---------------|----------|----------------|
| `oidc_passthrough` | Pass-through | App handles via OIDC flow | At OIDC server |
| `headers` | Session check | X-Piccolo-* injected | At proxy |
| `protected` | Session check | None | At proxy |
| `public` | Pass-through | None | No |

**Note:** For `oidc_passthrough` strategy, `allowed_apps` is checked at authorization and token refresh. Existing valid tokens remain usable until expiration.

> **⚠️ Warning: `oidc_passthrough` requires app-side enforcement.**
> The `oidc_passthrough` strategy is pass-through at the proxy level — it does **not** block unauthenticated requests. Protection depends entirely on the app correctly implementing the OIDC flow and validating tokens. Misconfigured apps (e.g., OIDC disabled, optional auth) will be publicly accessible.
>
> **Validation:** If any auth rule uses `oidc_passthrough`, at least one service MUST declare `oidc_client`. This validation is a **guardrail, not a guarantee** — it prevents the most obvious misconfiguration (no OIDC credentials at all) but cannot verify that the app actually enforces authentication. App authors must ensure their apps correctly implement the OIDC flow. Apps without proper OIDC integration should use `protected` or `headers` strategy instead.

### 3.3 When to Use Each Strategy

```
App needs auth?
├─ YES
│  └─ App supports auth mechanism?
│     ├─ OIDC → strategy: oidc_passthrough
│     │  App redirects to Piccolo OIDC, manages session, validates tokens
│     ├─ Headers → strategy: headers
│     │  App trusts X-Piccolo-* headers from proxy
│     └─ None → strategy: protected
│        Piccolo gates access, but app has no user identity
└─ NO → strategy: public
```

## 4. Proposed Changes

### 4.1 Listener Auth Rules

```yaml
listeners:
  - name: web
    guest_port: 8080
    flow: tcp          # Required: auth rules require HTTP visibility
    protocol: http
    auth:
      rules:
        - path: "/admin/"
          type: prefix
          strategy: protected
        - path: "/"
          type: prefix
          strategy: oidc_passthrough
```

#### 4.1.1 Evaluation Algorithm

1. If `auth` block omitted → all paths `protected`
2. If `auth` block present but `rules` is empty → all paths `protected`
3. If rules present → evaluate in order, first match wins:
   - `type: exact` — match if request path equals rule path exactly
   - `type: prefix` — match if request path starts with rule path (element-wise, split by `/`)
   - `type: pattern` — match if `path.Match(rule.path, requestPath)` returns true
4. No match → `protected`

> **Note:** There are no implicit public paths. Apps requiring ACME HTTP-01 challenges or other `/.well-known` endpoints must explicitly declare them:
> ```yaml
> auth:
>   rules:
>     - path: "/.well-known/acme-challenge/"
>       type: prefix
>       strategy: public
>     - path: "/"
>       type: prefix
>       strategy: protected
> ```

#### 4.1.2 Path Matching

Each rule specifies a `type` that determines how matching is performed:

| Type | Behavior | Use Case |
|------|----------|----------|
| `exact` | Byte-for-byte match | Single endpoints: `/health`, `/api/ping` |
| `prefix` | Path-element prefix match | Directory trees: `/api/`, `/admin/` |
| `pattern` | Go `path.Match` wildcards | Extension/version matching: `/static/*.js` |

**Match type semantics:**

| Type | Path | Matches | Does Not Match |
|------|------|---------|----------------|
| `exact` | `/health` | `/health` | `/health/`, `/healthz` |
| `exact` | `/health/` | `/health/` | `/health` |
| `prefix` | `/api/` | `/api/`, `/api/users`, `/api/users/123` | `/api`, `/apikeys` |
| `prefix` | `/` | All paths | *(none)* |
| `pattern` | `/static/*.js` | `/static/app.js` | `/static/css/app.css` |
| `pattern` | `/api/v?/users` | `/api/v1/users`, `/api/v2/users` | `/api/v10/users` |

**Type-specific rules:**

- **`exact`**: Strict byte-for-byte comparison. `/health` ≠ `/health/`.
- **`prefix`**: Element-wise matching (split by `/`). Path `/api/` matches `/api/users` but NOT `/apikeys`. Prefix paths MUST end with `/`.
- **`pattern`**: Uses Go's `path.Match` from the standard library:
  - `*` matches any sequence of non-`/` characters
  - `?` matches any single non-`/` character
  - `[abc]` matches any character in the set
  - `[a-z]` matches any character in the range
  - No `**` support — use `prefix` for recursive matching

**Test conformance:** Implementation MUST include tests validating each example in the table above. A conformance test suite will be added at `internal/services/auth_path_test.go`.

**Path handling (Go-specific):**

Go's `net/http` already decodes URLs when parsing requests. The proxy processes paths as follows:

**Step 1: Reject dangerous patterns**
- Parse the path portion from `r.RequestURI` (the raw request target, before Go URL parsing)
- Reject if path contains encoded separators (case-insensitive check): `%2F`, `%5C`
- Reject if path contains `%25` (percent-encoded percent sign)
  - This blanket rule blocks ALL double-encoding (`%252e` → dot, `%252f` → slash, etc.)
  - Rationale: Legitimate paths rarely need literal `%` characters; double-encoding is a common bypass technique where backends decode twice, causing proxy/backend interpretation splits
  - Note: `r.URL.RawPath` is often empty unless the path required encoding; `RequestURI` is always populated
- Reject if `r.URL.Path` contains raw backslash `\` (some stacks treat as separator)
- Return 400 Bad Request

**Step 2: Detect root traversal**
Walk path segments and track depth. Reject if depth goes negative:
```
/foo/../bar     → depth: 1, 0, 1 → OK (clean to /bar)
/foo/../../bar  → depth: 1, 0, -1 → REJECT 400 (traversal above root)
/../admin       → depth: -1 → REJECT 400
```

**Step 3: Clean path (preserving trailing slash)**
1. Save whether path ends with `/`
2. Apply `path.Clean()` to resolve `.` and `..`, collapse `//`
3. Restore trailing `/` if original had one (except root `/` stays as `/`)

**Step 4: Forward cleaned path**
The cleaned path is used for both rule matching AND forwarding to backend.

**Critical invariant:** The path matched against rules MUST be identical to the path forwarded to the backend.

> **⚠️ Compatibility Note:**
> Path cleaning (`path.Clean()`) normalizes paths that some apps may rely on in non-standard ways:
> - Double slashes (`//`) are collapsed to single slashes
> - Dot segments (`/.` and `/..`) are resolved
>
> Apps that use these patterns meaningfully (though incorrect per RFC 3986) may break. App authors should:
> - Test their apps against cleaned paths
> - Avoid relying on non-canonical path representations
> - Update app routing to handle canonical paths only

| Request | Action |
|---------|--------|
| `/api/users` | Match `/api/users` |
| `/api/users/` | Match `/api/users/` (trailing slash preserved) |
| `/api/%75sers` | Match `/api/users` (Go decoded `%75`→`u`) |
| `/api/%2Fusers` | **Reject 400** (encoded separator) |
| `/foo%252e%252e/bar` | **Reject 400** (double-encoding: `%25` present) |
| `/api\users` | **Reject 400** (raw backslash) |
| `/foo/../admin` | Clean → `/admin`, match `/admin` |
| `/foo/../../x` | **Reject 400** (traversal above root) |

**Notes:**
- Trailing slashes are significant: `/api` ≠ `/api/`.
- Paths are case-sensitive.
- Malformed percent-encoding (e.g., `%ZZ`) is rejected by Go's parser before reaching the handler.

#### 4.1.3 Supported Protocols

Auth rules require HTTP visibility. This means:

- `flow: tcp` + `protocol: http` — **Supported.** Piccolo terminates TLS, inspects HTTP.
- `flow: tcp` + `protocol: websocket` — **Supported.** Rules evaluated at HTTP upgrade request only.

  **WebSocket Security Implications:**
  - Auth rules are checked **once** during the HTTP upgrade handshake
  - After upgrade, the connection is a persistent bidirectional channel with no further auth checks
  - Session expiration or `allowed_apps` revocation during an active WebSocket connection has **no effect** until reconnection
  - Apps requiring session-aware WebSocket behavior must implement application-level heartbeat/revalidation
  - For security-sensitive apps, consider shorter WebSocket connection timeouts or app-level token refresh over the WebSocket channel
- `flow: tls` — **Not supported.** Piccolo passes through encrypted traffic, cannot inspect HTTP. Auth block is rejected.
- `protocol: raw` — **Not supported.** No HTTP to inspect. Auth block is rejected.

**Validation:** Parser rejects `auth` block on listeners with `flow: tls` or `protocol: raw`.

#### 4.1.4 Response Behavior

Response behavior varies by strategy:

**For `protected` and `headers` strategies (proxy-enforced):**

| Scenario | Response |
|----------|----------|
| Unauthenticated, browser navigation | 302 redirect to login |
| Unauthenticated, API client | 401 `{"error": "authentication_required"}` |
| Authenticated but not in `allowed_apps` | 403 (redirect to "access denied" page for browsers, JSON for API) |

**For `oidc_passthrough` strategy (app-driven):**

The proxy passes requests through without authentication checks. The app is responsible for:
- Redirecting unauthenticated users to Piccolo's OIDC authorization endpoint
- Validating tokens and managing sessions
- Enforcing access control based on token claims

Note: `allowed_apps` is enforced at the OIDC server during authorization and token refresh (see Section 4.1.4.1).

**For `public` strategy:**

All requests pass through without any authentication checks.

**Browser detection:** Use a two-tier approach:

1. **Prefer `Sec-Fetch-*` headers** (modern browsers): If `Sec-Fetch-Mode: navigate` AND `Sec-Fetch-Dest: document` → browser navigation
2. **Fallback heuristics** (older browsers, curl, etc.): If Sec-Fetch headers absent, redirect (302) when ALL conditions are met:
   - Request method is GET
   - `Accept` header contains `text/html`
   - No `X-Requested-With` header (filters out XHR/fetch)

Otherwise, return JSON error response.

##### 4.1.4.1 `allowed_apps` Enforcement for OIDC Strategy

For `oidc_passthrough` strategy, `allowed_apps` is enforced at the OIDC server, not the proxy:

| Enforcement Point | Behavior |
|-------------------|----------|
| Authorization request | User without app in `allowed_apps` receives 403 at `/oauth/authorize` |
| Token refresh | Refresh rejected if app removed from `allowed_apps` since last auth |
| Existing tokens | **Remain valid until expiration** — revocation is eventual, not immediate |

**Implications:**
- Access revocation latency = remaining access token lifetime (default: 15 minutes)
- For immediate revocation, administrators should use token revocation endpoint or rely on short token lifetimes
- Apps SHOULD use short-lived access tokens (≤15 minutes) and refresh frequently for timely `allowed_apps` enforcement

**Token Lifetimes (Defaults):**

| Token Type | Default TTL | Rationale |
|------------|-------------|-----------|
| Access token | 15 minutes | Balance between UX (fewer refreshes) and revocation latency |
| Refresh token | 7 days | Allows reasonable session duration without re-authentication |

**Revocation and Introspection:**

Piccolo's OIDC server supports immediate token invalidation:

| Endpoint | Purpose |
|----------|---------|
| `/oauth/revoke` | Immediate token revocation (RFC 7009) |
| `/oauth/introspect` | Token validation for resource servers (RFC 7662) |

Apps requiring immediate access revocation should either:
1. Use token introspection to validate tokens on each request, or
2. Rely on the short access token TTL (15 minutes maximum revocation delay)

**Login URL format:** `<portal-origin>/login?next=<url-encoded-original-absolute-uri>`

**Portal Origin Resolution:**
Piccolo operates in dynamic access modes (LAN vs WAN). The redirect target MUST preserve the user's access context. Resolution uses multiple sources (consistent with existing OIDC discovery and redirect URI validation):

| Access Context | Detection Method | Portal Origin |
|----------------|------------------|---------------|
| LAN via mDNS | `mdnsManager.Hostname()` returns `piccolo.local` | `http://piccolo.local` |
| LAN via custom mDNS | `mdnsManager.Hostname()` returns `piccolo-xyz.local` | `http://piccolo-xyz.local` |
| LAN via IP | `isLocalMachineIP()` validates request IP | `http://<request-host-ip>` |
| WAN via subdomain | `remoteManager.Status().PortalHostname` | `https://<portal-hostname>` |

**Resolution algorithm:**
1. **Detect access mode:** Check if `RemoteAddr` is loopback (127.0.0.1/::1) — if so, request came via Nexus proxy (WAN).
2. **WAN path:** If WAN and `remoteManager.Status().Enabled`:
   - Use `https://<remoteManager.Status().PortalHostname>`.
3. **LAN path:** Otherwise:
   - Prefer `mdnsManager.Hostname()` if available (handles mDNS conflicts).
   - Fallback: Use request's `Host` header or `getPreferredOutboundIP` to redirect to machine IP.
   - Scheme: `http` for LAN (or `https` if TLS terminated locally).
4. **Construct:** `<scheme>://<portal-host>`.

**`next` Parameter Validation:**

The `next` parameter is validated against an allowlist to prevent open redirects:

**Allowed values:**

| Type | Format | Example |
|------|--------|---------|
| Relative path | Starts with `/`, no scheme/host | `/dashboard`, `/app/settings` |
| Portal origin | Full URI to portal hostname | `https://piccolo-xyz.example.com/...` |
| App hostname (remote) | Full URI to an app hostname under the remote base domain | `https://immich.piccolo-xyz.example.com/...`, `https://metrics-immich.piccolo-xyz.example.com/...` |
| LAN app hostname | Full URI to an app hostname under the LAN base hostname (mDNS) | `http://immich.piccolo.local/...`, `http://metrics-immich.piccolo.local/...` |
| LAN port-based origin (legacy) | Full URI to mDNS hostname/IP + listener port | `http://piccolo.local:35080/...`, `http://192.168.1.50:35080/...` |
| Alias domain | Full URI to configured alias domain | `https://myblog.com/...` |

**Validation algorithm:**
1. Parse `next` as URL
2. If scheme is empty (relative path):
   - Must start with `/`
   - Reject if starts with `//` (protocol-relative URL)
   - Reject if contains `..` path traversal
3. If scheme is present (absolute URI):
   - Host must match one of: portal hostname, app hostnames (derived from current listener config and hostname scheme), LAN port-based origins (from listener config), alias domains (from remote config)
   - Reject any host not in the dynamic allowlist

**Allowlist is dynamic:** Derived at validation time from current listener configuration and remote config. Changes to listeners or alias domains take effect immediately.

#### 4.1.5 Header & Cookie Handling

**Piccolo Cookie Names:**
The following cookies are reserved and managed by Piccolo:
- `piccolo_session`
- `piccolo_oidc_state`
- `piccolo_nonce`

**Request Handling (Incoming from Client):**
- **Header Stripping:** Strip all `X-Piccolo-*` headers (prevent spoofing).
- **Cookie Stripping:** Strip all "Piccolo Cookie Names" from the `Cookie` header before forwarding to apps.
- **Forwarded Headers:** Set standard forwarded headers per RFC 7239 with trust boundary awareness:

  **Trust boundary detection:**

  | Source IP | Trust Status | Action |
  |-----------|--------------|--------|
  | Loopback (127.0.0.1/::1) | Trusted | Preserve existing forwarded headers |
  | Non-loopback | Untrusted | Overwrite forwarded headers |

  **Header handling by trust status:**

  | Header | Trusted (Loopback) | Untrusted (LAN Client) |
  |--------|-------------------|------------------------|
  | `X-Forwarded-For` | Append client IP | Overwrite with client IP |
  | `X-Forwarded-Proto` | Preserve | Overwrite |
  | `X-Forwarded-Host` | Preserve | Overwrite |
  | `X-Forwarded-Port` | Preserve | Overwrite |
  | `X-Real-IP` | Preserve | Overwrite |
  | `Forwarded` | Append | Overwrite |

  > **Security Note:** This trust model assumes local process spoofing is out-of-scope. A malicious process with local code execution could connect via loopback and inject fake forwarded headers. This is acceptable because:
  > - Local code execution implies the system is already compromised
  > - Nexus remote proxy is the only expected loopback client in production
  > - If stronger isolation is needed in future, HMAC-signed headers using the existing `device_secret` can be added
- **`Authorization` Header Handling:**

  | Strategy | `Authorization` Header Behavior |
  |----------|--------------------------------|
  | `public` | Pass through unmodified |
  | `oidc_passthrough` | Pass through unmodified (app validates its own tokens) |
  | `headers` | Pass through unmodified (app may use for API keys, etc.) |
  | `protected` | Pass through unmodified |

  Note: Piccolo does not inspect or validate `Authorization` headers for any strategy. Apps using `oidc_passthrough` strategy receive tokens they requested from Piccolo's OIDC endpoints and validate them independently.

**Response Handling (Outgoing from App):**
- **Set-Cookie Blocking:** Strip `Set-Cookie` headers that:
  - Attempt to set any "Piccolo Cookie Names" (prevent session fixation/clobbering)
  - Include `Domain=` attribute that doesn't match the app's own host exactly

  | `Set-Cookie` Header (app at `immich.piccolo-xyz.example.com`) | Action |
  |--------------------------------------------------|--------|
  | `session=abc; Path=/` | Pass through (host-only cookie) |
  | `piccolo_session=xyz` | **Strip** (reserved name) |
  | `token=xyz; Domain=immich.piccolo-xyz.example.com` | Pass through (matches app host) |
  | `token=xyz; Domain=piccolo-xyz.example.com` | **Strip** (doesn't match app host) |
  | `token=xyz; Domain=example.com` | **Strip** (doesn't match app host) |
  | `token=xyz; Domain=other.piccolo-xyz.example.com` | **Strip** (doesn't match app host) |

  **Host normalization for Domain comparison:**
  1. Lowercase both Domain attribute value and app host
  2. Strip leading `.` from Domain (`.piccolo-xyz.example.com` → `piccolo-xyz.example.com`)
  3. Strip port from app host for comparison (cookies are port-agnostic)
  4. Compare normalized strings for exact match

  **"App host" in each access context:**
  | Context | App Host |
  |---------|----------|
  | LAN (host-based) | `<app>.<local-host>` or `<listener>-<app>.<local-host>` (e.g., `immich.piccolo.local`) |
  | LAN (port-based legacy) | `piccolo.local` (shared across apps; cookie isolation requires additional measures) |
  | WAN | `<app>.<remote-base>` or `<listener>-<app>.<remote-base>` (e.g., `immich.piccolo-xyz.example.com`) |
  | Alias | The alias domain (e.g., `myblog.com`) |

  This simple rule prevents apps from setting cookies that affect other apps or the portal **when apps are accessed via distinct hostnames** (WAN and LAN host-based routing). In port-based LAN mode, multiple apps share `piccolo.local`, so `Domain` filtering alone cannot prevent cross-app scoping; use hostname-based routing (preferred) or the optional cookie isolation mechanism in Section 4.1.8.

#### 4.1.6 Strategy Implementation Details

Each strategy requires specific proxy middleware behavior:

**`public` strategy:**
```
Request → Strip X-Piccolo-* headers → Forward to app
```
- No session validation
- No `allowed_apps` check
- Headers/cookies passed through (except X-Piccolo-* stripped for security)

**`protected` strategy:**
```
Request → Strip X-Piccolo-* headers → Validate session → Check allowed_apps → Forward to app
```
- Validate `piccolo_session` cookie against session store
- If invalid/missing: return 401 (or 302 redirect for browsers)
- If valid: check user's `allowed_apps` (skip for admin role)
- If app not allowed: return 403
- If allowed: forward request **without** injecting identity headers
- App receives no user identity information — only access gating

**`headers` strategy:**
```
Request → Strip X-Piccolo-* headers → Validate session → Check allowed_apps → Inject headers → Forward to app
```
- Same validation as `protected`
- Additionally inject identity headers before forwarding:
  - `X-Piccolo-User`: username
  - `X-Piccolo-Email`: email
  - `X-Piccolo-Name`: display name
  - `X-Piccolo-Role`: `admin` or `standard`

**`oidc_passthrough` strategy:**
```
Request → Strip X-Piccolo-* headers → Forward to app
```
- No proxy-level session validation
- App handles authentication via OIDC flow with Piccolo as IdP
- `allowed_apps` enforced at OIDC authorization endpoint (see Section 4.1.4.1)

**Implementation:** The `protected` strategy reuses `TrustedHeadersMiddleware` logic but skips the header injection step. This can be implemented as a configuration flag on the existing middleware or as a separate `ProtectedMiddleware` that shares validation code.

#### 4.1.7 Session Cookie Scope

Piccolo session cookies work across app listeners without special configuration:

- **LAN (host-based, preferred):** Apps are on per-app hostnames (e.g., `immich.piccolo.local`). Piccolo session cookies SHOULD be set with `Domain=<local-host>` (e.g., `piccolo.local`) so portal and apps share SSO within LAN context.
- **LAN (port-based legacy):** Apps are `piccolo.local:<port>` — cookies are port-agnostic and therefore shared across apps (see Section 4.1.8).
- **Remote:** Portal is served from `remoteManager.Status().PortalHostname` (e.g., `piccolo-xyz.example.com`) and apps are per-app hostnames under that base (e.g., `immich.piccolo-xyz.example.com`). Piccolo session cookies SHOULD be set with `Domain=<remote-base>` (e.g., `piccolo-xyz.example.com`) so portal and apps share SSO within Remote context.

Users authenticate separately for LAN and Remote access (separate sessions).

#### 4.1.8 LAN Cookie Isolation (Port-Based Legacy)

**Preferred:** Hostname-based routing for LAN HTTP/WebSocket listeners (see `docs/rfc/20260114-hostname-scheme-routing.md`). With per-app hostnames, browsers naturally isolate cookies by hostname and Piccolo does not need to rewrite app cookies.

**Problem (legacy mode):** In LAN port-based mode, all apps share the same hostname (`piccolo.local`) with different ports. Since cookies are port-agnostic, app cookies leak across apps — causing breakage if cookie names collide and potential security issues.

**Optional mitigation (legacy mode):** Proxy-level cookie rewriting with app-specific prefixes.

To minimize compatibility issues, Piccolo SHOULD default to rewriting **only `HttpOnly` cookies** (typically server-side session cookies). This preserves common client-side patterns that read cookies in the browser (e.g., CSRF token cookies), while still isolating the most sensitive cookies.

**Mode A (recommended default): Rewrite `HttpOnly` cookies only**

**Request path (incoming cookies):**
1. Strip Piccolo reserved cookies (`piccolo_session`, etc.) from the `Cookie` header before forwarding (handled separately).
2. For each cookie name in `Cookie`, if it has prefix `__piccolo_<app-id>_`:
   - If prefix matches current app: strip prefix and forward the cookie.
   - Otherwise: drop cookie (belongs to different app).
3. Forward all **non-prefixed** cookies unchanged (legacy behavior).

**Response path (outgoing Set-Cookie):**
1. For each `Set-Cookie` header from app, parse cookie name + attributes.
2. If the cookie is `HttpOnly`, prepend `__piccolo_<app-id>_` to the cookie name.
3. Forward the modified `Set-Cookie` to client.

**Mode B (strict): Rewrite all cookies**

Piccolo MAY provide a stricter mode that rewrites all cookies, but it is likely to break apps that read cookies via `document.cookie`.

**Example:**
```
App "immich" sets: Set-Cookie: session=abc123; Path=/; HttpOnly
Client receives:   Set-Cookie: __piccolo_immich_session=abc123; Path=/; HttpOnly

Client sends:      Cookie: __piccolo_immich_session=abc123
App receives:      Cookie: session=abc123
```

**Benefits:**
- Prevents cookie *name collisions* and reduces accidental cross-app breakage on LAN
- Provides **server-side cookie namespacing** — backends receive only cookies intended for that app
- Works with all cookie attributes (Path, Expires, HttpOnly, etc.)

**Limitations:**
- **Cross-app cookie visibility remains for non-HttpOnly cookies:** In port-based LAN mode, the browser cookie domain is still shared. Rewriting improves what backends receive, but does not isolate what browsers expose to scripts.
- **Not a strong security boundary:** A malicious app on the shared cookie domain could attempt cookie injection/cookie-tossing. This mechanism is primarily for collision avoidance and backend hygiene, not hardened isolation.

> **Threat Model: LAN is a Shared Cookie Domain**
>
> In LAN mode, all apps share the hostname `piccolo.local` (different ports). Because cookies are scoped to the hostname (not the port), all apps effectively share a **single cookie domain**. Consequences:
>
> - **Client-side cookie isolation is not possible** — JavaScript from any app can read non-HttpOnly cookies from all apps
> - **Server-side namespacing only** — Cookie name rewriting isolates what backends receive, not what browsers expose to scripts
>
> This is an **intentional design tradeoff**. For full client-side isolation, use per-app hostnames (LAN host-based routing) or WAN access. Apps on LAN MUST use `HttpOnly` for sensitive cookies.

**Excluded from rewriting:**
- Piccolo reserved cookies (already isolated via stripping)
- LAN host-based routing and Remote/WAN access (per-app hostnames provide natural isolation)

> **⚠️ Cookie Prefix Limitation:**
> Rewriting breaks `__Host-` and `__Secure-` cookie prefixes on LAN:
> - `__Host-` requires no `Domain` and `Path=/` — LAN multi-port violates this spirit
> - `__Secure-` requires HTTPS — LAN is typically HTTP
>
> Apps using these prefixes will have them rewritten (e.g., `__Host-session` → `__piccolo_app___Host-session`), breaking their security semantics. These prefixes are primarily useful for WAN where we don't rewrite anyway. Apps requiring strict cookie security should be accessed via WAN (subdomain-per-app).

#### 4.1.9 Alias Domains

Alias domains are custom domains mapped to app listeners (e.g., `myblog.com` → `blog.mypiccolo.com`).

**Strategy compatibility:**

| Strategy | Alias Domain Support | Reason |
|----------|---------------------|--------|
| `public` | Yes | No auth required |
| `oidc_passthrough` | Yes | App manages its own session via OIDC tokens |
| `headers` | **No** | Session cookie not shared across domains |
| `protected` | **No** | Session cookie not shared across domains |

**Behavior:**
- Requests to alias domains with `headers` or `protected` strategy will fail authentication (no session cookie)
- Proxy logs a warning when `headers`/`protected` strategy is accessed via alias domain
- Apps requiring auth on alias domains MUST use `oidc_passthrough` strategy

**Dynamic resolution:**
Alias domains are derived from remote config (`remoteManager`), which is user-editable. The system auto-adjusts at runtime:
- OIDC redirect URI validation dynamically includes current alias domains
- Changes to alias mappings take effect immediately (no restart required)

**OIDC redirect URI validation:**
When validating redirect URIs for `oidc_passthrough` apps, Piccolo accepts URIs from:
1. Standard listener URLs (e.g., `https://immich.piccolo-xyz.example.com/callback`)
2. Alias domain URLs (`https://myblog.com/callback`)

Alias list is resolved at auth-time from current remote config.

### 4.2 Service-Level OIDC Client

```yaml
services:
  web:
    image: nextcloud:latest
    bind_ports: [8080]
    oidc_client:
      redirect_uris:
        - "app.immich:///oauth-callback"
      ca_mount_path: /var/www/html/certs/piccolo-ca.crt
      env:
        ISSUER_URL: "{{ .System.Auth.Issuer }}"
        CLIENT_ID: "{{ .System.Auth.ClientID }}"
        CLIENT_SECRET: "{{ .System.Auth.ClientSecret }}"

  db:
    image: postgres:16
    bind_ports: [5432]
    # No oidc_client — credentials not injected
```

#### 4.2.1 Fields

| Field | Required | Description |
|-------|----------|-------------|
| `redirect_uris` | No | Additional URIs for native/desktop apps |
| `ca_mount_path` | Yes | Path where Piccolo's CA cert will be mounted inside the container |
| `env` | Yes | Environment variables with template support |

#### 4.2.2 Redirect URIs

By default, Piccolo accepts redirect URIs matching the app's listeners. For native apps, declare additional URIs:

| Type | Example |
|------|---------|
| Custom scheme | `app.immich:///oauth-callback` |
| Localhost | `http://localhost/callback` |
| IPv4 Loopback | `http://127.0.0.1:8080/callback` |
| IPv6 Loopback | `http://[::1]:8080/callback` |

**Rejected:** Any redirect URI with a host other than `localhost`, `127.0.0.1`, `::1`, or custom schemes.

**Security:**
- PKCE is required for all authorization flows (S256 method only; `plain` is rejected).
- Authorization requests without `code_challenge` are rejected.
- Custom scheme redirect URIs should use app-specific prefixes (e.g., `app.<appname>:`) to reduce hijacking risk.

**Client Model:**
Piccolo registers a single **confidential client** per app, used by both web and native flows:

| Flow | Client Secret | PKCE | Token Endpoint Auth |
|------|--------------|------|---------------------|
| Web (server-side) | Required | Required | `client_secret_post` or `client_secret_basic` |
| Native (mobile/desktop) | Optional | Required | PKCE alone sufficient |

**Rationale:**
- **Single client simplifies management** — one client ID/secret pair per app, shared across all services
- **PKCE required for all flows** — provides protection even if secret is compromised or unavailable
- **Native apps can omit secret** — when PKCE is present and redirect URI is loopback/custom-scheme, token endpoint accepts requests without client authentication (per RFC 8252 §8.4)
- **Web apps should include secret** — defense in depth alongside PKCE

This hybrid model allows native companion apps to use the same client ID as the web app without embedding secrets, while web backends benefit from secret-based authentication.

**Token endpoint authentication rules:**

The token endpoint skips client authentication when ALL conditions are met:
1. Valid PKCE `code_verifier` is present (matches `code_challenge` from authorization)
2. The authorization code was issued to a loopback (`127.0.0.1`, `::1`, `localhost`) or custom-scheme redirect URI
3. The `redirect_uri` in token request matches the original authorization request

If any condition is not met, client authentication (`client_secret`) is required.

```
Token Request Decision:
├─ code_verifier present AND valid?
│  ├─ NO → Require client_secret
│  └─ YES → Original redirect_uri is loopback/custom-scheme?
│           ├─ NO → Require client_secret
│           └─ YES → Client auth optional (PKCE sufficient)
```

#### 4.2.3 Template Variables & Discovery

Piccolo guarantees these variables when `oidc_client` is declared:

- `{{ .System.Auth.Issuer }}` — OIDC issuer URL (e.g., `https://piccolo.local`)
- `{{ .System.Auth.ClientID }}` — Client ID
- `{{ .System.Auth.ClientSecret }}` — Client secret

The internal CA certificate is mounted at the service’s `oidc_client.ca_mount_path`.

**Discovery:**
Apps **SHOULD** use the provided `ISSUER_URL` to fetch the OIDC discovery document (`/.well-known/openid-configuration`) to locate the correct `authorization_endpoint`, `token_endpoint`, and `jwks_uri`.
The `authorization_endpoint` may vary based on the access context (LAN vs. WAN). Hardcoding paths (e.g., `{{Issuer}}/oauth/authorize`) is **discouraged** as it may break in remote access scenarios.

**Timing:** Template variables are evaluated at container creation time. Values are static for the container's lifetime.

#### 4.2.4 Client Lifecycle

- Registered at app install if any service declares `oidc_client`
- Same credentials shared across services in the app
- Deleted on app uninstall

**Storage:** OIDC client credentials are stored in `oidc_clients` table within the encrypted control volume.

**Multiple services:** If multiple services declare `oidc_client`:
- `redirect_uris` arrays are merged (deduplicated).
- `env` mappings must not conflict (same key with different values is rejected).
- `ca_mount_path` is service-specific (each service declares its own path).

### 4.3 App-Level `auth` Removed

App-level `auth` block is rejected. Use `listeners[].auth` for access control and `services[].oidc_client` for credentials.

## 5. Examples

### 5.1 OIDC App with Mobile Support (Immich)

```yaml
name: immich

listeners:
  - name: web
    guest_port: 3001
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/.well-known/"
          type: prefix
          strategy: public
        - path: "/api/server-info/ping"
          type: exact
          strategy: public
        - path: "/"
          type: prefix
          strategy: oidc_passthrough

services:
  server:
    image: ghcr.io/immich-app/immich-server:release
    bind_ports: [3001]
    oidc_client:
      redirect_uris:
        - "app.immich:///oauth-callback"
        - "http://localhost/oauth-callback"
      ca_mount_path: /usr/local/share/ca-certificates/piccolo-ca.crt
      env:
        IMMICH_OAUTH_ISSUER_URL: "{{ .System.Auth.Issuer }}"
        IMMICH_OAUTH_CLIENT_ID: "{{ .System.Auth.ClientID }}"
        IMMICH_OAUTH_CLIENT_SECRET: "{{ .System.Auth.ClientSecret }}"

  db:
    image: postgres:16
    bind_ports: [5432]

x-piccolo:
  mode: service
```

### 5.2 Header Auth App (FileBrowser)

```yaml
name: filebrowser

listeners:
  - name: web
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/api/public/"
          type: prefix
          strategy: public
        - path: "/"
          type: prefix
          strategy: headers

services:
  main:
    image: filebrowser/filebrowser:latest
    bind_ports: [8080]
    environment:
      FB_AUTH_METHOD: proxy
      FB_AUTH_HEADER: X-Piccolo-User

x-piccolo:
  mode: service
```

### 5.3 Protected App (Simple Tool)

```yaml
name: internal-dashboard

listeners:
  - name: web
    guest_port: 8080
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/health"
          type: exact
          strategy: public
        - path: "/"
          type: prefix
          strategy: protected

services:
  main:
    image: my-dashboard:latest
    bind_ports: [8080]

x-piccolo:
  mode: service
```

### 5.4 Multiple Listeners (Grafana)

```yaml
name: grafana

listeners:
  - name: web
    guest_port: 3000
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/api/health"
          type: exact
          strategy: public
        - path: "/public/"
          type: prefix
          strategy: public
        - path: "/"
          type: prefix
          strategy: oidc_passthrough

  - name: metrics
    guest_port: 9090
    flow: tcp
    protocol: http
    auth:
      rules:
        - path: "/"
          type: prefix
          strategy: public

services:
  main:
    image: grafana/grafana:latest
    bind_ports: [3000, 9090]
    oidc_client:
      ca_mount_path: /etc/ssl/certs/piccolo-ca.crt
      env:
        # Note: Grafana supports discovery via GF_AUTH_GENERIC_OAUTH_AUTH_URL if configured correctly,
        # but here we use discovery-derived values if the app supports it, or standard paths if needed.
        # Prefer apps that support discovery from the Issuer URL.
        GF_AUTH_GENERIC_OAUTH_AUTH_URL: "{{ .System.Auth.Issuer }}/oauth/authorize"
        GF_AUTH_GENERIC_OAUTH_TOKEN_URL: "{{ .System.Auth.Issuer }}/oauth/token"
        GF_AUTH_GENERIC_OAUTH_CLIENT_ID: "{{ .System.Auth.ClientID }}"
        GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET: "{{ .System.Auth.ClientSecret }}"

x-piccolo:
  mode: service
```

> **⚠️ WAN Compatibility Warning:** This example uses hardcoded OAuth URLs (`{{ .System.Auth.Issuer }}/oauth/authorize`). Apps with static URL configuration are **LAN-only compatible**. In WAN mode, the authorization endpoint is served from a different origin (e.g., `https://piccolo-xyz.example.com/oauth/authorize`), which these hardcoded URLs won't reach.
>
> For WAN support, use apps that support OIDC discovery from the issuer URL (`/.well-known/openid-configuration`).

## 6. Schema

### 6.1 Listener Auth

For complete listener schema, see RFC 20260102. The `auth` block extends the base listener definition:

```yaml
listeners[]:
  name: string                               # Required
  guest_port: int                            # Required
  flow: tcp                                  # Required for auth (tls not supported)
  protocol: http | websocket                 # Required for auth (raw not supported)
  auth:                                      # Optional; omit for all-protected
    rules:
      - path: string                         # The path to match (required)
        type: exact | prefix | pattern       # Match type (required)
        strategy: oidc_passthrough | headers | protected | public  # Required
```

### 6.2 Service OIDC Client

```yaml
services.<name>:
  oidc_client:
    redirect_uris: []string                  # Optional
    ca_mount_path: string                    # Required
    env: map[string]string                   # Required
```

## 7. Validation Rules

1. `auth` block only valid on `flow: tcp` with `protocol: http` or `protocol: websocket`.
2. `auth` block rejected on `flow: tls` or `protocol: raw`.
3. `auth.rules[].type` is required and must be `exact`, `prefix`, or `pattern`.
4. `auth.rules[].path` must be valid for the specified type:
   - `prefix` paths must end with `/`
   - `pattern` paths must be valid `path.Match` patterns
5. `auth.rules[].strategy` must be `oidc_passthrough`, `headers`, `protected`, or `public`.
6. **If any rule uses `oidc_passthrough`, at least one service MUST declare `oidc_client`.** *(Guardrail: prevents missing credentials, but cannot verify app enforces auth)*
7. `oidc_client.env` must not be empty if `oidc_client` declared.
8. `oidc_client.ca_mount_path` is required if `oidc_client` declared.
9. `oidc_client.redirect_uris` must be custom schemes, localhost, or loopback (IPv4 `127.0.0.1` or IPv6 `::1`).
10. App-level `auth` block is rejected.

### 7.1 Validation Error Messages

Parser validation failures return structured errors:

| Rule Violated | Error Code | Error Message |
|---------------|------------|---------------|
| Rule 1 | `INVALID_AUTH_FLOW` | `auth block requires flow: tcp with protocol: http or websocket` |
| Rule 2 | `INVALID_AUTH_PROTOCOL` | `auth block not supported on flow: tls or protocol: raw` |
| Rule 3 | `INVALID_MATCH_TYPE` | `auth.rules[].type is required and must be one of: exact, prefix, pattern` |
| Rule 4 | `INVALID_PATH` | `invalid path "<path>" for type "<type>": <reason>` |
| Rule 5 | `INVALID_STRATEGY` | `invalid strategy "<value>", must be one of: oidc_passthrough, headers, protected, public` |
| Rule 6 | `OIDC_CLIENT_REQUIRED` | `oidc_passthrough strategy requires at least one service to declare oidc_client` |
| Rule 7 | `OIDC_ENV_REQUIRED` | `oidc_client.env must not be empty` |
| Rule 8 | `OIDC_CA_PATH_REQUIRED` | `oidc_client.ca_mount_path is required` |
| Rule 9 | `INVALID_REDIRECT_URI` | `redirect_uri "<uri>" must be localhost, loopback (127.0.0.1, ::1), or custom scheme` |
| Rule 10 | `APP_AUTH_DEPRECATED` | `app-level auth block is deprecated; use listeners[].auth and services[].oidc_client` |

### 7.2 Runtime Error Responses

Proxy runtime errors return JSON responses:

| Scenario | HTTP Status | Response Body |
|----------|-------------|---------------|
| Session missing/invalid | 401 | `{"error": "authentication_required", "code": "AUTH_REQUIRED"}` |
| Session expired | 401 | `{"error": "session_expired", "code": "SESSION_EXPIRED"}` |
| App not in `allowed_apps` | 403 | `{"error": "app_access_denied", "code": "APP_NOT_ALLOWED"}` |
| Malformed request path | 400 | `{"error": "invalid_request_path", "code": "INVALID_PATH"}` |
| Path normalization failed | 400 | `{"error": "path_normalization_failed", "code": "PATH_INVALID"}` |

## 8. Migration

This RFC introduces a **breaking change** by removing the app-level `auth` block.

**Migration Checklist:**
1.  **Apps using `auth: oidc`:** Move config to `listeners[].auth` (with `strategy: oidc_passthrough`) and `services[].oidc_client`.
2.  **Apps using `auth: headers`:** Move config to `listeners[].auth` (with `strategy: headers`).
3.  **Apps using `auth: public`:** Move config to `listeners[].auth` (with `strategy: public`).
4.  **Apps with Mixed Rules:** Define specific rules in `listeners[].auth.rules`.

**Handling Old Config:**
The parser will reject valid manifests containing the legacy `auth` block with a helpful error message pointing to the new schema.

## 9. Implementation Plan

1. **Parser:** Update validation logic.
2. **Proxy:** Implement path matching using Go `path.Match` (for `pattern`) plus prefix/exact matching and normalization logic.
3. **OIDC Injection:** Refactor to be service-scoped.
4. **Cookie Security:** Implement strict cookie stripping for defined names.
5. **Tests:** Add extensive test cases for match types (`exact`, `prefix`, `pattern`), normalization edge cases, and header/cookie behavior.

## 10. Security Considerations

- **Default protected:** Public access requires explicit declaration.
- **Header stripping:** ALL incoming `X-Piccolo-*` headers stripped; forwarded headers overwritten for untrusted clients (preserved/appended for trusted loopback sources).
- **Cookie isolation:** Piccolo session cookies (`piccolo_session`, etc.) stripped from requests before forwarding.
- **Set-Cookie blocking:** App responses cannot set cookies targeting Piccolo cookie names.
- **Credential isolation:** OIDC secrets only injected where declared.
- **Path normalization:** Strict normalization prevents bypasses; 400 on failure.
- **PKCE required:** All OIDC flows require PKCE.
- **Redirect URI validation:** External URLs rejected; `next` param limited to same-origin.

## 11. ConnectionAuth (L4 IP-based access control)

Per RFC 20260505 §3.4, the `auth` block defined above operates at L7 (path
+ HTTP request). A complementary `connection_auth` block on the listener
provides L4 IP-based access control that runs BEFORE TLS termination. It
is permitted on every flow including `flow: tls` (passthrough) and
`flow: udp` — paths where the L7 `auth` block isn't applicable because
piccolod doesn't decode the request payload.

### 11.1 Schema

```yaml
listeners:
  - name: api
    flow: tcp
    protocol: http
    connection_auth:
      default: deny
      rules:
        - match: 192.168.0.0/16
          strategy: allow
        - match: 192.168.0.42/32
          strategy: deny
```

- `default`: `"allow"` (default — preserves implicit-allow behavior) or
  `"deny"`.
- `rules[]`: evaluated in declaration order; first match wins; falls
  through to `default` when nothing matches.
- `rules[].match`: CIDR (IPv4 or IPv6).
- `rules[].strategy`: `"allow"` or `"deny"`.

### 11.2 Semantics

When the field is non-nil, the registry composes the `connection_auth`
canonical L4 / L4UDP middleware automatically — no need to also list it
under `Middleware[]`. Symmetric to how this RFC's `auth` block composes
the `path_auth` middleware.

The middleware reads
`internal/services/middleware/EffectiveSourceIP(ctx)`, which honors the
TrustedLoopback gate: connections relayed via the TLS mux carry the
real-client IP via `ConnContext.Hint`, which overrides the loopback
`SourceAddr`. Unresolvable source IP is fail-closed (deny) regardless of
`default`.

Denied connections are closed at L4 before any handler runs. A deny event
is recorded by the `conn_metrics` registry with reason `"connection_auth"`
and labels `{listener, source_ip}`.

### 11.3 Coexistence with `auth`

`auth` (path-based, L7) and `connection_auth` (IP-based, L4) coexist. L4
evaluates first; if L4 denies, the connection drops before TLS termination.
L7 path rules apply only after L4 admits. Apps with `connection_auth` get
firewall-style protection at the network edge.

For `flow: tls` listeners (passthrough), only `connection_auth` is
applicable — `auth.rules` cannot be evaluated without HTTP visibility.
The parser already rejects `auth` on `flow: tls` (existing behavior).
The parser permits `connection_auth` on any flow including UDP.

### 11.4 Reconcile equality

`nil` and `{Default: "allow", Rules: nil}` compare equal (no spurious
chain rebuild on YAML-roundtrip variation per RFC 20260505 plan §D17).
`Rules: nil` and `Rules: []` also compare equal (sibling-shape
consistency with existing `middlewareEqual`).

## 12. Future Enhancements

### 12.1 App-Driven User Provisioning

Apps could notify Piccolo when they invite external users. Deferred to future RFC.

### 12.2 Access Policies

Future RFCs may introduce richer access policies (e.g., allow specific user groups per path) within the `rules` block.

## 13. Implementation Notes & Status

- **Status:** Draft
- **Depends on:** RFC 20260106 (Native OIDC Auth), RFC 20260102 (Multi-Container Apps)
- **Supersedes:** App-level `auth` block from RFC 20260106 §4
