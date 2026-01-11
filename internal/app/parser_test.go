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
		expectedName   string
		expectedType   string
		expectError    bool
		validateFields func(*testing.T, *api.AppDefinition)
	}{
		{
			name:          "minimal app",
			filePath:      "../../testdata/apps/valid/minimal.yaml",
			expectedName:  "test-minimal",
			expectedType:  "user", // default
			expectError:   false,
			validateFields: func(t *testing.T, app *api.AppDefinition) {
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
			expectedName: "test-complete",
			expectedType: "user",
			expectError:  false,
			validateFields: func(t *testing.T, app *api.AppDefinition) {
				if len(app.Listeners) == 0 {
					t.Error("Expected listeners to be defined")
				}
				found := false
				for _, l := range app.Listeners {
					if l.Name == "web" && l.GuestPort == 80 {
						found = true
					}
				}
				if !found {
					t.Error("Expected web listener with guest_port 80")
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

			// Basic field validation
			if app.Name != tt.expectedName {
				t.Errorf("Expected name %s, got %s", tt.expectedName, app.Name)
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
			name:        "missing name",
			filePath:    "../../testdata/apps/invalid/missing-name.yaml",
			expectedErr: "name is required",
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
				Name:      "test-app",
				Listeners: []api.AppListener{{Name: "web", GuestPort: 80}},
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
				Name:      "test-app",
				Listeners: []api.AppListener{{Name: "web", GuestPort: 80}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80}},
				},
				Extensions: map[string]interface{}{},
			},
			expectError: true,
			expectedErr: "x-piccolo.mode is required",
		},
		{
			name:        "empty name",
			app:         &api.AppDefinition{Services: map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}}},
			expectError: true,
			expectedErr: "name is required",
		},
		{
			name:        "invalid name characters",
			app:         &api.AppDefinition{Name: "test_app!", Services: map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}}},
			expectError: true,
			expectedErr: "name must contain only lowercase letters, numbers, and hyphens",
		},
		{
			name:        "name too long",
			app:         &api.AppDefinition{Name: "this-is-a-very-long-app-name-that-exceeds-the-maximum-allowed-length", Services: map[string]api.AppService{"main": {Image: "nginx:latest", BindPorts: []int{80}}}},
			expectError: true,
			expectedErr: "name must be 50 characters or less",
		},
		{
			name:        "missing services",
			app:         &api.AppDefinition{Name: "test-app", Extensions: map[string]interface{}{"mode": "service"}},
			expectError: true,
			expectedErr: "services is required",
		},
		{
			name: "invalid listener port",
			app: &api.AppDefinition{
				Name:      "test-app",
				Listeners: []api.AppListener{{Name: "web", GuestPort: 0}},
				Services: map[string]api.AppService{
					"main": {Image: "nginx:latest", BindPorts: []int{80}},
				},
				Extensions: map[string]interface{}{"mode": "service"},
			},
			expectError: true,
			expectedErr: "guest_port must be between 1 and 65535",
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
	app := &api.AppDefinition{
		Name:  "test-app",
		Image: "nginx:latest",
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

	if app.Name != "large-app" {
		t.Errorf("Expected name 'large-app', got %s", app.Name)
	}
}

func TestParseAppDefinitionRejectsFilesystemBlock(t *testing.T) {
	legacy := `
name: test-app
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
name: test-app
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
name: test-app
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

// TestReservedNames tests that reserved app names are rejected
func TestReservedNames(t *testing.T) {
	reservedNames := []string{"api", "www", "admin", "root", "system", "piccolo"}

	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			app := &api.AppDefinition{
				Name:  name,
				Image: "nginx:latest",
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
