# RFC: Replace HTTP-01 with TLS-ALPN-01 for Self-Hosted ACME

- **Status:** Draft
- **Date:** 2026-03-27
- **Authors:** Engineering Team
- **Reviewers:** @piccolo-os/core
- **Amends:** RFC 20260125 (Listener Health and Self-Healing ACME), RFC 20260312 (Namek-Managed Remote Access)

## 1. Summary

Replace the HTTP-01 ACME challenge solver with TLS-ALPN-01 for self-hosted remote certificate issuance. This eliminates the need for port 80 (plain HTTP) from remote access entirely and removes ~300 lines of HTTP-01-specific plumbing (challenge token store, auth bypasses, port mapping).

Managed mode (namek) continues to use DNS-01 via the orchestrator API — no change there.

After this change, remote access is TLS-only: port 443 for web traffic and ACME challenges, plus custom port claims for apps. Port 80 is no longer exposed remotely.

## 2. Motivation

### 2.1 HTTP-01 Requires Port 80 From Remote

HTTP-01 is the only reason port 80 is exposed on the remote tunnel. The ACME CA must reach `http://<domain>:80/.well-known/acme-challenge/<token>` to validate domain ownership. This requires:

- A dedicated internal fallback port (`ACMEHTTPFallbackPort = 5002`) mapped to remote port 80
- Port normalization logic (`normalizeRemotePort()`) throughout the service layer
- Port 80 in `defaultRemotePorts` for app listeners

Eliminating HTTP-01 means remote access becomes TLS-only — a cleaner security posture.

### 2.2 HTTP-01 Creates Auth Bypass Surface

The ACME challenge path `/.well-known/acme-challenge/` must bypass all authentication because external ACME verifiers have no session or credentials. This bypass exists in three separate locations:

1. **Gin router** (`gin_server.go:1547`): Direct route before auth middleware
2. **Service proxy** (`proxy.go:551`): Path-prefix check bypassing proxy auth
3. **Remote redirect middleware** (`gin_server.go:3259`): Skip redirect for challenge paths

Each bypass is a potential attack surface if the path matching diverges or if the challenge token store has unexpected behavior. TLS-ALPN-01 eliminates all three — the challenge is validated during the TLS handshake itself, before any HTTP layer.

### 2.3 Dual-Solver Complexity

The current codebase maintains two complete solver paths:
- **HTTP-01**: `ChallengeManager` (token store), `ChallengeSink` interface, `http01Provider` bridge, three auth bypass points, port 80 plumbing
- **DNS-01**: `OrchestratorClient` interface, `PiccoloProvider`, orchestrator API

These share only the lego `Client` and account management. The HTTP-01 path adds ~300 lines of infrastructure that has no overlap with DNS-01. Replacing HTTP-01 with TLS-ALPN-01 reuses the existing TLS infrastructure (TlsMux) rather than requiring its own parallel HTTP infrastructure.

## 3. Goals & Non-Goals

### 3.1 Goals

- **Eliminate port 80 from remote access** — remote becomes TLS-only (443 + port claims)
- **Remove HTTP-01 infrastructure** — challenge token store, auth bypasses, port mapping
- **Leverage existing TLS infrastructure** — TLS-ALPN-01 integrates with TlsMux's `GetCertificate` / `tls.Config`
- **Preserve relay gating semantics** — all relays must be connected before issuance (unchanged reasoning)
- **Maintain DNS-01 for managed mode** — no change to namek/orchestrator flow

### 3.2 Non-Goals

- Modifying the nexus relay — it already passes raw TCP, no changes needed
- Supporting HTTP-01 as a fallback — clean removal, not dual-support
- Changing the managed mode (DNS-01) pipeline
- Modifying the self-healing retry/backoff system (RFC 20260125) — failure classification and backoff apply identically to TLS-ALPN-01

## 4. Background: How TLS-ALPN-01 Works

TLS-ALPN-01 (RFC 8737) validates domain ownership during a TLS handshake on port 443:

1. Client requests a certificate from the ACME CA for `example.com`
2. CA issues a challenge with a token and key authorization
3. Client creates an ephemeral self-signed certificate for `example.com` containing:
   - A critical `acmeIdentifier` extension (OID 1.3.6.1.5.5.7.1.31) with the SHA-256 digest of the key authorization
   - The `subjectAlternativeName` extension matching the domain
4. Client configures its TLS listener to negotiate the `acme-tls/1` ALPN protocol and serve this certificate
5. CA connects to `example.com:443` with ALPN `acme-tls/1`
6. TLS handshake completes — CA reads the `acmeIdentifier` extension, validates the digest
7. Client removes the ephemeral certificate

Key properties:
- **Port 443 only** — same port used for regular HTTPS traffic
- **TLS-layer validation** — no HTTP request/response needed
- **ALPN isolation** — normal HTTPS traffic uses `h2` or `http/1.1`, challenge traffic uses `acme-tls/1`; they coexist on the same listener
- **lego support** — `cli.Challenge.SetTLSALPN01Provider()` is built-in

## 5. Design

### 5.1 Architecture Overview

```
ACME CA ──(TLS, ALPN=acme-tls/1)──► Nexus Relay ──(raw TCP)──► 127.0.0.1:<tlsMuxPort>
                                                                       │
                                                                   TlsMux
                                                                       │
                                                              ┌────────┴────────┐
                                                              │ ALPN check      │
                                                              │ acme-tls/1 ?    │
                                                              ├────────┬────────┤
                                                              │ YES    │ NO     │
                                                              ▼        ▼        │
                                                     Serve challenge   Normal   │
                                                     cert (ephemeral)  TLS flow │
                                                                       ▼
                                                              Forward to proxy
```

The nexus relay tunnels raw TCP — it does not terminate TLS. The TLS handshake occurs directly between the ACME CA and TlsMux on piccolod. This is the same path regular HTTPS traffic follows, with ALPN protocol negotiation selecting the challenge flow.

### 5.2 TLS-ALPN-01 Challenge Provider

A new `tlsALPN01Provider` replaces `http01Provider` as the lego challenge bridge:

```go
// tlsALPN01Sink stores ephemeral challenge certificates for TLS-ALPN-01.
type tlsALPN01Sink struct {
    mu    sync.RWMutex
    certs map[string]*tls.Certificate // domain → challenge cert
}

func (s *tlsALPN01Sink) Put(domain string, cert *tls.Certificate) { ... }
func (s *tlsALPN01Sink) Delete(domain string)                     { ... }
func (s *tlsALPN01Sink) Get(domain string) (*tls.Certificate, bool) { ... }
```

The lego TLS-ALPN-01 provider interface:

```go
// tlsALPN01Provider bridges lego's TLS-ALPN-01 to our sink.
type tlsALPN01Provider struct {
    sink *tlsALPN01Sink
}

func (p *tlsALPN01Provider) Present(domain, token, keyAuth string) error {
    // Use lego's tlsalpn01.ChallengeCert() to generate the ephemeral cert
    cert, err := tlsalpn01.ChallengeCert(domain, keyAuth)
    if err != nil {
        return err
    }
    p.sink.Put(domain, cert)
    return nil
}

func (p *tlsALPN01Provider) CleanUp(domain, token, keyAuth string) error {
    p.sink.Delete(domain)
    return nil
}
```

### 5.3 TlsMux Integration

The TlsMux `tls.Config` gains two changes:

1. **ALPN negotiation**: Add `"acme-tls/1"` to `NextProtos` **after** `"h2"` and `"http/1.1"`:

```go
NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}
```

Ordering matters: Go's ALPN negotiation picks the first match from the server's `NextProtos` list that appears in the client's list. Putting `acme-tls/1` last ensures it is only selected when the client exclusively offers it (as the ACME CA does), not when a non-ACME client happens to include it alongside `h2`.

2. **GetCertificate callback**: Check the challenge sink before the regular cert store. Only serve challenge certs when `acme-tls/1` is the **sole** ALPN protocol offered — per RFC 8737, the ACME CA sends exactly `["acme-tls/1"]`. If a non-ACME client happens to include `acme-tls/1` alongside `h2`, we must not serve the challenge cert (it would cause a transient TLS error for that connection).

```go
func (m *TlsMux) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
    // Serve ACME TLS-ALPN-01 challenge cert only when acme-tls/1 is the sole ALPN.
    // Per RFC 8737, ACME CAs send exactly ["acme-tls/1"]. Mixed ALPN lists
    // (e.g., ["h2", "acme-tls/1"]) are not ACME validation — skip challenge lookup.
    m.mu.RLock()
    sink := m.challengeSink
    m.mu.RUnlock()
    if sink != nil && len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == alpnACME {
        if cert, ok := sink.Get(hello.ServerName); ok {
            log.Printf("INFO: tlsmux: serving ACME TLS-ALPN-01 challenge cert for %s", hello.ServerName)
            return cert, nil
        }
    }
    // Normal cert lookup (existing path)
    return m.certs.GetCertificate(hello.ServerName)
}
```

3. **ALPN short-circuit in `serveTLSConn`**: After the TLS handshake completes, check if `acme-tls/1` was negotiated and close the connection immediately. Without this, TlsMux would attempt to resolve an upstream backend and dial it — creating spurious connections, polluting proxy hints, and logging false "unknown host" warnings during initial issuance.

```go
func (m *TlsMux) serveTLSConn(c net.Conn, services *ServiceManager) {
    tlsConn, ok := c.(*tls.Conn)
    if !ok {
        c.Close()
        return
    }
    if err := tlsConn.Handshake(); err != nil {
        log.Printf("WARN: tlsmux handshake failed: %v", err)
        _ = tlsConn.Close()
        return
    }
    state := tlsConn.ConnectionState()

    // Consume proxy hint before any early-return to prevent hint map leak.
    var (
        hint     connectionHint
        haveHint bool
    )
    if services != nil {
        if addr, ok := tlsConn.RemoteAddr().(*net.TCPAddr); ok {
            hint, haveHint = services.consumeProxyHint(m.Port(), addr.Port)
        }
    }

    // ACME TLS-ALPN-01: challenge validated during handshake, no HTTP follows.
    // Close immediately — hint already consumed, no upstream dial needed.
    if state.NegotiatedProtocol == alpnACME {
        _ = tlsConn.Close()
        return
    }

    // ... existing host resolution, upstream dial, bidirectional copy ...
    // (hint and haveHint are used downstream as before)
}
```

This is the critical integration point — the ACME CA closes the connection after the handshake, so no data transfer occurs. The short-circuit avoids:
- `resolveUpstream()` lookup for a host that may not have routing configured yet (initial issuance)
- Spurious backend TCP connection and proxy hint registration
- False `WARN: tlsmux: unknown host` log noise during every cert issuance

**Challenge sink on TlsMux**: The sink is stored as a new field on `TlsMux` under the existing `mu` lock, accessed via `SetChallengeSink()`. The `getCertificate` callback captures `m` (the TlsMux pointer) and reads the sink under `m.mu.RLock()`, matching the existing pattern for `m.certs`. This keeps the sink injection in the same layer as `SetCertProvider()`.

```go
// ChallengeCertProvider returns ephemeral ACME challenge certs by domain.
type ChallengeCertProvider interface {
    Get(domain string) (*tls.Certificate, bool)
}

func (m *TlsMux) SetChallengeSink(s ChallengeCertProvider) {
    m.mu.Lock()
    m.challengeSink = s
    m.mu.Unlock()
}
```

### 5.4 Solver Configuration

The ACME manager replaces `SolverHTTP01` with `SolverTLSALPN01`:

```go
const (
    SolverTLSALPN01 = "tls-alpn-01"
    SolverDNS01     = "dns-01"
    alpnACME        = "acme-tls/1" // ALPN protocol string (RFC 8737)
)
```

Both `Configure()` (self-hosted mode) and user-managed mode (`gin_remote_handlers.go:202`) set `solver: tls-alpn-01` instead of `solver: http-01`. User-managed mode has the same architecture (relay passes raw TCP, TlsMux terminates TLS), so TLS-ALPN-01 applies identically.

The `configureChallenge()` function routes to:

```go
func configureChallengeWith(cli *lego.Client, solver string, ...) error {
    if strings.EqualFold(solver, SolverDNS01) {
        return cli.Challenge.SetDNS01Provider(...)
    }
    // TLS-ALPN-01 for self-hosted and user-managed
    return cli.Challenge.SetTLSALPN01Provider(&tlsALPN01Provider{sink: sink})
}
```

Additional structural changes in the ACME manager:
- `NewManager()` default solver changes from `SolverHTTP01` to `SolverTLSALPN01`
- `SetSolver()` validation changes from `s != SolverHTTP01 && s != SolverDNS01` to `s != SolverTLSALPN01 && s != SolverDNS01`
- `SetSolver()` empty-string default changes from `SolverHTTP01` to `SolverTLSALPN01`

### 5.5 Relay Gating (Preserved)

The relay gating logic is preserved with renamed functions:

- `httpChallengeReachable()` → `challengeReachable()`
- `needsRelay()` semantics unchanged: returns `true` for TLS-ALPN-01 (CA connects through relay), `false` for DNS-01 (orchestrator API, no relay)

The "all relays must be connected" invariant is unchanged. The ACME CA connects to `domain:443` — external DNS determines which relay carries the traffic, and we can't track per-cert relay affinity (fragile to DNS changes). Requiring all relays connected ensures every traffic path is available.

The relay event handler, `ClearRelayState`, and requeue-on-connect logic are all preserved — only the function names and comments change from "HTTP-01" to "challenge".

### 5.6 Alias Cert Issuance

Alias certs (custom domains added to the self-hosted portal) currently use HTTP-01:

```go
// manager.go:1143
solver: acme.SolverHTTP01,
```

These switch to TLS-ALPN-01 identically. The alias hostname resolves to the relay, the relay tunnels TCP to TlsMux, and TLS-ALPN-01 validates during the handshake.

### 5.7 ACME Manager Interface Change

The `acme.Manager` constructor currently takes a `ChallengeSink` (with `Handler()`, `Put(token, value)`, `Delete(token)` — HTTP-01 specific). This is replaced with a `ChallengeCertSink` interface matching the TLS-ALPN-01 flow:

```go
// ChallengeCertSink stores and retrieves ephemeral ACME TLS-ALPN-01 challenge certs.
type ChallengeCertSink interface {
    Put(domain string, cert *tls.Certificate)
    Delete(domain string)
    Get(domain string) (*tls.Certificate, bool)
}
```

`acme.NewManager` signature changes from `NewManager(stateDir, sink ChallengeSink, email, directoryURL)` to `NewManager(stateDir, sink ChallengeCertSink, email, directoryURL)`. The `sink` field type on `acme.Manager` changes accordingly.

The `tlsALPN01Sink` struct (§5.2) implements both `ChallengeCertSink` (for the ACME manager) and `ChallengeCertProvider` (for TlsMux, §5.3). It is a single concrete type shared between the two.

### 5.8 Challenge Sink Lifecycle

The `tlsALPN01Sink` is created by the remote `Manager` and injected into both the ACME manager and TlsMux. Lifetime:

1. Remote manager creates `tlsALPN01Sink` at construction
2. Passes it to `acme.NewManager()` (replaces `ChallengeSink`)
3. GinServer wires it into TlsMux via `SetChallengeSink()` during remote setup
4. Challenge certs are ephemeral — present for seconds during ACME validation, then cleaned up by lego's `CleanUp()` call
5. On process crash between `Present()` and `CleanUp()`, the sink is in-memory only — restart clears it. The cert stays "pending" in the inventory and is requeued by `requeueOutstandingIssuances()`. This is strictly better than HTTP-01 (where stale tokens also lived in memory).

### 5.9 Namek Alias Cert Solver

`gin_namek_domains.go:341` currently hardcodes `Solver: "http-01"` for namek domain alias cert force-retries. This is a pre-existing inconsistency: namek primary certs use DNS-01 via the orchestrator API, but alias certs were issued with HTTP-01.

With this RFC, namek alias certs switch to TLS-ALPN-01. Aliases resolve to a relay (either self-hosted or namek), and TLS-ALPN-01 validates through the same raw TCP tunnel path. The `forceRetryAliasCerts()` call becomes:

```go
Solver: acme.SolverTLSALPN01,
```

Note: if a namek alias cert should instead use DNS-01 (via the namek orchestrator), that is a separate decision. TLS-ALPN-01 is correct as a default because alias domains may point to any relay, and the orchestrator API may not control their DNS zone.

## 6. Migration

### 6.1 Config Migration

Existing self-hosted configs have `solver: http-01` persisted in `remote.json`. On load:

```go
if strings.EqualFold(cfg.Solver, "http-01") {
    cfg.Solver = "tls-alpn-01"
}
```

This is a one-way migration. The config is re-saved on next `Configure()` or cert renewal.

### 6.2 Certificate Inventory

Existing certificates in the inventory with `solver: http-01` are migrated to `tls-alpn-01`. Both the top-level config `Solver` field (§6.1) and each individual `Certificate.Solver` entry must be migrated. Without per-cert migration, `health.go` cert ID resolution (which switches on `cert.Solver`) would fail to match existing certs after the `case "http-01":` is changed to `case "tls-alpn-01":`, causing false "unhealthy" states.

Migration runs on config load, iterating `cfg.Certificates`:

```go
for i := range cfg.Certificates {
    if strings.EqualFold(cfg.Certificates[i].Solver, "http-01") {
        cfg.Certificates[i].Solver = "tls-alpn-01"
    }
}
```

Certificates themselves don't change — only the solver metadata for future renewals.

### 6.3 No Relay-Side Changes

The nexus relay passes raw TCP. It does not inspect ALPN, terminate TLS, or filter by port. No relay changes are needed.

## 7. Removals and Updates

### 7.1 Files Deleted

| File | Reason |
|------|--------|
| `internal/remote/challenge.go` | `ChallengeManager` (HTTP-01 token store) — replaced by `tlsALPN01Sink` |
| `internal/remote/challenge_test.go` | Tests for removed code |

### 7.2 Code Removed

| Location | What | Lines (approx) |
|----------|------|-----------------|
| `internal/remote/acme/issuer.go` | `http01Provider` struct, `ChallengeSink` interface, `SolverHTTP01` constant | ~30 |
| `internal/server/gin_server.go:1547` | `/.well-known/acme-challenge/:token` Gin route | ~10 |
| `internal/server/gin_server.go:3259` | ACME challenge bypass in remote redirect middleware | ~5 |
| `internal/services/proxy.go:551` | ACME challenge bypass in proxy auth middleware | ~10 |
| `internal/services/manager.go` | `ACMEHTTPFallbackPort`, `normalizeRemotePort()` | ~10 |
| `internal/server/gin_server.go:63,383` | `acmeHTTPFallbackPort` references and normalization | ~5 |
| `internal/services/proxy.go` | `acme` field on ProxyManager, `SetACMEHandler()` | ~15 |

### 7.3 Code Updated (`"http-01"` → `"tls-alpn-01"`)

All hardcoded `"http-01"` string literals must be updated to use the `acme.SolverTLSALPN01` constant:

| Location | Context |
|----------|---------|
| `internal/remote/manager.go:972` | `Configure()` self-hosted mode — `solver: acme.SolverHTTP01` → `acme.SolverTLSALPN01` |
| `internal/remote/manager.go:1143` | Alias cert issuance — `solver: acme.SolverHTTP01` → `acme.SolverTLSALPN01` |
| `internal/server/gin_remote_handlers.go:202` | User-managed mode configure request — `Solver: "http-01"` → `acme.SolverTLSALPN01` |
| `internal/server/gin_app_handlers.go:144` | Portal cert entry status check — `"http-01"` → `acme.SolverTLSALPN01` |
| `internal/server/gin_app_handlers.go:148` | Portal cert entry source — `Solver: "http-01"` → `acme.SolverTLSALPN01` |
| `internal/server/gin_app_handlers.go:164` | Alias cert entry source — `Solver: "http-01"` → `acme.SolverTLSALPN01` |
| `internal/server/gin_server.go:992` | Self-hosted per-hostname cert recovery check — `"http-01"` → `acme.SolverTLSALPN01` |
| `internal/server/gin_server.go:1020` | Self-hosted cert issuance enqueue — `Solver: "http-01"` → `acme.SolverTLSALPN01` |
| `internal/server/gin_namek_domains.go:341` | Namek alias cert force-retry — `"http-01"` → `acme.SolverTLSALPN01` (see §5.9) |
| `internal/services/health.go:328` | Solver-specific cert ID resolution — `case "http-01":` → `case "tls-alpn-01":` (logic unchanged — TLS-ALPN-01 uses the same per-listener cert strategy as HTTP-01) |
| `internal/services/health_test.go:273,276` | Corresponding test cases — update solver strings |
| `internal/remote/manager.go:2538` | Error message for `cert_connection_failed` — update from "Verify port 80 is forwarded..." to "Verify port 443 is reachable via the relay" |
| `internal/remote/manager.go` | `httpChallengeReachable()` → `challengeReachable()` (rename + comment updates) |
| `internal/remote/manager_test.go` | `TestHttpChallengeReachable` → `TestChallengeReachable` (rename) |

### 7.4 Structural Code Changes

These are not string-literal swaps but constructor, wiring, and validation changes:

| Location | Change |
|----------|--------|
| `internal/remote/acme/issuer.go:87` | `NewManager()` default solver: `SolverHTTP01` → `SolverTLSALPN01` |
| `internal/remote/acme/issuer.go:208-211` | `SetSolver()` validation and default: replace `SolverHTTP01` references |
| `internal/remote/acme/issuer.go` | `sink` field type: `ChallengeSink` → `ChallengeCertSink`; constructor parameter changes accordingly |
| `internal/remote/manager.go:205` | `challenges *ChallengeManager` field → `challengeSink *tlsALPN01Sink` |
| `internal/remote/manager.go:284` | `NewChallengeManager()` → `newTLSALPN01Sink()` |
| `internal/remote/manager.go:286` | Pass new sink type to `acme.NewManager()` |
| `internal/remote/manager.go:1486-1491` | Remove `HTTPChallengeHandler()` method entirely |
| `internal/remote/manager.go:991` | Update log message: `"solver=http-01"` → `"solver=tls-alpn-01"` |
| `internal/server/gin_server.go:946` | Remove `svcMgr.ProxyManager().SetAcmeHandler(rm.HTTPChallengeHandler())` wiring |
| `internal/services/proxy.go:77` | Remove `acme` field on `ProxyManager` |
| `internal/services/proxy.go:246` | Remove `SetAcmeHandler()` method |
| `internal/services/proxy_test.go:604-686` | Remove `TestProxy_ACMEChallengeBypassesAuth` (tests deleted behavior) |
| `internal/remote/manager_test.go` | Update all `acme.SolverHTTP01` references in tests (~6 locations) |

### 7.5 Constants / Defaults Changed

- `defaultRemotePorts()`: `[]int{80, 443}` → `[]int{443}` — port 80 no longer exposed remotely by default. This only affects new app listeners; existing apps with persisted `RemotePorts: [80, 443]` retain their config. Port 80 becomes a no-op (no ACME challenge handler, no remote HTTP listener), so the dangling mapping is harmless but wasteful. A future cleanup pass can strip port 80 from persisted configs.
- `SolverHTTP01` removed, `SolverTLSALPN01` added

## 8. Testing

### 8.1 Unit Tests

- `TestTLSALPN01Sink`: Put/Get/Delete lifecycle, concurrent access
- `TestTLSALPN01Provider_Present`: Verify `tlsalpn01.ChallengeCert()` generates valid challenge cert with correct `acmeIdentifier` extension
- `TestTlsMux_ALPNChallenge`: TLS handshake with `acme-tls/1` ALPN returns challenge cert; `h2`/`http/1.1` ALPN returns normal cert
- `TestTlsMux_ALPNShortCircuit`: Verify `acme-tls/1` connection is closed immediately after handshake (no upstream dial, no proxy hint)
- `TestChallengeReachable`: Existing relay gating tests renamed from `TestHttpChallengeReachable`
- `TestNeedsRelay`: Updated for `SolverTLSALPN01` (still returns true)
- `TestConfigMigration_HTTP01ToTLSALPN01`: Verify solver field migration on config load
- `TestHealthCertResolution_TLSALPN01`: Verify `health.go` cert ID resolution works with the new solver string

### 8.2 Integration Tests

- ACME issuance against Let's Encrypt staging with `PICCOLO_ACME_DIR_URL` set to staging endpoint
- Relay end-to-end: nexus relay → TlsMux → ALPN challenge → cert issued
- Verify port 80 is NOT reachable on remote tunnel after migration

### 8.3 Regression Tests

- Existing DNS-01 tests unchanged
- Existing relay gating tests pass with renamed functions
- Self-hosted `Configure()` flow works end-to-end

## 8.5 Observability

The HTTP-01 path had implicit observability via HTTP access logs (the challenge GET request appeared in Gin logs). TLS-ALPN-01 handshakes are invisible at the HTTP layer. To maintain debuggability:

- **Challenge presented**: `INFO: acme: TLS-ALPN-01 challenge presented for <domain>` (in `Present()`)
- **Challenge served**: `INFO: tlsmux: serving ACME TLS-ALPN-01 challenge cert for <domain>` (in `getCertificate`)
- **Challenge cleaned up**: `INFO: acme: TLS-ALPN-01 challenge cleaned up for <domain>` (in `CleanUp()`)
- **ALPN short-circuit**: No log needed (high-frequency during MPIC — multiple CA perspectives connect in rapid succession). The handshake error path already logs.

This provides three diagnosis points: "was the cert generated?", "was it served to the CA?", "was it cleaned up?" — sufficient to localize any issuance failure.

## 9. Risks & Mitigations

### 9.1 ALPN Filtering by Middleboxes

**Risk:** Some corporate firewalls or CDNs strip non-standard ALPN values, which could prevent `acme-tls/1` from reaching TlsMux.

**Mitigation:** The nexus relay is a WebSocket tunnel — the ALPN negotiation happens inside the tunnel, not on the open internet. Middleboxes between the user and the relay see only the relay's WebSocket connection. The ACME CA connects to the relay's public endpoint, and the relay tunnels the raw TCP to piccolod. ALPN filtering would only be an issue if the relay itself filtered ALPN, which it does not (raw TCP passthrough).

### 9.2 Port 443 Must Be Reachable

**Risk:** TLS-ALPN-01 requires port 443 to be reachable. If only port 80 were tunneled, this would be a problem.

**Mitigation:** Port 443 is always tunneled for HTTPS traffic — it's the primary remote access port. If port 443 is unreachable, remote access itself is broken (not just cert issuance). This is a strict improvement over HTTP-01, which required both ports 80 AND 443.

### 9.3 Concurrent Challenges and Normal Traffic

**Risk:** During ALPN challenge, normal HTTPS traffic to the same domain might be affected.

**Mitigation:** ALPN negotiation is per-connection. A connection with `acme-tls/1` ALPN gets the challenge cert; connections with `h2`/`http/1.1` ALPN (all normal browser/API traffic) get the normal cert. These are independent TLS handshakes. The challenge cert is only served when the client explicitly requests the `acme-tls/1` protocol, which only the ACME CA does.

### 9.4 MPIC Timing and CleanUp Race

**Risk:** Let's Encrypt uses Multi-Perspective Issuance Corroboration (MPIC) — multiple vantage points connect to validate the challenge. Lego calls `CleanUp()` after the ACME server confirms the challenge is complete, but trailing MPIC connections from slower vantage points could arrive after `CleanUp()` has removed the challenge cert from the sink.

**Mitigation:** Lego only calls `CleanUp()` after the ACME server responds with challenge success, which means all MPIC validators have already completed their checks server-side. Trailing TCP connections from validators that haven't completed the TLS handshake yet will see the cert removed and fall through to the normal cert path — the handshake completes with the real cert (or fails if no cert exists yet), and the ACME validation has already succeeded. This is a benign race.

### 9.5 Let's Encrypt TLS-ALPN-01 Support

**Risk:** Not all ACME CAs support TLS-ALPN-01.

**Mitigation:** Let's Encrypt has supported TLS-ALPN-01 since March 2019. It is a stable, widely-deployed challenge type. The `PICCOLO_ACME_DIR_URL` override allows users to point to any ACME CA that supports it.

## 10. Alternatives Considered

### 10.1 DNS-01 for All Modes

Using DNS-01 for self-hosted mode would eliminate both HTTP-01 and TLS-ALPN-01. However, this requires self-hosted users to either:
- Have orchestrator API access for DNS record manipulation
- Configure third-party DNS provider credentials (Cloudflare, Route53, etc.)

This shifts complexity to user configuration. TLS-ALPN-01 is zero-configuration — it works automatically with the existing relay tunnel.

### 10.2 Support Both HTTP-01 and TLS-ALPN-01

Adding TLS-ALPN-01 as an option while keeping HTTP-01 as a fallback increases complexity rather than reducing it. The motivation is simplification — maintaining both solvers defeats the purpose. HTTP-01 has no advantage over TLS-ALPN-01 in this architecture (relay passes raw TCP on both ports).
