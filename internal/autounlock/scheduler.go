package autounlock

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// UpdateManager is the slice of the OS update manager the scheduler depends
// on. Defined here so the scheduler can be unit-tested without pulling the
// real update package into tests.
type UpdateManager interface {
	HasStagedUpdate(ctx context.Context) bool
	Reboot(ctx context.Context) error
}

// Audit kinds emitted by the scheduler.
const (
	AuditSchedulerFired  = "auto_unlock.scheduler.fired"
	AuditSchedulerFailed = "auto_unlock.scheduler.failed"
)

// schedulerTickRate is the cadence at which the scheduler evaluates its
// gates. 60s is generous for an hour-granular window — sub-second precision
// adds nothing.
const schedulerTickRate = 60 * time.Second

// SchedulerDeps wires the scheduler to its environment.
type SchedulerDeps struct {
	UpdateManager UpdateManager
	PublishAudit  AuditEmitter
	Now           func() time.Time
}

// Scheduler triggers the maintenance-window auto-reboot. Gated on:
//   - state.Enabled (auto-unlock itself)
//   - state.AutoReboot.Enabled (overnight-apply opt-in)
//   - current local hour ∈ [start, end) (with wrap-around)
//   - updateManager.HasStagedUpdate
//   - alreadyTriedThisWindow flag (resets on window-cycle rollover)
//
// On fire: persists last_fired_at BEFORE calling Reboot (since Reboot may
// not return), audits, calls Reboot. Failure → records last_failed_at +
// audit, retries next window-cycle (not next minute).
type Scheduler struct {
	deps SchedulerDeps

	mu sync.Mutex

	// alreadyTriedThisWindow guards against firing twice in the same window-cycle.
	// Set after a fire (success or failure) and on PUT-window-edit; cleared
	// when the window-cycle rolls over (live tick detects via tEdge change).
	alreadyTriedThisWindow atomic.Bool
	// lastTEdge holds the unix time of the previously-computed tEdge.
	// When the next tick computes a different tEdge the cycle has rolled
	// over — alreadyTriedThisWindow gets cleared.
	lastTEdge atomic.Int64
}

// NewScheduler constructs a Scheduler.
func NewScheduler(deps SchedulerDeps) *Scheduler {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Scheduler{deps: deps}
}

// Run is the scheduler's main loop. Call from a goroutine; returns when
// ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.rehydrate()
	ticker := time.NewTicker(schedulerTickRate)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// MarkWindowEdit blocks the scheduler from firing for the rest of the
// current window-cycle. Called by the Update handler whenever the window
// bounds or auto_reboot.enabled toggle changes — closes the compromised-
// admin DoS where a malicious window edit could otherwise trigger an
// immediate reboot.
//
// In-memory only — a piccolod restart between PUT and next window edge
// loses the marker. Acknowledged in the plan: the same admin who can edit
// the state file can also restart piccolod, so persistence wouldn't add
// real defense.
func (s *Scheduler) MarkWindowEdit() {
	s.alreadyTriedThisWindow.Store(true)
}

func (s *Scheduler) tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := LoadState()
	if err != nil && err != ErrInvalidStateFile {
		return
	}
	if !state.Enabled || !state.AutoReboot.Enabled {
		return
	}

	now := s.deps.Now()
	startHour := state.AutoReboot.WindowStartHour
	endHour := state.AutoReboot.WindowEndHour

	// Live edge detection: if the current window-cycle has changed since the
	// previous tick, reset the already-tried flag.
	curEdge := tEdge(now, startHour, endHour)
	prev := s.lastTEdge.Load()
	if prev != 0 && curEdge.Unix() != prev {
		s.alreadyTriedThisWindow.Store(false)
	}
	s.lastTEdge.Store(curEdge.Unix())

	if !inWindow(now.Hour(), startHour, endHour) {
		return
	}
	if s.alreadyTriedThisWindow.Load() {
		return
	}
	if !s.deps.UpdateManager.HasStagedUpdate(ctx) {
		return
	}

	// Persist BEFORE Reboot — Reboot triggers SIGTERM which may stop us
	// before we get back here.
	state.AutoReboot.LastFiredAt = &now
	if err := SaveState(state); err != nil {
		log.Printf("WARN: autounlock scheduler: persist last_fired_at: %v", err)
	}

	if s.deps.PublishAudit != nil {
		s.deps.PublishAudit(AuditSchedulerFired, map[string]any{
			"reason":            "auto_apply_update",
			"hour":              now.Hour(),
			"has_staged_update": true,
		})
	}

	s.alreadyTriedThisWindow.Store(true)

	if err := s.deps.UpdateManager.Reboot(ctx); err != nil {
		log.Printf("ERROR: autounlock scheduler: Reboot failed: %v", err)
		failedAt := s.deps.Now()
		state.AutoReboot.LastFailedAt = &failedAt
		_ = SaveState(state)
		if s.deps.PublishAudit != nil {
			s.deps.PublishAudit(AuditSchedulerFailed, map[string]any{
				"reason":     err.Error(),
				"error_kind": "reboot_failed",
			})
		}
	}
}

// rehydrate is called once on Run start. If the persisted last_fired_at
// falls within the current window-cycle, set alreadyTriedThisWindow=true so
// a piccolod restart inside the window doesn't double-fire.
func (s *Scheduler) rehydrate() {
	state, err := LoadState()
	if err != nil && err != ErrInvalidStateFile {
		return
	}
	now := s.deps.Now()
	startHour := state.AutoReboot.WindowStartHour
	endHour := state.AutoReboot.WindowEndHour
	curEdge := tEdge(now, startHour, endHour)
	s.lastTEdge.Store(curEdge.Unix())
	if state.AutoReboot.LastFiredAt == nil {
		return
	}
	if !state.AutoReboot.LastFiredAt.Before(curEdge) && !state.AutoReboot.LastFiredAt.After(now.Add(time.Hour)) {
		// last_fired_at is between the current cycle's edge and now+1h.
		// (The +1h tolerance defends against trivial clock skew between fire
		// time and rehydrate time without doing full corruption gating.)
		s.alreadyTriedThisWindow.Store(true)
	}
}

// tEdge returns the most recent moment when inWindow flipped false→true,
// i.e., the start of the window-cycle that `now` belongs to.
//
// Non-wrap (start ≤ end): cycle boundary is today's `start_hour:00` if
// `now.Hour ≥ start_hour`, else yesterday's.
//
// Wrap (start > end): cycle boundary is today's `start_hour:00` if
// `now.Hour ≥ start_hour` (we're in the pre-midnight head), else
// yesterday's `start_hour:00` (we're in the post-midnight tail).
func tEdge(now time.Time, startHour, endHour int) time.Time {
	today := time.Date(now.Year(), now.Month(), now.Day(), startHour, 0, 0, 0, now.Location())
	if startHour <= endHour {
		if now.Hour() < startHour {
			return today.AddDate(0, 0, -1)
		}
		return today
	}
	// wrap window
	if now.Hour() >= startHour {
		return today
	}
	return today.AddDate(0, 0, -1)
}

// inWindow reports whether the given hour falls inside [start, end).
// Wrap-around (start > end) means [start..23] ∪ [0..end).
func inWindow(hour, start, end int) bool {
	if start <= end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}
