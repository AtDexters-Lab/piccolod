package l7

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/services/middleware"
)

// ArrivedViaTLSFn reports whether the original client request reached piccolod
// over TLS. The dep is closure-injected because today's implementation reads
// the connection hint via the services-package private context key; step 9
// migrates this to middleware.HintFromContext, after which the dep can be
// inlined and removed.
type ArrivedViaTLSFn func(r *http.Request) bool

// ShouldRewriteAsHTTPS reports whether the proxy should rewrite the upstream
// origin scheme to https for this listener and request. Only flow:tcp +
// protocol:http|websocket listeners may rewrite — flow:tls and protocol:raw
// keep the original scheme because piccolod doesn't terminate TLS for them.
func ShouldRewriteAsHTTPS(ep middleware.EndpointInfo, r *http.Request, arrivedViaTLS ArrivedViaTLSFn) bool {
	if ep.Flow != api.FlowTCP {
		return false
	}
	switch ep.Protocol {
	case api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
		if arrivedViaTLS == nil {
			return false
		}
		return arrivedViaTLS(r)
	default:
		return false
	}
}

// AbsoluteRequestURL builds the absolute URL for the request as the original
// client would have seen it (uses the rewrite-eligible scheme, the request
// Host, and r.URL.Path). Used by the path_auth middleware when constructing
// the `next` parameter for portal redirects.
func AbsoluteRequestURL(r *http.Request, ep middleware.EndpointInfo, arrivedViaTLS ArrivedViaTLSFn) string {
	if r == nil {
		return ""
	}
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	scheme := "http"
	if ShouldRewriteAsHTTPS(ep, r, arrivedViaTLS) {
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

// ComputeRequestOrigin returns the request's origin (scheme://host[:port]) for
// session-binding purposes. Lower-cases and strips trailing dot from host;
// preserves any port present on r.Host. Returns "" if r.Host is empty.
func ComputeRequestOrigin(r *http.Request, ep middleware.EndpointInfo, arrivedViaTLS ArrivedViaTLSFn) string {
	scheme := "http"
	if ShouldRewriteAsHTTPS(ep, r, arrivedViaTLS) || r.TLS != nil {
		scheme = "https"
	}

	// Use net.SplitHostPort for correct IPv6 handling.
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		// No port in Host header
		host = r.Host
		portStr = ""
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}

	// Strip existing brackets before re-adding to avoid double-bracketing
	// (e.g., Host: [::1] without port).
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	// Re-add brackets if the host is an IPv6 address (contains ':').
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	// Omit default ports per RFC 3986 §3.2.3.
	if portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
				return scheme + "://" + host + ":" + portStr
			}
		}
	}
	return scheme + "://" + host
}
