package events

import (
	"sync"
	"time"
)

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
	r.last[evt.TaskID] = evt
	r.mu.Unlock()
	r.bus.Publish(Event{Topic: TopicTaskProgress, Payload: evt})
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
