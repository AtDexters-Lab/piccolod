package server

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestPortalOriginForRequest_UsesPortalPortNotListenerPort(t *testing.T) {
	t.Setenv("PORT", "8080")

	s := &GinServer{}
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "http://piccolo.local:8080" {
		t.Fatalf("expected origin http://piccolo.local:8080, got %q", origin)
	}
}

func TestPortalOriginForRequest_OmitsDefaultHTTPPort(t *testing.T) {
	t.Setenv("PORT", "80")

	s := &GinServer{}
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "http://piccolo.local" {
		t.Fatalf("expected origin http://piccolo.local, got %q", origin)
	}
}

func TestPortalOriginForRequest_HTTPSDoesNotAppendPort80(t *testing.T) {
	t.Setenv("PORT", "80")

	s := &GinServer{}
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	// Use the secure loopback context key (the trusted TLS indicator) instead of
	// X-Forwarded-Proto which is client-spoofable and no longer trusted.
	ctx := context.WithValue(req.Context(), secureContextKeyInstance, true)
	req = req.WithContext(ctx)
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "https://piccolo.local" {
		t.Fatalf("expected origin https://piccolo.local, got %q", origin)
	}
}

func TestPortalOriginForRequest_IgnoresSpoofedXForwardedProto(t *testing.T) {
	t.Setenv("PORT", "80")

	s := &GinServer{}
	req := httptest.NewRequest("GET", "http://piccolo.local:35080/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "http://piccolo.local" {
		t.Fatalf("expected spoofed X-Forwarded-Proto to be ignored, got %q", origin)
	}
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
