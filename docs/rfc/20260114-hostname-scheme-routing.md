# RFC: Hostname Scheme & Host-Based Routing (LAN + Remote)

- **Status:** Draft
- **Date:** 2026-01-14
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core

## 1. Summary

This RFC proposes a unified hostname scheme for HTTP/WebSocket listeners on both LAN and Remote (Nexus) access, plus optional host-based routing on LAN.

Key outcomes:
1. **Per-app hostnames** that are stable and conflict-free (`<app>.<base>`).
2. **Multi-listener hostnames** that remain wildcard-cert compatible (`<listener>-<app>.<base>`).
3. **LAN host-based routing (HTTP only)** to simplify URLs and eliminate LAN cookie-domain collisions without cookie rewriting.
4. **Port-based routing remains supported** on LAN for compatibility and fallback (`<host>:<port>`), including when mDNS is unreliable or disabled.

This RFC intentionally scopes hostname changes to HTTP visibility (HTTP + WebSocket). Raw/TCP listeners continue to use port-based routing on LAN.

## 2. Motivation

### 2.1 Fix Remote Hostname Collisions

Remote routing currently derives service hostnames from listener names alone (e.g., `web.<tld>`). Since many apps naturally use `web`, this creates collisions and ambiguous routing.

### 2.2 Fix LAN Cookie Isolation Correctly

In LAN mode, port-based routing (`piccolo.local:<port>`) makes all apps share a single cookie domain. This causes:
- cookie name collisions (breakage),
- cross-app cookie visibility in the browser (for non-HttpOnly cookies),
- brittle workarounds (cookie rewriting).

Host-based routing provides real isolation because cookies are scoped by hostname.

### 2.3 Improve UX and Operability

Host-based URLs are easier to remember (`immich.piccolo.local`) and reduce the number of ports users need to remember (often just 80/443), while still retaining port-based access for compatibility and fallback.

## 3. Goals & Non-Goals

### 3.1 Goals

- Provide a **conflict-free** hostname scheme across apps.
- Support **multiple listeners** per app without requiring multi-level wildcards.
- Keep Remote TLS simple with a single wildcard certificate (`*.<base>`), plus an apex certificate for `<base>`.
- Allow LAN access that does not depend on router/DNS configuration (mDNS-first).
- Maintain **port-based access** for reliability and compatibility.

### 3.2 Non-Goals

- Eliminate port-based routing on LAN (port-based URLs continue to work).
- Guarantee mDNS reliability on every network (Piccolo should degrade gracefully).
- Introduce a full LAN DNS server / DHCP integration in v1.

## 4. Proposed Hostname Scheme

### 4.1 Base Domains

**LAN base:** the device mDNS hostname (e.g., `piccolo.local` or `piccolo-xyz.local` if conflict).

**Remote base (device base domain):** the portal hostname itself (apex), e.g., `piccolo-xyz.example.com`.

Portal is served from the remote base domain apex:
- Portal: `https://<base>`
- Apps: `https://<app>.<base>` and `https://<listener>-<app>.<base>`

### 4.2 Hostname Format

For each app `<app>`:

- **Primary HTTP/WebSocket listener:**  
  `host = <app>.<base>`

- **Additional HTTP/WebSocket listeners:**  
  `host = <listener>-<app>.<base>`

Examples:
- `immich.piccolo.local`
- `metrics-immich.piccolo.local`
- `immich.piccolo-xyz.example.com`
- `metrics-immich.piccolo-xyz.example.com`

This uses a single DNS label (left-most) for both primary and secondary listeners, preserving compatibility with a single-level wildcard certificate on Remote (`*.<base>`).

### 4.3 “Primary” Listener Selection

Each app’s listeners MUST have exactly one “primary” HTTP/WebSocket listener when using host-based URLs.

Proposal:
- Add optional `listeners[].primary: bool`.
- If omitted, the first HTTP/WebSocket listener in manifest order is treated as primary.
- Multiple `primary: true` entries MUST be rejected.
- `protocol: raw` and `flow: tls` listeners are never eligible for primary host-based URLs.

### 4.4 Validation Requirements

- App names and listener names MUST be single DNS labels and MUST be validated:
  - Lowercase only
  - Starts with a letter
  - Contains only `[a-z0-9]` (no `-`)
  - Length ≤ 31 characters
  - Reserved names: `piccolo`, `piccoloos`

These constraints ensure:
- `<listener>-<app>` is unambiguous (split on `-`), and
- the left-most DNS label stays within 63 characters (`31 + 1 + 31 = 63`).

Piccolo MUST compute all derived hostnames for an app (`<app>.<base>` and `<listener>-<app>.<base>`) and reject any duplicates (including collisions with the portal hostname).

Piccolo MUST reject configurations that would produce ambiguous or invalid hostnames.

## 5. Routing Behavior

### 5.1 Remote (Nexus)

Remote traffic is already routed by hostname (SNI/Host). Piccolo must be able to map a request hostname to a specific `(app, listener)` endpoint:

**Resolution:**
1. If hostname matches `<base>` → route to portal server.
2. Else, if hostname matches `<app>.<base>` → route to app primary HTTP/WebSocket listener.
3. Else, if hostname matches `<listener>-<app>.<base>` → route to that specific listener for that app.
4. Else → no route (404/NoRoute).

This requires updating remote hostname derivation, certificate queueing, and the hostname-to-endpoint resolver to be app-aware (not listener-only).

### 5.2 LAN (HTTP/WebSocket)

Piccolo MUST retain port-based LAN routing behavior for all HTTP/WebSocket listeners:
- Requests arriving on an allocated listener public port continue to route by port (`http://<host>:<port>`), where `<host>` may be:
  - the device mDNS hostname (`piccolo.local` / `piccolo-xyz.local`)
  - a local IP (`192.168.x.y`)
  - `localhost` (dev)
  - an app hostname alias (`<app>.<lan-base>`) if advertised via mDNS

Piccolo MAY additionally expose a shared LAN HTTP entrypoint:
- `0.0.0.0:80` (and optionally `:443` if/when local TLS is supported),
- route by `Host` header:
  - `<lan-base>` → portal
  - `localhost` / local-machine IPs → portal (fallback when mDNS is unavailable/disabled)
  - `<app>.<lan-base>` → app primary listener
  - `<listener>-<app>.<lan-base>` → specific listener
  - unknown host → 404/NoRoute

### 5.3 LAN (Non-HTTP)

For `protocol: raw` or `flow: tls`, Piccolo continues to allocate and expose per-listener public ports on LAN.

## 6. mDNS & Fallback Strategy

### 6.1 mDNS Advertising

When host-based LAN routing is enabled, Piccolo SHOULD advertise:
- the device hostname (`piccolo.local`), and
- each app host alias (`immich.piccolo.local`, `metrics-immich.piccolo.local`, etc.).

### 6.2 Reliability and Fallback

mDNS is not universally reliable. Piccolo MUST retain a port-based fallback for LAN access:
- UI can show preferred host-based URLs and provide fallback `http://<ip>:<port>` URLs.
- If `PICCOLO_DISABLE_MDNS=1`, host-based `.local` URLs are not expected to work; port-based URLs become primary.

## 7. Security Considerations

- **Cookie isolation:** host-based routing provides true browser-side isolation across apps by hostname; port-based routing does not.
- **Host header validation:** host-based routing MUST only route known/expected hostnames to prevent confused-deputy issues.
- **Session cookies:** Piccolo session cookies may be scoped to the LAN base domain to enable SSO across subdomains (portal + apps), while apps should use host-only cookies.
- **Wildcard TLS:** multi-listener hostnames MUST remain single-label to preserve `*.<base>` certificate coverage on Remote.

## 8. Migration & Rollout

1. Introduce hostnames in API/UI (display only).
2. Implement remote hostname scheme and resolver changes (server-side).
3. Add LAN host-based routing for HTTP/WebSocket (optional, feature-flagged).
4. Enforce DNS-label naming constraints for app/listener names (no `-`, length ≤ 31).
5. Keep legacy port-based URLs working indefinitely (at least through v1), with explicit documentation of cookie-domain caveats.

## 9. Relationship to Other RFCs

- **Listener auth rules**: Listener-level auth and cookie/header stripping are specified in RFC 20260112. This RFC changes the hostnames those rules operate under, and can eliminate the need for LAN cookie rewriting.
- **Native OIDC auth**: OIDC redirect validation and dynamic discovery must accept the new hostnames.

## 10. Implementation Notes & Status

- **Status:** Draft
- **Notes:**
  - 2026-01-20: mDNS manager supports dynamic alias labels and multi-name announcements; wiring to app/service registry pending.
