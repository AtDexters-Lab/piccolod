package server

import (
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

func (s *GinServer) handleIdentitySetHostname(c *gin.Context) {
	svc := s.identityService
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	var req setHostnameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Hostname = strings.TrimSpace(strings.ToLower(req.Hostname))
	// Empty hostname clears the custom hostname (reverts to default enrolled hostname).
	if req.Hostname != "" && !isValidDNSLabel(req.Hostname) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hostname must be a valid DNS label (lowercase alphanumeric and hyphens, 1-63 chars, no leading/trailing hyphens)"})
		return
	}
	if err := svc.SetCustomHostname(c.Request.Context(), req.Hostname); err != nil {
		// Treat namekclient validation errors (e.g., reserved names) as client errors.
		if strings.Contains(err.Error(), "validation") || strings.Contains(err.Error(), "invalid") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hostname": req.Hostname})
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
