package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/state/paths"
)

// Model attachment generations: detach removes mounted content, and a later
// attach returns a different populated mount path for the same durable volume.
// The ordinary stub only records detach, which cannot expose stale handles.
type attachmentBoundaryRootfs struct {
	*stubRootfsManager
	persistence.GoldenContentManager
	artifactAttaches []string
	attached         []string
	mounts           map[string][]string
	latest           map[string]persistence.RootfsHandle
	attachErr        error
	attachResult     *persistence.RootfsHandle
	writable         map[string]bool
}

func newAttachmentBoundaryRootfs(t *testing.T) *attachmentBoundaryRootfs {
	t.Helper()
	return &attachmentBoundaryRootfs{
		stubRootfsManager: newStubRootfsManager(t.TempDir()),
		mounts:            map[string][]string{}, latest: map[string]persistence.RootfsHandle{},
	}
}

func (s *attachmentBoundaryRootfs) mount(volumeID string) (persistence.RootfsHandle, error) {
	mp := filepath.Join(s.baseDir, "attachments", fmt.Sprintf("%d", len(s.mounts[volumeID])), volumeID)
	if err := os.MkdirAll(mp, 0755); err != nil {
		return persistence.RootfsHandle{}, err
	}
	for _, name := range []string{"pause", "mounted-content"} {
		if err := os.WriteFile(filepath.Join(mp, name), []byte("mounted"), 0755); err != nil {
			return persistence.RootfsHandle{}, err
		}
	}
	h := persistence.RootfsHandle{VolumeID: volumeID, MountPath: mp, ReadOnly: !s.writable[volumeID], GoldenLV: "config-" + volumeID}
	s.mounts[volumeID] = append(s.mounts[volumeID], mp)
	s.latest[volumeID] = h
	return h, nil
}

func (s *attachmentBoundaryRootfs) AttachRootfs(ctx context.Context, volumeID string) (persistence.RootfsHandle, error) {
	s.attached = append(s.attached, volumeID)
	if s.attachErr != nil {
		return persistence.RootfsHandle{}, s.attachErr
	}
	if s.attachResult != nil {
		return *s.attachResult, nil
	}
	if _, err := s.stubRootfsManager.AttachRootfs(ctx, volumeID); err != nil {
		return persistence.RootfsHandle{}, err
	}
	return s.mount(volumeID)
}

func (s *attachmentBoundaryRootfs) AttachArtifactReference(_ context.Context, referenceID string) (persistence.ArtifactHandle, error) {
	s.artifactAttaches = append(s.artifactAttaches, referenceID)
	return persistence.ArtifactHandle{}, errors.New("artifact preparation must follow rootfs validation")
}

func (s *attachmentBoundaryRootfs) CreateServiceRootfs(ctx context.Context, req persistence.ServiceRootfsRequest) (persistence.RootfsHandle, error) {
	h, err := s.stubRootfsManager.CreateServiceRootfs(ctx, req)
	if err != nil {
		return h, err
	}
	return s.mount(h.VolumeID)
}

func (s *attachmentBoundaryRootfs) DetachRootfs(ctx context.Context, volumeID string) error {
	if err := s.stubRootfsManager.DetachRootfs(ctx, volumeID); err != nil {
		return err
	}
	for _, mp := range s.mounts[volumeID] {
		if err := os.RemoveAll(mp); err != nil {
			return err
		}
	}
	return nil
}

func (s *attachmentBoundaryRootfs) ReadGoldenImageConfig(_ context.Context, goldenID string) (persistence.GoldenImageConfig, error) {
	s.goldenReads = append(s.goldenReads, goldenID)
	if strings.Contains(goldenID, "anchor") {
		return persistence.GoldenImageConfig{Entrypoint: []string{"/pause"}}, nil
	}
	return persistence.GoldenImageConfig{Cmd: []string{"/bin/sh"}}, nil
}

type attachmentBoundaryContainers struct {
	*MockContainerManager
	creates []container.ContainerCreateSpec
}

func (m *attachmentBoundaryContainers) CreateContainer(ctx context.Context, rt container.PodmanRuntime, spec container.ContainerCreateSpec) (string, error) {
	m.creates = append(m.creates, spec)
	name := "mounted-content"
	if len(spec.Entrypoint) > 0 && spec.Entrypoint[0] == "/pause" {
		name = "pause"
	}
	if _, err := os.Stat(filepath.Join(spec.Rootfs, name)); err != nil {
		return "", fmt.Errorf("rootfs executable %s unavailable at consumed mount %s: %w", name, spec.Rootfs, err)
	}
	return m.MockContainerManager.CreateContainer(ctx, rt, spec)
}

func TestApplyCustomManifestUpdateReacquiresStagedRootfsAfterQuiesce(t *testing.T) {
	for _, sameDigestAlias := range []bool{false, true} {
		name := "changed-main-unchanged-anchor-and-sidecar"
		if sameDigestAlias {
			name = "same-digest-alias"
		}
		t.Run(name, func(t *testing.T) {
			tempDir := t.TempDir()
			paths.SetCoreRootForTest(t, tempDir)
			mock := &attachmentBoundaryContainers{MockContainerManager: NewMockContainerManager()}
			mock.inspectImageHook = func(image string) (*container.ImageConfig, error) {
				digest := "sha256:new"
				if image == networkAnchorImage() {
					digest = "sha256:pause"
				}
				if strings.Contains(image, "sidecar") {
					digest = "sha256:sidecar"
				}
				return &container.ImageConfig{Cmd: []string{"/bin/sh"}, Digest: digest, Size: 500 << 20}, nil
			}
			mgr, err := NewAppManagerForTest(mock, tempDir)
			if err != nil {
				t.Fatal(err)
			}
			allowHostStorage(t, mgr)
			mgr.ForceLockState(false)
			mgr.SetVolumeManager(&manifestUpdateSnapshotVolumeManager{stubVolumeManager: &stubVolumeManager{root: tempDir}})
			mgr.imageDigestResolver = func(_ context.Context, image string) (string, error) {
				cfg, err := mock.inspectImageHook(image)
				if err != nil {
					return "", err
				}
				return cfg.Digest, nil
			}
			rootfs := newAttachmentBoundaryRootfs(t)
			mainVolume, oldDigest := "rootfs-main", "sha256:old"
			if sameDigestAlias {
				mainVolume = persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:new"))
				oldDigest = "sha256:new"
			}
			rootfs.exists = map[string]bool{mainVolume: true, "rootfs-anchor": true, "rootfs-sidecar": true}
			rootfs.identities = map[string]persistence.RootfsImageIdentity{
				mainVolume:       {VolumeID: mainVolume, BaseImageRef: "docker.io/example/piclu:stable", BaseImageDigest: oldDigest},
				"rootfs-anchor":  {VolumeID: "rootfs-anchor", BaseImageRef: networkAnchorImage(), BaseImageDigest: "sha256:pause"},
				"rootfs-sidecar": {VolumeID: "rootfs-sidecar", BaseImageRef: "docker.io/example/sidecar:stable", BaseImageDigest: "sha256:sidecar"},
			}
			mgr.SetRootfsManager(rootfs)
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatal(err)
			}
			baseDef := customManifestPolicyBaseDef()
			baseDef.Services["sidecar"] = api.AppService{Image: "docker.io/example/sidecar:stable"}
			candidateDef := customManifestPolicyClone(t, baseDef)
			svc := candidateDef.Services["main"]
			svc.Image = "docker.io/example/piclu:new"
			if sameDigestAlias {
				svc.Image = "docker.io/example/piclu:alias"
			}
			candidateDef.Services["main"] = svc
			now := time.Now().UTC()
			appInst := &AppInstance{InstanceID: "piclu", Enabled: true, PrimaryService: "main", ActiveRootfs: map[string]string{"main": mainVolume, "sidecar": "rootfs-sidecar", networkAnchorServiceName: "rootfs-anchor"}, Definition: baseDef, CreatedAt: now, UpdatedAt: now}
			if err := state.StoreApp(appInst); err != nil {
				t.Fatal(err)
			}
			cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("attachment boundary image update"))
			_, err = mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{InstanceID: "piclu", BaseManifestHash: cand.BaseManifestHash, RuntimeFingerprint: cand.RuntimeFingerprint, DryRunToken: cand.Token, Confirmations: cand.Classification.RequiredConfirmations})
			if err != nil {
				t.Fatalf("Modify should consume newly attached rootfs after quiesce; detached=%v attached=%v: %v", rootfs.detached, rootfs.attached, err)
			}
			stored, ok := state.GetApp("piclu")
			if !ok {
				t.Fatal("app missing")
			}
			wantMain := persistence.VersionedServiceRootfsVolumeID("piclu", "main", persistence.ShortDigest("sha256:new"))
			if stored.ActiveRootfs["main"] != wantMain || stored.ActiveRootfs[networkAnchorServiceName] != "rootfs-anchor" || stored.ActiveRootfs["sidecar"] != "rootfs-sidecar" {
				t.Fatalf("wrong committed rootfs: %v", stored.ActiveRootfs)
			}
			for _, volume := range []string{mainVolume, "rootfs-anchor", "rootfs-sidecar"} {
				if !slices.Contains(rootfs.detached, volume) {
					t.Fatalf("quiesce did not invalidate active volume %s: %v", volume, rootfs.detached)
				}
			}
			for _, spec := range mock.creates {
				found := false
				for _, h := range rootfs.latest {
					if h.MountPath == spec.Rootfs {
						found = true
					}
				}
				if !found {
					t.Fatalf("container consumed historical mount: %+v", spec)
				}
			}
			if slices.Contains(rootfs.destroyed, "rootfs-anchor") || slices.Contains(rootfs.destroyed, "rootfs-sidecar") || (sameDigestAlias && slices.Contains(rootfs.destroyed, mainVolume)) {
				t.Fatalf("retained rootfs destroyed: %v", rootfs.destroyed)
			}
		})
	}
}

func TestInstallContainerGroupRejectsInvalidPrebuiltBeforePreparation(t *testing.T) {
	cases := []struct {
		name        string
		selected    *rootfsMountInfo
		returned    *persistence.RootfsHandle
		attachErr   error
		validAnchor bool
	}{
		{name: "nil-selected"},
		{name: "nil-main-with-valid-anchor", validAnchor: true},
		{name: "empty-selected-volume", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{MountPath: "/historical"}}},
		{name: "whitespace-selected-volume", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: " ", MountPath: "/historical"}}},
		{name: "attach-error", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: "selected-main", MountPath: "/historical"}}, attachErr: errors.New("attachment unavailable")},
		{name: "empty-returned-mount", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: "selected-main", MountPath: "/historical"}}, returned: &persistence.RootfsHandle{VolumeID: "selected-main"}},
		{name: "whitespace-returned-mount", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: "selected-main", MountPath: "/historical"}}, returned: &persistence.RootfsHandle{VolumeID: "selected-main", MountPath: " "}},
		{name: "empty-returned-volume", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: "selected-main", MountPath: "/historical"}}, returned: &persistence.RootfsHandle{MountPath: "/fresh"}},
		{name: "mismatched-returned-volume", selected: &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: "selected-main", MountPath: "/historical"}}, returned: &persistence.RootfsHandle{VolumeID: "different-volume", MountPath: "/fresh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, mock, rootfs, def, layout, rt := attachmentBoundaryInstallFixture(t)
			rootfs.attachErr, rootfs.attachResult = tc.attachErr, tc.returned
			def.Artifacts = map[string]api.AppArtifact{"model": {Source: api.ArtifactSource{Type: "oci", Reference: "example.test/model:latest"}}}
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatal(err)
			}
			if err := state.StoreApp(&AppInstance{InstanceID: "boundary", Definition: def, ArtifactReferences: map[string]string{"model": "retained-artifact"}}); err != nil {
				t.Fatal(err)
			}
			selected := map[string]*rootfsMountInfo{"main": tc.selected}
			if tc.validAnchor {
				// The anchor sorts before main: validating and attaching entries
				// one at a time would attach it before discovering invalid main.
				selected[networkAnchorServiceName] = &rootfsMountInfo{handle: persistence.RootfsHandle{VolumeID: "valid-anchor", MountPath: "/historical-anchor"}}
			}
			_, err = mgr.installContainerGroup(context.Background(), def, "boundary", layout, rt, nil, selected, true, false)
			if err == nil || !strings.Contains(err.Error(), "main") {
				t.Fatalf("error = %v, want rootfs validation identifying main", err)
			}
			if tc.attachErr != nil && !errors.Is(err, tc.attachErr) {
				t.Fatalf("attach error cause lost: %v", err)
			}
			if len(mock.creates) != 0 || len(rootfs.createServiceReqs) != 0 || len(mock.pulledImages) != 0 || len(rootfs.artifactAttaches) != 0 {
				t.Fatalf("preparation ran after invalid prebuilt: creates=%v local-rootfs=%v pulls=%v artifacts=%v", mock.creates, rootfs.createServiceReqs, mock.pulledImages, rootfs.artifactAttaches)
			}
			if len(rootfs.detached) != 0 || len(rootfs.destroyed) != 0 {
				t.Fatalf("installer took caller cleanup ownership: detach=%v destroy=%v", rootfs.detached, rootfs.destroyed)
			}
			if tc.selected == nil || strings.TrimSpace(tc.selected.handle.VolumeID) == "" {
				if len(rootfs.attached) != 0 {
					t.Fatalf("invalid selection was attached: %v", rootfs.attached)
				}
			} else if len(rootfs.attached) != 1 || rootfs.attached[0] != "selected-main" {
				t.Fatalf("attach requests = %v, want exact selected-main without fallback", rootfs.attached)
			}
		})
	}
}

func attachmentBoundaryInstallFixture(t *testing.T) (*AppManager, *attachmentBoundaryContainers, *attachmentBoundaryRootfs, *api.AppDefinition, appVolumeLayout, container.PodmanRuntime) {
	t.Helper()
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	mock := &attachmentBoundaryContainers{MockContainerManager: NewMockContainerManager()}
	mgr, err := NewAppManagerForTest(mock, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	rootfs := newAttachmentBoundaryRootfs(t)
	mgr.SetRootfsManager(rootfs)
	def := customManifestPolicyBaseDef()
	svc := def.Services["main"]
	svc.Storage = nil
	def.Services["main"] = svc
	SetDefaults(def)
	ctx := context.Background()
	layout, err := mgr.ensureAppVolumeLayout(ctx, "boundary")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := mgr.podmanRuntimeForApp(ctx, "boundary", layout, ModeService, appRuntimeEnsureReady)
	if err != nil {
		t.Fatal(err)
	}
	return mgr, mock, rootfs, def, layout, rt
}

func TestInstallContainerGroupRefreshesPrebuiltHandlesWithoutChangingCallerOwnership(t *testing.T) {
	for _, failCandidate := range []bool{false, true} {
		name := "success"
		if failCandidate {
			name = "candidate-create-failure"
		}
		t.Run(name, func(t *testing.T) {
			mgr, mock, rootfs, def, layout, rt := attachmentBoundaryInstallFixture(t)
			rootfs.writable = map[string]bool{"workspace-clone": true}
			svc := def.Services["main"]
			svc.Init = "image" // workspace clone keeps its original image entrypoint
			def.Services["main"] = svc
			def.Extensions["mode"] = string(ModeWorkspace)
			cfg := persistence.GoldenImageConfig{Entrypoint: []string{"/custom-entrypoint"}, Cmd: []string{"serve"}, Env: []string{"IMAGE_VALUE=preserved"}, WorkingDir: "/workspace", User: "1000:1000"}
			selected := map[string]*rootfsMountInfo{
				"main":                   {handle: persistence.RootfsHandle{VolumeID: "workspace-clone", MountPath: "/historical-clone", ReadOnly: true, GoldenLV: "historical-golden"}, imgConfig: cfg},
				networkAnchorServiceName: {handle: persistence.RootfsHandle{VolumeID: "selected-anchor", MountPath: "/historical-anchor"}, imgConfig: persistence.GoldenImageConfig{Entrypoint: []string{"/pause"}}},
				"unused":                 nil, // stale entries outside the requested group are not consumed
			}
			createErr := errors.New("candidate create failed")
			if failCandidate {
				mock.createError = createErr
			}
			inst, err := mgr.installContainerGroup(context.Background(), def, "boundary", layout, rt, nil, selected, false, false)
			if failCandidate {
				if !errors.Is(err, createErr) {
					t.Fatalf("error = %v, want create cause", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if inst == nil {
					t.Fatal("installed workspace missing")
				}
				var main *container.ContainerCreateSpec
				for i := range mock.creates {
					if mock.creates[i].Labels["io.piccolo.service"] == "main" {
						main = &mock.creates[i]
					}
				}
				if main == nil {
					t.Fatal("main was not created")
				}
				fresh := rootfs.latest["workspace-clone"]
				if main.Rootfs != fresh.MountPath || main.RootfsOverlay {
					t.Fatalf("writable clone did not use fresh writable attachment: %+v fresh=%+v", main, fresh)
				}
				if !slices.Equal(main.Entrypoint, cfg.Entrypoint) || !slices.Equal(main.Command, cfg.Cmd) || main.Environment["IMAGE_VALUE"] != "preserved" || main.WorkingDir != cfg.WorkingDir || main.User != cfg.User {
					t.Fatalf("selected image config lost: %+v", main)
				}
			}
			if selected["main"].handle.MountPath != "/historical-clone" || !selected["main"].handle.ReadOnly || selected["main"].handle.GoldenLV != "historical-golden" || selected[networkAnchorServiceName].handle.MountPath != "/historical-anchor" {
				t.Fatalf("caller handles mutated: %v", selected)
			}
			if len(rootfs.attached) != 2 || !slices.Contains(rootfs.attached, "workspace-clone") || !slices.Contains(rootfs.attached, "selected-anchor") {
				t.Fatalf("attach requests = %v, want only consumed exact volumes", rootfs.attached)
			}
			if len(rootfs.detached) != 0 || len(rootfs.destroyed) != 0 {
				t.Fatalf("caller cleanup authority stolen: detach=%v destroy=%v", rootfs.detached, rootfs.destroyed)
			}
			if len(rootfs.goldenReads) != 0 || len(rootfs.createServiceReqs) != 0 || len(mock.pulledImages) != 0 {
				t.Fatalf("fresh attach reconstructed selected content/config: reads=%v creates=%v pulls=%v", rootfs.goldenReads, rootfs.createServiceReqs, mock.pulledImages)
			}
		})
	}
}

func TestInstallContainerGroupLeavesReattachedBorrowedRootfsOwnedByCallerOnLaterAttachFailure(t *testing.T) {
	mgr, mock, rootfs, def, layout, rt := attachmentBoundaryInstallFixture(t)
	attachErr := errors.New("later service attachment unavailable")
	rootfs.attachHook = func() { rootfs.attachErr = attachErr }
	selected := map[string]*rootfsMountInfo{
		networkAnchorServiceName: {handle: persistence.RootfsHandle{VolumeID: "borrowed-anchor", MountPath: "/historical-anchor"}},
		"main":                   {handle: persistence.RootfsHandle{VolumeID: "borrowed-main", MountPath: "/historical-main"}},
	}
	_, err := mgr.installContainerGroup(context.Background(), def, "boundary", layout, rt, nil, selected, false, false)
	if !errors.Is(err, attachErr) {
		t.Fatalf("error = %v, want later attach error", err)
	}
	if !slices.Equal(rootfs.attached, []string{"borrowed-anchor", "borrowed-main"}) {
		t.Fatalf("attach requests = %v, want exact selected volumes", rootfs.attached)
	}
	if len(mock.creates) != 0 || len(rootfs.createServiceReqs) != 0 || len(mock.pulledImages) != 0 {
		t.Fatalf("candidate preparation ran after failed attachment: creates=%v rootfs=%v pulls=%v", mock.creates, rootfs.createServiceReqs, mock.pulledImages)
	}
	if len(rootfs.detached) != 0 || len(rootfs.destroyed) != 0 {
		t.Fatalf("partially reattached caller volume was cleaned by installer: detach=%v destroy=%v", rootfs.detached, rootfs.destroyed)
	}
	if _, err := os.Stat(filepath.Join(rootfs.latest["borrowed-anchor"].MountPath, "pause")); err != nil {
		t.Fatalf("caller attachment no longer mounted: %v", err)
	}
	if selected[networkAnchorServiceName].handle.MountPath != "/historical-anchor" || selected["main"].handle.MountPath != "/historical-main" {
		t.Fatal("caller handles changed on partial attach failure")
	}
}
