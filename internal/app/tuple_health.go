package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	// healthVerificationDuration is how long an active generation must run healthy
	// before the previous snapshot is deprecated (RFC 20260302 Phase 4).
	healthVerificationDuration = 24 * time.Hour
)

// recordFailedRollbackLV gives tuple GC durable ownership of the displaced
// data LV. The synthetic path covers first-update failures where no active
// generation was recorded before rollback began.
func recordFailedRollbackLV(ts *TupleState, trackingGen *TupleGeneration, failedLVName string, failedGenNumber int) *TupleGeneration {
	if trackingGen == nil {
		for i := range ts.Generations {
			gen := &ts.Generations[i]
			if gen.Status == TupleStatusFailed && strings.TrimSpace(gen.FailedLVName) == failedLVName {
				trackingGen = gen
				break
			}
		}
	}
	if trackingGen == nil {
		failedNow := time.Now()
		ts.Generations = append(ts.Generations, TupleGeneration{
			ID:           fmt.Sprintf("gen-failed-%d", failedGenNumber),
			Status:       TupleStatusFailed,
			FailedLVName: failedLVName,
			FailedAt:     &failedNow,
			CreatedAt:    failedNow,
		})
		return &ts.Generations[len(ts.Generations)-1]
	}
	trackingGen.Status = TupleStatusFailed
	trackingGen.FailedLVName = failedLVName
	if trackingGen.FailedAt == nil {
		failedNow := time.Now()
		trackingGen.FailedAt = &failedNow
	}
	return trackingGen
}

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

// commitRollbackAppState copies tuple-authoritative rollback state into the
// split app.yaml/metadata.json representation, then records that both files
// are durable. Runtime reacquisition is forbidden until the final tuple write.
func (m *AppManager) commitRollbackAppState(state *FilesystemStateManager, appInst *AppInstance, ts *TupleState, active *TupleGeneration) error {
	if active == nil || active.RollbackAppStateCommitted == nil || *active.RollbackAppStateCommitted {
		return nil
	}

	prevDef, err := state.GetPreviousAppDefinition(appInst.InstanceID)
	if err != nil {
		return fmt.Errorf("load previous definition for rollback generation %s: %w", active.ID, err)
	}
	candidate, err := detachedAppCandidate(appInst)
	if err != nil {
		return err
	}
	candidate.ActiveRootfs = make(map[string]string, len(active.RootfsVolIDs))
	for serviceName, volumeID := range active.RootfsVolIDs {
		candidate.ActiveRootfs[serviceName] = volumeID
	}
	candidate.Definition = prevDef
	candidate.UpdatedAt = time.Now()
	if err := commitDetachedApp(state, appInst, candidate); err != nil {
		return fmt.Errorf("store tuple-authoritative rollback app state: %w", err)
	}

	committed := true
	active.RollbackAppStateCommitted = &committed
	if err := state.StoreTupleState(appInst.InstanceID, ts); err != nil {
		return fmt.Errorf("persist rollback app-state commit marker: %w", err)
	}
	return nil
}

// reconcilePartialRollback runs before volume-layout or runtime acquisition.
// It resumes an interrupted active->failed, snapshot->active LV swap and then
// completes the tuple-authoritative app.yaml/metadata.json commit protocol.
// The boolean result reports that a pending rollback was completed.
func (m *AppManager) reconcilePartialRollback(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) (bool, error) {
	instanceID := appInst.InstanceID
	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return false, fmt.Errorf("load tuple state for rollback recovery: %w", err)
	}
	if ts == nil {
		return false, nil
	}

	current := ts.GenerationByID(ts.CurrentGeneration)
	snapshot := ts.LatestSnapshot()
	failedLVName := ""
	pendingLVSwap := false
	if snapshot != nil && strings.TrimSpace(snapshot.DataSnapshot) != "" {
		failedLVName = strings.TrimSpace(snapshot.RollbackFailedLVName)
		pendingLVSwap = snapshot.RollbackPending && failedLVName != ""
		// Compatibility with a partial swap recorded before the explicit
		// pre-LV intent fields were introduced.
		if !pendingLVSwap && current != nil && current.Status == TupleStatusFailed {
			failedLVName = strings.TrimSpace(current.FailedLVName)
			pendingLVSwap = failedLVName != ""
		}
	}
	active := ts.ActiveGeneration()
	pendingAppCommit := active != nil && active.RollbackAppStateCommitted != nil && !*active.RollbackAppStateCommitted
	if !pendingLVSwap && !pendingAppCommit {
		return false, nil
	}

	// Do not consult the possibly-missing app LV or rootless Podman store: PID 1
	// owns the authoritative process-absence proof at this recovery boundary.
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}
	if err := m.quiesceAppUserSession(ctx, instanceID); err != nil {
		return false, fmt.Errorf("quiesce user session before rollback recovery: %w", err)
	}

	if pendingLVSwap {
		rollbacker, ok := m.currentVolumeManager().(dataVolumeRollbacker)
		if !ok {
			return false, fmt.Errorf("volume manager does not support rollback recovery")
		}
		renamed, promoted, rollbackErr := rollbacker.RollbackDataVolume(ctx, instanceID, snapshot.DataSnapshot, failedLVName)
		if rollbackErr != nil && !renamed {
			return false, fmt.Errorf("resume partial data-volume rollback: %w", rollbackErr)
		}
		trackingGen := current
		if trackingGen == nil {
			trackingGen = active
		}
		if renamed {
			failedGenNumber := ts.NextGenNumber - 1
			if failedGenNumber < 1 {
				failedGenNumber = 1
			}
			recordFailedRollbackLV(ts, trackingGen, failedLVName, failedGenNumber)
			// The synthetic append can reallocate the generation slice.
			snapshot = ts.LatestSnapshot()
			if snapshot == nil {
				return false, fmt.Errorf("rollback snapshot lost while tracking failed LV")
			}
		}
		if !promoted {
			if storeErr := state.StoreTupleState(instanceID, ts); storeErr != nil {
				return false, fmt.Errorf("persist still-partial rollback state: %w", storeErr)
			}
			if rollbackErr == nil {
				rollbackErr = fmt.Errorf("snapshot promotion did not commit")
			}
			return false, fmt.Errorf("resume partial data-volume rollback: %w", rollbackErr)
		}
		if rollbackErr != nil {
			// Both renames are durable. A following ensureAppVolumeLayout call is
			// the authoritative retry for an attach-only failure.
			log.Printf("WARN: rollback %s: promotion recovered with attach error; retrying layout: %v", instanceID, rollbackErr)
		}

		snapshot.Status = TupleStatusActive
		snapshot.DataSnapshot = ""
		snapshot.RollbackAttempted = true
		snapshot.RollbackPending = false
		snapshot.RollbackFailedLVName = ""
		appStateCommitted := false
		snapshot.RollbackAppStateCommitted = &appStateCommitted
		ts.CurrentGeneration = snapshot.ID
		if err := state.StoreTupleState(instanceID, ts); err != nil {
			return false, fmt.Errorf("persist promoted rollback tuple during recovery: %w", err)
		}
		active = snapshot
	}

	if err := m.commitRollbackAppState(state, appInst, ts, active); err != nil {
		return false, err
	}
	return true, nil
}
