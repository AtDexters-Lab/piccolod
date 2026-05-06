package l7

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// loopbackContext attaches a loopback LocalAddr to a request context, mirroring
// what http.Server does for a connection accepted on 127.0.0.1.
func loopbackContext(ctx context.Context) context.Context {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}
	return context.WithValue(ctx, http.LocalAddrContextKey, net.Addr(addr))
}

// nonLoopbackContext attaches a non-loopback LocalAddr.
func nonLoopbackContext(ctx context.Context) context.Context {
	addr := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 8080}
	return context.WithValue(ctx, http.LocalAddrContextKey, net.Addr(addr))
}

func TestForwardedScrub_stripsOnNonLoopback(t *testing.T) {
	mw := ForwardedScrub()
	var observed http.Header
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.Header.Clone()
	})
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(nonLoopbackContext(r.Context()))
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Port", "X-Real-IP"} {
		r.Header.Set(h, "spoofed")
	}

	mw(next).ServeHTTP(httptest.NewRecorder(), r)

	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Port", "X-Real-IP"} {
		if observed.Get(h) != "" {
			t.Errorf("non-loopback: header %s should be stripped, got %q", h, observed.Get(h))
		}
	}
}

func TestForwardedScrub_preservesOnLoopback(t *testing.T) {
	mw := ForwardedScrub()
	var observed http.Header
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.Header.Clone()
	})
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(loopbackContext(r.Context()))
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	r.Header.Set("X-Forwarded-Proto", "https")

	mw(next).ServeHTTP(httptest.NewRecorder(), r)

	if observed.Get("X-Forwarded-For") != "203.0.113.5" {
		t.Errorf("loopback: X-Forwarded-For should be preserved; got %q", observed.Get("X-Forwarded-For"))
	}
	if observed.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("loopback: X-Forwarded-Proto should be preserved; got %q", observed.Get("X-Forwarded-Proto"))
	}
}

func TestForwardedScrub_noLocalAddrTreatedAsUntrusted(t *testing.T) {
	// Defensive: missing LocalAddrContextKey defaults to untrusted (strip).
	mw := ForwardedScrub()
	var observed http.Header
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = r.Header.Clone()
	})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "spoofed")

	mw(next).ServeHTTP(httptest.NewRecorder(), r)

	if observed.Get("X-Forwarded-For") != "" {
		t.Errorf("missing LocalAddr should be treated as untrusted; got %q", observed.Get("X-Forwarded-For"))
	}
}

func TestPathNormalize_passesThroughOnSuccess(t *testing.T) {
	called := false
	normalize := func(r *http.Request) (string, string) {
		r.URL.Path = "/normalized"
		return "/normalized", ""
	}
	writeErr := func(_ http.ResponseWriter, _ int, _, _ string) {
		t.Fatal("writeErr should not be called on success")
	}
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/normalized" {
			t.Errorf("expected normalized path; got %q", r.URL.Path)
		}
	})

	mw := PathNormalize(normalize, writeErr)
	mw(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/foo", nil))

	if !called {
		t.Fatal("next handler should have been called")
	}
}

func TestPathNormalize_invalidPathError(t *testing.T) {
	normalize := func(_ *http.Request) (string, string) { return "", "INVALID_PATH" }
	var (
		gotStatus  int
		gotMessage string
		gotCode    string
	)
	writeErr := func(_ http.ResponseWriter, status int, message, code string) {
		gotStatus = status
		gotMessage = message
		gotCode = code
	}
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("next handler should NOT run after path-normalize error")
	})

	mw := PathNormalize(normalize, writeErr)
	mw(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/foo", nil))

	if gotStatus != http.StatusBadRequest {
		t.Errorf("status: want 400; got %d", gotStatus)
	}
	if gotMessage != "invalid_request_path" {
		t.Errorf("message: want invalid_request_path; got %q", gotMessage)
	}
	if gotCode != "INVALID_PATH" {
		t.Errorf("code: want INVALID_PATH; got %q", gotCode)
	}
}

func TestPathNormalize_otherError(t *testing.T) {
	normalize := func(_ *http.Request) (string, string) { return "", "PATH_INVALID" }
	var gotMessage, gotCode string
	writeErr := func(_ http.ResponseWriter, _ int, message, code string) {
		gotMessage = message
		gotCode = code
	}

	mw := PathNormalize(normalize, writeErr)
	mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/foo", nil))

	if gotMessage != "path_normalization_failed" {
		t.Errorf("message: want path_normalization_failed; got %q", gotMessage)
	}
	if gotCode != "PATH_INVALID" {
		t.Errorf("code: want PATH_INVALID; got %q", gotCode)
	}
}

func TestForwardHeaders_callsApplyThenNext(t *testing.T) {
	applyCalled := false
	nextCalled := false
	apply := func(r *http.Request) {
		applyCalled = true
		// Verify next has not run yet — order must be apply→next.
		if nextCalled {
			t.Error("next ran before apply")
		}
		r.Header.Set("X-Test", "applied")
	}
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		nextCalled = true
		if r.Header.Get("X-Test") != "applied" {
			t.Error("apply did not run before next")
		}
	})

	mw := ForwardHeaders(apply)
	mw(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if !applyCalled || !nextCalled {
		t.Fatalf("apply=%v next=%v; both should be true", applyCalled, nextCalled)
	}
}

func TestSecurityHeadersResponse_stripsAndRewrites(t *testing.T) {
	mod := SecurityHeadersResponse()
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Frame-Options", "DENY")
	resp.Header.Set("Cross-Origin-Resource-Policy", "same-origin")
	resp.Header.Set("Cross-Origin-Embedder-Policy", "require-corp")
	resp.Header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; script-src 'self'")

	if err := mod(resp); err != nil {
		t.Fatalf("modifier err: %v", err)
	}

	for _, h := range []string{"X-Frame-Options", "Cross-Origin-Resource-Policy", "Cross-Origin-Embedder-Policy"} {
		if resp.Header.Get(h) != "" {
			t.Errorf("%s should be deleted; got %q", h, resp.Header.Get(h))
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("CSP should not be empty after frame-ancestors strip")
	}
	if contains(csp, "frame-ancestors") {
		t.Errorf("frame-ancestors should be stripped; got %q", csp)
	}
	if !contains(csp, "default-src") || !contains(csp, "script-src") {
		t.Errorf("other CSP directives should be preserved; got %q", csp)
	}
}

func TestSecurityHeadersResponse_noCSPLeavesItAlone(t *testing.T) {
	mod := SecurityHeadersResponse()
	resp := &http.Response{Header: http.Header{}}
	if err := mod(resp); err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Errorf("absent CSP should stay absent; got %q", resp.Header.Get("Content-Security-Policy"))
	}
}

func TestEmbeddedMarkerResponse_addsWhenNeeded(t *testing.T) {
	mod := EmbeddedMarkerResponse(
		func(_ *http.Request) bool { return true },
		func() string { return "piccolo_embed=1; Path=/; HttpOnly" },
	)
	resp := &http.Response{Header: http.Header{}, Request: httptest.NewRequest("GET", "/", nil)}
	if err := mod(resp); err != nil {
		t.Fatalf("err: %v", err)
	}
	got := resp.Header.Values("Set-Cookie")
	if len(got) != 1 || got[0] != "piccolo_embed=1; Path=/; HttpOnly" {
		t.Errorf("expected single marker cookie; got %v", got)
	}
}

func TestEmbeddedMarkerResponse_skipsWhenNotNeeded(t *testing.T) {
	mod := EmbeddedMarkerResponse(
		func(_ *http.Request) bool { return false },
		func() string {
			t.Fatal("markerCookie should not be called when needsMarker=false")
			return ""
		},
	)
	resp := &http.Response{Header: http.Header{}, Request: httptest.NewRequest("GET", "/", nil)}
	if err := mod(resp); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("no marker should be added; got %v", got)
	}
}

func TestEmbeddedMarkerResponse_handlesNilRequest(t *testing.T) {
	// Defensive: response with nil Request shouldn't panic.
	mod := EmbeddedMarkerResponse(
		func(_ *http.Request) bool { t.Fatal("should not be called for nil Request"); return true },
		func() string { return "" },
	)
	resp := &http.Response{Header: http.Header{}, Request: nil}
	if err := mod(resp); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
