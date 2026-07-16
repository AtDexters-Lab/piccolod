package health

import "testing"

func TestTrackerSetAndSnapshot(t *testing.T) {
	tracker := NewTracker()
	tracker.Setf("component", LevelOK, "initialized")
	snap := tracker.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}
	if snap["component"].Level != LevelOK {
		t.Fatalf("expected level ok")
	}
}

func TestTrackerReady(t *testing.T) {
	tracker := NewTracker()
	tracker.Setf("a", LevelOK, "ready")
	tracker.Setf("b", LevelWarn, "partial")

	ready, _ := tracker.Ready("a")
	if !ready {
		t.Fatal("component a should be ready")
	}

	ready, _ = tracker.Ready("a", "b")
	if ready {
		t.Fatal("component b should make readiness fail")
	}
}

func TestTrackerEvaluateReadinessSeparatesBootHealthFromStrictReadiness(t *testing.T) {
	tests := []struct {
		name            string
		configure       func(*Tracker)
		wantReady       bool
		wantBootHealthy bool
	}{
		{
			name: "all required checks ok",
			configure: func(tracker *Tracker) {
				tracker.Setf("a", LevelOK, "ready")
				tracker.Setf("b", LevelOK, "ready")
			},
			wantReady:       true,
			wantBootHealthy: true,
		},
		{
			name: "required warning",
			configure: func(tracker *Tracker) {
				tracker.Setf("a", LevelOK, "ready")
				tracker.Setf("b", LevelWarn, "initializing")
			},
			wantReady:       false,
			wantBootHealthy: true,
		},
		{
			name: "required error",
			configure: func(tracker *Tracker) {
				tracker.Setf("a", LevelOK, "ready")
				tracker.Setf("b", LevelError, "failed")
			},
			wantReady:       false,
			wantBootHealthy: false,
		},
		{
			name: "missing required check",
			configure: func(tracker *Tracker) {
				tracker.Setf("a", LevelOK, "ready")
			},
			wantReady:       false,
			wantBootHealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			tt.configure(tracker)
			ready, bootHealthy, _, _ := tracker.EvaluateReadiness("a", "b")
			if ready != tt.wantReady || bootHealthy != tt.wantBootHealthy {
				t.Fatalf("EvaluateReadiness = ready %v bootHealthy %v, want %v/%v", ready, bootHealthy, tt.wantReady, tt.wantBootHealthy)
			}
		})
	}
}

func TestTrackerOverall(t *testing.T) {
	tracker := NewTracker()
	tracker.Setf("a", LevelOK, "ok")
	tracker.Setf("b", LevelWarn, "warn")
	if tracker.Overall() != LevelWarn {
		t.Fatalf("expected overall warn")
	}
	tracker.Setf("c", LevelError, "fail")
	if tracker.Overall() != LevelError {
		t.Fatalf("expected overall error")
	}
}
