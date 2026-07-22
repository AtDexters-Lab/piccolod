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

	primary := primaryServiceFor(def, appInst)
	startOrder, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}

	observed := m.observeContainerGroup(ctx, runtime, appInst, def)
	if !observed.known() {
		return fmt.Errorf("start: container group observation unknown: %w", observed.Err)
	}
	if err := m.applyContainerGroupObservation(state, appInst, observed); err != nil {
		return err
	}
	anchorID := observed.Anchor.ID

	// Only a complete known observation may project Starting or attach rootfs.
	m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusStarting, "Starting containers")
	mode := piccoloModeFromExtensions(def.Extensions)
	blockNativeRootfsMap, err := m.ensureAllServiceRootfsAttached(ctx, appInst.InstanceID, mode, def, appInst)
	if err != nil {
		m.updateStatusWithEvent(appInst.InstanceID, StatusError)
		return fmt.Errorf("failed to attach rootfs: %w", err)
	}

	// Podman may retain running metadata after the container PID has died or
	// been reused. In that state `podman start` is a successful no-op, so manual
	// Start must use the same strict whole-group recovery as reconciliation.
	if observed.Outcome == containerGroupStale || observed.Outcome == containerGroupMissing {
		reason := "container group is authoritatively incomplete during manual start"
		if observed.Outcome == containerGroupStale {
			reason = "container process no longer belongs to its libpod cgroup during manual start"
		}
		if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
			reason, blockNativeRootfsMap); recoverErr != nil {
			m.updateStatusWithEvent(appInst.InstanceID, StatusError)
			return recoverErr
		}
		m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
		return nil
	}

	// Start anchor first, then services in order.
	if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
		// Start failed — attempt recreation of the entire container group.
		log.Printf("INFO: start %s: anchor start failed (%v), recreating", appInst.InstanceID, err)
		if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
			"anchor start failed during manual start, recreating", blockNativeRootfsMap); recoverErr != nil {
			m.updateStatusWithEvent(appInst.InstanceID, StatusError)
			return recoverErr
		}
		m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
		return nil
	}

	for _, svcName := range startOrder {
		cid := observed.Services[svcName].ID
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
			if svcRootfs, ok := blockNativeRootfsMap[svcName]; ok {
				opts.rootfsHandle = &svcRootfs.handle
				opts.goldenImgConfig = &svcRootfs.imgConfig
			}
			if err := m.recreateServiceContainer(ctx, state, appInst, runtime, cid, opts); err != nil {
				m.updateStatusWithEvent(appInst.InstanceID, StatusError)
				return err
			}
			continue
		}
	}

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
				if _, restoreErr := m.serviceManager.RestoreFromPodmanContext(ctx, appInst.InstanceID, def.Listeners, ports); restoreErr != nil {
					log.Printf("WARN: start app %s: failed to restore services: %v", appInst.InstanceID, restoreErr)
				} else {
					m.configureOIDCAuthorizePaths(appInst.InstanceID, def)
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
	_ = state // Desired-state persistence is owned by the lifecycle caller.

	mode := piccoloModeFromExtensions(def.Extensions)
	// Strictly check both recorded IDs and deterministic names. A container may
	// have been recreated just before a crash persisted its new ID; only a
	// typed not-found for the deterministic name proves that replacement absent.
	if err := m.stopContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		return err
	}

	m.finalizeQuiescedContainerGroup(ctx, appInst, def, mode)
	return nil
}

func (m *AppManager) finalizeQuiescedContainerGroup(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, mode PiccoloMode) {
	// StopAllApps can quiesce several independent Podman groups in parallel,
	// but rootfs-manager detach bookkeeping is a shared lifecycle surface.
	m.quiesceFinalizeMu.Lock()
	defer m.quiesceFinalizeMu.Unlock()
	// Detach rootfs only after graceful stop or PID 1 empty-cgroup proof.
	if m.appHasAnyServiceRootfs(appInst.InstanceID, mode, def, appInst) {
		m.detachAllServiceRootfs(ctx, appInst.InstanceID, mode, def, appInst)
	}
	m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(appInst.InstanceID)
	}
	m.interruptStartupProbation(appInst.InstanceID)
}

// quiesceContainerGroupRuntime prefers graceful Podman stop. Its boolean
// result says whether the returned runtime remains usable for post-quiescence
// metadata cleanup. PID 1 fallback proves process absence but stops the user
// manager, so callers must skip Podman cleanup or explicitly reacquire later.
func (m *AppManager) quiesceContainerGroupRuntime(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout) (container.PodmanRuntime, bool, error) {
	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, sessionQuiesced, err := m.quiesceRuntimeForApp(ctx, appInst.InstanceID, layout, mode)
	if err != nil {
		return container.PodmanRuntime{}, false, err
	}
	if sessionQuiesced {
		m.finalizeQuiescedContainerGroup(ctx, appInst, def, mode)
		return container.PodmanRuntime{}, false, nil
	}
	if err := m.stopContainerGroup(ctx, state, appInst, def, layout, runtime); err == nil {
		return runtime, true, nil
	} else {
		log.Printf("WARN: graceful Podman stop failed for %s, quiescing dedicated user unit: %v", appInst.InstanceID, err)
		if quiesceErr := m.quiesceAppUserSession(ctx, appInst.InstanceID); quiesceErr != nil {
			return container.PodmanRuntime{}, false, errors.Join(err, quiesceErr)
		}
	}
	m.finalizeQuiescedContainerGroup(ctx, appInst, def, mode)
	return container.PodmanRuntime{}, false, nil
}

// quiesceContainerGroup is the common lifecycle boundary for callers that do
// not need to issue further Podman commands after process-absence proof.
func (m *AppManager) quiesceContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout) error {
	_, _, err := m.quiesceContainerGroupRuntime(ctx, state, appInst, def, layout)
	return err
}

// uninstallContainerGroup removes an already-quiesced container group.
// This is the unified uninstall path for both service and workspace modes.
// Note: This does not handle volume destruction or state removal - those are handled by the caller.
func (m *AppManager) uninstallContainerGroup(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	if appInst == nil || def == nil {
		return fmt.Errorf("uninstall: app definition required")
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	primary := primaryServiceFor(def, appInst)

	order, _ := serviceStartOrder(def.Services)

	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	// Rootfs is normally detached by the quiescence proof. Keep this
	// idempotent detach for uninstall retries that resume after partial cleanup.
	if m.appHasAnyServiceRootfs(appInst.InstanceID, mode, def, appInst) {
		m.detachAllServiceRootfs(ctx, appInst.InstanceID, mode, def, appInst)
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
