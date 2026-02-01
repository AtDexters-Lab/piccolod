package server

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"piccolod/internal/remote"
)

// remoteConfigureRequest is for user-managed mode (HTTP-01 only)
type remoteConfigureRequest struct {
	Endpoint       string `json:"endpoint"`
	DeviceSecret   string `json:"device_secret"`
	PortalHostname string `json:"portal_hostname"`
}

// remoteManagedConfigureRequest is for managed mode (DNS-01 via orchestrator)
type remoteManagedConfigureRequest struct {
	OrchestratorEndpoint string `json:"orchestrator_endpoint"`
	DeviceToken          string `json:"device_token"`
	PortalHostname       string `json:"portal_hostname"`
}

// handleRemoteConfigure handles POST /api/v1/remote/configure (user-managed mode, HTTP-01 only)
func (s *GinServer) handleRemoteConfigure(c *gin.Context) {
	var req remoteConfigureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "invalid json body")
		return
	}
	configureReq := remote.ConfigureRequest{
		Endpoint:       req.Endpoint,
		DeviceSecret:   req.DeviceSecret,
		PortalHostname: req.PortalHostname,
	}
	if s.dispatcher != nil {
		resp, err := s.dispatcher.Dispatch(c.Request.Context(), remote.ConfigureCommand{Req: configureReq})
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		if _, ok := resp.(remote.ConfigureResponse); !ok {
			writeGinError(c, http.StatusInternalServerError, "unexpected response from remote dispatcher")
			return
		}
	} else {
		if err := s.remoteManager.Configure(configureReq); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.refreshRemoteRuntime()
	// User-managed mode uses HTTP-01 (wildcard unsupported), proactively issue per-listener certs
	if strings.TrimSpace(req.PortalHostname) != "" && s.remoteManager != nil {
		base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.PortalHostname)), ".")
		hosts := map[string]struct{}{}
		for _, ep := range s.serviceManager.GetAll() {
			// Only queue certs for HTTP/WS listeners that have host-based routing
			// DerivedHostLabel is empty for raw/tls listeners (per RFC 20260114)
			if ep.DerivedHostLabel == "" {
				continue
			}
			host := ep.DerivedHostLabel + "." + base
			hosts[host] = struct{}{}
		}
		for h := range hosts {
			s.remoteManager.QueueHostnameCertificate(h)
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "remote configured"})
}

// handleRemoteManagedConfigure handles POST /api/v1/remote/managed/configure (managed mode, DNS-01 via orchestrator)
func (s *GinServer) handleRemoteManagedConfigure(c *gin.Context) {
	var req remoteManagedConfigureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "invalid json body")
		return
	}
	configureReq := remote.ManagedConfigureRequest{
		OrchestratorEndpoint: req.OrchestratorEndpoint,
		DeviceToken:          req.DeviceToken,
		PortalHostname:       req.PortalHostname,
	}
	if s.dispatcher != nil {
		resp, err := s.dispatcher.Dispatch(c.Request.Context(), remote.ManagedConfigureCommand{Req: configureReq})
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		if _, ok := resp.(remote.ManagedConfigureResponse); !ok {
			writeGinError(c, http.StatusInternalServerError, "unexpected response from remote dispatcher")
			return
		}
	} else {
		if err := s.remoteManager.ConfigureManaged(configureReq); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.refreshRemoteRuntime()
	// Managed mode uses DNS-01 with wildcard cert, no need to queue per-listener certs
	c.JSON(http.StatusOK, gin.H{"message": "remote configured (managed)"})
}

func (s *GinServer) resolvePortalPort() int {
	if s != nil && s.securePort > 0 {
		return s.securePort
	}
	if p := os.Getenv("PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			return v
		}
	}
	return 80
}

// handleRemoteDisable handles POST /api/v1/remote/disable
func (s *GinServer) handleRemoteDisable(c *gin.Context) {
	if s.dispatcher != nil {
		if _, err := s.dispatcher.Dispatch(c.Request.Context(), remote.DisableCommand{}); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := s.remoteManager.Disable(); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.refreshRemoteRuntime()
	c.JSON(http.StatusOK, gin.H{"message": "remote disabled"})
}

// handleRemoteRotate handles POST /api/v1/remote/rotate
func (s *GinServer) handleRemoteRotate(c *gin.Context) {
	var secret string
	if s.dispatcher != nil {
		resp, err := s.dispatcher.Dispatch(c.Request.Context(), remote.RotateSecretCommand{})
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		rotateResp, ok := resp.(remote.RotateSecretResponse)
		if !ok {
			writeGinError(c, http.StatusInternalServerError, "unexpected response from remote dispatcher")
			return
		}
		secret = rotateResp.Secret
	} else {
		resp, err := s.remoteManager.Rotate()
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		secret = resp
	}
	c.JSON(http.StatusOK, gin.H{"device_secret": secret})
}

// handleRemotePreflight runs a preflight validation.
func (s *GinServer) handleRemotePreflight(c *gin.Context) {
	var result remote.PreflightResult

	// Check if body is provided for stateless preflight
	var req remoteConfigureRequest
	var candidate *remote.Config

	// BindJSON returns error if body is empty or invalid, but we want to support empty body (use active config)
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err == nil {
			// Only treat as candidate if it has content.
			// Clients sending {} should be treated as "check active config"
			if req.Endpoint != "" || req.PortalHostname != "" {
				// Map request to config candidate (user-managed mode, HTTP-01)
				candidate = &remote.Config{
					Endpoint:       req.Endpoint,
					DeviceSecret:   req.DeviceSecret,
					Solver:         "http-01", // User-managed mode is always HTTP-01
					PortalHostname: req.PortalHostname,
					Managed:        false,
				}
			}
		}
	}

	if s.dispatcher != nil {
		resp, err := s.dispatcher.Dispatch(c.Request.Context(), remote.RunPreflightCommand{Candidate: candidate})
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		preResp, ok := resp.(remote.RunPreflightResponse)
		if !ok {
			writeGinError(c, http.StatusInternalServerError, "unexpected response from remote dispatcher")
			return
		}
		result = preResp.Result
	} else {
		resp, err := s.remoteManager.RunPreflight(candidate)
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		result = resp
	}
	c.JSON(http.StatusOK, gin.H{
		"checks": result.Checks,
		"ran_at": result.RanAt.Format(time.RFC3339),
	})
}

// handleRemoteAliasesList returns the current alias inventory.
func (s *GinServer) handleRemoteAliasesList(c *gin.Context) {
	aliases := s.remoteManager.ListAliases()
	c.JSON(http.StatusOK, gin.H{"aliases": aliases})
}

type remoteAliasRequest struct {
	Listener string `json:"listener"`
	Hostname string `json:"hostname"`
}

// handleRemoteAliasesCreate appends a new alias.
func (s *GinServer) handleRemoteAliasesCreate(c *gin.Context) {
	var req remoteAliasRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "invalid json body")
		return
	}
	var alias remote.Alias
	if s.dispatcher != nil {
		resp, err := s.dispatcher.Dispatch(c.Request.Context(), remote.AddAliasCommand{Listener: req.Listener, Hostname: req.Hostname})
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		aliasResp, ok := resp.(remote.AddAliasResponse)
		if !ok {
			writeGinError(c, http.StatusInternalServerError, "unexpected response from remote dispatcher")
			return
		}
		alias = aliasResp.Alias
	} else {
		resp, err := s.remoteManager.AddAlias(req.Listener, req.Hostname)
		if err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
		alias = resp
	}
	c.JSON(http.StatusOK, alias)
}

// handleRemoteAliasesDelete removes an alias by ID.
func (s *GinServer) handleRemoteAliasesDelete(c *gin.Context) {
	id := c.Param("id")
	if s.dispatcher != nil {
		if _, err := s.dispatcher.Dispatch(c.Request.Context(), remote.RemoveAliasCommand{ID: id}); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
	} else {
		if err := s.remoteManager.RemoveAlias(id); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "alias removed"})
}

// handleRemoteCertificatesList returns certificate metadata.
func (s *GinServer) handleRemoteCertificatesList(c *gin.Context) {
	certs := s.remoteManager.ListCertificates()
	c.JSON(http.StatusOK, gin.H{"certificates": certs})
}

// handleRemoteCertificateRenew triggers a manual renewal.
func (s *GinServer) handleRemoteCertificateRenew(c *gin.Context) {
	id := c.Param("id")
	if s.dispatcher != nil {
		if _, err := s.dispatcher.Dispatch(c.Request.Context(), remote.RenewCertCommand{ID: id}); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
	} else {
		if err := s.remoteManager.RenewCertificate(id); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusNotFound, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "renewal queued"})
}

// handleRemoteEvents returns the activity log.
func (s *GinServer) handleRemoteEvents(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"events": s.remoteManager.ListEvents()})
}

type guideVerifyRequest struct {
	Endpoint       string `json:"endpoint"`
	PortalHostname string `json:"portal_hostname"`
	JWTSecret      string `json:"jwt_secret"`
}

// handleRemoteGuideVerify records helper verification details.
func (s *GinServer) handleRemoteGuideVerify(c *gin.Context) {
	var req guideVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "invalid json body")
		return
	}
	verification := remote.GuideVerification{
		Endpoint:       req.Endpoint,
		PortalHostname: req.PortalHostname,
		JWTSecret:      req.JWTSecret,
	}
	if s.dispatcher != nil {
		if _, err := s.dispatcher.Dispatch(c.Request.Context(), remote.GuideVerifyCommand{Verification: verification}); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if err := s.remoteManager.MarkGuideVerified(verification); err != nil {
			if errors.Is(err, remote.ErrLocked) {
				writeGinError(c, http.StatusLocked, "storage locked; unlock Piccolo to continue")
				return
			}
			writeGinError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	info := s.remoteManager.GuideInfo()
	c.JSON(http.StatusOK, gin.H{
		"verified_at": info.VerifiedAt,
		"message":     "nexus helper verified",
	})
}

// handleRemoteGuideInfo returns the static guide metadata.
func (s *GinServer) handleRemoteGuideInfo(c *gin.Context) {
	info := s.remoteManager.GuideInfo()
	c.JSON(http.StatusOK, info)
}

