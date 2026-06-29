# Network Transition Reconciliation

## Scope

**Problem:** Piccolo already detects active uplink changes, but the emitted event is a legacy compatibility signal rather than a typed reconciliation boundary. In the June 29, 2026 RPi400 incident, Ethernet and Wi-Fi were both healthy, Ethernet was removed, NetworkManager moved the default route and DNS to Wi-Fi, and the supervisor reported Wi-Fi L3 up within seconds. Namek remote access still remained unavailable until its retry loop recovered later, and Pi-hole/DNS symptoms appeared around the same transition. The root class is not "DNS only"; it is that subsystems with network-facing state do not get a deterministic, owner-safe transition reconciliation pass.

**In scope:**

- Introduce a typed network transition event derived from the supervisor tick/snapshot when active uplink, default route/DNS interface, per-interface WAN/LAN role, interface address set, connectivity, or AP mode changes.
- Preserve the existing `TopicNetworkStateChanged` / `NetworkStateChangedEvent` wire contract for UI, identity wakeups, STUN, and health tracking.
- Add owner-local reconcilers for remote adapters, mDNS interface advertisements, and app/service network publication checks. The event wakes owners; each owner decides whether to repair, restart, reload, or no-op under its own locking and serialization rules.
- Make Namek and self-hosted Nexus adapters respond quickly to a proven uplink transition without depending only on backend retry timers.
- Treat Pi-hole/DNS as part of app listener publication and port-claim routing, not as a special one-off DNS daemon path, unless static implementation work proves a narrower DNS-specific hook is required.
- Add focused unit tests around event emission, deduplication, adapter wake/restart behavior, mDNS transition hints, and app/service publication reapplication.

**Out of scope:**

- Power-supply or undervoltage remediation. Undervoltage is incident evidence and should remain operator-visible, but this RFC does not fix hardware power.
- Namek server, Nexus server, or router-side changes.
- Repairing upstream ISP/router DNS or making Piccolo the client's only DNS recovery mechanism.
- SSH-based live debugging or hardware end-to-end validation; current device access is unavailable.
- Broad app platform refactors, manifest schema changes, or UI redesign.
- Replacing the stateless network supervisor's probe/decide/act model.

---

## Evidence

### Incident timeline

The appended diagnostic log shows the transition as a host-local network event with several downstream symptoms:

- `09:43:00-09:43:07`: `pasta` drops UDP datagrams to port 53 while both interfaces are still present, and Pi-hole's runtime user is being inspected/restored. This proves DNS traffic disruption near the event; it does not by itself prove Pi-hole process failure.
- `09:43:41`: kernel reports `end0: Link is Down`.
- `09:43:47`: NetworkManager sets Wi-Fi connection `D111` on `wlan0` as default for IPv4 routing and DNS. The dispatcher immediately logs `Name or service not known` for two scripts.
- `09:43:55`: Namek's relay write pump fails with `use of closed network connection`.
- `09:44:01`: the network supervisor reports `uplink=wifi l3=up nmconn=full`.
- `09:44:09`: mDNS drops stale `end0` state after sustained IP loss and then announces names on `wlan0`.
- `09:44:25-10:00:05`: Namek repeatedly fails fetching the handshake nonce from `https://namek.piccolo0.atdexters.com/api/v1/nonce`, mostly with client timeouts.
- `10:00:10`: Namek reconnects and the relay reports connected.

Undervoltage messages also appear during the same period. That is serious hardware evidence, but the logs do not show a reboot, watchdog, OOM, filesystem failure, or loss of Wi-Fi L3. The software defect to fix is therefore not "ignore undervoltage"; it is "do not let a valid uplink transition leave remote/app/LAN-network surfaces waiting on unrelated retry paths."

### Current code shape

`TopicNetworkStateChanged` is explicitly a preserved compatibility contract. `NetworkStateChangedEvent` says subscribers pattern-match on `(ActiveUplink, SignalDBm)` (`internal/network/types.go:86`), and `publishLegacyEvent` dedupes a small `(state, uplink, apActive, apSSID)` tuple for old subscribers (`internal/network/supervisor.go:465`). Wi-Fi signal tier changes also publish to this topic (`internal/network/signal.go`), so consumers currently filter ad hoc.

The only server-level network subscriber wakes identity enrollment and STUN (`internal/server/gin_server.go:1517`). It does not reconcile remote adapter lifecycle, mDNS, service endpoints, or app publication.

Namek lifecycle is owned by `applyNamekState`, whose comment states it must only run from the identity event subscriber goroutine because concurrent calls race adapter lifecycle state (`internal/server/gin_server.go:3046`). A network subscriber must not call this method directly. The self-hosted remote manager has a similar config-change path: `applyAdapterState` restarts only when its config key changes, and explicitly no-ops when the same config is already running (`internal/remote/manager.go:1395`).

The Nexus adapter starts backend clients from a static config snapshot (`internal/remote/nexusclient/backend.go:114`). There is no Piccolo-level `WakeNetwork` or `RestartForNetworkTransition` method today.

mDNS already has its own interface watcher and handles stale IP loss by closing sockets and deleting stale interface state after three ticks (`internal/mdns/interface.go:440`). It is separate from the supervisor transition stream and therefore can lag or disagree with the supervisor's active-uplink truth.

App listener DNS exposure is represented as ordinary listener and port-claim state: `AppListener.PortClaim` exists in the public definition (`internal/api/types.go:259`), the service manager caches active port claims (`internal/services/manager.go:392`), the service proxy opens public listeners and firewall rules (`internal/services/manager.go:1721`, `internal/services/proxy.go:423`, `internal/services/proxy_udp.go:416`), the Nexus adapter exports those claims to remote route config, and Podman publishes backend listener ports on `127.0.0.1` (`internal/container/podman.go:579`). App reconciliation verifies Podman publications and recreates containers only if expected bindings drift (`internal/app/app_manager.go:1837`, `internal/app/container_group_reconcile.go:434`).

---

## Design

### D1. Add `TopicNetworkTransition` beside the legacy state topic

Add a new event topic with a typed payload. To avoid an import cycle, the topic constant lives in `internal/events`, while the payload types live in `internal/network`; `internal/events` must not import `internal/network`.

```go
type NetworkTransitionEvent struct {
    Generation    uint64
    Reasons       []NetworkTransitionReason
    Previous      NetworkTransitionState
    Current       NetworkTransitionState
}

type NetworkTransitionState struct {
    ActiveUplink      UplinkType
    ActiveUplinkIface string
    Connectivity      Connectivity
    APActive          bool
    DefaultRouteIface string
    DefaultRouteKnown bool
    DNSDefaultIface   string
    DNSDefaultKnown   bool
    Interfaces        []NetworkInterfaceState
    At                time.Time
}

type NetworkInterfaceRole string // wan_lan, wan, lan, not_connected, unknown, filtered

type NetworkInterfaceState struct {
    Kind       DeviceKind
    Iface      string
    Role       NetworkInterfaceRole
    LinkUp     bool
    HasIP      bool
    IPv4       []netip.Addr
    IPv6       []netip.Addr
}
```

This payload is intentionally richer than the legacy event but still factual. It is not a command object. It tells owners what changed; it does not decide their repair policy.

`Reasons` should be derived mechanically from the old/current transition state, except for explicitly dampened facts such as stable global IPv6 renumbering and synthetic delivery facts such as history overflow. It is a set, not a single enum, because one physical event can change uplink, default route, DNS default, and interface addresses in the same supervisor tick:

- `active_uplink_changed`
- `default_route_changed`
- `dns_default_changed`
- `route_dns_observation_changed`
- `interface_roles_changed`
- `interface_addresses_changed`
- `connectivity_changed`
- `ap_mode_changed`
- `history_overflow`

The supervisor owns emission because it already has the coherent `Tick` and `Snapshot`. If the existing `Tick` lacks address lists or NetworkManager default route/DNS projection, extend observation to capture those facts in the probe layer; do not have consumers re-query the OS independently for transition classification. The typed transition state must enumerate all NetworkManager-managed physical interfaces by stable `(Kind, Iface)`; do not reuse the existing single-device-per-kind probe shape for role classification.

Transition state must be canonical before reason derivation:

- Interfaces are sorted by `(Kind, Iface)`.
- Address slices are sorted by `netip.Addr.Compare`.
- Same-kind multi-NIC cases are first-class: `eth0=lan`, `usb0=wan`, and `wlan0=wan_lan` are three separate interface states, not one coarse Ethernet or Wi-Fi state.
- IPv6 addresses are carried for diagnostics, but this implementation does not use raw IPv6 address churn for restart-producing `interface_addresses_changed` decisions. Stable non-temporary IPv6 renumbering requires address-flag observation and is deferred.
- `DefaultRouteIface` is the interface that NetworkManager currently uses for the default route. `DNSDefaultIface` is the interface whose NetworkManager connection contributes the active default DNS configuration.
- `ActiveUplink` and `ActiveUplinkIface` preserve the legacy single-device-per-kind status contract. When `DefaultRouteKnown` is true, `DefaultRouteIface` is the authoritative egress identity for transition owners and may identify a second same-kind NIC that legacy status does not model.
- Each interface also has an independent role: `wan_lan` can reach WAN and serve LAN clients, `wan` can reach WAN but is not a LAN publication/discovery surface, `lan` can serve LAN clients but cannot reach WAN, `not_connected` has no usable link/address, `unknown` is connected but temporarily not classifiable, and `filtered` is intentionally excluded/non-advertisable. Do not collapse `unknown` or `filtered` into `not_connected`.
- Owner behavior consumes the role it needs: Namek/Nexus care about `DefaultRouteIface` as the selected WAN/egress path when known; mDNS, local app listeners, Pi-hole/DNS publication, and firewall applicability evaluate all LAN-capable interfaces (`wan_lan` or `lan`) independently of `ActiveUplink`.
- If route/DNS projection fails, the corresponding `Known` field is false, no route/DNS change reason is emitted, and remote/app reconcilers do not restart from that partial observation alone. Gaining route/DNS observation after an unknown projection emits `route_dns_observation_changed` as a wake-only reason so owners with pending recovery can re-check the latest state without treating observation gain as a fresh restart reason. A route table that is successfully observed with no default route is different from projection failure: private no-default interfaces can still classify as `lan`, while unobserved route state stays `unknown`.
- A usable remote uplink means `Connectivity` is `ConnectivityFull` or `ConnectivityLimited` and either a non-empty known `DefaultRouteIface` exists, or the legacy `ActiveUplink` is non-`none` while route projection is unavailable.

### D2. Keep `TopicNetworkStateChanged` unchanged

The legacy topic remains the source for UI network state, identity `NotifyNetworkUp`, STUN trigger, health tracker updates, and SSE compatibility.

Do not change the meaning of `SignalDBm == nil` or the small dedupe key as part of this RFC. Consumers that need transition detail subscribe to the new topic.

### D3. Add a latest-state plus owner-delta transition source

The event bus is lossy by design when subscriber buffers fill, so it must not be the sole reconciliation delivery contract. The supervisor must store a bounded transition history plus the latest canonical state in memory and expose a non-blocking wake channel or callback to the server coordinator:

```go
type NetworkTransitionSource interface {
    TransitionDeltaSince(lastIngested uint64) (network.NetworkTransitionDelta, bool)
    SubscribeNetworkTransitionWake(func()) func()
}

type NetworkTransitionDelta struct {
    FromGeneration uint64
    ToGeneration   uint64
    Reasons        []NetworkTransitionReason
    Previous       NetworkTransitionState
    Current        NetworkTransitionState
    Coalesced      bool
}
```

The wake path is latest-state plus accumulated reasons: if several transitions arrive while the coordinator is busy, each owner reconciles the newest state with the union of reasons since that owner's last ingested generation. A restart-relevant reason cannot be erased by a later connectivity-only generation. If the bounded history no longer contains `lastIngested + 1`, `TransitionDeltaSince` returns a conservative delta from the oldest retained generation, sets `Coalesced=true`, and includes `history_overflow` so owners treat the delta as restart-relevant when the current uplink is usable.

Owner handling distinguishes ingestion from satisfaction. If an owner receives restart-relevant reasons while the current uplink or required interface role is not usable, those reasons become owner-pending rather than satisfied. The owner may advance its ingested generation to avoid hot-looping on the same delta, but the pending reasons are unioned into later handling until a usable current state triggers the restart/verification, or until the owner is disabled or the affected adapter/app is no longer expected to be active. A later usable `connectivity_changed`-only or `interface_roles_changed`-only generation must therefore satisfy pending route/uplink/DNS/address/role or `history_overflow` reason instead of no-oping.

`TopicNetworkTransition` is still published for diagnostic consumers, but owner recovery must read owner deltas from this source, not rely on receiving every bus event.

Server wiring registers one coordinator against the transition source and fans out to registered owner reconcilers:

```go
type NetworkTransitionReconciler interface {
    ReconcileNetworkTransition(context.Context, network.NetworkTransitionDelta) // owner-specific shape may pass derived publish/preserve sets
}
```

Fan-out is non-blocking per owner and coalesced by owner generation. A slow app publication check must not block remote wake, and a remote restart must not block mDNS cleanup. Each owner must be idempotent and must persist its ingested generation only after it has accepted the delta for handling; restart-relevant pending reasons have their own satisfaction state and are not cleared by an unusable-uplink no-op.

This coordinator is glue only. It must not hold server-wide locks while invoking reconcilers, and it must not directly mutate owner internals.

### D4. Remote adapters get an explicit network wake/restart contract

Add a lifecycle method around Piccolo's adapter ownership layer rather than reaching into the upstream Nexus client:

```go
type NetworkAwareAdapter interface {
    RestartForNetworkTransition(ctx context.Context, reasons []NetworkTransitionReason) error
}
```

For the concrete `BackendAdapter`, restart means:

1. Snapshot the already-configured adapter config.
2. Stop the current backend clients if running.
3. Start new backend clients with the same config.
4. Emit the same relay lifecycle events as normal disconnect/connect paths.

This is a bounded owner-initiated restart, not config reconciliation. It applies only when the transition delta proves there is a usable current uplink, the adapter is expected to be active, and the accumulated or owner-pending reason set is relevant to WAN/egress. Always-relevant remote reasons are `active_uplink_changed`, `default_route_changed`, `dns_default_changed`, and `history_overflow`. `interface_addresses_changed` or `interface_roles_changed` are remote-relevant only when the changed interface is the previous or current `ActiveUplinkIface`, `DefaultRouteIface`, `DNSDefaultIface`, or an address otherwise used by the selected egress path. LAN-only role/address changes must not restart Namek or self-hosted Nexus. A plain `connectivity_changed` event must not flap remote adapters by itself unless it is satisfying a previously pending WAN/egress-relevant reason.

The reason to restart rather than merely call `Start` is that the current adapter no-ops if already running (`internal/remote/nexusclient/backend.go:114`), while the stale connection path observed in the incident had already failed and entered retry behavior outside Piccolo's control.

A transition-triggered restart also opens a bounded owner-local recovery window. If the first restart attempt fails before the current-run relay aggregate reaches connected, the owner schedules capped retry attempts until one of these happens: the aggregate relay state connects, adapter config/enablement changes, the uplink becomes unusable again, or the transition recovery deadline/cooldown expires. The retry window must respect the existing Namek identity debounce/cooldown so a settling DNS path cannot turn into a nonce-request storm.

### D5. Namek wake must preserve the single-owner invariant

Do not call `applyNamekState` from the network transition goroutine. Instead, add a request channel owned by the same goroutine that currently processes identity events:

```go
type namekApplyReason string

const (
    namekApplyIdentity namekApplyReason = "identity"
    namekApplyNetwork  namekApplyReason = "network-transition"
)
```

Identity events and network transition events both enqueue into that goroutine. The goroutine debounces them, then invokes the same owner-local state machine. On `namekApplyNetwork`, if the Namek config key is unchanged and an adapter is running, it requests `RestartForNetworkTransition` instead of skipping work solely because the config key is unchanged.

The debounce loop must accumulate reasons across the whole window. Network reasons remain sticky until the owner satisfies them on a usable current uplink or explicitly drops them because Namek is disabled/no longer expected active; an identity event in the same window must not downgrade a network restart into the existing unchanged-config no-op path.

This keeps the current serialization guarantee while adding a fast recovery path for active-uplink changes.

### D6. Self-hosted remote manager mirrors the same contract

The remote manager adds `ReconcileNetworkTransition`. When self-hosted remote access is enabled, the transition delta has a usable current uplink, and the accumulated or owner-pending reason set is WAN/egress-relevant by the same rules as D4, it restarts the adapter if the existing config key is unchanged and an adapter is running. If the adapter is not running, it falls back to the normal `applyAdapterState` path using the current snapshot.

This should reuse the existing adapter lock and relay state machinery. It must not clear config or certificate state.

### D7. mDNS consumes transitions as a reconciliation hint

mDNS keeps its current interface watcher. The new transition event is an additional hint:

- If an interface disappears or is link-down, withdraw or close stale records for that interface promptly. If an interface only loses addresses, preserve the existing dampening policy before withdrawal.
- If the active interface, LAN-capable role, or address set changes, announce current hostnames on currently-advertisable interfaces.
- Keep the existing virtual-interface filters and gateway leader behavior from RFC 20260129 / RFC 20260201.

The transition handler should not bypass mDNS ownership. It should call an mDNS-owned method such as `ReconcileInterfacesFromNetworkTransition(event)`.

Advertisable mDNS interfaces are not the same as the supervisor's active uplink. Active-uplink or default-route changes may trigger mDNS re-evaluation and reannouncement, but goodbye/withdrawal is allowed only when an interface is missing, link-down, `filtered` as non-advertisable, proven no longer LAN-capable, or truly addressless after the existing dampening policy. An `unknown` role preserves the last known advertisable state under the existing dampening policy while logging/retrying classification. A LAN-only interface can keep advertising even when another interface is the WAN uplink.

### D8. App/DNS path gets publication reapplication, not blanket restarts

DNS-serving apps such as Pi-hole are ordinary service apps with port claims. The network transition handler should not restart all apps or special-case Pi-hole by name.

For this implementation, add a service-manager publication reconciler that, after an active-uplink/default-route/DNS-default/interface-role/interface-address transition:

- Reapplies expected firewall openings for currently registered public TCP/UDP port claims only when the current interface roles are safely LAN-publishable.
- Closes expected firewall openings for currently registered public TCP/UDP port claims when role or zone applicability is not safely known (for example mixed LAN plus WAN-only, or unknown ingress), leaving proxy/app registry state authoritative for later re-open.
- Keeps app/container repair ownership unchanged; this transition hook must not restart apps or recreate containers by itself.
- Logs the reconciliation attempt/result so the next incident can distinguish "transition observed" from "publication reapply attempted".

Automated DNS success in this RFC is limited to publication reapplication. Manual validation remains responsible for functional LAN DNS.

The deeper structural verifier is deferred. That follow-up should distinguish:

- `unknown`: inspection or probe failed; log `failed` and retry later without repair.
- `publication_drift`: two consecutive stable observations show listener or backend publication mismatch; schedule existing app reconcile repair.
- `healthy`: public listener and backend publication match expected state.

Publication verification should have a named retry owner: the service manager owns public listener/firewall verification and keeps a small in-memory drift ledger keyed by `(app, listener, public_port, protocol)`. After an `unknown` or first `publication_drift`, it schedules a bounded retry with short backoff (for example 5s, 15s, then the ordinary 30s app reconcile cadence). Service-manager listener/firewall drift is repaired by restarting that endpoint publication and applying the expected firewall rule to the current LAN-capable ingress zone(s); Podman backend drift remains repaired by the app manager's existing container reconciliation path.

Firewall verification is positive only when the expected rule applies to the current LAN-capable ingress interface(s)/zone(s), when those interfaces are known. A rule that exists in firewalld but is not applicable to the current LAN-capable interface is `publication_drift` after two stable observations; inability to determine role or zone/interface applicability is `unknown`, not healthy. Firewall-open or zone-application failures are logged as `failed` or `unknown`; they must not be hidden behind a running listener. After repair, the service manager must recheck zone/interface applicability before logging `publication-verified`. This prevents `publication-verified` from masking a transition where the listener is wildcard-bound but the firewall rule is effective only on the old interface or zone.

This keeps the blast radius small: network changes can wake verification, but app repair remains in the app manager and only proven drift schedules disruptive repair.

### D9. Undervoltage does not gate reconciliation

Software reconciliation must run even when undervoltage was recently observed. Hardware remediation and new health/audit surfacing remain out of scope.

### D10. Diagnostics log the transition and each owner result

Every emitted transition logs:

- generation
- reasons
- previous/current active uplink
- previous/current active interface names
- current connectivity
- AP active state

Each owner logs one bounded result per generation:

- `no-op`
- `pending-waiting-uplink`
- `restarted`
- `retry-scheduled`
- `publication-reapplied`
- `publication-closed`
- `repair-scheduled`
- `failed`

This is required because the next field incident needs to answer whether Piccolo saw the transition, whether each owner reacted, and where recovery stalled.

### D11. Deferred: fence stale relay callbacks and aggregate endpoint state

Network-triggered adapter restarts create a stale-callback risk: old backend clients can emit a disconnect after the new client has connected. The adapter lifecycle must attach a monotonically increasing run token to relay events or otherwise fence stale callbacks before they reach `remote.Manager.handleRelayEvent`.

Namek can register multiple relay endpoints. The adapter must track current-run endpoint state separately and aggregate to one adapter-level state for the remote manager: connected if any current endpoint is connected, disconnected only when all current endpoints are disconnected or errored. The remote manager's public relay state remains keyed by adapter name for UI/status, but only aggregated events from the current adapter run may update that state.

This is deferred from the current implementation because it changes the nexus adapter event contract and endpoint-state aggregation model. The follow-up must cover `old disconnect after new connect`, same-run partial endpoint failure, all-endpoints-down, and old-run endpoint events.

---

## Implementation Plan

1. Define `TopicNetworkTransition` and payload types in `internal/events` / `internal/network`.
2. Extend supervisor transition state capture, all-managed-interface enumeration, canonicalization, bounded transition history/latest-state storage, and diagnostic bus publication after each successful tick.
3. Add unit tests for transition reason derivation and dedupe behavior, including default-route-only, DNS-default-only, interface-role-only, same-kind multi-NIC (`eth0=lan`, `usb0=wan`, Wi-Fi present), reordered-equivalent-address, IPv6-churn suppression, and partial route/DNS observation cases.
4. Add the server-level owner-delta coordinator and owner reconciler interface, including full-buffer/coalesced-wake tests proving restart-relevant reasons survive a later connectivity-only generation, history overflow produces a conservative restart-relevant delta, and restart-relevant reasons observed while unusable are satisfied by a later connectivity-only usable generation.
5. Add self-hosted remote transition restart/retry wiring through `remote.Manager`, with tests for restart, config disable, deadline state, and negative LAN-only role/address changes that must not restart remotes.
6. Refactor Namek's identity subscriber goroutine into a serialized apply loop that accumulates both identity and network reasons, including tests for identity/network coalescence and unusable-uplink pending reasons.
7. Add mDNS transition reconciliation for interface loss/address/role change and tests for stale interface withdrawal versus active-uplink-only reannouncement, including a LAN-only interface that remains advertisable while Wi-Fi is the WAN uplink and an `unknown` role that preserves last known advertisable state during dampening.
8. Add service-manager publication reapplication on transition for existing active port claims, with fake-firewall tests proving current claims are reopened only for safely LAN-publishable states and closed for unsafe/unknown ingress states.
9. Add alpha VM netlab support for one management/WAN NIC plus one LAN-only test NIC and validate transition logs/roles under practical VirtualBox constraints.
10. Defer service-manager publication drift verification/repair and relay run-token endpoint aggregation to follow-up RFC work.

---

## Validation

Static/unit validation:

- `go test ./internal/network`
- `go test ./internal/remote/...`
- `go test ./internal/mdns`
- `go test ./internal/app ./internal/services`
- `go test ./internal/server`

Manual validation when device access returns:

- Start with Ethernet and Wi-Fi active; verify transition logs show active uplink Ethernet.
- Remove Ethernet; verify one transition whose reasons include `active_uplink_changed`, `default_route_changed`, and `dns_default_changed`, with current uplink Wi-Fi.
- Validate a LAN-only Ethernet plus WAN Wi-Fi case: Namek uses Wi-Fi for WAN/egress, while mDNS and claimed LAN app ports remain published on Ethernet.
- Validate a same-kind multi-NIC case when hardware is available: one Ethernet-class interface is LAN-only, another Ethernet-class interface is WAN-capable, and the transition state keeps both by concrete interface name.
- Verify Namek reconnect attempt starts immediately after the transition rather than waiting for the old retry cap.
- If the first reconnect attempt fails during route/DNS settling, verify the transition-recovery window schedules bounded retries until the current-run relay aggregate connects or the deadline expires.
- Verify `piccolospace.com` route returns through Namek after the transition.
- Verify `piccolo.local` and app hostnames withdraw old Ethernet addresses and announce Wi-Fi addresses.
- Verify Pi-hole DNS on its claimed port responds on the LAN after transition. This remains manual validation rather than an automated RFC guarantee.
- Repeat with Ethernet reinserted; verify the same owner reconcilers no-op or restart deterministically.

---

## Risks

- **Adapter restart flap:** Network changes can be noisy. Mitigation: generation dedupe plus per-owner cooldown on identical transition state.
- **Pending-reason overreach:** Sticky restart reasons could cause a later connectivity event to restart after the original transition is no longer relevant. Mitigation: clear pending reasons when the owner is disabled, the adapter/app is no longer expected active, or a newer satisfied transition supersedes the same surface.
- **Lossy bus delivery:** The existing event bus drops when subscriber buffers fill. Mitigation: owner recovery is driven by transition deltas read from retained supervisor state, while the bus topic is diagnostic only.
- **Namek rate limits:** Restarting immediately can request nonce tokens. Mitigation: keep existing debounce, restart only on proven usable uplink transitions, and do not loop faster than the existing identity debounce/cooldown allows; transition-recovery retries are capped by a deadline/cooldown.
- **mDNS goodbye behavior:** Immediate withdrawal can be wrong if transient DHCP renewal temporarily removes addresses. Mitigation: only immediate-withdraw for interface missing, down, or truly addressless; active-uplink/default-route changes reannounce or re-evaluate but do not withdraw an otherwise advertisable LAN interface.
- **WAN/LAN role conflation:** A LAN-only interface can be valid for local discovery and app ingress even when it cannot reach WAN. Mitigation: classify roles per interface and keep WAN/egress owner decisions separate from LAN/ingress owner decisions.
- **Role-classification uncertainty:** A temporarily unclassifiable LAN interface can look like a filtered or disconnected interface. Mitigation: keep `unknown` distinct from `filtered` and preserve last known advertisable state under dampening while role/firewall classification is retried.
- **Resolver-only transitions:** A default DNS interface change can occur without an active-uplink change. Mitigation: include `dns_default_changed` as a first-class transition reason and let app/DNS verification subscribe to it.
- **IPv6 churn classification:** Suppressing all global IPv6 churn can hide real renumbering, while treating privacy addresses as durable can flap relays. Mitigation: observe IPv6 temporary-address flags before suppressing global churn; stable non-temporary renumbering is restart-producing.
- **App repair blast radius:** Recreating containers on every transition would be disruptive. Mitigation: verification first, no repair on unknown/inspection failure, and repair only on repeated proven publication drift or existing health failure.
- **Firewall-zone uncertainty:** A listener can be bound while its firewall rule is effective on the wrong ingress interface. Mitigation: publication verification treats unknown zone/interface applicability as `unknown` and proven wrong applicability as drift, never healthy.
- **Legacy topic confusion:** Adding a second network topic creates two surfaces. Mitigation: document that the old topic is UI/compatibility only and the new topic is owner reconciliation.

---

## Alternatives Considered

### Alt-A. Patch only Namek restart on network-up

Rejected. It addresses the visible PiccoloSpace symptom but leaves the same stale-state class in self-hosted remote, mDNS advertisements, and app DNS publication. The incident included DNS symptoms before and during the remote outage.

### Alt-B. Make DNS multi-interface handling the only fix

Rejected. DNS was the first user-visible symptom, but the log shows Namek remained down after Wi-Fi L3 was up. Also, app DNS exposure is represented through listener/port-claim publication, not a standalone DNS manager in `piccolod`.

### Alt-C. Expand `NetworkStateChangedEvent`

Rejected for now. The existing payload is a compatibility contract and is also used for Wi-Fi signal-tier updates. Overloading it with reconciliation detail would force every legacy consumer to preserve new filtering rules.

### Alt-D. Central network manager directly restarts every dependent subsystem

Rejected. It violates current ownership boundaries, especially `applyNamekState`'s single-goroutine requirement. Owner-local reconcilers keep locking and repair policy with the subsystem that owns the state.

### Alt-E. Rely on upstream Nexus retry behavior

Rejected. The incident shows Wi-Fi was usable at `09:44:01`, while Namek did not reconnect until `10:00:10`. Retry-only behavior is not a sufficient recovery contract for a local active-uplink transition.

---

## Implementation Notes & Status

Status: implementation in progress in the current dirty tree. The scoped implementation covers transition state/deltas, owner-local remote/Namek retry, mDNS/service-publication wakeups, fail-closed firewall publication handling for unsafe mixed/unknown ingress, alpha netlab validation, and focused tests. Publication drift verification, positive firewalld zone applicability proof/retry, stable IPv6 renumbering, and relay endpoint-generation fencing are deferred follow-ups.
