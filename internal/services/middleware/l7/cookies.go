package l7

import (
	"net/http"
	"strings"
)

// Cookie names for Piccolo sessions and OIDC flows. Apps using
// `piccolo_`-prefixed names is reserved per RFC 20260122 §6.1; piccolod strips
// any such cookies from incoming requests and from upstream Set-Cookie responses.
const (
	SessionCookieName   = "piccolo_session"
	OIDCStateCookieName = "piccolo_oidc_state"
	NonceCookieName     = "piccolo_nonce"
	EmbeddedCookieName  = "piccolo_embedded"
)

// IsPiccoloCookieName reports whether the cookie name is in the reserved
// piccolo_ namespace and should be filtered from incoming request and response
// cookie headers.
func IsPiccoloCookieName(name string) bool {
	// RFC 20260122 §6.1: The entire piccolo_ namespace is reserved.
	return strings.HasPrefix(name, "piccolo_")
}

// CookiePrefixForApp returns the LAN-port-based cookie isolation prefix used to
// rewrite app cookies so they don't collide across apps that share the same
// origin (LAN port-based mode where every app is served on piccolo.local:<port>).
func CookiePrefixForApp(appID string) string {
	return "__piccolo_" + appID + "_"
}

// SplitCookieHeader splits a raw Cookie header value on top-level semicolons,
// honoring quoted-value pairs (no semicolon split inside quotes).
func SplitCookieHeader(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := make([]string, 0)
	start := 0
	inQuotes := false
	escaped := false

	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if inQuotes {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inQuotes = false
				continue
			}
			continue
		}

		if ch == '"' {
			inQuotes = true
			continue
		}
		if ch == ';' {
			part := strings.TrimSpace(raw[start:i])
			if part != "" {
				parts = append(parts, part)
			}
			start = i + 1
		}
	}

	if start <= len(raw) {
		part := strings.TrimSpace(raw[start:])
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// SplitCookiePair splits a single "name=value" cookie pair.
func SplitCookiePair(raw string) (name, value string, ok bool) {
	eq := strings.IndexByte(raw, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(raw[:eq])
	value = strings.TrimSpace(raw[eq+1:])
	if name == "" {
		return "", "", false
	}
	return name, value, true
}

// StripAndRewriteRequestCookies filters and (optionally) un-prefixes cookies on
// an incoming request before forwarding to the backend. Piccolo-namespaced
// cookies are dropped; in rewrite mode (LAN port-based), the
// `__piccolo_<app>_` prefix is stripped from cookies belonging to this app and
// cookies for other apps are dropped.
func StripAndRewriteRequestCookies(r *http.Request, appID string, rewrite bool) {
	if r == nil {
		return
	}

	raw := r.Header.Get("Cookie")
	if strings.TrimSpace(raw) == "" {
		r.Header.Del("Cookie")
		return
	}

	needsProcessing := strings.Contains(raw, "piccolo_")
	if rewrite && strings.Contains(raw, "__piccolo_") {
		needsProcessing = true
	}
	if !needsProcessing {
		return
	}

	appPrefix := CookiePrefixForApp(appID)
	parts := SplitCookieHeader(raw)
	if len(parts) == 0 {
		r.Header.Del("Cookie")
		return
	}

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name, value, ok := SplitCookiePair(part)
		if !ok {
			continue
		}
		if IsPiccoloCookieName(name) {
			continue
		}

		if rewrite && strings.HasPrefix(name, "__piccolo_") {
			if strings.HasPrefix(name, appPrefix) {
				name = strings.TrimPrefix(name, appPrefix)
				out = append(out, name+"="+value)
			}
			// Drop cookies for other apps (or malformed) in rewrite mode.
			continue
		}

		out = append(out, name+"="+value)
	}

	if len(out) == 0 {
		r.Header.Del("Cookie")
		return
	}
	r.Header.Set("Cookie", strings.Join(out, "; "))
}

// NormalizeCookieDomain lowercases and strips a leading '.' from a cookie
// Domain attribute value for comparison.
func NormalizeCookieDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimPrefix(d, ".")
	return d
}

// ParseSetCookieName extracts the name from a raw Set-Cookie header value and
// returns the index of the '=' sign so callers can reconstruct the value
// portion without re-splitting.
func ParseSetCookieName(setCookie string) (name string, eqIndex int) {
	eq := strings.IndexByte(setCookie, '=')
	if eq <= 0 {
		return "", -1
	}
	return strings.TrimSpace(setCookie[:eq]), eq
}

// SetCookieHasHttpOnly reports whether a raw Set-Cookie header carries the
// HttpOnly flag (case-insensitive).
func SetCookieHasHttpOnly(setCookie string) bool {
	for _, part := range strings.Split(setCookie, ";") {
		if strings.EqualFold(strings.TrimSpace(part), "httponly") {
			return true
		}
	}
	return false
}

// SetCookieDomain extracts the Domain attribute from a raw Set-Cookie header
// (returns "", false if absent).
func SetCookieDomain(setCookie string) (string, bool) {
	for _, part := range strings.Split(setCookie, ";") {
		p := strings.TrimSpace(part)
		if len(p) < 7 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(p), "domain=") {
			return strings.TrimSpace(p[len("domain="):]), true
		}
	}
	return "", false
}

// HasEmbeddedMarker reports whether the request carries the piccolo_embedded
// marker cookie. The marker is Partitioned, so it's only present in the
// iframe's cookie partition and absent in top-level tab requests.
func HasEmbeddedMarker(r *http.Request) bool {
	if r == nil {
		return false
	}
	c, err := r.Cookie(EmbeddedCookieName)
	return err == nil && c != nil && c.Value == "1"
}

// EmbeddedMarkerCookie returns the iframe-context marker cookie as an
// *http.Cookie struct. Use for direct http.SetCookie; for rp.ModifyResponse
// closures use EmbeddedMarkerSetCookie.
func EmbeddedMarkerCookie() *http.Cookie {
	return &http.Cookie{
		Name:        EmbeddedCookieName,
		Value:       "1",
		Path:        "/",
		HttpOnly:    true,
		Secure:      true,
		SameSite:    http.SameSiteNoneMode,
		Partitioned: true,
	}
}

// EmbeddedMarkerSetCookie returns the raw Set-Cookie header value for the
// embedded marker cookie. Session-scoped (no Max-Age/Expires), Partitioned,
// SameSite=None, Secure, HttpOnly.
func EmbeddedMarkerSetCookie() string {
	return EmbeddedCookieName + "=1; Path=/; HttpOnly; Secure; SameSite=None; Partitioned"
}

// HasCookieAttribute checks if a raw Set-Cookie header string contains a given
// flag-style attribute (case-insensitive, e.g., "Secure" or "Partitioned").
func HasCookieAttribute(sc, attr string) bool {
	for _, part := range strings.Split(sc, ";") {
		if strings.EqualFold(strings.TrimSpace(part), attr) {
			return true
		}
	}
	return false
}

// RemoveCookieAttribute removes all occurrences of a cookie attribute from a
// raw Set-Cookie header string. Matches by case-insensitive prefix before '='
// (e.g., "samesite" matches "SameSite=Lax") and flag-style attributes (e.g.,
// "secure"). The first segment (name=value pair) is always preserved.
func RemoveCookieAttribute(sc, attr string) string {
	parts := strings.Split(sc, ";")
	if len(parts) == 0 {
		return sc
	}
	out := make([]string, 0, len(parts))
	out = append(out, parts[0]) // always keep name=value pair
	lowerAttr := strings.ToLower(attr)
	for _, part := range parts[1:] {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if lower == lowerAttr || strings.HasPrefix(lower, lowerAttr+"=") {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, ";")
}

// EnsurePartitionedAttributes rewrites a raw Set-Cookie header string to add
// CHIPS attributes: Partitioned, SameSite=None, and Secure. Idempotent.
func EnsurePartitionedAttributes(sc string) string {
	sc = RemoveCookieAttribute(sc, "samesite")
	sc = RemoveCookieAttribute(sc, "partitioned")
	if !HasCookieAttribute(sc, "secure") {
		sc += "; Secure"
	}
	sc += "; SameSite=None; Partitioned"
	return sc
}
