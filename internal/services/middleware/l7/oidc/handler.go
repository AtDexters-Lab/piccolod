package oidc

import (
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

	"piccolod/internal/auth"
	"piccolod/internal/cryptoutil"
	"piccolod/internal/services/middleware"
	"piccolod/internal/services/middleware/l7"
)

const (
	defaultSessionTTL   = 3600 // 1 hour default session TTL in seconds
	codeVerifierBytes   = 32   // PKCE code verifier size
	proxyClientIDPrefix = "piccolo-"
	proxyClientIDSuffix = "-proxy"
)

// ExchangeResult contains the result of an OIDC token exchange per
// RFC 20260122 §5.6.
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

// Config holds dependencies for the proxy OIDC handler.
//
// The caller-supplied half (SessionStore..UserCanAccessApp) is wired by
// gin_server.go. The proxy-supplied half (ArrivedViaTLS, NeedsEmbeddedMarker,
// ShouldPartitionCookies) is filled in by ProxyManager.SetProxyOIDCConfig
// because those predicates depend on the services-internal hint chain
// (migrates to middleware.HintFromContext in step 9). Callers MUST NOT set
// the proxy-supplied fields — SetProxyOIDCConfig overwrites them.
type Config struct {
	SessionStore     *auth.SessionStore
	UserManager      *auth.UserManager
	GetPortalOrigin  func(r *http.Request) string
	GetLocalHostname func() string
	ExchangeCode     func(ctx context.Context, code, redirectURI, codeVerifier string) (*ExchangeResult, error)
	GetUserInfo      func(ctx context.Context, accessToken string) (*UserInfoResult, error)
	UserCanAccessApp func(ctx context.Context, userID, appName string) (bool, error)

	// Wired by ProxyManager — see comment above.
	ArrivedViaTLS          l7.ArrivedViaTLSFn
	NeedsEmbeddedMarker    func(r *http.Request) bool
	ShouldPartitionCookies func(r *http.Request) bool
}

// Handler handles proxy-level OIDC flows per RFC 20260122 §5.
type Handler struct {
	stateStore *StateStore
	config     Config
}

// NewHandler creates a new proxy OIDC handler.
func NewHandler(config Config) *Handler {
	return &Handler{
		stateStore: NewStateStore(),
		config:     config,
	}
}

// InitiateFlow initiates an OIDC flow for an unauthenticated request per
// RFC 20260122 §5.2.
func (h *Handler) InitiateFlow(w http.ResponseWriter, r *http.Request, appName string, ep middleware.EndpointInfo) {
	codeVerifier, err := cryptoutil.GenerateSecureToken(codeVerifierBytes)
	if err != nil {
		log.Printf("ERROR: proxy OIDC: failed to generate code verifier: %v", err)
		l7.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "OIDC_INIT_FAILED")
		return
	}
	codeChallenge := computeCodeChallenge(codeVerifier)

	callbackOrigin := h.computeCallbackOrigin(r, ep)
	if callbackOrigin == "" {
		log.Printf("ERROR: proxy OIDC: failed to compute callback origin")
		l7.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "ORIGIN_COMPUTE_FAILED")
		return
	}

	originalPath := r.URL.Path
	if r.URL.RawQuery != "" {
		originalPath += "?" + r.URL.RawQuery
	}

	// Detect iframe context for CHIPS propagation. The initial OIDC redirect
	// is the last point where Sec-Fetch-Dest: iframe is available; subsequent
	// redirects within the frame carry Sec-Fetch-Dest: document.
	isIframe := h.config.NeedsEmbeddedMarker(r)

	state := &State{
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

	portalOrigin := ""
	if h.config.GetPortalOrigin != nil {
		portalOrigin = h.config.GetPortalOrigin(r)
	}
	if portalOrigin == "" {
		portalOrigin = h.computePortalOrigin(r)
	}

	redirectURI := callbackOrigin + CallbackPath
	clientID := ProxyClientID(appName)

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
func (h *Handler) HandleCallback(w http.ResponseWriter, r *http.Request, appName string, ep middleware.EndpointInfo) {
	code := r.URL.Query().Get("code")
	stateID := r.URL.Query().Get("state")

	if code == "" || stateID == "" {
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

	state, ok := h.stateStore.Validate(stateID)
	if !ok {
		log.Printf("WARN: proxy OIDC: invalid or expired state (len=%d)", len(stateID))
		l7.WriteJSONError(w, http.StatusBadRequest, "invalid_or_expired_state", "INVALID_STATE")
		return
	}

	callbackOrigin := h.computeCallbackOrigin(r, ep)
	if callbackOrigin != state.ExpectedOrigin {
		log.Printf("WARN: proxy OIDC: origin mismatch: got %s, expected %s", callbackOrigin, state.ExpectedOrigin)
		l7.WriteJSONError(w, http.StatusBadRequest, "origin_mismatch", "ORIGIN_MISMATCH")
		return
	}

	if state.ExpectedApp != appName {
		log.Printf("WARN: proxy OIDC: app mismatch: got %s, expected %s", appName, state.ExpectedApp)
		l7.WriteJSONError(w, http.StatusBadRequest, "app_mismatch", "APP_MISMATCH")
		return
	}

	redirectURI := state.ExpectedOrigin + CallbackPath
	result, err := h.config.ExchangeCode(r.Context(), code, redirectURI, state.CodeVerifier)
	if err != nil {
		log.Printf("ERROR: proxy OIDC: token exchange failed: %v", err)
		l7.WriteJSONError(w, http.StatusUnauthorized, "token_exchange_failed", "TOKEN_FAILED")
		return
	}

	userInfo, err := h.config.GetUserInfo(r.Context(), result.AccessToken)
	if err != nil {
		log.Printf("ERROR: proxy OIDC: userinfo failed: %v", err)
		l7.WriteJSONError(w, http.StatusUnauthorized, "userinfo_failed", "USERINFO_FAILED")
		return
	}

	if userInfo.Role != "admin" && h.config.UserCanAccessApp != nil {
		allowed, err := h.config.UserCanAccessApp(r.Context(), userInfo.Sub, appName)
		if err != nil || !allowed {
			log.Printf("WARN: proxy OIDC: user %s not allowed to access app %s", userInfo.Sub, appName)
			h.renderAccessDenied(w, r, appName)
			return
		}
	}

	sess := h.config.SessionStore.CreateAppSession(auth.AppSessionParams{
		UserID:          userInfo.Sub,
		Username:        userInfo.Username,
		Role:            userInfo.Role,
		AppName:         appName,
		BoundOrigin:     callbackOrigin,
		ParentSessionID: result.PortalSessionID,
		TTLSeconds:      defaultSessionTTL,
	})

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
	}

	if h.config.ArrivedViaTLS(r) {
		cookie.Secure = true
	}

	// CHIPS: partition cookies for host-based HTTPS LAN iframe embedding.
	// Use the stored IsIframe flag from the OIDC state since Sec-Fetch-Dest
	// is no longer "iframe" by the time the callback fires (it's "document"
	// for navigations within a frame).
	if state.IsIframe || h.config.ShouldPartitionCookies(r) {
		cookie.SameSite = http.SameSiteNoneMode
		cookie.Secure = true
		cookie.Partitioned = true
	}

	http.SetCookie(w, cookie)

	// Re-set the embedded marker cookie so subsequent XHR/fetch from within
	// the iframe propagate the CHIPS context.
	if state.IsIframe {
		http.SetCookie(w, l7.EmbeddedMarkerCookie())
	}

	redirectURL := state.ExpectedOrigin + state.OriginalPath
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// computeCallbackOrigin computes the canonical callback origin for the request.
//
// LAN access paths (host-based + port-based) derive the origin from trusted
// internal state per RFC 20260122 §5.4. Remote/Alias access falls back to the
// generic l7.ComputeRequestOrigin formatter; for those paths the OIDC
// provider's redirect URI validation (exact match against pre-computed valid
// URIs) is the security boundary.
func (h *Handler) computeCallbackOrigin(r *http.Request, ep middleware.EndpointInfo) string {
	scheme := "http"
	if h.config.ArrivedViaTLS(r) {
		scheme = "https"
	}

	localHostname := ""
	if h.config.GetLocalHostname != nil {
		localHostname = strings.ToLower(h.config.GetLocalHostname())
	}

	reqHost := l7.NormalizeHostNoPort(r.Host)

	// LAN host-based: derive from ep.DerivedHostLabel + known base hostname.
	if ep.DerivedHostLabel != "" && localHostname != "" {
		expectedHost := strings.ToLower(ep.DerivedHostLabel + "-" + localHostname)
		if strings.EqualFold(reqHost, expectedHost) {
			return scheme + "://" + expectedHost
		}
	}

	// LAN port-based: derive from known base hostname + ep.PublicPort.
	if ep.PublicPort > 0 && localHostname != "" {
		if strings.EqualFold(reqHost, localHostname) {
			if (scheme == "http" && ep.PublicPort == 80) || (scheme == "https" && ep.PublicPort == 443) {
				return scheme + "://" + localHostname
			}
			return scheme + "://" + localHostname + ":" + strconv.Itoa(ep.PublicPort)
		}
	}

	return l7.ComputeRequestOrigin(r, ep, h.config.ArrivedViaTLS)
}

// computePortalOrigin computes the portal origin for redirects.
func (h *Handler) computePortalOrigin(r *http.Request) string {
	scheme := "http"
	if h.config.ArrivedViaTLS(r) {
		scheme = "https"
	}

	hostname := "piccolo.local"
	if h.config.GetLocalHostname != nil {
		if name := h.config.GetLocalHostname(); name != "" {
			hostname = name
		}
	}

	return scheme + "://" + hostname
}

// renderAccessDenied renders a user-friendly access denied page per
// RFC 20260122 §5.3.
func (h *Handler) renderAccessDenied(w http.ResponseWriter, r *http.Request, appName string) {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		l7.WriteJSONError(w, http.StatusForbidden, "forbidden", "APP_NOT_ALLOWED")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	page := fmt.Sprintf(`<!DOCTYPE html>
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
	w.Write([]byte(page))
}

// ProxyClientID returns the OIDC client ID for an app's proxy client.
func ProxyClientID(appName string) string {
	return proxyClientIDPrefix + appName + proxyClientIDSuffix
}

// computeCodeChallenge computes the PKCE code challenge using S256 method.
func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// isPortBasedAccess checks if the request is using port-based LAN routing.
func isPortBasedAccess(r *http.Request) bool {
	if r == nil {
		return false
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
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
