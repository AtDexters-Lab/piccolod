package services

import "testing"

func TestHintClientIPNormalizesNexusRemoteAddress(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare ipv4", raw: "203.0.113.10", want: "203.0.113.10"},
		{name: "ipv4 remote addr", raw: "203.0.113.10:54321", want: "203.0.113.10"},
		{name: "bare ipv6", raw: "2001:db8::10", want: "2001:db8::10"},
		{name: "bracketed ipv6", raw: "[2001:db8::10]", want: "2001:db8::10"},
		{name: "ipv6 remote addr", raw: "[2001:db8::10]:54321", want: "2001:db8::10"},
		{name: "malformed stays fail-closed", raw: "not an ip:54321", want: "not an ip:54321"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hintClientIP(connectionHint{clientIP: tt.raw}, true)
			if got != tt.want {
				t.Fatalf("hintClientIP(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}

	if got := hintClientIP(connectionHint{clientIP: "203.0.113.10:54321"}, false); got != "" {
		t.Fatalf("hintClientIP without hint = %q, want empty", got)
	}
}
