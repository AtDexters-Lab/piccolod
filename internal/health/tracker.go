package health

import (
	"fmt"
	"sync"
	"time"
)

type Level int

const (
	LevelOK Level = iota
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(l))
	}
}

type Status struct {
	Level     Level                  `json:"level"`
	Message   string                 `json:"message"`
	Details   map[string]interface{} `json:"details,omitempty"`
	UpdatedAt time.Time              `json:"updated_at"`
}

func NewStatus(level Level, message string) Status {
	return Status{Level: level, Message: message, UpdatedAt: time.Now().UTC()}
}

// Tracker maintains a thread-safe collection of component health statuses.
type Tracker struct {
	mu       sync.RWMutex
	statuses map[string]Status
}

func NewTracker() *Tracker {
	return &Tracker{statuses: make(map[string]Status)}
}

func (t *Tracker) Set(name string, status Status) {
	if status.UpdatedAt.IsZero() {
		status.UpdatedAt = time.Now().UTC()
	}
	t.mu.Lock()
	t.statuses[name] = status
	t.mu.Unlock()
}

// Setf records a status with a printf-style message. When args is empty,
// format is treated as a literal so callers can pass strings containing '%'
// without escaping.
func (t *Tracker) Setf(name string, level Level, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	t.Set(name, Status{Level: level, Message: msg, UpdatedAt: time.Now().UTC()})
}

func (t *Tracker) Status(name string) (Status, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.statuses[name]
	return s, ok
}

// Clear removes a status entry. Used by callers that need to retract a
// transient error condition (e.g., RFC 20260510's auth-migration
// degraded-storage flag once the next successful unlock proves the
// underlying issue is resolved). Idempotent on absent keys.
func (t *Tracker) Clear(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.statuses, name)
}

func (t *Tracker) Snapshot() map[string]Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]Status, len(t.statuses))
	for k, v := range t.statuses {
		out[k] = v
	}
	return out
}

func (t *Tracker) Overall() Level {
	t.mu.RLock()
	defer t.mu.RUnlock()
	worst := LevelOK
	for _, st := range t.statuses {
		if st.Level > worst {
			worst = st.Level
		}
	}
	return worst
}

func (t *Tracker) Ready(required ...string) (bool, map[string]Status) {
	ready, _, _, snapshot := t.EvaluateReadiness(required...)
	return ready, snapshot
}

// EvaluateReadiness returns both strict service readiness and boot health from
// one coherent snapshot. Warnings make strict readiness false but remain
// boot-healthy because pre-unlock and initialization warnings are expected.
// Missing or error-level required components fail both.
func (t *Tracker) EvaluateReadiness(required ...string) (ready bool, bootHealthy bool, overall Level, snapshot map[string]Status) {
	snapshot = t.Snapshot()
	overall = LevelOK
	for _, st := range snapshot {
		if st.Level > overall {
			overall = st.Level
		}
	}
	if len(required) == 0 {
		return true, true, overall, snapshot
	}
	ready = true
	bootHealthy = true
	for _, name := range required {
		st, exists := snapshot[name]
		if !exists {
			ready = false
			bootHealthy = false
			continue
		}
		if st.Level > LevelOK {
			ready = false
		}
		if st.Level >= LevelError {
			bootHealthy = false
		}
	}
	return ready, bootHealthy, overall, snapshot
}
