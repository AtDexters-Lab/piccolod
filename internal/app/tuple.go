package app

import (
	"fmt"
	"time"
)

// TupleState tracks generation metadata for app snapshot tuples (RFC 20260302).
// Each update creates a "snapshot" generation (pre-update state) and an "active"
// generation (post-update state). Rollback swaps between them.
type TupleState struct {
	InstanceID        string            `json:"instance_id"`
	CurrentGeneration string            `json:"current_generation"`
	NextGenNumber     int               `json:"next_gen_number"`
	Generations       []TupleGeneration `json:"generations"`
	LastGCAt          *time.Time        `json:"last_gc_at,omitempty"`
}

// TupleGeneration captures the volume state at a point in time.
type TupleGeneration struct {
	ID           string            `json:"id"`                          // "gen-1", "gen-2", ...
	RootfsVolIDs map[string]string `json:"rootfs"`                     // svcName → rootfs volume ID
	DataSnapshot string            `json:"data_snapshot"`              // snapshot LV name (empty = live data volume)
	CreatedAt    time.Time         `json:"created_at"`
	DeprecatedAt *time.Time        `json:"deprecated_at,omitempty"`
	FailedAt     *time.Time        `json:"failed_at,omitempty"`        // when generation was marked failed (for GC retention)
	Status       string            `json:"status"`                     // "active" | "snapshot" | "deprecated" | "failed"
	FailedLVName string            `json:"failed_lv_name,omitempty"`   // data LV name after rollback (for GC)

	// EverHealthy is set to true when the reconciler first confirms all containers running.
	// Prevents auto-rollback on transient errors after a successful post-update startup.
	EverHealthy  bool       `json:"ever_healthy,omitempty"`
	HealthySince *time.Time `json:"healthy_since,omitempty"` // when EverHealthy was first set

	// RollbackAttempted guards against auto-rollback retry loops.
	// Set to true and persisted before attempting auto-rollback.
	RollbackAttempted bool `json:"rollback_attempted,omitempty"`
}

// Generation status constants.
const (
	TupleStatusActive     = "active"
	TupleStatusSnapshot   = "snapshot"
	TupleStatusDeprecated = "deprecated"
	TupleStatusFailed     = "failed"
)

// AllocateGenerationID increments the counter and returns "gen-{N}".
func (ts *TupleState) AllocateGenerationID() string {
	if ts.NextGenNumber <= 0 {
		ts.NextGenNumber = 1
	}
	id := fmt.Sprintf("gen-%d", ts.NextGenNumber)
	ts.NextGenNumber++
	return id
}

// ActiveGeneration returns the generation with Status=="active", or nil.
func (ts *TupleState) ActiveGeneration() *TupleGeneration {
	for i := range ts.Generations {
		if ts.Generations[i].Status == TupleStatusActive {
			return &ts.Generations[i]
		}
	}
	return nil
}

// LatestSnapshot returns the most recent generation with Status=="snapshot", or nil.
func (ts *TupleState) LatestSnapshot() *TupleGeneration {
	for i := len(ts.Generations) - 1; i >= 0; i-- {
		if ts.Generations[i].Status == TupleStatusSnapshot {
			return &ts.Generations[i]
		}
	}
	return nil
}

// GenerationByID returns the generation with the given ID, or nil.
func (ts *TupleState) GenerationByID(id string) *TupleGeneration {
	for i := range ts.Generations {
		if ts.Generations[i].ID == id {
			return &ts.Generations[i]
		}
	}
	return nil
}

// HasSnapshotSinceGeneration returns true if a data snapshot was taken after the given generation.
// Used by GC to determine if a failed data LV can be safely removed (RFC §7.3).
func (ts *TupleState) HasSnapshotSinceGeneration(genID string) bool {
	found := false
	for _, g := range ts.Generations {
		if g.ID == genID {
			found = true
			continue
		}
		if found && g.DataSnapshot != "" {
			return true
		}
	}
	return false
}

// DataSnapshotLVName returns the LV name for a data volume snapshot.
// Format: "snap-app-{instanceID}--gen{N}" (RFC §4.1 convention).
func DataSnapshotLVName(instanceID string, genNumber int) string {
	return fmt.Sprintf("snap-app-%s--gen%d", instanceID, genNumber)
}

// FailedDataLVName returns the LV name for a failed data volume after rollback.
// Format: "vol-app-{instanceID}--failed-gen{N}".
func FailedDataLVName(instanceID string, genNumber int) string {
	return fmt.Sprintf("vol-app-%s--failed-gen%d", instanceID, genNumber)
}
