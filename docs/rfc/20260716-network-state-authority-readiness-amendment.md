# Network State Authority and Boot Readiness Amendment

**Problem:** A single-device compatibility projection can contradict the actual default-route interface, and allowing that optional network diagnostic to determine boot readiness can take an otherwise healthy Piccolo appliance offline.
**In scope:** Make typed per-interface state the sole network authority; remove the known-client ConnState REST and event contract; migrate health, identity, STUN, event-stream, alpha, and Flutter consumers; and make /health/ready depend only on required local services.
**Out of scope:** piccolo-os-support rollback and emergency-mode terminal behavior; NetworkManager, driver, router, power, or SSD failures; the still-unexplained inbound remote-alias and LAN-access failure; remote adapter, mDNS, and service-publication policy; and compatibility for unknown API clients.

Status: Implemented — review and RFC closure verified
Date: 2026-07-16
Amends:

- docs/rfc/20260505-stateless-network-supervision.md
- docs/rfc/20260629-network-transition-reconciliation.md

## Context and evidence

The incident machine had one Ethernet interface without a usable connection and
a second Ethernet interface with the default route. NetworkManager reported
global connectivity, and STUN and the relay established outbound connectivity.
The compatibility path nevertheless examined only the first sorted Ethernet
device and reported disconnected.

The network diagnostic then reported an error. /health/ready used the aggregate
health level rather than the result of only the required local checks, returned
HTTP 503, and piccolo-os-support subsequently stopped piccolod and
NetworkManager. That operating-system response is owned by piccolo-os-support,
but piccolod must not produce the false readiness failure that triggers it.

The repository has no unknown network-state API clients. The known consumers are
the health service, identity manager, STUN manager, unified WebSocket event stream,
alpha validation, and the bundled Flutter UI.

## Superseded decisions

This amendment supersedes prior decisions to:

- preserve ConnState, deriveLegacyState, the old connectivity event, or its
  event topic;
- preserve /api/v1/wifi/status as a compatibility endpoint; and
- let aggregate diagnostic health determine the HTTP status of /health/ready.

## Decisions

### D1. Separate connectivity truth from recovery observations

The supervisor observes every NetworkManager physical interface. The concrete
interface carrying the default route is the active uplink. Multiple interfaces
of the same kind remain independent first-class records.

DeviceObservation remains a class-level recovery input for actions such as
connection activation and access-point decisions. It is not connectivity truth
and cannot identify the active member of a same-kind interface set.

A completed observation with no default route publishes no active uplink.

An incomplete observation publishes active uplink none, omits the active
interface, and publishes connectivity as unknown. It must not replace a
previously concrete interface with the first interface of the same kind, and
must not expose a last-known route as current truth. Per-interface records may
remain present with unknown roles where observation supports that distinction.
Recovery logic may still use the class-level observation.

Recovery chooses the per-interface path only when interface enumeration and
route ownership are both completely observed. Interface enumeration alone is
not proof that the connectivity projection is complete. When route ownership is
unknown, recovery uses class-level device and L3 observations without promoting
their single-device interface field into connectivity truth.

### D2. Remove parallel compatibility state

ConnState, deriveLegacyState, the old connectivity event, and the old event
topic are removed. No adapter may recreate the enum behind the new API.

Topology and Wi-Fi signal are distinct internal facts. Signal changes do not
change route ownership, connectivity classification, or topology generation.

### D3. Expose one typed network-status API

/api/v1/network/status is the sole network status endpoint. It retains the
existing administration and LAN-or-same-public-IP authorization boundary.

The response exposes:

| Field | Meaning |
| --- | --- |
| active_uplink | none, ethernet, or wifi |
| active_uplink_iface | Concrete current default-route interface; omitted when unknown or absent |
| connectivity | full, limited, portal, none, or unknown |
| interfaces | Per-interface availability and connection facts |
| ap_active | Whether Piccolo's Wi-Fi access point is active |
| wifi_available | Whether Wi-Fi hardware is available |
| ssid, signal_dbm, signal_tier | Current active Wi-Fi details when applicable |
| frequency_mhz, band, ip_address | Current active Wi-Fi connection details when applicable |
| has_saved_network, saved_ssid | Saved-network recovery context |

The response does not contain a state field that projects these facts back into
ConnState. Wi-Fi fields remain flat to preserve the existing Wi-Fi management
model shape while removing only the compatibility state.

### D4. Publish latest state with monotonic generations

The supervisor owns the latest typed state and a monotonic in-process
generation. The first observation is generation 1 and wakes subscribers.
Material topology transitions advance the generation. Subscribers treat wakes
as hints and read the current state after waking.

Every latest-state consumer installs its subscription before reading current
state. The post-subscription read handles attachment after generation 1; later
wakes handle changes. Availability consumers also register before the
supervisor starts. Neither startup ordering nor a wake that happened before
subscription is the sole initial-delivery guarantee.

A bounded retained delta stream supports consumers that must reconcile every
topology transition. Consumers deduplicate by generation. Coalesced wake-ups are
valid because current state is authoritative.

The event bus publishes an initial network_status snapshot and subsequent full
snapshots. Topology and signal remain separate internal updates, but both cause
the external network_status event to carry the complete current response. A
client subscribing to network_status receives both sources of updates.

The full network_status event has the same access boundary as the REST
endpoint: admin plus LAN or same-public-IP access. Other event-stream clients
do not subscribe to or receive this topic, including its initial snapshot.
This prevents the event stream from bypassing the interface-address and SSID
boundary.

### D5. Migrate every known consumer

- Health reports the typed connectivity classification and concrete interface.
  A completed no-route observation may be an error diagnostic; incomplete route
  observation is warning/unknown and cannot claim that no network exists.
- Identity and STUN react only to full or limited connectivity on a concrete
  interface whose observed route matches that interface.
- The event stream sends an initial full snapshot and full transition updates.
- Wi-Fi signal lookup uses the concrete active Wi-Fi interface.
- Alpha checks assert the factual schema rather than compatibility states.

Identity and STUN consume the first proven generation as well as later
transitions. Signal-only and uncertain observations cannot wake them as if
connectivity had become available.

### D6. Make readiness local and explicit

The required readiness set is:

- persistence;
- app manager; and
- service manager.

/health/ready returns HTTP 503 with ready false when a required check is
missing or error. A required warning is a normal locally recoverable state
during pre-unlock and initialization, so it returns HTTP 200. The payload may
still report ready false and the warning component details until every required
check is OK.

The aggregate diagnostic status remains informational and may be error while
ready is true. Network health is optional for boot readiness because an offline
Piccolo must remain locally operational and recoverable.

HTTP status is the boot-health contract consumed by piccolo-os-support.
The ready field is the stricter full-local-service readiness projection. This
intentional distinction prevents a normal pre-unlock warning from triggering
rollback while still exposing that apps are not yet available.

### D7. Derive UI presentation from typed facts

The user-visible scope is the Network settings status card and desktop dock
indicator. A first-time owner needs an actionable AP or captive-portal state; a
returning owner needs quick current status and recovery direction; a power user
needs the factual connection state without compatibility terminology.

The UI presents full as connected and limited as connected with a limited
qualifier. none is Disconnected. unknown is Checking network, with copy that
the status is temporarily unavailable rather than claiming the cable or
internet is down. portal is Sign-in required, with copy that Piccolo cannot
complete a browser captive-portal flow and the owner should choose another
network. The existing Change Network action is the recovery path.

Access-point presentation has precedence while the Piccolo AP is active. A
reconnecting presentation may be derived when no uplink is active, Wi-Fi
hardware is available, saved networks exist, and connectivity is none. unknown
and portal never collapse into reconnecting. Settings and the dock consume the
same typed model and vocabulary. The dock may hide ordinary full Ethernet, but
it must show limited, unknown, portal, reconnecting, disconnected, and AP
states.

The Network settings screen always presents the current uplink status, including
Ethernet-only devices. Absence of Wi-Fi hardware hides Wi-Fi actions; it does
not replace the whole screen with a no-Wi-Fi message. Settings shows the
concrete active interface when one is known; the dock stays concise.

### D8. Ship as one coordinated breaking artifact

The daemon and bundled UI ship from the same build. There is no compatibility
window for the removed REST or event contract.

An already-open browser tab from the previous build may show stale network
status or an endpoint error until it is refreshed after an upgrade. This cannot
change daemon state or boot readiness. A page reload revalidates code assets and
loads the coordinated bundle; the release handoff calls out that recovery. This
bounded version-skew consequence does not justify retaining a second network
authority or endpoint alias.

## Implementation site list

### Network authority and recovery

- internal/network/observation.go
- internal/network/probe.go
- internal/network/probe_transition.go
- internal/network/decide_ap.go
- internal/network/transition.go
- internal/network/supervisor.go
- internal/network/snapshot.go
- internal/network/types.go
- internal/network/manager.go
- internal/network/signal.go
- internal/network/nmclient/client.go
- internal/network/nmclient/dbus_client.go
- internal/network/nmclient/stub.go
- internal/events/bus.go

Input contracts verified without implementation changes:

- internal/network/probe_eth.go
- internal/network/probe_wifi.go
- internal/network/probe_l3.go
- internal/network/nmclient/types.go

### Server and consumers

- internal/server/gin_wifi_handlers.go
- internal/server/gin_server.go
- internal/server/network_transition.go
- internal/server/gin_event_stream.go
- internal/server/gin_middleware.go
- internal/health/tracker.go

### Bundled client and validation

- ui/lib/core/models/wifi_models.dart
- ui/lib/core/services/wifi_service.dart
- ui/lib/core/services/event_stream_client.dart
- ui/lib/shells/desktop/features/settings/tabs/network/network_controller.dart
- ui/lib/shells/desktop/features/settings/tabs/network/network_tab.dart
- ui/lib/shells/desktop/features/settings/tabs/network/widgets/wifi_status_card.dart
- ui/lib/shells/desktop/widgets/dock.dart
- scripts/alpha/dev-vm-alpha-test.sh

The generic WebSocket transport in
ui/lib/core/services/websocket_connection.dart required no protocol-specific
change. Generated web assets and the packaged piccolod binary remain release
outputs rather than source changes.

### Tests and documentation

- internal/network/decide_test.go
- internal/network/manager_health_test.go
- internal/network/manager_status_test.go
- internal/network/probe_test.go
- internal/network/transition_test.go
- internal/health/tracker_test.go
- internal/server/gin_event_stream_network_test.go
- internal/server/gin_health_handlers_test.go
- internal/server/network_transition_test.go
- ui/test/network_status_model_test.dart
- ui/test/wifi_status_card_test.dart
- docs/rfc/20260505-stateless-network-supervision.md
- docs/rfc/20260629-network-transition-reconciliation.md

Tests cover the authority, subscriber, handler, readiness, consumer, event, and
UI contracts above. Prior RFCs remain historical context and point to this
amendment where their compatibility decisions are superseded.

## Temporal composition

The shared protocol invariant is that optional external-network observations
cannot acquire authority over either concrete per-interface truth or local
boot readiness. Network state has a long-lived lifecycle; readiness is a
synchronous projection of required local checks and introduces no separate
persisted lifecycle.

### Canonical lifecycle events

| Event | Authority | State transition | Observable effect | Durable record | Retry | Cleanup or compensation |
| --- | --- | --- | --- | --- | --- | --- |
| Start or activation | Supervisor | No state to generation 1 with completed or explicitly uncertain observation | Wake and initial event after state is stored | In-memory current state and generation; no disk state | Probes continue until a later observation restores certainty | A pre-generation client receives the initial snapshot when available |
| Normal completion or commit | Supervisor | Current generation to next generation on a material observation | Wake and full event snapshot | In-memory current state plus bounded delta history | Not needed for commit; consumers may replay retained generations | Older current state is superseded |
| Abnormal failure before an authoritative projection | Prober and supervisor | Current state to active uplink none and explicit uncertainty | No manufactured availability, last-known-as-current route, or first-same-kind substitution | Uncertain in-memory generation; no disk state | Next probe may restore certainty; recovery may use class observation | Later concrete observation replaces uncertainty |
| Pause or suspension | None | Not applicable because supervision and subscriptions have no paused state | None | None | None | Cancellation or process restart is the supported boundary |
| Resume or reacquisition | None | Not applicable because no paused state exists | None | None | None | Re-observation after process restart establishes a new sequence |
| Cancellation, interruption, or abort | Owning manager or server | Active probes, polling, subscribers, and registrations to terminated | No further wake reaches the cancelled owner | Registration is removed; no persistent record | Owner restart may subscribe again | Queued wakes cannot retain or call the cancelled owner |
| Supersession, handoff, or owner change | Supervisor | Generation N to a newer current generation | Coalesced wakes direct latest-state consumers to the newest state | Bounded retained generations remain for reliable reconcilers | Reliable consumers continue after their last handled generation | Old generations are discarded when bounded history expires |
| Retry or replay | Consumer | Last handled generation to current retained generation | Idempotent reconciliation; each generation handled at most once | Consumer-local last handled generation | Retry from that generation while history is retained | History overflow forces reconciliation from full current state |
| Restart or recovery | Process and supervisor | Old in-memory sequence is discarded; first new observation becomes generation 1 | New initial snapshot; readiness may succeed while offline | Network state is rebuilt, not restored from disk | Normal probing resumes | Consumers discard the old process generation domain and rebuild |
| Rollback or compensation | Release owner | Coordinated daemon and bundled UI return to the prior artifact | Prior API and UI resume together | Prior release artifact is the rollback record | Redeploy the coordinated artifact if required | Refresh browser tabs that retained the replaced bundle |
| Partial completion or one-sided effect | Event-stream server and latest-state consumers | Subscription is installed before current state is read | A transition cannot be lost between subscribe and snapshot, and attachment after generation 1 still initializes | Supervisor state remains authoritative | Post-subscription current read corrects pre-generation or pre-attachment timing | Unsubscribe on owner or stream termination |
| Concurrent overlap or reordering | Supervisor, signal monitor, and event-stream server | Topology and signal updates may arrive in either order without signal changing topology generation | Every external update contains a complete current response | Topology generation and current signal snapshot remain separately owned | A later full snapshot converges the client | Client replaces presentation from the complete snapshot |

### Effect ordering

- The supervisor stores current state and generation before publishing a wake or
  event.
- Network status is intentionally ephemeral; no disk write is added.
- Readiness evaluates the current required-check results synchronously and does
  not wait for network recovery.
- Required warnings remain visible in the payload but do not produce the
  boot-fatal HTTP status reserved for missing or failed required checks.

### Ownership and concurrency

- The supervisor owns typed state, generation, retained topology deltas, and
  their synchronization.
- The network manager and server own their subscriber goroutines and
  registrations.
- Event payloads are wake hints internally; consumers read authoritative current
  state rather than treating a possibly delayed hint as state.
- No consumer holds its own lock while calling into the supervisor.
- Coalescing is allowed for latest-state consumers. Reliable reconcilers use
  generations.
- Signal updates cannot advance topology generation or trigger identity or STUN
  availability work.

### Adversarial scenarios

1. enp1s0 is disconnected while enp2s0 owns the default route.
2. A primary Wi-Fi interface is unavailable while a second Wi-Fi interface is
   active.
3. Projection becomes incomplete after a concrete second interface was active.
4. An event-stream client connects before generation 1.
5. An availability subscriber attaches after generation 1 but before the next
   transition.
6. Signal changes concurrently with a topology transition.
7. The active route becomes none or its projection becomes unknown.
8. The Piccolo access point becomes active while route projection is unknown.
9. All required local checks are OK while network health is error.
10. A required local check is warning during pre-unlock.
11. A required local check is missing or error.
12. A remote or non-admin event-stream client requests network_status.
13. Cancellation races with a queued subscriber wake.
14. The process restarts while a browser still has the previous JavaScript
    bundle open.

## Alternatives considered

### Patch only the first-Ethernet selection

Rejected. It fixes one observed topology but preserves two competing
authorities and repeats the defect for multiple Wi-Fi devices and future
consumers.

### Keep a permanent compatibility adapter

Rejected. All clients are known, so the adapter would add an unowned authority
without serving a real migration requirement.

### Keep compatibility for one release

Rejected. The daemon and bundled client are a coordinated artifact. The bounded
stale-tab refresh risk is smaller than another release of conflicting state.

### Make network a required readiness check

Rejected. It makes loss of an external dependency capable of disabling the
local appliance and its recovery control plane.

### Fix only piccolo-os-support

Rejected as incomplete. The operating system must prevent a stopped-daemon
terminal state, but piccolod must also stop emitting a false readiness failure.

## Implementation and closure

1. Reconcile the current implementation against D1-D8, especially incomplete
   projection and the prohibition on last-known-as-current route behavior.
2. Run the full Code Review Flow with a review ledger, including security, UX,
   and dirty-tree gating lanes.
3. Run RFC implementation closure against this amendment and its acceptance
   criteria.
4. Commit and release only after both review flows converge.

### Closure result — 2026-07-16

The implementation, scoped code review, security review, UX review, dirty-tree
gating review, and RFC-to-code closure have converged. Reviewer role contracts
were applied locally because this run did not authorize delegated reviewers.

Review ledger:

| ID | Finding | Disposition | Verification |
| --- | --- | --- | --- |
| CR-1 | Losing route/DNS observation could fail to replace otherwise unchanged current state | Fixed by making observation loss a material transition | Transition-store observation-loss test |
| CR-2 | Cancellation could return while an availability consumer still owned an in-flight wake | Fixed with cancellation-aware worker draining and post-read cancellation checks | Queued-wake cancellation test and race tests |
| CR-3 | Availability accepted matching route strings without proving the concrete interface still existed and was usable | Fixed by requiring observed interface kind, link, address, and route agreement | First/later proven-generation tests |
| CR-4 | Limited connectivity was reported as healthy rather than degraded diagnostic state | Fixed as warning while remaining optional for boot readiness | Network-health tests |
| CR-5 | The anticipated site list included files that required inspection but no code change | Split changed sites from verified unchanged dependencies | Dirty-tree scope check |
| CR-6 | Active WiFi band/frequency came from an SSID scan-cache match, which could select stale data or another AP sharing the SSID | Read the frequency from NetworkManager's concrete active access point | Concrete-active-interface status test |

Security review found no remaining authorization-boundary issue: both REST and
event-stream network status require admin plus LAN or same-public-IP access.
UX review found the typed state vocabulary and Ethernet-only view consistent
across Settings and the dock. Dirty-tree gating found no out-of-scope change.

Validation evidence:

- `go test ./...` passed.
- focused network/server race tests passed.
- relevant Flutter model and widget tests passed on both VM and Chrome.
- `flutter analyze --no-fatal-infos` passed with nine pre-existing info-level
  lints and no errors or warnings.
- `flutter build web`, the production piccolod build, alpha-script syntax, and
  `git diff --check` passed.
- The current dirty-tree build was deployed to the three-interface alpha VM.
  Pre-setup passed 4/4, first-run setup passed 4/4, post-setup passed 6/6,
  net-supervisor passed 7 checks with only its two WiFi-hardware checks skipped,
  and multi-interface transition validation passed 7/7 with no skips. The live
  typed status reported `ethernet` on the concrete `enp0s3` default-route
  interface while retaining both non-active Ethernet interfaces, readiness
  returned HTTP 200 with `ready: true`, and piccolod remained active.

The repository-wide Flutter suite still contains two pre-existing,
target-incompatible groups: a browser-only `dart:js_interop` test cannot load
on the VM, while two tests that read source files through `dart:io` cannot run
in Chrome. These failures are outside this change; all network-state tests and
the production web build pass.

## Acceptance criteria

- A second Ethernet or Wi-Fi interface carrying the default route is reported
  exactly, regardless of sort order.
- ConnState, its derivation, endpoint, event, and event topic no longer exist.
- Every known consumer uses typed state.
- Incomplete projection publishes no active uplink or interface, cannot
  substitute the first same-kind interface, and makes uncertainty explicit.
- Optional network failure alone cannot make /health/ready return 503.
- A required warning remains HTTP 200 with warning details; a missing or failed
  required check returns 503.
- Identity and STUN handle the first and later proven generations, but not
  signal-only or uncertain changes, including when they attach after generation
  1.
- The event stream supplies an initial snapshot and both topology and signal
  updates without losing a transition during subscription, and enforces the
  REST endpoint's access boundary.
- The UI distinguishes full, limited, offline, uncertain, captive-portal,
  reconnecting, and AP-active presentations with AP precedence.
- Ethernet-only devices retain a useful Network settings status view while
  Wi-Fi actions are absent.
- Relevant Go and Flutter tests and production builds pass.

## Validated assumptions and external dependencies

- The owner confirmed on 2026-07-16 that the repository client inventory is
  complete and there are no external consumers of the removed endpoint or
  event.
- NetworkManager's default route is the authority for the active uplink.
- limited connectivity is sufficient to wake identity and STUN when it is tied
  to a concrete routed interface.
- Removing the compatibility contract accepts manual refresh of a stale browser
  tab as the coordinated-upgrade boundary; code assets revalidate on reload.
- piccolo-os-support will separately ensure that health-check failure cannot
  leave a 24x7 appliance powered on with piccolod stopped.
