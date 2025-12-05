package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

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
	if err := s.updateManager.Rollback(context.Background(), strings.TrimSpace(req.SnapshotID)); err != nil {
		s.respondUpdateError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "rollback staged"})
}

func (s *GinServer) respondUpdateError(c *gin.Context, err error) {
	switch err {
	case update.ErrUnsupported:
		c.JSON(http.StatusNotImplemented, gin.H{"error": err.Error()})
	case update.ErrInProgress:
		c.Header("Retry-After", "30")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
	case update.ErrInvalidSnapshot:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case update.ErrTimeout:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// handleRemoteStatus returns basic remote access status (device-terminated TLS).
func (s *GinServer) handleRemoteStatus(c *gin.Context) {
	st := s.remoteManager.Status()
	c.JSON(http.StatusOK, st)
}

// handleStorageDisks lists physical disks (read-only); returns an empty list if unknown.
func (s *GinServer) handleStorageDisks(c *gin.Context) {
	// Placeholder: storage manager not yet implemented; return empty list.
	c.JSON(http.StatusOK, gin.H{"disks": []gin.H{}})
}

// handleOSUpdateReboot triggers a system reboot.
func (s *GinServer) handleOSUpdateReboot(c *gin.Context) {
	if s.updateManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update manager not available"})
		return
	}

	// This is a fire-and-forget operation from the client's perspective,
	// as the server will likely die immediately.
	if err := s.updateManager.Reboot(context.Background()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to trigger reboot: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "reboot initiated"})
}
