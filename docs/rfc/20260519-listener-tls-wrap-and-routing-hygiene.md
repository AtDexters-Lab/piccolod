# Raw Listener TLS-Wrap Opt-In & Port-Claim Routing Hygiene

## Scope

**Problem:** Two coupled bugs surface together when a catalog manifest declares a non-HTTP listener (e.g., gitea's `flow: tcp, protocol: raw, port_claim: 2222` for SSH; namek's `flow: tcp, protocol: raw, port_claim: 53` for DNS-over-TCP). They cascade and currently present as persistent "failed cert" rows in the operator UI for hostnames where a cert was never useful, plus a latent byte-leak where an external HTTP GET to such a hostname's port 80 is silently forwarded to a non-HTTP backend (SSH, DNS) that doesn't speak HTTP.

The two layered bugs:

1. **Wrong premise — cert issued for hostnames that cannot present TLS.** `ResolveCertificatesForListener` (`internal/services/health.go:309`) only skips `flow:tls` listeners. It queues a cert for any other listener whose `DerivedHostLabel` is non-empty, regardless of whether the listener actually terminates TLS at any access path. Raw-TCP listeners with `port_claim` get cert issuance scheduled — those certs can never be presented to a real client (SSH/DNS clients don't speak TLS), so they exist only to fail ACME validation and surface as broken UI rows.

2. **Wrong default — `matchesRemotePort` claims port 80/443 universally.** `matchesRemotePort` (`internal/services/manager.go:646`) defaults to `return remotePort == 80 || remotePort == 443` when an endpoint has no explicit `RemotePorts`. This convenience was written for HTTP-flavor listeners (which typically want both ports for redirect + HTTPS) and applied uniformly to all flow/protocol combinations. For raw-TCP listeners with `port_claim`, this silently asserts that the listener serves port 80 — causing the resolver to dial the raw TCP proxy at the listener's host bind for *any* incoming port-80 connection. External HTTP traffic ends up in the SSH/DNS socket. The same is structurally true for `port_claim`s other than 2222 — the function is blind to `port_claim` as a routing signal.

A third layering issue — HTTP-01 challenge routing being entangled with listener routing — surfaces during investigation but is **not addressed by this RFC**. Its proper architectural fix is TLS-ALPN-01 (RFC 20260327), which moves ACME validation entirely to the TLS mux on :443 and removes port-80 routing from the cert-issuance equation. See "Sequencing" below.

**In scope:**

- A manifest-level opt-in flag (`tls_wrap`) on raw-protocol listeners that controls whether the listener participates in TLS-mux hostname routing (and therefore: `DerivedHostLabel` derivation, TLS mux SNI presentation with a piccolod-managed cert, and ACME cert issuance for the listener's derived hostname). Default `false` for `flow:tcp + protocol:raw`. Rejected by the parser on any other flow/protocol (meaningless for HTTP/WS — they always TLS-wrap; meaningless for `flow:tls` — listener self-manages; meaningless for UDP — no TLS path).
- `RemotePorts` derivation: at endpoint registration time, `port_claim` (when set) is unioned (with uniqueness) into `RemotePorts`. The empty-default `(80, 443)` continues to apply only when `RemotePorts` is empty *and* the protocol is HTTP-flavor (`http` or `websocket`); raw-protocol listeners with empty `RemotePorts` get an empty set (no port match unless port_claim adds entries, or unless `tls_wrap=true` adds 443 for the TLS mux path).
- Parser strictness for `tls_wrap`: rejected on non-raw or non-tcp listeners where it is structurally meaningless (D3).
- Catalog manifests: no edits required — defaults handle ssh, dnstcp, and similar listeners (drop into the no-cert no-host-label path automatically).
- Tests: parser validation of `tls_wrap` placement; `RemotePorts` derivation including `port_claim` union and HTTP-vs-raw default branching; byte-leak regression (port-80 to raw listener returns `ErrNoRoute`); regression on the originating SSH/DNS-TCP failure mode.

**Sequencing relative to TLS-ALPN-01 (RFC 20260327):** this RFC ships first; ALPN-01 is the immediate next RFC. The byte-leak closure (D2) is independent of ACME mechanism and lands now. The cert-issuance gate (D1) is the right shape regardless of validation path. The third bug — HTTP-01 routing for hypothetical configurations like an HTTP listener on `RemotePorts: [8080]` only — is left unfixed in this RFC because its proper architectural answer is to stop using HTTP-01 for those listeners, not to add port-80 path-peeking to the routing layer. ALPN-01 covers it.

**Out of scope:**

- HTTP-01 routing fixes for listeners that don't serve port 80 (e.g., `RemotePorts: [8080]`-only HTTP listeners). Deferred to TLS-ALPN-01 (RFC 20260327). Today no catalog manifest hits this configuration; it remains a hypothetical edge case until ALPN-01 lands.
- TLS-ALPN-01 enablement itself (RFC 20260327). Tracked separately; this RFC's design composes cleanly with it.
- DNS-01 wildcard cert migration for the atdexters.com side. The piccolospace.com side already uses wildcard (visible in the operator's UI as `*.0b864w64r6n5sbt8n8fw.piccolospace.com`); the atdexters.com side issues per-hostname HTTP-01 certs. A wildcard via DNS-01 would eliminate per-subdomain HTTP-01 validation entirely. Separate work item; requires DNS provider integration for atdexters.com.
- Pre-existing failing cert rows for raw listeners (e.g., piccolo0's `ssh-git.piccolo0.atdexters.com` and `dnstcp-namek.piccolo0.atdexters.com`). Cleaning these up requires engaging with the broader cert-lifecycle architecture (see "Deferred: cert lifecycle as state-reconciliation" below). Two earlier iterations of this RFC included a one-shot startup orphan-pruner; both attempts surfaced new sequencing races against the existing event-driven cert reactors (`requeueOutstandingIssuances`, `RestoreServices`, async base hydration) because the cert lifecycle in piccolod is event-handled rather than state-reconciled. The architecturally-correct fix lives in a follow-up RFC; meanwhile the operator workaround for piccolo0 is app uninstall + reinstall (no admin-side per-cert delete endpoint exists today; adding one is a candidate scope-change but out of this RFC's scope).
- A general "expose this raw-TCP listener via multiple paths simultaneously" capability (port_claim + tls_wrap together is one variant of this; arbitrary multi-path is not). The RFC supports both flags coexisting on a single listener (one for direct-port access, one for TLS-mux access at the hostname); broader path orchestration is not in scope.
- UX changes to the cert list in Settings → Remote Access. Fresh installs after this RFC do not produce failing rows for raw listeners in the first place; no UI work required for that case. Pre-existing failing rows remain visible until the deferred cert-lifecycle work lands or the operator manually intervenes.

---

## Background

### Origin

On 2026-05-19, gitea install on a Linode-hosted piccolo device (piccolo0) surfaced two failing cert rows in Settings → Remote Access — `ssh-git.piccolo0.atdexters.com` (new) and `dnstcp-namek.piccolo0.atdexters.com` (pre-existing) — both `flow: tcp, protocol: raw` listeners with `port_claim`. App functionality unaffected (direct-port access via port_claim works). Investigation traced two independent code paths that compose unfortunately on raw listeners: cert issuance for hostnames that cannot present TLS, and `matchesRemotePort` defaulting to `(80, 443)` for any endpoint with empty `RemotePorts` (causing port-80 traffic to misroute into the SSH socket). The operator reframe — "why issue a TLS cert for SSH at all?" — motivates D1 (`tls_wrap` opt-in). A third concern (HTTP-01 challenge routing for non-HTTP listeners) is structurally a hostname-level rather than listener-level responsibility; its proper architectural fix is TLS-ALPN-01 and is deferred to RFC 20260327.

### Cert-needed matrix (current vs. desired)

| Flow | Protocol | Today: cert queued? | Today: useful? | Desired |
|------|----------|---------------------|----------------|---------|
| tcp  | http     | yes                 | yes (L7 chain TLS-terminates on :443) | yes |
| tcp  | websocket| yes                 | yes (L7 chain TLS-terminates on :443) | yes |
| tcp  | raw      | yes                 | **no** unless TLS-mux SNI access is intended | **opt-in via `tls_wrap`** |
| tls  | any      | no (already skipped at health.go:311) | n/a — listener self-manages | no (unchanged) |
| udp  | any      | no (`IsEligibleForHostRouting` returns false) | n/a — no TLS path | no (unchanged) |

The `tcp+raw` row is the entire surface area of the bug. The other rows are correct today and stay correct under this RFC.

---

## Decisions

### D1 — `tls_wrap` flag on raw-protocol listeners

A new boolean field on `api.AppListener`. Semantics:

- Valid only when `flow == tcp && protocol == raw`. Parser rejects with an explicit error for any other combination.
- Default `false`.
- When `false` (or absent): the listener participates in *direct-port access only* (port_claim / bind_ports paths). No `DerivedHostLabel`. No TLS mux SNI presentation. No cert issuance. Hostname-routed access to this listener is unavailable.
- When `true`: the listener gains `DerivedHostLabel`. TLS mux on :443 SNI-routes to this listener and presents a piccolod-managed cert. ACME cert issuance is scheduled for the derived hostname. The TLS mux terminates TLS using the cert and forwards plaintext bytes to the backend (the "stunnel-like" use case — backend speaks plaintext, edge speaks TLS).

Per-listener granularity matches the existing flow/protocol/port_claim axis. App-level wouldn't fit — a single app may have raw listeners with and without TLS-wrapping intent.

### D2 — `RemotePorts` derivation

At endpoint registration (in `internal/services/manager.go`'s allocator/builder), `RemotePorts` is computed as the union of:

1. Explicit `RemotePorts` from the manifest, if set.
2. `port_claim` value, if set, appended uniquely.
3. The HTTP-flavor default `[80, 443]` only when (a) `RemotePorts` is empty after steps 1-2 *and* (b) protocol is `http` or `websocket`.
4. For `tls_wrap: true` raw listeners: `443` is appended uniquely (the TLS mux path).

Raw listeners with `tls_wrap: false` and no explicit `RemotePorts` end up with `RemotePorts = [port_claim]` (if port_claim is set) or `[]` (if not). The empty-default branch in `matchesRemotePort` becomes a true HTTP-flavor convenience — never a wildcard.

The function `matchesRemotePort` itself stays as-is in shape (the empty-default-to-80/443 branch is preserved because it's correct for HTTP-flavor listeners with no explicit RemotePorts); the derivation logic ensures raw listeners never reach that branch with empty RemotePorts.

### D3 — Parser validation strictness

`tls_wrap: true` on a listener whose flow/protocol is *not* `tcp+raw` is a parse error, with a message naming the listener and the violating combination. This is strictly additive — the rejection only fires when someone explicitly adds the new field to a non-applicable listener. Pre-existing manifests without `tls_wrap` are unaffected.

Specific rejection cases:

- `tls_wrap: true` on `flow:tls`: TLS is already terminated by the listener — `tls_wrap` would be meaningless and likely indicates the catalog author misunderstood the model.
- `tls_wrap: true` on `protocol:http` or `protocol:websocket`: piccolod's L7 chain already TLS-terminates on :443 — `tls_wrap` would be meaningless.
- `tls_wrap: true` on `flow:udp`: no TLS path exists. `tls_wrap` is structurally inapplicable.

`tls_wrap: false` remains implicit (the default) and not surfaced — manifests don't need to spell it out for non-raw listeners. Use the existing `newValidationError` shape with code `INVALID_TLS_WRAP` (or similar; align with existing error-code conventions).

**What's deliberately NOT rejected here:** `flow:tcp, protocol:raw` with neither `port_claim` nor `tls_wrap: true`. Such a listener is structurally unreachable from outside the device, but rejecting it at parse time fires on the cold-start `RestoreServices → GetAppDefinition → ParseAppDefinition` path (`app_manager.go:1280-1283`, `filesystem.go:352`), which would brick pre-existing sideloaded apps with such a listener on upgrade — the manifest fails to parse, `RestoreServices` logs WARN and `continue`s without restoring the proxy, the listener silently goes dark. The "structurally unreachable" check is genuinely worth surfacing but needs a parse-mode distinction (strict at install/edit, permissive at restore) that isn't worth introducing for this RFC's scope. Captured as a deferred follow-up.

**On `tls_wrap: true` semantics — guidance for catalog authors:** D1 enables TLS termination by piccolod's mux + plaintext forwarding to the backend. This is the stunnel-style pattern, correct *only* for backends that accept the same byte stream the client sends, just transported over plain TCP instead of TLS-wrapped TCP. The parser cannot enforce semantic correctness; this is a documentation responsibility.

**Do NOT set `tls_wrap: true` on listeners for these protocols** — the cert will issue cleanly but no client will be able to connect:

- **SSH** — clients speak the SSH binary protocol directly on the raw TCP socket. They do not initiate TLS first. The `:443` SNI-routed path would terminate TLS and feed gibberish-after-TLS-decrypt to sshd. Use `port_claim` for SSH and leave `tls_wrap: false`.
- **Plain DNS-over-TCP** — DNS-over-TLS (DoT) clients use a different wire format and target port 853, not a TLS-wrapped raw DNS stream. Use `port_claim: 53` for DNS-TCP.
- **Anything already speaking TLS** — Postgres native, MySQL native with SSL handshake, anything with its own embedded TLS. Use `flow: tls` (passthrough, listener self-manages cert) instead.
- **Binary protocols with their own framing** — Redis RESP, AMQP native, MQTT (use `mqtts://` directly if you want TLS), proprietary wire formats. Either the protocol embeds its own TLS or it doesn't benefit from edge wrapping.

**Use `tls_wrap: true` only when** the backend reads its protocol from a plain TCP socket AND the client will speak TLS on the wire toward the device. Prototypical correct uses: line-based plaintext protocols where edge TLS is added for transport security (custom protocols, certain IoT patterns). When in doubt, leave `tls_wrap: false` and expose via `port_claim` — direct-port access works for any backend protocol.

---

## Site list

The set of decision surfaces that change. Implementation expands these to code-level diffs.

| File | What changes |
|------|--------------|
| `internal/api/types.go` | Add `TLSWrap bool` field to `AppListener` (yaml: `tls_wrap`, json: `tls_wrap`). |
| `internal/app/parser.go` | Validate `tls_wrap` placement (D3). Reject `tls_wrap: true` on non-raw, non-tcp listeners. Use `newValidationError("INVALID_TLS_WRAP", …)` shape. No new rejection on existing field combinations — strictly additive. |
| `internal/services/types.go` | `IsEligibleForHostRouting` signature gains `tlsWrap bool` parameter. Returns true for: any `flow:tls`; `flow:tcp` with HTTP/WS protocol; `flow:tcp` with raw protocol *and* `tlsWrap == true`. Returns false otherwise. |
| `internal/services/manager.go` | All four call sites of `IsEligibleForHostRouting` (currently at :489, :543, :704, :1134) pass the new `tlsWrap` argument. Endpoint registration logic implements D2 (`RemotePorts` derivation with `port_claim` union and protocol-aware default). |
| `internal/services/health.go` | `ResolveCertificatesForListener` — no signature change. Cert queue gates on `DerivedHostLabel != ""`, which becomes empty automatically for non-wrapped raw listeners via D1 composition. |
| Tests: `internal/app/parser_test.go` | Add `tls_wrap` validation cases (valid on tcp+raw; valid absent on any flow/protocol; rejected only when `tls_wrap: true` is set on tcp+http, tcp+websocket, tls, udp). |
| Tests: `internal/services/types_test.go` | Update `IsEligibleForHostRouting` table tests for the new parameter. |
| Tests: `internal/services/manager_test.go` | `RemotePorts` derivation: port_claim union, HTTP-default branch, HTTP+port_claim composition (port_claim alone determines RemotePorts; the 80/443 default does NOT layer on top), raw-without-port-claim defaults to empty, raw-with-tls-wrap includes 443. Byte-leak regression: `serviceRemoteResolver.Resolve("ssh-git.<portal>", 80, isTLS=false)` returns `(0, false)` (assertion on resolver return value, not downstream byte recording). |

No catalog (piccolo-store) edits required. The default-false behavior of `tls_wrap` drops gitea's ssh listener and namek's dnstcp listener into the no-cert no-host-label path on the next manifest re-render.

---

## Migration

**Fresh installs** post-RFC:

- Gitea SSH listener defaults to no `tls_wrap` → no cert queued, no UI row. SSH access via port_claim:2222 is unaffected.
- Namek dnstcp listener defaults to no `tls_wrap` → no cert queued, no UI row. DNS-over-TCP access via port_claim:53 is unaffected.
- Catalog authors who *do* want TLS-wrapping for a raw listener (stunnel-like, with a backend protocol that fits the wrapping pattern per D3's guidance) set `tls_wrap: true` explicitly.
- Byte-leak (D2) is closed at registration time — `matchesRemotePort` no longer defaults to (80, 443) for raw listeners.

**Pre-existing devices with failing cert rows** (e.g., piccolo0's `ssh-git` and `dnstcp-namek` rows): not auto-cleaned by this RFC. See Deferred → cert lifecycle as state-reconciliation. Operator workaround: uninstall + reinstall the affected app forces a fresh manifest parse and clears the orphan rows. Real cost on hosted apps with operator data — operator weighs against the UI cosmetic cost.

**Backward compatibility & rollback:** field is additive. Newer piccolod parses older manifests fine. Older (pre-RFC) piccolod ignores `tls_wrap` and falls back to pre-RFC behavior (cert queued for raw listeners, byte-leak present). One-sided: newer binaries handle older manifests cleanly; older binaries do not handle newer manifests cleanly. No data migration in either direction.

---

## Tests

Listed by decision; each maps to one or more test files in the site list above.

### D1 + D3 — `tls_wrap` parser

- Accept `tls_wrap: true` on `flow:tcp, protocol:raw`.
- Accept `tls_wrap: false` (or absent) on `flow:tcp, protocol:raw` (any combination with or without `port_claim` — D3 is strictly additive, does not reject pre-existing field combinations).
- Reject `tls_wrap: true` on `flow:tcp, protocol:http` with specific error message naming the listener.
- Reject `tls_wrap: true` on `flow:tcp, protocol:websocket`, `flow:tls`, `flow:udp` similarly.
- Default to false when field is absent — verified via parsed struct.
- Cold-start regression: `parseAppDefinitionWithLegacyMigration` accepts pre-RFC manifests (any flow/protocol combination without `tls_wrap`) without error. Confirms `RestoreServices → GetAppDefinition` path does not brick pre-existing sideloaded apps.

### D2 — `RemotePorts` derivation

- HTTP listener, no `port_claim`, no explicit `RemotePorts` → `RemotePorts = [80, 443]`.
- HTTP listener with explicit `RemotePorts: [8080]` → `RemotePorts = [8080]` (no default).
- HTTP listener with `port_claim: 8080`, no explicit `RemotePorts` → `RemotePorts = [8080]` (port_claim alone; the HTTP-flavor default applies *only* when `RemotePorts` is empty after port_claim union).
- HTTP listener with `port_claim: 80`, no explicit `RemotePorts` → `RemotePorts = [80]` (port_claim alone; no synthetic 443).
- Raw listener with `port_claim: 2222`, `tls_wrap: false` → `RemotePorts = [2222]`.
- Raw listener with `port_claim: 2222`, `tls_wrap: true` → `RemotePorts = [2222, 443]`.
- Raw listener without `port_claim`, `tls_wrap: true` → `RemotePorts = [443]`.
- Raw listener without `port_claim`, `tls_wrap: false` → `RemotePorts = []` (silently accepted at registration; listener is unreachable from outside, which is the operator's choice if they declared the manifest this way — see deferred parser-warning follow-up).
- Explicit `RemotePorts` + `port_claim` (overlapping or non-overlapping) → union with uniqueness.

### Composition with existing event-driven cert removal (unchanged by this RFC)

- In-process flip of `tls_wrap: true → false` on a live raw listener → `services.ServiceManager.Reconcile` emits `Removed` event → existing handler at `gin_server.go:3340-3368` calls `RemoveHostnameCertificate`. Confirms the D1 → `DerivedHostLabel` → cert-enqueue composition retains correct runtime cert removal on label flip.
- `Deactivated` event (app restart, not uninstall) → existing handler does NOT remove cert (regression test for the deactivated-vs-removed distinction).
- Re-enable: listener re-renders with `tls_wrap: true` → `DerivedHostLabel` set → existing event-driven enqueue path (`gin_server.go:~3322`, `:~3400`) re-queues issuance.

### Byte-leak regression (D2)

- Endpoint registered for `ssh-git` listener (raw TCP, `port_claim: 2222`). `serviceRemoteResolver.Resolve("ssh-git.<portal>", 80, isTLS=false)` returns `(0, false)`. Assertion is on the resolver return value directly — not on downstream byte recording, because host bind ports can be recycled to other listeners across reconciles, making a byte-level assertion fragile and ambiguous.
- Same setup, `serviceRemoteResolver.Resolve("ssh-git.<portal>", 2222, isTLS=false)` returns the SSH host bind port. Direct-port access regression preserved.
- Same setup, `matchesRemotePort(sshEp, 80)` unit test returns `false`. Confirms the underlying predicate behavior.

### Originating-bug regression — piccolod-controlled paths only

- Catalog parses gitea manifest with ssh listener (`flow:tcp, protocol:raw, port_claim:2222`, no `tls_wrap`).
- Endpoint registration produces empty `DerivedHostLabel` for ssh listener.
- `ResolveCertificatesForListener` returns no cert ID for the ssh listener; no cert is enqueued for fresh installs.
- New installs: UI cert list does not contain a row for ssh-git.
- Resolver returns `ErrNoRoute` for port-80 requests on `ssh-git.<portal>` (byte-leak regression above).

*DNS-side hygiene (whether namek deletes its A record when `DerivedHostLabel` goes empty, and whether atdexters.com-side DNS persists hostnames piccolod no longer claims) is upstream of this RFC. The originating regression test does not assert DNS-side behavior; it tests only what piccolod controls. DNS lifecycle on label loss is captured as a deferred follow-up.*

---

## Deferred follow-ups

Captured here for completeness; not in scope.

- **Cert lifecycle as state-reconciliation — the load-bearing architectural follow-up.** Anchored at `project_cert_lifecycle_reconciler.md` (memory). Piccolod's cert lifecycle today is event-handled: multiple independent reactors (`Start` enqueuing pending entries, `ServiceEndpointsChanged.Removed` handler removing certs, `reloadFromStorage` requeuing on unlock, `requeueOutstandingIssuances` re-arming jobs, ACME worker processing) each act on their own subset of state with no central authority that owns "what should the cert inventory look like, given the current consistent state of {live listeners, aliases, portal config, namek state}?" The races this produces are why two earlier iterations of this RFC failed to land a clean startup orphan-cleanup module (the user-visible symptom: piccolo0's failing `ssh-git` and `dnstcp-namek` rows). The correct shape is a state-reconciler that computes the desired cert set from {liveListeners, aliases, portalConfig, namekState}, snapshots the observed inventory, computes a diff, and applies it atomically — Kubernetes-controller pattern. This is a multi-week effort that touches every existing reactor and warrants its own RFC. Out-of-scope here; tracked as the immediate cert-lifecycle work after this RFC and TLS-ALPN-01 (RFC 20260327) ship.

  Symptoms that share this root cause (would be resolved by the state-reconciliation work):
  - piccolo0's persistent failing rows for `ssh-git.piccolo0.atdexters.com` and `dnstcp-namek.piccolo0.atdexters.com` — orphan inventory rows with no live listener.
  - In-flight ACME race in `RemoveHostnameCertificate` — no shared lock between worker and removal path.
  - `project_cert_inventory_persistence.md` — cert writes failing when issued before unlock (write reactor doesn't know about persistence-ready state).
  - Synthetic-Removed for crash recovery (crash mid-Reconcile drops `Removed` event delivery).
  - DNS record lifecycle on `DerivedHostLabel` going empty (DNS reactor independent of cert reactor).
  - The two failed iterations of D3 in this RFC's review history — concrete evidence of the architectural pattern producing the races.

- **TLS-ALPN-01 enablement (RFC 20260327) — the next RFC.** Closes the third bug surfaced during this investigation (HTTP-01 routing for HTTP listeners that don't serve port 80, and for `tls_wrap: true` raw listeners). Moves ACME validation entirely to :443 via the TLS mux, hostname-level rather than listener-level. Composable with this RFC's D1/D2 — they assume nothing about which ACME path is used.

- **Parser strictness for `flow:tcp, protocol:raw` with neither `port_claim` nor `tls_wrap: true`.** Such a listener is structurally unreachable from outside the device and almost certainly an authoring mistake. Surfacing this as a parse-time rejection requires a parse-mode distinction (strict at install/edit time, permissive at `RestoreServices`/`RestoreFromPodman` cold-start) so pre-existing sideloaded apps don't brick on upgrade. Worth doing once the parse pipeline supports a strict/permissive mode flag.

- **Catalog manifest sync for new fields** — covered by existing deferred finding `project_manifest_sync.md`. Independent concern; no longer load-bearing for this RFC's user-visible outcome (which now narrowly affects fresh installs only) but remains a real gap for other manifest-evolution concerns.

- **DNS record lifecycle on label loss.** When a listener loses `DerivedHostLabel`, namek-managed DNS records for that hostname should be deleted (and atdexters.com-side records acknowledged as out-of-band). Subsumed under the state-reconciliation work above.

