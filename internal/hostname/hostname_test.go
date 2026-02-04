package hostname

import (
	"testing"

	"piccolod/internal/api"
)

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
			name: "implicit primary - skip raw",
			listeners: []api.AppListener{
				{Name: "tcp", Protocol: api.ListenerProtocolRaw, Flow: api.FlowTCP},
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
			},
			want:    "web",
			wantErr: false,
		},
		{
			name: "implicit primary - skip tls",
			listeners: []api.AppListener{
				{Name: "secure", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTLS},
				{Name: "web", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTCP},
			},
			want:    "web",
			wantErr: false,
		},
		{
			name: "no eligible primary",
			listeners: []api.AppListener{
				{Name: "tcp", Protocol: api.ListenerProtocolRaw, Flow: api.FlowTCP},
				{Name: "secure", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTLS},
			},
			want:    "",
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
			name: "primary on tls",
			listeners: []api.AppListener{
				{Name: "secure", Protocol: api.ListenerProtocolHTTP, Flow: api.FlowTLS, Primary: true},
			},
			wantErr: true,
		},
		{
			name: "primary on raw",
			listeners: []api.AppListener{
				{Name: "tcp", Protocol: api.ListenerProtocolRaw, Flow: api.FlowTCP, Primary: true},
			},
			wantErr: true,
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
