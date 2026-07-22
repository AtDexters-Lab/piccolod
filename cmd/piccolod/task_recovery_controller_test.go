package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeTaskRecoveryControllerClock struct{ now time.Time }

func (c *fakeTaskRecoveryControllerClock) Now() time.Time { return c.now }
func (c *fakeTaskRecoveryControllerClock) Sleep(delay time.Duration) {
	c.now = c.now.Add(delay)
}
func (c *fakeTaskRecoveryControllerClock) Advance(delay time.Duration) {
	c.now = c.now.Add(delay)
}

type fakeTaskRecoveryControllerMarkerIO struct {
	writes    []taskRecoveryMarker
	writeErr  func(int, taskRecoveryMarker) error
	removes   int
	removeErr error
}

func (f *fakeTaskRecoveryControllerMarkerIO) Write(marker taskRecoveryMarker) error {
	copy := cloneTaskRecoveryMarker(marker)
	f.writes = append(f.writes, copy)
	if f.writeErr != nil {
		return f.writeErr(len(f.writes), copy)
	}
	return nil
}

func (f *fakeTaskRecoveryControllerMarkerIO) Remove() error {
	f.removes++
	return f.removeErr
}

func validTaskRecoveryControllerMarker() taskRecoveryMarker {
	return taskRecoveryMarker{
		SchemaVersion: taskRecoveryMarkerSchema,
		Timestamp:     time.Unix(1, 0).UTC(),
		ReasonCode:    "task_high_water",
		Generation:    1,
	}
}

func TestTaskRecoveryControllerCommitsProgressBeforeAttemptAndThrottlesWriteFailure(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(100, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	failWrites := true
	markerIO.writeErr = func(_ int, _ taskRecoveryMarker) error {
		if failWrites {
			return errors.New("forced marker write failure")
		}
		return nil
	}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-2", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:namek"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	attempt := func() taskRecoveryOwnerAttemptResult {
		attempts++
		last := markerIO.writes[len(markerIO.writes)-1]
		if last.ActiveOwner != "app:namek" || last.ActiveOwnerInvocationID != "invocation-2" {
			t.Fatalf("attempt started before progress commit: %+v", last)
		}
		if last.Generation != 1 || last.GlobalStrike != 0 {
			t.Fatalf("progress commit advanced failure state: %+v", last)
		}
		return taskRecoveryOwnerAttemptResult{Active: true}
	}

	first := controller.RunAttempt("app:namek", attempt)
	if first.Kind != taskRecoveryAttemptMarkerWriteDeferred || attempts != 0 || len(markerIO.writes) != 1 {
		t.Fatalf("first outcome=%+v attempts=%d writes=%d", first, attempts, len(markerIO.writes))
	}
	clock.Advance(29 * time.Second)
	second := controller.RunAttempt("app:namek", attempt)
	if second.Kind != taskRecoveryAttemptMarkerWriteDeferred || attempts != 0 || len(markerIO.writes) != 1 {
		t.Fatalf("throttled outcome=%+v attempts=%d writes=%d", second, attempts, len(markerIO.writes))
	}
	if second.RetryAt.Sub(clock.Now()) != time.Second {
		t.Fatalf("retry remaining = %s, want 1s", second.RetryAt.Sub(clock.Now()))
	}

	clock.Advance(time.Second)
	failWrites = false
	third := controller.RunAttempt("app:namek", attempt)
	if third.Kind != taskRecoveryAttemptReturned || attempts != 1 {
		t.Fatalf("third outcome=%+v attempts=%d", third, attempts)
	}
	if len(markerIO.writes) != 3 {
		t.Fatalf("writes=%d, want failed begin + committed begin + clear", len(markerIO.writes))
	}
	cleared := markerIO.writes[len(markerIO.writes)-1]
	if cleared.ActiveOwner != "" || cleared.ActiveOwnerInvocationID != "" {
		t.Fatalf("progress not cleared after return: %+v", cleared)
	}
}

func TestTaskRecoveryControllerQualificationDoesNotConsumeFailedOrdinaryPass(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(150, 0)}
	controller := newTaskRecoveryControllerWithDeps(
		validTaskRecoveryControllerMarker(), "invocation-qualification", &fakeTaskRecoveryControllerMarkerIO{}, clock,
	)
	if err := controller.SetDesiredOwners([]string{"app:alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	qualification := controller.RunQualificationAttempt("app:alpha", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Err: errors.New("route not ready")}
	})
	if qualification.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("qualification outcome=%+v", qualification)
	}
	decision := taskRecoveryDecisionsByOwner(controller.Schedule())["app:alpha"]
	if !decision.Eligible || decision.AlreadyAttempted || decision.ReturnedRetryPending || decision.Remaining != 0 {
		t.Fatalf("failed qualification consumed ordinary pass: %+v", decision)
	}
	ordinaryCalls := 0
	ordinary := controller.RunAttempt("app:alpha", func() taskRecoveryOwnerAttemptResult {
		ordinaryCalls++
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if ordinary.Kind != taskRecoveryAttemptReturned || ordinaryCalls != 1 {
		t.Fatalf("ordinary outcome=%+v calls=%d", ordinary, ordinaryCalls)
	}
}

func TestTaskRecoveryControllerSuccessfulQualificationSatisfiesOrdinaryPass(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(175, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:alpha", Strike: 1}}
	controller := newTaskRecoveryControllerWithDeps(
		marker, "invocation-qualification-success", &fakeTaskRecoveryControllerMarkerIO{}, clock,
	)
	if err := controller.SetDesiredOwners([]string{"app:alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	outcome := controller.RunQualificationAttempt("app:alpha", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if outcome.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("qualification outcome=%+v", outcome)
	}
	decision := taskRecoveryDecisionsByOwner(controller.Schedule())["app:alpha"]
	if decision.Eligible || !decision.AlreadyAttempted || decision.ReturnedRetryPending {
		t.Fatalf("successful qualification did not satisfy ordinary pass: %+v", decision)
	}
}

func TestTaskRecoveryControllerClearFailureReturnsProgressUncertainAndFencesNextOwner(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(200, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	markerIO.writeErr = func(call int, _ taskRecoveryMarker) error {
		if call > 1 {
			return errors.New("forced clear failure")
		}
		return nil
	}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-3", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"storage", "network"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	started := clock.Now()
	outcome := controller.RunAttempt("storage", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if outcome.Kind != taskRecoveryAttemptProgressStateUncertain {
		t.Fatalf("outcome=%+v, want progress-state-uncertain", outcome)
	}
	var uncertain *taskRecoveryProgressStateUncertainError
	if !errors.As(outcome.Err, &uncertain) {
		t.Fatalf("error type=%T, want taskRecoveryProgressStateUncertainError", outcome.Err)
	}
	if elapsed := clock.Now().Sub(started); elapsed != taskRecoveryProgressClearBudget {
		t.Fatalf("clear retry elapsed=%s, want %s", elapsed, taskRecoveryProgressClearBudget)
	}

	nextStarted := false
	next := controller.RunAttempt("network", func() taskRecoveryOwnerAttemptResult {
		nextStarted = true
		return taskRecoveryOwnerAttemptResult{}
	})
	if next.Kind != taskRecoveryAttemptProgressStateUncertain || nextStarted {
		t.Fatalf("next outcome=%+v started=%v", next, nextStarted)
	}
	controller.mu.Lock()
	activeOwner := controller.marker.ActiveOwner
	activeInvocation := controller.marker.ActiveOwnerInvocationID
	controller.mu.Unlock()
	if activeOwner != "storage" || activeInvocation != "invocation-3" {
		t.Fatalf("uncertain progress was cleared in memory: %q/%q", activeOwner, activeInvocation)
	}
}

func TestTaskRecoveryControllerExactPairMismatchReturnsProgressUncertain(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(250, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-exact", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"storage"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	outcome := controller.RunAttempt("storage", func() taskRecoveryOwnerAttemptResult {
		controller.mu.Lock()
		controller.marker.ActiveOwnerInvocationID = "newer-invocation"
		controller.mu.Unlock()
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if outcome.Kind != taskRecoveryAttemptProgressStateUncertain {
		t.Fatalf("outcome=%+v, want progress-state-uncertain", outcome)
	}
	if len(markerIO.writes) != 1 {
		t.Fatalf("writes=%d, stale completion attempted a clear write", len(markerIO.writes))
	}
}

func TestTaskRecoveryControllerFatalCommitRetainsProgressAndFencesLaterOwner(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(275, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-fatal", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"storage", "network"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	outcome := controller.RunAttempt("storage", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Err: context.DeadlineExceeded, FatalCommitted: true}
	})
	if outcome.Kind != taskRecoveryAttemptFatalCommitted {
		t.Fatalf("outcome=%+v, want fatal committed", outcome)
	}
	if len(markerIO.writes) != 1 {
		t.Fatalf("writes=%d, fatal completion cleared progress", len(markerIO.writes))
	}
	controller.mu.Lock()
	activeOwner := controller.marker.ActiveOwner
	activeInvocation := controller.marker.ActiveOwnerInvocationID
	controller.mu.Unlock()
	if activeOwner != "storage" || activeInvocation != "invocation-fatal" {
		t.Fatalf("retained progress=%q/%q", activeOwner, activeInvocation)
	}

	nextStarted := false
	next := controller.RunAttempt("network", func() taskRecoveryOwnerAttemptResult {
		nextStarted = true
		return taskRecoveryOwnerAttemptResult{}
	})
	if next.Kind != taskRecoveryAttemptProgressStateUncertain || nextStarted {
		t.Fatalf("next outcome=%+v started=%v", next, nextStarted)
	}
}

func TestTaskRecoveryControllerScheduleOrdersNonSuspectsAndAppliesBackoffs(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(300, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{
		{Owner: "app:first", Strike: 1},
		{Owner: "app:second", Strike: 2},
		{Owner: "app:third", Strike: 3},
	}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-4", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:first", "app:healthy", "app:second", "app:third"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}

	plan := controller.Schedule()
	if len(plan.Decisions) != 4 || plan.Decisions[0].Owner != "app:healthy" || !plan.Decisions[0].Eligible {
		t.Fatalf("initial plan does not lead with eligible non-suspect: %+v", plan.Decisions)
	}
	if !plan.Decisions[1].BlockedByNonSuspects || plan.Decisions[1].Owner != "app:first" {
		t.Fatalf("strike-one suspect was not held behind non-suspect: %+v", plan.Decisions)
	}
	if outcome := controller.RunAttempt("app:healthy", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true}
	}); outcome.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("healthy attempt outcome=%+v", outcome)
	}

	plan = controller.Schedule()
	decisions := taskRecoveryDecisionsByOwner(plan)
	if !decisions["app:first"].Eligible || decisions["app:first"].Delay != 0 {
		t.Fatalf("strike-one decision=%+v, want immediate", decisions["app:first"])
	}
	if decisions["app:second"].Remaining != 10*time.Minute || decisions["app:third"].Remaining != 30*time.Minute {
		t.Fatalf("recurrent decisions=%+v/%+v", decisions["app:second"], decisions["app:third"])
	}
	clock.Advance(30 * time.Second)
	decisions = taskRecoveryDecisionsByOwner(controller.Schedule())
	if decisions["app:second"].Eligible {
		t.Fatalf("30s standard decision=%+v", decisions["app:second"])
	}
	clock.Advance(9*time.Minute + 30*time.Second)
	decisions = taskRecoveryDecisionsByOwner(controller.Schedule())
	if !decisions["app:second"].Eligible || decisions["app:third"].Eligible {
		t.Fatalf("10m decisions strike2=%+v strike3=%+v", decisions["app:second"], decisions["app:third"])
	}
}

func TestTaskRecoveryEnumerationUsesPersistedStrikeBackoffAcrossRestart(t *testing.T) {
	tests := []struct {
		strike int
		delay  time.Duration
	}{
		{strike: 1, delay: 0},
		{strike: 2, delay: 10 * time.Minute},
		{strike: 3, delay: 30 * time.Minute},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("strike-%d", test.strike), func(t *testing.T) {
			clock := &fakeTaskRecoveryControllerClock{now: time.Unix(500, 0)}
			marker := validTaskRecoveryControllerMarker()
			marker.Suspects = []taskRecoverySuspect{{Owner: taskRecoveryEnumerationOwner, Strike: test.strike}}
			controller := newTaskRecoveryControllerWithDeps(marker, "enumeration-restart", &fakeTaskRecoveryControllerMarkerIO{}, clock)
			if err := controller.SetDesiredOwners([]string{taskRecoveryEnumerationOwner, "app:stale"}); err != nil {
				t.Fatal(err)
			}
			if err := controller.ObserveState(true, true); err != nil {
				t.Fatal(err)
			}
			decision := taskRecoveryDecisionsByOwner(controller.Schedule())[taskRecoveryEnumerationOwner]
			if decision.Cohort != taskRecoveryEnumerationCohort || decision.Delay != test.delay || decision.Remaining != test.delay || decision.Eligible != (test.delay == 0) {
				t.Fatalf("initial enumeration decision=%+v, want delay %s", decision, test.delay)
			}
			if test.delay > 35*time.Second {
				clock.Advance(35 * time.Second)
				decision = taskRecoveryDecisionsByOwner(controller.Schedule())[taskRecoveryEnumerationOwner]
				if decision.Eligible || decision.Remaining != test.delay-35*time.Second {
					t.Fatalf("35-second restart loop bypassed recurrence: %+v", decision)
				}
				clock.Advance(test.delay - 35*time.Second)
			}
			outcome := controller.RunAttempt(taskRecoveryEnumerationOwner, func() taskRecoveryOwnerAttemptResult {
				return taskRecoveryOwnerAttemptResult{
					Active: true, EnumerationDesiredKnown: true,
					EnumerationDesired: []string{taskRecoveryEnumerationOwner, "app:fresh"},
				}
			})
			if outcome.Kind != taskRecoveryAttemptReturned {
				t.Fatalf("enumeration after backoff=%+v", outcome)
			}
			decisions := taskRecoveryDecisionsByOwner(controller.Schedule())
			if _, stale := decisions["app:stale"]; stale {
				t.Fatalf("stale owner survived fresh enumeration: %+v", decisions)
			}
			if fresh := decisions["app:fresh"]; !fresh.Eligible {
				t.Fatalf("fresh owner not exposed after enumeration: %+v", fresh)
			}
			controller.mu.Lock()
			strike := controller.marker.suspectStrike(taskRecoveryEnumerationOwner)
			controller.mu.Unlock()
			if strike != test.strike {
				t.Fatalf("enumeration suspect cleared before stability: strike=%d want=%d", strike, test.strike)
			}
		})
	}
}

func TestTaskRecoveryEnumerationFailedRefreshCannotClearStableSuspect(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(575, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: taskRecoveryEnumerationOwner, Strike: 1}}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "enumeration-refresh", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{taskRecoveryEnumerationOwner}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if outcome := controller.RunAttempt(taskRecoveryEnumerationOwner, func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true, EnumerationDesiredKnown: true, EnumerationDesired: []string{taskRecoveryEnumerationOwner}}
	}); outcome.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("initial enumeration=%+v", outcome)
	}
	controller.RequestOwnerRefresh(taskRecoveryEnumerationOwner)
	clock.Advance(taskMarkerNormalWindow)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if outcome := controller.RunAttempt(taskRecoveryEnumerationOwner, func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Err: errors.New("refresh failed")}
	}); outcome.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("failed refresh=%+v", outcome)
	}
	controller.mu.Lock()
	strike := controller.marker.suspectStrike(taskRecoveryEnumerationOwner)
	controller.mu.Unlock()
	if strike != 1 || markerIO.removes != 0 {
		t.Fatalf("failed refresh cleared stable suspect: strike=%d removes=%d", strike, markerIO.removes)
	}
}

func TestTaskRecoveryControllerGlobalAndUnlockSchedules(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(400, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.GlobalStrike = 2
	marker.Suspects = []taskRecoverySuspect{{Owner: taskRecoveryUnlockChainOwner, Strike: 2}}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-5", &fakeTaskRecoveryControllerMarkerIO{}, clock)
	if err := controller.SetDesiredOwners([]string{"app:healthy", taskRecoveryUnlockChainOwner}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, false); err != nil {
		t.Fatal(err)
	}
	schedule := controller.Schedule()
	if schedule.GlobalDelay != 10*time.Minute || schedule.GlobalRemaining != 10*time.Minute {
		t.Fatalf("global schedule=%+v", schedule)
	}
	decisions := taskRecoveryDecisionsByOwner(schedule)
	if decisions["app:healthy"].Eligible || decisions["app:healthy"].Remaining != 10*time.Minute {
		t.Fatalf("global decision while non-Ready=%+v", decisions["app:healthy"])
	}
	if decisions[taskRecoveryUnlockChainOwner].Remaining != 2*time.Minute {
		t.Fatalf("unlock-chain delay=%+v", decisions[taskRecoveryUnlockChainOwner])
	}
	clock.Advance(2 * time.Minute)
	decisions = taskRecoveryDecisionsByOwner(controller.Schedule())
	if !decisions[taskRecoveryUnlockChainOwner].Eligible {
		t.Fatalf("unlock chain was suppressed by lifecycle/global breaker: %+v", decisions[taskRecoveryUnlockChainOwner])
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	unlockStrike := controller.marker.suspectStrike(taskRecoveryUnlockChainOwner)
	controller.mu.Unlock()
	if unlockStrike != 0 {
		t.Fatalf("lifecycle Ready retained unlock-chain strike %d", unlockStrike)
	}
}

func TestTaskRecoveryControllerWarningAndNonReadyResetIntervals(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(500, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "storage", Strike: 2}}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-6", &fakeTaskRecoveryControllerMarkerIO{}, clock)
	if err := controller.SetDesiredOwners([]string{"storage"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(9 * time.Minute)
	if remaining := taskRecoveryDecisionsByOwner(controller.Schedule())["storage"].Remaining; remaining != time.Minute {
		t.Fatalf("remaining=%s, want 1m", remaining)
	}
	if err := controller.ObserveState(false, true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if err := controller.ObserveState(true, false); err != nil {
		t.Fatal(err)
	}
	decision := taskRecoveryDecisionsByOwner(controller.Schedule())["storage"]
	if decision.Eligible || decision.Remaining != 10*time.Minute {
		t.Fatalf("non-Ready decision after reset=%+v", decision)
	}
	clock.Advance(time.Hour)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	decision = taskRecoveryDecisionsByOwner(controller.Schedule())["storage"]
	if decision.Eligible || decision.Remaining != 10*time.Minute {
		t.Fatalf("Ready interval did not restart=%+v", decision)
	}
}

func TestTaskRecoveryControllerClearsStableSuspectGlobalAndMarker(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(600, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.GlobalStrike = 1
	marker.Suspects = []taskRecoverySuspect{
		{Owner: "app:suspect", Strike: 1},
		{Owner: "app:disabled", Strike: 3},
	}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-7", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:healthy", "app:suspect"}); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	if controller.marker.suspectStrike("app:disabled") != 0 {
		t.Fatal("disabled owner suspect was retained")
	}
	controller.mu.Unlock()
	if markerIO.removes != 0 {
		t.Fatal("marker removed before a fresh desired pass")
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"app:healthy", "app:suspect"} {
		outcome := controller.RunAttempt(owner, func() taskRecoveryOwnerAttemptResult {
			return taskRecoveryOwnerAttemptResult{Active: true}
		})
		if outcome.Kind != taskRecoveryAttemptReturned {
			t.Fatalf("attempt %s outcome=%+v", owner, outcome)
		}
	}
	if markerIO.removes != 0 {
		t.Fatal("marker removed before stability window")
	}
	clock.Advance(taskMarkerNormalWindow - time.Second)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if markerIO.removes != 0 {
		t.Fatal("marker removed before ten continuous minutes")
	}
	clock.Advance(time.Second)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if markerIO.removes != 1 {
		t.Fatalf("marker removes=%d, want 1", markerIO.removes)
	}
	controller.mu.Lock()
	removed := controller.markerRemoved
	remainingSuspects := len(controller.marker.Suspects)
	globalStrike := controller.marker.GlobalStrike
	controller.mu.Unlock()
	if !removed || remainingSuspects != 0 || globalStrike != 0 {
		t.Fatalf("final controller state removed=%v suspects=%d global=%d", removed, remainingSuspects, globalStrike)
	}
}

func TestTaskRecoveryControllerOwnerMustRemainActiveForStabilityWindow(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(650, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:suspect", Strike: 1}}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-active", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:suspect"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if outcome := controller.RunAttempt("app:suspect", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true}
	}); outcome.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("outcome=%+v", outcome)
	}
	clock.Advance(9 * time.Minute)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetOwnerActive("app:suspect", false); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	strike := controller.marker.suspectStrike("app:suspect")
	controller.mu.Unlock()
	if strike != 1 || markerIO.removes != 0 {
		t.Fatalf("inactive owner cleared suspect: strike=%d removes=%d", strike, markerIO.removes)
	}
	if err := controller.SetOwnerActive("app:suspect", true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(taskMarkerNormalWindow)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if markerIO.removes != 1 {
		t.Fatalf("stable active owner did not clear marker: removes=%d", markerIO.removes)
	}
}

func TestTaskRecoveryControllerActivityRefreshRequiresAttemptAndPreservesSince(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(675, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:suspect", Strike: 1}}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-refresh", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:suspect"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetOwnerActive("app:suspect", true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(taskMarkerNormalWindow)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if markerIO.removes != 0 {
		t.Fatal("pre-attempt observation cleared suspect")
	}
	if outcome := controller.RunAttempt("app:suspect", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true}
	}); outcome.Kind != taskRecoveryAttemptReturned {
		t.Fatalf("outcome=%+v", outcome)
	}
	clock.Advance(5 * time.Minute)
	if err := controller.SetOwnerActive("app:suspect", true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Minute)
	if err := controller.SetOwnerActive("app:suspect", true); err != nil {
		t.Fatal(err)
	}
	if markerIO.removes != 1 {
		t.Fatalf("positive refresh restarted interval: removes=%d, want 1", markerIO.removes)
	}
}

func TestTaskRecoveryControllerDoesNotRemoveStrikeFreeMarkerBeforeFreshPass(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(700, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-8", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"network"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(taskMarkerNormalWindow)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if markerIO.removes != 0 {
		t.Fatal("strike-free marker removed without desired pass")
	}
	outcome := controller.RunAttempt("network", func() taskRecoveryOwnerAttemptResult {
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if outcome.Kind != taskRecoveryAttemptReturned || markerIO.removes != 1 {
		t.Fatalf("outcome=%+v removes=%d, want returned and removal", outcome, markerIO.removes)
	}
}

func TestTaskRecoveryControllerListenerlessWorkspaceConvergesMarker(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(725, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:workspace", Strike: 1}}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-workspace", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	outcome := controller.RunAttempt("app:workspace", func() taskRecoveryOwnerAttemptResult {
		// The app capability maps a complete listenerless reconcile to Active;
		// no route-publication proof exists or is required for this owner shape.
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if outcome.Kind != taskRecoveryAttemptReturned || !outcome.Result.Active {
		t.Fatalf("listenerless outcome = %+v", outcome)
	}
	if controller.Complete() || markerIO.removes != 0 {
		t.Fatalf("listenerless suspect skipped stability window: complete=%v removes=%d", controller.Complete(), markerIO.removes)
	}
	clock.Advance(taskMarkerNormalWindow)
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}
	if !controller.Complete() || markerIO.removes != 1 {
		t.Fatalf("stable listenerless owner did not converge marker: complete=%v removes=%d", controller.Complete(), markerIO.removes)
	}
}

func TestTaskRecoveryControllerReturnedFailureRetriesWithoutDesiredSetChange(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(750, 0)}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-retry", markerIO, clock)
	if err := controller.SetDesiredOwners([]string{"app:retry"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ObserveState(true, true); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	first := controller.RunAttempt("app:retry", func() taskRecoveryOwnerAttemptResult {
		attempts++
		return taskRecoveryOwnerAttemptResult{Active: true, Err: errors.New("transient recovery failure")}
	})
	if first.Kind != taskRecoveryAttemptReturned || attempts != 1 {
		t.Fatalf("first outcome=%+v attempts=%d", first, attempts)
	}
	if controller.Complete() || markerIO.removes != 0 {
		t.Fatalf("failed owner completed recovery: complete=%v removes=%d", controller.Complete(), markerIO.removes)
	}

	decision := taskRecoveryDecisionsByOwner(controller.Schedule())["app:retry"]
	if decision.Eligible || !decision.AlreadyAttempted || !decision.ReturnedRetryPending || decision.Remaining != taskRecoveryReturnedOwnerRetryDelay {
		t.Fatalf("initial retry decision=%+v", decision)
	}
	clock.Advance(taskRecoveryReturnedOwnerRetryDelay - time.Second)
	if err := controller.SetDesiredOwners([]string{"app:retry"}); err != nil {
		t.Fatal(err)
	}
	decision = taskRecoveryDecisionsByOwner(controller.Schedule())["app:retry"]
	if decision.Eligible || decision.Remaining != time.Second {
		t.Fatalf("unchanged desire reset/bypassed retry: %+v", decision)
	}

	clock.Advance(time.Second)
	second := controller.RunAttempt("app:retry", func() taskRecoveryOwnerAttemptResult {
		attempts++
		return taskRecoveryOwnerAttemptResult{Active: true}
	})
	if second.Kind != taskRecoveryAttemptReturned || attempts != 2 {
		t.Fatalf("second outcome=%+v attempts=%d", second, attempts)
	}
	if !controller.Complete() || markerIO.removes != 1 {
		t.Fatalf("successful retry did not complete recovery: complete=%v removes=%d", controller.Complete(), markerIO.removes)
	}
}

func taskRecoveryDecisionsByOwner(plan taskRecoverySchedule) map[string]taskRecoveryScheduleDecision {
	out := make(map[string]taskRecoveryScheduleDecision, len(plan.Decisions))
	for _, decision := range plan.Decisions {
		out[decision.Owner] = decision
	}
	return out
}
