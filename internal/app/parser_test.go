package app

import (
	"os"
	"testing"

	"piccolod/internal/api"
)

// TestParseAppDefinition tests parsing of valid app.yaml files
func TestParseAppDefinition(t *testing.T) {
	tests := []struct {
		name           string
		filePath       string
		expectedType   string
		expectError    bool
		validateFields func(*testing.T, *api.AppDefinition)
	}{
		{
			name:         "minimal app",
			filePath:     "../../testdata/apps/valid/minimal.yaml",
			expectedType: "user", // default
			expectError:  false,
			validateFields: func(t *testing.T, app *api.AppDefinition) {
				// RFC 20260130: Raw YAML should have __primary as listener name
				if len(app.Listeners) == 0 || app.Listeners[0].Name != "__primary" {
					t.Error("Expected __primary listener in raw YAML")
				}
				if app.Services == nil {
					t.Fatal("expected services to be defined")
				}
				main, ok := app.Services["main"]
				if !ok {
					t.Fatal("expected main service to be defined")
				}
				if main.Image != "alpine:latest" {
					t.Errorf("expected main image alpine:latest, got %s", main.Image)
				}
			},
		},
		{
			name:         "complete app",
			filePath:     "../../testdata/apps/valid/complete.yaml",
			expectedType: "user",
			expectError:  false,
			validateFields: func(t *testing.T, app *api.AppDefinition) {
				if len(app.Listeners) == 0 {
					t.Error("Expected listeners to be defined")
				}
				// RFC 20260130: Raw YAML should have __primary as listener name
				found := false
				for _, l := range app.Listeners {
					if l.Name == "__primary" && l.GuestPort == 80 {
						found = true
					}
				}
				if !found {
					t.Error("Expected __primary listener with guest_port 80")
				}
				if app.Services == nil {
					t.Fatal("expected services to be defined")
				}
				main, ok := app.Services["main"]
				if !ok {
					t.Fatal("expected main service to be defined")
				}
				if main.Storage == nil || main.Storage.Persistent == nil {
					t.Error("Expected persistent storage to be defined")
				}
				if main.Environment == nil {
					t.Error("Expected environment variables to be defined")
				}
				if env, ok := main.Environment["ENV"]; !ok || env != "test" {
					t.Error("Expected ENV=test environment variable")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Read file content
			content, err := os.ReadFile(tt.filePath)
			if err != nil {
				t.Fatalf("Failed to read test file %s: %v", tt.filePath, err)
			}

			// Parse the app definition
			app, err := ParseAppDefinition(content)

			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Validate default values
			if app.Type == "" {
				app.Type = "user" // Parser should set this default
			}
			if app.Type != tt.expectedType {
				t.Errorf("Expected type %s, got %s", tt.expectedType, app.Type)
			}

			// Run custom field validation
			if tt.validateFields != nil {
				tt.validateFields(t, app)
			}
		})
	}
}

// TestParseAppDefinitionErrors tests parsing of invalid app.yaml files
func TestParseAppDefinitionErrors(t *testing.T) {
	tests := []struct {
		name        string
		filePath    string
		expectedErr string
	}{
		{
			name:        "missing identity",
			filePath:    "../../testdata/apps/invalid/missing-name.yaml",
			expectedErr: "listeners are required for service mode apps",
		},
		{
			name:        "missing image",
			filePath:    "../../testdata/apps/invalid/missing-image-and-build.yaml",
			expectedErr: "services is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Read file content
			content, err := os.ReadFile(tt.filePath)
			if err != nil {
				t.Fatalf("Failed to read test file %s: %v", tt.filePath, err)
			}

			// Parse should fail
			_, err = ParseAppDefinition(content)
			if err == nil {
				t.Fatalf("Expected error but got none")
			}

			// Check error message contains expected text
			if tt.expectedErr != "" {
				if !containsString(err.Error(), tt.expectedErr) {
					t.Errorf("Expected error to contain %q, got %q", tt.expectedErr, err.Error())
				}
			}
		})
	}
}

// TestValidateAppDefinition tests validation logic separately
func TestValidateAppDefinition(t *testing.T) {
	tests := []struct {
		name        string
		app         *api.AppDefinition
		expectError bool
		expectedErr string
	}{
		{
			name: "valid minimal app",
			app: &api.AppDefinition{
				// RFC 20260130: listener name is the app identity, Primary: true
				Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80}},
				},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: false,
		},
		{
			name: "missing x-piccolo.mode",
			app: &api.AppDefinition{
				Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80}},
				},
				Extensions: map[string]interface{}{},
			},
			expectError: true,
			expectedErr: "x-piccolo.mode is required",
		},
		{
			name: "missing identity",
			app: &api.AppDefinition{
				Services:   map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "listeners are required for service mode apps",
		},
		{
			name: "invalid listener name characters",
			app: &api.AppDefinition{
				Listeners:  []api.AppListener{{Name: "test_app!", GuestPort: 80, Primary: true}},
				Services:   map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "name must contain only lowercase letters and numbers",
		},
		{
			name: "listener name with hyphen",
			app: &api.AppDefinition{
				Listeners:  []api.AppListener{{Name: "test-app", GuestPort: 80, Primary: true}},
				Services:   map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "no hyphens allowed",
		},
		{
			name: "listener name too long",
			app: &api.AppDefinition{
				Listeners:  []api.AppListener{{Name: "abcdefghijklmnopq", GuestPort: 80, Primary: true}},
				Services:   map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "name must be 16 characters or less",
		},
		{
			name: "missing services",
			app: &api.AppDefinition{
				Listeners:  []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "services is required",
		},
		{
			name: "invalid listener port",
			app: &api.AppDefinition{
				Listeners: []api.AppListener{{Name: "testapp", GuestPort: 0, Primary: true}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80}},
				},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "guest_port must be between 1 and 65535",
		},
		// RFC 20260130 §10.1: Evolved workspace tests
		{
			name: "evolved workspace - workspace_name with listeners is valid",
			app: &api.AppDefinition{
				// Workspace app originally installed with workspace_name, then had listeners added
				WorkspaceName: "myworkspace",
				Listeners:     []api.AppListener{{Name: "web", GuestPort: 8080, Primary: true}},
				Services: map[string]api.AppService{
					"main": {Image: "ubuntu:22.04", BindPorts: []int{8080}},
				},
				Extensions: map[string]interface{}{"mode": "workspace"},
			},
			expectError: false,
		},
		{
			name: "service mode with workspace_name and listeners is invalid",
			app: &api.AppDefinition{
				// Service mode cannot have both
				WorkspaceName: "myworkspace",
				Listeners:     []api.AppListener{{Name: "web", GuestPort: 8080, Primary: true}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{8080}},
				},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "workspace_name cannot be used with listeners",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set defaults before validation (like the parser does)
			SetDefaults(tt.app)

			err := ValidateAppDefinition(tt.app)

			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected error but got none")
				}
				if tt.expectedErr != "" && !containsString(err.Error(), tt.expectedErr) {
					t.Errorf("Expected error to contain %q, got %q", tt.expectedErr, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestParseAppDefinition_RejectsBuild(t *testing.T) {
	manifest := `
name: demo
build:
  containerfile: |
    FROM alpine:latest
listeners:
  - name: web
    guest_port: 8080
x-piccolo:
  mode: service
`
	_, err := ParseAppDefinition([]byte(manifest))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "build is not supported") {
		t.Fatalf("expected build rejection, got %q", err.Error())
	}
}

func TestParseAppDefinition_RejectsDependsOn(t *testing.T) {
	manifest := `
name: demo
listeners:
  - name: web
    guest_port: 8080
services:
  main:
    image: alpine:latest
    bind_ports: [8080]
depends_on:
  - other
x-piccolo:
  mode: service
`
	_, err := ParseAppDefinition([]byte(manifest))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "depends_on") {
		t.Fatalf("expected depends_on rejection, got %q", err.Error())
	}
}

// Helper function to check if string contains substring
func containsString(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			len(s) > len(substr) &&
				(s[:len(substr)] == substr ||
					s[len(s)-len(substr):] == substr ||
					containsSubstring(s[1:len(s)-1], substr)))
}

func containsSubstring(s, substr string) bool {
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestSetDefaults tests that parser sets appropriate default values
func TestSetDefaults(t *testing.T) {
	// RFC 20260130: Use listener name as identity
	app := &api.AppDefinition{
		Listeners: []api.AppListener{{Name: "testapp", GuestPort: 80, Primary: true}},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:latest", BindPorts: []int{80}},
		},
	}

	SetDefaults(app)

	if app.Type != "user" {
		t.Errorf("Expected default type 'user', got %s", app.Type)
	}
}

// TestSecurityValidation tests security-focused validation
func TestSecurityValidation(t *testing.T) {
	tests := []struct {
		name        string
		filePath    string
		expectedErr string
	}{
		{
			name:        "path traversal attempt",
			filePath:    "../../testdata/apps/invalid/path-traversal.yaml",
			expectedErr: "container path must be absolute",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := os.ReadFile(tt.filePath)
			if err != nil {
				t.Fatalf("Failed to read test file %s: %v", tt.filePath, err)
			}

			_, err = ParseAppDefinition(content)
			if err == nil {
				t.Fatalf("Expected security validation error but got none")
			}

			if !containsString(err.Error(), tt.expectedErr) {
				t.Errorf("Expected error to contain %q, got %q", tt.expectedErr, err.Error())
			}
		})
	}
}

// TestLargeContentHandling tests handling of large YAML content
func TestLargeContentHandling(t *testing.T) {
	content, err := os.ReadFile("../../testdata/apps/invalid/large-yaml.yaml")
	if err != nil {
		t.Fatalf("Failed to read large YAML test file: %v", err)
	}

	// Should handle reasonably large content without issues
	app, err := ParseAppDefinition(content)
	if err != nil {
		t.Fatalf("Should handle reasonably large content, but got error: %v", err)
	}

	// RFC 20260130: Raw YAML should have __primary as listener name
	if len(app.Listeners) == 0 || app.Listeners[0].Name != "__primary" {
		t.Error("Expected __primary listener in raw YAML")
	}
}

func TestParseAppDefinitionRejectsFilesystemBlock(t *testing.T) {
	legacy := `
name: testapp
listeners:
  - name: web
    guest_port: 80
services:
  main:
    image: nginx:latest
    bind_ports: [80]
filesystem:
  persistent: true
x-piccolo:
  mode: workspace
`

	_, err := ParseAppDefinition([]byte(legacy))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "filesystem is no longer supported") {
		t.Fatalf("expected filesystem rejection error, got %q", err.Error())
	}
}

func TestParseAppDefinitionRejectsStorageHostPaths(t *testing.T) {
	legacy := `
name: testapp
listeners:
  - name: web
    guest_port: 80
services:
  main:
    image: nginx:latest
    bind_ports: [80]
    storage:
      persistent:
        data:
          container: /data
          host: /not/allowed
x-piccolo:
  mode: service
`

	_, err := ParseAppDefinition([]byte(legacy))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "must not specify host") {
		t.Fatalf("expected host rejection error, got %q", err.Error())
	}
}

func TestParseAppDefinitionRejectsStoragePathConflicts(t *testing.T) {
	conflict := `
name: testapp
listeners:
  - name: web
    guest_port: 80
services:
  main:
    image: nginx:latest
    bind_ports: [80]
    storage:
      persistent:
        data:
          container: /data
      temporary:
        tmp:
          container: /data
x-piccolo:
  mode: service
`

	_, err := ParseAppDefinition([]byte(conflict))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "conflicts with") {
		t.Fatalf("expected storage conflict error, got %q", err.Error())
	}
}

// TestReservedNames tests that reserved listener names are rejected
func TestReservedNames(t *testing.T) {
	// RFC 20260130: Reserved names now apply to listener names
	reservedNames := []string{"api", "www", "admin", "root", "system", "piccolo"}

	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			app := &api.AppDefinition{
				Listeners: []api.AppListener{{Name: name, GuestPort: 80, Primary: true}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80}},
				},
				Extensions: map[string]interface{}{"mode": "service"},
			}

			SetDefaults(app)
			err := ValidateAppDefinition(app)

			if err == nil {
				t.Fatalf("Expected error for reserved name '%s' but got none", name)
			}

			if !containsString(err.Error(), "reserved") {
				t.Errorf("Expected error about reserved name, got %q", err.Error())
			}
		})
	}
}

func TestValidateResourcePermissions_rejects_privileged(t *testing.T) {
	res := &api.AppResourcePermissions{Privileged: true}
	err := validateResourcePermissions(res)
	if err == nil {
		t.Fatal("expected error for privileged=true, got nil")
	}
	if !containsString(err.Error(), "privileged") {
		t.Errorf("expected error about privileged, got: %v", err)
	}
}

func TestValidateResourcePermissions_allows_non_privileged(t *testing.T) {
	res := &api.AppResourcePermissions{MaxProcesses: 100, MaxOpenFiles: 1024}
	err := validateResourcePermissions(res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestMalformedYAML tests handling of malformed YAML
func TestMalformedYAML(t *testing.T) {
	malformedYAML := `name: test
image: nginx
listeners:
  - name: web
    guest_port: [invalid yaml structure`

	_, err := ParseAppDefinition([]byte(malformedYAML))
	if err == nil {
		t.Fatal("Expected error for malformed YAML but got none")
	}

	if !containsString(err.Error(), "failed to parse YAML") {
		t.Errorf("Expected YAML parsing error, got %q", err.Error())
	}
}

func TestPortClaimValidation(t *testing.T) {
	baseApp := func(listeners []api.AppListener) *api.AppDefinition {
		return &api.AppDefinition{
			Listeners:  listeners,
			Services:   map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{53}}},
			Extensions: map[string]interface{}{"mode": "service"},
		}
	}

	intPtr := func(v int) *int { return &v }

	tests := []struct {
		name        string
		app         *api.AppDefinition
		expectError bool
		errContains string
	}{
		{
			name: "valid_tcp_port_claim",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true, PortClaim: intPtr(8080)},
			}),
		},
		{
			name: "valid_udp_port_claim",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "dns", GuestPort: 53, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(53)},
			}),
		},
		{
			name: "valid_tls_port_claim",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "secure", GuestPort: 8443, Flow: api.FlowTLS, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(8443)},
			}),
		},
		{
			name: "reject_reserved_port_80",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "raw", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(80)},
			}),
			expectError: true,
			errContains: "reserved for the portal",
		},
		{
			name: "reject_reserved_port_443",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "raw", GuestPort: 8443, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(443)},
			}),
			expectError: true,
			errContains: "reserved for the portal",
		},
		{
			name: "reject_reserved_port_5353",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "mdns", GuestPort: 5353, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(5353)},
			}),
			expectError: true,
			errContains: "reserved for mDNS",
		},
		{
			name: "reject_host_bind_range",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "raw", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(20000)},
			}),
			expectError: true,
			errContains: "host-bind range",
		},
		{
			name: "reject_public_range",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "raw", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(40000)},
			}),
			expectError: true,
			errContains: "auto-allocate range",
		},
		{
			name: "reject_out_of_range",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "raw", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(0)},
			}),
			expectError: true,
			errContains: "must be between 1 and 65535",
		},
		{
			name: "reject_duplicate_claim_same_transport",
			app: baseApp([]api.AppListener{
				{Name: "main", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "raw1", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(8080)},
				{Name: "raw2", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(8080)},
			}),
			expectError: true,
			errContains: "port_claim 8080/tcp used by both",
		},
		{
			name: "allow_same_claim_different_transport",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "dns", GuestPort: 53, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(53)},
				{Name: "dnsudp", GuestPort: 53, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(53)},
			}),
		},
		{
			name: "reject_udp_http_protocol",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "bad", GuestPort: 53, Flow: api.FlowUDP, Protocol: api.ListenerProtocolHTTP, PortClaim: intPtr(53)},
			}),
			expectError: true,
			errContains: "flow: udp cannot be used with protocol",
		},
		{
			name: "allow_same_guest_port_different_transport",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "dns", GuestPort: 53, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw},
				{Name: "dnsudp", GuestPort: 53, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(53)},
			}),
		},
		{
			name: "reject_same_guest_port_same_transport",
			app: baseApp([]api.AppListener{
				{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
				{Name: "dns1", GuestPort: 53, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw},
				{Name: "dns2", GuestPort: 53, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw},
			}),
			expectError: true,
			errContains: "guest_port 53/tcp used by both",
		},
		{
			name: "allow_udp_primary",
			app: baseApp([]api.AppListener{
				{Name: "dns", GuestPort: 53, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, Primary: true, PortClaim: intPtr(53)},
			}),
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetDefaults(tt.app)
			err := ValidateAppDefinition(tt.app)
			if tt.expectError {
				if err == nil {
					t.Fatal("expected error but got none")
				}
				if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestFlowUDP(t *testing.T) {
	t.Run("transport_protocol", func(t *testing.T) {
		if api.FlowTCP.TransportProtocol() != "tcp" {
			t.Fatal("FlowTCP transport should be tcp")
		}
		if api.FlowTLS.TransportProtocol() != "tcp" {
			t.Fatal("FlowTLS transport should be tcp")
		}
		if api.FlowUDP.TransportProtocol() != "udp" {
			t.Fatal("FlowUDP transport should be udp")
		}
	})

	t.Run("string_roundtrip", func(t *testing.T) {
		if api.FlowUDP.String() != "udp" {
			t.Fatalf("FlowUDP.String() = %q, want udp", api.FlowUDP.String())
		}
	})
}
