package server

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTLSErrorFilter_RateLimits(t *testing.T) {
	var buf bytes.Buffer
	w := newTLSErrorFilterWriter(&buf)

	msg := "http: TLS handshake error from 192.168.1.10:12345: EOF\n"
	for i := 0; i < 10; i++ {
		w.Write([]byte(msg))
	}

	lines := strings.Count(buf.String(), "TLS handshake error")
	if lines != 1 {
		t.Errorf("expected 1 pass-through, got %d", lines)
	}
}

func TestTLSErrorFilter_DifferentClients(t *testing.T) {
	var buf bytes.Buffer
	w := newTLSErrorFilterWriter(&buf)

	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("http: TLS handshake error from 10.0.0.%d:9999: EOF\n", i)
		w.Write([]byte(msg))
	}

	lines := strings.Count(buf.String(), "TLS handshake error")
	if lines != 5 {
		t.Errorf("expected 5 (one per client), got %d", lines)
	}
}

func TestTLSErrorFilter_NonTLSPassThrough(t *testing.T) {
	var buf bytes.Buffer
	w := newTLSErrorFilterWriter(&buf)

	msg := "some other log message\n"
	for i := 0; i < 5; i++ {
		w.Write([]byte(msg))
	}

	lines := strings.Count(buf.String(), "some other log message")
	if lines != 5 {
		t.Errorf("expected 5 (all pass through), got %d", lines)
	}
}

func TestTLSErrorFilter_MapCleanup(t *testing.T) {
	var buf bytes.Buffer
	w := newTLSErrorFilterWriter(&buf)

	// Fill with stale entries
	stale := time.Now().Add(-3 * tlsFilterMinInterval)
	for i := 0; i < 100; i++ {
		w.lastLogged[fmt.Sprintf("10.0.%d.%d", i/256, i%256)] = stale
	}

	// Write a new entry to trigger cleanup
	w.Write([]byte("http: TLS handshake error from 99.99.99.99:1234: EOF\n"))

	w.mu.Lock()
	remaining := len(w.lastLogged)
	w.mu.Unlock()

	if remaining > tlsFilterMaxEntries {
		t.Errorf("expected map to be cleaned, still has %d entries", remaining)
	}
}

func TestExtractTLSClientKey(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{"ipv4", "http: TLS handshake error from 192.168.1.10:12345: EOF", "192.168.1.10"},
		{"ipv6", "http: TLS handshake error from [::1]:12345: EOF", "[::1]"},
		{"no_match", "some other message", ""},
		{"no_colon_after", "http: TLS handshake error from 1.2.3.4", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTLSClientKey(tt.msg)
			if got != tt.want {
				t.Errorf("extractTLSClientKey(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}
