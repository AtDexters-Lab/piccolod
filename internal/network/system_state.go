package network

import (
	"context"
	"sync"

	"piccolod/internal/events"
)

// SystemState reports whether the system is in a state where bouncing wifi
// or rebooting could disrupt an irrecoverable in-flight operation. It returns
// true iff onboarding phase ∈ {pending, install_disk}.
//
// Other "busy" signals (transactional-update mid-flight, LVM ops) are
// restart-tolerant by design — bouncing wifi during a TU fetch at worst
// causes a TCP retry. We don't gate on them.
type SystemState interface {
	// O(1) in-memory read.
	SystemBusy() (busy bool, reason string)
}

// busSystemState subscribes to TopicOnboardingStateChanged and caches the
// latest phase under a mutex.
type busSystemState struct {
	mu     sync.RWMutex
	phase  string // "" | "pending" | "install_disk" | "complete" | "try_piccolo" | ...
	cancel func()
}

// NewBusSystemState constructs a SystemState backed by the event bus.
// It immediately subscribes to TopicOnboardingStateChanged and spawns a
// goroutine to watch for transitions. The goroutine exits when ctx is done.
//
// Note: callers must publish the current onboarding phase as an initial
// event after subscribers are wired (or call SetInitialPhase) to avoid a
// brief window where SystemBusy returns false until the first event arrives.
// The orchestrator calls this with a sane default at construction; the bus
// then drives state transitions.
func NewBusSystemState(ctx context.Context, bus *events.Bus) SystemState {
	s := &busSystemState{}
	if bus == nil {
		return s
	}
	ch, cancel := bus.SubscribeWithCancel(events.TopicOnboardingStateChanged, 4)
	s.cancel = cancel

	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-ch:
				if !ok {
					return
				}
				if p, ok := evt.Payload.(events.OnboardingStateChangedEvent); ok {
					s.mu.Lock()
					s.phase = p.State
					s.mu.Unlock()
				}
			}
		}
	}()

	return s
}

// SetInitialPhase seeds the cached onboarding phase. Useful for tests and for
// the orchestrator startup path where the current phase is known synchronously
// from disk before the event bus carries any transition.
func (s *busSystemState) SetInitialPhase(phase string) {
	s.mu.Lock()
	s.phase = phase
	s.mu.Unlock()
}

func (s *busSystemState) SystemBusy() (busy bool, reason string) {
	s.mu.RLock()
	p := s.phase
	s.mu.RUnlock()
	switch p {
	case "pending", "install_disk":
		return true, "onboarding:" + p
	default:
		return false, ""
	}
}

// staticSystemState is a test/fallback impl that always reports the same value.
type staticSystemState struct {
	busy   bool
	reason string
}

// NewStaticSystemState returns a SystemState that always returns the given
// values. Used by tests and as a fallback when no event bus is available.
func NewStaticSystemState(busy bool, reason string) SystemState {
	return staticSystemState{busy: busy, reason: reason}
}

func (s staticSystemState) SystemBusy() (bool, string) { return s.busy, s.reason }
