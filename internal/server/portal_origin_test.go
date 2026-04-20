package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"piccolod/internal/mdns"
)

// newPortalOriginTestServer returns a GinServer with a real mDNS manager so
// portalOriginForRequest's server-known-hostname branch fires (production has
// mdnsManager wired; the prior &GinServer{} style exercised a fail-open Host-
// echo fallback that was closed for security — see Sec-Low-1).
func newPortalOriginTestServer(t *testing.T) (*GinServer, string) {
	t.Helper()
	m := mdns.NewManager()
	return &GinServer{mdnsManager: m}, m.Hostname()
}

func TestPortalOriginForRequest_UsesPortalPortNotListenerPort(t *testing.T) {
	t.Setenv("PORT", "8080")

	s, host := newPortalOriginTestServer(t)
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	want := "http://" + host + ":8080"
	if origin != want {
		t.Fatalf("expected origin %s, got %q", want, origin)
	}
}

func TestPortalOriginForRequest_OmitsDefaultHTTPPort(t *testing.T) {
	t.Setenv("PORT", "80")

	s, host := newPortalOriginTestServer(t)
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	want := "http://" + host
	if origin != want {
		t.Fatalf("expected origin %s, got %q", want, origin)
	}
}

func TestPortalOriginForRequest_HTTPSDoesNotAppendPort80(t *testing.T) {
	t.Setenv("PORT", "80")

	s, host := newPortalOriginTestServer(t)
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	// Use the secure loopback context key (the trusted TLS indicator) instead of
	// X-Forwarded-Proto which is client-spoofable and no longer trusted.
	ctx := context.WithValue(req.Context(), secureContextKeyInstance, true)
	req = req.WithContext(ctx)
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	want := "https://" + host
	if origin != want {
		t.Fatalf("expected origin %s, got %q", want, origin)
	}
}

func TestPortalOriginForRequest_IgnoresSpoofedXForwardedProto(t *testing.T) {
	t.Setenv("PORT", "80")

	s, host := newPortalOriginTestServer(t)
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	want := "http://" + host
	if origin != want {
		t.Fatalf("expected spoofed X-Forwarded-Proto to be ignored, got %q", origin)
	}
}

// TestPortalOriginForRequest_RejectsSpoofedHostWhenMDNSDown verifies the
// fail-closed fix (Sec-Low-1): with no mDNS manager and a non-loopback
// RemoteAddr, portalOriginForRequest must NOT echo client-supplied r.Host
// into the origin — otherwise a spoofed Host header under a DNS-rebinding or
// middlebox-proxy attack would flow through to any redirect target built
// from this function (e.g., the OIDC gate refusal).
func TestPortalOriginForRequest_RejectsSpoofedHostWhenMDNSDown(t *testing.T) {
	t.Setenv("PORT", "80")

	s := &GinServer{} // no mdnsManager — exercises the fallback
	req := httptest.NewRequest("GET", "http://attacker.example.com/", nil)
	req.Host = "attacker.example.com"
	req.RemoteAddr = "192.0.2.1:1234" // non-loopback, non-local-iface

	origin := s.portalOriginForRequest(req)
	if origin == "http://attacker.example.com" {
		t.Fatalf("attacker-supplied Host was echoed into portal origin: %q", origin)
	}
	// Outbound-IP fallback is acceptable; empty is acceptable; anything but
	// the attacker Host is acceptable.
}

func TestPortalOriginForRequest_RemoteLoopbackUsesPortalHostname(t *testing.T) {
	t.Setenv("PORT", "8080")

	resolver := newServiceRemoteResolver(nil)
	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})

	s := &GinServer{remoteResolver: resolver}
	req := httptest.NewRequest("GET", "http://portal.example.com/", nil)
	req.Host = "portal.example.com"
	req.RemoteAddr = "127.0.0.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "https://portal.example.com" {
		t.Fatalf("expected origin https://portal.example.com, got %q", origin)
	}
}
