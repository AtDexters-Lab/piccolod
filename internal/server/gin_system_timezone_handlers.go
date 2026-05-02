package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/sysconfig/timezone"
)

// handleSystemTimezoneGet: GET /api/v1/system/timezone
//
// Returns the current IANA zone (e.g. "America/New_York"). UI consumes this
// to render the Settings → System → Timezone screen with the current value
// pre-selected.
func (s *GinServer) handleSystemTimezoneGet(c *gin.Context) {
	if s.timezoneMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "timezone unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"timezone": s.timezoneMgr.Get(),
		"is_set":   s.timezoneMgr.IsSet(),
	})
}

// handleSystemTimezonePut: PUT /api/v1/system/timezone { timezone }
//
// Operator-initiated override. Validates the IANA name with
// time.LoadLocation, then shells out to `timedatectl set-timezone`. Returns
// the new value on success.
func (s *GinServer) handleSystemTimezonePut(c *gin.Context) {
	if s.timezoneMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "timezone unavailable"})
		return
	}
	var body struct {
		Timezone string `json:"timezone"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	zone := strings.TrimSpace(body.Timezone)
	if zone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "timezone required"})
		return
	}
	ctx, cancel := s.opContext(c, 10*time.Second)
	defer cancel()
	if err := s.timezoneMgr.Set(ctx, zone); err != nil {
		if errors.Is(err, timezone.ErrInvalidTimezone) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "set failed: " + err.Error()})
		return
	}
	s.publishAuditEvent(c, "system.timezone.changed", map[string]any{"to": zone})
	c.JSON(http.StatusOK, gin.H{"timezone": s.timezoneMgr.Get()})
}
