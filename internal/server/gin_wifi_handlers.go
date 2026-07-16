package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// WiFi management API handlers. All endpoints are admin-only and LAN-only.
// During onboarding (no admin account), WiFi is configured exclusively via
// the captive portal — these REST endpoints are not accessible pre-setup.

func (s *GinServer) handleNetworkStatus(c *gin.Context) {
	if s.networkManager == nil {
		c.JSON(http.StatusOK, gin.H{
			"active_uplink":     "none",
			"connectivity":      "unknown",
			"interfaces":        []any{},
			"ap_active":         false,
			"wifi_available":    false,
			"has_saved_network": false,
		})
		return
	}
	status := s.networkManager.Status()
	c.JSON(http.StatusOK, status)
}

func (s *GinServer) handleWifiScan(c *gin.Context) {
	if s.networkManager == nil {
		writeGinError(c, http.StatusServiceUnavailable, "WiFi not available")
		return
	}
	results, err := s.networkManager.ScanNetworks(false)
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "scan failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"networks": results})
}

func (s *GinServer) handleWifiConnect(c *gin.Context) {
	if s.networkManager == nil {
		writeGinError(c, http.StatusServiceUnavailable, "WiFi not available")
		return
	}

	var req struct {
		SSID       string `json:"ssid" binding:"required"`
		Passphrase string `json:"passphrase" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := s.opContext(c, 35*time.Second)
	defer cancel()

	err := s.networkManager.Connect(ctx, req.SSID, req.Passphrase)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
	})
}

func (s *GinServer) handleWifiDisconnect(c *gin.Context) {
	if s.networkManager == nil {
		writeGinError(c, http.StatusServiceUnavailable, "WiFi not available")
		return
	}
	if err := s.networkManager.ForgetNetwork(); err != nil {
		writeGinError(c, http.StatusInternalServerError, "disconnect failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
	})
}

func (s *GinServer) handleWifiAPStatus(c *gin.Context) {
	if s.networkManager == nil {
		c.JSON(http.StatusOK, gin.H{"active": false, "suppressed": false})
		return
	}
	status := s.networkManager.APStatus()
	c.JSON(http.StatusOK, status)
}

func (s *GinServer) handleWifiAPSuppress(c *gin.Context) {
	if s.networkManager == nil {
		writeGinError(c, http.StatusServiceUnavailable, "WiFi not available")
		return
	}
	var req struct {
		Suppress bool `json:"suppress"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeGinError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.networkManager.SetAPSuppressed(req.Suppress); err != nil {
		writeGinError(c, http.StatusInternalServerError, "failed to update AP suppression: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "suppressed": req.Suppress})
}
