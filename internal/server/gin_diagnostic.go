package server

import (
	"net/http"
	"os/exec"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/health"
)

// handleDiagnosticLog: GET /api/v1/system/diagnostic-log
// Serves piccolod.service journal entries since boot as a downloadable text file.
// Gated: LAN-only (allowLANOnly middleware) + system must be unhealthy.
func (s *GinServer) handleDiagnosticLog(c *gin.Context) {
	// Only accessible when system is unhealthy — generic mechanism for any failure.
	// Fail-closed: if healthTracker is nil, block access.
	if s.healthTracker == nil || s.healthTracker.Overall() == health.LevelOK {
		c.JSON(http.StatusForbidden, gin.H{"error": "system is operational"})
		return
	}

	ctx := c.Request.Context()
	cmd := exec.CommandContext(ctx, "journalctl",
		"-u", "piccolod.service",
		"-b",
		"--no-pager",
		"--lines=10000",
		"-o", "short-iso",
	)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read journal"})
		return
	}

	c.Header("Content-Disposition", "attachment; filename=piccolod-diagnostic.log")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", out)
}
