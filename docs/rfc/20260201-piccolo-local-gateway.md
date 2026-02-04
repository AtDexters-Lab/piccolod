# RFC: piccolo.local as Gateway Domain

## Problem Statement

When multiple Piccolo devices exist on the same LAN, they compete for the `piccolo.local` mDNS hostname through conflict resolution. This causes several UX problems:

1. **Session/Cookie Confusion**: Users save passwords and cookies for `piccolo.local`, but different devices may serve that domain at different times due to race conditions during startup or network changes.

2. **Unpredictable Access**: Users can't reliably bookmark or remember which device they're accessing when using `piccolo.local`.

3. **Authentication Friction**: Saved credentials work intermittently depending on which device currently "owns" the hostname.

4. **Discovery Overhead**: Users must manually find device-specific hostnames (e.g., `piccolo-abc123.local`) to have reliable access.

## Proposed Solution

Transform `piccolo.local` from a contested resource into a **gateway/discovery domain** that provides a consistent entry point to the Piccolo ecosystem on a LAN.

### Key Principles

1. **Every device always publishes its unique domain**: `piccolo-<machineId>.local` (e.g., `piccolo-a1b2c3.local`)
2. **One device additionally serves the gateway**: `piccolo.local` shows a device selector
3. **Single-device optimization**: Auto-redirect to the only device's specific domain
4. **No authentication on gateway**: The gateway is purely for discovery/navigation

### Hostname Format

The specific hostname format is: `piccolo-<machineId>.local` where `machineId` is a 6-character hex string derived from `/etc/machine-id`, MAC address, or hostname (see `getMachineID()` in `internal/mdns/manager.go`).

Examples:
- `piccolo-a1b2c3.local`
- `piccolo-f13541.local`

## Behavior Matrix

| Scenario | piccolo.local Behavior |
|----------|------------------------|
| Single device on LAN | HTTP 302 redirect to `piccolo-xyz.local` |
| Multiple devices on LAN | Show device selector UI |
| Remote access (via Nexus) | Return empty/404 (same as `/api/v1/network/peers`) |

## Architecture

### mDNS Publishing Changes

```
Current:
┌─────────────┐     ┌─────────────┐
│  Device A   │     │  Device B   │
│ piccolo.local│←→  │piccolo.local│  (conflict!)
└─────────────┘     └─────────────┘

Proposed:
┌─────────────────────────┐     ┌─────────────────────────┐
│       Device A          │     │       Device B          │
│ piccolo-a1b2c3.local    │     │ piccolo-d4e5f6.local    │
│ + piccolo.local         │     │                         │
│   (gateway leader)      │     │                         │
└─────────────────────────┘     └─────────────────────────┘
```

### Leader Election for Gateway

The device that serves `piccolo.local` (gateway leader) is determined by:

1. **Lowest machine ID wins** (deterministic, avoids flip-flopping)
2. **Optimistic leadership on startup** with deferred conflict resolution
3. **Graceful handoff** via mDNS goodbye announcements
4. **Conflict detection**: If multiple leaders detected, defer to lowest ID

#### Peer "Online" Status Definition

A peer is considered **online** if:
```go
const PeerOnlineThreshold = 180 * time.Second  // 3x mDNS discovery interval (60s)

func (p *DiscoveredPeer) IsOnline() bool {
    return time.Since(p.LastSeen) < PeerOnlineThreshold
}
```

This matches the existing stale threshold used in `handleNetworkPeers()`.

#### Startup Sequence & Race Handling

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    DEVICE STARTUP SEQUENCE                               │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  T+0ms    Device boots, generates/loads machineID                       │
│     │                                                                   │
│     ▼                                                                   │
│  T+0ms    Immediately publish piccolo-<machineId>.local                 │
│     │     (always, unconditionally)                                     │
│     │                                                                   │
│     ▼                                                                   │
│  T+0ms    Start peer discovery (send PTR queries)                       │
│     │                                                                   │
│     ▼                                                                   │
│  T+2s     LEADERSHIP_CLAIM_DELAY expires                                │
│     │     Check: Any peers with lower machineID discovered?             │
│     │                                                                   │
│     ├──── NO peers with lower ID ────►  Claim piccolo.local leadership  │
│     │                                   (probe, then publish)           │
│     │                                                                   │
│     └──── YES peer with lower ID ───►  Do NOT claim leadership          │
│                                        (wait for that peer to lead)     │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

**Key constant:**
```go
const LeadershipClaimDelay = 2 * time.Second  // Wait for initial peer discovery
```

**Why 2 seconds?** This is enough time for 2-3 mDNS query/response cycles on a typical LAN, allowing discovery of existing peers before claiming leadership. It's a balance between:
- Too short: Risk claiming leadership before discovering existing leader
- Too long: Slow startup experience for single-device users

#### Simultaneous Startup Handling

When two devices start at the same time:

```
Device A (machineID: a1b2c3)          Device B (machineID: d4e5f6)
─────────────────────────────         ─────────────────────────────
T+0:  Publish piccolo-a1b2c3.local    T+0:  Publish piccolo-d4e5f6.local
T+0:  Start peer discovery            T+0:  Start peer discovery
T+1s: Discovers Device B              T+1s: Discovers Device A
T+2s: I have lowest ID → claim lead   T+2s: A has lower ID → don't claim
T+2s: Probe piccolo.local
T+2.5s: Publish piccolo.local

Result: Device A is leader (deterministic)
```

If both somehow claim leadership (network delay edge case):
- mDNS conflict resolution triggers (probe/defend)
- Both devices compare machineIDs
- Higher ID yields to lower ID
- Integrates with existing `ConflictDetector` in `internal/mdns/conflict.go`

#### Leader Election State Machine

```go
type LeadershipState int

const (
    LeadershipUnknown LeadershipState = iota  // Just started, waiting for discovery
    LeadershipClaimed                          // We are the leader
    LeadershipDeferred                         // Another device is leader
)

type GatewayLeader struct {
    mu            sync.RWMutex
    selfMachineID string
    state         LeadershipState
    claimTimer    *time.Timer

    // Callback when leadership changes
    onLeadershipChange func(isLeader bool)
}

func (g *GatewayLeader) Start() {
    g.state = LeadershipUnknown

    // Schedule leadership claim attempt after delay
    g.claimTimer = time.AfterFunc(LeadershipClaimDelay, func() {
        g.evaluateLeadership()
    })
}

func (g *GatewayLeader) OnPeerDiscovered(peer DiscoveredPeer) {
    g.mu.Lock()
    defer g.mu.Unlock()

    // If we discover a peer with lower ID before claiming, cancel our claim
    if g.state == LeadershipUnknown && peer.MachineID < g.selfMachineID {
        g.claimTimer.Stop()
        g.state = LeadershipDeferred
    }

    // If we're leader and discover lower ID peer, yield
    if g.state == LeadershipClaimed && peer.MachineID < g.selfMachineID {
        g.yieldLeadership()
    }
}

func (g *GatewayLeader) OnPeerGoodbye(peer DiscoveredPeer) {
    g.mu.Lock()
    defer g.mu.Unlock()

    // If the leader said goodbye, immediately re-evaluate
    if g.state == LeadershipDeferred {
        g.evaluateLeadership()
    }
}

func (g *GatewayLeader) evaluateLeadership() {
    g.mu.Lock()
    defer g.mu.Unlock()

    peers := g.getOnlinePeers()
    shouldLead := g.shouldBeLeader(peers)

    if shouldLead && g.state != LeadershipClaimed {
        g.claimLeadership()
    } else if !shouldLead && g.state == LeadershipClaimed {
        g.yieldLeadership()
    }
}

func (g *GatewayLeader) shouldBeLeader(peers []DiscoveredPeer) bool {
    for _, p := range peers {
        if p.IsOnline() && p.MachineID < g.selfMachineID {
            return false  // Someone else should lead
        }
    }
    return true  // We have lowest ID (or no peers)
}
```

### Leader Handoff

#### Graceful Shutdown (Clean Exit)

When a leader shuts down cleanly:

```
T0: Leader (Device A) calls Stop()
T1: Leader sends mDNS goodbye for piccolo.local
T2: Other devices receive goodbye
T3: Device B immediately re-evaluates leadership (no 180s wait)
T4: Device B claims piccolo.local
```

**Integration with existing code:** The `Manager.Stop()` method already sends goodbye announcements. We add a hook to trigger immediate leader re-evaluation on goodbye receipt:

```go
// In peer_discovery.go, handle goodbye packets
func (m *Manager) handleGoodbye(hostname string) {
    // Remove from peer registry immediately
    m.peerRegistry.Remove(hostname)

    // Trigger immediate leadership re-evaluation
    if m.gatewayLeader != nil {
        m.gatewayLeader.OnPeerGoodbye(hostname)
    }
}
```

#### Crash / Network Disconnect (Ungraceful)

When a leader disappears without goodbye:

```
T0:    Leader (Device A) crashes
T60s:  Peer discovery cycle - A doesn't respond
T120s: Peer discovery cycle - A still missing
T180s: A marked offline (3x missed intervals)
T180s: Device B re-evaluates, claims leadership
```

**The 180s gap is unavoidable** for ungraceful exits - this is inherent to mDNS's distributed nature. During this window:
- `piccolo.local` is unreachable
- Users can still access devices via `piccolo-xyz.local` (always works)
- Gateway UI shows "Checking for devices..." or similar

**Mitigation:** The gateway UI (when eventually reachable) can show a notice if it detects it recently took over leadership.

### Gateway Request Detection

The server must distinguish requests to `piccolo.local` (gateway) from requests to `piccolo-xyz.local` (device-specific).

#### Implementation: `isGatewayRequest()`

```go
const GatewayHostname = "piccolo.local"

// isGatewayRequest returns true if this request is for the gateway domain
func (s *GinServer) isGatewayRequest(c *gin.Context) bool {
    // Only serve gateway if we're the leader
    if !s.mdnsManager.IsGatewayLeader() {
        return false
    }

    // Check Host header
    host := c.Request.Host

    // Strip port if present (e.g., "piccolo.local:8080" -> "piccolo.local")
    if h, _, err := net.SplitHostPort(host); err == nil {
        host = h
    }

    // Case-insensitive comparison
    return strings.EqualFold(host, GatewayHostname)
}
```

#### Middleware Integration

The gateway route is registered early, before the existing `lanHostRoutingMiddleware()`:

```go
func (s *GinServer) setupGinRoutes() {
    // ... existing setup ...

    // Gateway routing (must be before lanHostRoutingMiddleware)
    r.Use(s.gatewayMiddleware())

    // Existing LAN host routing for app-specific domains
    r.Use(s.lanHostRoutingMiddleware())

    // ... rest of routes ...
}

func (s *GinServer) gatewayMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        if s.isGatewayRequest(c) {
            s.handleGateway(c)
            c.Abort()
            return
        }
        c.Next()
    }
}
```

#### Port Handling

The gateway works on any port the server listens on:
- Production: `piccolo.local:80` (default PORT=80)
- Development: `piccolo.local:8080` (when PORT=8080)

The `isGatewayRequest()` function strips the port before comparison, so both work correctly.

### Gateway UI (Flutter)

The gateway UI is implemented in Flutter for consistency with the rest of the UI. It's a minimal shell that:
- Has no authentication
- Fetches peer list from `/api/v1/network/peers`
- Shows device selector or auto-redirects

#### Gateway Shell Structure

```
ui/lib/shells/gateway/
├── gateway_shell.dart       # Main entry point
├── gateway_controller.dart  # Fetch peers, handle redirect logic
└── widgets/
    └── device_selector.dart # Device list UI
```

#### Gateway Controller

```dart
class GatewayController extends ChangeNotifier {
  final NetworkService _networkService;

  List<DiscoveredPeer> _peers = [];
  NetworkSelf? _self;
  bool _isLoading = true;
  String? _error;

  Future<void> initialize() async {
    _isLoading = true;
    notifyListeners();

    try {
      final response = await _networkService.getPeers();
      _self = response.self;
      _peers = response.peers;

      // Auto-redirect if single device
      if (_peers.isEmpty && _self != null) {
        _redirectToSelf();
        return;
      }

      _isLoading = false;
      notifyListeners();
    } catch (e) {
      _error = 'Could not discover devices';
      _isLoading = false;
      notifyListeners();
    }
  }

  void _redirectToSelf() {
    // Redirect to our specific hostname
    final url = 'http://${_self!.hostname}';
    html.window.location.replace(url);
  }

  void navigateToDevice(DiscoveredPeer peer) {
    html.window.location.href = peer.url;
  }
}
```

#### Shell Selection Logic

In `main.dart`, determine which shell to show:

```dart
void main() async {
  // ... existing initialization ...

  // Determine shell based on Host header
  final isGateway = await _isGatewayAccess();

  if (isGateway) {
    runApp(const GatewayShell());
  } else {
    runApp(const DesktopShell());  // or MobileShell
  }
}

Future<bool> _isGatewayAccess() async {
  final host = html.window.location.hostname?.toLowerCase() ?? '';
  return host == 'piccolo.local';
}
```

#### Gateway UI Design

```
┌─────────────────────────────────────────────────────────────┐
│                                                             │
│                    ◆ Piccolo                                │
│                                                             │
│           Select a device to continue:                      │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  ● Piccolo A1B2C3                                     │  │
│  │    Raspberry Pi 4 • 192.168.1.42                      │  │
│  │                                          [Enter →]    │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  ● Piccolo D4E5F6                                     │  │
│  │    NucBox K8 Plus • 192.168.1.50                      │  │
│  │                                          [Enter →]    │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  ○ Piccolo G7H8I9 (offline)                           │  │
│  │    Raspberry Pi 5                                     │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### Single-Device Auto-Redirect

When only one Piccolo device exists on the LAN:

```http
GET http://piccolo.local/
HTTP/1.1 302 Found
Location: http://piccolo-a1b2c3.local/
```

This preserves the simple `piccolo.local` experience for users with single devices while enabling proper domain isolation.

**Implementation:** The redirect happens client-side in Flutter after fetching peers and finding none. This allows showing a brief loading state rather than a server-side redirect that might confuse users.

### Session/Cookie Isolation

With this architecture:
- `piccolo.local` - No cookies (gateway only, no auth)
- `piccolo-a1b2c3.local` - Device A's session cookies
- `piccolo-d4e5f6.local` - Device B's session cookies

No more cookie collisions or credential confusion.

## API Changes

### Modified: mDNS Manager

```go
type Manager struct {
    // ... existing fields ...

    // Gateway leadership
    gatewayLeader    *GatewayLeader
    specificHostname string  // Always "piccolo-<machineId>.local"
}

// SpecificHostname returns the device's unique hostname (always includes machine ID)
func (m *Manager) SpecificHostname() string {
    return m.specificHostname
}

// IsGatewayLeader returns true if this device serves piccolo.local
func (m *Manager) IsGatewayLeader() bool {
    if m.gatewayLeader == nil {
        return false
    }
    return m.gatewayLeader.IsLeader()
}

// PublishedHostnames returns all hostnames this device advertises
func (m *Manager) PublishedHostnames() []string {
    hostnames := []string{m.specificHostname}
    if m.IsGatewayLeader() {
        hostnames = append(hostnames, "piccolo.local")
    }
    return hostnames
}
```

### Existing Endpoint Used: `GET /api/v1/network/peers`

The gateway UI uses the existing network peers endpoint (implemented in current PR) to fetch the device list. No new API endpoints needed for the gateway UI itself.

## Migration Path

### Single Release - Full Rollout

All changes ship together in one release:

1. **mDNS Changes**
   - All devices publish `piccolo-<machineId>.local` (always)
   - Leader election determines who additionally publishes `piccolo.local`

2. **Gateway UI**
   - New Flutter gateway shell
   - Served when accessing `piccolo.local`

3. **Backward Compatibility**
   - Single-device users: Auto-redirect preserves existing UX
   - Multi-device users: Now get proper domain isolation
   - Bookmarks to `piccolo.local`: Still work (gateway or redirect)
   - Bookmarks to `piccolo-xyz.local`: Work immediately after update

### Rollback Safety

If issues arise:
- Users can always access devices via `piccolo-<machineId>.local`
- The specific hostname is published unconditionally
- Only gateway functionality would be affected by bugs

## Edge Cases

### Leader Goes Offline (Graceful)

```
T0:   Leader (Device A) calls Stop()
T0:   Sends mDNS goodbye for piccolo.local
T0:   Device B receives goodbye, immediately re-evaluates
T0.5: Device B claims piccolo.local

Downtime: ~500ms (probe + publish)
```

### Leader Goes Offline (Crash)

```
T0:    Leader (Device A) crashes
T180s: Device B marks A as offline
T180s: Device B claims piccolo.local

Downtime: ~180s (unavoidable for ungraceful exit)
User mitigation: Access via piccolo-<machineId>.local
```

### Network Partition

If devices can't see each other but are on the same network:
- Multiple devices may claim `piccolo.local`
- mDNS conflict resolution handles this (probe/defend cycle)
- Lower machineID wins conflict
- Users may experience brief inconsistency until resolution

### First Boot / New Device

1. Device generates machine ID on first boot
2. Immediately publishes `piccolo-<machineId>.local`
3. Waits `LeadershipClaimDelay` (2s) for peer discovery
4. If no peers with lower ID discovered → claims gateway leadership
5. If peer with lower ID exists → defers to that peer

### New Device Joins Existing Network

1. New device (machineID: z9y8x7) starts
2. Publishes `piccolo-z9y8x7.local`
3. Discovers existing leader (machineID: a1b2c3)
4. a1b2c3 < z9y8x7 → new device does NOT claim leadership
5. Existing leader continues serving `piccolo.local`

### New Device Has Lower ID Than Existing Leader

1. New device (machineID: 000001) starts
2. Publishes `piccolo-000001.local`
3. Discovers existing leader (machineID: a1b2c3)
4. 000001 < a1b2c3 → new device should be leader
5. New device probes for `piccolo.local`
6. Existing leader detects conflict, compares IDs
7. Existing leader yields (stops publishing `piccolo.local`)
8. New device becomes leader

## Security Considerations

1. **No Auth on Gateway**: Gateway page has no sensitive data (just hostnames/IPs already broadcast via mDNS)
2. **LAN-Only**: Gateway uses same loopback detection as `/api/v1/network/peers` - returns empty for Nexus proxy requests
3. **No Cross-Device Access**: Gateway cannot access other devices' data, only redirects
4. **CSRF Safe**: Gateway uses GET redirects, no state-changing operations
5. **Rate Limiting**: Gateway endpoint should have rate limiting to prevent enumeration abuse (standard rate limit middleware)

## Implementation Checklist

| Component | Description | Status |
|-----------|-------------|--------|
| mDNS: Always publish specific hostname | `piccolo-<machineId>.local` always advertised | Pending |
| mDNS: GatewayLeader state machine | Leadership claim/yield logic | Pending |
| mDNS: Goodbye-triggered re-evaluation | Fast leader handoff on clean shutdown | Pending |
| mDNS: Conflict integration | Yield to lower machineID on conflict | Pending |
| Backend: Gateway middleware | Route requests to gateway handler | Pending |
| Backend: `isGatewayRequest()` | Host header detection | Pending |
| UI: Gateway shell | New Flutter shell for gateway | Pending |
| UI: Gateway controller | Peer fetching, redirect logic | Pending |
| UI: Device selector widget | Gateway device list UI | Pending |
| UI: Shell selection | Detect gateway vs device access | Pending |
| Tests: Leader election | Unit tests for state machine | Pending |
| Tests: Conflict resolution | Integration tests for ID comparison | Pending |
| Tests: Gateway routing | E2E tests for redirect behavior | Pending |
| Docs: Update hostname guidance | Recommend specific hostnames for bookmarks | Pending |

## Open Questions

1. **Device Labels/Names?**
   - Should devices have user-assigned names (e.g., "Living Room")?
   - Could use existing device name from system settings
   - **Decision needed**: Defer to future enhancement or include in this RFC?

2. **Remember Last Device?**
   - Should gateway remember (via localStorage) which device user last accessed?
   - Could auto-redirect to last-used device with "Switch device" option
   - **Recommendation**: Start simple (no memory), add later if requested

3. **Mobile App Implications?**
   - Does mobile app use `piccolo.local` for discovery?
   - If yes, app needs to handle 302 redirects or check response type
   - **Decision needed**: Verify mobile app behavior
