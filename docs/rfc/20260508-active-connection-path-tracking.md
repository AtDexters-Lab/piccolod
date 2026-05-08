# WiFi Activation Wait — Path-Scoped Subscription

## Scope

**Problem:** `nmclient.WaitForActivation` watches device-scoped state events. Device state on `wlan0` is shared across every connection's lifecycle — so when `Manager.Connect` runs while the OLD AP connection is still in `Deactivating`, the OLD connection's trailing `(Deactivating→Disconnected, UserRequested)` event is misread as the NEW connection regressing. Connect returns failure ~1.3s into the wait, captive rollback deletes the just-queued NEW profile, NM observes `connection-removed` exactly when the secret agent (RFC 20260507) would have answered `need-auth`. Net effect on RPi400 brcmfmac: every captive-portal WiFi attempt fails despite the agent path being correct. Diagnosed in `~/Downloads/piccolod-diagnostic (4).log`.

**In scope:**

- Replace device-scoped wait with path-scoped wait at every `WaitForActivation` call site. Subscribe to `org.freedesktop.NetworkManager.Connection.Active` PropertiesChanged on the active-connection path; track its `State` property (`NMActiveConnectionState`).
- Extend `nmclient.ActivateHotspot` to return the active-connection path it creates (currently the value is discarded internally — `hotspot.go:55-57`). AP-Manager's `Start` captures it and threads it through `WaitForActivation`. Without this, the captive→AP-reentry sequence (Connect failure → supervisor APEnter → AP `Start`) hits `WaitForActivation` while `wlan0` is still `Deactivating(110)` from the just-failed STA, and the OLD STA's trailing event re-trips the same false-positive shape on the AP branch — bricking the device into "no STA, no AP" until the next supervisor tick. AP-mode is not a separate problem; it is the second site of the same problem, and the device-scoped fallback is unsafe in the captive operational hot path.
- Preserve `WaitForActivation`'s existing public signature `(ctx, device, expectedActiveConn) → (state, reason, err)`. Both callers (Connect and AP `Start`) now always pass a non-empty path; the device-scoped fallback branch is deleted (no remaining caller). Empty `expectedActiveConn` becomes a programmer error returning a sentinel — defensive, not a supported flow.
- Map active-connection state and reason back to `NMDeviceState` / `NMDeviceStateReason` for caller compatibility (Connect's failure log, AP rollback message, all existing reason-string consumers stay byte-identical for the failure modes already on the hot path; new failure modes — `DependencyFailed`, `DeviceRealizeFailed`, `DeviceRemoved` — gain explicit translation rows so brcmfmac firmware crashes don't degrade to `reason=other`).
- Fix the latent edge that the prior patchwork bool-gate left exposed: STA→STA switching from `Activated(OLD)` without an explicit teardown also fails the same way under device-scoped wait; path-scoped wait eliminates it by construction.
- Handle PropertiesChanged's full payload: `(interface_name, changed_properties, invalidated_properties)`. When `State` arrives via `invalidated[]` (the documented escape hatch NM uses during property-undergoing-object-teardown), re-read the State property via `Properties.Get` rather than dropping the signal. The Get's three failure modes (path-removed, transient, returns Unknown) each have a named decision.
- Stub support: `StubClient` adds `SubscribeActiveConnectionState` + `WaitForActivationByPath{State,Reason,Err}` fields. `StubClient.WaitForActivation` always uses the path fields (the device-scoped legacy fields are deleted alongside the production device-scoped branch).
- Unit tests covering captive AP→STA success, captive failure (connection-removed), STA→STA-from-Activated success, AP-Start under captive-reentry (the F1 site), path-disappearance via `invalidated`, path-disappearance via `Get` returning UnknownObject, subscribe-then-State-already-terminal TOCTOU, channel-close, ctx-cancel, 30s timeout.
- One Stage-16 hardware assertion: full captive cycle produces `nmagent: GetSecrets … served=true` then `Connect succeeded`. Plus a captive-failure-then-AP-reentry assertion: a deliberate wrong-PSK Connect failure followed by `apMgr.Start` reaching `Activated`, not rollback.

**Out of scope:**

- Replacing `SubscribeDeviceStateChanges` everywhere. `probe.go:101` legitimately uses device-scoped state for last-reason-per-device tracking; that is correct and stays.
- Removing the secret-agent path. The agent (RFC 20260507) and this fix are independently load-bearing — agent backstops the brcmfmac inline-PSK fallthrough, this fix lets the agent actually be reached. Both ship, both stay.
- Refactoring `Manager.Connect`'s rollback orchestration. The rollback path is the symptom-amplifier, not the cause; the cause is the false-positive failure signal that triggers it.
- Adding `NMActiveConnectionState` as a new public type on the `Client` interface. We translate at the boundary; callers continue to receive `NMDeviceState`/`NMDeviceStateReason`.
- Retyping `ActiveConnectionInfo.State uint32` (types.go:362) to `NMActiveConnectionState`. No readers compare against state values today; deferred as benign sibling-shape drift.
- Periodic State property polling inside the wait loop. Defensive-only; current NM emission semantics make it unnecessary; deferred via A-Risk-1.
- Re-running RFC 20260507. The agent fix is correct; the deferred-followups memory's `WaitForActivation latent edge` entry collapses into this RFC.

---

## Background — what the diagnostic chain proved

Two captive cycles in `(4).log` show the identical failure shape. From the second cycle (17:32:11–12), aligned to the source line:

```
17:32:05.0  AP-teardown begins (user submitted credentials over /api/connect)
17:32:11.0  ap: AP mode deactivated                         ← piccolod-side AP layer's view
17:32:11.0  network: Connect entry … wlan0_state=deactivating  ← but NM still in Deactivating(110)
17:32:11.x  nm.AddAndActivateConnection succeeds, returns active-conn path
17:32:11.x  NM: device (wlan0): disconnecting for new activation request
17:32:12.4  NM: device (wlan0): state change: deactivating(110) → disconnected(30) reason='user-requested'  ← OLD AP teardown done
17:32:12.4  WaitForActivation returns (Disconnected, UserRequested)  ← false positive
17:32:12.4  Manager.Connect: rollback path runs, deletes the queued new profile
17:32:12.5  NM: device (wlan0): Activation: starting connection 'D111'  ← NEW conn was about to start
17:32:13.0  NM: device (wlan0): state change: prepare → config
17:32:13.0  NM: device (wlan0): "access point 'D111' has security, but secrets are required"
17:32:13.0  NM: device (wlan0): state change: config → need-auth
17:32:13.0  NM: device (wlan0): state change: need-auth → deactivating reason='connection-removed'
                                                                          ↑ piccolod's rollback finally observed
```

The problem mechanism is a layering error: `WaitForActivation`'s contract is "wait for THIS connection to activate," but its implementation is "wait for ANY device-state event," with a `seenProgressing` heuristic to disambiguate. The heuristic is fundamentally lossy — device state is a per-device singleton property whose value is the union of all currently-active and recently-deactivated connections' lifecycles on that device. There is no value of the heuristic that disambiguates correctly under back-to-back activations.

Active-connection state, by contrast, is path-scoped: every distinct connection NM activates gets its own `/org/freedesktop/NetworkManager/ActiveConnection/N` D-Bus object whose `State` property reflects only its own lifecycle. Subscribing to that object's PropertiesChanged eliminates the contamination at the source.

---

## Alternatives considered

### Alt-A. Bool-gate on `seenProgressing` (the patchwork)

Tighten `seenProgressing` initialization to exclude `Deactivating(110)`. Same gate applied to in-loop state changes. **Rejected as the proper fix; reverted from `30b3eca` after discussion.** Empirically resolves the captive AP→STA case but does not address the latent STA→STA-from-Activated edge (advisor-flagged): `Activated(OLD,100) → Deactivating → Disconnected` for an OLD connection still trips the same false-positive shape, because Activated `<= 100` so `seenProgressing` is bootstrapped true. Shipping the bool-gate now means the same root cause re-emerges as soon as a future setting-UI surface lets users switch SSIDs without an explicit AP teardown. Patchwork over patchwork is the bloat path the team's plan-scope-creep memory warns against.

### Alt-B. Pre-flight: wait for `wlan0_state ≠ Deactivating` before AddAndActivate

Add a pre-flight inside `Manager.Connect` that polls `DeviceState` until the device leaves `Deactivating`, then proceeds with AddAndActivate. **Rejected:** moves the heuristic earlier without removing it. The pre-flight is itself a device-scoped check; it cannot disambiguate "Deactivating because OLD AP teardown finishing" from "Deactivating because NM is between activations of two of MY connections." Worse, it adds a synchronous wait before every Connect, slowing the success path (the captive flow needs to be fast — user is staring at a phone). Same architectural mismatch as Alt-A, different surface.

### Alt-C. Filter device events by polling `ActiveConnection` between events

For each device-state event, poll `device.ActiveConnection` and accept the event only when it matches `expectedActiveConn`. **Rejected:** introduces a new TOCTOU race — between event arrival and `ActiveConnection` poll, NM may have transitioned. The active-connection property update is asynchronous with the device-state update. Worse, NM clears `ActiveConnection` to empty between activations (during the disconnect gap), and the wait would have to interpret "empty" as "still ours, keep waiting" vs. "ours got torn down" — a discrimination the device path simply cannot make. Adds complexity, doesn't solve correctness.

### Alt-D. Subscribe to PropertiesChanged on the path returned by AddAndActivate

The path `expectedActiveConn` is the D-Bus object representing OUR specific activation attempt. Its `State` property is the value of THAT activation's lifecycle, exclusive of any other connection. Subscribe to `org.freedesktop.DBus.Properties.PropertiesChanged` filtered by sender path = `expectedActiveConn`. **Chosen.** Eliminates the entire class of multi-tenant device-state contamination by construction. Also eliminates the Alt-A latent STA→STA edge by the same construction.

### Alt-E. Track via `Manager.ActiveConnections` ObjectManager-style

Use NM's `ActiveConnections` property and watch `ActiveConnections` add/remove. **Rejected:** ObjectManager-style watching is for "what connections exist?", not "what state is THIS connection in?" — wrong granularity. PropertiesChanged on the specific path is the right primitive.

---

## Decisions

### D1. Path-scoped subscription via PropertiesChanged on the active-connection D-Bus object

`WaitForActivation` subscribes to `org.freedesktop.DBus.Properties.PropertiesChanged` filtered by `path = expectedActiveConn`. The signal payload is `(string interface_name, dict<string,variant> changed, list<string> invalidated)`. Filter to `interface_name == "org.freedesktop.NetworkManager.Connection.Active"`.

Three signal-handling cases:

- **Case A — `changed["State"]` present.** Read the new state value (uint32 `NMActiveConnectionState`); apply terminal-state logic (D3).
- **Case B — `changed["State"]` absent, `State` not in `invalidated[]`.** A non-State property changed (`Ip4Config`, `Default`, etc.). The wait loop continues — we don't re-read State, we don't drop the goroutine; we ignore the event entirely.
- **Case C — `State` listed in `invalidated[]`.** NM's documented escape hatch when "the new value cannot be efficiently conveyed" — observed in NM source for properties undergoing object-lifecycle teardown. Issue a synchronous `org.freedesktop.DBus.Properties.Get(interface, "State")` against the path. The Get has three outcomes, each with a named decision (D5).

`NMActiveConnectionState` values (from NM upstream):
- `0` Unknown — only valid as a transient-read-result (D5); never returned terminal from this wait
- `1` Activating — non-terminal; loop continues
- `2` Activated — terminal success
- `3` Deactivating — terminal failure (begins teardown)
- `4` Deactivated — terminal failure (teardown complete; path may be removed shortly after)

### D2. Public signature preserved; device-scoped fallback deleted

Signature stays `(ctx, device, expectedActiveConn) → (NMDeviceState, NMDeviceStateReason, error)`. Both production callers (Connect at `manager.go:441`; AP `Start` at `ap/manager.go:117`) now always pass a non-empty active-connection path — Connect already does, AP `Start` gains it via D11.

The implementation is a single path-scoped flow. There is no longer a device-scoped fallback branch; the prior helper (`runActivationWait` in the reverted patchwork) is replaced by `runActivationWaitByPath`. The internal helper-extraction shape mirrors the earlier one: a pure-loop `runActivationWaitByPath(ctx, eventsCh, initialState) → (NMDeviceState, NMDeviceStateReason, error)` exposed for unit testing.

`expectedActiveConn == ""` becomes a programmer error: `WaitForActivation` returns immediately with a sentinel error `errEmptyActiveConnPath` and `(NMDeviceStateUnknown, NMDeviceStateReasonNone)`. Defensive-only — both production callers always pass a path post-D11. Test seam (`StubClient`) likewise enforces the precondition; `nmclient` does not silently accept empty.

### D3. Result translation at the boundary — `NMActiveConnectionState{Reason}` → `NMDeviceState{Reason}`

Active-connection state and reason values are NOT identical to device state and reason. `WaitForActivation`'s callers (Connect, AP, log lines, sentinel comparisons in `manager.go:444`) all expect `NMDeviceState`/`NMDeviceStateReason`. Translate inside `waitForActivationByPath` at return:

State translation:
- `NMActiveConnectionStateActivated(2)` → `NMDeviceStateActivated(100)`
- `NMActiveConnectionStateDeactivated(4)` → `NMDeviceStateDisconnected(30)`
- `NMActiveConnectionStateDeactivating(3)` → `NMDeviceStateDisconnected(30)` (mapped to a terminal device state for caller's `state == Activated` check; the *reason* carries the failure detail)
- `NMActiveConnectionStateActivating(1)` → never returned terminal; either advances to Activated/Deactivating/Deactivated or times out
- `NMActiveConnectionStateUnknown(0)` → `NMDeviceStateUnknown(0)`

Reason translation: `NMActiveConnectionStateReason` and `NMDeviceStateReason` overlap in semantics but use different numeric values. `translateActiveConnReason()` in `dbus_client.go` maps every defined ACR value to a DSR value; rows on the captive hot path:
- `ACR.None(1)` → `DSR.None(0)`
- `ACR.UserDisconnected(2)` → `DSR.UserRequested(39)`
- `ACR.DeviceDisconnected(3)` → `DSR.UserRequested(39)` *(distinction lost — both render as `reason=user_requested`; A-Risk-2)*
- `ACR.ServiceStopped(4)` → `DSR.SupplicantDisconnect(8)`
- `ACR.IPConfigInvalid(5)` → `DSR.ConfigFailed(4)`
- `ACR.ConnectTimeout(6)` → `DSR.SupplicantTimeout(11)`
- `ACR.ServiceStartTimeout(7)` → `DSR.SupplicantTimeout(11)`
- `ACR.ServiceStartFailed(8)` → `DSR.SupplicantFailed(10)`
- `ACR.NoSecrets(9)` → `DSR.NoSecrets(7)`
- `ACR.LoginFailed(10)` → `DSR.SupplicantFailed(10)`
- `ACR.ConnectionRemoved(11)` → `DSR.ConnectionRemoved(38)`
- `ACR.DependencyFailed(12)` → `DSR.ConfigFailed(4)` *(brcmfmac dependency cascades)*
- `ACR.DeviceRealizeFailed(13)` → `DSR.FirmwareMissing(35)` *(brcmfmac firmware crash)*
- `ACR.DeviceRemoved(14)` → `DSR.Removed(36)` *(USB hotplug / interface vanish)*
- Anything else → `DSR.Unknown(1)`

The brcmfmac-relevant rows (12, 13, 14) are explicitly enumerated because they are the observed firmware-failure modes on the headline hardware; collapsing them to `Unknown` would degrade operator triage on exactly the device this RFC ships for (red-team F3).

Defense-in-depth for any *future* unmapped value: when `translateActiveConnReason` returns `Unknown(1)` due to translation-table fallthrough, `WaitForActivation` itself logs the original ACR value at WARN level via a fingerprint line including `ac_reason=N` (uint32). Operators get triage information regardless of whether the table has caught up.

Synthetic terminal cases that are NOT a real ACR value (D4-iv `path_removed` synthesis on `Properties.Get` returning UnknownObject) emit the same fingerprint with a different token: `synth=path_removed`. The two tokens are type-stable independently — `ac_reason=` is always uint32, `synth=` is always a string discriminator. Operators writing log-grep tooling for one token won't accidentally match the other.

**Emission site is `WaitForActivation`, not the caller.** Connect and AP-Start both call `WaitForActivation`; pushing the discriminator into the wait function means both callers' failure paths see the fingerprint without each having to thread a fourth return value or duplicate fallthrough logic. Caller log lines remain shaped around `(state, reason, err)` only; the discriminator line is emitted by the wait function on its way out.

Log shape — example fingerprint lines emitted by `WaitForActivation`:

```
# Translation-table fallthrough (real ACR value, unmapped):
WARN: nmclient: WaitForActivation translation-fallthrough path=… ac_reason=15

# Synthetic D4-iv terminal (path disappeared, no real reason from NM):
WARN: nmclient: WaitForActivation path-removed-on-toctou path=… synth=path_removed
```

Both lines fire only when their respective condition fires; otherwise absent. Caller-side log lines (Connect's `Connect failed …`, AP-Start's `wait for hotspot activation …`) remain unchanged in shape — the fingerprint precedes them in the log stream so operators see (synth-line, then caller-line) as a paired emission.

### D4. TOCTOU guard — subscribe, then read `State`, with named decisions for every Get outcome

Between AddAndActivate's return (which yields the path) and `WaitForActivation`'s subscribe call, NM may have already transitioned the path's State (most likely Activating → Activated for a fast-path success, or Activating → Deactivating for an immediate fail). Subscribe-then-check pattern:
1. Subscribe to PropertiesChanged on `expectedActiveConn`.
2. Read `State` property via `org.freedesktop.DBus.Properties.Get`.
3. Branch on the Get outcome — every case has a named decision (red-team F5):
   - **(i) Get returns `Activated(2)` / `Deactivating(3)` / `Deactivated(4)`:** terminal — return immediately with the appropriate translation per D3.
   - **(ii) Get returns `Activating(1)`:** non-terminal — proceed to the signal-receive loop.
   - **(iii) Get returns `Unknown(0)`:** ambiguous — proceed to the signal-receive loop. NM may have not yet populated the property; subsequent PropertiesChanged signals will resolve.
   - **(iv) Get errors with `org.freedesktop.DBus.Error.UnknownObject` / `UnknownInterface` / `UnknownProperty`:** path is gone, but the *cause* is ambiguous. The path could have been removed by an explicit `DeleteConnection` (Connect's rollback path or a user `ForgetNetwork`), by NM losing the device (USB unplug, brcmfmac firmware crash), by NM mid-restart, or by a D-Bus broker hiccup. Synthesizing `ConnectionRemoved` (which operationally implies "user/we deleted the profile") would misattribute the brcmfmac firmware-crash mode — the very mode D3's explicit `DeviceRealizeFailed`/`DeviceRemoved` rows exist to surface. Instead: return terminal `(NMDeviceStateDisconnected, NMDeviceStateReasonUnknown(1))` and emit the `synth=path_removed` token (D3 separate-token mechanism — distinct from `ac_reason=` which is reserved for real-ACR-value fallthrough). Operators see a fingerprint mapped to "the path vanished without a state-change reason," distinct from explicit ConnectionRemoved (which carries `reason=connection_removed`).
   - **(v) Get errors with any other D-Bus error (transient broker hiccup, NM mid-restart):** log at WARN, proceed to signal-receive loop. The 30s ctx-timeout is the floor; if the path resurfaces, signals will arrive.

Subscribe-before-read order is required: read-then-subscribe loses any transition that occurs between the two calls.

Subscribe-then-read may double-observe (a State change emits a signal AND is reflected in the synchronous Get reply): idempotent on terminal states because the second observation re-evaluates the same value. Activating-then-Activated within the gap: the Get returns Activated, we return success — correct.

### D5. Path disappearance — three observed mechanisms, each with terminal decision

NM tears down an active connection through one of three mechanism shapes. Each is named here so the wait loop reaches a terminal decision rather than stalling.

- **Mechanism 1 — Normal teardown (most common).**
  1. `changed["State"]: Activating → Deactivating` (PropertiesChanged fires)
  2. `changed["State"]: Deactivating → Deactivated` (PropertiesChanged fires)
  3. NM removes the D-Bus object (no further signals; subsequent property reads on the path return UnknownObject)
  
  Loop returns terminal at step 1 or 2 per D3; subsequent path removal is post-return, harmless.

- **Mechanism 2 — Invalidated-then-Get (NM property-teardown escape hatch).**
  1. NM emits PropertiesChanged with `changed=∅, invalidated=["State"]` per the documented spec for properties undergoing object-lifecycle teardown.
  2. Wait loop case-C (D1) issues `Properties.Get`. Get outcomes, each handled per D4 sub-cases (i)–(v):
     - Get returns terminal value (Activated/Deactivating/Deactivated) → terminal return
     - Get returns Unknown(0) or Activating(1) → continue waiting (next signal resolves)
     - Get returns UnknownObject error → synthesize `(Disconnected, Unknown)` with `synth=path_removed` token (D4 sub-case (iv); cause ambiguous, fingerprint distinct from explicit ConnectionRemoved)
     - Get returns other error → continue, ctx-timeout floors

  Four Get outcomes mapping to three distinct terminal-or-continue actions.

- **Mechanism 3 — Defensive: path removed without prior State signal.**
  Not observed in our captured NM versions, but the `Properties.Get → UnknownObject` synthesis (D4-iv) catches it deterministically if hit during a TOCTOU read. If hit during the steady-state loop *with no triggering signal*, the loop sees no events and ctx-timeout (30s) terminates with `wait_err=context deadline exceeded`. UX cost on this defensive path is the full 30s wait visible to the captive-portal user — acceptable while the case is unobserved (A-Risk-3); revisit only if hardware logs show this fingerprint.

### D6. Subscribe scope — exact match on path; filter chain explicit

**`AddMatch` rule:** `type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',path='<expectedActiveConn>'`. Path-exact match (no `path_namespace`) — this is the only subscription that exists for this Connect call, no fan-out to other active connections.

**In-process filter chain** (red-team F4, addressing godbus's connection-wide signal channel fan-out):
1. `sig.Path == expectedActiveConn` — match-rule already filters this at the broker, but reassert in-process as defense-in-depth (godbus delivers all signals on the shared connection; our path-scoped goroutine must drop anything that's not ours, or other subscribers' signals would feed our wait loop).
2. `sig.Name == "org.freedesktop.DBus.Properties.PropertiesChanged"` — match-rule already filters this; reassert.
3. `body[0] == "org.freedesktop.NetworkManager.Connection.Active"` — interface filter; the path could in principle host other interfaces, this guards against handling foreign-interface PropertiesChanged.

All three checks are cheap (string/path comparison); the goroutine drops non-matching signals silently.

**Channel buffer size:** the new subscription channel is buffered to **64** entries. Rationale: under captive AP-teardown-while-Connect-runs signal storms, the existing device-state subscription's smaller buffer (`signals.go` default of 16) has been observed to drop on burst — and per the existing `signals.go:46-48, 101-105, 159-163` pattern, drops are non-blocking with a "channel full, dropping" log. For path-scoped wait, a dropped terminal event is correctness-affecting (loop misses the State transition that signals success or failure, falls through to 30s ctx-timeout). 64 is generous for a single-path subscription's expected emission rate (a full Activating→Activated path emits ~5 signals); the cost is negligible memory.

**Drop policy:** drop-and-log per existing pattern, but flag as risk: if drops are observed in field for the path-scoped subscription, the buffer should be raised. A-Risk-6 captures this explicitly so the trade-off is durable.

The subscription is scoped to a child context cancelled on return — same lifecycle pattern as the existing device-state subscription, so cleanup is mechanical.

### D7. 30s timeout budget unchanged

Same `context.WithTimeout(ctx, 30*time.Second)` budget at the caller (Manager.Connect). The waitForActivationByPath path uses the inherited ctx; no new timeout layer.

### D8. Stub-client extension for testing the path-scoped flow

`StubClient` adds:
- `WaitForActivationByPathState NMActiveConnectionState`
- `WaitForActivationByPathReason NMActiveConnectionStateReason`
- `WaitForActivationByPathErr error`

Plus a `SubscribeActiveConnectionState(ctx, path) → <-chan ActiveConnectionStateChange` method on the `Client` interface, mirroring `SubscribeDeviceStateChanges`. The new method is the seam that makes the path-scoped wait unit-testable without a real D-Bus.

`ActiveConnectionStateChange` (new type in `nmclient/types.go`):
```
type ActiveConnectionStateChange struct {
    Path     dbus.ObjectPath
    NewState NMActiveConnectionState
    Reason   NMActiveConnectionStateReason
}
```

Tests drive deterministic event sequences through the channel; the loop logic is extracted into `runActivationWaitByPath(ctx, ch, initialState)` for unit testing without any D-Bus.

**Stub dispatch (reviewer F-1):** `StubClient.WaitForActivation` always returns the `WaitForActivationByPath{State,Reason,Err}` fields. The legacy device-scoped fields (`WaitForActivationState`, `WaitForActivationReason`, `WaitForActivationErr`) are **deleted from the stub** alongside the production device-scoped branch (D2). Test authors who try to use the legacy fields get a compile error — surface bug, not silent default. The empty-`expectedActiveConn` branch in stub returns `errEmptyActiveConnPath` symmetric to production (D2).

**Stub-vs-production divergence (red-team F4) — explicitly bounded:**

The stub channel is intentionally simpler than godbus's signal channel. Three known divergences and the corresponding mitigations:

- **(a) Signal fan-out filtering.** Stub channel delivers exactly what tests send; production must filter the connection-wide channel via D6's three-step filter chain. Risk: an off-by-one in the production filter is invisible to stub-driven tests. Mitigation: a single integration test (in addition to the unit tests) that exercises the actual `signals.go`-style filter on a synthetic multi-path godbus signal stream. Lives in `nmclient/signals_test.go` (new file; small).

- **(b) Channel-full drops.** Stub never drops; production drops at buffer overflow per D6's drop-and-log policy. A-Risk-6 names this; mitigation is the 64-entry buffer chosen in D6 plus telemetry-via-log if the drop ever fires.

- **(c) Async property-vs-signal ordering at TOCTOU read (D4).** In real D-Bus, the synchronous `Properties.Get` reply and async PropertiesChanged signals can arrive in any order. The wait loop's loop-iteration logic is idempotent on terminal values (D4 closing paragraph) — re-observing a terminal State on the next signal returns the same answer. Stub fires events in test-author order; integration test (a) covers a real interleaving.

Stage 16 hardware test asserts the captive-success path AND the captive-failure-then-AP-reentry path (in-scope per scope block), forcing the AP path-scoped wait into the assertion surface and catching gross filter/buffer/ordering regressions on the headline hardware.

### D9. Connect's failure-log shape unchanged; success branch silent post-D12

`Manager.Connect`'s log line stays:
```
WARN: network: Connect failed ssid=%q final_state=%s reason=%s wait_elapsed=%s wait_err=%v active_id=%q
```
Field semantics preserved by D3's translation. Operators tailing logs see the same vocabulary; existing alerts/dashboards (none today on this codebase, but trivially future-proofed) don't break.

D12 makes the success branch silent — the line is emitted only on the failure branch. `active_id=` is populated by `ActiveConnectionInfo(dev.Path)` at `manager.go:468` (the rollback decision: "did NM auto-reconnect to OLD?"), which legitimately needs device-scoped semantics; D12 retains that read on the failure branch only.

D3's WaitForActivation-emitted fingerprint lines (`translation-fallthrough` and `path-removed-on-toctou`) precede Connect's WARN line in the log stream when their condition fires — operators see paired emissions. Same paired shape applies to AP-Start's failure path.

### D10. Sibling-shape audit — every consumer of "device state" that's actually waiting on "connection state"

The composition-blindness check (per `vocabulary.md`):

| Site | Today | After RFC | Action |
|---|---|---|---|
| `nmclient/dbus_client.go:WaitForActivation` | device-scoped subscribe | path-scoped, single branch (device-scoped fallback removed) | rewritten |
| `nmclient/hotspot.go:ActivateHotspot` | discards activePath | returns `(activePath, error)` | signature change |
| `network/manager.go:Connect` (line 441) | passes `newActivePath` to wait | unchanged | no change |
| `network/manager.go:Connect` (line 447-457 post-check) | reads `ActiveConnectionInfo(dev.Path).ID` for SSID-match TOCTOU | post-check removed entirely (D12: path identity is proof) | deleted |
| `network/ap/manager.go:Start` (line 117) | passes `""` | captures activePath from D11; passes to WaitForActivation | caller updated |
| `network/probe.go:RunSignalLoop` (line 101) | `SubscribeDeviceStateChanges` | unchanged; legitimate device-scoped use (last-reason-per-device) | no change |
| `nmclient/types.go:ActiveConnectionInfo.State uint32` | raw uint32 | unchanged; deferred sibling-shape | deferred |

The first five rows are the change-set for this RFC. `ActiveConnectionInfo.State` (sixth row) keeps its raw `uint32` shape; verified no readers compare against state values today (only set at `dbus_client.go:266`), so the type drift between this field and the new `NMActiveConnectionState` enum is benign and deferred. Retyping is a separate cleanup.

Sibling shape — `WaitForDeactivation`? Grepped — no such function exists. AP-mode rollback uses `DeactivateHotspot` which is a synchronous D-Bus call; it does not wait for confirmation via state events. Confirmed not affected.

### D11. `nmclient.ActivateHotspot` returns both paths from `AddAndActivateConnection`

Signature change: `ActivateHotspot(device, ssid, opts) error` → `ActivateHotspot(device, ssid, opts) (activePath, settingsPath dbus.ObjectPath, err error)`. NM's `AddAndActivateConnection` already returns both paths — the existing implementation captures them at `hotspot.go:55-57` then discards both. Both are now returned.

**Why both, not just `activePath`:** AP `Start`'s rollback path needs to delete the just-created connection profile if activation fails. The current rollback at `ap/manager.go:103-105` invokes `DeactivateHotspot(device)`, which does a **device-scoped** lookup of the active connection on `wlan0` to find the profile to delete (`hotspot.go:66-101`). Under the I6-break path (D4-iv synthesizes terminal failure because TOCTOU `Properties.Get` returns `UnknownObject`), the device's `ActiveConnection` may have already been cleared by NM during the deactivation gap — `DeactivateHotspot` then finds nothing, returns nil silently, and **the transient AP profile NM created (id `piccolo-ap-XXXX`) is leaked**. Each I6-break leaks one profile; the supervisor's retry loop accumulates them.

The `settingsPath` return gives the caller a direct handle on the profile to delete on rollback, independent of device-scoped active-conn lookup state.

`ap/manager.go:Start` captures both paths and appends a path-scoped cleanup *before* the device-scoped one:
- Cleanup 1 (path-scoped): `_ = m.nm.DeleteConnection(settingsPath)` — guarantees the just-created profile is reachable for cleanup regardless of whether NM has cleared `device.ActiveConnection`. Idempotent (deleting an already-deleted connection is no-op-with-error).
- Cleanup 2 (device-scoped, existing): `_ = m.nm.DeactivateHotspot(device)` — kept for the case where the AP path *did* fully activate before the rollback decision (e.g., wait succeeded but a later AP-Start step failed); ensures the device-active-connection is cleanly torn down.

`activePath` is passed to `WaitForActivation(waitCtx, device, activePath)` instead of the empty string.

`StubClient.ActivateHotspot` returns fixed test paths for both. The signature change ripples to: `client.go:53` (interface), `stub.go:185` (stub), `ap/manager.go:Start` (caller). No other callers of `ActivateHotspot` exist (grepped).

**Reply contract** (red-team RT-F-23, code-review S-2): on `err == nil` both paths are populated. On `err != nil` both paths are empty — this is structural to godbus and D-Bus, not a choice. godbus's `(*Call).Store()` returns the transport error before populating retvalue locals; D-Bus METHOD_RETURN and ERROR are mutually exclusive message types with no partial-reply transport. Earlier RFC iterations described a "best-effort partial-reply" contract; that contract is unimplementable through `AddAndActivateConnection`'s D-Bus shape and has been retracted.

The path-scoped `DeleteConnection(settingsPath)` cleanup in AP `Start` therefore protects the *post-Activate* failure window only — the case where AddAndActivate returned a populated path pair but a later step in `Start` (firewall, IP query) failed. The brcmfmac firmware-mid-crash window where NM allocates settingsPath then synchronously fails activation cannot be cleaned up by the caller because `settingsPath` is unrecoverable on the err path; if this case ever fires, the supervisor's retry loop is the residual mitigation. R-13 mitigation reflects this honest framing.

This eliminates the device-scoped fallback rationale that previously justified `expectedActiveConn==""` in the AP-mode caller. With both production callers always providing a path, the device-scoped branch in `WaitForActivation` has no remaining caller and is deleted (D2) — a real simplification, not just a rearrangement.

### D12. Connect's post-success Id check is removed; path identity is proof

`Manager.Connect` (`manager.go:447-457`) currently confirms the right SSID activated by reading `ActiveConnectionInfo(dev.Path).ID` and comparing against the requested `ssid`. Pre-RFC, this was load-bearing as a TOCTOU guard: device-scoped wait returns Activated for *whatever* connection the device is currently activated on, including the OLD SSID before NM transitioned away — so the post-check was the only signal that "the activation we waited for was actually ours."

Post-RFC, the wait is **path-scoped on the exact `expectedActiveConn` returned by `AddAndActivateConnection`**. Path identity is its own proof: NM allocates a unique `/org/freedesktop/NetworkManager/ActiveConnection/N` for every activation; the path is created by NM and returned to us in the AddAndActivate reply; no other call can reuse it. If wait fires terminal on State=Activated for that specific path, the connection that activated *is* the one we queued. There is no SSID-vs-path discrimination problem to solve.

**Decision:** delete the post-check entirely. Once `WaitForActivation` returns `(NMDeviceStateActivated, ..., nil)`, Connect proceeds directly to single-SSID-policy delete-old-profiles. No `ActiveConnectionInfo` call, no `Id` compare, no `errConnectWrongSSID` sentinel raised on success.

This eliminates:
- The Id-vs-State timing race (red-team RT-F-13: NM may not have populated `Id` on the path by the time wait returns terminal on `State`; reading `Id` could yield empty string or `UnknownProperty` error, causing false rollback on a successful activation).
- The need for a new `ActiveConnectionID(path)` helper (no consumer remains).
- The composition complexity of teaching the existing `ActiveConnectionInfo` device-scoped reader new path-scoped semantics.

**What's preserved:**
- Failure-path log line at `manager.go:463` keeps the `active_id=` token, but it's always empty on the success branch (which is now silent — no log line). On the failure branch, the device-scoped `ActiveConnectionInfo(dev.Path)` read at `manager.go:468` (the rollback decision: "did NM auto-reconnect to the OLD SSID?") legitimately needs device-scoped semantics — keep as-is.
- Subsequent IP4Address / Gateway queries via `ActiveConnectionInfo(dev.Path)` later in Connect's success path: keep as-is. They want the *current* active connection's IP config; device-scoped is correct.

**Risk acknowledged (A-Risk-5):** removing the post-check is a defense-in-depth reduction. If a future bug in `WaitForActivation` causes it to return Activated for the wrong path (e.g., a filter-chain off-by-one in D6), Connect would silently accept it. The cost of this reduction is bounded: the wait is the single source of truth for "did our activation succeed," and the integration test in `signals_test.go` (D8) exercises the filter chain. If higher defense-in-depth is desired later, `Uuid` (set by NM at AddAndActivate time, durable per the NM connection-settings contract — distinct from `Id` which is populated lazily) would be a better post-check substrate than `Id` ever was. Deferred until empirically motivated.

---

## Q1. Site list — every site that observes the new behavior

- **Producer:** `internal/network/nmclient/dbus_client.go:WaitForActivation` — single-branch path-scoped flow.
  - `runActivationWaitByPath(ctx, ch, initialState) → (NMDeviceState, NMDeviceStateReason, error)` — pure-loop helper for testing.
  - `translateActiveConnReason(NMActiveConnectionStateReason) → NMDeviceStateReason` — boundary mapping; total over defined ACR values.
  - `errEmptyActiveConnPath` — sentinel for D2 programmer-error case.
- **Producer:** `internal/network/nmclient/hotspot.go:ActivateHotspot` — signature change to return active-conn path (D11).
- **Producer:** `internal/network/nmclient/signals.go:SubscribeActiveConnectionState` — new method on `*DBusClient`. Implements D6 filter chain + 64-entry buffer + drop-and-log policy.
- **Producer:** `internal/network/nmclient/types.go` — new types (`NMActiveConnectionState` enum + String, `NMActiveConnectionStateReason` enum + String, `ActiveConnectionStateChange` struct).
- **Producer:** `internal/network/nmclient/client.go` — interface gains `SubscribeActiveConnectionState`; `ActivateHotspot` signature updated.
- **Producer:** `internal/network/nmclient/stub.go` — stub gains `SubscribeActiveConnectionState` + `WaitForActivationByPath{State,Reason,Err}` fields. Legacy `WaitForActivation{State,Reason,Err}` fields **deleted**. `ActivateHotspot` returns a fixed stub path.
- **Consumer:** `internal/network/manager.go:Connect` — post-success `ActiveConnectionInfo(dev.Path)` read, `info.ID != ssid` compare, and `errConnectWrongSSID` raise at `manager.go:447-457` are deleted per D12. Failure-branch log line shape unchanged; the `synth=path_removed` and `ac_reason=N` fingerprint lines come from `WaitForActivation` itself per D3, not from Connect. `errConnectWrongSSID` sentinel: delete if grep confirms no remaining consumers post-D12.
- **Consumer:** `internal/network/ap/manager.go:Start` — captures activePath from `ActivateHotspot`; passes to `WaitForActivation`.
- **Test:** `internal/network/nmclient/wait_activation_test.go` (new file) — captive AP→STA success, captive failure (connection-removed via path), STA→STA-from-Activated success, **AP-Start under captive-reentry** (the red-team F1 site — wlan0 in Deactivating from failed STA, AP path-scoped wait reaches Activated), path-disappearance via `invalidated[]` Get-returns-terminal, path-disappearance via `invalidated[]` Get-returns-UnknownObject, subscribe-then-State-already-terminal TOCTOU, channel close, ctx cancel, 30s timeout.
- **Test:** `internal/network/nmclient/signals_test.go` (new file) — single integration-style test against a synthetic godbus signal stream exercising the D6 filter chain with foreign-path and foreign-interface signals interleaved (red-team F4 mitigation).
- **Test:** existing `nmclient/types_test.go` — extend with `translateActiveConnReason` table tests covering every defined ACR value plus the Unknown-fallthrough.
- **Test:** new `internal/network/manager_test.go` row (or extend existing) — assert Connect's WARN log shape: when reason is Unknown, the line includes `ac_reason=N`; when reason is mapped, the token is absent.
- **Test:** Stage 16 (`scripts/alpha/dev-vm-alpha-test.sh`) — extend 16.x to assert (a) full captive cycle produces `nmagent: GetSecrets … served=true` then `Connect succeeded`, AND (b) deliberate-wrong-PSK Connect failure followed by AP-Start reaching Activated (not rollback). Test (b) requires a known wrong-password input and a way to detect AP re-entry success — both available via existing AP detection logic. **Coverage probe (red-team F-10):** Test (b) reads `wlan0_state` via `nmcli -t -f STATE device status wlan0` (or equivalent) at the moment APEnter is invoked. If state ∈ {Deactivating, Disconnected-with-trailing-events}, the F1-site contamination is reproducible on this hardware; assert path-scoped wait reaches Activated. If state == Disconnected-clean, log `WARN: F1 site not reproducible on this hardware/firmware — coverage degraded` and SKIP the assertion (do not silently pass). Catch-when-coverage-degrades, not when behavior breaks. Driver/firmware updates that close the timing window surface as visible warnings, not invisible test rot.
- **Doc:** `docs/rfc/20260507-wifi-secret-agent.md` — add a one-line cross-reference at the top noting that path-scoped wait makes the agent reachable.

No app, frontend, or systemd unit changes — the surface is contained to `nmclient`, `network/ap`, and one paragraph of behavior change in `manager.go:Connect`'s log-line shape.

---

## Q2. Behaviors enumerated for each lifecycle stage

### Captive AP→STA (the bug)
- Pre-state: device `wlan0` in `Deactivating(110)` (OLD AP teardown), `expectedActiveConn = /org/freedesktop/NetworkManager/ActiveConnection/N` (NEW STA, just queued).
- Path-scoped wait subscribes, reads `State=Activating(1)`.
- AP teardown completes — device emits `(Deactivating→Disconnected, UserRequested)`. **Path-scoped wait does not observe this** (different D-Bus object). ← the fix.
- NM begins activating NEW conn. PropertiesChanged: `State: Activating → Activated(2)`.
- Wait returns `(NMDeviceStateActivated, NMDeviceStateReasonNone, nil)` (after D3 translation).
- Connect proceeds to single-SSID-policy delete-old-profiles, returns success.

### Captive AP→STA, NEW conn fails at need-auth (regression-test path)
- Same pre-state as above.
- NEW conn proceeds Activating → (NM hits need-auth, secret agent answers per RFC 20260507) → Activated. (Same as success case.)
- OR NEW conn fails: PropertiesChanged: `State: Activating → Deactivating(3)` with `Reason: NoSecrets(9)`.
- Wait returns `(NMDeviceStateDisconnected, NMDeviceStateReasonNoSecrets, nil)` (D3 translation).
- Manager.Connect logs `Connect failed … reason=no_secrets`, runs rollback path.

### STA→STA switching from Activated(OLD) (the latent-edge fix)
- Pre-state: device `wlan0` in `Activated(100)` (OLD STA conn), user submits new SSID via settings UI (hypothetical future feature).
- AddAndActivate returns NEW path (NM starts Deactivating OLD in parallel).
- Path-scoped wait subscribes on NEW path, reads `State=Activating(1)`.
- OLD conn's lifecycle (Deactivating → Deactivated) emits on its OWN path's PropertiesChanged — **path-scoped wait does not observe**. ← latent edge gone.
- NEW conn lifecycle proceeds normally to Activated.
- Wait returns success.

(This case is hypothetical today — UX never exposes it — but path-scoped construction handles it for free.)

### AP-mode activation (path-scoped, post-D11)
- Caller: `ap/manager.go:Start` calls `m.nm.ActivateHotspot(...)` which now returns `(activePath, error)` per D11.
- Caller passes the captured `activePath` to `WaitForActivation(ctx, dev, activePath)`.
- Path-scoped wait subscribes to PropertiesChanged on the AP active-conn path. The AP path's own State lifecycle (Activating → Activated for success; Activating → Deactivating → Deactivated for failure) is observed exclusive of any concurrent OLD-conn teardown on `wlan0`.
- Cold-boot AP-Start (no antecedent activity on `wlan0`): subscribe lands, Get returns Activating(1), loop waits, Activated(2) emitted, return success.

### AP-mode activation under captive-reentry (the red-team F1 site)
- Pre-state: device `wlan0` in `Deactivating(110)` (just-failed STA conn being torn down by NM after Manager.Connect's `DeleteConnection`), `expectedActiveConn = /org/freedesktop/NetworkManager/ActiveConnection/M` (NEW AP, just queued by ActivateHotspot).
- Path-scoped wait subscribes on AP path, Get returns Activating(1).
- Failed-STA's path emits its own teardown lifecycle on a DIFFERENT path — wait does not observe (D6 filter chain drops it).
- `wlan0` device emits `(Deactivating→Disconnected, UserRequested)` — wait does not observe (the subscription is path-scoped, not device-scoped).
- AP path proceeds Activating → Activated(2). Wait returns success. ← The fix.
- AP `Start` proceeds to firewall verification + IP query. Hotspot active. Captive portal accessible. User retries with corrected credentials.

### Path already terminal at subscribe time (TOCTOU)
- Subscribe completes; State property read returns `Activated(2)`.
- Wait returns immediately without entering signal loop.
- (Symmetric for `Deactivating` and `Deactivated` — return failure immediately.)

### Path removed during wait (defensive)
- Subscribe completes; signal loop entered.
- NM removes the D-Bus object without prior `Deactivated` signal (not observed in our logs; defensive only).
- Signal channel goes idle (no further events for the now-gone path).
- Ctx 30s timeout fires → wait returns `(NMDeviceStateUnknown, NMDeviceStateReasonNone, ctx.Err())`.
- Caller treats as failure (existing semantics for ctx-timeout return).

---

## Q3. Invariants

- **I1.** Every NM `AddAndActivateConnection` call returns a unique active-connection path. Path identity is the disambiguator on which the entire RFC depends.
- **I2.** A path's `State` property is monotonically progressing through `Activating → Activated` for success and `Activating → Deactivating → Deactivated` for failure; no other transitions are observed in normal NM operation. Wait loop relies on this for terminal-state detection.
- **I3.** PropertiesChanged is emitted by NM on every State transition during the path's lifetime. NM's behavior here is documented and stable across the versions we ship against.
- **I4.** `Manager.connectMu` ensures at most one in-flight `WaitForActivation` per Connect surface; the path-scoped subscription is exclusive within Connect's window. (Stage-16 hardware-AP can run in parallel with Connect on different devices, but that's the AP path — device-scoped, no contention with the new path-scoped surface.)
- **I5.** Reason translation (`translateActiveConnReason`) is total — every `NMActiveConnectionStateReason` value maps to some `NMDeviceStateReason`, with `Unknown(1)` as the catchall. Callers never see an untranslated active-conn reason on the public API.
- **I6.** `AddAndActivateConnection` returns a path with queryable State even while the device is mid-Deactivating from a prior connection on the same device. Empirically witnessed in `(4).log` for the STA-during-AP-Deactivating direction (the captive-failure trace); assumed symmetric for AP-during-STA-Deactivating per NM's documented per-device serialization. Failure mode if assumption breaks: TOCTOU `Properties.Get` (D4) returns `UnknownObject` immediately at AP-Start, D4 sub-case (iv) synthesizes `(Disconnected, Unknown)` with `synth=path_removed` and returns terminal *immediately* (no 30s wait). AP `Start` invokes rollback; the path-scoped `DeleteConnection(settingsPath)` cleanup (D11) ensures no profile leak. Supervisor's next tick re-attempts. Not a brick state. Stage 16 test (b) is the empirical safety net for this invariant on the headline hardware.

---

## Sibling-shape audit (composition-blindness check)

Per `vocabulary.md`'s sibling-shapes list, every new behavior shape requires auditing existing sites that observe the analogous old shape. Going through:

- **New error types:** None added. `error` interface only; existing sentinel `errConnectTimeout` remains.
- **New permissions:** None.
- **New lifecycle states:** `NMActiveConnectionState{Reason}` — internal to `nmclient`, never escapes the package on the public API (D3 translation).
- **Default value changes:** None. Behavior change is conditional on `expectedActiveConn != ""`.
- **New fields in serialized types:** None.
- **Tightened invariants:** I2 — relies on NM emitting all State transitions. Documented as risk (R-3).
- **Shared-utility refactors:** `WaitForActivation` is the shared utility being changed. Both call sites audited (D10 table); both still satisfy the public contract.
- **Sync→async conversions:** None. Both branches stay synchronous within the wait window.

The new types (`NMActiveConnectionState`, `ActiveConnectionStateChange`) are NEW shapes, not sibling shapes — no pre-existing readers to audit.

---

## Acknowledged risks

### A-Risk-1. NM behavior assumption — PropertiesChanged on every State transition
We rely on NM emitting PropertiesChanged for every Active-Connection State transition (either via `changed["State"]` or via `invalidated[]` per D1 case-C). NM upstream documentation specifies this. If a future NM version skips a transition entirely (no `changed`, no `invalidated`), the wait could miss a terminal state.

Mitigation: ctx-timeout (30s) is the floor — we don't hang forever. Detection: Stage 16 hardware test catches it; field telemetry via `wait_err=context deadline exceeded` with no `final_state` is a unique fingerprint.

Defensive enhancement (deferred): a periodic `Properties.Get` poll inside the wait loop (every ~5s) would catch coalesced/skipped transitions in 5s rather than 30s. Not implemented today because it adds D-Bus traffic on the success hot path for no current benefit; revisit if A-Risk-1 ever fires in field.

### A-Risk-2. Reason-translation lossiness
`NMActiveConnectionStateReason` and `NMDeviceStateReason` overlap but are not 1:1. Two specific lossiness modes:
- **Two ACR codes collapse to DSR.UserRequested:** `ACR.UserDisconnected(2)` and `ACR.DeviceDisconnected(3)` both render as `reason=user_requested`. Operator triage outcome is the same (both are non-error teardowns initiated by the user/device); the audit-log distinction is lost.
- **Future ACR values not in the table** fall through to `DSR.Unknown(1)` and render as `reason=other`. Mitigation per D3: the `ac_reason=N` log token surfaces the original ACR value alongside, so operators can manually disambiguate even before the table catches up.

The brcmfmac firmware-failure modes (DependencyFailed, DeviceRealizeFailed, DeviceRemoved) are explicitly enumerated in the D3 table to prevent the operator-triage degradation the red-team flagged.

### A-Risk-3. Path-deletion-without-State-signal edge
D5 mechanism 3: if NM ever removes the path without emitting Deactivated AND without emitting `invalidated[State]`, the wait falls back to ctx-timeout. Not observed in current NM versions.

UX cost on this defensive path: the captive-portal user sees "connecting…" for the full 30s before the failure surfaces. Acceptable while unobserved; revisit if hardware logs ever show this fingerprint.

Detection: `wait_err=context deadline exceeded`, `final_state=unknown`, no `ac_reason=` token — unique fingerprint distinct from all other failure modes.

### A-Risk-4. RFC 20260507 secret agent — interaction
The secret agent fix is independent of this RFC. With device-scoped wait, the agent was being preempted by false-positive failure detection. With path-scoped wait, the agent is reached. Both ship; both stay; neither subsumes the other. (The secret agent backstops NM's brcmfmac inline-PSK fallthrough at the `need-auth` step, regardless of how the wait loop is implemented.)

### A-Risk-5. Defense-in-depth reduction — Connect post-check removed

D12 deletes the `Id != ssid` post-check that previously sat between `WaitForActivation` returning Activated and Connect declaring success. With device-scoped wait, the post-check was load-bearing (wait could return Activated for the OLD connection); with path-scoped wait, path identity from `AddAndActivateConnection` is sufficient proof that what activated is what we queued.

The post-check removal eliminates a documented false-rollback race (red-team RT-F-13: `Id` populated lazily relative to `State=Activated`) at the cost of a defense-in-depth layer. If a future bug in `WaitForActivation` returns Activated for the wrong path (e.g., a filter-chain off-by-one in D6's three-step in-process filter), Connect would silently accept it.

Mitigations:
- Integration test (`signals_test.go`, D8) exercises the filter chain on a synthetic multi-path godbus signal stream — this is the load-bearing detector. CI must run it on every change to nmclient/dbus_client.go or nmclient/signals.go.
- D6's match-rule path-exact filter at the broker is the first line of defense; the in-process filter is belt-and-suspenders.
- If higher defense-in-depth is desired later, `Uuid` (set by NM at AddAndActivate time, durable per the NM connection-settings contract — distinct from `Id` which is populated lazily) would be a better post-check substrate than `Id` ever was.

Detection — honest framing: a filter-chain regression would manifest as Connect succeeding for a connection whose activation NM never completed; the user would lose connectivity shortly after Connect returns success. **This shape is observationally indistinguishable from existing brcmfmac firmware flap, AP roam-out, weak-signal disassociation, or neighbor BSS interference** — all of which produce the same `Connect succeeded` followed by `device state regression within seconds`. There is no field log fingerprint that lets an operator triage "filter-chain bug" vs. "flaky radio." Field detection is therefore not load-bearing; the integration test is.

Operational consequence: if `signals_test.go` is ever weakened or removed, this risk transitions from Acknowledged to a real defense-in-depth gap with no replacement detector. Revisit if that test is touched.

### A-Risk-6. Channel-full drop on path-scoped subscription
godbus's connection-wide signal channel is buffered; on overflow, signals are dropped per `signals.go`'s drop-and-log pattern. With device-scoped subscriptions, drops manifested as observability gaps (probe.go missed a reason update). With path-scoped subscriptions, dropped terminal events are correctness-affecting — the wait loop falls through to 30s timeout instead of returning the right answer.

D6 mitigation: 64-entry channel buffer for the path-scoped subscription (vs. signals.go's typical 16). 64 is generous for a single-path subscription's expected emission rate (~5 events per full activation); cost is negligible memory.

Detection: existing "channel full, dropping" log lines. If observed in field for the path-scoped subscription, raise the buffer or move to a blocking-with-deadline pattern. Deferred until observed.

---

## Risk register

| ID | Risk | Severity | Mitigation |
|---|---|---|---|
| R-1 | NM upstream changes PropertiesChanged emission semantics | Low | ctx-timeout floor + Stage 16 catches it on next NM upgrade; deferred poll mitigation |
| R-2 | Reason translation loses fidelity for rare codes | Acknowledged | brcmfmac modes explicit; `ac_reason=N` log token for fallthrough |
| R-3 | (Retired — AP path-scoped per D11/D2) | — | — |
| R-4 | Stub/test-fake drift from real D-Bus behavior | Mitigated | D8 names three divergences; new `signals_test.go` integration test; Stage 16 captive-failure-then-AP-reentry assertion |
| R-5 | Path-disappearance edge not exercised | Low | D4-iv synthesizes `(Disconnected, Unknown)` with `synth=path_removed` token on UnknownObject; A-Risk-3 names the residual fingerprint |
| R-6 | invalidated[State] case dropped silently | Mitigated | D1 case-C + D5 mechanism-2 explicit; Get re-read with named outcomes |
| R-7 | Path-scoped channel-full drop = lost terminal | Acknowledged | 64-entry buffer (D6); A-Risk-6 names trade-off |
| R-8 | Read-error path picks an implementer default | Mitigated | D4 sub-cases (i)–(v) named; UnknownObject synthesizes terminal `(Disconnected, Unknown, synth=path_removed)` |
| R-9 | Connect post-check has Id-vs-State timing race | Mitigated | D12 deletes the post-check entirely; path identity from AddAndActivate is sufficient proof |
| R-10 | Stage 16 test (b) silently degrades coverage when timing window closes | Mitigated | Reproducibility probe via `wlan0_state` check at APEnter; SKIP/WARN if site not reproducible |
| R-11 | I6 symmetric-AddAndActivate assumption breaks | Bounded | TOCTOU UnknownObject path returns terminal `(Disconnected, Unknown, synth=path_removed)` immediately; AP rollback uses path-scoped `DeleteConnection(settingsPath)` cleanup; supervisor retries |
| R-12 | Defense-in-depth reduction: post-check removal is silent if WaitForActivation has filter-chain bug | Acknowledged | Integration test (signals_test.go) exercises filter chain; A-Risk-5 names trigger; future Uuid-based post-check deferred |
| R-13 | AP profile leak via DeactivateHotspot device-scoped lookup losing the path on I6-break | Partially mitigated | D11 returns settingsPath on `err == nil`; AP Start path-scoped cleanup covers the post-Activate failure window. Brcmfmac firmware-mid-crash window where NM populates settingsPath then synchronously fails the call has unrecoverable settingsPath (godbus contract) → supervisor retry is the residual mitigation. |

---

## Cross-references

- RFC 20260507 (secret agent) — the path-scoped wait is what makes the agent's `GetSecrets` actually reachable on hardware.
- Diagnostic instrumentation: commits `3949007` (network/Connect entry+failed), `74e2911` (captive client_diag).
- Failure logs: `~/Downloads/piccolod-diagnostic (1)..(4).log`.
- Reverted patchwork: commit `30b3eca` (soft-reset prior to this RFC).
