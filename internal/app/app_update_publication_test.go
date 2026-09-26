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
	"piccolod/internal/services"
)

func TestApplyCustomManifestUpdatePreservesUnchangedListeners(t *testing.T) {
	for _, failReadiness := range []bool{false, true} {
		name := "commit"
		if failReadiness {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			mock := NewMockContainerManager()
			mgr, err := NewAppManagerForTest(mock, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			allowHostStorage(t, mgr)
			// Exercise the production endpoint check, which the storage fixture
			// normally bypasses along with mount verification.
			t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "")
			mgr.ForceLockState(false)
			state, err := mgr.ensureStateManager()
			if err != nil {
				t.Fatal(err)
			}
			baseDef := customManifestPolicyBaseDef()
			svc := baseDef.Services["main"]
			svc.Storage = nil
			baseDef.Services["main"] = svc
			before, err := mgr.serviceManager.AllocateForApp("piclu", baseDef.Listeners)
			if err != nil {
				t.Fatal(err)
			}
			advertisements := 0
			mgr.serviceManager.SetRuntimePublicationCallbacks(nil, func() {
				advertisements++
				if _, ok := mgr.serviceManager.ResolveByHostLabelAnyPort(before[0].DerivedHostLabel); !ok {
					t.Error("runtime advertised before publication became active")
				}
			})
			mock.containers["main-old"] = &mockContainer{
				ID: "main-old", Status: "running",
				Spec: container.ContainerCreateSpec{Name: "piclu", Labels: map[string]string{"io.piccolo.instance": "piclu"}},
			}
			mock.containers["anchor-old"] = &mockContainer{
				ID: "anchor-old", Status: "running",
				Spec: container.ContainerCreateSpec{Name: networkAnchorContainerName("piclu"), Labels: map[string]string{"io.piccolo.instance": "piclu"}},
			}
			now := time.Now().UTC()
			appInst := &AppInstance{
				InstanceID: "piclu", Enabled: true, PrimaryService: "main",
				NetworkAnchorID: "anchor-old", Containers: map[string]string{"main": "main-old"},
				ActiveRootfs: map[string]string{"main": "rootfs-main", networkAnchorServiceName: "rootfs-anchor"},
				CreatedAt:    now, UpdatedAt: now, Definition: baseDef,
			}
			if err := state.StoreApp(appInst); err != nil {
				t.Fatal(err)
			}
			candidateDef := customManifestPolicyClone(t, baseDef)
			svc = candidateDef.Services["main"]
			svc.Image = "docker.io/example/piclu:new"
			candidateDef.Services["main"] = svc
			cand := storeManifestUpdateCandidateForTest(t, mgr, state, appInst, candidateDef, []byte("image update"))
			readinessCalled := false
			mgr.runtimeReadinessProbe = func(_ context.Context, endpoints []services.ServiceEndpoint, _ time.Duration) error {
				readinessCalled = true
				if !reflect.DeepEqual(endpoints, before) {
					t.Fatalf("readiness endpoints = %+v, want original bindings %+v", endpoints, before)
				}
				if mgr.serviceManager.AppPublicationActive("piclu") {
					t.Fatal("candidate published before readiness and ledger commit")
				}
				current, _ := state.GetApp("piclu")
				if current.Containers["main"] == "main-old" || mock.containers[current.Containers["main"]].Status != "running" {
					t.Fatal("readiness checked before replacement started")
				}
				if failReadiness {
					return errors.New("candidate backend unreachable")
				}
				return nil
			}
			result, err := mgr.ApplyCustomManifestUpdate(context.Background(), ManifestUpdateRequest{
				InstanceID: "piclu", BaseManifestHash: cand.BaseManifestHash,
				RuntimeFingerprint: cand.RuntimeFingerprint, DryRunToken: cand.Token,
				Confirmations: cand.Classification.RequiredConfirmations,
			})
			if failReadiness {
				if err == nil || !strings.HasPrefix(err.Error(), "app update rolled back:") || !strings.Contains(err.Error(), "candidate backend unreachable") {
					t.Fatalf("apply error = %v, want readiness rollback", err)
				}
			} else if err != nil || result.AccessRepairPending {
				t.Fatalf("apply = %+v, err=%v, want committed and published update", result, err)
			}
			if !readinessCalled {
				t.Fatal("replacement never reached readiness")
			}
			current, _ := state.GetApp("piclu")
			wantImage := candidateDef.Services["main"].Image
			if failReadiness {
				wantImage = baseDef.Services["main"].Image
				if current.ActiveRootfs["main"] != "rootfs-main" {
					t.Fatalf("rollback rootfs = %q, want previous rootfs", current.ActiveRootfs["main"])
				}
			}
			if current.Definition.Services["main"].Image != wantImage {
				t.Fatalf("image = %q, want %q", current.Definition.Services["main"].Image, wantImage)
			}
			after, err := mgr.serviceManager.GetByApp("piclu")
			if err != nil || !reflect.DeepEqual(after, before) || !mgr.serviceManager.AppPublicationActive("piclu") {
				t.Fatalf("final publication = %+v, err=%v, want active original bindings %+v", after, err, before)
			}
			if advertisements != 1 {
				t.Fatalf("runtime advertisements = %d, want one restored publication", advertisements)
			}
			if _, err := state.LoadManifestUpdateTransaction("piclu"); !os.IsNotExist(err) {
				t.Fatalf("completed update retained transaction: %v", err)
			}
			if err := mgr.rejectIfTransitionInProgress(state, "piclu", TransitionFenceModifyApp); err != nil {
				t.Fatalf("completed update retained transition fence: %v", err)
			}
		})
	}
}
