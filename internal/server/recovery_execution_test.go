package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/health"
	"piccolod/internal/lifecycle"
)

type controlledUnlockTimer struct {
	stopped atomic.Bool
	f       func()
}

func (t *controlledUnlockTimer) Stop() bool {
	return !t.stopped.Swap(true)
}

func (t *controlledUnlockTimer) fire() {
	// Invoke even after Stop so the tests exercise the coordinator's atomic
	// decision rather than relying on timer cancellation for correctness.
	t.f()
}

func testUnlockCoordinator(
	t *testing.T,
	body func(context.Context) (unlockChainResult, error),
	onReady func(),
	onFatal func(),
) (*unlockExecutionCoordinator, *lifecycle.Coordinator) {
	t.Helper()
	lc := lifecycle.New(lifecycle.StateLocked)
	coord := newUnlockExecutionCoordinator(unlockExecutionConfig{
		lifecycle: lc,
		body:      body,
		onReady:   onReady,
		onFatal:   onFatal,
		liveness:  time.Hour,
		afterFunc: func(_ time.Duration, f func()) unlockExecutionTimer {
			return &controlledUnlockTimer{f: f}
		},
	})
	return coord, lc
}

func activeUnlockAttempt(t *testing.T, coord *unlockExecutionCoordinator) *unlockExecutionAttempt {
	t.Helper()
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if coord.active == nil {
		t.Fatal("unlock execution has no active attempt")
	}
	return coord.active
}

func TestUnlockExecutionExactlyOneBodyForAutomaticJoiners(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	coord, lc := testUnlockCoordinator(t, func(context.Context) (unlockChainResult, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return unlockChainResult{setupComplete: true}, nil
	}, nil, nil)

	first := make(chan error, 1)
	go func() {
		_, err := coord.execute(context.Background(), unlockCallerAutomatic)
		first <- err
	}()
	<-started

	second := make(chan error, 1)
	go func() {
		_, err := coord.execute(context.Background(), unlockCallerAutomatic)
		second <- err
	}()
	close(release)

	if err := <-first; err != nil {
		t.Fatalf("first automatic caller: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("joined automatic caller: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("complete-unlock body calls = %d, want 1", got)
	}
	if got := lc.State(); got != lifecycle.StateReady {
		t.Fatalf("lifecycle = %s, want ready", got)
	}
}

func TestUnlockExecutionCallerTimeoutDoesNotReleaseBodyOwnership(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	coord, lc := testUnlockCoordinator(t, func(context.Context) (unlockChainResult, error) {
		calls.Add(1)
		close(started)
		<-release
		return unlockChainResult{setupComplete: true}, nil
	}, nil, nil)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := coord.execute(callerCtx, unlockCallerAutomatic)
		returned <- err
	}()
	<-started
	cancelCaller()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller error = %v, want context canceled", err)
	}
	if got := lc.State(); got != lifecycle.StateUnlocking {
		t.Fatalf("lifecycle after caller timeout = %s, want unlocking", got)
	}

	attempt := activeUnlockAttempt(t, coord)
	close(release)
	<-attempt.bodyReturned
	if got := calls.Load(); got != 1 {
		t.Fatalf("body calls after caller timeout = %d, want 1", got)
	}
	if got := lc.State(); got != lifecycle.StateReady {
		t.Fatalf("lifecycle after continuing body = %s, want ready", got)
	}
}

func TestUnlockExecutionCompletionWinsOverLiveness(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var readyCalls atomic.Int32
	var fatalCalls atomic.Int32
	coord, lc := testUnlockCoordinator(t, func(context.Context) (unlockChainResult, error) {
		close(started)
		<-release
		return unlockChainResult{setupComplete: true}, nil
	}, func() { readyCalls.Add(1) }, func() { fatalCalls.Add(1) })

	returned := make(chan error, 1)
	go func() {
		_, err := coord.execute(context.Background(), unlockCallerAutomatic)
		returned <- err
	}()
	<-started
	attempt := activeUnlockAttempt(t, coord)
	timer := attempt.timer.(*controlledUnlockTimer)
	close(release)
	if err := <-returned; err != nil {
		t.Fatalf("completion result: %v", err)
	}
	<-attempt.bodyReturned
	timer.fire()
	if got := fatalCalls.Load(); got != 0 {
		t.Fatalf("fatal callback calls = %d, want 0", got)
	}
	if got := readyCalls.Load(); got != 1 {
		t.Fatalf("Ready callback calls = %d, want 1", got)
	}
	if got := lc.State(); got != lifecycle.StateReady {
		t.Fatalf("lifecycle = %s, want ready", got)
	}
}

func TestUnlockExecutionFatalWinsAndDiscardsLateReturn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var readyCalls atomic.Int32
	var fatalCalls atomic.Int32
	coord, lc := testUnlockCoordinator(t, func(context.Context) (unlockChainResult, error) {
		close(started)
		// Intentionally ignore cancellation to model the liveness failure.
		<-release
		return unlockChainResult{setupComplete: true}, nil
	}, func() { readyCalls.Add(1) }, func() { fatalCalls.Add(1) })

	returned := make(chan error, 1)
	go func() {
		_, err := coord.execute(context.Background(), unlockCallerAutomatic)
		returned <- err
	}()
	<-started
	attempt := activeUnlockAttempt(t, coord)
	timer := attempt.timer.(*controlledUnlockTimer)
	timer.fire()
	if err := <-returned; !errors.Is(err, errUnlockExecutionFatalCommit) {
		t.Fatalf("fatal result = %v", err)
	}
	timer.fire()
	if got := fatalCalls.Load(); got != 1 {
		t.Fatalf("fatal callback calls = %d, want exactly 1", got)
	}
	if got := lc.State(); got != lifecycle.StateUnlocking {
		t.Fatalf("lifecycle after fatal commit = %s, want unlocking", got)
	}

	close(release)
	<-attempt.bodyReturned
	if got := readyCalls.Load(); got != 0 {
		t.Fatalf("late body return invoked Ready callback %d times", got)
	}
	if got := lc.State(); got != lifecycle.StateUnlocking {
		t.Fatalf("late body return changed lifecycle to %s", got)
	}
}

func TestUnlockExecutionDeadlineCancellationReturnCannotBeatFatalCommit(t *testing.T) {
	started := make(chan struct{})
	var readyCalls atomic.Int32
	var fatalCalls atomic.Int32
	coord, lc := testUnlockCoordinator(t, func(ctx context.Context) (unlockChainResult, error) {
		close(started)
		<-ctx.Done()
		return unlockChainResult{setupComplete: true}, ctx.Err()
	}, func() { readyCalls.Add(1) }, func() { fatalCalls.Add(1) })

	returned := make(chan error, 1)
	go func() {
		_, err := coord.execute(context.Background(), unlockCallerAutomatic)
		returned <- err
	}()
	<-started
	attempt := activeUnlockAttempt(t, coord)
	attempt.timer.(*controlledUnlockTimer).fire()
	if err := <-returned; !errors.Is(err, errUnlockExecutionFatalCommit) {
		t.Fatalf("deadline boundary result = %v, want fatal commit", err)
	}
	<-attempt.bodyReturned
	if got := fatalCalls.Load(); got != 1 {
		t.Fatalf("fatal callback calls = %d, want 1", got)
	}
	if got := readyCalls.Load(); got != 0 {
		t.Fatalf("cancellation return invoked Ready callback %d times", got)
	}
	if got := lc.State(); got != lifecycle.StateUnlocking {
		t.Fatalf("deadline cancellation changed lifecycle to %s", got)
	}
}

func TestUnlockExecutionManualJoinReturnsRecoveryInProgress(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	coord, _ := testUnlockCoordinator(t, func(context.Context) (unlockChainResult, error) {
		calls.Add(1)
		close(started)
		<-release
		return unlockChainResult{}, nil
	}, nil, nil)

	first := make(chan error, 1)
	go func() {
		_, err := coord.execute(context.Background(), unlockCallerAutomatic)
		first <- err
	}()
	<-started
	_, err := coord.execute(context.Background(), unlockCallerManual)
	var inProgress *recoveryInProgressError
	if !errors.As(err, &inProgress) {
		t.Fatalf("manual join error = %v, want recoveryInProgressError", err)
	}
	if inProgress.Code() != errorCodeRecoveryInProgress {
		t.Fatalf("manual join code = %q", inProgress.Code())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("manual join launched %d bodies, want 1", got)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("owner result: %v", err)
	}
}

func TestUnlockReadyBarrierRequiresCoordinatorPostReadySignal(t *testing.T) {
	lc := lifecycle.New(lifecycle.StateLocked)
	ready := make(chan struct{})
	srv := &GinServer{lifecycle: lc, unlockReady: make(chan struct{})}
	started := make(chan struct{})
	go func() {
		close(started)
		if srv.waitForUnlockReady(context.Background()) {
			close(ready)
		}
	}()
	<-started
	if err := lc.BeginUnlock(); err != nil {
		t.Fatal(err)
	}
	if err := lc.MarkReady(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
		t.Fatal("lifecycle transition alone released optional owners")
	default:
	}
	srv.onUnlockChainReady()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("post-Ready signal did not release optional owners")
	}
}

func TestRawPersistenceUnlockDoesNotReachDecryptedOwners(t *testing.T) {
	mainBus := events.NewBus()
	decryptedBus := events.NewBus()
	lc := lifecycle.New(lifecycle.StateLocked)
	srv := &GinServer{
		lifecycle:       lc,
		healthTracker:   health.NewTracker(),
		decryptedEvents: decryptedBus,
		unlockReady:     make(chan struct{}),
	}
	srv.decryptedOwnersStarted.Store(true)
	srv.observeLockState(mainBus)
	filtered, cancel := decryptedBus.SubscribeWithCancel(events.TopicLockStateChanged, 1)
	defer cancel()

	mainBus.Publish(events.Event{
		Topic:   events.TopicLockStateChanged,
		Payload: events.LockStateChanged{Locked: false},
	})
	deadline := time.Now().Add(time.Second)
	rawObserved := false
	for time.Now().Before(deadline) {
		if st, ok := srv.healthTracker.Status("persistence"); ok && st.Message == "control store unlocked; recovery finalizing" {
			rawObserved = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !rawObserved {
		t.Fatal("server did not observe raw persistence unlock")
	}
	select {
	case evt := <-filtered:
		t.Fatalf("raw persistence unlock reached decrypted owners: %#v", evt)
	default:
	}

	if err := lc.BeginUnlock(); err != nil {
		t.Fatal(err)
	}
	if err := lc.MarkReady(); err != nil {
		t.Fatal(err)
	}
	srv.onUnlockChainReady()
	select {
	case evt := <-filtered:
		payload, ok := evt.Payload.(events.LockStateChanged)
		if !ok || payload.Locked {
			t.Fatalf("filtered Ready payload = %#v", evt.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle Ready did not reach decrypted owners")
	}
}

func readyLifecycle(t *testing.T) *lifecycle.Coordinator {
	t.Helper()
	lc := lifecycle.New(lifecycle.StateLocked)
	if err := lc.BeginUnlock(); err != nil {
		t.Fatal(err)
	}
	if err := lc.MarkReady(); err != nil {
		t.Fatal(err)
	}
	return lc
}

func TestRootfsMaintenanceStartsAfterReadyAndCoalescesNudges(t *testing.T) {
	lc := readyLifecycle(t)
	opCtx, opCancel := context.WithCancel(context.Background())
	defer opCancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	srv := &GinServer{
		lifecycle:                       lc,
		opCtx:                           opCtx,
		healthTracker:                   health.NewTracker(),
		rootfsMaintenanceRetryInterval:  time.Hour,
		rootfsMaintenanceAttemptTimeout: time.Second,
		rootfsMaintenancePass: func(ctx context.Context) error {
			if !lc.IsReady() {
				t.Error("physical maintenance started before lifecycle Ready")
			}
			call := calls.Add(1)
			current := concurrent.Add(1)
			defer concurrent.Add(-1)
			for {
				maximum := maxConcurrent.Load()
				if current <= maximum || maxConcurrent.CompareAndSwap(maximum, current) {
					break
				}
			}
			if call == 1 {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	}

	srv.onUnlockChainReady()
	<-started
	for range 8 {
		srv.onUnlockChainReady()
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent maintenance calls = %d, want 1", got)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("coalesced maintenance calls = %d, want 2", got)
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent maintenance passes = %d, want 1", got)
	}
	if err := srv.cancelAndJoinRootfsMaintenance(context.Background()); err != nil {
		t.Fatalf("cancelAndJoinRootfsMaintenance: %v", err)
	}
}

func TestRootfsMaintenanceRetryIntervalStartsAfterPassCompletion(t *testing.T) {
	lc := readyLifecycle(t)
	opCtx, opCancel := context.WithCancel(context.Background())
	defer opCancel()
	const retryInterval = 40 * time.Millisecond
	completed := make(chan time.Time, 1)
	retried := make(chan time.Time, 1)
	var calls atomic.Int32
	srv := &GinServer{
		lifecycle:                       lc,
		opCtx:                           opCtx,
		rootfsMaintenanceRetryInterval:  retryInterval,
		rootfsMaintenanceAttemptTimeout: time.Second,
		rootfsMaintenancePass: func(context.Context) error {
			if calls.Add(1) == 1 {
				time.Sleep(20 * time.Millisecond)
				completed <- time.Now()
			} else {
				retried <- time.Now()
			}
			return nil
		},
	}

	srv.onUnlockChainReady()
	firstCompleted := <-completed
	secondStarted := <-retried
	if elapsed := secondStarted.Sub(firstCompleted); elapsed < retryInterval-5*time.Millisecond {
		t.Fatalf("retry began %s after completion, want at least %s", elapsed, retryInterval)
	}
	if err := srv.cancelAndJoinRootfsMaintenance(context.Background()); err != nil {
		t.Fatalf("cancelAndJoinRootfsMaintenance: %v", err)
	}
}

func TestRootfsMaintenanceFailureWarnsWithoutReversingReady(t *testing.T) {
	lc := readyLifecycle(t)
	opCtx, opCancel := context.WithCancel(context.Background())
	defer opCancel()
	attempted := make(chan struct{})
	srv := &GinServer{
		lifecycle:                       lc,
		opCtx:                           opCtx,
		healthTracker:                   health.NewTracker(),
		rootfsMaintenanceRetryInterval:  time.Hour,
		rootfsMaintenanceAttemptTimeout: time.Second,
		rootfsMaintenancePass: func(context.Context) error {
			close(attempted)
			return errors.New("injected maintenance failure")
		},
	}

	srv.onUnlockChainReady()
	<-attempted
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if status, ok := srv.healthTracker.Status("rootfs-maintenance"); ok {
			if status.Level != health.LevelWarn {
				t.Fatalf("maintenance health = %s, want warn", status.Level)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := lc.State(); got != lifecycle.StateReady {
		t.Fatalf("lifecycle after maintenance failure = %s, want ready", got)
	}
	if err := srv.cancelAndJoinRootfsMaintenance(context.Background()); err != nil {
		t.Fatalf("cancelAndJoinRootfsMaintenance: %v", err)
	}
}

func TestRootfsMaintenanceCancellationJoinsBusyPass(t *testing.T) {
	lc := readyLifecycle(t)
	opCtx, opCancel := context.WithCancel(context.Background())
	defer opCancel()
	started := make(chan struct{})
	returned := make(chan struct{})
	srv := &GinServer{
		lifecycle:                       lc,
		opCtx:                           opCtx,
		rootfsMaintenanceRetryInterval:  time.Hour,
		rootfsMaintenanceAttemptTimeout: time.Minute,
		rootfsMaintenancePass: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(returned)
			return ctx.Err()
		},
	}

	srv.onUnlockChainReady()
	<-started
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := srv.cancelAndJoinRootfsMaintenance(joinCtx); err != nil {
		t.Fatalf("cancelAndJoinRootfsMaintenance: %v", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("maintenance join returned before the active pass exited")
	}
}

func TestRootfsMaintenanceDoesNotStartAfterOperationContextCancellation(t *testing.T) {
	opCtx, opCancel := context.WithCancel(context.Background())
	opCancel()
	var calls atomic.Int32
	srv := &GinServer{
		lifecycle: readyLifecycle(t),
		opCtx:     opCtx,
		rootfsMaintenancePass: func(context.Context) error {
			calls.Add(1)
			return nil
		},
	}

	srv.nudgeRootfsMaintenance()

	srv.rootfsMaintenanceMu.Lock()
	done := srv.rootfsMaintenanceDone
	srv.rootfsMaintenanceMu.Unlock()
	if done != nil {
		t.Fatal("maintenance worker was admitted after operation context cancellation")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("maintenance calls = %d, want 0", got)
	}
}
