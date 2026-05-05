package l7

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// IsBrowserNavigation reports whether the request looks like a top-level
// browser navigation (vs an XHR/fetch). Used by the path_auth middleware to
// decide whether to redirect into an OIDC flow (safe) or return JSON 401.
func IsBrowserNavigation(r *http.Request) bool {
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

// WriteJSONError writes a structured JSON error response with the given HTTP
// status. Used by every middleware that needs to short-circuit with a clean
// error payload.
func WriteJSONError(w http.ResponseWriter, status int, errStr, code string) {
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

// PortalLoginURL builds the portal login URL with the next-target encoded
// as a query parameter so post-login can redirect back to the original app URL.
func PortalLoginURL(portalOrigin, nextAbs string) string {
	next := url.QueryEscape(nextAbs)
	return strings.TrimSuffix(portalOrigin, "/") + "/login?next=" + next
}

// PortalAccessDeniedURL builds the portal access-denied URL with the next-
// target encoded for browser navigations to apps the user isn't allowed.
func PortalAccessDeniedURL(portalOrigin, nextAbs string) string {
	next := url.QueryEscape(nextAbs)
	return strings.TrimSuffix(portalOrigin, "/") + "/access-denied?next=" + next
}

// NormalizeHostNoPort lowercases a host[:port] string and returns just the
// host portion (with any IPv6 brackets and leading "." removed).
func NormalizeHostNoPort(hostport string) string {
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

// ShouldRewriteLegacyCookies reports whether the host belongs to the LAN
// port-based access mode where apps share an origin and need the
// `__piccolo_<app>_` prefix to keep cookies isolated.
func ShouldRewriteLegacyCookies(hostport string) bool {
	host := NormalizeHostNoPort(hostport)
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
