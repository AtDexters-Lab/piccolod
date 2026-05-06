package hostname

import (
	"testing"

	"piccolod/internal/api"
)

func TestIsValidDNSLabel(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"my-device", true},
		{"a", true},
		{"abc123", true},
		{"a-b-c", true},
		// Mixed case accepted: function lowercases internally per its contract.
		{"UPPER", true},
		{"My-Device", true},
		{"", false},
		{"-leading", false},
		{"trailing-", false},
		{"under_score", false},
		{"has space", false},
		{"has.dot", false},
		{"café", false},                 // non-ASCII rune in UTF-8 bytes
		{"a\x00b", false},               // NUL byte
		{"a\x01b", false},               // control char
		{string(make([]byte, 64)), false}, // 64 chars, too long
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			if got := IsValidDNSLabel(tt.label); got != tt.want {
				t.Errorf("IsValidDNSLabel(%q) = %v, want %v", tt.label, got, tt.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"example.com", "example.com"},
		{"Example.COM", "example.com"},
		{"example.com.", "example.com"},
		{" example.com ", "example.com"},
		{" Example.COM. ", "example.com"},
		{"", ""},
		{".", ""},
		{" . ", ""},
		{"LOCALHOST", "localhost"},
	}
	for _, tt := range tests {
		if got := Normalize(tt.input); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestValidateAppName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "myapp", false},
		{"valid with numbers", "app123", false},
		{"valid single char", "a", false},
		{"valid max length", "abcdefghij123456", false}, // 16 chars (RFC 20260122 §4.3)

		{"empty", "", true},
		{"too long", "abcdefghij1234567", true}, // 17 chars (RFC 20260122: 16 max)
		{"starts with number", "1app", true},
		{"contains hyphen", "my-app", true},
		{"contains underscore", "my_app", true},
		{"contains uppercase", "MyApp", true},
		{"reserved api", "api", true},
		{"reserved www", "www", true},
		{"reserved admin", "admin", true},
		{"reserved root", "root", true},
		{"reserved system", "system", true},
		{"reserved piccolo", "piccolo", true},
		{"reserved piccoloos", "piccoloos", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAppName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateAppName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateListenerName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "web", false},
		{"valid with numbers", "http8080", false},
		{"valid metrics", "metrics", false},

		{"empty", "", true},
		{"too long", "abcdefghijklmnopqrstuvwxyz123456", true},
		{"starts with number", "8080", true},
		{"contains hyphen", "my-listener", true},
		{"reserved piccolo", "piccolo", true},
		{"reserved piccoloos", "piccoloos", true},
		// RFC 20260130: listener name is app identity, so "api" is reserved
		{"api reserved for listener", "api", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateListenerName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateListenerName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateListenerNameFormat(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// Format-valid names (including reserved names — allowed for non-primary)
		{"valid simple", "web", false},
		{"valid metrics", "metrics", false},
		{"reserved api", "api", false},
		{"reserved admin", "admin", false},
		{"reserved www", "www", false},
		{"reserved root", "root", false},
		{"reserved system", "system", false},
		{"reserved piccolo", "piccolo", false},
		{"reserved piccoloos", "piccoloos", false},
		{"valid single char", "a", false},
		{"valid max length", "abcdefghij123456", false},

		// Invalid format
		{"empty", "", true},
		{"too long", "abcdefghij1234567", true},
		{"starts with number", "8080", true},
		{"contains hyphen", "my-listener", true},
		{"contains uppercase", "MyApp", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateListenerNameFormat(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateListenerNameFormat(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestDeriveHostLabel(t *testing.T) {
	tests := []struct {
		name     string
		app      string
		listener string
		primary  bool
		eligible bool // whether listener is eligible for host routing
		want     string
	}{
		// RFC 20260130: For primary listeners, the listener name IS the app identity
		// so app == listener for primary. DeriveHostLabel returns the listener name.
		{"primary http", "immich", "immich", true, true, "immich"},
		{"primary ws", "myapp", "myapp", true, true, "myapp"},
		// Non-primary listeners use "listener-app" format
		{"non-primary http", "immich", "metrics", false, true, "metrics-immich"},
		{"non-primary ws", "immich", "live", false, true, "live-immich"},
		{"raw listener", "myapp", "tcp", true, false, ""},
		{"tls listener", "myapp", "secure", true, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveHostLabel(tt.app, tt.listener, tt.primary, tt.eligible)
			if got != tt.want {
				t.Errorf("DeriveHostLabel(%q, %q, %v, %v) = %q, want %q",
					tt.app, tt.listener, tt.primary, tt.eligible, got, tt.want)
			}
		})
	}
}

func TestResolvePrimaryListener(t *testing.T) {
	tests := []struct {
		name      string
		listeners []api.AppListener
		want      string
		wantErr   bool
	}{
		{
			name: "explicit primary",
			listeners: []api.AppListener{
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP, Primary: true},
				{Name: "metrics", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
			},
			want:    "web",
			wantErr: false,
		},
		{
			name: "implicit primary - first http",
			listeners: []api.AppListener{
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
				{Name: "metrics", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
			},
			want:    "web",
			wantErr: false,
		},
		{
			// Plan §D8: tcp+raw is now host-routable, so it's a valid implicit
			// primary. Auto-select picks the first non-UDP listener.
			name: "implicit primary - tcp raw is now eligible",
			listeners: []api.AppListener{
				{Name: "tcp", Protocol: api.ListenerProtocolRaw, Flow: api.FlowTCP},
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
			},
			want:    "tcp",
			wantErr: false,
		},
		{
			name: "implicit primary - tls is eligible",
			listeners: []api.AppListener{
				{Name: "secure", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTLS},
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
			},
			want:    "secure",
			wantErr: false,
		},
		{
			// Plan §D8: every listener is auto-primary-eligible. First wins.
			name: "implicit primary - first listener wins",
			listeners: []api.AppListener{
				{Name: "dns", Protocol: api.ListenerProtocolRaw, Flow: api.FlowUDP},
				{Name: "tcp", Protocol: api.ListenerProtocolRaw, Flow: api.FlowTCP},
			},
			want:    "dns",
			wantErr: false,
		},
		{
			name: "multiple explicit primaries",
			listeners: []api.AppListener{
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP, Primary: true},
				{Name: "api", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP, Primary: true},
			},
			wantErr: true,
		},
		{
			name: "primary on tls - allowed",
			listeners: []api.AppListener{
				{Name: "secure", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTLS, Primary: true},
			},
			want:    "secure",
			wantErr: false,
		},
		{
			// Plan §D8: tcp+raw + Primary is now allowed. The TLS mux
			// terminates TLS for the client by SNI and proxies raw bytes to
			// the backend.
			name: "primary on raw tcp - allowed (plan §D8)",
			listeners: []api.AppListener{
				{Name: "tcp", Protocol: api.ListenerProtocolRaw, Flow: api.FlowTCP, Primary: true},
			},
			want:    "tcp",
			wantErr: false,
		},
		{
			name: "primary on udp - allowed (explicit)",
			listeners: []api.AppListener{
				{Name: "dns", Protocol: api.ListenerProtocolRaw, Flow: api.FlowUDP, Primary: true},
			},
			want:    "dns",
			wantErr: false,
		},
		{
			// Plan §D8: every flow is auto-primary-eligible.
			name: "implicit primary - udp eligible",
			listeners: []api.AppListener{
				{Name: "dns", Protocol: api.ListenerProtocolRaw, Flow: api.FlowUDP},
			},
			want:    "dns",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePrimaryListener(tt.listeners)
			if (err != nil) != tt.wantErr {
				t.Errorf("ResolvePrimaryListener() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ResolvePrimaryListener() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeHostLabel(t *testing.T) {
	// RFC 20260122 §4.4: 2-level format uses hyphen separator
	tests := []struct {
		name     string
		hostname string
		base     string
		want     string
	}{
		{"simple app 2-level", "immich-piccolo.local", "piccolo.local", "immich"},
		{"listener-app 2-level", "metrics-immich-piccolo.local", "piccolo.local", "metrics-immich"},
		{"apex returns empty", "piccolo.local", "piccolo.local", ""},
		{"no match", "example.com", "piccolo.local", ""},
		{"trailing dots", "immich-piccolo.local.", "piccolo.local.", "immich"},
		{"case insensitive", "IMMICH-PICCOLO.LOCAL", "piccolo.local", "immich"},
		{"old 3-level format no match", "immich.piccolo.local", "piccolo.local", ""}, // Old format should not match
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeHostLabel(tt.hostname, tt.base)
			if got != tt.want {
				t.Errorf("NormalizeHostLabel(%q, %q) = %q, want %q", tt.hostname, tt.base, got, tt.want)
			}
		})
	}
}

func TestParseHostLabel(t *testing.T) {
	tests := []struct {
		name        string
		label       string
		wantApp     string
		wantListen  string
		wantPrimary bool
	}{
		{"primary", "immich", "immich", "", true},
		{"non-primary", "metrics-immich", "immich", "metrics", false},
		{"empty", "", "", "", false},
		{"multiple hyphens", "foo-bar-app", "bar-app", "foo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, listener, isPrimary := ParseHostLabel(tt.label)
			if app != tt.wantApp || listener != tt.wantListen || isPrimary != tt.wantPrimary {
				t.Errorf("ParseHostLabel(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.label, app, listener, isPrimary, tt.wantApp, tt.wantListen, tt.wantPrimary)
			}
		})
	}
}

func TestCheckCollisions(t *testing.T) {
	tests := []struct {
		name      string
		newLabels []string
		existing  map[string]string
		wantErr   bool
	}{
		{
			name:      "no collision",
			newLabels: []string{"myapp", "metrics-myapp"},
			existing:  map[string]string{"otherapp": "app:otherapp"},
			wantErr:   false,
		},
		{
			name:      "collision with existing",
			newLabels: []string{"myapp"},
			existing:  map[string]string{"myapp": "app:myapp"},
			wantErr:   true,
		},
		{
			name:      "collision with reserved",
			newLabels: []string{"piccolo"},
			existing:  map[string]string{},
			wantErr:   true,
		},
		{
			name:      "empty labels ok",
			newLabels: []string{},
			existing:  map[string]string{"existing": "app:existing"},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckCollisions(tt.newLabels, tt.existing)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckCollisions() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
