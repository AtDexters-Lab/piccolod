package persistence

import (
	"encoding/json"
	"fmt"
	"log"

	"piccolod/internal/fsutil"
)

// App rootfs volumes and artifact references reach containers through
// idmapped mounts. On-disk owners are container-relative, so the correct map
// is always the owning app user's live one: container root to the user's
// UID/GID and container IDs 1..N to its subordinate ranges. Callers pass that
// live map on every create, clone, and attach. Metadata only records the map
// the mount was last built with, which is how a live mount built for an
// older identity is recognized.

// liveIDMapChanged reports whether a rootfs volume's recorded ID map must
// follow the caller's live map before the volume attaches.
func liveIDMapChanged(volumeID string, meta *volumeMetaV3, live *IDMapConfig) (bool, error) {
	if meta.Type == volumeTypeGolden {
		if live != nil {
			return false, fmt.Errorf("rootfs %s: golden content is shared and never takes an app ID map", volumeID)
		}
		return false, nil
	}
	if live == nil {
		if meta.IDMap != nil {
			return false, fmt.Errorf("rootfs %s: attach requires the owning app user's live ID map", volumeID)
		}
		return false, nil
	}
	return meta.IDMap == nil || *meta.IDMap != idMapMeta(*live), nil
}

// writeLiveIDMap records the app user's live map as the volume's applied map.
// It is the only metadata writer allowed to change a persisted map; every
// other writer goes through writeVolumeMetaV3 and its ErrIDMapImmutable
// guard. Callers hold the per-volume lock and have proven that no idmapped
// mount built from the old record is live.
func writeLiveIDMap(path, volumeID string, meta *volumeMetaV3, live IDMapConfig) error {
	next := *meta
	applied := idMapMeta(live)
	next.IDMap = &applied
	next.IDMapFingerprint = computeIDMapFingerprint(next.IDMap)
	data, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("rootfs %s: record live ID map: %w", volumeID, err)
	}
	log.Printf("INFO: rootfs %s: ID map follows its app user: %s -> %s",
		volumeID, describeIDMap(meta.IDMap), describeIDMap(next.IDMap))
	*meta = next
	return nil
}

// staleIDMapMountError reports a live idmapped mount whose recorded map the
// app user no longer has.
func staleIDMapMountError(subject string, recorded *IDMapMeta, live IDMapConfig) error {
	current := idMapMeta(live)
	return fmt.Errorf("%w: %s is mounted with %s but its app user is now %s; stop the app so the mount is rebuilt",
		ErrIDMapStaleMount, subject, describeIDMap(recorded), describeIDMap(&current))
}

func describeIDMap(m *IDMapMeta) string {
	if m == nil {
		return "no ID map"
	}
	return fmt.Sprintf("uid %d gid %d subuid %d+%d subgid %d+%d",
		m.AppUID, m.AppGID, m.SubUIDStart, m.SubUIDCount, m.SubGIDStart, m.SubGIDCount)
}
