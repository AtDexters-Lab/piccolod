package services

import (
	"bufio"
	"compress/gzip"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minCompressBytes is the minimum Content-Length for compression to be worthwhile.
const minCompressBytes = 256

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// gzipResponseWriter wraps http.ResponseWriter with conditional gzip compression.
// It defers the compression decision until WriteHeader, when Content-Type and
// Content-Encoding are known from the proxied backend.
type gzipResponseWriter struct {
	http.ResponseWriter
	gzWriter   *gzip.Writer
	request    *http.Request
	compress   bool // true = gzip, false = passthrough
	decided    bool // compression decision made
	headerSent bool // WriteHeader called on underlying writer
	statusCode int  // buffered status code
}

func newGzipResponseWriter(w http.ResponseWriter, r *http.Request) *gzipResponseWriter {
	return &gzipResponseWriter{
		ResponseWriter: w,
		request:        r,
	}
}

func (w *gzipResponseWriter) WriteHeader(statusCode int) {
	if w.headerSent {
		return
	}

	w.statusCode = statusCode

	if !w.decided {
		w.decided = true
		w.compress = w.shouldCompress(statusCode)

		if w.compress {
			h := w.ResponseWriter.Header()
			h.Del("Content-Length")
			h.Set("Content-Encoding", "gzip")
			if !strings.Contains(h.Get("Vary"), "Accept-Encoding") {
				h.Add("Vary", "Accept-Encoding")
			}

			gz := gzipWriterPool.Get().(*gzip.Writer)
			gz.Reset(w.ResponseWriter)
			w.gzWriter = gz
		}
	}

	w.headerSent = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *gzipResponseWriter) shouldCompress(statusCode int) bool {
	// No body expected, or partial content (compressing would invalidate Content-Range)
	if statusCode < 200 || statusCode == 204 || statusCode == 206 || statusCode == 304 {
		return false
	}

	h := w.ResponseWriter.Header()

	// Backend already compressed
	if h.Get("Content-Encoding") != "" {
		return false
	}

	// Client doesn't accept gzip
	if !clientAcceptsGzip(w.request) {
		return false
	}

	// Check content type
	ct := h.Get("Content-Type")
	if !isCompressibleContentType(ct) {
		return false
	}

	// Small responses not worth compressing
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n < minCompressBytes {
			return false
		}
	}

	return true
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.headerSent {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		return w.gzWriter.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush flushes buffered gzip data and the underlying writer.
func (w *gzipResponseWriter) Flush() {
	if w.compress && w.gzWriter != nil {
		w.gzWriter.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying http.Hijacker for WebSocket upgrades.
func (w *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController
// interface discovery (Go 1.20+).
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Close finalizes gzip compression and returns the writer to the pool.
func (w *gzipResponseWriter) Close() error {
	if w.gzWriter != nil {
		err := w.gzWriter.Close()
		if err == nil {
			gzipWriterPool.Put(w.gzWriter)
		}
		w.gzWriter = nil
		return err
	}
	return nil
}

// clientAcceptsGzip checks if the request's Accept-Encoding includes gzip
// with a non-zero quality value. Per RFC 7231, "gzip;q=0" means the client
// explicitly refuses gzip.
func clientAcceptsGzip(r *http.Request) bool {
	ae := r.Header.Get("Accept-Encoding")
	for _, part := range strings.Split(ae, ",") {
		part = strings.TrimSpace(part)
		name, params, _ := strings.Cut(part, ";")
		if strings.TrimSpace(name) != "gzip" {
			continue
		}
		// Check for explicit q=0 (refusal). q=0.0 also counts, but q=0.1 etc. are fine.
		params = strings.TrimSpace(params)
		if params == "" {
			return true // no quality parameter means q=1
		}
		for _, p := range strings.Split(params, ";") {
			p = strings.TrimSpace(p)
			if k, v, ok := strings.Cut(p, "="); ok && strings.TrimSpace(k) == "q" {
				v = strings.TrimSpace(v)
				if v == "0" || v == "0.0" || v == "0.00" || v == "0.000" {
					return false
				}
			}
		}
		return true
	}
	return false
}

// isCompressibleContentType returns true if the media type is text-based or
// a known compressible application type.
func isCompressibleContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(ct)
	if mediaType == "" {
		return false
	}

	// All text types are compressible, except event-stream (SSE latency)
	if strings.HasPrefix(mediaType, "text/") {
		return mediaType != "text/event-stream"
	}

	switch mediaType {
	case "application/json",
		"application/javascript",
		"application/xml",
		"application/xhtml+xml",
		"application/rss+xml",
		"application/atom+xml",
		"application/ld+json",
		"image/svg+xml":
		return true
	}
	return false
}
