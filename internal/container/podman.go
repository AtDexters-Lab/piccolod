package container

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ContainerNotFoundError indicates Podman could not find a container by the given reference.
type ContainerNotFoundError struct {
	Ref string
}

func (e *ContainerNotFoundError) Error() string {
	return fmt.Sprintf("container not found: %s", e.Ref)
}

// ErrContainerNotFound returns an error for when a container is not found.
func ErrContainerNotFound(containerRef string) error {
	return &ContainerNotFoundError{Ref: containerRef}
}

// PodmanCLI provides safe Podman CLI integration with injection prevention
type PodmanCLI struct{}

// PodmanRuntime configures where Podman stores persistent and runtime state.
//
// When Root is set, all container metadata, writable layers, and (by default) images
// will be stored under that directory.
//
// RunRoot configures where Podman stores runtime state (typically under /run or XDG_RUNTIME_DIR).
//
// Imagestore optionally splits image storage out of Root into a shared image store.
type PodmanRuntime struct {
	Root          string
	RunRoot       string
	Imagestore    string
	StorageDriver string
	StorageOpts   []string
}

// ContainerState captures minimal observed container state.
type ContainerState struct {
	Exists  bool
	Running bool
}

// Validation patterns for different argument types
var (
	// Container/image names: lowercase letters, numbers, hyphens, slashes, colons
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*[a-z0-9]$|^[a-z0-9]$`)

	// Volume paths: absolute paths only, no special chars
	pathPattern = regexp.MustCompile(`^/[a-zA-Z0-9._/-]*$`)

	// Resource values: numbers with units
	resourcePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[kmgtKMGT]?[bB]?$`)

	// Environment variable keys
	envKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

var portInUseRe = regexp.MustCompile(`:(\d+): bind: address already in use`)

func exitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// PortInUseError indicates Podman failed to bind the requested host port.
type PortInUseError struct {
	Port   int
	Output string
	Err    error
}

func (e *PortInUseError) Error() string {
	if e.Port > 0 {
		return fmt.Sprintf("podman port %d already in use: %v", e.Port, e.Err)
	}
	return fmt.Sprintf("podman host port already in use: %v", e.Err)
}

func (e *PortInUseError) Unwrap() error {
	return e.Err
}

// ValidateContainerName validates container/image names for security
func ValidateContainerName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("name too long (max 255 chars)")
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("name contains invalid characters: %s", name)
	}
	return nil
}

// ValidatePort validates port numbers
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", port)
	}
	return nil
}

// ValidatePath validates filesystem paths for security
func ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("path must be absolute: %s", path)
	}
	if !pathPattern.MatchString(path) {
		return fmt.Errorf("path contains invalid characters: %s", path)
	}
	// Additional security checks
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal not allowed: %s", path)
	}
	return nil
}

// ValidateResource validates resource limits (memory, CPU)
func ValidateResource(resource string) error {
	if resource == "" {
		return fmt.Errorf("resource cannot be empty")
	}
	if !resourcePattern.MatchString(resource) {
		return fmt.Errorf("invalid resource format: %s", resource)
	}
	return nil
}

// ValidateEnvKey validates environment variable keys
func ValidateEnvKey(key string) error {
	if key == "" {
		return fmt.Errorf("environment key cannot be empty")
	}
	if len(key) > 255 {
		return fmt.Errorf("environment key too long")
	}
	if !envKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid environment key format: %s", key)
	}
	return nil
}

// ValidateEnvValue validates environment variable values
func ValidateEnvValue(value string) error {
	// Environment values can contain most characters but not control characters
	if strings.ContainsAny(value, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0B\x0C\x0E\x0F\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1A\x1B\x1C\x1D\x1E\x1F\x7F") {
		return fmt.Errorf("environment value contains control characters")
	}
	if len(value) > 4096 {
		return fmt.Errorf("environment value too long (max 4096 chars)")
	}
	return nil
}

// ContainerCreateSpec defines validated parameters for container creation
type ContainerCreateSpec struct {
	Name          string
	Image         string
	Ports         []PortMapping
	Volumes       []VolumeMapping
	Tmpfs         []TmpfsMount
	Environment   map[string]string
	Resources     ResourceLimits
	NetworkMode   string
	RestartPolicy string
}

type PortMapping struct {
	Host      int
	Container int
}

type VolumeMapping struct {
	Host      string
	Container string
	Options   string // "ro", "rw", etc.
}

type TmpfsMount struct {
	Container string
	Options   string // e.g. "rw,size=1g"
}

type ResourceLimits struct {
	Memory string
	CPU    string
}

func buildRunArgs(spec ContainerCreateSpec) []string {
	args := []string{"run", "-d"}

	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}

	for _, port := range spec.Ports {
		args = append(args, "--publish",
			fmt.Sprintf("127.0.0.1:%d:%d", port.Host, port.Container))
	}

	for _, volume := range spec.Volumes {
		volumeArg := fmt.Sprintf("%s:%s", volume.Host, volume.Container)
		if volume.Options != "" {
			volumeArg += ":" + volume.Options
		}
		args = append(args, "--volume", volumeArg)
	}

	for _, mount := range spec.Tmpfs {
		tmpfsArg := mount.Container
		if mount.Options != "" {
			tmpfsArg += ":" + mount.Options
		}
		args = append(args, "--tmpfs", tmpfsArg)
	}

	if spec.Resources.Memory != "" {
		args = append(args, "--memory", spec.Resources.Memory)
	}
	if spec.Resources.CPU != "" {
		args = append(args, "--cpus", spec.Resources.CPU)
	}

	for key, value := range spec.Environment {
		args = append(args, "--env", fmt.Sprintf("%s=%s", key, value))
	}

	if spec.NetworkMode != "" {
		args = append(args, "--network", spec.NetworkMode)
	}

	if spec.RestartPolicy != "" {
		args = append(args, "--restart", spec.RestartPolicy)
	}

	if spec.Image != "" {
		args = append(args, spec.Image)
	}

	return args
}

func buildPodmanArgs(runtime PodmanRuntime, args []string) ([]string, error) {
	out := make([]string, 0, 6+len(runtime.StorageOpts)+len(args))
	if runtime.Root != "" {
		if err := ValidatePath(runtime.Root); err != nil {
			return nil, fmt.Errorf("invalid podman --root path: %w", err)
		}
		out = append(out, "--root", runtime.Root)
	}
	if runtime.RunRoot != "" {
		if err := ValidatePath(runtime.RunRoot); err != nil {
			return nil, fmt.Errorf("invalid podman --runroot path: %w", err)
		}
		out = append(out, "--runroot", runtime.RunRoot)
	}
	if runtime.Imagestore != "" {
		if err := ValidatePath(runtime.Imagestore); err != nil {
			return nil, fmt.Errorf("invalid podman --imagestore path: %w", err)
		}
		out = append(out, "--imagestore", runtime.Imagestore)
	}
	if runtime.StorageDriver != "" {
		if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(runtime.StorageDriver) {
			return nil, fmt.Errorf("invalid storage driver name")
		}
		out = append(out, "--storage-driver", runtime.StorageDriver)
	}
	for _, opt := range runtime.StorageOpts {
		// Basic validation for options (key=value or key)
		if !regexp.MustCompile(`^[a-z0-9_]+=[/a-zA-Z0-9._-]+$|^[a-z0-9_]+$`).MatchString(opt) {
			return nil, fmt.Errorf("invalid storage option: %s", opt)
		}
		out = append(out, "--storage-opt", opt)
	}
	out = append(out, args...)
	return out, nil
}

// CreateContainer creates a container using pre-validated arguments
// NameInUseError indicates the container name is already taken.
type NameInUseError struct {
	Name string
	ID   string
}

func (e *NameInUseError) Error() string {
	return fmt.Sprintf("container name %q already in use by %s", e.Name, e.ID)
}

func (p *PodmanCLI) CreateContainer(ctx context.Context, runtime PodmanRuntime, spec ContainerCreateSpec) (string, error) {
	// All inputs must be validated before calling this method

	// Execute command using exec.CommandContext (no shell interpretation)
	runArgs := buildRunArgs(spec)
	args, err := buildPodmanArgs(runtime, runArgs)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "address already in use") {
			port := 0
			if match := portInUseRe.FindStringSubmatch(outStr); len(match) == 2 {
				if parsed, perr := strconv.Atoi(match[1]); perr == nil {
					port = parsed
				}
			}
			return "", &PortInUseError{Port: port, Output: outStr, Err: fmt.Errorf("podman run failed: %w", err)}
		}
		if strings.Contains(outStr, "name is already in use") || strings.Contains(outStr, "already in use by") {
			// Try to extract the ID
			// Error: ... name "code-server" is already in use by <id>. ...
			// Simple regex to find the ID?
			// The ID is usually 64 chars hex.
			re := regexp.MustCompile(`already in use by ([a-f0-9]{12,64})`)
			if match := re.FindStringSubmatch(outStr); len(match) == 2 {
				return "", &NameInUseError{Name: spec.Name, ID: match[1]}
			}
		}
		return "", fmt.Errorf("podman run failed: %w, output: %s", err, outStr)
	}

	// Extract container ID from output - look for the actual hex container ID
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && isValidContainerID(line) {
			return line, nil
		}
	}

	return "", fmt.Errorf("could not extract valid container ID from output: %s", string(output))
}

// StartContainer starts a container by validated ID
func (p *PodmanCLI) StartContainer(ctx context.Context, runtime PodmanRuntime, containerID string) error {
	// Validate container ID format (typically hex string)
	if !isValidContainerID(containerID) {
		return fmt.Errorf("invalid container ID format: %s", containerID)
	}

	args, err := buildPodmanArgs(runtime, []string{"start", containerID})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("podman start failed: %w, output: %s", err, string(output))
	}

	return nil
}

// StopContainer stops a container by validated ID
func (p *PodmanCLI) StopContainer(ctx context.Context, runtime PodmanRuntime, containerID string) error {
	if !isValidContainerID(containerID) {
		return fmt.Errorf("invalid container ID format: %s", containerID)
	}

	args, err := buildPodmanArgs(runtime, []string{"stop", containerID})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("podman stop failed: %w, output: %s", err, string(output))
	}

	return nil
}

// RemoveContainer removes a container by validated ID
func (p *PodmanCLI) RemoveContainer(ctx context.Context, runtime PodmanRuntime, containerID string) error {
	if !isValidContainerID(containerID) {
		return fmt.Errorf("invalid container ID format: %s", containerID)
	}

	args, err := buildPodmanArgs(runtime, []string{"rm", containerID})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("podman rm failed: %w, output: %s", err, string(output))
	}

	return nil
}

// PullImage pulls an image by name
func (p *PodmanCLI) PullImage(ctx context.Context, runtime PodmanRuntime, image string) error {
	if err := ValidateContainerName(image); err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}
	args, err := buildPodmanArgs(runtime, []string{"pull", image})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman pull failed: %w, output: %s", err, string(output))
	}
	return nil
}

// Logs returns recent log lines from a container
func (p *PodmanCLI) Logs(ctx context.Context, runtime PodmanRuntime, containerID string, lines int) ([]string, error) {
	if !isValidContainerID(containerID) {
		return nil, fmt.Errorf("invalid container ID format: %s", containerID)
	}
	if lines <= 0 {
		lines = 200
	}
	args := []string{"logs", "--tail", fmt.Sprintf("%d", lines)}
	args = append(args, containerID)
	cmdArgs, err := buildPodmanArgs(runtime, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "podman", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman logs failed: %w, output: %s", err, string(output))
	}
	// Split into lines
	var linesOut []string
	for _, ln := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		linesOut = append(linesOut, ln)
	}
	return linesOut, nil
}

func (p *PodmanCLI) containerExists(ctx context.Context, runtime PodmanRuntime, containerRef string) (bool, error) {
	args, err := buildPodmanArgs(runtime, []string{"container", "exists", containerRef})
	if err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if code, ok := exitCode(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("podman container exists failed: %w, output: %s", err, string(out))
}

// ResolveContainerIDByName returns the container ID for a container with the given name.
func (p *PodmanCLI) ResolveContainerIDByName(ctx context.Context, runtime PodmanRuntime, name string) (string, error) {
	if err := ValidateContainerName(name); err != nil {
		return "", fmt.Errorf("invalid container name: %w", err)
	}
	exists, err := p.containerExists(ctx, runtime, name)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", ErrContainerNotFound(name)
	}

	args, err := buildPodmanArgs(runtime, []string{"inspect", "--format", "{{.Id}}", name})
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("podman inspect (id) failed: %w, stderr: %s", err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("podman inspect (id) failed: %w", err)
	}

	// Parse output: take the last non-empty line
	lines := strings.Split(string(out), "\n")
	var id string
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			id = trimmed
			break
		}
	}

	if id == "" || !isValidContainerID(id) {
		return "", fmt.Errorf("podman inspect returned invalid container id for %s: %q", name, id)
	}
	return id, nil
}

// InspectContainerState returns whether the container exists, and if so whether it is running.
func (p *PodmanCLI) InspectContainerState(ctx context.Context, runtime PodmanRuntime, containerID string) (ContainerState, error) {
	if !isValidContainerID(containerID) {
		return ContainerState{}, fmt.Errorf("invalid container ID format: %s", containerID)
	}
	exists, err := p.containerExists(ctx, runtime, containerID)
	if err != nil {
		return ContainerState{}, err
	}
	if !exists {
		return ContainerState{Exists: false, Running: false}, nil
	}

	args, err := buildPodmanArgs(runtime, []string{"inspect", "--format", "{{.State.Running}}", containerID})
	if err != nil {
		return ContainerState{}, err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return ContainerState{}, fmt.Errorf("podman inspect (running) failed: %w, stderr: %s", err, string(exitErr.Stderr))
		}
		return ContainerState{}, fmt.Errorf("podman inspect (running) failed: %w", err)
	}

	// Parse output: take the last non-empty line to ignore potential warnings on stdout
	lines := strings.Split(string(out), "\n")
	var result string
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			result = trimmed
			break
		}
	}

	switch result {
	case "true":
		return ContainerState{Exists: true, Running: true}, nil
	case "false":
		return ContainerState{Exists: true, Running: false}, nil
	default:
		return ContainerState{}, fmt.Errorf("podman inspect returned unexpected running state: %q", result)
	}
}

// InspectPublishedPorts returns a map of guest_port -> host_port for a container.
func (p *PodmanCLI) InspectPublishedPorts(ctx context.Context, runtime PodmanRuntime, containerID string) (map[int]int, error) {
	if containerID == "" {
		return nil, fmt.Errorf("container ID required")
	}
	args, err := buildPodmanArgs(runtime, []string{"port", containerID})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("podman port failed: %w", err)
	}

	result := make(map[int]int)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "->")
		if len(parts) != 2 {
			continue
		}
		left := strings.TrimSpace(parts[0])  // e.g. "80/tcp"
		right := strings.TrimSpace(parts[1]) // e.g. "127.0.0.1:15001"
		guestStr := strings.Split(left, "/")[0]
		guest, _ := strconv.Atoi(strings.TrimSpace(guestStr))
		hostParts := strings.Split(right, ":")
		hostStr := hostParts[len(hostParts)-1]
		host, _ := strconv.Atoi(strings.TrimSpace(hostStr))
		if guest > 0 && host > 0 {
			result[guest] = host
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdatePublishAdd adds a port publish mapping to a running container
func (p *PodmanCLI) UpdatePublishAdd(ctx context.Context, runtime PodmanRuntime, containerID string, hostBind, guestPort int) error {
	if !isValidContainerID(containerID) {
		return fmt.Errorf("invalid container ID format: %s", containerID)
	}
	if err := ValidatePort(hostBind); err != nil {
		return err
	}
	if err := ValidatePort(guestPort); err != nil {
		return err
	}
	mapping := fmt.Sprintf("127.0.0.1:%d:%d", hostBind, guestPort)
	args, err := buildPodmanArgs(runtime, []string{"container", "update", "--publish-add", mapping, containerID})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "address already in use") {
			port := hostBind
			if match := portInUseRe.FindStringSubmatch(outStr); len(match) == 2 {
				if parsed, perr := strconv.Atoi(match[1]); perr == nil {
					port = parsed
				}
			}
			return &PortInUseError{Port: port, Output: outStr, Err: fmt.Errorf("podman update --publish-add failed: %w", err)}
		}
		return fmt.Errorf("podman update --publish-add failed: %w, output: %s", err, outStr)
	}
	return nil
}

// UpdatePublishRemove removes a port publish mapping from a running container
func (p *PodmanCLI) UpdatePublishRemove(ctx context.Context, runtime PodmanRuntime, containerID string, hostBind, guestPort int) error {
	if !isValidContainerID(containerID) {
		return fmt.Errorf("invalid container ID format: %s", containerID)
	}
	if err := ValidatePort(hostBind); err != nil {
		return err
	}
	if err := ValidatePort(guestPort); err != nil {
		return err
	}
	mapping := fmt.Sprintf("127.0.0.1:%d:%d", hostBind, guestPort)
	args, err := buildPodmanArgs(runtime, []string{"container", "update", "--publish-rm", mapping, containerID})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman update --publish-rm failed: %w, output: %s", err, string(output))
	}
	return nil
}

// isValidContainerID validates container ID format
func isValidContainerID(id string) bool {
	// Container IDs are typically 64-character hex strings (may be shortened)
	if len(id) < 12 || len(id) > 64 {
		return false
	}
	// Check for hex characters only
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// ValidateContainerSpec validates all fields in a ContainerCreateSpec
func ValidateContainerSpec(spec ContainerCreateSpec) error {
	// Validate name
	if err := ValidateContainerName(spec.Name); err != nil {
		return fmt.Errorf("invalid container name: %w", err)
	}

	// Validate image
	if err := ValidateContainerName(spec.Image); err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}

	// Validate ports
	for i, port := range spec.Ports {
		if err := ValidatePort(port.Host); err != nil {
			return fmt.Errorf("invalid host port at index %d: %w", i, err)
		}
		if err := ValidatePort(port.Container); err != nil {
			return fmt.Errorf("invalid container port at index %d: %w", i, err)
		}
	}

	// Validate volumes
	for i, volume := range spec.Volumes {
		if err := ValidatePath(volume.Host); err != nil {
			return fmt.Errorf("invalid host path at index %d: %w", i, err)
		}
		if err := ValidatePath(volume.Container); err != nil {
			return fmt.Errorf("invalid container path at index %d: %w", i, err)
		}
	}

	for i, mount := range spec.Tmpfs {
		if err := ValidatePath(mount.Container); err != nil {
			return fmt.Errorf("invalid tmpfs container path at index %d: %w", i, err)
		}
	}

	// Validate environment variables
	for key, value := range spec.Environment {
		if err := ValidateEnvKey(key); err != nil {
			return fmt.Errorf("invalid environment key '%s': %w", key, err)
		}
		if err := ValidateEnvValue(value); err != nil {
			return fmt.Errorf("invalid environment value for key '%s': %w", key, err)
		}
	}

	// Validate resources
	if spec.Resources.Memory != "" {
		if err := ValidateResource(spec.Resources.Memory); err != nil {
			return fmt.Errorf("invalid memory resource: %w", err)
		}
	}
	if spec.Resources.CPU != "" {
		if err := ValidateResource(spec.Resources.CPU); err != nil {
			return fmt.Errorf("invalid CPU resource: %w", err)
		}
	}

	return nil
}
