package l7

import (
	"net/http"

	"piccolod/internal/services/middleware"
)

// ApplyForwardHeaders is the function the services package uses to populate
// X-Forwarded-* / Forwarded headers on the request before reverse-proxying to
// the backend. Its signature matches services.applyForwardHeaders(r, ep) where
// ep is captured at factory-construction time.
//
// Step 2 wraps the existing applyForwardHeaders without modification. Step 9
// (the connectionHint→middleware.HintFromContext migration) will rewrite the
// applyForwardHeaders body, but the L7 middleware shape and call site stay the
// same.
type ApplyForwardHeaders func(r *http.Request)

// ForwardHeaders returns an L7 middleware that calls the supplied applier on
// each request, then proceeds to the next handler. The applier captures
// endpoint-specific state (e.g., the listener's external port) so the L7
// middleware itself stays endpoint-agnostic.
func ForwardHeaders(apply ApplyForwardHeaders) middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apply(r)
			next.ServeHTTP(w, r)
		})
	}
}
