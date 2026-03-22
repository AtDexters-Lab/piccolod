package identity

import "testing"

func TestExtractHostname(t *testing.T) {
	tests := []struct {
		name string
		fqdn string
		want string
	}{
		{"standard lego FQDN", "_acme-challenge.mydevice.example.com.", "mydevice.example.com"},
		{"without trailing dot", "_acme-challenge.mydevice.example.com", "mydevice.example.com"},
		{"canonical hostname", "_acme-challenge.a1b2c3d4.example.com.", "a1b2c3d4.example.com"},
		{"missing prefix", "mydevice.example.com.", ""},
		{"empty string", "", ""},
		{"prefix only with dot", "_acme-challenge.", ""},
		{"prefix with only trailing dot", "_acme-challenge..", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHostname(tt.fqdn)
			if got != tt.want {
				t.Errorf("extractHostname(%q) = %q, want %q", tt.fqdn, got, tt.want)
			}
		})
	}
}
