package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAllocateGenerationID(t *testing.T) {
	t.Run("empty_state", func(t *testing.T) {
		ts := &TupleState{}
		id := ts.AllocateGenerationID()
		if id != "gen-1" {
			t.Errorf("got %q, want gen-1", id)
		}
		if ts.NextGenNumber != 2 {
			t.Errorf("NextGenNumber: got %d, want 2", ts.NextGenNumber)
		}
	})

	t.Run("increments", func(t *testing.T) {
		ts := &TupleState{NextGenNumber: 5}
		id := ts.AllocateGenerationID()
		if id != "gen-5" {
			t.Errorf("got %q, want gen-5", id)
		}
		id2 := ts.AllocateGenerationID()
		if id2 != "gen-6" {
			t.Errorf("got %q, want gen-6", id2)
		}
	})

	t.Run("zero_resets_to_one", func(t *testing.T) {
		ts := &TupleState{NextGenNumber: 0}
		id := ts.AllocateGenerationID()
		if id != "gen-1" {
			t.Errorf("got %q, want gen-1", id)
		}
	})
}

func TestDataSnapshotLVName(t *testing.T) {
	got := DataSnapshotLVName("myapp", 3)
	want := "snap-app-myapp--gen3"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFailedDataLVName(t *testing.T) {
	got := FailedDataLVName("myapp", 2)
	want := "vol-app-myapp--failed-gen2"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestActiveGeneration(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		ts := &TupleState{
			Generations: []TupleGeneration{
				{ID: "gen-1", Status: TupleStatusSnapshot},
				{ID: "gen-2", Status: TupleStatusActive},
			},
		}
		active := ts.ActiveGeneration()
		if active == nil {
			t.Fatal("expected non-nil")
		}
		if active.ID != "gen-2" {
			t.Errorf("got %q, want gen-2", active.ID)
		}
	})

	t.Run("none", func(t *testing.T) {
		ts := &TupleState{
			Generations: []TupleGeneration{
				{ID: "gen-1", Status: TupleStatusSnapshot},
			},
		}
		if ts.ActiveGeneration() != nil {
			t.Error("expected nil")
		}
	})

	t.Run("empty", func(t *testing.T) {
		ts := &TupleState{}
		if ts.ActiveGeneration() != nil {
			t.Error("expected nil")
		}
	})
}

func TestLatestSnapshot(t *testing.T) {
	t.Run("multiple_snapshots", func(t *testing.T) {
		ts := &TupleState{
			Generations: []TupleGeneration{
				{ID: "gen-1", Status: TupleStatusDeprecated},
				{ID: "gen-2", Status: TupleStatusSnapshot},
				{ID: "gen-3", Status: TupleStatusActive},
				{ID: "gen-4", Status: TupleStatusSnapshot},
			},
		}
		snap := ts.LatestSnapshot()
		if snap == nil {
			t.Fatal("expected non-nil")
		}
		if snap.ID != "gen-4" {
			t.Errorf("got %q, want gen-4", snap.ID)
		}
	})

	t.Run("no_snapshots", func(t *testing.T) {
		ts := &TupleState{
			Generations: []TupleGeneration{
				{ID: "gen-1", Status: TupleStatusActive},
			},
		}
		if ts.LatestSnapshot() != nil {
			t.Error("expected nil")
		}
	})
}

func TestGenerationByID(t *testing.T) {
	ts := &TupleState{
		Generations: []TupleGeneration{
			{ID: "gen-1", Status: TupleStatusSnapshot},
			{ID: "gen-2", Status: TupleStatusActive},
		},
	}

	t.Run("found", func(t *testing.T) {
		gen := ts.GenerationByID("gen-1")
		if gen == nil || gen.ID != "gen-1" {
			t.Error("expected gen-1")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		if ts.GenerationByID("gen-99") != nil {
			t.Error("expected nil")
		}
	})
}

func TestHasSnapshotSinceGeneration(t *testing.T) {
	ts := &TupleState{
		Generations: []TupleGeneration{
			{ID: "gen-1", Status: TupleStatusFailed, DataSnapshot: ""},
			{ID: "gen-2", Status: TupleStatusSnapshot, DataSnapshot: "snap-data"},
			{ID: "gen-3", Status: TupleStatusActive, DataSnapshot: ""},
		},
	}

	t.Run("has_snapshot_after", func(t *testing.T) {
		if !ts.HasSnapshotSinceGeneration("gen-1") {
			t.Error("expected true: gen-2 has DataSnapshot after gen-1")
		}
	})

	t.Run("no_snapshot_after", func(t *testing.T) {
		if ts.HasSnapshotSinceGeneration("gen-2") {
			t.Error("expected false: gen-3 has no DataSnapshot")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		if ts.HasSnapshotSinceGeneration("gen-99") {
			t.Error("expected false for non-existent gen")
		}
	})
}

func TestTupleStateSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ts := &TupleState{
		InstanceID:    "test-app",
		NextGenNumber: 3,
		Generations: []TupleGeneration{
			{
				ID:           "gen-1",
				RootfsVolIDs: map[string]string{"main": "rootfs-vol-1"},
				DataSnapshot: "snap-app-test--gen1",
				CreatedAt:    now.Add(-48 * time.Hour),
				Status:       TupleStatusSnapshot,
			},
			{
				ID:           "gen-2",
				RootfsVolIDs: map[string]string{"main": "rootfs-vol-2"},
				CreatedAt:    now,
				Status:       TupleStatusActive,
			},
		},
	}

	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded TupleState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.InstanceID != ts.InstanceID {
		t.Errorf("InstanceID: got %q, want %q", decoded.InstanceID, ts.InstanceID)
	}
	if decoded.NextGenNumber != ts.NextGenNumber {
		t.Errorf("NextGenNumber: got %d, want %d", decoded.NextGenNumber, ts.NextGenNumber)
	}
	if len(decoded.Generations) != 2 {
		t.Fatalf("Generations: got %d, want 2", len(decoded.Generations))
	}
	if decoded.Generations[0].DataSnapshot != "snap-app-test--gen1" {
		t.Errorf("DataSnapshot: got %q", decoded.Generations[0].DataSnapshot)
	}
	if decoded.Generations[1].Status != TupleStatusActive {
		t.Errorf("Status: got %q, want active", decoded.Generations[1].Status)
	}
}

func TestRollbackAttemptedGuard(t *testing.T) {
	ts := &TupleState{
		Generations: []TupleGeneration{
			{ID: "gen-1", Status: TupleStatusSnapshot, RollbackAttempted: true},
			{ID: "gen-2", Status: TupleStatusActive},
		},
	}

	snap := ts.LatestSnapshot()
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if !snap.RollbackAttempted {
		t.Error("RollbackAttempted should be true")
	}
}

func TestLoadTupleState(t *testing.T) {
	t.Run("no_file", func(t *testing.T) {
		tmpDir := t.TempDir()
		fsm, err := NewFilesystemStateManager(tmpDir)
		if err != nil {
			t.Fatalf("NewFilesystemStateManager: %v", err)
		}

		ts, err := fsm.LoadTupleState("nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ts != nil {
			t.Error("expected nil for non-existent file")
		}
	})

	t.Run("valid_file", func(t *testing.T) {
		tmpDir := t.TempDir()
		fsm, err := NewFilesystemStateManager(tmpDir)
		if err != nil {
			t.Fatalf("NewFilesystemStateManager: %v", err)
		}

		instanceID := "test-app"
		appDir := filepath.Join(tmpDir, "apps", instanceID)
		if err := os.MkdirAll(appDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		ts := &TupleState{
			InstanceID:    instanceID,
			NextGenNumber: 2,
			Generations: []TupleGeneration{
				{ID: "gen-1", Status: TupleStatusActive},
			},
		}
		data, _ := json.MarshalIndent(ts, "", "  ")
		if err := os.WriteFile(filepath.Join(appDir, "generations.json"), data, 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		loaded, err := fsm.LoadTupleState(instanceID)
		if err != nil {
			t.Fatalf("LoadTupleState: %v", err)
		}
		if loaded == nil {
			t.Fatal("expected non-nil")
		}
		if loaded.NextGenNumber != 2 {
			t.Errorf("NextGenNumber: got %d, want 2", loaded.NextGenNumber)
		}
		if len(loaded.Generations) != 1 {
			t.Fatalf("Generations: got %d, want 1", len(loaded.Generations))
		}
	})
}

func TestStoreTupleState(t *testing.T) {
	tmpDir := t.TempDir()
	fsm, err := NewFilesystemStateManager(tmpDir)
	if err != nil {
		t.Fatalf("NewFilesystemStateManager: %v", err)
	}

	instanceID := "test-app"
	ts := &TupleState{
		InstanceID:    instanceID,
		NextGenNumber: 3,
		Generations: []TupleGeneration{
			{ID: "gen-1", Status: TupleStatusSnapshot},
			{ID: "gen-2", Status: TupleStatusActive},
		},
	}

	if err := fsm.StoreTupleState(instanceID, ts); err != nil {
		t.Fatalf("StoreTupleState: %v", err)
	}

	// Read back.
	loaded, err := fsm.LoadTupleState(instanceID)
	if err != nil {
		t.Fatalf("LoadTupleState: %v", err)
	}
	if loaded.NextGenNumber != 3 {
		t.Errorf("NextGenNumber: got %d, want 3", loaded.NextGenNumber)
	}
	if len(loaded.Generations) != 2 {
		t.Errorf("Generations: got %d, want 2", len(loaded.Generations))
	}
}

func TestMapsEqual(t *testing.T) {
	t.Run("equal", func(t *testing.T) {
		a := map[string]string{"x": "1", "y": "2"}
		b := map[string]string{"x": "1", "y": "2"}
		if !mapsEqual(a, b) {
			t.Error("expected equal")
		}
	})

	t.Run("different_values", func(t *testing.T) {
		a := map[string]string{"x": "1"}
		b := map[string]string{"x": "2"}
		if mapsEqual(a, b) {
			t.Error("expected not equal")
		}
	})

	t.Run("different_keys", func(t *testing.T) {
		a := map[string]string{"x": "1"}
		b := map[string]string{"y": "1"}
		if mapsEqual(a, b) {
			t.Error("expected not equal")
		}
	})

	t.Run("nil_and_empty", func(t *testing.T) {
		if !mapsEqual(nil, nil) {
			t.Error("nil == nil should be true")
		}
		if !mapsEqual(nil, map[string]string{}) {
			t.Error("nil == empty should be true")
		}
	})
}
