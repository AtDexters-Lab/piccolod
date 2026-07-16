package network

import (
	"context"
	"net"
	"time"

	"piccolod/internal/network/nmclient"
)

// probeL3 performs the TCP-connect probe and per-device gateway ARP probe.
// Returns L3ProbeResult and the per-device GwReachable map. Run after device
// probes so it knows which interfaces have IP.
//
// L3 probe targets are well-known DNS resolver IPs reached on TCP/53.
var l3ProbeTargets = []string{"8.8.8.8:53", "1.1.1.1:53"}

const l3ProbeTimeout = 2 * time.Second

type gatewayProbeKey struct {
	iface   string
	gateway string
}

type gatewayProbeResult struct {
	key       gatewayProbeKey
	reachable Tri
	probed    bool
}

func (p *Prober) probeL3(ctx context.Context, devices map[DeviceKind]DeviceObservation, selected gatewayProbeResult) (L3ProbeResult, map[DeviceKind]Tri) {
	// TCP-connect probe — sequential dials, first success wins. Parent
	// budget MUST cover the sum of per-target attempts so a silently-
	// blackholed first target (corp guest WiFi DROP, regional blocks —
	// catalog C9) does not strangle the fallback's full timeout. Without
	// this, the second target gets only ~250ms after the first 2s elapses,
	// producing false L3 Down (and after dampening, spurious WiFi bounce).
	probeCtx, cancel := context.WithTimeout(ctx,
		time.Duration(len(l3ProbeTargets))*l3ProbeTimeout+250*time.Millisecond)
	defer cancel()
	tcpUp := tcpConnectAny(probeCtx, l3ProbeTargets, l3ProbeTimeout)

	gw := make(map[DeviceKind]Tri, len(devices))
	for kind, obs := range devices {
		gw[kind] = p.probeGateway(ctx, obs, selected)
	}

	if tcpUp {
		return L3ProbeUp, gw
	}
	return L3ProbeDown, gw
}

// tcpConnectAny tries each target sequentially; returns true on first success.
// Honors per-attempt timeout and parent ctx cancellation.
//
// Test seam: tests override this to simulate L3 reachability.
var tcpConnectAny = func(ctx context.Context, targets []string, perAttempt time.Duration) bool {
	dialer := net.Dialer{Timeout: perAttempt}
	for _, t := range targets {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		conn, err := dialer.DialContext(ctx, "tcp", t)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

// probeGateway runs an in-process ARP probe to the device's NM-reported
// gateway. Reads the gateway from NM's IP4Config — if the device has no
// active connection or no gateway, returns TriInactive.
//
// Stage 1 implementation: shells out to `arping -c 1 -w 2 -I <iface> <gw>`
// via the runner. The full netlink/in-process arping equivalent is a
// follow-up; arping is universally available on piccolo OS.
func (p *Prober) probeGateway(ctx context.Context, obs DeviceObservation, selected gatewayProbeResult) Tri {
	if !obs.Present || !obs.LinkUp || obs.Iface == "" {
		return TriInactive
	}
	gw := p.deviceGateway(obs.Kind)
	if gw == "" {
		return TriInactive
	}
	key := gatewayProbeKey{iface: obs.Iface, gateway: gw}
	if selected.probed && selected.key == key {
		return selected.reachable
	}
	return p.probeGatewayAddress(ctx, key)
}

func (p *Prober) probeGatewayAddress(ctx context.Context, key gatewayProbeKey) Tri {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := p.runner.Run(probeCtx, "arping", "-c", "1", "-w", "2", "-I", key.iface, key.gateway); err != nil {
		// arping returns non-zero on no reply — treat as unreachable.
		// Don't distinguish "command failed" from "no reply"; both mean we
		// can't confirm L2 reachability.
		return TriFaulted
	}
	return TriHealthy
}

// deviceGateway reads the NM-reported gateway for a device kind. Returns ""
// if no active connection or no gateway. Reads p.devicePath under p.mu —
// even though all current callers run in the same goroutine as the
// per-device probes that populate the map, locking here is defense-
// in-depth: the actuator-side pathFn callback is on a different goroutine
// and locks too, so any future call site that reads devicePath without
// holding mu would race against it.
func (p *Prober) deviceGateway(kind DeviceKind) string {
	p.mu.Lock()
	path, ok := p.devicePath[kind]
	p.mu.Unlock()
	if !ok {
		return ""
	}
	info, err := p.nm.ActiveConnectionInfo(path)
	if err != nil || info == nil {
		return ""
	}
	return info.IP4Gateway
}

// probeProjectionGateway probes the gateway from the concrete default-route
// interface selected by the completed projection. Its result is the sole
// gateway input to published connectivity.
func (p *Prober) probeProjectionGateway(ctx context.Context, proj transitionProjection) gatewayProbeResult {
	if !proj.interfacesObserved || !proj.defaultRouteObserved || !proj.defaultRouteKnown {
		return gatewayProbeResult{reachable: TriInactive}
	}
	probe, ok := proj.probeForInterface(proj.defaultRouteIface)
	if !ok || probe.unknown || !probe.linkUp || !probe.hasIP || probe.info == nil || probe.info.IP4Gateway == "" {
		return gatewayProbeResult{reachable: TriInactive}
	}
	key := gatewayProbeKey{iface: probe.iface, gateway: probe.info.IP4Gateway}
	return gatewayProbeResult{
		key:       key,
		reachable: p.probeGatewayAddress(ctx, key),
		probed:    true,
	}
}

// connectivityForProjection classifies connectivity only from the concrete
// interface selected by the completed default-route projection. Class-level
// DeviceObservation remains available to recovery decisions, but cannot
// influence published connectivity for a different same-kind interface.
func connectivityForProjection(
	l3 L3ProbeResult,
	nmConn Connectivity,
	proj transitionProjection,
	gwReachable Tri,
) Connectivity {
	if !proj.interfacesObserved || !proj.defaultRouteObserved {
		return ConnectivityUnknown
	}
	if !proj.defaultRouteKnown {
		return ConnectivityNone
	}
	probe, ok := proj.probeForInterface(proj.defaultRouteIface)
	if !ok || probe.unknown || !probe.linkUp || !probe.hasIP || probe.info == nil {
		return ConnectivityUnknown
	}

	return classifyConnectivityForGateway(l3, gwReachable, nmConn)
}

// classifyConnectivityForGateway maps the L3 probe, the selected concrete
// default-route interface's gateway result, and advisory NMConn into the
// published connectivity classification.
//
// Priority:
//   - Full   — L3 up AND selected gateway is reachable
//   - Limited — L3 up AND selected gateway is not confirmed reachable
//   - Portal  — NMConn==Portal (advisory; only shown when L3 is reachable)
//   - None    — L3 down AND selected gateway is not reachable
func classifyConnectivityForGateway(l3 L3ProbeResult, gwReachable Tri, nmConn Connectivity) Connectivity {
	gwHealthy := gwReachable == TriHealthy
	switch {
	case l3 == L3ProbeUp && gwHealthy && nmConn == ConnectivityPortal:
		return ConnectivityPortal
	case l3 == L3ProbeUp && gwHealthy:
		return ConnectivityFull
	case l3 == L3ProbeUp:
		return ConnectivityLimited
	case gwHealthy:
		return ConnectivityLimited
	default:
		return ConnectivityNone
	}
}

// nmConnAdvisory reads NM's advisory connectivity property. Errors translate
// to ConnectivityUnknown.
func nmConnAdvisory(nm nmclient.Client) Connectivity {
	v, err := nm.Connectivity()
	if err != nil {
		return ConnectivityUnknown
	}
	switch v {
	case nmclient.NMConnectivityNone:
		return ConnectivityNone
	case nmclient.NMConnectivityPortal:
		return ConnectivityPortal
	case nmclient.NMConnectivityLimited:
		return ConnectivityLimited
	case nmclient.NMConnectivityFull:
		return ConnectivityFull
	default:
		return ConnectivityUnknown
	}
}

// activeUplinkFor returns the priority-based active uplink. Predicate:
// HWHealth==Healthy AND LinkUp AND HasIP. Priority: ethernet > wifi > none.
// AP mode maps to UplinkNone per existing wire convention.
//
// HasIP is required so that DHCP-in-flight (NMState=IPConfig=70 with no
// address yet) does not produce a false "uplink up" signal: gin_server.go
// triggers identity.NotifyNetworkUp + stun.Trigger on any non-none uplink,
// and we don't want enrollment refreshes mid-DHCP.
//
// Critically, HasIP does NOT blip during transient gateway-ARP loss (it
// reflects NM's lease state, not gateway reachability), so this preserves
// the RFC §"Risks" #8 no-flicker property the loose predicate was meant
// to give us. The flip case codex round-2 P2-A flagged is when LinkUp
// becomes true at IPConfig but HasIP is still false — that's the gap.
func activeUplinkFor(devs map[DeviceKind]DeviceObservation) UplinkType {
	if obs, ok := devs[DeviceEthernet]; ok && obs.HWHealth == TriHealthy && obs.LinkUp && obs.HasIP {
		return UplinkEthernet
	}
	if obs, ok := devs[DeviceWiFi]; ok && obs.HWHealth == TriHealthy && obs.LinkUp && obs.HasIP {
		return UplinkWiFi
	}
	return UplinkNone
}
