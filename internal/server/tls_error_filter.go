package server

import (
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	tlsFilterMinInterval = 1 * time.Minute
	tlsFilterMaxEntries  = 64
)

// tlsErrorFilterWriter rate-limits "TLS handshake error" messages from Go's
// net/http server. Non-TLS messages pass through unfiltered.
type tlsErrorFilterWriter struct {
	out        io.Writer
	mu         sync.Mutex
	lastLogged map[string]time.Time
}

func newTLSErrorFilterWriter(out io.Writer) *tlsErrorFilterWriter {
	return &tlsErrorFilterWriter{
		out:        out,
		lastLogged: make(map[string]time.Time),
	}
}

func (w *tlsErrorFilterWriter) Write(p []byte) (int, error) {
	msg := string(p)
	if !strings.Contains(msg, "TLS handshake error") {
		return w.out.Write(p)
	}

	clientKey := extractTLSClientKey(msg)
	if clientKey == "" {
		return w.out.Write(p)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	if last, ok := w.lastLogged[clientKey]; ok && now.Sub(last) < tlsFilterMinInterval {
		return len(p), nil // suppress
	}

	w.lastLogged[clientKey] = now

	// Bounded cleanup: purge stale entries when map grows beyond threshold
	if len(w.lastLogged) > tlsFilterMaxEntries {
		cutoff := now.Add(-2 * tlsFilterMinInterval)
		for k, t := range w.lastLogged {
			if t.Before(cutoff) {
				delete(w.lastLogged, k)
			}
		}
		// Hard cap: if still over limit after stale cleanup, reset map to
		// prevent unbounded growth from many unique client IPs in a burst.
		if len(w.lastLogged) > tlsFilterMaxEntries {
			w.lastLogged = make(map[string]time.Time)
		}
	}

	return w.out.Write(p)
}

// extractTLSClientKey extracts the client IP from a TLS handshake error message.
// Format: "http: TLS handshake error from 192.168.1.10:12345: ..."
func extractTLSClientKey(msg string) string {
	const prefix = "TLS handshake error from "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(prefix):]
	// Extract IP:port, then strip port
	ipPort, _, ok := strings.Cut(rest, ": ")
	if !ok {
		return ""
	}
	// ipPort is "IP:port" — strip trailing port
	if lastColon := strings.LastIndex(ipPort, ":"); lastColon > 0 {
		return ipPort[:lastColon]
	}
	return ipPort
}

// newFilteredErrorLogger creates a log.Logger that rate-limits TLS handshake errors.
func newFilteredErrorLogger() *log.Logger {
	return log.New(newTLSErrorFilterWriter(log.Writer()), "", 0)
}
