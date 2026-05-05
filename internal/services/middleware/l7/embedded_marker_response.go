package l7

import (
	"net/http"

	"piccolod/internal/services/middleware"
)

// NeedsMarkerFn reports whether the request should trigger embedded-marker
// cookie injection. Typically bound to NeedsMarkerFromContext, which inspects
// the request context for the iframe-context flag set during request-side
// proxy-context setup. Kept as a func type so callers can swap the predicate
// (e.g., for tests) without changing the L7 factory signature.
type NeedsMarkerFn func(r *http.Request) bool

// MarkerCookieFn returns the literal Set-Cookie header value for the marker.
// Typically bound to EmbeddedMarkerSetCookie. A function rather than a constant
// so future changes to the marker (e.g., per-app variant) don't force the L7
// factory signature to change.
type MarkerCookieFn func() string

// EmbeddedMarkerResponse adds the iframe-context marker cookie to the response
// when the request flagged itself as needing one. Today's behavior at
// proxy.go:524–526.
//
// MUST run AFTER cookie_isolation_response in the canonical chain — the marker
// is `piccolo_`-prefixed, so cookie_isolation_response's snapshot+strip+rewrite
// loop would filter it via isPiccoloCookieName if it appeared in the snapshot.
// Adding it AFTER the loop completes is what makes it survive (sequence-as-
// protective-mechanism, see plan §H ordering rationale).
func EmbeddedMarkerResponse(needsMarker NeedsMarkerFn, markerCookie MarkerCookieFn) middleware.ResponseModifier {
	return func(resp *http.Response) error {
		if resp.Request != nil && needsMarker(resp.Request) {
			resp.Header.Add("Set-Cookie", markerCookie())
		}
		return nil
	}
}
