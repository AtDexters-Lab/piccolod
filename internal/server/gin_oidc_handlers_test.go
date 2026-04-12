package server

import "testing"

func TestIsCustomSchemeRedirectURI(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want bool
	}{
		// Custom schemes — should route to handleCustomSchemeAuthorize
		{"immich custom scheme", "app.immich:///oauth-callback", true},
		{"vendor scheme", "myapp://callback", true},
		{"vendor scheme with host", "io.example.app://oauth/callback", true},
		{"intent scheme", "intent://callback#Intent;scheme=myapp;end", true},

		// Standard schemes — should pass through to zitadel library
		{"http LAN", "http://piccolo.local/auth/login", false},
		{"https remote", "https://app.example.com/auth/callback", false},
		{"http localhost", "http://localhost:8080/callback", false},
		{"http loopback", "http://127.0.0.1:8080/callback", false},
		{"https with port", "https://myhost:8443/cb", false},

		// Edge cases
		{"empty string", "", false},

		// Schemes that look similar but aren't http/https
		{"httpx", "httpx://example.com", true},
		{"https-prefix-scheme", "httpsx://example.com", true},

		// Case-insensitive: uppercase HTTP/HTTPS should not be classified as custom
		{"uppercase HTTP", "HTTP://example.com/cb", false},
		{"uppercase HTTPS", "HTTPS://example.com/cb", false},
		{"mixed case Http", "Http://example.com/cb", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isCustomSchemeRedirectURI(tc.uri)
			if got != tc.want {
				t.Errorf("isCustomSchemeRedirectURI(%q) = %v, want %v", tc.uri, got, tc.want)
			}
		})
	}
}
