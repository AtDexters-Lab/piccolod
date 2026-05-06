package l7

import (
	"context"
	"net/http"
)

// Context keys for per-request proxy state. Keep them unexported empty-struct
// types so other packages can't accidentally collide with the same string-ish
// key. Callers use the typed accessors (Set, AppHostFromContext, etc.) below.
type (
	proxyAppIDContextKey            struct{}
	proxyAppHostContextKey          struct{}
	proxyCookieRewriteContextKey    struct{}
	proxyPartitionCookiesContextKey struct{}
	proxyNeedsMarkerContextKey      struct{}
)

// SetProxyContext returns a copy of r whose context carries the bundled
// per-request proxy state used by the cookie isolation, marker injection, and
// OIDC rewrite middlewares.
//
// All fields must be supplied together — the request handler computes them
// once at the post-auth point and the response-side modifiers read whatever's
// stashed here.
func SetProxyContext(r *http.Request, appID, appHost string, rewriteCookies, partitionCookies, needsMarker bool) *http.Request {
	if r == nil {
		return nil
	}
	ctx := r.Context()
	ctx = context.WithValue(ctx, proxyAppIDContextKey{}, appID)
	ctx = context.WithValue(ctx, proxyAppHostContextKey{}, appHost)
	ctx = context.WithValue(ctx, proxyCookieRewriteContextKey{}, rewriteCookies)
	ctx = context.WithValue(ctx, proxyPartitionCookiesContextKey{}, partitionCookies)
	ctx = context.WithValue(ctx, proxyNeedsMarkerContextKey{}, needsMarker)
	return r.WithContext(ctx)
}

// AppHostFromContext returns the request's normalized app host (the domain the
// browser used to reach the app), or "" if not set.
func AppHostFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(proxyAppHostContextKey{}).(string); ok {
		return v
	}
	return ""
}

// CookieRewriteFromContext reports whether the response-side cookie isolation
// middleware should rewrite app cookie names with the per-app prefix (LAN
// port-based mode where apps share the host:port origin).
func CookieRewriteFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Value(proxyCookieRewriteContextKey{}).(bool); ok {
		return v
	}
	return false
}

// PartitionCookiesFromContext reports whether response-side cookies should
// receive CHIPS attributes (cross-site iframe embedding context).
func PartitionCookiesFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Value(proxyPartitionCookiesContextKey{}).(bool); ok {
		return v
	}
	return false
}

// NeedsMarkerFromContext reports whether the response should add the
// embedded-iframe marker cookie. Only true on initial iframe loads.
func NeedsMarkerFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Value(proxyNeedsMarkerContextKey{}).(bool); ok {
		return v
	}
	return false
}
