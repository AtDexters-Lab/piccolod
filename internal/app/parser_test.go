package app

import (
	"os"
	"strings"
	"testing"

	"piccolod/internal/api"

	"gopkg.in/yaml.v3"
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
	reservedNames := []string{"api", "www", "admin", "root", "system", "piccolo", "piccoloos"}

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

// TestReservedNames_nonprimary_listener_allowed tests that non-primary listeners
// with reserved names pass validation (their derived hostname <listener>-<app> cannot collide).
func TestReservedNames_nonprimary_listener_allowed(t *testing.T) {
	reservedNames := []string{"api", "www", "admin", "root", "system", "piccolo", "piccoloos"}

	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			app := &api.AppDefinition{
				Listeners: []api.AppListener{
					{Name: "web", GuestPort: 80, Primary: true},
					{Name: name, GuestPort: 8080},
				},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80, 8080}},
				},
				Extensions: map[string]interface{}{"mode": "service"},
			}

			SetDefaults(app)
			err := ValidateAppDefinition(app)

			if err != nil {
				t.Fatalf("Non-primary listener with reserved name '%s' should be allowed, got: %v", name, err)
			}
		})
	}
}

// TestValidateListeners_nonprimary_invalid_format verifies that format validation
// still applies to non-primary listeners (only reserved name check is relaxed).
func TestValidateListeners_nonprimary_invalid_format(t *testing.T) {
	app := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "web", GuestPort: 80, Primary: true},
			{Name: "my-listener", GuestPort: 8080}, // Invalid: contains hyphen
		},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:latest", BindPorts: []int{80, 8080}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	SetDefaults(app)
	err := ValidateAppDefinition(app)

	if err == nil {
		t.Fatal("expected error for invalid format on non-primary listener, got nil")
	}
	if !strings.Contains(err.Error(), "no hyphens allowed") {
		t.Errorf("expected error about invalid format (no hyphens), got: %v", err)
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
				{Name: "web", GuestPort: 9090, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true, PortClaim: intPtr(9090)},
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
				{Name: "raw1", GuestPort: 9090, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(9090)},
				{Name: "raw2", GuestPort: 9091, Flow: api.FlowTCP, Protocol: api.ListenerProtocolRaw, PortClaim: intPtr(9090)},
			}),
			expectError: true,
			errContains: "port_claim 9090/tcp used by both",
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

func TestParseAppSchema_UnquotedTemplateExpressions(t *testing.T) {
	// Unquoted {{ }} in YAML is flow mapping syntax. ParseAppSchema must
	// handle this for raw template manifests where non-string types (booleans,
	// numbers) can't be quoted without changing the rendered YAML type.
	manifest := `
type: system
inputs:
  enabled:
    type: boolean
    label: "Enabled"
    default: true
  port:
    type: number
    label: "Port"
    default: 8080
services:
  main:
    image: example:latest
    bind_ports: [8080]
listeners:
  - name: __primary
    guest_port: 8080
app_config:
  enabled: {{ .Inputs.enabled }}
  port: {{ .Inputs.port }}
  mixed: "prefix-{{ .Inputs.enabled }}-suffix"
x-piccolo:
  mode: service
`
	def, err := ParseAppSchema([]byte(manifest))
	if err != nil {
		t.Fatalf("ParseAppSchema should handle unquoted template expressions: %v", err)
	}
	if def.Type != "system" {
		t.Errorf("expected type 'system', got %q", def.Type)
	}

	// Verify app_config preserves original template text after round-trip
	cfg, ok := def.AppConfig.(map[string]interface{})
	if !ok {
		t.Fatalf("expected app_config to be a map, got %T", def.AppConfig)
	}
	if v, _ := cfg["enabled"].(string); v != "{{ .Inputs.enabled }}" {
		t.Errorf("expected app_config.enabled to be '{{ .Inputs.enabled }}', got %q", v)
	}
	if v, _ := cfg["port"].(string); v != "{{ .Inputs.port }}" {
		t.Errorf("expected app_config.port to be '{{ .Inputs.port }}', got %q", v)
	}
	if v, _ := cfg["mixed"].(string); v != "prefix-{{ .Inputs.enabled }}-suffix" {
		t.Errorf("expected app_config.mixed to preserve template, got %q", v)
	}
}

func TestNeutralizeTemplates(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "unquoted_expression",
			input:  "key: {{ .Inputs.foo }}",
			expect: "key: __TPL_L__ .Inputs.foo __TPL_R__",
		},
		{
			name:   "quoted_expression",
			input:  `key: "{{ .Inputs.foo }}"`,
			expect: `key: "__TPL_L__ .Inputs.foo __TPL_R__"`,
		},
		{
			name:   "no_templates",
			input:  "key: value",
			expect: "key: value",
		},
		{
			name:   "yaml_flow_mapping_preserved",
			input:  "data: {key: {nested: val}}",
			expect: "data: {key: {nested: val}}",
		},
		{
			name:   "multiple_on_one_line",
			input:  "url: {{ .Inputs.host }}:{{ .Inputs.port }}",
			expect: "url: __TPL_L__ .Inputs.host __TPL_R__:__TPL_L__ .Inputs.port __TPL_R__",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(neutralizeTemplates([]byte(tt.input)))
			if got != tt.expect {
				t.Errorf("neutralizeTemplates(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestValidateInputs(t *testing.T) {
	t.Run("string_valid", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"name": {Type: "string"},
		}, map[string]interface{}{"name": "hello"})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("string_wrong_type", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"name": {Type: "string"},
		}, map[string]interface{}{"name": 123.0})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("string_regex_pass", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"name": {Type: "string", Validation: &api.AppInputValidation{Regex: `^[a-z]+$`}},
		}, map[string]interface{}{"name": "hello"})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("string_regex_fail", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"name": {Type: "string", Validation: &api.AppInputValidation{Regex: `^[a-z]+$`, Message: "lowercase only"}},
		}, map[string]interface{}{"name": "Hello123"})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "lowercase only") {
			t.Errorf("expected custom message, got: %s", err)
		}
	})

	t.Run("boolean_valid", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"enabled": {Type: "boolean"},
		}, map[string]interface{}{"enabled": true})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("boolean_string_true", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"enabled": {Type: "boolean"},
		}, map[string]interface{}{"enabled": "true"})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("boolean_wrong_type", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"enabled": {Type: "boolean"},
		}, map[string]interface{}{"enabled": 1.0})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("int_valid", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"port": {Type: "int"},
		}, map[string]interface{}{"port": float64(8080)})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("int_not_whole", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"port": {Type: "int"},
		}, map[string]interface{}{"port": 3.14})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("number_valid", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"ratio": {Type: "number"},
		}, map[string]interface{}{"ratio": 3.14})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("array_valid", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"domains": {Type: "array"},
		}, map[string]interface{}{"domains": []interface{}{"a.com", "b.com"}})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("array_wrong_type", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"domains": {Type: "array"},
		}, map[string]interface{}{"domains": "not-an-array"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("array_non_string_element", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"domains": {Type: "array"},
		}, map[string]interface{}{"domains": []interface{}{"ok.com", 123}})
		if err == nil {
			t.Fatal("expected error for non-string array element")
		}
	})

	t.Run("required_missing", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"name": {Type: "string", Required: true},
		}, map[string]interface{}{})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "required") {
			t.Errorf("expected 'required' in error, got: %s", err)
		}
	})

	t.Run("required_but_generated", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"secret": {Type: "string", Required: true, Generate: true},
		}, map[string]interface{}{})
		if err != nil {
			t.Fatalf("generated inputs should not require user value: %v", err)
		}
	})

	t.Run("optional_missing_ok", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"desc": {Type: "string"},
		}, map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown_type_skipped", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"custom": {Type: "futuretype"},
		}, map[string]interface{}{"custom": "anything"})
		if err != nil {
			t.Fatalf("unknown types should be skipped: %v", err)
		}
	})

	t.Run("password_type", func(t *testing.T) {
		err := ValidateInputs(map[string]api.AppInput{
			"pass": {Type: "password"},
		}, map[string]interface{}{"pass": "secret123"})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestRenderManifest_ToJSON(t *testing.T) {
	t.Run("array_input", func(t *testing.T) {
		manifest := `domains: {{ .Inputs.domain_list | toJSON }}`
		inputs := map[string]interface{}{
			"domain_list": []interface{}{".example.com", ".test.org"},
		}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `[".example.com",".test.org"]`) {
			t.Errorf("expected JSON array in output, got:\n%s", rendered)
		}
	})

	t.Run("empty_array", func(t *testing.T) {
		manifest := `domains: {{ .Inputs.list | toJSON }}`
		inputs := map[string]interface{}{"list": []interface{}{}}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), "domains: []") {
			t.Errorf("expected empty JSON array, got: %s", rendered)
		}
	})

	t.Run("scalar_passthrough", func(t *testing.T) {
		manifest := `flag: {{ .Inputs.flag | toJSON }}`
		inputs := map[string]interface{}{"flag": true}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), "flag: true") {
			t.Errorf("expected boolean, got: %s", rendered)
		}
	})

	t.Run("mixed_array_and_scalar", func(t *testing.T) {
		manifest := `
app_config:
  name: "{{ .Inputs.app_name }}"
  suffixes: {{ .Inputs.suffixes | toJSON }}
`
		inputs := map[string]interface{}{
			"app_name": "myapp",
			"suffixes": []interface{}{".a.com", ".b.com"},
		}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		output := string(rendered)
		if !strings.Contains(output, `name: "myapp"`) {
			t.Errorf("expected scalar preserved, got:\n%s", output)
		}
		if !strings.Contains(output, `[".a.com",".b.com"]`) {
			t.Errorf("expected JSON array, got:\n%s", output)
		}
	})

	t.Run("round_trip_yaml_parse", func(t *testing.T) {
		manifest := `domains: {{ .Inputs.list | toJSON }}`
		inputs := map[string]interface{}{
			"list": []interface{}{"alpha", "beta"},
		}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var result map[string]interface{}
		if err := yaml.Unmarshal(rendered, &result); err != nil {
			t.Fatalf("rendered YAML should parse: %v\nrendered:\n%s", err, rendered)
		}
		domains, ok := result["domains"].([]interface{})
		if !ok {
			t.Fatalf("expected []interface{}, got %T", result["domains"])
		}
		if len(domains) != 2 || domains[0] != "alpha" || domains[1] != "beta" {
			t.Errorf("unexpected domains: %v", domains)
		}
	})

	t.Run("array_auto_renders_without_toJSON", func(t *testing.T) {
		manifest := `domains: {{ .Inputs.list }}`
		inputs := map[string]interface{}{
			"list": []interface{}{".example.com", ".test.org"},
		}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `[".example.com",".test.org"]`) {
			t.Errorf("expected auto JSON array without | toJSON, got:\n%s", rendered)
		}
		// Verify it's valid YAML
		var result map[string]interface{}
		if err := yaml.Unmarshal(rendered, &result); err != nil {
			t.Fatalf("rendered YAML should parse: %v\nrendered:\n%s", err, rendered)
		}
	})
}

func TestRenderManifest_OptionalInputBackfill(t *testing.T) {
	// Full manifest template with declared inputs and template references.
	// RenderManifest should backfill optional inputs with zero/default values
	// so {{ .Inputs.x }} doesn't panic with missingkey=error.

	t.Run("optional_string_gets_zero_value", func(t *testing.T) {
		manifest := `
inputs:
  name:
    type: string
    label: "Name"
    required: true
  subtitle:
    type: string
    label: "Subtitle"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
result: "{{ .Inputs.subtitle }}"
`
		inputs := map[string]interface{}{"name": "myapp"}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `result: ""`) {
			t.Errorf("expected empty string for optional input, got:\n%s", rendered)
		}
	})

	t.Run("optional_boolean_gets_false", func(t *testing.T) {
		manifest := `
inputs:
  debug:
    type: boolean
    label: "Debug"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
debug: {{ .Inputs.debug }}
`
		rendered, err := RenderManifest([]byte(manifest), nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), "debug: false") {
			t.Errorf("expected false for optional boolean, got:\n%s", rendered)
		}
	})

	t.Run("optional_int_gets_zero", func(t *testing.T) {
		manifest := `
inputs:
  retries:
    type: int
    label: "Retries"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
retries: {{ .Inputs.retries }}
`
		rendered, err := RenderManifest([]byte(manifest), nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), "retries: 0") {
			t.Errorf("expected 0 for optional int, got:\n%s", rendered)
		}
	})

	t.Run("optional_array_gets_empty_array", func(t *testing.T) {
		manifest := `
inputs:
  nameservers:
    type: array
    label: "Nameservers"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
nameservers: {{ .Inputs.nameservers }}
`
		rendered, err := RenderManifest([]byte(manifest), nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), "nameservers: []") {
			t.Errorf("expected empty array for optional array, got:\n%s", rendered)
		}
	})

	t.Run("optional_with_default_uses_default", func(t *testing.T) {
		manifest := `
inputs:
  region:
    type: string
    label: "Region"
    default: "us-east-1"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
region: "{{ .Inputs.region }}"
`
		rendered, err := RenderManifest([]byte(manifest), nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `region: "us-east-1"`) {
			t.Errorf("expected default value, got:\n%s", rendered)
		}
	})

	t.Run("required_input_still_errors_when_missing", func(t *testing.T) {
		manifest := `
inputs:
  name:
    type: string
    label: "Name"
    required: true
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
name: "{{ .Inputs.name }}"
`
		_, err := RenderManifest([]byte(manifest), nil, nil)
		if err == nil {
			t.Fatal("expected error for missing required input")
		}
	})

	t.Run("optional_password_gets_empty_string", func(t *testing.T) {
		manifest := `
inputs:
  secret:
    type: password
    label: "Secret"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
secret: "{{ .Inputs.secret }}"
`
		rendered, err := RenderManifest([]byte(manifest), nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `secret: ""`) {
			t.Errorf("expected empty string for optional password, got:\n%s", rendered)
		}
	})

	t.Run("optional_array_default_renders_as_json", func(t *testing.T) {
		manifest := `
inputs:
  suffixes:
    type: array
    label: "Suffixes"
    default:
      - ".example.com"
      - ".test.org"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
suffixes: {{ .Inputs.suffixes }}
`
		rendered, err := RenderManifest([]byte(manifest), nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `[".example.com",".test.org"]`) {
			t.Errorf("expected default array as JSON, got:\n%s", rendered)
		}
	})

	t.Run("provided_optional_not_overwritten", func(t *testing.T) {
		manifest := `
inputs:
  color:
    type: string
    label: "Color"
    default: "blue"
services:
  main:
    image: test:latest
    bind_ports: [8080]
x-piccolo:
  mode: service
color: "{{ .Inputs.color }}"
`
		inputs := map[string]interface{}{"color": "red"}
		rendered, err := RenderManifest([]byte(manifest), inputs, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(rendered), `color: "red"`) {
			t.Errorf("expected user-provided value preserved, got:\n%s", rendered)
		}
	})
}
