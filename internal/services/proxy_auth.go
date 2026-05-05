package services

import (
	"net"
	"net/http"
	"strings"

	"piccolod/internal/services/middleware/l7"
)

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

// isPortBasedAccess checks if the request is using port-based LAN routing.
func isPortBasedAccess(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := r.Host
	if _, port, err := net.SplitHostPort(host); err == nil {
		if port != "80" && port != "443" {
			return true
		}
	}
	return false
}
