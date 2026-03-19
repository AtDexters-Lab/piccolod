package nexusclient

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/router"

	backend "github.com/AtDexters-Lab/nexus-proxy-backend-client/client"
)

type fakeClient struct {
	start func(context.Context)
	stop  func()
}

func (f *fakeClient) Start(ctx context.Context) {
	if f.start != nil {
		f.start(ctx)
	}
}

func (f *fakeClient) Stop() {
	if f.stop != nil {
		f.stop()
	}
}

func TestStartConfiguresAttestation(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil)
	cfg := Config{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "  secret-value  ",
		PortalHostname: "portal.example.com",
	}
	if err := adapter.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	var captured backend.ClientBackendConfig
	started := make(chan struct{}, 1)

	adapter.factory = func(cfg backend.ClientBackendConfig, opts ...backend.Option) (backendClient, error) {
		captured = cfg
		return &fakeClient{
			start: func(context.Context) {
				started <- struct{}{}
			},
		}, nil
	}

	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if err := adapter.Stop(context.Background()); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})

	select {
	case <-started:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected client Start to be invoked")
	}

	expectedHosts := []string{"portal.example.com", "*.portal.example.com"}
	if !reflect.DeepEqual(captured.Hostnames, expectedHosts) {
		t.Fatalf("unexpected hostnames: got %v want %v", captured.Hostnames, expectedHosts)
	}
	if captured.Attestation.HMACSecret != "secret-value" {
		t.Fatalf("expected HMAC secret to be trimmed, got %q", captured.Attestation.HMACSecret)
	}
	if captured.Weight != 1 {
		t.Fatalf("expected default weight 1, got %d", captured.Weight)
	}
	if strings.TrimSpace(captured.Attestation.Command) != "" {
		t.Fatalf("expected no command configured, got %q", captured.Attestation.Command)
	}
	if captured.Attestation.TokenTTL != attestationTokenTTL {
		t.Fatalf("unexpected token TTL: got %v want %v", captured.Attestation.TokenTTL, attestationTokenTTL)
	}
	if captured.Attestation.ReauthIntervalSeconds != attestationReauthIntervalSec {
		t.Fatalf("unexpected reauth interval: got %d want %d", captured.Attestation.ReauthIntervalSeconds, attestationReauthIntervalSec)
	}
	if len(captured.PortMappings) != 0 {
		t.Fatalf("expected empty PortMappings with no claims, got %d entries", len(captured.PortMappings))
	}
}

func TestStartPropagatesFactoryError(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil)
	cfg := Config{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}
	if err := adapter.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	adapter.factory = func(cfg backend.ClientBackendConfig, opts ...backend.Option) (backendClient, error) {
		return nil, errors.New("construction failed")
	}

	err := adapter.Start(context.Background())
	if err == nil {
		t.Fatalf("expected error from start when factory fails")
	}
	if !strings.Contains(err.Error(), "all 1 endpoint(s) failed") {
		t.Fatalf("expected all-endpoints-failed error, got %v", err)
	}
}

func TestConfigReady_WithTokenProvider(t *testing.T) {
	// Without token provider, DeviceSecret is required
	adapter := NewBackendAdapter(nil, nil)
	cfg := Config{Endpoint: "wss://x", PortalHostname: "x.y"}
	if adapter.configReady(cfg, cfg.ResolvedEndpoints()) {
		t.Fatal("expected configReady=false without secret and without token provider")
	}

	// With token provider, DeviceSecret is NOT required
	tp := &fakeTokenProvider{}
	adapter = NewBackendAdapter(nil, nil, WithAdapterTokenProvider(tp))
	if !adapter.configReady(cfg, cfg.ResolvedEndpoints()) {
		t.Fatal("expected configReady=true with token provider and no secret")
	}
}

func TestWithAdapterName(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil, WithAdapterName("piccolo-namek"))
	if adapter.name != "piccolo-namek" {
		t.Fatalf("expected name piccolo-namek, got %s", adapter.name)
	}
}

func TestStartWithPortClaims(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil)
	cfg := Config{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
		ClaimMappings: []api.PortClaimInfo{
			{Port: 53, HostBind: 15001, Protocol: "udp"},
			{Port: 22, HostBind: 15002, Protocol: "tcp"},
		},
	}
	if err := adapter.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	var captured backend.ClientBackendConfig
	adapter.factory = func(cfg backend.ClientBackendConfig, opts ...backend.Option) (backendClient, error) {
		captured = cfg
		return &fakeClient{}, nil
	}

	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { adapter.Stop(context.Background()) })

	// Verify TCPPorts
	if len(captured.TCPPorts) != 1 || captured.TCPPorts[0] != 22 {
		t.Fatalf("expected TCPPorts=[22], got %v", captured.TCPPorts)
	}

	// Verify UDPRoutes
	if len(captured.UDPRoutes) != 1 || captured.UDPRoutes[0].Port != 53 {
		t.Fatalf("expected UDPRoutes=[{53}], got %v", captured.UDPRoutes)
	}

	// Verify ports 80/443 are NOT in PortMappings (isolation boundary — remote
	// tunnel traffic must never fall back to the LAN listener).
	if _, ok := captured.PortMappings[80]; ok {
		t.Fatal("port 80 must not have a default mapping (isolation boundary)")
	}
	if _, ok := captured.PortMappings[443]; ok {
		t.Fatal("port 443 must not have a default mapping (isolation boundary)")
	}
	pm53, ok := captured.PortMappings[53]
	if !ok {
		t.Fatal("expected port mapping for claimed port 53")
	}
	if pm53.Default != "127.0.0.1:15001" {
		t.Fatalf("port 53 mapping = %q, want 127.0.0.1:15001", pm53.Default)
	}
	pm22, ok := captured.PortMappings[22]
	if !ok {
		t.Fatal("expected port mapping for claimed port 22")
	}
	if pm22.Default != "127.0.0.1:15002" {
		t.Fatalf("port 22 mapping = %q, want 127.0.0.1:15002", pm22.Default)
	}
}

type fakeTokenProvider struct{}

func (f *fakeTokenProvider) IssueToken(ctx context.Context, req backend.TokenRequest) (backend.Token, error) {
	return backend.Token{Value: "test-token"}, nil
}

// mockResolver implements RemoteResolver for connectHandler tests.
type mockResolver struct {
	port int
	ok   bool
}

func (m *mockResolver) Resolve(hostname string, remotePort int, isTLS bool) (int, bool) {
	return m.port, m.ok
}

func TestConnectHandler(t *testing.T) {
	t.Run("resolver_valid_port", func(t *testing.T) {
		// Start a real TCP listener so the dial succeeds.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		adapter := NewBackendAdapter(nil, &mockResolver{port: port, ok: true})
		handler := adapter.connectHandler()
		conn, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "example.com",
			OriginalHostname: "example.com",
			Port:             443,
			IsTLS:            true,
		})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		conn.Close()
	})

	t.Run("resolver_0_false", func(t *testing.T) {
		// Both lookups return (0, false) → ErrNoRoute.
		adapter := NewBackendAdapter(nil, &mockResolver{port: 0, ok: false})
		handler := adapter.connectHandler()
		_, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "unknown.com",
			OriginalHostname: "unknown.com",
			Port:             443,
			IsTLS:            true,
		})
		if !errors.Is(err, backend.ErrNoRoute) {
			t.Fatalf("expected ErrNoRoute, got %v", err)
		}
	})

	t.Run("resolver_0_true", func(t *testing.T) {
		// Resolver returns (0, true) — the bug scenario. Must be a fatal error, NOT ErrNoRoute.
		adapter := NewBackendAdapter(nil, &mockResolver{port: 0, ok: true})
		handler := adapter.connectHandler()
		_, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "piccolospace.com",
			OriginalHostname: "piccolospace.com",
			Port:             443,
			IsTLS:            true,
		})
		if err == nil {
			t.Fatal("expected error when resolver returns port 0")
		}
		if errors.Is(err, backend.ErrNoRoute) {
			t.Fatal("port-0 must NOT fall back to ErrNoRoute (would route to wrong cert)")
		}
	})

	t.Run("nil_resolver", func(t *testing.T) {
		adapter := NewBackendAdapter(nil, nil)
		handler := adapter.connectHandler()
		_, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "example.com",
			OriginalHostname: "example.com",
			Port:             443,
			IsTLS:            true,
		})
		if !errors.Is(err, backend.ErrNoRoute) {
			t.Fatalf("expected ErrNoRoute for nil resolver, got %v", err)
		}
	})

	t.Run("port_claim_tcp", func(t *testing.T) {
		// Resolver returns ErrNoRoute for port-claim hostnames like "tcp:53".
		// The handler should fall through to protocol-aware port-claim resolution.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		tcpPort := ln.Addr().(*net.TCPAddr).Port

		adapter := NewBackendAdapter(nil, &mockResolver{port: 0, ok: false})
		adapter.cfg = Config{
			ClaimMappings: []api.PortClaimInfo{
				{Port: 53, HostBind: 19999, Protocol: "udp"},
				{Port: 53, HostBind: tcpPort, Protocol: "tcp"},
			},
		}
		handler := adapter.connectHandler()
		conn, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "tcp:53",
			OriginalHostname: "tcp:53",
			Port:             53,
			Transport:        backend.TransportTCP,
		})
		if err != nil {
			t.Fatalf("expected TCP port-claim to resolve, got %v", err)
		}
		conn.Close()
	})

	t.Run("port_claim_udp", func(t *testing.T) {
		// UDP port-claim resolution must return the UDP host-bind, not the TCP one.
		udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		pc, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer pc.Close()
		udpPort := pc.LocalAddr().(*net.UDPAddr).Port

		adapter := NewBackendAdapter(nil, &mockResolver{port: 0, ok: false})
		adapter.cfg = Config{
			ClaimMappings: []api.PortClaimInfo{
				{Port: 53, HostBind: udpPort, Protocol: "udp"},
				{Port: 53, HostBind: 19999, Protocol: "tcp"},
			},
		}
		handler := adapter.connectHandler()
		conn, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "udp:53",
			OriginalHostname: "udp:53",
			Port:             53,
			Transport:        backend.TransportUDP,
		})
		if err != nil {
			t.Fatalf("expected UDP port-claim to resolve, got %v", err)
		}
		conn.Close()
	})

	t.Run("port_claim_no_match", func(t *testing.T) {
		// No claim exists for the requested port — should return ErrNoRoute.
		adapter := NewBackendAdapter(nil, &mockResolver{port: 0, ok: false})
		adapter.cfg = Config{
			ClaimMappings: []api.PortClaimInfo{
				{Port: 53, HostBind: 15001, Protocol: "udp"},
			},
		}
		handler := adapter.connectHandler()
		_, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "tcp:22",
			OriginalHostname: "tcp:22",
			Port:             22,
			Transport:        backend.TransportTCP,
		})
		if !errors.Is(err, backend.ErrNoRoute) {
			t.Fatalf("expected ErrNoRoute for unclaimed port, got %v", err)
		}
	})

	t.Run("tunnel_mode", func(t *testing.T) {
		rm := router.NewManager()
		rm.RegisterAppRoute("myapp", router.ModeTunnel, "leader-1")
		adapter := NewBackendAdapter(rm, &mockResolver{port: 8080, ok: true})
		handler := adapter.connectHandler()
		_, err := handler(context.Background(), backend.ConnectRequest{
			Hostname:         "myapp",
			OriginalHostname: "myapp.example.com",
			Port:             443,
			IsTLS:            true,
		})
		if !errors.Is(err, backend.ErrNoRoute) {
			t.Fatalf("expected ErrNoRoute in tunnel mode, got %v", err)
		}
	})
}

func TestStartWithMultipleEndpoints(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil, WithAdapterTokenProvider(&fakeTokenProvider{}))
	cfg := Config{
		Endpoints:      []string{"wss://a/connect", "wss://b/connect"},
		PortalHostname: "portal.example.com",
	}
	if err := adapter.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	var captured []backend.ClientBackendConfig
	var capturedMu sync.Mutex
	started := make(chan struct{}, 2)

	adapter.factory = func(cfg backend.ClientBackendConfig, opts ...backend.Option) (backendClient, error) {
		capturedMu.Lock()
		captured = append(captured, cfg)
		capturedMu.Unlock()
		return &fakeClient{
			start: func(context.Context) { started <- struct{}{} },
		}, nil
	}

	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for both clients to start.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("expected 2 client starts, only got %d", i)
		}
	}

	capturedMu.Lock()
	defer capturedMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("expected factory called 2 times, got %d", len(captured))
	}

	addrs := map[string]bool{captured[0].NexusAddress: true, captured[1].NexusAddress: true}
	if !addrs["wss://a/connect"] || !addrs["wss://b/connect"] {
		t.Fatalf("unexpected NexusAddresses: %v, %v", captured[0].NexusAddress, captured[1].NexusAddress)
	}

	// Stop should stop both clients.
	stopped := 0
	adapter.mu.Lock()
	for _, c := range adapter.clients {
		fc := c.(*fakeClient)
		fc.stop = func() { stopped++ }
	}
	adapter.mu.Unlock()

	if err := adapter.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped != 2 {
		t.Fatalf("expected 2 stops, got %d", stopped)
	}
}

func TestStartWithMultipleEndpoints_PartialFailure(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil, WithAdapterTokenProvider(&fakeTokenProvider{}))
	cfg := Config{
		Endpoints:      []string{"wss://a/connect", "wss://b/connect"},
		PortalHostname: "portal.example.com",
	}
	if err := adapter.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	started := make(chan struct{}, 1)
	adapter.factory = func(cfg backend.ClientBackendConfig, opts ...backend.Option) (backendClient, error) {
		if cfg.NexusAddress == "wss://a/connect" {
			return nil, errors.New("connection refused")
		}
		return &fakeClient{
			start: func(context.Context) { started <- struct{}{} },
		}, nil
	}

	// Should succeed — one endpoint worked.
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("expected partial success, got error: %v", err)
	}
	t.Cleanup(func() { adapter.Stop(context.Background()) })

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected surviving client to start")
	}
}

func TestStartWithMultipleEndpoints_AllFail(t *testing.T) {
	adapter := NewBackendAdapter(nil, nil, WithAdapterTokenProvider(&fakeTokenProvider{}))
	cfg := Config{
		Endpoints:      []string{"wss://a/connect", "wss://b/connect"},
		PortalHostname: "portal.example.com",
	}
	if err := adapter.Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	adapter.factory = func(cfg backend.ClientBackendConfig, opts ...backend.Option) (backendClient, error) {
		return nil, errors.New("connection refused")
	}

	err := adapter.Start(context.Background())
	if err == nil {
		t.Fatal("expected error when all endpoints fail")
	}
	if !strings.Contains(err.Error(), "all 2 endpoint(s) failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected underlying errors to be preserved, got: %v", err)
	}
}

func TestResolvedEndpoints(t *testing.T) {
	t.Run("backward_compat_single_endpoint", func(t *testing.T) {
		cfg := Config{Endpoint: "wss://x/connect"}
		got := cfg.ResolvedEndpoints()
		if len(got) != 1 || got[0] != "wss://x/connect" {
			t.Fatalf("expected [wss://x/connect], got %v", got)
		}
	})

	t.Run("endpoints_takes_precedence", func(t *testing.T) {
		cfg := Config{
			Endpoint:  "wss://old/connect",
			Endpoints: []string{"wss://a/connect", "wss://b/connect"},
		}
		got := cfg.ResolvedEndpoints()
		if len(got) != 2 || got[0] != "wss://a/connect" || got[1] != "wss://b/connect" {
			t.Fatalf("expected Endpoints to take precedence, got %v", got)
		}
	})

	t.Run("dedup", func(t *testing.T) {
		cfg := Config{Endpoints: []string{"a", "a", "b"}}
		got := cfg.ResolvedEndpoints()
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("expected dedup [a b], got %v", got)
		}
	})

	t.Run("trimming", func(t *testing.T) {
		cfg := Config{Endpoints: []string{"  ", "a", " b "}}
		got := cfg.ResolvedEndpoints()
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("expected trimmed [a b], got %v", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		cfg := Config{}
		got := cfg.ResolvedEndpoints()
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})
}
