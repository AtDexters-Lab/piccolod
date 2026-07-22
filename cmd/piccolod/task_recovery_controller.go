package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	taskRecoveryMarkerWriteRetryInterval = 30 * time.Second
	taskRecoveryProgressClearBudget      = time.Second
	taskRecoveryProgressClearRetry       = 100 * time.Millisecond
	// A conclusive ordinary failure is degraded truth, not a recurrence
	// strike. Retry it on the normal observation cadence without letting it
	// monopolize the current desired-owner pass.
	taskRecoveryReturnedOwnerRetryDelay = 30 * time.Second
	taskRecoveryUnlockChainOwner        = "unlock-chain"
)

type taskRecoveryControllerClock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type realTaskRecoveryControllerClock struct{}

func (realTaskRecoveryControllerClock) Now() time.Time            { return time.Now() }
func (realTaskRecoveryControllerClock) Sleep(delay time.Duration) { time.Sleep(delay) }

type taskRecoveryControllerMarkerIO interface {
	Write(taskRecoveryMarker) error
	Remove() error
}

type taskRecoveryFileMarkerIO struct{ path string }

func (f taskRecoveryFileMarkerIO) Write(marker taskRecoveryMarker) error {
	return writeTaskRecoveryMarker(f.path, marker)
}

func (f taskRecoveryFileMarkerIO) Remove() error {
	err := os.Remove(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type taskRecoveryAttemptOutcomeKind string

const (
	taskRecoveryAttemptReturned               taskRecoveryAttemptOutcomeKind = "returned"
	taskRecoveryAttemptFatalCommitted         taskRecoveryAttemptOutcomeKind = "fatal_committed"
	taskRecoveryAttemptNotEligible            taskRecoveryAttemptOutcomeKind = "not_eligible"
	taskRecoveryAttemptMarkerWriteDeferred    taskRecoveryAttemptOutcomeKind = "marker_write_deferred"
	taskRecoveryAttemptProgressStateUncertain taskRecoveryAttemptOutcomeKind = "progress_state_uncertain"
)

type taskRecoveryOwnerAttemptResult struct {
	Active                  bool
	RouteKnown              bool
	RouteActive             bool
	Err                     error
	FatalCommitted          bool
	EnumerationDesired      []string
	EnumerationDesiredKnown bool
}

type taskRecoveryAttemptOutcome struct {
	Kind           taskRecoveryAttemptOutcomeKind
	Owner          string
	Result         taskRecoveryOwnerAttemptResult
	Err            error
	MaintenanceErr error
	RetryAt        time.Time
}

type taskRecoveryProgressStateUncertainError struct {
	Owner        string
	InvocationID string
	Cause        error
}

func (e *taskRecoveryProgressStateUncertainError) Error() string {
	return fmt.Sprintf("task recovery progress state uncertain for %q invocation %q: %v", e.Owner, e.InvocationID, e.Cause)
}

func (e *taskRecoveryProgressStateUncertainError) Unwrap() error { return e.Cause }

type taskRecoveryScheduleCohort string

const (
	taskRecoveryNonSuspectCohort  taskRecoveryScheduleCohort = "non_suspect"
	taskRecoverySuspectCohort     taskRecoveryScheduleCohort = "suspect"
	taskRecoveryUnlockCohort      taskRecoveryScheduleCohort = "unlock_chain"
	taskRecoveryEnumerationCohort taskRecoveryScheduleCohort = "desired_owner_enumeration"
)

type taskRecoveryScheduleDecision struct {
	Owner                string
	Cohort               taskRecoveryScheduleCohort
	Strike               int
	Eligible             bool
	AlreadyAttempted     bool
	ReturnedRetryPending bool
	BlockedByNonSuspects bool
	Delay                time.Duration
	Remaining            time.Duration
}

type taskRecoverySchedule struct {
	Decisions       []taskRecoveryScheduleDecision
	GlobalDelay     time.Duration
	GlobalRemaining time.Duration
}

// taskRecoveryController is the single cmd-owned marker authority. Callers may
// report desired owners and health, but only this controller writes or removes
// the volatile marker.
type taskRecoveryController struct {
	attemptMu sync.Mutex
	mu        sync.Mutex

	marker       taskRecoveryMarker
	invocationID string
	markerIO     taskRecoveryControllerMarkerIO
	clock        taskRecoveryControllerClock

	markerRemoved       bool
	progressUncertain   error
	nextProgressWriteAt time.Time

	desiredOrder       []string
	desired            map[string]struct{}
	attempted          map[string]struct{}
	returnedRetrySince map[string]time.Time
	refreshPending     map[string]struct{}
	freshPass          bool
	desiredInitialized bool

	normal            bool
	ready             bool
	normalSince       time.Time
	normalReadySince  time.Time
	globalStableSince time.Time
	activeOwners      map[string]struct{}
	ownerStableSince  map[string]time.Time
}

func newTaskRecoveryController(path string, marker taskRecoveryMarker, invocationID string) *taskRecoveryController {
	return newTaskRecoveryControllerWithDeps(
		marker,
		invocationID,
		taskRecoveryFileMarkerIO{path: path},
		realTaskRecoveryControllerClock{},
	)
}

func newTaskRecoveryControllerWithDeps(
	marker taskRecoveryMarker,
	invocationID string,
	markerIO taskRecoveryControllerMarkerIO,
	clock taskRecoveryControllerClock,
) *taskRecoveryController {
	if clock == nil {
		clock = realTaskRecoveryControllerClock{}
	}
	return &taskRecoveryController{
		marker:             cloneTaskRecoveryMarker(marker),
		invocationID:       invocationID,
		markerIO:           markerIO,
		clock:              clock,
		desired:            make(map[string]struct{}),
		attempted:          make(map[string]struct{}),
		returnedRetrySince: make(map[string]time.Time),
		refreshPending:     make(map[string]struct{}),
		activeOwners:       make(map[string]struct{}),
		ownerStableSince:   make(map[string]time.Time),
	}
}

func cloneTaskRecoveryMarker(marker taskRecoveryMarker) taskRecoveryMarker {
	marker.Suspects = append([]taskRecoverySuspect(nil), marker.Suspects...)
	return marker
}

// SetDesiredOwners starts a fresh desired-owner pass when durable desire
// changes. Suspects for disabled or removed owners are removed from the marker;
// durable desire itself remains external authority.
func (c *taskRecoveryController) SetDesiredOwners(owners []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setDesiredOwnersLocked(owners)
}

func (c *taskRecoveryController) setDesiredOwnersLocked(owners []string) error {
	order, desired := normalizeTaskRecoveryOwners(owners)
	changed := !c.desiredInitialized || !sameTaskRecoveryOwnerSet(c.desired, desired)
	c.desiredOrder = order
	c.desired = desired
	c.desiredInitialized = true
	if changed {
		c.attempted = make(map[string]struct{})
		c.freshPass = len(desired) == 0
		c.globalStableSince = time.Time{}
	}

	next := cloneTaskRecoveryMarker(c.marker)
	markerChanged := false
	for i := len(next.Suspects) - 1; i >= 0; i-- {
		owner := next.Suspects[i].Owner
		if _, ok := desired[owner]; ok {
			continue
		}
		next.Suspects = append(next.Suspects[:i], next.Suspects[i+1:]...)
		delete(c.activeOwners, owner)
		delete(c.ownerStableSince, owner)
		delete(c.returnedRetrySince, owner)
		markerChanged = true
	}
	for owner := range c.returnedRetrySince {
		if _, ok := desired[owner]; !ok {
			delete(c.returnedRetrySince, owner)
		}
	}
	for owner := range c.refreshPending {
		if _, ok := desired[owner]; !ok {
			delete(c.refreshPending, owner)
		}
	}
	return c.commitMaintenanceLocked(next, markerChanged)
}

// RequestOwnerRefresh schedules a fresh bounded pass without bypassing the
// owner's existing suspect, global, or returned-failure backoff.
func (c *taskRecoveryController) RequestOwnerRefresh(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.markerRemoved {
		return
	}
	if _, desired := c.desired[owner]; desired {
		c.refreshPending[owner] = struct{}{}
	}
}

func normalizeTaskRecoveryOwners(owners []string) ([]string, map[string]struct{}) {
	order := make([]string, 0, len(owners))
	desired := make(map[string]struct{}, len(owners))
	for _, owner := range owners {
		if owner == "" {
			continue
		}
		if _, duplicate := desired[owner]; duplicate {
			continue
		}
		desired[owner] = struct{}{}
		order = append(order, owner)
	}
	return order, desired
}

func sameTaskRecoveryOwnerSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for owner := range a {
		if _, ok := b[owner]; !ok {
			return false
		}
	}
	return true
}

// ObserveState supplies the controller's health clock. Warning resets both
// intervals. Non-Ready resets the standard suspect/global interval; the
// unlock-chain interval remains Normal-only because it must run before Ready.
func (c *taskRecoveryController) ObserveState(normal, ready bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()

	c.normal = normal
	c.ready = ready
	if !normal {
		c.normalSince = time.Time{}
		c.normalReadySince = time.Time{}
		c.globalStableSince = time.Time{}
		c.resetOwnerStabilityLocked()
		c.resetReturnedRetryIntervalsLocked(false)
	} else {
		if c.normalSince.IsZero() {
			c.normalSince = now
			c.startReturnedRetryIntervalsLocked(now, true)
		}
		if !ready {
			c.normalReadySince = time.Time{}
			c.globalStableSince = time.Time{}
			c.resetOwnerStabilityLocked()
			c.resetReturnedRetryIntervalsLocked(true)
		} else if c.normalReadySince.IsZero() {
			c.normalReadySince = now
			c.startReturnedRetryIntervalsLocked(now, false)
			for owner := range c.activeOwners {
				c.ownerStableSince[owner] = now
			}
			if c.freshPass {
				c.globalStableSince = now
			}
		}
	}
	return c.reconcileStabilityLocked(now)
}

func (c *taskRecoveryController) resetOwnerStabilityLocked() {
	for owner := range c.activeOwners {
		c.ownerStableSince[owner] = time.Time{}
	}
}

func (c *taskRecoveryController) resetReturnedRetryIntervalsLocked(unlockKeepsRunning bool) {
	for owner := range c.returnedRetrySince {
		if unlockKeepsRunning && owner == taskRecoveryUnlockChainOwner {
			continue
		}
		c.returnedRetrySince[owner] = time.Time{}
	}
}

func (c *taskRecoveryController) startReturnedRetryIntervalsLocked(now time.Time, unlockOnly bool) {
	for owner, since := range c.returnedRetrySince {
		if !since.IsZero() || (owner == taskRecoveryUnlockChainOwner) != unlockOnly {
			continue
		}
		c.returnedRetrySince[owner] = now
	}
}

// SetOwnerActive refreshes the observe-only active proof without granting
// marker-write authority to server callbacks. Repeated positive observations
// preserve the original interval; losing or being unable to prove activity
// resets, but does not remove, the owner's suspect.
func (c *taskRecoveryController) SetOwnerActive(owner string, active bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, desired := c.desired[owner]; !desired {
		active = false
	}
	if _, attempted := c.attempted[owner]; !attempted {
		// Current activity is only a continuation proof after this invocation
		// successfully reacquired the owner. Pre-existing healthy state cannot
		// satisfy a suspect retry by observation alone.
		active = false
	}
	if active {
		delete(c.returnedRetrySince, owner)
		if _, alreadyActive := c.activeOwners[owner]; !alreadyActive {
			c.activeOwners[owner] = struct{}{}
			if c.normal && c.ready {
				c.ownerStableSince[owner] = c.clock.Now()
			} else {
				c.ownerStableSince[owner] = time.Time{}
			}
		}
	} else {
		delete(c.activeOwners, owner)
		delete(c.ownerStableSince, owner)
	}
	return c.reconcileStabilityLocked(c.clock.Now())
}

func (c *taskRecoveryController) Schedule() taskRecoverySchedule {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scheduleLocked(c.clock.Now())
}

func (c *taskRecoveryController) Complete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.markerRemoved
}

func (c *taskRecoveryController) HasEligibleOwnerExcept(excluded string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, decision := range c.scheduleLocked(c.clock.Now()).Decisions {
		if decision.Owner != excluded && decision.Eligible {
			return true
		}
	}
	return false
}

func (c *taskRecoveryController) scheduleLocked(now time.Time) taskRecoverySchedule {
	globalDelay := taskRecoveryBackoff(c.marker.GlobalStrike)
	globalRemaining := taskRecoveryIntervalRemaining(now, c.normalReadySince, globalDelay)
	pendingNonSuspects := false
	for _, owner := range c.desiredOrder {
		if owner == taskRecoveryUnlockChainOwner || owner == taskRecoveryEnumerationOwner || c.marker.suspectStrike(owner) != 0 {
			continue
		}
		_, retryPending := c.returnedRetrySince[owner]
		if _, attempted := c.attempted[owner]; !attempted && !retryPending {
			pendingNonSuspects = true
			break
		}
	}

	nonSuspectFresh := make([]taskRecoveryScheduleDecision, 0, len(c.desiredOrder))
	nonSuspectRetries := make([]taskRecoveryScheduleDecision, 0, len(c.desiredOrder))
	suspectFresh := make([]taskRecoveryScheduleDecision, 0, len(c.desiredOrder))
	suspectRetries := make([]taskRecoveryScheduleDecision, 0, len(c.desiredOrder))
	enumeration := make([]taskRecoveryScheduleDecision, 0, 1)
	for _, owner := range c.desiredOrder {
		strike := c.marker.suspectStrike(owner)
		_, attempted := c.attempted[owner]
		_, refreshPending := c.refreshPending[owner]
		retrySince, retryPending := c.returnedRetrySince[owner]
		retryRemaining := time.Duration(0)
		if retryPending {
			retryRemaining = taskRecoveryIntervalRemaining(now, retrySince, taskRecoveryReturnedOwnerRetryDelay)
		}
		attemptEligible := ((!attempted || refreshPending) && !retryPending) || (retryPending && retryRemaining == 0)
		decision := taskRecoveryScheduleDecision{
			Owner:                owner,
			Strike:               strike,
			AlreadyAttempted:     attempted,
			ReturnedRetryPending: retryPending,
		}
		if owner == taskRecoveryEnumerationOwner {
			decision.Cohort = taskRecoveryEnumerationCohort
			ownerDelay := taskRecoveryBackoff(strike)
			ownerRemaining := taskRecoveryIntervalRemaining(now, c.normalReadySince, ownerDelay)
			decision.Delay = maxTaskRecoveryDuration(ownerDelay, globalDelay)
			decision.Remaining = maxTaskRecoveryDuration(ownerRemaining, globalRemaining)
			if retryPending {
				decision.Delay = maxTaskRecoveryDuration(decision.Delay, taskRecoveryReturnedOwnerRetryDelay)
				decision.Remaining = maxTaskRecoveryDuration(decision.Remaining, retryRemaining)
			}
			decision.Eligible = c.normal && c.ready && attemptEligible && decision.Remaining == 0
		} else if owner == taskRecoveryUnlockChainOwner {
			decision.Cohort = taskRecoveryUnlockCohort
			if strike > 0 {
				decision.Delay = unlockChainRecoveryBackoff(strike)
			}
			decision.Remaining = taskRecoveryIntervalRemaining(now, c.normalSince, decision.Delay)
			if retryPending {
				decision.Delay = maxTaskRecoveryDuration(decision.Delay, taskRecoveryReturnedOwnerRetryDelay)
				decision.Remaining = maxTaskRecoveryDuration(decision.Remaining, retryRemaining)
			}
			decision.Eligible = c.normal && attemptEligible && decision.Remaining == 0
		} else if strike == 0 {
			decision.Cohort = taskRecoveryNonSuspectCohort
			decision.Delay = globalDelay
			decision.Remaining = globalRemaining
			if retryPending {
				decision.Delay = maxTaskRecoveryDuration(decision.Delay, taskRecoveryReturnedOwnerRetryDelay)
				decision.Remaining = maxTaskRecoveryDuration(decision.Remaining, retryRemaining)
			}
			decision.Eligible = c.normal && c.ready && attemptEligible && decision.Remaining == 0
		} else {
			decision.Cohort = taskRecoverySuspectCohort
			ownerDelay := taskRecoveryBackoff(strike)
			ownerRemaining := taskRecoveryIntervalRemaining(now, c.normalReadySince, ownerDelay)
			decision.Delay = maxTaskRecoveryDuration(ownerDelay, globalDelay)
			decision.Remaining = maxTaskRecoveryDuration(ownerRemaining, globalRemaining)
			if retryPending {
				decision.Delay = maxTaskRecoveryDuration(decision.Delay, taskRecoveryReturnedOwnerRetryDelay)
				decision.Remaining = maxTaskRecoveryDuration(decision.Remaining, retryRemaining)
			}
			decision.BlockedByNonSuspects = pendingNonSuspects
			decision.Eligible = c.normal && c.ready && attemptEligible && !pendingNonSuspects && decision.Remaining == 0
		}
		if decision.Cohort == taskRecoveryEnumerationCohort {
			enumeration = append(enumeration, decision)
		} else if decision.Cohort == taskRecoveryNonSuspectCohort {
			if retryPending {
				nonSuspectRetries = append(nonSuspectRetries, decision)
			} else {
				nonSuspectFresh = append(nonSuspectFresh, decision)
			}
		} else if retryPending {
			suspectRetries = append(suspectRetries, decision)
		} else {
			suspectFresh = append(suspectFresh, decision)
		}
	}
	// A due returned retry remains behind every owner that has not yet
	// received its current pass. This lets deterministic app-first restore
	// make forward progress even when an earlier owner keeps returning
	// degraded.
	decisions := append(enumeration, nonSuspectFresh...)
	decisions = append(decisions, nonSuspectRetries...)
	decisions = append(decisions, suspectFresh...)
	decisions = append(decisions, suspectRetries...)

	return taskRecoverySchedule{
		Decisions:       decisions,
		GlobalDelay:     globalDelay,
		GlobalRemaining: globalRemaining,
	}
}

func taskRecoveryIntervalRemaining(now, since time.Time, delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	if since.IsZero() {
		return delay
	}
	remaining := delay - now.Sub(since)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func maxTaskRecoveryDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (c *taskRecoveryController) decisionForOwnerLocked(owner string, now time.Time) (taskRecoveryScheduleDecision, bool) {
	for _, decision := range c.scheduleLocked(now).Decisions {
		if decision.Owner == owner {
			return decision, true
		}
	}
	return taskRecoveryScheduleDecision{}, false
}

// RunAttempt serializes automatic owners and commits progress before invoking
// attempt. A failed progress write never calls attempt. A returned owner is not
// followed by another owner until the exact progress pair is durably cleared.
func (c *taskRecoveryController) RunAttempt(owner string, attempt func() taskRecoveryOwnerAttemptResult) taskRecoveryAttemptOutcome {
	return c.runAttempt(owner, attempt, false)
}

// RunQualificationAttempt uses the same durable progress protocol as an
// ordinary owner, but an inconclusive route qualification does not consume the
// owner's ordinary recovery pass or impose its returned-owner retry delay.
func (c *taskRecoveryController) RunQualificationAttempt(owner string, attempt func() taskRecoveryOwnerAttemptResult) taskRecoveryAttemptOutcome {
	return c.runAttempt(owner, attempt, true)
}

func (c *taskRecoveryController) runAttempt(owner string, attempt func() taskRecoveryOwnerAttemptResult, qualificationOnly bool) taskRecoveryAttemptOutcome {
	c.attemptMu.Lock()
	defer c.attemptMu.Unlock()

	c.mu.Lock()
	now := c.clock.Now()
	if c.progressUncertain != nil {
		err := c.progressUncertain
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{Kind: taskRecoveryAttemptProgressStateUncertain, Owner: owner, Err: err}
	}
	decision, desired := c.decisionForOwnerLocked(owner, now)
	if !desired || !decision.Eligible {
		retryAt := now.Add(decision.Remaining)
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{
			Kind:    taskRecoveryAttemptNotEligible,
			Owner:   owner,
			Err:     fmt.Errorf("task recovery owner %q is not eligible", owner),
			RetryAt: retryAt,
		}
	}
	if !c.nextProgressWriteAt.IsZero() && now.Before(c.nextProgressWriteAt) {
		retryAt := c.nextProgressWriteAt
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{
			Kind:    taskRecoveryAttemptMarkerWriteDeferred,
			Owner:   owner,
			Err:     fmt.Errorf("task recovery marker write deferred until %s", retryAt.UTC().Format(time.RFC3339Nano)),
			RetryAt: retryAt,
		}
	}
	if c.markerRemoved {
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{
			Kind:  taskRecoveryAttemptMarkerWriteDeferred,
			Owner: owner,
			Err:   errors.New("task recovery marker is absent"),
		}
	}
	if c.invocationID == "" {
		c.nextProgressWriteAt = now.Add(taskRecoveryMarkerWriteRetryInterval)
		retryAt := c.nextProgressWriteAt
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{
			Kind:    taskRecoveryAttemptMarkerWriteDeferred,
			Owner:   owner,
			Err:     errors.New("task recovery invocation ID is empty"),
			RetryAt: retryAt,
		}
	}
	if c.marker.ActiveOwner != "" || c.marker.ActiveOwnerInvocationID != "" {
		err := &taskRecoveryProgressStateUncertainError{
			Owner:        c.marker.ActiveOwner,
			InvocationID: c.marker.ActiveOwnerInvocationID,
			Cause:        errors.New("task recovery marker already has active progress"),
		}
		c.progressUncertain = err
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{Kind: taskRecoveryAttemptProgressStateUncertain, Owner: owner, Err: err}
	}
	next := cloneTaskRecoveryMarker(c.marker)
	next.ActiveOwner = owner
	next.ActiveOwnerInvocationID = c.invocationID
	if err := c.markerIO.Write(next); err != nil {
		c.nextProgressWriteAt = now.Add(taskRecoveryMarkerWriteRetryInterval)
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{
			Kind:    taskRecoveryAttemptMarkerWriteDeferred,
			Owner:   owner,
			Err:     err,
			RetryAt: c.nextProgressWriteAt,
		}
	}
	c.marker = next
	c.nextProgressWriteAt = time.Time{}
	c.mu.Unlock()

	result := taskRecoveryOwnerAttemptResult{}
	if attempt != nil {
		result = attempt()
	}
	if result.FatalCommitted {
		// The common process fatal owner now owns termination. Keep the exact
		// active-owner/invocation pair in the marker so ExecStopPost can
		// attribute this invocation. A late operation return has no authority
		// to clear progress or let another owner start.
		return taskRecoveryAttemptOutcome{
			Kind:   taskRecoveryAttemptFatalCommitted,
			Owner:  owner,
			Result: result,
		}
	}

	c.mu.Lock()
	if result.EnumerationDesiredKnown {
		if owner != taskRecoveryEnumerationOwner {
			uncertain := &taskRecoveryProgressStateUncertainError{
				Owner: owner, InvocationID: c.invocationID,
				Cause: errors.New("non-enumeration owner returned desired-owner snapshot"),
			}
			c.progressUncertain = uncertain
			c.mu.Unlock()
			return taskRecoveryAttemptOutcome{Kind: taskRecoveryAttemptProgressStateUncertain, Owner: owner, Result: result, Err: uncertain}
		}
		if err := c.setDesiredOwnersLocked(result.EnumerationDesired); err != nil {
			uncertain := &taskRecoveryProgressStateUncertainError{Owner: owner, InvocationID: c.invocationID, Cause: err}
			c.progressUncertain = uncertain
			c.mu.Unlock()
			return taskRecoveryAttemptOutcome{Kind: taskRecoveryAttemptProgressStateUncertain, Owner: owner, Result: result, Err: uncertain}
		}
	}
	if err := c.clearProgressLocked(owner, c.invocationID); err != nil {
		c.progressUncertain = err
		c.mu.Unlock()
		return taskRecoveryAttemptOutcome{
			Kind:   taskRecoveryAttemptProgressStateUncertain,
			Owner:  owner,
			Result: result,
			Err:    err,
		}
	}
	var maintenanceErr error
	if !qualificationOnly || (result.Active && result.Err == nil) {
		maintenanceErr = c.recordReturnedOwnerLocked(owner, result.Active && result.Err == nil, c.clock.Now())
	}
	c.mu.Unlock()
	return taskRecoveryAttemptOutcome{
		Kind:           taskRecoveryAttemptReturned,
		Owner:          owner,
		Result:         result,
		MaintenanceErr: maintenanceErr,
	}
}

func (c *taskRecoveryController) clearProgressLocked(owner, invocationID string) error {
	if c.marker.ActiveOwner != owner || c.marker.ActiveOwnerInvocationID != invocationID {
		return &taskRecoveryProgressStateUncertainError{
			Owner:        owner,
			InvocationID: invocationID,
			Cause: fmt.Errorf(
				"task recovery progress changed: active=%q invocation=%q",
				c.marker.ActiveOwner,
				c.marker.ActiveOwnerInvocationID,
			),
		}
	}
	next := cloneTaskRecoveryMarker(c.marker)
	next.ActiveOwner = ""
	next.ActiveOwnerInvocationID = ""
	started := c.clock.Now()
	deadline := started.Add(taskRecoveryProgressClearBudget)
	var lastErr error
	for {
		if err := c.markerIO.Write(next); err == nil {
			c.marker = next
			return nil
		} else {
			lastErr = err
		}
		now := c.clock.Now()
		if !now.Before(deadline) {
			return &taskRecoveryProgressStateUncertainError{
				Owner:        owner,
				InvocationID: invocationID,
				Cause:        lastErr,
			}
		}
		delay := taskRecoveryProgressClearRetry
		if remaining := deadline.Sub(now); delay > remaining {
			delay = remaining
		}
		c.clock.Sleep(delay)
	}
}

func (c *taskRecoveryController) recordReturnedOwnerLocked(owner string, active bool, now time.Time) error {
	_, desired := c.desired[owner]
	if desired {
		c.attempted[owner] = struct{}{}
	}
	if active && desired {
		delete(c.returnedRetrySince, owner)
		delete(c.refreshPending, owner)
		if _, alreadyActive := c.activeOwners[owner]; !alreadyActive {
			c.activeOwners[owner] = struct{}{}
			if c.normal && c.ready {
				c.ownerStableSince[owner] = now
			} else {
				c.ownerStableSince[owner] = time.Time{}
			}
		}
	} else {
		delete(c.activeOwners, owner)
		delete(c.ownerStableSince, owner)
		if desired {
			if c.normal && (owner == taskRecoveryUnlockChainOwner || c.ready) {
				c.returnedRetrySince[owner] = now
			} else {
				c.returnedRetrySince[owner] = time.Time{}
			}
		}
	}
	if !c.freshPass && len(c.attempted) == len(c.desired) {
		c.freshPass = true
		if c.normal && c.ready {
			c.globalStableSince = now
		}
	}
	return c.reconcileStabilityLocked(now)
}

func (c *taskRecoveryController) reconcileStabilityLocked(now time.Time) error {
	if c.markerRemoved {
		return nil
	}
	next := cloneTaskRecoveryMarker(c.marker)
	changed := false

	if c.ready && next.clearSuspect(taskRecoveryUnlockChainOwner) {
		delete(c.activeOwners, taskRecoveryUnlockChainOwner)
		delete(c.ownerStableSince, taskRecoveryUnlockChainOwner)
		changed = true
	}
	if c.normal && c.ready {
		for owner := range c.activeOwners {
			if _, refreshPending := c.refreshPending[owner]; refreshPending {
				continue
			}
			since := c.ownerStableSince[owner]
			if since.IsZero() || now.Sub(since) < taskMarkerNormalWindow {
				continue
			}
			if next.clearSuspect(owner) {
				changed = true
			}
			delete(c.activeOwners, owner)
			delete(c.ownerStableSince, owner)
		}
		if next.GlobalStrike > 0 && c.freshPass && !c.globalStableSince.IsZero() && now.Sub(c.globalStableSince) >= taskMarkerNormalWindow {
			next.GlobalStrike = 0
			changed = true
		}
	}
	return c.commitMaintenanceLocked(next, changed)
}

func (c *taskRecoveryController) commitMaintenanceLocked(next taskRecoveryMarker, changed bool) error {
	if c.markerRemoved {
		return nil
	}
	removable := len(next.Suspects) == 0 && next.GlobalStrike == 0 && len(c.returnedRetrySince) == 0 && len(c.refreshPending) == 0 && c.freshPass && next.ActiveOwner == "" && next.ActiveOwnerInvocationID == ""
	if removable {
		if err := c.markerIO.Remove(); err != nil {
			return err
		}
		c.marker = next
		c.markerRemoved = true
		return nil
	}
	if !changed {
		return nil
	}
	if err := c.markerIO.Write(next); err != nil {
		return err
	}
	c.marker = next
	return nil
}
