package network

import (
	"context"
	"net"
	"strings"
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

func (p *Prober) probeL3(ctx context.Context, devices map[DeviceKind]DeviceObservation) (L3ProbeResult, map[DeviceKind]Tri) {
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
		gw[kind] = p.probeGateway(ctx, obs)
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
func (p *Prober) probeGateway(ctx context.Context, obs DeviceObservation) Tri {
	if !obs.Present || !obs.LinkUp || obs.Iface == "" {
		return TriInactive
	}
	gw := p.deviceGateway(obs.Kind)
	if gw == "" {
		return TriInactive
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := p.runner.Run(probeCtx, "arping", "-c", "1", "-w", "2", "-I", obs.Iface, gw); err != nil {
		// arping returns non-zero on no reply — treat as unreachable.
		// Don't distinguish "command failed" from "no reply"; both mean we
		// can't confirm L2 reachability.
		return TriFaulted
	}
	return TriHealthy
}

// deviceGateway reads the NM-reported gateway for a device kind. Returns ""
// if no active connection or no gateway. Cached at probe construction time
// via devicePath[kind].
func (p *Prober) deviceGateway(kind DeviceKind) string {
	path, ok := p.devicePath[kind]
	if !ok {
		return ""
	}
	info, err := p.nm.ActiveConnectionInfo(path)
	if err != nil || info == nil {
		return ""
	}
	return info.IP4Gateway
}

// classifyConnectivity maps the L3 probe + per-device GwReachable + advisory
// NMConn into a single Connectivity classification used by snapshot readers.
//
// Priority:
//   - Full   — L3 up AND any device GwReachable=Healthy
//   - Limited — L3 up AND no GwReachable=Healthy (rare: ARP-suppressed network)
//   - Portal  — NMConn==Portal (advisory; only shown when L3 is reachable)
//   - None    — L3 down AND no GwReachable=Healthy
func classifyConnectivity(l3 L3ProbeResult, devs map[DeviceKind]DeviceObservation, nmConn Connectivity) Connectivity {
	gwHealthy := false
	for _, obs := range devs {
		if obs.GwReachable == TriHealthy {
			gwHealthy = true
			break
		}
	}
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
// HWHealth==Healthy AND LinkUp. Priority: ethernet > wifi > none. AP mode
// maps to UplinkNone per existing wire convention (see types.go).
func activeUplinkFor(devs map[DeviceKind]DeviceObservation) UplinkType {
	if obs, ok := devs[DeviceEthernet]; ok && obs.HWHealth == TriHealthy && obs.LinkUp {
		return UplinkEthernet
	}
	if obs, ok := devs[DeviceWiFi]; ok && obs.HWHealth == TriHealthy && obs.LinkUp {
		return UplinkWiFi
	}
	return UplinkNone
}

// strContains is a thin wrapper to keep imports tidy if needed in future.
//
//nolint:unused // kept for symmetry with sibling probes
func strContains(s, sub string) bool { return strings.Contains(s, sub) }
