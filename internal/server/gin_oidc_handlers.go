package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	zitadelOIDC "github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"piccolod/internal/oidc"
)

// getOIDCClientManager helper to get client manager from persistence
func (s *GinServer) getOIDCClientManager() *oidc.ClientManager {
	if s.persistence == nil || s.persistence.Control() == nil {
		return nil
	}
	repo := s.persistence.Control().OIDCClients()
	return oidc.NewClientManager(repo)
}

// stableIssuer is the constant OIDC issuer URL.
// All OIDC operations use HTTPS for the back-channel; HTTP is not supported.
const stableIssuer = "https://piccolo.local"

type requestedRedirectURIKey struct{}

var requestedRedirectURIContextKey = requestedRedirectURIKey{}

// initOIDCProvider initializes the OIDC provider if enabled/configured
func (s *GinServer) initOIDCProvider() (*oidc.Provider, error) {
	if s.persistence == nil {
		return nil, errors.New("persistence not available")
	}
	control := s.persistence.Control()

	// Config - issuer is always https://piccolo.local
	cfg := oidc.ProviderConfig{
		Issuer:          stableIssuer,
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

// resolveAppRedirectURI resolves the valid redirect URIs for an app based on its listeners.
func (s *GinServer) resolveAppRedirectURI(ctx context.Context, appID string) ([]string, error) {
	if s.serviceManager == nil {
		return nil, nil
	}

	endpoints, err := s.serviceManager.GetByApp(appID)
	if err != nil {
		return nil, nil
	}

	// Collect additional redirect URIs declared via services[].oidc_client.redirect_uris.
	extraRedirectURIs := make([]string, 0)
	if s.appManager != nil {
		if inst, err := s.appManager.Get(ctx, appID); err == nil && inst != nil && inst.Definition != nil {
			seen := make(map[string]struct{})
			for _, svc := range inst.Definition.Services {
				if svc.OIDCClient == nil {
					continue
				}
				for _, u := range svc.OIDCClient.RedirectURIs {
					u = strings.TrimSpace(u)
					if u == "" {
						continue
					}
					if _, ok := seen[u]; ok {
						continue
					}
					seen[u] = struct{}{}
					extraRedirectURIs = append(extraRedirectURIs, u)
				}
			}
		}
	}

	// Get the current local hostname (handles mDNS conflicts like "piccolo-abc123.local")
	localHostname := "piccolo.local"
	if s.mdnsManager != nil {
		localHostname = s.mdnsManager.Hostname()
	}

	// 1. Dynamic Origin Matching (RFC compliant)
	// If we have a requested redirect_uri, check if its origin matches a listener.
	requested, _ := ctx.Value(requestedRedirectURIContextKey).(string)
	if requested != "" {
		// First, allow explicitly-declared redirect URIs (native/loopback/custom scheme).
		for _, allowed := range extraRedirectURIs {
			if requested == allowed {
				return []string{requested}, nil
			}
		}

		u, err := url.Parse(requested)
		if err == nil {
			// Extract port from host (u.Host includes port if present)
			_, portStr, _ := net.SplitHostPort(u.Host)
			requestedPort, _ := strconv.Atoi(portStr)

			// Check against known listeners
			for _, ep := range endpoints {
				// Check Remote Host
				if s.remoteManager != nil {
					st := s.remoteManager.Status()
					if st.Enabled && st.TLD != "" {
						remoteHost := s.remoteServiceHostname(&st, ep)
						if remoteHost != "" && u.Scheme == "https" {
							// Compare hostname, handling explicit :443 port
							reqHost := u.Host
							if h, p, err := net.SplitHostPort(u.Host); err == nil && p == "443" {
								reqHost = h // Strip explicit :443
							}
							if strings.EqualFold(reqHost, remoteHost) {
								return []string{requested}, nil
							}
						}
					}
				}

				// Check Alias Domains (remote config)
				if s.remoteManager != nil && u.Scheme == "https" {
					reqHost := u.Host
					if h, p, err := net.SplitHostPort(u.Host); err == nil && p == "443" {
						reqHost = h
					}
					for _, alias := range s.remoteManager.ListAliases() {
						if strings.TrimSpace(alias.Hostname) == "" {
							continue
						}
						if alias.Listener != ep.Name {
							continue
						}
						if strings.EqualFold(reqHost, alias.Hostname) {
							return []string{requested}, nil
						}
					}
				}

				// Check Local Host - support both mDNS hostname and IP address access
				// Local origin can be:
				// - http://piccolo.local:<PublicPort> (or piccolo-abc123.local if conflict)
				// - http://<lan-ip>:<PublicPort> (for clients without mDNS)
				localHost := fmt.Sprintf("%s:%d", localHostname, ep.PublicPort)
				if strings.EqualFold(u.Host, localHost) {
					// Allow http for local LAN access as per RFC
					if u.Scheme == "http" || u.Scheme == "https" {
						return []string{requested}, nil
					}
				}

				// Also accept IP address access if it's a local machine IP with matching port
				// This supports clients that can't resolve mDNS names but limits to piccolo's own IPs
				// to prevent redirects to attacker-controlled servers on LAN
				if requestedPort == ep.PublicPort && u.Scheme == "http" {
					host, _, _ := net.SplitHostPort(u.Host)
					if ip := net.ParseIP(host); ip != nil && isLocalMachineIP(ip) {
						return []string{requested}, nil
					}
				}
			}
		}
	}

	// 2. Fallback: Return best-guess list for clients that don't provide context or
	// for introspection/display purposes.
	var uris []string

	// Remote info
	if s.remoteManager != nil {
		st := s.remoteManager.Status()
		if st.Enabled && st.TLD != "" {
			for _, ep := range endpoints {
				host := s.remoteServiceHostname(&st, ep)
				if host != "" {
					uris = append(uris, "https://"+host+"/callback")
					uris = append(uris, "https://"+host+"/oauth/callback")
					uris = append(uris, "https://"+host+"/")
				}
			}
		}
	}

	// Local info (using dynamic hostname which handles mDNS conflicts)
	uris = append(uris, "http://"+localHostname+"/callback")
	uris = append(uris, "https://"+localHostname+"/callback")

	// Let's try to return roots and common paths.
	for _, ep := range endpoints {
		// Local
		uris = append(uris, fmt.Sprintf("http://%s:%d/callback", localHostname, ep.PublicPort))
		uris = append(uris, fmt.Sprintf("http://%s:%d/auth/callback", localHostname, ep.PublicPort))
		uris = append(uris, fmt.Sprintf("http://%s:%d/oauth/callback", localHostname, ep.PublicPort))
		uris = append(uris, fmt.Sprintf("http://%s:%d/login/callback", localHostname, ep.PublicPort))
	}

	uris = append(uris, extraRedirectURIs...)

	return uris, nil
}

// OIDC Handlers ---------------------------------------------------------------

func (s *GinServer) handleOIDCDiscovery(c *gin.Context) {
	// Custom discovery handler that is "Split-Horizon" aware
	cfg := oidc.DiscoveryConfig{
		StableIssuer: stableIssuer,
		IsRemoteActive: func() bool {
			if s.remoteManager == nil {
				return false
			}
			st := s.remoteManager.Status()
			return st.Enabled && st.PortalHostname != ""
		},
		GetPortalHostname: func() string {
			if s.remoteManager == nil {
				return ""
			}
			return s.remoteManager.Status().PortalHostname
		},
		GetLocalHostname: func() string {
			// Use mDNS hostname if available (handles conflicts like "piccolo-abc123.local")
			if s.mdnsManager != nil {
				return s.mdnsManager.Hostname()
			}
			// mDNS disabled: fall back to local IP address
			return getPreferredOutboundIP()
		},
		Logger: slog.Default(),
	}
	h := oidc.NewDiscoveryHandler(cfg)
	h.ServeHTTP(c.Writer, c.Request)
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
				"client_id", clientID,
			)

			// User is already logged in - create auth request and immediately complete it
			// This avoids redirecting to login page
			s.handleOIDCAuthorizeFastPath(c, p, sess.UserID)
			return
		}
	}

	// Case 3: Regular authorize request (user not logged in)
	// Inject redirect_uri for validation
	if redirectURI != "" {
		ctx := context.WithValue(c.Request.Context(), requestedRedirectURIContextKey, redirectURI)
		c.Request = c.Request.WithContext(ctx)
	}

	p.Handler().ServeHTTP(c.Writer, c.Request)
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
func (s *GinServer) handleOIDCAuthorizeFastPath(c *gin.Context, p *oidc.Provider, userID string) {
	redirectURI := c.Request.FormValue("redirect_uri")

	// Inject redirect_uri for validation
	ctx := c.Request.Context()
	if redirectURI != "" {
		ctx = context.WithValue(ctx, requestedRedirectURIContextKey, redirectURI)
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

	if err := p.CompleteAuthRequest(c.Request.Context(), req.AuthRequestID, sess.UserID); err != nil {
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
