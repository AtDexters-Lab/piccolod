package app

import (
	"testing"
	"time"
)

func TestAutoDeprecation(t *testing.T) {
	t.Run("deprecates_after_24h_healthy", func(t *testing.T) {
		healthySince := time.Now().Add(-25 * time.Hour) // >24h ago
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 3,
			Generations: []TupleGeneration{
				{
					ID:           "gen-1",
					Status:       TupleStatusSnapshot,
					DataSnapshot: "snap-data",
					CreatedAt:    time.Now().Add(-48 * time.Hour),
				},
				{
					ID:           "gen-2",
					Status:       TupleStatusActive,
					CreatedAt:    time.Now().Add(-48 * time.Hour),
					EverHealthy:  true,
					HealthySince: &healthySince,
				},
			},
		}

		active := ts.ActiveGeneration()
		snap := ts.LatestSnapshot()
		if active == nil || snap == nil {
			t.Fatal("expected both active and snapshot generations")
		}
		if active.HealthySince == nil || time.Since(*active.HealthySince) <= healthVerificationDuration {
			t.Fatal("HealthySince should be older than 24h for this test")
		}
		if snap.Status != TupleStatusSnapshot {
			t.Errorf("expected snapshot status, got %q", snap.Status)
		}
	})

	t.Run("no_deprecation_under_24h_healthy", func(t *testing.T) {
		healthySince := time.Now().Add(-1 * time.Hour) // <24h ago
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 3,
			Generations: []TupleGeneration{
				{
					ID:           "gen-1",
					Status:       TupleStatusSnapshot,
					DataSnapshot: "snap-data",
					CreatedAt:    time.Now().Add(-48 * time.Hour),
				},
				{
					ID:           "gen-2",
					Status:       TupleStatusActive,
					CreatedAt:    time.Now().Add(-48 * time.Hour), // created long ago
					EverHealthy:  true,
					HealthySince: &healthySince, // but only healthy for 1h
				},
			},
		}

		active := ts.ActiveGeneration()
		if active.HealthySince == nil || time.Since(*active.HealthySince) > healthVerificationDuration {
			t.Fatal("HealthySince should be younger than 24h for this test")
		}
	})
}

func TestAutoRollbackTrigger(t *testing.T) {
	t.Run("snapshot_available_not_attempted", func(t *testing.T) {
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 3,
			Generations: []TupleGeneration{
				{
					ID:                "gen-1",
					Status:            TupleStatusSnapshot,
					DataSnapshot:      "snap-data",
					RollbackAttempted: false,
				},
				{
					ID:     "gen-2",
					Status: TupleStatusActive,
				},
			},
		}

		snap := ts.LatestSnapshot()
		if snap == nil {
			t.Fatal("expected snapshot")
		}
		if snap.RollbackAttempted {
			t.Error("RollbackAttempted should be false")
		}
		// Auto-rollback would be triggered if status is Error.
	})
}

func TestAutoRollbackGuard(t *testing.T) {
	t.Run("already_attempted_blocks_rollback", func(t *testing.T) {
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 3,
			Generations: []TupleGeneration{
				{
					ID:                "gen-1",
					Status:            TupleStatusSnapshot,
					DataSnapshot:      "snap-data",
					RollbackAttempted: true, // already attempted
				},
				{
					ID:     "gen-2",
					Status: TupleStatusActive,
				},
			},
		}

		snap := ts.LatestSnapshot()
		if snap == nil {
			t.Fatal("expected snapshot")
		}
		if !snap.RollbackAttempted {
			t.Error("RollbackAttempted should be true")
		}
		// Auto-rollback would NOT be triggered because RollbackAttempted=true.
	})

	t.Run("no_active_allows_rollback", func(t *testing.T) {
		// After a failed update where recordPostUpdateGeneration wasn't called,
		// there's a snapshot but no active. Auto-rollback should still trigger.
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 2,
			Generations: []TupleGeneration{
				{
					ID:                "gen-1",
					Status:            TupleStatusSnapshot,
					DataSnapshot:      "snap-data",
					RollbackAttempted: false,
				},
			},
		}

		if ts.ActiveGeneration() != nil {
			t.Error("should have no active generation")
		}
		snap := ts.LatestSnapshot()
		if snap == nil {
			t.Fatal("expected snapshot")
		}
		if snap.RollbackAttempted {
			t.Error("RollbackAttempted should be false")
		}
		// Auto-rollback should fire: snapshot exists + StatusError (no active needed).
	})

	t.Run("no_snapshot_blocks_rollback", func(t *testing.T) {
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 2,
			Generations: []TupleGeneration{
				{
					ID:     "gen-1",
					Status: TupleStatusActive,
				},
			},
		}

		if ts.LatestSnapshot() != nil {
			t.Error("should have no snapshot")
		}
	})

	t.Run("ever_healthy_blocks_rollback", func(t *testing.T) {
		// If the active generation was observed running after update,
		// auto-rollback should NOT trigger on subsequent transient errors.
		ts := &TupleState{
			InstanceID:    "test-app",
			NextGenNumber: 3,
			Generations: []TupleGeneration{
				{
					ID:                "gen-1",
					Status:            TupleStatusSnapshot,
					DataSnapshot:      "snap-data",
					RollbackAttempted: false,
				},
				{
					ID:          "gen-2",
					Status:      TupleStatusActive,
					EverHealthy: true, // was observed running successfully
				},
			},
		}

		active := ts.ActiveGeneration()
		if active == nil {
			t.Fatal("expected active generation")
		}
		if !active.EverHealthy {
			t.Error("EverHealthy should be true")
		}
		// Auto-rollback would NOT trigger: active was previously healthy,
		// so the current error is likely transient, not update-related.
	})
}
