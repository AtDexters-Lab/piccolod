package services

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func makeResponse(status int, contentType, body string, location string) *http.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if location != "" {
		header.Set("Location", location)
	}
	header.Set("Content-Length", itoa(len(body)))
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       &http.Request{URL: &url.URL{Path: "/auth/login"}},
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestOIDCRewrite_Layer1_LocationRewrite(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin: "http://piccolo-abc123.local",
		portalOrigin: "https://slug.piccolospace.com",
	}
	resp := makeResponse(302, "text/html", "", "http://piccolo-abc123.local/oauth/authorize?client_id=foo&state=bar")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got := resp.Header.Get("Location")
	want := "https://slug.piccolospace.com/oauth/authorize?client_id=foo&state=bar"
	if got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

func TestOIDCRewrite_Layer1_NonMatchingLocation(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin: "http://piccolo-abc123.local",
		portalOrigin: "https://slug.piccolospace.com",
	}
	resp := makeResponse(302, "text/html", "", "https://example.com/some/other/path")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	if got := resp.Header.Get("Location"); got != "https://example.com/some/other/path" {
		t.Errorf("Location should not be modified, got %q", got)
	}
}

func TestOIDCRewrite_Layer1_NotRedirect(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin: "http://piccolo-abc123.local",
		portalOrigin: "https://slug.piccolospace.com",
	}
	// 200 response — Location should not be touched even if matching
	resp := makeResponse(200, "text/html", "", "http://piccolo-abc123.local/oauth/authorize?client_id=foo")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	if got := resp.Header.Get("Location"); got != "http://piccolo-abc123.local/oauth/authorize?client_id=foo" {
		t.Errorf("Location should not be modified on 200, got %q", got)
	}
}

func TestOIDCRewrite_Layer2_BodyRewrite_JSON(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login", "/api/oauth/authorize"},
	}
	body := `{"url":"http://piccolo-abc123.local/oauth/authorize?client_id=foo&state=bar"}`
	resp := makeResponse(200, "application/json", body, "")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	newBody, _ := io.ReadAll(resp.Body)
	wantBody := `{"url":"https://slug.piccolospace.com/oauth/authorize?client_id=foo&state=bar"}`
	if string(newBody) != wantBody {
		t.Errorf("body = %q, want %q", string(newBody), wantBody)
	}
	if resp.ContentLength != int64(len(wantBody)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(wantBody))
	}
	if resp.Header.Get("Transfer-Encoding") != "" {
		t.Errorf("Transfer-Encoding should be cleared")
	}
}

func TestOIDCRewrite_Layer2_NonMatchingPath(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	body := `{"url":"http://piccolo-abc123.local/oauth/authorize"}`
	resp := makeResponse(200, "application/json", body, "")
	resp.Request.URL.Path = "/some/other/path"

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body should not be modified for non-matching path, got %q", string(got))
	}
}

func TestOIDCRewrite_Layer2_ExactMatchOnly(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	body := `{"url":"http://piccolo-abc123.local/oauth/authorize"}`
	resp := makeResponse(200, "application/json", body, "")
	// Trailing slash — should NOT match (exact equality)
	resp.Request.URL.Path = "/auth/login/"

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body should not be modified for non-exact match, got %q", string(got))
	}
}

func TestOIDCRewrite_Layer2_BinaryContentType(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	body := `binary content with http://piccolo-abc123.local/oauth/authorize embedded`
	resp := makeResponse(200, "application/octet-stream", body, "")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("binary content should not be modified, got %q", string(got))
	}
}

func TestOIDCRewrite_Layer2_CompressedSkipped(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	body := `compressed payload`
	resp := makeResponse(200, "application/json", body, "")
	resp.Header.Set("Content-Encoding", "gzip")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("compressed body should not be touched, got %q", string(got))
	}
}

func TestOIDCRewrite_Layer2_OversizedBody_ContentLengthKnown(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	// Body larger than 1MB with Content-Length set — should skip without consuming.
	big := bytes.Repeat([]byte("x"), oidcAuthorizeRewriteMaxBody+100)
	resp := &http.Response{
		StatusCode:    200,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(big)),
		ContentLength: int64(len(big)),
		Request:       &http.Request{URL: &url.URL{Path: "/auth/login"}},
	}

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	// Body must be fully preserved (no truncation).
	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(big) {
		t.Errorf("oversized body truncated: got %d bytes, want %d", len(got), len(big))
	}
	if !bytes.Equal(got, big) {
		t.Error("oversized body content corrupted")
	}
}

func TestOIDCRewrite_Layer2_OversizedBody_Chunked(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	// Body larger than 1MB with ContentLength=-1 (chunked) — must splice without truncation.
	big := bytes.Repeat([]byte("y"), oidcAuthorizeRewriteMaxBody+200)
	resp := &http.Response{
		StatusCode:    200,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(big)),
		ContentLength: -1, // chunked
		Request:       &http.Request{URL: &url.URL{Path: "/auth/login"}},
	}

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(big) {
		t.Errorf("chunked oversized body truncated: got %d bytes, want %d", len(got), len(big))
	}
	if !bytes.Equal(got, big) {
		t.Error("chunked oversized body content corrupted")
	}
}

func TestOIDCRewrite_Layer1_PrefixBoundary(t *testing.T) {
	// Layer 1 must NOT match URLs that share the prefix but extend differently
	// (e.g., .../authorize_token, .../authorizefoo).
	snap := &oidcRewriteSnapshot{
		issuerOrigin: "http://piccolo-abc123.local",
		portalOrigin: "https://slug.piccolospace.com",
	}
	// Should NOT be rewritten (not a real authorize URL)
	resp := makeResponse(302, "text/html", "", "http://piccolo-abc123.local/oauth/authorize_token")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	if got := resp.Header.Get("Location"); got != "http://piccolo-abc123.local/oauth/authorize_token" {
		t.Errorf("Layer 1 should not match prefix boundary, got %q", got)
	}
}

func TestOIDCRewrite_Layer1_ExactMatch(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin: "http://piccolo-abc123.local",
		portalOrigin: "https://slug.piccolospace.com",
	}
	// Exact URL with no query — should be rewritten
	resp := makeResponse(302, "text/html", "", "http://piccolo-abc123.local/oauth/authorize")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	want := "https://slug.piccolospace.com/oauth/authorize"
	if got := resp.Header.Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// errReader returns the given error after reading n bytes successfully.
type errReader struct {
	data []byte
	n    int
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.n >= len(r.data) {
		return 0, r.err
	}
	available := len(r.data) - r.n
	if available > len(p) {
		available = len(p)
	}
	copy(p, r.data[r.n:r.n+available])
	r.n += available
	return available, nil
}

func TestOIDCRewrite_Layer2_BodyReadError(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	// Reader that returns 10 bytes then errors. The rewrite must splice
	// the partial read back into resp.Body so the downstream client gets
	// the original payload (or at least is not blocked at length=10 with
	// stale Content-Length).
	partial := []byte("partial...")
	body := &errReader{data: partial, err: io.ErrUnexpectedEOF}
	resp := &http.Response{
		StatusCode:    200,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(body),
		ContentLength: -1,
		Request:       &http.Request{URL: &url.URL{Path: "/auth/login"}},
	}

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	// The spliceCloser should preserve the partial read; further Read calls
	// will surface the same error. We must not return a 502 from ModifyResponse.
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, partial) {
		t.Errorf("body mismatch after splice: got %q, want %q", string(got), string(partial))
	}
}

func TestOIDCRewrite_Layer2_EmptyBody(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	resp := makeResponse(204, "application/json", "", "")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if len(got) != 0 {
		t.Errorf("empty body should remain empty, got %q", string(got))
	}
}

func TestOIDCRewrite_Layer2_ZeroReplacements(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	body := `{"foo":"bar"}` // no issuer URL present
	resp := makeResponse(200, "application/json", body, "")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body without matches should be passed through unchanged, got %q", string(got))
	}
}

func TestOIDCRewrite_Layer2_PrefixBoundary(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	// A URL that extends the authorize prefix with a letter (e.g., _token)
	// must NOT be rewritten — mirrors Layer 1's prefix boundary guarantee.
	body := `{"a":"http://piccolo-abc123.local/oauth/authorize_token","b":"http://piccolo-abc123.local/oauth/authorize"}`
	resp := makeResponse(200, "application/json", body, "")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	want := `{"a":"http://piccolo-abc123.local/oauth/authorize_token","b":"https://slug.piccolospace.com/oauth/authorize"}`
	if string(got) != want {
		t.Errorf("body = %q, want %q", string(got), want)
	}
}

func TestOIDCRewrite_Layer2_BoundaryVariants(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	cases := []struct {
		name      string
		body      string
		wantCount int // number of expected replacements
	}{
		{"with query", `"http://piccolo-abc123.local/oauth/authorize?x=1"`, 1},
		{"with fragment", `"http://piccolo-abc123.local/oauth/authorize#frag"`, 1},
		{"with subpath", `"http://piccolo-abc123.local/oauth/authorize/sub"`, 1},
		{"exact end", `http://piccolo-abc123.local/oauth/authorize`, 1},
		{"quoted", `"http://piccolo-abc123.local/oauth/authorize"`, 1},
		{"not a boundary (letter)", `"http://piccolo-abc123.local/oauth/authorizefoo"`, 0},
		{"not a boundary (digit)", `"http://piccolo-abc123.local/oauth/authorize0"`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := makeResponse(200, "application/json", tc.body, "")
			rewriteOIDCAuthorizeResponse(resp, snap, "test-app")
			got, _ := io.ReadAll(resp.Body)
			replaced := strings.Count(string(got), "https://slug.piccolospace.com/oauth/authorize")
			if replaced != tc.wantCount {
				t.Errorf("got %d replacements, want %d; body=%q", replaced, tc.wantCount, string(got))
			}
		})
	}
}

func TestOIDCRewrite_Layer2_TextHTML(t *testing.T) {
	snap := &oidcRewriteSnapshot{
		issuerOrigin:   "http://piccolo-abc123.local",
		portalOrigin:   "https://slug.piccolospace.com",
		authorizePaths: []string{"/auth/login"},
	}
	body := `<a href="http://piccolo-abc123.local/oauth/authorize?client_id=test">Login</a>`
	resp := makeResponse(200, "text/html; charset=utf-8", body, "")

	rewriteOIDCAuthorizeResponse(resp, snap, "test-app")

	got, _ := io.ReadAll(resp.Body)
	want := `<a href="https://slug.piccolospace.com/oauth/authorize?client_id=test">Login</a>`
	if string(got) != want {
		t.Errorf("body = %q, want %q", string(got), want)
	}
}
