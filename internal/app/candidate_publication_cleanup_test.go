package app

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/services"
)

func TestRemoveUncommittedContainerGroupPreservesSuspendedPublicationBindings(t *testing.T) {
	for _, suspended := range []bool{true, false} {
		name := "suspended-transaction"
		if !suspended {
			name = "ordinary-unsuspended-cleanup"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mock := NewMockContainerManager()
			mgr, err := NewAppManagerForTest(mock, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			def := customManifestPolicyBaseDef()
			const instanceID = "publication-cleanup"
			original, err := mgr.serviceManager.AllocateForApp(instanceID, def.Listeners)
			if err != nil {
				t.Fatalf("allocate original publication: %v", err)
			}
			if len(original) != 1 || original[0].HostBind == 0 || original[0].PublicPort == 0 {
				t.Fatalf("expected allocated original host/public bindings, got %+v", original)
			}
			originalRegistry := mgr.serviceManager.SnapshotRegistry()[instanceID]
			rt := container.PodmanRuntime{}
			anchorID, err := mock.CreateContainer(ctx, rt, container.ContainerCreateSpec{Name: networkAnchorContainerName(instanceID), Labels: piccoloLabels(instanceID, networkAnchorServiceName, "network_anchor")})
			if err != nil {
				t.Fatal(err)
			}
			mainID, err := mock.CreateContainer(ctx, rt, container.ContainerCreateSpec{Name: containerNameForService(instanceID, "main", "main"), Labels: piccoloLabels(instanceID, "main", "service")})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{anchorID, mainID} {
				if err := mock.StartContainer(ctx, rt, id); err != nil {
					t.Fatal(err)
				}
			}
			mgr.serviceManager.SetAppContainerID(instanceID, anchorID)
			var token services.PublicationResumeToken
			if suspended {
				token = mgr.serviceManager.SuspendAppPublication(instanceID)
			}
			candidate := &AppInstance{InstanceID: instanceID, PrimaryService: "main", NetworkAnchorID: anchorID, Containers: map[string]string{"main": mainID}}
			if err := mgr.removeUncommittedContainerGroup(ctx, candidate, def, rt); err != nil {
				t.Fatalf("remove candidate: %v", err)
			}
			if len(mock.containers) != 0 {
				t.Fatalf("candidate containers survived cleanup: %+v", mock.containers)
			}
			if mgr.serviceManager.AppPublicationActive(instanceID) {
				t.Fatal("cleanup left publication active")
			}
			if id, _ := mgr.serviceManager.GetAppContainerID(instanceID); id != "" {
				t.Fatalf("candidate backend identity retained: %s", id)
			}
			retained := mgr.serviceManager.SnapshotRegistry()[instanceID]
			if !suspended {
				if len(retained) != 0 {
					t.Fatalf("ordinary cleanup retained active endpoints: %+v", retained)
				}
				return
			}
			if !reflect.DeepEqual(retained, originalRegistry) {
				t.Errorf("suspended cleanup lost original endpoint identity/bindings: got %+v, want %+v", retained, originalRegistry)
			}
			rollback, err := mgr.serviceManager.PrepareReconcile(instanceID, def.Listeners)
			if err != nil {
				t.Fatalf("prepare rollback publication: %v", err)
			}
			defer rollback.Release()
			restored := rollback.Endpoints()
			if !reflect.DeepEqual(restored, original) {
				t.Errorf("rollback endpoint allocation drifted: got %+v, want original %+v", restored, original)
			}
			if err := mgr.serviceManager.ResumeAppPublicationCheckedContext(ctx, instanceID); !errors.Is(err, services.ErrPublicationSuspended) {
				t.Fatalf("passive resume bypassed transaction suspension: %v", err)
			}
			if err := mgr.serviceManager.ResumeAppPublicationWithResumeTokenContext(ctx, token, instanceID); err != nil {
				t.Fatalf("owning rollback token no longer valid: %v", err)
			}
			if !mgr.serviceManager.AppPublicationActive(instanceID) {
				t.Error("owning rollback token did not restore publication")
			}
		})
	}
}

func TestApplyCustomManifestUpdateCandidateStartFailureRestoresOriginalPublicationBindings(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	mock := &attachmentBoundaryContainers{MockContainerManager: NewMockContainerManager()}
	mock.inspectImageHook = func(image string) (*container.ImageConfig, error) {
		digest := "sha256:new"
		if image == networkAnchorImage() {
			digest = "sha256:pause"
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
	originalRootfs := map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"}
	rootfs.exists = map[string]bool{"rootfs-main": true, "rootfs-anchor": true}
	rootfs.identities = map[string]persistence.RootfsImageIdentity{
		"rootfs-main":   {VolumeID: "rootfs-main", BaseImageRef: "docker.io/example/piclu:stable", BaseImageDigest: "sha256:old"},
		"rootfs-anchor": {VolumeID: "rootfs-anchor", BaseImageRef: networkAnchorImage(), BaseImageDigest: "sha256:pause"},
	}
	mgr.SetRootfsManager(rootfs)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatal(err)
	}
	const instanceID = "piclu"
	baseDef := customManifestPolicyBaseDef()
	originalEndpoints, err := mgr.serviceManager.AllocateForApp(instanceID, baseDef.Listeners)
	if err != nil {
		t.Fatalf("allocate original listeners: %v", err)
	}
	originalRegistry := mgr.serviceManager.SnapshotRegistry()[instanceID]
	rt := container.PodmanRuntime{}
	anchorHandle, err := rootfs.AttachRootfs(ctx, originalRootfs[networkAnchorServiceName])
	if err != nil {
		t.Fatal(err)
	}
	mainHandle, err := rootfs.AttachRootfs(ctx, originalRootfs["main"])
	if err != nil {
		t.Fatal(err)
	}
	anchorID, err := mock.CreateContainer(ctx, rt, container.ContainerCreateSpec{Name: networkAnchorContainerName(instanceID), Rootfs: anchorHandle.MountPath, Entrypoint: []string{"/pause"}, Labels: piccoloLabels(instanceID, networkAnchorServiceName, "network_anchor")})
	if err != nil {
		t.Fatal(err)
	}
	mainID, err := mock.CreateContainer(ctx, rt, container.ContainerCreateSpec{Name: containerNameForService(instanceID, "main", "main"), Rootfs: mainHandle.MountPath, Labels: piccoloLabels(instanceID, "main", "service")})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{anchorID, mainID} {
		if err := mock.StartContainer(ctx, rt, id); err != nil {
			t.Fatal(err)
		}
	}
	mgr.serviceManager.SetAppContainerID(instanceID, anchorID)
	now := time.Now().UTC()
	appInst := &AppInstance{InstanceID: instanceID, Enabled: true, PrimaryService: "main", NetworkAnchorID: anchorID, Containers: map[string]string{"main": mainID}, ActiveRootfs: cloneStringMap(originalRootfs), Definition: baseDef, CreatedAt: now, UpdatedAt: now}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatal(err)
	}
	candidateDef := customManifestPolicyClone(t, baseDef)
	svc := candidateDef.Services["main"]
	svc.Image = "docker.io/example/piclu:new"
	candidateDef.Services["main"] = svc
	cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("candidate startup failure"))
	injected := errors.New("injected candidate OCI startup failure")
	failedAnchorID := generateMockContainerID(mock.nextID)
	mock.startErrorForContainer = map[string]error{failedAnchorID: injected}
	_, err = mgr.ApplyCustomManifestUpdate(ctx, ManifestUpdateRequest{InstanceID: instanceID, BaseManifestHash: cand.BaseManifestHash, RuntimeFingerprint: cand.RuntimeFingerprint, DryRunToken: cand.Token, Confirmations: cand.Classification.RequiredConfirmations})
	if err == nil || !strings.Contains(err.Error(), injected.Error()) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("apply error = %v, want candidate failure with successful rollback", err)
	}
	stored, ok := state.GetApp(instanceID)
	if !ok || !reflect.DeepEqual(stored.ActiveRootfs, originalRootfs) || !reflect.DeepEqual(stored.Definition, baseDef) {
		t.Fatalf("rollback failed to restore selected original app: %+v", stored)
	}
	restoredRegistry := mgr.serviceManager.SnapshotRegistry()[instanceID]
	if !reflect.DeepEqual(restoredRegistry, originalRegistry) {
		t.Fatalf("rollback changed original endpoint identity/HostBind/PublicPort: got %+v, want %+v (original endpoints %+v)", restoredRegistry, originalRegistry, originalEndpoints)
	}
	if !mgr.serviceManager.AppPublicationActive(instanceID) {
		t.Fatal("rollback did not resume original publication")
	}
	if backendID, _ := mgr.serviceManager.GetAppContainerID(instanceID); backendID != stored.NetworkAnchorID || backendID == "" {
		t.Fatalf("publication backend = %q, want restored anchor %q", backendID, stored.NetworkAnchorID)
	}
	if _, exists := mock.containers[failedAnchorID]; exists {
		t.Fatalf("failed candidate anchor %s survived", failedAnchorID)
	}
	if len(mock.containers) != 2 {
		t.Fatalf("rollback container group = %+v, want only original anchor/service recreated", mock.containers)
	}
	for _, id := range []string{stored.NetworkAnchorID, stored.Containers["main"]} {
		c, exists := mock.containers[id]
		if !exists || c.Status != "running" {
			t.Fatalf("restored container %s is not running: %+v", id, c)
		}
	}
	if _, err := state.LoadManifestUpdateTransaction(instanceID); !os.IsNotExist(err) {
		t.Fatalf("completed rollback left pending transaction: %v", err)
	}
}
