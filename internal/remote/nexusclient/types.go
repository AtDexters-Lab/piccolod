package nexusclient

import "context"

// PortalHostLabel is the sentinel value for aliases that target the portal itself
// rather than a specific app listener. It uses a double-underscore prefix that is
// prohibited by appNameRegex, making collisions with real DerivedHostLabels impossible.
const PortalHostLabel = "__portal"

// AliasEntry pairs a public hostname with the DerivedHostLabel of the listener
// it targets. HostLabel is either a real DerivedHostLabel (e.g., "myapp",
// "home-myapp") or PortalHostLabel for portal-targeted aliases.
type AliasEntry struct {
	Hostname  string
	HostLabel string
}

// Config represents the minimum information needed to connect to the nexus proxy.
type Config struct {
	Endpoint       string
	DeviceSecret   string
	PortalHostname string       // Fully-qualified hostname (e.g., portal.home.example.com)
	Aliases        []AliasEntry // Additional hostnames routed to this device (e.g., custom domains)
}

// Adapter provides a lifecycle wrapper around the nexus backend client.
type Adapter interface {
	Configure(Config) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// RemoteResolver resolves incoming Nexus requests to local listener ports.
type RemoteResolver interface {
	Resolve(hostname string, remotePort int, isTLS bool) (int, bool)
}

// PortController is an optional extension. Implementers may choose to take
// explicit action when a local public port is no longer available (e.g.,
// proactively refuse or unregister routes).
type PortController interface {
	UnregisterPublicPort(port int)
}

// PortPublisher is an optional extension to re-enable a public port for
// inbound streams once a service comes back or a port is recycled.
type PortPublisher interface {
	RegisterPublicPort(port int)
}
