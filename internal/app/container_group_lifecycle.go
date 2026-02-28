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

// startContainerGroup starts a container group (network anchor + service containers).
// This is the unified start path for both service and workspace modes.
func (m *AppManager) startContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("start: app definition required")
	}

	// Set Enabled=true and persist
	appInst.Enabled = true
	if err := state.UpdateAppEnabled(appInst.InstanceID, true); err != nil {
		log.Printf("WARN: start %s: failed to persist enabled state: %v", appInst.InstanceID, err)
	}

	// Update status to starting immediately so UI reflects progress
	m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusStarting, "Starting containers")

	mode := piccoloModeFromExtensions(def.Extensions)

	// Ensure rootfs is attached before starting containers.
	// ensureRootfsAttached returns (nil, nil) for legacy apps without rootfs volumes.
	var blockNativeRootfs *rootfsMountInfo
	rInfo, err := m.ensureRootfsAttached(ctx, appInst.InstanceID, mode)
	if err != nil {
		m.updateStatusWithEvent(appInst.InstanceID, StatusError)
		return fmt.Errorf("failed to attach rootfs: %w", err)
	}
	if rInfo != nil {
		// Block-native path: read image config for the rootfs.
		imgConfig, cfgErr := m.readImageConfigForRootfsFromInstance(ctx, appInst)
		if cfgErr != nil {
			m.updateStatusWithEvent(appInst.InstanceID, StatusError)
			return fmt.Errorf("failed to read rootfs image config: %w", cfgErr)
		}
		rInfo.imgConfig = imgConfig
		blockNativeRootfs = rInfo
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
		if err := state.StoreAppMetadata(appInst); err != nil {
			log.Printf("WARN: start %s: failed to persist repaired container IDs: %v", appInst.InstanceID, err)
		}
	}

	// Start anchor first, then services in order.
	if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
		// Start failed — attempt recreation of the entire container group.
		log.Printf("INFO: start %s: anchor start failed (%v), recreating", appInst.InstanceID, err)
		if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
			"anchor start failed during manual start, recreating"); recoverErr != nil {
			m.updateStatusWithEvent(appInst.InstanceID, StatusError)
			return recoverErr
		}
		m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
		return nil
	}

	for _, svcName := range startOrder {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			m.updateStatusWithEvent(appInst.InstanceID, StatusError)
			return fmt.Errorf("missing container ID for service '%s'", svcName)
		}
		if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
			log.Printf("INFO: start %s: service '%s' start failed (%v), recreating",
				appInst.InstanceID, svcName, err)

			opts := serviceContainerOptions{
				layout:     layout,
				appDef:     def,
				instanceID: appInst.InstanceID,
				primary:    primary,
				svcName:    svcName,
				anchorID:   anchorID,
				credential: runtime.Credential,
			}
			if blockNativeRootfs != nil {
				opts.rootfsHandle = &blockNativeRootfs.handle
				opts.goldenImgConfig = &blockNativeRootfs.imgConfig
			}
			if err := m.recreateServiceContainer(ctx, state, appInst, runtime, cid, opts); err != nil {
				m.updateStatusWithEvent(appInst.InstanceID, StatusError)
				return err
			}
			continue
		}
	}

	// Reset startup failure tracking and persist.
	resetStartupTracking(appInst)
	appInst.UpdatedAt = time.Now()
	if err := state.StoreAppMetadata(appInst); err != nil {
		return fmt.Errorf("failed to update app metadata: %w", err)
	}
	m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)

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

// stopContainerGroupOpts contains options for stopping a container group.
type stopContainerGroupOpts struct {
	// ShutdownMode indicates this is a graceful shutdown operation.
	// When true, status update failures are logged but don't cause the stop to fail,
	// since the control volume may become unavailable during shutdown.
	ShutdownMode bool
}

// stopContainerGroup stops a container group (network anchor + service containers).
// This is the unified stop path for both service and workspace modes.
func (m *AppManager) stopContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	return m.stopContainerGroupWithOpts(ctx, state, appInst, def, layout, runtime, stopContainerGroupOpts{})
}

// stopContainerGroupWithOpts stops a container group with configurable options.
func (m *AppManager) stopContainerGroupWithOpts(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, opts stopContainerGroupOpts) error {
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

	// Detach rootfs on clean stop.
	if m.appHasBlockNativeRootfs(appInst.InstanceID, mode) {
		m.detachAppRootfs(ctx, appInst.InstanceID, mode)
	}

	// For explicit user-initiated stops, set Enabled=false and persist.
	// During graceful shutdown, keep Enabled unchanged so apps auto-start on service restart.
	if !opts.ShutdownMode {
		appInst.Enabled = false
		if err := state.UpdateAppEnabled(appInst.InstanceID, false); err != nil {
			return fmt.Errorf("failed to persist disabled state: %w", err)
		}
	} else {
		log.Printf("DEBUG: stop %s: preserving enabled state for auto-restart on service resume", appInst.InstanceID)
	}
	m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(appInst.InstanceID)
	}
	return nil
}

// uninstallContainerGroup removes a container group (network anchor + service containers).
// This is the unified uninstall path for both service and workspace modes.
// Note: This does not handle volume destruction or state removal - those are handled by the caller.
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

	// Detach rootfs before container removal.
	if m.appHasBlockNativeRootfs(appInst.InstanceID, mode) {
		m.detachAppRootfs(ctx, appInst.InstanceID, mode)
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

	// Remove network anchor. Treat "not found" as success since the goal is removal.
	if anchorID != "" {
		if err := m.containerManager.RemoveContainer(ctx, runtime, anchorID); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				return fmt.Errorf("failed to remove network anchor: %w", err)
			}
			log.Printf("INFO: uninstall %s: network anchor %s already removed", appInst.InstanceID, anchorID)
		}
	}

	// Remove service listeners for this app.
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(appInst.InstanceID)
	}

	return nil
}
