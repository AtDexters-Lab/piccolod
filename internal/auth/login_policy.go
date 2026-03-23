package auth

import (
	"net"
	"strings"

	"piccolod/internal/persistence"
)

// AccessContext represents whether a request is from LAN or remote.
type AccessContext int

const (
	AccessContextLAN    AccessContext = iota
	AccessContextRemote
)

// String returns a human-readable context name.
func (c AccessContext) String() string {
	if c == AccessContextRemote {
		return "remote"
	}
	return "lan"
}

// DetermineRPID derives the WebAuthn RP ID from the request hostname.
// For .local hostnames (LAN), the RP ID is the full hostname.
// For remote hostnames, the RP ID is the BaseDomain from identity service,
// covering all subdomains (slug, custom) under one passkey registration.
// If baseDomain is empty, falls back to the full hostname.
//
// Note: alias domains on different base domains are not served by passkey
// auth — they use OIDC-delegated authentication to the primary remote domain.
func DetermineRPID(requestHost, baseDomain string) string {
	host := requestHost
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)

	// .local hostnames: use the full hostname as RP ID
	if strings.HasSuffix(host, ".local") {
		return host
	}

	// Remote hostnames: use the base domain if available
	if baseDomain != "" {
		return strings.ToLower(baseDomain)
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
