// Package services provides proxy-level OIDC authentication per RFC 20260122.
// This file implements the OIDC client flow at the proxy layer for `headers` and `protected`
// auth strategies, enabling seamless SSO across all access domains.
package services

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/auth"
	"piccolod/internal/cryptoutil"
	"piccolod/internal/services/middleware/l7"
)

const (
	// ProxyOIDCCallbackPath is the reserved path for proxy OIDC callbacks per RFC 20260122 §5.10.
	ProxyOIDCCallbackPath = "/__piccolod_oidc/callback"

	// ProxyOIDCLogoutPath is the reserved path for app-specific logout (future use).
	ProxyOIDCLogoutPath = "/__piccolod_oidc/logout"

	// ProxyOIDCSessionPath is the reserved path for session status API (future use).
	ProxyOIDCSessionPath = "/__piccolod_oidc/session"

	// Default state store configuration per RFC 20260122 §5.7.
	defaultStateTTL     = 10 * time.Minute // TTL for OIDC state entries
	defaultMaxEntries   = 10000            // Max entries before LRU eviction
	defaultSessionTTL   = 3600             // 1 hour default session TTL in seconds
	stateTokenBytes     = 32               // 256-bit state tokens
	codeVerifierBytes   = 32               // PKCE code verifier size
	proxyClientIDPrefix = "piccolo-"       // Prefix for proxy OIDC client IDs
	proxyClientIDSuffix = "-proxy"         // Suffix for proxy OIDC client IDs
)

// OIDCState represents the stored state for an OIDC authorization flow per RFC 20260122 §5.7.
type OIDCState struct {
	ID             string    // Opaque state token sent to authorization server
	CodeVerifier   string    // PKCE code_verifier (for token exchange)
	OriginalPath   string    // Path+query to redirect back to (MUST be relative, no scheme/host)
	ExpectedApp    string    // App name (prevents confused deputy)
	ExpectedOrigin string    // Expected callback origin (scheme://host[:port])
	IsIframe       bool      // True when the OIDC flow was initiated from an iframe context (CHIPS)
	CreatedAt      time.Time // For expiry
}

// OIDCStateStore manages OIDC state entries with TTL and LRU eviction per RFC 20260122 §5.7.
type OIDCStateStore struct {
	mu         sync.Mutex
	states     map[string]*list.Element
	lruList    *list.List
	maxEntries int
	ttl        time.Duration
}

type stateEntry struct {
	state     *OIDCState
	expiresAt time.Time
}

// NewOIDCStateStore creates a new OIDC state store with default configuration.
func NewOIDCStateStore() *OIDCStateStore {
	return &OIDCStateStore{
		states:     make(map[string]*list.Element),
		lruList:    list.New(),
		maxEntries: defaultMaxEntries,
		ttl:        defaultStateTTL,
	}
}

// Create creates a new OIDC state entry and returns its ID.
func (s *OIDCStateStore) Create(state *OIDCState) error {
	if state == nil {
		return fmt.Errorf("state is nil")
	}

	// Generate state ID
	stateID, err := cryptoutil.GenerateSecureToken(stateTokenBytes)
	if err != nil {
		return fmt.Errorf("failed to generate state ID: %w", err)
	}
	state.ID = stateID
	state.CreatedAt = time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Prune expired entries
	s.pruneExpiredLocked()

	// Evict LRU if at capacity
	for len(s.states) >= s.maxEntries && s.lruList.Len() > 0 {
		oldest := s.lruList.Back()
		if oldest != nil {
			entry := oldest.Value.(*stateEntry)
			delete(s.states, entry.state.ID)
			s.lruList.Remove(oldest)
		}
	}

	// Add new entry
	entry := &stateEntry{
		state:     state,
		expiresAt: state.CreatedAt.Add(s.ttl),
	}
	elem := s.lruList.PushFront(entry)
	s.states[state.ID] = elem

	return nil
}

// Validate retrieves and removes an OIDC state entry (one-time use).
// Returns the state and true if valid, nil and false otherwise.
func (s *OIDCStateStore) Validate(stateID string) (*OIDCState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked()

	elem, ok := s.states[stateID]
	if !ok {
		return nil, false
	}

	entry := elem.Value.(*stateEntry)
	if time.Now().After(entry.expiresAt) {
		// Expired
		delete(s.states, stateID)
		s.lruList.Remove(elem)
		return nil, false
	}

	// One-time use: remove after validation
	delete(s.states, stateID)
	s.lruList.Remove(elem)

	return entry.state, true
}

func (s *OIDCStateStore) pruneExpiredLocked() {
	now := time.Now()
	for s.lruList.Len() > 0 {
		oldest := s.lruList.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*stateEntry)
		if now.After(entry.expiresAt) {
			delete(s.states, entry.state.ID)
			s.lruList.Remove(oldest)
		} else {
			// List is ordered by insertion time, so if oldest is not expired, none are
			break
		}
	}
}

// ProxyOIDCConfig holds configuration for the proxy OIDC flow.
type ProxyOIDCConfig struct {
	// SessionStore for creating app sessions
	SessionStore *auth.SessionStore

	// UserManager for looking up user info
	UserManager *auth.UserManager

	// GetPortalOrigin returns the portal origin for a request context
	GetPortalOrigin func(r *http.Request) string

	// GetLocalHostname returns the current mDNS hostname
	GetLocalHostname func() string

	// ExchangeCode performs the token exchange (in-process call to OIDC provider)
	ExchangeCode func(ctx context.Context, code, redirectURI, codeVerifier string) (*ExchangeResult, error)

	// GetUserInfo retrieves user info from access token
	GetUserInfo func(ctx context.Context, accessToken string) (*UserInfoResult, error)

	// UserCanAccessApp checks if a user can access an app (allowed_apps)
	UserCanAccessApp func(ctx context.Context, userID, appName string) (bool, error)
}

// ExchangeResult contains the result of an OIDC token exchange per RFC 20260122 §5.6.
type ExchangeResult struct {
	AccessToken     string
	RefreshToken    string
	TokenType       string
	ExpiresIn       int64
	PortalSessionID string // The portal session that approved this authorization
}

// UserInfoResult contains user info from the OIDC userinfo endpoint.
type UserInfoResult struct {
	Sub      string
	Username string
	Email    string
	Role     string
}

// ProxyOIDCHandler handles proxy-level OIDC flows per RFC 20260122 §5.
type ProxyOIDCHandler struct {
	stateStore *OIDCStateStore
	config     ProxyOIDCConfig
}

// NewProxyOIDCHandler creates a new proxy OIDC handler.
func NewProxyOIDCHandler(config ProxyOIDCConfig) *ProxyOIDCHandler {
	return &ProxyOIDCHandler{
		stateStore: NewOIDCStateStore(),
		config:     config,
	}
}

// IsReservedPath returns true if the path is reserved for proxy OIDC operations.
func IsReservedPath(path string) bool {
	return strings.HasPrefix(path, "/__piccolod_oidc/")
}

// InitiateOIDCFlow initiates an OIDC flow for an unauthenticated request per RFC 20260122 §5.2.
// Returns the redirect URL to the authorization endpoint.
func (h *ProxyOIDCHandler) InitiateOIDCFlow(w http.ResponseWriter, r *http.Request, appName string, endpoint ServiceEndpoint) {
	// Generate PKCE code verifier and challenge
	codeVerifier, err := cryptoutil.GenerateSecureToken(codeVerifierBytes)
	if err != nil {
		log.Printf("ERROR: proxy OIDC: failed to generate code verifier: %v", err)
		l7.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "OIDC_INIT_FAILED")
		return
	}
	codeChallenge := computeCodeChallenge(codeVerifier)

	// Compute expected callback origin
	callbackOrigin := h.computeCallbackOrigin(r, endpoint)
	if callbackOrigin == "" {
		log.Printf("ERROR: proxy OIDC: failed to compute callback origin")
		l7.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "ORIGIN_COMPUTE_FAILED")
		return
	}

	// Store original path (relative only for open redirect prevention)
	originalPath := r.URL.Path
	if r.URL.RawQuery != "" {
		originalPath += "?" + r.URL.RawQuery
	}

	// Detect iframe context for CHIPS propagation. The initial OIDC redirect
	// is the last point where Sec-Fetch-Dest: iframe is available; subsequent
	// redirects within the frame carry Sec-Fetch-Dest: document.
	isIframe := needsEmbeddedMarker(r)

	// Create state entry
	state := &OIDCState{
		CodeVerifier:   codeVerifier,
		OriginalPath:   originalPath,
		ExpectedApp:    appName,
		ExpectedOrigin: callbackOrigin,
		IsIframe:       isIframe,
	}

	if err := h.stateStore.Create(state); err != nil {
		log.Printf("ERROR: proxy OIDC: failed to create state: %v", err)
		l7.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "STATE_CREATE_FAILED")
		return
	}

	// Set the embedded marker cookie early (on the redirect response) so the
	// browser stores it in the iframe partition before navigating to the IdP.
	if isIframe {
		http.SetCookie(w, l7.EmbeddedMarkerCookie())
	}

	// Build authorization URL
	portalOrigin := ""
	if h.config.GetPortalOrigin != nil {
		portalOrigin = h.config.GetPortalOrigin(r)
	}
	if portalOrigin == "" {
		// Fallback to computing from request
		portalOrigin = h.computePortalOrigin(r)
	}

	// Build redirect URI for callback
	redirectURI := callbackOrigin + ProxyOIDCCallbackPath

	// Build client ID for this app
	clientID := ProxyClientID(appName)

	// Build authorization URL per RFC 20260122 §5.2
	authURL := fmt.Sprintf("%s/oauth/authorize?"+
		"client_id=%s&"+
		"redirect_uri=%s&"+
		"response_type=code&"+
		"scope=openid&"+
		"state=%s&"+
		"code_challenge=%s&"+
		"code_challenge_method=S256",
		portalOrigin,
		url.QueryEscape(clientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape(state.ID),
		url.QueryEscape(codeChallenge),
	)

	http.Redirect(w, r, authURL, http.StatusFound)
}

// HandleCallback handles the OIDC callback per RFC 20260122 §5.8.
func (h *ProxyOIDCHandler) HandleCallback(w http.ResponseWriter, r *http.Request, appName string, endpoint ServiceEndpoint) {
	// Extract authorization code and state
	code := r.URL.Query().Get("code")
	stateID := r.URL.Query().Get("state")

	if code == "" || stateID == "" {
		// Check for error response
		if errCode := r.URL.Query().Get("error"); errCode != "" {
			errDesc := r.URL.Query().Get("error_description")
			log.Printf("WARN: proxy OIDC callback error: %s - %s", errCode, errDesc)
			if errCode == "access_denied" {
				h.renderAccessDenied(w, r, appName)
				return
			}
			l7.WriteJSONError(w, http.StatusBadRequest, "authorization_failed", "AUTH_FAILED")
			return
		}
		l7.WriteJSONError(w, http.StatusBadRequest, "invalid_callback", "MISSING_PARAMS")
		return
	}

	// Validate state and recover stored data
	state, ok := h.stateStore.Validate(stateID)
	if !ok {
		log.Printf("WARN: proxy OIDC: invalid or expired state (len=%d)", len(stateID))
		l7.WriteJSONError(w, http.StatusBadRequest, "invalid_or_expired_state", "INVALID_STATE")
		return
	}

	// Verify callback origin matches expected origin
	callbackOrigin := h.computeCallbackOrigin(r, endpoint)
	if callbackOrigin != state.ExpectedOrigin {
		log.Printf("WARN: proxy OIDC: origin mismatch: got %s, expected %s", callbackOrigin, state.ExpectedOrigin)
		l7.WriteJSONError(w, http.StatusBadRequest, "origin_mismatch", "ORIGIN_MISMATCH")
		return
	}

	// Verify app matches
	if state.ExpectedApp != appName {
		log.Printf("WARN: proxy OIDC: app mismatch: got %s, expected %s", appName, state.ExpectedApp)
		l7.WriteJSONError(w, http.StatusBadRequest, "app_mismatch", "APP_MISMATCH")
		return
	}

	// Exchange code for tokens (back-channel, using stored PKCE verifier)
	redirectURI := state.ExpectedOrigin + ProxyOIDCCallbackPath
	result, err := h.config.ExchangeCode(r.Context(), code, redirectURI, state.CodeVerifier)
	if err != nil {
		log.Printf("ERROR: proxy OIDC: token exchange failed: %v", err)
		l7.WriteJSONError(w, http.StatusUnauthorized, "token_exchange_failed", "TOKEN_FAILED")
		return
	}

	// Get user info
	userInfo, err := h.config.GetUserInfo(r.Context(), result.AccessToken)
	if err != nil {
		log.Printf("ERROR: proxy OIDC: userinfo failed: %v", err)
		l7.WriteJSONError(w, http.StatusUnauthorized, "userinfo_failed", "USERINFO_FAILED")
		return
	}

	// Check allowed_apps for non-admin users
	if userInfo.Role != "admin" && h.config.UserCanAccessApp != nil {
		allowed, err := h.config.UserCanAccessApp(r.Context(), userInfo.Sub, appName)
		if err != nil || !allowed {
			log.Printf("WARN: proxy OIDC: user %s not allowed to access app %s", userInfo.Sub, appName)
			h.renderAccessDenied(w, r, appName)
			return
		}
	}

	// Create app-scoped session with parent linkage
	sess := h.config.SessionStore.CreateAppSession(auth.AppSessionParams{
		UserID:          userInfo.Sub,
		Username:        userInfo.Username,
		Role:            userInfo.Role,
		AppName:         appName,
		BoundOrigin:     callbackOrigin,
		ParentSessionID: result.PortalSessionID,
		TTLSeconds:      defaultSessionTTL,
	})

	// Set host-only cookie with appropriate security attributes
	cookieName := l7.SessionCookieName
	if isPortBasedAccess(r) {
		if port := requestPort(r); port > 0 {
			cookieName = fmt.Sprintf("piccolo_app_session_p%d", port)
		}
	}

	cookie := &http.Cookie{
		Name:     cookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Domain omitted = host-only
	}

	// Set Secure flag for HTTPS (must match computeCallbackOrigin logic)
	if RequestArrivedViaTLS(r) {
		cookie.Secure = true
	}

	// CHIPS: partition cookies for host-based HTTPS LAN iframe embedding.
	// Use the stored IsIframe flag from the OIDC state since Sec-Fetch-Dest
	// is no longer "iframe" by the time the callback fires (it's "document"
	// for navigations within a frame).
	if state.IsIframe || shouldPartitionCookies(r) {
		cookie.SameSite = http.SameSiteNoneMode
		cookie.Secure = true
		cookie.Partitioned = true
	}

	http.SetCookie(w, cookie)

	// Re-set the embedded marker cookie on the callback response so subsequent
	// XHR/fetch from within the iframe propagate the CHIPS context.
	if state.IsIframe {
		http.SetCookie(w, l7.EmbeddedMarkerCookie())
	}

	// Redirect to original path (safe: OriginalPath is relative, not absolute)
	redirectURL := state.ExpectedOrigin + state.OriginalPath
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// computeCallbackOrigin computes the canonical callback origin for the request.
func (h *ProxyOIDCHandler) computeCallbackOrigin(r *http.Request, ep ServiceEndpoint) string {
	scheme := "http"
	// Use RequestArrivedViaTLS to detect TLS from both direct TLS and TLS mux (connection hint).
	if RequestArrivedViaTLS(r) {
		scheme = "https"
	}

	// RFC 20260122 §5.4: Derive origin from trusted internal state when possible.
	localHostname := ""
	if h.config.GetLocalHostname != nil {
		localHostname = strings.ToLower(h.config.GetLocalHostname())
	}

	reqHost := l7.NormalizeHostNoPort(r.Host)

	// LAN host-based: derive from ep.DerivedHostLabel + known base hostname
	if ep.DerivedHostLabel != "" && localHostname != "" {
		expectedHost := strings.ToLower(ep.DerivedHostLabel + "-" + localHostname)
		if strings.EqualFold(reqHost, expectedHost) {
			return scheme + "://" + expectedHost
		}
	}

	// LAN port-based: derive from known base hostname + ep.PublicPort
	if ep.PublicPort > 0 && localHostname != "" {
		if strings.EqualFold(reqHost, localHostname) {
			if (scheme == "http" && ep.PublicPort == 80) || (scheme == "https" && ep.PublicPort == 443) {
				return scheme + "://" + localHostname
			}
			return scheme + "://" + localHostname + ":" + strconv.Itoa(ep.PublicPort)
		}
	}

	// Fallback for Remote/Alias access: derive from r.Host.
	// Security note: the OIDC provider's redirect URI validation (exact match against
	// pre-computed valid URIs) is the security boundary for these access paths.
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
		portStr = ""
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}

	// Strip existing brackets before re-adding to avoid double-bracketing
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")

	// Re-bracket IPv6 per RFC 3986
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	// Omit default ports
	if portStr != "" {
		port, parseErr := strconv.Atoi(portStr)
		if parseErr == nil {
			if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
				return scheme + "://" + host + ":" + portStr
			}
		}
	}

	return scheme + "://" + host
}

// computePortalOrigin computes the portal origin for redirects.
func (h *ProxyOIDCHandler) computePortalOrigin(r *http.Request) string {
	scheme := "http"
	if RequestArrivedViaTLS(r) {
		scheme = "https"
	}

	// Get base hostname from mDNS manager
	hostname := "piccolo.local"
	if h.config.GetLocalHostname != nil {
		if name := h.config.GetLocalHostname(); name != "" {
			hostname = name
		}
	}

	return scheme + "://" + hostname
}

// renderAccessDenied renders a user-friendly access denied page per RFC 20260122 §5.3.
func (h *ProxyOIDCHandler) renderAccessDenied(w http.ResponseWriter, r *http.Request, appName string) {
	// Check if API client
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		l7.WriteJSONError(w, http.StatusForbidden, "forbidden", "APP_NOT_ALLOWED")
		return
	}

	// Render HTML for browsers
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>Access Denied</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 0; padding: 40px; background: #f5f5f5; }
        .container { max-width: 500px; margin: 0 auto; background: white; padding: 40px; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
        h1 { color: #d32f2f; margin-top: 0; }
        p { color: #666; line-height: 1.6; }
        a { color: #1976d2; text-decoration: none; }
        a:hover { text-decoration: underline; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Access Denied</h1>
        <p>You don't have permission to access <strong>%s</strong>.</p>
        <p>Contact your administrator to request access.</p>
        <p><a href="/">Back to Portal</a></p>
    </div>
</body>
</html>`, html.EscapeString(appName))
	w.Write([]byte(html))
}

// ProxyClientID returns the OIDC client ID for an app's proxy client.
func ProxyClientID(appName string) string {
	return proxyClientIDPrefix + appName + proxyClientIDSuffix
}

// computeCodeChallenge computes the PKCE code challenge using S256 method.
func computeCodeChallenge(verifier string) string {
	// S256: BASE64URL(SHA256(verifier))
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// isPortBasedAccess checks if the request is using port-based LAN routing.
func isPortBasedAccess(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := r.Host
	// If host contains a non-default port, it's port-based
	if _, port, err := net.SplitHostPort(host); err == nil {
		if port != "80" && port != "443" {
			return true
		}
	}
	return false
}

// requestPort extracts the port from the request Host.
func requestPort(r *http.Request) int {
	if r == nil {
		return 0
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		if p, err := strconv.Atoi(port); err == nil {
			return p
		}
	}
	return 0
}
