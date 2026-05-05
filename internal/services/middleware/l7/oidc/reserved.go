package oidc

import "strings"

const (
	// CallbackPath is the reserved path for proxy OIDC callbacks per
	// RFC 20260122 §5.10.
	CallbackPath = "/__piccolod_oidc/callback"

	// LogoutPath is the reserved path for app-specific logout (future use).
	LogoutPath = "/__piccolod_oidc/logout"

	// SessionPath is the reserved path for session status API (future use).
	SessionPath = "/__piccolod_oidc/session"

	reservedPrefix = "/__piccolod_oidc/"
)

// IsReservedPath returns true if the path is reserved for proxy OIDC operations.
func IsReservedPath(path string) bool {
	return strings.HasPrefix(path, reservedPrefix)
}
