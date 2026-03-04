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

// Startup failure escalation thresholds (RFC 20260125)
// After these thresholds, status escalates from "starting" to "error".
// Tracking is intentionally in-memory only (not in AppMetadata on disk),
// so counters reset naturally on daemon restart — giving apps a fresh
// escalation window after the admin fixes the underlying issue.
const (
	startupEscalateAfterAttempts = 5
	startupEscalateAfterDuration = 10 * time.Minute
	msgStartupFailed             = "Startup failed after repeated attempts"
)

// shouldEscalateToError checks if startup failures have exceeded escalation thresholds.
func shouldEscalateToError(app *AppInstance) bool {
	if app.StartupAttempts >= startupEscalateAfterAttempts {
		return true
	}
	if app.FirstStartupFailureAt != nil &&
		time.Since(*app.FirstStartupFailureAt) >= startupEscalateAfterDuration {
		return true
	}
	return false
}

// recordStartupFailure increments the startup attempt counter and sets first failure time.
// Returns the appropriate status ("starting" or "error" if escalated).
func recordStartupFailure(app *AppInstance) string {
	app.StartupAttempts++
	if app.FirstStartupFailureAt == nil {
		now := time.Now()
		app.FirstStartupFailureAt = &now
	}

	if shouldEscalateToError(app) {
		return "error"
	}
	return "starting"
}

// resetStartupTracking clears startup failure tracking on successful start.
func resetStartupTracking(app *AppInstance) {
	app.StartupAttempts = 0
	app.FirstStartupFailureAt = nil
}

// handleStartupFailure records a startup failure, persists state, and emits the appropriate status event.
// Returns the computed status ("starting" or "error" if escalated).
// When escalating to "error", sets a user-facing message. When remaining in "starting",
// preserves whatever message was set by the caller (e.g., "Containers not found, recreating").
func (m *AppManager) handleStartupFailure(state *FilesystemStateManager, appInst *AppInstance) string {
	status := recordStartupFailure(appInst)
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: handleStartupFailure %s: failed to persist state: %v", appInst.InstanceID, err)
	}
	if status == StatusError {
		m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
	} else {
		// Keep the existing message (set by caller) — only update status.
		m.updateStatusPreservingMessageWithEvent(appInst.InstanceID, status)
	}
	return status
}

// recreateServiceContainer removes a broken service container and recreates it with the current anchor.
// The caller is responsible for logging context and handling failure escalation.
// On success the new container ID is stored in appInst.Containers and persisted.
func (m *AppManager) recreateServiceContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, runtime container.PodmanRuntime, oldCID string, opts serviceContainerOptions) error {
	svcName := opts.svcName

	// Remove the broken container
	if removeErr := m.containerManager.RemoveContainer(ctx, runtime, oldCID); removeErr != nil {
		log.Printf("WARN: app %s: remove failed service '%s': %v",
			appInst.InstanceID, svcName, removeErr)
	}
	delete(appInst.Containers, svcName)
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: app %s: failed to persist deleted container: %v",
			appInst.InstanceID, err)
	}

	newCID, createErr := m.createAndStartServiceContainer(ctx, runtime, opts)
	if createErr != nil {
		return fmt.Errorf("failed to recreate service '%s': %w", svcName, createErr)
	}

	appInst.Containers[svcName] = newCID
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: app %s: failed to persist recreated container: %v",
			appInst.InstanceID, err)
	}
	return nil
}

// recoverStaleAnchor handles recovery when the network anchor cannot be started.
// It stops and removes all containers, clears state, and recreates the entire container group.
// This covers stale runtime state after reboot, dead network namespaces, corrupted containers, etc.
func (m *AppManager) recoverStaleAnchor(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, reason string) error {
	log.Printf("INFO: app %s: %s", appInst.InstanceID, reason)

	// Stop and remove with strict error handling.
	// If stop/remove fails (e.g. unkillable zombie), abort recreation to avoid
	// duplicate container conflicts or resource leaks.
	if err := m.stopContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		return fmt.Errorf("stop failed during recovery: %w", err)
	}
	if err := m.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		return fmt.Errorf("remove failed during recovery: %w", err)
	}
	m.serviceManager.RemoveApp(appInst.InstanceID)

	// Clear stale IDs from state before recreation
	appInst.NetworkAnchorID = ""
	appInst.Containers = nil
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: app %s: failed to persist cleared state: %v", appInst.InstanceID, err)
	}

	return m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime)
}

// reconcileContainerGroup reconciles a container group (network anchor + service containers).
// This is the unified reconcile path for both service and workspace modes.
func (m *AppManager) reconcileContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, desiredRunning bool) error {
	if m.serviceManager == nil {
		return fmt.Errorf("app manager: service manager not configured")
	}
	if appInst == nil || def == nil || def.Services == nil {
		return fmt.Errorf("reconcile: invalid container group app state")
	}

	// Emit "starting" status if we're about to start containers (RFC 20260125).
	// This ensures UI shows the "Starting..." banner during reconciliation-triggered starts.
	// Skip if already escalated to "error" — that status must persist until daemon restart
	// resets in-memory counters, so the escalation guards below can short-circuit cheaply.
	observed := m.getObservedStatus(appInst.InstanceID)
	if desiredRunning && observed != StatusRunning && observed != StatusStarting && observed != StatusError {
		m.updateStatusWithEvent(appInst.InstanceID, StatusStarting)
	}

	mode := piccoloModeFromExtensions(def.Extensions)

	// Recover from partially-completed rollbacks (crash between LV rename steps).
	if err := m.reconcilePartialRollback(ctx, state, appInst.InstanceID); err != nil {
		log.Printf("WARN: reconcile app %s: partial rollback recovery: %v", appInst.InstanceID, err)
	}

	// Tuple health: auto-rollback (StatusError from previous pass) and auto-deprecation (24h healthy).
	// Must run before container state checks so auto-rollback triggers before container recreation attempts.
	// Only for running apps — stopped/follower apps should not trigger rollback or deprecation.
	if desiredRunning {
		m.checkTupleHealth(ctx, state, appInst)
	}

	// Ensure all service rootfs volumes are attached before reconciling containers.
	// Returns nil for legacy apps without rootfs volumes.
	var blockNativeRootfsMap map[string]*rootfsMountInfo
	if desiredRunning {
		var rootfsErr error
		blockNativeRootfsMap, rootfsErr = m.ensureAllServiceRootfsAttached(ctx, appInst.InstanceID, mode, def, appInst)
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
		}
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
			if err := state.StoreAppMetadata(appInst); err != nil {
				log.Printf("WARN: reconcile app %s: failed to persist anchor ID: %v", appInst.InstanceID, err)
			}
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
			// Best-effort cleanup; errors don't block the desired stopped state.
			if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
				log.Printf("WARN: reconcile app %s: best-effort stop failed: %v", appInst.InstanceID, stopErr)
			}
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(appInst.InstanceID)
			}
			// Observed status reflects local container state - containers are stopped on this machine.
			m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
			return nil
		}

		// If startup failures have exceeded escalation thresholds, stop retrying
		// expensive recreation. This prevents infinite loops when recreation
		// consistently fails (e.g., storage path mismatch after upgrade).
		if shouldEscalateToError(appInst) {
			// Clean up stale containers/proxies once on first escalation,
			// then skip on subsequent cycles (status already error).
			if m.getObservedStatus(appInst.InstanceID) != StatusError {
				if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
					log.Printf("WARN: reconcile app %s: escalation stop failed: %v", appInst.InstanceID, stopErr)
				}
				if removeErr := m.removeContainersForMultiApp(ctx, appInst, def, runtime); removeErr != nil {
					log.Printf("WARN: reconcile app %s: escalation remove failed: %v", appInst.InstanceID, removeErr)
				}
				m.serviceManager.RemoveApp(appInst.InstanceID)
				m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
			}
			return nil
		}

		// The anchor is missing but we desire the app running. Stop and remove all service containers
		// so we can recreate the entire group (anchor + services) deterministically.
		// Best-effort cleanup before recreation; errors logged but don't block.
		if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
			log.Printf("WARN: reconcile app %s: pre-recreate stop failed: %v", appInst.InstanceID, stopErr)
		}
		if removeErr := m.removeContainersForMultiApp(ctx, appInst, def, runtime); removeErr != nil {
			log.Printf("WARN: reconcile app %s: pre-recreate remove failed: %v", appInst.InstanceID, removeErr)
		}
		m.serviceManager.RemoveApp(appInst.InstanceID)

		m.setObservedStatusMessage(appInst.InstanceID, "Containers not found, recreating")
		if err := m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime); err != nil {
			m.handleStartupFailure(state, appInst)
			return err
		}
		return nil
	}

	// If we don't desire running (stopped app or follower), stop all containers and remove proxies
	// but do not change persisted desired state (Enabled field).
	if !desiredRunning {
		// Best-effort cleanup; errors don't block the desired stopped state.
		if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
			log.Printf("WARN: reconcile app %s: best-effort stop failed: %v", appInst.InstanceID, stopErr)
		}
		if m.serviceManager != nil {
			m.serviceManager.RemoveApp(appInst.InstanceID)
		}
		// Observed status reflects local container state - containers are stopped on this machine.
		m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
		return nil
	}

	// Ensure anchor running.
	if !anchorState.Running {
		if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
			// Container can't start — remove and recreate the entire group.
			// Covers all stale state: reboot (wiped /run/), dead netns, corrupted runtime, etc.
			m.setObservedStatusMessage(appInst.InstanceID, "Container start failed, recreating")
			if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
				fmt.Sprintf("anchor start failed (%v), recreating container group", err)); recoverErr != nil {
				m.handleStartupFailure(state, appInst)
				return recoverErr
			}
			return nil
		}
	}

	m.serviceManager.SetAppContainerID(appInst.InstanceID, anchorID)

	// Ensure all declared services exist and are running.
	for _, svcName := range startOrder {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			name := containerNameForService(appInst.InstanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" {
				cid = id
				if appInst.Containers == nil {
					appInst.Containers = make(map[string]string)
				}
				appInst.Containers[svcName] = id
				if err := state.StoreAppMetadata(appInst); err != nil {
					log.Printf("WARN: reconcile app %s: failed to persist service container ID: %v", appInst.InstanceID, err)
				}
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

		// If stored ID is stale (container doesn't exist), try name-based resolution before recreating.
		if cid != "" && !st.Exists {
			name := containerNameForService(appInst.InstanceID, svcName, primary)
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" {
				cid = id
				appInst.Containers[svcName] = id
				if err := state.StoreAppMetadata(appInst); err != nil {
					log.Printf("WARN: reconcile app %s: failed to persist resolved container ID: %v", appInst.InstanceID, err)
				}
				if observed, err := m.containerManager.InspectContainerState(ctx, runtime, cid); err == nil {
					st = observed
				}
			}
		}

		if cid == "" || !st.Exists {
			m.setObservedStatusMessage(appInst.InstanceID, fmt.Sprintf("Recreating service '%s'", svcName))
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
			newCID, err := m.createAndStartServiceContainer(ctx, runtime, opts)
			if err != nil {
				m.handleStartupFailure(state, appInst)
				return err
			}
			if appInst.Containers == nil {
				appInst.Containers = make(map[string]string)
			}
			appInst.Containers[svcName] = newCID
			if err := state.StoreAppMetadata(appInst); err != nil {
				log.Printf("WARN: reconcile app %s: failed to persist new container ID: %v", appInst.InstanceID, err)
			}
			continue
		}

		if !st.Running {
			if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
				log.Printf("INFO: reconcile app %s: service '%s' (cid=%s) start failed (%v), recreating",
					appInst.InstanceID, svcName, cid, err)
				m.setObservedStatusMessage(appInst.InstanceID, "Service start failed, recreating")

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
					m.handleStartupFailure(state, appInst)
					return err
				}
				continue
			}
		}
	}

	if m.getObservedStatus(appInst.InstanceID) != StatusRunning {
		resetStartupTracking(appInst)
		if err := state.StoreAppMetadata(appInst); err != nil {
			log.Printf("WARN: reconcile %s: failed to persist startup tracking reset: %v", appInst.InstanceID, err)
		}
		m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
	} else {
		// Clear any transient message from in-place recovery (e.g. single service recreation)
		// when the app was already running and no status transition occurred.
		m.setObservedStatusMessage(appInst.InstanceID, "")
	}

	// Mark active generation as healthy now that reconciler confirmed all containers running.
	// Must be after container verification (above), not in checkTupleHealth (which runs before).
	m.markTupleHealthy(state, appInst.InstanceID)

	// Restore endpoints/proxies and ensure published ports match our expected allocations.
	if err := m.ensureServicesForRunningApp(ctx, def, appInst.InstanceID, anchorID, runtime); err != nil {
		log.Printf("WARN: reconcile app %s: restore services failed: %v", appInst.InstanceID, err)
	}
	if err := m.ensurePodmanPublishes(ctx, def, appInst.InstanceID, anchorID, runtime); err != nil {
		if errors.Is(err, container.ErrPortReconciliationRequired) {
			if shouldEscalateToError(appInst) {
				if m.getObservedStatus(appInst.InstanceID) != StatusError {
					m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
				}
				return nil
			}
			log.Printf("INFO: reconcile app %s: port bindings mismatch, recreating containers", appInst.InstanceID)
			m.setObservedStatusMessage(appInst.InstanceID, "Port mismatch, recreating containers")
			// Best-effort cleanup before port-reconciliation recreation; errors logged but don't block.
			if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
				log.Printf("WARN: reconcile app %s: port-reconcile stop failed: %v", appInst.InstanceID, stopErr)
			}
			if removeErr := m.removeContainersForMultiApp(ctx, appInst, def, runtime); removeErr != nil {
				log.Printf("WARN: reconcile app %s: port-reconcile remove failed: %v", appInst.InstanceID, removeErr)
			}
			m.serviceManager.RemoveApp(appInst.InstanceID)
			if err := m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime); err != nil {
				m.handleStartupFailure(state, appInst)
				return err
			}
			return nil
		}
		log.Printf("WARN: reconcile app %s: publish reconcile failed: %v", appInst.InstanceID, err)
	}

	// Tuple GC: remove expired deprecated and failed generations (daily).
	if gcErr := m.garbageCollectGenerations(ctx, state, appInst.InstanceID); gcErr != nil {
		log.Printf("WARN: reconcile app %s: tuple GC: %v", appInst.InstanceID, gcErr)
	}

	// Detect stale DNAT rules: if the existing backend health check (15s interval,
	// 3-failure debounce) reports unhealthy while containers are confirmed running,
	// the nftables DNAT is likely routing to a dead IP from a previous container.
	// Recreate the affected app with new host-bind ports.
	//
	// Guards against infinite recreation loops for genuinely unhealthy apps:
	//   - 2-minute cooldown after last update (covers startup + health debounce)
	//   - Reuses startup failure escalation: after startupEscalateAfterAttempts
	//     consecutive repairs, status escalates to "error" and stops retrying
	if eps, err := m.serviceManager.GetByApp(appInst.InstanceID); err == nil {
		anyUnhealthy := false
		for _, ep := range eps {
			endpointKey := ep.App + "/" + ep.Name
			healthy, _ := m.serviceManager.GetBackendHealth(endpointKey)
			if !healthy {
				anyUnhealthy = true
				break
			}
		}
		if anyUnhealthy {
			if time.Since(appInst.UpdatedAt) < 2*time.Minute {
				log.Printf("WARN: reconcile app %s: backend unhealthy but recently updated, deferring DNAT repair",
					appInst.InstanceID)
			} else if shouldEscalateToError(appInst) {
				log.Printf("WARN: reconcile app %s: backend unhealthy after %d DNAT repair attempts, escalating to error",
					appInst.InstanceID, appInst.StartupAttempts)
				m.handleStartupFailure(state, appInst)
			} else {
				log.Printf("INFO: reconcile app %s: backend unhealthy with running containers, likely stale DNAT — recreating with new ports (attempt %d)",
					appInst.InstanceID, appInst.StartupAttempts+1)
				m.setObservedStatusMessage(appInst.InstanceID, "Repairing stale network routes")
				m.handleStartupFailure(state, appInst)
				// Preserve the startup attempt counter across recreation.
				// recreateMissingMultiContainer resets it on success, but for DNAT repair
				// we need to accumulate attempts so the escalation threshold works.
				savedAttempts := appInst.StartupAttempts
				savedFirstFailure := appInst.FirstStartupFailureAt
				err := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
					"stale DNAT rules detected: backend unhealthy with running containers")
				if err == nil {
					appInst.StartupAttempts = savedAttempts
					appInst.FirstStartupFailureAt = savedFirstFailure
					if storeErr := state.StoreAppMetadata(appInst); storeErr != nil {
						log.Printf("WARN: reconcile app %s: failed to persist DNAT repair tracking: %v", appInst.InstanceID, storeErr)
					}
				}
				return err
			}
		}
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
		endpoints, err := m.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners)
		if err != nil {
			return fmt.Errorf("allocate service ports: %w", err)
		}

		newInst, err := m.installContainerGroup(ctx, def, appInst.InstanceID, layout, runtime, endpoints, nil)
		if err == nil {
			// Preserve timestamps and reset failure tracking after successful recovery.
			newInst.CreatedAt = appInst.CreatedAt
			newInst.UpdatedAt = time.Now()
			appInst.PrimaryService = newInst.PrimaryService
			appInst.NetworkAnchorID = newInst.NetworkAnchorID
			appInst.Containers = newInst.Containers
			resetStartupTracking(appInst)
			m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
			if err := state.StoreAppMetadata(appInst); err != nil {
				log.Printf("WARN: reconcile app %s: failed to persist recovered state: %v", appInst.InstanceID, err)
			}
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

func (m *AppManager) createAndStartServiceContainer(ctx context.Context, runtime container.PodmanRuntime, opts serviceContainerOptions) (string, error) {
	spec, err := m.buildServiceContainerSpec(opts)
	if err != nil {
		return "", fmt.Errorf("build container spec for service '%s': %w", opts.svcName, err)
	}
	// Per-app runtimes must never pull: images are pre-pulled to the shared
	// imagestore and per-app users lack write access to it.
	if spec.Image != "" {
		spec.PullPolicy = "never"
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
			log.Printf("INFO: recreate %s: removing zombie container %s (service=%s)", opts.instanceID, zombieID, opts.svcName)
			_ = m.containerManager.StopContainer(ctx, runtime, zombieID)
			_ = m.containerManager.RemoveContainer(ctx, runtime, zombieID)
			continue
		}
		break
	}
	if err != nil {
		return "", fmt.Errorf("create service container '%s': %w", opts.svcName, err)
	}
	if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
		_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		return "", fmt.Errorf("start service container '%s': %w", opts.svcName, err)
	}
	return cid, nil
}

func (m *AppManager) stopContainersForMultiApp(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	primary := primaryServiceFor(def, appInst)
	order, _ := serviceStartOrder(def.Services)
	var errs []error

	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		stored := strings.TrimSpace(appInst.Containers[svcName])
		if stored != "" {
			if err := m.containerManager.StopContainer(ctx, runtime, stored); err != nil {
				var notFound *container.ContainerNotFoundError
				if !errors.As(err, &notFound) {
					errs = append(errs, fmt.Errorf("stop %s: %w", svcName, err))
				}
			}
		}
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" && id != stored {
			if err := m.containerManager.StopContainer(ctx, runtime, id); err != nil {
				var notFound *container.ContainerNotFoundError
				if !errors.As(err, &notFound) {
					errs = append(errs, fmt.Errorf("stop %s (resolved): %w", svcName, err))
				}
			}
		}
	}
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	if anchorID != "" {
		if err := m.containerManager.StopContainer(ctx, runtime, anchorID); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				errs = append(errs, fmt.Errorf("stop anchor: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

func (m *AppManager) removeContainersForMultiApp(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	primary := primaryServiceFor(def, appInst)
	order, _ := serviceStartOrder(def.Services)
	var errs []error

	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		stored := strings.TrimSpace(appInst.Containers[svcName])
		if stored != "" {
			if err := m.containerManager.RemoveContainer(ctx, runtime, stored); err != nil {
				var notFound *container.ContainerNotFoundError
				if !errors.As(err, &notFound) {
					errs = append(errs, fmt.Errorf("remove %s: %w", svcName, err))
				}
			}
		}
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" && id != stored {
			if err := m.containerManager.RemoveContainer(ctx, runtime, id); err != nil {
				var notFound *container.ContainerNotFoundError
				if !errors.As(err, &notFound) {
					errs = append(errs, fmt.Errorf("remove %s (resolved): %w", svcName, err))
				}
			}
		}
	}
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
			anchorID = id
		}
	}
	if anchorID != "" {
		if err := m.containerManager.RemoveContainer(ctx, runtime, anchorID); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				errs = append(errs, fmt.Errorf("remove anchor: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}
