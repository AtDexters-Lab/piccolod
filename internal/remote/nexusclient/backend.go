package nexusclient

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	backend "github.com/AtDexters-Lab/nexus-proxy-backend-client/client"

	"piccolod/internal/router"
)

type backendClient interface {
	Start(context.Context)
	Stop()
}

type clientFactory func(backend.ClientBackendConfig, ...backend.Option) (backendClient, error)

type realBackendClient struct {
	*backend.Client
}

func (c *realBackendClient) Start(ctx context.Context) { c.Client.Start(ctx) }
func (c *realBackendClient) Stop()                     { c.Client.Stop() }

const (
	attestationTokenTTL           = 60 * time.Second
	attestationHandshakeMaxAgeSec = 5
	attestationReauthIntervalSec  = 300
	attestationReauthGraceSec     = 30
	attestationMaintenanceCapSec  = 600
	attestationCacheHandshake     = 5 * time.Second
)

// BackendAdapterOption configures a BackendAdapter.
type BackendAdapterOption func(*BackendAdapter)

// WithAdapterTokenProvider injects a custom token provider that replaces HMAC attestation.
// When set, DeviceSecret is not required in the adapter config.
func WithAdapterTokenProvider(p backend.TokenProvider) BackendAdapterOption {
	return func(a *BackendAdapter) { a.tokenProvider = p }
}

// WithAdapterName sets the backend name used for Nexus registration.
// Defaults to "piccolo-portal" if not set.
func WithAdapterName(name string) BackendAdapterOption {
	return func(a *BackendAdapter) { a.name = name }
}

// BackendAdapter bridges piccolod with the nexus proxy backend client. It now uses
// the upstream token provider hook so that every connection attempt receives a
// freshly minted JWT without custom reconnect loops on our side.
type BackendAdapter struct {
	mu       sync.Mutex
	cfg      Config
	router   *router.Manager
	resolver RemoteResolver

	factory       clientFactory
	cancel        context.CancelFunc
	client        backendClient
	tokenProvider backend.TokenProvider // nil = use HMAC from config
	name          string               // backend name for Nexus registration
}

func NewBackendAdapter(r *router.Manager, resolver RemoteResolver, opts ...BackendAdapterOption) *BackendAdapter {
	a := &BackendAdapter{
		router:   r,
		resolver: resolver,
		name:     "piccolo-portal",
		factory: func(cfg backend.ClientBackendConfig, clientOpts ...backend.Option) (backendClient, error) {
			client, err := backend.New(cfg, clientOpts...)
			if err != nil {
				return nil, err
			}
			return &realBackendClient{
				Client: client,
			}, nil
		},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *BackendAdapter) Configure(cfg Config) error {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return nil
}

func (a *BackendAdapter) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		return nil
	}
	cfg := a.cfg
	if !a.configReady(cfg) {
		a.mu.Unlock()
		log.Printf("WARN: nexus adapter start skipped, missing configuration")
		return nil
	}
	hosts := buildHostnameList(cfg)

	// Build port mappings from port claims only — ports 80/443 are intentionally
	// excluded so that unresolved remote tunnel traffic is rejected rather than
	// falling through to the LAN-only internal HTTPS listener.
	//
	// Note: portMappings is keyed by port number alone (no protocol dimension).
	// When both TCP and UDP claim the same port, the last entry wins. This is
	// fine because connectHandler() resolves port claims with protocol awareness
	// via resolvePortClaim() before the fallback to this map.
	portMappings := make(map[int]backend.PortMapping, len(cfg.ClaimMappings))
	var tcpPorts []int
	var udpRoutes []backend.UDPRouteConfig
	for _, cm := range cfg.ClaimMappings {
		portMappings[cm.Port] = backend.PortMapping{
			Default: fmt.Sprintf("127.0.0.1:%d", cm.HostBind),
		}
		if cm.Protocol == "udp" {
			udpRoutes = append(udpRoutes, backend.UDPRouteConfig{Port: cm.Port})
		} else {
			tcpPorts = append(tcpPorts, cm.Port)
		}
	}

	backendCfg := backend.ClientBackendConfig{
		Name:         a.name,
		Hostnames:    hosts,
		NexusAddress: cfg.Endpoint,
		Weight:       1,
		TCPPorts:     tcpPorts,
		UDPRoutes:    udpRoutes,
		PortMappings: portMappings,
		Attestation: backend.AttestationOptions{
			HMACSecret:                 strings.TrimSpace(cfg.DeviceSecret),
			TokenTTL:                   attestationTokenTTL,
			CacheHandshake:             attestationCacheHandshake,
			HandshakeMaxAgeSeconds:     attestationHandshakeMaxAgeSec,
			ReauthIntervalSeconds:      attestationReauthIntervalSec,
			ReauthGraceSeconds:         attestationReauthGraceSec,
			MaintenanceGraceCapSeconds: attestationMaintenanceCapSec,
		},
	}
	handler := a.connectHandler()

	var clientOpts []backend.Option
	clientOpts = append(clientOpts, backend.WithConnectHandler(handler))
	if a.tokenProvider != nil {
		clientOpts = append(clientOpts, backend.WithTokenProvider(a.tokenProvider))
	}

	client, err := a.factory(backendCfg, clientOpts...)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("construct backend client: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.client = client
	a.cancel = cancel
	a.mu.Unlock()

	go client.Start(runCtx)
	return nil
}

func (a *BackendAdapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	cancel := a.cancel
	client := a.client
	a.cancel = nil
	a.client = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if client != nil {
		client.Stop()
	}
	return nil
}

func (a *BackendAdapter) connectHandler() backend.ConnectHandler {
	return func(ctx context.Context, req backend.ConnectRequest) (net.Conn, error) {
		if a.router != nil {
			route := a.router.DecideAppRoute(req.Hostname)
			if route.Mode == router.ModeTunnel {
				return nil, backend.ErrNoRoute
			}
		}

		if a.resolver == nil {
			// No resolver configured — reject connection.
			return nil, backend.ErrNoRoute
		}

		localPort := 0
		if port, ok := a.resolver.Resolve(req.OriginalHostname, req.Port, req.IsTLS); ok {
			localPort = port
		} else if port, ok := a.resolver.Resolve(req.Hostname, req.Port, req.IsTLS); ok {
			localPort = port
		} else if port, ok := a.resolvePortClaim(req.Port, req.Transport); ok {
			// Port-claim resolution: match by port AND protocol to handle
			// dual-protocol claims (e.g., DNS with both TCP and UDP on port 53)
			// that have different host-bind ports per protocol.
			localPort = port
		} else {
			return nil, backend.ErrNoRoute
		}

		if localPort == 0 {
			return nil, fmt.Errorf("resolver returned invalid port 0 for %s:%d", req.Hostname, req.Port)
		}

		target := fmt.Sprintf("127.0.0.1:%d", localPort)
		network := "tcp"
		if req.Transport == backend.TransportUDP {
			network = "udp"
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, target)
		if err != nil {
			return nil, err
		}
		if recorder, ok := a.resolver.(interface{ RecordConnectionHint(int, int, int, bool) }); ok {
			if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
				recorder.RecordConnectionHint(localPort, addr.Port, req.Port, req.IsTLS)
			}
		}
		return conn, nil
	}
}

// resolvePortClaim matches a port claim by port number and transport protocol.
func (a *BackendAdapter) resolvePortClaim(port int, transport backend.Transport) (int, bool) {
	cfg := a.currentConfig()
	proto := string(transport) // backend.Transport is "tcp" or "udp", matching PortClaimInfo.Protocol
	for _, cm := range cfg.ClaimMappings {
		if cm.Port == port && cm.Protocol == proto {
			return cm.HostBind, true
		}
	}
	return 0, false
}

func (a *BackendAdapter) currentConfig() Config {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	return cfg
}

func buildHostnameList(cfg Config) []string {
	base := strings.TrimSuffix(strings.ToLower(cfg.PortalHostname), ".")
	hosts := []string{base}
	if base != "" {
		// RFC 20260114: remote base is the portal hostname apex; apps are <label>.<base>.
		hosts = append(hosts, "*."+base)
	}
	for _, a := range cfg.Aliases {
		h := strings.TrimSuffix(strings.ToLower(a.Hostname), ".")
		if h != "" {
			hosts = append(hosts, h, "*."+h)
		}
	}
	return hosts
}

// configReady checks whether the adapter has enough config to start.
// When a tokenProvider is set, DeviceSecret is not required.
func (a *BackendAdapter) configReady(cfg Config) bool {
	if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.PortalHostname) == "" {
		return false
	}
	if a.tokenProvider == nil && strings.TrimSpace(cfg.DeviceSecret) == "" {
		return false
	}
	return true
}
