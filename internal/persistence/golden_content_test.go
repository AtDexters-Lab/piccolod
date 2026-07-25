package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/testutil"
)

func TestOCIImageGoldenIdentityKeepsLegacyReuseKey(t *testing.T) {
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: "sha256:" + strings.Repeat("a", 64),
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	candidates, err := candidateGoldenIDs(identity, identity.ResolvedIdentity)
	if err != nil {
		t.Fatalf("candidateGoldenIDs: %v", err)
	}
	if len(candidates) == 0 || candidates[0] != goldenLVPrefix+ShortDigest(identity.ResolvedIdentity) {
		t.Fatalf("legacy image golden is not preferred: %v", candidates)
	}
	legacy := &volumeMetaV3{
		BaseImageDigest: identity.ResolvedIdentity,
		FlattenComplete: "2026-07-23T00:00:00Z",
	}
	if !goldenMetaMatchesIdentity(legacy, identity) {
		t.Fatalf("legacy image metadata did not prove identical generic identity")
	}
}

func TestGoldenIdentitySeparatesDifferentProjections(t *testing.T) {
	base := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("a", 40),
		Projection:       GoldenProjectionHuggingFace + ":models",
	}
	other := base
	other.Projection = GoldenProjectionHuggingFace + ":tokenizer"

	first, err := candidateGoldenIDs(base, "")
	if err != nil {
		t.Fatalf("first candidate: %v", err)
	}
	second, err := candidateGoldenIDs(other, "")
	if err != nil {
		t.Fatalf("second candidate: %v", err)
	}
	if first[0] == second[0] {
		t.Fatalf("different projections shared a preferred storage key")
	}
	if goldenMetaMatchesIdentity(&volumeMetaV3{GoldenIdentity: &base}, other) {
		t.Fatalf("different complete identity was accepted for reuse")
	}
}

func TestPreferredGoldenIDReplacesMetadataLessOrphan(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("a", 40),
		Projection:       GoldenProjectionHuggingFace + ":model.gguf",
	}
	candidates, err := candidateGoldenIDs(identity, "")
	if err != nil {
		t.Fatalf("candidateGoldenIDs: %v", err)
	}
	preferred := candidates[0]
	lvsKey := testutil.BuildKey("lvs", []string{"--noheadings", lvm.DefaultVGName + "/" + preferred})
	run := &testutil.FakeRunner{
		Errs: map[string]error{
			"lvs":  errors.New("missing"),
			lvsKey: nil,
		},
	}
	manager := &luksVolumeManager{
		run:       run,
		lvMgr:     lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
		goldenLVs: map[string]*volumeMetaV3{},
	}

	ordinary, err := manager.selectGoldenStorage(context.Background(), identity, "", "")
	if err != nil {
		t.Fatalf("ordinary selection: %v", err)
	}
	if ordinary.goldenID == preferred {
		t.Fatalf("ordinary selection reused unproven orphan %s", preferred)
	}
	manager.goldenLVs[preferred] = &volumeMetaV3{
		GoldenIdentity:      &identity,
		MaterializeComplete: time.Now().UTC().Format(time.RFC3339Nano),
	}

	exact, err := manager.selectGoldenStorage(context.Background(), identity, "", preferred)
	if err != nil {
		t.Fatalf("preferred selection: %v", err)
	}
	if exact.goldenID != preferred || exact.meta != nil || exact.ready {
		t.Fatalf("preferred selection = %+v, want empty exact slot %s", exact, preferred)
	}
	foundRemoval := false
	for _, call := range run.GetCalls() {
		if strings.Contains(call, "lvremove -f "+lvm.DefaultVGName+"/"+preferred) {
			foundRemoval = true
			break
		}
	}
	if !foundRemoval {
		t.Fatalf("metadata-less preferred orphan was not removed: %v", run.GetCalls())
	}
}

func TestFailedPreferredOrphanRemovalEvictsReadyCache(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceHuggingFace,
		ResolvedIdentity: strings.Repeat("b", 40),
		Projection:       GoldenProjectionHuggingFace + ":model.gguf",
	}
	candidates, err := candidateGoldenIDs(identity, "")
	if err != nil {
		t.Fatalf("candidateGoldenIDs: %v", err)
	}
	preferred := candidates[0]
	lvsKey := testutil.BuildKey("lvs", []string{"--noheadings", lvm.DefaultVGName + "/" + preferred})
	run := &testutil.FakeRunner{
		Errs: map[string]error{
			"lvs":      errors.New("missing"),
			lvsKey:     nil,
			"lvremove": errors.New("busy"),
		},
	}
	manager := &luksVolumeManager{
		run:   run,
		lvMgr: lvm.NewLVManager(run, lvm.DefaultVGName, lvm.DefaultThinPoolName),
		goldenLVs: map[string]*volumeMetaV3{
			preferred: {
				GoldenIdentity:      &identity,
				MaterializeComplete: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}

	if _, err := manager.selectGoldenStorage(context.Background(), identity, "", preferred); err != nil {
		t.Fatalf("preferred selection: %v", err)
	}
	if _, cached := manager.goldenLVs[preferred]; cached {
		t.Fatal("failed orphan removal retained Ready cache entry")
	}
	ordinary, err := manager.selectGoldenStorage(context.Background(), identity, "", "")
	if err != nil {
		t.Fatalf("ordinary selection after failed removal: %v", err)
	}
	if ordinary.goldenID == preferred {
		t.Fatalf("ordinary selection reused unproven orphan %s", preferred)
	}
}

func TestArtifactReferenceIDRejectsFilesystemSyntax(t *testing.T) {
	for _, referenceID := range []string{"", "../escape", "nested/reference", "bad\x00id"} {
		if validateArtifactReferenceID(referenceID) == nil {
			t.Fatalf("invalid reference ID accepted: %q", referenceID)
		}
	}
	if err := validateArtifactReferenceID("provider--artifact--model--abcdef"); err != nil {
		t.Fatalf("valid reference ID rejected: %v", err)
	}
}

func TestArtifactReferencePersistsCompleteGoldenIdentity(t *testing.T) {
	core := t.TempDir()
	paths.SetCoreRootForTest(t, core)
	if err := os.MkdirAll(artifactReferencesDir(), 0o700); err != nil {
		t.Fatalf("create reference directory: %v", err)
	}

	referenceID := "provider--artifact--model--abcdef"
	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: referenceID,
		GoldenID:    "golden-abcdef",
		Identity: GoldenContentIdentity{
			SourceKind:       GoldenSourceHuggingFace,
			ResolvedIdentity: strings.Repeat("a", 40),
			Projection:       GoldenProjectionHuggingFace + ":model.gguf",
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal reference: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactReferencesDir(), referenceID+".json"), data, 0o600); err != nil {
		t.Fatalf("write reference: %v", err)
	}
	loaded, err := loadArtifactReference(referenceID)
	if err != nil {
		t.Fatalf("load reference: %v", err)
	}
	if loaded.Identity != record.Identity {
		t.Fatalf("loaded identity = %+v, want %+v", loaded.Identity, record.Identity)
	}
}

func TestGoldenReferenceRecheckSeesNewRootfsMetadata(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	goldenID := "golden-rootfs-recheck"

	referenced, err := goldenHasRootfsReference(goldenID)
	if err != nil {
		t.Fatalf("initial rootfs reference check: %v", err)
	}
	if referenced {
		t.Fatal("golden unexpectedly referenced before rootfs metadata publication")
	}

	rootfsID := "svc-rootfs-provider--model"
	metaDir := paths.VolumeMetaDir(rootfsID)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("create rootfs metadata directory: %v", err)
	}
	if err := writeVolumeMetaV3(filepath.Join(metaDir, metadataV2File), &volumeMetaV3{
		Version:  metadataV3Version,
		Type:     volumeTypeServiceRootfs,
		LVName:   rootfsID,
		VGName:   lvm.DefaultVGName,
		FSType:   "btrfs",
		GoldenLV: goldenID,
	}); err != nil {
		t.Fatalf("publish rootfs metadata: %v", err)
	}

	referenced, err = goldenHasRootfsReference(goldenID)
	if err != nil {
		t.Fatalf("rootfs reference recheck: %v", err)
	}
	if !referenced {
		t.Fatal("rootfs reference published after initial inventory was not observed")
	}
}

func TestGoldenRootfsReferenceRecheckFailsClosedOnUnreadableMetadata(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	metaDir := paths.VolumeMetaDir("svc-rootfs-unreadable")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("create rootfs metadata directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, metadataV2File), []byte("{"), 0o600); err != nil {
		t.Fatalf("write unreadable rootfs metadata: %v", err)
	}

	if referenced, err := goldenHasRootfsReference("golden-maybe-referenced"); err == nil {
		t.Fatalf("unreadable rootfs metadata produced (%v, nil), want fail-closed error", referenced)
	}
}

func TestGoldenReferenceRecheckSeesNewArtifactRecord(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	goldenID := "golden-artifact-recheck"

	referenced, err := goldenHasArtifactReference(goldenID)
	if err != nil {
		t.Fatalf("initial artifact reference check: %v", err)
	}
	if referenced {
		t.Fatal("golden unexpectedly referenced before artifact record publication")
	}

	if err := os.MkdirAll(artifactReferencesDir(), 0o700); err != nil {
		t.Fatalf("create artifact reference directory: %v", err)
	}
	record := artifactReferenceRecord{
		Version:     artifactReferenceVersion,
		ReferenceID: "provider--artifact--model--recheck",
		GoldenID:    goldenID,
		Identity: GoldenContentIdentity{
			SourceKind:       GoldenSourceHuggingFace,
			ResolvedIdentity: strings.Repeat("c", 40),
			Projection:       GoldenProjectionHuggingFace + ":model.gguf",
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal artifact reference: %v", err)
	}
	if err := os.WriteFile(artifactReferencePath(record.ReferenceID), data, 0o600); err != nil {
		t.Fatalf("publish artifact reference: %v", err)
	}

	referenced, err = goldenHasArtifactReference(goldenID)
	if err != nil {
		t.Fatalf("artifact reference recheck: %v", err)
	}
	if !referenced {
		t.Fatal("artifact reference published after initial inventory was not observed")
	}
}

func TestFindGoldenByImageRefIgnoresNewerRawOCIArtifact(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	imageRef := "example.test/model:latest"
	imageID := "golden-image"
	artifactID := "golden-artifact"
	imageIdentity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: "sha256:image",
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	artifactIdentity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: "sha256:artifact",
		Projection:       GoldenProjectionOCIArtifact,
	}
	manager := &luksVolumeManager{goldenLVs: map[string]*volumeMetaV3{
		imageID: {
			BaseImageRef:        imageRef,
			BaseImageDigest:     imageIdentity.ResolvedIdentity,
			GoldenIdentity:      &imageIdentity,
			MaterializeComplete: "2026-07-24T10:00:00Z",
		},
		artifactID: {
			BaseImageRef:        imageRef,
			BaseImageDigest:     artifactIdentity.ResolvedIdentity,
			GoldenIdentity:      &artifactIdentity,
			MaterializeComplete: "2026-07-24T11:00:00Z",
		},
	}}
	writeGoldenImageConfigForLookupTest(t, imageID, []byte(`{"cmd":["serve"]}`))

	digest, goldenID, found := manager.FindGoldenByImageRef(imageRef)
	if !found || digest != imageIdentity.ResolvedIdentity || goldenID != imageID {
		t.Fatalf("FindGoldenByImageRef = (%q, %q, %v), want (%q, %q, true)",
			digest, goldenID, found, imageIdentity.ResolvedIdentity, imageID)
	}
}

func TestFindGoldenByImageRefDoesNotReturnRawOCIArtifactAlone(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	imageRef := "example.test/model:latest"
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: "sha256:artifact",
		Projection:       GoldenProjectionOCIArtifact,
	}
	manager := &luksVolumeManager{goldenLVs: map[string]*volumeMetaV3{
		"golden-artifact": {
			BaseImageRef:        imageRef,
			BaseImageDigest:     identity.ResolvedIdentity,
			GoldenIdentity:      &identity,
			MaterializeComplete: "2026-07-24T11:00:00Z",
		},
	}}

	if digest, goldenID, found := manager.FindGoldenByImageRef(imageRef); found {
		t.Fatalf("FindGoldenByImageRef = (%q, %q, true), want not found", digest, goldenID)
	}
}

func TestFindGoldenByImageRefReturnsTypedOCIImageProjection(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	imageRef := "example.test/model:latest"
	goldenID := "golden-image-as-artifact"
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: "sha256:image",
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	manager := &luksVolumeManager{goldenLVs: map[string]*volumeMetaV3{
		goldenID: {
			BaseImageRef:        imageRef,
			BaseImageDigest:     identity.ResolvedIdentity,
			GoldenIdentity:      &identity,
			MaterializeComplete: "2026-07-24T11:00:00Z",
		},
	}}
	writeGoldenImageConfigForLookupTest(t, goldenID, []byte(`{"entrypoint":["/entrypoint"]}`))

	digest, foundID, found := manager.FindGoldenByImageRef(imageRef)
	if !found || digest != identity.ResolvedIdentity || foundID != goldenID {
		t.Fatalf("FindGoldenByImageRef = (%q, %q, %v), want (%q, %q, true)",
			digest, foundID, found, identity.ResolvedIdentity, goldenID)
	}
}

func TestFindGoldenByImageRefSkipsMissingDigestOrImageConfig(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	imageRef := "example.test/model:latest"
	validID := "golden-valid"
	missingDigestID := "golden-missing-digest"
	missingConfigID := "golden-missing-config"
	invalidConfigID := "golden-invalid-config"
	manager := &luksVolumeManager{goldenLVs: map[string]*volumeMetaV3{
		validID: {
			BaseImageRef:    imageRef,
			BaseImageDigest: "sha256:valid",
			FlattenComplete: "2026-07-24T10:00:00Z",
		},
		missingDigestID: {
			BaseImageRef:    imageRef,
			FlattenComplete: "2026-07-24T11:00:00Z",
		},
		missingConfigID: {
			BaseImageRef:    imageRef,
			BaseImageDigest: "sha256:missing-config",
			FlattenComplete: "2026-07-24T12:00:00Z",
		},
		invalidConfigID: {
			BaseImageRef:    imageRef,
			BaseImageDigest: "sha256:invalid-config",
			FlattenComplete: "2026-07-24T13:00:00Z",
		},
	}}
	writeGoldenImageConfigForLookupTest(t, validID, []byte(`{"cmd":["serve"]}`))
	writeGoldenImageConfigForLookupTest(t, missingDigestID, []byte(`{"cmd":["serve"]}`))
	writeGoldenImageConfigForLookupTest(t, invalidConfigID, []byte(`{`))

	digest, goldenID, found := manager.FindGoldenByImageRef(imageRef)
	if !found || digest != "sha256:valid" || goldenID != validID {
		t.Fatalf("FindGoldenByImageRef = (%q, %q, %v), want (%q, %q, true)",
			digest, goldenID, found, "sha256:valid", validID)
	}
}

func TestFindGoldenByImageRefReturnsNewestValidImageProjection(t *testing.T) {
	paths.SetCoreRootForTest(t, t.TempDir())
	imageRef := "example.test/model:latest"
	legacyID := "golden-legacy"
	typedID := "golden-typed"
	identity := GoldenContentIdentity{
		SourceKind:       GoldenSourceOCI,
		ResolvedIdentity: "sha256:new",
		Projection:       GoldenProjectionOCIImageRootfs,
	}
	manager := &luksVolumeManager{goldenLVs: map[string]*volumeMetaV3{
		legacyID: {
			BaseImageRef:    imageRef,
			BaseImageDigest: "sha256:old",
			FlattenComplete: "2026-07-24T10:00:00Z",
		},
		typedID: {
			BaseImageRef:        imageRef,
			BaseImageDigest:     identity.ResolvedIdentity,
			GoldenIdentity:      &identity,
			MaterializeComplete: "2026-07-24T11:00:00Z",
		},
	}}
	writeGoldenImageConfigForLookupTest(t, legacyID, []byte(`{"cmd":["old"]}`))
	writeGoldenImageConfigForLookupTest(t, typedID, []byte(`{"cmd":["new"]}`))

	digest, goldenID, found := manager.FindGoldenByImageRef(imageRef)
	if !found || digest != identity.ResolvedIdentity || goldenID != typedID {
		t.Fatalf("FindGoldenByImageRef = (%q, %q, %v), want (%q, %q, true)",
			digest, goldenID, found, identity.ResolvedIdentity, typedID)
	}
}

func writeGoldenImageConfigForLookupTest(t *testing.T, goldenID string, data []byte) {
	t.Helper()
	dir := paths.VolumeMetaDir(goldenID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create golden metadata directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, imageConfigFile), data, 0o600); err != nil {
		t.Fatalf("write golden image config: %v", err)
	}
}
