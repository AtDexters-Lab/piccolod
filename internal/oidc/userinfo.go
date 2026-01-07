package oidc

import (
	"context"

	"github.com/zitadel/oidc/v3/pkg/oidc"

	"piccolod/internal/persistence"
)

// UserInfoClaims creates OIDC claims from a user.
func UserInfoClaims(user *persistence.User, scopes []string) *oidc.UserInfo {
	info := &oidc.UserInfo{
		Subject: user.ID,
	}

	// Check which scopes are requested
	hasProfile := false
	hasEmail := false
	for _, scope := range scopes {
		switch scope {
		case oidc.ScopeProfile:
			hasProfile = true
		case oidc.ScopeEmail:
			hasEmail = true
		}
	}

	// Add profile claims
	if hasProfile {
		info.PreferredUsername = user.Username
		info.Name = user.Username
	}

	// Add email claims
	if hasEmail {
		info.Email = user.Email
		info.EmailVerified = oidc.Bool(true) // Family users are always verified
	}

	return info
}

// GetPrivateClaimsFromScopes returns custom claims based on requested scopes.
func GetPrivateClaimsFromScopes(ctx context.Context, user *persistence.User, scopes []string) map[string]any {
	claims := make(map[string]any)

	// Add role claim
	claims["role"] = string(user.Role)

	// Add allowed_apps for standard users
	if user.Role == persistence.UserRoleStandard && len(user.AllowedApps) > 0 {
		claims["allowed_apps"] = user.AllowedApps
	}

	return claims
}
