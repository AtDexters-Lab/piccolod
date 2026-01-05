package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func (m *AppManager) startMultiContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("start: app definition required")
	}
	primary := primaryServiceFor(def, appInst)

	startOrder, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}

	// Resolve missing container IDs by deterministic names (best-effort).
	changed := false
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil && strings.TrimSpace(id) != "" {
			anchorID = id
			appInst.NetworkAnchorID = id
			changed = true
		}
	}
	if anchorID == "" {
		return fmt.Errorf("start: network anchor container missing for %s", appInst.InstanceID)
	}

	if appInst.Containers == nil {
		appInst.Containers = make(map[string]string, len(def.Services))
		changed = true
	}
	for svcName := range def.Services {
		if strings.TrimSpace(appInst.Containers[svcName]) != "" {
			continue
		}
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" {
			appInst.Containers[svcName] = id
			changed = true
		}
	}

	// Persist repaired metadata before starting.
	if changed {
		if err := state.StoreApp(appInst, nil); err != nil {
			log.Printf("WARN: start %s: failed to persist repaired container IDs: %v", appInst.InstanceID, err)
		}
	}

	// Start anchor first, then services in order.
	if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
		_ = state.UpdateAppStatus(appInst.InstanceID, "error")
		return fmt.Errorf("failed to start network anchor: %w", err)
	}

	for _, svcName := range startOrder {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			_ = state.UpdateAppStatus(appInst.InstanceID, "error")
			return fmt.Errorf("missing container ID for service '%s'", svcName)
		}
		if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
			_ = state.UpdateAppStatus(appInst.InstanceID, "error")
			return fmt.Errorf("failed to start service '%s': %w", svcName, err)
		}
	}

	// Update status to running.
	if err := state.UpdateAppStatus(appInst.InstanceID, "running"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}

	// Rehydrate service proxies if they were removed while the app was stopped.
	if m.serviceManager != nil {
		if _, err := m.serviceManager.GetByApp(appInst.InstanceID); err != nil {
			ports, portErr := m.containerManager.InspectPublishedPorts(ctx, runtime, anchorID)
			if portErr != nil {
				log.Printf("WARN: start app %s: inspect ports failed: %v", appInst.InstanceID, portErr)
			} else if len(ports) == 0 {
				log.Printf("WARN: start app %s: no published ports found during restore", appInst.InstanceID)
			} else {
				if _, restoreErr := m.serviceManager.RestoreFromPodman(appInst.InstanceID, def.Listeners, ports); restoreErr != nil {
					log.Printf("WARN: start app %s: failed to restore services: %v", appInst.InstanceID, restoreErr)
				} else {
					m.serviceManager.SetAppContainerID(appInst.InstanceID, anchorID)
				}
			}
		}
	}

	return nil
}

func (m *AppManager) stopMultiContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("stop: app definition required")
	}
	primary := primaryServiceFor(def, appInst)

	startOrder, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}

	// Stop services in reverse order, then anchor.
	for i := len(startOrder) - 1; i >= 0; i-- {
		svcName := startOrder[i]
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			// Best-effort resolve by name.
			name := containerNameForService(appInst.InstanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
				cid = id
			}
		}
		if strings.TrimSpace(cid) == "" {
			continue
		}
		_ = m.containerManager.StopContainer(ctx, runtime, cid)
	}

	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	if strings.TrimSpace(anchorID) != "" {
		_ = m.containerManager.StopContainer(ctx, runtime, anchorID)
	}

	if err := state.UpdateAppStatus(appInst.InstanceID, "stopped"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(appInst.InstanceID)
	}
	return nil
}
