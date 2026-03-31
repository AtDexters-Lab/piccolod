package auth

import (
	"net"
	"strings"

	"piccolod/internal/persistence"
)

// AccessContext represents whether a request is from LAN or remote.
type AccessContext int

const (
	AccessContextLAN AccessContext = iota
	AccessContextRemote
)

// String returns a human-readable context name.
func (c AccessContext) String() string {
	if c == AccessContextRemote {
		return "remote"
	}
	return "lan"
}

// NormalizeHost extracts a clean hostname from a request Host header:
// strips port, lowercases, and removes any trailing dot.
func NormalizeHost(requestHost string) string {
	host := requestHost
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// ShortHostLabel returns a concise host label for browser credential pickers.
// If the host is a subdomain of rpID, strips the rpID suffix (e.g.
// "jane404.piccolospace.com" with rpID "piccolospace.com" → "jane404").
// Otherwise returns the full host.
func ShortHostLabel(host, rpID string) string {
	if rpID != "" && host != rpID && strings.HasSuffix(host, "."+rpID) {
		if short := strings.TrimSuffix(host, "."+rpID); short != "" {
			return short
		}
	}
	return host
}

// DetermineRPID derives the WebAuthn RP ID from the request hostname.
//
// For .local hostnames (LAN), the RP ID is the full hostname.
//
// For Namek hostnames under the identity base domain (e.g. slug.piccolospace.com
// or custom-name.piccolospace.com), the RP ID is the base domain so all
// subdomains share one passkey registration.
//
// For hosts on a different domain (e.g. a self-hosted Nexus portal like
// mydevice.example.com), the base domain doesn't apply — the RP ID is the
// request host itself, giving that domain its own independent passkey.
func DetermineRPID(requestHost, baseDomain string) string {
	host := NormalizeHost(requestHost)

	// .local hostnames: use the full hostname as RP ID
	if strings.HasSuffix(host, ".local") {
		return host
	}

	// Namek base domain: use it only when the host is actually under it.
	if baseDomain != "" {
		bd := strings.ToLower(baseDomain)
		if host == bd || strings.HasSuffix(host, "."+bd) {
			return bd
		}
	}

	return host
}

// AllowedMethods returns the authentication methods available for the given context.
func AllowedMethods(ctx AccessContext, secure bool, role string, hasPasskeyForRP bool) []string {
	// HTTP (non-secure context): no WebAuthn available
	if !secure {
		if role == string(persistence.UserRoleAdmin) || role == "" {
			return []string{"password"}
		}
		// Standard users can't log in over HTTP (no password, no WebAuthn)
		return nil
	}

	switch ctx {
	case AccessContextRemote:
		if hasPasskeyForRP {
			// Steady state: passkey only
			return []string{"passkey"}
		}
		// Bootstrap: allow password for first-time passkey registration
		return []string{"passkey", "password"}

	case AccessContextLAN:
		if role == string(persistence.UserRoleAdmin) || role == "" {
			return []string{"passkey", "password"}
		}
		// Standard users: passkey only on LAN
		return []string{"passkey"}
	}

	return []string{"passkey"}
}
