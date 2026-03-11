package events

import (
	"testing"
	"time"
)

func TestBusProgressReporter_ActiveTasks_nil_receiver(t *testing.T) {
	var r *BusProgressReporter
	got := r.ActiveTasks()
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestBusProgressReporter_ActiveTasks_empty(t *testing.T) {
	bus := NewBus()
	r := NewBusProgressReporter(bus)
	got := r.ActiveTasks()
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %d items", len(got))
	}
}

func TestBusProgressReporter_ActiveTasks_filters_completed(t *testing.T) {
	bus := NewBus()
	r := NewBusProgressReporter(bus)

	now := time.Now()
	r.Report(TaskProgressEvent{TaskID: "a", Phase: "pulling", Timestamp: now})
	r.Report(TaskProgressEvent{TaskID: "b", Phase: "pulling", Timestamp: now})
	r.Report(TaskProgressEvent{TaskID: "c", Phase: "done", IsComplete: true, Timestamp: now})

	got := r.ActiveTasks()
	if len(got) != 2 {
		t.Fatalf("expected 2 active tasks, got %d", len(got))
	}

	ids := map[string]bool{}
	for _, evt := range got {
		ids[evt.TaskID] = true
	}
	if !ids["a"] || !ids["b"] {
		t.Fatalf("expected tasks a and b, got %v", ids)
	}
}

func TestBusProgressReporter_ActiveTasks_filters_stale(t *testing.T) {
	bus := NewBus()
	r := NewBusProgressReporter(bus)

	// Directly insert a stale event (timestamp older than taskStaleness)
	staleTime := time.Now().Add(-taskStaleness - time.Minute)
	r.mu.Lock()
	r.last["stale"] = TaskProgressEvent{TaskID: "stale", Phase: "pulling", Timestamp: staleTime}
	r.mu.Unlock()

	// Add a fresh active task
	r.Report(TaskProgressEvent{TaskID: "fresh", Phase: "pulling", Timestamp: time.Now()})

	got := r.ActiveTasks()
	if len(got) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(got))
	}
	if got[0].TaskID != "fresh" {
		t.Fatalf("expected task 'fresh', got %q", got[0].TaskID)
	}
}

func TestBusProgressReporter_completed_eviction(t *testing.T) {
	bus := NewBus()
	r := NewBusProgressReporter(bus)

	r.Report(TaskProgressEvent{TaskID: "x", Phase: "done", IsComplete: true, Timestamp: time.Now()})

	// Immediately after reporting, the entry should still be in last
	r.mu.RLock()
	_, exists := r.last["x"]
	r.mu.RUnlock()
	if !exists {
		t.Fatal("expected completed task to still be in last map immediately after report")
	}

	// The AfterFunc cleanup runs after 2 minutes — we can't easily test real timers
	// in a unit test without making the code testable with injectable timers.
	// Instead, verify the entry is present and the test for ActiveTasks filtering
	// confirms completed tasks don't appear in ActiveTasks.
	got := r.ActiveTasks()
	if len(got) != 0 {
		t.Fatalf("expected 0 active tasks (completed should be filtered), got %d", len(got))
	}
}
