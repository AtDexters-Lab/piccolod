package server

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"piccolod/internal/autounlock"
)

// handleAutoUnlockGet: GET /api/v1/security/auto-unlock
//
// Returns the persisted state plus blob-presence for the UI's "outstanding
// cycle" indicator. No auth tier branching — everything posture-related was
// removed; the only flag is `enabled`.
func (s *GinServer) handleAutoUnlockGet(c *gin.Context) {
	state, err := autounlock.LoadState()
	if err != nil && !errors.Is(err, autounlock.ErrInvalidStateFile) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load state"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":              state.Enabled,
		"auto_reboot":          state.AutoReboot,
		"last_deposit_at":      state.LastDepositAt,
		"last_pickup_at":       state.LastPickupAt,
		"last_failure_at":      state.LastFailureAt,
		"last_failure_reason":  state.LastFailureReason,
		"has_outstanding_blob": autounlock.BlobExists(),
	})
}

// handleAutoUnlockPut: PUT /api/v1/security/auto-unlock
//
// Partial-update body: `{enabled?, auto_reboot?: {enabled?, window_start_hour?, window_end_hour?}}`.
// Disable transition triggers cleanup (revoke at namek + delete on-disk blob).
// Window validation: 0..23 each, start != end.
func (s *GinServer) handleAutoUnlockPut(c *gin.Context) {
	if s.autounlockOrch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auto-unlock unavailable"})
		return
	}
	var body autounlock.UpdateInput
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := s.autounlockOrch.Update(c.Request.Context(), body); err != nil {
		if errors.Is(err, autounlock.ErrInvalidWindow) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("ERROR: auto-unlock update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
		return
	}
	// Window or auto_reboot.enabled change → block today's scheduler fire.
	// Closes the compromised-admin DoS where a window edit could otherwise
	// trigger an immediate reboot. In-memory only by design.
	if s.autounlockScheduler != nil && body.AutoReboot != nil {
		s.autounlockScheduler.MarkWindowEdit()
	}
	s.handleAutoUnlockGet(c)
}

// handleAutoUnlockDelete: DELETE /api/v1/security/auto-unlock
//
// Equivalent to PUT `{enabled: false}`. Returns the updated state.
func (s *GinServer) handleAutoUnlockDelete(c *gin.Context) {
	if s.autounlockOrch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auto-unlock unavailable"})
		return
	}
	disabled := false
	if err := s.autounlockOrch.Update(c.Request.Context(), autounlock.UpdateInput{Enabled: &disabled}); err != nil {
		log.Printf("ERROR: auto-unlock disable: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "disable failed"})
		return
	}
	s.handleAutoUnlockGet(c)
}

// handleAutoUnlockTest: POST /api/v1/security/auto-unlock/test
//
// Runs a deposit + pickup + revoke round-trip against namek without touching
// the on-disk blob or the SDEK. Validates connectivity for the auto-unlock
// service end-to-end. 412 when preconditions are not met (disabled, manager
// locked, identity not ready).
func (s *GinServer) handleAutoUnlockTest(c *gin.Context) {
	if s.autounlockOrch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auto-unlock unavailable"})
		return
	}
	res, err := s.autounlockOrch.RunTest(c.Request.Context())
	if err != nil {
		if errors.Is(err, autounlock.ErrTestPreconditions) {
			c.JSON(http.StatusPreconditionFailed, gin.H{"error": err.Error()})
			return
		}
		log.Printf("ERROR: auto-unlock test: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "test failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":    res.Success,
		"error_kind": res.ErrorKind,
		"latency_ms": res.LatencyMs,
	})
}
