package l7

import (
	"net"
	"net/http"

	"piccolod/internal/services/middleware"
)

// ForwardedScrub strips client-provided forwarding headers when the request did
// not arrive via a trusted-loopback listener. Loopback-only sources (Nexus via
// TLS mux) keep their forwarded headers because piccolod itself populates them
// via applyForwardHeaders.
//
// Trust is derived from the listener's local address (LocalAddrContextKey) per
// the existing pattern at proxy.go:540–548. SourceTrust on ConnContext is
// L4-internal and is not bridged to L7 — see plan D13/D20.
func ForwardedScrub() middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isTrustedLoopbackRequest(r) {
				r.Header.Del("Forwarded")
				r.Header.Del("X-Forwarded-For")
				r.Header.Del("X-Forwarded-Proto")
				r.Header.Del("X-Forwarded-Host")
				r.Header.Del("X-Forwarded-Port")
				r.Header.Del("X-Real-IP")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isTrustedLoopbackRequest reports whether the request arrived on a loopback
// listener address. Used by both forwarded_scrub and forward_headers; kept
// package-local until a third caller appears.
func isTrustedLoopbackRequest(r *http.Request) bool {
	localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(localAddr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
