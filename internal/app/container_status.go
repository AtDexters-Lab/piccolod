package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ServiceContainerStatus captures per-service container identity and observed running state.
// This is intended for client-side service selectors (logs/exec) on multi-container apps.
type ServiceContainerStatus struct {
	Service     string `json:"service"`
	ContainerID string `json:"container_id"`
	Running     bool   `json:"running"`
}

// ContainerStatuses returns per-service container status for an app instance.
func (m *AppManager) ContainerStatuses(ctx context.Context, instanceID string) ([]ServiceContainerStatus, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	def := appInst.Definition
	if def == nil || def.Services == nil {
		return nil, fmt.Errorf("app %s has no valid definition", instanceID)
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return nil, err
	}

	primary := primaryServiceFor(def, appInst)

	names := make([]string, 0, len(def.Services))
	for name := range def.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	if primary != "" {
		for i, name := range names {
			if name == primary {
				names = append([]string{primary}, append(names[:i], names[i+1:]...)...)
				break
			}
		}
	}

	changed := false
	out := make([]ServiceContainerStatus, 0, len(names))
	for _, svcName := range names {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			name := containerNameForService(instanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" {
				cid = id
				if appInst.Containers == nil {
					appInst.Containers = make(map[string]string)
				}
				appInst.Containers[svcName] = id
				if svcName == primary {
					appInst.ContainerID = id
				}
				changed = true
			}
		}

		running := false
		if cid != "" {
			if st, err := m.containerManager.InspectContainerState(ctx, runtime, cid); err == nil && st.Exists {
				running = st.Running
			}
		}

		out = append(out, ServiceContainerStatus{
			Service:     svcName,
			ContainerID: cid,
			Running:     running,
		})
	}

	if changed {
		_ = state.StoreApp(appInst, nil)
	}

	return out, nil
}
