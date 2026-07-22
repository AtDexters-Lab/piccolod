package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/autounlock"
	"piccolod/internal/resources/pressure"
)

type fakeTaskFatalContinuity struct {
	prepare func(context.Context, autounlock.PrepareTrigger, time.Duration) (autounlock.PrepareResult, error)
}

func (f *fakeTaskFatalContinuity) Prepare(ctx context.Context, trigger autounlock.PrepareTrigger, ttl time.Duration) (autounlock.PrepareResult, error) {
	return f.prepare(ctx, trigger, ttl)
}

func (*fakeTaskFatalContinuity) Recover(context.Context, autounlock.CompleteUnlockChain) (autounlock.RecoverResult, error) {
	return autounlock.RecoverResult{}, errors.New("unexpected Recover")
}

func (*fakeTaskFatalContinuity) Cancel(context.Context) error {
	return errors.New("unexpected Cancel")
}

type manualTaskFatalTimer struct {
	mu       sync.Mutex
	callback func()
	stopped  bool
	fired    bool
}

func (t *manualTaskFatalTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (t *manualTaskFatalTimer) Fire() bool {
	t.mu.Lock()
	if t.stopped || t.fired {
		t.mu.Unlock()
		return false
	}
	t.fired = true
	callback := t.callback
	t.mu.Unlock()
	callback()
	return true
}

type taskFatalTestHarness struct {
	owner         *taskFatalOwner
	requests      *taskFatalRequestSignal
	holder        *taskRestartUnlockContinuityHolder
	timers        chan *manualTaskFatalTimer
	timerDelay    chan time.Duration
	exits         chan int
	fenced        atomic.Bool
	controlFenced atomic.Bool
	markerPath    string
}

func newTaskFatalTestHarness(t *testing.T, invocationID string) *taskFatalTestHarness {
	t.Helper()
	harness := &taskFatalTestHarness{
		requests:   newTaskFatalRequestSignal(),
		holder:     &taskRestartUnlockContinuityHolder{},
		timers:     make(chan *manualTaskFatalTimer, 4),
		timerDelay: make(chan time.Duration, 4),
		exits:      make(chan int, 2),
		markerPath: t.TempDir() + "/task-recovery.json",
	}
	harness.owner = newTaskFatalOwner(taskFatalOwnerConfig{
		requests:     harness.requests,
		continuity:   harness.holder,
		markerPath:   harness.markerPath,
		invocationID: invocationID,
		now:          func() time.Time { return time.Unix(1_000, 123).UTC() },
		currentOwner: func() string { return "fallback-owner" },
		afterFunc: func(delay time.Duration, callback func()) taskFatalTimer {
			timer := &manualTaskFatalTimer{callback: callback}
			harness.timerDelay <- delay
			harness.timers <- timer
			return timer
		},
		exit: func(code int) {
			harness.exits <- code
		},
		fenceNewWork: func() {
			harness.fenced.Store(true)
		},
		handoffPresent: func() bool { return false },
		fenceControlPlane: func() {
			harness.controlFenced.Store(true)
		},
		logf: func(string, ...any) {},
	})
	return harness
}

func (h *taskFatalTestHarness) assertOneExit(t *testing.T, want int) {
	h.assertOneExitWithFinalDelay(t, want, taskEmergencyFinalizationBudget)
}

func (h *taskFatalTestHarness) assertOneExitWithFinalDelay(t *testing.T, want int, wantFinalDelay time.Duration) {
	t.Helper()
	select {
	case got := <-h.exits:
		if got != want {
			t.Fatalf("exit=%d, want %d", got, want)
		}
	default:
		t.Fatal("fatal owner did not exit")
	}
	select {
	case got := <-h.exits:
		t.Fatalf("fatal owner exited twice; second=%d", got)
	default:
	}
	if !h.fenced.Load() {
		t.Fatal("fatal owner did not fence new work")
	}
	if !h.controlFenced.Load() {
		t.Fatal("fatal owner did not fence new control-plane freezes")
	}
	if got := <-h.timerDelay; got != taskEmergencyDeadline {
		t.Fatalf("hard deadline=%s, want %s", got, taskEmergencyDeadline)
	}
	if got := <-h.timerDelay; got != wantFinalDelay {
		t.Fatalf("final deadline=%s, want %s", got, wantFinalDelay)
	}
}

func TestTaskFatalOwnerBlockingContinuityCannotExtendHardExit(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-blocked")
	started := make(chan struct{})
	release := make(chan struct{})
	harness.holder.Attach(&fakeTaskFatalContinuity{prepare: func(ctx context.Context, trigger autounlock.PrepareTrigger, ttl time.Duration) (autounlock.PrepareResult, error) {
		close(started)
		<-release // Deliberately ignore ctx; the outer hard exit still owns liveness.
		return autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionPrepared}, nil
	}})
	if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("fatal request was not accepted")
	}
	done := make(chan taskFatalOwnerOutcome, 1)
	go func() { done <- harness.owner.Run(nil) }()

	timer := <-harness.timers
	<-started
	if !timer.Fire() {
		t.Fatal("hard timer did not fire")
	}
	select {
	case got := <-harness.exits:
		if got != taskEmergencyExitCode {
			t.Fatalf("hard exit=%d, want %d", got, taskEmergencyExitCode)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking continuity extended the hard exit")
	}
	if !harness.controlFenced.Load() {
		t.Fatal("hard exit occurred before the control-plane freeze fence")
	}
	close(release)
	outcome := <-done
	if !outcome.ContinuityAttempted || outcome.MarkerErr != nil {
		t.Fatalf("outcome=%+v", outcome)
	}
	select {
	case got := <-harness.exits:
		t.Fatalf("normal completion issued a second exit=%d", got)
	default:
	}
}

func TestTaskFatalOwnerHardExitDoesNotWaitForControlPlaneRecovery(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-control-plane-blocked")
	fenceStarted := make(chan struct{})
	releaseFence := make(chan struct{})
	harness.owner.config.fenceControlPlane = func() {
		harness.controlFenced.Store(true)
		close(fenceStarted)
		<-releaseFence
	}
	if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("fatal request was not accepted")
	}
	done := make(chan taskFatalOwnerOutcome, 1)
	go func() { done <- harness.owner.Run(nil) }()

	hardTimer := <-harness.timers
	<-fenceStarted
	if !hardTimer.Fire() {
		t.Fatal("hard timer did not fire")
	}
	select {
	case code := <-harness.exits:
		if code != taskEmergencyExitCode {
			t.Fatalf("hard exit=%d, want %d", code, taskEmergencyExitCode)
		}
	case <-time.After(time.Second):
		t.Fatal("control-plane recovery extended the absolute hard exit")
	}
	close(releaseFence)
	<-done
	if !harness.controlFenced.Load() {
		t.Fatal("control-plane freeze fence was not attempted")
	}
}

func TestTaskFatalOwnerFinalExitLatchBoundsBlockedFinalization(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-final-latch")
	harness.holder.Attach(&fakeTaskFatalContinuity{prepare: func(context.Context, autounlock.PrepareTrigger, time.Duration) (autounlock.PrepareResult, error) {
		return autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionPrepared}, nil
	}})
	logStarted := make(chan struct{})
	releaseLog := make(chan struct{})
	var logOnce sync.Once
	harness.owner.config.logf = func(string, ...any) {
		logOnce.Do(func() { close(logStarted) })
		<-releaseLog
	}
	if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("fatal request was not accepted")
	}
	done := make(chan taskFatalOwnerOutcome, 1)
	go func() { done <- harness.owner.Run(nil) }()

	outerTimer := <-harness.timers
	finalTimer := <-harness.timers
	if got := <-harness.timerDelay; got != taskEmergencyDeadline {
		t.Fatalf("hard deadline=%s, want %s", got, taskEmergencyDeadline)
	}
	if got := <-harness.timerDelay; got != taskEmergencyFinalizationBudget {
		t.Fatalf("final deadline=%s, want %s", got, taskEmergencyFinalizationBudget)
	}
	<-logStarted
	if !finalTimer.Fire() {
		t.Fatal("final exit latch did not fire")
	}
	select {
	case got := <-harness.exits:
		if got != taskEmergencyExitCode {
			t.Fatalf("final exit=%d, want %d", got, taskEmergencyExitCode)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked finalization extended the final exit latch")
	}
	if !harness.controlFenced.Load() {
		t.Fatal("final exit occurred before the control-plane freeze fence")
	}
	if !outerTimer.Fire() {
		t.Fatal("absolute outer timer was not independently armed")
	}
	select {
	case got := <-harness.exits:
		t.Fatalf("outer timer issued a second exit=%d", got)
	default:
	}
	close(releaseLog)
	<-done
	if !harness.fenced.Load() {
		t.Fatal("fatal owner did not fence new work")
	}
}

func TestTaskFatalOwnerFinalExitLatchUsesRemainingOuterBudget(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-final-remaining")
	base := time.Unix(3_000, 0)
	var clockCalls atomic.Int32
	harness.owner.config.now = func() time.Time {
		if clockCalls.Add(1) == 1 {
			return base
		}
		return base.Add(2500 * time.Millisecond)
	}
	harness.holder.Attach(&fakeTaskFatalContinuity{prepare: func(context.Context, autounlock.PrepareTrigger, time.Duration) (autounlock.PrepareResult, error) {
		return autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionPrepared}, nil
	}})
	if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("fatal request was not accepted")
	}
	harness.owner.Run(nil)
	harness.assertOneExitWithFinalDelay(t, taskEmergencyExitCode, 500*time.Millisecond)
}

func TestTaskFatalOwnerContinuityPreparedReusedAndFailureStillExit(t *testing.T) {
	prepareErr := errors.New("provider unavailable")
	tests := []struct {
		name       string
		result     autounlock.PrepareResult
		err        error
		wantResult autounlock.PrepareDisposition
	}{
		{name: "prepared", result: autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionPrepared, ExpiresAt: time.Unix(2_000, 0)}, wantResult: autounlock.PrepareDispositionPrepared},
		{name: "reused", result: autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionReused, ExpiresAt: time.Unix(2_100, 0)}, wantResult: autounlock.PrepareDispositionReused},
		{name: "failure", result: autounlock.PrepareResult{Disposition: autounlock.PrepareDispositionUnavailable}, err: prepareErr, wantResult: autounlock.PrepareDispositionUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newTaskFatalTestHarness(t, "invocation-"+test.name)
			calls := 0
			harness.holder.Attach(&fakeTaskFatalContinuity{prepare: func(ctx context.Context, trigger autounlock.PrepareTrigger, ttl time.Duration) (autounlock.PrepareResult, error) {
				calls++
				if trigger != autounlock.PrepareTriggerRecoveryFatal {
					t.Fatalf("trigger=%q, want %q", trigger, autounlock.PrepareTriggerRecoveryFatal)
				}
				if ttl != taskEmergencyHandoffTTL {
					t.Fatalf("ttl=%s, want %s", ttl, taskEmergencyHandoffTTL)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > taskContinuityPrepareBudget {
					t.Fatalf("prepare deadline=%s ok=%v", deadline, ok)
				}
				return test.result, test.err
			}})
			if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestSerializedOwner, Owner: "storage"}) {
				t.Fatal("fatal request was not accepted")
			}
			outcome := harness.owner.Run(nil)
			if calls != 1 || !outcome.ContinuityAttempted || outcome.ContinuityResult.Disposition != test.wantResult || !errors.Is(outcome.ContinuityErr, test.err) {
				t.Fatalf("calls=%d outcome=%+v", calls, outcome)
			}
			if outcome.MarkerErr != nil || outcome.Marker.ContinuityOutcome != string(test.wantResult) ||
				len(outcome.Marker.Suspects) != 1 || outcome.Marker.Suspects[0].Owner != "storage" {
				t.Fatalf("marker=%+v err=%v", outcome.Marker, outcome.MarkerErr)
			}
			harness.assertOneExit(t, taskEmergencyExitCode)
		})
	}
}

func TestTaskFatalSignalFirstRequestWinsOverLaterCritical(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-first")
	if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestSerializedOwner, Owner: "app:first"}) {
		t.Fatal("first request rejected")
	}
	if harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestProgressStateUncertain}) ||
		harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("later fatal request replaced the first")
	}
	snapshot := pressure.TaskSnapshot{State: pressure.TaskPressureCritical, ReasonCode: pressure.ReasonHighWater}
	if harness.requests.RequestCritical(snapshot) {
		t.Fatal("later Critical request replaced the first")
	}
	outcome := harness.owner.Run(nil)
	if outcome.Request.Kind != taskFatalRequestSerializedOwner || outcome.Request.Owner != "app:first" {
		t.Fatalf("winner=%+v", outcome.Request)
	}
	if len(outcome.Marker.Suspects) != 1 || outcome.Marker.Suspects[0].Owner != "app:first" || outcome.Marker.GlobalStrike != 0 {
		t.Fatalf("first request attribution=%+v", outcome.Marker)
	}
	harness.assertOneExit(t, taskEmergencyExitCode)
}

func TestTaskFatalSignalCriticalFirstWinsOverLaterGeneric(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-critical-first")
	snapshot := pressure.TaskSnapshot{
		State:        pressure.TaskPressureCritical,
		ReasonCode:   pressure.ReasonMaxEvent,
		Current:      200,
		Limit:        2311,
		CurrentKnown: true,
		LimitKnown:   true,
	}
	if !harness.requests.RequestCritical(snapshot) {
		t.Fatal("Critical request was not accepted")
	}
	if harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestSerializedOwner, Owner: "app:later"}) ||
		harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("later generic fatal request replaced Critical")
	}

	outcome := harness.owner.Run(nil)
	if outcome.Request.Kind != taskFatalRequestTaskCritical || !reflect.DeepEqual(outcome.Request.Snapshot, snapshot) {
		t.Fatalf("winner=%+v, want exact Critical snapshot", outcome.Request)
	}
	if outcome.Marker.ReasonCode != pressure.ReasonMaxEvent || outcome.Marker.GlobalStrike != 0 ||
		len(outcome.Marker.Suspects) != 1 || outcome.Marker.Suspects[0].Owner != "fallback-owner" {
		t.Fatalf("Critical attribution=%+v", outcome.Marker)
	}
	harness.assertOneExit(t, taskEmergencyExitCode)
}

func TestTaskFatalOwnerCriticalPreservesSnapshotAndCensus(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "invocation-critical")
	snapshot := pressure.TaskSnapshot{
		State:        pressure.TaskPressureCritical,
		ReasonCode:   pressure.ReasonSustainedHighWater,
		ActionTaken:  "restart",
		Current:      2200,
		Limit:        2311,
		CurrentKnown: true,
		LimitKnown:   true,
		MaxEvents:    7,
		SampledAt:    time.Unix(900, 0).UTC(),
	}
	captured := pressure.TaskCensus{
		Snapshot:       snapshot,
		Goroutines:     99,
		Processes:      []pressure.TaskProcess{{PID: 12, PPID: 1, Comm: "podman", State: "S", Threads: 8}},
		ByComm:         map[string]int{"podman": 1},
		ByState:        map[string]int{"S": 1},
		ThreadsByComm:  map[string]int{"podman": 8},
		SessionCount:   3,
		LifecycleOwner: "app:namek",
	}
	census := make(chan pressure.TaskCensus, 1)
	census <- captured
	if !harness.requests.RequestCritical(snapshot) {
		t.Fatal("Critical request was not accepted")
	}
	outcome := harness.owner.Run(census)
	if outcome.ContinuityAttempted {
		t.Fatal("Critical before continuity registration attempted preparation")
	}
	if !reflect.DeepEqual(outcome.Request.Snapshot, snapshot) || !reflect.DeepEqual(*outcome.Census, captured) {
		t.Fatalf("request/census changed: request=%+v census=%+v", outcome.Request, outcome.Census)
	}
	if outcome.Marker.ReasonCode != snapshot.ReasonCode || outcome.Marker.ContinuityOutcome != "no_handoff" ||
		!outcome.Marker.DetectionAt.Equal(snapshot.SampledAt) ||
		outcome.Marker.TaskCurrent == nil || *outcome.Marker.TaskCurrent != snapshot.Current ||
		outcome.Marker.TaskLimit == nil || *outcome.Marker.TaskLimit != snapshot.Limit || len(outcome.Marker.Suspects) != 1 ||
		outcome.Marker.Suspects[0].Owner != captured.LifecycleOwner {
		t.Fatalf("critical marker=%+v", outcome.Marker)
	}
	harness.assertOneExit(t, taskEmergencyExitCode)
}

func TestTaskFatalOwnerUnlockChainAndProgressUncertainAttribution(t *testing.T) {
	tests := []struct {
		name       string
		request    taskFatalRequest
		wantExit   int
		wantOwner  string
		wantGlobal int
		wantReason string
	}{
		{name: "unlock-chain", request: taskFatalRequest{Kind: taskFatalRequestUnlockChain}, wantExit: taskEmergencyExitCode, wantOwner: taskRecoveryUnlockChainOwner, wantReason: taskFatalReasonUnlockChain},
		{name: "progress-uncertain", request: taskFatalRequest{Kind: taskFatalRequestProgressStateUncertain, Owner: "stale-owner"}, wantExit: taskProgressUncertainExitCode, wantGlobal: 1, wantReason: taskFatalReasonProgressUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newTaskFatalTestHarness(t, "invocation-"+test.name)
			if !harness.requests.Request(test.request) {
				t.Fatal("fatal request was not accepted")
			}
			outcome := harness.owner.Run(nil)
			if outcome.Marker.ReasonCode != test.wantReason || outcome.Marker.GlobalStrike != test.wantGlobal {
				t.Fatalf("marker=%+v", outcome.Marker)
			}
			if test.wantOwner == "" {
				if len(outcome.Marker.Suspects) != 0 {
					t.Fatalf("global failure was attributed: %+v", outcome.Marker)
				}
			} else if len(outcome.Marker.Suspects) != 1 || outcome.Marker.Suspects[0].Owner != test.wantOwner {
				t.Fatalf("owner attribution=%+v", outcome.Marker)
			}
			harness.assertOneExit(t, test.wantExit)
		})
	}
}

func TestTaskFatalOwnerMarkerAdvancementIsInvocationIdempotent(t *testing.T) {
	harness := newTaskFatalTestHarness(t, "same-invocation")
	previous := taskRecoveryMarker{
		SchemaVersion:          taskRecoveryMarkerSchema,
		Timestamp:              time.Unix(700, 0).UTC(),
		ReasonCode:             taskFatalReasonUnlockChain,
		Generation:             4,
		LastFailedInvocationID: "same-invocation",
		Suspects:               []taskRecoverySuspect{{Owner: taskRecoveryUnlockChainOwner, Strike: 2}},
	}
	if err := writeTaskRecoveryMarker(harness.markerPath, previous); err != nil {
		t.Fatal(err)
	}
	if !harness.requests.Request(taskFatalRequest{Kind: taskFatalRequestUnlockChain}) {
		t.Fatal("fatal request was not accepted")
	}
	outcome := harness.owner.Run(nil)
	if !reflect.DeepEqual(outcome.Marker, previous) {
		t.Fatalf("same invocation advanced marker:\n got %+v\nwant %+v", outcome.Marker, previous)
	}
	stored, present, err := loadTaskRecoveryMarker(harness.markerPath)
	if err != nil || !present || !reflect.DeepEqual(stored, previous) {
		t.Fatalf("stored marker=%+v present=%v err=%v", stored, present, err)
	}
	harness.assertOneExit(t, taskEmergencyExitCode)
}
