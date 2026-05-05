package services

import (
	"net/http"
	"strings"

	"piccolod/internal/services/middleware/l7"
)

// proxyOIDCRewriteContextKey carries the per-request snapshot used by the
// response-side OIDC URL rewrite. Lives in services/ for now; moves to
// middleware/l7/oidc/ in step 1.5d alongside ProxyOIDCHandler.
type proxyOIDCRewriteContextKey struct{}

// oidcRewriteSnapshot holds OIDC rewrite state captured under lock in the
// request handler and threaded into ModifyResponse via request context.
type oidcRewriteSnapshot struct {
	issuerOrigin   string   // e.g., "http://piccolo-abc123.local"
	portalOrigin   string   // e.g., "https://slug.piccolospace.com"
	authorizePaths []string // app's declared authorize_paths
}

// absoluteRequestURL builds the absolute URL for the request as the client
// would have seen it. Stays in services/ until step 1.5c because it takes
// ServiceEndpoint to call shouldRewriteAsHTTPS — both refactor to EndpointInfo
// in that substep before moving to middleware/l7/.
func absoluteRequestURL(r *http.Request, ep ServiceEndpoint) string {
	if r == nil {
		return ""
	}
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	scheme := "http"
	if shouldRewriteAsHTTPS(ep, r) {
		scheme = "https"
	}
	path := "/"
	if r.URL != nil && r.URL.Path != "" {
		path = r.URL.Path
	}
	out := scheme + "://" + host + path
	if r.URL != nil && r.URL.RawQuery != "" {
		out += "?" + r.URL.RawQuery
	}
	return out
}

// chipsEligible returns true when the request's host/TLS context is eligible
// for CHIPS Partitioned cookies. Stays in services/ until step 9 because it
// transitively depends on RequestArrivedViaTLS, which itself depends on the
// hint chain that step 9 migrates to middleware.HintFromContext.
func chipsEligible(r *http.Request) bool {
	if !RequestArrivedViaTLS(r) {
		return false
	}
	if isPortBasedAccess(r) {
		return false
	}
	host := l7.NormalizeHostNoPort(r.Host)
	if !strings.HasSuffix(host, ".local") {
		return false
	}
	return !l7.ShouldRewriteLegacyCookies(r.Host)
}

// shouldPartitionCookies returns true when CHIPS (Partitioned) attributes
// should be applied to Set-Cookie headers. Targets host-based HTTPS LAN access
// where the app iframe is cross-site relative to the portal.
//
// CHIPS is only meaningful in cross-site (iframe) contexts. Top-level
// navigations (new tab) must use standard SameSite=Lax cookies — applying
// Partitioned there causes browsers to reject or mishandle the cookie, leading
// to OIDC redirect loops. Initial iframe load detected via Sec-Fetch-Dest:
// iframe; subsequent XHR/fetch requests carry the piccolo_embedded marker
// cookie (itself Partitioned) to propagate the iframe context.
func shouldPartitionCookies(r *http.Request) bool {
	if r == nil {
		return false
	}
	dest := strings.ToLower(r.Header.Get("Sec-Fetch-Dest"))
	if dest != "iframe" && !l7.HasEmbeddedMarker(r) {
		return false
	}
	return chipsEligible(r)
}

// needsEmbeddedMarker returns true only for the initial iframe navigation load
// where the marker cookie should be set. Subsequent XHR/fetch requests already
// carry the marker and don't need it re-set.
func needsEmbeddedMarker(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.ToLower(r.Header.Get("Sec-Fetch-Dest")) != "iframe" {
		return false
	}
	return chipsEligible(r)
}
