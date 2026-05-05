package l7

import (
	"net/http"
	"regexp"

	"piccolod/internal/services/middleware"
)

// frameAncestorsRe matches the `frame-ancestors` directive in a CSP header
// value so it can be stripped (the proxy applies its own frame-ancestors
// allowlist via the request-side securityHeaders middleware in proxy.go).
var frameAncestorsRe = regexp.MustCompile(`(?i)frame-ancestors\s+[^;]+(; ?)?`)

// SecurityHeadersResponse strips backend security headers that conflict with
// proxy-level policies. Today's behavior at proxy.go:466–478:
//
//   - Delete X-Frame-Options (proxy enforces frame-ancestors instead).
//   - Delete Cross-Origin-Resource-Policy and Cross-Origin-Embedder-Policy
//     (proxy applies consistent CORP/COEP).
//   - Strip the frame-ancestors directive from any backend Content-Security-Policy
//     header while preserving the rest of the policy.
//
// No dependencies — pure header transforms.
func SecurityHeadersResponse() middleware.ResponseModifier {
	return func(resp *http.Response) error {
		resp.Header.Del("X-Frame-Options")
		resp.Header.Del("Cross-Origin-Resource-Policy")
		resp.Header.Del("Cross-Origin-Embedder-Policy")

		if val := resp.Header.Get("Content-Security-Policy"); val != "" {
			newVal := frameAncestorsRe.ReplaceAllString(val, "")
			resp.Header.Set("Content-Security-Policy", newVal)
		}
		return nil
	}
}
