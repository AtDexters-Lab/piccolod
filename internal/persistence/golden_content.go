package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"piccolod/internal/state/paths"
)

func validateGoldenIdentity(identity GoldenContentIdentity) error {
	if strings.TrimSpace(identity.SourceKind) == "" {
		return fmt.Errorf("golden identity source kind is required")
	}
	if strings.TrimSpace(identity.ResolvedIdentity) == "" {
		return fmt.Errorf("golden identity resolved identity is required")
	}
	if strings.TrimSpace(identity.Projection) == "" {
		return fmt.Errorf("golden identity projection is required")
	}
	return nil
}

func canonicalGoldenIdentity(identity GoldenContentIdentity) (string, error) {
	if err := validateGoldenIdentity(identity); err != nil {
		return "", err
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal golden identity: %w", err)
	}
	return string(data), nil
}

func goldenReadyTimestamp(meta *volumeMetaV3) string {
	if meta == nil {
		return ""
	}
	if meta.MaterializeComplete != "" {
		return meta.MaterializeComplete
	}
	return meta.FlattenComplete
}

func goldenMetaMatchesIdentity(meta *volumeMetaV3, identity GoldenContentIdentity) bool {
	if meta == nil {
		return false
	}
	if meta.GoldenIdentity != nil {
		return *meta.GoldenIdentity == identity
	}
	// Compatibility for image goldens created before generic identities were
	// persisted. Their exact OCI image digest remains complete proof.
	return identity.SourceKind == GoldenSourceOCI &&
		identity.Projection == GoldenProjectionOCIImageRootfs &&
		meta.BaseImageDigest == identity.ResolvedIdentity
}

func goldenContentHash(identity GoldenContentIdentity) (string, error) {
	canonical, err := canonicalGoldenIdentity(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

func candidateGoldenIDs(identity GoldenContentIdentity, legacyImageDigest string) ([]string, error) {
	fullHash, err := goldenContentHash(identity)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, 15)
	seen := make(map[string]struct{})
	add := func(candidate string) {
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}
	if legacyImageDigest != "" {
		add(goldenLVPrefix + ShortDigest(legacyImageDigest))
	}
	for length := 12; length <= len(fullHash); length += 4 {
		add(goldenLVPrefix + fullHash[:length])
	}
	return candidates, nil
}

func goldenIdentityLockKeys(identity GoldenContentIdentity) ([]string, error) {
	legacyImageDigest := ""
	if identity.SourceKind == GoldenSourceOCI &&
		identity.Projection == GoldenProjectionOCIImageRootfs {
		legacyImageDigest = identity.ResolvedIdentity
	}
	candidates, err := candidateGoldenIDs(identity, legacyImageDigest)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		keys = append(keys, strings.TrimPrefix(candidate, goldenLVPrefix))
	}
	sort.Strings(keys)
	return keys, nil
}

// lockGoldenIdentity serializes every shortened storage candidate in stable
// order. Unrelated identities remain parallel, while a collision at any
// disambiguation depth cannot race selection, reference attachment, or GC.
func (m *luksVolumeManager) lockGoldenIdentity(identity GoldenContentIdentity) (func(), error) {
	keys, err := goldenIdentityLockKeys(identity)
	if err != nil {
		return nil, err
	}
	mutexes := make([]*sync.Mutex, 0, len(keys))
	for _, key := range keys {
		mutexes = append(mutexes, m.goldenMutex(key))
	}
	for _, mutex := range mutexes {
		mutex.Lock()
	}
	return func() {
		for index := len(mutexes) - 1; index >= 0; index-- {
			mutexes[index].Unlock()
		}
	}, nil
}

type goldenStorageSelection struct {
	goldenID string
	meta     *volumeMetaV3
	ready    bool
}

// selectGoldenStorage compares the complete durable identity before reuse.
// A shortened-key collision simply advances to a longer storage key.
func (m *luksVolumeManager) selectGoldenStorage(
	ctx context.Context,
	identity GoldenContentIdentity,
	legacyImageDigest,
	preferredGoldenID string,
) (goldenStorageSelection, error) {
	candidates, err := candidateGoldenIDs(identity, legacyImageDigest)
	if err != nil {
		return goldenStorageSelection{}, err
	}
	if preferredGoldenID != "" {
		valid := false
		for _, candidate := range candidates {
			if candidate == preferredGoldenID {
				valid = true
				break
			}
		}
		if !valid {
			return goldenStorageSelection{}, fmt.Errorf(
				"preferred golden ID %s is not derived from the requested identity",
				preferredGoldenID,
			)
		}
		candidates = []string{preferredGoldenID}
	}
	for _, goldenID := range candidates {
		m.mu.Lock()
		cached := m.goldenLVs[goldenID]
		m.mu.Unlock()
		if preferredGoldenID == "" &&
			goldenMetaMatchesIdentity(cached, identity) &&
			goldenReadyTimestamp(cached) != "" &&
			(m.lvMgr == nil || m.lvMgr.LVExists(ctx, goldenID)) {
			return goldenStorageSelection{goldenID: goldenID, meta: cached, ready: true}, nil
		}

		metaPath := filepath.Join(paths.VolumeMetaDir(goldenID), metadataV2File)
		meta, readErr := readVolumeMetaV3(metaPath)
		if readErr == nil {
			if !goldenMetaMatchesIdentity(meta, identity) {
				if preferredGoldenID == goldenID {
					return goldenStorageSelection{}, fmt.Errorf(
						"preferred golden ID %s is owned by different content",
						goldenID,
					)
				}
				continue
			}
			lvExists := m.lvMgr == nil || m.lvMgr.LVExists(ctx, goldenID)
			return goldenStorageSelection{
				goldenID: goldenID,
				meta:     meta,
				ready:    goldenReadyTimestamp(meta) != "" && lvExists,
			}, nil
		}
		if !os.IsNotExist(readErr) {
			// Corrupt or unreadable metadata is not identity proof. Preserve it
			// for reconciliation/diagnostics and disambiguate storage.
			if preferredGoldenID == goldenID {
				return goldenStorageSelection{}, fmt.Errorf(
					"preferred golden ID %s has unreadable metadata: %w",
					goldenID,
					readErr,
				)
			}
			continue
		}
		if m.lvMgr != nil && m.lvMgr.LVExists(ctx, goldenID) {
			// An LV without matching durable metadata is likewise not reusable.
			if preferredGoldenID == goldenID {
				// Exact reconstruction has a surviving reference containing
				// the complete identity and expected storage ID. Under every
				// candidate-identity lock, replace this unproven orphan rather
				// than disambiguating away from the durable reference.
				m.destroyGoldenLVUnsafe(ctx, goldenID)
				return goldenStorageSelection{goldenID: goldenID}, nil
			}
			continue
		}
		return goldenStorageSelection{goldenID: goldenID}, nil
	}
	return goldenStorageSelection{}, fmt.Errorf("unable to disambiguate golden storage identity")
}

// EnsureGoldenContent routes every source adapter through the same encrypted
// golden-LV staging/publication lifecycle used by OCI image rootfs content.
func (m *luksVolumeManager) EnsureGoldenContent(ctx context.Context, req GoldenContentRequest) (GoldenContentHandle, error) {
	if err := validateGoldenIdentity(req.Identity); err != nil {
		return GoldenContentHandle{}, err
	}
	if req.Materialize == nil {
		return GoldenContentHandle{}, fmt.Errorf("golden content materializer is required")
	}

	legacyDigest := ""
	if req.Identity.SourceKind == GoldenSourceOCI && req.Identity.Projection == GoldenProjectionOCIImageRootfs {
		legacyDigest = req.Identity.ResolvedIdentity
	}
	identity := req.Identity
	goldenID, err := m.EnsureGoldenLV(ctx, GoldenLVRequest{
		ImageDigest:       legacyDigest,
		ImageRef:          req.SourceRef,
		Identity:          &identity,
		SourceRef:         req.SourceRef,
		ContentSizeHint:   req.SizeHint,
		PreferredGoldenID: req.PreferredGoldenID,
		Materialize:       req.Materialize,
	})
	if err != nil {
		return GoldenContentHandle{}, err
	}
	return GoldenContentHandle{GoldenID: goldenID, Identity: identity}, nil
}
