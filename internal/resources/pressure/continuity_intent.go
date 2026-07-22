package pressure

import "sync"

// RestartContinuityIntent is the task guard's latest desired state for the
// restart-unlock continuity owner. Generation increases only when the guard's
// effective pressure state changes between Normal, Warning, and Critical.
//
// Warning asks the owner to prepare a short-lived handoff, Normal asks it to
// discard an unused Warning handoff, and Critical tells it that the emergency
// process owner has taken over the bounded last chance. The capability, rather
// than the task guard, owns all provider I/O and lifecycle decisions.
type RestartContinuityIntent struct {
	State      TaskPressureState
	Generation uint64
}

// RestartContinuityIntentView reads the relay's current desired state without
// acquiring a TaskGuard lock. Capabilities use it after acquiring their own
// continuity gate and again before committing a handoff mutation, so an Apply
// call that waited behind newer work cannot act on stale intent.
type RestartContinuityIntentView func() RestartContinuityIntent

// Latest returns the most recently committed pressure intent.
func (v RestartContinuityIntentView) Latest() RestartContinuityIntent {
	if v == nil {
		return RestartContinuityIntent{}
	}
	return v()
}

// IsCurrent reports whether intent is still the relay's latest generation and
// state. Comparing both fields makes misuse with a fabricated generation fail
// closed.
func (v RestartContinuityIntentView) IsCurrent(intent RestartContinuityIntent) bool {
	latest := v.Latest()
	return latest.Generation != 0 && latest == intent
}

// RestartContinuityCapability applies one task-pressure desired state. Apply
// may block while the continuity owner serializes provider work: TaskGuard
// invokes it only from a contained asynchronous dispatcher. Returning means
// the generation has finished applying; the dispatcher then rechecks the
// latest generation before becoming idle.
type RestartContinuityCapability interface {
	ApplyTaskPressureIntent(RestartContinuityIntent, RestartContinuityIntentView)
}

// RestartContinuityCapabilityFunc adapts a function to
// RestartContinuityCapability.
type RestartContinuityCapabilityFunc func(RestartContinuityIntent, RestartContinuityIntentView)

func (f RestartContinuityCapabilityFunc) ApplyTaskPressureIntent(
	intent RestartContinuityIntent,
	view RestartContinuityIntentView,
) {
	f(intent, view)
}

// restartContinuityRelay retains intent before the capability exists and owns
// at most one callback goroutine. A blocked or slow callback cannot delay the
// task sampler, and intermediate generations are coalesced to the latest one.
type restartContinuityRelay struct {
	mu sync.Mutex

	latest    RestartContinuityIntent
	attached  RestartContinuityCapability
	dispatch  bool
	attachGen uint64
}

func (r *restartContinuityRelay) attach(capability RestartContinuityCapability) {
	r.mu.Lock()
	r.attached = capability
	r.attachGen++
	r.startLocked()
	r.mu.Unlock()
}

func (r *restartContinuityRelay) publish(intent RestartContinuityIntent) {
	if intent.Generation == 0 {
		return
	}
	r.mu.Lock()
	if intent.Generation <= r.latest.Generation {
		r.mu.Unlock()
		return
	}
	r.latest = intent
	r.startLocked()
	r.mu.Unlock()
}

func (r *restartContinuityRelay) startLocked() {
	if r.dispatch || r.attached == nil || r.latest.Generation == 0 {
		return
	}
	r.dispatch = true
	capability := r.attached
	attachment := r.attachGen
	intent := r.latest
	go r.run(capability, attachment, intent)
}

func (r *restartContinuityRelay) run(
	capability RestartContinuityCapability,
	attachment uint64,
	intent RestartContinuityIntent,
) {
	view := RestartContinuityIntentView(r.latestIntent)
	for {
		capability.ApplyTaskPressureIntent(intent, view)

		r.mu.Lock()
		if r.attached == nil {
			r.dispatch = false
			r.mu.Unlock()
			return
		}
		if r.attachGen != attachment {
			capability = r.attached
			attachment = r.attachGen
			intent = r.latest
			r.mu.Unlock()
			continue
		}
		if r.latest.Generation != intent.Generation {
			intent = r.latest
			r.mu.Unlock()
			continue
		}
		r.dispatch = false
		r.mu.Unlock()
		return
	}
}

func (r *restartContinuityRelay) latestIntent() RestartContinuityIntent {
	r.mu.Lock()
	latest := r.latest
	r.mu.Unlock()
	return latest
}
