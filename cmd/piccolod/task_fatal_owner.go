package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"piccolod/internal/autounlock"
	"piccolod/internal/pcv"
	"piccolod/internal/resources/pressure"
)

const (
	taskEmergencyCensusBudget = 150 * time.Millisecond
	taskEmergencyHandoffTTL   = 10 * time.Minute
)

type taskFatalRequestKind string

const (
	taskFatalRequestTaskCritical           taskFatalRequestKind = "task_critical"
	taskFatalRequestUnlockChain            taskFatalRequestKind = "unlock_chain"
	taskFatalRequestSerializedOwner        taskFatalRequestKind = "serialized_owner"
	taskFatalRequestProgressStateUncertain taskFatalRequestKind = "progress_state_uncertain"
)

const (
	taskFatalReasonUnlockChain     = "unlock_chain_liveness"
	taskFatalReasonOwnerLiveness   = "owner_liveness"
	taskFatalReasonProgressUnknown = "progress_state_uncertain"
)

// taskFatalRequest is deliberately bounded to non-secret attribution. It must
// never carry operation arguments, credentials, environment, or terminal data.
type taskFatalRequest struct {
	Kind     taskFatalRequestKind
	Owner    string
	Snapshot pressure.TaskSnapshot
}

func (r taskFatalRequest) valid() bool {
	switch r.Kind {
	case taskFatalRequestTaskCritical:
		return r.Snapshot.State == pressure.TaskPressureCritical && r.Snapshot.ReasonCode != ""
	case taskFatalRequestUnlockChain, taskFatalRequestProgressStateUncertain:
		return true
	case taskFatalRequestSerializedOwner:
		return r.Owner != ""
	default:
		return false
	}
}

func (r taskFatalRequest) exitCode() int {
	if r.Kind == taskFatalRequestProgressStateUncertain {
		return taskProgressUncertainExitCode
	}
	return taskEmergencyExitCode
}

func (r taskFatalRequest) prepareTrigger() autounlock.PrepareTrigger {
	if r.Kind == taskFatalRequestTaskCritical {
		return autounlock.PrepareTriggerTaskCritical
	}
	return autounlock.PrepareTriggerRecoveryFatal
}

func (r taskFatalRequest) markerSnapshot(now time.Time) pressure.TaskSnapshot {
	if r.Kind == taskFatalRequestTaskCritical {
		return r.Snapshot
	}
	reason := taskFatalReasonOwnerLiveness
	switch r.Kind {
	case taskFatalRequestUnlockChain:
		reason = taskFatalReasonUnlockChain
	case taskFatalRequestProgressStateUncertain:
		reason = taskFatalReasonProgressUnknown
	}
	return pressure.TaskSnapshot{ReasonCode: reason, SampledAt: now.UTC()}
}

func (r taskFatalRequest) markerOwner() string {
	switch r.Kind {
	case taskFatalRequestUnlockChain:
		return taskRecoveryUnlockChainOwner
	case taskFatalRequestSerializedOwner:
		return r.Owner
	default:
		// Task Critical is attributed from the guard census/current owner.
		// Progress uncertainty is always global: stale active progress is not
		// reliable enough to accuse its recorded owner.
		return ""
	}
}

// taskFatalRequestSignal is a one-shot, non-blocking producer boundary. The
// channel is capacity one and the atomic latch stays committed after the owner
// consumes it, so a later fatal source cannot replace the first attribution.
type taskFatalRequestSignal struct {
	committed atomic.Bool
	requests  chan taskFatalRequest
}

func newTaskFatalRequestSignal() *taskFatalRequestSignal {
	return &taskFatalRequestSignal{requests: make(chan taskFatalRequest, 1)}
}

func (s *taskFatalRequestSignal) Request(request taskFatalRequest) bool {
	if s == nil || !request.valid() || !s.committed.CompareAndSwap(false, true) {
		return false
	}
	s.requests <- request
	return true
}

// RequestCritical commits the exact sampler snapshot through the same
// producer-side first-wins latch as every generic process-fatal source.
func (s *taskFatalRequestSignal) RequestCritical(snapshot pressure.TaskSnapshot) bool {
	return s.Request(taskFatalRequest{Kind: taskFatalRequestTaskCritical, Snapshot: snapshot})
}

var processTaskFatalRequests = newTaskFatalRequestSignal()

func requestUnlockChainFatalRecovery() bool {
	return processTaskFatalRequests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain})
}

func requestSerializedOwnerFatalRecovery(owner string) bool {
	return processTaskFatalRequests.Request(taskFatalRequest{Kind: taskFatalRequestSerializedOwner, Owner: owner})
}

func requestProgressStateUncertainFatalRecovery() bool {
	return processTaskFatalRequests.Request(taskFatalRequest{Kind: taskFatalRequestProgressStateUncertain})
}

type taskRestartUnlockContinuityRegistration struct {
	continuity autounlock.RestartUnlockContinuity
}

// taskRestartUnlockContinuityHolder permits process construction to register
// continuity after the emergency owner is already armed. A Critical transition
// before registration observes nil and proceeds directly to marker/exit.
type taskRestartUnlockContinuityHolder struct {
	registration atomic.Pointer[taskRestartUnlockContinuityRegistration]
}

func (h *taskRestartUnlockContinuityHolder) Attach(continuity autounlock.RestartUnlockContinuity) {
	if h == nil {
		return
	}
	if continuity == nil {
		h.registration.Store(nil)
		return
	}
	h.registration.Store(&taskRestartUnlockContinuityRegistration{continuity: continuity})
}

func (h *taskRestartUnlockContinuityHolder) Load() autounlock.RestartUnlockContinuity {
	if h == nil {
		return nil
	}
	registration := h.registration.Load()
	if registration == nil {
		return nil
	}
	return registration.continuity
}

var processTaskRestartUnlockContinuity taskRestartUnlockContinuityHolder

func attachTaskRestartUnlockContinuity(continuity autounlock.RestartUnlockContinuity) {
	processTaskRestartUnlockContinuity.Attach(continuity)
}

type taskFatalTimer interface {
	Stop() bool
}

type taskFatalOwnerConfig struct {
	requests          *taskFatalRequestSignal
	continuity        *taskRestartUnlockContinuityHolder
	markerPath        string
	invocationID      string
	now               func() time.Time
	currentOwner      func() string
	afterFunc         func(time.Duration, func()) taskFatalTimer
	censusAfter       func(time.Duration) <-chan time.Time
	exit              func(int)
	fenceNewWork      func()
	fenceControlPlane func()
	handoffPresent    func() bool
	logf              func(string, ...any)
	prepareBudget     time.Duration
	censusBudget      time.Duration
	handoffTTL        time.Duration
}

type taskFatalOwner struct {
	config   taskFatalOwnerConfig
	exitOnce sync.Once
}

type taskFatalOwnerOutcome struct {
	Request             taskFatalRequest
	ContinuityAttempted bool
	ContinuityResult    autounlock.PrepareResult
	ContinuityErr       error
	Census              *pressure.TaskCensus
	Marker              taskRecoveryMarker
	MarkerErr           error
}

func newTaskFatalOwner(config taskFatalOwnerConfig) *taskFatalOwner {
	if config.requests == nil {
		config.requests = newTaskFatalRequestSignal()
	}
	if config.markerPath == "" {
		config.markerPath = taskRecoveryMarkerPath
	}
	if config.now == nil {
		config.now = time.Now
	}
	if config.currentOwner == nil {
		config.currentOwner = pressure.CurrentLifecycleOwner
	}
	if config.afterFunc == nil {
		config.afterFunc = func(delay time.Duration, callback func()) taskFatalTimer {
			return time.AfterFunc(delay, callback)
		}
	}
	if config.censusAfter == nil {
		config.censusAfter = time.After
	}
	if config.exit == nil {
		config.exit = os.Exit
	}
	if config.fenceNewWork == nil {
		config.fenceNewWork = pressure.DefaultAdmission.FenceCritical
	}
	if config.fenceControlPlane == nil {
		config.fenceControlPlane = pcv.FenceEmergencyControlPlaneFreeze
	}
	if config.handoffPresent == nil {
		config.handoffPresent = autounlock.BlobExists
	}
	if config.logf == nil {
		config.logf = log.Printf
	}
	if config.prepareBudget <= 0 || config.prepareBudget > taskContinuityPrepareBudget {
		config.prepareBudget = taskContinuityPrepareBudget
	}
	if config.censusBudget <= 0 || config.censusBudget > taskEmergencyFinalizationBudget {
		config.censusBudget = taskEmergencyCensusBudget
	}
	if config.handoffTTL <= 0 || config.handoffTTL > taskEmergencyHandoffTTL {
		config.handoffTTL = taskEmergencyHandoffTTL
	}
	return &taskFatalOwner{config: config}
}

// Run consumes exactly one process-fatal source. The hard exit timer is armed
// immediately after ownership is committed; all remaining work is best effort.
func (o *taskFatalOwner) Run(census <-chan pressure.TaskCensus) taskFatalOwnerOutcome {
	request, ok := <-o.config.requests.requests
	if !ok {
		return taskFatalOwnerOutcome{}
	}
	outcome := taskFatalOwnerOutcome{Request: request}
	exitCode := request.exitCode()
	emergencyStartedAt := o.config.now()
	hardExit := o.config.afterFunc(taskEmergencyDeadline, func() {
		o.commitExit(exitCode)
	})
	defer hardExit.Stop()

	// A task Critical source already fenced admission in the sampler. Generic
	// liveness sources arrive without that sampler transition, so the common
	// owner repeats the allocation-free hard fence for every source.
	o.config.fenceNewWork()
	o.config.fenceControlPlane()

	fallbackOwner := ""
	if request.Kind == taskFatalRequestTaskCritical {
		// Capture attribution before continuity or the bounded census walk can
		// allow another lifecycle owner to replace the immediate culprit.
		fallbackOwner = o.config.currentOwner()
	}

	o.prepareContinuity(&outcome)
	finalExitDelay := taskEmergencyFinalizationBudget
	if remaining := taskEmergencyDeadline - o.config.now().Sub(emergencyStartedAt); remaining < finalExitDelay {
		finalExitDelay = remaining
	}
	if finalExitDelay <= 0 {
		o.commitExit(exitCode)
	} else {
		finalExit := o.config.afterFunc(finalExitDelay, func() {
			o.commitExit(exitCode)
		})
		defer finalExit.Stop()
	}

	if request.Kind == taskFatalRequestTaskCritical {
		outcome.Census = o.captureCensus(census)
	}
	markerCensus := taskMarkerCensus(outcome.Census, fallbackOwner)
	if request.Kind != taskFatalRequestTaskCritical {
		markerCensus = taskMarkerCensus(nil, request.markerOwner())
	}
	attributedOwner := ""
	if markerCensus != nil {
		attributedOwner = markerCensus.LifecycleOwner
	}

	now := o.config.now()
	snapshot := request.markerSnapshot(now)
	previous, _, markerErr := loadTaskRecoveryMarker(o.config.markerPath)
	alreadyRecorded := markerErr == nil && o.config.invocationID != "" && previous.LastFailedInvocationID == o.config.invocationID
	if markerErr != nil {
		outcome.Marker = malformedTaskRecoveryMarker("marker_malformed", now)
		outcome.Marker.LastFailedInvocationID = o.config.invocationID
		if !snapshot.SampledAt.IsZero() {
			outcome.Marker.DetectionAt = snapshot.SampledAt.UTC()
		}
		if snapshot.CurrentKnown {
			current := snapshot.Current
			outcome.Marker.TaskCurrent = &current
		}
		if snapshot.LimitKnown {
			limit := snapshot.Limit
			outcome.Marker.TaskLimit = &limit
		}
	} else {
		outcome.Marker = buildTaskRecoveryMarkerForInvocationAt(snapshot, markerCensus, previous, o.config.invocationID, now)
	}
	if !alreadyRecorded {
		outcome.Marker.ContinuityOutcome = taskFatalContinuityOutcome(outcome, o.config.handoffPresent())
	}
	outcome.MarkerErr = writeTaskRecoveryMarker(o.config.markerPath, outcome.Marker)
	if outcome.MarkerErr != nil {
		o.config.logf("ERROR: process fatal marker write failed: %v", outcome.MarkerErr)
	}

	if outcome.Census != nil {
		if data, err := json.Marshal(outcome.Census); err == nil {
			o.config.logf("TASK_CENSUS: %s", data)
		}
	}
	o.logContinuity(outcome)
	o.config.logf("ERROR: process fatal recovery: source=%s reason=%s owner=%s current=%d limit=%d generation=%d invocation=%s exit_status=%d",
		request.Kind, snapshot.ReasonCode, attributedOwner, snapshot.Current, snapshot.Limit,
		outcome.Marker.Generation, outcome.Marker.LastFailedInvocationID, exitCode)
	o.commitExit(exitCode)
	return outcome
}

func taskFatalContinuityOutcome(outcome taskFatalOwnerOutcome, handoffPresent bool) string {
	if outcome.ContinuityAttempted {
		if outcome.ContinuityErr != nil {
			return string(autounlock.PrepareDispositionUnavailable)
		}
		switch outcome.ContinuityResult.Disposition {
		case autounlock.PrepareDispositionPrepared,
			autounlock.PrepareDispositionReused,
			autounlock.PrepareDispositionNotNeeded,
			autounlock.PrepareDispositionUnavailable:
			return string(outcome.ContinuityResult.Disposition)
		default:
			return "unknown"
		}
	}
	if handoffPresent {
		return "preexisting_handoff"
	}
	return "no_handoff"
}

func (o *taskFatalOwner) prepareContinuity(outcome *taskFatalOwnerOutcome) {
	continuity := o.config.continuity.Load()
	if continuity == nil {
		return
	}
	outcome.ContinuityAttempted = true
	ctx, cancel := context.WithTimeout(context.Background(), o.config.prepareBudget)
	defer cancel()
	type result struct {
		value autounlock.PrepareResult
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := continuity.Prepare(ctx, outcome.Request.prepareTrigger(), o.config.handoffTTL)
		done <- result{value: value, err: err}
	}()
	select {
	case prepared := <-done:
		outcome.ContinuityResult = prepared.value
		outcome.ContinuityErr = prepared.err
	case <-ctx.Done():
		outcome.ContinuityResult = autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionUnavailable}
		outcome.ContinuityErr = ctx.Err()
	}
}

func (o *taskFatalOwner) captureCensus(census <-chan pressure.TaskCensus) *pressure.TaskCensus {
	if census == nil {
		return nil
	}
	select {
	case captured, open := <-census:
		if !open {
			return nil
		}
		return &captured
	case <-o.config.censusAfter(o.config.censusBudget):
		return nil
	}
}

func (o *taskFatalOwner) logContinuity(outcome taskFatalOwnerOutcome) {
	if !outcome.ContinuityAttempted {
		return
	}
	if outcome.ContinuityErr != nil {
		o.config.logf("WARN: restart unlock continuity preparation failed: %v", outcome.ContinuityErr)
		return
	}
	switch outcome.ContinuityResult.Disposition {
	case autounlock.PrepareDispositionPrepared, autounlock.PrepareDispositionReused:
		o.config.logf("INFO: restart unlock continuity %s; expiry=%s",
			outcome.ContinuityResult.Disposition, outcome.ContinuityResult.ExpiresAt.UTC().Format(time.RFC3339Nano))
	case autounlock.PrepareDispositionNotNeeded, autounlock.PrepareDispositionUnavailable:
		o.config.logf("INFO: restart unlock continuity %s", outcome.ContinuityResult.Disposition)
	default:
		o.config.logf("WARN: restart unlock continuity returned unknown disposition %q", outcome.ContinuityResult.Disposition)
	}
}

func (o *taskFatalOwner) commitExit(code int) {
	o.exitOnce.Do(func() {
		o.config.exit(code)
	})
}

// runTaskEmergencyOwner retains the early-construction entry point used by
// main. Server/continuity wiring can attach capabilities after this goroutine
// is already waiting without weakening Critical-before-construction behavior.
func runTaskEmergencyOwner(census <-chan pressure.TaskCensus) {
	owner := newTaskFatalOwner(taskFatalOwnerConfig{
		requests:     processTaskFatalRequests,
		continuity:   &processTaskRestartUnlockContinuity,
		markerPath:   taskRecoveryMarkerPath,
		invocationID: os.Getenv("INVOCATION_ID"),
	})
	owner.Run(census)
}
