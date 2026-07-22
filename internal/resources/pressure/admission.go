package pressure

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// WorkClass identifies child-producing work at the shared admission boundary.
type WorkClass string

const (
	WorkPodman       WorkClass = "podman"
	WorkTerminal     WorkClass = "terminal"
	WorkLogStream    WorkClass = "log_stream"
	WorkDiagnostic   WorkClass = "diagnostic"
	WorkLifecycle    WorkClass = "lifecycle"
	WorkOnboarding   WorkClass = "onboarding"
	WorkStorage      WorkClass = "storage"
	WorkUpdate       WorkClass = "update"
	WorkNetworkProbe WorkClass = "network_probe"
)

var ErrTaskPressure = errors.New("task pressure admission fenced")

// AdmissionError is safe to expose through an HTTP 503 response.
type AdmissionError struct {
	Class WorkClass
}

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("%s: %s", ErrTaskPressure, e.Class)
}

func (e *AdmissionError) Unwrap() error { return ErrTaskPressure }

func IsAdmissionError(err error) bool { return errors.Is(err, ErrTaskPressure) }

// AdmissionGate is the process-wide, allocation-free child admission latch.
// Warning and Critical fence it before publishing their health transition.
type AdmissionGate struct {
	pressureFenced atomic.Bool
	hardFenced     atomic.Bool
	startupFenced  atomic.Bool
	unavailable    atomic.Bool
	coreStartup    atomic.Bool
}

func NewAdmissionGate() *AdmissionGate { return &AdmissionGate{} }

func (g *AdmissionGate) Fence() {
	if g != nil {
		g.pressureFenced.Store(true)
	}
}

// OpenPressure releases only sampler-owned pressure and monitor-unavailable
// fences. A process-fatal hard fence is monotonic for the lifetime of the
// process and cannot be reopened by a later normal task sample.
func (g *AdmissionGate) OpenPressure() {
	if g != nil {
		g.pressureFenced.Store(false)
		g.unavailable.Store(false)
	}
}

// ResetForTest clears every admission reason. Production recovery must use
// the reason-specific open methods so a process-fatal fence remains sticky.
func (g *AdmissionGate) ResetForTest() {
	if g != nil {
		g.pressureFenced.Store(false)
		g.hardFenced.Store(false)
		g.startupFenced.Store(false)
		g.unavailable.Store(false)
		g.coreStartup.Store(false)
	}
}

func (g *AdmissionGate) Fenced() bool {
	return g != nil && (g.pressureFenced.Load() || g.hardFenced.Load() || g.startupFenced.Load() || g.unavailable.Load())
}

// FenceCritical is the hard task fence. Unlike a Warning fence, a durable
// transition continuation cannot bypass it while the emergency owner exits.
func (g *AdmissionGate) FenceCritical() {
	if g != nil {
		g.hardFenced.Store(true)
	}
}

// FenceUnavailable closes optional child admission while the task monitor
// cannot prove headroom. Unlike a Critical fence, it permits only the bounded
// core construction window needed to bring Piccolod's sole access plane up in
// a degraded state.
func (g *AdmissionGate) FenceUnavailable() {
	if g != nil {
		g.unavailable.Store(true)
	}
}

// BeginCoreStartup and EndCoreStartup bound the synchronous construction work
// required before Piccolod can bind its access plane. They do not bypass a
// real Warning or Critical pressure fence.
func (g *AdmissionGate) BeginCoreStartup() {
	if g != nil {
		g.coreStartup.Store(true)
	}
}

func (g *AdmissionGate) EndCoreStartup() {
	if g != nil {
		g.coreStartup.Store(false)
	}
}

// FenceStartup keeps production constructors and pre-Ready components from
// spawning optional children. It is independent of the pressure latch so a
// normal task sample cannot accidentally release startup ordering.
func (g *AdmissionGate) FenceStartup() {
	if g != nil {
		g.startupFenced.Store(true)
	}
}

// OpenStartup releases only the core-before-optional startup fence. A task
// Warning, Critical, or unavailable monitor continues to reject child work.
func (g *AdmissionGate) OpenStartup() {
	if g != nil {
		g.startupFenced.Store(false)
	}
}

type transitionContinuationKey struct{}
type workClassKey struct{}

// WithTransitionContinuation marks work already owned by a durable transition
// that is advancing only to its next recorded crash-safe boundary.
func WithTransitionContinuation(ctx context.Context) context.Context {
	return context.WithValue(ctx, transitionContinuationKey{}, true)
}

func IsTransitionContinuation(ctx context.Context) bool {
	return ctx != nil && ctx.Value(transitionContinuationKey{}) == true
}

// WithWorkClass assigns a more specific owner class to a shared command seam.
func WithWorkClass(ctx context.Context, class WorkClass) context.Context {
	return context.WithValue(ctx, workClassKey{}, class)
}

func WorkClassFromContext(ctx context.Context, fallback WorkClass) WorkClass {
	if ctx != nil {
		if class, ok := ctx.Value(workClassKey{}).(WorkClass); ok && class != "" {
			return class
		}
	}
	return fallback
}

func (g *AdmissionGate) Check(ctx context.Context, class WorkClass) error {
	if g == nil {
		return nil
	}
	if g.startupFenced.Load() || g.hardFenced.Load() {
		return &AdmissionError{Class: class}
	}
	if g.unavailable.Load() && !g.coreStartup.Load() {
		return &AdmissionError{Class: class}
	}
	if !g.pressureFenced.Load() || IsTransitionContinuation(ctx) {
		return nil
	}
	return &AdmissionError{Class: class}
}

// DefaultAdmission is shared by production child factories. Tests that need
// isolation should inject a separate gate into the task guard and reset this
// latch in cleanup when they deliberately fence it.
var DefaultAdmission = NewAdmissionGate()
