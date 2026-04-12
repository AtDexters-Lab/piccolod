package oidc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscovery_AlwaysReturnsLocalAuth(t *testing.T) {
	h := NewDiscoveryHandler(DiscoveryConfig{
		StableIssuer: "https://piccolo-abc123.local",
		GetLocalHostname: func() string {
			return "piccolo-abc123.local"
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var doc OpenIDConfiguration
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := "http://piccolo-abc123.local/oauth/authorize"
	if doc.AuthorizationEndpoint != want {
		t.Errorf("authorization_endpoint = %q, want %q", doc.AuthorizationEndpoint, want)
	}

	// Back-channel endpoints should use stable issuer.
	if doc.TokenEndpoint != "https://piccolo-abc123.local/oauth/token" {
		t.Errorf("token_endpoint mismatch: %q", doc.TokenEndpoint)
	}
	if doc.JwksURI != "https://piccolo-abc123.local/oauth/jwks" {
		t.Errorf("jwks_uri mismatch: %q", doc.JwksURI)
	}
	if doc.Issuer != "https://piccolo-abc123.local" {
		t.Errorf("issuer mismatch: %q", doc.Issuer)
	}
}

func TestDiscovery_FallbackHostname(t *testing.T) {
	// When GetLocalHostname returns empty, fallback to "piccolo.local".
	h := NewDiscoveryHandler(DiscoveryConfig{
		StableIssuer:     "https://piccolo.local",
		GetLocalHostname: func() string { return "" },
	})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var doc OpenIDConfiguration
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if doc.AuthorizationEndpoint != "http://piccolo.local/oauth/authorize" {
		t.Errorf("authorization_endpoint fallback = %q, want http://piccolo.local/oauth/authorize", doc.AuthorizationEndpoint)
	}
}

func TestDiscovery_MdnsDisabled_IPFallback(t *testing.T) {
	// When mDNS is disabled, GetLocalHostname returns an IP address.
	// The discovery doc must use it as the authorization_endpoint host.
	h := NewDiscoveryHandler(DiscoveryConfig{
		StableIssuer:     "https://192.168.1.50",
		GetLocalHostname: func() string { return "192.168.1.50" },
	})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var doc OpenIDConfiguration
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := "http://192.168.1.50/oauth/authorize"
	if doc.AuthorizationEndpoint != want {
		t.Errorf("authorization_endpoint = %q, want %q", doc.AuthorizationEndpoint, want)
	}
}
