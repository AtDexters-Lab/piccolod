package app

import (
	"context"
	"time"

	"piccolod/internal/container"
)

// ContainerManager describes the container runtime operations required by the app manager.
type ContainerManager interface {
	CreateContainer(ctx context.Context, runtime container.PodmanRuntime, spec container.ContainerCreateSpec) (string, error)
	StartContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	StopContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	RemoveContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	PullImage(ctx context.Context, runtime container.PodmanRuntime, image string) error
	Logs(ctx context.Context, runtime container.PodmanRuntime, containerID string, lines int) ([]string, error)
	ResolveContainerIDByName(ctx context.Context, runtime container.PodmanRuntime, name string) (string, error)
	InspectContainerState(ctx context.Context, runtime container.PodmanRuntime, containerID string) (container.ContainerState, error)
	InspectPublishedPorts(ctx context.Context, runtime container.PodmanRuntime, containerID string) (map[int]int, error)
	UpdatePublishAdd(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error
	UpdatePublishRemove(ctx context.Context, runtime container.PodmanRuntime, containerID string, hostBind, guestPort int) error
}

// AppInstance captures the runtime metadata for an installed application.
type AppInstance struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	ContainerID string            `json:"container_id"`
	Environment map[string]string `json:"environment,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}
