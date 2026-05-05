package l7

import (
	"net/http"
	"strings"

	"piccolod/internal/services/middleware"
)

// ACMEChallengePathPrefix is the well-known URL prefix for HTTP-01
// authorization challenges per RFC 8555 §8.3.
const ACMEChallengePathPrefix = "/.well-known/acme-challenge/"

// ACMEHandlerFn returns the currently registered ACME HTTP-01 challenge
// handler, or nil if no handler is registered. Resolved per matching request.
type ACMEHandlerFn func() http.Handler

// ACMEBypass intercepts /.well-known/acme-challenge/* requests and dispatches
// them to the registered ACME handler. These requests bypass per-app auth
// because they're infrastructure-level (piccolod's TLS termination), not app
// business logic — external ACME verifiers have no Piccolo session.
//
// path_normalize MUST run before this middleware so r.URL.Path holds the
// normalized form, closing path-mangling bypass surfaces (e.g.,
// `/foo/..%2f.well-known/acme-challenge/X`).
//
// No-op (passes through to next) when getHandler returns nil — the chain
// continues and the request flows to path_auth.
func ACMEBypass(getHandler ACMEHandlerFn) middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, ACMEChallengePathPrefix) {
				if h := getHandler(); h != nil {
					h.ServeHTTP(w, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
