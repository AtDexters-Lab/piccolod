package app

import (
	"context"
	"io"
	"os/exec"
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
	ContainerID string `json:"container_id"`
	// Multi-container runtime metadata (service mode only).
	PrimaryService  string             `json:"primary_service,omitempty"`
	NetworkAnchorID string             `json:"network_anchor_id,omitempty"`
	Containers      map[string]string  `json:"containers,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	Definition      *api.AppDefinition `json:"definition,omitempty"`
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
	if a.Definition == nil {
		return ""
	}
	return a.Definition.Image
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

// PublishContainerID returns the container ID that owns published listener ports.
// For single-container apps this is the primary container; for multi-container apps it is the network anchor.
func (a *AppInstance) PublishContainerID() string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.NetworkAnchorID) != "" {
		return a.NetworkAnchorID
	}
	return a.ContainerID
}
