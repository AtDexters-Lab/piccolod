package identity

import (
	"fmt"
	"testing"
)

func TestExtractErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error", nil, ""},
		{"non-server error", fmt.Errorf("connection refused"), ""},
		{"device not found", fmt.Errorf(`server error 401: {"error":"device not found"}`), "device not found"},
		{"quote verification failed", fmt.Errorf(`server error 401: {"error":"quote verification failed"}`), "quote verification failed"},
		{"nonce expired", fmt.Errorf(`server error 401: {"error":"nonce expired"}`), "nonce expired"},
		{"device suspended", fmt.Errorf(`server error 403: {"error":"device suspended"}`), "device suspended"},
		{"non-JSON body", fmt.Errorf("server error 401: plain text error"), "plain text error"},
		{"empty JSON error field", fmt.Errorf(`server error 401: {"error":""}`), `{"error":""}`},
		{"no colon separator", fmt.Errorf("server error 401"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractErrorMessage(tt.err)
			if got != tt.want {
				t.Errorf("extractErrorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractHTTPStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"401", fmt.Errorf(`server error 401: {"error":"device not found"}`), 401},
		{"403", fmt.Errorf(`server error 403: {"error":"suspended"}`), 403},
		{"non-server", fmt.Errorf("connection refused"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHTTPStatus(tt.err)
			if got != tt.want {
				t.Errorf("extractHTTPStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}
