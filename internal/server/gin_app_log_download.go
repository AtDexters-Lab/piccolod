package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/app"
	"piccolod/internal/hostname"
)

// appLogDownloadMaxLines caps the combined download, mirroring the diagnostic
// endpoint's intent.
const appLogDownloadMaxLines = diagnosticMaxLines

// appLogDownloadInflight enforces single-flight per app: one combined download
// at a time per instance. This bounds concurrent merge cost and
// prevents two requests colliding on the same per-app temp snapshot dir.
var appLogDownloadInflight sync.Map

// handleGinAppLogDownload: GET /api/v1/apps/:name/logs/download?from=&to=
// Streams the app's persistent logs over the window, combined and
// time-interleaved across ALL services (ignoring any per-service viewer
// selection in the UI), as a text/plain attachment.
func (s *GinServer) handleGinAppLogDownload(c *gin.Context) {
	appName := c.Param("name")

	// Validate the name before it becomes a filesystem path component
	//: reject anything outside the instance-ID
	// charset so crafted segments (e.g. "lost+found", ".download-tmp") can
	// never select a non-app subtree under the log mount.
	if err := hostname.ValidateAppName(appName); err != nil {
		writeGinError(c, http.StatusNotFound, "app not found")
		return
	}

	// Standard users may only download logs for apps allowed to them (mirrors
	// handleGinAppLogs).
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		allowed, err := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, appName)
		if err != nil || !allowed {
			writeGinError(c, http.StatusForbidden, "Access denied")
			return
		}
	}

	since, until, err := parseDiagnosticRange(c.Query("from"), c.Query("to"), time.Now())
	if err != nil {
		writeGinError(c, http.StatusBadRequest, err.Error())
		return
	}

	// Retryable: the log store is locked / not yet attached. Returned before
	// any attachment bytes are committed so the client gets a clean 503.
	if !app.AppLogsAvailable() {
		writeGinError(c, http.StatusServiceUnavailable, "App log store is unavailable right now (locked or starting) — try again in a moment")
		return
	}

	// Single-flight per app.
	if _, busy := appLogDownloadInflight.LoadOrStore(appName, struct{}{}); busy {
		writeGinError(c, http.StatusTooManyRequests, "A log download for this app is already in progress")
		return
	}
	defer appLogDownloadInflight.Delete(appName)

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=app-%s-logs.txt", appName))
	c.Header("Content-Type", "text/plain; charset=utf-8")

	err = s.appManager.WriteHistoricLogsForApp(c.Request.Context(), appName, since, until, appLogDownloadMaxLines, c.Writer)
	if err != nil && !c.Writer.Written() {
		// The fail-prone preamble (mount probe, readdir, temp-dir create,
		// snapshots) all runs before the first body byte. If it failed and
		// nothing was streamed, return a real status instead of an empty 200
		// attachment so the client's retry/error handling fires.
		c.Writer.Header().Del("Content-Disposition")
		if errors.Is(err, app.ErrAppLogsUnavailable) {
			writeGinError(c, http.StatusServiceUnavailable, "App log store is unavailable right now (locked or starting) — try again in a moment")
		} else {
			writeGinError(c, http.StatusInternalServerError, "Failed to read app logs")
		}
		return
	}
	if err != nil {
		// Streaming had already begun — the 200 is committed, can't change it.
		log.Printf("WARN: app-logs download %s: error after partial output: %v", appName, err)
	}
}
