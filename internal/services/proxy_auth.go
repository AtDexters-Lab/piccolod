package services

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type proxyAppIDContextKey struct{}
type proxyAppHostContextKey struct{}
type proxyCookieRewriteContextKey struct{}
type proxyPartitionCookiesContextKey struct{}
type proxyNeedsMarkerContextKey struct{}

// Cookie names for Piccolo sessions and OIDC flows
const (
	sessionCookieName   = "piccolo_session"
	oidcStateCookieName = "piccolo_oidc_state"
	nonceCookieName     = "piccolo_nonce"
	embeddedCookieName  = "piccolo_embedded"
)

var piccoloCookieNames = map[string]struct{}{
	sessionCookieName:   {},
	oidcStateCookieName: {},
	nonceCookieName:     {},
	embeddedCookieName:  {},
}

func isPiccoloCookieName(name string) bool {
	// RFC 20260122 §6.1: The entire piccolo_ namespace is reserved.
	// This covers piccolo_session, piccolo_oidc_state, piccolo_nonce,
	// and port-based cookies like piccolo_app_session_p<port>.
	return strings.HasPrefix(name, "piccolo_")
}

func isBrowserNavigation(r *http.Request) bool {
	if r == nil {
		return false
	}

	mode := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")))
	dest := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")))
	if mode == "navigate" && (dest == "document" || dest == "iframe") {
		return true
	}

	// Fallback heuristics: GET + Accept: text/html + not XHR/fetch.
	if strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")) == "" && strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")) == "" {
		if r.Method != http.MethodGet {
			return false
		}
		accept := strings.ToLower(r.Header.Get("Accept"))
		if !strings.Contains(accept, "text/html") {
			return false
		}
		if strings.TrimSpace(r.Header.Get("X-Requested-With")) != "" {
			return false
		}
		return true
	}

	return false
}

func writeProxyJSONError(w http.ResponseWriter, status int, errStr, code string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": errStr,
		"code":  code,
	})
}

func absoluteRequestURL(r *http.Request, ep ServiceEndpoint) string {
	if r == nil {
		return ""
	}
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	scheme := "http"
	if shouldRewriteAsHTTPS(ep, r) {
		scheme = "https"
	}
	path := "/"
	if r.URL != nil && r.URL.Path != "" {
		path = r.URL.Path
	}
	out := scheme + "://" + host + path
	if r.URL != nil && r.URL.RawQuery != "" {
		out += "?" + r.URL.RawQuery
	}
	return out
}

func portalLoginURL(portalOrigin, nextAbs string) string {
	next := url.QueryEscape(nextAbs)
	return strings.TrimSuffix(portalOrigin, "/") + "/login?next=" + next
}

func portalAccessDeniedURL(portalOrigin, nextAbs string) string {
	next := url.QueryEscape(nextAbs)
	return strings.TrimSuffix(portalOrigin, "/") + "/access-denied?next=" + next
}

func withProxyContext(r *http.Request, appID, appHost string, rewriteCookies, partitionCookies, needsMarker bool) *http.Request {
	if r == nil {
		return nil
	}
	ctx := r.Context()
	ctx = context.WithValue(ctx, proxyAppIDContextKey{}, appID)
	ctx = context.WithValue(ctx, proxyAppHostContextKey{}, appHost)
	ctx = context.WithValue(ctx, proxyCookieRewriteContextKey{}, rewriteCookies)
	ctx = context.WithValue(ctx, proxyPartitionCookiesContextKey{}, partitionCookies)
	ctx = context.WithValue(ctx, proxyNeedsMarkerContextKey{}, needsMarker)
	return r.WithContext(ctx)
}

func proxyContextAppHost(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(proxyAppHostContextKey{}).(string); ok {
		return v
	}
	return ""
}

func proxyContextCookieRewrite(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Value(proxyCookieRewriteContextKey{}).(bool); ok {
		return v
	}
	return false
}

func proxyContextPartitionCookies(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Value(proxyPartitionCookiesContextKey{}).(bool); ok {
		return v
	}
	return false
}

func proxyContextNeedsMarker(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Value(proxyNeedsMarkerContextKey{}).(bool); ok {
		return v
	}
	return false
}

func normalizeHostNoPort(hostport string) string {
	host := strings.TrimSpace(hostport)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, ".")
	host = strings.Trim(host, "[]")
	return strings.ToLower(host)
}

func shouldRewriteLegacyCookies(hostport string) bool {
	host := normalizeHostNoPort(hostport)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	if strings.EqualFold(host, "piccolo.local") {
		return true
	}
	// mDNS conflict hostnames: piccolo-xyz.local
	if strings.HasPrefix(host, "piccolo-") && strings.HasSuffix(host, ".local") && strings.Count(host, ".") == 1 {
		return true
	}
	return false
}

func cookiePrefixForApp(appID string) string {
	return "__piccolo_" + appID + "_"
}

func splitCookieHeader(raw string) []string {
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

func splitCookiePair(raw string) (name, value string, ok bool) {
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

func stripAndRewriteRequestCookies(r *http.Request, appID string, rewrite bool) {
	if r == nil {
		return
	}

	raw := r.Header.Get("Cookie")
	if strings.TrimSpace(raw) == "" {
		r.Header.Del("Cookie")
		return
	}

	// RFC 20260122 §6.1: Check for any piccolo_ prefixed cookie or rewrite-eligible cookies
	needsProcessing := strings.Contains(raw, "piccolo_")
	if rewrite && strings.Contains(raw, "__piccolo_") {
		needsProcessing = true
	}
	if !needsProcessing {
		return
	}

	appPrefix := cookiePrefixForApp(appID)
	parts := splitCookieHeader(raw)
	if len(parts) == 0 {
		r.Header.Del("Cookie")
		return
	}

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name, value, ok := splitCookiePair(part)
		if !ok {
			continue
		}
		if isPiccoloCookieName(name) {
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

func normalizeCookieDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimPrefix(d, ".")
	return d
}

func parseSetCookieName(setCookie string) (name string, eqIndex int) {
	eq := strings.IndexByte(setCookie, '=')
	if eq <= 0 {
		return "", -1
	}
	return strings.TrimSpace(setCookie[:eq]), eq
}

func setCookieHasHttpOnly(setCookie string) bool {
	for _, part := range strings.Split(setCookie, ";") {
		if strings.EqualFold(strings.TrimSpace(part), "httponly") {
			return true
		}
	}
	return false
}

func setCookieDomain(setCookie string) (string, bool) {
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

// chipsEligible returns true when the request's host/TLS context is eligible for
// CHIPS Partitioned cookies. This checks TLS, non-port-based, .local hostname,
// and non-rewrite-eligible — everything except the iframe/marker detection.
func chipsEligible(r *http.Request) bool {
	if !RequestArrivedViaTLS(r) {
		return false
	}
	if isPortBasedAccess(r) {
		return false
	}
	// Only LAN .local hostnames need CHIPS — remote subdomains are same-site.
	host := normalizeHostNoPort(r.Host)
	if !strings.HasSuffix(host, ".local") {
		return false
	}
	return !shouldRewriteLegacyCookies(r.Host)
}

// shouldPartitionCookies returns true when CHIPS (Partitioned) attributes should
// be applied to Set-Cookie headers. This targets host-based HTTPS LAN access where
// the app iframe is cross-site relative to the portal (e.g., homebox-piccolo-xyz.local
// embedded by piccolo-xyz.local). Remote mode apps (blog.example.com) are same-site
// with the portal and don't need partitioning.
//
// CHIPS is only meaningful in cross-site (iframe) contexts. Top-level navigations
// (new tab) must use standard SameSite=Lax cookies — applying Partitioned there
// causes browsers to reject or mishandle the cookie, leading to OIDC redirect loops.
//
// The initial iframe load is detected via Sec-Fetch-Dest: iframe. Subsequent XHR/fetch
// requests from within the iframe carry the piccolo_embedded marker cookie (itself
// Partitioned, so absent in top-level tabs) to propagate the iframe context.
func shouldPartitionCookies(r *http.Request) bool {
	if r == nil {
		return false
	}
	dest := strings.ToLower(r.Header.Get("Sec-Fetch-Dest"))
	if dest != "iframe" && !hasEmbeddedMarker(r) {
		return false
	}
	return chipsEligible(r)
}

// needsEmbeddedMarker returns true only for the initial iframe navigation load
// where the marker cookie should be set. Subsequent XHR/fetch requests already
// carry the marker and don't need it re-set.
func needsEmbeddedMarker(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.ToLower(r.Header.Get("Sec-Fetch-Dest")) != "iframe" {
		return false
	}
	return chipsEligible(r)
}

// hasEmbeddedMarker checks if the request carries the piccolo_embedded marker cookie.
// The marker is Partitioned, so it's only present in the iframe's cookie partition
// and absent in top-level tab requests.
func hasEmbeddedMarker(r *http.Request) bool {
	if r == nil {
		return false
	}
	c, err := r.Cookie(embeddedCookieName)
	return err == nil && c != nil && c.Value == "1"
}

// embeddedMarkerCookie returns the marker cookie as an *http.Cookie struct.
// Use this for http.SetCookie; embeddedMarkerSetCookie returns the raw header.
func embeddedMarkerCookie() *http.Cookie {
	return &http.Cookie{
		Name:        embeddedCookieName,
		Value:       "1",
		Path:        "/",
		HttpOnly:    true,
		Secure:      true,
		SameSite:    http.SameSiteNoneMode,
		Partitioned: true,
	}
}

// embeddedMarkerSetCookie returns a raw Set-Cookie header value for the embedded
// marker cookie. Session-scoped (no Max-Age/Expires), Partitioned, SameSite=None,
// Secure, HttpOnly.
func embeddedMarkerSetCookie() string {
	return embeddedCookieName + "=1; Path=/; HttpOnly; Secure; SameSite=None; Partitioned"
}

// hasCookieAttribute checks if a raw Set-Cookie header string contains
// a given attribute (case-insensitive, flag-style like "Secure" or "Partitioned").
func hasCookieAttribute(sc, attr string) bool {
	for _, part := range strings.Split(sc, ";") {
		if strings.EqualFold(strings.TrimSpace(part), attr) {
			return true
		}
	}
	return false
}

// removeCookieAttribute removes all occurrences of a cookie attribute from a raw
// Set-Cookie header string. Matches attributes by case-insensitive prefix before '='
// (e.g., "samesite" matches "SameSite=Lax") and flag-style attributes (e.g., "secure").
// The first segment (name=value pair) is always preserved.
func removeCookieAttribute(sc, attr string) string {
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
		// Match flag-style (e.g., "Secure") or key=value (e.g., "SameSite=Lax")
		if lower == lowerAttr || strings.HasPrefix(lower, lowerAttr+"=") {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, ";")
}

// ensurePartitionedAttributes rewrites a raw Set-Cookie header string to add
// CHIPS attributes: Partitioned, SameSite=None, and Secure. Idempotent.
func ensurePartitionedAttributes(sc string) string {
	sc = removeCookieAttribute(sc, "samesite")
	sc = removeCookieAttribute(sc, "partitioned")
	if !hasCookieAttribute(sc, "secure") {
		sc += "; Secure"
	}
	sc += "; SameSite=None; Partitioned"
	return sc
}
