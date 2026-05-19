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

// IsEligibleForHostRouting is a thin wrapper for api.AppListener's method
// of the same name. Retained as a package-level function so existing call
// sites in this package read naturally; new callers should prefer the
// method form `l.IsEligibleForHostRouting()`.
func IsEligibleForHostRouting(l api.AppListener) bool {
	return l.IsEligibleForHostRouting()
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
	DerivedHostLabel string                      `json:"derived_host_label,omitempty"` // "<app>" for primary, "<listener>-<app>" for others, "" when IsEligibleForHostRouting returns false (udp; raw-tcp without tls_wrap per RFC 20260519)
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
