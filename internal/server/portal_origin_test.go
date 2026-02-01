package server

import (
	"net/http/httptest"
	"testing"

	"piccolod/internal/remote"
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
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "piccolo.local:35080"
	req.RemoteAddr = "192.0.2.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "https://piccolo.local" {
		t.Fatalf("expected origin https://piccolo.local, got %q", origin)
	}
}

func TestPortalOriginForRequest_RemoteLoopbackUsesPortalHostname(t *testing.T) {
	t.Setenv("PICCOLO_REMOTE_FAKE_ACME", "1")
	t.Setenv("PORT", "8080")

	baseDir := t.TempDir()
	rm, err := remote.NewManager(baseDir)
	if err != nil {
		t.Fatalf("remote mgr: %v", err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	if err := rm.Configure(remote.ConfigureRequest{
		Endpoint:       "wss://nexus.example.com/connect",
		DeviceSecret:   "secret",
		PortalHostname: "portal.example.com",
	}); err != nil {
		t.Fatalf("configure remote: %v", err)
	}

	s := &GinServer{remoteManager: rm}
	req := httptest.NewRequest("GET", "http://portal.example.com/", nil)
	req.Host = "portal.example.com"
	req.RemoteAddr = "127.0.0.1:1234"

	origin := s.portalOriginForRequest(req)
	if origin != "https://portal.example.com" {
		t.Fatalf("expected origin https://portal.example.com, got %q", origin)
	}
}
