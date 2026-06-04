# Piccolo Direct Browser Data Plane

**Date:** 2026-06-04
**Status:** Future draft

## Scope

**Problem:** Piccolo remote app traffic currently routes through a cloud node, which increases latency and cloud egress cost for home-server workloads.
**In scope:** A browser-to-piccolod direct data plane for already-loaded app pages, using WebRTC/DataChannel where possible, with cloud HTTPS as bootstrap, signaling, and fallback; v1 focus on JavaScript-initiated HTTP requests.
**Out of scope:** Replacing the remote cloud control plane, implementing a VPN, making service workers a Piccolo-owned primitive, transparent interception of all browser resource loads, WebSocket compatibility, backend HTTP/2 support, and changing app listener semantics.

This RFC is a draft tracking artifact for the "Piccolo Direct Mode" discussion. It intentionally does not propose HTTP/2 as the primary solution. HTTP/2 can improve the cloud path, but Direct Mode targets a different goal: moving eligible traffic off the cloud relay path after the page has bootstrapped.

---

## Background

Piccolo is a home-server product. The remote access path is valuable because it gives users a stable browser entry point without requiring router configuration, but routing every byte through a cloud node has two costs:

- Cloud relay egress and infrastructure cost.
- Added latency compared with a direct browser-to-device path.

The initial discussion started from client-facing HTTP/2. A proxy can speak HTTP/2 to the browser while speaking HTTP/1.1 to backends, but in Piccolo that requires careful TLS-mux and WebSocket handling. That work improves the remote HTTPS path, but it does not remove the relay from the data plane.

Direct Mode is the more strategically important track:

1. The page loads through the existing remote/cloud path.
2. Piccolo injects a small browser module into eligible app HTML.
3. The module establishes a WebRTC connection to piccolod using the cloud path only for signaling and fallback.
4. Eligible future requests are sent over the direct channel.
5. Unsupported requests continue over the normal cloud HTTPS path.

The design must remain honest about browser limits. A page-owned monkey-patch module can intercept JavaScript APIs such as `fetch`. It cannot transparently catch every browser resource request. A service worker can intercept resource loads, but Piccolo should not own app service workers in v1 because apps may already have their own service workers, caching/offline behavior is app-owned, and service workers are not a suitable owner for long-lived WebRTC peer connections.

---

## Design Principles

1. **Cloud remains the bootstrap and fallback path.** Direct Mode must never make an app unreachable when WebRTC fails, NAT traversal fails, or browser support is missing.
2. **Piccolo owns transport acceleration, not app caching.** Apps keep ownership of service workers, offline behavior, and cache policy.
3. **Do not bypass Piccolo's app boundary.** Direct requests must be authorized and routed as if they reached the same app listener through the existing Piccolo access model.
4. **Start with a narrow transparent surface.** V1 should target `window.fetch` for same-origin app requests. XHR, WebSocket, EventSource, parser resource loads, and navigations are later compatibility layers.
5. **Prefer opt-in and observability before default rollout.** Direct Mode should begin behind device/app/listener feature gates with metrics for direct success rate, fallback rate, latency, and relay savings.
6. **Do not make HTTP/2 a dependency.** Direct Mode gets request multiplexing from its own framing over a DataChannel. Backend apps can remain HTTP/1.1.

---

## Core Model

### Direct session

A Direct session is an authenticated browser-to-piccolod channel associated with:

- device identity;
- browser session or app access grant;
- app and listener identity;
- remote origin that bootstrapped the page;
- direct-mode capabilities negotiated by the browser module and piccolod.

The session is established only after the browser has loaded an eligible page through the normal Piccolo route. The bootstrap response can include a short-lived Direct token or signaling grant. That grant must be origin-bound and listener-bound so it cannot be reused against another app or another device.

### Browser module

The v1 browser module lives in the page/window context and owns:

- WebRTC peer connection setup;
- one or more RTCDataChannels;
- direct request framing;
- fallback decisions;
- monkey-patching `window.fetch`;
- metrics and health pings.

The module does not own:

- app service workers;
- browser cache policy;
- app auth flows;
- complete browser network interception.

### Piccolod Direct gateway

Piccolod needs a Direct gateway that accepts DataChannel request frames and maps them to the intended app listener.

The gateway must preserve the existing Piccolo security model. It must not become a separate unauthenticated route to app containers. Future implementation should decide whether the gateway:

- re-enters the existing L7 proxy handler for the target app listener; or
- uses a dedicated internal handler that is mechanically equivalent to the existing L7 chain.

The first shape is preferable if it can be implemented without duplicating auth, header, cookie, and response-modifier behavior.

### Direct request framing

The Direct channel needs a small HTTP-like framing protocol.

Required request fields:

- request ID;
- method;
- URL path and query, normalized to same-origin app routes;
- app/listener identity or a session-bound implicit route;
- selected request headers;
- body chunks, if any;
- abort/cancel signal.

Required response fields:

- request ID;
- status code;
- response headers;
- body chunks;
- error/fallback reason.

The framing layer must support concurrent in-flight requests. It should not assume request-response serialization.

---

## Decisions

### D1 - Direct Mode is an opportunistic data-plane optimization

Direct Mode is not a replacement for the Piccolo remote access path. The cloud path remains required for:

- first page load;
- auth and setup flows;
- WebRTC signaling;
- fallback when direct connectivity fails;
- clients or environments where WebRTC is unavailable or blocked;
- TURN relay fallback when direct ICE candidates cannot connect.

The product contract should be:

> Piccolo may accelerate eligible browser requests over a direct browser-to-device channel. If Direct Mode is unavailable, the app continues to work through normal remote HTTPS.

### D2 - V1 uses a page-owned monkey-patch module, not a Piccolo-owned service worker

V1 should not install or rely on a Piccolo-owned service worker.

Reasons:

- A browser origin can have only one active service worker per scope. Many apps may already own this surface.
- Service workers are app caching/offline infrastructure, not Piccolo transport infrastructure.
- Service worker lifecycle is event-driven and not a good fit for owning a persistent WebRTC connection.
- WebRTC peer connection ownership is cleanest in a normal page/window context.

The v1 module should be injected into eligible HTML responses and execute early enough to patch app JavaScript APIs before most app code runs.

### D3 - V1 intercepts `fetch`; XHR is a follow-up

V1 should target JavaScript-initiated `fetch` requests first.

Eligibility rules should be conservative:

- same-origin only;
- ordinary HTTP methods;
- no unsupported request body type;
- no unsupported response mode;
- explicit fallback on any ambiguity;
- preserve abort behavior where possible;
- collect fallback reasons for future compatibility work.

`XMLHttpRequest` is a near-term follow-up because many older apps still use it. It should use the same Direct framing and fallback policy.

### D4 - Browser parser resource loads are not covered in v1

Monkey-patching `fetch` does not catch browser-initiated resource loads such as:

- `<img src>`;
- `<script src>`;
- `<link rel="stylesheet">`;
- CSS `url(...)`;
- fonts;
- media;
- iframes;
- parser preloads;
- full navigations.

Piccolo could attempt response rewriting and DOM/API monkey-patching to cover some of these later, but that becomes fragile quickly:

- script execution ordering;
- module loading;
- stylesheet imports;
- `srcset`;
- Subresource Integrity;
- Content Security Policy;
- redirects;
- range requests;
- streaming media;
- browser preload scanners.

V1 should not promise transparent interception of these resource classes.

### D5 - WebSocket compatibility is out of scope for v1

Existing apps can serve HTTP and WebSocket routes from the same listener. Direct Mode must not assume listener-level separation.

V1 should leave browser WebSocket traffic on the existing cloud HTTPS path. A later phase may monkey-patch `window.WebSocket` and provide a WebSocket-like compatibility layer over the Direct channel.

That future work must handle:

- constructor compatibility;
- readyState transitions;
- binary type behavior;
- close codes and reasons;
- backpressure;
- subprotocol negotiation;
- error semantics;
- interaction with existing app reconnect logic.

### D6 - Cookie and session semantics are a first-class design risk

Monkey-patched `fetch` cannot simply read and forward all browser cookies. In particular, JavaScript cannot read HttpOnly cookies.

This matters because app backends may rely on app-owned HttpOnly cookies, and Piccolo's L7 proxy already rewrites and isolates cookies in some access modes.

Possible solutions:

1. **Server-side Direct cookie jar:** piccolod associates the Direct session with the browser cookies it observed on the bootstrap/cloud path, updates that jar when Direct responses set cookies, and attaches the correct cookie header when replaying Direct requests.
2. **Direct token auth only:** Direct requests use a Piccolo-issued session token and are limited to apps or routes that do not depend on app HttpOnly cookies.
3. **Hybrid:** start with token-authenticated public/asset/API routes and add server-side cookie mirroring once the behavior is proven.

This RFC does not settle the cookie model. It marks it as a blocking design question before Direct Mode can be enabled for arbitrary authenticated apps.

### D7 - Direct routing must preserve Piccolo auth boundaries

The Direct gateway must verify:

- the Direct session is valid and not expired;
- the session is bound to the origin/app/listener that bootstrapped it;
- the requested URL is same-origin relative to that app listener;
- the app is still installed/running and the listener still exists;
- any Piccolo app authorization still holds.

Direct Mode must fail closed when the app is stopped, access is revoked, the session expires, or routing no longer matches.

### D8 - TURN fallback is useful but changes the economics

WebRTC may fail to find a direct path because of NAT, firewalls, corporate networks, or browser policy. TURN fallback can preserve connectivity, but TURN relays data and therefore reintroduces relay cost.

Direct Mode metrics must distinguish:

- direct peer-to-peer success;
- TURN-relayed success;
- cloud HTTPS fallback;
- setup failure;
- request-level fallback after setup.

Without this distinction, Piccolo cannot know whether Direct Mode is reducing cloud cost or merely moving traffic from one relay shape to another.

---

## Access Path Matrix

| Traffic class | V1 Direct behavior | Notes |
| --- | --- | --- |
| Initial document navigation | Cloud path | Needed for bootstrap and injection. |
| Same-origin `fetch` | Direct when eligible, cloud fallback | Primary v1 target. |
| XHR | Cloud path initially | Follow-up using same framing. |
| Browser resource loads | Cloud path | Not caught by `fetch` monkey patch. |
| WebSocket | Cloud path | Future monkey-patch compatibility layer. |
| EventSource/SSE | Cloud path initially | Needs streaming semantics before Direct. |
| File upload/download | Cloud path unless explicitly supported | Needs streaming, abort, and backpressure tests. |
| `flow:tls` apps | Out of scope | App owns TLS and HTTP behavior. |
| raw TCP listeners | Out of scope | Existing tunnel/mTLS model remains separate. |

---

## What We Gain

- Reduced cloud relay traffic for eligible app runtime requests.
- Lower latency when direct ICE succeeds.
- Multiplexed request handling independent of HTTP/2.
- Backend apps can remain HTTP/1.1.
- The feature can roll out opportunistically with cloud fallback.
- WebSocket and full resource interception can be deferred without blocking a useful first version.

---

## What We Lose or Take On

- Piccolo becomes responsible for a browser compatibility shim.
- Request semantics must be rebuilt carefully for fetch/XHR.
- Cookie behavior is non-trivial because HttpOnly cookies are not visible to JavaScript.
- Direct Mode will not cover every byte on the page in v1.
- WebRTC success rate depends on network environment.
- TURN fallback may still cost money.
- Response injection must be safe across HTML shapes and CSP settings.
- Metrics, diagnostics, and fallback behavior become product-critical.

---

## Affected Modules and APIs

Likely future touch points:

- `internal/services/proxy.go` and L7 middleware: injection point, request equivalence, response handling.
- `internal/services/middleware/l7/*`: potential Direct bootstrap response modifier and cookie/session integration.
- `internal/server/*`: Direct signaling endpoints and Direct gateway endpoints.
- `internal/remote/*`: signaling through existing remote/cloud path, if appropriate.
- `ui/` or static web assets: Direct module packaging if served as a Piccolo-owned asset.
- App platform docs: Direct Mode feature gates and compatibility contract.

No implementation is proposed in this draft yet.

---

## Rollout Plan

1. Add a feature flag disabled by default.
2. Build a minimal signaling and Direct session handshake for one test app.
3. Inject a page module only for an explicit app/listener allowlist.
4. Implement `fetch` interception for same-origin GET/POST requests with cloud fallback.
5. Add metrics for setup success, direct request success, fallback reason, latency, and bytes by path.
6. Test with public routes, protected routes, app cookies, large responses, aborts, and app restart/revoke cases.
7. Decide whether XHR or cookie mirroring is the next blocker before any wider rollout.

---

## Open Questions

1. Should Direct Mode be configured per device, per app, per listener, or per route?
2. What is the minimal safe cookie/session model for authenticated apps?
3. Can the Direct gateway reuse the existing L7 proxy handler without duplicating auth and cookie behavior?
4. How should CSP be adjusted so the injected Direct module can run without weakening app security more than necessary?
5. Should Direct signaling use existing Piccolo remote endpoints, a new endpoint namespace, or the app listener origin?
6. What browser support baseline is acceptable?
7. What metrics prove Direct Mode is reducing cloud cost rather than shifting traffic to TURN?
8. When should XHR support enter the scope?
9. Which app examples should be used as compatibility fixtures?
10. Is WebSocket monkey-patching product-worthy, or should WebSocket remain on the cloud path until apps opt in?

---

## Implementation Notes & Status

Status as of 2026-06-04: draft only. No code has landed.

This RFC captures the discussion outcome:

- HTTP/2 is not the primary architecture track for reducing cloud relay cost.
- Piccolo Direct Mode is the more relevant goal.
- V1 should avoid Piccolo-owned service workers.
- V1 should start with a page-owned monkey-patch module and `fetch` interception.
- WebSocket compatibility, browser parser resource loads, and arbitrary app service-worker integration are deferred.
- Cookie/session handling is a blocking design question before arbitrary authenticated app rollout.
