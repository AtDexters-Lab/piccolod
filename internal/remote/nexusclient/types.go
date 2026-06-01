package nexusclient

import (
	"context"
	"strings"

	"piccolod/internal/api"
)

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
	Endpoint       string   // Single endpoint (backward compat for self-hosted adapter)
	Endpoints      []string // Multiple relay endpoints (namek); takes precedence over Endpoint
	DeviceSecret   string
	PortalHostname string              // Fully-qualified hostname (e.g., portal.home.example.com)
	Aliases        []AliasEntry        // Additional hostnames routed to this device (e.g., custom domains)
	ClaimMappings  []api.PortClaimInfo // Port claims with local targets; TCP/UDP derived at start time
}

// ResolvedEndpoints returns a deduplicated, trimmed list of endpoints.
// If Endpoints is set, it takes precedence; otherwise Endpoint is wrapped in a slice.
func (c Config) ResolvedEndpoints() []string {
	source := c.Endpoints
	if len(source) == 0 && c.Endpoint != "" {
		source = []string{c.Endpoint}
	}
	seen := make(map[string]struct{}, len(source))
	var out []string
	for _, ep := range source {
		ep = strings.TrimSpace(ep)
		if ep == "" {
			continue
		}
		if _, dup := seen[ep]; dup {
			continue
		}
		seen[ep] = struct{}{}
		out = append(out, ep)
	}
	return out
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

type RouteDecision int

const (
	RouteNoMatch RouteDecision = iota
	RouteAllow
	RouteDeny
)

type RouteResolution struct {
	Decision RouteDecision
	Port     int
}

// RemoteRouteResolver is the newer resolver shape that can distinguish an
// ordinary miss from a terminal policy denial. Nexus may try port-claim
// fallback only after RouteNoMatch.
type RemoteRouteResolver interface {
	ResolveRoute(hostname string, remotePort int, isTLS bool) RouteResolution
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
