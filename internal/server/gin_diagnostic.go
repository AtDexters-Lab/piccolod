package server

import (
	"context"
	"log"
	"net/http"
	"os/exec"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/health"
	"piccolod/internal/logutil"
)

// handleDiagnosticLog: GET /api/v1/system/diagnostic-log
// Serves redacted piccolod.service journal entries since boot as a downloadable text file.
// Gated: LAN-only (allowLANOnly middleware) + system must be unhealthy.
func (s *GinServer) handleDiagnosticLog(c *gin.Context) {
	// Only accessible when system is unhealthy — generic mechanism for any failure.
	// Fail-closed: if healthTracker is nil, block access.
	if s.healthTracker == nil || s.healthTracker.Overall() == health.LevelOK {
		c.JSON(http.StatusForbidden, gin.H{"error": "system is operational"})
		return
	}
	s.serveDiagnosticLog(c)
}

// handleAdminDiagnosticLog: GET /api/v1/system/admin/diagnostic-log
// Serves redacted diagnostic log to authenticated admins — always available.
func (s *GinServer) handleAdminDiagnosticLog(c *gin.Context) {
	s.serveDiagnosticLog(c)
}

// serveDiagnosticLog is the shared implementation for both diagnostic log endpoints.
func (s *GinServer) serveDiagnosticLog(c *gin.Context) {
	out, err := s.fetchRedactedJournal(c.Request.Context())
	if err != nil {
		log.Printf("WARN: fetchRedactedJournal: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read journal"})
		return
	}
	c.Header("Content-Disposition", "attachment; filename=piccolod-diagnostic.log")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", out)
}

// fetchRedactedJournal runs journalctl and applies defense-in-depth redaction.
func (s *GinServer) fetchRedactedJournal(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "journalctl",
		"-u", "piccolod.service",
		"-b", "--no-pager", "--lines=10000", "-o", "short-iso",
	)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return logutil.Redact(out), nil
}
