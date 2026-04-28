package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
	"github.com/gin-gonic/gin"

	hostnamepkg "piccolod/internal/hostname"
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
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	result, err := svc.Enroll(ctx)
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
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	if err := svc.SetEnabled(ctx, true); err != nil {
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
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	if err := svc.SetEnabled(ctx, false); err != nil {
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
// ctx is used for the SetCustomHostname call — callers should pass a decoupled
// context for remote-accessible handlers, or c.Request.Context() for LAN-only handlers.
func (s *GinServer) validateAndSetHostname(ctx context.Context, c *gin.Context) (string, error) {
	svc := s.identityService
	if svc == nil {
		return "", fmt.Errorf("identity service unavailable")
	}
	var req setHostnameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return "", err
	}
	req.Hostname = hostnamepkg.Normalize(req.Hostname)
	if req.Hostname != "" && !hostnamepkg.IsValidDNSLabel(req.Hostname) {
		return "", fmt.Errorf("hostname must be a valid DNS label (lowercase alphanumeric and hyphens, 1-63 chars, no leading/trailing hyphens)")
	}
	if err := svc.SetCustomHostname(ctx, req.Hostname); err != nil {
		return req.Hostname, err
	}
	return req.Hostname, nil
}

// hostnameClientErrorKeywords are substrings that indicate a client-actionable
// hostname error (400) rather than an internal server error (500).
var hostnameClientErrorKeywords = []string{"validation", "invalid", "DNS label", "unavailable"}

// respondHostnameError classifies a hostname error and writes the appropriate
// JSON response. Prefers structured APIError unwrapping (from namekclient) to
// get the upstream HTTP status; falls back to keyword matching for local errors.
func respondHostnameError(c *gin.Context, err error) {
	var apiErr *namekclient.APIError
	if errors.As(err, &apiErr) {
		status := http.StatusBadRequest
		if apiErr.StatusCode >= 500 {
			status = http.StatusInternalServerError
		}
		// apiErr.Message is the raw HTTP body (e.g., `{"error":"hostname already taken"}`).
		// Extract the inner "error" field if it's JSON; fall back to raw string.
		c.JSON(status, gin.H{"error": extractAPIErrorMessage(apiErr.Message)})
		return
	}
	// Fallback: keyword matching for local validation errors.
	errMsg := err.Error()
	for _, kw := range hostnameClientErrorKeywords {
		if strings.Contains(errMsg, kw) {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
			return
		}
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
}

// extractAPIErrorMessage extracts the "error" field from a JSON error body.
// Returns the raw string if it's not JSON or has no "error" field.
func extractAPIErrorMessage(raw string) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	return raw
}

func (s *GinServer) handleIdentitySetHostname(c *gin.Context) {
	if s.identityService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	hostname, err := s.validateAndSetHostname(ctx, c)
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
	ctx, cancel := s.opContext(c, 2*time.Minute)
	defer cancel()
	if err := svc.SetNamekURL(ctx, req.URL); err != nil {
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

	hostname, err := s.validateAndSetHostname(c.Request.Context(), c)
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

// handleRemoteReadiness: GET /api/v1/identity/remote-readiness
// Polled by the frontend after setup-hostname to wait for relay + cert
// before redirecting to the remote domain.
func (s *GinServer) handleRemoteReadiness(c *gin.Context) {
	rm := s.remoteManager
	if rm == nil {
		c.JSON(http.StatusOK, gin.H{"ready": false, "relay": false, "cert": false})
		return
	}

	// Check only the namek relay — self-hosted relays are irrelevant for
	// initial setup of a namek hostname.
	relayOK := rm.RelayConnectedByName("piccolo-namek")

	// Only the custom wildcard cert matters — the redirect URL uses the
	// custom FQDN, not the slug hostname.
	certOK := false
	for _, cert := range rm.ListCertificates() {
		if cert.ID == "namek-custom-wildcard" &&
			cert.Status == "ok" &&
			cert.IssuedAt != nil {
			certOK = true
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ready": relayOK && certOK,
		"relay": relayOK,
		"cert":  certOK,
	})
}
