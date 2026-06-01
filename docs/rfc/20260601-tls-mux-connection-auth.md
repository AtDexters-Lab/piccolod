# TLS Mux Connection Auth and Piccolo Tunnels

## Scope

**Problem:** Piccolo needs authenticated remote access to non-browser listener protocols, initially SSH/SCP, without adding an SSH-specific access surface or bypassing Piccolo as the only guard.
**In scope:** A generic TLS-mux connection-auth layer for host-routed listeners, Piccolo-session mTLS certificates, a tunnel CLI contract, raw-listener usage guidance, capability-gated rollout for mTLS manifests, and the multi-container listener binding cleanup needed for per-service developer ports.
**Out of scope:** A VPN/P2P transport implementation, running or configuring `sshd` inside app containers, app-managed client CA verification beyond schema reservation, public-WebPKI client identity, and web UI terminal changes.

This RFC supersedes the earlier SSH-specific dev-access draft. SSH remains the first motivating use case, but the primitive is "authenticated tunnel to a listener host," not "Piccolo owns SSH."

---

## Background

Piccolo already has three pieces that make this feasible:

- Listener host routing: HTTP/WebSocket listeners and `flow: tcp, protocol: raw, tls_wrap: true` listeners can derive host labels and be reached through the TLS mux on remote port 443.
- Existing listener `connection_auth`: currently IP/CIDR allow/deny rules enforced in L4 proxy middleware.
- A network-anchor container: service containers share the app network namespace, while the anchor owns published ports for listener guest ports.

The missing piece is connection authentication at the point where Piccolo still sees the TLS client identity. For raw protocols, there is no HTTP request where `auth.rules` can run. The TLS mux must therefore enforce admission before forwarding opaque bytes to the listener.

---

## Core Model

### L7 `auth` and L4 `connection_auth` stay separate

`auth` remains HTTP/WebSocket request authorization. It matches paths and applies strategies such as `protected`, `headers`, `public`, and `oidc_passthrough`.

`connection_auth` is connection admission. It runs before protocol bytes reach the listener. For raw listeners, this is the Piccolo auth boundary. For HTTP/WebSocket listeners, it is an optional precondition before ordinary L7 auth.

If both are present on an HTTP/WebSocket listener, both must pass:

```yaml
listeners:
  - name: admin
    guest_port: 8080
    flow: tcp
    protocol: http
    connection_auth:
      mtls:
        verifier:
          type: piccolo_session
    auth:
      rules:
        - path: /
          type: prefix
          strategy: protected
```

Browser requests to that listener will fail unless the browser presents an accepted client certificate and reaches the listener through a Piccolo TLS-mux hostname. That is intentional, and should be documented as an advanced configuration. Normal browser apps should continue to rely on `auth` alone.

### SSH is just a raw listener use case

The recommended v1 SSH listener shape is:

```yaml
listeners:
  - name: ssh
    guest_port: 22
    flow: tcp
    protocol: raw
    tls_wrap: true
    connection_auth:
      mtls:
        verifier:
          type: piccolo_session
```

The container image remains responsible for running an SSH-compatible server on port 22. Piccolo does not configure container users, SSH host keys, or in-container auth. In the Piccolo tunnel path, the container may choose no-auth SSH or its own keys; Piccolo's boundary is the mTLS tunnel admission.

Stock SSH clients do not speak TLS before SSH. SSH over this listener therefore requires `piccolo tunnel` or an equivalent TLS wrapper, usually via OpenSSH `ProxyCommand`. Direct `ssh user@ssh-myapp.example.com` without the tunnel wrapper will not work.

---

## Decisions

### D1 - Extend `connection_auth` additively

The existing schema must remain valid:

```yaml
connection_auth:
  default: deny
  rules:
    - match: 10.0.0.0/8
      strategy: allow
```

This RFC adds an optional sibling:

```yaml
connection_auth:
  mtls:
    verifier:
      type: piccolo_session
```

Combined use is valid:

```yaml
connection_auth:
  default: deny
  rules:
    - match: 10.0.0.0/8
      strategy: allow
  mtls:
    verifier:
      type: piccolo_session
```

Semantics:

- Existing `default` and `rules` keep their current IP allow/deny behavior.
- `mtls` adds TLS client-certificate admission where Piccolo terminates TLS.
- If both IP rules and mTLS are configured, both must allow the connection. For mTLS listeners, the TLS mux is the authoritative enforcement point for the IP-rule portion and evaluates it as part of route admission before upstream dial. The existing proxy L4 middleware remains the enforcement point for IP-only listeners; it must not re-enforce IP rules on the TLS-mux upstream path unless the original client source is proven to be preserved through the loopback hop.
- `connection_auth` with only `mtls` must not accidentally become an operator-listed IP middleware requirement; the IP middleware should gate on the IP-rule portion, not on the non-nil `ConnectionAuth` pointer.

The RFC intentionally does not use a top-level `strategy: mtls` because `connection_auth` already has a stable IP-rule shape with `rules[].strategy`. A top-level strategy would either break existing manifests or create two competing encodings for the same field.

Source-IP semantics for mTLS admission:

- Direct TLS-mux connections use the TCP peer address from the accepted TLS connection.
- Nexus-routed connections use the existing trusted proxy hint. If IP rules are configured and the trusted client IP hint is missing, admission fails closed.
- Future P2P/VPN transports must provide a stable trusted peer/client address before IP rules can be evaluated. If they cannot, IP rules plus mTLS are unsupported on that transport.

### D2 - V1 verifier support

V1 supports exactly one verifier:

```yaml
verifier:
  type: piccolo_session
```

`piccolo_session` means:

- Piccolo issues a short-lived client certificate to an authenticated Piccolo user.
- The certificate is scoped to one listener host/listener audience in v1. Remote port is used to resolve and validate the target listener before issuance; it is not a substitute identity on its own.
- The TLS mux accepts the certificate only for matching hosts/listeners.
- Cert issuance requires the user to be authorized for the requested listener. V1 is admin-only unless the implementation already has a clean allowed-app authorization hook.

The schema reserves future verifier types but the parser rejects them in v1:

- `ca_bundle`: app/operator-supplied client CA, with explicit SAN/EKU constraints.
- `public_webpki`: public CA client certificates, only with strict constraints. This is not a shortcut for "any Let's Encrypt cert"; public server certificates are not user identity.

### D3 - Placement rules

`connection_auth.mtls` is valid only where Piccolo terminates TLS and can see the client certificate:

- Valid: `flow: tcp, protocol: raw, tls_wrap: true`.
- Valid: `flow: tcp, protocol: http`.
- Valid: `flow: tcp, protocol: websocket`.
- Invalid: `flow: tcp, protocol: raw` without `tls_wrap: true`.
- Invalid: `flow: tls`, because the app terminates TLS.
- Invalid: `flow: udp`, because there is no TLS connection.
- Invalid in v1: `port_claim` on the same listener, because direct claimed ports do not traverse the TLS mux and would bypass mTLS.

Explicit `remote_ports` remain valid on mTLS listeners. For these listeners, each effective remote port is a TLS-mux port, not a direct app proxy port. A Nexus request for that host/port must resolve to the TLS mux when `isTLS=true`, and must return no route when `isTLS=false`.

For raw listeners, the parser must require `tls_wrap: true` when `connection_auth.mtls` is present. This preserves the rule that a raw tunnel is reachable through the TLS mux only when the manifest explicitly opts into TLS wrapping.

### D4 - mTLS is an all-path listener policy

`connection_auth.mtls` means "this listener requires Piccolo-verified TLS client authentication before bytes reach the app." It is not merely an extra check on the remote TLS-mux path.

Therefore, when a listener declares `connection_auth.mtls`:

- The listener's upstream proxy path must be reachable by the TLS mux.
- Direct LAN/public proxy access that cannot enforce mTLS must be disabled.
- `port_claim` is rejected in v1.
- Explicit `remote_ports` are allowed, but are interpreted as additional TLS-mux remote ports for this listener.
- Effective remote exposure is TLS-mux-only on the listener's derived remote ports.
- Alias and remote-base hostnames inherit the same mTLS requirement.
- HTTP/WebSocket L7 `auth` still runs after mTLS for HTTP/WebSocket listeners.

Implementation shape:

- Endpoint metadata needs a single shared predicate/flag, such as `RequiresTLSMuxAuth`, derived from `connection_auth.mtls`.
- The proxy listener used as TLS mux upstream binds loopback-only and is not advertised as a direct access surface.
- Remote/Nexus routing for mTLS listeners targets the TLS mux on each effective remote port only when the inbound connection is TLS/SNI-routed.
- Nexus is still the remote relay. The fail-closed invariant is that a Nexus connection for an mTLS listener resolves either to the TLS mux or to no route; it must never resolve directly to the app listener's public proxy port.
- For every effective remote port on an mTLS listener, `isTLS=true` routes to the TLS mux and `isTLS=false` returns no route.
- A request for a recognized mTLS hostname with `isTLS=false`, or with a remote port that is not one of that listener's effective remote ports, is a terminal deny for that hostname. Nexus may fall back to `port_claim` routing only when no protected host route matched at all.
- Any resolver path that would expose the listener without TLS-mux mTLS returns no route.
- LAN host routing, LAN/local URL formatting, mDNS announcements, remote alias resolution, remote base resolution, Nexus fallback routing, and public-port proxy binding must all consume the same predicate and fail closed.

This preserves the invariant that adding `connection_auth.mtls` cannot accidentally create a weaker sibling path.

### D5 - TLS mux route resolution returns route metadata

Today the TLS mux resolves SNI to an upstream port. This RFC changes that mental model to "resolve SNI plus effective requested remote port to a route":

```text
host + effectiveRemotePort -> TLSMuxRoute
```

The TLS mux must not hard-code `443` when resolving app listener routes. It obtains `effectiveRemotePort` from a trusted connection hint produced by the Nexus/public remote adapter. For direct TLS-mux connections with no trusted hint, the default effective remote port is `443`.

This is load-bearing for custom remote ports. An mTLS listener with `remote_ports: [8443]` and no `443` must be reachable through Nexus on `8443`, and a host-only TLS mux lookup must not accidentally accept that listener on a different port.

Route data:

- host name as received through SNI
- effective requested remote port
- portal route or app listener route
- app name and listener name when app-routed
- upstream public port
- endpoint flow/protocol
- listener `connection_auth`
- whether the route requires TLS-mux auth
- route source, such as direct remote base or alias

Portal routes do not inherit app listener connection auth. Alias routes inherit the target listener's connection auth.

Remote route resolution also needs a terminal-deny outcome distinct from "no host matched." The Nexus backend may continue to try port-claim fallback only for "no host matched"; it must not try fallback after a protected hostname matched but was denied because the connection was non-TLS, used a disallowed remote port, or otherwise failed the TLS-mux-only policy.

### D6 - TLS mux enforces mTLS at handshake time

The TLS mux must choose client-certificate behavior per SNI. In Go, this means moving from a certificate-only callback shape to a per-client TLS config shape, so the mux can require a client certificate for routes that declare `connection_auth.mtls`.

Handshake behavior:

- Unknown SNI follows the existing unknown-route behavior and must not forward bytes.
- Known route without mTLS behaves as today.
- Known route with mTLS requires a client certificate.
- After handshake, the mux verifies the peer certificate and any mTLS-composed IP rules against the route's verifier.
- Verification failure closes the connection before dialing the upstream.
- Successful verification dials the upstream and forwards opaque bytes.

The verifier must fail closed if its CA, session ledger, persistence, or route metadata is unavailable.

TLS resumption must not bypass route, ledger, revocation, audience, or IP-rule checks. V1 must either disable session tickets for mTLS routes or use a post-handshake connection verifier that runs on every accepted connection, including resumed sessions.

### D7 - Piccolo session certificate lifecycle

Piccolo adds a tunnel certificate issuer/verifier service.

Issuance contract:

- The API is `POST /api/v1/tunnels/certificates`.
- The route is mounted under the existing authenticated API stack with session, CSRF, and v1 admin checks.
- The requester specifies one listener host plus the intended remote port, defaulting to 443.
- The requester supplies a CLI-generated public key in v1. CSR support is deferred; if added later, Piccolo must ignore client-requested subject, SAN, EKU, and audience fields.
- Piccolo validates the submitted public key type and strength before signing. V1 accepts Ed25519 and ECDSA P-256 public keys; weak, malformed, or unsupported keys are rejected.
- Piccolo validates that each host plus remote port resolves to an app listener whose `connection_auth.mtls.verifier.type` is `piccolo_session`.
- Piccolo validates that the current user may access those listeners.
- Piccolo returns a short-lived client certificate for CLI use.

V1 request shape:

```yaml
host: ssh-myapp.example.com
remote_port: 443
public_key_pem: <PEM-encoded public key>
requested_ttl_seconds: 3600
```

V1 response shape:

```yaml
certificate_pem: <PEM-encoded client certificate>
serial: <server-generated serial>
not_after: <RFC3339 timestamp>
max_tunnel_lifetime_seconds: 3600
```

Piccolo derives certificate subject, SAN/URI audience, EKU, user identity, and listener scope solely from server-side route and session state. The client controls only the public key and requested TTL.

Certificate claims:

- certificate serial or session id
- user id and role
- allowed host labels or FQDNs
- app/listener audience
- client-auth key usage
- expiry and optional max tunnel lifetime

Verifier contract:

- Validate chain to Piccolo's tunnel-client CA.
- Validate time bounds and client-auth usage.
- Validate serial/session id against the active issuance ledger.
- Validate host/listener audience against the resolved TLS mux route.
- Validate any configured IP-rule portion using the TLS mux's trusted effective client IP.
- Record allow/deny audit events with app, listener, user id when known, verifier type, and deny reason.

V1 lifetime bounds:

- Default TTL: 1 hour.
- Hard maximum TTL: 4 hours.
- The TLS mux closes active tunnel connections no later than the certificate `NotAfter` timestamp.
- Active revocation before expiry is deferred, but every new connection checks the serial/session ledger.
- Issued-cert ledger records are pruned after `NotAfter` plus a small grace window. Verification never depends on retaining expired cert records beyond that window.

The tunnel-client CA should be separate from the internal server/OIDC CA. Reusing the existing internal CA would blur server trust and client identity.

### D8 - CLI tunnel contract

The CLI opens an ordinary TLS connection to the listener host and bridges stdin/stdout. This makes OpenSSH and SCP work through `ProxyCommand`.

Example user flow:

```text
piccolo tunnel ssh-myapp.example.com
```

OpenSSH shape:

```text
ssh -o ProxyCommand="piccolo tunnel ssh-myapp.example.com" user@ssh-myapp.example.com
scp -o ProxyCommand="piccolo tunnel ssh-myapp.example.com" file user@ssh-myapp.example.com:/tmp/
```

CLI behavior:

- Authenticate to Piccolo using a configured Piccolo profile or portal origin. The target listener host is only the tunnel audience; it is not assumed to serve the certificate issuance API.
- Generate an ephemeral keypair locally.
- Request a `piccolo_session` client certificate for the target host and remote port using the generated public key.
- Dial the target host on the requested remote port, defaulting to 443 when the user does not specify a port.
- Present the client certificate.
- Verify the server certificate for the logical target host. The CLI must not use insecure TLS verification by default.
- Bridge local stdio to the TLS stream without inspecting SSH or any other payload.

The CLI does not need to know whether the payload is SSH, Postgres, Redis, or another raw protocol. It only needs the target host, optional remote port, local private key, and Piccolo-issued client certificate.

ProxyCommand I/O invariant:

- stdout is payload-only from process start.
- Login URLs, prompts, diagnostics, and errors go to stderr.
- If login or certificate issuance cannot complete before the payload stream starts, the CLI exits nonzero without writing to stdout.
- Once the TLS stream is established, stdout carries only bytes read from the TLS stream.

Discovery rules:

- V1 requires the CLI to have a configured Piccolo portal/API origin for the device.
- For known remote-base hosts, the CLI may infer the portal origin from the configured device profile.
- Alias/custom listener hosts do not need to expose the certificate issuance API; issuance still goes to the portal/API origin.
- Unknown target hosts fail before certificate request.

### D9 - Nexus and future P2P/VPN transport

Nexus remains a transport relay. It should not terminate the tunnel's client certificate or understand Piccolo session certs.

Remote path:

```text
CLI -> TLS with client cert -> Nexus TCP relay -> piccolod TLS mux -> listener upstream
```

Future P2P/VPN path:

```text
CLI -> TLS with client cert -> P2P transport -> piccolod TLS mux -> listener upstream
```

The VPN/P2P work swaps the dial path, not the listener auth primitive. As long as the CLI can reach the Piccolo host's TLS mux with SNI and client certificate intact, the same connection-auth design applies.

Physical dial address and logical TLS identity stay separate. A future P2P/VPN dialer may connect to a peer IP or tunnel endpoint, but SNI and server-certificate verification must use the logical listener host. There is no `InsecureSkipVerify` escape hatch in the tunnel contract.

### D10 - Capability-gated rollout

`connection_auth.mtls` is not safely backward-compatible with older piccolod binaries that ignore unknown YAML fields under the existing `connection_auth` object. A manifest that relies on mTLS for no-auth SSH would fail open on such binaries.

Manifest shape:

```yaml
x-piccolo:
  mode: service
  requires_features:
    - connection_auth_mtls_v1
```

Therefore:

- Catalog apps using `connection_auth.mtls` must declare `x-piccolo.requires_features: [connection_auth_mtls_v1]` and a catalog-level minimum piccolod version/capability.
- Catalog filtering must hide or reject those app versions on unsupported devices before install or sync.
- Installed-config/catalog sync must reject updates that introduce `connection_auth.mtls` on unsupported binaries.
- Newer piccolod must reject `connection_auth.mtls` manifests that omit the required feature/capability declaration.
- Documentation must state that container no-auth SSH is acceptable only behind a verified `connection_auth_mtls_v1` Piccolo guard.
- OS rollback/downgrade must not boot an older piccolod against persisted mTLS-dependent manifests. The rollback guard or update manager must either refuse rollback below the required capability, stop/disable mTLS-dependent apps before reboot into the older binary, or establish a hard release floor once any `connection_auth_mtls_v1` app is installed.

Manual sideloading into an older binary cannot be fixed retroactively by this RFC, because the older parser does not know the new field. The supported rollout path is catalog/version gated.

### D11 - Multi-container listener binding contract

The current parser requires the primary service to declare every listener guest port in `bind_ports`. With the network-anchor container owning published ports and all service containers sharing its namespace, that is no longer the right manifest contract.

New validation:

- `services.*.bind_ports` remains required and globally unique per app network namespace.
- Every listener guest port must appear in exactly one service's `bind_ports`.
- The listener does not need a `service` field in v1.
- Runtime routing remains by app network namespace and guest port.
- The service that declares the port is the documented owner for human readability and validation, not a separate routing target or runtime proof of which process bound the port.

This lets a multi-container app expose developer ports naturally:

```yaml
listeners:
  - name: web
    guest_port: 8080
    flow: tcp
    protocol: http
  - name: sshdb
    guest_port: 2022
    flow: tcp
    protocol: raw
    tls_wrap: true
    connection_auth:
      mtls:
        verifier:
          type: piccolo_session

services:
  web:
    image: example/web
    bind_ports: [8080]
  db:
    image: example/db-dev
    bind_ports: [2022]
```

No `target_service` is needed because duplicate port ownership is already invalid in the shared namespace. At runtime, the first process in the shared namespace to bind the port receives traffic; app authors remain responsible for ensuring the intended service owns that port.

### D12 - Observability and failure behavior

TLS mux connection-auth decisions need first-class audit and metrics, separate from HTTP auth:

- missing client certificate
- invalid client certificate
- expired certificate
- unknown or revoked certificate serial
- audience/host mismatch
- unauthorized user/listener
- verifier unavailable
- route changed or disappeared before upstream dial
- direct-route bypass attempt for an mTLS listener

TLS-mux admission failures close the connection before upstream dial. Existing IP-only proxy middleware keeps its current enforcement point. Error details should be logged server-side and summarized to metrics; the wire error can remain a TLS/auth failure without exposing policy internals.

---

## Site List

| Site | Required behavior |
|------|-------------------|
| `internal/api/types.go` | Extend `ConnectionAuth` with an optional `MTLS` child while preserving existing `Default` and `Rules`. Add typed structs for mTLS verifier config. |
| `internal/app/parser.go` | Validate `connection_auth.mtls` placement and supported verifier types. Preserve legacy IP-rule validation. Replace the primary-service listener-port check with union-of-services ownership validation. |
| `internal/app/multi_container_parser_test.go` | Replace the primary-service-only test with tests proving listener ports may be declared by any one service, must be declared by some service, and must remain unique across services. |
| `internal/app/install_pipeline.go`, `internal/app/catalog/manager.go`, and `internal/app/catalog_sync_apply.go` | Enforce the mTLS capability/version gate for catalog install and catalog sync, and reject supported-binary manifests that use `connection_auth.mtls` without the required capability declaration. |
| `docs/app-platform/specification.yaml` | Add the missing full `connection_auth` documentation. Cover existing IP/CIDR `default` + `rules`, new `mtls`, combined IP+mTLS semantics, valid listener placements, raw listener tunnel example, browser behavior when mTLS is applied to HTTP listeners, and the updated multi-container `bind_ports` contract. |
| `internal/services/types.go` | Keep `ServiceEndpoint.ConnectionAuth` as the route-facing snapshot. Add a shared predicate/flag such as `RequiresTLSMuxAuth`, plus helper methods only if needed to distinguish IP-rule auth from mTLS auth. Include the flag in endpoint/event payloads consumed by routing, UI, and mDNS surfaces. |
| `internal/services/manager.go` | Update `connectionAuthEqual` to include mTLS fields. Keep endpoint construction as the single source of truth. Add or expose a route resolver that returns endpoint metadata for TLS mux routing. Carry the route/access flag needed to prevent non-mux bypass for mTLS listeners. Reject/neutralize `port_claim` for mTLS listeners in v1. Preserve explicit `remote_ports` as additional TLS-mux-only remote ports. |
| `internal/services/proxy.go` and `internal/services/proxy_udp.go` | Preserve existing behavior for ordinary listeners. For mTLS listeners, prevent direct public/LAN exposure that cannot enforce TLS client auth, while keeping the loopback upstream path available to the TLS mux. |
| `internal/services/middleware/builtin` and `internal/services/middleware/l4` | Keep existing IP allow/deny behavior for IP-only listeners. Gate the IP middleware on the presence of IP rules/defaults, not merely on `ConnectionAuth != nil`, so mTLS-only listeners are not treated as IP-auth listeners. For mTLS listeners, do not double-enforce IP rules on the TLS-mux upstream path unless trusted original-source propagation is explicitly proven. |
| `internal/services/tlsmux.go` | Resolve SNI plus trusted effective remote port to route metadata, select per-route TLS client-cert policy, invoke the connection-auth verifier after handshake, evaluate mTLS-composed IP rules using trusted source IP as the authoritative IP enforcement point, prevent TLS resumption from bypassing verifier checks, enforce max tunnel lifetime, and forward bytes only after successful verification. |
| `internal/server/gin_middleware.go` | Ensure LAN host-based routing refuses direct routes for endpoints requiring TLS-mux auth. |
| `internal/mdns/manager.go` | Avoid advertising direct LAN hostnames/URLs for endpoints requiring TLS-mux auth unless the advertised route reaches the TLS mux with client-cert enforcement. |
| `internal/server/gin_server.go` endpoint formatting | Suppress or mark LAN/local URLs for endpoints requiring TLS-mux auth so UI/API consumers do not advertise bypass paths. |
| `internal/server/gin_server.go` remote resolver | Make alias and remote-base resolution return the TLS mux path for mTLS listeners only when the inbound connection is TLS/SNI-routed on an effective remote port for that listener; non-TLS, disallowed-port, and direct public-port routes return terminal deny for recognized mTLS hostnames. |
| `internal/remote/nexusclient/backend.go` | Preserve opaque TLS forwarding to the TLS mux, preserve the original requested remote port in the trusted hint consumed by the TLS mux, and ensure fallback routes cannot run after a protected hostname was terminally denied. |
| New `internal/services/tunnelauth` or equivalent | Own tunnel-client CA loading/creation, cert issuance, cert verification, active serial ledger, and route-audience checks. |
| `internal/server/gin_server.go` | Wire the tunnel auth service into the TLS mux, register the authenticated API endpoint for issuing tunnel client certificates, and ensure remote resolver paths for mTLS listeners go through the TLS mux rather than a direct public-port route. |
| `internal/auth` and session integration | Provide the user/session facts needed for certificate issuance. V1 should be admin-only unless allowed-app checks are already cleanly reusable. |
| Persistence/control storage | Store tunnel-client CA material and issued cert/session ledger in the appropriate protected state. Fail closed when unavailable. |
| Update/rollback guard surfaces (`docs/rfc/20260328-piccolo-core-rollback-guard.md`, update manager, or piccolo-os support package) | Prevent downgrade/rollback to binaries lacking `connection_auth_mtls_v1` while persisted mTLS-dependent manifests remain enabled, or enforce a hard release floor. |
| Remote/Nexus integration surfaces | Preserve opaque TLS forwarding. Nexus does not inspect or terminate client certs. Add regression coverage that client certs survive the relay path to piccolod. |
| External Piccolo CLI repo or future local CLI package | Implement `piccolo tunnel <host>` with login, certificate request, TLS dial, and stdio bridge. This checkout currently contains `cmd/piccolod` but no end-user CLI command. |

---

## Test Plan

### Schema and parser

- Existing IP-only `connection_auth` manifests still parse.
- The app-platform specification documents the existing IP/CIDR shape, not just the new mTLS shape.
- The app-platform specification includes examples for IP-only, mTLS-only, and combined IP+mTLS policies.
- `connection_auth.mtls.verifier.type: piccolo_session` parses on HTTP/WebSocket listeners.
- `connection_auth.mtls.verifier.type: piccolo_session` parses on raw listeners only when `tls_wrap: true`.
- mTLS on raw without `tls_wrap`, `flow: tls`, and `flow: udp` is rejected with explicit validation errors.
- mTLS with `port_claim` is rejected in v1 to avoid a direct-port bypass.
- mTLS with explicit `remote_ports` preserves those ports as TLS-mux-only remote ports.
- mTLS without the required `connection_auth_mtls_v1` capability declaration is rejected on supported binaries.
- Unknown verifier types are rejected in v1.
- Combined IP rules plus mTLS parse and preserve both policies.

### Multi-container binding

- Multi-service app with listener port declared by non-primary service is valid.
- Listener guest port absent from all services is invalid.
- Listener guest port declared by two services is invalid through existing bind-port collision behavior.
- Single-service behavior remains valid.
- Runtime install still maps listener guest ports onto the network anchor.

### TLS mux routing and auth

- Route resolver returns portal routes without listener auth.
- Route resolver returns alias routes with the target listener's connection auth.
- Route resolver returns remote-base listener routes with upstream port and endpoint metadata.
- Non-mTLS route behaves as before.
- mTLS listener is not reachable through a direct public/LAN proxy path that bypasses the TLS mux.
- mTLS listener is not advertised through mDNS, LAN/local URL formatting, or remote resolver outputs as a direct route.
- Nexus `isTLS=false` attempts return no route for mTLS listeners on every effective remote port.
- Nexus `isTLS=true` on every effective remote port resolves to the TLS mux.
- mTLS HTTP/WebSocket listener with `remote_ports: [8443]` and no `443` succeeds through Nexus on `8443`.
- TLS mux app-route lookup uses the trusted effective remote port from the Nexus hint, while direct no-hint TLS-mux connections default to effective remote port `443`.
- mTLS hostname with `isTLS=false` and a colliding port-claim fallback is terminally denied rather than falling through to the port claim.
- Alias routes to mTLS listeners inherit the same TLS-mux-only behavior.
- mTLS route without client cert fails before upstream dial.
- mTLS route with invalid, expired, revoked, or wrong-audience cert fails before upstream dial.
- mTLS route with IP deny rules fails before upstream dial using the correct effective source IP for direct and Nexus paths.
- Resumed TLS connections still run route, ledger, audience, revocation, and IP checks, or session tickets are disabled for mTLS routes.
- mTLS route with valid `piccolo_session` cert reaches the upstream and forwards opaque bytes.
- Proxy L4 IP middleware does not reject a valid mTLS tunnel due to loopback source after TLS mux admission has already accepted the original client IP.

### Tunnel certificate lifecycle

- Authenticated admin can request a cert for an mTLS-enabled listener host.
- Cert requests include the target remote port, defaulting to 443, so custom remote-port listeners can be validated before issuance.
- Cert request accepts a CLI-generated public key; piccolod does not generate or return the private key.
- Cert request is served only by the configured portal/API origin, not by the protected listener host.
- Cert request route requires session, CSRF, and admin authorization in v1.
- Cert issuance derives SAN/EKU/audience from server-side route state and ignores client-controlled identity fields.
- Cert issuance rejects weak, malformed, or unsupported public keys.
- Request for a non-mTLS listener host is rejected.
- Request for an unknown host is rejected.
- Standard user behavior is explicit: either rejected in v1 or accepted only through a proven allowed-app check.
- Cert serial is recorded and checked by the verifier.
- Verifier fails closed when ledger or CA state is unavailable.
- Active tunnel closes no later than certificate `NotAfter`.
- Expired ledger entries are pruned after `NotAfter` plus the grace window.

### Capability rollout

- Catalog filtering hides or rejects mTLS app versions on devices below the required piccolod capability/version.
- Catalog sync refuses updates that introduce `connection_auth.mtls` without the capability gate.
- Newer parser rejects mTLS manifests that omit the required capability declaration.
- OS rollback/downgrade below the required capability is refused, disables mTLS-dependent apps before reboot, or is blocked by a hard release floor.

### CLI and SCP compatibility

- `piccolo tunnel <host>` opens a TLS stream with SNI and client cert on remote port 443 by default.
- `piccolo tunnel <host>:<port>` or an equivalent port option opens the TLS stream on a custom effective remote port while preserving SNI/server verification for the logical host.
- CLI uses a configured Piccolo portal/API origin for certificate issuance.
- CLI verifies the server certificate for the logical listener host even if a future P2P/VPN transport dials a different physical address.
- CLI stdout is payload-only under `ProxyCommand`; diagnostics, login prompts, and errors use stderr, and pre-tunnel failures emit no stdout bytes.
- OpenSSH `ProxyCommand` can complete an SSH handshake through the tunnel to a test backend.
- SCP over the same `ProxyCommand` transfers a file to a test backend.
- CLI does not require any SSH-specific behavior from piccolod.

### Nexus/P2P composition

- Nexus remote path forwards the client cert through to piccolod TLS mux.
- Auth failure through Nexus fails before upstream dial.
- Success through Nexus reaches the same upstream as direct TLS mux testing.
- P2P/VPN transport is not implemented here, but tests should keep the tunnel dialer abstract enough that a future transport can reuse the same TLS/mTLS layer.

---

## Migration and Compatibility

- Existing manifests using IP-only `connection_auth` keep their current behavior.
- Existing HTTP/WebSocket auth remains L7 and unchanged.
- Existing raw listeners without `tls_wrap` remain unreachable through host-routed TLS mux paths.
- Existing `tls_wrap: true` raw listeners keep their TLS-wrapped routing behavior; they gain all-path mTLS enforcement only when `connection_auth.mtls` is declared.
- Multi-container manifests that previously duplicated listener ports into the primary service only for validation optics may be simplified, but existing manifests remain valid as long as listener ports are declared by exactly one service.
- Older binaries may ignore the new nested mTLS schema and fail open. Catalog rollout must gate mTLS manifests on a piccolod version/capability that supports this RFC. Manual sideloading of mTLS manifests into older binaries is unsupported.

---

## Security Notes

- Piccolo-session client certs and their local private keys are bearer credentials while valid. The CLI must store them only in process memory or a restrictive temp file that is deleted after use.
- Certs must be scoped to host/listener audiences. A cert issued for one listener must not open another listener on the same Piccolo.
- mTLS authorization belongs at piccolod, not Nexus. Nexus remains a relay and cannot become the policy authority.
- The tunnel-client CA must be distinct from server cert authorities.
- If active tunnel revocation before expiry is not implemented in v1, the documented guarantee is admission-time authorization plus enforced certificate-expiry tunnel closure, not continuous per-byte authorization.

---

## Deferred Follow-Ups

- App/operator-supplied client CAs via `verifier.type: ca_bundle`.
- Browser-friendly client certificate enrollment for HTTP apps that intentionally want mTLS plus L7 auth.
- Active connection revocation hooks tied to logout, role changes, or explicit tunnel kill.
- P2P/VPN dialer integration for `piccolo tunnel`.
- UX affordances in app settings showing which listeners are tunnel-capable.
