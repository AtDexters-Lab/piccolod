package oidc

import (
	"net/http"

	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/l7"
)

// AuthorizeSnapshotState is the cohesive set of mutable proxy-manager state
// that AuthorizeSnapshot resolves once per request via Deps.Snapshot.
// Captured atomically under the proxy manager's lock so the request observes
// a consistent view across all three fields.
type AuthorizeSnapshotState struct {
	// IssuerOrigin is a closure (not a snapshot) so it tracks hostname/IP
	// changes — important when mDNS is disabled and the value comes from
	// getPreferredOutboundIP. Returns the LAN-based authorize_endpoint origin
	// the OIDC discovery document advertises (e.g., "http://piccolo-abc.local").
	IssuerOrigin func() string

	// PortalOrigin returns the request-bound portal origin
	// (e.g., "https://slug.piccolospace.com").
	PortalOrigin func(r *http.Request) string

	// AuthorizePaths are the per-app authorize_paths declared in the manifest.
	AuthorizePaths []string
}

// AuthorizeSnapshotDeps holds the dependencies for the AuthorizeSnapshot
// middleware. Snapshot is invoked per WAN request to refresh live values;
// ArrivedViaTLS is bound once at chain-build time.
type AuthorizeSnapshotDeps struct {
	ArrivedViaTLS l7.ArrivedViaTLSFn
	Snapshot      func() AuthorizeSnapshotState
}

// AuthorizeSnapshot is a request-side L7 middleware that captures the OIDC
// authorize-rewrite snapshot for WAN requests and stashes it on the request
// context. The response-side AuthorizeRewriteResponse modifier reads that
// context to perform the actual URL rewrite.
//
// Skips capture for non-TLS requests (rewrite only applies to WAN traffic).
// Skips capture if either the issuer origin or portal origin is unconfigured
// (no rewrite is possible without both).
func AuthorizeSnapshot(deps AuthorizeSnapshotDeps) middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if deps.ArrivedViaTLS(r) {
				state := deps.Snapshot()
				if state.IssuerOrigin != nil && state.PortalOrigin != nil {
					issuerOrigin := state.IssuerOrigin()
					portal := state.PortalOrigin(r)
					if issuerOrigin != "" && portal != "" {
						snap := &Snapshot{
							IssuerOrigin:   issuerOrigin,
							PortalOrigin:   portal,
							AuthorizePaths: state.AuthorizePaths,
						}
						r = r.WithContext(ContextWithSnapshot(r.Context(), snap))
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AuthorizeRewriteResponse is the response-side counterpart of
// AuthorizeSnapshot. Reads the per-request Snapshot from context (if any)
// and invokes RewriteAuthorizeResponse. No-op when no snapshot is present
// (non-TLS request, OIDC not configured, etc.).
//
// The appName parameter is used only for log lines emitted by
// RewriteAuthorizeResponse.
func AuthorizeRewriteResponse(appName string) middleware.ResponseModifier {
	return func(resp *http.Response) error {
		if snap, ok := SnapshotFromContext(resp.Request.Context()); ok {
			RewriteAuthorizeResponse(resp, snap, appName)
		}
		return nil
	}
}
