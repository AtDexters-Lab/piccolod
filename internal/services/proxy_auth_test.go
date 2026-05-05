package services

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/l7"
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
			name: "HTTPS host-based app access in iframe (should partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: true,
		},
		{
			name: "HTTPS host-based app in top-level navigation (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "document")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS host-based XHR (no partition, not iframe dest)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/api/data", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS host-based without Sec-Fetch-Dest (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS port-based access in iframe (same-site, no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo-xyz.local:8080/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo-xyz.local:8080"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS portal hostname in iframe (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS piccolo.local in iframe (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo.local"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS localhost in iframe (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://localhost/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "localhost"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS IP address in iframe (no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://192.168.1.100/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "192.168.1.100"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "TLS via hint (host-based app, iframe)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://homebox-piccolo-xyz.local/", nil)
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				r = r.WithContext(middleware.ContextWithHint(r.Context(), middleware.Hint{IsTLS: true}))
				return r
			},
			expect: true,
		},
		{
			name: "HTTPS remote subdomain in iframe (same-site, no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox.example.com/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox.example.com"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS remote with port in iframe (same-site, no partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://blog.my-site.com/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "blog.my-site.com"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "HTTPS XHR with embedded marker cookie (should partition)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/api/data", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				return r
			},
			expect: true,
		},
		{
			name: "XHR with marker but no TLS (false)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://homebox-piccolo-xyz.local/api/data", nil)
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				return r
			},
			expect: false,
		},
		{
			name: "XHR with marker but port-based (false)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo-xyz.local:8080/api/data", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo-xyz.local:8080"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				return r
			},
			expect: false,
		},
		{
			name: "XHR with marker but non-.local host (false)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox.example.com/api/data", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox.example.com"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				return r
			},
			expect: false,
		},
		{
			name: "XHR with marker in middle of Cookie header (true)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/api/data", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				r.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				r.AddCookie(&http.Cookie{Name: "other", Value: "xyz"})
				return r
			},
			expect: true,
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
			got := l7.HasCookieAttribute(tt.sc, tt.attr)
			if got != tt.expect {
				t.Errorf("l7.HasCookieAttribute(%q, %q) = %v, want %v", tt.sc, tt.attr, got, tt.expect)
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
			got := l7.RemoveCookieAttribute(tt.sc, tt.attr)
			if got != tt.expect {
				t.Errorf("l7.RemoveCookieAttribute(%q, %q) = %q, want %q", tt.sc, tt.attr, got, tt.expect)
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
			got := l7.EnsurePartitionedAttributes(tt.sc)
			for _, want := range tt.checks {
				if !strings.Contains(got, want) {
					t.Errorf("l7.EnsurePartitionedAttributes(%q) = %q, missing %q", tt.sc, got, want)
				}
			}
			for _, nowant := range tt.absent {
				if strings.Contains(got, nowant) {
					t.Errorf("l7.EnsurePartitionedAttributes(%q) = %q, should not contain %q", tt.sc, got, nowant)
				}
			}
			// Verify no duplicate Partitioned attribute
			count := strings.Count(got, "Partitioned")
			if count != 1 {
				t.Errorf("l7.EnsurePartitionedAttributes(%q) = %q, has %d occurrences of Partitioned (want 1)", tt.sc, got, count)
			}
		})
	}
}

func TestWithProxyContext_PartitionCookies(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)

	// partition=true
	r2 := l7.SetProxyContext(r, "homebox", "homebox-piccolo-xyz.local", false, true, false)
	if !l7.PartitionCookiesFromContext(r2.Context()) {
		t.Error("expected partitionCookies=true in context")
	}

	// partition=false
	r3 := l7.SetProxyContext(r, "homebox", "piccolo-xyz.local", true, false, false)
	if l7.PartitionCookiesFromContext(r3.Context()) {
		t.Error("expected partitionCookies=false in context")
	}
}

func TestWithProxyContext_NeedsMarker(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)

	r2 := l7.SetProxyContext(r, "homebox", "homebox-piccolo-xyz.local", false, true, true)
	if !l7.NeedsMarkerFromContext(r2.Context()) {
		t.Error("expected needsMarker=true in context")
	}

	r3 := l7.SetProxyContext(r, "homebox", "homebox-piccolo-xyz.local", false, true, false)
	if l7.NeedsMarkerFromContext(r3.Context()) {
		t.Error("expected needsMarker=false in context")
	}
}

func TestHasEmbeddedMarker(t *testing.T) {
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
			name: "no cookies",
			setup: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
			},
			expect: false,
		},
		{
			name: "marker present",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				return r
			},
			expect: true,
		},
		{
			name: "marker in middle of cookie header",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				r.AddCookie(&http.Cookie{Name: "other", Value: "xyz"})
				return r
			},
			expect: true,
		},
		{
			name: "different piccolo cookie (not marker)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.AddCookie(&http.Cookie{Name: l7.SessionCookieName, Value: "abc"})
				return r
			},
			expect: false,
		},
		{
			name: "marker with wrong value",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "wrong"})
				return r
			},
			expect: false,
		},
		{
			name: "marker with empty value",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: ""})
				return r
			},
			expect: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := l7.HasEmbeddedMarker(tt.setup())
			if got != tt.expect {
				t.Errorf("l7.HasEmbeddedMarker() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestNeedsEmbeddedMarker(t *testing.T) {
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
			name: "iframe + CHIPS eligible → true",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: true,
		},
		{
			name: "document dest → false",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "document")
				return r
			},
			expect: false,
		},
		{
			name: "XHR with marker cookie → false (not iframe dest)",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox-piccolo-xyz.local/api/data", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "empty")
				r.AddCookie(&http.Cookie{Name: l7.EmbeddedCookieName, Value: "1"})
				return r
			},
			expect: false,
		},
		{
			name: "iframe + no TLS → false",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://homebox-piccolo-xyz.local/", nil)
				r.Host = "homebox-piccolo-xyz.local"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "iframe + port-based → false",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://piccolo-xyz.local:8080/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "piccolo-xyz.local:8080"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
		{
			name: "iframe + non-.local host → false",
			setup: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://homebox.example.com/", nil)
				r.TLS = &tls.ConnectionState{}
				r.Host = "homebox.example.com"
				r.Header.Set("Sec-Fetch-Dest", "iframe")
				return r
			},
			expect: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsEmbeddedMarker(tt.setup())
			if got != tt.expect {
				t.Errorf("needsEmbeddedMarker() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestEmbeddedMarkerSetCookie(t *testing.T) {
	sc := l7.EmbeddedMarkerSetCookie()
	required := []string{
		l7.EmbeddedCookieName + "=1",
		"Path=/",
		"HttpOnly",
		"Secure",
		"SameSite=None",
		"Partitioned",
	}
	for _, want := range required {
		if !strings.Contains(sc, want) {
			t.Errorf("l7.EmbeddedMarkerSetCookie() = %q, missing %q", sc, want)
		}
	}
}
