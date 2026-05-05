# Stateless Probe-Decide-Act Network Supervision

## Scope

**Problem:** A 24×7 single-user appliance lost LAN + remote reachability for 13 days because `internal/network/watchdog.go::tick()` silently early-returns when `gateway == ""`, the `state.go` ConnState machine has 7 fragmented AP entry paths, and `internal/mdns/interface.go::checkInterfaceChanges` reconfigures every 10s when an interface loses its IP — together producing a silent stuck state that violates the never-silent-failure constraint stated in the supervision PS.

**In scope:**
- Replace `internal/network/state.go` (ConnState machine + `cancelSTA` + 7 AP entry paths) and `internal/network/watchdog.go` (procedural ladder with silent early-returns) with a layered probe → decide → act architecture.
- Replace `manager.go::handleAPTransition` + `apRetryActive` + `revertAPState` with a single APArbiter rule.
- Rewrite `internal/mdns/interface.go::checkInterfaceChanges` to treat IP-loss as transient (eliminate the 10s reconfig storm).
- Introduce a Tri-state observation model (Healthy / Faulted / Inactive) for HW and Config health per device.
- Introduce a single ActionLedger (bounces, reboots, AP-toggles) as the only persistent state, used purely for self-rate-shaping; no world-state caching.
- Expose a `Supervisor.Snapshot()` reader for three on-demand consumers (captive portal, mDNS TXT updater, journald-as-it-happens). No fan-out, no event bus, no namek channel. Existing `/api/wifi/status` wire contract preserved via a `deriveLegacyState` mapping.

**Out of scope:**
- NM driver-level fixes (`rtw88_8723de` Realtek, `brcmfmac` Broadcom) — OS-team concern.
- `internal/mdns/resilience.go` parallel health-check loop — separate cluster, captured as deferred follow-up.
- Namek-side health surfacing / telemetry pipeline — server-side detects piccolod tunnel-drop already; piccolod→namek event push would solve the wrong end.
- L3 deep probes beyond gateway ARP and NM `Connectivity` property (no upstream HTTP probes added).
- Channel-congestion / RSSI / throughput metrics (B5 in catalog explicitly accepted as undetectable).
- Migration of historical `/var/lib/piccolo/net-watchdog-reboots` data — read-and-discard at first orchestrator start; reboot history starts fresh post-cutover.

---

## Failure catalog (acceptance bar)

Every scenario below maps to a deterministic decision over `(Tick, Ledger, UserIntent)`. Each becomes a unit test on the pure deciders. The catalog is the contract.

### A. Hardware-intrinsic faults
- **A1** WiFi driver wedge (Realtek/BCM-style) — scan empty, NM Unavailable past grace
- **A2** WiFi rfkill stuck on with no profiles — Inactive (intent allows off)
- **A3** Ethernet driver wedge — carrier=1 + NM Unavailable past grace
- **A4** NIC firmware crash unrecoverable by bounce — escalation to reboot
- **A5** Cold-boot device init slow — grace window suppresses action (system uptime, not process — S1-rev)
- **A6** WiFi rfkill stuck on with profiles configured — Faulted (intent contradicts), recovery via rfkill soft-unblock
- **A7** HW Faulted while onboarding install_disk in progress — both bounce and reboot deferred until `SystemBusy()=false`
- **A8** Multi-device-per-kind ignored — second wlan/eth device present, supervisor picks first NM-managed device of each kind, others ignored
- **A9** WiFi rfkill **hard**-blocked (physical switch off) — `Snapshot.Hint = "physical wifi switch off"`; bounce won't help; AP impossible if no other uplink (Inactive status + Hint conveys this; owner sees in journald/mDNS-TXT or via captive portal if reaching us via eth)

### B. Medium / link faults
- **B1** Eth cable unplugged — Inactive, no action
- **B2** Eth cable damaged (carrier flapping) — N-of-M dampens; surface only
- **B3** WiFi out of range — scan empty for SSID, others may be visible — retry via NM, AP after sustained loss
- **B4** WiFi RSSI too weak (assoc fails) — ConfigHealth Faulted → AP-handover candidate
- **B5** WiFi channel congestion — undetectable; do not act (accepted)

### C. Peer / upstream faults
- **C1** Router powered off — both uplinks may fail; AP if wifi HW healthy
- **C2** Router rebooting (transient <60s) — N-of-M dampens, no action
- **C3** ISP down, router up — `NMConn=limited`, GwReachable=True — surface only
- **C4** DHCP refused / lease expired — NM stuck Activating — bounce on persistence
- **C5** IP conflict — surface only (NM handles)
- **C6** Captive portal upstream — `NMConn=portal` — surface only
- **C7** DNS broken upstream — surface only
- **C8** ARP-suppressed network — `GwReachable=False` + `L3Probe=Up` (TCP-connect succeeds) — surface only, no bounce. NMConn-disagreement is benign (advisory only). HW-Healthy + L3-dead persistent cases (DHCP wedge, stale post-reboot association) also fall into surface-only — NM handles its own re-association on disassoc; we don't second-guess.
- **C9** Corporate guest WiFi blocking probe targets — TCP-connect to 8.8.8.8:53 / 1.1.1.1:53 both blocked. Falls into C8 surface-only behavior; no bounce.

### D. Configuration drift (AP-handover triggers)
- **D1** SSID renamed — saved profile never appears in scan — AP after retries
- **D2** WiFi password changed — assoc auth-error — rate-limit, then AP
- **D3** Security mode changed — assoc EAP/WPA error — same as D2
- **D4** Router replaced, same SSID — NM auto-reuses BSSID, transparent
- **D5** Router replaced, new SSID — same as D1

### E. User intent shifts (no anomaly)
- **E1** User unplugs eth permanently, wifi profile exists — wifi takes over silently
- **E2** User plugs eth into wifi-only device — eth used per NM priority
- **E3** User moves device to new SSID environment — same as D1
- **E4** User explicitly forces AP via UI — `UserIntent.ForceAP=true`
- **E5** User unplugs eth briefly to power-cycle router — auto-recover

### F. Fresh-install / onboarding
- **F1** No saved wifi, eth plugged — eth carries
- **F2** No saved wifi, no eth — AP for owner
- **F3** Saved wifi from clone/reset, wrong env — falls into D1 → AP
- **F4** Cold boot, wifi HW slow init — grace window (A5)

### G. Composition / race scenarios
- **G1** Eth comes up while in recovery-AP — exit AP unconditionally (S2-rev: exit never suppressed by toggle budget)
- **G2** WiFi STA recovers while in user-forced AP — stay in AP (intent.ForceAP overrides). Captive portal reads `Snapshot.APMode.Reason="user-forced"` for narrative copy: "you forced AP — release here"
- **G3** Both uplinks present, one fails — other carries; no HW action on healthy one
- **G4** Both uplinks fail simultaneously — escalate per dept; AP if wifi HW healthy
- **G5** Bounce in flight while next tick fires — N-of-M counter does not advance for the bounced device for 5 ticks (F-B1 quiet period); decideHW returns Wait
- **G6** piccolod restart mid-recovery (deploy, OOM-kill, transactional-update reboot) — volatile ledger missing post-tmpfs-clear is the **steady-state** path: fail-OPEN, empty bounce budget. Persistent reboot ledger preserved across boot. Corruption (rare) is the only fail-closed path. (F-B4 fix.)
- **G7** Reboot decision concurrent with disk-op start — `SystemActuator.Reboot` queries `SystemBusy()` and proceeds. Microsecond TOCTOU window is bounded by transactional-update's restart-tolerant integrity model. No reboot-lock primitive.
- **G8** Tick produces both `Bounce(WiFi)` and `APEnter` — HW action runs first; APEnter waits for next tick after bounce settles via per-device mutex (S5-rev)

---

## Architecture

```
            ┌──────────────────┐
            │  Orchestrator    │  for tick := range ticker
            │  (single loop)   │     1. probe  → Tick
            └────────┬─────────┘     2. decide → Actions
                     │               3. act    → update Ledger
       ┌─────────────┼──────────────────┐
       ↓             ↓                   ↓
  ┌─────────┐ ┌──────────────┐    ┌───────────────┐
  │ Probe   │ │ Decide (3×)  │    │ Actuators     │
  │ - WiFi  │ │ - HWRecover  │    │ - Device      │
  │ - Eth   │ │ - APArbiter  │    │ - APMode      │
  │ - L3    │ │ - Surfacer   │    │ - System      │
  └─────────┘ └──────────────┘    └───────────────┘
```

### Design principles

1. **Three departments, never overlapping.** HW (device intrinsic), Config (profile/peer match), Env (upstream beyond peer). Every anomaly belongs to exactly one. Each department has its own decider.
2. **Stateless world-model, stateful self-model.** Every tick reprobes the world; we never cache "device was healthy yesterday". The only state we keep is *what we ourselves recently did* (action ledger, for rate-shaping).
3. **Three modes, all perpetual, no terminal states.** Act / Wait+Retry / Wait+Observe. Rate-shaping bounds aggressiveness, not duration. The 24×7 appliance never stops trying.
4. **Probe-driven, not phase-driven.** No states in the orchestrator — only fresh observations and pure decisions over them. NM keeps its own L2 state machine; we don't duplicate it.
5. **Surfacing must not depend on the channel it's reporting about being broken.** Multi-channel fan-out with reachability-graded fallback.

---

## Type shapes (decisions, not bodies)

```go
type Tri int  // Healthy | Faulted | Inactive

type DeviceKind int  // WiFi | Ethernet

type DeviceObservation struct {
    Kind          DeviceKind
    Present       bool
    HWHealth      Tri
    ConfigHealth  Tri            // Healthy | Faulted | Inactive — DHCP-in-flight maps to Healthy
    LinkUp        bool
    HasIP         bool
    GwReachable   Tri
    RfkillHard    bool           // S6-rev: distinguishes physical kill switch from soft-block
    NMState       string         // diagnostic only
    NMReason      string         // diagnostic only
}

type Connectivity int  // None | Portal | Limited | Full

type Tick struct {
    Devices       map[DeviceKind]DeviceObservation
    NMConn        Connectivity      // advisory only — see L3 probe section
    L3Probe       L3ProbeResult     // primary L3-truth: TCP-connect probe to 8.8.8.8:53 / 1.1.1.1:53

    // ActiveUplink — N-S7 fix: predicate is HWHealth==Healthy AND LinkUp
    //   - GwReachable is intentionally NOT required (avoids one-tick flicker
    //     to None during transient gateway-ARP loss; matches current loose
    //     state.go semantics).
    //   - Priority: ethernet-if-eligible-else-wifi-if-eligible-else-none.
    //   - AP mode maps to UplinkNone per existing wire convention
    //     (UplinkType has no UplinkAP value).
    ActiveUplink  UplinkType

    // SystemBusy — N-B3 fix: read once per tick from SystemState, passed as
    // data to deciders (not as a re-callable interface method). Closes
    // mid-tick TOCTOU between deciders and avoids per-decider call cost.
    SystemBusy       bool
    SystemBusyReason string

    SystemUptime  time.Duration     // S1-rev: sourced from /proc/uptime, NOT process Uptime
    At            time.Time
}

type ActionLedger struct {
    // Persistent — /var/lib/piccolo/net-ledger.json — survives boot
    // Migration: legacy /var/lib/piccolo/net-watchdog-reboots is MIGRATED (not discarded —
    // S8-rev fix), then legacy file deleted. Preserves reboot budget across cutover.
    Reboots []time.Time

    // Volatile — /run/piccolo/net-ledger-volatile.json — cleared on boot
    Bounces       map[DeviceKind][]time.Time
    APToggles     []APEvent
    LastBounceAt  map[DeviceKind]time.Time   // F-B1 fix: post-action quiet period anchor
}

type UserIntent struct {
    // N4-S4 fix: ForceAP and SuppressAP are BOOT-VOLATILE — NOT persisted
    // across reboot. Both default to false on every piccolod start. Rationale:
    //   - ForceAP=true is typically transient (user fixing something now); a
    //     stale ForceAP surviving reboot would silently lock the device into
    //     AP mode after upgrade.
    //   - SuppressAP=true would similarly survive reboots and silently disable
    //     AP recovery long after the user forgot they set it.
    //   - Clean post-reboot baseline avoids stale-intent foot-guns.
    // Captive portal credential entry calls SetForceAP(false) defensively
    // (no-op when already false; correct behavior either way).
    ForceAP    bool   // user explicitly requested AP via UI (boot-volatile)
    SuppressAP bool   // user explicitly disabled AP recovery (boot-volatile)
}

type SystemState interface {
    // O(1) in-memory read.
    //
    // Internally: SystemState subscribes to TopicOnboardingStateChanged at
    // construction; the latest payload is cached under a mutex. SystemBusy()
    // returns true iff the latest onboarding phase ∈ {pending, install_disk}.
    //
    // Why this is enough: install_disk is the only condition where bouncing
    // wifi could disrupt something irrecoverable (in-flight OS install). All
    // other "busy" signals on this OS (transactional-update mid-flight, LVM
    // ops) are restart-tolerant by design — bouncing wifi during a TU fetch
    // at worst causes a TCP retry. We don't need to gate on them.
    //
    // Reboot precondition is also gated on SystemBusy() (advisory query,
    // not a lock). The microsecond TOCTOU between the busy check and
    // systemctl reboot is bounded by transactional-update's restart-tolerant
    // integrity model — TU's whole design is "build snapshot → mark default
    // → reboot to finalize", so a reboot mid-flight at worst retries the
    // snapshot on next boot.
    SystemBusy() (busy bool, reason string)
}

// Decider signatures (decideSurface dropped — Snapshot is mechanical, see Surface section)
func decideHW(obs DeviceObservation, ledger ActionLedger, tick Tick, sys SystemState) HWAction
func decideAP(tick Tick, ledger ActionLedger, intent UserIntent, sys SystemState) APAction

// Action union types
type HWAction interface{}  // Bounce(dev) | Reboot | Wait
type APAction interface{}  // APEnter | APExit | APUnchanged

// B3-rev fix: legacy wire-contract preservation for /api/wifi/status.
// Pure mapping function from new-world observations to the legacy 5-string ConnState enum.
// Flutter app remains unchanged.
//
// Cases evaluated in order (priority encodes eth>wlan tiebreaker — N-B1/F-B6 fix):
//   "ap_mode"        — apActive=true (highest precedence)
//   "ethernet"       — eth.GwReachable=Healthy
//   "wifi_connected" — wlan.GwReachable=Healthy
//   "reconnecting"   — wlan.HWHealth=Recovering OR wlan.ConfigHealth=Faulted with HW Healthy
//   "disconnected"   — none of the above
//
// Status.ActiveUplink is sourced from tick.ActiveUplink (priority-based, see Tick type).
// Both fields populated from the same source — never inconsistent.
func deriveLegacyState(tick Tick, apActive bool) string
```

### Tri-state classification matrix

`HWHealth` for a device:

| Preconditions (rfkill, carrier, intent) | NM | Classification |
|---|---|---|
| OK to work | Available / connected | **Healthy** |
| OK to work | Unavailable past grace | **Faulted** |
| Not OK (cable out, rfkill+no profile) | Unavailable | **Inactive** |
| **WiFi: rfkill ON + profiles configured** | Unavailable | **Faulted** (M2 — intent contradicts) |

`ConfigHealth` for WiFi:

| Profile state | NM assoc result | Classification |
|---|---|---|
| Active profile, assoc succeeded, IP obtained | — | **Healthy** |
| Active profile, NM auth-error / no-network | persistent | **Faulted** (AP trigger) |
| No matching profile / no SSID in scan | — | **Inactive** (AP trigger via "no other uplink + HW healthy") |

`Inactive` is non-actionable for HWRecoverer — both signals agree, nothing wrong. Eth-no-cable, wifi-rfkill-by-intent, wifi-no-profile.

---

## Decider rules

### HWRecoverer (per-device, pure)

```
# F-B1 quiet period: post-action grace window (5 ticks ≈ 2.5min) — observations
# during this window do NOT advance N-of-M counters and decideHW returns Wait.
# Anchored on ledger.LastBounceAt[dev]. Probe layer enforces N-of-M suppression.

# B4-rev gate: both Bounce AND Reboot honor tick.SystemBusy (covers install_disk,
# transactional-update). Bounces during install_disk could disrupt the install.
# N-B3: read from tick (data), not from sys.SystemBusy() (interface call) —
# closes mid-tick TOCTOU between deciders.
if tick.SystemBusy: return Wait

# Per-device action ladder (S5-rev: HW evaluated before AP within a tick)
if obs.HWHealth == Faulted AND past-grace:
    if tick.L3Probe == Down:                             # primary L3-truth (B5-rev: TCP-connect probe, NOT NMConn)
        if ledger.bounces(dev, 1h) < 3:  return Bounce(dev)
        if ledger.reboots(2h)      < 1:  return Reboot   # SystemActuator acquires reboot lock (F-S9)
        return Wait                                       # budget refills naturally
    return Wait                                           # L3 is up; don't bounce HW

elif obs.RfkillHard:                                      # S6-rev: hardware kill switch — unblock impossible
    return Wait                                           # Snapshot.Hint = "physical wifi switch off"

else:
    return Wait
```

`Wait` is `Nothing` semantically, but framed as "still trying, rate-limited". There is no `GiveUp`. `past-grace` means `tick.SystemUptime >= 60s` (S1-rev: system uptime, not process uptime).

### APArbiter (cross-device, pure)

```
if tick.SystemBusy: return APUnchanged                   # B4-rev: don't toggle AP mid-install (N-B3 read from tick)

# Exit is unconditional when any uplink works — never suppressed by toggle budget (S2-rev).
if intent.ForceAP:                                        return APEnter
if intent.SuppressAP:                                     return APExit
if anyDeviceGwReachable(tick):                            return APExit

wifiObs := tick.Devices[WiFi]
configBroken := wifiObs.ConfigHealth == Faulted OR
                wifiObs.ConfigHealth == Inactive    # no profile case
wifiHWHealthy := wifiObs.HWHealth == Healthy

if wifiHWHealthy AND configBroken AND apEntryPermitted(ledger):
    return APEnter
return APUnchanged   # AP impossible when wifi HW is Faulted; Snapshot.Hint surfaces the cause
```

`apEntryPermitted(ledger)` (S2-rev / F-S14 fix): returns true iff `(count(toggles in last 1h) < 4) OR (now − last_toggle_at > 10min)`. Worst-case lockout window is the 10-minute cooldown — never the full hour. Exit is always permitted unconditionally (handled by the earlier `anyDeviceGwReachable` exit branch). Owner is never permanently locked out of AP recovery.

### Snapshot construction (mechanical, no decider)

After `decideHW` and `decideAP` complete, the orchestrator constructs a fresh `Supervisor.Snapshot()` from `(Tick, Ledger, UserIntent, SystemState, hwActions, apAction)`. Status enum per device:

- `Healthy` — proved working
- `Recovering` — bounce or reboot just initiated, in quiet period
- `WedgedAwaitingBudget` — Faulted, budget temporarily depleted
- `WedgedAwaitingPrecondition` — Faulted, blocked on `SystemBusy()` (onboarding install_disk)
- `EnvSuspected` — HW Healthy + L3 dead persistent
- `Inactive` — no profile / no cable / rfkill-by-intent / hard rfkill / AP-impossible — `Snapshot.Hint` carries the specific cause when one applies

User-facing AP context (forced-by-user vs recovery vs onboarding) lives in `Snapshot.APMode.Reason`, read by captive portal HTML for narrative copy. The DeviceStatus enum stays minimal.

Snapshot writes are atomic (whole-snapshot RWMutex swap, never partial); readers see consistent state.

---

## Probes (input layer — what each device kind exposes)

### WiFi probe (`probe_wifi.go`)
- NM `Device.State` and `StateReason`
- `/sys/class/rfkill/rfkill*/type` (`wlan`) + `soft` + `hard` files (S6-rev: distinguishes physical kill switch from soft-block — only soft is unblock-recoverable)
- NM `Settings` → profiles configured for this device (presence/absence)
- `nmclient.Scan(device)` → BSSID list (HW healthy ⇔ ≥1 BSS or NM Available)
- N-of-M dampening: 3-of-3 ticks before flipping to Faulted

### Ethernet probe (`probe_eth.go`)
- NM `Device.State` and `StateReason`
- `/sys/class/net/<iface>/carrier` → L1 link
- N-of-M dampening: 3-of-3

### L3 probe (`probe_l3.go`) — B5-rev: piccolod-owned, NMConn becomes advisory
- **Primary L3-truth (`L3Probe`):** TCP-connect to a small fixed list of well-known IPs (`8.8.8.8:53`, `1.1.1.1:53`) with 2s timeout. Result: `Up` / `Down`. Cheap, deterministic, freshness guaranteed at tick cadence — does not depend on NM's `connectivity-check-interval` (which clamps at 60s and triggers external-host externalities, F-S3 / F-S8).
- **Per-device `GwReachable`:** ARP probe to default-route gateway via in-process arping equivalent (or netlink) — proves L2-to-router.
- **Advisory `NMConn`:** read NM's `Connectivity` property as it stands; used for diagnostic detail only (Snapshot.Connectivity field), NOT for decider input.
- **N-of-M dampening on `L3Probe` (S4-rev):** require 3-of-3 consecutive `Down` ticks before deciders treat L3 as Down. Avoids transient flap fooling the M3 false-positive guard.

### Probe constants
- **Tick interval:** 30s
- **Cold-boot grace:** 60s, sourced from **system uptime** via `/proc/uptime` (S1-rev: NOT process uptime — piccolod restart doesn't reset grace if system has been up for hours)
- **N-of-M (HW + L3):** 3-of-3
- **Post-action quiet period (F-B1, F-S13 fix):** 3 ticks (~90s) after any Bounce — N-of-M counters do not advance for that device during this window. Reduced from iter-2's 5 ticks: bounce settles within 5–10s in practice; 90s of suppression is sufficient and the worst-case-cluster latency drops accordingly.
- **Action latency:**
  - Steady-state: ~90s after fault first observed (3-of-3 ticks)
  - Worst-case post-bounce cluster (F-S13 honest framing): ~3min (90s quiet period + 90s N-of-M for the new fault)
- **Signal-triggered probes (F-S6):** NM D-Bus signals only refresh latest snapshot; deciders consume strictly tick-aligned snapshots; signals never advance N-of-M counters mid-tick

---

## Actuators (output layer — side-effect surface)

### `DeviceActuator` (per-device bounce mechanism — M5)

```go
type DeviceActuator interface {
    Bounce(ctx context.Context, dev DeviceKind) error
}
```

Dispatch by kind:
- **WiFi**: `rfkill unblock wifi` → `nmcli radio wifi off` → 2s sleep → `nmcli radio wifi on` (subsumes M2)
- **Ethernet**: `nmcli device disconnect <iface>` → 2s sleep → `nmcli device connect <iface>`

Holds a per-device mutex so AP-mode entry cannot race with WiFi bounce.

### `APModeActuator`

```go
type APModeActuator interface {
    Enter(ctx context.Context) error
    Exit(ctx context.Context) error
    Active() bool
}
```

Wraps existing AP machinery (`internal/network/ap/manager.go::Start/Stop`). Records every transition in `ledger.APToggles` for `apToggleStable()` budget enforcement.

### `SystemActuator` (reboot)

```go
type SystemActuator interface {
    Reboot(ctx context.Context, reason string) error
}
```

Preconditions:

1. `ledger.reboots(2h) < 1` — checked before call site
2. `sys.SystemBusy() == false` — checked at call site; if busy, return `ErrRebootDeferred` (not a hard failure) and emit `WedgedAwaitingPrecondition` status. Orchestrator re-evaluates next tick.
3. `systemctl reboot`

The microsecond-scale TOCTOU between step 2 and step 3 is bounded by transactional-update's own integrity model — transactional-update is restart-tolerant by design (build new btrfs snapshot → mark default → reboot to finalize). Reboot during a transactional-update mid-flight at worst causes the update to be re-tried on next reboot; it does not corrupt state. **No reboot-lock primitive is added to the design.**

**`SystemBusy()` sources** — see `SystemState` interface above. Concrete: `TopicOnboardingStateChanged` subscription + `update.Manager.Status(ctx)` query. No fictional topics.

**Bounce gating:** `Bounce` also honors `SystemBusy()` in `decideHW`, before any HWAction is returned. A WiFi bounce during the onboarding install_disk phase could disrupt the in-flight install. Today's `watchdog.go` suppresses both bounce and reboot during onboarding; the new design preserves that via the same gate. Note: bouncing wifi during a transactional-update fetch is *safe* — fetch is resumable — so `SystemBusy()` reason is exposed and the gate could differentiate later if needed (currently treats all busy reasons identically).

---

## Surface — three on-demand readers, no fan-out

**Governing principle:** if connectivity is broken, the UI is unreachable; if connectivity is alive, transient internal recovery is not actionable by the owner. The "live status banner in main UI" concept solves a non-problem. Only three surfaces matter:

| Surface | Reachable when | Purpose | Mechanism |
|---|---|---|---|
| **journald** | Always (local disk) | Forensic trail, post-mortem diagnosis | `slog.WithGroup("net.supervisor")` — synchronous |
| **Captive portal HTML** | AP mode active | The *only* actionable surface — owner is here BECAUSE something needs action | Captive portal reads `Supervisor.Snapshot()` directly |
| **mDNS TXT field** | L2 alive on at least one device | Passive discovery hint to LAN devices | mDNS TXT updater reads `Supervisor.Snapshot()` periodically |

**No event bus, no fan-out, no subscribers, no `LifecycleCoordinator` extension, no namek channel.** Existing `/api/wifi/status` endpoint preserves wire contract via `deriveLegacyState` (see Decider rules below); the diagnostics page in the main UI reads it for the curious owner — never as a banner alert.

`Supervisor.Snapshot()` exposes the current state read from the latest tick:

```go
type Snapshot struct {
    Devices       map[DeviceKind]DeviceStatus  // status enum + last-action timestamp + reason
    ActiveUplink  UplinkType                    // ethernet / wifi / none (UplinkAP not exposed —
                                                // AP mode maps to UplinkNone per current wire contract)
    APMode        APModeInfo                    // active + reason (recovery / user-forced / setup)
    Connectivity  Connectivity                  // full / limited / portal / none
    Hint          string                        // owner-actionable hint when one applies
}

type DeviceStatus int  // Healthy | Recovering | WedgedAwaitingBudget |
                       // WedgedAwaitingPrecondition | EnvSuspected | Inactive
```

**Snapshot construction order (single-shot per tick, end-of-tick):**

```
for each tick:
    1. probe       → Tick (fresh observations, ActiveUplink derivation,
                          SystemBusy read from SystemState)
    2. decideHW    → HWAction[]                (per device; HW first)
    3. decideAP    → APAction                  (cross-device, after HW)
    4. actuate     → orchestrator writes ledger entries (LastBounceAt,
                     Bounces, Reboots) synchronously, THEN spawns actuator
                     goroutines for the side-effecting nmcli calls. Ledger
                     write before goroutine spawn ensures step 5's snapshot
                     reflects this tick's actions; goroutine outcome is
                     observed by next tick's probe.
    5. construct   → Snapshot from (Tick + Ledger + UserIntent + system state)
    6. publish     → atomic RWMutex swap of Supervisor.snapshot
```

Snapshot is built and published exactly once per tick. Readers see consistent whole-snapshot state — never partial. **Out-of-tick mutations to the snapshot are not permitted.** Signal-triggered probes refresh the latest tick's working observations; they do not republish the snapshot.

**DeviceStatus resolution priority:**

```
# Quiet-period gate wins regardless of current probe — prevents Healthy→Recovering→Healthy flicker
if (now - ledger.LastBounceAt[dev]) < quiet_period:  status = Recovering

# HW Faulted ladder
elif obs.HWHealth == Faulted AND tick.SystemBusy:    status = WedgedAwaitingPrecondition
elif obs.HWHealth == Faulted:                         status = WedgedAwaitingBudget

# HW Healthy but L3 dead — env-class
elif obs.HWHealth == Healthy AND obs.GwReachable == False AND tick.L3Probe == Down:
                                                      status = EnvSuspected

# HW Healthy, Config unhealthy — pre-AP-handover narrative
elif obs.HWHealth == Healthy AND obs.ConfigHealth == Faulted AND dev == WiFi:
                                                      status = WedgedAwaitingBudget

# Inactive — no profile / no cable / rfkill (soft or hard) / AP impossible.
# Snapshot.Hint carries the specific cause: "physical wifi switch off",
# "no wifi profile configured", "supervisor unhealthy — see journald", etc.
elif obs.HWHealth == Inactive OR obs.RfkillHard:      status = Inactive

else:                                                 status = Healthy
```

`Recovering` means "supervisor has initiated a bounce within the quiet period; outcome observable on next tick." It does not promise the bounce will succeed.

`Snapshot.Hint` is a single string. At most one of these applies at a time; if multiple are technically possible, the orchestrator picks the most specific:
- `"physical wifi switch off"` — rfkill hard-blocked
- `"supervisor unhealthy — see journald"` — covers all supervisor-internal failure modes (dbus disconnected, etc.) since they all route the owner to the same forensic channel
- (no Hint when status fully describes state)

`decideSurface` is dropped as a separate decider — Snapshot construction is mechanical from inputs, no judgment required.

**Supervisor external API (F-S15 + N-S5/F3-S3 fix — field-level setters, no clobber):**

```go
type Supervisor interface {
    Snapshot() Snapshot                        // O(1) read from atomic-swap pointer

    // Async signals from external callers (captive portal credential entry, UI Force-AP toggle).
    // Non-blocking — buffered chan size 1, drop-if-pending semantics.
    // Effects observable on the *next* tick boundary (≤30s by tick interval).
    //
    // N4-S2 (red-team) ack: dropped requests do NOT lose intent state. Intent
    // lives on intentMu-protected fields (SetForceAP/SetSuppressAP), which are
    // unconditionally read by the next tick regardless of how many request
    // signals were coalesced. The chan only signals "wake the tick early";
    // a missed wake-up degrades to "wait up to 30s for the periodic tick" —
    // bounded, not lost.
    RequestProbeAndDecide()                    // hint: run a tick early; orchestrator may coalesce

    // Field-level intent setters (N-S5 / F3-S3 fix): single SetUserIntent
    // atomic-swap was a footgun — boot-time loader and UI click could
    // silently clobber each other's sibling fields. Each setter mutates
    // exactly one field under a shared intent mutex; no information loss.
    SetForceAP(bool)
    SetSuppressAP(bool)
}
```

Captive portal credential-entry path calls `SetForceAP(false)` then `RequestProbeAndDecide` — the next tick observes the cleared intent and a fresh probe. UI's "force AP" toggle calls `SetForceAP(true)`. UI's "disable AP recovery" toggle calls `SetSuppressAP(true)`. The two toggles compose correctly without clobbering.

---

## Site list

### Sites being deleted (full)

- `internal/network/state.go` — entire `ConnState` machine, `cancelSTA`, `backoffSchedule`, all 7 AP entry call sites:
  - `handleEthernetDown` no-saved-WiFi (around line 155)
  - `handleWiFiDisconnected` no-profile (around line 197)
  - `handleReconnectResult` ceiling failures (around line 235)
  - `handleAuthFailure` (around line 256) — auth-failure classification logic moves to `probe_wifi.go` to populate `ConfigHealth=Faulted` (B6-rev: not lost, relocated)
  - `Manager.ForgetNetwork` (around line 423) — preserved via `UserIntent` propagation path
  - `Manager.ForceAPMode` (around line 496) — preserved as `UserIntent.ForceAP=true`
  - `determineInitialState` (entire function around lines 768-817) — replaced by orchestrator's first-tick probe + decide; no special "initial state" pre-computation
- `internal/network/manager.go::handleAPTransition` (around line 509), `apRetryActive` flag, `revertAPState` (around line 698)

### Sites being rewritten (B6-rev: previously-missed sites enumerated)

- `internal/network/watchdog.go` → becomes `internal/network/decide_hw.go`. The silent `gateway==""` early-return ceases to exist (`Inactive` is a first-class case).
- `internal/mdns/interface.go::checkInterfaceChanges` (around line 395) — rewrite: treat IP-loss as transient. "Sustained loss" defined as **3-of-3 ticks at the existing 10s networkMonitor cadence** (=30s). Legitimate IP changes (DHCP renewal / network move) disambiguated by the kernel `RTM_DELLINK` / interface-down signal — only act when interface itself goes down, not when IP merely changes. Interplay with `mdns/resilience.go::performHealthCheck` is **not addressed in this plan** — captured as deferred.
- `internal/network/signal.go::signalMonitor.poll` (around line 60) — replace `sm.Current() == StateWiFiSTA` gate with `tick.Devices[WiFi].ConfigHealth == Healthy`. Signal-strength polling continues unchanged; gating logic is the only delta.
- `internal/network/manager.go::nmStateLoop` + `handleWifiStateChanges` + `handleEthStateChanges` (lines ~884-946) — collapse the 60-line NM-state-translation block to a thin "request immediate probe re-run" trigger. **Auth-failure classification** (NoSecrets / SupplicantConfigFailed / SupplicantFailed / SupplicantTimeout — currently routed through `handleAuthFailure`) moves into `probe_wifi.go`'s `ConfigHealth` derivation logic. The Failed(120) vs Disconnected(30) double-counting comment becomes obsolete (no transition counter anymore).
- `internal/network/manager.go::connectFromCaptivePortal` — currently reads `m.state.current` and writes `transition(StateAPMode, ...)`. Rewrite to invoke `Supervisor.SetUserIntent(UserIntent{ForceAP: false}); Supervisor.RequestProbeAndDecide()` after successfully applying the credential (F-S15 named API). Captive portal flow exits AP via the next normal tick.
- `internal/network/manager.go::determineInitialState` (lines 768-817) — fully deleted (called out above under "Sites being deleted").
- `internal/network/manager.go::APStatus` (lines 431-444) — currently reads `m.state.Current() == StateAPMode`. Rewrite to read `Supervisor.Snapshot().APMode.Active`.
- `internal/network/manager.go::Status` (lines 187-225) — wire contract endpoint. Rewrite to construct `Status{State: deriveLegacyState(snapshot, apActive), Uplink: snapshot.ActiveUplink, ...}` from `Supervisor.Snapshot()`. Flutter wire contract preserved (B3-rev).
- `internal/network/manager.go::Start` `onTransition` callback (lines 89-101) — currently maps `ConnState → health.Tracker` levels. Rewrite to subscribe to Supervisor state-change notifications and update `health.Tracker` from `Snapshot.DeviceStatus + Connectivity`. **The full `health.Tracker` collapse vs parallel-feed decision is deferred** (A2-rev).

### Sites surviving (consumed by new code, no rewrite)

- `internal/network/nmclient/device.go::Scan` (around line 56) — used by WiFi HW probe unchanged.
- `internal/network/nmclient/signals.go` — D-Bus subscription survives. **Probe layer uses a separate `*nmclient.DBusClient` instance** (built on its own `dbus.Conn`, constructed in `cmd/piccolod/main.go`) to avoid godbus's connection-wide signal cross-talk with `manager.go`'s legacy subscribers — F3-B1 fix. Signals trigger immediate snapshot refresh; signals do NOT advance N-of-M counters mid-tick (F-S6).
- `internal/network/ap/manager.go::Start/Stop` — wrapped by new `APModeActuator`, no changes to the AP machinery itself.
- `internal/lifecycle/coordinator.go` — **untouched** (the prior plan's "extend with NetworkSurface field" was wrong; LifecycleCoordinator stays single-purpose for crypto-unlock readiness).
- Existing mDNS TXT publication code — gains `status=<DeviceStatus enum>` field by reading `Supervisor.Snapshot()` directly. No subscription, no event bus — periodic read on existing TXT-update cadence.
- Existing captive portal HTML template — reads `Supervisor.Snapshot()` directly. Copy is driven by `Snapshot.APMode.Reason` (e.g. `"user-forced"` → "you forced AP — release here") and `Snapshot.Hint` (e.g. "physical wifi switch off"). New copy for `WedgedAwaitingPrecondition` (onboarding install_disk in flight).
- Existing `events.Bus` topics — none added, none removed. `TopicOnboardingStateChanged` is consumed by `SystemState.SystemBusy()` source.

### Site explicitly NOT touched (catalog completeness — F-S7)

- Multi-device-per-kind (USB WiFi dongle as second `wlan*`, USB ethernet as second `eth*`) — declared single-device-per-kind constraint: probe layer picks the first NM-managed device of each kind, additional devices ignored. Catalog entry **A8** added. Re-scope if production hardware demands it.

### New files

| File | Approx LoC | Responsibility |
|---|---|---|
| `internal/network/observation.go` | ~80 | `Tri`, `DeviceKind`, `DeviceObservation`, `Tick` (incl. `ActiveUplink`), `L3ProbeResult`, `Connectivity` |
| `internal/network/probe.go` | ~120 | Top-level probe coordination, signal-trigger refresh (signals never advance N-of-M counters mid-tick), `ActiveUplink` derivation (priority-based) |
| `internal/network/probe_wifi.go` | ~180 | WiFi probe + rfkill soft/hard distinction + auth-failure classification (relocated from `manager.go::handleAuthFailure`) |
| `internal/network/probe_eth.go` | ~80 | Ethernet probe (NM, carrier) |
| `internal/network/probe_l3.go` | ~120 | ARP gateway probe + TCP-connect L3 probe to `8.8.8.8:53` / `1.1.1.1:53` + NMConn advisory read |
| `internal/network/ledger.go` | ~180 | `ActionLedger`, persistent/volatile split, fail-open-on-missing-volatile / fail-closed-on-corrupt, legacy `net-watchdog-reboots` migration |
| `internal/network/decide_hw.go` | ~120 | `decideHW` pure with quiet period (3 ticks ≈ 90s) and SystemBusy gate |
| `internal/network/decide_ap.go` | ~100 | `decideAP` pure with rate-shaped re-entry — `(count<4 in 1h) OR (now − last_toggle > 10min)` |
| `internal/network/snapshot.go` | ~100 | `Snapshot` construction + atomic swap, Status enum, `deriveLegacyState` |
| `internal/network/actuator.go` | ~200 | `DeviceActuator` per-kind dispatch, `APModeActuator`, `SystemActuator` (advisory `SystemBusy` check, no reboot-lock primitive) |
| `internal/network/orchestrator.go` | ~180 | Tick loop, deciders evaluated HW-first then AP, end-of-tick Snapshot publication, `Supervisor` external API (`RequestProbeAndDecide`, `SetForceAP`, `SetSuppressAP`) |
| `internal/network/system_state.go` | ~30 | `SystemState` interface + concrete impl: subscribes to `TopicOnboardingStateChanged`, caches latest phase under mutex. `SystemBusy()` returns true iff phase ∈ {pending, install_disk}. |

### State files

- `/var/lib/piccolo/net-ledger.json` (persistent) — `Reboots` only.
- `/run/piccolo/net-ledger-volatile.json` (volatile) — `Bounces`, `APToggles`, `LastBounceAt`.
- `/var/lib/piccolo/net-watchdog-reboots` (legacy) — **migrated** (not discarded — S8-rev) at first orchestrator start: each `unix-timestamp\n` line becomes a `Reboots` entry in the new ledger; legacy file deleted. Preserves "1 reboot per 2h" budget across the cutover.

### Ledger startup semantics (F-B2 fix — fail-closed for adversarial paths only; F-B4 fix — fail-open for steady-state reboot)

Orchestrator startup (`ledger.go::Load`):

1. **Load `/var/lib/piccolo/net-ledger.json`** (persistent — survives boot):
   - If file is corrupt: log error, **fail-closed**: `Reboots` initialized to `[now()]` (effectively "we just rebooted") so the 2h budget is exhausted from start. Prevents post-crash reboot loops.
   - If file is missing: empty `Reboots` (first-ever startup).
   - If file is present and valid: load as-is.

2. **Load `/run/piccolo/net-ledger-volatile.json`** (volatile — tmpfs-cleared on every boot, F-B4 fix):
   - **If file is missing** (the steady-state post-clean-reboot path — tmpfs is always cleared on boot): **fail-open**, empty `Bounces` and `APToggles`. The persistent reboot ledger still caps cascading harm at 1 reboot/2h. Treating "missing volatile" as adversarial would suppress bounce budget for 1h after every clean reboot — a fleet-wide post-deploy outage surface (rfc-red-team F-B4).
   - **If file is present but corrupt**: **fail-closed** — pre-populate `Bounces[dev]` with 3 synthetic timestamps anchored at `now()` for each device. Corruption is genuinely adversarial (signals crash-during-write or disk fault).
   - **If file is present and valid**: load as-is.

3. **Optional refinement** (deliberately not blocking iter-3): if persistent `Reboots` shows a recent timestamp (< 5 minutes ago) AND volatile is missing, treat as crash-mid-bounce-after-reboot path; fail-closed instead of fail-open. This handles the edge case where a panic immediately post-reboot would otherwise see fresh budget. Add only if soak validation surfaces the pathology.

4. **Migrate legacy `net-watchdog-reboots`** if present (one-shot): each `unix-timestamp\n` line becomes a `Reboots` entry; delete legacy file. Preserves reboot budget across cutover (S8-rev).

5. **Ledger writes happen on action *initiation*, not completion** — F-B2: a crash mid-bounce still consumes a budget slot. Idempotent retry is the goal; double-counting from race is the lesser evil.

---

## Stages (big-bang cutover, staged review)

**Wiring discipline (F-S12 / N-S4 fix):** Stages 1-3 land truly observer-only. The probe loop is started from `cmd/piccolod/main.go` (NOT from `manager.go`) as `Supervisor.Run(ctx)`. Probe outputs are discarded until Stage 4. Reverting Stage 4 alone restores prior behavior cleanly; Stages 1-3 code becomes dormant.

**D-Bus connection isolation (F3-B1 fix + N4-S2 lifecycle):** the supervisor's probe layer constructs its own `*nmclient.DBusClient` against a **separate `dbus.Conn`** — NOT the shared `dbus.SystemBus()` singleton (which would re-introduce the cross-talk because godbus's `c.conn.Signal(sigCh)` is connection-wide).

Specifically:
- **Connection construction**: use `dbus.SystemBusPrivate()` followed by explicit `Auth(nil)` + `Hello()`. The shared `dbus.SystemBus()` returns a process-singleton; `SystemBusPrivate` is the only path to a truly independent connection. **This distinction is load-bearing** — without it, the F3-B1 fix is a no-op and signal cross-talk recurs. If construction fails at startup, piccolod fails fast — systemd handles restart via the existing unit dependency on dbus.service.
- **Disconnect recovery**: a per-connection watchdog goroutine monitors `conn.Disconnected()` (godbus's connection-loss notification). On disconnect: backoff-reconnect (1s, 2s, 4s, capped at 30s); during the disconnected window, all device probes return `HWHealth=Inactive` (probe layer cannot observe → cannot fault) and Snapshot.Hint = `"supervisor unhealthy — see journald"`. This surfaces to journald rather than silently freezing observations.
- **Ownership**: `Supervisor.Run(ctx)` owns the connection lifecycle. On `ctx.Done()`, the supervisor calls `conn.Close()` cleanly. No package-level singletons.
- **Construction site**: in `cmd/piccolod/main.go` next to `Supervisor.Run(ctx)`. The supervisor's `*nmclient.DBusClient` is constructed once at startup and passed in; the probe layer does not construct it itself.

| Stage | Deliverable | Reviewable as |
|---|---|---|
| 1 | `observation.go` + `probe*.go` + `system_state.go` + `ledger.go` — observer-only `Supervisor.Run(ctx)` started from `cmd/piccolod/main.go`; outputs discarded | Pure data; unit-tested with NM mocks. Verify Tick output across 6 representative scenarios (A1, B1, C3, D2, F2, G1). |
| 2 | `decide_hw.go` + `decide_ap.go` + `snapshot.go` — pure functions + Snapshot construction + `deriveLegacyState` | Full catalog (41 scenarios) as parameterized unit tests over `(Tick, Ledger, Intent) → (Actions, Snapshot)`. |
| 3 | `actuator.go` — `DeviceActuator` per-kind bounce, `APModeActuator`, `SystemActuator` | Integration tests on dev VM. Verify per-kind bounce (M5), reboot precondition (`SystemBusy()` gate via `update.Manager.Status` + onboarding state), per-device mutex against AP entry. |
| 4 | `orchestrator.go` cutover — delete `state.go`, `watchdog.go`, `handleAPTransition`, `apRetryActive`, `revertAPState`, `determineInitialState`; rewrite `manager.go::Status / APStatus / Start.onTransition`; wire mDNS TXT and captive portal to read `Supervisor.Snapshot()` | Big-bang commit. Revert is single-commit. End-to-end soak on dev VM with simulated A1/D2/F2/G1. |
| 5 | `mdns/interface.go` rewrite — IP-loss tolerance (3-of-3 ticks at 10s cadence + interface-down kernel signal disambiguation) | Unit tests with simulated interface flap. Verify no 10s reconfig storm. |
| 6 | End-to-end validation against full catalog on real HW (HP laptop with rtw88_8723de + RPi with brcmfmac) | Manual + integration. |

Stages 1-3 land additively without touching `manager.go`. Stage 4 is the cutover. Stages 5-6 finalize.

---

## Migration / rollback

- **Stages 1-3 land truly observer-only** (S9-rev / F-S12): `Supervisor.Run(ctx)` is started from `cmd/piccolod/main.go`, not from `manager.go`. No `manager.go` wiring, no Snapshot consumers, no actuator calls until Stage 4. Stage 4 is the only commit that switches the control plane.
- **Stage 4 cutover commit**: deletes `state.go`/`watchdog.go`/affected `manager.go` symbols, adds Snapshot consumers (mDNS TXT, captive portal, `manager.go::Status`+`APStatus`), wires orchestrator into `manager.go`. Revert is single-commit.
- **Legacy reboot-history migration** (S8-rev): MIGRATE — one-shot read of `/var/lib/piccolo/net-watchdog-reboots` (each line is a unix timestamp), merge timestamps into `Reboots` in `net-ledger.json`, delete legacy file. Preserves reboot budget across cutover. If migration fails (parse error / missing file), log warning and proceed with empty history (acceptable: reboot budget naturally caps risk at 1 reboot in next 2h).
- **NMConn cadence**: NMConn is advisory only — not used as decider input. The TCP-connect probe (in `probe_l3.go`) is the primary L3-truth source. No NM config change required.

---

## Risks / unknowns

1. **TCP-connect probe externalities** — probing `8.8.8.8:53` and `1.1.1.1:53` from thousands of appliances is unsolicited but at 30s cadence with 2s timeout it's well within normal client traffic profiles. Targets are public DNS — same as any standard glibc resolver. No NM external-probe externality.
2. **AP-toggle re-entry semantics with G1** — exit always permitted; re-entry rate-shaped at 4/h OR 10min cooldown (S2-rev). Verify on dev VM that a flapping eth can't lock the device out of AP recovery indefinitely.
3. **N-of-M with rare devices that flap intentionally** — some kernel drivers transient-Unavailable during regulatory domain changes / channel hops. 3-of-3 over 90s should absorb; needs validation on real HW.
4. **rfkill state source consistency** — `/sys/class/rfkill/rfkill*/{soft,hard,type}` is the canonical kernel surface; `nmcli radio wifi` is derived. Probe reads sysfs only.
5. **Catalog completeness** — 41 scenarios from one real incident + reasoned enumeration + reviewer audit. Rare combinations may surface during validation. Adding new tests is cheap (parameterized over decider signatures).
6. **mDNS `checkInterfaceChanges` rewrite scope** — verify the storm fix doesn't regress legitimate interface re-add scenarios (e.g., USB ethernet hot-plug on Pi). Interplay with `mdns/resilience.go::performHealthCheck` is **deferred** (S3-rev): captured as `deferred_mdns_resilience_collapse.md`.
7. **`health.Tracker` collapse vs parallel-feed** — A2-rev. `manager.go::Start` `onTransition` callback drives `health.Tracker`. New design subscribes to Snapshot updates and feeds `health.Tracker` from `(DeviceStatus, Connectivity)`. Whether to ultimately delete `health.Tracker` in favor of Snapshot-only is **deferred** — captured as `deferred_health_tracker_collapse.md`.

8. **`Tick.ActiveUplink` loose predicate trade-off (N4-A1 ack):** the predicate `HWHealth==Healthy AND LinkUp` (no `GwReachable` requirement) is loose — eth with carrier=1 but no DHCP/no upstream still shows `ActiveUplink=ethernet`. Snapshot.Connectivity field correctly tells the truth (`none`/`limited`); UI consumers that need reachability-aware status read `Connectivity` not `ActiveUplink`. The trade-off is "no one-tick `none` flicker on transient gateway-ARP loss" vs "honest activeness during persistent gateway loss". Loose form chosen to match current `state.go` semantics; revisit if user-visible regression observed.

9. **Step 4a sync ledger write to tmpfs (N4-A2 ack):** `/run/piccolo/net-ledger-volatile.json` write is synchronous before snapshot construction. Under tmpfs ENOSPC or EIO, the write fails. Behavior: log error, continue with in-memory ledger update only — the in-memory state is the source of truth for the current tick; persistence is best-effort. A subsequent successful tick will rewrite the file. Permanent tmpfs failure is its own incident-level concern outside this scope.

10. **dbus connection-count pressure (N4-A4 ack):** the supervisor adds one private `dbus.Conn` per piccolod instance (in addition to manager.go's connection). dbus-daemon's default `max_connections_per_user=256` is well above piccolod's steady-state count (4-6); even pathological crash-loop scenarios remain comfortably below the limit. Pattern should NOT become a default for new subsystems without revisiting this assumption.

---

## What this collapses (relative to current code)

- 7 AP entry paths → 1 APArbiter rule
- Procedural watchdog with silent early-returns → 1 pure decider with no early-return
- ConnState machine + `cancelSTA` + backoff schedule → stateless tick loop, wire contract preserved via `deriveLegacyState`
- Multiple ad-hoc state files (`net-watchdog-failures`, `net-watchdog-recoveries`, `net-watchdog-last-action`, `net-watchdog-reboots`) → 1 split ActionLedger
- 7 implicit "what should we do here?" sites in `state.go` transitions → consolidated into 2 explicit deciders + mechanical Snapshot construction
- "Live banner alert in main UI" surfacing → eliminated entirely. Three on-demand readers (journald / captive portal / mDNS TXT) replace the imagined event-bus fan-out. **Governing realization: if connectivity is broken, UI is unreachable; if connectivity is alive, transient internal recovery is not actionable by the owner.**

The 13-day silent failure was a missing-tick-evaluation bug. This design has no equivalent because every tick re-evaluates from fresh observations and the only persistent state is *what we did*, not *what we believed*.
