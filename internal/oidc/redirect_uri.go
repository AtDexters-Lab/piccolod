package oidc

import (
	"net"
	"net/url"
	"strings"
)

func isLoopbackOrCustomSchemeRedirectURI(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if strings.TrimSpace(u.Scheme) == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		host := strings.ToLower(u.Hostname())
		if host == "localhost" {
			return true
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				return ip4[0] == 127 && ip4[1] == 0 && ip4[2] == 0 && ip4[3] == 1
			}
			return ip.Equal(net.IPv6loopback)
		}
		return false
	default:
		// Custom schemes (RFC 8252) have no host and rely on PKCE for protection.
		return true
	}
}
