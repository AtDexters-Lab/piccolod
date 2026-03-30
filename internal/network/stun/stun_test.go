package stun

import "testing"

func TestNormalizeIP(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"bare IPv4", "203.0.113.5", "203.0.113.5"},
		{"IPv4 with port", "203.0.113.5:14812", "203.0.113.5"},
		{"IPv4 with whitespace", "  203.0.113.5  ", "203.0.113.5"},
		{"IPv4-mapped IPv6", "::ffff:203.0.113.5", "203.0.113.5"},
		{"bare IPv6", "2001:db8::1", "2001:db8::1"},
		{"bracketed IPv6 with port", "[2001:db8::1]:443", "2001:db8::1"},
		{"IPv6 loopback", "::1", "::1"},
		{"bracketed IPv6 loopback with port", "[::1]:8080", "::1"},
		{"empty string", "", ""},
		{"non-IP string", "hello", "hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeIP(tc.input)
			if got != tc.expected {
				t.Errorf("NormalizeIP(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}
