package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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

// startupRecoveryWindow contains the process-local portion of startup
// recovery tracking. AppInstance retains the existing attempt count and first
// failure time; this map records whether that attempt is currently in flight
// and when continuous-running probation began.
type startupRecoveryWindow struct {
	AttemptActive  bool
	ProbationSince time.Time
}

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

// resetStartupTracking clears the existing process-local startup history.
func resetStartupTracking(app *AppInstance) {
	app.StartupAttempts = 0
	app.FirstStartupFailureAt = nil
}

// beginAutomaticStartupAttempt checks the existing escalation guard before
// consuming an attempt. An already-active attempt belongs to the current
// reconcile pass and must not be counted twice by a nested repair path.
func (m *AppManager) beginAutomaticStartupAttempt(state *FilesystemStateManager, appInst *AppInstance) bool {
	m.startupRecoveryMu.Lock()
	if m.startupRecovery == nil {
		m.startupRecovery = make(map[string]startupRecoveryWindow)
	}
	window := m.startupRecovery[appInst.InstanceID]
	if window.AttemptActive {
		m.startupRecoveryMu.Unlock()
		log.Printf("INFO: startup recovery reusing active attempt instance=%s attempt=%d", appInst.InstanceID, appInst.StartupAttempts)
		return true
	}
	// Reaching this path means the desired-running app was observed lost or
	// unusable. That observation breaks continuous-running probation even when
	// the existing attempt/time guard rejects the next automatic effect.
	window.ProbationSince = time.Time{}
	m.startupRecovery[appInst.InstanceID] = window
	if shouldEscalateToError(appInst) {
		m.startupRecoveryMu.Unlock()
		m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
		return false
	}

	appInst.StartupAttempts++
	if appInst.FirstStartupFailureAt == nil {
		now := time.Now()
		appInst.FirstStartupFailureAt = &now
	}
	window.AttemptActive = true
	m.startupRecovery[appInst.InstanceID] = window
	m.startupRecoveryMu.Unlock()
	log.Printf("INFO: startup recovery attempt started instance=%s attempt=%d", appInst.InstanceID, appInst.StartupAttempts)

	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: begin startup recovery %s: failed to persist state: %v", appInst.InstanceID, err)
	}
	m.updateStatusPreservingMessageWithEvent(appInst.InstanceID, StatusStarting)
	return true
}

func (m *AppManager) startupAttemptActive(instanceID string) bool {
	m.startupRecoveryMu.Lock()
	defer m.startupRecoveryMu.Unlock()
	return m.startupRecovery[instanceID].AttemptActive
}

// markStartupRecoverySucceeded starts (or advances) the continuous-running
// probation window. Recovery history is cleared only after a full healthy
// window has been observed by ordinary reconciliation.
func (m *AppManager) markStartupRecoverySucceeded(state *FilesystemStateManager, appInst *AppInstance) {
	m.startupRecoveryMu.Lock()
	window := m.startupRecovery[appInst.InstanceID]
	window.AttemptActive = false
	if appInst.StartupAttempts == 0 || appInst.FirstStartupFailureAt == nil {
		delete(m.startupRecovery, appInst.InstanceID)
		m.startupRecoveryMu.Unlock()
		return
	}

	now := time.Now()
	if window.ProbationSince.IsZero() {
		window.ProbationSince = now
		m.startupRecovery[appInst.InstanceID] = window
		m.startupRecoveryMu.Unlock()
		return
	}
	if now.Sub(window.ProbationSince) < startupEscalateAfterDuration {
		m.startupRecovery[appInst.InstanceID] = window
		m.startupRecoveryMu.Unlock()
		return
	}

	resetStartupTracking(appInst)
	delete(m.startupRecovery, appInst.InstanceID)
	m.startupRecoveryMu.Unlock()
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: startup probation %s: failed to persist cleared state: %v", appInst.InstanceID, err)
	}
}

func (m *AppManager) clearStartupRecovery(appInst *AppInstance) {
	if appInst == nil {
		return
	}
	m.startupRecoveryMu.Lock()
	resetStartupTracking(appInst)
	delete(m.startupRecovery, appInst.InstanceID)
	m.startupRecoveryMu.Unlock()
}

func (m *AppManager) interruptStartupProbation(instanceID string) {
	m.startupRecoveryMu.Lock()
	if window, ok := m.startupRecovery[instanceID]; ok {
		// Quiescence breaks continuous-running probation, but it must not erase
		// ownership of an in-flight recovery attempt. Nested recreate failures
		// would otherwise count the same attempt twice.
		window.ProbationSince = time.Time{}
		m.startupRecovery[instanceID] = window
	}
	m.startupRecoveryMu.Unlock()
}

// containerGroupObservedRunning is deliberately observe-only. It is used to
// distinguish a healthy reconcile pass from one that is about to perform a
// session or container recovery effect.
func (m *AppManager) containerGroupObservedRunning(ctx context.Context, runtime container.PodmanRuntime, appInst *AppInstance, def *api.AppDefinition) bool {
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID == "" {
		return false
	}
	anchorState, err := m.containerManager.InspectContainerState(ctx, runtime, anchorID)
	if err != nil || !anchorState.Exists || !anchorState.Running {
		return false
	}
	for svcName := range def.Services {
		cid := strings.TrimSpace(appInst.Containers[svcName])
		if cid == "" {
			return false
		}
		state, err := m.containerManager.InspectContainerState(ctx, runtime, cid)
		if err != nil || !state.Exists || !state.Running {
			return false
		}
	}
	return true
}

// handleStartupFailure records a startup failure, persists state, and emits the appropriate status event.
// Returns the computed status ("starting" or "error" if escalated).
// When escalating to "error", sets a user-facing message. When remaining in "starting",
// preserves whatever message was set by the caller (e.g., "Containers not found, recreating").
func (m *AppManager) handleStartupFailure(state *FilesystemStateManager, appInst *AppInstance) string {
	m.startupRecoveryMu.Lock()
	window := m.startupRecovery[appInst.InstanceID]
	ownedAttempt := window.AttemptActive
	if window.AttemptActive {
		window.AttemptActive = false
		window.ProbationSince = time.Time{}
		m.startupRecovery[appInst.InstanceID] = window
	} else {
		recordStartupFailure(appInst)
	}
	status := StatusStarting
	if shouldEscalateToError(appInst) {
		status = StatusError
	}
	m.startupRecoveryMu.Unlock()
	if ownedAttempt {
		log.Printf("WARN: startup recovery attempt failed instance=%s attempt=%d", appInst.InstanceID, appInst.StartupAttempts)
	} else {
		log.Printf("WARN: startup failure recorded outside an owned attempt instance=%s attempt=%d", appInst.InstanceID, appInst.StartupAttempts)
	}
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
func (m *AppManager) recoverStaleAnchor(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, reason string, prebuiltRootfs map[string]*rootfsMountInfo) error {
	log.Printf("INFO: app %s: %s", appInst.InstanceID, reason)

	// Prefer Podman's graceful stop. A dead per-app user manager can leave
	// Podman metadata claiming that containers are running even though their
	// conmon processes were killed. In that case, use the existing PID 1
	// quiescence proof, repair the user session, and continue metadata cleanup
	// through the reacquired rootless runtime.
	if err := m.stopContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		log.Printf("WARN: app %s: graceful stop failed during recovery, quiescing dedicated user unit: %v", appInst.InstanceID, err)
		if quiesceErr := m.quiesceAppUserSession(ctx, appInst.InstanceID); quiesceErr != nil {
			return errors.Join(
				fmt.Errorf("stop failed during recovery: %w", err),
				fmt.Errorf("PID 1 quiesce failed during recovery: %w", quiesceErr),
			)
		}
		repairedRuntime, runtimeErr := m.podmanRuntimeForApp(ctx, appInst.InstanceID, layout, piccoloModeFromExtensions(def.Extensions), appRuntimeEnsureReady)
		if runtimeErr != nil {
			return errors.Join(
				fmt.Errorf("stop failed during recovery: %w", err),
				fmt.Errorf("reacquire rootless runtime after PID 1 quiesce: %w", runtimeErr),
			)
		}
		runtime = repairedRuntime
	}
	if err := m.removeContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		return fmt.Errorf("remove failed during recovery: %w", err)
	}
	m.serviceManager.DeactivateApp(appInst.InstanceID)

	// Clear stale IDs from state before recreation
	appInst.NetworkAnchorID = ""
	appInst.Containers = nil
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: app %s: failed to persist cleared state: %v", appInst.InstanceID, err)
	}

	return m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime, prebuiltRootfs)
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

	// Tuple health: auto-rollback (StatusError from previous pass) and auto-deprecation (24h healthy).
	// Must run before container state checks so auto-rollback triggers before container recreation attempts.
	// Only for running apps — stopped/follower apps should not trigger rollback or deprecation.
	if desiredRunning {
		m.checkTupleHealth(ctx, state, appInst)
	}

	// Service rootfs volumes attach lazily: sync.OnceValues memoizes the
	// (map, error) so the first create/recreate consumer triggers the attach and
	// the rest reuse it. A steady-state pass reaches no consumer and never
	// attaches, probes, or logs. Constructed only on the running path —
	// consumers are unreachable when this is nil (the !desiredRunning branches
	// return first).
	var rootfsMap func() (map[string]*rootfsMountInfo, error)
	if desiredRunning {
		rootfsMap = sync.OnceValues(func() (map[string]*rootfsMountInfo, error) {
			return m.ensureAllServiceRootfsAttached(ctx, appInst.InstanceID, mode, def, appInst)
		})
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
				m.serviceManager.DeactivateApp(appInst.InstanceID)
			}
			// Observed status reflects local container state - containers are stopped on this machine.
			m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
			return nil
		}

		// If startup failures have exceeded escalation thresholds, stop retrying
		// expensive recreation. This prevents infinite loops when recreation
		// consistently fails (e.g., storage path mismatch after upgrade).
		if shouldEscalateToError(appInst) && !m.startupAttemptActive(appInst.InstanceID) {
			// Clean up stale containers/proxies once on first escalation,
			// then skip on subsequent cycles (status already error).
			if m.getObservedStatus(appInst.InstanceID) != StatusError {
				if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
					log.Printf("WARN: reconcile app %s: escalation stop failed: %v", appInst.InstanceID, stopErr)
				}
				if removeErr := m.removeContainersForMultiApp(ctx, appInst, def, runtime); removeErr != nil {
					log.Printf("WARN: reconcile app %s: escalation remove failed: %v", appInst.InstanceID, removeErr)
				}
				m.serviceManager.DeactivateApp(appInst.InstanceID)
				m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
			}
			return nil
		}

		// Resolve rootfs handles before teardown: a transient attach error must
		// not tear down the existing group before we can recreate it.
		bn, rootfsErr := rootfsMap()
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
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
		m.serviceManager.DeactivateApp(appInst.InstanceID)

		m.setObservedStatusMessage(appInst.InstanceID, "Containers not found, recreating")
		if err := m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime, bn); err != nil {
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
			m.serviceManager.DeactivateApp(appInst.InstanceID)
		}
		// Observed status reflects local container state - containers are stopped on this machine.
		m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
		return nil
	}

	// A killed user manager can leave Podman claiming that the recorded PID is
	// running after it has disappeared or been reused. Starting that metadata is
	// a successful no-op, so route the stale state through the existing strict
	// whole-group recovery boundary instead.
	if anchorState.Stale {
		bn, rootfsErr := rootfsMap()
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
		}
		return m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
			"anchor process no longer belongs to its libpod cgroup", bn)
	}

	// Ensure anchor running.
	if !anchorState.Running {
		if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
			// Container can't start — remove and recreate the entire group.
			// Covers all stale state: reboot (wiped /run/), dead netns, corrupted runtime, etc.
			m.setObservedStatusMessage(appInst.InstanceID, "Container start failed, recreating")
			bn, rootfsErr := rootfsMap()
			if rootfsErr != nil {
				return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
			}
			if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
				fmt.Sprintf("anchor start failed (%v), recreating container group", err), bn); recoverErr != nil {
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
			bn, rootfsErr := rootfsMap()
			if rootfsErr != nil {
				return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
			}
			opts := serviceContainerOptions{
				layout:     layout,
				appDef:     def,
				instanceID: appInst.InstanceID,
				primary:    primary,
				svcName:    svcName,
				anchorID:   anchorID,
				credential: runtime.Credential,
			}
			if svcRootfs, ok := bn[svcName]; ok {
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

		if st.Stale {
			bn, rootfsErr := rootfsMap()
			if rootfsErr != nil {
				return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
			}
			return m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
				fmt.Sprintf("service %q process no longer belongs to its libpod cgroup", svcName), bn)
		}

		if !st.Running {
			if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
				log.Printf("INFO: reconcile app %s: service '%s' (cid=%s) start failed (%v), recreating",
					appInst.InstanceID, svcName, cid, err)
				m.setObservedStatusMessage(appInst.InstanceID, "Service start failed, recreating")

				bn, rootfsErr := rootfsMap()
				if rootfsErr != nil {
					return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
				}
				opts := serviceContainerOptions{
					layout:     layout,
					appDef:     def,
					instanceID: appInst.InstanceID,
					primary:    primary,
					svcName:    svcName,
					anchorID:   anchorID,
					credential: runtime.Credential,
				}
				if svcRootfs, ok := bn[svcName]; ok {
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
			if !m.beginAutomaticStartupAttempt(state, appInst) {
				if m.getObservedStatus(appInst.InstanceID) != StatusError {
					m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
				}
				return nil
			}
			log.Printf("INFO: reconcile app %s: port bindings mismatch, recreating containers", appInst.InstanceID)
			m.setObservedStatusMessage(appInst.InstanceID, "Port mismatch, recreating containers")
			// Resolve rootfs handles before any teardown: a transient attach
			// error must not leave the group half-torn-down.
			bn, rootfsErr := rootfsMap()
			if rootfsErr != nil {
				return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
			}
			// Best-effort cleanup before port-reconciliation recreation; errors logged but don't block.
			if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
				log.Printf("WARN: reconcile app %s: port-reconcile stop failed: %v", appInst.InstanceID, stopErr)
			}
			if removeErr := m.removeContainersForMultiApp(ctx, appInst, def, runtime); removeErr != nil {
				log.Printf("WARN: reconcile app %s: port-reconcile remove failed: %v", appInst.InstanceID, removeErr)
			}
			m.serviceManager.DeactivateApp(appInst.InstanceID)
			if err := m.recreateMissingMultiContainer(ctx, state, appInst, def, layout, runtime, bn); err != nil {
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
			} else if !m.beginAutomaticStartupAttempt(state, appInst) {
				log.Printf("WARN: reconcile app %s: backend unhealthy after %d DNAT repair attempts, escalating to error",
					appInst.InstanceID, appInst.StartupAttempts)
			} else {
				log.Printf("INFO: reconcile app %s: backend unhealthy with running containers, likely stale DNAT — recreating with new ports (attempt %d)",
					appInst.InstanceID, appInst.StartupAttempts)
				m.setObservedStatusMessage(appInst.InstanceID, "Repairing stale network routes")
				// Resolve rootfs handles before handleStartupFailure / teardown:
				// a transient attach error must not escalate a running app to
				// StatusError, nor tear the group down before recreate.
				bn, rootfsErr := rootfsMap()
				if rootfsErr != nil {
					return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
				}
				err := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
					"stale DNAT rules detected: backend unhealthy with running containers", bn)
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

func (m *AppManager) recreateMissingMultiContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, prebuiltRootfs map[string]*rootfsMountInfo) error {
	// Allocate endpoints and recreate the group (anchor + services).
	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		endpoints, err := m.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners)
		if err != nil {
			return fmt.Errorf("allocate service ports: %w", err)
		}
		m.configureOIDCAuthorizePaths(appInst.InstanceID, def)

		newInst, err := m.installContainerGroup(ctx, def, appInst.InstanceID, layout, runtime, endpoints, prebuiltRootfs)
		if err == nil {
			// Preserve timestamps and recovery tracking after successful recovery.
			newInst.CreatedAt = appInst.CreatedAt
			newInst.UpdatedAt = time.Now()
			appInst.PrimaryService = newInst.PrimaryService
			appInst.NetworkAnchorID = newInst.NetworkAnchorID
			appInst.Containers = newInst.Containers
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
			m.serviceManager.DeactivateApp(appInst.InstanceID)
			continue
		}

		m.serviceManager.DeactivateApp(appInst.InstanceID)
		return err
	}

	return fmt.Errorf("failed to recreate %s: exhausted host-port retries", appInst.InstanceID)
}

func (m *AppManager) createAndStartServiceContainer(ctx context.Context, runtime container.PodmanRuntime, opts serviceContainerOptions) (string, error) {
	spec, err := m.buildServiceContainerSpec(opts)
	if err != nil {
		return "", fmt.Errorf("build container spec for service '%s': %w", opts.svcName, err)
	}
	// Per-app runtimes must never pull: service containers use --rootfs
	// from golden LV snapshots.
	if spec.Image != "" {
		spec.PullPolicy = "never"
	}

	cid, err := m.createContainerWithRetry(ctx, runtime, spec, fmt.Sprintf("recreate %s service=%s", opts.instanceID, opts.svcName))
	if err != nil {
		return "", fmt.Errorf("create service container '%s': %w", opts.svcName, err)
	}
	if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
		_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		return "", fmt.Errorf("start service container '%s': %w", opts.svcName, err)
	}
	return cid, nil
}

// createContainerWithRetry attempts to create a container, retrying once if the
// failure is due to a zombie container with the same name. PortInUseError is
// returned immediately without retry.
func (m *AppManager) createContainerWithRetry(ctx context.Context, runtime container.PodmanRuntime, spec container.ContainerCreateSpec, logPrefix string) (string, error) {
	var cid string
	var err error
	for i := 0; i < 2; i++ {
		cid, err = m.containerManager.CreateContainer(ctx, runtime, spec)
		if err == nil {
			return cid, nil
		}

		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			return "", err
		}

		zombieID := ""
		var nameErr *container.NameInUseError
		if errors.As(err, &nameErr) {
			zombieID = nameErr.ID
		} else if id, resolveErr := m.containerManager.ResolveContainerIDByName(ctx, runtime, spec.Name); resolveErr == nil {
			zombieID = id
		}
		if zombieID != "" {
			log.Printf("INFO: %s: removing zombie container %s", logPrefix, zombieID)
			_ = m.containerManager.StopContainer(ctx, runtime, zombieID)
			_ = m.containerManager.RemoveContainer(ctx, runtime, zombieID)
			continue
		}
		break
	}
	return "", err
}

func (m *AppManager) stopContainersForMultiApp(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	primary := primaryServiceFor(def, appInst)
	order, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}
	var errs []error
	seenIDs := make(map[string]struct{})
	stopID := func(label, id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, seen := seenIDs[id]; seen {
			return
		}
		seenIDs[id] = struct{}{}
		if err := m.containerManager.StopContainer(ctx, runtime, id); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				errs = append(errs, fmt.Errorf("stop %s (%s): %w", label, id, err))
			}
		}
	}
	stopResolved := func(label, name, stored string) {
		id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name)
		if err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				errs = append(errs, fmt.Errorf("resolve %s by deterministic name %s: %w", label, name, err))
			}
			return
		}
		id = strings.TrimSpace(id)
		if id != "" && id != strings.TrimSpace(stored) {
			stopID(label+" (resolved)", id)
		}
	}

	for i := len(order) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		svcName := order[i]
		stored := strings.TrimSpace(appInst.Containers[svcName])
		stopID(svcName, stored)
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		stopResolved(svcName, name, stored)
	}
	if ctx.Err() == nil {
		anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
		stopID("anchor", anchorID)
		stopResolved("anchor", networkAnchorContainerName(appInst.InstanceID), anchorID)
	}
	if ctx.Err() == nil {
		items, listErr := m.containerManager.ListContainersByLabel(ctx, runtime, "io.piccolo.instance", appInst.InstanceID)
		if listErr != nil {
			errs = append(errs, fmt.Errorf("list containers owned by app %s: %w", appInst.InstanceID, listErr))
		} else {
			for _, item := range items {
				id := strings.TrimSpace(item.ID)
				if id == "" {
					errs = append(errs, fmt.Errorf("container owned by app %s has no ID (name %q)", appInst.InstanceID, item.Name))
					continue
				}
				stopID("label-owned container "+item.Name, id)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *AppManager) removeContainersForMultiApp(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime) error {
	primary := primaryServiceFor(def, appInst)
	order, _ := serviceStartOrder(def.Services)
	var errs []error
	remove := func(label, id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if err := m.containerManager.StopContainer(ctx, runtime, id); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				log.Printf("WARN: remove %s: failed to stop container %s: %v", label, id, err)
			}
		}
		if err := m.containerManager.RemoveContainer(ctx, runtime, id); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				errs = append(errs, fmt.Errorf("remove %s: %w", label, err))
			}
		}
	}

	for i := len(order) - 1; i >= 0; i-- {
		svcName := order[i]
		stored := strings.TrimSpace(appInst.Containers[svcName])
		if stored != "" {
			remove(svcName, stored)
		}
		// Image-update recovery can crash after candidate containers are created
		// but before their IDs are recorded; deterministic names keep cleanup
		// durable across that window.
		name := containerNameForService(appInst.InstanceID, svcName, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil && strings.TrimSpace(id) != "" && id != stored {
			remove(svcName+" (resolved)", id)
		}
	}
	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	if anchorID != "" {
		remove("anchor", anchorID)
	}
	if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(appInst.InstanceID)); err == nil {
		id = strings.TrimSpace(id)
		if id != "" && id != anchorID {
			remove("anchor (resolved)", id)
		}
	}
	return errors.Join(errs...)
}
