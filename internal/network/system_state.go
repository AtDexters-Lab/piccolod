package network

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"

	"piccolod/internal/events"
	"piccolod/internal/state/paths"
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
//
// At construction it self-seeds by reading the current onboarding phase
// synchronously from /piccolo-core/network-bootstrap/onboarding.json. This
// closes the post-restart window where SystemBusy=false until the first
// bus event arrives — events.Bus has no replay-on-subscribe, and
// publishOnboardingEvent only fires from POST handlers, so without the
// disk seed a piccolod restart mid-onboarding would let the supervisor
// bounce wifi during install_disk (defeats catalog A7/G7).
//
// It also subscribes to TopicOnboardingStateChanged for subsequent
// transitions; the goroutine exits when ctx is done.
func NewBusSystemState(ctx context.Context, bus *events.Bus) SystemState {
	s := &busSystemState{}

	// Self-seed from disk before subscribing — readers see the correct
	// phase from the very first SystemBusy() call.
	s.phase = readOnboardingPhase()

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

// readOnboardingPhase parses the on-disk onboarding marker and returns the
// current phase string. Returns "" if the file is missing/unparseable —
// safe default since "" maps to SystemBusy=false (devices without an
// onboarding marker are presumed past first-run).
func readOnboardingPhase() string {
	path := paths.CoreJoin("network-bootstrap", "onboarding.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		State       string `json:"state"`
		InstallDone bool   `json:"install_done"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("WARN: net-supervisor: parse %s: %v — assuming SystemBusy=false", path, err)
		return ""
	}
	// Mirror onboarding.NewManager's boot-recovery path: install_disk +
	// !InstallDone resolves to pending. Either way the supervisor stays
	// busy, but log the equivalence so the journald trace is honest.
	if cfg.State == "install_disk" && !cfg.InstallDone {
		return "pending"
	}
	return cfg.State
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
