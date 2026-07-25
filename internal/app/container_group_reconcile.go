package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/services"
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
	return m.beginAutomaticStartupAttemptWithProjection(state, appInst, true)
}

// beginAutomaticStartupAttemptWithProjection lets observation-prerequisite
// repair consume the existing bounded attempt without replacing a last-known
// Running projection with an unproven Starting/Error state.
func (m *AppManager) beginAutomaticStartupAttemptWithProjection(state *FilesystemStateManager, appInst *AppInstance, projectStarting bool) bool {
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
		if projectStarting {
			m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusError, msgStartupFailed)
		}
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
	if projectStarting {
		m.updateStatusPreservingMessageWithEvent(appInst.InstanceID, StatusStarting)
	}
	return true
}

func (m *AppManager) startupAttemptActive(instanceID string) bool {
	m.startupRecoveryMu.Lock()
	defer m.startupRecoveryMu.Unlock()
	return m.startupRecovery[instanceID].AttemptActive
}

// finishUnknownObservation releases ownership of an in-flight prerequisite
// repair without projecting a failure state. The consumed attempt remains
// persisted and bounds subsequent automatic repair.
func (m *AppManager) finishUnknownObservation(state *FilesystemStateManager, appInst *AppInstance) {
	if appInst == nil {
		return
	}
	m.startupRecoveryMu.Lock()
	window := m.startupRecovery[appInst.InstanceID]
	window.AttemptActive = false
	window.ProbationSince = time.Time{}
	m.startupRecovery[appInst.InstanceID] = window
	m.startupRecoveryMu.Unlock()
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: preserve unknown observation %s: failed to persist startup attempt: %v", appInst.InstanceID, err)
	}
}

// pauseStartupAttemptForAdmission rolls back only the attempt ownership that
// this reconcile pass acquired. A task-pressure fence is a pause signal, not a
// startup failure, so it must not consume the retry budget. Before destructive
// cleanup it restores the last-known projection; after committed removal it
// retains the safe Starting projection instead.
func (m *AppManager) pauseStartupAttemptForAdmission(state *FilesystemStateManager, appInst *AppInstance, previousStatus, previousMessage string) {
	if appInst == nil {
		return
	}
	m.startupRecoveryMu.Lock()
	window := m.startupRecovery[appInst.InstanceID]
	if window.AttemptActive {
		window.AttemptActive = false
		window.ProbationSince = time.Time{}
		m.startupRecovery[appInst.InstanceID] = window
		if appInst.StartupAttempts > 0 {
			appInst.StartupAttempts--
		}
		if appInst.StartupAttempts == 0 {
			appInst.FirstStartupFailureAt = nil
		}
	}
	m.startupRecoveryMu.Unlock()
	if err := state.StoreAppMetadata(appInst); err != nil {
		log.Printf("WARN: pause startup recovery %s: failed to persist admission pause: %v", appInst.InstanceID, err)
	}
	// Once authoritative cleanup has removed the runtime and persisted empty
	// IDs, the former Running projection is no longer safe to restore. Warning
	// may still defer recreation, but the app must remain visibly Starting with
	// publication withdrawn until a later pass completes it.
	if strings.TrimSpace(appInst.NetworkAnchorID) == "" && len(appInst.Containers) == 0 {
		previousStatus = StatusStarting
		previousMessage = "Containers removed; recreation pending"
	} else if previousStatus == "" {
		previousStatus = appInst.Status
	}
	if previousStatus == "" {
		previousStatus = StatusStopped
	}
	m.updateStatusAndMessageWithEvent(appInst.InstanceID, previousStatus, previousMessage)
}

func (m *AppManager) handleStartupEffectFailure(state *FilesystemStateManager, appInst *AppInstance, err error) {
	if pressure.IsAdmissionError(err) {
		return
	}
	m.handleStartupFailure(state, appInst)
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

	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}

	// Remove the broken container
	if removeErr := m.containerManager.RemoveContainer(ctx, runtime, oldCID); removeErr != nil {
		if pressure.IsAdmissionError(removeErr) {
			return removeErr
		}
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

	// Observation and rootfs preflight may take long enough for task pressure to
	// change. Re-check at the destructive boundary so Warning cannot turn a
	// last-known active publication into an avoidable outage.
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	listenerPlan, err := m.serviceManager.PrepareReconcile(appInst.InstanceID, def.Listeners)
	if err != nil {
		return fmt.Errorf("prepare listeners for recovery: %w", err)
	}
	defer listenerPlan.Release()
	// Local denial and adapter withdrawal must precede the first operation that
	// can stop any backend. A later admission failure may mean that only part of
	// the group stopped, so publication cannot safely remain active.
	publicationResumeToken := m.serviceManager.SuspendAppPublication(appInst.InstanceID)

	// Prefer Podman's graceful stop. A dead per-app user manager can leave
	// Podman metadata claiming that containers are running even though their
	// conmon processes were killed. In that case, use the existing PID 1
	// quiescence proof, repair the user session, and continue metadata cleanup
	// through the reacquired rootless runtime.
	if err := m.stopContainersForMultiApp(ctx, appInst, def, runtime); err != nil {
		if pressure.IsAdmissionError(err) {
			return err
		}
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
	if err := m.commitRemovedContainerGroup(state, appInst); err != nil {
		return err
	}
	// Warning can defer expensive recreation, but only after the now-absent
	// runtime has been removed from every active projection.
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}

	return m.recreateMissingMultiContainerPrepared(ctx, state, appInst, def, layout, runtime, prebuiltRootfs, listenerPlan, publicationResumeToken)
}

// cleanupDesiredRunningGroupForRecreate owns the best-effort teardown used by
// desired-running repair paths. Publication is suspended before the first stop
// because an admission failure can arrive after only part of a multi-container
// group has stopped. Once removal succeeds, cleared IDs form the commit that a
// later Warning fence may not roll back.
func (m *AppManager) cleanupDesiredRunningGroupForRecreate(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, runtime container.PodmanRuntime, phase string) (services.PublicationResumeToken, error) {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return services.PublicationResumeToken{}, err
	}
	publicationResumeToken := m.serviceManager.SuspendAppPublication(appInst.InstanceID)
	if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
		if pressure.IsAdmissionError(stopErr) {
			return services.PublicationResumeToken{}, stopErr
		}
		log.Printf("WARN: reconcile app %s: %s stop failed: %v", appInst.InstanceID, phase, stopErr)
	}
	if removeErr := m.removeContainersForMultiApp(ctx, appInst, def, runtime); removeErr != nil {
		if pressure.IsAdmissionError(removeErr) {
			return services.PublicationResumeToken{}, removeErr
		}
		log.Printf("WARN: reconcile app %s: %s remove failed: %v", appInst.InstanceID, phase, removeErr)
		return publicationResumeToken, fmt.Errorf("%s remove failed: %w", phase, removeErr)
	}
	if err := m.commitRemovedContainerGroup(state, appInst); err != nil {
		return publicationResumeToken, err
	}
	return publicationResumeToken, nil
}

// commitRemovedContainerGroup projects an authoritatively absent runtime. It
// runs without a task-pressure admission point because returning after removal
// while keeping active routes or stale IDs would publish a state known to be
// false. The suspension token is the sole authority allowed to republish after
// successful recreation.
func (m *AppManager) commitRemovedContainerGroup(state *FilesystemStateManager, appInst *AppInstance) error {
	candidate, err := detachedAppCandidate(appInst)
	if err != nil {
		return fmt.Errorf("prepare cleared container state for %s: %w", appInst.InstanceID, err)
	}
	candidate.NetworkAnchorID = ""
	candidate.Containers = nil
	candidate.AcceleratorDevices = nil
	candidate.CapabilityBindings = nil
	if err := commitDetachedAppMetadata(state, appInst, candidate); err != nil {
		return fmt.Errorf("persist cleared container state for %s: %w", appInst.InstanceID, err)
	}
	m.serviceManager.SetAppContainerID(appInst.InstanceID, "")
	m.updateStatusAndMessageWithEvent(appInst.InstanceID, StatusStarting, "Containers removed; recreation pending")
	return nil
}

func (m *AppManager) recreateDesiredRunningContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, prebuiltRootfs map[string]*rootfsMountInfo, phase string) error {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	listenerPlan, err := m.serviceManager.PrepareReconcile(appInst.InstanceID, def.Listeners)
	if err != nil {
		return fmt.Errorf("prepare listeners for %s: %w", phase, err)
	}
	defer listenerPlan.Release()

	publicationResumeToken, err := m.cleanupDesiredRunningGroupForRecreate(ctx, state, appInst, def, runtime, phase)
	if err != nil {
		return err
	}
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	return m.recreateMissingMultiContainerPrepared(ctx, state, appInst, def, layout, runtime, prebuiltRootfs, listenerPlan, publicationResumeToken)
}

// reconcileDesiredStoppedContainerGroup proves process absence before
// publishing Stopped. The selected app retains its host device-node permission;
// stopped containers have no mapping or process that can exercise it. Manual
// disable may withdraw publication immediately; a follower retains publication
// until the local workload is authoritatively quiesced.
func (m *AppManager) reconcileDesiredStoppedContainerGroup(
	ctx context.Context,
	state *FilesystemStateManager,
	appInst *AppInstance,
	def *api.AppDefinition,
	runtime container.PodmanRuntime,
) error {
	if !appInst.Enabled && m.serviceManager != nil {
		m.serviceManager.DeactivateApp(appInst.InstanceID)
	}

	if stopErr := m.stopContainersForMultiApp(ctx, appInst, def, runtime); stopErr != nil {
		log.Printf(
			"WARN: reconcile app %s: graceful desired-stopped quiescence failed, quiescing dedicated user unit: %v",
			appInst.InstanceID,
			stopErr,
		)
		if quiesceErr := m.quiesceAppUserSession(ctx, appInst.InstanceID); quiesceErr != nil {
			return errors.Join(
				fmt.Errorf("stop desired-stopped container group: %w", stopErr),
				fmt.Errorf("PID 1 quiesce desired-stopped container group: %w", quiesceErr),
			)
		}
	}

	// A follower's publication remains active until the process-absence proof
	// above succeeds. After that boundary, do not leave routes pointing at a
	// stopped workload.
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(appInst.InstanceID)
	}

	selected, err := m.selectedAcceleratorProvider(state)
	if err != nil {
		return err
	}
	if selected != appInst.InstanceID && len(appInst.AcceleratorDevices) > 0 {
		if err := m.revokeAcceleratorAccess(ctx, state, appInst.InstanceID); err != nil {
			return fmt.Errorf("revoke stale accelerator access for non-selected provider: %w", err)
		}
		appInst.AcceleratorDevices = nil
		if err := state.StoreAppMetadata(appInst); err != nil {
			return fmt.Errorf("persist withdrawn accelerator generation: %w", err)
		}
	}

	m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
	return nil
}

// reconcileContainerGroup reconciles a container group (network anchor + service containers).
// This is the unified reconcile path for both service and workspace modes.
func (m *AppManager) reconcileContainerGroup(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, desiredRunning bool, suppliedObservation ...containerGroupObservation) error {
	if m.serviceManager == nil {
		return fmt.Errorf("app manager: service manager not configured")
	}
	if appInst == nil || def == nil || def.Services == nil {
		return fmt.Errorf("reconcile: invalid container group app state")
	}
	if !desiredRunning {
		return m.reconcileDesiredStoppedContainerGroup(ctx, state, appInst, def, runtime)
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
	// consumers are unreachable when this is nil (the !desiredRunning branch
	// return first).
	var rootfsMap func() (map[string]*rootfsMountInfo, error)
	if desiredRunning {
		rootfsMap = sync.OnceValues(func() (map[string]*rootfsMountInfo, error) {
			return m.ensureAllServiceRootfsAttached(ctx, appInst.InstanceID, mode, def, appInst)
		})
	}
	// Artifact mounts are needed only when creating a service container. Keep
	// steady-state reconciliation independent of artifact size: the first
	// create/recreate consumer attaches and validates the recorded references,
	// and any later consumer in this pass reuses the same handles.
	artifactMap := sync.OnceValues(func() (map[string]persistence.ArtifactHandle, error) {
		return m.ensureAppArtifactAttachments(ctx, state, appInst, def, runtime)
	})

	primary := primaryServiceFor(def, appInst)
	startOrder, err := serviceStartOrder(def.Services)
	if err != nil {
		return err
	}

	var groupObservation containerGroupObservation
	if desiredRunning {
		if len(suppliedObservation) > 0 {
			groupObservation = suppliedObservation[0]
		} else {
			groupObservation = m.observeContainerGroup(ctx, runtime, appInst, def)
		}
		if !groupObservation.known() {
			return fmt.Errorf("container group observation unknown: %w", groupObservation.Err)
		}
		if err := m.applyContainerGroupObservation(state, appInst, groupObservation); err != nil {
			return err
		}
	}

	expectedNames := make(map[string]struct{}, 1+len(def.Services))
	expectedNames[networkAnchorContainerName(appInst.InstanceID)] = struct{}{}
	for svcName := range def.Services {
		expectedNames[containerNameForService(appInst.InstanceID, svcName, primary)] = struct{}{}
	}
	if desiredRunning {
		m.pruneObservedMultiContainerZombies(ctx, runtime, appInst.InstanceID, expectedNames, groupObservation.Owned)
	}

	anchorID := strings.TrimSpace(appInst.NetworkAnchorID)
	anchorState := container.ContainerState{}
	if desiredRunning {
		anchorID = groupObservation.Anchor.ID
		anchorState = groupObservation.Anchor.State
	}

	if anchorID == "" || !anchorState.Exists {
		// If startup failures have exceeded escalation thresholds, stop retrying
		// expensive recreation. This prevents infinite loops when recreation
		// consistently fails (e.g., storage path mismatch after upgrade).
		if shouldEscalateToError(appInst) && !m.startupAttemptActive(appInst.InstanceID) {
			// Clean up stale containers/proxies once on first escalation,
			// then skip on subsequent cycles (status already error).
			if m.getObservedStatus(appInst.InstanceID) != StatusError {
				if _, err := m.cleanupDesiredRunningGroupForRecreate(ctx, state, appInst, def, runtime, "escalation"); err != nil {
					return err
				}
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
		m.setObservedStatusMessage(appInst.InstanceID, "Containers not found, recreating")
		if err := m.recreateDesiredRunningContainerGroup(ctx, state, appInst, def, layout, runtime, bn, "pre-recreate"); err != nil {
			m.handleStartupEffectFailure(state, appInst, err)
			return err
		}
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
			if pressure.IsAdmissionError(err) {
				return err
			}
			// Container can't start — remove and recreate the entire group.
			// Covers all stale state: reboot (wiped /run/), dead netns, corrupted runtime, etc.
			m.setObservedStatusMessage(appInst.InstanceID, "Container start failed, recreating")
			bn, rootfsErr := rootfsMap()
			if rootfsErr != nil {
				return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
			}
			if recoverErr := m.recoverStaleAnchor(ctx, state, appInst, def, layout, runtime,
				fmt.Sprintf("anchor start failed (%v), recreating container group", err), bn); recoverErr != nil {
				m.handleStartupEffectFailure(state, appInst, recoverErr)
				return recoverErr
			}
			return nil
		}
	}

	m.serviceManager.SetAppContainerID(appInst.InstanceID, anchorID)
	desiredBindings, err := desiredCapabilityBindings(state, appInst.InstanceID, def)
	if err != nil {
		return fmt.Errorf("resolve capability bindings: %w", err)
	}
	if !capabilityGenerationMatches(appInst, desiredBindings) {
		bn, rootfsErr := rootfsMap()
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
		}
		m.setObservedStatusMessage(appInst.InstanceID, "Reconciling capability bindings")
		if err := m.recreateDesiredRunningContainerGroup(ctx, state, appInst, def, layout, runtime, bn, "capability-binding-reconcile"); err != nil {
			m.handleStartupEffectFailure(state, appInst, err)
			return err
		}
		return nil
	}
	bindingEnvironment, err := m.ensureCapabilityBindingEnvironment(ctx, state, appInst, def, anchorID, runtime)
	if err != nil {
		return fmt.Errorf("prepare capability bindings: %w", err)
	}
	acceleratorUIDs := []uint32(nil)
	desiredAccelerators, err := m.desiredAcceleratorDevices(state, appInst.InstanceID, def)
	if err != nil {
		return fmt.Errorf("resolve accelerator grant: %w", err)
	}
	if len(desiredAccelerators) > 0 {
		bn, rootfsErr := rootfsMap()
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
		}
		acceleratorUIDs, err = acceleratorHostUIDs(appInst.InstanceID, runtime, def, bn)
		if err != nil {
			return fmt.Errorf("resolve accelerator principals: %w", err)
		}
	}
	acceleratorDevices, err := m.ensureAcceleratorAccess(
		ctx,
		state,
		appInst.InstanceID,
		runtime,
		def,
		acceleratorUIDs,
	)
	if err != nil {
		return fmt.Errorf("prepare accelerator grant: %w", err)
	}
	if !acceleratorGenerationMatches(appInst, acceleratorDevices) {
		if len(appInst.AcceleratorDevices) > 0 && len(acceleratorDevices) == 0 {
			m.setObservedStatusMessage(appInst.InstanceID, "Withdrawing accelerator devices")
			if err := m.recreateAppForCapabilityEffects(ctx, state, appInst.InstanceID, func() error {
				return m.revokeAcceleratorAccess(ctx, state, appInst.InstanceID)
			}); err != nil {
				m.handleStartupEffectFailure(state, appInst, err)
				return err
			}
			return nil
		}
		bn, rootfsErr := rootfsMap()
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
		}
		m.setObservedStatusMessage(appInst.InstanceID, "Reconciling accelerator devices")
		if err := m.recreateDesiredRunningContainerGroup(ctx, state, appInst, def, layout, runtime, bn, "accelerator-reconcile"); err != nil {
			m.handleStartupEffectFailure(state, appInst, err)
			return err
		}
		return nil
	}

	// Ensure all declared services exist and are running.
	for _, svcName := range startOrder {
		observedService := groupObservation.Services[svcName]
		cid := observedService.ID
		st := observedService.State

		if cid == "" || !st.Exists {
			m.setObservedStatusMessage(appInst.InstanceID, fmt.Sprintf("Recreating service '%s'", svcName))
			bn, rootfsErr := rootfsMap()
			if rootfsErr != nil {
				return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
			}
			artifactHandles, artifactErr := artifactMap()
			if artifactErr != nil {
				return fmt.Errorf("failed to attach artifacts: %w", artifactErr)
			}
			opts := serviceContainerOptions{
				layout:             layout,
				appDef:             def,
				instanceID:         appInst.InstanceID,
				primary:            primary,
				svcName:            svcName,
				anchorID:           anchorID,
				credential:         runtime.Credential,
				artifactHandles:    artifactHandles,
				bindingEnvironment: bindingEnvironment[svcName],
				acceleratorDevices: acceleratorDevices,
			}
			if svcRootfs, ok := bn[svcName]; ok {
				opts.rootfsHandle = &svcRootfs.handle
				opts.goldenImgConfig = &svcRootfs.imgConfig
			}
			newCID, err := m.createAndStartServiceContainer(ctx, runtime, opts)
			if err != nil {
				m.handleStartupEffectFailure(state, appInst, err)
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
				if pressure.IsAdmissionError(err) {
					return err
				}
				log.Printf("INFO: reconcile app %s: service '%s' (cid=%s) start failed (%v), recreating",
					appInst.InstanceID, svcName, cid, err)
				m.setObservedStatusMessage(appInst.InstanceID, "Service start failed, recreating")

				bn, rootfsErr := rootfsMap()
				if rootfsErr != nil {
					return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
				}
				artifactHandles, artifactErr := artifactMap()
				if artifactErr != nil {
					return fmt.Errorf("failed to attach artifacts: %w", artifactErr)
				}
				opts := serviceContainerOptions{
					layout:             layout,
					appDef:             def,
					instanceID:         appInst.InstanceID,
					primary:            primary,
					svcName:            svcName,
					anchorID:           anchorID,
					credential:         runtime.Credential,
					artifactHandles:    artifactHandles,
					bindingEnvironment: bindingEnvironment[svcName],
					acceleratorDevices: acceleratorDevices,
				}
				if svcRootfs, ok := bn[svcName]; ok {
					opts.rootfsHandle = &svcRootfs.handle
					opts.goldenImgConfig = &svcRootfs.imgConfig
				}
				if err := m.recreateServiceContainer(ctx, state, appInst, runtime, cid, opts); err != nil {
					m.handleStartupEffectFailure(state, appInst, err)
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
	restoreServicesErr := m.ensureServicesForRunningApp(ctx, def, appInst.InstanceID, anchorID, runtime)
	if restoreServicesErr != nil {
		log.Printf("WARN: reconcile app %s: restore services failed: %v", appInst.InstanceID, restoreServicesErr)
	}
	publishErr := m.ensurePodmanPublishes(ctx, def, appInst.InstanceID, anchorID, runtime)
	// A partial Podman binding snapshot leaves no complete registry for the
	// ordinary publish comparison. Preserve that typed mismatch so the existing
	// recreation owner repairs the container rather than merely retrying a
	// permanently incomplete publication.
	if errors.Is(restoreServicesErr, container.ErrPortReconciliationRequired) {
		publishErr = restoreServicesErr
	}
	if err := publishErr; err != nil {
		if errors.Is(err, container.ErrPortReconciliationRequired) {
			if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
				return err
			}
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
			if err := m.recreateDesiredRunningContainerGroup(ctx, state, appInst, def, layout, runtime, bn, "port-reconcile"); err != nil {
				m.handleStartupEffectFailure(state, appInst, err)
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
			} else if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
				return err
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

func (m *AppManager) pruneObservedMultiContainerZombies(ctx context.Context, runtime container.PodmanRuntime, instanceID string, expectedNames map[string]struct{}, items []container.ContainerListItem) {
	if m.containerManager == nil {
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

// pruneMultiContainerZombies is retained for explicit install cleanup, where
// the install request itself owns mutation. Reconciliation instead supplies
// the enumeration from its complete observation snapshot.
func (m *AppManager) pruneMultiContainerZombies(ctx context.Context, runtime container.PodmanRuntime, instanceID string, expectedNames map[string]struct{}) {
	items, err := m.containerManager.ListContainersByLabel(ctx, runtime, "io.piccolo.instance", instanceID)
	if err != nil {
		log.Printf("WARN: app %s: list containers for explicit cleanup failed: %v", instanceID, err)
		return
	}
	m.pruneObservedMultiContainerZombies(ctx, runtime, instanceID, expectedNames, items)
}

// detachedAppCandidate returns a caller-owned copy suitable for staging
// positive replacement state. FilesystemStateManager.GetApp returns the
// cache-owned committed instance, so candidate fields must never be assigned
// to that pointer before the corresponding durable write succeeds.
func detachedAppCandidate(current *AppInstance) (*AppInstance, error) {
	if current == nil {
		return nil, fmt.Errorf("detached app candidate requires current state")
	}
	candidate := *current
	candidate.Containers = cloneStringMap(current.Containers)
	candidate.ActiveRootfs = cloneStringMap(current.ActiveRootfs)
	candidate.ArtifactReferences = cloneStringMap(current.ArtifactReferences)
	candidate.AcceleratorDevices = append([]string(nil), current.AcceleratorDevices...)
	candidate.CapabilityBindings = cloneStringMap(current.CapabilityBindings)
	candidate.Init = cloneInitState(current.Init)
	candidate.InitScriptHashes = cloneStringMap(current.InitScriptHashes)
	return &candidate, nil
}

func recreatedAppCandidate(current, recreated *AppInstance) (*AppInstance, error) {
	if recreated == nil {
		return nil, fmt.Errorf("recreated app candidate requires replacement state")
	}
	candidate, err := detachedAppCandidate(current)
	if err != nil {
		return nil, err
	}
	candidate.PrimaryService = recreated.PrimaryService
	candidate.NetworkAnchorID = recreated.NetworkAnchorID
	candidate.Containers = cloneStringMap(recreated.Containers)
	candidate.ArtifactReferences = cloneStringMap(recreated.ArtifactReferences)
	candidate.AcceleratorDevices = append([]string(nil), recreated.AcceleratorDevices...)
	candidate.CapabilityBindings = cloneStringMap(recreated.CapabilityBindings)
	candidate.UpdatedAt = time.Now()
	return candidate, nil
}

// commitDetachedApp publishes a positive candidate only after both app files
// are durable. The prior cache pointer is synchronized afterward for callers
// that retain it across the lifecycle transaction.
func commitDetachedApp(
	state *FilesystemStateManager,
	current, candidate *AppInstance,
) error {
	if state == nil || current == nil || candidate == nil || current == candidate {
		return fmt.Errorf("detached app commit requires distinct current and candidate state")
	}
	published, err := detachedAppCandidate(candidate)
	if err != nil {
		return err
	}
	if err := state.StoreApp(published); err != nil {
		return err
	}
	synced, err := detachedAppCandidate(published)
	if err != nil {
		return err
	}
	*current = *synced
	return nil
}

func commitDetachedAppMetadata(
	state *FilesystemStateManager,
	current, candidate *AppInstance,
) error {
	if state == nil || current == nil || candidate == nil || current == candidate {
		return fmt.Errorf("detached metadata commit requires distinct current and candidate state")
	}
	published, err := detachedAppCandidate(candidate)
	if err != nil {
		return err
	}
	if err := state.StoreAppMetadata(published); err != nil {
		return err
	}
	synced, err := detachedAppCandidate(published)
	if err != nil {
		return err
	}
	*current = *synced
	return nil
}

func commitRecreatedAppMetadata(
	state *FilesystemStateManager,
	current, recreated *AppInstance,
) error {
	candidate, err := recreatedAppCandidate(current, recreated)
	if err != nil {
		return err
	}
	return commitDetachedAppMetadata(state, current, candidate)
}

type uncommittedContainerGroupMaySurviveError struct {
	cause   error
	cleanup error
}

func (e *uncommittedContainerGroupMaySurviveError) Error() string {
	return errors.Join(
		e.cause,
		fmt.Errorf("candidate process absence is unproven: %w", e.cleanup),
	).Error()
}

func (e *uncommittedContainerGroupMaySurviveError) Unwrap() []error {
	return []error{e.cause, e.cleanup}
}

func uncommittedContainerGroupMaySurvive(err error) bool {
	var target *uncommittedContainerGroupMaySurviveError
	return errors.As(err, &target)
}

// removeUncommittedContainerGroup withdraws reachability and proves that no
// process created by an uncommitted candidate remains. Container identity is
// attempted first because it preserves the app user session; PID 1 user-session
// quiescence is the authoritative fallback when Podman cannot prove removal.
// Create/start errors may be ambiguous, and returned IDs may be missing or
// stale.
func (m *AppManager) removeUncommittedContainerGroup(
	ctx context.Context,
	candidate *AppInstance,
	def *api.AppDefinition,
	runtime container.PodmanRuntime,
) error {
	if candidate == nil || def == nil || strings.TrimSpace(candidate.InstanceID) == "" {
		return fmt.Errorf("candidate container group is required for removal")
	}
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(candidate.InstanceID)
		m.serviceManager.SetAppContainerID(candidate.InstanceID, "")
	}
	m.removeCapabilityIngresses(candidate.InstanceID)

	var removalErrs []error
	quiesceAfterPodmanFailure := func(podmanErr error) error {
		if podmanErr != nil {
			removalErrs = append(removalErrs, podmanErr)
		}
		if err := m.quiesceAppUserSession(ctx, candidate.InstanceID); err != nil {
			removalErrs = append(removalErrs, fmt.Errorf("quiesce candidate user session: %w", err))
			return errors.Join(removalErrs...)
		}
		log.Printf(
			"WARN: Podman could not prove candidate container removal for %s; authoritative user-session quiescence proved process absence",
			candidate.InstanceID,
		)
		return nil
	}
	// Known IDs and deterministic names cover the normal case. A final
	// label-owned sweep covers an ambiguous create that succeeded without
	// returning its ID.
	if err := m.removeContainersForMultiApp(ctx, candidate, def, runtime); err != nil {
		log.Printf("WARN: remove uncommitted group %s by recorded identity: %v", candidate.InstanceID, err)
		removalErrs = append(removalErrs, err)
	}
	items, err := m.containerManager.ListContainersByLabel(
		ctx,
		runtime,
		"io.piccolo.instance",
		candidate.InstanceID,
	)
	if err != nil {
		return quiesceAfterPodmanFailure(
			fmt.Errorf("list candidate containers for absence proof: %w", err),
		)
	}
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if err := m.containerManager.StopContainer(ctx, runtime, id); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				log.Printf("WARN: stop uncommitted container %s (%s): %v", item.Name, id, err)
				removalErrs = append(removalErrs, fmt.Errorf("stop %s: %w", item.Name, err))
			}
		}
		if err := m.containerManager.RemoveContainer(ctx, runtime, id); err != nil {
			var notFound *container.ContainerNotFoundError
			if !errors.As(err, &notFound) {
				log.Printf("WARN: remove uncommitted container %s (%s): %v", item.Name, id, err)
				removalErrs = append(removalErrs, fmt.Errorf("remove %s: %w", item.Name, err))
			}
		}
	}
	remaining, err := m.containerManager.ListContainersByLabel(
		ctx,
		runtime,
		"io.piccolo.instance",
		candidate.InstanceID,
	)
	if err != nil {
		return quiesceAfterPodmanFailure(
			fmt.Errorf("verify candidate container absence: %w", err),
		)
	}
	if len(remaining) == 0 {
		return nil
	}
	names := make([]string, 0, len(remaining))
	for _, item := range remaining {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = strings.TrimSpace(item.ID)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	removalErrs = append(removalErrs, fmt.Errorf("candidate containers remain: %s", strings.Join(names, ", ")))
	return quiesceAfterPodmanFailure(nil)
}

// compensateUncommittedContainerGroup withdraws the live effects returned by
// installContainerGroup when the caller cannot commit the candidate metadata.
// The committed app remains the recovery authority; deterministic container
// names and durable artifact/accelerator ownership make a partial retry
// discoverable without adding another transition protocol.
func (m *AppManager) compensateUncommittedContainerGroup(
	state *FilesystemStateManager,
	committed, candidate *AppInstance,
	def *api.AppDefinition,
	runtime container.PodmanRuntime,
) error {
	if candidate == nil || def == nil {
		return fmt.Errorf("candidate container group is required for compensation")
	}
	cleanupCtx, cancel := context.WithTimeout(
		pressure.WithTransitionContinuation(context.Background()),
		cleanupBudget,
	)
	defer cancel()

	// Mount ownership cannot be withdrawn until every candidate process is
	// authoritatively absent. Accelerator permission belongs to the selected app
	// instance, so a failed replacement must not revoke it with the candidate
	// generation.
	if err := m.removeUncommittedContainerGroup(cleanupCtx, candidate, def, runtime); err != nil {
		return fmt.Errorf("remove candidate containers: %w", err)
	}

	var errs []error
	if err := m.detachArtifactReferences(cleanupCtx, candidate.ArtifactReferences); err != nil {
		errs = append(errs, fmt.Errorf("detach candidate artifacts: %w", err))
	}
	committedArtifacts := map[string]string(nil)
	if committed != nil {
		committedArtifacts = committed.ArtifactReferences
	}
	if err := m.discardUncommittedArtifactReferences(
		cleanupCtx,
		candidate.ArtifactReferences,
		committedArtifacts,
	); err != nil {
		errs = append(errs, fmt.Errorf("destroy candidate artifact references: %w", err))
	}
	m.detachAllServiceRootfs(
		cleanupCtx,
		candidate.InstanceID,
		piccoloModeFromExtensions(def.Extensions),
		def,
		candidate,
	)
	return errors.Join(errs...)
}

func (m *AppManager) abortUncommittedContainerGroup(
	cause error,
	state *FilesystemStateManager,
	committed, candidate *AppInstance,
	def *api.AppDefinition,
	runtime container.PodmanRuntime,
) error {
	if cleanupErr := m.compensateUncommittedContainerGroup(
		state,
		committed,
		candidate,
		def,
		runtime,
	); cleanupErr != nil {
		return errors.Join(cause, fmt.Errorf("compensate uncommitted container group: %w", cleanupErr))
	}
	return cause
}

func (m *AppManager) recreateMissingMultiContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, prebuiltRootfs map[string]*rootfsMountInfo) error {
	// Allocate endpoints and recreate the group (anchor + services).
	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		endpoints, err := m.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners)
		if err != nil {
			return fmt.Errorf("allocate service ports: %w", err)
		}
		m.configureOIDCAuthorizePaths(appInst.InstanceID, def)

		newInst, err := m.installContainerGroup(ctx, def, appInst.InstanceID, layout, runtime, endpoints, prebuiltRootfs, true, false)
		if err == nil {
			if err := commitRecreatedAppMetadata(state, appInst, newInst); err != nil {
				commitErr := fmt.Errorf("persist recovered container state for %s: %w", appInst.InstanceID, err)
				return m.abortUncommittedContainerGroup(
					commitErr,
					state,
					appInst,
					newInst,
					def,
					runtime,
				)
			}
			m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
			return nil
		}
		if uncommittedContainerGroupMaySurvive(err) {
			return err
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

func (m *AppManager) recreateMissingMultiContainerPrepared(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime, prebuiltRootfs map[string]*rootfsMountInfo, initialPlan *services.PreparedReconcile, publicationResumeToken services.PublicationResumeToken) error {
	listenerPlan := initialPlan
	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		if listenerPlan == nil {
			var err error
			listenerPlan, err = m.serviceManager.PrepareReconcile(appInst.InstanceID, def.Listeners)
			if err != nil {
				return fmt.Errorf("prepare service ports: %w", err)
			}
		}
		endpoints := listenerPlan.Endpoints()
		if len(endpoints) == 0 && len(def.Listeners) > 0 && !allowMissingListenerEndpointsForTest() {
			listenerPlan.Release()
			return fmt.Errorf("prepare service ports: no endpoints for %d listeners", len(def.Listeners))
		}
		m.configureOIDCAuthorizePaths(appInst.InstanceID, def)

		newInst, err := m.installContainerGroup(ctx, def, appInst.InstanceID, layout, runtime, endpoints, prebuiltRootfs, true, false)
		if err == nil {
			if err := commitRecreatedAppMetadata(state, appInst, newInst); err != nil {
				listenerPlan.Release()
				commitErr := fmt.Errorf("persist recovered container state for %s: %w", appInst.InstanceID, err)
				return m.abortUncommittedContainerGroup(
					commitErr,
					state,
					appInst,
					newInst,
					def,
					runtime,
				)
			}
			publicationCtx := pressure.WithTransitionContinuation(ctx)
			if _, _, err := listenerPlan.PublishWithResumeTokenContext(publicationCtx, publicationResumeToken); err != nil {
				listenerPlan.Release()
				return fmt.Errorf("publish recovered listeners: %w", err)
			}
			m.serviceManager.SetAppContainerID(appInst.InstanceID, appInst.NetworkAnchorID)
			m.updateStatusWithEvent(appInst.InstanceID, StatusRunning)
			return nil
		}
		if uncommittedContainerGroupMaySurvive(err) {
			listenerPlan.Release()
			return err
		}

		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			listenerPlan.Release()
			listenerPlan = nil
			log.Printf("WARN: recreate app %s: host port conflict port=%d attempt=%d", appInst.InstanceID, portErr.Port, attempt)
			if portErr.Port > 0 {
				_ = m.serviceManager.ReserveHostPort(portErr.Port)
			} else {
				for _, ep := range endpoints {
					_ = m.serviceManager.ReserveHostPort(ep.HostBind)
				}
			}
			// Drop the inactive registry allocation before preparing the retry so
			// it can choose a different host bind. DeactivateApp preserves an exact
			// suspension record, so the owning resume token remains authoritative.
			m.serviceManager.DeactivateApp(appInst.InstanceID)
			continue
		}

		listenerPlan.Release()
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
