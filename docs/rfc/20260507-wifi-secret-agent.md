# WiFi NM Secret Agent

## Scope

**Problem:** Connecting to a WiFi LAN uplink via the captive portal flow on the RPi 400 (brcmfmac) consistently fails: NM enters `need-auth` ~10 ms into the `config` phase, finds no registered secret agent, and tears down the volatile connection within ~8 ms. The piccolod-side passphrase is byte-correct end-to-end (proven in `diag(network)` 3949007 + `diag(captive)` 74e2911) — `psk_len=11` reaches the inline `802-11-wireless-security.psk` field in the AddAndActivateConnection dict — yet NM still falls into the agent path. With no agent registered, every `need-auth` is fatal in the agent-discovery timeout window. The captive portal flow is therefore unusable on this device.

**In scope:**

- Register a NetworkManager secret agent owned by piccolod that answers `GetSecrets` for connections piccolod initiated via `Manager.Connect`.
- Wire `Manager.Connect` to populate the agent's in-memory cache *before* invoking `nm.Connect`, and drain it on Connect return (success **or** failure).
- Gate the agent registration to piccolod's lifetime: register at startup once `networkManager` is constructed; unregister on shutdown; recover on NM-restart (NameOwnerChanged on `org.freedesktop.NetworkManager`).
- Preserve existing behavior: continue passing inline secrets in the AddAndActivate dict (defense in depth — agent only matters when NM falls through to it).
- Cover the failure path: when `GetSecrets` is called for a setting/SSID/UUID combination piccolod does not own, return an empty response so NM falls through to other agents (none exist on this appliance) or fails cleanly.
- Tests: stub-bus unit tests for the agent (D-Bus method shapes, cache scoping, concurrency); integration test for the populate-before-AddAndActivate / drain-after-return ordering inside `Manager.Connect`.

**Out of scope:**

- Replacing the inline-PSK path in `nmclient/connection.go` with a "set `secret-flags=1` (agent-owned) and rely on the agent" model. The agent is additive defense-in-depth; we do not rewrite the AddAndActivate dict shape. This avoids regressing the LAN-portal path that already works on inline secrets, and keeps the change boundary tight.
- A general-purpose NM-secrets-management subsystem (VPN secrets, 802.1X EAP secrets, mobile broadband). WiFi-PSK only.
- Persisting secrets in piccolod across restarts. Cache is transient, in-memory only.
- UX changes to the captive portal (the "wrong password" misnomer banner, the rate-shaped second-AP-reappearance behavior) — both noted as `deferred_` follow-ups, neither caused by the agent gap.
- Replacing the existing two-loop network supervision (legacy resilience + new supervisor); already deferred per `deferred_health_tracker_collapse.md` and `deferred_mdns_resilience_collapse.md`.
- Changes to `errConnectTimeout` sentinel naming or the captive portal copy "check your password and try again" — deferred UX cleanup.

---

## Background — what the diagnostic chain proved

Three commits already shipped (`3949007`, `74e2911`) instrument the JS → ECDH → Go → NM byte path. On the RPi the chain is byte-perfect every cycle: `client_diag={input=11 utf8=11 boxed=27 nacl_util=true}` → `psk_len=11 ciphertext_len=36` → `Connect entry psk_len=11`. NM still emits *"access point 'D111' has security, but secrets are required"* and exits `need-auth` in 8 ms with reason `connection-removed`. That fingerprint means NM, on this hardware/version/transition combination, does not honor the inline PSK across the AP→STA mode transition. We do not need to nail down the precise NM-internal cause — the `need-auth` path is the documented escape hatch, and an agent is the standard way to satisfy it.

The same flow over the LAN-portal path (`/api/v1/wifi/connect`, no preceding AP teardown) succeeds on the same SSID with inline secrets — confirming the failure is bound to the AP→STA transition window. The agent fixes the captive flow without touching the LAN-portal path because the LAN flow never enters `need-auth`.

---

## Alternatives considered

The diagnostic chain proves the inline PSK is byte-correct end-to-end and NM still falls into `need-auth` on the AP→STA transition. We don't have a kernel/wpa_supplicant trace that pinpoints the upstream cause. Three simpler fixes were considered and rejected; one is honest deferred uncertainty.

### Alt-A. `secret-flags=1` (NM_SETTING_SECRET_FLAGS_AGENT_OWNED) on the inline PSK
Set `802-11-wireless-security.psk-flags = 1` in the AddAndActivate dict; tell NM the secret comes from an agent. **Rejected:** this still requires registering an agent (the flag *means* "ask the agent for this"), so it does not eliminate the producer surface — it just makes the agent the only path. Worse, it changes the LAN-portal path (which works today) to also depend on the agent, adding a new failure mode where the LAN flow becomes broken if the agent is unhealthy. The chosen approach (D9 defense-in-depth) keeps the inline PSK as primary and the agent as fallback — strictly safer.

### Alt-B. Two-step AddConnection then ActivateConnection
Issue `org.freedesktop.NetworkManager.Settings.AddConnection` to persist the profile (which writes secrets to NM's keyfile storage) followed by `org.freedesktop.NetworkManager.ActivateConnection` against the saved-profile path. **Rejected as primary fix, kept as deferred experiment:** there is a plausible mechanism (saved profiles use NM's persistent-storage secret resolution path, distinct from AddAndActivate's transient dict path) where this could sidestep the need-auth fallthrough. But: (a) we do not have empirical evidence that it does, on this brcmfmac/NM combination; (b) it changes the connection-lifecycle model (saved profile lingers on failure unless explicitly deleted, complicating the existing rollback logic); (c) if it doesn't fix the underlying issue, we now have both behavior changes and need the agent anyway. The agent fix is hardware-NM-quirk-agnostic; this experiment is brittle. Captured as `deferred_` follow-up: if the agent path proves problematic in the field, run a dev-VM AB test of the two-step approach.

### Alt-C. Wait for `wlan0` supplicant interface state to be stably `disconnected` (not `interface_disabled`) before AddAndActivate
The diagnostic shows the `disconnected → interface_disabled → disconnected` transition happens *during* NM's activation, suggesting the brcmfmac mode-switch overlaps with NM's secret-load step. A pre-flight wait could in principle land the activation cleanly. **Rejected:** the diagnostic also shows the `interface_disabled` window happens *after* NM's "secrets are required" log line in cycle 1, meaning the supplicant transition is a *consequence* of the failed activation, not a precursor. Waiting for it to settle before AddAndActivate would not have helped. (See diag log `(3).log:2585–2620` for the timing.) An explicit wait gates on a signal that does not exist before the failure has already occurred.

### Alt-D. Do nothing; investigate upstream NM/brcmfmac
Honest acknowledgement: the agent fix works around an upstream condition we have not fully characterized. **Rejected as the only response:** the appliance ships now, the captive flow is unusable, and the agent path is the documented NM API for exactly this kind of fallback. Noted in the risk register: if a kernel/NM upstream fix later eliminates the need-auth fallthrough, the agent registration becomes net-zero scope but does no harm (it answers only when NM asks).

---

## Decisions

### D1. New package `internal/network/nmagent`

A new package owns the agent. Boundary: the package exports a `*Agent` struct whose lifecycle is managed by `Manager`, plus the agent's `Stash` / `Drain` helpers used by `Manager.Connect` to populate / drain the per-connect-attempt cache.

Rationale: the agent has its own D-Bus connection lifecycle (private bus, registration with `org.freedesktop.NetworkManager.AgentManager`, NameOwnerChanged watcher) and its own concurrency surface (D-Bus method handlers run on godbus's read goroutines). Bundling this into `nmclient` would conflate consumer-side and producer-side D-Bus surfaces. Separating keeps both packages single-purpose.

### D2. Private system-bus connection (`dbus.SystemBusPrivate`)

The agent registers on its own private connection rather than sharing `dbus.SystemBus()` (the singleton used by `nmclient.NewDBusClient`). Two reasons: (a) `org.freedesktop.NetworkManager.AgentManager.Register` ties the agent to the bus-connection identity, so re-registration after NM restart needs predictable connection-ownership semantics; (b) godbus signal multiplexing on the shared bus would route NM's `StateChanged` and other consumer signals through the agent goroutine path. Mirrors the existing precedent in `nmclient.NewPrivateDBusClient`.

### D3. Agent identifier `"piccolod-wifi"`

Single registered identifier. NM treats this as the agent name in audit logs (e.g., `agent_name="piccolod-wifi"`). Capabilities flag: `NM_SECRET_AGENT_CAPABILITY_NONE` — we do not handle VPN hints or any other extension.

### D4. Cache key: SSID, not UUID

The agent caches `{ssid → passphrase}`, not `{uuid → passphrase}`. UUID is generated by NM and returned only after `AddAndActivateConnection` completes; NM may invoke `GetSecrets` during the activation pipeline before that return value is observable in `Manager.Connect`. SSID is known to piccolod at the moment Stash is called (before the AddAndActivate D-Bus call) and is present in the connection-settings dict NM passes to `GetSecrets` (`802-11-wireless.ssid` byte field).

Lookup policy and SSID byte/UTF-8 mismatch handling is fully specified in D6.

Q3 invariant audit: SSID uniqueness within piccolod's in-flight Connect set is enforced by `Manager.connectMu` — at most one Connect call is active at a time. The cache may transiently hold up to two entries (primary + rollback per D5), each keyed by a distinct SSID. A map (vs. single slot) is used for clarity and to support the primary+rollback case cleanly.

### D5. Cache lifetime: bracket every AddAndActivate site reachable from `Manager.Connect`

`Manager.Connect` has **three** AddAndActivate-reachable sites: the primary `nm.Connect` call, plus **two** rollback sites — one early (synchronous error from `nm.Connect` at `manager.go:338`) and one late (post-`WaitForActivation` failure at `manager.go:386`). All three need agent backstop because each is its own `AddAndActivateConnection` against the same brcmfmac AP→STA transition window.

```
Manager.Connect(ssid, passphrase):
    agent.Stash(ssid, passphrase); defer agent.Drain(ssid)
    agent.SetConnectInFlight(true); defer agent.SetConnectInFlight(false)

    err = nm.Connect(...)
    if err != nil:
        if rollbackSnapshot != nil and rollbackSnapshot.PSK() != "":
            agent.Stash(snap.SSID(), snap.PSK()); defer agent.Drain(snap.SSID())
        nm.RestoreConnection(rollbackSnapshot)        # early rollback site
        return err

    ... WaitForActivation / SSID-match check ...

    if !activated:
        if rollbackSnapshot != nil and needsRestore and rollbackSnapshot.PSK() != "":
            agent.Stash(snap.SSID(), snap.PSK()); defer agent.Drain(snap.SSID())
        nm.RestoreConnection(rollbackSnapshot)        # late rollback site
```

Pseudocode shape only.

**New accessor:** implementation adds `(*nmclient.ConnectionSnapshot).PSK() string` mirroring the existing `SSID()` accessor (`nmclient/types.go:302–321`). Pulls from `Settings["802-11-wireless-security"]["psk"]` via `variantString` — same extraction `RestoreConnection` already does inline at `connection.go:162–167`. Both call sites then call the accessor; no duplication.

**Empty-PSK rollback case:** `snapshot.PSK()` is "" in two distinct sub-cases that today's `RestoreConnection` (line 199 `if psk != ""`) handles correctly:
- **Open-AP rollback** — original profile had no `802-11-wireless-security` section. `RestoreConnection` builds a no-security profile and re-associates. Agent is irrelevant (no need-auth on open APs).
- **Secured-but-unrecoverable** — `getConnectionSecrets` (`connection.go:135`) silently swallows D-Bus errors; rare cases include profiles originally `secret-flags=1` agent-owned, NM keyfile read glitch, NM mid-restart. `RestoreConnection` builds an open-profile AddAndActivate against a WPA SSID, NM rejects on WPA-mismatch — same end-state as today.

**Decision:** when `snapshot.PSK() == ""`, **skip Stash only** (Stash would be rejected by Q2's empty guard anyway). **Always proceed with `RestoreConnection`** — preserves the current open-AP-rollback behavior (which actually works) and matches today's behavior for the secured-but-unrecoverable case. Log INFO `nmagent: rollback Stash skipped — snapshot has no PSK ssid=...` only when the section *was* present; for open-AP rollback no log is needed (it's the normal open-profile path). The agent never fires on rollback's open-profile AddAndActivate (NM doesn't enter need-auth on open APs), so Stash is genuinely dead code for both sub-cases.

**Sub-case asymmetry on the early-error rollback site:** the early-error site triggers when `nmclient.Connect` returns synchronous error from any of three sub-paths: `SavedWiFiConnections` D-Bus failure (`connection.go:19`), `Delete` of pre-existing same-SSID profile (`connection.go:37`), or `AddAndActivateConnection` itself (`connection.go:67`). Only the third actually invokes NM's activation pipeline; agent backstop on rollback is genuine defense only for that sub-case. The first two over-defensively bracket Stash/Drain that won't be queried — harmless cost (atomic store + map insert + map delete), kept for code uniformity rather than tightening the conditional.

**Drain is keyed by SSID** and clears only that entry. The primary and rollback Stashes coexist in the cache during RestoreConnection (LIFO defer ordering — rollback Drain runs first, primary Drain runs second on Connect return). This is safe: GetSecrets matching is keyed by SSID extracted from NM's call dict, so the primary entry answers only the primary SSID's lookup and the rollback entry answers only the rollback SSID's lookup. Only the single-cache-entry-fallback (D6) blurs this — and that fallback is gated on `len(cache) == 1`, which is false during the both-entries-present window.

### D6. GetSecrets scoping

The agent's `GetSecrets` handler signature carries five arguments per the NM SecretAgent spec: `(connection_settings, connection_path, setting_name, hints, flags)`. The handler returns `{"802-11-wireless-security": {"psk": ...}}` only when **all** of these hold:

- **Lifecycle gate:** `agent.state == Registered`. The agent's exported D-Bus object is created in Phase 1 (D7) before Phase 2's AgentManager.Register. A bus peer that calls our exported method during Phase 1 (or while we are in Lost) must not receive a cached secret. This gate is the first check; on `state != Registered`, return empty + INFO log `served=false reason=not_registered`.
- `setting_name == "802-11-wireless-security"` (reject VPN, 802.1X, etc.).
- `flags & NM_SECRET_AGENT_GET_SECRETS_FLAG_REQUEST_NEW == 0`. NM sets `REQUEST_NEW` (0x2) when the previously-supplied secret was rejected and it wants a *fresh* one. We don't have a fresh one — the user supplied one secret via captive form. Returning the same secret on REQUEST_NEW would either loop or cause NM to surface a misleading retry-timeout. Empty response on REQUEST_NEW lets NM fail cleanly with an honest reason code.
- An SSID lookup against the cache succeeds. SSID extraction from the connection_settings dict reads the `802-11-wireless.ssid` byte array. Lookup policy:
  1. **Exact byte match** between extracted SSID bytes and a cache key's bytes.
  2. On miss, if `len(cache) == 1` AND `agent.connectInFlight.Load() == true`, return that entry's secret regardless of SSID match. The `connectInFlight` predicate is a `sync/atomic.Bool` set inside `Manager.Connect` immediately after Stash and cleared in the Drain-paired defer (see D5 pseudocode). **Implementer note:** do NOT evaluate "Connect in flight" via `connectMu.TryLock()` — TryLock acquires the lock on success and would interfere with whatever holds it. The atomic predicate is the load-bearing mechanism.

  This single-cache-entry fallback handles SSID byte/UTF-8 divergence cases (trailing whitespace, non-NFC normalization, hidden-SSID broadcast-as-empty, embedded non-UTF-8 bytes) at the cost of: any `GetSecrets` arriving while a Connect is in flight gets the cached secret. On a single-user appliance with no other root system-bus peers (see A-Risk-1), this is acceptable.
- The cached passphrase is non-empty (defense against an empty-Stash bug).

When any condition fails, the handler returns an empty `a{sa{sv}}` (no secrets). NM falls through; on this appliance with no other agents, the activation fails — same observable behavior as today, but cleanly without abuse of credentials we don't own.

**Other GetSecrets flags** (`ALLOW_INTERACTION` 0x1, `USER_REQUESTED` 0x4) are ignored by the handler — we don't prompt and we don't differentiate user vs. NM-driven calls.

### D7. Agent lifecycle — two phases, one state machine

The lifecycle has a deliberate **two-phase startup**: bus-setup is independent of and prerequisite to AgentManager registration. Splitting prevents an initial Register failure from leaving recovery dead.

**Phase 1: Bus + observers (always succeeds when D-Bus is reachable).**
- Open private system-bus connection.
- Subscribe to `NameOwnerChanged` for `org.freedesktop.NetworkManager`.
- Subscribe to godbus disconnect channel (`conn.Eavesdrop`-style: a Go channel that closes when the bus connection itself dies, distinct from NM going away).
- Export the SecretAgent object at its well-known path on this private connection.
- Failure here means D-Bus itself is unreachable — degraded mode is genuinely "no D-Bus, nothing we can do." Log WARN, return error, `m.agent = nil`.

**Phase 2: AgentManager.Register (may fail, recoverable).**
- Call `org.freedesktop.NetworkManager.AgentManager.RegisterWithCapabilities("piccolod-wifi", 0)`.
- On success → state `Registered`.
- On failure (NM not yet name-owned, transient policy error, NM mid-restart) → state `Lost`. **The watcher from Phase 1 is already running** — NM's eventual appearance triggers re-register on the same backoff schedule below.

**State machine:**

```
                ┌──────────────────┐
       D-Bus    │  bus + watchers  │
       reachable│  (no agent yet)  │
                └────────┬─────────┘
                         │ Phase 2: Register
                         ▼
                 ┌──────────────────┐
                 │   Registered     │◄────────────────┐
                 └────────┬─────────┘                 │
                          │                           │
       NM-name-disappear  │       ▲ NM-name-appear    │
       OR bus-disconnect  │       │ → re-Register     │
                          ▼       │   (backoff)       │
                 ┌──────────────────┐                 │
                 │       Lost       │─────────────────┘
                 └──────────────────┘
                          ▲
                          │ initial Register failed
                          │ (Phase 2 of startup)
```

**Lost-state recovery** uses exponential backoff: 1s, 2s, 4s, 8s, capped at 30s; resets on success.

**Three transition triggers, three sites:**

- **NM name disappears** (`NameOwnerChanged` with new owner empty) → enter Lost.
- **NM name appears** (`NameOwnerChanged` with new owner non-empty) → schedule immediate re-Register; on failure, fall back to backoff.
- **Bus connection itself dies** (godbus disconnect channel closes — happens on dbus-daemon restart, broken pipe, or godbus internal failure; **distinct from NM-name churn**) → tear down state, attempt to re-open the private bus from scratch (Phase 1 → Phase 2). If Phase 1 fails (D-Bus genuinely down), retry Phase 1 on the same backoff schedule.

  **Teardown ordering on bus death:** state transitions to Lost *before* closing the dead bus connection. State-gate in D6 means any in-flight `GetSecrets` dispatch on the dead connection returns empty (state != Registered). Closing the connection signals godbus to drain its dispatcher goroutines; the implementation waits for the connection's signal channel to close before re-opening on a fresh `dbus.SystemBusPrivate()`. The fresh connection's exported object is a new instance — handlers running on the dead connection cannot reach it.

**Shutdown:** `Manager.Stop()` calls `agent.Unregister(ctx)` best-effort. Errors logged at INFO.

**Cache semantics across Lost transitions:** the cache is *not* cleared when entering Lost. A Connect in flight may still be holding a Stash; if NM comes back and re-issues `GetSecrets` for the same SSID, the answer is correct. A Connect that completes during Lost runs Drain normally. NM-restart-during-Connect is benign in outcome (any in-flight activation is lost regardless because NM lost its activation state) — see "NM-restart during in-flight Connect" in the acknowledged-risks section.

### D8. Concurrency

- Cache mutated under `agent.mu` (`sync.Mutex`). `Stash`, `Drain`, `lookup` all acquire it.
- `GetSecrets` runs on a godbus dispatch goroutine. It acquires `agent.mu` for the lookup, copies the passphrase out, releases the mutex, returns.
- Lifecycle methods (`Register`, `Unregister`, NameOwnerChanged handler) serialize through `agent.lifecycleMu` distinct from the cache mutex — registration churn must not block in-flight GetSecrets.
- `Manager.Connect`'s `connectMu` continues to serialize Connect calls; the cache map is therefore practically a single-slot today. The map shape is for future-proofing only.

### D9. Defense-in-depth, not replacement

`nmclient/connection.go` continues to pass `psk` inline in the AddAndActivate dict. The agent only fires when NM falls through to `need-auth`. This means:

- LAN-portal path (already working): inline PSK is honored, agent is never queried, behavior unchanged.
- Captive-portal path (currently failing): inline PSK is set as before, NM falls through to `need-auth` on the AP→STA transition, agent answers, NM retries activation, succeeds.
- Future regressions where a different code path lands in `need-auth`: agent answers, no extra fixup needed.

### D10. Logging policy

- `INFO: nmagent: registered with NM` on successful Register.
- `WARN: nmagent: registration failed: %v` — degraded mode message.
- `INFO: nmagent: NM owner lost, will re-register on reappearance` / `INFO: nmagent: NM owner restored, re-registered`.
- `INFO: nmagent: GetSecrets ssid_len=N setting=%q served=true|false` — never log psk content. `served=false` log includes a one-word reason (`unknown_ssid`, `wrong_setting`, `empty_cache`).
- No DEBUG logs in the hot path; one INFO per GetSecrets is acceptable on this appliance (single user, low-rate).

Existing `Connect entry` and `Connect failed` diagnostics from commits `3949007`/`74e2911` stay as-is.

---

## Site list (Q1)

Sites that read, write, or compose with the new behavior:

### New files

- `internal/network/nmagent/agent.go` — `Agent` struct, `New`, `Register`, `Unregister`, NameOwnerChanged watcher, `Stash`, `Drain`, GetSecrets/CancelGetSecrets/SaveSecrets/DeleteSecrets D-Bus method exports. The four method exports are required by the `org.freedesktop.NetworkManager.SecretAgent` interface; all but GetSecrets are no-ops returning nil error (we don't persist secrets — NM does after we deliver them).
- `internal/network/nmagent/agent_test.go` — table-driven tests for cache scoping (matching SSID, mismatched SSID, wrong setting_name, empty cache, multiple stashes); concurrency test (parallel GetSecrets on stash/drain); lifecycle test (Register/Unregister/NameOwnerChanged-driven re-register) using a stub bus.

### Modified files

- `internal/network/manager.go`
  - `Manager` struct gains an `agent *nmagent.Agent` field.
  - Constructor: call `nmagent.New(ctx)`; on Phase-1 failure log WARN + set `m.agent = nil`. Otherwise call `agent.Start(ctx)` (returns immediately; Phase 2 + lifecycle goroutine).
  - `Connect`: at the top, after `connectMu.Lock` and wifiDevice nil-check, set `connectInFlight=true` (defer-clear), Stash primary SSID+PSK (defer-Drain). Wrap **both** rollback sites — the early one at line 338 (synchronous `nm.Connect` error) and the late one at line 386 (post-WaitForActivation failure) — with conditional Stash/Drain on `rollbackSnapshot != nil && rollbackSnapshot.PSK() != ""`. When PSK is empty, skip the Stash AND skip `RestoreConnection` itself per D5; log INFO. All four agent calls are no-ops when `m.agent == nil` (degraded mode).
  - `Stop` (or equivalent shutdown hook): call `m.agent.Stop(shutdownCtx)` best-effort.

- `internal/network/nmclient/types.go`
  - Add `(*ConnectionSnapshot).PSK() string` accessor mirroring the existing `SSID()` method (line 309). Pulls `Settings["802-11-wireless-security"]["psk"]` via `variantString`. Replaces the inline extraction in `connection.go::RestoreConnection` lines 162–167 (call-site refactor — both `RestoreConnection` and `Manager.Connect` rollback path use the accessor).

- `internal/network/nmclient/connection.go`
  - `RestoreConnection`: lines 162–167 inline PSK extraction replaced with `snapshot.PSK()` call. Behavior unchanged.

- `internal/network/nmclient/client.go` — interface extension is **not required**. nmagent issues `AgentManager.RegisterWithCapabilities` / `Unregister` directly on its private connection. Keeps the consumer-side `Client` interface focused.

- `cmd/piccolod/...` (or wherever `Manager` is constructed at startup) — no source change if `Manager`'s constructor now accepts the bus-connection param and main.go already passes a bus. If main.go doesn't currently know about a private bus, `nmagent.New` opens its own — the constructor signature decision is deferred to implementation; the plan-level constraint is "the agent owns its own bus connection."

### Read-only sites (no change required, audited for invariant preservation)

- `internal/network/captive/server.go` — captive flow still calls `connectFn(ssid, passphrase)`; the agent is invisible from this layer. No change.
- `internal/server/gin_wifi_handlers.go::handleWifiConnect` — LAN flow still calls `Manager.Connect(ssid, passphrase)`; the agent is invisible from this layer. No change. Q3: the LAN path's existing successful behavior must remain unchanged. Verified: agent never fires unless NM enters `need-auth`, which the LAN path does not.
- `internal/network/nmclient/connection.go` — `Connect` and `RestoreConnection` continue to set inline `psk` in the AddAndActivate dict. No change. Q3: the dict shape is preserved; agent is additive.
- `internal/network/nmclient/hotspot.go::ActivateHotspot` (line 56) — uses `AddAndActivateConnection` for AP mode. The connection has no `802-11-wireless-security` section (open AP); NM cannot enter `need-auth` for it. Audited and explicitly excluded from agent involvement.
- `internal/network/supervisor.go` and decide_*.go — supervisor never calls Connect directly. No change.

---

## Q2 — behaviors named at each site

| Site | Required behavior |
|---|---|
| `nmagent.Agent.New` | **Phase 1 of lifecycle.** Open private system bus. Subscribe to `NameOwnerChanged` for `org.freedesktop.NetworkManager`. Subscribe to godbus disconnect channel. Export the SecretAgent object at the well-known path on this private connection. Initialize `cacheMu`, `lifecycleMu`, empty cache, `connectInFlight` atomic.Bool=false, state = `Lost` (not Registered yet). Returns error only if the bus itself is unreachable. Does **not** call AgentManager.Register. |
| `nmagent.Agent.Start(ctx)` | **Phase 2 of lifecycle.** Launch the lifecycle goroutine. Goroutine: attempt `AgentManager.RegisterWithCapabilities("piccolod-wifi", 0)`. On success, state → Registered. On failure, stay in Lost. Loop on signals: NM-name-disappear → Lost; NM-name-appear → re-Register; bus-disconnect → tear down + re-open private bus + re-do Phase 1 + retry Register. Backoff on Register-after-failure: 1s, 2s, 4s, 8s, capped at 30s; reset on success. Idempotent: calling Start twice is a no-op. |
| `nmagent.Agent.Stop(ctx)` | Best-effort shutdown. Cancel lifecycle goroutine. Call `AgentManager.Unregister` (best-effort, log INFO on failure). Close private bus. Returns nil. |
| `nmagent.Agent.Stash(ssid, passphrase)` | Reject empty SSID or empty passphrase (no-op + WARN log). Otherwise insert under `cacheMu`. Replaces any prior entry for the same SSID. |
| `nmagent.Agent.Drain(ssid)` | Remove entry under `cacheMu`. No-op if not present. |
| `nmagent.Agent.SetConnectInFlight(bool)` | Atomic store on `connectInFlight`. Read by `GetSecrets` to gate the single-cache-entry-fallback in D6. Caller responsibility: pair true with eventual false (Manager.Connect uses defer). |
| `nmagent.Agent.GetSecrets` (D-Bus method) | Per D6 scoping in order: (1) state-gate (return empty if `state != Registered`), (2) setting_name match, (3) REQUEST_NEW reject, (4) cache lookup (exact byte match OR single-cache-entry-fallback gated on `connectInFlight==true`), (5) non-empty PSK. Single INFO log per call: `served=true|false reason=<ok|not_registered|wrong_setting|request_new|unknown_ssid|empty_cache>`. |
| `nmagent.Agent.CancelGetSecrets` (D-Bus method) | No-op + INFO log. We don't run async secret-fetch — Stash is synchronous and complete before AddAndActivate. |
| `nmagent.Agent.SaveSecrets` (D-Bus method) | No-op + INFO log. NM persists secrets to its own storage; piccolod does not maintain shadow storage. |
| `nmagent.Agent.DeleteSecrets` (D-Bus method) | No-op + INFO log. Same rationale as SaveSecrets. |
| `Manager` constructor | Call `nmagent.New(ctx)`. On error, log WARN + set `m.agent = nil`. Otherwise call `agent.Start(ctx)` (returns immediately; lifecycle is a goroutine). Continue startup regardless of NM availability. |
| `Manager.Connect` | After `connectMu.Lock` and wifiDevice nil-check: call `agent.SetConnectInFlight(true)` and defer `agent.SetConnectInFlight(false)`; call `agent.Stash(ssid, passphrase)` and defer `agent.Drain(ssid)`. **Before each of the two `RestoreConnection` call sites** (early at manager.go:338, late at manager.go:386): if `rollbackSnapshot != nil && rollbackSnapshot.PSK() != ""`, call `agent.Stash(snap.SSID(), snap.PSK())` and defer `agent.Drain(snap.SSID())`. **Always proceed with `RestoreConnection`** when `rollbackSnapshot != nil` — the empty-PSK case is handled by today's `RestoreConnection` line 199 logic (no-security profile for open-AP rollback). If `m.agent == nil`, all agent calls are no-ops — Connect proceeds with inline-only secrets (degraded mode = current behavior); rollback behavior is unchanged. |
| `Manager.Stop` (or shutdown hook) | Call `m.agent.Stop(shutdownCtx)` best-effort. |

---

## Q3 — invariant audit at each site

| Invariant | How preserved |
|---|---|
| LAN-portal Connect path remains successful with no behavior change | Agent only fires on `need-auth`; LAN path never enters `need-auth`. Inline-PSK dict unchanged. |
| `Manager.connectMu` continues to serialize Connect calls | Stash/Drain are inside `Connect`'s mutex region; cache entries' lifetime is bounded by the lock. Map may transiently hold up to 2 entries (primary + rollback per D5). |
| Rollback path (`m.nm.RestoreConnection(rollbackSnapshot)`) on Connect failure still works | Per D5: **both** rollback sites in `Manager.Connect` (early-error at line 338, late-failure at line 386) are bracketed with conditional Stash/Drain when `snapshot.PSK() != ""`. PSK is read via the new `(*ConnectionSnapshot).PSK()` accessor (mirrors existing `SSID()`). `RestoreConnection` runs unconditionally when `rollbackSnapshot != nil`. **Empty-PSK rollback:** today's `RestoreConnection` (line 199) gates the security section on `psk != ""` and produces an open-profile AddAndActivate when PSK is empty — correctly restores open-AP rollbacks; for secured-but-unrecoverable cases NM rejects on WPA-mismatch (same end-state as today). Agent is dead code for both empty-PSK sub-cases (no need-auth on open profiles), but RestoreConnection's existing behavior is preserved bit-identically. |
| `dbus.SystemBus()` singleton not polluted with agent signal handlers | Agent uses `dbus.SystemBusPrivate()` per D2. |
| No secret content leaks to logs | All log lines include lengths only. Reviewed line-by-line in D10. |
| NM autoconnect for previously-saved profiles still works after the agent is registered | Agent returns empty for unknown SSIDs (D6); NM's autoconnect uses its own persisted secrets path. The agent is a fallback for inline-secret failures, not a primary store. The acknowledged-noise note for the autoconnect-driven `unknown_ssid` log line is in the Acknowledged Risks section. |
| `secret-flags=0` (system-stored) on the inline PSK is unchanged | We do not modify the inline-PSK flags; behavior on systems that already work is bit-identical. |
| piccolod startup still completes when NM is absent or unreachable | Agent.New separates bus setup from AgentManager.Register; Phase 1 (bus + watchers) almost always succeeds, Phase 2 (Register) failure transitions to Lost with active recovery via NameOwnerChanged. If even Phase 1 fails (D-Bus unreachable), `m.agent = nil` and Connect runs in degraded mode = current behavior. |
| Agent's exported `GetSecrets` is callable only by trusted bus peers | We rely on the appliance's system-bus policy (`/etc/dbus-1/system.d/...`) restricting non-root callers from invoking arbitrary methods on services. piccolod runs as root; on this single-user appliance no other root processes register agents or query ours. Audited as `acknowledged × in-scope` in the next section. |

---

## Sibling-shape audit (composition blindness check)

Per `vocabulary.md` sibling-shapes list:

| Sibling shape | Triggered by this change? | How handled |
|---|---|---|
| New error types | Yes (registration failure, GetSecrets-served=false) | Logged at one site each; not propagated as Go errors to callers; do not affect Connect's return value. |
| New permissions | Yes — piccolod gains the right to register as an NM agent | Verified: NM's `AgentManager.Register` requires being on the system bus and (by default policy) UID 0. piccolod runs as root system service. No new privilege escalation. |
| New lifecycle states | Yes — agent's two-phase startup + Lost/Registered state machine in D7 | Phase 1 (bus + watchers) is unconditional; Phase 2 (Register) failure transitions to Lost with active recovery. Three transition triggers enumerated in D7 (NM-name churn, NM-restart, bus-disconnect). Lost behaves identically to "agent not answering": cache may still be populated, Connect still works in degraded mode (relying on inline PSK only). |
| Default value changes | None | No existing default is changed. Inline PSK still set, supervisor decisions unchanged, Connect signature unchanged. |
| New fields in serialized types | None | No persistent storage. Cache is in-memory. |
| Tightened invariants | None | Existing invariants (e.g., "Connect serializes via connectMu") preserved. |
| Shared-utility refactors | None | nmclient unchanged in surface; nmagent is new. |
| Sync→async conversions | One — GetSecrets is an async D-Bus method handler | Concurrency analysis in D8. |

---

## Acknowledged risks (in-scope, documented, not blocking)

These are surfaced explicitly so the implementer and operator know the trade-offs that came out of plan review:

- **A-Risk-1: GetSecrets has no agent-side caller authentication.** Any system-bus peer that can invoke `org.freedesktop.NetworkManager.SecretAgent.GetSecrets` on the path piccolod exports — while `state == Registered` AND a Connect is in flight — would receive the cached PSK. The state-gate (D6) limits the window to `Registered`; the cache window further limits it to "Connect in flight." Within that window, **piccolod ships no system-bus policy file gating callers by UID**. The threat model rests on the appliance's process topology, not on D-Bus policy:

  1. piccolod itself runs as root and is the only process registering an NM secret agent.
  2. The systemd unit topology under `system/` ships no other non-root local processes with system-bus connectivity that would have any reason to call `org.freedesktop.NetworkManager.SecretAgent.GetSecrets` on piccolod's exported path.
  3. The agent's exported method does NOT do caller-UID inspection at the handler level. The state-gate plus cache-window plus the empty-cache-empty-response rule are the only mitigations.

  **What this means concretely:** if a future change adds any non-root service to the appliance that gains system-bus access, the assumption breaks. The mitigation is then either (a) add a `<deny send_destination="..."/>` rule in a `/etc/dbus-1/system.d/piccolod-nmagent.conf` shipped with piccolod, or (b) add a `c.Sender()` UID check at the top of the handler (godbus exposes the sender via the Message context). Neither is in scope for this RFC; both are well-bounded follow-ups. Acknowledged for the single-user appliance ship.

- **A-Risk-2: NM autoconnect is a third caller of `need-auth`.** When NM autoconnects on a saved profile (router reboot, transient disconnect) and that activation lands in `need-auth`, NM queries our agent. The cache will not contain that SSID (no Connect is in flight), so the handler returns empty with `served=false reason=unknown_ssid`. NM then uses its own persisted secrets storage. Functional outcome is correct; the operator-facing log line may look surprising. Mitigation: the GetSecrets log line distinguishes `reason=unknown_ssid` (no Connect in flight or SSID mismatch) from `reason=empty_cache` (unusual — would imply a Stash bug).

- **A-Risk-3: Log rate during failure paths.** NM may call `GetSecrets` more than once per activation attempt — each retry within the activation pipeline triggers a fresh call. On the failure path (genuinely wrong password), this can produce 5–10 log lines per failed connect attempt. Acceptable on a single-user appliance with low Connect rate. If observed to be excessive in operation, add per-Connect-attempt log dedup later.

- **A-Risk-4: NM-restart during in-flight Connect.** Scenario: piccolod has Stashed and called `AddAndActivateConnection`; NM crashes/restarts while `Manager.Connect` is in `WaitForActivation`. The lifecycle goroutine observes NameOwnerChanged → enters Lost. New NM appears; agent re-registers. In the meantime, `WaitForActivation` returns an error (the activation path is invalid post-restart). Connect proceeds to its failure branch, attempts rollback (which now Stashes the rollback's SSID per D5), and Drains everything on return. End-state: same as a normal Connect failure; user retries from captive portal. Reviewer-conflict note: rfc-reviewer rated this `significant` (race window deserves explicit handling), rfc-red-team rated `acknowledged` (outcome is benign). Captured here at acknowledged with both views recorded.

- **A-Risk-5: SSID byte/UTF-8 divergence corner cases.** Per D6's lookup policy, exact-byte SSID match handles ASCII cases; the single-cache-entry-fallback handles divergence cases (trailing whitespace, hidden SSIDs broadcast as empty, non-NFC normalization, embedded non-UTF-8). The fallback's threat-model trade is named in D6.

- **A-Risk-6: We are working around an upstream NM/brcmfmac condition.** The agent fix is the documented escape hatch but does not address the underlying reason NM mishandles the inline PSK across the AP→STA transition. If a kernel/wpa_supplicant/NM upstream fix later eliminates the need-auth fallthrough, the agent is harmless (it answers only when NM asks) but redundant. Captured as `deferred_` follow-up: revisit when NM upstream fixes the inline-PSK behavior or when we have the kernel/supplicant trace to file an upstream bug.

---

## Out of scope but adjacent — captured for deferral

The following are flagged for `deferred_` memory entries, not absorbed into this change:

- "Wrong password" copy in `manager.go::SetConnectError` is misleading for non-auth failures (already noted in earlier diagnostics commits, not yet captured in MEMORY).
- Rate-shaped second-AP-reappearance UX in `decide_ap.go` ("AP cooling down — please wait" cue in portal page).
- `errConnectTimeout` sentinel naming — sub-second failures shown as "timed out" is misleading post-diagnostics.
- Transient `APEnter failed: ap-actuator: no wifi device for AP entry` immediately after teardown — supervisor handle staleness, recovers next tick, harmless but noisy.

---

## Acceptance criteria

A successful implementation:

1. On the RPi 400, captive-portal connect to D111 succeeds end-to-end. Repro logs show: `nmagent: GetSecrets ssid_len=4 setting="802-11-wireless-security" served=true` followed by `Connect failed` **absent** and `Connect entry → activated` reached. The same scenario the diagnostics chain (commits 3949007 + 74e2911) currently shows as failing must succeed.
2. The LAN-portal path (`/api/v1/wifi/connect` over ethernet) continues to succeed with no observable behavior change. Connect log line shows `Connect entry psk_len=N` and the agent is **not** queried (no GetSecrets log line for that flow).
3. Unit tests in `nmagent/agent_test.go` exercise the scoping matrix (right SSID / wrong SSID / wrong setting_name / empty cache); the populate-before-AddAndActivate / drain-after-return ordering is asserted in `manager_test.go` (or equivalent).
4. piccolod restart loop: stop/start during a normal connected session does not break the WiFi connection; agent re-registers, NM continues using its own persisted secrets.
5. NM restart simulation (where feasible — best-effort manual test): agent observes NameOwnerChanged, re-registers, subsequent Connect attempts work.
6. No content (secrets) appears in any log line.

---

## Risk register

| Risk | Mitigation |
|---|---|
| godbus method-export pattern is novel for piccolod (first D-Bus service producer) | Mitigated by a stub-bus unit test and a manual integration test on the dev VM before prod. |
| Agent-priority races with another agent on the same bus | This appliance has no other agents. Verified by grep on system; if a future change adds one, the unit-test scoping suite catches misrouting. |
| NM API differences between versions (1.54.x on RPi vs older builds) | `AgentManager.RegisterWithCapabilities` has been stable since NM ~0.9 with the `(s u)` signature. Use it directly; do not fall back to the older `Register(s)` call. |
| Implementation introduces a new failure mode that currently-working LAN flow inherits | Defense-in-depth design: agent is fallback only, never primary. LAN flow does not invoke `need-auth`. Q3 audit explicit. |
| Cached secret in process memory could leak via coredump | Coredumps are disabled on the appliance via systemd unit hardening (`LimitCORE=0`); if a future operator overrides, the secret is exposed for the Connect-in-flight window. Acceptable trade for a single-user appliance. |
| Agent path is a workaround for an unidentified upstream condition | A-Risk-6: harmless if upstream fixes appear. Deferred follow-up to revisit once kernel/supplicant trace is available. |
