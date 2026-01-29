package app

import (
	"context"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

// ContainerManager describes the container runtime operations required by the app manager.
type ContainerManager interface {
	CreateContainer(ctx context.Context, runtime container.PodmanRuntime, spec container.ContainerCreateSpec) (string, error)
	StartContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	StopContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	RemoveContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	ListContainersByLabel(ctx context.Context, runtime container.PodmanRuntime, labelKey, labelValue string) ([]container.ContainerListItem, error)
	PullImage(ctx context.Context, runtime container.PodmanRuntime, image string) error
	Logs(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int) ([]string, error)
	LogsStream(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int, timestamps bool) (io.ReadCloser, error)
	ResolveContainerIDByName(ctx context.Context, runtime container.PodmanRuntime, name string) (string, error)
	InspectContainerState(ctx context.Context, runtime container.PodmanRuntime, containerID string) (container.ContainerState, error)
	InspectPublishedPorts(ctx context.Context, runtime container.PodmanRuntime, containerID string) (map[int]int, error)
	UpdatePublishAdd(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error
	UpdatePublishRemove(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error
	ResetStorage(ctx context.Context, runtime container.PodmanRuntime) error
	// ValidateAndRepairStorage checks if overlay storage is healthy and repairs if corrupted.
	// Returns true if repair was performed.
	ValidateAndRepairStorage(ctx context.Context, runtime container.PodmanRuntime) (bool, error)
	// ImageExists checks if an image exists in local storage.
	ImageExists(ctx context.Context, runtime container.PodmanRuntime, imageName string) (bool, error)
	// RemoveImage removes an image from local storage.
	RemoveImage(ctx context.Context, runtime container.PodmanRuntime, imageName string) error
	// InspectImage retrieves the configuration of a container image.
	InspectImage(ctx context.Context, runtime container.PodmanRuntime, imageName string) (*container.ImageConfig, error)
	// SearchRegistry searches for images in container registries.
	SearchRegistry(ctx context.Context, runtime container.PodmanRuntime, query string, limit int) ([]container.ImageSearchResult, error)
	// ExecShellCmd returns an exec.Cmd for running a shell inside a container.
	ExecShellCmd(runtime container.PodmanRuntime, containerID string) (*exec.Cmd, error)
}

// AppInstance captures the runtime metadata for an installed application instance.
// InstanceID is the system-generated unique key used everywhere (containers, volumes, services).
// DisplayName is an optional user-provided friendly name for UI purposes.
// Definition contains the full app manifest (image, type, listeners, extensions, etc).
type AppInstance struct {
	InstanceID  string `json:"instance_id"`
	DisplayName string `json:"display_name,omitempty"`
	Status      string `json:"status"`
	// Container runtime metadata.
	PrimaryService  string            `json:"primary_service,omitempty"`
	NetworkAnchorID string            `json:"network_anchor_id,omitempty"`
	Containers      map[string]string `json:"containers,omitempty"` // service name -> container ID
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Definition      *api.AppDefinition `json:"definition,omitempty"`

	// Startup failure tracking for escalation (RFC 20260125)
	// After StartupEscalateAfterAttempts consecutive failures OR StartupEscalateAfterDuration,
	// status escalates from "starting" to "error".
	StartupAttempts       int        `json:"startup_attempts,omitempty"`
	FirstStartupFailureAt *time.Time `json:"first_startup_failure_at,omitempty"`
}

// Helper methods to access commonly used Definition fields safely

// AppName returns the app name from the definition, or empty string if nil.
func (a *AppInstance) AppName() string {
	if a.Definition == nil {
		return ""
	}
	return a.Definition.Name
}

// Image returns the image from the definition, or empty string if nil.
func (a *AppInstance) Image() string {
	return imageFromDefinition(a.Definition)
}

func imageFromDefinition(def *api.AppDefinition) string {
	if def == nil || def.Services == nil {
		return ""
	}
	primary := strings.TrimSpace(def.PrimaryService)
	if primary == "" {
		primary = defaultPrimaryServiceName
	}
	if svc, ok := def.Services[primary]; ok {
		return svc.Image
	}
	// Fallback to first service alphabetically if primary not found
	names := make([]string, 0, len(def.Services))
	for name := range def.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return def.Services[names[0]].Image
	}
	return ""
}

// Type returns the type from the definition, or empty string if nil.
func (a *AppInstance) Type() string {
	if a.Definition == nil {
		return ""
	}
	return a.Definition.Type
}

// Mode returns the piccolo mode from the definition extensions.
func (a *AppInstance) Mode() PiccoloMode {
	if a.Definition == nil {
		return ModeUnknown
	}
	return piccoloModeFromExtensions(a.Definition.Extensions)
}

// PrimaryContainerID returns the container ID of the primary service.
// This is the canonical method to get the primary container ID.
func (a *AppInstance) PrimaryContainerID() string {
	if a == nil || a.Containers == nil {
		return ""
	}
	primary := a.PrimaryService
	if primary == "" {
		primary = defaultPrimaryServiceName
	}
	return a.Containers[primary]
}

// SetPrimaryContainerID sets the container ID for the primary service.
func (a *AppInstance) SetPrimaryContainerID(cid string) {
	if a == nil {
		return
	}
	primary := a.PrimaryService
	if primary == "" {
		primary = defaultPrimaryServiceName
	}
	if a.Containers == nil {
		a.Containers = make(map[string]string)
	}
	a.Containers[primary] = cid
}

// PublishContainerID returns the container ID that owns published listener ports.
// For single-container apps this is the primary container; for multi-container apps it is the network anchor.
func (a *AppInstance) PublishContainerID() string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.NetworkAnchorID) != "" {
		return a.NetworkAnchorID
	}
	return a.PrimaryContainerID()
}
