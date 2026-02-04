package services

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShouldPartitionCookies(t *testing.T) {
	tests := []struct {
		name   string
		setup  func() *http.Request
		expect bool
	}{
		{
			name:   "nil request",
			setup:  func() *http.Request { return nil },
			expect: false,
		},
		{
			name: "plain HTTP request",
			setup: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://homebox-piccolo-xyz.local/", nil)
			},
			expect: false,
		},
		{
			name: "HTTPS host-based app access (should partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				return r
			},
			expect: true,
		},
		{
			name: "HTTPS port-based access (same-site, no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo-xyz.local:8080/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo-xyz.local:8080"
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS portal hostname (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo-xyz.local"
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS piccolo.local (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo.local"
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS localhost (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://localhost/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "localhost"
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS IP address (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://192.168.1.100/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "192.168.1.100"
				return r
			},
			expect: false,
		},
		{
			name: "TLS via hint (host-based app)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://homebox-piccolo-xyz.local/", nil)
				r.Host = "homebox-piccolo-xyz.local"
				r = r.WithContext(context.WithValue(r.Context(), hintContextKey{}, connectionHint{isTLS: true}))
				return r
			},
			expect: true,
		},
		{
			name: "HTTPS remote subdomain (same-site, no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox.example.com/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox.example.com"
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS remote with port (same-site, no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://blog.my-site.com/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "blog.my-site.com"
				return r
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup()
			got := shouldPartitionCookies(r)
			if got != tt.expect {
				t.Errorf("shouldPartitionCookies() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestHasCookieAttribute(t *testing.T) {
	tests := []struct {
		sc     string
		attr   string
		expect bool
	}{
		{"session=abc; Secure; HttpOnly", "Secure", true},
		{"session=abc; Secure; HttpOnly", "secure", true},
		{"session=abc; HttpOnly", "Secure", false},
		{"session=abc; SameSite=Lax; HttpOnly", "SameSite=Lax", true},
		{"session=abc; SameSite=Lax; HttpOnly", "SameSite", false}, // key=value doesn't match bare key
		{"session=abc; Partitioned", "Partitioned", true},
		{"session=abc; partitioned", "Partitioned", true},
	}
	for _, tt := range tests {
		t.Run(tt.sc+"_"+tt.attr, func(t *testing.T) {
			got := hasCookieAttribute(tt.sc, tt.attr)
			if got != tt.expect {
				t.Errorf("hasCookieAttribute(%q, %q) = %v, want %v", tt.sc, tt.attr, got, tt.expect)
			}
		})
	}
}

func TestRemoveCookieAttribute(t *testing.T) {
	tests := []struct {
		name   string
		sc     string
		attr   string
		expect string
	}{
		{
			name:   "remove SameSite=Lax",
			sc:     "session=abc; Path=/; SameSite=Lax; HttpOnly",
			attr:   "samesite",
			expect: "session=abc; Path=/; HttpOnly",
		},
		{
			name:   "remove SameSite=None",
			sc:     "session=abc; Path=/; SameSite=None; Secure",
			attr:   "samesite",
			expect: "session=abc; Path=/; Secure",
		},
		{
			name:   "no SameSite present",
			sc:     "session=abc; Path=/; HttpOnly",
			attr:   "samesite",
			expect: "session=abc; Path=/; HttpOnly",
		},
		{
			name:   "remove Secure flag",
			sc:     "session=abc; Secure; HttpOnly",
			attr:   "secure",
			expect: "session=abc; HttpOnly",
		},
		{
			name:   "preserves cookie named like attribute",
			sc:     "secure=myvalue; Path=/; HttpOnly",
			attr:   "secure",
			expect: "secure=myvalue; Path=/; HttpOnly",
		},
		{
			name:   "preserves cookie named samesite",
			sc:     "samesite=test; Path=/; SameSite=Lax",
			attr:   "samesite",
			expect: "samesite=test; Path=/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := removeCookieAttribute(tt.sc, tt.attr)
			if got != tt.expect {
				t.Errorf("removeCookieAttribute(%q, %q) = %q, want %q", tt.sc, tt.attr, got, tt.expect)
			}
		})
	}
}

func TestEnsurePartitionedAttributes(t *testing.T) {
	tests := []struct {
		name   string
		sc     string
		checks []string // substrings that must be present
		absent []string // substrings that must NOT be present
	}{
		{
			name:   "basic cookie with SameSite=Lax",
			sc:     "session=abc; Path=/; SameSite=Lax; HttpOnly",
			checks: []string{"SameSite=None", "Partitioned", "Secure"},
			absent: []string{"SameSite=Lax"},
		},
		{
			name:   "cookie already has Secure",
			sc:     "token=xyz; Path=/; Secure; HttpOnly",
			checks: []string{"SameSite=None", "Partitioned", "Secure"},
		},
		{
			name:   "cookie with no SameSite or Secure",
			sc:     "id=123; Path=/; HttpOnly",
			checks: []string{"SameSite=None", "Partitioned", "Secure"},
		},
		{
			name:   "cookie with SameSite=Strict",
			sc:     "csrftoken=xyz; SameSite=Strict",
			checks: []string{"SameSite=None", "Partitioned", "Secure"},
			absent: []string{"SameSite=Strict"},
		},
		{
			name:   "idempotent: already has Partitioned",
			sc:     "session=abc; Path=/; Secure; SameSite=None; Partitioned",
			checks: []string{"SameSite=None", "Partitioned", "Secure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ensurePartitionedAttributes(tt.sc)
			for _, want := range tt.checks {
				if !strings.Contains(got, want) {
					t.Errorf("ensurePartitionedAttributes(%q) = %q, missing %q", tt.sc, got, want)
				}
			}
			for _, nowant := range tt.absent {
				if strings.Contains(got, nowant) {
					t.Errorf("ensurePartitionedAttributes(%q) = %q, should not contain %q", tt.sc, got, nowant)
				}
			}
			// Verify no duplicate Partitioned attribute
			count := strings.Count(got, "Partitioned")
			if count != 1 {
				t.Errorf("ensurePartitionedAttributes(%q) = %q, has %d occurrences of Partitioned (want 1)", tt.sc, got, count)
			}
		})
	}
}

func TestProxyContextPartitionCookies(t *testing.T) {
	// Default context returns false
	ctx := context.Background()
	if proxyContextPartitionCookies(ctx) {
		t.Error("expected false for context without partition key")
	}

	// Context with true
	ctx = context.WithValue(ctx, proxyPartitionCookiesContextKey{}, true)
	if !proxyContextPartitionCookies(ctx) {
		t.Error("expected true for context with partition=true")
	}

	// Context with false
	ctx = context.WithValue(context.Background(), proxyPartitionCookiesContextKey{}, false)
	if proxyContextPartitionCookies(ctx) {
		t.Error("expected false for context with partition=false")
	}
}

func TestWithProxyContext_PartitionCookies(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)

	// partition=true
	r2 := withProxyContext(r, "homebox", "homebox-piccolo-xyz.local", false, true)
	if !proxyContextPartitionCookies(r2.Context()) {
		t.Error("expected partitionCookies=true in context")
	}

	// partition=false
	r3 := withProxyContext(r, "homebox", "piccolo-xyz.local", true, false)
	if proxyContextPartitionCookies(r3.Context()) {
		t.Error("expected partitionCookies=false in context")
	}
}
