package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/api"
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
	if def != nil && def.Type == "system" {
		return "always"
	}
	return ""
}

func appNetworkMode(def *api.AppDefinition) string {
	if def != nil && def.Permissions != nil && def.Permissions.Network != nil {
		if def.Permissions.Network.Internet == "deny" {
			return "none"
		}
	}
	return ""
}

func (m *AppManager) buildServiceContainerSpec(layout appVolumeLayout, def *api.AppDefinition, instanceID, primary, svcName, anchorID string) (container.ContainerCreateSpec, error) {
	if def == nil || def.Services == nil {
		return container.ContainerCreateSpec{}, fmt.Errorf("service container spec requires app definition services")
	}
	svc, ok := def.Services[svcName]
	if !ok {
		return container.ContainerCreateSpec{}, fmt.Errorf("unknown service '%s'", svcName)
	}
	if strings.TrimSpace(anchorID) == "" {
		return container.ContainerCreateSpec{}, fmt.Errorf("service container spec requires network anchor id")
	}

	spec := container.ContainerCreateSpec{
		Name:          containerNameForService(instanceID, svcName, primary),
		Image:         svc.Image,
		Environment:   svc.Environment,
		NetworkMode:   fmt.Sprintf("container:%s", anchorID),
		RestartPolicy: appRestartPolicy(def),
		Labels:        piccoloLabels(instanceID, svcName, "service"),
	}
	if svc.Resources != nil && svc.Resources.Limits != nil {
		spec.Resources = container.ResourceLimits{
			Memory: svc.Resources.Limits.Memory,
			CPU:    fmt.Sprintf("%.1f", svc.Resources.Limits.CPU),
		}
	}
	if err := m.applyServiceStorageAndTmpfs(&spec, svc.Storage, layout, def.Extensions); err != nil {
		return container.ContainerCreateSpec{}, err
	}
	m.applyAuthInjection(&spec, def)
	if err := container.ValidateContainerSpec(spec); err != nil {
		return container.ContainerCreateSpec{}, fmt.Errorf("invalid service container spec for '%s': %w", svcName, err)
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
