package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

func (m *AppManager) reconcileMultiContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, desiredRunning bool) error {
	if m.serviceManager == nil {
		return fmt.Errorf("app manager: service manager not configured")
	}
	if appInst == nil || def == nil || def.Services == nil {
		return fmt.Errorf("reconcile: invalid multi-container app state")
	}

	primary := primaryServiceFor(def, appInst)
	startOrder, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}

	expectedNames := make(map[string]struct{}, 1+len(def.Services))
	expectedNames[networkAnchorContainerName(appInst.InstanceID)] = struct{}{}
	for svcName := range def.Services {
		expectedNames[containerNameForService(appInst.InstanceID, svcName, primary)] = struct{}{}
	}
	m.pruneMultiContainerZombies(ctx, runtime, appInst.InstanceID, expectedNames)

	// Resolve anchor ID.
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil && strings.TrimSpace(id) != "" {
			anchorID = id
			appInst.NetworkAnchorID = id
			_ = state.StoreApp(appInst, nil)
		}
	}

	// Inspect anchor existence/running state.
	anchorState := container.ContainerState{}
	if anchorID != "" {
		if st, err := m.containerManager.InspectContainerState(ctx, runtime, anchorID); err == nil {
			anchorState = st
		} else {
			log.Printf("WARN: reconcile app %s: inspect anchor state failed: %v", appInst.InstanceID, err)
		}
	}

	if anchorID == "" || !anchorState.Exists {
		if !desiredRunning {
			// Stop all service containers even if the network anchor is missing (e.g., manually removed).
			_ = m.stopContainersForMultiApp(ctx, appInst, def, runtime)
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(appInst.InstanceID)
			}
			return nil
		}

		// The anchor is missing but we desire the app running. Stop and remove all service containers
		// so we can recreate the entire group (anchor + services) deterministically.
		_ = m.stopContainersForMultiApp(ctx, appInst, def, runtime)
		_ = m.removeContainersForMultiApp(ctx, appInst, def, runtime)
		m.serviceManager.RemoveApp(appInst.InstanceID)

		return m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime)
	}

	// If we don't desire running (stopped app or follower), stop all containers and remove proxies
	// but do not change persisted desired state (status field).
	if !desiredRunning {
		_ = m.stopContainersForMultiApp(ctx, appInst, def, runtime)
		if m.serviceManager != nil {
			m.serviceManager.RemoveApp(appInst.InstanceID)
		}
		return nil
	}

	// Ensure anchor running.
	if !anchorState.Running {
		if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
			_ = state.UpdateAppStatus(appInst.InstanceID, "error")
			return fmt.Errorf("failed to start network anchor: %w", err)
		}
	}

	m.serviceManager.SetAppContainerID(appInst.InstanceID, anchorID)

	if appInst.Containers == nil {
		appInst.Containers = make(map[string]string, len(def.Services))
	}

	// Ensure all declared services exist and are running.
	for _, svcName := range startOrder {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			name := containerNameForService(appInst.InstanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" {
				cid = id
				appInst.Containers[svcName] = id
				_ = state.StoreApp(appInst, nil)
			}
		}

		st := container.ContainerState{}
		if cid != "" {
			if observed, err := m.containerManager.InspectContainerState(ctx, runtime, cid); err == nil {
				st = observed
			} else {
				log.Printf("WARN: reconcile app %s: inspect service state failed service=%s: %v", appInst.InstanceID, svcName, err)
			}
		}

		if cid == "" || !st.Exists {
			newCID, err := m.createAndStartServiceContainer(ctx, runtime, layout, def, appInst.InstanceID, primary, svcName, anchorID)
			if err != nil {
				_ = state.UpdateAppStatus(appInst.InstanceID, "error")
				return err
			}
			appInst.Containers[svcName] = newCID
			if svcName == primary {
				appInst.ContainerID = newCID
			}
			_ = state.StoreApp(appInst, nil)
			continue
		}

		if !st.Running {
			if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
				_ = state.UpdateAppStatus(appInst.InstanceID, "error")
				return fmt.Errorf("failed to start service '%s': %w", svcName, err)
			}
		}
	}

	if appInst.Status != "running" {
		_ = state.UpdateAppStatus(appInst.InstanceID, "running")
	}

	// Restore endpoints/proxies and ensure published ports match our expected allocations.
	if err := m.ensureServicesForRunningApp(ctx, def, appInst.InstanceID, anchorID, runtime); err != nil {
		log.Printf("WARN: reconcile app %s: restore services failed: %v", appInst.InstanceID, err)
	}
	if err := m.ensurePodmanPublishes(ctx, def, appInst.InstanceID, anchorID, runtime); err != nil {
		if errors.Is(err, container.ErrPortReconciliationRequired) {
			log.Printf("INFO: reconcile app %s: port bindings mismatch, recreating containers", appInst.InstanceID)
			_ = m.stopContainersForMultiApp(ctx, appInst, def, runtime)
			_ = m.removeContainersForMultiApp(ctx, appInst, def, runtime)
			m.serviceManager.RemoveApp(appInst.InstanceID)
			return m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime)
		}
		log.Printf("WARN: reconcile app %s: publish reconcile failed: %v", appInst.InstanceID, err)
	}

	return nil
}

func (m *AppManager) pruneMultiContainerZombies(ctx context.Context, runtime container.PodmanRuntime, instanceID string, expectedNames map[string]struct{}) {
	if m.containerManager == nil {
		return
	}

	items, err := m.containerManager.ListContainersByLabel(ctx, runtime, "io.piccolo.instance", instanceID)
	if err != nil {
		log.Printf("WARN: reconcile app %s: list containers by label failed: %v", instanceID, err)
		return
	}

	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		if _, ok := expectedNames[name]; ok {
			continue
		}
		log.Printf("INFO: reconcile app %s: pruning zombie container %s (name=%s)", instanceID, item.ID, name)
		_ = m.containerManager.StopContainer(ctx, runtime, item.ID)
		_ = m.containerManager.RemoveContainer(ctx, runtime, item.ID)
	}
}

func (m *AppManager) recreateMissingMultiContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	// Allocate endpoints and recreate the group (anchor + services).
	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		endpoints, err := m.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners, getAuthStrategy(def))
		if err != nil {
			return fmt.Errorf("allocate service ports: %w", err)
		}

		newInst, err := m.installMultiContainer(ctx, def, appInst.InstanceID, appInst.DisplayName, layout, runtime, endpoints)
		if err == nil {
			// Preserve timestamps.
			newInst.CreatedAt = appInst.CreatedAt
			newInst.UpdatedAt = time.Now()
			appInst.ContainerID = newInst.ContainerID
			appInst.PrimaryService = newInst.PrimaryService
			appInst.NetworkAnchorID = newInst.NetworkAnchorID
			appInst.Containers = newInst.Containers
			appInst.Status = "running"
			_ = state.StoreApp(appInst, nil)
			return nil
		}

		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			log.Printf("WARN: recreate app %s: host port conflict port=%d attempt=%d", appInst.InstanceID, portErr.Port, attempt)
			if portErr.Port > 0 {
				_ = m.serviceManager.ReserveHostPort(portErr.Port)
			} else {
				for _, ep := range endpoints {
					_ = m.serviceManager.ReserveHostPort(ep.HostBind)
				}
			}
			m.serviceManager.RemoveApp(appInst.InstanceID)
			continue
		}

		m.serviceManager.RemoveApp(appInst.InstanceID)
		return err
	}

	return fmt.Errorf("failed to recreate %s: exhausted host-port retries", appInst.InstanceID)
}

func (m *AppManager) createAndStartServiceContainer(ctx context.Context, runtime container.PodmanRuntime, layout appVolumeLayout, def *api.AppDefinition, instanceID, primary, svcName, anchorID string) (string, error) {
	spec, err := m.buildServiceContainerSpec(layout, def, instanceID, primary, svcName, anchorID)
	if err != nil {
		return "", err
	}

	var cid string
	for i := 0; i < 2; i++ {
		cid, err = m.containerManager.CreateContainer(ctx, runtime, spec)
		if err == nil {
			break
		}
		zombieID := ""
		var nameErr *container.NameInUseError
		if errors.As(err, &nameErr) {
			zombieID = nameErr.ID
		} else if id, resolveErr := m.containerManager.ResolveContainerIDByName(ctx, runtime, spec.Name); resolveErr == nil {
			zombieID = id
		}
		if zombieID != "" {
			log.Printf("INFO: recreate %s: removing zombie container %s (service=%s)", instanceID, zombieID, svcName)
			_ = m.containerManager.RemoveContainer(ctx, runtime, zombieID)
			continue
		}
		break
	}
	if err != nil {
		return "", fmt.Errorf("create service container '%s': %w", svcName, err)
	}
	if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
		_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		return "", fmt.Errorf("start service container '%s': %w", svcName, err)
	}
	return cid, nil
}

func (m *AppManager) stopContainersForMultiApp(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	primary := primaryServiceFor(def, appInst)
	order, _ := serviceStartOrder(def.Services)
	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		stored := strings.TrimSpace(appInst.Containers[svcName])
		if stored != "" {
			_ = m.containerManager.StopContainer(ctx, runtime, stored)
		}
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" && id != stored {
			_ = m.containerManager.StopContainer(ctx, runtime, id)
		}
	}
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	if anchorID != "" {
		_ = m.containerManager.StopContainer(ctx, runtime, anchorID)
	}
	return nil
}

func (m *AppManager) removeContainersForMultiApp(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	primary := primaryServiceFor(def, appInst)
	order, _ := serviceStartOrder(def.Services)
	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		stored := strings.TrimSpace(appInst.Containers[svcName])
		if stored != "" {
			_ = m.containerManager.RemoveContainer(ctx, runtime, stored)
		}
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" && id != stored {
			_ = m.containerManager.RemoveContainer(ctx, runtime, id)
		}
	}
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	if anchorID != "" {
		_ = m.containerManager.RemoveContainer(ctx, runtime, anchorID)
	}
	return nil
}
