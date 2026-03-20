package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"piccolod/internal/services"
)

func TestContextAwareRemoteHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resolver := newServiceRemoteResolver(nil)
	resolver.SetRemoteBases("self-hosted", []remoteBase{
		{source: "self-hosted", portalHost: "portal.example.com", domain: "portal.example.com"},
	})
	resolver.SetRemoteBases("namek", []remoteBase{
		{source: "namek", portalHost: "slug.piccolospace.com", domain: "piccolospace.com"},
	})

	s := &GinServer{remoteResolver: resolver}

	portalHosts := resolver.PortalHosts() // ["portal.example.com", "slug.piccolospace.com"]

	ep := services.ServiceEndpoint{
		App:              "codeserver",
		Name:             "codeserver",
		DerivedHostLabel: "codeserver",
	}

	t.Run("request_via_self_hosted_portal", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "https://portal.example.com/api/v1/apps/codeserver", nil)
		c.Request.Host = "portal.example.com"

		got := s.contextAwareRemoteHost(c, ep, portalHosts)
		want := "codeserver.portal.example.com"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("request_via_namek_portal", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "https://slug.piccolospace.com/api/v1/apps/codeserver", nil)
		c.Request.Host = "slug.piccolospace.com"

		got := s.contextAwareRemoteHost(c, ep, portalHosts)
		want := "codeserver.slug.piccolospace.com"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("request_via_app_subdomain_resolves_to_parent_portal", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "https://codeserver.slug.piccolospace.com/api/v1/apps/codeserver", nil)
		c.Request.Host = "codeserver.slug.piccolospace.com"

		got := s.contextAwareRemoteHost(c, ep, portalHosts)
		want := "codeserver.slug.piccolospace.com"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("lan_access_falls_back_to_first_portal", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "http://192.168.1.100:8080/api/v1/apps/codeserver", nil)
		c.Request.Host = "192.168.1.100:8080"

		got := s.contextAwareRemoteHost(c, ep, portalHosts)
		// Falls back to first portal (self-hosted)
		want := "codeserver.portal.example.com"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("empty_derived_host_label", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "https://portal.example.com/api/v1/apps/demo", nil)
		c.Request.Host = "portal.example.com"

		noLabel := services.ServiceEndpoint{App: "demo", Name: "demo"}
		got := s.contextAwareRemoteHost(c, noLabel, portalHosts)
		if got != "" {
			t.Errorf("expected empty string for no DerivedHostLabel, got %q", got)
		}
	})

	t.Run("nil_resolver", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "https://portal.example.com/api/v1/apps/codeserver", nil)
		c.Request.Host = "portal.example.com"

		noResolver := &GinServer{remoteResolver: nil}
		got := noResolver.contextAwareRemoteHost(c, ep, portalHosts)
		if got != "" {
			t.Errorf("expected empty string for nil resolver, got %q", got)
		}
	})
}

func TestAllRemoteHostsForEndpoint(t *testing.T) {
	portalHosts := []string{"portal.example.com", "slug.piccolospace.com"}

	t.Run("returns_all_hosts", func(t *testing.T) {
		ep := services.ServiceEndpoint{DerivedHostLabel: "codeserver"}
		got := allRemoteHostsForEndpoint(ep, portalHosts)
		if len(got) != 2 {
			t.Fatalf("expected 2 hosts, got %d", len(got))
		}
		if got[0] != "codeserver.portal.example.com" {
			t.Errorf("got[0] = %q, want %q", got[0], "codeserver.portal.example.com")
		}
		if got[1] != "codeserver.slug.piccolospace.com" {
			t.Errorf("got[1] = %q, want %q", got[1], "codeserver.slug.piccolospace.com")
		}
	})

	t.Run("empty_derived_host_label", func(t *testing.T) {
		ep := services.ServiceEndpoint{}
		got := allRemoteHostsForEndpoint(ep, portalHosts)
		if got != nil {
			t.Errorf("expected nil for empty DerivedHostLabel, got %v", got)
		}
	})

	t.Run("no_portals", func(t *testing.T) {
		ep := services.ServiceEndpoint{DerivedHostLabel: "codeserver"}
		got := allRemoteHostsForEndpoint(ep, nil)
		if got != nil {
			t.Errorf("expected nil for no portals, got %v", got)
		}
	})

	t.Run("single_portal", func(t *testing.T) {
		ep := services.ServiceEndpoint{DerivedHostLabel: "blog"}
		got := allRemoteHostsForEndpoint(ep, []string{"portal.example.com"})
		if len(got) != 1 {
			t.Fatalf("expected 1 host, got %d", len(got))
		}
		if got[0] != "blog.portal.example.com" {
			t.Errorf("got %q, want %q", got[0], "blog.portal.example.com")
		}
	})
}
