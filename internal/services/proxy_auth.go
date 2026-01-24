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

// Cookie names for Piccolo sessions and OIDC flows
const (
	sessionCookieName   = "piccolo_session"
	oidcStateCookieName = "piccolo_oidc_state"
	nonceCookieName     = "piccolo_nonce"
)

var piccoloCookieNames = map[string]struct{}{
	sessionCookieName:   {},
	oidcStateCookieName: {},
	nonceCookieName:     {},
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

func withProxyContext(r *http.Request, appID, appHost string, rewriteCookies bool) *http.Request {
	if r == nil {
		return nil
	}
	ctx := r.Context()
	ctx = context.WithValue(ctx, proxyAppIDContextKey{}, appID)
	ctx = context.WithValue(ctx, proxyAppHostContextKey{}, appHost)
	ctx = context.WithValue(ctx, proxyCookieRewriteContextKey{}, rewriteCookies)
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
