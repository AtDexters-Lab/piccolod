package services

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"piccolod/internal/api"
)

func TestListenerStrategyForPath_Conformance(t *testing.T) {
	t.Run("exact match", func(t *testing.T) {
		auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/health",
			Type:     "exact",
			Strategy: "public",
		}}}

		if got := listenerStrategyForPath(auth, "/health"); got != "public" {
			t.Fatalf("expected match")
		}
		if got := listenerStrategyForPath(auth, "/health/"); got != "protected" {
			t.Fatalf("expected no match")
		}
		if got := listenerStrategyForPath(auth, "/healthz"); got != "protected" {
			t.Fatalf("expected no match")
		}
	})

	t.Run("exact trailing slash", func(t *testing.T) {
		auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/health/",
			Type:     "exact",
			Strategy: "public",
		}}}

		if got := listenerStrategyForPath(auth, "/health/"); got != "public" {
			t.Fatalf("expected match")
		}
		if got := listenerStrategyForPath(auth, "/health"); got != "protected" {
			t.Fatalf("expected no match")
		}
	})

	t.Run("prefix match", func(t *testing.T) {
		auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/api/",
			Type:     "prefix",
			Strategy: "public",
		}}}

		for _, p := range []string{"/api/", "/api/users", "/api/users/123"} {
			if got := listenerStrategyForPath(auth, p); got != "public" {
				t.Fatalf("expected match for %q", p)
			}
		}
		for _, p := range []string{"/api", "/apikeys"} {
			if got := listenerStrategyForPath(auth, p); got != "protected" {
				t.Fatalf("expected no match for %q", p)
			}
		}
	})

	t.Run("prefix root", func(t *testing.T) {
		auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/",
			Type:     "prefix",
			Strategy: "public",
		}}}
		for _, p := range []string{"/", "/api", "/api/"} {
			if got := listenerStrategyForPath(auth, p); got != "public" {
				t.Fatalf("expected match for %q", p)
			}
		}
	})

	t.Run("pattern match", func(t *testing.T) {
		auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/static/*.js",
			Type:     "pattern",
			Strategy: "public",
		}}}

		if got := listenerStrategyForPath(auth, "/static/app.js"); got != "public" {
			t.Fatalf("expected match")
		}
		if got := listenerStrategyForPath(auth, "/static/css/app.css"); got != "protected" {
			t.Fatalf("expected no match")
		}
	})

	t.Run("pattern single-char", func(t *testing.T) {
		auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{
			Path:     "/api/v?/users",
			Type:     "pattern",
			Strategy: "public",
		}}}

		if got := listenerStrategyForPath(auth, "/api/v1/users"); got != "public" {
			t.Fatalf("expected match")
		}
		if got := listenerStrategyForPath(auth, "/api/v2/users"); got != "public" {
			t.Fatalf("expected match")
		}
		if got := listenerStrategyForPath(auth, "/api/v10/users"); got != "protected" {
			t.Fatalf("expected no match")
		}
	})
}

func TestNormalizeAndSetRequestPath_RFCExamples(t *testing.T) {
	t.Run("preserves trailing slash", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/users/", nil)
		got, code := normalizeAndSetRequestPath(r)
		if code != "" {
			t.Fatalf("unexpected error code %q", code)
		}
		if got != "/api/users/" || r.URL.Path != "/api/users/" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("decodes normal escapes", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/%75sers", nil)
		got, code := normalizeAndSetRequestPath(r)
		if code != "" {
			t.Fatalf("unexpected error code %q", code)
		}
		if got != "/api/users" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("rejects encoded slash", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/%2Fusers", nil)
		_, code := normalizeAndSetRequestPath(r)
		if code != "INVALID_PATH" {
			t.Fatalf("expected INVALID_PATH, got %q", code)
		}
	})

	t.Run("rejects double-encoding via %25", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/foo%252e%252e/bar", nil)
		_, code := normalizeAndSetRequestPath(r)
		if code != "INVALID_PATH" {
			t.Fatalf("expected INVALID_PATH, got %q", code)
		}
	})

	t.Run("rejects raw backslash", func(t *testing.T) {
		r := &http.Request{
			Method:     http.MethodGet,
			RequestURI: "/api\\users",
			URL:        &url.URL{Path: "/api\\users"},
			Header:     make(http.Header),
		}
		_, code := normalizeAndSetRequestPath(r)
		if code != "INVALID_PATH" {
			t.Fatalf("expected INVALID_PATH, got %q", code)
		}
	})

	t.Run("cleans dot segments", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/foo/../admin", nil)
		got, code := normalizeAndSetRequestPath(r)
		if code != "" {
			t.Fatalf("unexpected error code %q", code)
		}
		if got != "/admin" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("rejects traversal above root", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/foo/../../x", nil)
		_, code := normalizeAndSetRequestPath(r)
		if code != "PATH_INVALID" {
			t.Fatalf("expected PATH_INVALID, got %q", code)
		}
	})

	t.Run("rejects leading traversal", func(t *testing.T) {
		r := &http.Request{
			Method:     http.MethodGet,
			RequestURI: "/../admin",
			URL:        &url.URL{Path: "/../admin"},
			Header:     make(http.Header),
		}
		_, code := normalizeAndSetRequestPath(r)
		if code != "PATH_INVALID" {
			t.Fatalf("expected PATH_INVALID, got %q", code)
		}
	})
}
