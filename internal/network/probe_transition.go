package network

import (
	"log"
	"net/netip"
	"sort"

	"github.com/godbus/dbus/v5"

	"piccolod/internal/network/nmclient"
)

type transitionProjection struct {
	interfaces           []NetworkInterfaceState
	probes               []transitionInterfaceProbe
	interfacesObserved   bool
	defaultRouteIface    string
	defaultRouteObserved bool
	defaultRouteKnown    bool
	dnsDefaultIface      string
	dnsDefaultObserved   bool
	dnsDefaultKnown      bool
	activeUplinkIface    string
}

type transitionInterfaceProbe struct {
	kind        DeviceKind
	iface       string
	path        dbus.ObjectPath
	linkUp      bool
	hasIP       bool
	hotspot     bool
	ipv4        []netip.Addr
	ipv6        []netip.Addr
	info        *nmclient.ActiveConnectionInfo
	routeMetric uint32
	routesKnown bool
	unknown     bool
}

// activeUplinkFromProjection returns the kind and concrete owner of the
// observed default route. A completed projection that proves there is no
// default route authoritatively reports no uplink; an incomplete projection
// fails closed without promoting class-level recovery observations.
func activeUplinkFromProjection(proj transitionProjection) (UplinkType, string, bool) {
	if !proj.interfacesObserved || !proj.defaultRouteObserved {
		return UplinkNone, "", false
	}
	if !proj.defaultRouteKnown {
		return UplinkNone, "", true
	}
	uplink, ok := activeUplinkForRoute(proj.interfaces, proj.defaultRouteIface)
	if !ok {
		return UplinkNone, "", false
	}
	return uplink, proj.defaultRouteIface, true
}

func (p *Prober) probeTransitionProjection(connectivity Connectivity) transitionProjection {
	probes, ok := p.probeTransitionInterfaces()
	proj := transitionProjection{probes: probes, interfacesObserved: ok}
	if !ok {
		proj.probes = nil
		return proj
	}
	proj.defaultRouteObserved = true
	proj.dnsDefaultObserved = true

	for _, probe := range probes {
		if probe.hotspot || !probe.linkUp {
			continue
		}
		if probe.unknown {
			proj.defaultRouteObserved = false
			proj.dnsDefaultObserved = false
			continue
		}
		if probe.info == nil || !probe.hasIP {
			continue
		}
		if !probe.routesKnown {
			proj.defaultRouteObserved = false
			proj.dnsDefaultObserved = false
			continue
		}
		if probe.info.IP4HasDefaultRoute || probe.info.IP6HasDefaultRoute {
			if !proj.defaultRouteKnown || probe.routeMetric < routeMetricForInterface(probes, proj.defaultRouteIface) {
				proj.defaultRouteIface = probe.iface
				proj.defaultRouteKnown = true
			}
		}
		if (probe.info.IP4HasDefaultRoute || probe.info.IP6HasDefaultRoute) &&
			(len(probe.info.IP4Nameservers) > 0 || len(probe.info.IP6Nameservers) > 0) {
			if !proj.dnsDefaultKnown || probe.routeMetric < routeMetricForInterface(probes, proj.dnsDefaultIface) {
				proj.dnsDefaultIface = probe.iface
				proj.dnsDefaultKnown = true
			}
		}
	}
	if !proj.defaultRouteObserved {
		proj.defaultRouteIface = ""
		proj.defaultRouteKnown = false
	}
	if !proj.dnsDefaultObserved {
		proj.dnsDefaultIface = ""
		proj.dnsDefaultKnown = false
	}
	if !proj.dnsDefaultKnown && proj.defaultRouteKnown {
		proj.dnsDefaultIface = proj.defaultRouteIface
		proj.dnsDefaultKnown = true
	}

	if proj.defaultRouteKnown {
		proj.activeUplinkIface = proj.defaultRouteIface
	}

	proj.applyConnectivity(connectivity)
	return proj
}

// applyConnectivity rebuilds the public interface projection after the
// selected default-route interface has been classified. The retained probes
// keep route selection, connectivity, and interface roles on one coherent
// NetworkManager observation pass.
func (proj *transitionProjection) applyConnectivity(connectivity Connectivity) {
	if !proj.interfacesObserved {
		proj.interfaces = nil
		return
	}
	proj.interfaces = make([]NetworkInterfaceState, 0, len(proj.probes))
	for _, probe := range proj.probes {
		proj.interfaces = append(proj.interfaces, NetworkInterfaceState{
			Kind:   probe.kind,
			Iface:  probe.iface,
			Role:   roleForInterface(probe, proj.activeUplinkIface, proj.defaultRouteIface, proj.defaultRouteKnown, connectivity),
			LinkUp: probe.linkUp,
			HasIP:  probe.hasIP,
			IPv4:   probe.ipv4,
			IPv6:   probe.ipv6,
		})
	}
}

func (proj transitionProjection) probeForInterface(iface string) (transitionInterfaceProbe, bool) {
	for _, probe := range proj.probes {
		if probe.iface == iface {
			return probe, true
		}
	}
	return transitionInterfaceProbe{}, false
}

func (p *Prober) probeTransitionInterfaces() ([]transitionInterfaceProbe, bool) {
	var probes []transitionInterfaceProbe
	complete := true

	wifi, err := p.nm.WiFiDevices()
	if err != nil {
		log.Printf("WARN: net-probe: transition wifi devices: %v", err)
		complete = false
	} else {
		for _, dev := range wifi {
			probes = append(probes, p.transitionProbeForDevice(DeviceWiFi, dev.Path, dev.Interface, dev.State >= nmclient.NMDeviceStateIPConfig))
		}
	}

	eth, err := p.nm.EthernetDevices()
	if err != nil {
		log.Printf("WARN: net-probe: transition ethernet devices: %v", err)
		complete = false
	} else {
		for _, dev := range eth {
			linkUp := dev.Carrier && dev.State >= nmclient.NMDeviceStateIPConfig
			probes = append(probes, p.transitionProbeForDevice(DeviceEthernet, dev.Path, dev.Interface, linkUp))
		}
	}

	sort.Slice(probes, func(i, j int) bool {
		if probes[i].kind != probes[j].kind {
			return probes[i].kind.String() < probes[j].kind.String()
		}
		return probes[i].iface < probes[j].iface
	})
	return probes, complete
}

func (p *Prober) transitionProbeForDevice(kind DeviceKind, path dbus.ObjectPath, iface string, linkUp bool) transitionInterfaceProbe {
	probe := transitionInterfaceProbe{
		kind:   kind,
		iface:  iface,
		path:   path,
		linkUp: linkUp,
	}
	if !linkUp {
		return probe
	}
	info, err := p.nm.ActiveConnectionInfo(path)
	if err != nil || info == nil {
		probe.unknown = true
		return probe
	}
	probe.info = info
	probe.hotspot = info.IsHotspot()
	probe.ipv4 = parseAddrStrings(append(info.IP4Addresses, info.IP4Address))
	probe.ipv6 = parseAddrStrings(info.IP6Addresses)
	probe.hasIP = len(probe.ipv4) > 0 || len(probe.ipv6) > 0
	probe.routeMetric = info.RouteMetric
	probe.routesKnown = info.IP4RoutesKnown || info.IP6RoutesKnown || info.IP4HasDefaultRoute || info.IP6HasDefaultRoute
	return probe
}

func parseAddrStrings(in []string) []netip.Addr {
	out := make([]netip.Addr, 0, len(in))
	for _, raw := range in {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			continue
		}
		out = append(out, addr)
	}
	return canonicalAddrs(out)
}

func routeMetricForInterface(probes []transitionInterfaceProbe, iface string) uint32 {
	for _, probe := range probes {
		if probe.iface == iface {
			return probe.routeMetric
		}
	}
	return 0
}

func roleForInterface(probe transitionInterfaceProbe, activeIface, defaultRouteIface string, defaultRouteKnown bool, connectivity Connectivity) NetworkInterfaceRole {
	if probe.hotspot {
		return InterfaceRoleFiltered
	}
	if probe.unknown {
		return InterfaceRoleUnknown
	}
	if !probe.linkUp || !probe.hasIP {
		return InterfaceRoleNotConnected
	}
	hasDefaultRoute := probe.info != nil && (probe.info.IP4HasDefaultRoute || probe.info.IP6HasDefaultRoute)
	if !defaultRouteKnown && !hasDefaultRoute {
		if probe.routesKnown && interfaceHasLANAddress(probe) {
			return InterfaceRoleLAN
		}
		return InterfaceRoleUnknown
	}
	selectedDefault := defaultRouteKnown && probe.iface == defaultRouteIface
	if probe.iface == activeIface || selectedDefault {
		if connectivity == ConnectivityFull || connectivity == ConnectivityLimited {
			if !interfaceHasLANAddress(probe) {
				return InterfaceRoleWAN
			}
			return InterfaceRoleWANLAN
		}
		if interfaceHasLANAddress(probe) {
			return InterfaceRoleLAN
		}
		return InterfaceRoleWAN
	}
	if hasDefaultRoute {
		if interfaceHasLANAddress(probe) {
			return InterfaceRoleWANLAN
		}
		return InterfaceRoleWAN
	}
	if interfaceHasLANAddress(probe) {
		return InterfaceRoleLAN
	}
	return InterfaceRoleUnknown
}

func interfaceHasLANAddress(probe transitionInterfaceProbe) bool {
	for _, addr := range probe.ipv4 {
		if addr.IsPrivate() || addr.IsLinkLocalUnicast() {
			return true
		}
	}
	for _, addr := range probe.ipv6 {
		if addr.IsPrivate() {
			return true
		}
	}
	return false
}
