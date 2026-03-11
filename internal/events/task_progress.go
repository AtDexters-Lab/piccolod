package events

import (
	"sync"
	"time"
)

const taskStaleness = 35 * time.Minute

const (
	TopicTaskProgress Topic = "task_progress"
)

type TaskProgressEvent struct {
	TaskID     string         `json:"task_id"`
	TaskType   string         `json:"task_type"`
	InstanceID string         `json:"instance_id,omitempty"`
	Phase      string         `json:"phase"`
	Progress   int            `json:"progress"`
	Message    string         `json:"message"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	IsComplete bool           `json:"is_complete"`
	Error      string         `json:"error,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
}

type ProgressReporter interface {
	Report(TaskProgressEvent)
}

type BusProgressReporter struct {
	bus  *Bus
	mu   sync.RWMutex
	last map[string]TaskProgressEvent
}

func NewBusProgressReporter(bus *Bus) *BusProgressReporter {
	return &BusProgressReporter{
		bus:  bus,
		last: make(map[string]TaskProgressEvent),
	}
}

func (r *BusProgressReporter) Report(evt TaskProgressEvent) {
	if r == nil || r.bus == nil || evt.TaskID == "" {
		return
	}
	r.mu.Lock()
	prev, hadPrev := r.last[evt.TaskID]
	r.last[evt.TaskID] = evt
	r.mu.Unlock()
	r.bus.Publish(Event{Topic: TopicTaskProgress, Payload: evt})

	// Schedule eviction only on the first completion report to avoid duplicate timers.
	if evt.IsComplete && !(hadPrev && prev.IsComplete) {
		time.AfterFunc(2*time.Minute, func() {
			r.mu.Lock()
			delete(r.last, evt.TaskID)
			r.mu.Unlock()
		})
	}
}

func (r *BusProgressReporter) ActiveTasks() []TaskProgressEvent {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	cutoff := time.Now().Add(-taskStaleness)
	var active []TaskProgressEvent
	for _, evt := range r.last {
		if !evt.IsComplete && evt.Timestamp.After(cutoff) {
			active = append(active, evt)
		}
	}
	return active
}

func (r *BusProgressReporter) Last(taskID string) (TaskProgressEvent, bool) {
	if r == nil || taskID == "" {
		return TaskProgressEvent{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	evt, ok := r.last[taskID]
	return evt, ok
}
