package l7

import (
	"net/http"
	"strings"

	"piccolod/internal/services/middleware"
)

// CookieIsolationResponse implements RFC 20260112 Set-Cookie isolation:
//   - Drops `piccolo_*` cookies from app responses (apps cannot impersonate
//     Piccolo session cookies).
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
