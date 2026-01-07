package services

import (
	"net/http"
	"strings"

	"piccolod/internal/auth"
	"piccolod/internal/persistence"
)

// TrustedHeadersConfig configures the trusted headers middleware.
type TrustedHeadersConfig struct {
	// UserManager provides user lookup for header injection
	UserManager *auth.UserManager

	// SessionGetter retrieves session from request
	SessionGetter func(r *http.Request) (*auth.Session, bool)

	// AppIDResolver resolves the target app ID from the request (e.g., from host or path)
	AppIDResolver func(r *http.Request) string
}

// TrustedHeadersMiddleware injects X-Piccolo-* headers for apps using the "headers" auth strategy.
// This middleware:
// 1. Strips any incoming X-Piccolo-* headers (security)
// 2. Validates the user session
// 3. Checks allowed_apps for standard users
// 4. Injects trusted headers: X-Piccolo-User, X-Piccolo-Email, X-Piccolo-Name, X-Piccolo-Role
type TrustedHeadersMiddleware struct {
	config TrustedHeadersConfig
}

// NewTrustedHeadersMiddleware creates a new trusted headers middleware.
func NewTrustedHeadersMiddleware(cfg TrustedHeadersConfig) *TrustedHeadersMiddleware {
	return &TrustedHeadersMiddleware{config: cfg}
}

// PiccoloHeaders are the trusted headers that will be injected.
const (
	HeaderPiccoloUser  = "X-Piccolo-User"
	HeaderPiccoloEmail = "X-Piccolo-Email"
	HeaderPiccoloName  = "X-Piccolo-Name"
	HeaderPiccoloRole  = "X-Piccolo-Role"
)

// piccoloHeaderPrefix is the prefix for all Piccolo headers that should be stripped.
const piccoloHeaderPrefix = "X-Piccolo-"

// Wrap wraps an http.Handler with trusted header injection.
func (m *TrustedHeadersMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Step 1: Strip all incoming X-Piccolo-* headers to prevent spoofing
		stripPiccoloHeaders(r)

		// Step 2: Get session
		sess, ok := m.config.SessionGetter(r)
		if !ok || sess == nil {
			// Authentication required for trusted header strategy
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Step 3: Get user info
		if m.config.UserManager == nil {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		user, err := m.config.UserManager.Get(r.Context(), sess.UserID)
		if err != nil {
			// User not found
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Step 4: Check allowed_apps for standard users
		if user.Role == persistence.UserRoleStandard {
			appID := ""
			if m.config.AppIDResolver != nil {
				appID = m.config.AppIDResolver(r)
			}
			if appID != "" && !isAppAllowed(user.AllowedApps, appID) {
				http.Error(w, "Forbidden: app not allowed for user", http.StatusForbidden)
				return
			}
		}

		// Step 5: Inject trusted headers
		r.Header.Set(HeaderPiccoloUser, user.Username)
		r.Header.Set(HeaderPiccoloEmail, user.Email)
		r.Header.Set(HeaderPiccoloName, user.Username) // Use username as display name
		r.Header.Set(HeaderPiccoloRole, string(user.Role))

		next.ServeHTTP(w, r)
	})
}

// stripPiccoloHeaders removes all headers starting with X-Piccolo- from the request.
// This prevents clients from spoofing trusted headers.
func stripPiccoloHeaders(r *http.Request) {
	toDelete := make([]string, 0)
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), strings.ToLower(piccoloHeaderPrefix)) {
			toDelete = append(toDelete, key)
		}
	}
	for _, key := range toDelete {
		r.Header.Del(key)
	}
}

// isAppAllowed checks if the given app ID is in the user's allowed apps list.
// Returns false if allowed_apps is empty (deny all) or if appID is not in the list.
func isAppAllowed(allowedApps []string, appID string) bool {
	if len(allowedApps) == 0 {
		// Empty allowed_apps means NO apps are allowed (explicit grant required)
		return false
	}
	for _, allowed := range allowedApps {
		if allowed == appID {
			return true
		}
	}
	return false
}

// InjectHeadersForRequest is a convenience function to inject headers into an outgoing request.
// This can be used by the reverse proxy to inject headers after authentication.
func InjectHeadersForRequest(r *http.Request, user *auth.UserInfo) {
	r.Header.Set(HeaderPiccoloUser, user.Username)
	r.Header.Set(HeaderPiccoloEmail, user.Email)
	r.Header.Set(HeaderPiccoloName, user.Username)
	r.Header.Set(HeaderPiccoloRole, string(user.Role))
}

// StripHeadersFromRequest removes all X-Piccolo-* headers from a request.
// Use this before forwarding requests to ensure clients cannot spoof headers.
func StripHeadersFromRequest(r *http.Request) {
	stripPiccoloHeaders(r)
}
