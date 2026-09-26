package services

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"piccolod/internal/api"
)

func TestDeactivateAppUnlessSuspendedPreservesTransactionBindings(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	before, err := mgr.AllocateForApp("landing", []api.AppListener{{
		Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP,
	}})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetAppContainerID("landing", "old-anchor")
	mgr.appTransient["landing"] = time.Now()
	resumeToken := mgr.SuspendAppPublication("landing")
	mgr.DeactivateAppUnlessSuspended("landing")

	after, err := mgr.GetByApp("landing")
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("endpoints after quiesce = %+v, err=%v, want %+v", after, err, before)
	}
	ep := before[0]
	if _, reserved := mgr.allocator.usedHost[ep.HostBind]; !reserved {
		t.Fatal("quiesce released the transaction's host binding")
	}
	if _, reserved := mgr.allocator.usedPublic[publicKey(ep.PublicPort, "tcp")]; !reserved {
		t.Fatal("quiesce released the transaction's public port")
	}
	if _, exists := mgr.containerIDs["landing"]; exists {
		t.Fatal("quiesce retained the stopped container identity")
	}
	if _, exists := mgr.appTransient["landing"]; exists {
		t.Fatal("quiesce retained transient runtime health state")
	}
	if mgr.AppPublicationActive("landing") {
		t.Fatal("quiesce reactivated suspended publication")
	}
	if err := mgr.ResumeAppPublicationChecked("landing"); !errors.Is(err, ErrPublicationSuspended) {
		t.Fatalf("passive resume = %v, want suspension owner required", err)
	}
	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), resumeToken, "landing"); err != nil {
		t.Fatalf("owning transaction could not resume: %v", err)
	}
	if !mgr.AppPublicationActive("landing") {
		t.Fatal("owning transaction did not restore publication")
	}
}

func TestQuiescePreservationDoesNotChangeEndpointRemoval(t *testing.T) {
	for _, mode := range []string{"ordinary_stop", "explicit_deactivate", "uninstall"} {
		t.Run(mode, func(t *testing.T) {
			mgr := NewServiceManager()
			mgr.UseInMemoryNetworkForTest()
			eps, err := mgr.AllocateForApp("landing", []api.AppListener{{
				Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP,
			}})
			if err != nil {
				t.Fatal(err)
			}
			var token PublicationResumeToken
			if mode != "ordinary_stop" {
				token = mgr.SuspendAppPublication("landing")
				mgr.DeactivateAppUnlessSuspended("landing")
			}
			switch mode {
			case "ordinary_stop":
				mgr.DeactivateAppUnlessSuspended("landing")
			case "explicit_deactivate":
				// Recovery deliberately releases bindings before retrying a
				// conflicting host port. Keep that API's existing behavior.
				mgr.DeactivateApp("landing")
			case "uninstall":
				mgr.RemoveApp("landing")
			}
			if _, err := mgr.GetByApp("landing"); err == nil {
				t.Fatal("endpoint removal retained registry")
			}
			ep := eps[0]
			if _, reserved := mgr.allocator.usedHost[ep.HostBind]; reserved {
				t.Fatal("endpoint removal retained host binding")
			}
			if _, reserved := mgr.allocator.usedPublic[publicKey(ep.PublicPort, "tcp")]; reserved {
				t.Fatal("endpoint removal retained public port")
			}
			if mode == "explicit_deactivate" && mgr.deactivated["landing"] != token.record {
				t.Fatal("explicit deactivation replaced the transaction's resume authority")
			}
			if mode == "uninstall" && mgr.deactivated["landing"] != nil {
				t.Fatal("uninstall retained suspension state")
			}
		})
	}
}
