package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"piccolod/internal/autounlock"
	"piccolod/internal/resources/pressure"
)

const (
	taskRecoveryOwnerCancellationGrace = 5 * time.Second
	taskRecoveryRunnerPollInterval     = pressure.TaskGuardPollInterval
	taskRecoveryOwnerRefreshInterval   = 30 * time.Second
	taskRecoveryEnumerationTimeout     = 30 * time.Second
	taskRecoveryEnumerationOwner       = "desired-owner-enumeration"
)

// taskRecoveryOwner is the narrow capability main needs from a server-owned
// lifecycle component. Durable state and the operation body stay with their
// existing package; marker authority and liveness arbitration stay in main.
type taskRecoveryOwner struct {
	Name               string
	AppID              string
	Timeout            time.Duration
	Attempt            func(context.Context) (active bool, err error)
	AttemptDetailed    func(context.Context) taskRecoveryOwnerAttemptResult
	ObserveActive      func(context.Context) (active bool, err error)
	RouteQualification *taskRecoveryQualification
}

type taskRecoveryQualification struct {
	Timeout time.Duration
	Attempt func(context.Context) (active bool, err error)
}

type taskRecoveryRuntime interface {
	TaskPressureSnapshot() pressure.TaskSnapshot
	LifecycleReady() bool
	CoreTaskRecoveryOwners() []taskRecoveryOwner
	DecryptedTaskRecoveryOwners(context.Context) ([]taskRecoveryOwner, error)
	PrepareTaskRecoveryApps([]string)
	ReleaseTaskRecoveryApp(string)
	SetTaskRecoveryGlobalSuppression(bool)
}

type taskRecoveryRunnerConfig struct {
	controller         *taskRecoveryController
	runtime            taskRecoveryRuntime
	initialDesired     []string
	processStartedAt   time.Time
	pollInterval       time.Duration
	refreshInterval    time.Duration
	enumerationTimeout time.Duration
	cancelGrace        time.Duration
	requestFatal       func(string) bool
	requestUncertain   func() bool
	now                func() time.Time
	logf               func(string, ...any)
}

type taskRecoveryRunner struct {
	config taskRecoveryRunnerConfig
}

func newTaskRecoveryRunner(config taskRecoveryRunnerConfig) *taskRecoveryRunner {
	if config.pollInterval <= 0 {
		config.pollInterval = taskRecoveryRunnerPollInterval
	}
	if config.refreshInterval <= 0 {
		config.refreshInterval = taskRecoveryOwnerRefreshInterval
	}
	if config.enumerationTimeout <= 0 {
		config.enumerationTimeout = taskRecoveryEnumerationTimeout
	}
	if config.cancelGrace <= 0 {
		config.cancelGrace = taskRecoveryOwnerCancellationGrace
	}
	if config.requestFatal == nil {
		config.requestFatal = requestSerializedOwnerFatalRecovery
	}
	if config.requestUncertain == nil {
		config.requestUncertain = requestProgressStateUncertainFatalRecovery
	}
	if config.now == nil {
		config.now = time.Now
	}
	if config.logf == nil {
		config.logf = log.Printf
	}
	return &taskRecoveryRunner{config: config}
}

// Run owns marked recovery until the controller proves every desired owner
// received a fresh pass and every retained strike met its stability window.
// It never starts more than one automatic owner at a time.
func (r *taskRecoveryRunner) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.config.controller == nil || r.config.runtime == nil {
		return
	}
	telemetry := newTaskRecoveryTelemetry(r.config.controller, r.config.processStartedAt, r.config.now, r.config.logf)
	telemetry.emitCoreReady()

	actions := make(map[string]taskRecoveryOwner)
	coreOwners := r.config.runtime.CoreTaskRecoveryOwners()
	for _, owner := range coreOwners {
		if normalized, ok := normalizeTaskRecoveryOwner(owner); ok {
			actions[normalized.Name] = normalized
		}
	}
	var enumeratedOwners []taskRecoveryOwner
	enumerationOwner := taskRecoveryOwner{
		Name: taskRecoveryEnumerationOwner, Timeout: r.config.enumerationTimeout,
		Attempt: func(attemptCtx context.Context) (bool, error) {
			owners, err := r.config.runtime.DecryptedTaskRecoveryOwners(attemptCtx)
			if err == nil {
				enumeratedOwners = owners
			}
			return err == nil, err
		},
	}
	actions[enumerationOwner.Name] = enumerationOwner
	initialDesired := append([]string{taskRecoveryEnumerationOwner}, r.config.initialDesired...)
	for _, owner := range coreOwners {
		initialDesired = append(initialDesired, owner.Name)
	}
	if err := r.config.controller.SetDesiredOwners(initialDesired); err != nil {
		r.config.logf("WARN: task recovery initialize desired owners: %v", err)
	}

	// preparedApps remembers the current durable desired incarnation so a
	// successfully released app is not suppressed again on every enumeration
	// refresh. pendingApps is the narrower set whose background reconciliation
	// is still fenced until its explicit owner succeeds or durable desire
	// removes it.
	preparedApps := make(map[string]struct{})
	pendingApps := make(map[string]struct{})
	decryptedLoaded := false
	enumerationRequired := true
	refreshAt := time.Time{}
	qualificationInitialized := false
	var pendingQualification taskRecoveryOwner
	qualificationPending := false
	lifecycleReadyObserved := false
	unlockPickupObserved := false
	routeRecoveryObserved := make(map[string]struct{})

	for {
		if r.config.controller.Complete() {
			telemetry.emitStage("eventual_convergence_complete", "complete", "")
			r.config.runtime.SetTaskRecoveryGlobalSuppression(false)
			r.config.logf("INFO: task recovery completed; volatile marker removed")
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		snapshot := r.config.runtime.TaskPressureSnapshot()
		normal := snapshot.AllowsAutomaticRecovery()
		ready := r.config.runtime.LifecycleReady()
		if ready && !unlockPickupObserved {
			if _, hasUnlockOwner := actions[taskRecoveryUnlockChainOwner]; hasUnlockOwner {
				unlockPickupObserved = true
				telemetry.emitStage("unlock_pickup_skipped", "lifecycle_already_ready", "")
			}
		}
		if ready && !lifecycleReadyObserved {
			lifecycleReadyObserved = true
			telemetry.emitStage("lifecycle_ready", "", "")
		}
		skipActivity := make(map[string]struct{})

		if ready && !enumerationRequired && time.Now().After(refreshAt) &&
			!r.config.controller.HasEligibleOwnerExcept(taskRecoveryEnumerationOwner) {
			r.config.controller.RequestOwnerRefresh(taskRecoveryEnumerationOwner)
			enumerationRequired = true
		}
		if enumerationRequired {
			for name, owner := range actions {
				if owner.AppID == "" {
					continue
				}
				skipActivity[name] = struct{}{}
				if err := r.config.controller.SetOwnerActive(name, false); err != nil {
					r.config.logf("WARN: task recovery invalidate owner activity: owner=%s err=%v", name, err)
				}
			}
		}

		r.observeOwnerActivity(ctx, actions, skipActivity)
		if err := r.config.controller.ObserveState(normal, ready); err != nil {
			r.config.logf("WARN: task recovery observe stability: %v", err)
		}

		var qualification *taskRecoveryOwner
		if qualificationPending {
			qualification = &pendingQualification
		}
		scheduleActions := actions
		if enumerationRequired {
			if ready {
				scheduleActions = map[string]taskRecoveryOwner{taskRecoveryEnumerationOwner: enumerationOwner}
			} else if unlockOwner, ok := actions[taskRecoveryUnlockChainOwner]; ok {
				// Desired-owner enumeration requires decrypted lifecycle state.
				// On a locked recovery boot, unattended unlock must therefore run
				// before enumeration; all other core owners remain Ready-gated.
				scheduleActions = map[string]taskRecoveryOwner{taskRecoveryUnlockChainOwner: unlockOwner}
			} else {
				scheduleActions = nil
			}
			qualification = nil
		}
		schedule := r.config.controller.Schedule()
		r.config.runtime.SetTaskRecoveryGlobalSuppression(
			taskRecoveryScheduleHasGlobalSuppression(schedule, enumerationRequired),
		)
		owner, found := nextEligibleTaskRecoveryOwner(schedule, scheduleActions, qualification)
		if found {
			qualificationAttempt := qualificationPending && owner.Name == pendingQualification.Name
			if owner.Name == taskRecoveryUnlockChainOwner && !unlockPickupObserved {
				unlockPickupObserved = true
				if ready {
					telemetry.emitStage("unlock_pickup_skipped", "lifecycle_already_ready", "")
				} else {
					telemetry.emitStage("unlock_pickup_started", "attempt_started", "")
				}
			}
			var nextActions map[string]taskRecoveryOwner
			var newApps []string
			var desiredApps map[string]struct{}
			decorate := func(*taskRecoveryOwnerAttemptResult) {}
			if owner.Name == taskRecoveryEnumerationOwner {
				decorate = func(result *taskRecoveryOwnerAttemptResult) {
					if result.FatalCommitted || result.Err != nil {
						return
					}
					nextActions = make(map[string]taskRecoveryOwner, len(coreOwners)+len(enumeratedOwners)+1)
					nextActions[taskRecoveryEnumerationOwner] = enumerationOwner
					desiredApps = make(map[string]struct{}, len(enumeratedOwners))
					desired := []string{taskRecoveryEnumerationOwner}
					for _, candidate := range enumeratedOwners {
						normalized, ok := normalizeTaskRecoveryOwner(candidate)
						if !ok {
							continue
						}
						if !qualificationInitialized && !qualificationPending {
							if qualificationOwner, ok := taskRecoveryQualificationOwner(normalized); ok {
								pendingQualification = qualificationOwner
								qualificationPending = true
							}
						}
						nextActions[normalized.Name] = normalized
						desired = append(desired, normalized.Name)
						if normalized.AppID != "" {
							desiredApps[normalized.AppID] = struct{}{}
							if _, exists := preparedApps[normalized.AppID]; !exists {
								newApps = append(newApps, normalized.AppID)
							}
						}
					}
					if !qualificationInitialized {
						qualificationInitialized = true
						if !qualificationPending {
							telemetry.emitQualification("listenerless_no_cohort", "")
						}
					}
					if qualificationPending && !containsTaskRecoveryOwner(desired, pendingQualification.Name) {
						desired = append(desired, pendingQualification.Name)
					}
					for _, candidate := range coreOwners {
						normalized, ok := normalizeTaskRecoveryOwner(candidate)
						if !ok || normalized.Name == taskRecoveryUnlockChainOwner {
							continue
						}
						if _, duplicate := nextActions[normalized.Name]; duplicate {
							continue
						}
						nextActions[normalized.Name] = normalized
						desired = append(desired, normalized.Name)
					}
					result.EnumerationDesired = desired
					result.EnumerationDesiredKnown = true
				}
			}
			outcome := r.runOwner(ctx, owner, qualificationAttempt, decorate)
			switch outcome.Kind {
			case taskRecoveryAttemptReturned:
				if owner.Name == taskRecoveryUnlockChainOwner {
					pickupOutcome := "inactive"
					if outcome.Result.Err != nil {
						pickupOutcome = "degraded"
					} else if outcome.Result.Active {
						pickupOutcome = "active"
					}
					telemetry.emitStage("unlock_pickup_complete", pickupOutcome, "")
				}
				if owner.Name == taskRecoveryEnumerationOwner {
					if outcome.Result.Err != nil {
						r.config.logf("WARN: task recovery enumerate desired owners: %v", outcome.Result.Err)
						continue
					}
					sort.Strings(newApps)
					if len(newApps) > 0 || !decryptedLoaded {
						r.config.runtime.PrepareTaskRecoveryApps(newApps)
					}
					for _, appID := range newApps {
						preparedApps[appID] = struct{}{}
						pendingApps[appID] = struct{}{}
					}
					removedApps := make([]string, 0)
					for appID := range preparedApps {
						if _, stillDesired := desiredApps[appID]; stillDesired {
							continue
						}
						delete(preparedApps, appID)
						if _, pending := pendingApps[appID]; pending {
							delete(pendingApps, appID)
							removedApps = append(removedApps, appID)
						}
					}
					sort.Strings(removedApps)
					for _, appID := range removedApps {
						r.config.runtime.ReleaseTaskRecoveryApp(appID)
					}
					actions = nextActions
					decryptedLoaded = true
					enumerationRequired = false
					refreshAt = time.Now().Add(r.config.refreshInterval)
					continue
				}
				if qualificationAttempt {
					qualificationPending = false
					outcomeName := "mixed_health_selector_changed"
					if outcome.Result.Active && outcome.Result.Err == nil {
						outcomeName = "eligible_pass"
					}
					telemetry.emitQualification(outcomeName, owner.AppID)
				}
				if owner.AppID != "" && outcome.Result.RouteKnown && outcome.Result.RouteActive &&
					outcome.Result.Active && outcome.Result.Err == nil {
					if _, emitted := routeRecoveryObserved[owner.AppID]; !emitted {
						routeRecoveryObserved[owner.AppID] = struct{}{}
						telemetry.emitStage("route_recovery_complete", "active", owner.AppID)
					}
				}
				if owner.AppID != "" && outcome.Result.Active && outcome.Result.Err == nil {
					if _, pending := pendingApps[owner.AppID]; pending {
						r.config.runtime.ReleaseTaskRecoveryApp(owner.AppID)
						delete(pendingApps, owner.AppID)
					}
				}
				if outcome.Result.Err != nil {
					r.config.logf("WARN: task recovery owner returned degraded: owner=%s err=%v", owner.Name, outcome.Result.Err)
				}
				if outcome.MaintenanceErr != nil {
					r.config.logf("WARN: task recovery owner maintenance: owner=%s err=%v", owner.Name, outcome.MaintenanceErr)
				}
			case taskRecoveryAttemptProgressStateUncertain:
				if qualificationAttempt {
					qualificationPending = false
					telemetry.emitQualification("mixed_health_selector_changed", owner.AppID)
				}
				r.config.logf("ERROR: task recovery progress state uncertain: owner=%s err=%v", owner.Name, outcome.Err)
				r.config.requestUncertain()
				return
			case taskRecoveryAttemptFatalCommitted:
				if qualificationAttempt {
					qualificationPending = false
					telemetry.emitQualification("mixed_health_selector_changed", owner.AppID)
				}
				r.config.logf("ERROR: task recovery owner liveness fatal committed: owner=%s", owner.Name)
				return
			case taskRecoveryAttemptMarkerWriteDeferred:
				r.config.logf("WARN: task recovery progress write deferred: owner=%s retry_at=%s err=%v",
					owner.Name, outcome.RetryAt.UTC().Format(time.RFC3339Nano), outcome.Err)
			case taskRecoveryAttemptNotEligible:
				// State changed between Schedule and the serialized progress
				// commit. The next poll recomputes from current truth.
			default:
				r.config.logf("WARN: task recovery owner returned unknown outcome: owner=%s kind=%s", owner.Name, outcome.Kind)
			}
			continue
		}

		if !r.waitForPoll(ctx) {
			return
		}
	}
}

type taskRecoveryTelemetry struct {
	detectionAt       time.Time
	markerAt          time.Time
	processStartedAt  time.Time
	generation        int
	failedInvocation  string
	reason            string
	cohort            string
	continuityOutcome string
	taskCurrent       *int64
	taskLimit         *int64
	now               func() time.Time
	logf              func(string, ...any)
}

func newTaskRecoveryTelemetry(
	controller *taskRecoveryController,
	processStartedAt time.Time,
	now func() time.Time,
	logf func(string, ...any),
) taskRecoveryTelemetry {
	controller.mu.Lock()
	marker := cloneTaskRecoveryMarker(controller.marker)
	controller.mu.Unlock()
	detectionAt := marker.DetectionAt.UTC()
	if detectionAt.IsZero() {
		// Schema-v2 markers written before detection_at existed used Timestamp
		// for both values. Keep those volatile markers readable across upgrade.
		detectionAt = marker.Timestamp.UTC()
	}
	continuityOutcome := marker.ContinuityOutcome
	if continuityOutcome == "" {
		continuityOutcome = "unknown"
	}
	return taskRecoveryTelemetry{
		detectionAt:       detectionAt,
		markerAt:          marker.Timestamp.UTC(),
		processStartedAt:  processStartedAt.UTC(),
		generation:        marker.Generation,
		failedInvocation:  marker.LastFailedInvocationID,
		reason:            marker.ReasonCode,
		cohort:            taskRecoveryTelemetryCohort(marker),
		continuityOutcome: continuityOutcome,
		taskCurrent:       marker.TaskCurrent,
		taskLimit:         marker.TaskLimit,
		now:               now,
		logf:              logf,
	}
}

func taskRecoveryTelemetryCohort(marker taskRecoveryMarker) string {
	if marker.Generation > 1 || marker.GlobalStrike > 1 {
		return "recurrence_containment"
	}
	for _, suspect := range marker.Suspects {
		if suspect.Strike > 1 {
			return "recurrence_containment"
		}
	}
	if marker.ReasonCode == "service_failure" || marker.ReasonCode == "marker_malformed" {
		switch marker.ContinuityOutcome {
		case "no_handoff":
			return "unexpected_no_handoff"
		case "preexisting_handoff", string(autounlock.PrepareDispositionPrepared), string(autounlock.PrepareDispositionReused):
			return "unexpected_with_handoff"
		default:
			return "unexpected_handoff_unknown"
		}
	}
	return "task_first_generation"
}

func (t taskRecoveryTelemetry) emitCoreReady() {
	t.emitStage("core_ready", "", "")
}

func (t taskRecoveryTelemetry) emitQualification(outcome, candidate string) {
	t.emitStage("first_route_qualification_complete", outcome, candidate)
}

func (t taskRecoveryTelemetry) emitStage(stage, outcome, candidate string) {
	if t.logf == nil || t.now == nil {
		return
	}
	t.logf("TASK_RECOVERY_STAGE: stage=%s observed_at=%s detection_at=%s marker_at=%s process_started_at=%s generation=%d failed_invocation=%s reason=%s cohort=%s continuity_outcome=%s task_current=%s task_limit=%s outcome=%s candidate=%s",
		stage,
		t.now().UTC().Format(time.RFC3339Nano),
		t.detectionAt.Format(time.RFC3339Nano),
		t.markerAt.Format(time.RFC3339Nano),
		t.processStartedAt.Format(time.RFC3339Nano),
		t.generation,
		t.failedInvocation,
		t.reason,
		t.cohort,
		t.continuityOutcome,
		taskRecoveryTelemetryMetric(t.taskCurrent),
		taskRecoveryTelemetryMetric(t.taskLimit),
		outcome,
		candidate,
	)
}

func taskRecoveryTelemetryMetric(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprint(*value)
}

func taskRecoveryScheduleHasGlobalSuppression(schedule taskRecoverySchedule, enumerationRequired bool) bool {
	if schedule.GlobalRemaining > 0 {
		return true
	}
	if !enumerationRequired {
		return false
	}
	// Before durable enumeration establishes per-app suppression, any delayed
	// retained owner is necessarily device-wide UI truth. Keep the global
	// recovery signal until enumeration transfers that state to exact apps.
	for _, decision := range schedule.Decisions {
		if decision.Remaining > 0 {
			return true
		}
	}
	return false
}

func (r *taskRecoveryRunner) waitForPoll(ctx context.Context) bool {
	timer := time.NewTimer(r.config.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *taskRecoveryRunner) observeOwnerActivity(
	ctx context.Context,
	actions map[string]taskRecoveryOwner,
	skip map[string]struct{},
) {
	names := make([]string, 0, len(actions))
	for name, owner := range actions {
		if owner.ObserveActive == nil {
			if owner.AppID != "" {
				if err := r.config.controller.SetOwnerActive(name, false); err != nil {
					r.config.logf("WARN: task recovery invalidate missing app activity observer: owner=%s err=%v", name, err)
				}
			}
			continue
		}
		if _, excluded := skip[name]; excluded {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		active, err := actions[name].ObserveActive(ctx)
		if err != nil {
			active = false
			r.config.logf("WARN: task recovery owner activity unknown: owner=%s err=%v", name, err)
		}
		if setErr := r.config.controller.SetOwnerActive(name, active); setErr != nil {
			r.config.logf("WARN: task recovery refresh owner activity: owner=%s err=%v", name, setErr)
		}
	}
}

func normalizeTaskRecoveryOwner(owner taskRecoveryOwner) (taskRecoveryOwner, bool) {
	owner.Name = strings.TrimSpace(owner.Name)
	owner.AppID = strings.TrimSpace(owner.AppID)
	if owner.Name == "" || owner.Timeout <= 0 || (owner.Attempt == nil && owner.AttemptDetailed == nil) {
		return taskRecoveryOwner{}, false
	}
	if owner.RouteQualification != nil &&
		(owner.RouteQualification.Timeout <= 0 || owner.RouteQualification.Attempt == nil) {
		owner.RouteQualification = nil
	}
	return owner, true
}

func taskRecoveryQualificationOwner(owner taskRecoveryOwner) (taskRecoveryOwner, bool) {
	if owner.RouteQualification == nil || owner.RouteQualification.Timeout <= 0 || owner.RouteQualification.Attempt == nil {
		return taskRecoveryOwner{}, false
	}
	owner.Timeout = owner.RouteQualification.Timeout
	owner.Attempt = owner.RouteQualification.Attempt
	qualificationAttempt := owner.RouteQualification.Attempt
	owner.AttemptDetailed = func(ctx context.Context) taskRecoveryOwnerAttemptResult {
		active, err := qualificationAttempt(ctx)
		return taskRecoveryOwnerAttemptResult{
			Active: active, RouteKnown: true, RouteActive: active && err == nil, Err: err,
		}
	}
	owner.RouteQualification = nil
	return owner, true
}

func containsTaskRecoveryOwner(owners []string, owner string) bool {
	for _, candidate := range owners {
		if candidate == owner {
			return true
		}
	}
	return false
}

func nextEligibleTaskRecoveryOwner(
	schedule taskRecoverySchedule,
	actions map[string]taskRecoveryOwner,
	qualification *taskRecoveryOwner,
) (taskRecoveryOwner, bool) {
	for _, decision := range schedule.Decisions {
		if !decision.Eligible {
			continue
		}
		if qualification != nil && decision.Owner == qualification.Name {
			return *qualification, true
		}
		owner, ok := actions[decision.Owner]
		if ok {
			return owner, true
		}
	}
	return taskRecoveryOwner{}, false
}

func (r *taskRecoveryRunner) runOwner(
	ctx context.Context,
	owner taskRecoveryOwner,
	qualificationOnly bool,
	decorate func(*taskRecoveryOwnerAttemptResult),
) taskRecoveryAttemptOutcome {
	attempt := func() taskRecoveryOwnerAttemptResult {
		result := executeBoundedTaskRecoveryOwner(ctx, owner, r.config.cancelGrace, r.config.requestFatal)
		if decorate != nil {
			decorate(&result)
		}
		return result
	}
	if qualificationOnly {
		return r.config.controller.RunQualificationAttempt(owner.Name, attempt)
	}
	return r.config.controller.RunAttempt(owner.Name, attempt)
}

const (
	taskRecoveryOperationRunning uint32 = iota
	taskRecoveryOperationReturned
	taskRecoveryOperationFatalCommitted
)

// executeBoundedTaskRecoveryOwner gives cancellation-ignoring lifecycle code
// one finite operation bound plus a five-second grace. Return and grace expiry
// contend on one atomic decision; a late return after fatal cannot clear the
// marker or start a successor.
func executeBoundedTaskRecoveryOwner(
	processCtx context.Context,
	owner taskRecoveryOwner,
	cancelGrace time.Duration,
	requestFatal func(string) bool,
) taskRecoveryOwnerAttemptResult {
	if processCtx == nil {
		processCtx = context.Background()
	}
	if owner.Timeout <= 0 || (owner.Attempt == nil && owner.AttemptDetailed == nil) || strings.TrimSpace(owner.Name) == "" {
		return taskRecoveryOwnerAttemptResult{Err: errors.New("task recovery owner requires a name, finite timeout, and attempt")}
	}
	if cancelGrace <= 0 {
		cancelGrace = taskRecoveryOwnerCancellationGrace
	}
	if requestFatal == nil {
		requestFatal = requestSerializedOwnerFatalRecovery
	}

	opCtx, cancel := context.WithTimeout(processCtx, owner.Timeout)
	defer cancel()
	var decision atomic.Uint32
	returned := make(chan taskRecoveryOwnerAttemptResult, 1)
	go func() {
		result := taskRecoveryOwnerAttemptResult{}
		if owner.AttemptDetailed != nil {
			result = owner.AttemptDetailed(opCtx)
		} else {
			result.Active, result.Err = owner.Attempt(opCtx)
		}
		if decision.CompareAndSwap(taskRecoveryOperationRunning, taskRecoveryOperationReturned) {
			returned <- result
		}
	}()

	select {
	case result := <-returned:
		return result
	case <-opCtx.Done():
	}
	if processCtx.Err() != nil {
		// Graceful process shutdown owns termination. Keep RunAttempt blocked
		// (and therefore keep active progress intact if the owner ignores
		// cancellation) instead of converting an operator shutdown into a
		// recurrence-attributed fatal restart.
		return <-returned
	}

	grace := time.NewTimer(cancelGrace)
	defer grace.Stop()
	select {
	case result := <-returned:
		return result
	case <-grace.C:
		if decision.CompareAndSwap(taskRecoveryOperationRunning, taskRecoveryOperationFatalCommitted) {
			requestFatal(owner.Name)
			return taskRecoveryOwnerAttemptResult{
				Err:            fmt.Errorf("task recovery owner %q exceeded %s plus %s cancellation grace", owner.Name, owner.Timeout, cancelGrace),
				FatalCommitted: true,
			}
		}
		return <-returned
	}
}
