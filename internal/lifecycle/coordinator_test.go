package lifecycle

import (
	"errors"
	"sync"
	"testing"
)

func TestState_String(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{StateNotInitialized, "not_initialized"},
		{StateLocked, "locked"},
		{StateUnlocking, "unlocking"},
		{StateReady, "ready"},
		{StateFailed, "failed"},
		{State(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestNew_Bootstrap(t *testing.T) {
	for _, initial := range []State{StateNotInitialized, StateLocked, StateReady} {
		c := New(initial)
		if c.State() != initial {
			t.Errorf("New(%s).State() = %s, want %s", initial, c.State(), initial)
		}
	}
}

func TestNilCoordinator_StateAndIsReady(t *testing.T) {
	var c *Coordinator
	if c.State() != StateNotInitialized {
		t.Errorf("nil.State() = %s, want %s", c.State(), StateNotInitialized)
	}
	if c.IsReady() {
		t.Error("nil.IsReady() = true, want false")
	}
}

func TestBeginUnlock_FromLegalSources(t *testing.T) {
	for _, initial := range []State{StateLocked, StateNotInitialized, StateFailed} {
		c := New(initial)
		if initial == StateFailed {
			// Get into StateFailed via the legal path so failure reason is set.
			c = New(StateLocked)
			if err := c.BeginUnlock(); err != nil {
				t.Fatalf("setup: BeginUnlock from Locked: %v", err)
			}
			if err := c.MarkFailed(errors.New("seed failure")); err != nil {
				t.Fatalf("setup: MarkFailed: %v", err)
			}
		}
		if err := c.BeginUnlock(); err != nil {
			t.Errorf("BeginUnlock from %s: %v", initial, err)
		}
		if c.State() != StateUnlocking {
			t.Errorf("after BeginUnlock from %s: state = %s, want %s", initial, c.State(), StateUnlocking)
		}
		if c.FailureReason() != nil {
			t.Errorf("after BeginUnlock from %s: FailureReason = %v, want nil", initial, c.FailureReason())
		}
	}
}

func TestBeginUnlock_FromIllegalSources(t *testing.T) {
	for _, initial := range []State{StateUnlocking, StateReady} {
		c := New(initial)
		err := c.BeginUnlock()
		if err == nil {
			t.Errorf("BeginUnlock from %s: want error, got nil", initial)
			continue
		}
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("BeginUnlock from %s: want ErrInvalidTransition, got %v", initial, err)
		}
		if c.State() != initial {
			t.Errorf("BeginUnlock from %s rejected but state = %s, want %s", initial, c.State(), initial)
		}
	}
}

func TestMarkReady_FromUnlocking(t *testing.T) {
	c := New(StateLocked)
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	if err := c.MarkReady(); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if c.State() != StateReady {
		t.Errorf("state after MarkReady = %s, want %s", c.State(), StateReady)
	}
	if !c.IsReady() {
		t.Error("IsReady() = false after MarkReady")
	}
}

func TestMarkReady_FromIllegalSources(t *testing.T) {
	for _, initial := range []State{StateNotInitialized, StateLocked, StateReady} {
		c := New(initial)
		if err := c.MarkReady(); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("MarkReady from %s: want ErrInvalidTransition, got %v", initial, err)
		}
		if c.State() != initial {
			t.Errorf("MarkReady from %s rejected but state = %s", initial, c.State())
		}
	}
}

func TestMarkFailed_FromUnlocking(t *testing.T) {
	c := New(StateLocked)
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	reason := errors.New("luks data volume failed")
	if err := c.MarkFailed(reason); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if c.State() != StateFailed {
		t.Errorf("state = %s, want %s", c.State(), StateFailed)
	}
	if got := c.FailureReason(); got != reason {
		t.Errorf("FailureReason = %v, want %v", got, reason)
	}
}

func TestMarkFailed_NilReasonReplaced(t *testing.T) {
	c := New(StateLocked)
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	if err := c.MarkFailed(nil); err != nil {
		t.Fatalf("MarkFailed(nil): %v", err)
	}
	if got := c.FailureReason(); got == nil {
		t.Error("FailureReason() = nil after MarkFailed(nil); want sentinel")
	}
}

func TestFailureReason_NilOutsideFailedState(t *testing.T) {
	c := New(StateLocked)
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	if err := c.MarkFailed(errors.New("boom")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	// Recover via BeginUnlock — the prior reason must clear.
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock retry: %v", err)
	}
	if got := c.FailureReason(); got != nil {
		t.Errorf("FailureReason after retry BeginUnlock = %v, want nil", got)
	}
}

func TestLock_FromTerminalStates(t *testing.T) {
	cases := []State{StateReady, StateFailed}
	for _, initial := range cases {
		c := New(StateLocked)
		if err := c.BeginUnlock(); err != nil {
			t.Fatalf("setup BeginUnlock for %s: %v", initial, err)
		}
		switch initial {
		case StateReady:
			if err := c.MarkReady(); err != nil {
				t.Fatalf("setup MarkReady: %v", err)
			}
		case StateFailed:
			if err := c.MarkFailed(errors.New("seed")); err != nil {
				t.Fatalf("setup MarkFailed: %v", err)
			}
		}
		if err := c.Lock(); err != nil {
			t.Errorf("Lock from %s: %v", initial, err)
		}
		if c.State() != StateLocked {
			t.Errorf("after Lock from %s: state = %s, want %s", initial, c.State(), StateLocked)
		}
		if c.FailureReason() != nil {
			t.Errorf("after Lock from %s: FailureReason = %v, want nil", initial, c.FailureReason())
		}
	}
}

func TestLock_IdempotentOnLocked(t *testing.T) {
	c := New(StateLocked)
	if err := c.Lock(); err != nil {
		t.Errorf("Lock from Locked: %v", err)
	}
	if c.State() != StateLocked {
		t.Errorf("state = %s, want %s", c.State(), StateLocked)
	}
}

func TestLock_RejectedFromTransientStates(t *testing.T) {
	for _, initial := range []State{StateNotInitialized} {
		c := New(initial)
		if err := c.Lock(); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("Lock from %s: want ErrInvalidTransition, got %v", initial, err)
		}
	}
	// Unlocking must reject Lock — caller must wait for terminal state.
	c := New(StateLocked)
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	if err := c.Lock(); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Lock from Unlocking: want ErrInvalidTransition, got %v", err)
	}
}

// TestRaceWindow_NoExposure asserts the property that motivates the package:
// once the coordinator transitions to Unlocking, IsReady stays false until a
// terminal MarkReady. No external observer ever sees a state where IsReady
// claims true while the chain is still running.
func TestRaceWindow_NoExposure(t *testing.T) {
	c := New(StateLocked)
	if c.IsReady() {
		t.Fatal("precondition: Locked.IsReady() = true")
	}
	if err := c.BeginUnlock(); err != nil {
		t.Fatalf("BeginUnlock: %v", err)
	}
	if c.IsReady() {
		t.Error("Unlocking.IsReady() = true; should be false")
	}
	if err := c.MarkReady(); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	if !c.IsReady() {
		t.Error("Ready.IsReady() = false; should be true")
	}
}

// TestConcurrent_BeginUnlockSerialization asserts that under concurrent
// BeginUnlock attempts (manual + auto-unlock racing at boot), exactly one
// caller wins and the others observe ErrInvalidTransition. This is the
// race-claim semantics handleCryptoUnlock and RunPickup rely on.
func TestConcurrent_BeginUnlockSerialization(t *testing.T) {
	c := New(StateLocked)
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			results <- c.BeginUnlock()
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	losses := 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrInvalidTransition) {
			losses++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1", wins)
	}
	if losses != goroutines-1 {
		t.Errorf("losses = %d, want %d", losses, goroutines-1)
	}
}

// TestConcurrent_StateReadsLockFree exercises the lock-free read path while
// transitions cycle. Passes silently if `go test -race` emits no warnings;
// the assertion is the race detector itself.
func TestConcurrent_StateReadsLockFree(t *testing.T) {
	c := New(StateLocked)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.State()
				_ = c.IsReady()
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		_ = c.BeginUnlock()
		_ = c.MarkReady()
		_ = c.Lock()
	}
	close(stop)
	wg.Wait()
}
