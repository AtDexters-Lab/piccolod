package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/app/workspacedisk"
	"piccolod/internal/container"
)

const (
	networkAnchorServiceName = "__netns__"
)

const defaultNetworkAnchorImage = "registry.k8s.io/pause:3.9"

func networkAnchorImage() string {
	if raw := strings.TrimSpace(os.Getenv("PICCOLO_NETNS_IMAGE")); raw != "" {
		return raw
	}
	return defaultNetworkAnchorImage
}

func networkAnchorContainerName(instanceID string) string {
	return fmt.Sprintf("%s__netns__", instanceID)
}

func containerNameForService(instanceID, serviceName, primaryService string) string {
	if strings.TrimSpace(serviceName) == "" {
		return instanceID
	}
	if serviceName == primaryService {
		return instanceID
	}
	return fmt.Sprintf("%s__%s", instanceID, serviceName)
}

func primaryServiceFor(def *api.AppDefinition, inst *AppInstance) string {
	if inst != nil && strings.TrimSpace(inst.PrimaryService) != "" {
		return inst.PrimaryService
	}
	if def != nil && strings.TrimSpace(def.PrimaryService) != "" {
		return strings.TrimSpace(def.PrimaryService)
	}
	if def != nil && def.Services != nil {
		if piccoloModeFromExtensions(def.Extensions) == ModeWorkspace && len(def.Services) == 1 {
			for name := range def.Services {
				return name
			}
		}
		return defaultPrimaryServiceName
	}
	return ""
}

func piccoloLabels(instanceID, serviceName, role string) map[string]string {
	return map[string]string{
		"io.piccolo.instance": instanceID,
		"io.piccolo.service":  serviceName,
		"io.piccolo.role":     role,
	}
}

func appRestartPolicy(def *api.AppDefinition) string {
	// Always return "no" - piccolod is the sole lifecycle manager for all containers.
	// We don't want Podman to auto-restart containers outside of piccolod's control.
	return "no"
}

func appNetworkMode(def *api.AppDefinition) string {
	if def != nil && def.Permissions != nil && def.Permissions.Network != nil {
		if def.Permissions.Network.Internet == "deny" {
			return "none"
		}
	}
	return ""
}

// serviceContainerOptions holds all parameters for building a service container spec.
// The workspace fields are optional and only used for workspace mode apps.
type serviceContainerOptions struct {
	layout     appVolumeLayout
	appDef     *api.AppDefinition
	instanceID string
	primary    string
	svcName    string
	anchorID   string

	// Workspace mode fields (optional, empty for service mode)
	mergedRootfs  string                       // Path to mounted workspace disk rootfs
	workspaceMeta *workspacedisk.WorkspaceMeta // Workspace metadata with image config
}

// buildServiceContainerSpec builds a container spec for a service container.
// For workspace mode (when workspaceMeta is provided), it configures --rootfs mode
// and applies the saved image config. For service mode, it uses the service image directly.
func (m *AppManager) buildServiceContainerSpec(opts serviceContainerOptions) (container.ContainerCreateSpec, error) {
	if opts.appDef == nil || opts.appDef.Services == nil {
		return container.ContainerCreateSpec{}, fmt.Errorf("service container spec requires app definition services")
	}
	svc, ok := opts.appDef.Services[opts.svcName]
	if !ok {
		return container.ContainerCreateSpec{}, fmt.Errorf("unknown service '%s'", opts.svcName)
	}
	if strings.TrimSpace(opts.anchorID) == "" {
		return container.ContainerCreateSpec{}, fmt.Errorf("service container spec requires network anchor id")
	}

	spec := container.ContainerCreateSpec{
		Name:          containerNameForService(opts.instanceID, opts.svcName, opts.primary),
		Image:         svc.Image,
		Environment:   svc.Environment,
		NetworkMode:   fmt.Sprintf("container:%s", opts.anchorID),
		RestartPolicy: appRestartPolicy(opts.appDef),
		Labels:        piccoloLabels(opts.instanceID, opts.svcName, "service"),
	}

	// Apply workspace mode configuration if provided
	if opts.workspaceMeta != nil && opts.mergedRootfs != "" {
		// Use --rootfs mode instead of image
		spec.Rootfs = opts.mergedRootfs
		spec.Image = ""

		// Apply image config since Podman doesn't do it in --rootfs mode.
		// Base image env is merged with manifest env (manifest takes precedence).
		spec.Environment = mergeEnvMaps(parseEnvSlice(opts.workspaceMeta.ImageConfig.Env), spec.Environment)
		spec.WorkingDir = opts.workspaceMeta.ImageConfig.WorkingDir
		spec.User = opts.workspaceMeta.ImageConfig.User

		if svc.Init == "image" {
			// Image manages its own init (e.g., s6-overlay). Let it be PID 1.
			spec.Entrypoint = opts.workspaceMeta.ImageConfig.Entrypoint
			spec.Command = opts.workspaceMeta.ImageConfig.Cmd
		} else {
			// Default: Piccolo manages init via catatonit + boot.sh wrapper
			originalCmd := opts.workspaceMeta.ImageConfig.BuildOriginalCommand()
			spec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			spec.Command = originalCmd
			spec.UseInit = true

			// Ensure workspace assets (boot.sh, piccolo-startup) exist on host filesystem
			if err := EnsureWorkspaceAssets(); err != nil {
				return container.ContainerCreateSpec{}, fmt.Errorf("failed to ensure workspace assets: %w", err)
			}

			// Mount boot.sh as read-only into the container
			// Use :z for SELinux shared label (required for rootless podman on SELinux systems)
			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host:      BootShHostPath(),
				Container: "/piccolo/boot.sh",
				Options:   "ro,z",
			})

			// Mount piccolo-startup helper to /usr/local/bin (which is in PATH by default)
			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host:      PiccoloStartupHostPath(),
				Container: "/usr/local/bin/piccolo-startup",
				Options:   "ro,z",
			})
		}

		// Mount a writable config directory for user startup hooks (start.sh)
		// This directory is persistent and writable by the container user.
		// Use 0o755 on host; the :U mount option handles UID mapping for rootless containers.
		configDir := filepath.Join(opts.layout.DataDir, "piccolo-config")
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			return container.ContainerCreateSpec{}, fmt.Errorf("failed to create piccolo config dir: %w", err)
		}
		spec.Volumes = append(spec.Volumes, container.VolumeMapping{
			Host:      configDir,
			Container: "/piccolo/config",
			Options:   "rw,U,z", // U for rootless UID mapping
		})
	}

	if svc.Resources != nil && svc.Resources.Limits != nil {
		spec.Resources = container.ResourceLimits{
			Memory: svc.Resources.Limits.Memory,
			CPU:    fmt.Sprintf("%.1f", svc.Resources.Limits.CPU),
		}
	}
	if err := m.applyServiceStorageAndTmpfs(&spec, svc.Storage, opts.layout, opts.appDef.Extensions); err != nil {
		return container.ContainerCreateSpec{}, err
	}
	m.applyOIDCClientInjection(&spec, svc.OIDCClient)
	if err := container.ValidateContainerSpec(spec); err != nil {
		return container.ContainerCreateSpec{}, fmt.Errorf("invalid service container spec for '%s': %w", opts.svcName, err)
	}

	return spec, nil
}

func (m *AppManager) applyServiceStorageAndTmpfs(spec *container.ContainerCreateSpec, storage *api.AppStorage, layout appVolumeLayout, extensions map[string]interface{}) error {
	if spec == nil {
		return nil
	}
	if layout.DataDir == "" {
		return fmt.Errorf("container spec requires app volume layout")
	}

	mountedPaths := map[string]struct{}{}

	if storage != nil {
		for volName, vol := range storage.Persistent {
			host := filepath.Join(layout.DataDir, volName)
			if err := ensureDir(host, 0o777); err != nil {
				return fmt.Errorf("ensure persistent volume '%s': %w", volName, err)
			}
			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host:      host,
				Container: vol.Container,
				Options:   "rw,U",
			})
			mountedPaths[vol.Container] = struct{}{}
		}
		for _, vol := range storage.Temporary {
			mountedPaths[vol.Container] = struct{}{}
		}
	}

	// Canonical tmpfs mounts: honor x-piccolo.tmpfs when present, otherwise defaults.
	for _, p := range tmpfsMountsFromExtensions(extensions) {
		if _, ok := mountedPaths[p]; ok {
			continue
		}
		spec.Tmpfs = append(spec.Tmpfs, container.TmpfsMount{Container: p})
	}

	if storage != nil {
		for _, vol := range storage.Temporary {
			opts := "rw"
			if vol.SizeLimit != "" {
				if sizeOpt := tmpfsSizeOpt(vol.SizeLimit); sizeOpt != "" {
					opts = opts + "," + sizeOpt
				}
			}
			spec.Tmpfs = append(spec.Tmpfs, container.TmpfsMount{
				Container: vol.Container,
				Options:   opts,
			})
		}
	}

	return nil
}
