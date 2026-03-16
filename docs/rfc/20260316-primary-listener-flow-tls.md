# RFC: Allow Primary Listeners to Use `flow: tls` and `flow: udp`

- **Status:** Draft
- **Date:** 2026-03-16
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core
- **Amends:** RFC 20260114 (Hostname Scheme & Routing), RFC 20260130 (Unified App Identity)

## 1. Summary

This RFC proposes removing the restriction that prevents `__primary` listeners from using `flow: tls` or `flow: udp`. Today, primary listeners must be `flow: tcp` with `protocol: http|websocket`. This change enables apps that natively speak TLS or UDP on their listening port to serve as primary listeners.

The key insight: **the primary listener's role is to provide app identity** (the app name is derived from the primary listener name). Routing is path-dependent and not all paths require HTTP semantics. Rather than blocking identity because one access path (LAN host-based) can't proxy a given flow type, we allow the identity and let each access path handle what it can.

**`flow: tls` primary listeners** get identity and routing on **remote (Nexus)** and **LAN port-based** paths via raw TCP passthrough. **LAN host-based routing is not supported** — this avoids TLS bridging complexity (mTLS breakage, double encryption, divergent code paths). Because LAN host-based is not attempted, `protocol: raw` is also allowed.

**`flow: udp` primary listeners** get identity and routing on **LAN port-based** (always) and **remote** (only when a `port_claim` is present — the Nexus relay registers claimed ports directly). LAN host-based routing is not supported (UDP has no HTTP/SNI semantics).

See Section 4.1 for per-path access semantics. In brief: remote and LAN port-based paths use raw passthrough (already working for secondary listeners). LAN host-based is not supported. UDP remote access requires a `port_claim`.

## 2. Motivation

### 2.1 Apps That Only Speak TLS

Many applications are designed to listen exclusively over TLS and have no plaintext mode. Examples include enterprise admin panels, apps with embedded HTTPS servers (Caddy, Traefik), database servers with mandatory TLS, and appliances that mandate TLS on their listening port. These apps cannot function as primary listeners today because Piccolo rejects `__primary` + `flow: tls`.

### 2.2 Apps That Manage Their Own Certificates

Some apps have built-in ACME support or manage their own certificate lifecycle. For remote access, these apps can obtain certificates for their remote domain (`app.<remote-base>`) via Let's Encrypt or similar CAs. Forcing Piccolo to terminate TLS on their behalf removes this capability.

### 2.3 UDP-Primary Apps (DNS, Game Servers, VPN)

Apps like Pi-hole (DNS on UDP 53), WireGuard (VPN on UDP 51820), or game servers use UDP as their primary protocol. Today these apps must have a separate HTTP listener designated as primary, even when their core functionality is UDP. With a `port_claim`, UDP apps can serve as primary and get both LAN port-based and remote access on their well-known port.

### 2.4 The Restriction Is Overly Broad

The current restriction ("not eligible for host routing") was originally motivated by LAN host-based routing requiring HTTP visibility. But:

1. **Remote routing already works** for `flow: tls` — the resolver returns `ep.PublicPort` directly, bypassing the TLS mux.
2. **Remote routing works for UDP with port claims** — the Nexus relay registers claimed ports directly.
3. **LAN port-based routing already works** — `startTCPProxy`/`startUDPProxy` does raw passthrough.
4. The only path that cannot work transparently is **LAN host-based** — and we can simply not support it rather than blocking the entire feature.

The restriction prevented non-HTTP apps from having primary listener identity, which blocked them from getting proper app naming and (for TLS) remote hostname-based access.

## 3. Goals & Non-Goals

### 3.1 Goals

- Allow `__primary` listeners to use `flow: tls` with any protocol (`http`, `websocket`, or `raw`).
- Allow `__primary` listeners to use `flow: udp`.
- Preserve end-to-end passthrough on remote and LAN port-based paths.
- Provide app identity (name derived from primary listener) for `flow: tls` and `flow: udp` apps.
- Document clearly that LAN host-based routing is not available for `flow: tls` or `flow: udp`.

### 3.2 Non-Goals

- Supporting LAN host-based routing (`app.piccolo.local`) for `flow: tls` or `flow: udp` listeners. See Section 4.1.3 for rationale.
- Provisioning certificates for apps — apps using `flow: tls` manage their own certificates.

## 4. Design

### 4.1 Access Path Semantics

#### 4.1.1 Remote (Nexus) — `flow: tls` — No Change

The Nexus relay provides the SNI hostname as out-of-band metadata to the backend client. The `serviceRemoteResolver.Resolve()` already checks `ep.Flow == api.FlowTLS` and returns `ep.PublicPort` directly, bypassing the TLS mux. Raw encrypted bytes from the relay are piped straight to the app's public port. The app completes the TLS handshake with its own certificate.

No code changes needed on this path.

#### 4.1.2 Remote (Nexus) — `flow: udp` — Port Claim Required

UDP remote access is available only when the listener has a `port_claim`. The Nexus relay registers claimed ports directly in its port mappings (`portMappings[cm.Port]`). When a remote client sends UDP to `relay:<claimed_port>`, the Nexus backend client receives it and forwards to the local host bind.

Without a port claim, the listener gets an auto-allocated port (35000-45000 range) which is not registered on the relay — no remote access. This is by design: UDP protocols require well-known ports (DNS on 53, WireGuard on 51820) and auto-allocated ports are not useful for remote UDP.

No code changes needed — port claim registration already handles UDP.

#### 4.1.3 LAN Port-Based — No Change

Clients connecting directly to `<host>:<public_port>` reach the proxy listener (`startTCPProxy` for `flow: tls`, UDP proxy for `flow: udp`), which performs raw passthrough. This already works for secondary listeners and will work identically for primary listeners.

No code changes needed on this path.

#### 4.1.4 LAN Host-Based — Not Supported

When `lanHostRoutingMiddleware` resolves the `Host` header to an endpoint with `flow: tls`, the middleware MUST skip the endpoint and fall through to portal routes. (`flow: udp` endpoints do not get a `DerivedHostLabel` and are never resolved by the middleware — see Section 5.4.)

This is intentional. For `flow: tls`, the alternatives were considered and rejected:

- **TLS bridging** (portal terminates TLS, re-encrypts to backend): Creates a divergent code path in `proxyToEndpoint` with its own forwarding headers, error handling, and transport management. Breaks mTLS. Adds double-TLS overhead. Makes app behavior depend on which access path the client used — surprising and hard to debug.
- **SNI-based passthrough on LAN** (peek SNI, route raw bytes): Requires replacing the portal's HTTPS listener with an SNI mux, and the app would need a certificate valid for `app.piccolo.local` — which no public CA issues for `.local` domains.

For `flow: udp`, LAN host-based routing is fundamentally impossible — UDP has no HTTP `Host` header or TLS SNI equivalent.

LAN users access these apps via `piccolo.local:<port>` (port-based), which works with zero changes.

### 4.2 Auth Rules Compatibility

Auth rules remain incompatible with `flow: tls` and `flow: udp` listeners. The parser currently hard-rejects these combinations, and this RFC does not change that behavior.

Apps that need auth rules should use `flow: tcp` + `protocol: http|websocket` (where Piccolo terminates TLS and enforces auth on all paths). Apps that need `flow: tls` or `flow: udp` manage their own authentication.

### 4.3 Host-Based Routing Eligibility

RFC 20260114 §4.3 states: "`protocol: raw` and `flow: tls` listeners are never eligible for primary host-based URLs."

This RFC amends that statement to:

> All flow types (`tcp`, `tls`, `udp`) are eligible as primary listeners. `flow: tls` listeners are eligible for **remote** host-based routing (hostname-based resolution via the Nexus resolver) but are **not eligible for LAN host-based routing**. `flow: udp` listeners use port-based routing only (LAN always, remote only with `port_claim`). `protocol: raw` is eligible when combined with `flow: tls` or `flow: udp`.

### 4.4 Certificate Considerations

**Remote:** The app must provide its own TLS certificate. For remote access, this typically means the app handles ACME for its remote domain (`app.<remote-base>`). Piccolo does not provision certificates for `flow: tls` listeners.

**LAN port-based:** The client negotiates directly with the app and sees the app's certificate. Standard `flow: tls` behavior.

**LAN host-based:** Not applicable — `flow: tls` endpoints are not served on this path.

### 4.5 `flow: tls` Semantics Clarification

`flow: tls` is an **operational declaration**, not a security isolation boundary. It declares: "this app speaks TLS natively on its listening port." PiccoloD, as the trusted platform orchestrator, has full access to the container filesystem and network namespace regardless of the flow type.

## 5. Implementation

### 5.1 Parser: Protocol Defaulting

**`internal/app/parser.go`** (`SetDefaults`, ~line 307): The current logic defaults the protocol to `http` for primary listeners ("Primary listeners require host-based routing, so default to http"). For `flow: tls` and `flow: udp` primary listeners, this default is misleading — HTTP semantics are not required. Update the defaulting logic:

```go
// Before:
if isPrimary {
    l.Protocol = api.ListenerProtocolHTTP
}

// After:
if isPrimary && l.Flow == api.FlowTCP {
    l.Protocol = api.ListenerProtocolHTTP
}
// flow: tls and flow: udp primary listeners keep the standard default (raw) if unspecified.
```

### 5.2 Parser: Validation (Relax)

**`internal/app/parser.go`** (~line 775): Remove the `FlowTLS` and `FlowUDP` checks for primary listeners. Remove the `ProtocolRaw` check — raw is now allowed for non-TCP primary listeners.

```go
// Before:
if l.Flow == api.FlowTLS {
    return fmt.Errorf("primary listener '%s' cannot use flow: tls (not eligible for host routing)", l.Name)
}
if l.Flow == api.FlowUDP {
    return fmt.Errorf("primary listener '%s' cannot use flow: udp (not eligible for host routing)", l.Name)
}
if l.Protocol == api.ListenerProtocolRaw {
    return fmt.Errorf("primary listener '%s' cannot use protocol: raw (not eligible for host routing)", l.Name)
}

// After:
if l.Flow == api.FlowTCP && l.Protocol == api.ListenerProtocolRaw {
    return fmt.Errorf("primary listener '%s' cannot use protocol: raw with flow: tcp (not eligible for host routing)", l.Name)
}
```

**`internal/hostname/hostname.go`** (~line 187): Remove the `FlowTLS` check in `ResolvePrimaryListener`. Update the `ProtocolRaw` check to only reject raw on `flow: tcp`:

```go
// Before:
if l.Flow == api.FlowTLS {
    return "", fmt.Errorf("primary not allowed on flow:tls listener '%s'", l.Name)
}
if l.Protocol == api.ListenerProtocolRaw {
    return "", fmt.Errorf("primary not allowed on protocol:raw listener '%s'", l.Name)
}

// After:
if l.Flow == api.FlowTCP && l.Protocol == api.ListenerProtocolRaw {
    return "", fmt.Errorf("primary not allowed on protocol:raw with flow:tcp listener '%s'", l.Name)
}
```

The auto-primary fallback logic (~line 202) MUST also be updated to allow `FlowTLS` and `FlowUDP` listeners to be auto-selected as primary for single-listener apps. Without this, single-listener non-TCP apps would silently fail to get primary status:

```go
// Before:
if l.Flow != api.FlowTLS && (l.Protocol == api.ListenerProtocolHTTP || l.Protocol == api.ListenerProtocolWebsocket) {
    return l.Name, nil
}

// After: allow any listener except flow:tcp + protocol:raw.
if l.Flow == api.FlowTCP && l.Protocol == api.ListenerProtocolRaw {
    continue
}
return l.Name, nil
```

### 5.3 Host Label Derivation

`flow: tls` primary listeners need a `DerivedHostLabel` so the remote resolver can find them via `ResolveByHostLabel`. However, `IsEligibleForHostRouting` currently returns `false` for `FlowTLS`, which causes `DeriveHostLabel` to return an empty string.

**Option A — Update `IsEligibleForHostRouting`:** Make it return `true` for `FlowTLS`. This sets `DerivedHostLabel`, enabling remote hostname resolution. But it also means `lanHostRoutingMiddleware` will resolve the endpoint — requiring a guard in the middleware to skip `flow: tls` endpoints.

**Option B — Derive host labels independently of `IsEligibleForHostRouting`:** Separate "should this listener have a host label" from "should this listener be proxied on LAN host-based." This avoids overloading a single function with two meanings.

**Recommended: Option A** — it is simpler. `IsEligibleForHostRouting` becomes "can this listener be found by hostname" (used by both remote and LAN), and the LAN middleware adds a flow-type guard.

**`internal/services/types.go`**: Update `IsEligibleForHostRouting`:

```go
func IsEligibleForHostRouting(protocol api.ListenerProtocol, flow api.ListenerFlow) bool {
    if flow == api.FlowUDP {
        return false
    }
    if flow == api.FlowTLS {
        return true // host label needed for remote routing; LAN host-based skips flow:tls
    }
    return protocol == api.ListenerProtocolHTTP || protocol == api.ListenerProtocolWebsocket
}
```

**Downstream effects:** This change propagates through the host-label chain:
1. `IsEligibleForHostRouting` returns `true` for `FlowTLS` (any protocol).
2. `DeriveHostLabel` (`hostname.go:~136`) returns a non-empty label for the listener.
3. `ServiceEndpoint.DerivedHostLabel` is populated by the ServiceManager.
4. `ResolveByHostLabel` (`manager.go:~627`) can resolve the endpoint by hostname.
5. Remote resolver uses this to route traffic via passthrough.
6. `lanHostRoutingMiddleware` resolves the endpoint but **skips** it (see Section 5.4).

### 5.4 LAN Host-Based Middleware Guard

**`internal/server/gin_middleware.go`** (`lanHostRoutingMiddleware`): After resolving an endpoint via `ResolveByHostLabel`, check if it is `flow: tls` and skip proxying. (`flow: udp` endpoints never reach this point because `IsEligibleForHostRouting` returns `false` for UDP — no `DerivedHostLabel` is set, so `ResolveByHostLabel` never finds them.)

```go
ep, found := s.serviceManager.ResolveByHostLabel(hostLabel, 0)
if !found {
    c.Next()
    return
}
// flow: tls endpoints are not supported on LAN host-based routing.
// Users access them via port-based (piccolo.local:<port>) or remote (app.<base>).
if ep.Flow == api.FlowTLS {
    c.Next()
    return
}
s.proxyToEndpoint(c, ep)
```

**No changes to `proxyToEndpoint`** — it continues to handle only `flow: tcp` endpoints as it does today.

### 5.5 Remote Resolver — No Change

The remote resolver (`gin_server.go:445, 487`) already returns `ep.PublicPort` directly for `flow: tls` endpoints, bypassing the TLS mux. No changes needed.

### 5.6 Proxy Manager — No Change

`startTCPProxy` (raw passthrough for port-based access) already handles `flow: tls`. No changes needed.

### 5.7 Health System: Skip Certificate Resolution for `flow: tls`

**`internal/services/health.go`** (`ResolveCertificatesForListener`, ~line 306): This package-level function resolves Piccolo-managed certificate IDs for listeners with a non-empty `DerivedHostLabel`. After this RFC, `flow: tls` primary listeners will have a `DerivedHostLabel`, causing the health system to report "cert_pending" or "cert_error" for certificates that Piccolo never provisions. (`flow: udp` is unaffected — it has no `DerivedHostLabel`.)

Add a flow-type guard at the top:

```go
func ResolveCertificatesForListener(ep ServiceEndpoint, remoteEnabled bool, solver, portalHostname string, aliases []RemoteAlias) ([]string, bool) {
    // flow: tls listeners manage their own certificates — skip Piccolo cert resolution.
    if ep.Flow == api.FlowTLS {
        return nil, false
    }
    // ... existing logic ...
}
```

The remote manager's ACME certificate queuing has two paths that MUST also skip `flow: tls` endpoints:

**`observeRemoteCertQueuing`** (`gin_server.go:~2596`): Receives `ServiceEndpointsChanged` events. After adding `Flow` to `ServiceEndpointInfo` (Section 5.8), add a guard when iterating added endpoints:

```go
for _, ep := range payload.Added {
    if ep.Flow == api.FlowTLS {
        continue // flow:tls apps manage their own certificates
    }
    // ... existing cert queuing logic ...
}
```

**`queueAllEndpointCerts`** (`gin_server.go:~2672`): Iterates `sm.GetAll()` which returns full `ServiceEndpoint` structs (already has `Flow`). Add a guard:

```go
for _, ep := range sm.GetAll() {
    if ep.Flow == api.FlowTLS {
        continue
    }
    // ... existing cert queuing logic ...
}
```

### 5.8 mDNS: Suppress Aliases for `flow: tls` Listeners

`flow: tls` primary listeners get a `DerivedHostLabel` (needed for remote resolver lookups), but advertising `app-piccolo.local` via mDNS would confuse LAN users — the hostname resolves but the portal page loads (since the middleware skips `flow: tls`).

**Decision: Suppress mDNS aliases for `flow: tls` endpoints.**

Add a `Flow` field to `ServiceEndpointInfo` (`internal/events/bus.go`), populated by `endpointInfoSlice` in `manager.go`. Then the mDNS handler (`handleServiceEndpointsChanged` in `internal/mdns/manager.go`) MUST skip entries with `Flow == api.FlowTLS` when registering aliases. The ACME cert queuer (`observeRemoteCertQueuing` in `gin_server.go`) MUST similarly skip `flow: tls` entries.

This is preferred over blanking `DerivedHostLabel` in the event payload, which would create a divergence between the `ServiceEndpoint` struct and its event representation and silently affect all downstream event consumers. Adding `Flow` to the event struct is a one-field change that keeps the event payload complete and lets each consumer make explicit decisions.

The LAN HTTPS certificate SAN refresh (RFC 20260203) MUST also skip `flow: tls` hostnames in its SAN list.

### 5.9 Specification Update

Update `docs/app-platform/specification.yaml`:
- Update the flow type documentation (~line 222: "Not eligible for host-based routing") to distinguish LAN vs remote eligibility for `flow: tls`, and document that `flow: udp` is port-based only.
- Remove restrictions that prevent `__primary` from using `flow: tls` or `flow: udp` (comments near the primary listener example at ~line 236).
- Allow `protocol: raw` for `flow: tls` and `flow: udp` primary listeners.
- Add examples of `flow: tls` and `flow: udp` primary listener configurations.
- Document the per-path access semantics (Section 6.1 of this RFC).
- Note that LAN host-based routing is not available for `flow: tls` or `flow: udp`.
- Document that `flow: udp` remote access requires `port_claim`.

## 6. Documentation Caveats

The following must be clearly documented in the app developer specification:

### 6.1 Per-Path Access for Non-TCP Primary Listeners

See Section 4.1 for the detailed per-path design. The developer specification should include an access summary table:

| Flow | Remote (Nexus) | LAN port-based | LAN host-based |
|---|---|---|---|
| `tcp` | Via TLS mux (Piccolo terminates TLS) | Via HTTP reverse proxy | Via `app.piccolo.local` |
| `tls` | End-to-end passthrough (app's cert) | End-to-end passthrough (app's cert) | **Not supported** |
| `udp` | **Only with `port_claim`** | Raw UDP passthrough | **Not supported** |

### 6.2 Auth Rules

Auth rules are not supported on `flow: tls` or `flow: udp` listeners (including primary). The parser rejects these combinations. Apps that need Piccolo-managed auth should use `flow: tcp`.

### 6.3 When to Use Which Flow

| Requirement | Recommended flow |
|---|---|
| App only speaks TLS / manages its own certs | `flow: tls` |
| Need LAN host-based routing (`app.piccolo.local`) | `flow: tcp` |
| Need Piccolo-managed auth rules | `flow: tcp` |
| Need mTLS (client certificates) | `flow: tls` |
| Non-HTTP TLS protocol (database, custom) | `flow: tls` + `protocol: raw` |
| UDP protocol (DNS, VPN, game server) | `flow: udp` + `protocol: raw` |
| UDP with remote access | `flow: udp` + `port_claim` |

## 7. Migration & Backward Compatibility

This change is purely additive. Existing apps with `flow: tcp` primary listeners are unaffected. Apps that previously could not use `flow: tls` or `flow: udp` as primary can now opt in.

No migration is needed. No existing manifests are invalidated.

## 8. Testing

### 8.1 Unit Tests
- Verify parser accepts `__primary` + `flow: tls` + `protocol: http`.
- Verify parser accepts `__primary` + `flow: tls` + `protocol: websocket`.
- Verify parser accepts `__primary` + `flow: tls` + `protocol: raw`.
- Verify parser accepts `__primary` + `flow: udp` + `protocol: raw`.
- Verify parser still rejects `__primary` + `flow: tcp` + `protocol: raw` (unchanged).
- Verify parser rejects `flow: tls` + auth rules (unchanged behavior).
- Verify parser rejects `flow: udp` + auth rules (unchanged behavior).
- Verify `SetDefaults` does NOT default protocol to `http` for `flow: tls` or `flow: udp` primary listeners.
- Verify `IsEligibleForHostRouting` returns `true` for `FlowTLS` (any protocol).
- Verify `IsEligibleForHostRouting` returns `false` for `FlowUDP`.
- Verify `ResolvePrimaryListener` selects a `flow: tls` listener as primary.
- Verify `ResolvePrimaryListener` selects a `flow: udp` listener as primary.
- Verify `ResolveCertificatesForListener` returns `(nil, false)` for `flow: tls` endpoints.

### 8.2 Integration Tests
- Install an app with `flow: tls` primary listener, verify LAN port-based access works (raw passthrough, app cert visible to client).
- Verify remote access works for `flow: tls` (raw passthrough via resolver, app cert visible to client).
- Verify LAN host-based access (`app.piccolo.local`) does NOT proxy to a `flow: tls` app.
- Verify mDNS alias is NOT advertised for `flow: tls` primary listener hostnames.
- Verify ACME certificates are NOT provisioned for `flow: tls` primary listeners.
- Install an app with `flow: tls` + `protocol: raw` as primary, verify remote and port-based access work.
- Install an app with `flow: udp` + `port_claim` as primary, verify LAN port-based access works.
- Verify remote access works for `flow: udp` with port claim.
- Verify `flow: udp` primary without port claim has LAN port-based access only (no remote).

## 9. Open Questions

None. All design decisions have been resolved in this RFC.
