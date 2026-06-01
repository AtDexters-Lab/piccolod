package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/api"
	"piccolod/internal/identity"
	"piccolod/internal/remote"
	"piccolod/internal/update"
)

// handleOSUpdateStatus returns a read-only snapshot of OS update info.
func (s *GinServer) handleOSUpdateStatus(c *gin.Context) {
	if s.updateManager == nil {
		c.JSON(http.StatusOK, gin.H{
			"current_version":   s.version,
			"available_version": s.version,
			"pending":           false,
			"requires_reboot":   false,
			"last_checked":      time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	st, err := s.updateManager.Status(c.Request.Context())
	if err != nil {
		s.respondUpdateError(c, err)
		return
	}
	// Ensure RFC3339 string for backward compat
	resp := gin.H{
		"current_version":   st.CurrentVersion,
		"available_version": st.AvailableVersion,
		"pending":           st.Pending,
		"requires_reboot":   st.RequiresReboot,
		"last_checked":      st.LastChecked.UTC().Format(time.RFC3339),
	}
	if len(st.Meta) > 0 {
		resp["meta"] = st.Meta
	}
	c.JSON(http.StatusOK, resp)
}

// handleOSUpdateApply triggers transactional-update dup.
func (s *GinServer) handleOSUpdateApply(c *gin.Context) {
	if s.updateManager == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "update manager unavailable"})
		return
	}
	if err := s.updateManager.Apply(context.Background()); err != nil {
		s.respondUpdateError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "update started"})
}

type rollbackRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// handleOSUpdateRollback sets the requested snapshot for next boot.
func (s *GinServer) handleOSUpdateRollback(c *gin.Context) {
	if s.updateManager == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "update manager unavailable"})
		return
	}
	var req rollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Accept empty body; reject malformed JSON
		if !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
			return
		}
	}
	if blocked, err := s.enabledMTLSGuardedApps(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to inspect installed apps before rollback"})
		return
	} else if len(blocked) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": "rollback blocked while enabled apps require connection_auth_mtls_v1; stop or uninstall those apps before rolling back",
			"apps":  blocked,
		})
		return
	}
	if err := s.updateManager.Rollback(context.Background(), strings.TrimSpace(req.SnapshotID)); err != nil {
		s.respondUpdateError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "rollback staged"})
}

func (s *GinServer) enabledMTLSGuardedApps(ctx context.Context) ([]string, error) {
	if s == nil || s.appManager == nil {
		return nil, nil
	}
	apps, err := s.appManager.List(ctx)
	if err != nil {
		return nil, err
	}
	blocked := make([]string, 0)
	for _, inst := range apps {
		if inst == nil || !inst.Enabled || !definitionUsesMTLSGuard(inst.Definition) {
			continue
		}
		blocked = append(blocked, inst.InstanceID)
	}
	sort.Strings(blocked)
	return blocked, nil
}

func definitionUsesMTLSGuard(def *api.AppDefinition) bool {
	if def == nil {
		return false
	}
	for _, listener := range def.Listeners {
		if listener.ConnectionAuth.RequiresMTLS() {
			return true
		}
	}
	return false
}

func (s *GinServer) respondUpdateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, update.ErrUnsupported):
		c.JSON(http.StatusNotImplemented, gin.H{"error": err.Error()})
	case errors.Is(err, update.ErrInProgress):
		c.Header("Retry-After", "30")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
	case errors.Is(err, update.ErrInvalidSnapshot):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, update.ErrTimeout):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
	case errors.Is(err, update.ErrInsufficientDisk):
		c.JSON(http.StatusInsufficientStorage, gin.H{"error": "Not enough free disk space to install the update. Free up space and try again."})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// handleRemoteStatus returns basic remote access status (device-terminated TLS).
// Merges self-hosted remote status with identity (namek) status at the handler level.
func (s *GinServer) handleRemoteStatus(c *gin.Context) {
	st := s.remoteManager.Status()
	type mergedStatus struct {
		remote.Status
		Namek           *identity.PublicIdentityStatus `json:"namek,omitempty"`
		AnyRemoteActive bool                           `json:"any_remote_active"`
	}
	resp := mergedStatus{Status: st, AnyRemoteActive: st.Enabled}
	if s.identityService != nil {
		ids := s.identityService.Status()
		resp.Namek = ids.Public()
		if ids.State == "active" {
			resp.AnyRemoteActive = true
		}
	}
	c.JSON(http.StatusOK, resp)
}

// handleOSUpdateReboot triggers a system reboot.
// Pass ?force=true to bypass snapshot validation (emergency escape hatch).
func (s *GinServer) handleOSUpdateReboot(c *gin.Context) {
	if s.updateManager == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "update manager unavailable"})
		return
	}

	force := c.Query("force") == "true"

	if force {
		s.publishAuditEvent(c, "system.force_reboot", nil)
		if err := s.updateManager.ForceReboot(context.Background()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to trigger reboot: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "force reboot initiated"})
		return
	}

	if err := s.updateManager.Reboot(context.Background()); err != nil {
		if errors.Is(err, update.ErrSnapshotValidationFailed) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to trigger reboot: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "reboot initiated"})
}
