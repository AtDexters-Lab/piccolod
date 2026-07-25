package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/fsutil"
	"piccolod/internal/state/paths"
)

const artifactReferenceVersion = 1

type artifactReferenceRecord struct {
	Version     int                   `json:"version"`
	ReferenceID string                `json:"reference_id"`
	GoldenID    string                `json:"golden_id"`
	Identity    GoldenContentIdentity `json:"identity"`
	IDMap       IDMapMeta             `json:"idmap"`
}

func artifactReferencesDir() string {
	return paths.CoreJoin("golden-references")
}

func artifactReferencePath(referenceID string) string {
	return filepath.Join(artifactReferencesDir(), referenceID+".json")
}

func artifactMountPath(referenceID string) string {
	return paths.CoreJoin("mounts", "artifact-"+referenceID)
}

func validateArtifactReferenceID(referenceID string) error {
	if referenceID == "" || len(referenceID) > 180 {
		return fmt.Errorf("artifact reference ID must be 1..180 characters")
	}
	if filepath.Base(referenceID) != referenceID || strings.ContainsAny(referenceID, `/\`+"\x00") {
		return fmt.Errorf("artifact reference ID contains a path separator")
	}
	return nil
}

func idMapMeta(config IDMapConfig) IDMapMeta {
	return IDMapMeta{
		AppUID:      config.AppUID,
		AppGID:      config.AppGID,
		SubUIDStart: config.SubUIDStart,
		SubUIDCount: config.SubUIDCount,
		SubGIDStart: config.SubGIDStart,
		SubGIDCount: config.SubGIDCount,
	}
}

func idMapConfig(meta IDMapMeta) IDMapConfig {
	return IDMapConfig{
		AppUID:      meta.AppUID,
		AppGID:      meta.AppGID,
		SubUIDStart: meta.SubUIDStart,
		SubUIDCount: meta.SubUIDCount,
		SubGIDStart: meta.SubGIDStart,
		SubGIDCount: meta.SubGIDCount,
	}
}

func loadArtifactReference(referenceID string) (*artifactReferenceRecord, error) {
	if err := validateArtifactReferenceID(referenceID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(artifactReferencePath(referenceID))
	if err != nil {
		return nil, err
	}
	var record artifactReferenceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse artifact reference %s: %w", referenceID, err)
	}
	if record.Version != artifactReferenceVersion || record.ReferenceID != referenceID || record.GoldenID == "" {
		return nil, fmt.Errorf("artifact reference %s has invalid durable identity", referenceID)
	}
	if err := validateGoldenIdentity(record.Identity); err != nil {
		return nil, fmt.Errorf("artifact reference %s has invalid content identity: %w", referenceID, err)
	}
	return &record, nil
}

func goldenIdentityFromMeta(meta *volumeMetaV3) (GoldenContentIdentity, error) {
	if meta == nil {
		return GoldenContentIdentity{}, fmt.Errorf("golden metadata is required")
	}
	if meta.GoldenIdentity != nil {
		if err := validateGoldenIdentity(*meta.GoldenIdentity); err != nil {
			return GoldenContentIdentity{}, err
		}
		return *meta.GoldenIdentity, nil
	}
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: meta.BaseImageDigest,
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	if err := validateGoldenIdentity(identity); err != nil {
		return GoldenContentIdentity{}, err
	}
	return identity, nil
}

func listArtifactReferences() ([]artifactReferenceRecord, error) {
	entries, err := os.ReadDir(artifactReferencesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]artifactReferenceRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		referenceID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := loadArtifactReference(referenceID)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, nil
}

func (m *luksVolumeManager) CreateArtifactReference(ctx context.Context, req ArtifactReferenceRequest) (ArtifactHandle, error) {
	if err := validateArtifactReferenceID(req.ReferenceID); err != nil {
		return ArtifactHandle{}, err
	}
	if !strings.HasPrefix(req.GoldenID, goldenLVPrefix) {
		return ArtifactHandle{}, fmt.Errorf("artifact reference requires a golden LV")
	}
	meta, err := readVolumeMetaV3(filepath.Join(paths.VolumeMetaDir(req.GoldenID), metadataV2File))
	if err != nil {
		return ArtifactHandle{}, fmt.Errorf("read referenced golden metadata: %w", err)
	}
	if meta.Type != volumeTypeGolden || goldenReadyTimestamp(meta) == "" {
		return ArtifactHandle{}, fmt.Errorf("artifact reference requires verified Ready golden content")
	}
	identity, err := goldenIdentityFromMeta(meta)
	if err != nil {
		return ArtifactHandle{}, fmt.Errorf("read referenced golden identity: %w", err)
	}

	lock := m.lockFor("artifact-reference-" + req.ReferenceID)
	lock.Lock()
	defer lock.Unlock()

	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: req.ReferenceID,
		GoldenID:    req.GoldenID,
		Identity:    identity,
		IDMap:       idMapMeta(req.IDMap),
	}
	created := false
	if existing, loadErr := loadArtifactReference(req.ReferenceID); loadErr == nil {
		if existing.GoldenID != record.GoldenID ||
			existing.Identity != record.Identity ||
			existing.IDMap != record.IDMap {
			return ArtifactHandle{}, fmt.Errorf("artifact reference %s already names different durable content", req.ReferenceID)
		}
		record = *existing
	} else if !os.IsNotExist(loadErr) {
		return ArtifactHandle{}, loadErr
	} else {
		if err := os.MkdirAll(artifactReferencesDir(), 0o700); err != nil {
			return ArtifactHandle{}, fmt.Errorf("create artifact reference directory: %w", err)
		}
		data, err := json.Marshal(record)
		if err != nil {
			return ArtifactHandle{}, fmt.Errorf("marshal artifact reference: %w", err)
		}
		if err := fsutil.AtomicWriteFile(artifactReferencePath(req.ReferenceID), data, 0o600); err != nil {
			return ArtifactHandle{}, fmt.Errorf("write artifact reference: %w", err)
		}
		created = true
	}

	handle, err := m.attachArtifactReferenceLocked(ctx, &record)
	if err != nil && created {
		_ = os.Remove(artifactReferencePath(req.ReferenceID))
	}
	handle.Created = created
	return handle, err
}

func (m *luksVolumeManager) AttachArtifactReference(ctx context.Context, referenceID string) (ArtifactHandle, error) {
	lock := m.lockFor("artifact-reference-" + referenceID)
	lock.Lock()
	defer lock.Unlock()

	record, err := loadArtifactReference(referenceID)
	if err != nil {
		return ArtifactHandle{}, err
	}
	return m.attachArtifactReferenceLocked(ctx, record)
}

func (m *luksVolumeManager) attachArtifactReferenceLocked(ctx context.Context, record *artifactReferenceRecord) (ArtifactHandle, error) {
	unlockIdentity, err := m.lockGoldenIdentity(record.Identity)
	if err != nil {
		return ArtifactHandle{}, err
	}
	defer unlockIdentity()

	meta, err := readVolumeMetaV3(filepath.Join(paths.VolumeMetaDir(record.GoldenID), metadataV2File))
	if os.IsNotExist(err) ||
		(err == nil && m.lvMgr != nil && !m.lvMgr.LVExists(ctx, record.GoldenID)) {
		return ArtifactHandle{}, &ArtifactContentMissingError{
			ReferenceID: record.ReferenceID,
			GoldenID:    record.GoldenID,
			Identity:    record.Identity,
		}
	}
	if err != nil {
		return ArtifactHandle{}, fmt.Errorf("read referenced golden metadata: %w", err)
	}
	if meta.Type != volumeTypeGolden ||
		goldenReadyTimestamp(meta) == "" ||
		!goldenMetaMatchesIdentity(meta, record.Identity) {
		return ArtifactHandle{}, fmt.Errorf("artifact reference %s no longer matches verified Ready content", record.ReferenceID)
	}

	goldenHandle, err := m.AttachRootfs(ctx, record.GoldenID)
	if err != nil {
		return ArtifactHandle{}, fmt.Errorf("attach golden content %s: %w", record.GoldenID, err)
	}
	if err := fsutil.ValidateArtifactTree(goldenHandle.MountPath); err != nil {
		_ = m.detachGoldenIfUnused(ctx, record.GoldenID, record.ReferenceID)
		return ArtifactHandle{}, fmt.Errorf("validate artifact content %s: %w", record.ReferenceID, err)
	}

	target := artifactMountPath(record.ReferenceID)
	expectedSource := "/dev/mapper/" + volMapperName(record.GoldenID)
	mounted, entry, probeErr := mountAtPath(target)
	if probeErr != nil {
		return ArtifactHandle{}, fmt.Errorf("probe artifact mount %s: %w", target, probeErr)
	}
	if mounted {
		if entry.Source != expectedSource {
			return ArtifactHandle{}, fmt.Errorf("foreign mount at artifact path %s", target)
		}
		return ArtifactHandle{MountPath: target}, nil
	}

	if err := fsutil.CreateIDMappedMount(goldenHandle.MountPath, target, idMapConfig(record.IDMap)); err != nil {
		_ = m.detachGoldenIfUnused(ctx, record.GoldenID, record.ReferenceID)
		return ArtifactHandle{}, fmt.Errorf("create artifact idmapped mount: %w", err)
	}
	return ArtifactHandle{MountPath: target}, nil
}

func (m *luksVolumeManager) DetachArtifactReference(ctx context.Context, referenceID string) error {
	lock := m.lockFor("artifact-reference-" + referenceID)
	lock.Lock()
	defer lock.Unlock()

	record, err := loadArtifactReference(referenceID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return m.detachArtifactReferenceLocked(ctx, record)
}

func (m *luksVolumeManager) detachArtifactReferenceLocked(ctx context.Context, record *artifactReferenceRecord) error {
	unlockIdentity, err := m.lockGoldenIdentity(record.Identity)
	if err != nil {
		return err
	}
	defer unlockIdentity()

	target := artifactMountPath(record.ReferenceID)
	mounted, _, probeErr := mountAtPath(target)
	if probeErr != nil {
		return fmt.Errorf("probe artifact mount %s: %w", target, probeErr)
	}
	if mounted {
		if err := m.run.Run(ctx, "umount", target); err != nil {
			return fmt.Errorf("unmount artifact reference %s: %w", record.ReferenceID, err)
		}
	}
	_ = os.RemoveAll(target)
	return m.detachGoldenIfUnused(ctx, record.GoldenID, record.ReferenceID)
}

func (m *luksVolumeManager) detachGoldenIfUnused(ctx context.Context, goldenID, excludingReferenceID string) error {
	records, err := listArtifactReferences()
	if err != nil {
		return err
	}
	for _, other := range records {
		if other.ReferenceID == excludingReferenceID || other.GoldenID != goldenID {
			continue
		}
		if mounted, _, probeErr := mountAtPath(artifactMountPath(other.ReferenceID)); probeErr == nil && mounted {
			return nil
		}
	}
	return m.DetachRootfs(ctx, goldenID)
}

func (m *luksVolumeManager) DestroyArtifactReference(ctx context.Context, referenceID string) error {
	lock := m.lockFor("artifact-reference-" + referenceID)
	lock.Lock()
	defer lock.Unlock()

	record, err := loadArtifactReference(referenceID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := m.detachArtifactReferenceLocked(ctx, record); err != nil {
		return err
	}
	if err := os.Remove(artifactReferencePath(referenceID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove artifact reference %s: %w", referenceID, err)
	}
	return nil
}

func (m *luksVolumeManager) GarbageCollectArtifactReferences(ctx context.Context, retained map[string]struct{}) error {
	records, err := listArtifactReferences()
	if err != nil {
		return err
	}
	var errs []error
	removed := false
	for _, record := range records {
		if _, keep := retained[record.ReferenceID]; keep {
			continue
		}
		// An uncommitted container create can succeed ambiguously while its
		// cleanup fails. In that state the reference is intentionally absent
		// from committed app metadata, but the mounted projection may still be
		// owned by a live process. Generic GC has no process-absence proof, so
		// it may collect only already-unmounted orphan references.
		mounted, _, probeErr := mountAtPath(artifactMountPath(record.ReferenceID))
		if probeErr != nil {
			errs = append(errs, fmt.Errorf("%s: probe artifact mount: %w", record.ReferenceID, probeErr))
			continue
		}
		if mounted {
			continue
		}
		if err := m.DestroyArtifactReference(ctx, record.ReferenceID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", record.ReferenceID, err))
			continue
		}
		removed = true
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if removed {
		return m.GarbageCollectGoldenLVs(ctx)
	}
	return nil
}
