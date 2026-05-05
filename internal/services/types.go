package services

import (
	"piccolod/internal/api"
	"piccolod/internal/services/middleware"
)

// PortRange defines an inclusive range of ports
type PortRange struct {
	Start int
	End   int
}

// IsEligibleForHostRouting returns true if a listener should have a DerivedHostLabel
// for hostname-based resolution (remote or LAN). Per RFC 20260316 +
// plan §D8 (tcp+raw enablement):
//   - flow:udp returns false (uses port-based routing only)
//   - flow:tls returns true (host label needed for remote resolver; LAN
//     host-based skips flow:tls because piccolod doesn't terminate TLS)
//   - flow:tcp returns true for any protocol — http/websocket route via
//     the L7 chain on the host-based label; raw routes via the TLS mux
//     (passthrough) on the host-based label, with TLS still terminated
//     by the mux (clients connect over TLS by SNI; backend sees raw bytes).
func IsEligibleForHostRouting(protocol api.ListenerProtocol, flow api.ListenerFlow) bool {
	if flow == api.FlowUDP {
		return false
	}
	return true
}

// ServiceEndpoint represents a fully allocated listener
type ServiceEndpoint struct {
	App              string                      `json:"app"`
	Name             string                      `json:"name"`
	GuestPort        int                         `json:"guest_port"`
	HostBind         int                         `json:"host_port"`   // 127.0.0.1:HostBind → container:GuestPort
	PublicPort       int                         `json:"public_port"` // 0.0.0.0:PublicPort → HostBind
	Flow             api.ListenerFlow            `json:"flow"`
	Protocol         api.ListenerProtocol        `json:"protocol"`
	Primary          bool                        `json:"primary,omitempty"`            // Is this the primary listener for host-based routing?
	DerivedHostLabel string                      `json:"derived_host_label,omitempty"` // "<app>" for primary, "<listener>-<app>" for others, "" for udp (per RFC 20260505 §3.5 raw(tcp) is now host-routable via TLS mux SNI)
	Middleware       []api.AppProtocolMiddleware `json:"middleware"`
	RemotePorts      []int                       `json:"remote_ports"`
	LocalURL         string                      `json:"local_url,omitempty"` // Optional pre-calculated LAN URL
	Auth             *api.ListenerAuth           `json:"auth,omitempty"`
	ConnectionAuth   *api.ConnectionAuth         `json:"connection_auth,omitempty"`
	PortClaim        *int                        `json:"port_claim,omitempty"` // Well-known port to bind on LAN (and claim on relay)
}

// endpointKey returns a unique key for an endpoint (app/listener)
func (e ServiceEndpoint) endpointKey() string {
	return e.App + "/" + e.Name
}

// AsMiddlewareInfo projects the orchestrator's ServiceEndpoint into the
// middleware-package's EndpointInfo shape (the minimal endpoint metadata that
// middleware implementations need at chain time). Used at every middleware
// dispatch site to bridge the type boundary.
func (e ServiceEndpoint) AsMiddlewareInfo() middleware.EndpointInfo {
	return middleware.EndpointInfo{
		App:              e.App,
		Listener:         e.Name,
		HostBind:         e.HostBind,
		PublicPort:       e.PublicPort,
		Flow:             e.Flow,
		Protocol:         e.Protocol,
		DerivedHostLabel: e.DerivedHostLabel,
		Auth:             e.Auth,
		ConnectionAuth:   e.ConnectionAuth,
	}
}
