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

// App status constants represent the observed runtime state of an application.
//
// Design: Intent vs Observed State
//
// This package separates two concepts:
//
//  1. Intent (Enabled bool) - Persisted in metadata.json. Answers: "Should this app run?"
//     - Enabled=true: User wants the app running (will auto-start on boot/leader election)
//     - Enabled=false: User explicitly stopped the app (stays stopped)
//
//  2. Observed Status (in-memory map) - Never persisted. Answers: "What is the app doing now?"
//     - Reflects local container state on this machine
//     - Published via event bus for real-time UI updates
//     - Returned in API responses
//
// Reconciliation logic:
// The reconciler starts/stops containers based on BOTH Enabled AND leadership:
//   - desiredRunning = Enabled && (isLeaderForApp)
// This ensures containers only run on the leader node while preserving user intent across failovers.
const (
	StatusRunning  = "running"  // Containers are running and healthy
	StatusStopped  = "stopped"  // Containers are stopped locally (Enabled may still be true on follower nodes)
	StatusStarting = "starting" // Containers are being started or recovering
	StatusError    = "error"    // Startup failed after escalation threshold
)

// ContainerManager describes the container runtime operations required by the app manager.
type ContainerManager interface {
	CreateContainer(ctx context.Context, runtime container.PodmanRuntime, spec container.ContainerCreateSpec) (string, error)
	StartContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	StopContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	RemoveContainer(ctx context.Context, runtime container.PodmanRuntime, containerID string) error
	ListContainersByLabel(ctx context.Context, runtime container.PodmanRuntime, labelKey, labelValue string) ([]container.ContainerListItem, error)
	PullImage(ctx context.Context, runtime container.PodmanRuntime, image string) error
	// PullImageWithProgress pulls an image with real-time progress callbacks.
	PullImageWithProgress(ctx context.Context, runtime container.PodmanRuntime, image string, callback container.ImagePullCallback) error
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
	// NetworkReload tears down and re-creates network configuration for a running container.
	NetworkReload(ctx context.Context, runtime container.PodmanRuntime, containerNameOrID string) error
	// ExecShellCmd returns an exec.Cmd for running a shell inside a container.
	ExecShellCmd(runtime container.PodmanRuntime, containerID string) (*exec.Cmd, error)
}

// AppInstance captures the runtime metadata for an installed application instance.
// InstanceID is the unique key derived from the primary listener name or workspace_name (RFC 20260130).
// Definition contains the full app manifest (image, type, listeners, extensions, etc).
type AppInstance struct {
	InstanceID    string `json:"instance_id"`
	Enabled       bool   `json:"enabled"`
	Status        string `json:"status"`                          // observed runtime status, not persisted
	StatusMessage string `json:"status_message,omitempty"`        // transient status context for UI, not persisted
	// Container runtime metadata.
	PrimaryService  string            `json:"primary_service,omitempty"`
	NetworkAnchorID string            `json:"network_anchor_id,omitempty"`
	Containers      map[string]string `json:"containers,omitempty"` // service name -> container ID
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Definition      *api.AppDefinition `json:"definition,omitempty"`

	// CatalogSource tracks the catalog item name this app was installed from.
	// Used for icon lookup and update tracking.
	CatalogSource string `json:"catalog_source,omitempty"`

	// ActiveRootfs tracks the active rootfs volume ID per service (RFC 20260302).
	// nil = legacy install (use ServiceRootfsVolumeID without digest).
	// Updated during image update to point to the new versioned rootfs.
	ActiveRootfs map[string]string `json:"active_rootfs,omitempty"`

	// ClonedFrom tracks the origin instance ID this app was cloned from (RFC 20260302).
	ClonedFrom string `json:"cloned_from,omitempty"`

	// Startup failure tracking for escalation (RFC 20260125)
	// After StartupEscalateAfterAttempts consecutive failures OR StartupEscalateAfterDuration,
	// status escalates from "starting" to "error".
	StartupAttempts       int        `json:"startup_attempts,omitempty"`
	FirstStartupFailureAt *time.Time `json:"first_startup_failure_at,omitempty"`
}

// Helper methods to access commonly used Definition fields safely

// AppName returns the app's canonical name, which is the instanceID.
// Per RFC 20260130, the instanceID is derived from the primary listener name
// or workspace_name, making it the app's identity.
func (a *AppInstance) AppName() string {
	return a.InstanceID
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
