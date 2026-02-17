package services

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipResponseWriter_CompressesText(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/html; charset=utf-8")
	gzw.WriteHeader(http.StatusOK)

	body := strings.Repeat("<p>hello world</p>", 100)
	gzw.Write([]byte(body))
	gzw.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected Content-Encoding: gzip")
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("expected Vary: Accept-Encoding")
	}

	// Verify body is valid gzip
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	decoded, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("reading gzip body: %v", err)
	}
	if string(decoded) != body {
		t.Fatalf("decoded body mismatch: got %d bytes, want %d", len(decoded), len(body))
	}
}

func TestGzipResponseWriter_SkipsAlreadyCompressed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/html")
	gzw.Header().Set("Content-Encoding", "gzip")
	gzw.WriteHeader(http.StatusOK)

	data := []byte("already-compressed-bytes")
	gzw.Write(data)
	gzw.Close()

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected original Content-Encoding preserved")
	}
	// Body should be passthrough (not double-compressed)
	if rec.Body.String() != string(data) {
		t.Fatalf("expected passthrough body, got %q", rec.Body.String())
	}
}

func TestGzipResponseWriter_SkipsNonCompressible(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "image/png")
	gzw.WriteHeader(http.StatusOK)

	data := []byte("fake-png-data")
	gzw.Write(data)
	gzw.Close()

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding for image/png")
	}
	if rec.Body.String() != string(data) {
		t.Fatal("expected passthrough body")
	}
}

func TestGzipResponseWriter_SkipsNoAcceptEncoding(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	// No Accept-Encoding header

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/html")
	gzw.WriteHeader(http.StatusOK)

	data := []byte(strings.Repeat("hello", 100))
	gzw.Write(data)
	gzw.Close()

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding without Accept-Encoding")
	}
	if rec.Body.String() != string(data) {
		t.Fatal("expected passthrough body")
	}
}

func TestGzipResponseWriter_SkipsGzipQZero(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0, deflate")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/html")
	gzw.WriteHeader(http.StatusOK)

	data := []byte(strings.Repeat("hello", 100))
	gzw.Write(data)
	gzw.Close()

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding when gzip;q=0")
	}
	if rec.Body.String() != string(data) {
		t.Fatal("expected passthrough body when gzip;q=0")
	}
}

func TestGzipResponseWriter_SkipsSmallResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "application/json")
	gzw.Header().Set("Content-Length", "100")
	gzw.WriteHeader(http.StatusOK)

	data := []byte(`{"ok":true}`)
	gzw.Write(data)
	gzw.Close()

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding for small response")
	}
}

func TestGzipResponseWriter_Status206(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "video/mp4")
	gzw.Header().Set("Content-Range", "bytes 0-999/5000")
	gzw.WriteHeader(http.StatusPartialContent)

	data := []byte("partial-content-bytes")
	gzw.Write(data)
	gzw.Close()

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding on 206 partial content")
	}
	if rec.Body.String() != string(data) {
		t.Fatal("expected passthrough body for 206")
	}
}

func TestGzipResponseWriter_Flush(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/event-stream")
	gzw.WriteHeader(http.StatusOK)

	gzw.Write([]byte("data: event1\n\n"))
	gzw.Flush()

	// Should not panic and body should have data
	if rec.Body.Len() == 0 {
		t.Fatal("expected flushed data in body")
	}
	gzw.Close()
}

func TestGzipResponseWriter_Status304(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/html")
	gzw.WriteHeader(http.StatusNotModified)
	gzw.Close()

	if rec.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding on 304")
	}
}

func TestGzipResponseWriter_ImplicitWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "application/json")

	// Write without calling WriteHeader first
	body := strings.Repeat(`{"data":"value"}`, 50)
	gzw.Write([]byte(body))
	gzw.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected implicit 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip compression via implicit WriteHeader")
	}

	// Verify body decompresses correctly
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	decoded, _ := io.ReadAll(gr)
	if string(decoded) != body {
		t.Fatal("decoded body mismatch")
	}
}

func TestGzipResponseWriter_ChunkedNoContentLength(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/plain")
	// No Content-Length set — should still compress
	gzw.WriteHeader(http.StatusOK)

	body := strings.Repeat("chunked data ", 100)
	gzw.Write([]byte(body))
	gzw.Close()

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip for chunked text/plain")
	}
}

// mockHijacker wraps httptest.ResponseRecorder with Hijack support.
type mockHijacker struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (m *mockHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rw := bufio.NewReadWriter(bufio.NewReader(m.conn), bufio.NewWriter(m.conn))
	return m.conn, rw, nil
}

func TestGzipResponseWriter_WebSocketUpgrade(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rec := &mockHijacker{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             server,
	}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Upgrade", "websocket")

	gzw := newGzipResponseWriter(rec, req)
	gzw.Header().Set("Content-Type", "text/html") // irrelevant for 101

	// 101 should not compress (statusCode < 200 check skips it)
	gzw.WriteHeader(http.StatusSwitchingProtocols)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("expected no Content-Encoding on 101")
	}

	// Hijack should delegate
	conn, _, err := gzw.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn from Hijack")
	}
	gzw.Close()
}

func TestIsCompressibleContentType(t *testing.T) {
	tests := []struct {
		ct     string
		expect bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"text/plain", true},
		{"text/css", true},
		{"text/xml", true},
		{"text/javascript", true},
		{"text/event-stream", false},
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/javascript", true},
		{"application/xml", true},
		{"application/xhtml+xml", true},
		{"application/rss+xml", true},
		{"application/atom+xml", true},
		{"application/ld+json", true},
		{"image/svg+xml", true},
		{"image/png", false},
		{"image/jpeg", false},
		{"video/mp4", false},
		{"audio/mpeg", false},
		{"application/octet-stream", false},
		{"application/wasm", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ct, func(t *testing.T) {
			got := isCompressibleContentType(tt.ct)
			if got != tt.expect {
				t.Errorf("isCompressibleContentType(%q) = %v, want %v", tt.ct, got, tt.expect)
			}
		})
	}
}
