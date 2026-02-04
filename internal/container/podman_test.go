package container

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestValidateContainerName tests container name validation
func TestValidateContainerName(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectError bool
	}{
		{"valid simple name", "nginx", false},
		{"valid name with hyphen", "my-app", false},
		{"valid registry image", "docker.io/nginx:latest", false},
		{"valid digest reference", "docker.io/library/ubuntu@sha256:45b23dee08af5e43a7fea6c4cf9c25ccf269ee113168c19722f87876677c5cb2", false},
		{"valid with underscore", "my_app", false},
		{"valid trailing underscore", "demo__netns__", false},
		{"empty name", "", true},
		{"too long", string(make([]byte, 256)), true},
		{"invalid chars", "app!@#", true},
		{"starts with hyphen", "-app", true},
		{"ends with hyphen", "app-", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateContainerName(tt.input)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for input %q but got none", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for input %q: %v", tt.input, err)
				}
			}
		})
	}
}

// TestValidatePort tests port validation
func TestValidatePort(t *testing.T) {
	tests := []struct {
		name        string
		port        int
		expectError bool
	}{
		{"valid port 80", 80, false},
		{"valid port 8080", 8080, false},
		{"valid port 65535", 65535, false},
		{"port too low", 0, true},
		{"port too high", 65536, true},
		{"negative port", -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePort(tt.port)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for port %d but got none", tt.port)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for port %d: %v", tt.port, err)
				}
			}
		})
	}
}

// TestValidatePath tests path validation
func TestValidatePath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		expectError bool
	}{
		{"valid absolute path", "/data", false},
		{"valid nested path", "/var/lib/data", false},
		{"relative path", "data", true},
		{"empty path", "", true},
		{"path traversal", "/data/../etc/passwd", true},
		{"invalid chars", "/data$pecial", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePath(tt.path)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for path %q but got none", tt.path)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for path %q: %v", tt.path, err)
				}
			}
		})
	}
}

// TestValidateEnvKey tests environment key validation
func TestValidateEnvKey(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		expectError bool
	}{
		{"valid key", "NODE_ENV", false},
		{"valid underscore", "_DEBUG", false},
		{"starts with letter", "DEBUG", false},
		{"empty key", "", true},
		{"starts with number", "123KEY", true},
		{"invalid chars", "KEY-NAME", true},
		{"too long", string(make([]byte, 256)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnvKey(tt.key)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for key %q but got none", tt.key)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for key %q: %v", tt.key, err)
				}
			}
		})
	}
}

// TestValidateEnvValue tests environment value validation
func TestValidateEnvValue(t *testing.T) {
	tests := []struct {
		name        string
		value       string
		expectError bool
	}{
		{"valid value", "production", false},
		{"empty value", "", false},
		{"value with spaces", "hello world", false},
		{"control character", "hello\x00world", true},
		{"too long", string(make([]byte, 4097)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnvValue(tt.value)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for value %q but got none", tt.value)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for value %q: %v", tt.value, err)
				}
			}
		})
	}
}

// TestValidateContainerSpec tests full spec validation
func TestValidateContainerSpec(t *testing.T) {
	tests := []struct {
		name          string
		spec          ContainerCreateSpec
		expectError   bool
		errorContains string
	}{
		{
			name: "valid spec",
			spec: ContainerCreateSpec{
				Name:        "test-app",
				Image:       "nginx:latest",
				Ports:       []PortMapping{{Host: 8080, Container: 80}},
				Environment: map[string]string{"NODE_ENV": "production"},
			},
			expectError: false,
		},
		{
			name: "invalid container name",
			spec: ContainerCreateSpec{
				Name:  "invalid!name",
				Image: "nginx:latest",
			},
			expectError:   true,
			errorContains: "invalid container name",
		},
		{
			name: "invalid port",
			spec: ContainerCreateSpec{
				Name:  "test-app",
				Image: "nginx:latest",
				Ports: []PortMapping{{Host: 0, Container: 80}},
			},
			expectError:   true,
			errorContains: "invalid host port",
		},
		{
			name: "invalid volume path",
			spec: ContainerCreateSpec{
				Name:    "test-app",
				Image:   "nginx:latest",
				Volumes: []VolumeMapping{{Host: "relative/path", Container: "/data"}},
			},
			expectError:   true,
			errorContains: "invalid host path",
		},
		{
			name: "invalid env key",
			spec: ContainerCreateSpec{
				Name:        "test-app",
				Image:       "nginx:latest",
				Environment: map[string]string{"123KEY": "value"},
			},
			expectError:   true,
			errorContains: "invalid environment key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateContainerSpec(tt.spec)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorContains != "" && !containsString(err.Error(), tt.errorContains) {
					t.Errorf("Expected error to contain %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestBuildCreateArgsDoesNotIncludeReplaceForNamedContainers(t *testing.T) {
	spec := ContainerCreateSpec{
		Name:  "nginxdemo",
		Image: "docker.io/library/nginx:alpine",
	}
	args := buildCreateArgs(spec)
	for _, arg := range args {
		if arg == "--replace" {
			t.Fatalf("did not expect --replace flag in args, got %v", args)
		}
	}
}

// TestPodmanCLI_CreateContainer tests container creation (requires Podman)
func TestPodmanCLI_CreateContainer(t *testing.T) {
	// Skip if running in CI without Podman
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("PICCOLO_TEST_PODMAN") != "1" {
		t.Skip("set PICCOLO_TEST_PODMAN=1 to run podman integration test")
	}

	podman := &PodmanCLI{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	root, err := os.MkdirTemp("", "podman-root-")
	if err != nil {
		t.Fatalf("mkdtemp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	runRoot, err := os.MkdirTemp("", "podman-runroot-")
	if err != nil {
		t.Fatalf("mkdtemp runroot: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })

	runtime := PodmanRuntime{Root: root, RunRoot: runRoot}

	// Use unique name with timestamp to avoid conflicts
	uniqueName := fmt.Sprintf("test-container-%d", time.Now().UnixNano())

	spec := ContainerCreateSpec{
		Name:  uniqueName,
		Image: "alpine:latest",
		Environment: map[string]string{
			"TEST_ENV": "integration-test",
		},
	}

	// Validate spec first
	if err := ValidateContainerSpec(spec); err != nil {
		t.Fatalf("Spec validation failed: %v", err)
	}

	// Create container
	containerID, err := podman.CreateContainer(ctx, runtime, spec)
	if err != nil {
		// If podman is not available, skip the test
		if containsString(err.Error(), "executable file not found") {
			t.Skip("Podman not available, skipping integration test")
		}
		t.Fatalf("Failed to create container: %v", err)
	}

	// Cleanup: remove container
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()

		// Stop container first
		_ = podman.StopContainer(cleanupCtx, runtime, containerID)

		// Remove container
		if err := podman.RemoveContainer(cleanupCtx, runtime, containerID); err != nil {
			t.Errorf("Failed to cleanup container %s: %v", containerID, err)
		}
	}()

	// Verify container ID format
	if !isValidContainerID(containerID) {
		t.Errorf("Invalid container ID format: %s", containerID)
	}

	// Test start container
	if err := podman.StartContainer(ctx, runtime, containerID); err != nil {
		t.Errorf("Failed to start container: %v", err)
	}

	// Test stop container
	if err := podman.StopContainer(ctx, runtime, containerID); err != nil {
		t.Errorf("Failed to stop container: %v", err)
	}
}

// TestIsValidContainerID tests container ID validation
func TestIsValidContainerID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		expected bool
	}{
		{"valid full ID", "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef", true},
		{"valid short ID", "1234567890ab", true},
		{"too short", "123", false},
		{"too long", "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef1", false},
		{"invalid chars", "123456789g", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidContainerID(tt.id)
			if result != tt.expected {
				t.Errorf("isValidContainerID(%q) = %v, expected %v", tt.id, result, tt.expected)
			}
		})
	}
}

func TestIsStaleNetworkNamespaceError(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected bool
	}{
		{
			name:     "runc namespace path error",
			output:   `Error: unable to start container "abc123": runc: runc create failed: unable to create new parent process: namespace path: lstat /proc/723206/ns/net: no such file or directory: OCI runtime attempted to invoke a command that was not found`,
			expected: true,
		},
		{
			name:     "netavark named netns error",
			output:   `Error: unable to start container "abc123": netavark: open container netns: open /run/netns/netns-a6fff1bd-d334-190e-ac07-b865cb1e9f9c: IO error: No such file or directory (os error 2)`,
			expected: true,
		},
		{
			name:     "netavark with IPAM error prefix",
			output:   "time=\"2026-01-30T14:25:30+05:30\" level=error msg=\"IPAM error: failed to get ips for container\"\nError: unable to start container: netavark: open container netns: open /run/netns/netns-xxx: IO error: No such file or directory",
			expected: true,
		},
		{
			name:     "unrelated podman error - already running",
			output:   `Error: unable to start container: container is already running`,
			expected: false,
		},
		{
			name:     "port in use error",
			output:   `Error: rootlessport cannot expose privileged port 80, you can add 'net.ipv4.ip_unprivileged_port_start=80' to /etc/sysctl.conf`,
			expected: false,
		},
		{
			name:     "partial match - has namespace but not netns",
			output:   `Error: namespace mismatch`,
			expected: false,
		},
		{
			name:     "case insensitive detection - uppercase",
			output:   `NAMESPACE PATH: LSTAT /PROC/123/NS/NET: NO SUCH FILE OR DIRECTORY`,
			expected: true,
		},
		{
			name:     "image not found error",
			output:   `Error: docker.io/nonexistent/image:latest: image not known`,
			expected: false,
		},
		{
			name:     "empty output",
			output:   ``,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStaleNetworkNamespaceError(tt.output)
			if result != tt.expected {
				t.Errorf("isStaleNetworkNamespaceError() = %v, want %v for output: %s", result, tt.expected, tt.output)
			}
		})
	}
}

func TestStaleNetworkNamespaceErrorType(t *testing.T) {
	baseErr := fmt.Errorf("podman start failed: exit status 125")
	err := &StaleNetworkNamespaceError{
		ContainerID: "abc123",
		Output:      "namespace path: lstat /proc/999/ns/net: no such file or directory",
		Err:         baseErr,
	}

	// Test Error() method
	errMsg := err.Error()
	if !containsSubstring(errMsg, "abc123") {
		t.Errorf("Error message should contain container ID, got: %s", errMsg)
	}
	if !containsSubstring(errMsg, "stale network namespace") {
		t.Errorf("Error message should mention stale network namespace, got: %s", errMsg)
	}

	// Test Unwrap() method
	unwrapped := err.Unwrap()
	if unwrapped != baseErr {
		t.Errorf("Unwrap() should return the wrapped error")
	}
}

// Helper function for string containment check
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

// Tests for image pull progress parser

func TestParseBytes(t *testing.T) {
	tests := []struct {
		numStr   string
		unit     string
		expected int64
	}{
		{"10", "B", 10},
		{"10", "", 10},
		{"1.5", "KiB", 1536},
		{"1.5", "kib", 1536},
		{"15.2", "MiB", 15938355},
		{"15.2", "mib", 15938355},
		{"1.0", "GiB", 1073741824},
		{"45.3", "MiB", 47500902},
		{"0", "B", 0},
		{"invalid", "B", 0},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s%s", tt.numStr, tt.unit), func(t *testing.T) {
			result := parseBytes(tt.numStr, tt.unit)
			// Allow some tolerance for floating point
			tolerance := int64(float64(tt.expected) * 0.01)
			if tolerance < 1 {
				tolerance = 1
			}
			diff := result - tt.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > tolerance {
				t.Errorf("parseBytes(%q, %q) = %d, expected %d (tolerance %d)", tt.numStr, tt.unit, result, tt.expected, tolerance)
			}
		})
	}
}

func TestPullProgressParser_ParseLine(t *testing.T) {
	parser := newPullProgressParser("docker.io/library/nginx:latest")

	tests := []struct {
		name           string
		line           string
		expectedLayers int
		checkFunc      func(t *testing.T, parser *pullProgressParser)
	}{
		{
			name:           "progress with bracket format",
			line:           "Copying blob sha256:abc123 [======>    ] 15.2MiB / 45.3MiB",
			expectedLayers: 1,
			checkFunc: func(t *testing.T, p *pullProgressParser) {
				layer, ok := p.layers["sha256:abc123"]
				if !ok {
					t.Fatal("layer not found")
				}
				if layer.Status != "downloading" {
					t.Errorf("status = %q, want 'downloading'", layer.Status)
				}
				if layer.BytesTotal == 0 {
					t.Error("BytesTotal should not be 0")
				}
			},
		},
		{
			name:           "progress without bracket format",
			line:           "Copying blob sha256:def456 1.2KiB / 5.6KiB",
			expectedLayers: 2,
			checkFunc: func(t *testing.T, p *pullProgressParser) {
				layer, ok := p.layers["sha256:def456"]
				if !ok {
					t.Fatal("layer not found")
				}
				if layer.Status != "downloading" {
					t.Errorf("status = %q, want 'downloading'", layer.Status)
				}
			},
		},
		{
			name:           "layer done",
			line:           "Copying blob sha256:abc123 done",
			expectedLayers: 2,
			checkFunc: func(t *testing.T, p *pullProgressParser) {
				layer, ok := p.layers["sha256:abc123"]
				if !ok {
					t.Fatal("layer not found")
				}
				if layer.Status != "done" {
					t.Errorf("status = %q, want 'done'", layer.Status)
				}
			},
		},
		{
			name:           "layer already exists",
			line:           "Copying blob sha256:exists123 skipped: already exists",
			expectedLayers: 3,
			checkFunc: func(t *testing.T, p *pullProgressParser) {
				layer, ok := p.layers["sha256:exists123"]
				if !ok {
					t.Fatal("layer not found")
				}
				if layer.Status != "exists" {
					t.Errorf("status = %q, want 'exists'", layer.Status)
				}
			},
		},
		{
			name:           "line with ANSI codes",
			line:           "\x1b[32mCopying blob sha256:ansi123 done\x1b[0m",
			expectedLayers: 4,
			checkFunc: func(t *testing.T, p *pullProgressParser) {
				layer, ok := p.layers["sha256:ansi123"]
				if !ok {
					t.Fatal("layer not found")
				}
				if layer.Status != "done" {
					t.Errorf("status = %q, want 'done'", layer.Status)
				}
			},
		},
		{
			name:           "unrelated line",
			line:           "Getting image source signatures",
			expectedLayers: 4,
			checkFunc:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser.parseLine(tt.line)
			if len(parser.layers) != tt.expectedLayers {
				t.Errorf("layer count = %d, want %d", len(parser.layers), tt.expectedLayers)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, parser)
			}
		})
	}
}

func TestPullProgressParser_Report(t *testing.T) {
	parser := newPullProgressParser("test-image")

	// Add some layers
	parser.layers["sha256:layer1"] = &ImagePullProgress{
		LayerID:      "sha256:layer1",
		Status:       "downloading",
		BytesCurrent: 50 * 1024 * 1024, // 50 MiB
		BytesTotal:   100 * 1024 * 1024, // 100 MiB
	}
	parser.layers["sha256:layer2"] = &ImagePullProgress{
		LayerID:      "sha256:layer2",
		Status:       "done",
		BytesCurrent: 30 * 1024 * 1024,
		BytesTotal:   30 * 1024 * 1024,
	}

	report := parser.report()

	if report.Image != "test-image" {
		t.Errorf("Image = %q, want 'test-image'", report.Image)
	}
	if len(report.Layers) != 2 {
		t.Errorf("Layers count = %d, want 2", len(report.Layers))
	}
	if report.TotalBytes != 130*1024*1024 {
		t.Errorf("TotalBytes = %d, want %d", report.TotalBytes, 130*1024*1024)
	}
	if report.DownloadedBytes != 80*1024*1024 {
		t.Errorf("DownloadedBytes = %d, want %d", report.DownloadedBytes, 80*1024*1024)
	}
	// 80/130 ≈ 61%
	expectedPercent := 61
	if report.OverallPercent < expectedPercent-2 || report.OverallPercent > expectedPercent+2 {
		t.Errorf("OverallPercent = %d, want ~%d", report.OverallPercent, expectedPercent)
	}
	if report.Phase != "pulling" {
		t.Errorf("Phase = %q, want 'pulling'", report.Phase)
	}
}

func TestPullProgressParser_Report_AllDone(t *testing.T) {
	parser := newPullProgressParser("test-image")

	parser.layers["sha256:layer1"] = &ImagePullProgress{
		LayerID: "sha256:layer1",
		Status:  "done",
	}
	parser.layers["sha256:layer2"] = &ImagePullProgress{
		LayerID: "sha256:layer2",
		Status:  "exists",
	}

	report := parser.report()

	if report.Phase != "complete" {
		t.Errorf("Phase = %q, want 'complete'", report.Phase)
	}
	if report.OverallPercent != 100 {
		t.Errorf("OverallPercent = %d, want 100", report.OverallPercent)
	}
}

func TestPullProgressParser_ShouldCallback(t *testing.T) {
	parser := newPullProgressParser("test-image")
	parser.minRate = 50 * time.Millisecond

	// First call should always allow
	if !parser.shouldCallback() {
		t.Error("First shouldCallback() should return true")
	}

	// Immediate second call should be throttled
	if parser.shouldCallback() {
		t.Error("Immediate second shouldCallback() should return false")
	}

	// Wait for throttle window
	time.Sleep(60 * time.Millisecond)

	// Should now allow
	if !parser.shouldCallback() {
		t.Error("shouldCallback() after waiting should return true")
	}
}
