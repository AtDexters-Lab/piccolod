package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	zitadelOIDC "github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"piccolod/internal/oidc"
	"piccolod/internal/services"
)

// getOIDCClientManager helper to get client manager from persistence
func (s *GinServer) getOIDCClientManager() *oidc.ClientManager {
	if s.persistence == nil || s.persistence.Control() == nil {
		return nil
	}
	repo := s.persistence.Control().OIDCClients()
	return oidc.NewClientManager(repo)
}

// oidcIssuer returns the OIDC issuer URL using the machine-specific hostname.
// Falls back to piccolo.local when mDNS is disabled.
func (s *GinServer) oidcIssuer() string {
	if s.mdnsManager != nil {
		return "https://" + s.mdnsManager.SpecificHostname()
	}
	return "https://piccolo.local"
}

type requestedRedirectURIKey struct{}

var requestedRedirectURIContextKey = requestedRedirectURIKey{}

// validOrigin represents a valid OAuth redirect origin for an app.
type validOrigin struct {
	scheme string // "http" or "https"
	host   string // hostname (lowercase, without port)
	port   int    // 0 = default port for scheme; >0 = explicit port required
}

// toBaseURL returns the origin as a URL string (without trailing slash).
func (o validOrigin) toBaseURL() string {
	if o.port == 0 {
		return o.scheme + "://" + o.host
	}
	return fmt.Sprintf("%s://%s:%d", o.scheme, o.host, o.port)
}

// initOIDCProvider initializes the OIDC provider if enabled/configured
func (s *GinServer) initOIDCProvider() (*oidc.Provider, error) {
	if s.persistence == nil {
		return nil, errors.New("persistence not available")
	}
	control := s.persistence.Control()

	cfg := oidc.ProviderConfig{
		Issuer:          s.oidcIssuer(),
		Users:           control.Users(),
		Clients:         control.OIDCClients(),
		Keys:            control.OIDCKeys(),
		AuthCodes:       control.OIDCAuthCodes(),
		RefreshTokens:   control.OIDCRefreshTokens(),
		Config:          control.OIDCConfig(),
		ResolveRedirect: s.resolveAppRedirectURI,
		Logger:          slog.Default(),
	}

	// Override VerifyPassword to use UserManager/AuthManager hash logic
	if s.authManager != nil {
		cfg.VerifyPassword = s.authManager.VerifyHash
	}

	return oidc.NewProvider(context.Background(), cfg)
}

// resolveAppRedirectURI generates the exhaustive list of valid redirect URIs for an app.
// It computes the cartesian product of:
//   - All valid access origins (Remote, Alias, LAN host-based, LAN port-based, local IP)
//   - All declared redirect_uri_paths from the app manifest
//
// Additionally, explicit redirect_uris (for native apps) are appended as-is.
// RFC 20260122 §5.3: Proxy OIDC clients also include the reserved callback path.
// This approach is fully compliant with OAuth 2.0 exact URI matching (RFC 6749, RFC 9700).
func (s *GinServer) resolveAppRedirectURI(ctx context.Context, appID string) ([]string, error) {
	if s.serviceManager == nil {
		return nil, nil
	}

	endpoints, err := s.serviceManager.GetByApp(appID)
	if err != nil {
		return nil, nil
	}

	// Collect redirect_uri_paths and explicit redirect_uris from app manifest.
	paths, explicitURIs := s.collectRedirectConfig(ctx, appID)

	// RFC 20260122 §5.3: Include proxy OIDC callback path for apps using headers/protected strategies.
	if s.appUsesProxyAuth(ctx, appID) {
		paths = append(paths, services.ProxyOIDCCallbackPath)
	}

	if len(paths) == 0 {
		// No paths declared - return only explicit URIs (if any).
		return explicitURIs, nil
	}

	// Build all valid origins for this app.
	origins := s.buildValidOrigins(endpoints)

	// Generate cartesian product: origins × paths
	var uris []string
	for _, origin := range origins {
		baseURL := origin.toBaseURL()
		for _, path := range paths {
			uris = append(uris, baseURL+path)
		}
	}

	// Append explicit URIs (native/desktop apps with loopback or custom schemes).
	uris = append(uris, explicitURIs...)

	return uris, nil
}

// appUsesProxyAuth returns true if the app has any listeners using headers/protected auth strategy.
// These apps require proxy OIDC clients with the reserved callback path.
func (s *GinServer) appUsesProxyAuth(ctx context.Context, appID string) bool {
	if s.appManager == nil {
		return false
	}

	inst, err := s.appManager.Get(ctx, appID)
	if err != nil || inst == nil || inst.Definition == nil {
		return false
	}

	for _, l := range inst.Definition.Listeners {
		if l.Auth == nil || len(l.Auth.Rules) == 0 {
			// Auth omitted → all paths default to "protected" strategy.
			return true
		}
		for _, rule := range l.Auth.Rules {
			if rule.Strategy == "headers" || rule.Strategy == "protected" {
				return true
			}
		}
	}
	return false
}

// collectRedirectConfig extracts redirect_uri_paths and redirect_uris from app manifest.
func (s *GinServer) collectRedirectConfig(ctx context.Context, appID string) (paths []string, explicitURIs []string) {
	if s.appManager == nil {
		return nil, nil
	}

	inst, err := s.appManager.Get(ctx, appID)
	if err != nil || inst == nil || inst.Definition == nil {
		return nil, nil
	}

	seenPaths := make(map[string]struct{})
	seenURIs := make(map[string]struct{})

	for _, svc := range inst.Definition.Services {
		if svc.OIDCClient == nil {
			continue
		}
		for _, p := range svc.OIDCClient.RedirectURIPaths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seenPaths[p]; ok {
				continue
			}
			seenPaths[p] = struct{}{}
			paths = append(paths, p)
		}
		for _, u := range svc.OIDCClient.RedirectURIs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if _, ok := seenURIs[u]; ok {
				continue
			}
			seenURIs[u] = struct{}{}
			explicitURIs = append(explicitURIs, u)
		}
	}

	return paths, explicitURIs
}

// buildValidOrigins constructs all valid redirect origins for an app's endpoints.
func (s *GinServer) buildValidOrigins(endpoints []services.ServiceEndpoint) []validOrigin {
	var origins []validOrigin

	localHostname := "piccolo.local"
	if s.mdnsManager != nil {
		localHostname = s.mdnsManager.Hostname()
	}
	localHostname = strings.ToLower(localHostname)

	// Collect all remote portal hostnames from the resolver (source-agnostic).
	portalHosts := s.portalHosts()

	for _, ep := range endpoints {
		// Remote access: https://<app>.<portal> for each known portal hostname
		for _, portal := range portalHosts {
			if remoteHost := services.RemoteServiceHostname(ep.DerivedHostLabel, portal); remoteHost != "" {
				origins = append(origins, validOrigin{"https", strings.ToLower(remoteHost), 0})
			}
		}

		// Alias domains: https://<alias>
		if s.remoteResolver != nil {
			for aliasHost, hostLabel := range s.remoteResolver.AliasHostLabels() {
				if hostLabel == ep.DerivedHostLabel {
					origins = append(origins, validOrigin{"https", aliasHost, 0})
				}
			}
		}

		// LAN host-based: http://<app>-piccolo.local (or https) - 2-level format per RFC 20260122
		if ep.DerivedHostLabel != "" {
			lanHost := strings.ToLower(ep.DerivedHostLabel + "-" + localHostname)
			origins = append(origins, validOrigin{"http", lanHost, 0})
			origins = append(origins, validOrigin{"https", lanHost, 0})
		}

		// LAN port-based: http://piccolo.local:<port> (or https)
		origins = append(origins, validOrigin{"http", localHostname, ep.PublicPort})
		origins = append(origins, validOrigin{"https", localHostname, ep.PublicPort})
	}

	// Local IP-based access for mDNS-less clients.
	// Add origins for each local machine IP with each app port.
	localIPs := getLocalMachineIPs()
	for _, ep := range endpoints {
		for _, ip := range localIPs {
			origins = append(origins, validOrigin{"http", ip, ep.PublicPort})
		}
	}

	return origins
}

// getLocalMachineIPs returns IPv4 addresses of local network interfaces.
func getLocalMachineIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				ips = append(ips, ip4.String())
			}
		}
	}
	return ips
}

// OIDC Handlers ---------------------------------------------------------------

func (s *GinServer) handleOIDCDiscovery(c *gin.Context) {
	cfg := oidc.DiscoveryConfig{
		StableIssuer:     s.oidcIssuer(),
		GetLocalHostname: s.oidcLocalHostname,
		Logger:           slog.Default(),
	}
	h := oidc.NewDiscoveryHandler(cfg)
	h.ServeHTTP(c.Writer, c.Request)
}

// oidcLocalHostname returns the stable local hostname used in the OIDC
// discovery authorization_endpoint. Must match the proxy's oidcIssuerOrigin.
func (s *GinServer) oidcLocalHostname() string {
	if s.mdnsManager != nil {
		return s.mdnsManager.SpecificHostname()
	}
	return getPreferredOutboundIP()
}

// getPreferredOutboundIP returns a local IP address suitable for LAN access.
// Used when mDNS is disabled.
func getPreferredOutboundIP() string {
	// Connect to a non-routable address to determine preferred outbound IP
	conn, err := net.Dial("udp", "10.255.255.255:1")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return "127.0.0.1"
}

// isLocalMachineIP checks if the IP belongs to this machine.
// This prevents accepting redirects to attacker-controlled servers on LAN.
func isLocalMachineIP(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ipnet.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func (s *GinServer) handleOIDCAuthorize(c *gin.Context) {
	clientID := c.Request.FormValue("client_id")
	redirectURI := c.Request.FormValue("redirect_uri")
	// Use "id" parameter (OIDC library standard)
	authRequestID := c.Request.FormValue("id")

	// Log incoming authorize request for debugging
	slog.Info("OIDC authorize request",
		"client_id", clientID,
		"redirect_uri", redirectURI,
		"id", authRequestID,
		"response_type", c.Request.FormValue("response_type"),
		"scope", c.Request.FormValue("scope"),
	)

	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}

	// Case 1: Callback with authRequestID (after user login via portal)
	if authRequestID != "" && clientID == "" {
		s.handleOIDCAuthorizeCallback(c, p, authRequestID)
		return
	}

	// Case 2: New authorize request - check if user already has a portal session
	if clientID != "" {
		sess := s.getSessionFromContext(c)
		if sess != nil {
			slog.Info("OIDC authorize: user already authenticated, fast-path",
				"user_id", sess.UserID,
				"session_id", sess.ID,
				"client_id", clientID,
			)

			// User is already logged in - create auth request and immediately complete it
			// This avoids redirecting to login page
			// RFC 20260122 §6.3: Pass portal session ID for logout propagation
			s.handleOIDCAuthorizeFastPath(c, p, sess.UserID, sess.ID)
			return
		}
	}

	// Case 3: Regular authorize request (user not logged in)
	// Custom-scheme redirect URIs (e.g., app.immich:///oauth-callback) are rejected
	// by the zitadel library for ApplicationTypeWeb clients. We validate them ourselves
	// and create the auth request manually, bypassing the library's type-based validation.
	if isCustomSchemeRedirectURI(redirectURI) {
		s.handleCustomSchemeAuthorize(c, p)
		return
	}

	// Inject redirect_uri for validation
	if redirectURI != "" {
		ctx := context.WithValue(c.Request.Context(), requestedRedirectURIContextKey, redirectURI)
		c.Request = c.Request.WithContext(ctx)
	}

	p.Handler().ServeHTTP(c.Writer, c.Request)
}

// isCustomSchemeRedirectURI returns true if the URI uses a custom scheme
// (anything other than http:// or https://). Used to route mobile/native app
// OIDC requests around the zitadel library's type-based validation.
// Comparison is case-insensitive per RFC 3986 §3.1 (scheme).
func isCustomSchemeRedirectURI(redirectURI string) bool {
	if redirectURI == "" {
		return false
	}
	lower := strings.ToLower(redirectURI)
	return !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://")
}

// handleCustomSchemeAuthorize handles authorize requests with custom-scheme
// redirect URIs (e.g., app.immich:///oauth-callback). The zitadel library
// rejects custom schemes for ApplicationTypeWeb clients, so we validate
// and create the auth request ourselves, then redirect to the login page.
//
// Validation chain (must mirror op.Authorize for security):
//   - client_id present
//   - client exists in storage
//   - redirect_uri is in client's registered RedirectURIs (exact match)
//   - response_type is allowed for client (op.ValidateAuthReqResponseType)
//   - scopes are allowed for client (op.ValidateAuthReqScopes)
//   - prompt parameter is valid (op.ValidateAuthReqPrompt)
func (s *GinServer) handleCustomSchemeAuthorize(c *gin.Context, p *oidc.Provider) {
	authReq, err := op.ParseAuthorizeRequest(c.Request, p.Inner().Decoder())
	if err != nil {
		slog.Error("OIDC custom-scheme: failed to parse authorize request", "error", err)
		writeGinError(c, http.StatusBadRequest, "Invalid authorize request")
		return
	}

	if authReq.ClientID == "" {
		writeGinError(c, http.StatusBadRequest, "client_id is required")
		return
	}

	// OIDC Core §6: reject `request` parameter when request object support is disabled.
	// Our provider sets RequestObjectSupported=false, so this mirrors what op.Authorize would do.
	if authReq.RequestParam != "" {
		writeGinError(c, http.StatusBadRequest, "request parameter not supported")
		return
	}

	client, err := p.Storage().GetClientByClientID(c.Request.Context(), authReq.ClientID)
	if err != nil {
		slog.Error("OIDC custom-scheme: client not found", "client_id", authReq.ClientID, "error", err)
		writeGinError(c, http.StatusBadRequest, "Invalid client_id")
		return
	}

	// Validate redirect_uri is registered for this client (exact match).
	redirectURI := authReq.RedirectURI
	if !slices.Contains(client.RedirectURIs(), redirectURI) {
		slog.Error("OIDC custom-scheme: redirect_uri not registered",
			"client_id", authReq.ClientID, "redirect_uri", redirectURI)
		writeGinError(c, http.StatusBadRequest, "redirect_uri is not registered for this client")
		return
	}

	// Run standard library validations.
	if err := op.ValidateAuthReqResponseType(client, authReq.ResponseType); err != nil {
		slog.Error("OIDC custom-scheme: invalid response_type", "error", err)
		writeGinError(c, http.StatusBadRequest, "Invalid response_type")
		return
	}
	scopes, err := op.ValidateAuthReqScopes(client, authReq.Scopes)
	if err != nil {
		slog.Error("OIDC custom-scheme: invalid scopes", "error", err)
		writeGinError(c, http.StatusBadRequest, "Invalid scopes")
		return
	}
	authReq.Scopes = scopes

	maxAge, err := op.ValidateAuthReqPrompt(authReq.Prompt, authReq.MaxAge)
	if err != nil {
		slog.Error("OIDC custom-scheme: invalid prompt", "error", err)
		writeGinError(c, http.StatusBadRequest, "Invalid prompt parameter")
		return
	}
	authReq.MaxAge = maxAge

	// Inject redirect_uri for the storage layer.
	ctx := context.WithValue(c.Request.Context(), requestedRedirectURIContextKey, redirectURI)

	// Create auth request (no userID — user hasn't logged in yet).
	storedReq, err := p.Storage().CreateAuthRequest(ctx, authReq, "")
	if err != nil {
		slog.Error("OIDC custom-scheme: failed to create auth request", "error", err)
		writeGinError(c, http.StatusInternalServerError, "Failed to create auth request")
		return
	}

	slog.Info("OIDC custom-scheme: created auth request, redirecting to login",
		"id", storedReq.GetID(),
		"client_id", authReq.ClientID,
		"redirect_uri", redirectURI,
	)

	// Redirect to login page.
	loginURL := client.LoginURL(storedReq.GetID())
	http.Redirect(c.Writer, c.Request, loginURL, http.StatusFound)
}

// handleOIDCAuthorizeCallback handles the authorize callback after user login
func (s *GinServer) handleOIDCAuthorizeCallback(c *gin.Context, p *oidc.Provider, authRequestID string) {
	authReq, err := p.Storage().AuthRequestByID(c.Request.Context(), authRequestID)
	if err != nil {
		slog.Error("OIDC callback: auth request not found", "id", authRequestID, "error", err)
		writeGinError(c, http.StatusBadRequest, "Auth request not found or expired")
		return
	}

	if !authReq.Done() {
		slog.Error("OIDC callback: auth request not completed", "id", authRequestID)
		writeGinError(c, http.StatusBadRequest, "Auth request not completed - please login first")
		return
	}

	// Inject redirect_uri from the auth request for validation
	ctx := context.WithValue(c.Request.Context(), requestedRedirectURIContextKey, authReq.GetRedirectURI())
	c.Request = c.Request.WithContext(ctx)

	slog.Info("OIDC callback: completing auth request",
		"id", authRequestID,
		"client_id", authReq.GetClientID(),
		"redirect_uri", authReq.GetRedirectURI(),
	)

	// Use the library's AuthorizeCallback to complete the flow
	op.AuthorizeCallback(c.Writer, c.Request, p.Inner())
}

// handleOIDCAuthorizeFastPath handles authorize when user is already logged in
// portalSessionID is passed for logout propagation per RFC 20260122 §6.3
func (s *GinServer) handleOIDCAuthorizeFastPath(c *gin.Context, p *oidc.Provider, userID, portalSessionID string) {
	redirectURI := c.Request.FormValue("redirect_uri")

	// Inject redirect_uri for validation
	ctx := c.Request.Context()
	if redirectURI != "" {
		ctx = context.WithValue(ctx, requestedRedirectURIContextKey, redirectURI)
	}

	// RFC 20260122 §6.3: Inject portal session ID for logout propagation
	if portalSessionID != "" {
		ctx = oidc.WithPortalSessionID(ctx, portalSessionID)
	}

	// Parse the authorize request parameters
	authReq, err := op.ParseAuthorizeRequest(c.Request, p.Inner().Decoder())
	if err != nil {
		slog.Error("OIDC fast-path: failed to parse authorize request", "error", err)
		// Fall back to regular flow
		c.Request = c.Request.WithContext(ctx)
		p.Handler().ServeHTTP(c.Writer, c.Request)
		return
	}

	// Create auth request with userID (marks it as Done)
	storedReq, err := p.Storage().CreateAuthRequest(ctx, authReq, userID)
	if err != nil {
		slog.Error("OIDC fast-path: failed to create auth request", "error", err)
		var oe *zitadelOIDC.Error
		if errors.As(err, &oe) && oe.ErrorType == zitadelOIDC.AccessDenied {
			c.Status(http.StatusForbidden)
			return
		}
		c.Request = c.Request.WithContext(ctx)
		p.Handler().ServeHTTP(c.Writer, c.Request)
		return
	}

	slog.Info("OIDC fast-path: created auth request with user",
		"id", storedReq.GetID(),
		"user_id", userID,
		"client_id", storedReq.GetClientID(),
	)

	// Create new request with id parameter for AuthorizeCallback
	// We must set id in both URL and Form because:
	// - Clone() preserves the cached Form field
	// - FormValue() reads from Form, not URL
	newURL := *c.Request.URL
	q := newURL.Query()
	q.Set("id", storedReq.GetID())
	newURL.RawQuery = q.Encode()

	newReq := c.Request.Clone(ctx)
	newReq.URL = &newURL

	// Ensure Form is initialized and contains the id parameter
	if newReq.Form == nil {
		newReq.Form = make(map[string][]string)
	}
	newReq.Form.Set("id", storedReq.GetID())

	op.AuthorizeCallback(c.Writer, newReq, p.Inner())
}

func (s *GinServer) handleOIDCToken(c *gin.Context) {
	ctx := c.Request.Context()

	// Inject requested redirect_uri into context for dynamic origin validation.
	redirectURI := c.Request.FormValue("redirect_uri")
	if redirectURI != "" {
		ctx = context.WithValue(ctx, requestedRedirectURIContextKey, redirectURI)
	}

	// RFC 20260112: hybrid token endpoint authentication uses PKCE-only for loopback/custom-scheme
	// redirect URIs. We pass code + redirect_uri through context so storage can apply the rule.
	if strings.EqualFold(c.Request.FormValue("grant_type"), "authorization_code") {
		code := c.Request.FormValue("code")
		if strings.TrimSpace(code) != "" && strings.TrimSpace(redirectURI) != "" {
			ctx = oidc.WithTokenExchangeContext(ctx, code, redirectURI)
		}
	}
	c.Request = c.Request.WithContext(ctx)

	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCJwks(c *gin.Context) {
	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCUserinfo(c *gin.Context) {
	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCRevoke(c *gin.Context) {
	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCIntrospect(c *gin.Context) {
	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCLogout(c *gin.Context) {
	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

// handleOIDCResume handles POST /api/v1/oauth/resume
// It completes an OIDC auth request for the currently logged-in user.
func (s *GinServer) handleOIDCResume(c *gin.Context) {
	var req struct {
		AuthRequestID string `json:"auth_request_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AuthRequestID == "" {
		writeGinError(c, http.StatusBadRequest, "Invalid request: auth_request_id required")
		return
	}

	sess := s.getSessionFromContext(c)
	if sess == nil {
		writeGinError(c, http.StatusUnauthorized, "Not authenticated")
		return
	}

	p, err := s.getOIDCProvider()
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "OIDC provider unavailable")
		return
	}

	if err := p.CompleteAuthRequest(c.Request.Context(), req.AuthRequestID, sess.UserID, sess.ID); err != nil {
		// Log detailed error server-side, return generic message to client
		slog.Error("OIDC resume: failed to complete auth request", "id", req.AuthRequestID, "error", err)
		writeGinError(c, http.StatusBadRequest, "Failed to complete authentication request")
		return
	}

	// Return the URL for the frontend to redirect to.
	// Redirecting back to the authorize endpoint with the ID triggers the final step (code issuance).
	target := fmt.Sprintf("/oauth/authorize?id=%s", req.AuthRequestID)
	writeGinSuccess(c, gin.H{"redirect_url": target}, "Auth request completed")
}

// Helper to lazy-load/cache provider
func (s *GinServer) getOIDCProvider() (*oidc.Provider, error) {
	s.oidcProviderMu.Lock()
	defer s.oidcProviderMu.Unlock()

	if s.oidcProvider != nil {
		return s.oidcProvider, nil
	}

	p, err := s.initOIDCProvider()
	if err != nil {
		return nil, err
	}

	s.oidcProvider = p
	return p, nil
}

// proxyOIDCExchangeCode performs the OIDC token exchange for proxy clients per RFC 20260122 §5.6.
// This is a back-channel request from the proxy OIDC handler to the OIDC provider.
func (s *GinServer) proxyOIDCExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (*services.ExchangeResult, error) {
	p, err := s.getOIDCProvider()
	if err != nil {
		return nil, fmt.Errorf("OIDC provider unavailable: %w", err)
	}

	// Retrieve the auth request by code
	authReq, err := p.Storage().AuthRequestByCode(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("invalid authorization code: %w", err)
	}

	// Verify PKCE code_verifier
	if authReq.GetCodeChallenge() != nil {
		expectedChallenge := authReq.GetCodeChallenge().Challenge
		computedChallenge := computePKCEChallenge(codeVerifier)
		if expectedChallenge != computedChallenge {
			return nil, fmt.Errorf("PKCE verification failed")
		}
	}

	// Verify redirect_uri matches
	if authReq.GetRedirectURI() != redirectURI {
		return nil, fmt.Errorf("redirect_uri mismatch")
	}

	// Verify user exists
	userID := authReq.GetSubject()
	if _, err := p.Storage().GetUserByID(ctx, userID); err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}

	// Create access token (we don't need a full JWT for in-process use)
	// The proxy OIDC handler will create an app session directly
	accessToken := userID // For in-process flow, we just pass the user ID

	// Extract PortalSessionID for logout propagation (RFC 20260122 §6.3)
	var portalSessionID string
	if ar, ok := authReq.(*oidc.AuthRequest); ok {
		portalSessionID = ar.PortalSessionID
	}

	return &services.ExchangeResult{
		AccessToken:     accessToken,
		TokenType:       "Bearer",
		ExpiresIn:       3600,
		PortalSessionID: portalSessionID,
	}, nil
}

// proxyOIDCGetUserInfo retrieves user info for proxy OIDC flow per RFC 20260122 §5.6.
func (s *GinServer) proxyOIDCGetUserInfo(ctx context.Context, accessToken string) (*services.UserInfoResult, error) {
	p, err := s.getOIDCProvider()
	if err != nil {
		return nil, fmt.Errorf("OIDC provider unavailable: %w", err)
	}

	// For in-process flow, accessToken is the user ID
	userID := accessToken
	user, err := p.Storage().GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}

	return &services.UserInfoResult{
		Sub:      user.ID,
		Username: user.Username,
		Email:    user.Email,
		Role:     string(user.Role),
	}, nil
}

// computePKCEChallenge computes the S256 PKCE code challenge.
func computePKCEChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
