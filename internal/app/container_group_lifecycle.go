package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

// startContainerGroup starts a container group (network anchor + service containers).
// This is the unified start path for both service and workspace modes.
func (m *AppManager) startContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("start: app definition required")
	}

	// Update status to starting immediately so UI reflects progress
	if err := m.updateStatusWithEvent(state, appInst.InstanceID, "starting"); err != nil {
		log.Printf("WARN: start %s: failed to persist starting status: %v", appInst.InstanceID, err)
	}

	mode := piccoloModeFromExtensions(def.Extensions)

	// For workspace mode, ensure workspace disk is mounted before starting containers
	if mode == ModeWorkspace {
		m.cleanupStaleWorkspaceMounts(ctx, appInst.InstanceID, layout)
		if _, err := m.ensureWorkspaceDiskMounted(ctx, appInst.InstanceID, layout); err != nil {
			_ = m.updateStatusWithEvent(state, appInst.InstanceID, "error")
			return fmt.Errorf("failed to mount workspace disk: %w", err)
		}
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

	for svcName := range def.Services {
		if strings.TrimSpace(appInst.Containers[svcName]) != "" {
			continue
		}
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" {
			if appInst.Containers == nil {
				appInst.Containers = make(map[string]string)
			}
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
		_ = m.updateStatusWithEvent(state, appInst.InstanceID, "error")
		return fmt.Errorf("failed to start network anchor: %w", err)
	}

	for _, svcName := range startOrder {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			_ = m.updateStatusWithEvent(state, appInst.InstanceID, "error")
			return fmt.Errorf("missing container ID for service '%s'", svcName)
		}
		if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
			_ = m.updateStatusWithEvent(state, appInst.InstanceID, "error")
			return fmt.Errorf("failed to start service '%s': %w", svcName, err)
		}
	}

	// Reset startup failure tracking and update status in a single persistence operation (RFC 20260125).
	// This avoids double disk writes and ensures atomic state transition.
	prevStatus := appInst.Status
	resetStartupTracking(appInst)
	appInst.Status = "running"
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst, nil); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}
	if prevStatus != "running" {
		m.publishAppStatusChanged(appInst.InstanceID, "running", prevStatus)
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

// stopContainerGroup stops a container group (network anchor + service containers).
// This is the unified stop path for both service and workspace modes.
func (m *AppManager) stopContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("stop: app definition required")
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	primary := primaryServiceFor(def, appInst)

	startOrder, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}

	// Stop services in reverse order, then anchor.
	for i := len(startOrder) - 1; i >= 0; i-- {
		// Check for context cancellation to avoid wasting time if shutdown is timing out
		if ctx.Err() != nil {
			log.Printf("WARN: stop %s: context cancelled, %d service containers not stopped", appInst.InstanceID, i+1)
			break
		}
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
		if err := m.containerManager.StopContainer(ctx, runtime, cid); err != nil {
			log.Printf("WARN: stop %s: failed to stop container %s (%s): %v", appInst.InstanceID, svcName, cid, err)
		}
	}

	// Check context before stopping anchor
	if ctx.Err() != nil {
		log.Printf("WARN: stop %s: context cancelled, network anchor not stopped", appInst.InstanceID)
		return ctx.Err()
	}

	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	if strings.TrimSpace(anchorID) != "" {
		if err := m.containerManager.StopContainer(ctx, runtime, anchorID); err != nil {
			log.Printf("WARN: stop %s: failed to stop network anchor %s: %v", appInst.InstanceID, anchorID, err)
		}
	}

	// For workspace mode apps, unmount the overlay on clean stop (RFC §5.6).
	// This is good practice but not strictly required since we remount on start.
	if mode == ModeWorkspace {
		if err := m.unmountWorkspaceDisk(ctx, appInst.InstanceID, layout); err != nil {
			// Log but don't fail - the data is safe, mount will be cleaned up on next start
			log.Printf("WARN: stop %s: failed to unmount workspace disk: %v", appInst.InstanceID, err)
		}
	}

	if err := m.updateStatusWithEvent(state, appInst.InstanceID, "stopped"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(appInst.InstanceID)
	}
	return nil
}

// uninstallContainerGroup removes a container group (network anchor + service containers).
// This is the unified uninstall path for both service and workspace modes.
// Note: This does not handle purge or state removal - those are handled by the caller.
func (m *AppManager) uninstallContainerGroup(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("uninstall: app definition required")
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	primary := primaryServiceFor(def, appInst)

	order, _ := serviceStartOrder(def.Services)

	// Stop containers in reverse order (best-effort).
	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			name := containerNameForService(appInst.InstanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
				cid = id
			}
		}
		if cid != "" {
			_ = m.containerManager.StopContainer(ctx, runtime, cid)
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

	// For workspace mode apps, unmount the workspace disk overlay.
	if mode == ModeWorkspace {
		if err := m.unmountWorkspaceDisk(ctx, appInst.InstanceID, layout); err != nil {
			log.Printf("WARN: workspace %s: failed to unmount workspace disk: %v", appInst.InstanceID, err)
		} else {
			log.Printf("INFO: workspace %s: unmounted workspace disk (data preserved)", appInst.InstanceID)
		}
	}

	// Remove containers in reverse order.
	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			name := containerNameForService(appInst.InstanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
				cid = id
			}
		}
		if cid != "" {
			_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		}
	}

	// Remove network anchor.
	if anchorID != "" {
		if err := m.containerManager.RemoveContainer(ctx, runtime, anchorID); err != nil {
			return fmt.Errorf("failed to remove network anchor: %w", err)
		}
	}

	// Remove service listeners for this app.
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(appInst.InstanceID)
	}

	return nil
}
