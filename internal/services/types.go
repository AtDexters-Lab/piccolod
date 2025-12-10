package services

import "piccolod/internal/api"

// PortRange defines an inclusive range of ports
type PortRange struct {
	Start int
	End   int
}

// ServiceEndpoint represents a fully allocated listener
type ServiceEndpoint struct {
	App         string                      `json:"app"`
	Name        string                      `json:"name"`
	GuestPort   int                         `json:"guest_port"`
	HostBind    int                         `json:"host_port"` // 127.0.0.1:HostBind → container:GuestPort
	PublicPort  int                         `json:"public_port"` // 0.0.0.0:PublicPort → HostBind
	Flow        api.ListenerFlow            `json:"flow"`
	Protocol    api.ListenerProtocol        `json:"protocol"`
	Middleware  []api.AppProtocolMiddleware `json:"middleware"`
	RemotePorts []int                       `json:"remote_ports"`
	LocalURL    string                      `json:"local_url,omitempty"` // Optional pre-calculated LAN URL
}
