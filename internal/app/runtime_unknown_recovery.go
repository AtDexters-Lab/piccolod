package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/events"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/state/paths"
)

const (
	persistentUnknownAttempts = 3
	persistentUnknownDuration = 60 * time.Second
	runtimeQuarantineSuffix   = ".task-recovery-quarantine"
)

type observationFailureCause string

const (
	observationCauseTaskPressure     observationFailureCause = "task_pressure"
	observationCauseUserSession      observationFailureCause = "dedicated_user_session"
	observationCauseCancellation     observationFailureCause = "cancellation_timeout"
	observationCausePodmanControl    observationFailureCause = "podman_control_plane"
	observationCauseSharedStorage    observationFailureCause = "shared_storage"
	observationCauseDedicatedRuntime observationFailureCause = "dedicated_runtime_storage"
	observationCauseInvalidOutput    observationFailureCause = "invalid_output"
)

type unknownObservationWindow struct {
	Cause          observationFailureCause
	Count          int
	FirstAt        time.Time
	LastAt         time.Time
	LastGeneration uint64
}

func (m *AppManager) beginObservationPass() {
	m.unknownObservationMu.Lock()
	m.observationGeneration++
	m.unknownObservationMu.Unlock()
}

func classifyObservationFailure(err error) observationFailureCause {
	var invalidOutput *container.InvalidOutputError
	switch {
	case pressure.IsAdmissionError(err):
		return observationCauseTaskPressure
	case errors.Is(err, container.ErrUserSessionUnavailable):
		return observationCauseUserSession
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return observationCauseCancellation
	case errors.Is(err, errAppVolumeObservationUnavailable), errors.Is(err, ErrVolumeUnavailable):
		return observationCauseSharedStorage
	case errors.Is(err, errAppRuntimeObservationUnavailable):
		return observationCauseDedicatedRuntime
	case errors.As(err, &invalidOutput):
		return observationCauseInvalidOutput
	default:
		return observationCausePodmanControl
	}
}

func (m *AppManager) recordUnknownObservation(instanceID string, causeErr error) unknownObservationWindow {
	now := time.Now()
	cause := classifyObservationFailure(causeErr)
	m.unknownObservationMu.Lock()
	generation := m.observationGeneration
	window := m.unknownObservations[instanceID]
	if window.Count == 0 || window.Cause != cause || (window.LastGeneration != 0 && generation > window.LastGeneration+1) {
		window = unknownObservationWindow{Cause: cause, FirstAt: now}
	}
	if window.Count == 0 || window.LastGeneration != generation {
		window.Count++
	}
	window.LastAt = now
	window.LastGeneration = generation
	m.unknownObservations[instanceID] = window
	m.unknownObservationMu.Unlock()
	m.publishCurrentRuntimeObservationPressure(instanceID)
	return window
}

func (m *AppManager) publishRuntimeObservationPressure(event events.ResourcePressureEvent) {
	bus := m.currentEventBus()
	if bus == nil {
		return
	}
	bus.Publish(events.Event{
		Topic:   events.TopicResourcePressure,
		Payload: event,
	})
}

func (m *AppManager) currentRuntimeObservationPressure(instanceID string) events.ResourcePressureEvent {
	event := events.ResourcePressureEvent{
		Resource:      events.PressureResourceRuntime,
		Severity:      events.PressureSeverityOK,
		AppInstanceID: instanceID,
		ReasonCode:    "normal",
		Message:       "Runtime observation recovered",
	}

	m.automaticSuppressionMu.RLock()
	suppressionReason, suppressed := m.automaticSuppression[instanceID]
	m.automaticSuppressionMu.RUnlock()
	if suppressed {
		event.Severity = events.PressureSeverityWarn
		event.ReasonCode = "automatic_recovery_suppressed"
		event.Message = suppressionReason
		return event
	}

	m.unknownObservationMu.Lock()
	window, unknown := m.unknownObservations[instanceID]
	m.unknownObservationMu.Unlock()
	if unknown {
		event.Severity = events.PressureSeverityWarn
		event.ReasonCode = "observation_unknown"
		event.Message = fmt.Sprintf("Runtime observation unavailable (%s); retaining last known state", window.Cause)
	}
	return event
}

func (m *AppManager) publishCurrentRuntimeObservationPressure(instanceID string) {
	m.publishRuntimeObservationPressure(m.currentRuntimeObservationPressure(instanceID))
}

// RuntimeObservationPressureSnapshot returns retained degraded observations so
// event-stream reconnects do not depend on having witnessed the original edge.
func (m *AppManager) RuntimeObservationPressureSnapshot() []events.ResourcePressureEvent {
	byInstanceID := make(map[string]events.ResourcePressureEvent)
	m.unknownObservationMu.Lock()
	for instanceID, window := range m.unknownObservations {
		byInstanceID[instanceID] = events.ResourcePressureEvent{
			Resource:      events.PressureResourceRuntime,
			Severity:      events.PressureSeverityWarn,
			AppInstanceID: instanceID,
			ReasonCode:    "observation_unknown",
			Message:       fmt.Sprintf("Runtime observation unavailable (%s); retaining last known state", window.Cause),
		}
	}
	m.unknownObservationMu.Unlock()
	m.automaticSuppressionMu.RLock()
	for instanceID, reason := range m.automaticSuppression {
		byInstanceID[instanceID] = events.ResourcePressureEvent{
			Resource:      events.PressureResourceRuntime,
			Severity:      events.PressureSeverityWarn,
			AppInstanceID: instanceID,
			ReasonCode:    "automatic_recovery_suppressed",
			Message:       reason,
		}
	}
	m.automaticSuppressionMu.RUnlock()
	out := make([]events.ResourcePressureEvent, 0, len(byInstanceID))
	for _, event := range byInstanceID {
		out = append(out, event)
	}
	return out
}

func (m *AppManager) SuppressAutomaticRecovery(instanceID, reason string) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "Automatic recovery paused after repeated task-pressure restarts"
	}
	m.automaticSuppressionMu.Lock()
	if m.automaticSuppression == nil {
		m.automaticSuppression = make(map[string]string)
	}
	m.automaticSuppression[instanceID] = reason
	m.automaticSuppressionMu.Unlock()
	m.publishCurrentRuntimeObservationPressure(instanceID)
}

func (m *AppManager) automaticRecoverySuppressed(instanceID string) bool {
	m.automaticSuppressionMu.RLock()
	_, suppressed := m.automaticSuppression[instanceID]
	m.automaticSuppressionMu.RUnlock()
	return suppressed
}

func (m *AppManager) clearAutomaticRecoverySuppression(instanceID string) {
	m.automaticSuppressionMu.Lock()
	_, existed := m.automaticSuppression[instanceID]
	delete(m.automaticSuppression, instanceID)
	m.automaticSuppressionMu.Unlock()
	if existed {
		m.publishCurrentRuntimeObservationPressure(instanceID)
	}
}

// ReleaseAutomaticRecoverySuppression returns one app to ordinary background
// reconciliation after the serialized task-recovery owner for that app has
// returned. It deliberately does not trigger an immediate reconcile: the
// explicit owner attempt remains the only startup mutation for that app, and
// the steady loop resumes on its normal cadence.
func (m *AppManager) ReleaseAutomaticRecoverySuppression(instanceID string) {
	m.clearAutomaticRecoverySuppression(strings.TrimSpace(instanceID))
}

func (m *AppManager) clearUnknownObservation(instanceID string) {
	m.unknownObservationMu.Lock()
	_, existed := m.unknownObservations[instanceID]
	delete(m.unknownObservations, instanceID)
	m.unknownObservationMu.Unlock()
	if existed {
		m.publishCurrentRuntimeObservationPressure(instanceID)
	}
}

// retireRuntimeObservation removes every process-local degraded cause for an
// app that has been authoritatively uninstalled. The final OK edge lets live
// clients remove stale degradation; reconnect snapshots contain no entry.
func (m *AppManager) retireRuntimeObservation(instanceID string) {
	m.unknownObservationMu.Lock()
	_, unknown := m.unknownObservations[instanceID]
	delete(m.unknownObservations, instanceID)
	m.unknownObservationMu.Unlock()

	m.automaticSuppressionMu.Lock()
	_, suppressed := m.automaticSuppression[instanceID]
	delete(m.automaticSuppression, instanceID)
	m.automaticSuppressionMu.Unlock()

	if unknown || suppressed {
		m.publishRuntimeObservationPressure(events.ResourcePressureEvent{
			Resource:      events.PressureResourceRuntime,
			Severity:      events.PressureSeverityOK,
			AppInstanceID: instanceID,
			ReasonCode:    "app_removed",
			Message:       "Runtime observation retired after app removal",
		})
	}
}

func (m *AppManager) unknownRecoveryEligible(instanceID string, window unknownObservationWindow) bool {
	if window.Count < persistentUnknownAttempts || time.Since(window.FirstAt) < persistentUnknownDuration {
		return false
	}
	m.stateMu.RLock()
	normalFn := m.taskPressureNormal
	m.stateMu.RUnlock()
	if normalFn != nil {
		if !normalFn() {
			return false
		}
	} else if pressure.DefaultAdmission.Fenced() {
		return false
	}
	switch window.Cause {
	case observationCausePodmanControl, observationCauseDedicatedRuntime, observationCauseInvalidOutput:
	default:
		return false
	}

	m.unknownObservationMu.Lock()
	defer m.unknownObservationMu.Unlock()
	currentGeneration := m.observationGeneration
	for otherID, other := range m.unknownObservations {
		if otherID == instanceID || other.Cause != window.Cause {
			continue
		}
		if other.LastGeneration+1 >= currentGeneration {
			return false
		}
	}
	return true
}

func (m *AppManager) handleUnknownContainerGroup(
	ctx context.Context,
	state *FilesystemStateManager,
	appInst *AppInstance,
	appDef *api.AppDefinition,
	layout appVolumeLayout,
	mode PiccoloMode,
	causeErr error,
) error {
	if appDef == nil {
		return nil
	}
	window := m.recordUnknownObservation(appInst.InstanceID, causeErr)
	if !m.unknownRecoveryEligible(appInst.InstanceID, window) {
		if m.startupAttemptActive(appInst.InstanceID) {
			m.finishUnknownObservation(state, appInst)
		}
		return nil
	}

	previousStatus, previousMessage := m.getObservedStatusAndMessage(appInst.InstanceID)
	lastKnownRunning := previousStatus == StatusRunning
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return nil
	}
	if !m.beginAutomaticStartupAttemptWithProjection(state, appInst, !lastKnownRunning) {
		return nil
	}
	if err := m.runSharedRuntimeSentinel(ctx); err != nil {
		log.Printf("WARN: app %s: persistent unknown is not exclusively attributable; sentinel failed: %v", appInst.InstanceID, err)
		if pressure.IsAdmissionError(err) {
			m.pauseStartupAttemptForAdmission(state, appInst, previousStatus, previousMessage)
		} else if lastKnownRunning {
			m.finishUnknownObservation(state, appInst)
		} else {
			m.handleStartupFailure(state, appInst)
		}
		return nil
	}
	if err := m.recoverPersistentUnknownRuntime(ctx, state, appInst, appDef, layout, mode); err != nil {
		if pressure.IsAdmissionError(err) {
			m.pauseStartupAttemptForAdmission(state, appInst, previousStatus, previousMessage)
		} else if lastKnownRunning {
			m.finishUnknownObservation(state, appInst)
		} else {
			m.handleStartupFailure(state, appInst)
		}
		return err
	}
	m.clearUnknownObservation(appInst.InstanceID)
	return nil
}

func (m *AppManager) runSharedRuntimeSentinel(ctx context.Context) error {
	if m.runtimeSentinel != nil {
		return m.runtimeSentinel(ctx)
	}
	runtime, cleanup, err := newEphemeralFlattenRuntime(paths.CoreJoin("tmp"), m.runtimeUser)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = m.containerManager.ListContainersByLabel(ctx, runtime, "io.piccolo.sentinel", "none")
	return err
}

func (m *AppManager) recoverPersistentUnknownRuntime(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, mode PiccoloMode) error {
	if err := m.quiesceAppUserSession(ctx, appInst.InstanceID); err != nil {
		return fmt.Errorf("prove dedicated app cgroup empty: %w", err)
	}
	// Unknown recovery retains last-known routes unless PID 1 proves this
	// dedicated cgroup empty. Once that proof succeeds, the old backend is
	// authoritatively absent and publication must fail closed before repair.
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(appInst.InstanceID)
	}
	runtime, err := m.podmanRuntimeForApp(ctx, appInst.InstanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return fmt.Errorf("reacquire dedicated runtime: %w", err)
	}
	observed := m.observeContainerGroup(ctx, runtime, appInst, def)
	if observed.known() {
		return m.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true, observed)
	}
	_, _ = m.containerManager.ValidateAndRepairStorage(ctx, runtime)
	observed = m.observeContainerGroup(ctx, runtime, appInst, def)
	if observed.known() {
		return m.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true, observed)
	}
	if err := m.validateRuntimeRecoveryDependencies(ctx, appInst, def, mode); err != nil {
		return err
	}
	if err := m.runSharedRuntimeSentinel(ctx); err != nil {
		return fmt.Errorf("shared runtime sentinel vetoed quarantine: %w", err)
	}

	record, err := newRuntimeRecoveryRecord(appInst, mode, runtime)
	if err != nil {
		return err
	}
	if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
		return fmt.Errorf("persist runtime quarantine intent: %w", err)
	}
	return m.recoverRuntimeRecoveryTransition(ctx, state, appInst, record)
}

func (m *AppManager) validateRuntimeRecoveryDependencies(ctx context.Context, appInst *AppInstance, def *api.AppDefinition, mode PiccoloMode) error {
	attached, err := m.ensureAllServiceRootfsAttached(ctx, appInst.InstanceID, mode, def, appInst)
	if err != nil {
		return fmt.Errorf("shared rootfs dependency unavailable: %w", err)
	}
	if attached == nil || attached[networkAnchorServiceName] == nil {
		return fmt.Errorf("shared rootfs dependency unavailable: network anchor rootfs missing")
	}
	if mode == ModeWorkspace {
		primary := primaryServiceFor(def, appInst)
		if attached[primary] == nil {
			return fmt.Errorf("shared rootfs dependency unavailable: workspace rootfs missing")
		}
		return nil
	}
	for serviceName, service := range def.Services {
		if service.Image != "" && attached[serviceName] == nil {
			return fmt.Errorf("shared rootfs dependency unavailable: service %s rootfs missing", serviceName)
		}
	}
	return nil
}

func newRuntimeRecoveryRecord(appInst *AppInstance, mode PiccoloMode, runtime container.PodmanRuntime) (*TransitionRecord, error) {
	originalRoot := filepath.Clean(runtime.Root)
	originalRunRoot := filepath.Clean(runtime.RunRoot)
	quarantineRoot := originalRoot + runtimeQuarantineSuffix
	quarantineRunRoot := originalRunRoot + runtimeQuarantineSuffix
	if err := validateRuntimeRecoveryPaths(originalRoot, quarantineRoot, originalRunRoot, quarantineRunRoot); err != nil {
		return nil, err
	}
	resourceKeys := map[string]string{
		"original_runtime_root":   originalRoot,
		"quarantine_runtime_root": quarantineRoot,
		"original_run_root":       originalRunRoot,
		"quarantine_run_root":     quarantineRunRoot,
	}
	plan, err := PlanInstalledAppTransition(TransitionPlanInput{
		OperationKind:   TransitionOperationRuntimeRecovery,
		SourceKind:      TransitionSourceAutomaticRecovery,
		Mode:            mode,
		Enabled:         appInst.Enabled,
		RuntimeChanging: true,
		Runtime: TransitionRuntimePolicy{
			RecreatePolicy: "metadata_quarantine",
		},
		ResourceKeys: resourceKeys,
	})
	if err != nil {
		return nil, err
	}
	return &TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationID:   fmt.Sprintf("runtime-recovery-%s-%d", appInst.InstanceID, time.Now().UnixNano()),
		InstanceID:    appInst.InstanceID,
		Phase:         TransitionPhaseRuntimeQuarantineIntent,
		Plan:          *plan,
		Resources: TransitionResources{
			OriginalRuntimeRoot:   originalRoot,
			QuarantineRuntimeRoot: quarantineRoot,
			OriginalRunRoot:       originalRunRoot,
			QuarantineRunRoot:     quarantineRunRoot,
		},
	}, nil
}

func (m *AppManager) recoverRuntimeRecoveryTransition(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, record *TransitionRecord) error {
	if record == nil || record.Plan.OperationKind != TransitionOperationRuntimeRecovery || record.Plan.SourceKind != TransitionSourceAutomaticRecovery {
		return fmt.Errorf("invalid runtime recovery transition")
	}
	r := record.Resources
	if record.Plan.ResourceKeys["original_runtime_root"] != r.OriginalRuntimeRoot ||
		record.Plan.ResourceKeys["quarantine_runtime_root"] != r.QuarantineRuntimeRoot ||
		record.Plan.ResourceKeys["original_run_root"] != r.OriginalRunRoot ||
		record.Plan.ResourceKeys["quarantine_run_root"] != r.QuarantineRunRoot {
		return fmt.Errorf("runtime recovery plan resources do not match transition resources")
	}
	if err := validateRuntimeRecoveryPaths(r.OriginalRuntimeRoot, r.QuarantineRuntimeRoot, r.OriginalRunRoot, r.QuarantineRunRoot); err != nil {
		return err
	}
	def, err := state.GetAppDefinition(appInst.InstanceID)
	if err != nil {
		return err
	}
	mode := piccoloModeFromExtensions(def.Extensions)
	if mode != ModeService && mode != ModeWorkspace {
		return fmt.Errorf("unsupported runtime recovery mode %s", mode)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, appInst.InstanceID)
	if err != nil {
		return fmt.Errorf("runtime recovery volume unavailable: %w", err)
	}

	for {
		switch record.Phase {
		case TransitionPhaseRuntimeQuarantineIntent:
			if err := m.quiesceAppUserSession(ctx, appInst.InstanceID); err != nil {
				return fmt.Errorf("prove dedicated app cgroup empty before quarantine: %w", err)
			}
			// Recovery can resume in a replacement process. Retain routes if
			// the independent PID 1 proof fails; withdraw only after success.
			if m.serviceManager != nil {
				m.serviceManager.DeactivateApp(appInst.InstanceID)
			}
			if err := quarantineRuntimePath(r.OriginalRuntimeRoot, r.QuarantineRuntimeRoot); err != nil {
				return err
			}
			if err := quarantineRuntimePath(r.OriginalRunRoot, r.QuarantineRunRoot); err != nil {
				return err
			}
			record.Phase = TransitionPhaseRuntimeQuarantined
			if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
				return err
			}
			if transitionRecoveryMustYield(ctx) {
				return nil
			}

		case TransitionPhaseRuntimeQuarantined:
			if _, err := m.podmanRuntimeForApp(ctx, appInst.InstanceID, layout, mode, appRuntimeEnsureReady); err != nil {
				return fmt.Errorf("create clean dedicated runtime: %w", err)
			}
			record.Phase = TransitionPhaseRuntimeCleanCreated
			if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
				return err
			}
			if transitionRecoveryMustYield(ctx) {
				return nil
			}

		case TransitionPhaseRuntimeCleanCreated:
			runtime, err := m.podmanRuntimeForApp(ctx, appInst.InstanceID, layout, mode, appRuntimeEnsureReady)
			if err != nil {
				return err
			}
			appInst.NetworkAnchorID = ""
			appInst.Containers = nil
			if err := state.StoreAppMetadata(appInst); err != nil {
				return fmt.Errorf("persist clean runtime metadata boundary: %w", err)
			}
			observed := m.observeContainerGroup(ctx, runtime, appInst, def)
			if !observed.known() {
				return fmt.Errorf("observe clean runtime: %w", observed.Err)
			}
			if err := m.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true, observed); err != nil {
				return err
			}
			record.Phase = TransitionPhaseRuntimeGroupCommitted
			if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
				return err
			}
			if transitionRecoveryMustYield(ctx) {
				return nil
			}

		case TransitionPhaseRuntimeGroupCommitted, TransitionPhaseCommittedCleanupPending:
			if err := removeRuntimeQuarantine(r.QuarantineRuntimeRoot, r.QuarantineRunRoot); err != nil {
				record.Phase = TransitionPhaseCommittedCleanupPending
				record.LastError = err.Error()
				_ = state.StoreTransitionRecord(appInst.InstanceID, record)
				return err
			}
			record.Phase = TransitionPhaseCommitted
			record.LastError = ""
			if err := state.StoreTransitionRecord(appInst.InstanceID, record); err != nil {
				return err
			}
			return state.ClearTransitionRecord(appInst.InstanceID)

		case TransitionPhaseCommitted:
			return state.ClearTransitionRecord(appInst.InstanceID)
		default:
			return fmt.Errorf("unsupported runtime recovery phase %s", record.Phase)
		}
	}
}

func validateRuntimeRecoveryPaths(originalRoot, quarantineRoot, originalRunRoot, quarantineRunRoot string) error {
	for _, entry := range []struct {
		base string
		pair [2]string
	}{
		{base: paths.PodmanRoot(), pair: [2]string{originalRoot, quarantineRoot}},
		{base: podmanRunRootBase(), pair: [2]string{originalRunRoot, quarantineRunRoot}},
	} {
		pair := entry.pair
		original := filepath.Clean(pair[0])
		quarantine := filepath.Clean(pair[1])
		if original == "." || original == string(filepath.Separator) || quarantine != original+runtimeQuarantineSuffix {
			return fmt.Errorf("invalid runtime recovery path pair")
		}
		base := filepath.Clean(entry.base)
		rel, err := filepath.Rel(base, original)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("runtime recovery path escapes podman root")
		}
		for _, candidate := range []string{original, quarantine} {
			if err := rejectSymlinksBelow(base, candidate); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectSymlinksBelow(base, candidate string) error {
	base = filepath.Clean(base)
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(base, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("runtime recovery path escapes its base")
	}
	current := base
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime recovery path contains a symlink: %s", current)
		}
	}
	return nil
}

func quarantineRuntimePath(original, quarantine string) error {
	originalInfo, originalErr := os.Lstat(original)
	_, quarantineErr := os.Lstat(quarantine)
	if originalErr == nil && originalInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to quarantine symlink %s", original)
	}
	if originalErr == nil && quarantineErr == nil {
		return fmt.Errorf("runtime quarantine already exists while original remains: %s", quarantine)
	}
	if errors.Is(originalErr, os.ErrNotExist) {
		if quarantineErr == nil || errors.Is(quarantineErr, os.ErrNotExist) {
			// Both absent is expected after a host reboot because the runtime is
			// tmpfs; the durable transition may safely continue with a clean root.
			return nil
		}
		return quarantineErr
	}
	if originalErr != nil {
		return originalErr
	}
	if err := os.Rename(original, quarantine); err != nil {
		return fmt.Errorf("quarantine runtime metadata: %w", err)
	}
	return nil
}

func removeRuntimeQuarantine(pathsToRemove ...string) error {
	for _, path := range pathsToRemove {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove runtime quarantine %s: %w", path, err)
		}
	}
	return nil
}
