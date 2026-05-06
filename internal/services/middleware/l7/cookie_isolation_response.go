package l7

import (
	"net/http"
	"strings"

	"piccolod/internal/services/middleware"
)

// CookieIsolationResponse implements RFC 20260112 Set-Cookie isolation:
//   - Drops `piccolo_*` cookies from app responses (apps cannot impersonate
//     Piccolo session cookies).
//   - Drops `__piccolo_*` cookies from app responses UNCONDITIONALLY — the
//     namespace is reserved for proxy-managed cookie isolation; backends
//     MUST NOT set cookies in it. Closes a cross-app cookie injection: in
//     port-based LAN mode, browsers scope cookies by host (not port —
//     RFC 6265 §8.5), so a compromised app A returning
//     `Set-Cookie: __piccolo_appB_session=evil` (no HttpOnly to bypass the
//     rewrite gate) would otherwise be stored host-scoped and then
//     unwrapped by app B's request-side strip into `Cookie: session=evil` —
//     forging app B's session. The proxy's own __piccolo_<thisApp>_*
//     rewrites (added by the rewrite block below) are added directly to
//     resp.Header.Add and don't re-enter the filter.
//   - Drops Set-Cookie with a Domain attribute that doesn't match the request
//     app host (defends against domain-scoped cookies leaking across apps).
//     Fails closed if the app host can't be determined.
//   - Optionally rewrites HttpOnly cookie names with the per-app prefix when
//     the request is on a LAN port-based access path (CookieRewriteFromContext).
//   - Optionally adds CHIPS attributes (Partitioned, SameSite=None, Secure)
//     when the request is in an iframe-eligible context
//     (PartitionCookiesFromContext).
//
// MUST run BEFORE EmbeddedMarkerResponse in the canonical chain — the marker
// is `piccolo_`-prefixed and would be filtered by IsPiccoloCookieName if it
// appeared in the snapshot. Adding it AFTER the loop completes is what makes
// it survive (sequence-as-protective-mechanism — see plan §H ordering
// rationale).
func CookieIsolationResponse(appName string) middleware.ResponseModifier {
	appPrefix := CookiePrefixForApp(appName)
	return func(resp *http.Response) error {
		setCookies := resp.Header.Values("Set-Cookie")
		if len(setCookies) == 0 {
			return nil
		}
		resp.Header.Del("Set-Cookie")

		ctx := resp.Request.Context()
		appHost := NormalizeHostNoPort(AppHostFromContext(ctx))
		rewriteCookies := CookieRewriteFromContext(ctx)
		partitionCookies := PartitionCookiesFromContext(ctx)

		for _, sc := range setCookies {
			name, eq := ParseSetCookieName(sc)
			if name == "" || eq == -1 {
				continue
			}
			if IsPiccoloCookieName(name) {
				continue
			}
			// Block backend attempts to set cookies in another app's
			// __piccolo_<otherApp>_ namespace. Same-app `__piccolo_<thisApp>_`
			// is also blocked at this point — the proxy itself produces those
			// via the rewrite block below, the backend MUST NOT.
			if strings.HasPrefix(name, "__piccolo_") {
				continue
			}

			if dom, ok := SetCookieDomain(sc); ok {
				if appHost == "" {
					continue
				}
				if NormalizeCookieDomain(dom) != appHost {
					continue
				}
			}

			if rewriteCookies && SetCookieHasHttpOnly(sc) && !strings.HasPrefix(name, appPrefix) {
				sc = appPrefix + name + sc[eq:]
			}

			if partitionCookies {
				sc = EnsurePartitionedAttributes(sc)
			}

			resp.Header.Add("Set-Cookie", sc)
		}
		return nil
	}
}
