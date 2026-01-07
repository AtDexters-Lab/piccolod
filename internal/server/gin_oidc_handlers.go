package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

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

	// 1. Dynamic Origin Matching (RFC compliant)
	// If we have a requested redirect_uri, check if its origin matches a listener.
	requested, _ := ctx.Value(requestedRedirectURIContextKey).(string)
	if requested != "" {
		u, err := url.Parse(requested)
		if err == nil {
			// Check against known listeners
			for _, ep := range endpoints {
				// Check Remote Host
				if s.remoteManager != nil {
					st := s.remoteManager.Status()
					if st.Enabled && st.TLD != "" {
						remoteHost := s.remoteServiceHostname(&st, ep)
						if remoteHost != "" && strings.EqualFold(u.Host, remoteHost) {
							if u.Scheme == "https" { // Remote is always https
								return []string{requested}, nil
							}
						}
					}
				}

				// Check Local Host
				// Local origin is http://piccolo.local:<PublicPort>
				// Note: u.Host includes port if present.
				localHost := fmt.Sprintf("piccolo.local:%d", ep.PublicPort)
				if strings.EqualFold(u.Host, localHost) {
					// Allow http for local LAN access as per RFC
					if u.Scheme == "http" || u.Scheme == "https" {
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

	// Local info
	uris = append(uris, "http://piccolo.local/callback")
	uris = append(uris, "https://piccolo.local/callback")

	// Let's try to return roots and common paths.
	for _, ep := range endpoints {
		// Local
		uris = append(uris, fmt.Sprintf("http://piccolo.local:%d/callback", ep.PublicPort))
		uris = append(uris, fmt.Sprintf("http://piccolo.local:%d/auth/callback", ep.PublicPort))
		uris = append(uris, fmt.Sprintf("http://piccolo.local:%d/oauth/callback", ep.PublicPort))
		uris = append(uris, fmt.Sprintf("http://piccolo.local:%d/login/callback", ep.PublicPort))
	}

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
		Logger: slog.Default(),
	}
	h := oidc.NewDiscoveryHandler(cfg)
	h.ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCAuthorize(c *gin.Context) {
	// Inject requested redirect_uri into context for dynamic origin validation
	redirectURI := c.Request.FormValue("redirect_uri")
	if redirectURI != "" {
		ctx := context.WithValue(c.Request.Context(), requestedRedirectURIContextKey, redirectURI)
		c.Request = c.Request.WithContext(ctx)
	}

	p, err := s.getOIDCProvider()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(c.Writer, c.Request)
}

func (s *GinServer) handleOIDCToken(c *gin.Context) {
	// Inject requested redirect_uri into context for dynamic origin validation
	// Note: For token exchange, redirect_uri is optional but if present must match.
	// We extract it from form values (POST body).
	redirectURI := c.Request.FormValue("redirect_uri")
	if redirectURI != "" {
		ctx := context.WithValue(c.Request.Context(), requestedRedirectURIContextKey, redirectURI)
		c.Request = c.Request.WithContext(ctx)
	}

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
		// This might fail if the request is expired or invalid
		writeGinError(c, http.StatusBadRequest, "Failed to complete auth request: "+err.Error())
		return
	}

	// Return the URL for the frontend to redirect to.
	// Redirecting back to the authorize endpoint with the ID triggers the final step (code issuance).
	target := fmt.Sprintf("/oauth/authorize?authRequestID=%s", req.AuthRequestID)
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
