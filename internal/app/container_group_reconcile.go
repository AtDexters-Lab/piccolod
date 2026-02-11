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
func (m *AppManager) handleStartupFailure(state *FilesystemStateManager, appInst *AppInstance) string {
	status := recordStartupFailure(appInst)
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: handleStartupFailure %s: failed to persist state: %v", appInst.InstanceID, err)
	}
	m.updateStatusWithEvent(appInst.InstanceID, status)
	return status
}

// recoverStaleAnchor handles recovery when the network anchor has a stale network namespace.
// It stops and removes all containers, clears state, and recreates the entire container group.
func (m *AppManager) recoverStaleAnchor(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, reason string) error {
	log.Printf("INFO: reconcile app %s: %s", appInst.InstanceID, reason)

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
		log.Printf("WARN: reconcile app %s: failed to persist cleared state: %v", appInst.InstanceID, err)
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

	// For workspace mode, ensure workspace disk is mounted before starting containers
	// and capture the mount info for container recreation.
	// NOTE: We do NOT call cleanupStaleWorkspaceMounts here because the container
	// may be actively using the overlay as its rootfs. Stale cleanup is only safe
	// during startContainerGroup when we know containers aren't running.
	var workspaceInfo *workspaceMountInfo
	if mode == ModeWorkspace && desiredRunning {
		if _, err := m.ensureWorkspaceDiskMounted(ctx, appInst.InstanceID, layout); err != nil {
			return fmt.Errorf("failed to mount workspace disk: %w", err)
		}
		workspaceInfo = m.getWorkspaceMountInfo(ctx, appInst.InstanceID)
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
				m.updateStatusWithEvent(appInst.InstanceID, StatusError)
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
			// If anchor has stale netns, recreate the entire container group
			var staleErr *container.StaleNetworkNamespaceError
			if errors.As(err, &staleErr) {
				if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
					"anchor has stale network namespace, recreating container group"); recoverErr != nil {
					m.handleStartupFailure(state, appInst)
					return recoverErr
				}
				return nil
			}
			m.handleStartupFailure(state, appInst)
			return fmt.Errorf("failed to start network anchor: %w", err)
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
			opts := serviceContainerOptions{
				layout:     layout,
				appDef:     def,
				instanceID: appInst.InstanceID,
				primary:    primary,
				svcName:    svcName,
				anchorID:   anchorID,
			}
			if workspaceInfo != nil && workspaceInfo.mergedPath != "" && workspaceInfo.meta != nil {
				opts.mergedRootfs = workspaceInfo.mergedPath
				opts.workspaceMeta = workspaceInfo.meta
			} else if mode == ModeWorkspace {
				// Workspace mode requires valid mount info for container recreation
				m.handleStartupFailure(state, appInst)
				return fmt.Errorf("workspace mount info unavailable for service '%s' recreation", svcName)
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
				// If service has stale netns, remove and recreate with current anchor
				var staleErr *container.StaleNetworkNamespaceError
				if errors.As(err, &staleErr) {
					log.Printf("INFO: reconcile app %s: service '%s' has stale network namespace, recreating container", appInst.InstanceID, svcName)

					// Remove the stale container (with error logging)
					if removeErr := m.containerManager.RemoveContainer(ctx, runtime, cid); removeErr != nil {
						log.Printf("WARN: reconcile app %s: remove stale service '%s' failed: %v", appInst.InstanceID, svcName, removeErr)
					}
					delete(appInst.Containers, svcName)
					if err := state.StoreAppMetadata(appInst); err != nil {
						log.Printf("WARN: reconcile app %s: failed to persist deleted container: %v", appInst.InstanceID, err)
					}

					// Recreate the service container with current anchor
					opts := serviceContainerOptions{
						layout:     layout,
						appDef:     def,
						instanceID: appInst.InstanceID,
						primary:    primary,
						svcName:    svcName,
						anchorID:   anchorID,
					}
					if workspaceInfo != nil && workspaceInfo.mergedPath != "" && workspaceInfo.meta != nil {
						opts.mergedRootfs = workspaceInfo.mergedPath
						opts.workspaceMeta = workspaceInfo.meta
					} else if mode == ModeWorkspace {
						m.handleStartupFailure(state, appInst)
						return fmt.Errorf("workspace mount info unavailable for service '%s' recreation after stale netns", svcName)
					}

					newCID, createErr := m.createAndStartServiceContainer(ctx, runtime, opts)
					if createErr != nil {
						// If anchor is stale (detected during service creation), recreate the whole group
						var anchorStaleErr *container.StaleNetworkNamespaceError
						if errors.As(createErr, &anchorStaleErr) {
							if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
								fmt.Sprintf("anchor found stale during service '%s' recreation, recreating group", svcName)); recoverErr != nil {
								m.handleStartupFailure(state, appInst)
								return recoverErr
							}
							return nil
						}

						// Use existing escalation mechanism for retry limiting
						m.handleStartupFailure(state, appInst)
						return fmt.Errorf("failed to recreate service '%s' after stale netns: %w", svcName, createErr)
					}

					if appInst.Containers == nil {
						appInst.Containers = make(map[string]string)
					}
					appInst.Containers[svcName] = newCID
					if err := state.StoreAppMetadata(appInst); err != nil {
						log.Printf("WARN: reconcile app %s: failed to persist recreated container ID: %v", appInst.InstanceID, err)
					}
					continue
				}

				m.handleStartupFailure(state, appInst)
				return fmt.Errorf("failed to start service '%s': %w", svcName, err)
			}
		}
	}

	if m.getObservedStatus(appInst.InstanceID) != StatusRunning {
		resetStartupTracking(appInst)
		if err := state.StoreAppMetadata(appInst); err != nil {
			log.Printf("WARN: reconcile %s: failed to persist startup tracking reset: %v", appInst.InstanceID, err)
		}
		m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
	}

	// Restore endpoints/proxies and ensure published ports match our expected allocations.
	if err := m.ensureServicesForRunningApp(ctx, def, appInst.InstanceID, anchorID, runtime); err != nil {
		log.Printf("WARN: reconcile app %s: restore services failed: %v", appInst.InstanceID, err)
	}
	if err := m.ensurePodmanPublishes(ctx, def, appInst.InstanceID, anchorID, runtime); err != nil {
		if errors.Is(err, container.ErrPortReconciliationRequired) {
			if shouldEscalateToError(appInst) {
				if m.getObservedStatus(appInst.InstanceID) != StatusError {
					m.updateStatusWithEvent(appInst.InstanceID, StatusError)
				}
				return nil
			}
			log.Printf("INFO: reconcile app %s: port bindings mismatch, recreating containers", appInst.InstanceID)
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

		newInst, err := m.installContainerGroup(ctx, def, appInst.InstanceID, layout, runtime, endpoints)
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
