package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	telemetryMaxBodyBytes  = 64 * 1024
	telemetryMaxEntries    = 50
	telemetryMaxMessageLen = 512
	telemetryMaxStackLen   = 2048
	telemetryMaxRouteLen   = 64
	telemetryMaxVersionLen = 32
	telemetryMaxScreenLen  = 16
)

var telemetryKnownTypes = map[string]bool{
	"flutter_error": true,
	"async_error":   true,
	"web_error":     true,
	"js_rejection":  true,
	"jank":          true,
}

type telemetryRequest struct {
	Entries []telemetryEntry `json:"entries"`
	Session telemetrySession `json:"session"`
}

type telemetryEntry struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Route   string `json:"route,omitempty"`
	Count   int    `json:"count,omitempty"`
	Ts      string `json:"ts,omitempty"`
}

type telemetrySession struct {
	PiccoloVersion string `json:"piccolo_version,omitempty"`
	Screen         string `json:"screen,omitempty"`
}

// handleTelemetryLog: POST /api/v1/telemetry/log
// Accepts batched UI telemetry entries and logs them to systemd journal via log.Printf.
// Uses json.NewDecoder instead of ShouldBindJSON to work with MaxBytesReader body limiting.
func (s *GinServer) handleTelemetryLog(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, telemetryMaxBodyBytes)

	var req telemetryRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if len(req.Entries) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "entries required"})
		return
	}
	if len(req.Entries) > telemetryMaxEntries {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many entries (max %d)", telemetryMaxEntries)})
		return
	}

	version := sanitizeTelemetryField(req.Session.PiccoloVersion, telemetryMaxVersionLen)
	if version == "" {
		version = "unknown"
	}
	screen := sanitizeTelemetryField(req.Session.Screen, telemetryMaxScreenLen)

	for _, e := range req.Entries {
		if !telemetryKnownTypes[e.Type] {
			continue
		}

		msg := sanitizeTelemetryField(e.Message, telemetryMaxMessageLen)
		route := sanitizeTelemetryField(e.Route, telemetryMaxRouteLen)
		ts := sanitizeTelemetryTimestamp(e.Ts)
		count := e.Count
		if count < 1 {
			count = 1
		}
		if count > 10000 {
			count = 10000
		}

		parts := []string{
			fmt.Sprintf("type=%s", e.Type),
		}
		if route != "" {
			parts = append(parts, fmt.Sprintf("route=%s", route))
		}
		parts = append(parts, fmt.Sprintf("count=%d", count))
		if ts != "" {
			parts = append(parts, fmt.Sprintf("ts=%s", ts))
		}
		parts = append(parts, fmt.Sprintf("msg=%q", msg))
		parts = append(parts, fmt.Sprintf("version=%s", version))
		if screen != "" {
			parts = append(parts, fmt.Sprintf("screen=%s", screen))
		}

		log.Printf("ui-telemetry: %s", strings.Join(parts, " "))

		// Log stack trace for error types only
		if e.Stack != "" && (e.Type == "flutter_error" || e.Type == "async_error" || e.Type == "web_error") {
			stack := sanitizeTelemetryField(e.Stack, telemetryMaxStackLen)
			log.Printf("ui-telemetry-stack: type=%s msg=%q stack=%q", e.Type, truncateTelemetryString(msg, 80), stack)
		}
	}

	c.Status(http.StatusAccepted)
}

// sanitizeTelemetryField strips control characters and truncates at a UTF-8 safe boundary.
func sanitizeTelemetryField(s string, maxLen int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1 // drop all control characters
		}
		return r
	}, s)
	return truncateTelemetryString(s, maxLen)
}

// sanitizeTelemetryTimestamp validates and returns a timestamp string, or empty if invalid.
func sanitizeTelemetryTimestamp(s string) string {
	if s == "" {
		return ""
	}
	// Accept RFC3339 timestamps only (Go accepts fractional seconds even with RFC3339 layout)
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return ""
	}
	return s
}

// truncateTelemetryString truncates s to at most maxLen bytes, respecting UTF-8 rune boundaries.
func truncateTelemetryString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Walk back from maxLen to find a valid rune boundary
	for maxLen > 0 && !utf8.RuneStart(s[maxLen]) {
		maxLen--
	}
	return s[:maxLen]
}
