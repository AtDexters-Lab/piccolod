package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupTelemetryTestRouter() (*gin.Engine, *GinServer) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	s := &GinServer{}
	r.POST("/api/v1/telemetry/log", s.handleTelemetryLog)
	return r, s
}

func postTelemetry(r *gin.Engine, body interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/log", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleTelemetryLog(t *testing.T) {
	t.Run("valid_single_entry", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		w := postTelemetry(r, telemetryRequest{
			Entries: []telemetryEntry{
				{Type: "flutter_error", Message: "test error", Count: 1},
			},
			Session: telemetrySession{PiccoloVersion: "0.9.1"},
		})
		if w.Code != http.StatusAccepted {
			t.Errorf("got status %d, want %d", w.Code, http.StatusAccepted)
		}
	})

	t.Run("valid_batch", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		entries := make([]telemetryEntry, 5)
		for i := range entries {
			entries[i] = telemetryEntry{Type: "jank", Message: "frame took 120ms", Count: 1}
		}
		w := postTelemetry(r, telemetryRequest{
			Entries: entries,
			Session: telemetrySession{PiccoloVersion: "dev"},
		})
		if w.Code != http.StatusAccepted {
			t.Errorf("got status %d, want %d", w.Code, http.StatusAccepted)
		}
	})

	t.Run("empty_entries", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		w := postTelemetry(r, telemetryRequest{
			Entries: []telemetryEntry{},
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("too_many_entries", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		entries := make([]telemetryEntry, 51)
		for i := range entries {
			entries[i] = telemetryEntry{Type: "jank", Message: "test", Count: 1}
		}
		w := postTelemetry(r, telemetryRequest{
			Entries: entries,
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("unknown_type_skipped", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		w := postTelemetry(r, telemetryRequest{
			Entries: []telemetryEntry{
				{Type: "unknown_type", Message: "test"},
			},
			Session: telemetrySession{PiccoloVersion: "dev"},
		})
		// Still returns 202 — unknown types are silently skipped
		if w.Code != http.StatusAccepted {
			t.Errorf("got status %d, want %d", w.Code, http.StatusAccepted)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/log",
			strings.NewReader("not json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("body_too_large", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		// Create a body larger than 64KB
		bigMsg := strings.Repeat("x", 65*1024)
		body, _ := json.Marshal(telemetryRequest{
			Entries: []telemetryEntry{
				{Type: "flutter_error", Message: bigMsg},
			},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry/log",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("got status %d, want %d; body too large should be rejected", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("all_known_types_accepted", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		for _, typ := range []string{"flutter_error", "async_error", "web_error", "js_rejection", "jank"} {
			w := postTelemetry(r, telemetryRequest{
				Entries: []telemetryEntry{
					{Type: typ, Message: "test " + typ, Count: 1},
				},
				Session: telemetrySession{PiccoloVersion: "dev"},
			})
			if w.Code != http.StatusAccepted {
				t.Errorf("type %q: got status %d, want %d", typ, w.Code, http.StatusAccepted)
			}
		}
	})

	t.Run("control_chars_stripped", func(t *testing.T) {
		r, _ := setupTelemetryTestRouter()
		w := postTelemetry(r, telemetryRequest{
			Entries: []telemetryEntry{
				{Type: "web_error", Message: "evil\nmsg=%q injected\x00null\ttab", Count: 1},
			},
			Session: telemetrySession{PiccoloVersion: "1.0"},
		})
		if w.Code != http.StatusAccepted {
			t.Errorf("got status %d, want %d", w.Code, http.StatusAccepted)
		}
	})
}

func TestSanitizeTelemetryField(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"plain", "hello", 10, "hello"},
		{"truncate", "hello world", 5, "hello"},
		{"newlines_stripped", "line1\nline2\rline3", 50, "line1line2line3"},
		{"null_bytes_stripped", "hello\x00world", 50, "helloworld"},
		{"tabs_stripped", "hello\tworld", 50, "helloworld"},
		{"quotes_preserved", `say "hello"`, 50, `say "hello"`},
		{"empty", "", 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeTelemetryField(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("sanitizeTelemetryField(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestSanitizeTelemetryTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid_rfc3339", "2026-02-14T10:00:00Z", "2026-02-14T10:00:00Z"},
		{"valid_with_offset", "2026-02-14T10:00:00+05:30", "2026-02-14T10:00:00+05:30"},
		{"valid_with_millis", "2026-02-14T10:00:00.000Z", "2026-02-14T10:00:00.000Z"},
		{"invalid", "not-a-timestamp", ""},
		{"empty", "", ""},
		{"partial", "2026-02-14", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeTelemetryTimestamp(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeTelemetryTimestamp(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateTelemetryString(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hi", 10, "hi"},
		{"exact", "hello", 5, "hello"},
		{"truncate_ascii", "hello world", 5, "hello"},
		{"truncate_utf8_safe", "hello\u00e9", 6, "hello"},                // é is 2 bytes, won't fit
		{"truncate_utf8_fits", "hello\u00e9", 7, "hello\u00e9"},          // é fits in 7 bytes
		{"multibyte_boundary", "\u00e9\u00e9\u00e9", 3, "\u00e9"},       // 3 x 2-byte chars, truncate mid-sequence
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateTelemetryString(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateTelemetryString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
