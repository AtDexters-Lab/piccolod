package app

import (
	"context"
	"log"
	"time"
)

const (
	// gcDeprecatedRetention is how long deprecated generations are retained before removal.
	gcDeprecatedRetention = 30 * 24 * time.Hour

	// gcFailedRootfsRetention is how long failed rootfs volumes are retained.
	// Failed rootfs volumes have no CoW dependency to data volumes.
	gcFailedRootfsRetention = 7 * 24 * time.Hour

	// gcInterval is the minimum interval between GC runs per app.
	gcInterval = 24 * time.Hour
)

// garbageCollectGenerations removes expired deprecated and failed generations (RFC 20260302 §7).
func (m *AppManager) garbageCollectGenerations(ctx context.Context, state *FilesystemStateManager, instanceID string) error {
	ts, err := state.LoadTupleState(instanceID)
	if ts == nil || err != nil {
		return nil
	}

	// Rate-limit GC to once per day.
	if ts.LastGCAt != nil && time.Since(*ts.LastGCAt) < gcInterval {
		return nil
	}

	snapshotter, _ := m.currentVolumeManager().(dataVolumeSnapshotter)
	rootfs := m.currentRootfsManager()

	// Build set of rootfs volume IDs still referenced by active or snapshot generations.
	// Unchanged services share the same rootfs volume across snapshot and active generations,
	// so we must not destroy a volume that's still in use by a live generation.
	inUseRootfs := make(map[string]bool)
	for _, gen := range ts.Generations {
		if gen.Status == TupleStatusActive || gen.Status == TupleStatusSnapshot {
			for _, volID := range gen.RootfsVolIDs {
				inUseRootfs[volID] = true
			}
		}
	}

	var kept []TupleGeneration
	changed := false

	for _, gen := range ts.Generations {
		switch gen.Status {
		case TupleStatusDeprecated:
			if gen.DeprecatedAt != nil && time.Since(*gen.DeprecatedAt) > gcDeprecatedRetention {
				// Remove data snapshot LV.
				if gen.DataSnapshot != "" && snapshotter != nil {
					if err := snapshotter.DestroyDataSnapshot(ctx, gen.DataSnapshot); err != nil {
						log.Printf("WARN: GC %s: destroy data snapshot %s: %v", instanceID, gen.DataSnapshot, err)
						kept = append(kept, gen) // retain on failure
						continue
					}
					log.Printf("INFO: GC %s: removed data snapshot %s", instanceID, gen.DataSnapshot)
				}
				// Remove old rootfs volumes (skip those still referenced by live generations).
				rootfsFailed := false
				if rootfs != nil {
					for svcName, volID := range gen.RootfsVolIDs {
						if inUseRootfs[volID] {
							continue // still referenced by active or snapshot generation
						}
						if err := rootfs.DestroyRootfs(ctx, volID); err != nil {
							log.Printf("WARN: GC %s: destroy rootfs %s (service %s): %v", instanceID, volID, svcName, err)
							rootfsFailed = true
						} else {
							log.Printf("INFO: GC %s: removed rootfs %s", instanceID, volID)
						}
					}
				}
				if rootfsFailed {
					// Retain generation to avoid orphaning rootfs volumes.
					gen.DataSnapshot = "" // data snapshot already destroyed above
					changed = true
					kept = append(kept, gen)
					continue
				}
				changed = true
				log.Printf("INFO: GC %s: removed deprecated generation %s", instanceID, gen.ID)
				continue // don't keep
			}
			kept = append(kept, gen)

		case TupleStatusFailed:
			// Failed rootfs: remove after 7 days from failure time.
			failedTime := gen.CreatedAt // fallback for pre-existing generations
			if gen.FailedAt != nil {
				failedTime = *gen.FailedAt
			}
			if failedTime.Add(gcFailedRootfsRetention).Before(time.Now()) && rootfs != nil && len(gen.RootfsVolIDs) > 0 {
				remaining := make(map[string]string)
				for svcName, volID := range gen.RootfsVolIDs {
					if inUseRootfs[volID] {
						continue // still referenced by active or snapshot generation
					}
					if err := rootfs.DestroyRootfs(ctx, volID); err != nil {
						log.Printf("WARN: GC %s: destroy failed rootfs %s (service %s): %v", instanceID, volID, svcName, err)
						remaining[svcName] = volID // retain for retry
					} else {
						log.Printf("INFO: GC %s: removed failed rootfs %s", instanceID, volID)
					}
				}
				if len(remaining) == 0 {
					gen.RootfsVolIDs = nil
				} else {
					gen.RootfsVolIDs = remaining
				}
				changed = true
			}

			// Failed data LVs: retain until a new snapshot exists from the promoted volume.
			// Per RFC §7.3 — avoids transitive CoW chain issues.
			if gen.FailedLVName != "" && ts.HasSnapshotSinceGeneration(gen.ID) && snapshotter != nil {
				if err := snapshotter.DestroyDataSnapshot(ctx, gen.FailedLVName); err != nil {
					log.Printf("WARN: GC %s: destroy failed data LV %s: %v", instanceID, gen.FailedLVName, err)
				} else {
					log.Printf("INFO: GC %s: removed failed data LV %s", instanceID, gen.FailedLVName)
					gen.FailedLVName = ""
					changed = true
				}
			}

			// Remove generation entry only when all resources are cleaned.
			if len(gen.RootfsVolIDs) == 0 && gen.FailedLVName == "" && gen.DataSnapshot == "" {
				changed = true
				log.Printf("INFO: GC %s: removed failed generation %s", instanceID, gen.ID)
				continue // don't keep
			}
			kept = append(kept, gen)

		default:
			kept = append(kept, gen)
		}
	}

	if changed {
		now := time.Now()
		ts.LastGCAt = &now
		ts.Generations = kept
		if err := state.StoreTupleState(instanceID, ts); err != nil {
			log.Printf("WARN: GC %s: persist tuple state: %v", instanceID, err)
			return err
		}
	} else {
		// Update LastGCAt even if nothing changed, to avoid re-scanning.
		now := time.Now()
		ts.LastGCAt = &now
		if err := state.StoreTupleState(instanceID, ts); err != nil {
			log.Printf("WARN: GC %s: persist GC timestamp: %v", instanceID, err)
		}
	}

	// Trigger golden LV GC (cleans orphaned golden images).
	if rootfs != nil {
		if gcErr := rootfs.GarbageCollectGoldenLVs(ctx); gcErr != nil {
			log.Printf("WARN: GC %s: golden LV GC: %v", instanceID, gcErr)
		}
	}

	return nil
}
