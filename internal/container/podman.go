package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/creack/pty"
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

// PullStallError indicates an image pull was aborted because no download
// progress (byte progress or PTY I/O) was observed for an extended period.
type PullStallError struct {
	Image           string
	StallDuration   time.Duration
	DownloadedBytes int64
	TotalBytes      int64
}

func (e *PullStallError) Error() string {
	return fmt.Sprintf("pull stalled: no progress for %s (downloaded %s/%s) for image %s",
		e.StallDuration.Truncate(time.Second), formatBytesCompact(e.DownloadedBytes), formatBytesCompact(e.TotalBytes), e.Image)
}

const (
	// pullWatchdogInterval is the tick rate for the pull progress watchdog.
	// Stall duration and PTY-liveness grace window are derived from this value.
	pullWatchdogInterval = 30 * time.Second

	// pullStallTicks is the number of consecutive watchdog ticks with zero byte
	// progress AND zero PTY I/O before a pull is declared stalled.
	// 10 ticks = 5 minutes. This balances detection speed against tolerance for
	// slow or bursty connections (TCP keepalive ~2 min handles truly dead connections).
	pullStallTicks = 10
)

// killProcess sends SIGTERM followed by SIGKILL after 5 seconds.
// Safe to call on an already-exited process (signals return errors that are discarded).
func killProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	time.AfterFunc(5*time.Second, func() {
		_ = p.Kill()
	})
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
type PodmanRuntime struct {
	Root          string
	RunRoot       string
	StorageDriver string
	StorageOpts   []string

	// Credential, when non-nil, causes all podman commands to run as the
	// specified user (rootless mode). When nil, commands inherit the parent
	// process credentials (rootful fallback).
	Credential *syscall.Credential
	// HomeDir is the home directory of the runtime user, used to set HOME
	// in the minimal environment for rootless execution.
	HomeDir string
}

// Validate rejects PodmanRuntime configurations that would cause rootful fallback.
// Returns an error if Credential is nil, UID is 0, or HomeDir is empty.
func (rt PodmanRuntime) Validate() error {
	if rt.Credential == nil {
		return errors.New("PodmanRuntime: Credential is nil (rootful fallback not allowed)")
	}
	if rt.Credential.Uid == 0 {
		return errors.New("PodmanRuntime: UID is 0 (rootful fallback not allowed)")
	}
	if rt.HomeDir == "" {
		return errors.New("PodmanRuntime: HomeDir is empty")
	}
	return nil
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

	// Environment variable keys
	envKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	// Label keys: allow dotted namespaces (e.g. io.piccolo.instance) with conservative characters.
	labelKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*[a-zA-Z0-9]$|^[a-zA-Z0-9]$`)

	// Storage driver name: lowercase alphanumeric only.
	storageDriverPattern = regexp.MustCompile(`^[a-z0-9]+$`)

	// Storage option: key=value or bare key with dotted namespaces.
	storageOptPattern = regexp.MustCompile(`^[a-z0-9_.]+=[/a-zA-Z0-9._-]+$|^[a-z0-9_.]+$`)

	// Container name-in-use error: extract the conflicting container ID.
	nameInUseIDPattern = regexp.MustCompile(`already in use by ([a-f0-9]{12,64})`)
)

var portInUseRe = regexp.MustCompile(`:(\d+): bind: address already in use`)

const podmanWaitDelay = 5 * time.Second

func podmanCmd(ctx context.Context, rt PodmanRuntime, args ...string) *exec.Cmd {
	// Validate credential presence outside test mode to catch rootful fallback bugs.
	// Non-fatal: logs ERROR but continues. Changing podmanCmd to return (cmd, error)
	// would require a large refactor of all callers. The ERROR log surfaces the issue
	// in production monitoring for investigation.
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") == "" {
		if err := rt.Validate(); err != nil {
			log.Printf("ERROR: podmanCmd: %v", err)
		}
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.WaitDelay = podmanWaitDelay
	ApplyRuntimeCredential(cmd, rt)
	return cmd
}

// runPodman builds args, runs a podman subcommand, and returns combined output.
// Encapsulates the buildPodmanArgs + podmanCmd + CombinedOutput pattern used by
// most methods. subcmdArgs must be non-empty; the first one or two non-flag
// elements are used for error messages (e.g., "network reload").
func runPodman(ctx context.Context, rt PodmanRuntime, subcmdArgs []string) ([]byte, error) {
	if len(subcmdArgs) == 0 {
		return nil, fmt.Errorf("runPodman: no subcommand provided")
	}
	args, err := BuildPodmanArgs(rt, subcmdArgs)
	if err != nil {
		return nil, err
	}
	cmd := podmanCmd(ctx, rt, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		label := subcmdArgs[0]
		if len(subcmdArgs) >= 2 && !strings.HasPrefix(subcmdArgs[1], "-") {
			label = subcmdArgs[0] + " " + subcmdArgs[1]
		}
		return output, fmt.Errorf("podman %s failed: %w, output: %s", label, err, string(output))
	}
	return output, nil
}

// checkContainerNotFound checks if podman output indicates a missing container.
// Returns a typed ContainerNotFoundError if detected, otherwise wraps with the
// given prefix.
func checkContainerNotFound(output string, containerRef, errPrefix string, err error) error {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "no such container") || strings.Contains(lower, "no container with") {
		return &ContainerNotFoundError{Ref: containerRef}
	}
	return fmt.Errorf("%s: %w, output: %s", errPrefix, err, output)
}

// lastNonEmptyLine returns the last non-empty, trimmed line from output.
// Used by inspect commands where podman may emit warnings before the result.
func lastNonEmptyLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// podmanInspectError wraps an exec.ExitError from a podman inspect call,
// including stderr when available.
func podmanInspectError(label string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("podman inspect (%s) failed: %w, stderr: %s", label, err, string(exitErr.Stderr))
	}
	return fmt.Errorf("podman inspect (%s) failed: %w", label, err)
}

// minimalRootlessEnv returns the minimal environment variables needed for
// rootless podman execution. This is the canonical set used by both
// ApplyRuntimeCredential and workspacedisk's PodmanImageMounter.
func minimalRootlessEnv(homeDir string, uid uint32) []string {
	return []string{
		fmt.Sprintf("HOME=%s", homeDir),
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid),
		fmt.Sprintf("PATH=%s", os.Getenv("PATH")),
		"LANG=C.UTF-8",
		"LC_ALL=C",
	}
}

// ProxyEnvVars returns proxy environment variables (HTTP_PROXY, HTTPS_PROXY, etc.)
// from the current process environment. Used to pass proxy settings into
// rootless subprocesses that get a minimal env instead of os.Environ().
func ProxyEnvVars() []string {
	var envs []string
	for _, key := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	} {
		if v := os.Getenv(key); v != "" {
			envs = append(envs, fmt.Sprintf("%s=%s", key, v))
		}
	}
	return envs
}

// ApplyRuntimeCredential configures cmd to run as the runtime user when
// rt.Credential is non-nil. It sets a minimal environment (with proxy vars)
// and SysProcAttr.Credential. When Credential is nil, this is a no-op
// and the command inherits the parent process environment.
// extraEnv values are appended to the minimal environment.
func ApplyRuntimeCredential(cmd *exec.Cmd, rt PodmanRuntime, extraEnv ...string) {
	if rt.Credential == nil {
		return
	}
	cmd.Env = append(minimalRootlessEnv(rt.HomeDir, rt.Credential.Uid), extraEnv...)
	cmd.Env = append(cmd.Env, ProxyEnvVars()...)

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = rt.Credential
}

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

	// Entrypoint/command override fields (service native, workspace boot.sh, init:image)
	UseInit    bool     // If true, adds --init flag for PID 1 safety
	Entrypoint []string // Custom entrypoint (overrides image default)
	Command    []string // Command arguments appended after image

	// Rootfs mode: when set, uses --rootfs instead of Image.
	// When Rootfs is set, Image is ignored and the container runs directly from the
	// specified rootfs path. The caller must ensure the rootfs is properly mounted.
	Rootfs string

	// RootfsOverlay appends ":O" to the --rootfs path, making podman create an
	// overlay writable layer on top of the rootfs. Required for read-only rootfs
	// where the container runtime needs to create /etc/hosts, /etc/hostname, etc.
	RootfsOverlay bool

	// ReadOnly makes the container's root filesystem read-only (--read-only).
	// Podman automatically adds tmpfs at /tmp and /run (--read-only-tmpfs, default true).
	// Persistent state is provided via bind-mounted volumes.
	ReadOnly bool

	// WorkingDir sets the working directory inside the container.
	// Used with --rootfs mode to apply image config since Podman doesn't do it automatically.
	WorkingDir string

	// User sets the user/group to run the container as (format: "uid:gid" or "user:group").
	// Used with --rootfs mode to apply image config since Podman doesn't do it automatically.
	User string

	// PullPolicy controls image pulling during create: "always", "missing", "never".
	// Per-app runtimes use "never" because service containers use --rootfs from
	// golden LV snapshots and do not pull images.
	PullPolicy string

	// ExtraHosts adds entries to /etc/hosts (format: "hostname:IP").
	// Used for OIDC back-channel communication to allow containers to reach piccolo.local.
	ExtraHosts []HostEntry

	// CAMounts contains paths to CA certificates to mount into the container.
	// Used to trust the internal CA for OIDC HTTPS communication.
	CAMounts []CAMount

	// SecurityOpt passes --security-opt flags to podman create.
	// Used to disable SELinux labeling (e.g., "label=disable") for containers
	// whose overlay layers or rootfs reside on filesystems where SELinux contexts
	// don't match the standard container_file_t type.
	SecurityOpt []string
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
	Protocol  string // "tcp" (default) or "udp"
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

// ResourceLimits carries container-scope resource knobs. Memory and CPU
// limits are intentionally absent — the resource-stewardship design
// enforces memory and CPU at the per-app-user systemd slice
// (see internal/container/slice_policy.go), not at individual container
// scopes. PidsLimit and NofileLimit remain per-container: they protect
// against fork-bomb and fd-exhaustion per-process, not shared-resource
// concerns.
type ResourceLimits struct {
	PidsLimit   int
	NofileLimit int
}

func buildCreateArgs(spec ContainerCreateSpec) []string {
	args := []string{"create"}

	// Pull policy: per-app runtimes set "never" because service containers
	// use --rootfs from golden LV snapshots and do not pull images.
	if spec.PullPolicy != "" {
		args = append(args, "--pull", spec.PullPolicy)
	}

	// Use k8s-file log driver instead of the system default (journald).
	// Rootless per-app users don't have systemd user sessions, so journald
	// logs are inaccessible via `podman logs`. k8s-file stores logs as flat
	// files in the container's graphroot, which the per-app user owns.
	args = append(args, "--log-driver=k8s-file")

	// Capability hardening: drop all capabilities and re-add only
	// the minimal set required for typical container workloads.
	args = append(args,
		"--cap-drop=ALL",
		"--cap-add=CHOWN",
		"--cap-add=DAC_OVERRIDE",
		"--cap-add=FOWNER",
		"--cap-add=FSETID",
		"--cap-add=SETUID",
		"--cap-add=SETGID",
		"--cap-add=NET_BIND_SERVICE",
		"--cap-add=KILL",
		"--cap-add=SYS_CHROOT",
		"--cap-add=SETFCAP",
		"--cap-add=SETPCAP",
		"--cap-add=AUDIT_WRITE",
	)

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
		publish := fmt.Sprintf("127.0.0.1:%d:%d", port.Host, port.Container)
		if port.Protocol == "udp" {
			publish += "/udp"
		}
		args = append(args, "--publish", publish)
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

	// Memory/CPU limits are enforced at the per-app-user systemd slice
	// (slice_policy.go), not at container scope. Only per-process guards
	// (pids-limit, nofile) are emitted here.
	if spec.Resources.PidsLimit > 0 {
		args = append(args, "--pids-limit", fmt.Sprintf("%d", spec.Resources.PidsLimit))
	}
	if spec.Resources.NofileLimit > 0 {
		args = append(args, "--ulimit", fmt.Sprintf("nofile=%d:%d", spec.Resources.NofileLimit, spec.Resources.NofileLimit))
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

	// Custom entrypoint (rootfs mode: workspace boot.sh, service native, init:image)
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

	// Security options (e.g., SELinux label disable)
	for _, opt := range spec.SecurityOpt {
		args = append(args, "--security-opt", opt)
	}

	// Read-only root filesystem: Podman adds tmpfs at /tmp and /run automatically.
	if spec.ReadOnly {
		args = append(args, "--read-only")
	}

	// Rootfs mode: use --rootfs instead of image reference.
	// :O suffix creates an overlay writable layer (required for read-only rootfs).
	if spec.Rootfs != "" {
		rootfsArg := spec.Rootfs
		if spec.RootfsOverlay {
			rootfsArg += ":O"
		}
		args = append(args, "--rootfs", rootfsArg)
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

// BuildPodmanArgs builds the CLI arguments for a podman command, prepending
// storage flags (--root, --runroot, --storage-driver, etc.) derived from runtime.
func BuildPodmanArgs(runtime PodmanRuntime, args []string) ([]string, error) {
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
	if runtime.StorageDriver != "" {
		if !storageDriverPattern.MatchString(runtime.StorageDriver) {
			return nil, fmt.Errorf("invalid storage driver name")
		}
		out = append(out, "--storage-driver", runtime.StorageDriver)
	}
	for _, opt := range runtime.StorageOpts {
		// Basic validation for options (key=value or key).
		// Keys can contain dots for driver-scoped options (e.g., overlay.mount_program).
		if !storageOptPattern.MatchString(opt) {
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
	args, err := BuildPodmanArgs(runtime, createArgs)
	if err != nil {
		return "", err
	}
	cmd := podmanCmd(ctx, runtime, args...)
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
			if match := nameInUseIDPattern.FindStringSubmatch(outStr); len(match) == 2 {
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
	if !isValidContainerID(containerID) {
		return fmt.Errorf("invalid container ID format: %s", containerID)
	}
	_, err := runPodman(ctx, runtime, []string{"start", containerID})
	return err
}

// StopContainer stops a container by validated ID.
// Uses a 30-second timeout to allow containers to gracefully shutdown.
func (p *PodmanCLI) StopContainer(ctx context.Context, runtime PodmanRuntime, containerID string) error {
	if !isValidContainerID(containerID) {
		return &ContainerNotFoundError{Ref: containerID}
	}
	args, err := BuildPodmanArgs(runtime, []string{"stop", "--time", "30", containerID})
	if err != nil {
		return err
	}
	cmd := podmanCmd(ctx, runtime, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return checkContainerNotFound(string(output), containerID, "podman stop failed", err)
	}
	return nil
}

// RemoveContainer removes a container by validated ID.
// Returns ContainerNotFoundError if the container does not exist.
func (p *PodmanCLI) RemoveContainer(ctx context.Context, runtime PodmanRuntime, containerID string) error {
	if !isValidContainerID(containerID) {
		return &ContainerNotFoundError{Ref: containerID}
	}
	args, err := BuildPodmanArgs(runtime, []string{"rm", containerID})
	if err != nil {
		return err
	}
	cmd := podmanCmd(ctx, runtime, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return checkContainerNotFound(string(output), containerID, "podman rm failed", err)
	}
	return nil
}

// ImageExists checks if an image exists in the local storage.
func (p *PodmanCLI) ImageExists(ctx context.Context, runtime PodmanRuntime, imageName string) (bool, error) {
	if err := ValidateContainerName(imageName); err != nil {
		return false, fmt.Errorf("invalid image name: %w", err)
	}

	args, err := BuildPodmanArgs(runtime, []string{"image", "exists", imageName})
	if err != nil {
		return false, err
	}
	cmd := podmanCmd(ctx, runtime, args...)
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
	_, err := runPodman(ctx, runtime, []string{"rmi", imageName})
	return err
}

// PullImage pulls an image by name
func (p *PodmanCLI) PullImage(ctx context.Context, runtime PodmanRuntime, image string) error {
	if err := ValidateContainerName(image); err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}
	_, err := runPodman(ctx, runtime, []string{"pull", image})
	return err
}

// Logs returns recent log lines from a container
func (p *PodmanCLI) Logs(ctx context.Context, runtime PodmanRuntime, containerID string, lines int) ([]string, error) {
	if !isValidContainerID(containerID) {
		return nil, fmt.Errorf("invalid container ID format: %s", containerID)
	}
	if lines <= 0 {
		lines = 200
	}
	output, err := runPodman(ctx, runtime, []string{"logs", "--tail", fmt.Sprintf("%d", lines), containerID})
	if err != nil {
		return nil, err
	}
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
	cmdArgs, err := BuildPodmanArgs(runtime, args)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := podmanCmd(runCtx, runtime, cmdArgs...)

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
	args, err := BuildPodmanArgs(runtime, []string{"container", "exists", containerRef})
	if err != nil {
		return false, err
	}
	cmd := podmanCmd(ctx, runtime, args...)
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

	args, err := BuildPodmanArgs(runtime, []string{"inspect", "--format", "{{.Id}}", name})
	if err != nil {
		return "", err
	}
	cmd := podmanCmd(ctx, runtime, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", podmanInspectError("id", err)
	}

	id := lastNonEmptyLine(string(out))
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
	out, err := runPodman(ctx, runtime, []string{
		"ps", "-a",
		"--filter", filter,
		"--format", "{{.ID}}\t{{.Names}}",
	})
	if err != nil {
		return nil, err
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

	// Get both running state and PID to detect stale state after reboot
	args, err := BuildPodmanArgs(runtime, []string{"inspect", "--format", "{{.State.Running}}|{{.State.Pid}}", containerID})
	if err != nil {
		return ContainerState{}, err
	}
	cmd := podmanCmd(ctx, runtime, args...)
	out, err := cmd.Output()
	if err != nil {
		return ContainerState{}, podmanInspectError("running", err)
	}

	result := lastNonEmptyLine(string(out))

	// Parse "running|pid" format
	parts := strings.SplitN(result, "|", 2)
	if len(parts) != 2 {
		return ContainerState{}, fmt.Errorf("podman inspect returned unexpected format: %q", result)
	}

	running := parts[0] == "true"
	pid := strings.TrimSpace(parts[1])

	// If podman reports running but PID doesn't exist, the container is stale
	// This happens after system reboot when podman DB has stale state
	if running && pid != "" && pid != "0" {
		procPath := fmt.Sprintf("/proc/%s", pid)
		if _, err := os.Stat(procPath); os.IsNotExist(err) {
			// PID doesn't exist - container is stale, treat as not running
			return ContainerState{Exists: true, Running: false}, nil
		}
	}

	return ContainerState{Exists: true, Running: running}, nil
}

// InspectPublishedPorts returns a map of "guest_port/proto" -> host_port for a container.
// Keys are formatted as "80/tcp" or "53/udp" to distinguish protocols. Callers
// must normalize FlowTLS to "tcp" when building lookup keys since TLS is TCP
// at the transport layer.
func (p *PodmanCLI) InspectPublishedPorts(ctx context.Context, runtime PodmanRuntime, containerID string) (map[string]int, error) {
	if containerID == "" {
		return nil, fmt.Errorf("container ID required")
	}
	args, err := BuildPodmanArgs(runtime, []string{"port", containerID})
	if err != nil {
		return nil, err
	}
	cmd := podmanCmd(ctx, runtime, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("podman port failed: %w", err)
	}

	result := make(map[string]int)
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
		left := strings.TrimSpace(parts[0])  // e.g. "80/tcp" or "53/udp"
		right := strings.TrimSpace(parts[1]) // e.g. "127.0.0.1:15001"
		leftParts := strings.Split(left, "/")
		guestStr := strings.TrimSpace(leftParts[0])
		guest, _ := strconv.Atoi(guestStr)
		hostParts := strings.Split(right, ":")
		hostStr := hostParts[len(hostParts)-1]
		host, _ := strconv.Atoi(strings.TrimSpace(hostStr))
		if guest > 0 && host > 0 {
			result[left] = host // key is "80/tcp" or "53/udp"
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

// NetworkReload tears down and re-creates network configuration (nftables rules,
// interfaces) for a running container. Treats "container not running" as non-fatal.
func (p *PodmanCLI) NetworkReload(ctx context.Context, runtime PodmanRuntime, containerNameOrID string) error {
	if containerNameOrID == "" {
		return fmt.Errorf("container name or ID required")
	}
	// Accept both container IDs and names; validate whichever format applies.
	if !isValidContainerID(containerNameOrID) {
		if err := ValidateContainerName(containerNameOrID); err != nil {
			return fmt.Errorf("invalid container reference: %w", err)
		}
	}

	output, err := runPodman(ctx, runtime, []string{"network", "reload", containerNameOrID})
	if err != nil {
		outStr := strings.ToLower(string(output))
		// Treat "not running" as non-fatal — container may have been stopped between check and reload
		if strings.Contains(outStr, "not running") || strings.Contains(outStr, "is not running") {
			log.Printf("INFO: network reload skipped for %s: container not running", containerNameOrID)
			return nil
		}
		return err
	}
	return nil
}

// ResetStorage cleans up container references for this runtime's storage.
func (p *PodmanCLI) ResetStorage(ctx context.Context, runtime PodmanRuntime) error {
	// Remove all containers for this runtime (should already be done, but be thorough)
	args, err := BuildPodmanArgs(runtime, []string{"rm", "--all", "--force"})
	if err != nil {
		return err
	}
	cmd := podmanCmd(ctx, runtime, args...)
	_ = cmd.Run() // Ignore errors - containers may already be gone
	return nil
}

// storageCorruptionIndicators are specific error patterns that always indicate overlay
// storage corruption from killed image pulls.
var storageCorruptionIndicators = []string{
	"creating file-getter",
	"readlink",
	"layers.lock",
	"layers.json",
	"structure needs cleaning",
	"input/output error",
}

// storageGenericErrors are generic error strings that only indicate corruption
// when the output also references storage-related paths. Without path context,
// these could come from unrelated failures (e.g., missing podman binary, bad --root path).
var storageGenericErrors = []string{
	"no such file or directory",
	"invalid argument",
}

// storagePathKeywords confirm that a generic error is storage-related.
var storagePathKeywords = []string{
	"overlay",
	"layer",
	"diff/",
}

// ValidateAndRepairStorage checks if the podman overlay storage for a runtime is healthy.
// If corruption is detected (e.g., from a previous killed container operation), it cleans
// up the per-app overlay directories. Only repairs when the error output matches known
// corruption patterns; other failures (missing binary, permission denied, context timeout)
// are returned as errors. Returns true if repair was performed.
func (p *PodmanCLI) ValidateAndRepairStorage(ctx context.Context, runtime PodmanRuntime) (bool, error) {
	// Quick health check: try listing images
	args, err := BuildPodmanArgs(runtime, []string{"images", "--quiet", "--noheading"})
	if err != nil {
		return false, fmt.Errorf("build podman args: %w", err)
	}
	cmd := podmanCmd(ctx, runtime, args...)
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return false, nil // Storage is healthy
	}

	// Check if the failure indicates storage corruption vs. a transient/unrelated error.
	// Two-tier matching: specific indicators always mean corruption; generic errors
	// only count when the output also mentions storage-related paths.
	outputStr := strings.ToLower(string(output) + " " + runErr.Error())
	isCorruption := false
	for _, pattern := range storageCorruptionIndicators {
		if strings.Contains(outputStr, pattern) {
			isCorruption = true
			break
		}
	}
	if !isCorruption {
		// Check generic errors, but only if output references storage paths.
		hasStorageContext := false
		for _, kw := range storagePathKeywords {
			if strings.Contains(outputStr, kw) {
				hasStorageContext = true
				break
			}
		}
		if hasStorageContext {
			for _, pattern := range storageGenericErrors {
				if strings.Contains(outputStr, pattern) {
					isCorruption = true
					break
				}
			}
		}
	}
	if !isCorruption {
		return false, fmt.Errorf("podman images check failed (non-corruption): %w, output: %s", runErr, string(output))
	}

	log.Printf("WARN: podman storage corruption detected for root=%s (output: %s), attempting repair", runtime.Root, strings.TrimSpace(string(output)))

	repaired := false

	// Phase 1: Clean per-app overlay storage directories.
	// These contain stale layers from killed pulls. Removing them allows a fresh pull.
	if runtime.Root != "" {
		overlayDirs := []string{"overlay", "overlay-images", "overlay-layers", "overlay-containers"}
		for _, dir := range overlayDirs {
			target := filepath.Join(runtime.Root, dir)
			if _, statErr := os.Stat(target); statErr == nil {
				if rmErr := os.RemoveAll(target); rmErr != nil {
					log.Printf("WARN: failed to remove stale overlay dir %s: %v", target, rmErr)
				} else {
					log.Printf("INFO: removed stale overlay dir: %s", target)
					repaired = true
				}
			}
		}
	}

	if repaired {
		log.Printf("INFO: podman storage repair completed for root=%s", runtime.Root)
	}
	return repaired, nil
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

	// Validate pull policy
	if spec.PullPolicy != "" {
		switch spec.PullPolicy {
		case "always", "missing", "never":
		default:
			return fmt.Errorf("invalid pull policy %q: must be always, missing, or never", spec.PullPolicy)
		}
	}

	// Validate image or rootfs (one must be set)
	if spec.Rootfs != "" {
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

	// Memory and CPU resources are enforced at the slice level, not per
	// container. Validation of those fields lives in the manifest parser
	// path (see internal/app/parser.go) against the new-shape schema.
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
	Size        int64    // Uncompressed image size in bytes
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
	args, err := BuildPodmanArgs(runtime, []string{
		"image", "inspect",
		"--format", `{"entrypoint":{{json .Config.Entrypoint}},"cmd":{{json .Config.Cmd}},"env":{{json .Config.Env}},"workingDir":{{json .Config.WorkingDir}},"user":{{json .Config.User}},"digest":{{json .Digest}},"repoDigests":{{json .RepoDigests}},"size":{{json .Size}}}`,
		imageName,
	})
	if err != nil {
		return nil, err
	}

	cmd := podmanCmd(ctx, runtime, args...)
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
		Size        int64    `json:"size"`
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
		Size:        result.Size,
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
	args, err := BuildPodmanArgs(runtime, []string{
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
	cmd.WaitDelay = podmanWaitDelay
	ApplyRuntimeCredential(cmd, runtime, "TERM=xterm-256color")
	return cmd, nil
}

// ExecScriptOptions configures non-interactive script execution inside a container.
type ExecScriptOptions struct {
	Shell      string            // Interpreter to run the script (default: "/bin/sh").
	ScriptPath string            // Absolute path to the script inside the container.
	Env        map[string]string // Environment variables injected via podman exec -e.
	Timeout    time.Duration     // Maximum execution time. Zero means no timeout.
}

// ExecScript executes a script non-interactively inside a running container.
// Returns the exit code, combined stdout+stderr output (capped at 1MB), and any error.
func (p *PodmanCLI) ExecScript(ctx context.Context, runtime PodmanRuntime, containerID string, opts ExecScriptOptions) (int, string, error) {
	if !isValidContainerID(containerID) {
		return -1, "", fmt.Errorf("invalid container ID format: %s", containerID)
	}

	shell := opts.Shell
	if shell == "" {
		shell = "/bin/sh"
	}

	// Build podman exec args: each -e K=V as separate args to avoid shell metachar issues.
	execArgs := []string{"exec"}
	// Sort env keys for deterministic ordering.
	envKeys := make([]string, 0, len(opts.Env))
	for k := range opts.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		execArgs = append(execArgs, "-e", k+"="+opts.Env[k])
	}
	execArgs = append(execArgs, containerID, shell, opts.ScriptPath)

	args, err := BuildPodmanArgs(runtime, execArgs)
	if err != nil {
		return -1, "", err
	}

	// Apply timeout via context.
	execCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := podmanCmd(execCtx, runtime, args...)

	// Capture stdout+stderr with a 1MB limit to prevent OOM from misbehaving scripts.
	var buf limitedBuffer
	buf.max = 1 << 20 // 1MB
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err = cmd.Run()
	outStr := buf.String()

	if err != nil {
		// Check for timeout FIRST: context.WithTimeout + CommandContext returns
		// *exec.ExitError with code -1 when the deadline fires, so checking exit
		// code first would misclassify timeouts as generic exit failures.
		if execCtx.Err() == context.DeadlineExceeded {
			return -1, outStr, fmt.Errorf("script timed out after %s", opts.Timeout)
		}
		if code, ok := exitCode(err); ok {
			return code, outStr, fmt.Errorf("script exited with code %d", code)
		}
		return -1, outStr, fmt.Errorf("exec failed: %w", err)
	}

	return 0, outStr, nil
}

// limitedBuffer is an io.Writer that caps total bytes written.
type limitedBuffer struct {
	buf []byte
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p) // preserve original length for io.Writer contract
	remaining := b.max - len(b.buf)
	if remaining <= 0 {
		return n, nil // silently discard
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.buf = append(b.buf, p...)
	return n, nil // always report full write to avoid io.ErrShortWrite
}

func (b *limitedBuffer) String() string {
	return string(b.buf)
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

	output, err := runPodman(ctx, runtime, []string{
		"search",
		"--format", "json",
		"--limit", fmt.Sprintf("%d", limit),
		fmt.Sprintf("docker.io/%s", query),
	})
	if err != nil {
		return nil, err
	}

	var results []ImageSearchResult
	if err := json.Unmarshal(output, &results); err != nil {
		return nil, fmt.Errorf("failed to parse search results: %w, output: %s", err, string(output))
	}

	return results, nil
}

// ImagePullProgress represents the download progress of a single layer.
type ImagePullProgress struct {
	LayerID      string `json:"layer_id"`
	BytesTotal   int64  `json:"bytes_total"`
	BytesCurrent int64  `json:"bytes_current"`
	Status       string `json:"status"` // "downloading", "extracting", "done", "exists"
}

// ImagePullReport aggregates progress across all layers being pulled.
type ImagePullReport struct {
	Image           string              `json:"image"`
	Layers          []ImagePullProgress `json:"layers"`
	TotalBytes      int64               `json:"total_bytes"`
	DownloadedBytes int64               `json:"downloaded_bytes"`
	OverallPercent  int                 `json:"overall_percent"` // -1 for indeterminate
	Phase           string              `json:"phase"`           // "pulling", "extracting", "complete"
}

// ImagePullCallback is invoked with progress updates during image pull.
type ImagePullCallback func(report ImagePullReport)

// pullProgressParser parses podman pull output and tracks layer progress.
type pullProgressParser struct {
	image   string
	layers  map[string]*ImagePullProgress
	mu      sync.Mutex
	lastCB  time.Time
	minRate time.Duration // minimum time between callbacks (throttle)
}

// newPullProgressParser creates a parser for the given image.
func newPullProgressParser(image string) *pullProgressParser {
	return &pullProgressParser{
		image:   image,
		layers:  make(map[string]*ImagePullProgress),
		minRate: 250 * time.Millisecond, // max 4 callbacks/sec
	}
}

// Regular expressions for parsing podman pull output
var (
	// Matches: "Copying blob sha256:abc123 [======>    ] 15.2MiB / 45.3MiB"
	// Also handles: "Copying blob sha256:abc123 1.2KiB / 5.6KiB"
	// Layer ID can be sha256:hex or just hex
	pullProgressRe = regexp.MustCompile(`Copying blob ([a-zA-Z0-9:]+)\s*(?:\[.*?\])?\s*([\d.]+)\s*([KMGTkmgt]?i?[Bb]?)\s*/\s*([\d.]+)\s*([KMGTkmgt]?i?[Bb]?)`)

	// Matches: "Copying blob sha256:abc123 done"
	pullDoneRe = regexp.MustCompile(`Copying blob ([a-zA-Z0-9:]+)\s+done`)

	// Matches: "Copying blob sha256:abc123 skipped: already exists"
	pullExistsRe = regexp.MustCompile(`Copying blob ([a-zA-Z0-9:]+)\s+skipped.*exists`)

)

// parseLine processes a single line of podman pull output.
func (p *pullProgressParser) parseLine(line string) {
	// Strip ANSI escape codes and handle carriage returns
	clean := stripansi.Strip(line)
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check for layer download progress
	if match := pullProgressRe.FindStringSubmatch(clean); len(match) >= 6 {
		layerID := match[1]
		current := parseBytes(match[2], match[3])
		total := parseBytes(match[4], match[5])

		layer, ok := p.layers[layerID]
		if !ok {
			layer = &ImagePullProgress{LayerID: layerID, Status: "downloading"}
			p.layers[layerID] = layer
		}
		layer.BytesCurrent = current
		layer.BytesTotal = total
		layer.Status = "downloading"
		return
	}

	// Check for layer done
	if match := pullDoneRe.FindStringSubmatch(clean); len(match) >= 2 {
		layerID := match[1]
		layer, ok := p.layers[layerID]
		if !ok {
			layer = &ImagePullProgress{LayerID: layerID}
			p.layers[layerID] = layer
		}
		layer.Status = "done"
		if layer.BytesTotal > 0 {
			layer.BytesCurrent = layer.BytesTotal
		}
		return
	}

	// Check for layer already exists
	if match := pullExistsRe.FindStringSubmatch(clean); len(match) >= 2 {
		layerID := match[1]
		layer, ok := p.layers[layerID]
		if !ok {
			layer = &ImagePullProgress{LayerID: layerID}
			p.layers[layerID] = layer
		}
		layer.Status = "exists"
		return
	}
}

// parseBytes converts a size string like "15.2" with unit "MiB" to bytes.
func parseBytes(numStr, unit string) int64 {
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	unit = strings.ToLower(strings.TrimSpace(unit))
	multiplier := float64(1)

	switch {
	case strings.HasPrefix(unit, "k"):
		multiplier = 1024
	case strings.HasPrefix(unit, "m"):
		multiplier = 1024 * 1024
	case strings.HasPrefix(unit, "g"):
		multiplier = 1024 * 1024 * 1024
	case strings.HasPrefix(unit, "t"):
		multiplier = 1024 * 1024 * 1024 * 1024
	}

	return int64(val * multiplier)
}

// formatBytesCompact formats bytes for log messages (e.g. "150MB", "2.1GB").
func formatBytesCompact(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// report generates an ImagePullReport from current state.
func (p *pullProgressParser) report() ImagePullReport {
	p.mu.Lock()
	defer p.mu.Unlock()

	report := ImagePullReport{
		Image:  p.image,
		Layers: make([]ImagePullProgress, 0, len(p.layers)),
		Phase:  "pulling",
	}

	// Collect and sort layer IDs for stable ordering in UI
	layerIDs := make([]string, 0, len(p.layers))
	for id := range p.layers {
		layerIDs = append(layerIDs, id)
	}
	sort.Strings(layerIDs)

	for _, id := range layerIDs {
		layer := p.layers[id]
		report.Layers = append(report.Layers, *layer)
		report.TotalBytes += layer.BytesTotal
		report.DownloadedBytes += layer.BytesCurrent
	}

	// Calculate overall percentage
	if report.TotalBytes > 0 {
		report.OverallPercent = int((report.DownloadedBytes * 100) / report.TotalBytes)
		if report.OverallPercent > 100 {
			report.OverallPercent = 100
		}
	} else if len(p.layers) > 0 {
		// Count done/exists layers
		done := 0
		for _, layer := range p.layers {
			if layer.Status == "done" || layer.Status == "exists" {
				done++
			}
		}
		if done == len(p.layers) {
			report.OverallPercent = 100
			report.Phase = "complete"
		} else {
			report.OverallPercent = -1 // indeterminate
		}
	} else {
		report.OverallPercent = -1 // indeterminate
	}

	// Check if all layers are done
	allDone := len(p.layers) > 0
	for _, layer := range p.layers {
		if layer.Status != "done" && layer.Status != "exists" {
			allDone = false
			break
		}
	}
	if allDone {
		report.Phase = "complete"
		report.OverallPercent = 100
	}

	return report
}

// shouldCallback returns true if enough time has passed since the last callback.
func (p *pullProgressParser) shouldCallback() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Since(p.lastCB) < p.minRate {
		return false
	}
	p.lastCB = time.Now()
	return true
}

// PullImageWithProgress pulls an image with real-time progress callbacks.
// Uses a PTY to get progress output from podman, which only outputs progress bars when running in a TTY.
// If callback is nil, behaves like PullImage without progress.
func (p *PodmanCLI) PullImageWithProgress(ctx context.Context, runtime PodmanRuntime, image string, callback ImagePullCallback) error {
	if err := ValidateContainerName(image); err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}

	args, err := BuildPodmanArgs(runtime, []string{"pull", image})
	if err != nil {
		return err
	}

	// If no callback, just use regular pull
	if callback == nil {
		cmd := podmanCmd(ctx, runtime, args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("podman pull failed: %w, output: %s", err, string(output))
		}
		return nil
	}

	// Use PTY for progress output
	log.Printf("INFO: PullImageWithProgress: starting podman pull %s (args=%v uid=%v)", image, args, runtime.Credential)
	cmd := exec.Command("podman", args...)
	cmd.WaitDelay = podmanWaitDelay
	// IMPORTANT: applyRuntimeCredential must be called before pty.StartWithSize
	// because creack/pty v1.1.24 merges Setsid/Setctty into existing SysProcAttr
	// rather than replacing it, so the Credential we set here is preserved.
	ApplyRuntimeCredential(cmd, runtime)

	// Start command with PTY using wide terminal to prevent line wrapping
	// which would break our line-based regex parsing
	ptySize := &pty.Winsize{Rows: 24, Cols: 500}
	ptmx, err := pty.StartWithSize(cmd, ptySize)
	if err != nil {
		return fmt.Errorf("failed to start podman pull with pty: %w", err)
	}
	defer ptmx.Close()
	log.Printf("INFO: PullImageWithProgress: podman process started (pid=%d) for %s", cmd.Process.Pid, image)

	// Channel to signal normal completion
	done := make(chan struct{})
	var cmdErr error

	// Handle context cancellation with proper signal handling
	go func() {
		select {
		case <-ctx.Done():
			killProcess(cmd.Process)
		case <-done:
			// Normal completion
		}
	}()

	parser := newPullProgressParser(image)

	// Stall detection state. Both signals must be silent for pullStallTicks
	// consecutive ticks before declaring a stall:
	//   1. Byte progress: parser.report().DownloadedBytes hasn't advanced
	//   2. PTY I/O liveness: no bytes read from PTY (covers slow connections
	//      where podman receives data but doesn't emit parseable progress lines)
	var stallDetected atomic.Bool
	var lastPTYReadNano atomic.Int64
	lastPTYReadNano.Store(time.Now().UnixNano())

	// Watchdog: log periodically + stall detection
	watchdog := time.NewTicker(pullWatchdogInterval)
	go func() {
		defer watchdog.Stop()
		var prevBytes int64
		var stallCount int
		for {
			select {
			case <-watchdog.C:
				report := parser.report()
				downloaded := report.DownloadedBytes
				total := report.TotalBytes
				pct := report.OverallPercent

				// Check for progress via either signal.
				byteProgress := downloaded > prevBytes
				ptyAlive := time.Since(time.Unix(0, lastPTYReadNano.Load())) < pullWatchdogInterval+5*time.Second

				if byteProgress || ptyAlive {
					stallCount = 0
					prevBytes = downloaded
				} else {
					// Count stall ticks unless all layers are already "done"/"exists"
					// (post-download extraction/verification is local CPU work, not
					// a network stall). Covers both:
					//  - Pre-download hang (no layers yet: auth/TLS/manifest resolution)
					//  - Mid-download hang (layers actively downloading)
					allComplete := len(report.Layers) > 0
					for _, l := range report.Layers {
						if l.Status != "done" && l.Status != "exists" {
							allComplete = false
							break
						}
					}
					if !allComplete {
						stallCount++
					}
				}

				if stallCount > 0 {
					log.Printf("INFO: PullImageWithProgress: still waiting for podman pull %s (pid=%d, %s/%s, %d%%, stall: %d/%d)",
						image, cmd.Process.Pid, formatBytesCompact(downloaded), formatBytesCompact(total), pct, stallCount, pullStallTicks)
				} else {
					log.Printf("INFO: PullImageWithProgress: still waiting for podman pull %s (pid=%d, %s/%s, %d%%)",
						image, cmd.Process.Pid, formatBytesCompact(downloaded), formatBytesCompact(total), pct)
				}

				if stallCount >= pullStallTicks {
					stallDuration := time.Duration(stallCount) * pullWatchdogInterval
					log.Printf("WARN: PullImageWithProgress: pull stalled for %s with no progress — aborting (downloaded %s/%s)",
						stallDuration, formatBytesCompact(downloaded), formatBytesCompact(total))
					stallDetected.Store(true)
					killProcess(cmd.Process)
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Emit initial progress
	callback(ImagePullReport{
		Image:          image,
		OverallPercent: 0,
		Phase:          "pulling",
	})

	// Capture recent PTY output lines for diagnostics on failure.
	const tailSize = 10
	var tailLines [tailSize]string
	var tailIdx int
	var tailMu sync.Mutex
	appendTail := func(line string) {
		tailMu.Lock()
		tailLines[tailIdx%tailSize] = line
		tailIdx++
		tailMu.Unlock()
	}
	getTail := func() string {
		tailMu.Lock()
		defer tailMu.Unlock()
		var lines []string
		start := 0
		if tailIdx > tailSize {
			start = tailIdx - tailSize
		}
		for i := start; i < tailIdx; i++ {
			if l := tailLines[i%tailSize]; l != "" {
				lines = append(lines, l)
			}
		}
		return strings.Join(lines, "\n")
	}

	// Read PTY output and parse progress
	go func() {
		buf := make([]byte, 4096)
		var lineBuf strings.Builder

		for {
			n, readErr := ptmx.Read(buf)
			if readErr != nil {
				break
			}
			if n == 0 {
				continue
			}

			// Update PTY-liveness signal for stall detection.
			lastPTYReadNano.Store(time.Now().UnixNano())

			// Process the bytes
			data := string(buf[:n])

			// Handle carriage returns - they reset the line buffer
			for _, ch := range data {
				if ch == '\r' {
					// Process current line and reset
					line := lineBuf.String()
					if line != "" {
						parser.parseLine(line)
						appendTail(line)
					}
					lineBuf.Reset()
				} else if ch == '\n' {
					// Process current line
					line := lineBuf.String()
					if line != "" {
						parser.parseLine(line)
						appendTail(line)
					}
					lineBuf.Reset()
				} else {
					lineBuf.WriteRune(ch)
				}
			}

			// Throttled callback
			if parser.shouldCallback() {
				callback(parser.report())
			}
		}

		// Process any remaining data
		if lineBuf.Len() > 0 {
			line := lineBuf.String()
			parser.parseLine(line)
			appendTail(line)
		}
	}()

	// Wait for command to finish
	cmdErr = cmd.Wait()
	close(done)
	log.Printf("INFO: PullImageWithProgress: podman pull %s finished (err=%v pid=%d)", image, cmdErr, cmd.Process.Pid)

	// Always emit final completion
	finalReport := parser.report()
	if cmdErr == nil {
		finalReport.Phase = "complete"
		finalReport.OverallPercent = 100
	}
	callback(finalReport)

	if cmdErr != nil {
		// Always log the diagnostic tail on failure — context cancellation is
		// often a symptom; the tail reveals the underlying cause (manifest
		// unknown, rate limit, network error, etc.).
		tail := getTail()
		if tail != "" {
			log.Printf("WARN: PullImageWithProgress: podman pull %s output tail:\n%s", image, tail)
		}
		// Check stall detection first — it's more specific than a generic
		// context timeout and provides structured diagnostic data.
		if stallDetected.Load() {
			report := parser.report()
			return &PullStallError{
				Image:           image,
				StallDuration:   time.Duration(pullStallTicks) * pullWatchdogInterval,
				DownloadedBytes: report.DownloadedBytes,
				TotalBytes:      report.TotalBytes,
			}
		}
		if ctx.Err() != nil {
			if tail != "" {
				// Surface the last meaningful output line in the error so it
				// reaches the UI. Truncate to prevent oversized error messages.
				lines := strings.Split(strings.TrimSpace(tail), "\n")
				lastLine := lines[len(lines)-1]
				if len(lastLine) > 200 {
					lastLine = lastLine[:200]
				}
				return fmt.Errorf("%w (last output: %s)", ctx.Err(), lastLine)
			}
			return ctx.Err()
		}
		return fmt.Errorf("podman pull failed: %w", cmdErr)
	}

	return nil
}
