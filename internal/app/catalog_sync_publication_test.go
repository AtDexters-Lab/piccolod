package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/firewall"
	"piccolod/internal/persistence"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/services"
	"piccolod/internal/state/paths"
)

func TestRollbackRecreateFailureKeepsExactPublicationSuspension(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	serviceManager := services.NewServiceManager()
	serviceManager.UseInMemoryNetworkForTest()
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTestWithServices(mock, tempDir, serviceManager, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}

	previousDef := customManifestPolicyBaseDef()
	failedDef := customManifestPolicyClone(t, previousDef)
	failedDef.Listeners[0].Name = "piclu-new"
	if _, err := serviceManager.AllocateForApp("piclu", previousDef.Listeners); err != nil {
		t.Fatalf("allocate previous listeners: %v", err)
	}

	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:      "piclu",
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "anchor-failed",
		Containers:      map[string]string{"main": "main-failed"},
		ActiveRootfs:    map[string]string{"main": "rootfs-main"},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      failedDef,
	}
	mock.containers["anchor-failed"] = &mockContainer{ID: "anchor-failed", Status: "running"}
	mock.containers["main-failed"] = &mockContainer{ID: "main-failed", Status: "running"}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store failed app state: %v", err)
	}

	resumeToken := serviceManager.SuspendAppPublication("piclu")
	mock.createError = errors.New("injected replacement create failure")
	txn := &ManifestUpdateTransaction{
		OperationKind:        "service_app_update",
		Phase:                "runtime_switch_started",
		RuntimeSwitchStarted: true,
		AccessSuspended:      true,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	err = mgr.restoreInstalledAppApplyFailure(
		context.Background(), state, appInst, previousDef, failedDef, txn,
		taskTypeUpdateServiceApp, "service_app_update", resumeToken,
		errors.New("candidate failed"),
	)
	if err == nil || !errors.Is(err, mock.createError) {
		t.Fatalf("rollback error = %v, want replacement create failure", err)
	}
	if !txn.AccessSuspended {
		t.Fatal("failed rollback cleared access suspension")
	}
	if serviceManager.AppPublicationActive("piclu") {
		t.Fatal("failed rollback reactivated publication")
	}
	if err := serviceManager.ResumeAppPublicationCheckedContext(context.Background(), "piclu"); !errors.Is(err, services.ErrPublicationSuspended) {
		t.Fatalf("passive resume error = %v, want exact suspension authority", err)
	}
	if err := serviceManager.ResumeAppPublicationWithResumeTokenContext(context.Background(), resumeToken, "piclu"); err != nil {
		t.Fatalf("original resume token no longer owns suspension: %v", err)
	}
}

func TestAuthorizedRollbackPublishesOnlyAfterInstallPersistUnderWarning(t *testing.T) {
	pressure.DefaultAdmission.ResetForTest()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)

	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	serviceManager := services.NewServiceManager()
	serviceManager.UseInMemoryNetworkForTest()
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTestWithServices(mock, tempDir, serviceManager, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}

	failedDef := customManifestPolicyBaseDef()
	previousDef := customManifestPolicyClone(t, failedDef)
	previousDef.Listeners[0].Name = "piclu-restored"
	claim := 18123
	previousDef.Listeners[0].PortClaim = &claim
	if _, err := serviceManager.AllocateForApp("piclu", failedDef.Listeners); err != nil {
		t.Fatalf("allocate failed listeners: %v", err)
	}

	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:      "piclu",
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "anchor-failed",
		Containers:      map[string]string{"main": "main-failed"},
		ActiveRootfs:    map[string]string{"main": "rootfs-main"},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      failedDef,
	}
	mock.containers["anchor-failed"] = &mockContainer{ID: "anchor-failed", Status: "running"}
	mock.containers["main-failed"] = &mockContainer{ID: "main-failed", Status: "running"}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store failed app state: %v", err)
	}

	resumeToken := serviceManager.SuspendAppPublication("piclu")
	preInstallErr := errors.New("injected pre-install failure")
	err = mgr.recreateContainersInPlaceWithHookAndPublicationResumeToken(
		context.Background(), "piclu", previousDef, failedDef, appInst,
		func() error { return preInstallErr },
		nil,
		resumeToken,
		false,
	)
	if !errors.Is(err, preInstallErr) {
		t.Fatalf("pre-install error = %v, want injected failure", err)
	}
	if serviceManager.AppPublicationActive("piclu") {
		t.Fatal("pre-install failure reactivated publication")
	}
	if err := serviceManager.ResumeAppPublicationCheckedContext(context.Background(), "piclu"); !errors.Is(err, services.ErrPublicationSuspended) {
		t.Fatalf("passive resume after pre-install failure = %v, want exact suspension authority", err)
	}
	// The failed attempt removed the materialized runtime. Recreate the fixture
	// so the same token can prove a successful retry publishes only after commit.
	mock.containers["anchor-failed"] = &mockContainer{ID: "anchor-failed", Status: "running"}
	mock.containers["main-failed"] = &mockContainer{ID: "main-failed", Status: "running"}

	beforeInstallSeen := false
	persistedReplacement := false
	state.storeAppMetadataHook = func(instanceID string, stored *AppInstance) error {
		if instanceID == "piclu" && stored.NetworkAnchorID != "" && stored.NetworkAnchorID != "anchor-failed" && stored.Containers["main"] != "" && stored.Containers["main"] != "main-failed" {
			persistedReplacement = true
		}
		return nil
	}
	fw := &publicationOrderFirewall{open: func(ctx context.Context, _ firewall.Rule) error {
		if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkNetworkProbe); err != nil {
			return err
		}
		if !beforeInstallSeen {
			return errors.New("publication preceded beforeInstall")
		}
		if !persistedReplacement {
			return errors.New("publication preceded replacement persistence")
		}
		return nil
	}}
	serviceManager.SetFirewallManager(fw)
	pressure.DefaultAdmission.Fence()

	err = mgr.recreateContainersInPlaceWithHookAndPublicationResumeToken(
		context.Background(), "piclu", previousDef, failedDef, appInst,
		func() error {
			beforeInstallSeen = true
			return nil
		},
		nil,
		resumeToken,
		false,
	)
	if err != nil {
		t.Fatalf("authorized rollback recreate: %v", err)
	}
	if fw.opened != 1 {
		t.Fatalf("published firewall rules = %d, want one", fw.opened)
	}
	if !serviceManager.AppPublicationActive("piclu") {
		t.Fatal("authorized rollback did not activate publication")
	}
	if _, ok := serviceManager.GetAppListener("piclu", "piclu-restored"); !ok {
		t.Fatal("authorized rollback did not publish restored listener set")
	}
}

func TestEqualListenerRecreatePreparesMissingRegistryBeforeDestructiveWork(t *testing.T) {
	tempDir := t.TempDir()
	paths.SetCoreRootForTest(t, tempDir)
	serviceManager := services.NewServiceManager()
	serviceManager.UseInMemoryNetworkForTest()
	mock := NewMockContainerManager()
	mgr, err := NewAppManagerForTestWithServices(mock, tempDir, serviceManager, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	allowHostStorage(t, mgr)
	mgr.ForceLockState(false)
	state, err := mgr.ensureStateManager()
	if err != nil {
		t.Fatalf("state manager: %v", err)
	}

	def := customManifestPolicyBaseDef()
	now := time.Now().UTC()
	appInst := &AppInstance{
		InstanceID:      "piclu",
		Enabled:         true,
		PrimaryService:  "main",
		NetworkAnchorID: "anchor-old",
		Containers:      map[string]string{"main": "main-old"},
		ActiveRootfs:    map[string]string{"main": "rootfs-main"},
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      def,
	}
	mock.containers["anchor-old"] = &mockContainer{ID: "anchor-old", Status: "running"}
	mock.containers["main-old"] = &mockContainer{ID: "main-old", Status: "running"}
	if err := state.StoreApp(appInst); err != nil {
		t.Fatalf("store app: %v", err)
	}
	resumeToken := serviceManager.SuspendAppPublication("piclu")

	if err := mgr.recreateContainersInPlaceWithHookAndPublicationResumeToken(context.Background(), "piclu", def, def, appInst, nil, nil, resumeToken, false); err != nil {
		t.Fatalf("equal-listener recreate with missing registry: %v", err)
	}
	endpoints, err := serviceManager.GetByApp("piclu")
	if err != nil || len(endpoints) != 1 {
		t.Fatalf("published endpoints = %+v, err=%v; want one", endpoints, err)
	}
	if !serviceManager.AppPublicationActive("piclu") {
		t.Fatal("missing-registry recreate did not publish prepared endpoints")
	}
}

func TestInstalledAppApplyPersistsCandidateArtifactsBeforeMetadataPublication(t *testing.T) {
	for _, test := range []struct {
		name        string
		failPersist bool
	}{
		{name: "publish after durable candidate ownership"},
		{name: "persistence failure compensates unpublished candidate", failPersist: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			tempDir := t.TempDir()
			paths.SetCoreRootForTest(t, tempDir)
			serviceManager := services.NewServiceManager()
			serviceManager.UseInMemoryNetworkForTest()
			mock := NewMockContainerManager()
			mgr, err := NewAppManagerForTestWithServices(mock, tempDir, serviceManager, nil)
			if err != nil {
				t.Fatalf("new manager: %v", err)
			}
			allowHostStorage(t, mgr)
			mgr.ForceLockState(false)
			rootfs := &compensationRootfsManager{
				stubRootfsManager: newStubRootfsManager(tempDir),
			}
			mgr.SetRootfsManager(rootfs)
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatalf("state manager: %v", err)
			}

			previousDef := customManifestPolicyBaseDef()
			candidateDef := customManifestPolicyClone(t, previousDef)
			previousDef.Artifacts = map[string]api.AppArtifact{
				"model": {Source: api.ArtifactSource{Type: persistence.GoldenSourceOCI, Reference: "docker.io/example/model:old"}},
			}
			candidateDef.Artifacts = map[string]api.AppArtifact{
				"model": {Source: api.ArtifactSource{Type: persistence.GoldenSourceOCI, Reference: "docker.io/example/model:new"}},
			}
			if _, err := serviceManager.AllocateForApp("piclu", previousDef.Listeners); err != nil {
				t.Fatalf("allocate previous listeners: %v", err)
			}

			now := time.Now().UTC()
			appInst := &AppInstance{
				InstanceID:         "piclu",
				Enabled:            true,
				PrimaryService:     "main",
				NetworkAnchorID:    "anchor-old",
				Containers:         map[string]string{"main": "main-old"},
				ActiveRootfs:       map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
				ArtifactReferences: map[string]string{"model": "ref-previous"},
				CreatedAt:          now,
				UpdatedAt:          now,
				Definition:         previousDef,
			}
			mock.containers["anchor-old"] = &mockContainer{ID: "anchor-old", Status: "running"}
			mock.containers["main-old"] = &mockContainer{ID: "main-old", Status: "running"}
			if err := state.StoreApp(appInst); err != nil {
				t.Fatalf("store app: %v", err)
			}

			manifestTxn := &ManifestUpdateTransaction{
				OperationID:           "op-artifact-publication-order",
				OperationKind:         "service_app_update",
				Phase:                 "recreating_runtime",
				RuntimeSwitchStarted:  true,
				RuntimeTouched:        true,
				PreviousArtifactRefs:  map[string]string{"model": "ref-previous"},
				CandidateArtifactRefs: map[string]string{"model": "ref-previous"},
				CreatedAt:             now,
				UpdatedAt:             now,
			}
			if err := state.StoreManifestUpdateTransaction(appInst.InstanceID, manifestTxn); err != nil {
				t.Fatalf("store transaction: %v", err)
			}
			applyTxn := &installedAppApplyTransaction{
				manager: mgr,
				ctx:     context.Background(),
				state:   state,
				spec: installedAppApplyTransactionSpec{
					InstanceID: appInst.InstanceID,
					AppInst:    appInst,
				},
				txn: manifestTxn,
			}
			wantReference := artifactReferenceID(appInst.InstanceID, "model", "golden-candidate-model")
			injected := errors.New("injected candidate ownership persistence failure")
			if test.failPersist {
				state.storeManifestUpdateTransactionHook = func(instanceID string, txn *ManifestUpdateTransaction) error {
					if instanceID == appInst.InstanceID && txn.CandidateArtifactRefs["model"] == wantReference {
						return injected
					}
					return nil
				}
			}
			metadataWrites := 0
			state.storeAppMetadataHook = func(instanceID string, candidate *AppInstance) error {
				if instanceID != appInst.InstanceID || candidate.NetworkAnchorID == "anchor-old" {
					return nil
				}
				metadataWrites++
				storedTxn, err := state.LoadManifestUpdateTransaction(instanceID)
				if err != nil {
					return fmt.Errorf("load transaction at metadata publication: %w", err)
				}
				if got := storedTxn.CandidateArtifactRefs; !reflect.DeepEqual(got, candidate.ArtifactReferences) {
					return fmt.Errorf("metadata candidate refs %v preceded durable transaction refs %v", candidate.ArtifactReferences, got)
				}
				return nil
			}

			resumeToken := serviceManager.SuspendAppPublication(appInst.InstanceID)
			err = mgr.recreateContainersInPlaceWithHookAndPublicationResumeToken(
				context.Background(),
				appInst.InstanceID,
				candidateDef,
				previousDef,
				appInst,
				nil,
				applyTxn.persistCandidateArtifactReferences,
				resumeToken,
				false,
			)
			if test.failPersist {
				if !errors.Is(err, injected) {
					t.Fatalf("recreate error = %v, want persistence failure", err)
				}
				if metadataWrites != 0 {
					t.Fatalf("candidate metadata writes = %d, want none", metadataWrites)
				}
				if len(mock.containers) != 0 {
					t.Fatalf("unpublished candidate containers survived: %+v", mock.containers)
				}
				if !reflect.DeepEqual(rootfs.artifactDetached, []string{wantReference}) {
					t.Fatalf("detached artifact references = %v, want %q", rootfs.artifactDetached, wantReference)
				}
				if !reflect.DeepEqual(rootfs.artifactDestroyed, []string{wantReference}) {
					t.Fatalf("destroyed artifact references = %v, want %q", rootfs.artifactDestroyed, wantReference)
				}
				return
			}
			if err != nil {
				t.Fatalf("recreate: %v", err)
			}
			if metadataWrites != 1 {
				t.Fatalf("candidate metadata writes = %d, want one", metadataWrites)
			}
			storedTxn, err := state.LoadManifestUpdateTransaction(appInst.InstanceID)
			if err != nil {
				t.Fatalf("load transaction: %v", err)
			}
			if got := storedTxn.CandidateArtifactRefs["model"]; got != wantReference {
				t.Fatalf("durable candidate artifact ref = %q, want %q", got, wantReference)
			}
		})
	}
}

type publicationOrderFirewall struct {
	opened int
	open   func(context.Context, firewall.Rule) error
}

func (f *publicationOrderFirewall) OpenPort(ctx context.Context, rule firewall.Rule) error {
	if f.open != nil {
		if err := f.open(ctx, rule); err != nil {
			return fmt.Errorf("test publication order: %w", err)
		}
	}
	f.opened++
	return nil
}

func (*publicationOrderFirewall) ClosePort(context.Context, firewall.Rule) error { return nil }
