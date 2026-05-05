// Package l4 implements the canonical and operator-listable L4 (TCP) and
// L4UDP (datagram) middlewares per plan §I. Lives sibling to l7/ to mirror
// the layered framework.
package l4

import (
	"net"

	"piccolod/internal/services/middleware"
)

// HintLookupFn looks up the connection hint registered for (listenerPort,
// sourcePort) by the relay path (TLS mux). Returns the hint and true on
// success, zero+false otherwise. Resolved per call so the registration race
// (mux registers a moment after the conn is accepted) is invisible to the
// caller.
type HintLookupFn func(listenerPort, sourcePort int) (middleware.Hint, bool)

// HintConsumer is a canonical L4 middleware that installs a lazy Hint
// resolver on ConnContext. Subsequent middlewares (ip_allowlist,
// ip_rate_limit, connection_auth) read the resolved hint via
// middleware.EffectiveSourceIP, which honors the TrustedLoopback gate.
//
// The resolver bridges into the relay-side hint chain (today: services
// package private connectionHint). Step 9 retires this bridge once the hint
// chain migrates wholly into the middleware package.
//
// Hint replacement is per-request — the middleware constructs a new
// ConnContext with the new Hint accessor and passes it down. Upstream
// ConnContext is unchanged (Go pass-by-value on the struct).
func HintConsumer(lookup HintLookupFn, listenerPort int) middleware.L4Middleware {
	return func(next middleware.ConnHandler) middleware.ConnHandler {
		return func(ctx middleware.ConnContext, c net.Conn) {
			ctx.Hint = func() (middleware.Hint, bool) {
				addr, ok := c.RemoteAddr().(*net.TCPAddr)
				if !ok || addr == nil {
					return middleware.Hint{}, false
				}
				return lookup(listenerPort, addr.Port)
			}
			next(ctx, c)
		}
	}
}
