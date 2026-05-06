package l7

import (
	"log"
	"net/http"

	"piccolod/internal/api"
	"piccolod/internal/auth"
	"piccolod/internal/services/middleware"
)

// Trusted-identity headers piccolod injects when an app's auth strategy is
// "headers" per RFC 4.1.6. Apps consume these to identify the authenticated
// user without re-implementing session validation.
//
// Invariant: every Piccolo-injected header MUST start with "X-Piccolo-" so
// l7.StripPiccoloHeaders (strip_piccolo_headers.go) can scrub client-spoofed
// copies before any handler that trusts X-Piccolo-* runs.
// X-Piccolo-Hint-Token (still in services/proxy.go pending step 9) shares
// the same prefix.
const (
	HeaderPiccoloUser  = "X-Piccolo-User"
	HeaderPiccoloEmail = "X-Piccolo-Email"
	HeaderPiccoloName  = "X-Piccolo-Name"
	HeaderPiccoloRole  = "X-Piccolo-Role"
)

// OIDCInitiator initiates the proxy-level OIDC flow for an unauthenticated
// browser navigation. Modeled as a function rather than an interface so the
// l7 package needn't import internal/services/middleware/l7/oidc (the
// concrete handler) — the call site in services/ binds it as a method value.
type OIDCInitiator func(w http.ResponseWriter, r *http.Request, appName string, ep middleware.EndpointInfo)

// PathAuthSnapshot is the cohesive set of mutable dependencies that the
// PathAuth middleware resolves once per request via PathAuthDeps.Snapshot.
// Captured atomically under the proxy manager's lock so the request observes
// a consistent view across all six fields.
type PathAuthSnapshot struct {
	UserManager   *auth.UserManager
	SessionGetter func(r *http.Request) (*auth.Session, bool)
	PortalOrigin  func(r *http.Request) string
	AliasChecker  func(host, listener string) bool
	SessionStore  *auth.SessionStore
	InitiateOIDC  OIDCInitiator // nil if proxy OIDC not configured
}

// PathAuthDeps holds the dependencies for the PathAuth middleware. Snapshot
// is invoked per-request to refresh live values; ArrivedViaTLS is bound
// once at chain-build time.
type PathAuthDeps struct {
	Snapshot      func() PathAuthSnapshot
	ArrivedViaTLS ArrivedViaTLSFn
}

// PathAuth implements RFC 4.1.6 strategy-specific access gating. It applies
// listenerAuth.Rules to classify the request path into a strategy
// (protected/headers/oidc_passthrough/public) and acts on it:
//
//   - protected/headers: validates the Piccolo session, checks allowed_apps,
//     redirects unauthenticated browser navigations into the OIDC flow (or
//     portal-login fallback), and (for headers) injects the X-Piccolo-* trusted
//     identity headers.
//   - oidc_passthrough/public: passes through; the app handles auth itself.
//   - any other (incl. unknown): fails closed with 401.
func PathAuth(ep middleware.EndpointInfo, listenerAuth *api.ListenerAuth, deps PathAuthDeps) middleware.L7Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			strategy := ListenerStrategyForPath(listenerAuth, r.URL.Path)

			switch strategy {
			case "protected", "headers":
				// RFC 20260122 §5: CORS preflight bypasses auth. Browsers send
				// preflight without cookies; blocking it breaks cross-origin XHR.
				if r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != "" {
					break
				}

				snap := deps.Snapshot()

				if snap.SessionGetter == nil {
					http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
					return
				}

				// RFC 20260112: alias domains are not compatible with protected/headers
				// strategies — the session cookie cannot be shared across custom domains.
				if snap.AliasChecker != nil && snap.AliasChecker(NormalizeHostNoPort(r.Host), ep.DerivedHostLabel) {
					log.Printf("WARN: alias domain access is not supported for auth strategy=%s (host=%s app=%s listener=%s)", strategy, NormalizeHostNoPort(r.Host), ep.App, ep.Listener)
				}

				_, cookieErr := r.Cookie(SessionCookieName)
				cookiePresent := cookieErr == nil

				// RFC 20260122 §5: validate session with app audience + origin binding.
				sess, ok := snap.SessionGetter(r)
				if ok && sess != nil && snap.SessionStore != nil {
					requestOrigin := ComputeRequestOrigin(r, ep, deps.ArrivedViaTLS)
					sess, ok = snap.SessionStore.ValidateAppSession(sess.ID, ep.App, requestOrigin)
				}

				if !ok || sess == nil {
					// RFC 20260122 §5.9: only safe methods may redirect into OIDC —
					// non-safe methods would lose the request body on redirect.
					isSafeMethod := r.Method == http.MethodGet || r.Method == http.MethodHead
					if isSafeMethod && IsBrowserNavigation(r) {
						if snap.InitiateOIDC != nil {
							snap.InitiateOIDC(w, r, ep.App, ep)
							return
						}
						origin := portalRedirectOrigin(r, ep, snap.PortalOrigin, deps.ArrivedViaTLS)
						http.Redirect(w, r, PortalLoginURL(origin, AbsoluteRequestURL(r, ep, deps.ArrivedViaTLS)), http.StatusFound)
						return
					}
					if cookiePresent {
						WriteJSONError(w, http.StatusUnauthorized, "session_expired", "SESSION_EXPIRED")
					} else {
						WriteJSONError(w, http.StatusUnauthorized, "authentication_required", "AUTH_REQUIRED")
					}
					return
				}

				// allowed_apps gate (admins have full access).
				if sess.Role != "admin" {
					if snap.UserManager == nil {
						http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
						return
					}
					allowed, err := snap.UserManager.IsAppAllowed(r.Context(), sess.UserID, ep.App)
					if err != nil || !allowed {
						if IsBrowserNavigation(r) {
							origin := portalRedirectOrigin(r, ep, snap.PortalOrigin, deps.ArrivedViaTLS)
							http.Redirect(w, r, PortalAccessDeniedURL(origin, AbsoluteRequestURL(r, ep, deps.ArrivedViaTLS)), http.StatusFound)
							return
						}
						WriteJSONError(w, http.StatusForbidden, "app_access_denied", "APP_NOT_ALLOWED")
						return
					}
				}

				if strategy == "headers" {
					if snap.UserManager == nil {
						http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
						return
					}
					user, err := snap.UserManager.Get(r.Context(), sess.UserID)
					if err != nil {
						WriteJSONError(w, http.StatusUnauthorized, "session_expired", "SESSION_EXPIRED")
						return
					}
					// RFC 4.1.6: inject trusted identity headers.
					r.Header.Set(HeaderPiccoloUser, user.Username)
					r.Header.Set(HeaderPiccoloEmail, user.Email)
					r.Header.Set(HeaderPiccoloName, user.Username)
					r.Header.Set(HeaderPiccoloRole, string(user.Role))
				}
			case "oidc_passthrough", "public":
				// Pass-through; the app manages auth or requires none.
			default:
				// Unknown strategy fails closed.
				WriteJSONError(w, http.StatusUnauthorized, "authentication_required", "AUTH_REQUIRED")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// portalRedirectOrigin returns the origin to use when redirecting an
// unauthenticated browser into the portal. Prefers the configured
// PortalOrigin resolver; falls back to the request's host with the
// rewrite-eligible scheme.
//
// Pre-existing limitation: the fallback strips the port and IPv6 brackets
// (matches the prior inline code). See deferred memory entry DEF6 for
// follow-up.
func portalRedirectOrigin(r *http.Request, ep middleware.EndpointInfo, portalOrigin func(*http.Request) string, arrivedViaTLS ArrivedViaTLSFn) string {
	if portalOrigin != nil {
		if origin := portalOrigin(r); origin != "" {
			return origin
		}
	}
	scheme := "http"
	if ShouldRewriteAsHTTPS(ep, r, arrivedViaTLS) {
		scheme = "https"
	}
	return scheme + "://" + NormalizeHostNoPort(r.Host)
}
