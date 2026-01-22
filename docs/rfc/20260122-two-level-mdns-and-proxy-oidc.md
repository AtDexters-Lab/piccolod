# RFC: Two-Level mDNS Domains & Proxy-Level OIDC Authentication

- **Status:** Draft
- **Date:** 2026-01-22
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC proposes three interconnected changes to Piccolo's network and authentication architecture:

1. **Two-level mDNS domains:** Change LAN app hostnames from `<app>.piccolo.local` (3-level) to `<app>-piccolo.local` (2-level) for compatibility with restrictive mDNS resolvers.

2. **Proxy-level OIDC authentication:** Introduce an OIDC client flow at the proxy layer for `headers` and `protected` auth strategies, enabling seamless SSO across all access domains without cookie sharing.

3. **Host-only session cookies:** Simplify cookie architecture by always using host-only cookies, with session audience + origin binding for isolation (and distinct cookie names for LAN port-based routing).

These changes together solve mDNS resolver compatibility issues while maintaining (and improving) the SSO experience across portal and apps.

## 2. Motivation

### 2.1 mDNS Resolver Compatibility

Many default mDNS resolver configurations reject 3-level `.local` domains:

- **Linux `mdns_minimal`:** Only resolves 2-level `.local` domains by default
- **Windows mDNS:** Similar restrictions in some configurations
- **Network appliances:** Various routers and DNS forwarders have 2-level assumptions

Current 3-level format (`immich.piccolo.local`) fails to resolve on these systems, forcing users to modify system DNS configuration or use IP-based access.

### 2.2 Cookie Domain Limitations

The current architecture relies on subdomain cookie sharing:

```
Portal: piccolo.local
  Cookie: piccolo_session; Domain=.piccolo.local

App: immich.piccolo.local (subdomain)
  → Receives portal cookie automatically
```

This approach has limitations:

1. **Doesn't work with 2-level domains:** `immich-piccolo.local` is a sibling of `piccolo.local`, not a subdomain
2. **Doesn't work with alias domains:** `myapp.com` cannot share cookies with `piccolo.local`
3. **Security concerns:** Portal session cookie sent to all app backends

### 2.3 Unified Auth Model

Currently, alias domains are incompatible with `headers`/`protected` auth strategies (RFC 20260112 §4.1.9). This RFC enables all auth strategies to work across all access paths uniformly.

## 3. Goals & Non-Goals

### 3.1 Goals

- **mDNS compatibility:** 2-level domains work with restrictive resolvers
- **Unified SSO:** Single sign-on works across portal, apps, and alias domains
- **Security improvement:** Host-only cookies + audience/origin binding prevent cross-domain session leakage and replay
- **Logout propagation:** Portal logout invalidates all app sessions
- **Consistent session TTL:** App sessions use same TTL as portal sessions
- **Backwards compatibility:** Existing apps continue to work without manifest changes
- **Alias domain support:** Enable `headers`/`protected` strategies on alias domains

### 3.2 Non-Goals

- Change Remote (WAN) hostname scheme (remains `<app>.<portal-base>`)
- Modify OIDC token format or claims
- Change app manifest schema for auth rules

### 3.3 Terminology

This RFC uses the following terms consistently:

| Term | Definition | Example |
|------|------------|---------|
| **Base hostname** | The device's mDNS hostname (FQDN without trailing dot) | `piccolo.local` |
| **Base label** | The DNS label portion of the base hostname (before `.local`) | `piccolo` |
| **Portal hostname** | The hostname where the portal UI and OIDC provider are accessible | LAN: `piccolo.local`, Remote: `portal.example.com` |
| **App hostname** | The hostname where an app is accessible | LAN: `immich-piccolo.local`, Remote: `immich.portal.example.com` |
| **Origin** | Canonical form: `scheme://host[:port]` with default ports omitted | `http://piccolo.local`, `https://portal.example.com` |

## 4. Proposed Hostname Scheme

### 4.1 LAN Hostname Format

**Current (3-level):**
```
Primary:   <app>.<base>           → immich.piccolo.local
Secondary: <listener>-<app>.<base> → metrics-immich.piccolo.local
```

**Proposed (2-level):**
```
Primary:   <app>-<base>           → immich-piccolo.local
Secondary: <listener>-<app>-<base> → metrics-immich-piccolo.local
```

### 4.2 Remote Hostname Format (Unchanged)

Remote access continues to use subdomain format for wildcard certificate compatibility:

```
Primary:   <app>.<portal-base>           → immich.piccolo-xyz.example.com
Secondary: <listener>-<app>.<portal-base> → metrics-immich.piccolo-xyz.example.com
```

### 4.3 Validation Constraints

This RFC tightens constraints from RFC 20260114 §4.4 to guarantee derived DNS labels remain valid under the 2-level LAN hostname format:

- App names: `^[a-z][a-z0-9]{0,15}$` (max 16 chars; no hyphens)
- Listener names: `^[a-z][a-z0-9]{0,15}$` (max 16 chars; no hyphens)
- LAN base label (device hostname label): `^[a-z][a-z0-9-]{0,15}$` (max 16 chars; hyphens allowed for conflict suffixing like `piccolo-abc123`)
- Reserved names: `api`, `www`, `admin`, `root`, `system`, `piccolo`, `piccoloos`

**Rationale for 16-char limit:** With the 2-level format `<listener>-<app>-<base>.local`, the worst case is `16 + 1 + 16 + 1 + 16 = 50` characters for the left-most label, safely under the 63-character DNS limit.

Additionally:
- Piccolo MUST reject any derived hostname whose left-most DNS label would exceed 63 characters (defense in depth).

The no-hyphen constraint on app/listener ensures unambiguous parsing:

```
metrics-immich-piccolo.local
         ↓ strip known base "-piccolo.local"
metrics-immich
         ↓ split on first hyphen
listener=metrics, app=immich
```

### 4.4 Base Hostname Extraction

Given a request hostname and known base:

```go
func extractHostLabel(reqHost, base string) string {
    suffix := "-" + strings.ToLower(base)
    if strings.HasSuffix(reqHost, suffix) {
        return strings.TrimSuffix(reqHost, suffix)
    }
    return ""
}

// Example:
// extractHostLabel("immich-piccolo.local", "piccolo.local") → "immich"
// extractHostLabel("metrics-immich-piccolo.local", "piccolo.local") → "metrics-immich"
```

## 5. Proxy-Level OIDC Authentication

### 5.1 Overview

For `headers` and `protected` auth strategies, the proxy acts as an OIDC Relying Party (client), authenticating users via Piccolo's OIDC provider before forwarding requests.

This replaces the current model where apps directly receive the portal's session cookie.

### 5.2 Authentication Flow

```
┌─────────┐     ┌─────────────────────┐     ┌──────────────┐     ┌─────────┐
│ Browser │     │ Proxy               │     │ OIDC Provider│     │ Backend │
│         │     │ (immich-piccolo)    │     │ (piccolo)    │     │ (app)   │
└────┬────┘     └──────────┬──────────┘     └──────┬───────┘     └────┬────┘
     │                     │                       │                  │
     │ GET /photos         │                       │                  │
     ├────────────────────►│                       │                  │
     │                     │                       │                  │
     │                     │ No session cookie     │                  │
     │                     │ for this domain       │                  │
     │                     │                       │                  │
     │ 302 Redirect        │                       │                  │
     │◄────────────────────┤                       │                  │
     │ Location: http://piccolo.local/oauth/authorize                 │
     │   ?client_id=piccolo-immich-proxy           │                  │
     │   &redirect_uri=http://immich-piccolo.local/__piccolod_oidc/callback
     │   &response_type=code                       │                  │
     │   &state=...&code_challenge=...             │                  │
     │                     │                       │                  │
     │ GET /oauth/authorize│                       │                  │
     ├─────────────────────┼──────────────────────►│                  │
     │                     │                       │                  │
     │                     │    User has portal    │                  │
     │                     │    session → approve  │                  │
     │                     │                       │                  │
     │ 302 Redirect        │                       │                  │
     │◄────────────────────┼───────────────────────┤                  │
     │ Location: http://immich-piccolo.local/__piccolod_oidc/callback?code=...│
     │                     │                       │                  │
     │ GET /__piccolod_oidc/callback?code=...      │                  │
     ├────────────────────►│                       │                  │
     │                     │                       │                  │
     │                     │ POST /oauth/token     │                  │
     │                     ├──────────────────────►│                  │
     │                     │                       │                  │
     │                     │ {access_token, ...}   │                  │
     │                     │◄──────────────────────┤                  │
     │                     │                       │                  │
     │                     │ GET /oauth/userinfo   │                  │
     │                     ├──────────────────────►│                  │
     │                     │                       │                  │
     │                     │ {sub, email, ...}     │                  │
     │                     │◄──────────────────────┤                  │
     │                     │                       │                  │
     │ 302 Redirect        │                       │                  │
     │◄────────────────────┤                       │                  │
     │ Set-Cookie: piccolo_session=xyz (host-only) │                  │
     │ Location: /photos   │                       │                  │
     │                     │                       │                  │
     │ GET /photos         │                       │                  │
     ├────────────────────►│                       │                  │
     │                     │                       │                  │
     │                     │ Session valid         │                  │
     │                     │ (for headers: inject X-Piccolo-*)        │
     │                     │                       │                  │
     │                     │ GET /photos           │                  │
     │                     ├──────────────────────────────────────────►
     │                     │                       │                  │
     │                     │                       │      Response    │
     │                     │◄──────────────────────────────────────────
     │      Response       │                       │                  │
     │◄────────────────────┤                       │                  │
     │                     │                       │                  │
```

### 5.3 OIDC Client Registration

Apps with `headers` or `protected` auth strategies automatically receive OIDC client credentials:

| App Auth Strategy | OIDC Client | Registration |
|-------------------|-------------|--------------|
| `public` | None | N/A |
| `oidc_passthrough` | Explicit | Declared in manifest |
| `headers` | Auto | Generated at app install |
| `protected` | Auto | Generated at app install |

**Client ID format:** `piccolo-<app>-proxy`

**Client credentials storage:** Same `oidc_clients` table, with `type: proxy` marker.

**Consent UX:** Proxy OIDC clients are **trusted first-party clients** and skip the OAuth consent prompt. The authorization endpoint auto-approves when:
1. The client type is `proxy`, AND
2. The user has a valid portal session, AND
3. The user passes `allowed_apps` access control checks

**Important:** "Auto-approval" bypasses the consent UI, **not** access control. The `allowed_apps` policy (RFC 20260112) is still enforced at authorization time:

```go
func (p *Provider) Authorize(req AuthRequest, portalSession Session) error {
    // 1. Validate portal session
    if !portalSession.Valid() {
        return ErrNoSession // Redirect to login
    }

    // 2. Check allowed_apps for this user (access control)
    if !p.userCanAccessApp(portalSession.UserID, req.AppName) {
        return ErrForbidden // 403 → render access denied page
    }

    // 3. Auto-approve (skip consent UI for proxy clients)
    if req.Client.Type == "proxy" {
        return p.issueAuthCode(req, portalSession)
    }
    // ... show consent for non-proxy clients
}
```

**When do allowed_apps changes take effect?**

`allowed_apps` is enforced at **two points** for defense in depth (consistent with RFC 20260112):

1. **Authorization time:** User cannot obtain an app session if not in `allowed_apps` for that app
2. **Per-request:** Proxy validates `allowed_apps` on every request, even with a valid session

This means changes take effect **immediately**:
- User removed from `allowed_apps` → next request returns 403 (even with valid session)
- User added to `allowed_apps` → can immediately initiate OIDC flow to get session

```go
// Per-request check in proxy (before forwarding to backend)
func (p *ProxyManager) validateRequest(sess Session, appName string) error {
    // 1. Validate session (audience, origin binding, expiry)
    if err := p.validateAppSession(sess, appName); err != nil {
        return err // 401 → initiate OIDC flow
    }

    // 2. Check allowed_apps (per-request, not cached)
    if !p.userCanAccessApp(sess.UserID, appName) {
        return ErrForbidden // 403 → access denied
    }

    return nil // Forward to backend
}
```

This per-request check ensures revocation is immediate without relying on logout propagation.

**Access denied UX:**

A raw HTTP 403 response is UX-hostile for browser navigations. When access is denied:

| Context | Response |
|---------|----------|
| API client (`Accept: application/json`) | 403 JSON: `{"error": "forbidden", "message": "You do not have access to this app"}` |
| Browser (authorization endpoint) | Redirect to portal "access denied" page with context |
| Browser (per-request proxy check) | Render friendly HTML error page |

For browser requests, the portal provides a user-friendly error page explaining:
- Which app access was denied
- Who to contact (admin) to request access
- Link back to the portal dashboard

```
┌─────────────────────────────────────────────────────────┐
│  ⚠️  Access Denied                                      │
│                                                         │
│  You don't have permission to access "Immich".          │
│                                                         │
│  Contact your administrator to request access.          │
│                                                         │
│  [← Back to Portal]                                     │
└─────────────────────────────────────────────────────────┘
```

### 5.4 Redirect URI Registration

Proxy OIDC clients use the same cartesian product approach as app-declared clients (§11), but with a single fixed callback path:

```
Proxy Redirect URIs = (All Valid Origins) × { "/__piccolod_oidc/callback" }
```

| Access Path | Redirect URI Pattern |
|-------------|---------------------|
| LAN 2-level | `http://<app>-<base>/__piccolod_oidc/callback` |
| LAN 2-level (listener) | `http://<listener>-<app>-<base>/__piccolod_oidc/callback` |
| LAN port-based | `http://<base>:<port>/__piccolod_oidc/callback` |
| Remote | `https://<app>.<portal-base>/__piccolod_oidc/callback` |
| Remote (listener) | `https://<listener>-<app>.<portal-base>/__piccolod_oidc/callback` |
| Alias | `https://<alias-domain>/__piccolod_oidc/callback` |

**Valid origins** are dynamically computed from **trusted configuration/state only**:
- Current mDNS hostname (handles conflicts like `piccolo-abc123.local`)
- Remote manager configuration (portal hostname, aliases)
- Service manager endpoints (listeners, ports)
- Local machine IP addresses (for mDNS-less clients)

**SECURITY:** Valid origins MUST come from trusted internal state, **never** from request `Host` headers or other client-controlled inputs. Using request headers would allow attackers to inject arbitrary redirect URIs.

This ensures full compliance with OAuth 2.0 exact URI matching (RFC 6749, RFC 9700). See §11 for detailed explanation of the cartesian product approach.

### 5.5 Authorization Endpoint Construction

Unlike external OIDC clients that rely on discovery, the proxy layer is **origin-aware**: it knows the request origin when initiating the OIDC flow. The proxy constructs the authorization URL directly using the appropriate portal origin for the access context:

| Access Context | Portal Origin | Authorization Endpoint |
|----------------|---------------|------------------------|
| LAN | `http://piccolo.local` | `http://piccolo.local/oauth/authorize` |
| Remote/Alias | `https://portal.example.com` | `https://portal.example.com/oauth/authorize` |

This avoids the heuristics used by the external discovery endpoint (which must guess context from the request).

### 5.6 Back-Channel Communication

All proxy back-channel calls (token exchange, userinfo) use **in-process interfaces** to the OIDC provider, avoiding network resolution and TLS trust issues:

```go
// In-process call returns tokens AND the portal session that authorized them.
// This enables parent-session linkage without modifying OIDC token claims.
type ExchangeResult struct {
    Tokens          *oauth2.Token
    IDToken         *oidc.IDToken
    PortalSessionID string  // The portal session that approved this authorization
}

result, err := p.oidcProvider.ExchangeCode(code, redirectURI, codeVerifier)
userInfo, err := p.oidcProvider.GetUserInfo(result.Tokens.AccessToken)

// The PortalSessionID is tracked server-side:
// 1. User initiates OIDC flow from app
// 2. Authorization endpoint validates portal session, issues auth code
// 3. Auth code is linked to portal session ID in server-side storage
// 4. Token exchange returns this linkage via ExchangeResult.PortalSessionID
// 5. App session stores ParentSessionID for scoped logout
```

**Important:** The `PortalSessionID` is returned via the in-process interface, not embedded in tokens. This preserves the non-goal "Modify OIDC token format or claims."

This ensures the proxy never depends on mDNS/DNS resolution for back-channel operations.

### 5.7 State Storage

The OIDC state parameter encodes or references stored data required for callback validation:

```go
type OIDCState struct {
    ID             string    // Opaque state token sent to authorization server
    CodeVerifier   string    // PKCE code_verifier (for token exchange)
    OriginalPath   string    // Path+query to redirect back to (MUST be relative, no scheme/host)
    ExpectedApp    string    // App name (prevents confused deputy)
    ExpectedOrigin string    // Expected callback origin (scheme://host[:port])
    CreatedAt      time.Time // For expiry (recommended: 10 minutes)
}
```

**Security properties:**
- **Open redirect prevention:** `OriginalPath` MUST be stored as a relative path+query only (e.g., `/photos?album=vacation`), never an absolute URL. The callback handler constructs the full redirect URL by combining `ExpectedOrigin + OriginalPath`.
- State entries are origin-bound: callback must arrive on the expected origin
- State entries are app-bound: prevents cross-app confused deputy attacks
- PKCE verifier is never exposed to browser (stored server-side, keyed by state ID)
- Short TTL prevents replay of old authorization flows

```go
// When initiating OIDC flow, store only the path:
state.OriginalPath = r.URL.Path
if r.URL.RawQuery != "" {
    state.OriginalPath += "?" + r.URL.RawQuery
}

// In callback, reconstruct safe redirect:
redirectURL := state.ExpectedOrigin + state.OriginalPath
```

**State-store lifecycle and DoS protection:**

The `/__piccolod_oidc/*` endpoints are reachable without authentication (the callback receives the auth code). The state store requires bounds to prevent resource exhaustion:

| Requirement | Value | Rationale |
|-------------|-------|-----------|
| TTL | 10 minutes | Sufficient for auth flow; auto-cleanup of abandoned states |
| Max entries | 10,000 | Bounded memory; ~1KB per entry = ~10MB max |
| Eviction | LRU when full | Graceful degradation under load |

**Rate limiting:** Comprehensive rate limiting (IP-based, per-user, etc.) is deferred to the future **proxy middleware system**. When implemented, the middleware pipeline will provide configurable protection including:
- IP-based rate limiting for `/__piccolod_oidc/callback`
- Request logging and anomaly detection
- Configurable throttling policies

Until the middleware system is available, the bounded state store provides basic protection against state exhaustion attacks. Implementations SHOULD add simple per-IP rate limiting at minimum.

### 5.8 Callback Handler

The proxy intercepts requests to `/__piccolod_oidc/callback`:

```go
func (p *ProxyManager) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
    // 1. Extract authorization code and state
    code := r.URL.Query().Get("code")
    stateID := r.URL.Query().Get("state")

    // 2. Validate state, recover stored data
    state, ok := p.stateStore.Validate(stateID)
    if !ok {
        http.Error(w, "invalid or expired state", http.StatusBadRequest)
        return
    }

    // 3. Verify callback origin matches expected origin
    callbackOrigin := p.requestOrigin(r)
    if callbackOrigin != state.ExpectedOrigin {
        http.Error(w, "origin mismatch", http.StatusBadRequest)
        return
    }

    // 4. Exchange code for tokens (back-channel, using stored PKCE verifier)
    // ExchangeResult includes PortalSessionID for parent-session linkage
    result, err := p.oidcProvider.ExchangeCode(code, redirectURI, state.CodeVerifier)
    if err != nil {
        http.Error(w, "token exchange failed", http.StatusUnauthorized)
        return
    }

    // 5. Get user info
    userInfo, err := p.oidcProvider.GetUserInfo(result.Tokens.AccessToken)
    if err != nil {
        http.Error(w, "userinfo failed", http.StatusUnauthorized)
        return
    }

    // 6. Create app-scoped session with parent linkage
    sess := p.sessions.CreateAppSession(SessionParams{
        UserID:          userInfo.Sub,
        Audience:        "app:" + state.ExpectedApp,
        BoundOrigin:     callbackOrigin,
        ParentSessionID: result.PortalSessionID,  // Enables scoped logout
    })

    // 7. Set host-only cookie with appropriate security attributes
    cookieName := "piccolo_session"
    if p.isPortBasedAccess(r) {
        cookieName = fmt.Sprintf("piccolo_app_session_p%d", p.requestPort(r))
    }
    cookie := &http.Cookie{
        Name:     cookieName,
        Value:    sess.ID,
        Path:     "/",
        HttpOnly: true,
        SameSite: http.SameSiteLaxMode,
        // Domain omitted = host-only
    }
    // Set Secure flag for HTTPS (direct TLS or behind trusted reverse proxy)
    if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
        cookie.Secure = true
    }
    http.SetCookie(w, cookie)

    // 8. Redirect to original path (safe: OriginalPath is relative, not absolute)
    http.Redirect(w, r, state.ExpectedOrigin+state.OriginalPath, http.StatusFound)
}
```

### 5.9 Browser vs API Client Behavior

The proxy distinguishes between browser and API clients when authentication is required (per RFC 20260112):

| Client Type | Method | Detection | Unauthenticated Response |
|-------------|--------|-----------|--------------------------|
| Browser | GET, HEAD | `Accept` includes `text/html` | 302 redirect to OIDC flow |
| Browser | POST, PUT, DELETE | Any | 401 JSON error |
| API client | Any | `Accept: application/json` | 401 JSON error |
| WebSocket | GET | `Upgrade: websocket` | 401 before upgrade |
| CORS preflight | OPTIONS | `Access-Control-Request-Method` | Pass through (no auth) |

**Non-idempotent method handling:**

Redirecting POST/PUT/DELETE requests loses the request body, making OIDC redirect unsuitable. For non-safe methods, the proxy MUST return 401 even for browser clients:

```go
func (p *ProxyManager) handleUnauthenticated(w http.ResponseWriter, r *http.Request) {
    // Non-safe methods: always 401 (redirect loses body)
    if r.Method != http.MethodGet && r.Method != http.MethodHead {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusUnauthorized)
        json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
        return
    }
    // Safe methods: redirect browsers, 401 for API clients
    if wantsBrowserResponse(r) {
        p.redirectToOIDC(w, r)
    } else {
        // ... 401 JSON
    }
}
```

**CORS preflight handling:**

OPTIONS requests with CORS preflight headers (`Access-Control-Request-Method`) MUST NOT trigger authentication. The proxy should either:
1. Pass through to backend for CORS header generation, or
2. Return appropriate CORS headers directly

Redirecting preflight requests breaks CORS and causes cryptic browser errors.

**WebSocket handling:**

WebSocket authentication occurs during the HTTP upgrade request, before the connection upgrades:

```
Client                          Proxy
  │                               │
  │ GET /ws (Upgrade: websocket)  │
  ├──────────────────────────────►│
  │                               │ No valid session cookie
  │ HTTP 401 Unauthorized         │
  │◄──────────────────────────────┤
  │                               │
  │ (Client authenticates via     │
  │  regular HTTP OIDC flow)      │
  │                               │
  │ GET /ws (Upgrade: websocket)  │
  │ Cookie: piccolo_session=xyz   │
  ├──────────────────────────────►│
  │                               │ Session valid
  │ HTTP 101 Switching Protocols  │
  │◄──────────────────────────────┤
  │                               │
  │ ═══════ WebSocket ═══════════ │
```

The client must authenticate via the standard OIDC flow (in a regular HTTP request), then retry the WebSocket upgrade with the session cookie.

### 5.10 Reserved Path

The path `/__piccolod_oidc/` is reserved for proxy-level OIDC operations:

| Path | Purpose |
|------|---------|
| `/__piccolod_oidc/callback` | OIDC authorization callback |
| `/__piccolod_oidc/logout` | Future: app-specific logout |
| `/__piccolod_oidc/session` | Future: session status API |

Apps MUST NOT use paths starting with `/__piccolod_oidc/`. The proxy intercepts these before forwarding.

## 6. Session Architecture

### 6.1 Host-Only Cookies

All session cookies use host-only scoping (no `Domain` attribute).

Cookie names differ by access mode to avoid collisions:
- **Host-based (LAN 2-level / Remote / Alias):** use `piccolo_session`
- **LAN port-based:** use `piccolo_app_session_p<port>` (cookies do not scope by port)

**⚠️ Port-based LAN: Compatibility-only mode with browser isolation limitations**

Port-based LAN routing (`piccolo.local:12345`, `piccolo.local:23456`) provides a **shared-origin** environment from the browser's perspective. All apps on different ports share:
- The same cookie jar (hence per-port cookie names for Piccolo sessions)
- The same `localStorage` / `sessionStorage`
- The same JavaScript origin for CORS purposes

This means **app cookies and JavaScript are NOT isolated** between apps—only Piccolo's session cookies are protected via distinct naming. Port-based routing is intended as a compatibility fallback for environments where mDNS-based 2-level hostnames don't work. For full browser isolation, use host-based routing (2-level mDNS or Remote).

This limitation coexists with RFC 20260112's cookie rewriting: the proxy rewrites backend `Set-Cookie` domains to prevent cross-app cookie leakage for backends that attempt to set domain-scoped cookies, but cannot prevent apps from setting host-scoped cookies that collide.

The proxy MUST only consider the cookie name for the current access mode:
- For app requests on LAN port-based routing, the portal cookie `piccolo_session` (for `piccolo.local`) MUST be ignored for app authentication.

```
Portal (piccolo.local):
  Set-Cookie: piccolo_session=aaa; Path=/; HttpOnly; SameSite=Lax

App (immich-piccolo.local):
  Set-Cookie: piccolo_session=bbb; Path=/; HttpOnly; SameSite=Lax

Alias (myapp.com):
  Set-Cookie: piccolo_session=ccc; Path=/; HttpOnly; SameSite=Lax

LAN port-based (piccolo.local:12345):
  Set-Cookie: piccolo_app_session_p12345=ddd; Path=/; HttpOnly; SameSite=Lax

LAN port-based (piccolo.local:23456):
  Set-Cookie: piccolo_app_session_p23456=eee; Path=/; HttpOnly; SameSite=Lax
```

**Benefits:**
- Portal session never sent to app backends
- No cross-domain cookie leakage
- Works uniformly across all access paths
- Simpler implementation (no domain scoping logic)

**Reserved cookie namespace:**

The `piccolo_` prefix is reserved for Piccolo session cookies. The proxy MUST strip any `piccolo_`-prefixed cookies from backend responses to prevent cookie tossing attacks.

**Implementation note:** Filter header strings directly rather than parse-reserialize, to avoid losing unknown `Set-Cookie` attributes:

```go
func (p *ProxyManager) stripReservedCookies(resp *http.Response) {
    // Filter in place to preserve unknown Set-Cookie attributes
    setCookies := resp.Header["Set-Cookie"]
    filtered := setCookies[:0]
    for _, cookie := range setCookies {
        // Extract cookie name (everything before first '=')
        name := cookie
        if idx := strings.Index(cookie, "="); idx > 0 {
            name = cookie[:idx]
        }
        if !strings.HasPrefix(name, "piccolo_") {
            filtered = append(filtered, cookie)
        }
    }
    resp.Header["Set-Cookie"] = filtered
}
```

**Cookie security attributes:**

| Access Context | Cookie Attributes |
|----------------|-------------------|
| LAN (HTTP) | `Path=/; HttpOnly; SameSite=Lax` |
| Remote/Alias (HTTPS) | `Path=/; HttpOnly; SameSite=Lax; Secure` |

The `Secure` flag is set for HTTPS contexts to prevent cookie transmission over unencrypted connections.

**Origin binding implications:**

Origin binding treats `http://` and `https://` as different origins. This is intentional:
- `http://piccolo.local` session ≠ `https://piccolo.local` session (if LAN TLS is added later)
- Users would need to re-authenticate when switching between HTTP and HTTPS on the same hostname

### 6.2 Session Audience Binding

Each session is bound to an audience and origin to prevent cross-domain replay:

```go
type Session struct {
    ID              string
    UserID          string
    User            string
    Role            string
    CSRF            string
    ExpiresAt       int64
    Audience        string  // "portal" | "app:<appname>"
    BoundOrigin     string  // Canonical origin: scheme://host[:port] (default ports omitted)
    ParentSessionID string  // For app sessions: ID of the portal session that authorized this session
}
```

**Origin canonicalization rules:**

`BoundOrigin` and all origin comparisons MUST use canonical form. The `requestOrigin()` function MUST normalize as follows:

| Rule | Example |
|------|---------|
| Lowercase hostname | `HTTP://Piccolo.Local` → `http://piccolo.local` |
| Omit default ports | `http://piccolo.local:80` → `http://piccolo.local` |
| Omit default ports | `https://portal.example.com:443` → `https://portal.example.com` |
| Keep non-default ports | `http://piccolo.local:8080` → `http://piccolo.local:8080` |
| No trailing slash | `http://piccolo.local/` → `http://piccolo.local` |
| No trailing dot | `http://piccolo.local.` → `http://piccolo.local` |
| IPv6 in brackets | `http://[::1]:8080` → `http://[::1]:8080` |

```go
func canonicalOrigin(r *http.Request, trustForwardedProto bool) string {
    scheme := "http"
    if r.TLS != nil {
        scheme = "https"
    } else if trustForwardedProto && r.Header.Get("X-Forwarded-Proto") == "https" {
        // ONLY trust X-Forwarded-Proto when explicitly configured
        scheme = "https"
    }

    host := strings.ToLower(strings.TrimSuffix(r.Host, "."))

    // Strip default port, handling IPv6 brackets correctly
    if h, p, err := net.SplitHostPort(host); err == nil {
        isDefaultPort := (scheme == "http" && p == "80") || (scheme == "https" && p == "443")
        if isDefaultPort {
            // Re-bracket IPv6 addresses (SplitHostPort strips brackets)
            if strings.Contains(h, ":") {
                host = "[" + h + "]"
            } else {
                host = h
            }
        }
        // Non-default port: keep original host (already has brackets if IPv6)
    }
    return scheme + "://" + host
}
```

**⚠️ X-Forwarded-Proto trust:**

The `trustForwardedProto` parameter MUST be set based on explicit configuration, NOT auto-detected:
- **Direct access:** `trustForwardedProto = false` (ignore header, use `r.TLS`)
- **Behind trusted reverse proxy:** `trustForwardedProto = true` (proxy terminates TLS)

Trusting `X-Forwarded-Proto` without explicit configuration is a security vulnerability: an attacker could send `X-Forwarded-Proto: https` to bypass Secure cookie requirements or confuse origin binding.

**Validation:**

```go
// Portal validates portal-audience sessions only
func validatePortalSession(sess Session, requestOrigin string) bool {
    return sess.Audience == "portal" && sess.BoundOrigin == requestOrigin
}

// Proxy validates app-audience sessions only
func validateAppSession(sess Session, appName string, requestOrigin string) bool {
    return sess.Audience == "app:" + appName && sess.BoundOrigin == requestOrigin
}
```

**Security properties:**
- Session from `immich-piccolo.local` cannot authenticate on `piccolo.local`
- Session from `immich-piccolo.local` cannot authenticate on `nextcloud-piccolo.local`
- Even if session ID is stolen and manually set, audience mismatch causes rejection
- Even if session ID is stolen and set on a different access origin (alias vs LAN vs Remote), origin mismatch causes rejection

### 6.3 Session Lifecycle

| Event | Action |
|-------|--------|
| Portal login | Create session with `audience: "portal"` |
| Proxy OIDC callback | Create session with `audience: "app:<name>"`, store `parent_session_id` |
| Portal logout | Invalidate portal session AND derived app sessions (same parent) |
| App logout (`/__piccolod_oidc/logout`) | Invalidate that app session only |
| OIDC token revocation | Invalidate related app sessions |

**Session TTL Model (Option C: Independent but Revocable):**

App sessions have **independent TTL** from the portal session that authorized them:
- App session gets the standard TTL (default: 1 hour) starting from creation time
- An app session created late in a portal session's lifetime may outlive that portal session
- This aligns with standard OIDC behavior (access tokens are independent once issued)
- Central revocation (logout propagation) handles the security requirement

This is intentional: if a user authenticates to an app 55 minutes into their 1-hour portal session, the app session gets a full hour, not 5 minutes.

**Logout propagation (scoped to parent session):**

Portal logout invalidates only app sessions derived from that specific portal session:

```go
func (s *SessionStore) LogoutPortalSession(portalSessionID string) {
    // 1. Invalidate the portal session itself
    s.Invalidate(portalSessionID)

    // 2. Invalidate only app sessions with this parent
    for _, sess := range s.FindByParent(portalSessionID) {
        s.Invalidate(sess.ID)
    }
}
```

**Implications:**
- Logging out on phone does NOT log you out on desktop (different portal sessions)
- Logging out on LAN does NOT invalidate Remote app sessions (different portal sessions)
- "Logout everywhere" (if implemented) would explicitly invalidate all user sessions

Note: Invalidated app session cookies remain in browser but fail validation. The browser initiates a new OIDC flow, which redirects to login (no portal session).

### 6.4 Separate LAN and Remote Sessions

Users maintain separate sessions for LAN and Remote access (unchanged from RFC 20260112 §4.1.7):

| Access Context | Portal Session | App Sessions |
|----------------|----------------|--------------|
| LAN | `piccolo.local` | `immich-piccolo.local`, etc. |
| Remote | `portal.example.com` | `immich.portal.example.com`, etc. |

Logging in on LAN does not create a Remote session, and vice versa.

## 7. Auth Strategy Compatibility

With proxy-level OIDC, all strategies work across all access paths:

| Strategy | LAN 2-level | LAN port | Remote | Alias |
|----------|-------------|----------|--------|-------|
| `public` | Yes | Yes | Yes | Yes |
| `oidc_passthrough` | Yes | Yes | Yes | Yes |
| `headers` | Yes | Yes | Yes | **Yes** ✓ |
| `protected` | Yes | Yes | Yes | **Yes** ✓ |

**Improvement:** `headers` and `protected` now work with alias domains (previously unsupported per RFC 20260112 §4.1.9).

## 8. Implementation Changes

### 8.1 mDNS Package (`internal/mdns/`)

**`names.go`:**
```go
// Change FQDN construction from dot to hyphen separator
func (r *NameRegistry) rebuildLocked() {
    // Before: label + "." + r.baseName + "." + localTLD + "."
    // After:  label + "-" + r.baseName + "." + localTLD + "."
    for label := range r.aliases {
        name := label + "-" + r.baseName + "." + localTLD + "."
        fqdns[name] = struct{}{}
    }
}
```

### 8.2 Hostname Package (`internal/hostname/`)

**`hostname.go`:**
```go
// Update NormalizeHostLabel for hyphen separator
func NormalizeHostLabel(hostname, base string) string {
    hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
    base = strings.ToLower(strings.TrimSuffix(base, "."))

    suffix := "-" + base
    if strings.HasSuffix(hostname, suffix) {
        return strings.TrimSuffix(hostname, suffix)
    }
    return ""
}
```

### 8.3 Server Package (`internal/server/`)

**`gin_middleware.go` - LAN host routing:**
```go
func (s *GinServer) lanHostRoutingMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        // ...
        baseSuffix := "-" + strings.ToLower(lanBase)  // Changed from "."
        if !strings.HasSuffix(reqHost, baseSuffix) {
            c.Next()
            return
        }
        // ...
    }
}
```

**`gin_auth_handlers.go` - Simplified cookie domain:**
```go
func (s *GinServer) sessionCookieDomain(r *http.Request) string {
    // Always return empty for host-only cookies
    return ""
}
```

### 8.4 Services Package (`internal/services/`)

**New file `proxy_oidc.go`:**
- OIDC client initialization
- Authorization redirect generation
- Callback handler
- Token exchange
- Session creation with audience binding

**`manager.go` - Auto-register proxy OIDC clients:**
```go
func (m *ServiceManager) AllocateForApp(appName string, listeners []api.AppListener) error {
    // ...
    if needsProxyOIDC(listeners) {
        clientID, clientSecret := m.oidcProvider.RegisterProxyClient(appName)
        m.storeProxyCredentials(appName, clientID, clientSecret)
    }
    // ...
}

func needsProxyOIDC(listeners []api.AppListener) bool {
    for _, l := range listeners {
        if l.Auth != nil {
            for _, rule := range l.Auth.Rules {
                if rule.Strategy == "headers" || rule.Strategy == "protected" {
                    return true
                }
            }
        }
    }
    return false
}
```

**`types.go` - Session with audience:**
```go
type ProxySession struct {
    ID        string
    UserID    string
    Username  string
    Email     string
    Role      string
    Audience  string    // "portal" | "app:<name>"
    ExpiresAt time.Time
}
```

### 8.5 OIDC Package (`internal/oidc/`)

**`client.go` - Proxy client registration:**
```go
func (p *Provider) RegisterProxyClient(appName string) (clientID, clientSecret string, err error) {
    clientID = "piccolo-" + appName + "-proxy"
    clientSecret = generateSecureSecret()

    // Store with type marker
    err = p.store.SaveClient(Client{
        ID:           clientID,
        SecretHash:   hashSecret(clientSecret),
        Type:         "proxy",  // Distinguishes from app-declared clients
        AppName:      appName,
        RedirectURIs: []string{},  // Dynamically resolved
    })

    return clientID, clientSecret, err
}
```

**`discovery.go` - Dynamic redirect URI validation:**
```go
func (p *Provider) ValidateRedirectURI(clientID, redirectURI string) bool {
    client, ok := p.store.GetClient(clientID)
    if !ok {
        return false
    }

    if client.Type == "proxy" {
        // Dynamically compute valid URIs for proxy clients
        validURIs := p.computeProxyRedirectURIs(client.AppName)
        return contains(validURIs, redirectURI)
    }

    // Explicit client: check declared URIs
    return contains(client.RedirectURIs, redirectURI)
}
```

## 9. Migration

### 9.1 Hostname Transition

The 3-level format (`app.piccolo.local`) is removed immediately. There are no active deployments requiring backward compatibility.

**Changes:**
- mDNS advertises only 2-level format (`app-piccolo.local`)
- LAN host routing only matches 2-level format
- Documentation uses 2-level format exclusively

### 9.2 Session Migration

Existing `oidc_passthrough` apps are unaffected. New proxy OIDC clients are registered automatically for `headers`/`protected` apps on next reconcile.

## 10. Security Considerations

### 10.1 Session Isolation

- **Browser enforcement:** Host-only cookies prevent cross-domain transmission
- **Server enforcement:** Audience + origin binding prevents replay across apps and access origins
- **Defense in depth:** Both layers must be bypassed for cross-app session hijacking

### 10.2 OIDC Security

- **PKCE required:** All proxy OIDC flows use PKCE (S256)
- **State validation:** CSRF protection via state parameter
- **Back-channel token exchange:** Tokens never exposed to browser
- **Short-lived tokens:** Access tokens expire in 15 minutes

### 10.3 Reserved Path Protection

The `/__piccolod_oidc/` path prefix is:
- Intercepted by proxy before reaching backends
- Never forwarded to app containers
- Used only for proxy-level operations

### 10.4 Credential Storage

Proxy OIDC client secrets are:
- Stored in encrypted control volume
- Hashed with Argon2id (same as explicit OIDC clients)
- Per-app isolation (compromise of one doesn't affect others)

## 11. App-Declared OIDC Client Redirect URIs

This section standardizes redirect URI handling for app-declared OIDC clients (`oidc_passthrough` strategy), separate from the proxy OIDC clients described in section 5.

> **Implementation Status:** This section has been implemented. The implementation currently uses dot-separator hostnames (`immich.piccolo.local`) pending the 2-level hostname migration from Section 4.

### 11.1 Background: OAuth Redirect URI Security

OAuth 2.0 (RFC 6749) and the Security BCP (RFC 9700) require **exact string matching** for redirect URIs to prevent open redirector attacks. The previous implementation used origin-based validation (accepting any path on valid origins), which was non-compliant.

### 11.2 Manifest Schema Change

Apps using `oidc_passthrough` strategy must declare callback paths in their manifest:

```yaml
services:
  main:
    oidc_client:
      redirect_uri_paths:    # REQUIRED: callback path segments
        - /callback
        - /oauth/callback
      redirect_uris:         # Optional: explicit URIs for native apps (RFC 8252)
        - "myapp://callback"
        - "http://localhost:8081/callback"
      ca_mount_path: /etc/ssl/certs/piccolo-internal-ca.crt
      env:
        ISSUER_URL: "{{ .System.Auth.Issuer }}"
        # ...
```

**Validation rules for `redirect_uri_paths`:**
- Must be a non-empty list (YAML validation fails otherwise)
- Each path must start with `/`
- Paths must not contain query strings (`?`) or fragments (`#`)
- No normalization is performed (case-sensitive per RFC 3986 §6.2.1)

### 11.3 URI Generation Algorithm

Piccolo generates the complete list of valid redirect URIs using the cartesian product:

```
Valid Redirect URIs = (All Valid Origins) × (Declared Paths) ∪ (Explicit URIs)
```

**Valid Origins** are dynamically computed from:
- Remote hostname: `https://<app>.<portal-base>`
- Alias domains: `https://<alias-domain>`
- LAN host-based: `http://<app>.<base>` (will become `http://<app>-<base>` after Section 4)
- LAN port-based: `http://<base>:<port>`
- Local IP addresses: `http://<local-ip>:<port>` (for mDNS-less clients)

**Example:** App `immich` declares `redirect_uri_paths: ["/callback", "/oauth/callback"]`

| Origin Type | Origin | Generated URIs |
|-------------|--------|----------------|
| Remote | `https://immich.portal.example.com` | `https://immich.portal.example.com/callback`, `.../oauth/callback` |
| Alias | `https://photos.mydomain.com` | `https://photos.mydomain.com/callback`, `.../oauth/callback` |
| LAN host | `http://immich.piccolo.local`* | `http://immich.piccolo.local/callback`, `.../oauth/callback` |
| LAN port | `http://piccolo.local:12345` | `http://piccolo.local:12345/callback`, `.../oauth/callback` |
| Local IP | `http://192.168.1.50:12345` | `http://192.168.1.50:12345/callback`, `.../oauth/callback` |

*Will become `http://immich-piccolo.local` after Section 4 hostname migration.

### 11.4 Implementation Details

```go
// collectRedirectConfig extracts paths and explicit URIs from app manifest
func (s *GinServer) collectRedirectConfig(ctx context.Context, appID string) (paths, explicitURIs []string)

// buildValidOrigins constructs all valid redirect origins for an app
func (s *GinServer) buildValidOrigins(endpoints []services.ServiceEndpoint) []validOrigin

// resolveAppRedirectURI generates the exhaustive list via cartesian product
func (s *GinServer) resolveAppRedirectURI(ctx context.Context, appID string) ([]string, error) {
    paths, explicitURIs := s.collectRedirectConfig(ctx, appID)
    origins := s.buildValidOrigins(endpoints)

    // Generate: origins × paths
    var uris []string
    for _, origin := range origins {
        for _, path := range paths {
            uris = append(uris, origin.toBaseURL()+path)
        }
    }

    // Append explicit URIs (native apps)
    uris = append(uris, explicitURIs...)
    return uris, nil
}
```

### 11.5 Rationale

This approach ensures full compliance with OAuth 2.0 exact URI matching (RFC 6749, RFC 9700):

| Property | Benefit |
|----------|---------|
| **Exact matching** | Compliant with OAuth 2.0 spec; no fuzzy origin-based validation |
| **Explicit paths** | App developers declare exactly which callback paths their app uses |
| **Dynamic origins** | Piccolo handles the complexity of multiple access origins transparently |
| **No normalization** | Case-sensitive matching per RFC 3986 §6.2.1 prevents bypass attacks |

### 11.6 Comparison: Proxy vs App-Declared OIDC Clients

| Aspect | Proxy OIDC (§5) | App-Declared OIDC (§11) |
|--------|-----------------|------------------------|
| **Strategy** | `headers`, `protected` | `oidc_passthrough` |
| **Client registration** | Auto-generated at install | Declared in manifest |
| **Callback path** | Fixed: `/__piccolod_oidc/callback` | Declared via `redirect_uri_paths` |
| **URI generation** | Origins × fixed path | Origins × declared paths |
| **Who handles auth** | Piccolo proxy | App itself |

## 12. Relationship to Other RFCs

| RFC | Relationship |
|-----|--------------|
| RFC 20260114 | **Amends:** Changes LAN hostname format from 3-level to 2-level |
| RFC 20260112 | **Amends:** Removes alias domain limitation for `headers`/`protected`; changes cookie domain model; adds `redirect_uri_paths` manifest field |
| RFC 20260106 | **Extends:** Adds proxy-level OIDC client pattern |

## 13. Implementation Plan

1. **Phase 0: App-declared OIDC redirect URIs (§11)** ✅ **COMPLETED**
   - Add `redirect_uri_paths` field to `ServiceOIDCClient` in `internal/api/types.go`
   - Add validation in `internal/app/parser.go` (non-empty, paths start with `/`, no query/fragment)
   - Refactor `resolveAppRedirectURI` in `internal/server/gin_oidc_handlers.go` to use cartesian product
   - Update `docs/app-platform/specification.yaml` with new field documentation

2. **Phase 1: Core hostname changes**
   - Update `internal/hostname/` for 2-level parsing
   - Update `internal/mdns/` for 2-level FQDN construction
   - Update `internal/server/` for 2-level routing
   - Update `buildValidOrigins()` to use hyphen separator for LAN hosts
   - Update tests for new hostname format

3. **Phase 2: Session architecture**
   - Add audience field to session types
   - Update session validation logic
   - Simplify cookie domain to always host-only
   - Implement logout propagation (portal logout invalidates app sessions)

4. **Phase 3: Proxy OIDC client**
   - Implement `internal/services/proxy_oidc.go`
   - Add auto-registration for `headers`/`protected` apps
   - Implement callback handler and session creation
   - Add `/__piccolod_oidc/callback` route interception

5. **Phase 4: Testing and documentation**
   - Add comprehensive tests for all access paths (LAN 2-level, Remote, Alias)
   - Test session isolation and audience binding
   - Test logout propagation
   - Update documentation

## 14. Open Questions

1. ~~**Transition period duration:**~~ **Resolved:** No transition period. 3-level format removed immediately (no active deployments).

2. ~~**Session TTL alignment:**~~ **Resolved:** Option C (independent but revocable). App sessions have independent TTL; central revocation handles security. See §6.3.

3. ~~**Logout propagation:**~~ **Resolved:** Scoped to parent session. Portal logout only invalidates app sessions derived from that specific portal session. See §6.3.

4. ~~**Cookie namespace:**~~ **Resolved:** The `piccolo_` prefix is reserved. Proxy strips `piccolo_`-prefixed cookies from backend responses. See §6.1.

5. ~~**Cookie security attributes:**~~ **Resolved:** `Secure` flag set for HTTPS contexts. Origin binding intentionally treats HTTP/HTTPS as different. See §6.1.

6. ~~**Back-channel communication:**~~ **Resolved:** Proxy uses in-process interfaces (not network calls) for token exchange and userinfo. See §5.6.

7. ~~**Validation constraints:**~~ **Resolved:** 16-char limit for app/listener names. Reserved names aligned with parser.go. See §4.3.

8. **Rate limiting:** Should proxy OIDC flows have separate rate limits from portal login? (TBD)

## 15. Appendix: Example Flows

> **Note:** These examples show the target 2-level hostname format (`immich-piccolo.local`). The current implementation uses dot-separator (`immich.piccolo.local`) until Section 4 is implemented.

### A.1 LAN Access with 2-Level Domain

```
User browses to: http://immich-piccolo.local/photos

1. Browser has no cookie for immich-piccolo.local
2. Proxy redirects to: http://piccolo.local/oauth/authorize?...
3. Browser has piccolo_session cookie for piccolo.local
4. OIDC server validates session, issues auth code
5. Redirect to: http://immich-piccolo.local/__piccolod_oidc/callback?code=...
6. Proxy exchanges code for tokens
7. Proxy creates session (audience: "app:immich")
8. Set-Cookie: piccolo_session=xyz (host-only for immich-piccolo.local)
9. Redirect to: http://immich-piccolo.local/photos
10. Request proceeds with valid session
```

### A.2 Alias Domain Access

```
User browses to: https://photos.mydomain.com/albums
(alias for immich listener)

1. Browser has no cookie for photos.mydomain.com
2. Proxy redirects to: https://portal.example.com/oauth/authorize?...
   (Remote context detected)
3. Browser has piccolo_session cookie for portal.example.com
4. OIDC server validates session, issues auth code
5. Redirect to: https://photos.mydomain.com/__piccolod_oidc/callback?code=...
6. Proxy exchanges code for tokens
7. Proxy creates session (audience: "app:immich")
8. Set-Cookie: piccolo_session=xyz (host-only for photos.mydomain.com)
9. Redirect to: https://photos.mydomain.com/albums
10. Request proceeds with valid session
```

### A.3 Cross-App Session Isolation

```
User has sessions on:
- piccolo.local (audience: "portal")
- immich-piccolo.local (audience: "app:immich")

User browses to: http://nextcloud-piccolo.local/files

1. Browser has no cookie for nextcloud-piccolo.local
   (immich cookie not sent - different domain)
2. Proxy initiates OIDC flow
3. piccolo.local session used for OIDC approval
4. New session created (audience: "app:nextcloud")
5. Separate cookie set for nextcloud-piccolo.local
```

### A.4 Portal Logout Propagation

```
User has sessions on:
- piccolo.local (audience: "portal", id: "aaa")
- immich-piccolo.local (audience: "app:immich", id: "bbb")
- nextcloud-piccolo.local (audience: "app:nextcloud", id: "ccc")

User clicks logout on piccolo.local:

1. Portal receives POST /api/v1/auth/logout
2. Portal invalidates session "aaa" (portal)
3. Portal queries all sessions for this user
4. Portal invalidates session "bbb" (app:immich)
5. Portal invalidates session "ccc" (app:nextcloud)
6. Response: logged out

User browses to: http://immich-piccolo.local/photos

1. Browser sends cookie piccolo_session=bbb
2. Proxy validates session "bbb" → INVALID (was invalidated)
3. Proxy initiates OIDC flow
4. Redirect to piccolo.local/oauth/authorize
5. piccolo.local has no session → redirect to login page
6. User must re-authenticate
```

## 16. Implementation Notes & Status

- **Status:** Draft
- **Depends on:** RFC 20260112 (Listener Auth Rules), RFC 20260114 (Hostname Scheme)
- **Amends:** RFC 20260112 §4.1.9 (Alias Domains), RFC 20260112 §4.2 (adds `redirect_uri_paths`), RFC 20260114 §4 (Hostname Format)
