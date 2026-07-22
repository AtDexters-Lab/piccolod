package services

import (
	"context"
	"testing"

	"piccolod/internal/api"
)

func TestAppPublicationActiveRequiresServingNonDeactivatedRoutes(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()

	if mgr.AppPublicationActive("missing") {
		t.Fatal("missing registry entry reported active")
	}
	if _, err := mgr.AllocateForApp("workspace", nil); err != nil {
		t.Fatalf("allocate empty app: %v", err)
	}
	if mgr.AppPublicationActive("workspace") {
		t.Fatal("empty listener set reported active")
	}

	endpoints, err := mgr.AllocateForApp("alpha", []api.AppListener{{
		Name:      "alpha",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		Primary:   true,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	if !mgr.AppPublicationActive("alpha") {
		t.Fatal("complete serving listener set did not report active")
	}

	resumeToken := mgr.SuspendAppPublication("alpha")
	if mgr.AppPublicationActive("alpha") {
		t.Fatal("retained but deactivated registry entry reported active")
	}
	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), resumeToken, "alpha"); err != nil {
		t.Fatalf("resume publication: %v", err)
	}
	if !mgr.AppPublicationActive("alpha") {
		t.Fatal("resumed publication did not report active")
	}

	// Registry state by itself is not proof: losing even one listener makes the
	// complete app projection ineligible.
	mgr.proxyManager.StopEndpoint(endpoints[0].PublicPort, endpoints[0].Flow)
	if mgr.AppPublicationActive("alpha") {
		t.Fatal("registry presence without a serving listener reported active")
	}
}
