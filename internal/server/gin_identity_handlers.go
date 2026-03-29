package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"piccolod/internal/identity"
)

func (s *GinServer) registerIdentityRoutes(rg *gin.RouterGroup) {
	if s.identityService == nil {
		return
	}
	ig := rg.Group("/identity")
	ig.GET("", s.handleIdentityStatus)
	ig.POST("/enroll", s.handleIdentityEnroll)
	ig.POST("/enable", s.handleIdentityEnable)
	ig.POST("/disable", s.handleIdentityDisable)
	ig.POST("/hostname", s.handleIdentitySetHostname)
	ig.POST("/namek-url", s.handleIdentitySetNamekURL)
}

func (s *GinServer) handleIdentityStatus(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	c.JSON(http.StatusOK, buildIdentityPayload(svc))
}

// buildIdentityPayload constructs the identity status map used by both the
// REST handler and WebSocket snapshot. Kept in one place to avoid drift.
func buildIdentityPayload(svc *identity.Service) map[string]any {
	cfg := svc.DeviceConfig()
	status := svc.Status()
	return map[string]any{
		"enabled":         cfg.Enabled,
		"available":       svc.IsAvailable(),
		"enrolled":        svc.IsEnrolled(),
		"suspended":       svc.IsSuspended(),
		"state":           status.State,
		"device_id":       cfg.DeviceID,
		"account_id":      cfg.AccountID,
		"hostname":        cfg.Hostname,
		"base_domain":     cfg.BaseDomain,
		"custom_hostname": cfg.CustomHostname,
		"identity_class":  cfg.IdentityClass,
		"recovery_status": status.RecoveryStatus,
		"nexus_endpoints": cfg.NexusEndpoints,
		"namek_url":       cfg.NamekURL,
	}
}

func (s *GinServer) handleIdentityEnroll(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	if !svc.IsAvailable() {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "TPM not available"})
		return
	}
	result, err := svc.Enroll(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *GinServer) handleIdentityEnable(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	if err := svc.SetEnabled(c.Request.Context(), true); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true})
}

func (s *GinServer) handleIdentityDisable(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	if err := svc.SetEnabled(c.Request.Context(), false); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": false})
}

type setHostnameRequest struct {
	Hostname string `json:"hostname"`
}

// validateAndSetHostname validates the hostname and calls SetCustomHostname.
// Returns the cleaned hostname and any error suitable for the client.
func (s *GinServer) validateAndSetHostname(c *gin.Context) (string, error) {
	svc := s.identityService
	if svc == nil {
		return "", fmt.Errorf("identity service unavailable")
	}
	var req setHostnameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return "", err
	}
	req.Hostname = strings.TrimSpace(strings.ToLower(req.Hostname))
	if req.Hostname != "" && !isValidDNSLabel(req.Hostname) {
		return "", fmt.Errorf("hostname must be a valid DNS label (lowercase alphanumeric and hyphens, 1-63 chars, no leading/trailing hyphens)")
	}
	if err := svc.SetCustomHostname(c.Request.Context(), req.Hostname); err != nil {
		return req.Hostname, err
	}
	return req.Hostname, nil
}

// hostnameClientErrorKeywords are substrings that indicate a client-actionable
// hostname error (400) rather than an internal server error (500).
var hostnameClientErrorKeywords = []string{"validation", "invalid", "DNS label", "unavailable"}

// respondHostnameError classifies a hostname error as 400 (client) or 500 (server)
// and writes the appropriate JSON response.
func respondHostnameError(c *gin.Context, err error) {
	errMsg := err.Error()
	for _, kw := range hostnameClientErrorKeywords {
		if strings.Contains(errMsg, kw) {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
			return
		}
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
}

func (s *GinServer) handleIdentitySetHostname(c *gin.Context) {
	if s.identityService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	hostname, err := s.validateAndSetHostname(c)
	if err != nil {
		respondHostnameError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"hostname": hostname})
}

type setNamekURLRequest struct {
	URL string `json:"url"`
}

func (s *GinServer) handleIdentitySetNamekURL(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	var req setNamekURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url must be a valid http or https URL"})
		return
	}
	if err := svc.SetNamekURL(c.Request.Context(), req.URL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": req.URL, "message": "namek URL changed, re-enrollment required"})
}

// handleSetupHostname: POST /api/v1/identity/setup-hostname
// Accessible from LAN or same public IP, pre-setup only.
// Sets the device's custom hostname on namek. Returns the FQDN and a setup nonce for the redirect.
func (s *GinServer) handleSetupHostname(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	if !svc.IsEnrolled() {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "device not enrolled"})
		return
	}

	hostname, err := s.validateAndSetHostname(c)
	if err != nil {
		respondHostnameError(c, err)
		return
	}

	// Generate a setup nonce for the remote redirect (CGNAT defense).
	nonce := s.sessions.CreateSetupNonce()

	cfg := svc.DeviceConfig()
	c.JSON(http.StatusOK, gin.H{
		"hostname":    hostname,
		"base_domain": cfg.BaseDomain,
		"fqdn":        cfg.CustomFQDN(),
		"setup_nonce": nonce,
	})
}
