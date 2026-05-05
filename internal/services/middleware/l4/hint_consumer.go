// Package l4 implements the canonical and operator-listable L4 (TCP) and
// L4UDP (datagram) middlewares per plan §I. Lives sibling to l7/ to mirror
// the layered framework.
package l4

import (
	"net"
	"sync"
	"time"

	"piccolod/internal/services/middleware"
)

// Hint-resolution retry budget per plan §D13: the TLS mux registers the
// hint a moment after the conn is accepted, so the first lookup may race
// the registration. Inherits the pre-refactor `initSecureLoopback`
// constants (40 attempts × 500µs = ~20ms total budget).
const (
	hintRetryAttempts = 40
	hintRetryInterval = 500 * time.Microsecond
)

// HintLookupFn looks up the connection hint registered for (listenerPort,
// sourcePort) by the relay path (TLS mux). Returns the hint and true on
// success, zero+false otherwise.
type HintLookupFn func(listenerPort, sourcePort int) (middleware.Hint, bool)

// HintConsumer is a canonical L4 middleware that installs a lazy Hint
// resolver on ConnContext. Subsequent middlewares (ip_allowlist,
// ip_rate_limit, connection_auth) read the resolved hint via
// middleware.EffectiveSourceIP, which honors the TrustedLoopback gate.
//
// Resolution semantics per plan §D13:
//   - Lazy: lookup runs on first ctx.Hint() call, not at chain entry.
//   - sync.Once-cached: subsequent calls return the cached result without
//     re-acquiring the lookup mutex.
//   - First call retries the lookup briefly (~20ms budget at 500µs interval)
//     to absorb the relay-registration race window.
//
// Hint replacement is per-request — the middleware constructs a new
// ConnContext with the new Hint accessor and passes it down. Upstream
// ConnContext is unchanged (Go pass-by-value on the struct).
func HintConsumer(lookup HintLookupFn, listenerPort int) middleware.L4Middleware {
	return func(next middleware.ConnHandler) middleware.ConnHandler {
		return func(ctx middleware.ConnContext, c net.Conn) {
			var (
				once   sync.Once
				cached middleware.Hint
				ok     bool
			)
			ctx.Hint = func() (middleware.Hint, bool) {
				once.Do(func() {
					addr, addrOK := c.RemoteAddr().(*net.TCPAddr)
					if !addrOK || addr == nil {
						return
					}
					for attempt := 0; attempt < hintRetryAttempts; attempt++ {
						if cached, ok = lookup(listenerPort, addr.Port); ok {
							return
						}
						time.Sleep(hintRetryInterval)
					}
				})
				return cached, ok
			}
			next(ctx, c)
		}
	}
}
