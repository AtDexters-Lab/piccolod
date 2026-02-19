# RCA: Remote portal HTTPS redirect loop after service restart

- **Status:** Resolved
- **Date:** 2026-02-19
- **Severity:** High (remote portal inaccessible until config reload event)
- **Environment:** Piccolo OS dev VM (VirtualBox), piccolod v0.1.24, remote access via nexus tunnel
- **Related:**
  - `internal/server/gin_server.go` (constructor ordering fix)
  - `internal/services/tlsmux.go` (TLS mux upstream routing)

## 1. Summary

After a `make service` rebuild/deploy, the remote access portal at `https://piccolo.abhishekborar.com/` entered an infinite redirect loop (`ERR_TOO_MANY_REDIRECTS`) in existing browser sessions. The portal worked in incognito mode. The root cause was a constructor initialization ordering bug: `refreshRemoteRuntime()` ran before `initSecureLoopback()`, so the TLS mux was configured to forward portal traffic to the main HTTP port (80) instead of the secure loopback. Requests arriving on port 80 lacked the `secureContextKey` context value, causing `isSecureRequest()` to return false and the HTTPS redirect middleware to emit 301s — which the browser cached permanently.

## 2. Timeline

Timestamps from `artifacts/logs/piccolo-dev.log`.

| Time | Event |
|---|---|
| 18:23:09 | piccolod starts. Remote config loaded (`portal=piccolo.abhishekborar.com`). |
| 18:23:09 | `refreshRemoteRuntime()` called — `securePort` is 0, so `resolvePortalPort()` returns 80. TLS mux configured with `portalPort=80`. |
| 18:23:09 | `initSecureLoopback()` called — binds ephemeral port, sets `securePort`. Too late. |
| 18:23:09 | TLS mux starts on `127.0.0.1:32985`. |
| 18:23:10 | First nexus connection arrives (port 443, tls:true). Resolver routes to tlsMuxPort. |
| 18:23:10–18:24:07 | All nexus connections → TLS mux → port 80 → no `secureContextKey` → 301 redirect. Browser caches 301. |
| 18:25:02 | `remote: reloaded config` — fires `TopicRemoteConfigChanged` event → `applyRemoteRuntimeFromStatus()` → `resolvePortalPort()` now returns `securePort` → TLS mux updated. |
| 18:25:20 | First successful 200 for GET "/" from nexus (new connection goes to secure loopback). |
| 18:25:20–18:28:06 | Mix of 200s (new connections → secure loopback) and 301s (old connections still piped to port 80 via HTTP keep-alive). |

## 3. Root Cause

**Initialization ordering bug in `NewGinServer()`.**

The `refreshRemoteRuntime()` call (line 759) ran before `initSecureLoopback()` (line 788). Inside `refreshRemoteRuntime()`, the call chain is:

```
refreshRemoteRuntime()
  → applyRemoteRuntimeFromStatus()
    → tlsMux.UpdateConfig(..., resolvePortalPort())
```

`resolvePortalPort()` prefers `s.securePort` when > 0, falling back to the `PORT` env var or 80. Since `initSecureLoopback()` hadn't run yet, `securePort` was 0, so the TLS mux was configured to forward portal cleartext HTTP to port 80 — the main HTTP listener.

The main HTTP listener does NOT set `secureContextKeyInstance` in the request context. Only the secure loopback handler does:

```go
// initSecureLoopback handler
handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    ctx := context.WithValue(r.Context(), secureContextKeyInstance, true)
    s.router.ServeHTTP(w, r.WithContext(ctx))
})
```

Without `secureContextKeyInstance`, `isSecureRequest()` returned false (and `r.TLS` was nil since the connection was cleartext from TLS mux). The `httpsRedirectMiddleware` then redirected to `https://` with status 301.

**Why it appeared as an infinite loop:** The browser followed the 301 redirect to `https://piccolo.abhishekborar.com/`, which went through the same nexus → TLS mux → port 80 path, producing another 301. The 301 was also cached by the browser (permanent redirect), so even after the config reload event fixed the TLS mux routing, existing browser sessions kept using the cached 301.

**Why incognito worked:** No cached 301 redirects.

**Why 200s eventually appeared:** At 18:25:02, the remote manager's config reload fired `TopicRemoteConfigChanged`, which re-invoked `applyRemoteRuntimeFromStatus()` — this time `securePort` was set, so the TLS mux was updated to forward to the secure loopback. New TCP connections after this point went to the correct port. However, old TCP connections (HTTP keep-alive from TLS mux to port 80) continued producing 301s until they were closed.

## 4. Impact

- Remote portal inaccessible after every service restart until a config reload event fires (typically 1-2 minutes).
- Once a 301 is cached by the browser, the session remains broken indefinitely (requires cache clear or incognito).
- LAN access via `piccolo.local` was unaffected (bypasses the HTTPS redirect middleware).

## 5. Resolution

### Immediate fixes

1. **Initialization ordering** (`gin_server.go`): Moved `initSecureLoopback()` before `refreshRemoteRuntime()` in `NewGinServer()` so `securePort` is known when the TLS mux is first configured. Added cleanup (`stopSecureLoopback`) on error paths after the move.

2. **301 → 307 redirect** (`gin_server.go`): Changed HTTPS redirect from `StatusMovedPermanently` (301, cached permanently by browsers) to `StatusTemporaryRedirect` (307, never cached). HSTS handles persistent HTTPS enforcement for returning clients. This prevents any future transient redirect from being permanently cached.

### Additional fixes (same changeset)

3. **Cache policy overhaul** (`static_cache.go`): Extension-based `no-cache` for code/data files (`.js`, `.wasm`, `.json`, `.html`, `.css`, `.bin`, `.map`) with ETag revalidation. Static assets (fonts, images, shaders) get `max-age=86400`.

4. **Custom PWA service worker** (`ui/web/sw.js`): Network-first strategy with offline SPA fallback, replacing Flutter's deprecated service worker that caused stale cache issues.

5. **Manifest/meta fixes** (`ui/web/index.html`, `ui/web/manifest.json`): Updated branding, colors, SW registration.

### Preventive

- The constructor now has a clear invariant: all port-dependent subsystems must be initialized before any configuration that reads those ports. The comment at the `initSecureLoopback` call site documents this dependency.

## 6. Remediation Status

- [x] Initialization ordering fix applied
- [x] 301 → 307 redirect change applied
- [x] Resource leak on error path addressed
- [x] Cache/PWA overhaul applied
- [ ] Pending commit and deploy verification
