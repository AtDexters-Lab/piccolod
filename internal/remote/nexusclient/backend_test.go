package nexusclient

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"piccolod/internal/api"

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
	if !strings.Contains(err.Error(), "construction failed") {
		t.Fatalf("expected factory error to propagate, got %v", err)
	}
}

func TestConfigReady_WithTokenProvider(t *testing.T) {
	// Without token provider, DeviceSecret is required
	adapter := NewBackendAdapter(nil, nil)
	if adapter.configReady(Config{Endpoint: "wss://x", PortalHostname: "x.y"}) {
		t.Fatal("expected configReady=false without secret and without token provider")
	}

	// With token provider, DeviceSecret is NOT required
	tp := &fakeTokenProvider{}
	adapter = NewBackendAdapter(nil, nil, WithAdapterTokenProvider(tp))
	if !adapter.configReady(Config{Endpoint: "wss://x", PortalHostname: "x.y"}) {
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

	// Verify PortMappings include both defaults and claims
	if _, ok := captured.PortMappings[80]; !ok {
		t.Fatal("expected default port mapping for 80")
	}
	if _, ok := captured.PortMappings[443]; !ok {
		t.Fatal("expected default port mapping for 443")
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
