package services

import "piccolod/internal/api"

// PortRange defines an inclusive range of ports
type PortRange struct {
	Start int
	End   int
}

// IsEligibleForHostRouting returns true if a listener can have host-based URLs.
// Only flow:tcp + protocol:http|websocket are eligible per RFC 20260114.
func IsEligibleForHostRouting(protocol api.ListenerProtocol, flow api.ListenerFlow) bool {
	if flow == api.FlowTLS {
		return false
	}
	return protocol == api.ListenerProtocolHTTP || protocol == api.ListenerProtocolWebsocket
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
	DerivedHostLabel string                      `json:"derived_host_label,omitempty"` // "<app>" for primary, "<listener>-<app>" for others, "" for raw/tls
	Middleware       []api.AppProtocolMiddleware `json:"middleware"`
	RemotePorts      []int                       `json:"remote_ports"`
	LocalURL         string                      `json:"local_url,omitempty"` // Optional pre-calculated LAN URL
	Auth             *api.ListenerAuth           `json:"auth,omitempty"`
}

// endpointKey returns a unique key for an endpoint (app/listener)
func (e ServiceEndpoint) endpointKey() string {
	return e.App + "/" + e.Name
}
