package app

import (
	"context"
	"fmt"
	"sort"
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

	layout, err := m.observeAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	def := appInst.Definition
	if def == nil || def.Services == nil {
		return nil, fmt.Errorf("app %s has no valid definition", instanceID)
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeObserve)
	if err != nil {
		return nil, err
	}

	observed := m.observeContainerGroup(ctx, runtime, appInst, def)
	if !observed.known() {
		return nil, fmt.Errorf("container status observation unknown for %s: %w", instanceID, observed.Err)
	}
	if err := m.applyContainerGroupObservation(state, appInst, observed); err != nil {
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

	out := make([]ServiceContainerStatus, 0, len(names))
	for _, svcName := range names {
		service := observed.Services[svcName]

		out = append(out, ServiceContainerStatus{
			Service:     svcName,
			ContainerID: service.ID,
			Running:     service.State.Exists && service.State.Running,
		})
	}

	return out, nil
}
