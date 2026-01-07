package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// ErrDynamicPortUpdateNotSupported indicates that Podman does not support
// dynamically adding or removing port bindings on running containers.
// Port binding changes require container recreation.
var ErrDynamicPortUpdateNotSupported = errors.New("podman does not support dynamic port binding updates on running containers")

// ErrPortReconciliationRequired indicates that the container's published ports
// do not match the expected ports and the container needs to be recreated.
var ErrPortReconciliationRequired = errors.New("container port bindings do not match expected; recreation required")

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

// ContainerListItem captures minimal container identity information from `podman ps`.
type ContainerListItem struct {
	ID   string
	Name string
}

// Validation patterns for different argument types
var (
	// Container/image names: lowercase letters, numbers, hyphens, slashes, colons, and @ (for digests)
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:@/-]*[a-z0-9_]$|^[a-z0-9]$`)

	// Volume paths: absolute paths only, no special chars
	pathPattern = regexp.MustCompile(`^/[a-zA-Z0-9._/-]*$`)

	// Resource values: numbers with units
	resourcePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[kmgtKMGT]?[bB]?$`)

	// Environment variable keys
	envKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	// Label keys: allow dotted namespaces (e.g. io.piccolo.instance) with conservative characters.
	labelKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*[a-zA-Z0-9]$|^[a-zA-Z0-9]$`)
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

// ValidateLabelKey validates container label keys.
func ValidateLabelKey(key string) error {
	if key == "" {
		return fmt.Errorf("label key cannot be empty")
	}
	if len(key) > 255 {
		return fmt.Errorf("label key too long (max 255 chars)")
	}
	if strings.ContainsAny(key, "=\n\r\t") {
		return fmt.Errorf("label key contains invalid characters")
	}
	if !labelKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid label key format: %s", key)
	}
	return nil
}

// ValidateLabelValue validates container label values.
func ValidateLabelValue(value string) error {
	// Reuse env value constraints (printable, no control chars, reasonable length).
	return ValidateEnvValue(value)
}

// ContainerCreateSpec defines validated parameters for container creation
type ContainerCreateSpec struct {
	Name          string
	Image         string
	Ports         []PortMapping
	Volumes       []VolumeMapping
	Tmpfs         []TmpfsMount
	Environment   map[string]string
	Labels        map[string]string
	Resources     ResourceLimits
	NetworkMode   string
	RestartPolicy string

	// Workspace mode fields
	UseInit    bool     // If true, adds --init flag for PID 1 safety
	Entrypoint []string // Custom entrypoint (overrides image default)
	Command    []string // Command arguments appended after image

	// Rootfs mode: when set, uses --rootfs instead of Image.
	// This is used for workspace disk mode where the rootfs is a pre-mounted overlay.
	// When Rootfs is set, Image is ignored and the container runs directly from the
	// specified rootfs path. The caller must ensure the rootfs is properly mounted.
	Rootfs string

	// WorkingDir sets the working directory inside the container.
	// Used with --rootfs mode to apply image config since Podman doesn't do it automatically.
	WorkingDir string

	// User sets the user/group to run the container as (format: "uid:gid" or "user:group").
	// Used with --rootfs mode to apply image config since Podman doesn't do it automatically.
	User string

	// ExtraHosts adds entries to /etc/hosts (format: "hostname:IP").
	// Used for OIDC back-channel communication to allow containers to reach piccolo.local.
	ExtraHosts []HostEntry

	// CAMounts contains paths to CA certificates to mount into the container.
	// Used to trust the internal CA for OIDC HTTPS communication.
	CAMounts []CAMount
}

// HostEntry represents an /etc/hosts entry for --add-host.
type HostEntry struct {
	Hostname string
	IP       string
}

// CAMount represents a CA certificate mount for OIDC trust.
type CAMount struct {
	HostPath      string
	ContainerPath string
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

func buildCreateArgs(spec ContainerCreateSpec) []string {
	args := []string{"create"}

	// PID 1 init process for proper signal handling and zombie reaping
	if spec.UseInit {
		args = append(args, "--init")
	}

	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}

	if len(spec.Labels) > 0 {
		keys := make([]string, 0, len(spec.Labels))
		for k := range spec.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--label", fmt.Sprintf("%s=%s", k, spec.Labels[k]))
		}
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

	// Custom entrypoint (for workspace mode boot.sh wrapper)
	if len(spec.Entrypoint) > 0 {
		args = append(args, "--entrypoint", spec.Entrypoint[0])
	}

	// Working directory (used with --rootfs to apply image config)
	if spec.WorkingDir != "" {
		args = append(args, "--workdir", spec.WorkingDir)
	}

	// User (used with --rootfs to apply image config)
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}

	// Extra hosts (for OIDC back-channel communication)
	for _, host := range spec.ExtraHosts {
		args = append(args, "--add-host", fmt.Sprintf("%s:%s", host.Hostname, host.IP))
	}

	// CA certificate mounts (for OIDC HTTPS trust)
	for _, ca := range spec.CAMounts {
		args = append(args, "--volume", fmt.Sprintf("%s:%s:ro", ca.HostPath, ca.ContainerPath))
	}

	// Rootfs mode: use --rootfs instead of image reference.
	// When using --rootfs, Podman runs the container directly from the specified
	// filesystem path without pulling or resolving an image. This is used for
	// workspace disk mode where we mount an overlay filesystem combining the base
	// image with a persistent writable layer.
	if spec.Rootfs != "" {
		args = append(args, "--rootfs", spec.Rootfs)
	} else if spec.Image != "" {
		args = append(args, spec.Image)
	}

	// Append remaining entrypoint args and command after image/rootfs
	if len(spec.Entrypoint) > 1 {
		args = append(args, spec.Entrypoint[1:]...)
	}
	args = append(args, spec.Command...)

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
	createArgs := buildCreateArgs(spec)
	args, err := buildPodmanArgs(runtime, createArgs)
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
			return "", &PortInUseError{Port: port, Output: outStr, Err: fmt.Errorf("podman create failed: %w", err)}
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
		return "", fmt.Errorf("podman create failed: %w, output: %s", err, outStr)
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

// ImageExists checks if an image exists in the local storage.
func (p *PodmanCLI) ImageExists(ctx context.Context, runtime PodmanRuntime, imageName string) (bool, error) {
	if err := ValidateContainerName(imageName); err != nil {
		return false, fmt.Errorf("invalid image name: %w", err)
	}

	args, err := buildPodmanArgs(runtime, []string{"image", "exists", imageName})
	if err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	// Exit code 1 means image doesn't exist
	if code, ok := exitCode(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("podman image exists failed: %w", err)
}

// RemoveImage removes an image from local storage.
func (p *PodmanCLI) RemoveImage(ctx context.Context, runtime PodmanRuntime, imageName string) error {
	if err := ValidateContainerName(imageName); err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}

	args, err := buildPodmanArgs(runtime, []string{"rmi", imageName})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("podman rmi failed: %w, output: %s", err, string(output))
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

type streamReadCloser struct {
	r    *io.PipeReader
	once sync.Once
	stop func() error
}

func (s *streamReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *streamReadCloser) Close() error {
	var err error
	s.once.Do(func() {
		if s.stop != nil {
			err = s.stop()
		}
		_ = s.r.Close()
	})
	return err
}

func (p *PodmanCLI) LogsStream(ctx context.Context, runtime PodmanRuntime, containerID string, lines int, timestamps bool) (io.ReadCloser, error) {
	if !isValidContainerID(containerID) {
		return nil, fmt.Errorf("invalid container ID format: %s", containerID)
	}
	if lines <= 0 {
		lines = 200
	}
	args := []string{"logs", "--follow", "--tail", fmt.Sprintf("%d", lines)}
	if timestamps {
		args = append(args, "--timestamps")
	}
	args = append(args, containerID)
	cmdArgs, err := buildPodmanArgs(runtime, args)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, "podman", cmdArgs...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("podman logs stream stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("podman logs stream stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("podman logs stream start failed: %w", err)
	}

	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(pw, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(pw, stderr)
	}()

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		wg.Wait()
		_ = pw.Close()
		done <- err
		close(done)
	}()

	stop := func() error {
		cancel()
		err, ok := <-done
		if ok && err != nil {
			return err
		}
		return nil
	}
	return &streamReadCloser{r: pr, stop: stop}, nil
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

// ListContainersByLabel returns all containers (running or stopped) matching a single label selector.
// This is used for best-effort cleanup of orphaned containers created by Piccolo.
func (p *PodmanCLI) ListContainersByLabel(ctx context.Context, runtime PodmanRuntime, labelKey, labelValue string) ([]ContainerListItem, error) {
	if err := ValidateLabelKey(labelKey); err != nil {
		return nil, fmt.Errorf("invalid label key: %w", err)
	}
	if err := ValidateLabelValue(labelValue); err != nil {
		return nil, fmt.Errorf("invalid label value: %w", err)
	}

	filter := fmt.Sprintf("label=%s=%s", labelKey, labelValue)
	args, err := buildPodmanArgs(runtime, []string{
		"ps", "-a",
		"--filter", filter,
		"--format", "{{.ID}}\t{{.Names}}",
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman ps failed: %w, output: %s", err, string(out))
	}

	lines := strings.Split(string(out), "\n")
	items := make([]ContainerListItem, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		if id == "" || name == "" {
			continue
		}
		if !isValidContainerID(id) {
			continue
		}
		// Podman may print multiple names; keep the first.
		if idx := strings.Index(name, ","); idx >= 0 {
			name = strings.TrimSpace(name[:idx])
		}
		items = append(items, ContainerListItem{ID: id, Name: name})
	}

	return items, nil
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

// UpdatePublishAdd adds a port publish mapping to a running container.
// NOTE: Podman does not support dynamic port binding updates on running containers.
// This function returns ErrDynamicPortUpdateNotSupported. Port changes require container recreation.
func (p *PodmanCLI) UpdatePublishAdd(ctx context.Context, runtime PodmanRuntime, containerID string, hostBind, guestPort int) error {
	return ErrDynamicPortUpdateNotSupported
}

// UpdatePublishRemove removes a port publish mapping from a running container.
// NOTE: Podman does not support dynamic port binding updates on running containers.
// This function returns ErrDynamicPortUpdateNotSupported. Port changes require container recreation.
func (p *PodmanCLI) UpdatePublishRemove(ctx context.Context, runtime PodmanRuntime, containerID string, hostBind, guestPort int) error {
	return ErrDynamicPortUpdateNotSupported
}

// ResetStorage cleans up container references for this runtime's storage.
// Only removes containers, does NOT touch the shared imagestore.
func (p *PodmanCLI) ResetStorage(ctx context.Context, runtime PodmanRuntime) error {
	// Remove all containers for this runtime (should already be done, but be thorough)
	rmArgs, err := buildPodmanArgs(runtime, []string{"rm", "--all", "--force"})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "podman", rmArgs...)
	_ = cmd.Run() // Ignore errors - containers may already be gone
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

	// Validate image or rootfs (one must be set)
	if spec.Rootfs != "" {
		// Rootfs mode: validate the path
		if err := ValidatePath(spec.Rootfs); err != nil {
			return fmt.Errorf("invalid rootfs path: %w", err)
		}
	} else if spec.Image != "" {
		// Image mode: validate the image name
		if err := ValidateContainerName(spec.Image); err != nil {
			return fmt.Errorf("invalid image name: %w", err)
		}
	} else {
		return fmt.Errorf("either image or rootfs must be specified")
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

	// Validate labels
	for key, value := range spec.Labels {
		if err := ValidateLabelKey(key); err != nil {
			return fmt.Errorf("invalid label key '%s': %w", key, err)
		}
		if err := ValidateLabelValue(value); err != nil {
			return fmt.Errorf("invalid label value for key '%s': %w", key, err)
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

// ImageConfig holds OCI image configuration fields.
// These are extracted from the base image and used when running containers in --rootfs mode,
// since Podman does not apply image config automatically in that mode.
type ImageConfig struct {
	Entrypoint  []string
	Cmd         []string
	Env         []string // OCI-style KEY=VALUE entries
	WorkingDir  string
	User        string
	Digest      string   // Canonical digest (e.g., "sha256:abc123...")
	RepoDigests []string // List of canonical references (e.g., "docker.io/library/ubuntu@sha256:...")
}

// InspectImage retrieves the configuration of a container image.
// This extracts entrypoint, cmd, env, working directory, user, and digest from the image config.
// When using --rootfs mode, these must be explicitly applied since Podman doesn't do it automatically.
// The digest is the canonical image digest (sha256:...) which should be used for persistence
// to ensure the same base image is used across reinstalls and failovers.
func (p *PodmanCLI) InspectImage(ctx context.Context, runtime PodmanRuntime, imageName string) (*ImageConfig, error) {
	if err := ValidateContainerName(imageName); err != nil {
		return nil, fmt.Errorf("invalid image name: %w", err)
	}

	// Use podman image inspect to get the full image configuration including digest
	args, err := buildPodmanArgs(runtime, []string{
		"image", "inspect",
		"--format", `{"entrypoint":{{json .Config.Entrypoint}},"cmd":{{json .Config.Cmd}},"env":{{json .Config.Env}},"workingDir":{{json .Config.WorkingDir}},"user":{{json .Config.User}},"digest":{{json .Digest}},"repoDigests":{{json .RepoDigests}}}`,
		imageName,
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman image inspect failed: %w, output: %s", err, string(output))
	}

	// Parse the JSON output
	var result struct {
		Entrypoint  []string `json:"entrypoint"`
		Cmd         []string `json:"cmd"`
		Env         []string `json:"env"`
		WorkingDir  string   `json:"workingDir"`
		User        string   `json:"user"`
		Digest      string   `json:"digest"`
		RepoDigests []string `json:"repoDigests"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse image config: %w, output: %s", err, string(output))
	}

	return &ImageConfig{
		Entrypoint:  result.Entrypoint,
		Cmd:         result.Cmd,
		Env:         result.Env,
		WorkingDir:  result.WorkingDir,
		User:        result.User,
		Digest:      result.Digest,
		RepoDigests: result.RepoDigests,
	}, nil
}

// ImageSearchResult represents a single search result from a container registry.
type ImageSearchResult struct {
	Index       string `json:"Index"`
	Name        string `json:"Name"`
	Description string `json:"Description"`
	Stars       int    `json:"Stars"`
	Official    string `json:"Official"`
}

// ExecShellCmd returns an exec.Cmd configured for running an interactive shell
// inside a container. The caller is responsible for starting the command,
// typically with pty.Start() for terminal access.
func (p *PodmanCLI) ExecShellCmd(runtime PodmanRuntime, containerID string) (*exec.Cmd, error) {
	if !isValidContainerID(containerID) {
		return nil, fmt.Errorf("invalid container ID format: %s", containerID)
	}

	// Shell wrapper that prefers bash (for readline/completion) but falls back to sh.
	// Uses login shell (-l) to load full shell initialization (.bash_profile, .bashrc).
	shellCmd := `if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh; fi`

	// Build podman exec args with proper environment propagation
	args, err := buildPodmanArgs(runtime, []string{
		"exec",
		"-i", "-t", // Interactive + TTY
		"-e", "TERM=xterm-256color", // Pass TERM into the container
		containerID,
		"/bin/sh", "-c", shellCmd,
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("podman", args...)
	// Preserve existing environment (XDG_RUNTIME_DIR, PATH, etc. needed for rootless podman)
	cmd.Env = os.Environ()
	return cmd, nil
}

// SearchRegistry searches for images in container registries using podman search.
// The query is passed directly to podman search. Results are limited by the limit parameter.
func (p *PodmanCLI) SearchRegistry(ctx context.Context, runtime PodmanRuntime, query string, limit int) ([]ImageSearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("search query cannot be empty")
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}

	args, err := buildPodmanArgs(runtime, []string{
		"search",
		"--format", "json",
		"--limit", fmt.Sprintf("%d", limit),
		fmt.Sprintf("docker.io/%s", query),
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman search failed: %w, output: %s", err, string(output))
	}

	var results []ImageSearchResult
	if err := json.Unmarshal(output, &results); err != nil {
		return nil, fmt.Errorf("failed to parse search results: %w, output: %s", err, string(output))
	}

	return results, nil
}
