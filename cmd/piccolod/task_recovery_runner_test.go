package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/autounlock"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/server"
)

const (
	testTaskRecoveryFirstRouteTimeout = 5 * time.Second
	testTaskRecoveryAppTimeout        = 30 * time.Second
)

func TestAdaptServerTaskRecoveryOwnersPreservesRouteQualification(t *testing.T) {
	ordinary := func(context.Context) (bool, error) { return true, nil }
	observe := func(context.Context) (bool, error) { return true, nil }
	qualification := func(context.Context) (bool, error) { return false, nil }
	owners := adaptServerTaskRecoveryOwners([]server.TaskRecoveryOwner{{
		Name:    "app:namek",
		Timeout: testTaskRecoveryAppTimeout,
		Attempt: ordinary,
		AttemptWithResult: func(context.Context) (server.TaskRecoveryOwnerResult, error) {
			return server.TaskRecoveryOwnerResult{Active: true, RouteBearing: true, ActivePublication: true}, nil
		},
		ObserveActive: observe,
		RouteQualification: &server.TaskRecoveryQualification{
			Timeout: testTaskRecoveryFirstRouteTimeout,
			Attempt: qualification,
		},
	}})
	if len(owners) != 1 {
		t.Fatalf("adapted owners=%d, want 1", len(owners))
	}
	owner := owners[0]
	if owner.Name != "app:namek" || owner.AppID != "namek" || owner.Timeout != testTaskRecoveryAppTimeout || owner.Attempt == nil || owner.AttemptDetailed == nil {
		t.Fatalf("ordinary owner=%+v", owner)
	}
	if owner.ObserveActive == nil {
		t.Fatal("activity observer was not adapted")
	}
	if owner.RouteQualification == nil || owner.RouteQualification.Timeout != testTaskRecoveryFirstRouteTimeout || owner.RouteQualification.Attempt == nil {
		t.Fatalf("route qualification=%+v", owner.RouteQualification)
	}
}

func TestTaskRecoveryScheduleGlobalSuppressionBeforeEnumeration(t *testing.T) {
	schedule := taskRecoverySchedule{Decisions: []taskRecoveryScheduleDecision{
		{Owner: "app:alpha", Remaining: 10 * time.Minute},
	}}
	if !taskRecoveryScheduleHasGlobalSuppression(schedule, true) {
		t.Fatal("delayed retained owner was not globally visible before enumeration")
	}
	if taskRecoveryScheduleHasGlobalSuppression(schedule, false) {
		t.Fatal("per-app delay remained global after enumeration")
	}
	schedule.GlobalRemaining = time.Minute
	if !taskRecoveryScheduleHasGlobalSuppression(schedule, false) {
		t.Fatal("controller-global delay was not visible after enumeration")
	}
}

func TestExecuteBoundedTaskRecoveryOwnerCompletionAndFatalArbitration(t *testing.T) {
	t.Run("return during grace wins", func(t *testing.T) {
		release := make(chan struct{})
		fatalCalls := 0
		done := make(chan taskRecoveryOwnerAttemptResult, 1)
		go func() {
			done <- executeBoundedTaskRecoveryOwner(context.Background(), taskRecoveryOwner{
				Name:    "network",
				Timeout: 10 * time.Millisecond,
				Attempt: func(ctx context.Context) (bool, error) {
					<-ctx.Done()
					<-release
					return true, ctx.Err()
				},
			}, 100*time.Millisecond, func(string) bool {
				fatalCalls++
				return true
			})
		}()
		time.Sleep(20 * time.Millisecond)
		close(release)
		result := <-done
		if result.FatalCommitted || !result.Active || !errors.Is(result.Err, context.DeadlineExceeded) {
			t.Fatalf("result=%+v", result)
		}
		if fatalCalls != 0 {
			t.Fatalf("fatal calls=%d, want 0", fatalCalls)
		}
	})

	t.Run("grace wins and late return is discarded", func(t *testing.T) {
		release := make(chan struct{})
		fatalOwner := make(chan string, 1)
		result := executeBoundedTaskRecoveryOwner(context.Background(), taskRecoveryOwner{
			Name:    "app:blocked",
			Timeout: 10 * time.Millisecond,
			Attempt: func(context.Context) (bool, error) {
				<-release
				return true, nil
			},
		}, 10*time.Millisecond, func(owner string) bool {
			fatalOwner <- owner
			return true
		})
		if !result.FatalCommitted {
			t.Fatalf("result=%+v, want fatal committed", result)
		}
		if got := <-fatalOwner; got != "app:blocked" {
			t.Fatalf("fatal owner=%q", got)
		}
		close(release)
	})
}

func TestExecuteBoundedTaskRecoveryOwnerProcessCancellationDoesNotRequestFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	done := make(chan taskRecoveryOwnerAttemptResult, 1)
	fatalCalls := make(chan struct{}, 1)
	go func() {
		done <- executeBoundedTaskRecoveryOwner(ctx, taskRecoveryOwner{
			Name:    "update",
			Timeout: time.Second,
			Attempt: func(ctx context.Context) (bool, error) {
				<-ctx.Done()
				<-release
				return false, ctx.Err()
			},
		}, 10*time.Millisecond, func(string) bool {
			fatalCalls <- struct{}{}
			return true
		})
	}()
	cancel()
	select {
	case <-done:
		t.Fatal("process-cancelled owner returned before its body acknowledged cancellation")
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-fatalCalls:
		t.Fatal("process cancellation requested a fatal recovery restart")
	default:
	}
	close(release)
	result := <-done
	if result.FatalCommitted || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("result=%+v", result)
	}
}

type fakeTaskRecoveryRuntime struct {
	mu sync.Mutex

	snapshot         pressure.TaskSnapshot
	ready            bool
	core             []taskRecoveryOwner
	owners           []taskRecoveryOwner
	enumerate        func(int) []taskRecoveryOwner
	enumerateErr     func(int) error
	enumerateContext func(context.Context, int) ([]taskRecoveryOwner, error)
	enumerations     int
	prepared         []string
	released         []string
	globalSuppressed bool
}

type concurrentTaskRecoveryClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *concurrentTaskRecoveryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *concurrentTaskRecoveryClock) Sleep(delay time.Duration) {
	c.Advance(delay)
}

func (c *concurrentTaskRecoveryClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func (f *fakeTaskRecoveryRuntime) TaskPressureSnapshot() pressure.TaskSnapshot { return f.snapshot }
func (f *fakeTaskRecoveryRuntime) LifecycleReady() bool                        { return f.ready }
func (f *fakeTaskRecoveryRuntime) SetTaskRecoveryGlobalSuppression(suppressed bool) {
	f.mu.Lock()
	f.globalSuppressed = suppressed
	f.mu.Unlock()
}
func (f *fakeTaskRecoveryRuntime) CoreTaskRecoveryOwners() []taskRecoveryOwner {
	return append([]taskRecoveryOwner(nil), f.core...)
}
func (f *fakeTaskRecoveryRuntime) DecryptedTaskRecoveryOwners(ctx context.Context) ([]taskRecoveryOwner, error) {
	f.mu.Lock()
	index := f.enumerations
	f.enumerations++
	enumerate := f.enumerate
	enumerateErr := f.enumerateErr
	enumerateContext := f.enumerateContext
	owners := append([]taskRecoveryOwner(nil), f.owners...)
	f.mu.Unlock()
	if enumerateContext != nil {
		return enumerateContext(ctx, index)
	}
	if enumerateErr != nil {
		if err := enumerateErr(index); err != nil {
			return nil, err
		}
	}
	if enumerate != nil {
		return append([]taskRecoveryOwner(nil), enumerate(index)...), nil
	}
	return owners, nil
}

func TestTaskRecoveryRunnerEnumerationFailureStartsNoSuccessorAndKeepsMarker(t *testing.T) {
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "enumeration-error", markerIO, &fakeTaskRecoveryControllerClock{now: time.Unix(650, 0)})
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
		owners: []taskRecoveryOwner{{
			Name: "app:stale", AppID: "stale", Timeout: time.Second,
			Attempt: func(context.Context) (bool, error) {
				t.Fatal("stale successor started after enumeration failure")
				return true, nil
			},
		}},
	}
	runtime.enumerateErr = func(int) error {
		cancel()
		return errors.New("enumeration unavailable")
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller: controller, runtime: runtime, initialDesired: []string{"app:stale"},
		pollInterval:     time.Millisecond,
		requestFatal:     func(string) bool { t.Fatal("returned enumeration failure requested fatal"); return false },
		requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
		logf:             func(string, ...any) {},
	})
	runner.Run(ctx)
	if markerIO.removes != 0 || controller.Complete() {
		t.Fatalf("enumeration failure changed marker completion: removes=%d complete=%v", markerIO.removes, controller.Complete())
	}
}

func TestTaskRecoveryRunnerEnumerationStallUsesFatalOwnerAndStops(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(fmt.Sprintf("request-accepted-%v", accepted), func(t *testing.T) {
			markerIO := &fakeTaskRecoveryControllerMarkerIO{}
			controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "enumeration-stall", markerIO, &fakeTaskRecoveryControllerClock{now: time.Unix(660, 0)})
			release := make(chan struct{})
			fatalOwner := make(chan string, 1)
			runtime := &fakeTaskRecoveryRuntime{
				snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal}, ready: true,
				owners: []taskRecoveryOwner{{Name: "app:successor", AppID: "successor", Timeout: time.Second, Attempt: func(context.Context) (bool, error) {
					t.Fatal("successor started after enumeration liveness fatal")
					return true, nil
				}}},
				enumerateContext: func(context.Context, int) ([]taskRecoveryOwner, error) {
					<-release
					return nil, nil
				},
			}
			runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
				controller: controller, runtime: runtime, initialDesired: []string{"app:successor"},
				enumerationTimeout: 10 * time.Millisecond, cancelGrace: 10 * time.Millisecond,
				requestFatal:     func(owner string) bool { fatalOwner <- owner; return accepted },
				requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
				logf:             func(string, ...any) {},
			})
			done := make(chan struct{})
			go func() { runner.Run(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("runner did not return after enumeration deadline plus grace")
			}
			if got := <-fatalOwner; got != taskRecoveryEnumerationOwner {
				t.Fatalf("fatal owner=%q, want %q", got, taskRecoveryEnumerationOwner)
			}
			if markerIO.removes != 0 || controller.Complete() {
				t.Fatalf("enumeration fatal changed marker completion: removes=%d complete=%v", markerIO.removes, controller.Complete())
			}
			close(release)
		})
	}
}
func (f *fakeTaskRecoveryRuntime) PrepareTaskRecoveryApps(ids []string) {
	f.mu.Lock()
	f.prepared = append(f.prepared, ids...)
	f.mu.Unlock()
}
func (f *fakeTaskRecoveryRuntime) ReleaseTaskRecoveryApp(id string) {
	f.mu.Lock()
	f.released = append(f.released, id)
	f.mu.Unlock()
}

func TestTaskRecoveryRunnerAttemptsNonSuspectBeforeSuspectAndReleasesApps(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(500, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:a", Strike: 1}}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-runner", &fakeTaskRecoveryControllerMarkerIO{}, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var orderMu sync.Mutex
	order := make([]string, 0, 2)
	owner := func(id string, finish bool) taskRecoveryOwner {
		return taskRecoveryOwner{
			Name:    "app:" + id,
			AppID:   id,
			Timeout: time.Second,
			Attempt: func(context.Context) (bool, error) {
				orderMu.Lock()
				order = append(order, id)
				orderMu.Unlock()
				if finish {
					cancel()
				}
				return true, nil
			},
		}
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
		owners:   []taskRecoveryOwner{owner("a", true), owner("c", false)},
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:     controller,
		runtime:        runtime,
		initialDesired: []string{"app:a"},
		pollInterval:   time.Millisecond,
		requestFatal:   func(string) bool { t.Fatal("unexpected fatal request"); return false },
		requestUncertain: func() bool {
			t.Fatal("unexpected uncertain request")
			return false
		},
		logf: func(string, ...any) {},
	})
	runner.Run(ctx)

	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if want := []string{"c", "a"}; !reflect.DeepEqual(gotOrder, want) {
		t.Fatalf("attempt order=%v, want %v", gotOrder, want)
	}
	runtime.mu.Lock()
	prepared := append([]string(nil), runtime.prepared...)
	released := append([]string(nil), runtime.released...)
	runtime.mu.Unlock()
	if want := []string{"a", "c"}; !reflect.DeepEqual(prepared, want) {
		t.Fatalf("prepared apps=%v, want %v", prepared, want)
	}
	if want := []string{"c", "a"}; !reflect.DeepEqual(released, want) {
		t.Fatalf("released apps=%v, want %v", released, want)
	}
}

func TestTaskRecoveryRunnerContinuesWithKnownHeadroomWhenMaxEventsUnavailable(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(700, 0)}
	controller := newTaskRecoveryControllerWithDeps(
		validTaskRecoveryControllerMarker(),
		"invocation-runner-events-unavailable",
		&fakeTaskRecoveryControllerMarkerIO{},
		clock,
	)

	attempted := make(chan struct{}, 1)
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{
			State: pressure.TaskPressureUnavailable, ReasonCode: pressure.ReasonMonitorUnavailable,
			Current: 10, Limit: 100, CurrentKnown: true, LimitKnown: true,
		},
		ready: true,
		owners: []taskRecoveryOwner{{
			Name: "app:a", AppID: "a", Timeout: time.Second,
			Attempt: func(context.Context) (bool, error) {
				attempted <- struct{}{}
				return true, nil
			},
		}},
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller: controller,
		runtime:    runtime,
		initialDesired: []string{
			"app:a",
		},
		pollInterval: time.Millisecond,
		requestFatal: func(string) bool {
			t.Fatal("unexpected fatal request")
			return false
		},
		requestUncertain: func() bool {
			t.Fatal("unexpected uncertain request")
			return false
		},
		logf: func(string, ...any) {},
	})

	done := make(chan struct{})
	go func() {
		runner.Run(context.Background())
		close(done)
	}()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("known low headroom did not admit automatic recovery")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not complete after admitted recovery")
	}
}

func TestTaskRecoveryRunnerReturnedFailureRetriesAfterOtherFreshOwner(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(800, 0)}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-runner-retry", &fakeTaskRecoveryControllerMarkerIO{}, clock)

	var orderMu sync.Mutex
	order := make([]string, 0, 3)
	retryAttempts := 0
	retryOwner := taskRecoveryOwner{
		Name:    "app:a",
		AppID:   "a",
		Timeout: time.Second,
		Attempt: func(context.Context) (bool, error) {
			orderMu.Lock()
			defer orderMu.Unlock()
			retryAttempts++
			order = append(order, "a")
			if retryAttempts == 1 {
				return false, nil
			}
			return true, nil
		},
	}
	otherOwner := taskRecoveryOwner{
		Name:    "app:b",
		AppID:   "b",
		Timeout: time.Second,
		Attempt: func(context.Context) (bool, error) {
			orderMu.Lock()
			order = append(order, "b")
			orderMu.Unlock()
			clock.Advance(taskRecoveryReturnedOwnerRetryDelay)
			return true, nil
		},
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
		owners:   []taskRecoveryOwner{retryOwner, otherOwner},
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:     controller,
		runtime:        runtime,
		initialDesired: []string{"app:a", "app:b"},
		pollInterval:   time.Millisecond,
		requestFatal:   func(string) bool { t.Fatal("unexpected fatal request"); return false },
		requestUncertain: func() bool {
			t.Fatal("unexpected uncertain request")
			return false
		},
		logf: func(string, ...any) {},
	})
	done := make(chan struct{})
	go func() {
		runner.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not finish after successful retry")
	}

	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if want := []string{"a", "b", "a"}; !reflect.DeepEqual(gotOrder, want) {
		t.Fatalf("attempt order=%v, want %v", gotOrder, want)
	}
	runtime.mu.Lock()
	released := append([]string(nil), runtime.released...)
	runtime.mu.Unlock()
	if want := []string{"b", "a"}; !reflect.DeepEqual(released, want) {
		t.Fatalf("released apps=%v, want %v", released, want)
	}
}

func TestTaskRecoveryRunnerKeepsFailedAppPreparedUntilSuccessfulRetry(t *testing.T) {
	tests := []struct {
		name   string
		active bool
		err    error
	}{
		{name: "inactive"},
		{name: "error", active: true, err: errors.New("recovery degraded")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &concurrentTaskRecoveryClock{now: time.Unix(820, 0)}
			controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-held-retry", &fakeTaskRecoveryControllerMarkerIO{}, clock)
			firstReturned := make(chan struct{})
			allowRetry := make(chan struct{})
			attempts := 0
			owner := taskRecoveryOwner{
				Name: "app:a", AppID: "a", Timeout: time.Second,
				Attempt: func(context.Context) (bool, error) {
					attempts++
					if attempts == 1 {
						close(firstReturned)
						return test.active, test.err
					}
					<-allowRetry
					return true, nil
				},
			}
			runtime := &fakeTaskRecoveryRuntime{
				snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
				ready:    true,
				owners:   []taskRecoveryOwner{owner},
			}
			runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
				controller: controller, runtime: runtime,
				initialDesired: []string{"app:a"}, pollInterval: time.Millisecond,
				requestFatal: func(string) bool { t.Fatal("unexpected fatal request"); return false },
				requestUncertain: func() bool {
					t.Fatal("unexpected uncertain request")
					return false
				},
				logf: func(string, ...any) {},
			})
			done := make(chan struct{})
			go func() { runner.Run(context.Background()); close(done) }()

			select {
			case <-firstReturned:
			case <-time.After(time.Second):
				t.Fatal("first recovery attempt did not return")
			}
			deadline := time.Now().Add(time.Second)
			for {
				decision := taskRecoveryDecisionsByOwner(controller.Schedule())["app:a"]
				if decision.ReturnedRetryPending {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("failed attempt did not enter controller retry: %+v", decision)
				}
				time.Sleep(time.Millisecond)
			}
			runtime.mu.Lock()
			prepared := append([]string(nil), runtime.prepared...)
			releasedBeforeRetry := append([]string(nil), runtime.released...)
			runtime.mu.Unlock()
			if !reflect.DeepEqual(prepared, []string{"a"}) {
				t.Fatalf("prepared apps=%v, want [a]", prepared)
			}
			if len(releasedBeforeRetry) != 0 {
				t.Fatalf("failed app released before controller retry: %v", releasedBeforeRetry)
			}

			clock.Advance(taskRecoveryReturnedOwnerRetryDelay)
			close(allowRetry)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("runner did not finish after successful retry")
			}
			runtime.mu.Lock()
			released := append([]string(nil), runtime.released...)
			runtime.mu.Unlock()
			if !reflect.DeepEqual(released, []string{"a"}) {
				t.Fatalf("released apps=%v, want [a] after successful retry", released)
			}
		})
	}
}

func TestTaskRecoveryRunnerRefreshesSuccessfulAppActivityUntilStrikeClears(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(850, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:alpha", Strike: 1}}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-active-refresh", markerIO, clock)

	attempts := 0
	observations := 0
	owner := taskRecoveryOwner{
		Name:    "app:alpha",
		AppID:   "alpha",
		Timeout: time.Second,
		Attempt: func(context.Context) (bool, error) {
			attempts++
			return true, nil
		},
		ObserveActive: func(context.Context) (bool, error) {
			observations++
			if attempts > 0 {
				clock.Advance(taskMarkerNormalWindow)
			}
			return true, nil
		},
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
		owners:   []taskRecoveryOwner{owner},
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:     controller,
		runtime:        runtime,
		initialDesired: []string{"app:alpha"},
		pollInterval:   time.Millisecond,
		requestFatal:   func(string) bool { t.Fatal("unexpected fatal request"); return false },
		requestUncertain: func() bool {
			t.Fatal("unexpected uncertain request")
			return false
		},
		logf: func(string, ...any) {},
	})
	runTaskRecoveryRunnerTest(t, runner, context.Background())

	if attempts != 1 || observations < 2 {
		t.Fatalf("attempts=%d observations=%d, want 1 and at least 2", attempts, observations)
	}
	if markerIO.removes != 1 {
		t.Fatalf("marker removes=%d, want 1", markerIO.removes)
	}
}

func TestTaskRecoveryRunnerLostOrUnknownAppActivityResetsStrikeStability(t *testing.T) {
	tests := []struct {
		name       string
		observeErr error
	}{
		{name: "inactive"},
		{name: "unknown", observeErr: errors.New("activity unknown")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &fakeTaskRecoveryControllerClock{now: time.Unix(860, 0)}
			marker := validTaskRecoveryControllerMarker()
			marker.Suspects = []taskRecoverySuspect{{Owner: "app:alpha", Strike: 1}}
			markerIO := &fakeTaskRecoveryControllerMarkerIO{}
			controller := newTaskRecoveryControllerWithDeps(marker, "invocation-activity-loss", markerIO, clock)
			ctx, cancel := context.WithCancel(context.Background())
			attempts := 0
			owner := taskRecoveryOwner{
				Name:    "app:alpha",
				AppID:   "alpha",
				Timeout: time.Second,
				Attempt: func(context.Context) (bool, error) {
					attempts++
					return true, nil
				},
				ObserveActive: func(context.Context) (bool, error) {
					if attempts == 0 {
						return true, nil
					}
					clock.Advance(taskMarkerNormalWindow)
					cancel()
					return false, test.observeErr
				},
			}
			runtime := &fakeTaskRecoveryRuntime{
				snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
				ready:    true,
				owners:   []taskRecoveryOwner{owner},
			}
			runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
				controller:     controller,
				runtime:        runtime,
				initialDesired: []string{"app:alpha"},
				pollInterval:   time.Millisecond,
				requestFatal:   func(string) bool { t.Fatal("unexpected fatal request"); return false },
				requestUncertain: func() bool {
					t.Fatal("unexpected uncertain request")
					return false
				},
				logf: func(string, ...any) {},
			})
			runTaskRecoveryRunnerTest(t, runner, ctx)

			controller.mu.Lock()
			strike := controller.marker.suspectStrike("app:alpha")
			controller.mu.Unlock()
			if attempts != 1 || strike != 1 || markerIO.removes != 0 {
				t.Fatalf("attempts=%d strike=%d removes=%d, want 1/1/0", attempts, strike, markerIO.removes)
			}
		})
	}
}

func TestTaskRecoveryRunnerEnumerationErrorInvalidatesRetainedAppActivity(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(870, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:alpha", Strike: 1}}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-enumeration-loss", markerIO, clock)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	owner := taskRecoveryOwner{
		Name:    "app:alpha",
		AppID:   "alpha",
		Timeout: time.Second,
		Attempt: func(context.Context) (bool, error) {
			attempts++
			return true, nil
		},
		ObserveActive: func(context.Context) (bool, error) { return true, nil },
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
		owners:   []taskRecoveryOwner{owner},
	}
	runtime.enumerateErr = func(index int) error {
		if index == 0 {
			return nil
		}
		clock.Advance(taskMarkerNormalWindow)
		cancel()
		return errors.New("durable owner enumeration unavailable")
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:       controller,
		runtime:          runtime,
		initialDesired:   []string{"app:alpha"},
		pollInterval:     time.Millisecond,
		refreshInterval:  time.Nanosecond,
		requestFatal:     func(string) bool { t.Fatal("unexpected fatal request"); return false },
		requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
		logf:             func(string, ...any) {},
	})
	runTaskRecoveryRunnerTest(t, runner, ctx)

	controller.mu.Lock()
	strike := controller.marker.suspectStrike("app:alpha")
	controller.mu.Unlock()
	if attempts != 1 || strike != 1 || markerIO.removes != 0 {
		t.Fatalf("attempts=%d strike=%d removes=%d, want 1/1/0", attempts, strike, markerIO.removes)
	}
}

func TestTaskRecoveryRunnerOwnerRemovalRetiresItsStrike(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(880, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:alpha", Strike: 1}}
	markerIO := &fakeTaskRecoveryControllerMarkerIO{}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-owner-removal", markerIO, clock)
	attempts := 0
	owner := taskRecoveryOwner{
		Name:          "app:alpha",
		AppID:         "alpha",
		Timeout:       time.Second,
		Attempt:       func(context.Context) (bool, error) { attempts++; return false, nil },
		ObserveActive: func(context.Context) (bool, error) { return true, nil },
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
	}
	runtime.enumerate = func(index int) []taskRecoveryOwner {
		if index == 0 {
			return []taskRecoveryOwner{owner}
		}
		return nil
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:       controller,
		runtime:          runtime,
		initialDesired:   []string{"app:alpha"},
		pollInterval:     time.Millisecond,
		refreshInterval:  time.Nanosecond,
		requestFatal:     func(string) bool { t.Fatal("unexpected fatal request"); return false },
		requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
		logf:             func(string, ...any) {},
	})
	runTaskRecoveryRunnerTest(t, runner, context.Background())
	if attempts != 1 || markerIO.removes != 1 {
		t.Fatalf("attempts=%d removes=%d, want 1/1", attempts, markerIO.removes)
	}
	runtime.mu.Lock()
	prepared := append([]string(nil), runtime.prepared...)
	released := append([]string(nil), runtime.released...)
	runtime.mu.Unlock()
	if !reflect.DeepEqual(prepared, []string{"alpha"}) {
		t.Fatalf("prepared apps=%v, want [alpha]", prepared)
	}
	if !reflect.DeepEqual(released, []string{"alpha"}) {
		t.Fatalf("removed desired owner did not release retained suppression: %v", released)
	}
}

func TestTaskRecoveryRunnerFailedQualificationGetsFreshOrdinaryAttemptWithoutTransfer(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(900, 0)}
	controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "invocation-qualification-drift", &fakeTaskRecoveryControllerMarkerIO{}, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var callsMu sync.Mutex
	calls := make([]string, 0, 2)
	var aOrdinaryBound time.Duration
	var bOrdinaryBound time.Duration
	owner := func(id string, timeout time.Duration, attempt func(context.Context) (bool, error), qualification func(context.Context) (bool, error)) taskRecoveryOwner {
		result := taskRecoveryOwner{
			Name: "app:" + id, AppID: id, Timeout: timeout, Attempt: attempt,
			AttemptDetailed: func(ctx context.Context) taskRecoveryOwnerAttemptResult {
				active, err := attempt(ctx)
				return taskRecoveryOwnerAttemptResult{
					Active: active, RouteKnown: true, RouteActive: active && err == nil, Err: err,
				}
			},
		}
		if qualification != nil {
			result.RouteQualification = &taskRecoveryQualification{Timeout: testTaskRecoveryFirstRouteTimeout, Attempt: qualification}
		}
		return result
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
	}
	runtime.enumerate = func(index int) []taskRecoveryOwner {
		ordinaryA := owner("a", testTaskRecoveryAppTimeout, func(ctx context.Context) (bool, error) {
			if deadline, ok := ctx.Deadline(); ok {
				aOrdinaryBound = time.Until(deadline)
			}
			callsMu.Lock()
			calls = append(calls, "a-ordinary")
			callsMu.Unlock()
			return true, nil
		}, nil)
		ordinaryB := func(ctx context.Context) (bool, error) {
			deadline, ok := ctx.Deadline()
			if ok {
				bOrdinaryBound = time.Until(deadline)
			}
			callsMu.Lock()
			calls = append(calls, "b-ordinary")
			callsMu.Unlock()
			cancel()
			return true, nil
		}
		if index == 0 {
			return []taskRecoveryOwner{
				owner("a", testTaskRecoveryAppTimeout, ordinaryA.Attempt, func(context.Context) (bool, error) {
					callsMu.Lock()
					calls = append(calls, "a-qualification")
					callsMu.Unlock()
					return false, nil
				}),
				owner("b", testTaskRecoveryAppTimeout, ordinaryB, nil),
			}
		}
		return []taskRecoveryOwner{
			owner("b", testTaskRecoveryAppTimeout, ordinaryB, func(context.Context) (bool, error) {
				callsMu.Lock()
				calls = append(calls, "b-qualification")
				callsMu.Unlock()
				return true, nil
			}),
			ordinaryA,
		}
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:     controller,
		runtime:        runtime,
		initialDesired: []string{"app:a", "app:b"},
		pollInterval:   time.Millisecond,
		requestFatal:   func(string) bool { t.Error("unexpected fatal request"); return false },
		requestUncertain: func() bool {
			t.Error("unexpected uncertain request")
			return false
		},
		logf: func(format string, args ...any) {
			callsMu.Lock()
			calls = append(calls, "log:"+fmt.Sprintf(format, args...))
			callsMu.Unlock()
		},
	})
	runTaskRecoveryRunnerTest(t, runner, ctx)

	callsMu.Lock()
	gotCalls := make([]string, 0, len(calls))
	logs := make([]string, 0)
	for _, call := range calls {
		if strings.HasPrefix(call, "log:") {
			logs = append(logs, strings.TrimPrefix(call, "log:"))
			continue
		}
		gotCalls = append(gotCalls, call)
	}
	callsMu.Unlock()
	if want := []string{"a-qualification", "a-ordinary", "b-ordinary"}; !reflect.DeepEqual(gotCalls, want) {
		t.Fatalf("calls=%v, want %v", gotCalls, want)
	}
	if aOrdinaryBound <= testTaskRecoveryFirstRouteTimeout || aOrdinaryBound > testTaskRecoveryAppTimeout {
		t.Fatalf("A ordinary bound=%s, want >%s and <=%s", aOrdinaryBound, testTaskRecoveryFirstRouteTimeout, testTaskRecoveryAppTimeout)
	}
	if bOrdinaryBound <= testTaskRecoveryFirstRouteTimeout || bOrdinaryBound > testTaskRecoveryAppTimeout {
		t.Fatalf("B ordinary bound=%s, want >%s and <=%s", bOrdinaryBound, testTaskRecoveryFirstRouteTimeout, testTaskRecoveryAppTimeout)
	}
	joinedLogs := strings.Join(logs, "\n")
	routeCandidates := make(map[string]struct{})
	for _, line := range logs {
		if !strings.Contains(line, "stage=route_recovery_complete") {
			continue
		}
		for _, candidate := range []string{"a", "b"} {
			if strings.Contains(line, "candidate="+candidate) {
				routeCandidates[candidate] = struct{}{}
			}
		}
	}
	for _, candidate := range []string{"a", "b"} {
		if _, present := routeCandidates[candidate]; !present {
			t.Fatalf("route telemetry missing candidate %s:\n%s", candidate, joinedLogs)
		}
	}
}

func TestTaskRecoveryRunnerRouteTelemetryUsesFreshAttemptTruth(t *testing.T) {
	tests := []struct {
		name              string
		qualification     *taskRecoveryQualification
		ordinaryRoute     bool
		wantQualification string
		wantRouteStage    bool
	}{
		{
			name: "route becomes listenerless", ordinaryRoute: false,
			qualification: &taskRecoveryQualification{
				Timeout: testTaskRecoveryFirstRouteTimeout,
				Attempt: func(context.Context) (bool, error) { return false, nil },
			},
			wantQualification: "mixed_health_selector_changed",
		},
		{
			name: "listenerless becomes published route", ordinaryRoute: true,
			wantQualification: "listenerless_no_cohort", wantRouteStage: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := newTaskRecoveryControllerWithDeps(
				validTaskRecoveryControllerMarker(), "invocation-route-shape", &fakeTaskRecoveryControllerMarkerIO{},
				&fakeTaskRecoveryControllerClock{now: time.Unix(950, 0)},
			)
			calls := make([]string, 0, 2)
			logs := make([]string, 0, 8)
			runtime := &fakeTaskRecoveryRuntime{
				snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal}, ready: true,
			}
			runtime.enumerate = func(int) []taskRecoveryOwner {
				return []taskRecoveryOwner{{
					Name: "app:shape", AppID: "shape", Timeout: testTaskRecoveryAppTimeout,
					Attempt: func(context.Context) (bool, error) {
						calls = append(calls, "ordinary")
						return true, nil
					},
					AttemptDetailed: func(context.Context) taskRecoveryOwnerAttemptResult {
						calls = append(calls, "ordinary")
						return taskRecoveryOwnerAttemptResult{
							Active: true, RouteKnown: true, RouteActive: test.ordinaryRoute,
						}
					},
					RouteQualification: func() *taskRecoveryQualification {
						if test.qualification == nil {
							return nil
						}
						return &taskRecoveryQualification{
							Timeout: test.qualification.Timeout,
							Attempt: func(ctx context.Context) (bool, error) {
								calls = append(calls, "qualification")
								return test.qualification.Attempt(ctx)
							},
						}
					}(),
				}}
			}
			runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
				controller: controller, runtime: runtime, pollInterval: time.Millisecond,
				requestFatal:     func(string) bool { t.Fatal("unexpected fatal request"); return false },
				requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
				logf:             func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
			})
			runTaskRecoveryRunnerTest(t, runner, context.Background())
			if test.qualification != nil {
				if want := []string{"qualification", "ordinary"}; !reflect.DeepEqual(calls, want) {
					t.Fatalf("calls=%v, want %v", calls, want)
				}
			} else if want := []string{"ordinary"}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%v, want %v", calls, want)
			}
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, "outcome="+test.wantQualification) {
				t.Fatalf("qualification outcome missing:\n%s", joined)
			}
			gotRouteStage := strings.Contains(joined, "stage=route_recovery_complete")
			if gotRouteStage != test.wantRouteStage {
				t.Fatalf("route stage=%v, want %v:\n%s", gotRouteStage, test.wantRouteStage, joined)
			}
			if !strings.Contains(joined, "stage=eventual_convergence_complete") {
				t.Fatalf("eventual convergence missing:\n%s", joined)
			}
		})
	}
}

func TestTaskRecoveryRunnerPeriodicRefreshPreservesInitialQualificationClosure(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(1000, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:a", Strike: 2}}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-qualification-refresh", &fakeTaskRecoveryControllerMarkerIO{}, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var callsMu sync.Mutex
	calls := make([]string, 0, 1)
	initial := taskRecoveryOwner{
		Name:    "app:a",
		AppID:   "a",
		Timeout: testTaskRecoveryAppTimeout,
		Attempt: func(context.Context) (bool, error) {
			callsMu.Lock()
			calls = append(calls, "initial-ordinary")
			callsMu.Unlock()
			return true, nil
		},
		RouteQualification: &taskRecoveryQualification{
			Timeout: testTaskRecoveryFirstRouteTimeout,
			Attempt: func(context.Context) (bool, error) {
				callsMu.Lock()
				calls = append(calls, "initial-qualification")
				callsMu.Unlock()
				cancel()
				return true, nil
			},
		},
	}
	refreshed := taskRecoveryOwner{
		Name:    "app:a",
		AppID:   "a",
		Timeout: testTaskRecoveryAppTimeout,
		Attempt: func(context.Context) (bool, error) {
			callsMu.Lock()
			calls = append(calls, "refreshed-ordinary")
			callsMu.Unlock()
			return true, nil
		},
		RouteQualification: &taskRecoveryQualification{
			Timeout: testTaskRecoveryFirstRouteTimeout,
			Attempt: func(context.Context) (bool, error) {
				callsMu.Lock()
				calls = append(calls, "refreshed-qualification")
				callsMu.Unlock()
				return true, nil
			},
		},
	}
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
	}
	runtime.enumerate = func(index int) []taskRecoveryOwner {
		if index == 0 {
			return []taskRecoveryOwner{initial}
		}
		if index == 1 {
			clock.Advance(taskRecoveryBackoff(2))
		}
		return []taskRecoveryOwner{refreshed}
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:       controller,
		runtime:          runtime,
		initialDesired:   []string{"app:a"},
		pollInterval:     time.Millisecond,
		refreshInterval:  time.Millisecond,
		requestFatal:     func(string) bool { t.Error("unexpected fatal request"); return false },
		requestUncertain: func() bool { t.Error("unexpected uncertain request"); return false },
		logf:             func(string, ...any) {},
	})
	runTaskRecoveryRunnerTest(t, runner, ctx)

	runtime.mu.Lock()
	enumerations := runtime.enumerations
	runtime.mu.Unlock()
	if enumerations < 2 {
		t.Fatalf("enumerations=%d, want periodic refresh before attempt", enumerations)
	}
	callsMu.Lock()
	gotCalls := append([]string(nil), calls...)
	callsMu.Unlock()
	if want := []string{"initial-qualification"}; !reflect.DeepEqual(gotCalls, want) {
		t.Fatalf("calls=%v, want %v", gotCalls, want)
	}
}

func runTaskRecoveryRunnerTest(t *testing.T, runner *taskRecoveryRunner, ctx context.Context) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task recovery runner did not finish")
	}
}

func TestTaskRecoveryRunnerInitialNoCohortDoesNotQualifyLaterRoute(t *testing.T) {
	clock := &fakeTaskRecoveryControllerClock{now: time.Unix(1100, 0)}
	marker := validTaskRecoveryControllerMarker()
	marker.Suspects = []taskRecoverySuspect{{Owner: "app:listenerless", Strike: 2}}
	controller := newTaskRecoveryControllerWithDeps(marker, "invocation-no-qualification-cohort", &fakeTaskRecoveryControllerMarkerIO{}, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var callsMu sync.Mutex
	calls := make([]string, 0, 1)
	var routeOrdinaryBound time.Duration
	runtime := &fakeTaskRecoveryRuntime{
		snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
		ready:    true,
	}
	runtime.enumerate = func(index int) []taskRecoveryOwner {
		listenerless := taskRecoveryOwner{
			Name:    "app:listenerless",
			AppID:   "listenerless",
			Timeout: testTaskRecoveryAppTimeout,
			Attempt: func(context.Context) (bool, error) {
				callsMu.Lock()
				calls = append(calls, "listenerless-ordinary")
				callsMu.Unlock()
				return true, nil
			},
		}
		if index == 0 {
			return []taskRecoveryOwner{listenerless}
		}
		route := taskRecoveryOwner{
			Name:    "app:route",
			AppID:   "route",
			Timeout: testTaskRecoveryAppTimeout,
			Attempt: func(ctx context.Context) (bool, error) {
				deadline, ok := ctx.Deadline()
				if ok {
					routeOrdinaryBound = time.Until(deadline)
				}
				callsMu.Lock()
				calls = append(calls, "route-ordinary")
				callsMu.Unlock()
				cancel()
				return true, nil
			},
			RouteQualification: &taskRecoveryQualification{
				Timeout: testTaskRecoveryFirstRouteTimeout,
				Attempt: func(context.Context) (bool, error) {
					callsMu.Lock()
					calls = append(calls, "route-qualification")
					callsMu.Unlock()
					return true, nil
				},
			},
		}
		return []taskRecoveryOwner{route, listenerless}
	}
	runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
		controller:       controller,
		runtime:          runtime,
		initialDesired:   []string{"app:listenerless"},
		pollInterval:     time.Millisecond,
		refreshInterval:  time.Millisecond,
		requestFatal:     func(string) bool { t.Error("unexpected fatal request"); return false },
		requestUncertain: func() bool { t.Error("unexpected uncertain request"); return false },
		logf:             func(string, ...any) {},
	})
	runTaskRecoveryRunnerTest(t, runner, ctx)

	callsMu.Lock()
	gotCalls := append([]string(nil), calls...)
	callsMu.Unlock()
	if want := []string{"route-ordinary"}; !reflect.DeepEqual(gotCalls, want) {
		t.Fatalf("calls=%v, want %v", gotCalls, want)
	}
	if routeOrdinaryBound <= testTaskRecoveryFirstRouteTimeout || routeOrdinaryBound > testTaskRecoveryAppTimeout {
		t.Fatalf("later route ordinary bound=%s, want >%s and <=%s", routeOrdinaryBound, testTaskRecoveryFirstRouteTimeout, testTaskRecoveryAppTimeout)
	}
}

func TestTaskRecoveryRunnerEmitsCorrelatedQualificationStages(t *testing.T) {
	marker := validTaskRecoveryControllerMarker()
	marker.Timestamp = time.Unix(1200, 0).UTC()
	marker.DetectionAt = time.Unix(1198, 0).UTC()
	marker.LastFailedInvocationID = "failed-invocation"
	marker.ContinuityOutcome = string(autounlock.PrepareDispositionPrepared)
	taskCurrent, taskLimit := int64(2200), int64(2311)
	marker.TaskCurrent, marker.TaskLimit = &taskCurrent, &taskLimit
	controller := newTaskRecoveryControllerWithDeps(marker, "replacement-invocation", &fakeTaskRecoveryControllerMarkerIO{}, &fakeTaskRecoveryControllerClock{now: time.Unix(1210, 0)})

	t.Run("eligible route", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logs := make([]string, 0, 4)
		runtime := &fakeTaskRecoveryRuntime{snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal}, ready: true}
		runtime.enumerate = func(int) []taskRecoveryOwner {
			return []taskRecoveryOwner{{
				Name: "app:alpha", AppID: "alpha", Timeout: time.Second,
				Attempt: func(context.Context) (bool, error) { return true, nil },
				AttemptDetailed: func(context.Context) taskRecoveryOwnerAttemptResult {
					return taskRecoveryOwnerAttemptResult{Active: true, RouteKnown: true, RouteActive: true}
				},
				RouteQualification: &taskRecoveryQualification{Timeout: time.Second, Attempt: func(context.Context) (bool, error) {
					return true, nil
				}},
			}}
		}
		now := time.Unix(1220, 0).UTC()
		runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
			controller: controller, runtime: runtime, processStartedAt: time.Unix(1215, 0).UTC(), pollInterval: time.Millisecond,
			now:              func() time.Time { now = now.Add(time.Second); return now },
			requestFatal:     func(string) bool { t.Fatal("unexpected fatal request"); return false },
			requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
			logf: func(format string, args ...any) {
				message := fmt.Sprintf(format, args...)
				logs = append(logs, message)
				if strings.Contains(message, "outcome=eligible_pass") {
					cancel()
				}
			},
		})
		runTaskRecoveryRunnerTest(t, runner, ctx)
		joined := strings.Join(logs, "\n")
		for _, want := range []string{
			"stage=core_ready", "stage=lifecycle_ready",
			"stage=first_route_qualification_complete", "outcome=eligible_pass", "candidate=alpha",
			"stage=route_recovery_complete",
			"detection_at=1970-01-01T00:19:58Z", "marker_at=1970-01-01T00:20:00Z",
			"process_started_at=1970-01-01T00:20:15Z", "failed_invocation=failed-invocation",
			"cohort=task_first_generation", "continuity_outcome=prepared", "task_current=2200", "task_limit=2311",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("telemetry missing %q:\n%s", want, joined)
			}
		}
	})
}

func TestTaskRecoveryRunnerEmitsListenerlessAndMixedQualificationOutcomes(t *testing.T) {
	tests := []struct {
		name          string
		qualification *taskRecoveryQualification
		wantOutcome   string
	}{
		{name: "listenerless", wantOutcome: "listenerless_no_cohort"},
		{name: "selector changed", qualification: &taskRecoveryQualification{Timeout: time.Second, Attempt: func(context.Context) (bool, error) { return false, nil }}, wantOutcome: "mixed_health_selector_changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marker := validTaskRecoveryControllerMarker()
			controller := newTaskRecoveryControllerWithDeps(marker, "replacement", &fakeTaskRecoveryControllerMarkerIO{}, &fakeTaskRecoveryControllerClock{now: time.Unix(1300, 0)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := make([]string, 0, 2)
			runtime := &fakeTaskRecoveryRuntime{snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal}, ready: true}
			runtime.enumerate = func(int) []taskRecoveryOwner {
				qualification := test.qualification
				if qualification != nil {
					qualification = &taskRecoveryQualification{Timeout: qualification.Timeout, Attempt: func(ctx context.Context) (bool, error) {
						defer cancel()
						return test.qualification.Attempt(ctx)
					}}
				}
				return []taskRecoveryOwner{{
					Name: "app:alpha", AppID: "alpha", Timeout: time.Second,
					Attempt:            func(context.Context) (bool, error) { cancel(); return true, nil },
					RouteQualification: qualification,
				}}
			}
			runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
				controller: controller, runtime: runtime, pollInterval: time.Millisecond,
				requestFatal:     func(string) bool { t.Fatal("unexpected fatal request"); return false },
				requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
				logf:             func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
			})
			runTaskRecoveryRunnerTest(t, runner, ctx)
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, "outcome="+test.wantOutcome) {
				t.Fatalf("telemetry missing outcome %q:\n%s", test.wantOutcome, joined)
			}
			if test.name == "listenerless" && strings.Contains(joined, "stage=route_recovery_complete") {
				t.Fatalf("listenerless recovery was counted as a route:\n%s", joined)
			}
		})
	}
}

func TestTaskRecoveryRunnerTelemetryRecordsReadyUnlockSkipAndConvergence(t *testing.T) {
	t.Run("ready unlock fast path", func(t *testing.T) {
		controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "ready-unlock", &fakeTaskRecoveryControllerMarkerIO{}, &fakeTaskRecoveryControllerClock{now: time.Unix(1400, 0)})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logs := make([]string, 0, 4)
		runtime := &fakeTaskRecoveryRuntime{
			snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal},
			ready:    true,
			core: []taskRecoveryOwner{{
				Name: taskRecoveryUnlockChainOwner, Timeout: time.Second,
				Attempt: func(context.Context) (bool, error) {
					t.Fatal("ready lifecycle unexpectedly invoked unlock pickup")
					return false, nil
				},
			}},
		}
		runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
			controller: controller, runtime: runtime, pollInterval: time.Millisecond,
			requestFatal:     func(string) bool { t.Fatal("unexpected fatal request"); return false },
			requestUncertain: func() bool { t.Fatal("unexpected uncertain request"); return false },
			logf: func(format string, args ...any) {
				message := fmt.Sprintf(format, args...)
				logs = append(logs, message)
				if strings.Contains(message, "stage=unlock_pickup_skipped") {
					cancel()
				}
			},
		})
		runTaskRecoveryRunnerTest(t, runner, ctx)
		joined := strings.Join(logs, "\n")
		if !strings.Contains(joined, "stage=unlock_pickup_skipped") || !strings.Contains(joined, "outcome=lifecycle_already_ready") || strings.Contains(joined, "stage=unlock_pickup_started") {
			t.Fatalf("unlock fast-path telemetry is not truthful:\n%s", joined)
		}
	})

	t.Run("eventual convergence", func(t *testing.T) {
		controller := newTaskRecoveryControllerWithDeps(validTaskRecoveryControllerMarker(), "complete", &fakeTaskRecoveryControllerMarkerIO{}, &fakeTaskRecoveryControllerClock{now: time.Unix(1450, 0)})
		controller.mu.Lock()
		controller.markerRemoved = true
		controller.mu.Unlock()
		logs := make([]string, 0, 2)
		runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
			controller: controller,
			runtime:    &fakeTaskRecoveryRuntime{snapshot: pressure.TaskSnapshot{State: pressure.TaskPressureNormal}, ready: true},
			logf:       func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		})
		runTaskRecoveryRunnerTest(t, runner, context.Background())
		joined := strings.Join(logs, "\n")
		if !strings.Contains(joined, "stage=eventual_convergence_complete") || !strings.Contains(joined, "outcome=complete") {
			t.Fatalf("eventual convergence telemetry missing:\n%s", joined)
		}
	})
}

func TestTaskRecoveryTelemetryCohortDoesNotMislabelExistingHandoff(t *testing.T) {
	tests := []struct {
		name       string
		marker     taskRecoveryMarker
		wantCohort string
	}{
		{name: "unexpected without handoff", marker: taskRecoveryMarker{ReasonCode: "service_failure", Generation: 1, ContinuityOutcome: "no_handoff"}, wantCohort: "unexpected_no_handoff"},
		{name: "unexpected with handoff", marker: taskRecoveryMarker{ReasonCode: "service_failure", Generation: 1, ContinuityOutcome: "preexisting_handoff"}, wantCohort: "unexpected_with_handoff"},
		{name: "legacy outcome unknown", marker: taskRecoveryMarker{ReasonCode: "service_failure", Generation: 1}, wantCohort: "unexpected_handoff_unknown"},
		{name: "recurrence wins", marker: taskRecoveryMarker{ReasonCode: "service_failure", Generation: 2, ContinuityOutcome: "no_handoff"}, wantCohort: "recurrence_containment"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := taskRecoveryTelemetryCohort(test.marker); got != test.wantCohort {
				t.Fatalf("cohort=%q, want %q", got, test.wantCohort)
			}
		})
	}
}
