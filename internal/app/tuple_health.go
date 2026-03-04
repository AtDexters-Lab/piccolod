package app

import (
	"context"
	"log"
	"time"
)

const (
	// healthVerificationDuration is how long an active generation must run healthy
	// before the previous snapshot is deprecated (RFC 20260302 Phase 4).
	healthVerificationDuration = 24 * time.Hour
)

// checkTupleHealth checks tuple generation health during reconciliation.
// - Auto-deprecation: active generation healthy for 24h → deprecate previous snapshot.
// - Auto-rollback: StatusError after update with available snapshot → trigger rollback.
//
// Called BEFORE container state checks so auto-rollback triggers before recreation attempts.
// EverHealthy tracking is done separately by markTupleHealthy (called AFTER container verification).
func (m *AppManager) checkTupleHealth(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) {
	ts, err := state.LoadTupleState(appInst.InstanceID)
	if ts == nil || err != nil {
		return // no generations, nothing to do
	}

	active := ts.ActiveGeneration()
	snapshot := ts.LatestSnapshot()

	// Auto-deprecation: active generation verified healthy for 24h.
	// Use HealthySince (when reconciler first confirmed running) rather than CreatedAt.
	if active != nil && active.HealthySince != nil && snapshot != nil {
		if time.Since(*active.HealthySince) > healthVerificationDuration {
			observedStatus := m.getObservedStatus(appInst.InstanceID)
			if observedStatus == StatusRunning {
				now := time.Now()
				snapshot.DeprecatedAt = &now
				snapshot.Status = TupleStatusDeprecated
				if storeErr := state.StoreTupleState(appInst.InstanceID, ts); storeErr != nil {
					log.Printf("WARN: %s: failed to persist deprecation: %v", appInst.InstanceID, storeErr)
				} else {
					log.Printf("INFO: %s: generation %s deprecated (24h healthy)", appInst.InstanceID, snapshot.ID)
				}
			}
		}
	}

	// Auto-rollback: StatusError after update with available snapshot.
	// Only trigger if the active generation was NEVER observed healthy — prevents
	// rollback on transient errors (OOM, disk full) after a successful post-update startup.
	observedStatus := m.getObservedStatus(appInst.InstanceID)
	activeNeverHealthy := active == nil || !active.EverHealthy
	if snapshot != nil && !snapshot.RollbackAttempted && observedStatus == StatusError && activeNeverHealthy {
		log.Printf("INFO: %s: auto-rollback triggered (StatusError with snapshot %s, active never healthy)", appInst.InstanceID, snapshot.ID)

		// Persist guard BEFORE attempting rollback to prevent retry loops.
		snapshot.RollbackAttempted = true
		if storeErr := state.StoreTupleState(appInst.InstanceID, ts); storeErr != nil {
			log.Printf("WARN: %s: failed to persist rollback guard: %v", appInst.InstanceID, storeErr)
			return // don't attempt rollback if we can't persist the guard
		}

		if err := m.rollbackToSnapshotLocked(ctx, state, appInst); err != nil {
			log.Printf("ERROR: %s: auto-rollback failed: %v", appInst.InstanceID, err)
		}
	}
}

// markTupleHealthy records that the active generation has been observed running by the reconciler.
// Called AFTER the reconciler verifies all containers are running (not from the update path).
func (m *AppManager) markTupleHealthy(state *FilesystemStateManager, instanceID string) {
	ts, err := state.LoadTupleState(instanceID)
	if ts == nil || err != nil {
		return
	}

	active := ts.ActiveGeneration()
	if active == nil || active.EverHealthy {
		return
	}

	active.EverHealthy = true
	now := time.Now()
	active.HealthySince = &now
	if storeErr := state.StoreTupleState(instanceID, ts); storeErr != nil {
		log.Printf("WARN: %s: failed to persist EverHealthy: %v", instanceID, storeErr)
	}
}

// reconcilePartialRollback detects and recovers from partially-completed LV renames.
// Called during reconciliation startup. Handles crash recovery between LV rename steps.
// TODO: Implement actual LV state recovery. Currently only logs inconsistencies for GC.
func (m *AppManager) reconcilePartialRollback(ctx context.Context, state *FilesystemStateManager, instanceID string) error {
	ts, err := state.LoadTupleState(instanceID)
	if ts == nil || err != nil {
		return nil
	}

	// Check for "failed" generations that might indicate an incomplete rollback.
	// If a generation is marked "failed" but the snapshot LV still exists,
	// the rollback was interrupted between steps.
	rollbacker, ok := m.currentVolumeManager().(dataVolumeRollbacker)
	if !ok {
		return nil // no LV operations possible
	}
	_ = rollbacker // recovery uses lower-level LV operations if needed

	// For now, log any inconsistencies. The existing Attach/Detach methods
	// handle LV state recovery during normal volume mount operations.
	for _, gen := range ts.Generations {
		if gen.Status == TupleStatusFailed && gen.FailedLVName != "" {
			log.Printf("INFO: %s: found failed generation %s (LV: %s) — will be cleaned by GC",
				instanceID, gen.ID, gen.FailedLVName)
		}
	}

	return nil
}
