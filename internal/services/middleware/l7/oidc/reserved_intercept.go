package oidc

import (
	"net/http"

	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/l7"
)

// CallbackHandlerFn dispatches the OIDC callback to the proxy OIDC handler.
// Modeled as a function type so callers (services/proxy.go) can bind the
// handler's HandleCallback method without wrapping.
type CallbackHandlerFn func(w http.ResponseWriter, r *http.Request, appName string, ep middleware.EndpointInfo)

// ReservedPathInterceptDeps holds the dependencies for the reserved-path
// intercept middleware. GetCallbackHandler is invoked only when a request
// path matches the reserved namespace (cold path).
type ReservedPathInterceptDeps struct {
	GetCallbackHandler func() CallbackHandlerFn
}

// ReservedPathIntercept intercepts /__piccolod_oidc/* paths and dispatches
// them to the proxy OIDC handler per RFC 20260122 §5.10. Today only the
// callback path is implemented; other reserved paths return 404.
//
// path_normalize MUST run before this middleware so r.URL.Path holds the
// normalized form (matches the path-representation contract from plan §H).
//
// Returns 503 if no OIDC handler is configured but the reserved path was hit
// — fail-closed semantics, matches the prior inline behavior.
func ReservedPathIntercept(ep middleware.EndpointInfo, deps ReservedPathInterceptDeps) middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cleanedPath := r.URL.Path
			if !IsReservedPath(cleanedPath) {
				next.ServeHTTP(w, r)
				return
			}

			callback := deps.GetCallbackHandler()
			if callback == nil {
				l7.WriteJSONError(w, http.StatusServiceUnavailable, "proxy_oidc_not_configured", "OIDC_UNAVAILABLE")
				return
			}
			if cleanedPath == CallbackPath {
				callback(w, r, ep.App, ep)
				return
			}

			// Other reserved paths (future: logout, session status).
			l7.WriteJSONError(w, http.StatusNotFound, "not_found", "RESERVED_PATH_NOT_IMPLEMENTED")
		})
	}
}
