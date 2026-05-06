# RFC: Protocol-Agnostic Listener Pipeline + Layered Middleware Framework

- **Status:** Draft
- **Date:** 2026-05-05
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core
- **Amends:** RFC 20260114 (Hostname & Routing), RFC 20260316 (Primary Listener Flow), RFC 20260112 (Listener Auth Rules)
- **Plan:** `.claude/plans/protocol-agnostic-listener-pipeline.md`

## 1. Summary

This RFC describes the layered middleware framework that replaces the
monolithic `startHTTPProxy` and `startTCPProxy` request paths with a
registry-driven L4 / L4UDP / L7 / L7Response chain composed per listener
from a mix of canonical (always-on) and operator-listed (opt-in) factories.
Two user-visible features ride on the framework:

1. **`ConnectionAuth`** — a typed L4 IP-allow/deny field on every listener.
   Composes automatically as the canonical `connection_auth` middleware when
   the field is non-nil. Permitted on every flow including TLS-passthrough
   and UDP — gives apps without their own access control a firewall-style
   gate at the network edge.
2. **`tcp+raw` host-routable** — `flow:tcp + protocol:raw` is now valid as
   `__primary` and gets a `DerivedHostLabel` on remote (Nexus) and the TLS
   mux SNI route. Apps with raw-TCP backends (DNS-over-TLS, custom binary
   protocols) can serve as primary listeners and get hostname-based access.

## 2. Motivation

### 2.1 The monolithic proxy

Pre-refactor, `services.startHTTPProxy` was a 280-line handler with the
request-side concerns (forwarded-header scrub, path normalization, hint
consumption, reserved-path intercept, ACME bypass, header strip, path
auth, cookie isolation glue, OIDC authorize snapshot, forward headers,
gzip + reverse_proxy) all woven together. The response-side `ModifyResponse`
closure carried four interleaved concerns (security headers, cookie
isolation snapshot+strip+rewrite, embedded marker, OIDC authorize rewrite).

Adding any new behavior — operator-supplied middleware, IP-rule access
control, observability hooks — required touching the monolith. Each addition
risked regressing one of the existing concerns because the dependencies
between them were implicit (lock acquisition order, sequence-as-protective
mechanisms, e.g. embedded marker AFTER cookie strip so its `piccolo_` prefix
isn't filtered). The size of the function made it hostile to review.

### 2.2 Two user-visible features blocked on the framework

`ConnectionAuth` and `tcp+raw + __primary` both want to compose with the
existing chain rather than bolt on more inline logic. ConnectionAuth wants
to deny connections at L4 before any L7 work happens. `tcp+raw` wants to
share the canonical L4 chain (hint consumer, IP rules, metrics) with L7
listeners. Without a registry-driven chain composition, each feature would
have to introduce its own bespoke wiring — and the next feature after that
would be worse.

## 3. Design

### 3.1 Layered middleware framework

`internal/services/middleware/` defines four layer types:

| Layer | Shape | Terminal |
| --- | --- | --- |
| LayerL4 (TCP) | `func(next ConnHandler) ConnHandler` | Forwards conn to backend (raw passthrough) or hands off to http.Server |
| LayerL4UDP | `func(next UDPHandler) UDPHandler` | Forwards datagram to backend |
| LayerL7 (HTTP req) | `func(next http.Handler) http.Handler` | gzip + httputil.ReverseProxy |
| LayerL7Response | `func(*http.Response) error` | (no terminal — sequential) |

A `Registry` holds named factories (canonical or operator-listable) and
composes per-listener chains via `BuildL4` / `BuildL4UDP` / `BuildL7` /
`BuildL7Response`. Canonical factories register via `RegisterCanonical`
(always run, in registration order); operator-listed factories register via
`Register` and append to the canonical chain when listed in the listener's
`Middleware[]`.

Conditional canonical entries (`connection_auth`, `path_auth`) compose only
when their backing field on the listener is non-nil — gated via
`spec.HasConnectionAuth` and `spec.HasAuth`.

### 3.2 Canonical L4 chain (every listener)

```
hint_consumer_l4    (lazy ConnContext.Hint resolver)
  → connection_auth (gated on ConnectionAuth != nil)
  → conn_metrics    (records Received counter; rule middlewares record Denied)
```

L4UDP mirrors with `connection_auth_udp` + `conn_metrics_udp`.

### 3.3 Canonical L7 chain (`flow:tcp + protocol:http|websocket` only)

Request side:
```
forwarded_scrub
  → path_normalize          (mutates r.URL.Path)
  → hint_consumer_l7        (X-Piccolo-Hint-Token reader; LAN-host-based hop)
  → reserved_path_intercept (l7oidc; /__piccolod_oidc/* dispatch)
  → strip_piccolo_headers   (RFC 4.1.5 spoof guard)
  → acme_bypass             (/.well-known/acme-challenge/*)
  → path_auth               (gated on Auth != nil)
  → cookie_context          (LAN port-based isolation + CHIPS marker stash)
  → oidc_authorize_snapshot (l7oidc; WAN issuer→portal rewrite snapshot)
  → forward_headers         (X-Forwarded-* + Forwarded RFC 7239)
  → terminal: gzip + reverse_proxy
```

Response side:
```
security_headers_response       (X-Frame-Options, CORP, COEP strip; CSP frame-ancestors rewrite)
  → cookie_isolation_response   (Set-Cookie snapshot + strip + per-app prefix rewrite + CHIPS)
  → embedded_marker_response    (iframe context marker — runs AFTER snapshot to survive)
  → oidc_authorize_rewrite_response (l7oidc; rewrites Location + body URLs for WAN)
```

Two write sites populate `middleware.HintFromContext` per plan §D13: the
L4→L7 conn-level bridge via `http.Server.ConnContext` and the L7
header-token consumer. Last writer wins; the LAN-host-based hop's
header-token is the source of truth on that case.

### 3.4 ConnectionAuth

```
type ConnectionAuth struct {
    Default string                  // "allow" | "deny" (default "allow")
    Rules   []ConnectionAuthRule    // first-match-wins
}
type ConnectionAuthRule struct {
    Match    string  // CIDR (IPv4 or IPv6)
    Strategy string  // "allow" | "deny"
}
```

Permitted on every flow (TCP, TLS-passthrough, UDP). The middleware reads
`middleware.EffectiveSourceIP(ctx)` which honors the TrustedLoopback gate
— for connections relayed via the TLS mux, the resolved real-client IP from
`ConnContext.Hint` overrides the loopback `SourceAddr`.

Reconcile equality: `nil` and `{Default:"allow", Rules:nil}` compare equal
(plan §D17 upgrade-churn fix). Slice nil/empty distinction is also
collapsed (`Rules: nil == Rules: []`).

### 3.5 tcp+raw + __primary

Per plan §D8, `flow:tcp + protocol:raw` is now eligible for hostname
routing:
- `IsEligibleForHostRouting(_, FlowTCP)` returns true (only flow:udp is
  excluded).
- The parser allows `__primary + tcp+raw`.
- `hostname.ResolvePrimaryListener` no longer skips tcp+raw.

LAN host-based routing is NOT extended — that path is HTTP-only because gin
terminates TLS for the portal cert and decodes the request before routing.
Remote (Nexus) and LAN port-based paths work via the TLS mux SNI route:
client connects over TLS by SNI; mux terminates TLS for the listener cert;
backend sees raw bytes. mDNS suppression mirrors flow:tls — tcp+raw
listeners aren't announced for LAN host-based.

`api.LanHostBasedEligible(flow, protocol)` is the single source of truth
for the LAN-host-based check (lives in `api/` so both `mdns/` and `server/`
consume it without dragging in `services/`).

## 4. Registry contract

`Build*` returns an error when (a) operator-listed middleware name isn't
registered, (b) a factory's required dep isn't bound in the supplied
`RegistryDeps`, or (c) a factory returns its own validation error
(e.g., bad CIDR in `ip_allowlist` params). Reconcile treats any Build
error as fail-closed: the listener is rejected with a `config_error` health
badge per plan §S5.

Factories MUST capture deps as getter functions (not snapshot values) so
runtime swaps (`SetUserManager`, `SetSessionStore`, `SetProxyOIDCConfig`)
propagate without registry rebuild. Cache invalidation is field-driven:
listener config changes to `Middleware`, `Auth`, or `ConnectionAuth`
trigger a chain rebuild; service-singleton swaps don't.

## 5. Test surface

- Per-built-in unit tests: `internal/services/middleware/l4/l4_test.go`,
  `connection_auth_test.go`, plus `l7/*_test.go` retained from earlier
  steps. Each built-in is independently testable.
- Registry composition: `internal/services/middleware/builtin/builtin_test.go`
  pins chain shape (full chain, conditional gates, missing-dep failure).
- Equivalence harness: `internal/services/proxy_equivalence_test.go` pins
  the dep-key wiring between `ProxyManager.buildL7Deps` and the canonical
  factories (drift in either direction surfaces as a build-time test
  failure, not a production proxy startup failure).
- End-to-end: `internal/services/proxy_connection_auth_test.go` exercises
  ConnectionAuth's deny-blocks-LAN-connection scenario through a real
  listener.

## 6. Backward compatibility

- Existing HTTP apps: canonical L7 chain reproduces the prior inline
  behavior. The full proxy_test.go and proxy_auth_test.go suites pass
  unchanged across the registry refactor — that is the equivalence proof.
- Existing `flow:tls` apps: L4 chain is empty by default; `ConnectionAuth`
  is opt-in. No behavior change.
- Existing `flow:udp` apps: same.
- `tcp+raw + __primary`: additive (parser rejected this combination before;
  no existing app uses it). User-facing docs SHOULD recommend pairing it
  with `ConnectionAuth` rules — the app's own protocol-level auth may be
  weak; ConnectionAuth provides defense-in-depth.

## 7. Future work

- `Middleware[]` composition controls (position, disable-canonical,
  per-entry tests) — deferred per plan DEF3 until operator demand
  materializes.
- Bytes-counted variant of `conn_metrics` (today: counts only).
- Centralized typed-strategy constants for path_auth strategies — plan
  DEF7 captures the cross-package cleanup.
- Conn-level hint sweeper (today: hints linger until source-port reuse
  overwrites — pre-existing leak, low impact).

## 8. References

- Plan: `.claude/plans/protocol-agnostic-listener-pipeline.md`
- RFC 20260114 §4.3, §5.3 — hostname routing now extends to tcp+raw on
  remote.
- RFC 20260316 — primary listener flow access matrix; tcp+raw row added.
- RFC 20260112 — `ConnectionAuth` section added.
