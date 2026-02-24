package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/container"
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

// handleNetworkCheck: GET /api/v1/system/network-check
// Runs quick connectivity probes: raw IP (8.8.8.8:53), DNS (google.com),
// and container registry (registry-1.docker.io:443).
func (s *GinServer) handleNetworkCheck(c *gin.Context) {
	type probe struct {
		Name    string `json:"name"`
		Target  string `json:"target"`
		OK      bool   `json:"ok"`
		Latency string `json:"latency,omitempty"`
		Error   string `json:"error,omitempty"`
	}

	timeout := 5 * time.Second
	checks := []struct {
		name   string
		target string // host:port for TCP dial
		dns    string // hostname to resolve (empty = skip DNS, just TCP)
	}{
		{"ip_connectivity", "8.8.8.8:53", ""},
		{"dns_resolution", "google.com:443", "google.com"},
		{"docker_registry", "registry-1.docker.io:443", "registry-1.docker.io"},
	}

	results := make([]probe, 0, len(checks))
	for _, chk := range checks {
		p := probe{Name: chk.name, Target: chk.target}
		start := time.Now()

		if chk.dns != "" {
			// Test DNS resolution first
			addrs, err := net.LookupHost(chk.dns)
			if err != nil {
				p.Error = fmt.Sprintf("dns: %v", err)
				p.Latency = time.Since(start).Round(time.Millisecond).String()
				results = append(results, p)
				continue
			}
			_ = addrs
		}

		// TCP connect
		conn, err := net.DialTimeout("tcp", chk.target, timeout)
		if err != nil {
			p.Error = fmt.Sprintf("tcp: %v", err)
		} else {
			conn.Close()
			p.OK = true
		}
		p.Latency = time.Since(start).Round(time.Millisecond).String()
		results = append(results, p)
	}

	c.JSON(http.StatusOK, gin.H{"probes": results})
}

// handleStorageCheck: GET /api/v1/system/storage-check
// Runs a quick podman command to test if storage initialization works for the
// image runtime. This helps diagnose fuse-overlayfs / overlay driver issues.
func (s *GinServer) handleStorageCheck(c *gin.Context) {
	if s.appManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "app manager not ready"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	type checkResult struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Output  string `json:"output,omitempty"`
		Error   string `json:"error,omitempty"`
		Latency string `json:"latency,omitempty"`
	}

	// Test: list images using image runtime (quick storage init check)
	start := time.Now()
	imageRT, err := s.appManager.PodmanImageRuntime()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"checks": []checkResult{
			{Name: "image_runtime_init", Error: err.Error()},
		}})
		return
	}

	results := make([]checkResult, 0, 2)
	results = append(results, checkResult{
		Name:    "image_runtime_init",
		OK:      true,
		Output:  fmt.Sprintf("root=%s driver=%s", imageRT.Root, imageRT.StorageDriver),
		Latency: time.Since(start).Round(time.Millisecond).String(),
	})

	// Test: run "podman images" with the image runtime to probe storage
	start2 := time.Now()
	imagesArgs := []string{
		"--root", imageRT.Root,
		"--runroot", imageRT.RunRoot,
		"--storage-driver", imageRT.StorageDriver,
	}
	for _, opt := range imageRT.StorageOpts {
		imagesArgs = append(imagesArgs, "--storage-opt", opt)
	}
	imagesArgs = append(imagesArgs, "images", "--format", "{{.Repository}}:{{.Tag}}")

	cmd := exec.CommandContext(ctx, "podman", imagesArgs...)
	container.ApplyRuntimeCredential(cmd, imageRT)
	out, cmdErr := cmd.CombinedOutput()
	r := checkResult{
		Name:    "podman_images",
		OK:      cmdErr == nil,
		Output:  string(out),
		Latency: time.Since(start2).Round(time.Millisecond).String(),
	}
	if cmdErr != nil {
		r.Error = cmdErr.Error()
	}
	results = append(results, r)

	c.JSON(http.StatusOK, gin.H{"checks": results})
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
