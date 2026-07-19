package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
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

func TestBuildCreateArgs_cap_drop_and_cap_add(t *testing.T) {
	spec := ContainerCreateSpec{
		Name:  "testapp",
		Image: "alpine:latest",
	}
	args := buildCreateArgs(spec)

	// Verify --cap-drop=ALL is present
	found := false
	for _, arg := range args {
		if arg == "--cap-drop=ALL" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected --cap-drop=ALL in args")
	}

	// Verify exactly 12 --cap-add flags are present (no accidental additions)
	capAddCount := 0
	for _, arg := range args {
		if len(arg) > 10 && arg[:10] == "--cap-add=" {
			capAddCount++
		}
	}
	if capAddCount != 12 {
		t.Errorf("expected exactly 12 --cap-add flags, got %d", capAddCount)
	}

	// Verify all 12 --cap-add flags are present
	expectedCaps := []string{
		"--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE", "--cap-add=FOWNER",
		"--cap-add=FSETID", "--cap-add=SETUID", "--cap-add=SETGID",
		"--cap-add=NET_BIND_SERVICE", "--cap-add=KILL", "--cap-add=SYS_CHROOT",
		"--cap-add=SETFCAP", "--cap-add=SETPCAP", "--cap-add=AUDIT_WRITE",
	}
	for _, expected := range expectedCaps {
		found := false
		for _, arg := range args {
			if arg == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in args", expected)
		}
	}
}

func TestBuildCreateArgs_pids_limit_and_nofile(t *testing.T) {
	spec := ContainerCreateSpec{
		Name:  "testapp",
		Image: "alpine:latest",
		Resources: ResourceLimits{
			PidsLimit:   4096,
			NofileLimit: 65536,
		},
	}
	args := buildCreateArgs(spec)

	// Check --pids-limit
	foundPids := false
	for i, arg := range args {
		if arg == "--pids-limit" && i+1 < len(args) && args[i+1] == "4096" {
			foundPids = true
			break
		}
	}
	if !foundPids {
		t.Errorf("expected --pids-limit 4096 in args, got %v", args)
	}

	// Check --ulimit nofile
	foundNofile := false
	for i, arg := range args {
		if arg == "--ulimit" && i+1 < len(args) && args[i+1] == "nofile=65536:65536" {
			foundNofile = true
			break
		}
	}
	if !foundNofile {
		t.Errorf("expected --ulimit nofile=65536:65536 in args, got %v", args)
	}
}

func TestBuildCreateArgs_pids_limit_zero_omitted(t *testing.T) {
	spec := ContainerCreateSpec{
		Name:  "testapp",
		Image: "alpine:latest",
	}
	args := buildCreateArgs(spec)

	for _, arg := range args {
		if arg == "--pids-limit" {
			t.Fatal("--pids-limit should not be present when PidsLimit is 0")
		}
		if arg == "--ulimit" {
			t.Fatal("--ulimit should not be present when NofileLimit is 0")
		}
	}
}

func TestApplyRuntimeCredential_nil_credential(t *testing.T) {
	rt := PodmanRuntime{}
	cmd := exec.Command("/usr/bin/podman", "ps")
	originalPath := cmd.Path
	originalArgs := append([]string(nil), cmd.Args...)

	ApplyRuntimeCredential(cmd, rt)

	// No-op: env and SysProcAttr should remain unset
	if cmd.Env != nil {
		t.Error("expected nil Env for nil credential")
	}
	if cmd.SysProcAttr != nil {
		t.Error("expected nil SysProcAttr for nil credential")
	}
	if cmd.Path != originalPath || !reflect.DeepEqual(cmd.Args, originalArgs) {
		t.Fatalf("rootful command changed: path=%q args=%v", cmd.Path, cmd.Args)
	}
}

func TestApplyRuntimeCredential_with_credential(t *testing.T) {
	rt := PodmanRuntime{
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
		HomeDir:    "/home/testuser",
	}
	cmd := exec.Command("/usr/bin/podman", "ps", "--all")

	ApplyRuntimeCredential(cmd, rt, "TERM=xterm-256color")

	// Verify env is set
	if cmd.Env == nil {
		t.Fatal("expected non-nil Env")
	}

	envMap := make(map[string]bool)
	for _, e := range cmd.Env {
		envMap[e] = true
	}
	if !envMap["HOME=/home/testuser"] {
		t.Error("expected HOME=/home/testuser in env")
	}
	if !envMap["XDG_RUNTIME_DIR=/run/user/1000"] {
		t.Error("expected XDG_RUNTIME_DIR=/run/user/1000 in env")
	}
	if !envMap["LANG=C.UTF-8"] {
		t.Error("expected LANG=C.UTF-8 in env")
	}
	if !envMap["LC_ALL=C"] {
		t.Error("expected LC_ALL=C in env")
	}
	if !envMap["TERM=xterm-256color"] {
		t.Error("expected TERM=xterm-256color in env (extraEnv)")
	}

	// Verify SysProcAttr.Credential
	if cmd.SysProcAttr == nil {
		t.Fatal("expected non-nil SysProcAttr")
	}
	if cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected non-nil Credential in SysProcAttr")
	}
	if cmd.SysProcAttr.Credential.Uid != 1000 {
		t.Errorf("expected Uid=1000, got %d", cmd.SysProcAttr.Credential.Uid)
	}
	wantArgs := []string{"/usr/bin/choom", "-n", "0", "--", "/usr/bin/podman", "ps", "--all"}
	if cmd.Path != "/usr/bin/choom" || !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("rootless command wrapper = path %q args %v, want %q %v", cmd.Path, cmd.Args, "/usr/bin/choom", wantArgs)
	}
}

func TestPodmanExitOneMeansAbsentFailsClosedOnChoomDiagnostic(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("expected exit status 1")
	}
	rt := PodmanRuntime{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}

	for _, existsPath := range []string{"image exists", "container exists"} {
		t.Run(existsPath, func(t *testing.T) {
			if podmanExitOneMeansAbsent(rt, []byte("choom: failed to set score adjust value: permission denied\n"), err) {
				t.Fatal("choom failure was classified as Podman absence")
			}
			if !podmanExitOneMeansAbsent(rt, nil, err) {
				t.Fatal("ordinary Podman exit 1 was not classified as absence")
			}
		})
	}
}

func TestContainerPIDIsLiveRequiresMatchingLibpodCgroup(t *testing.T) {
	procRoot := t.TempDir()
	const (
		containerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		otherID     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		pid         = "1234"
	)
	pidDir := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte("0::/user.slice/user-475.slice/user@475.service/user.slice/libpod-"+otherID+".scope\n"), 0o644); err != nil {
		t.Fatalf("write mismatched cgroup: %v", err)
	}
	if live, err := containerPIDIsLive(procRoot, containerID, pid); err != nil || live {
		t.Fatalf("reused PID live=%v err=%v, want false/nil", live, err)
	}

	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte("0::/user.slice/user-475.slice/user@475.service/user.slice/libpod-"+containerID+".scope\n"), 0o644); err != nil {
		t.Fatalf("write matching cgroup: %v", err)
	}
	if live, err := containerPIDIsLive(procRoot, containerID, pid); err != nil || !live {
		t.Fatalf("matching PID live=%v err=%v, want true/nil", live, err)
	}

	if live, err := containerPIDIsLive(procRoot, containerID, "0"); err != nil || live {
		t.Fatalf("zero PID live=%v err=%v, want false/nil", live, err)
	}
}

func TestApplyRuntimeCredential_preserves_existing_sysprocattr(t *testing.T) {
	rt := PodmanRuntime{
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
		HomeDir:    "/home/testuser",
	}
	cmd := &exec.Cmd{
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}

	ApplyRuntimeCredential(cmd, rt)

	// Setsid should be preserved
	if !cmd.SysProcAttr.Setsid {
		t.Error("expected Setsid to remain true")
	}
	if cmd.SysProcAttr.Credential == nil {
		t.Error("expected Credential to be set")
	}
}

func TestApplyRuntimeCredentialPreservesCommandTransport(t *testing.T) {
	rt := PodmanRuntime{
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
		HomeDir:    "/home/testuser",
	}
	stdin := bytes.NewBufferString("input")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	extraRead, extraWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create extra file pipe: %v", err)
	}
	t.Cleanup(func() {
		extraRead.Close()
		extraWrite.Close()
	})
	terminalRead, terminalWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create terminal-style pipe: %v", err)
	}
	t.Cleanup(func() {
		terminalRead.Close()
		terminalWrite.Close()
	})

	tests := []struct {
		name   string
		stdin  io.Reader
		stdout io.Writer
		stderr io.Writer
	}{
		{
			name:   "independent streams",
			stdin:  stdin,
			stdout: stdout,
			stderr: stderr,
		},
		{
			name:   "shared terminal endpoint",
			stdin:  terminalRead,
			stdout: terminalWrite,
			stderr: terminalWrite,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "/usr/bin/podman", "exec", "cid", "sh")
			cmd.Stdin = tt.stdin
			cmd.Stdout = tt.stdout
			cmd.Stderr = tt.stderr
			cmd.ExtraFiles = []*os.File{extraRead}
			cmd.WaitDelay = 3 * time.Second
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			cancelCalled := false
			cmd.Cancel = func() error {
				cancelCalled = true
				return nil
			}

			ApplyRuntimeCredential(cmd, rt)

			if cmd.Stdin != tt.stdin || cmd.Stdout != tt.stdout || cmd.Stderr != tt.stderr {
				t.Fatal("stdin/stdout/stderr changed while applying rootless credential")
			}
			if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != extraRead {
				t.Fatalf("ExtraFiles = %v, want original descriptor", cmd.ExtraFiles)
			}
			if cmd.WaitDelay != 3*time.Second {
				t.Fatalf("WaitDelay = %v, want 3s", cmd.WaitDelay)
			}
			if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
				t.Fatal("existing process-group attributes were not preserved")
			}
			if cmd.Cancel == nil {
				t.Fatal("command cancellation hook was removed")
			}
			if err := cmd.Cancel(); err != nil {
				t.Fatalf("invoke preserved cancellation hook: %v", err)
			}
			if !cancelCalled {
				t.Fatal("original cancellation hook was not preserved")
			}
			wantArgs := []string{
				"/usr/bin/choom", "-n", "0", "--",
				"/usr/bin/podman", "exec", "cid", "sh",
			}
			if cmd.Path != "/usr/bin/choom" || !reflect.DeepEqual(cmd.Args, wantArgs) {
				t.Fatalf("wrapped command = path %q args %v, want %q %v", cmd.Path, cmd.Args, "/usr/bin/choom", wantArgs)
			}
		})
	}
}

func TestApplyRuntimeCredentialPreservesCommandContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/usr/bin/podman", "ps")
	ApplyRuntimeCredential(cmd, PodmanRuntime{
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
		HomeDir:    "/home/testuser",
	})

	cancel()
	if err := cmd.Start(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start after context cancellation = %v, want context.Canceled", err)
	}
}

func TestPodmanCmd_with_credential(t *testing.T) {
	rt := PodmanRuntime{
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
		HomeDir:    "/home/testuser",
	}
	cmd := podmanCmd(context.Background(), rt, "images")

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected credential to be set on cmd from podmanCmd")
	}
	if cmd.SysProcAttr.Credential.Uid != 1000 {
		t.Errorf("expected Uid=1000, got %d", cmd.SysProcAttr.Credential.Uid)
	}
	if cmd.Env == nil {
		t.Error("expected Env to be set for rootless")
	}
}

func TestApplyRuntimeCredential_includes_proxy_env(t *testing.T) {
	// Set proxy env vars
	t.Setenv("HTTP_PROXY", "http://proxy:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy:8443")
	t.Setenv("NO_PROXY", "localhost")

	rt := PodmanRuntime{
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
		HomeDir:    "/home/testuser",
	}
	cmd := &exec.Cmd{}
	ApplyRuntimeCredential(cmd, rt)

	envMap := make(map[string]bool)
	for _, e := range cmd.Env {
		envMap[e] = true
	}
	if !envMap["HTTP_PROXY=http://proxy:8080"] {
		t.Error("expected HTTP_PROXY in env")
	}
	if !envMap["HTTPS_PROXY=http://proxy:8443"] {
		t.Error("expected HTTPS_PROXY in env")
	}
	if !envMap["NO_PROXY=localhost"] {
		t.Error("expected NO_PROXY in env")
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
		BytesCurrent: 50 * 1024 * 1024,  // 50 MiB
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
