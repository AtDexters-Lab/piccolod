package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/health"
	"piccolod/internal/logutil"
)

const (
	diagnosticDefaultDays = 3
	diagnosticMaxDays     = 7
	diagnosticMaxLines    = 50000
)

// handleDiagnosticLog: GET /api/v1/system/diagnostic-log
// Serves redacted piccolod.service journal entries from current boot.
// Gated: LAN-only (allowLANOnly middleware) + system must be unhealthy.
func (s *GinServer) handleDiagnosticLog(c *gin.Context) {
	// Only accessible when system is unhealthy — generic mechanism for any failure.
	// Fail-closed: if healthTracker is nil, block access.
	if s.healthTracker == nil || s.healthTracker.Overall() == health.LevelOK {
		c.JSON(http.StatusForbidden, gin.H{"error": "system is operational"})
		return
	}

	out, err := fetchRedactedJournal(c.Request.Context(), "-b", "--lines=10000")
	if err != nil {
		log.Printf("WARN: fetchRedactedJournal: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read journal"})
		return
	}
	c.Header("Content-Disposition", "attachment; filename=piccolod-diagnostic.log")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", out)
}

// handleAdminDiagnosticLog: GET /api/v1/system/admin/diagnostic-log
// Serves redacted diagnostic log to authenticated admins — always available.
// Supports optional ?from=YYYY-MM-DD&to=YYYY-MM-DD query params (max 7-day window).
// Default: last 3 days. Spans across reboots (no -b flag).
func (s *GinServer) handleAdminDiagnosticLog(c *gin.Context) {
	now := time.Now()
	since, until, err := parseDiagnosticRange(c.Query("from"), c.Query("to"), now)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	out, err := fetchRedactedJournal(c.Request.Context(),
		"--since", since.Format("2006-01-02 15:04:05"),
		"--until", until.Format("2006-01-02 15:04:05"),
		"--lines", fmt.Sprintf("%d", diagnosticMaxLines),
	)
	if err != nil {
		log.Printf("WARN: fetchRedactedJournal: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read journal"})
		return
	}
	c.Header("Content-Disposition", "attachment; filename=piccolod-diagnostic.log")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", out)
}

// parseDiagnosticRange resolves from/to query params into a time range.
// Both params are optional (YYYY-MM-DD). Max window is 7 days.
func parseDiagnosticRange(fromStr, toStr string, now time.Time) (since, until time.Time, err error) {
	parseDate := func(s string) (time.Time, error) {
		return time.ParseInLocation("2006-01-02", s, now.Location())
	}

	switch {
	case fromStr == "" && toStr == "":
		// Default: last N days
		since = now.AddDate(0, 0, -diagnosticDefaultDays)
		until = now

	case fromStr != "" && toStr == "":
		// From specified, to = now
		since, err = parseDate(fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid 'from' date, use YYYY-MM-DD")
		}
		until = now

	case fromStr == "" && toStr != "":
		// To specified, from = to - default days
		var to time.Time
		to, err = parseDate(toStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid 'to' date, use YYYY-MM-DD")
		}
		until = to.AddDate(0, 0, 1) // include the full "to" day
		since = until.AddDate(0, 0, -diagnosticDefaultDays)

	default:
		// Both specified
		since, err = parseDate(fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid 'from' date, use YYYY-MM-DD")
		}
		var to time.Time
		to, err = parseDate(toStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid 'to' date, use YYYY-MM-DD")
		}
		until = to.AddDate(0, 0, 1) // include the full "to" day
	}

	if !since.Before(until) {
		return time.Time{}, time.Time{}, fmt.Errorf("'from' must be before 'to'")
	}
	if until.After(since.AddDate(0, 0, diagnosticMaxDays)) {
		return time.Time{}, time.Time{}, fmt.Errorf("date range must not exceed %d days", diagnosticMaxDays)
	}

	return since, until, nil
}

// fetchRedactedJournal runs journalctl with the given extra args and applies redaction.
func fetchRedactedJournal(ctx context.Context, extraArgs ...string) ([]byte, error) {
	args := []string{"-u", "piccolod.service", "--no-pager", "-o", "short-iso"}
	args = append(args, extraArgs...)

	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return logutil.Redact(out), nil
}
