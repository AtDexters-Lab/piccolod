# RFC: Namek-Managed Remote Access

**Date:** 2026-03-12
**Status:** Accepted

## Motivation

Piccolod needs TPM-backed device identity via namek-server for managed remote access through Nexus relays. Today the remote subsystem supports a single self-hosted Nexus connection with HMAC tokens. The target is dual concurrent Nexus connections — namek-managed (always-on by default, TPM-attested) and self-hosted (optional, user-configured) — plus DNS-01 ACME for namek hostnames.

Key invariants:
- Namek identity is always-on by default (opt-out via toggle in setup or settings)
- Namek relay and self-hosted Nexus coexist independently
- Peers reach each other via namek-assigned hostnames (`<slug>.baseDomain`)
- Self-hosted Nexus supports TCP/UDP port claims; namek does not
- Alias domains work with self-hosted only (namek alias support planned but not ready)
- Device must be reachable via namek relay even in locked state — identity/relay credentials persist on network-bootstrap (pre-unlock)
- TPM is an independent module (`internal/tpm/`) — identity is one consumer; future consumers include network-bootstrap encryption

## Architecture

```
Supervisor
  ├─ TPM Module (internal/tpm)
  │   - hw TPM / swtpm fallback
  │   - AK persistence
  │
  ├─ Identity Service (internal/identity)
  │   - uses tpm.Device
  │   - Enrollment via namekclient
  │   - persists to network-bootstrap
  │   │
  │   ├─ NamekTokenProvider (backend.TokenProvider)
  │   └─ NamekACMEClient (acme.OrchestratorClient)
  │
  └─ Remote Manager
      ├─ namekAdapter (always-on if enrolled)
      └─ selfHostedAdapter (user-configured)
```

### Connection Transparency Boundary

Both Nexus adapters (namek + self-hosted) share the same `router.Manager` and `RemoteResolver`. After `BackendAdapter.connectHandler()` resolves a hostname to a local port and dials `127.0.0.1:<port>`, all downstream modules (gin server, TLS mux, service proxy, app containers) are completely source-agnostic.

## Storage Model

### Pre-Unlock vs Post-Unlock

```
/piccolo-core/swtpm/                    ← swtpm virtual TPM state (outside network-bootstrap)
  /tpm2-00.permall                       ← virtual TPM image (IS the TPM)
  /localca/                              ← EK certificate CA hierarchy

/piccolo-core/network-bootstrap/        ← ALWAYS available pre-unlock
  /tpm/
    /ak_pub, /ak_priv                   ← AK blobs (attestation key state)
  /remote/
    /identity.json                      ← namek identity config
    /certs/                             ← namek TLS certs

/piccolo-core/mounts/control-plane/     ← ONLY after admin unlock
  /remote.json                          ← self-hosted nexus config
```

**Why this split:**
- AK blobs inside network-bootstrap: AK is for attestation, NOT decryption. TPM device can be opened without AK.
- swtpm state outside network-bootstrap: swtpm state IS the virtual TPM, must exist before any TPM operation.
- Recovery: everything in network-bootstrap is recoverable via re-enrollment (namek identifies devices by EK fingerprint).
- swtpm state is critical: loss means permanent identity loss (new device ID, hostname, slug).

## Boot Sequence

1. Open TPM device: hw via `/dev/tpmrm0` or swtpm from `/piccolo-core/swtpm/`
2. (Future: use TPM to decrypt network-bootstrap)
3. Load AK blobs from `network-bootstrap/tpm/`
4. Identity service starts immediately (config on network-bootstrap)
5. Namek adapter connects immediately (device reachable before admin unlock)
6. Self-hosted adapter starts only after admin unlock (config on encrypted control volume)
7. Namek ACME certs stored on network-bootstrap; self-hosted certs on control volume

## Resolver & TLS Mux

The resolver maintains a unified `remoteBases []remoteBase` list (source-tagged for removal) rather than separate per-adapter hostname sets. Each source (`"self-hosted"`, `"namek"`) manages its own entries via `SetRemoteBases(source, bases)`.

## ACME

Per-cert solver selection via `IssueWithSolver(solver, orchClient, ...)`. Each cert's `Solver` field drives selection. Lego client is created per-issuance (thread-safe). Namek certs use DNS-01 via namekclient; self-hosted certs use HTTP-01 or orchestrator DNS-01.

## API Surface

```
GET  /api/v1/identity           → identity status
POST /api/v1/identity/enroll    → trigger enrollment
POST /api/v1/identity/enable    → enable namek
POST /api/v1/identity/disable   → disable namek
POST /api/v1/identity/hostname  → set custom hostname
POST /api/v1/identity/namek-url → change namek URL
```

## Security Considerations

- EK fingerprint is the identity anchor — namek never sees the EK private key
- swtpm state directory is critical on devices without hw TPM: loss = permanent identity loss
- TPM ownership recovery: 401 from namek triggers AK recovery + re-enrollment
- 403 (suspended/revoked) stops namek adapter, requires operator resolution via namek admin
- All identity state on network-bootstrap is recoverable via re-enrollment
