package services

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/firewall"
	"piccolod/internal/resources/pressure"
)

func TestSuspendAppPublicationWithdrawsBeforeRouteTeardown(t *testing.T) {
	mgr := NewServiceManager()
	useFakeProxyListeners(mgr)
	ep := ServiceEndpoint{
		App:              "piclu",
		Name:             "web",
		GuestPort:        8080,
		HostBind:         15080,
		PublicPort:       35080,
		Flow:             api.FlowTCP,
		Protocol:         api.ListenerProtocolHTTP,
		DerivedHostLabel: "piclu",
		RemotePorts:      []int{80, 443},
	}
	mgr.registry["piclu"] = map[string]ServiceEndpoint{"web": ep}
	if err := mgr.proxyManager.StartListenerChecked(ep); err != nil {
		t.Fatalf("start route: %v", err)
	}
	if _, ok := mgr.ResolveByHostLabel("piclu", 443); !ok {
		t.Fatal("route was not active before suspension")
	}
	tlsMux := NewTlsMux(mgr)
	tlsMux.UpdateAliases(map[string]string{"piclu.example.net": "piclu"})
	if upstream := tlsMux.resolveUpstream("piclu.example.net", 443); upstream != ep.PublicPort {
		t.Fatalf("TLS mux upstream before suspension = %d, want %d", upstream, ep.PublicPort)
	}

	withdrawCalled := false
	mgr.SetRuntimePublicationCallbacks(func(ctx context.Context) error {
		withdrawCalled = true
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("withdrawal callback was not bounded")
		}
		if _, ok := mgr.ResolveByHostLabelAnyPort("piclu"); ok {
			t.Fatal("inactive registry endpoint remained resolvable during withdrawal")
		}
		if upstream := tlsMux.resolveUpstream("piclu.example.net", 443); upstream != 0 {
			t.Fatalf("TLS mux retained suspended alias upstream %d", upstream)
		}
		mgr.proxyManager.mu.Lock()
		_, stillRunning := mgr.proxyManager.listeners[ep.PublicPort]
		mgr.proxyManager.mu.Unlock()
		if !stillRunning {
			t.Fatal("route was torn down before runtime withdrawal")
		}
		return nil
	}, nil)

	mgr.SuspendAppPublication("piclu")
	if !withdrawCalled {
		t.Fatal("runtime withdrawal was not invoked")
	}
	if _, ok := mgr.ResolveByHostLabel("piclu", 443); ok {
		t.Fatal("suspended endpoint remained resolvable")
	}
	mgr.proxyManager.mu.Lock()
	_, stillRunning := mgr.proxyManager.listeners[ep.PublicPort]
	mgr.proxyManager.mu.Unlock()
	if stillRunning {
		t.Fatal("route remained running after suspension")
	}
}

func TestFailedResumeStaysWithdrawnAndDoesNotAdvertise(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	ep := ServiceEndpoint{
		App:              "piclu",
		Name:             "web",
		GuestPort:        8080,
		HostBind:         15080,
		PublicPort:       35080,
		Flow:             api.FlowTCP,
		Protocol:         api.ListenerProtocolHTTP,
		DerivedHostLabel: "piclu",
		RemotePorts:      []int{80, 443},
	}
	mgr.registry["piclu"] = map[string]ServiceEndpoint{"web": ep}
	mgr.deactivated["piclu"] = &publicationInactiveRecord{
		kind:      publicationInactiveStopped,
		endpoints: endpointInfoSlice([]ServiceEndpoint{ep}),
	}
	mgr.proxyManager.listenTCP = func(string, string) (net.Listener, error) {
		return nil, errors.New("forced resume bind failure")
	}
	advertised := false
	mgr.SetRuntimePublicationCallbacks(nil, func() { advertised = true })
	recorder := &recordingPublication{}
	mgr.SetRemotePublisher(recorder)

	if err := mgr.ResumeAppPublicationChecked("piclu"); err == nil {
		t.Fatal("resume error = nil, want bind failure")
	}
	if advertised {
		t.Fatal("failed resume advertised runtime projection")
	}
	if len(recorder.published) != 0 {
		t.Fatalf("failed resume published ports = %v", recorder.published)
	}
	if _, inactive := mgr.deactivated["piclu"]; !inactive {
		t.Fatal("failed resume committed active publication state")
	}
	if _, ok := mgr.ResolveByHostLabelAnyPort("piclu"); ok {
		t.Fatal("failed resume made endpoint resolvable")
	}
}

func TestSuspendAppPublicationPreservesRegistryAndAllocations(t *testing.T) {
	mgr := NewServiceManager()
	useFakeProxyListeners(mgr)
	recorder := &recordingPublication{}
	mgr.SetRemotePublisher(recorder)
	mgr.SetRemoteUnpublisher(recorder)

	ep := ServiceEndpoint{
		App:        "piclu",
		Name:       "web",
		GuestPort:  8080,
		HostBind:   15080,
		PublicPort: 35080,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
	}
	mgr.registry["piclu"] = map[string]ServiceEndpoint{"web": ep}
	mgr.allocator.usedHost[ep.HostBind] = struct{}{}
	mgr.allocator.usedPublic[publicKey(ep.PublicPort, ep.Flow.TransportProtocol())] = struct{}{}

	resumeToken := mgr.SuspendAppPublication("piclu")

	if _, ok := mgr.registry["piclu"]["web"]; !ok {
		t.Fatalf("suspend removed endpoint registry")
	}
	if _, ok := mgr.allocator.usedHost[ep.HostBind]; !ok {
		t.Fatalf("suspend released host bind allocation")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(ep.PublicPort, ep.Flow.TransportProtocol())]; !ok {
		t.Fatalf("suspend released public port allocation")
	}
	if len(recorder.unpublished) != 1 || recorder.unpublished[0] != ep.PublicPort {
		t.Fatalf("unpublished ports = %v, want [%d]", recorder.unpublished, ep.PublicPort)
	}

	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), resumeToken, "piclu"); err != nil {
		t.Fatalf("resume publication: %v", err)
	}

	if _, ok := mgr.registry["piclu"]["web"]; !ok {
		t.Fatalf("resume removed endpoint registry")
	}
	if len(recorder.published) != 1 || recorder.published[0] != ep.PublicPort {
		t.Fatalf("published ports = %v, want [%d]", recorder.published, ep.PublicPort)
	}
}

func TestReconcileNetworkPublicationsSkipsSuspendedApp(t *testing.T) {
	mgr := NewServiceManager()
	useFakeProxyListeners(mgr)
	fw := &recordingFirewall{}
	mgr.SetFirewallManager(fw)
	claim := 35080
	ep := ServiceEndpoint{
		App:        "piclu",
		Name:       "web",
		GuestPort:  8080,
		HostBind:   15080,
		PublicPort: claim,
		Flow:       api.FlowTCP,
		Protocol:   api.ListenerProtocolHTTP,
		PortClaim:  &claim,
	}
	mgr.registry["piclu"] = map[string]ServiceEndpoint{"web": ep}

	mgr.SuspendAppPublication("piclu")
	if len(fw.closed) != 1 {
		t.Fatalf("closed firewall rules = %v, want one close during suspend", fw.closed)
	}
	fw.opened = nil

	if got := mgr.ReconcileNetworkPublications(); got != 0 {
		t.Fatalf("reconciled publications = %d, want 0", got)
	}
	if len(fw.opened) != 0 {
		t.Fatalf("opened firewall rules = %v, want none for suspended app", fw.opened)
	}
}

func TestActivePortClaimsExcludeSuspendedApps(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := 35080
	if _, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}}); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if claims := mgr.ActivePortClaims(); len(claims) != 1 || claims[0].Port != claim {
		t.Fatalf("active claims before suspend = %+v, want one claim %d", claims, claim)
	}

	resumeToken := mgr.SuspendAppPublication("piclu")
	if claims := mgr.ActivePortClaims(); len(claims) != 0 {
		t.Fatalf("active claims while suspended = %+v, want none", claims)
	}

	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), resumeToken, "piclu"); err != nil {
		t.Fatalf("resume publication: %v", err)
	}
	if claims := mgr.ActivePortClaims(); len(claims) != 1 || claims[0].Port != claim {
		t.Fatalf("active claims after resume = %+v, want one claim %d", claims, claim)
	}
}

func TestPreparedReconcilePublishReportsListenerBindFailure(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	mgr.proxyManager.listenTCP = func(network, address string) (net.Listener, error) {
		return nil, errors.New("forced bind failure")
	}
	port := 35080

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &port,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()

	_, _, err = prepared.Publish()
	if err == nil {
		t.Fatalf("publish err = nil, want bind failure")
	}
	if !strings.Contains(err.Error(), "bind public listener") {
		t.Fatalf("publish err = %v, want bind failure", err)
	}
	if _, ok := mgr.GetAppListener("piclu", "web"); ok {
		t.Fatalf("failed publish should not update registry")
	}
}

func TestPreparedReconcilePublishRollsBackPartialStarts(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	recorder := &recordingPublication{}
	mgr.SetRemotePublisher(recorder)
	mgr.SetRemoteUnpublisher(recorder)
	attempts := 0
	mgr.proxyManager.listenTCP = func(network, address string) (net.Listener, error) {
		attempts++
		if attempts == 2 {
			return nil, errors.New("forced second bind failure")
		}
		return inMemoryTestListener{}, nil
	}
	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		{Name: "api", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}

	_, _, err = prepared.Publish()
	if err == nil {
		t.Fatalf("publish err = nil, want second bind failure")
	}
	if _, ok := mgr.GetAppListener("piclu", "web"); ok {
		t.Fatalf("failed publish should not update registry")
	}
	mgr.proxyManager.mu.Lock()
	listenerCount := len(mgr.proxyManager.listeners)
	mgr.proxyManager.mu.Unlock()
	if listenerCount != 0 {
		t.Fatalf("listeners after failed publish = %d, want none", listenerCount)
	}
	if len(recorder.published) != 1 || len(recorder.unpublished) != 1 || recorder.published[0] != recorder.unpublished[0] {
		t.Fatalf("publish/unpublish calls = %v/%v, want first start unwound", recorder.published, recorder.unpublished)
	}
	prepared.Release()
	if len(mgr.allocator.usedHost) != 0 || len(mgr.allocator.usedPublic) != 0 {
		t.Fatalf("allocations after release = host:%v public:%v, want none", mgr.allocator.usedHost, mgr.allocator.usedPublic)
	}
}

func TestReconcileRestoresProxyOnlyRestartOnPublishFailure(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	old := eps[0]
	attempts := 0
	mgr.proxyManager.listenTCP = func(network, address string) (net.Listener, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("forced restart bind failure")
		}
		return inMemoryTestListener{}, nil
	}
	auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}}}

	_, _, err = mgr.Reconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		Auth:      auth,
	}})
	if err == nil {
		t.Fatalf("reconcile err = nil, want restart bind failure")
	}
	ep, ok := mgr.GetAppListener("piclu", "web")
	if !ok {
		t.Fatalf("old endpoint missing after failed reconcile")
	}
	if ep.Auth != nil {
		t.Fatalf("registry auth after failed reconcile = %+v, want old nil auth", ep.Auth)
	}
	mgr.proxyManager.mu.Lock()
	_, running := mgr.proxyManager.listeners[old.PublicPort]
	mgr.proxyManager.mu.Unlock()
	if !running {
		t.Fatalf("old listener was not restored after failed proxy restart")
	}
}

func TestReconcileRollsBackEarlierProxyRestartsOnLaterFailure(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	oldEndpoints, err := mgr.AllocateForApp("piclu", []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		{Name: "api", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	attempts := 0
	mgr.proxyManager.listenTCP = func(network, address string) (net.Listener, error) {
		attempts++
		if attempts == 2 {
			return nil, errors.New("forced second restart bind failure")
		}
		return inMemoryTestListener{}, nil
	}
	auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}}}

	_, _, err = mgr.Reconcile("piclu", []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
		{Name: "api", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
	})
	if err == nil {
		t.Fatalf("reconcile err = nil, want second restart bind failure")
	}
	if attempts != 4 {
		t.Fatalf("listener start attempts = %d, want candidate1, candidate2, restore2, restore1", attempts)
	}
	for _, old := range oldEndpoints {
		ep, ok := mgr.GetAppListener("piclu", old.Name)
		if !ok {
			t.Fatalf("old endpoint %s missing after failed reconcile", old.Name)
		}
		if ep.Auth != nil {
			t.Fatalf("registry auth for %s after failed reconcile = %+v, want old nil auth", old.Name, ep.Auth)
		}
	}
	mgr.proxyManager.mu.Lock()
	defer mgr.proxyManager.mu.Unlock()
	for _, old := range oldEndpoints {
		if _, running := mgr.proxyManager.listeners[old.PublicPort]; !running {
			t.Fatalf("old listener %s on public %d was not restored", old.Name, old.PublicPort)
		}
	}
}

func TestPrepareReconcileReusesCurrentPublicPortWhenAddingClaim(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	old := eps[0]
	claim := old.PublicPort

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()
	if prepared.ContainerChange() {
		t.Fatalf("matching port claim should not require container change")
	}
	result := prepared.Result()
	if len(result.Added) != 0 || len(result.Removed) != 0 {
		t.Fatalf("result add/remove = %+v/%+v, want metadata update only", result.Added, result.Removed)
	}
	if len(result.Updated) != 1 || result.Updated[0].PublicPort != old.PublicPort {
		t.Fatalf("updated endpoints = %+v, want current public port metadata update", result.Updated)
	}

	if _, _, err := prepared.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	ep, ok := mgr.GetAppListener("piclu", "web")
	if !ok {
		t.Fatalf("listener missing after publish")
	}
	if ep.HostBind != old.HostBind || ep.PublicPort != old.PublicPort {
		t.Fatalf("published endpoint = %+v, want reused host/public from %+v", ep, old)
	}
	if ep.PortClaim == nil || *ep.PortClaim != old.PublicPort {
		t.Fatalf("published port claim = %v, want %d", ep.PortClaim, old.PublicPort)
	}
	claims := mgr.ActivePortClaims()
	if len(claims) != 1 || claims[0].Port != old.PublicPort || claims[0].HostBind != old.HostBind || claims[0].Protocol != old.Flow.TransportProtocol() {
		t.Fatalf("active port claims = %+v, want claim for reused endpoint %+v", claims, old)
	}
}

func TestPreparedReconcileRestartsClaimUpdateAfterSuspendedPublication(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	old := eps[0]
	claim := old.PublicPort

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()

	resumeToken := mgr.SuspendAppPublication("piclu")
	mgr.proxyManager.mu.Lock()
	_, runningBefore := mgr.proxyManager.listeners[old.PublicPort]
	mgr.proxyManager.mu.Unlock()
	if runningBefore {
		t.Fatalf("listener still running after suspended publication")
	}

	if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), resumeToken); err != nil {
		t.Fatalf("publish after suspended publication: %v", err)
	}
	mgr.proxyManager.mu.Lock()
	_, runningAfter := mgr.proxyManager.listeners[old.PublicPort]
	mgr.proxyManager.mu.Unlock()
	if !runningAfter {
		t.Fatalf("reused same-port claim listener was not restarted after suspended publication")
	}
	ep, ok := mgr.GetAppListener("piclu", "web")
	if !ok {
		t.Fatalf("listener missing after publish")
	}
	if ep.PortClaim == nil || *ep.PortClaim != old.PublicPort {
		t.Fatalf("published port claim = %v, want %d", ep.PortClaim, old.PublicPort)
	}
}

func TestPreparedReconcileDoesNotDoubleStartSamePortClaimProxyRestartAfterSuspension(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	starts := 0
	mgr.proxyManager.listenTCP = func(network, address string) (net.Listener, error) {
		starts++
		return inMemoryTestListener{}, nil
	}
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	old := eps[0]
	claim := old.PublicPort
	auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}}}

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
		Auth:      auth,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()
	if len(prepared.proxyRestart) != 1 || len(prepared.claimUpdate) != 1 {
		t.Fatalf("prepared restart/claim counts = %d/%d, want overlapping endpoint in both buckets", len(prepared.proxyRestart), len(prepared.claimUpdate))
	}

	resumeToken := mgr.SuspendAppPublication("piclu")
	startsBeforePublish := starts
	if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), resumeToken); err != nil {
		t.Fatalf("publish after suspended publication: %v", err)
	}
	if got := starts - startsBeforePublish; got != 1 {
		t.Fatalf("listener starts during publish = %d, want one shared start for proxy restart and claim update", got)
	}
	ep, ok := mgr.GetAppListener("piclu", "web")
	if !ok {
		t.Fatalf("listener missing after publish")
	}
	if ep.PortClaim == nil || *ep.PortClaim != old.PublicPort {
		t.Fatalf("published port claim = %v, want %d", ep.PortClaim, old.PublicPort)
	}
	if ep.Auth == nil {
		t.Fatalf("published listener auth missing")
	}
}

func TestPreparedReconcileRestartsUnchangedListenersAfterSuspendedPublication(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		{Name: "api", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	endpoints := map[string]ServiceEndpoint{}
	for _, ep := range eps {
		endpoints[ep.Name] = ep
	}
	auth := &api.ListenerAuth{Rules: []api.ListenerAuthRule{{Path: "/", Type: "prefix", Strategy: "public"}}}

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		{Name: "api", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Auth: auth},
	})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()

	resumeToken := mgr.SuspendAppPublication("piclu")
	if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), resumeToken); err != nil {
		t.Fatalf("publish after suspended publication: %v", err)
	}
	mgr.proxyManager.mu.Lock()
	_, webRunning := mgr.proxyManager.listeners[endpoints["web"].PublicPort]
	_, apiRunning := mgr.proxyManager.listeners[endpoints["api"].PublicPort]
	mgr.proxyManager.mu.Unlock()
	if !webRunning {
		t.Fatalf("unchanged listener web on public %d was not restarted", endpoints["web"].PublicPort)
	}
	if !apiRunning {
		t.Fatalf("changed listener api on public %d was not restarted", endpoints["api"].PublicPort)
	}
}

func TestPrepareReconcileReusesRemovedListenerPortClaim(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := 443
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	old := eps[0]

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "https",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()
	result := prepared.Result()
	if len(result.Added) != 1 || len(result.Removed) != 1 {
		t.Fatalf("result add/remove = %+v, want one renamed listener replacement", result)
	}
	if result.Added[0].PublicPort != old.PublicPort || result.Added[0].HostBind != old.HostBind {
		t.Fatalf("replacement endpoint = %+v, want reused host/public from %+v", result.Added[0], old)
	}

	if _, _, err := prepared.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	ep, ok := mgr.GetAppListener("piclu", "https")
	if !ok {
		t.Fatalf("renamed listener missing after publish")
	}
	if ep.PublicPort != old.PublicPort || ep.HostBind != old.HostBind {
		t.Fatalf("published endpoint = %+v, want reused host/public from %+v", ep, old)
	}
	if _, ok := mgr.GetAppListener("piclu", "web"); ok {
		t.Fatalf("old listener still present after rename")
	}
}

func TestPrepareReconcileReallocatesListenerOnTransportChange(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := 50053
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	old := eps[0]

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	result := prepared.Result()
	if len(result.Added) != 1 || len(result.Removed) != 1 {
		t.Fatalf("result add/remove = %+v, want transport replacement", result)
	}
	if result.Added[0].Flow != api.FlowUDP {
		t.Fatalf("replacement flow = %s, want udp", result.Added[0].Flow)
	}
	if result.Added[0].PublicPort != old.PublicPort {
		t.Fatalf("replacement public port = %d, want claimed port %d", result.Added[0].PublicPort, old.PublicPort)
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(old.PublicPort, "tcp")]; !ok {
		t.Fatalf("old tcp public claim released before publish")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(old.PublicPort, "udp")]; !ok {
		t.Fatalf("new udp public claim not reserved during prepare")
	}

	prepared.Release()
	if _, ok := mgr.allocator.usedPublic[publicKey(old.PublicPort, "tcp")]; !ok {
		t.Fatalf("old tcp public claim released after abandoned transport change")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(old.PublicPort, "udp")]; ok {
		t.Fatalf("new udp public claim still reserved after abandoned transport change")
	}
	ep, ok := mgr.GetAppListener("piclu", "dns")
	if !ok {
		t.Fatalf("listener missing after abandoned transport change")
	}
	if ep.Flow != api.FlowTCP || ep.PublicPort != old.PublicPort {
		t.Fatalf("published endpoint = %+v, want original tcp on public %d", ep, old.PublicPort)
	}
}

func TestPrepareReconcileMigratesAutoUDPToSamePortClaim(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("allocate auto udp listener: %v", err)
	}
	old := eps[0]
	claim := old.PublicPort
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "tcp")]; !ok {
		t.Fatalf("auto udp public port not tracked under tcp key before claim update")
	}

	resumeToken := mgr.SuspendAppPublication("piclu")
	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("prepare same-port udp claim: %v", err)
	}
	if prepared.ContainerChange() {
		t.Fatalf("same-port udp claim should not require container change")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "tcp")]; !ok {
		t.Fatalf("old auto public key released before publish")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "udp")]; !ok {
		t.Fatalf("new udp claim key not reserved during prepare")
	}
	if _, err := mgr.PrepareReconcile("other", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claim,
	}}); err == nil {
		t.Fatalf("second app should not prepare duplicate udp claim while first update is pending")
	}
	prepared.Release()
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "tcp")]; !ok {
		t.Fatalf("old auto public key released after abandoned claim update")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "udp")]; ok {
		t.Fatalf("udp claim key still reserved after abandoned claim update")
	}

	prepared, err = mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("prepare same-port udp claim for publish: %v", err)
	}
	if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), resumeToken); err != nil {
		t.Fatalf("publish same-port udp claim: %v", err)
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "tcp")]; ok {
		t.Fatalf("old auto public key still reserved after claim publish")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(claim, "udp")]; !ok {
		t.Fatalf("udp claim key missing after claim publish")
	}
	ep, ok := mgr.GetAppListener("piclu", "dns")
	if !ok {
		t.Fatalf("listener missing after claim publish")
	}
	if ep.PortClaim == nil || *ep.PortClaim != claim {
		t.Fatalf("published port claim = %v, want %d", ep.PortClaim, claim)
	}
	mgr.RemoveApp("piclu")
	if len(mgr.allocator.usedPublic) != 0 {
		t.Fatalf("public allocations after removing claimed udp listener = %v, want none", mgr.allocator.usedPublic)
	}
}

func TestPrepareReconcileRemovesUDPClaimWithoutReusingClaimedPublicPort(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := mgr.allocator.publicRange.Start
	mgr.allocator.nextPublic = claim
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("allocate claimed udp listener: %v", err)
	}
	old := eps[0]
	if old.PublicPort != claim {
		t.Fatalf("old public port = %d, want claim %d", old.PublicPort, claim)
	}

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("prepare udp claim removal: %v", err)
	}
	result := prepared.Result()
	if len(result.Added) != 1 || len(result.Removed) != 1 {
		t.Fatalf("prepare result added/removed = %+v/%+v, want replacement", result.Added, result.Removed)
	}
	replacement := result.Added[0]
	if replacement.PublicPort == old.PublicPort {
		t.Fatalf("replacement reused old udp claim public port %d", old.PublicPort)
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(old.PublicPort, "udp")]; !ok {
		t.Fatalf("old udp claim key released before publish")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(replacement.PublicPort, "tcp")]; !ok {
		t.Fatalf("replacement auto public key not reserved before publish")
	}
	if _, _, err := prepared.Publish(); err != nil {
		t.Fatalf("publish udp claim removal: %v", err)
	}

	mgr.proxyManager.mu.Lock()
	_, oldRunning := mgr.proxyManager.udpListeners[old.PublicPort]
	_, replacementRunning := mgr.proxyManager.udpListeners[replacement.PublicPort]
	mgr.proxyManager.mu.Unlock()
	if oldRunning {
		t.Fatalf("old udp listener still running on public %d", old.PublicPort)
	}
	if !replacementRunning {
		t.Fatalf("replacement udp listener not running on public %d", replacement.PublicPort)
	}
	ep, ok := mgr.GetAppListener("piclu", "dns")
	if !ok {
		t.Fatalf("listener missing after claim removal publish")
	}
	if ep.PortClaim != nil || ep.PublicPort != replacement.PublicPort {
		t.Fatalf("published endpoint = %+v, want auto public %d", ep, replacement.PublicPort)
	}
	mgr.RemoveApp("piclu")
}

func TestPrepareReconcileRestoresAutoUDPKeysAfterPartialClaimPublishFailure(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{
		{Name: "dns-a", GuestPort: 5353, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw},
		{Name: "dns-b", GuestPort: 5354, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw},
	})
	if err != nil {
		t.Fatalf("allocate auto udp listeners: %v", err)
	}
	var first, second ServiceEndpoint
	for _, ep := range eps {
		switch ep.Name {
		case "dns-a":
			first = ep
		case "dns-b":
			second = ep
		}
	}
	if first.PublicPort == 0 || second.PublicPort == 0 {
		t.Fatalf("allocated endpoints = %+v, want dns-a and dns-b", eps)
	}
	firstClaim := first.PublicPort
	secondClaim := second.PublicPort

	resumeToken := mgr.SuspendAppPublication("piclu")
	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{
		{Name: "dns-a", GuestPort: 5353, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, PortClaim: &firstClaim},
		{Name: "dns-b", GuestPort: 5354, Flow: api.FlowUDP, Protocol: api.ListenerProtocolRaw, PortClaim: &secondClaim},
	})
	if err != nil {
		t.Fatalf("prepare same-port udp claims: %v", err)
	}
	udpStarts := 0
	mgr.proxyManager.listenUDP = func(network string, laddr *net.UDPAddr) (udpPacketConn, error) {
		udpStarts++
		if udpStarts == 2 {
			return nil, errors.New("forced udp bind failure")
		}
		return inMemoryTestUDPConn{}, nil
	}

	if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), resumeToken); err == nil {
		t.Fatalf("publish same-port udp claims err = nil, want second bind failure")
	}
	prepared.Release()
	for _, ep := range []ServiceEndpoint{first, second} {
		if _, ok := mgr.allocator.usedPublic[publicKey(ep.PublicPort, "tcp")]; !ok {
			t.Fatalf("old auto public key %s missing after partial publish rollback", publicKey(ep.PublicPort, "tcp"))
		}
		if _, ok := mgr.allocator.usedPublic[publicKey(ep.PublicPort, "udp")]; ok {
			t.Fatalf("new udp claim key %s still reserved after partial publish rollback", publicKey(ep.PublicPort, "udp"))
		}
		stored, ok := mgr.GetAppListener("piclu", ep.Name)
		if !ok {
			t.Fatalf("old listener %s missing after partial publish rollback", ep.Name)
		}
		if stored.PortClaim != nil {
			t.Fatalf("old listener %s port claim = %v, want nil", ep.Name, stored.PortClaim)
		}
	}
}

func TestRestorePreparedPublicationAutoUDPUsesAutoPublicKey(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("allocate udp listener: %v", err)
	}
	restored := eps[0]
	mgr.RemoveApp("piclu")
	if len(mgr.allocator.usedHost) != 0 || len(mgr.allocator.usedPublic) != 0 {
		t.Fatalf("allocations after remove = host:%v public:%v, want none", mgr.allocator.usedHost, mgr.allocator.usedPublic)
	}

	if err := mgr.RestorePreparedPublication("piclu", []ServiceEndpoint{restored}); err != nil {
		t.Fatalf("restore prepared udp publication: %v", err)
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(restored.PublicPort, "tcp")]; !ok {
		t.Fatalf("restored auto udp public port tracked under %s, want tcp key", restored.Flow.TransportProtocol())
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(restored.PublicPort, "udp")]; ok {
		t.Fatalf("restored auto udp public port tracked under udp key")
	}
	mgr.RemoveApp("piclu")
	if len(mgr.allocator.usedPublic) != 0 {
		t.Fatalf("public allocations after restored udp removal = %v, want none", mgr.allocator.usedPublic)
	}
}

func TestRestoreFromPodmanAutoUDPSkipsUDPUnavailablePublicPort(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	mgr.allocator.publicRange = PortRange{Start: 35000, End: 35002}
	mgr.allocator.nextPublic = 35000
	mgr.allocator.portAvailable = func(host string, port int, network string) bool {
		_ = host
		return !(network == "udp" && port == 35000)
	}

	eps, err := mgr.RestoreFromPodman("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}}, map[string]int{"5353/udp": 15053})
	if err != nil {
		t.Fatalf("restore udp listener: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("restored endpoints = %d, want 1", len(eps))
	}
	if eps[0].PublicPort != 35001 {
		t.Fatalf("restored UDP public port = %d, want 35001 after skipping UDP-unavailable 35000", eps[0].PublicPort)
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(35000, "tcp")]; ok {
		t.Fatalf("UDP-unavailable port was reserved under auto key")
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(35001, "tcp")]; !ok {
		t.Fatalf("restored UDP auto port not tracked under auto key")
	}
}

func TestRestoreFromPodmanRejectsPartialBindingsBeforeWithdrawingRoutes(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	listeners := []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		{Name: "admin", GuestPort: 9090, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}
	before, err := mgr.AllocateForApp("piclu", listeners)
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}

	_, err = mgr.RestoreFromPodman("piclu", listeners, map[string]int{"8080/tcp": before[0].HostBind})
	if !errors.Is(err, ErrPublicationRestoreIncomplete) {
		t.Fatalf("partial restore error = %v, want %v", err, ErrPublicationRestoreIncomplete)
	}
	after, err := mgr.GetByApp("piclu")
	if err != nil || len(after) != len(before) {
		t.Fatalf("last-known routes after rejected restore = %v, err=%v; want %d routes", after, err, len(before))
	}
	if !mgr.AppPublicationActive("piclu") {
		t.Fatal("partial restore withdrew the complete last-known publication")
	}
}

func TestRestoreFromPodmanReservationFailureRollsBackWholeRestore(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	listeners := []api.AppListener{
		{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		{Name: "admin", GuestPort: 9090, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}

	_, err := mgr.RestoreFromPodman("piclu", listeners, map[string]int{
		"8080/tcp": 15080,
		"9090/tcp": 15080,
	})
	if !errors.Is(err, ErrPublicationRestoreIncomplete) {
		t.Fatalf("duplicate host restore error = %v, want %v", err, ErrPublicationRestoreIncomplete)
	}
	if _, err := mgr.GetByApp("piclu"); err == nil {
		t.Fatal("failed restore retained a partial registry")
	}
	if len(mgr.allocator.usedHost) != 0 || len(mgr.allocator.usedPublic) != 0 {
		t.Fatalf("failed restore leaked allocations: host=%v public=%v", mgr.allocator.usedHost, mgr.allocator.usedPublic)
	}
}

func TestSuspendWaitsForRestoreFromPodmanRouteReplacement(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := 18080
	listeners := []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}}
	eps, err := mgr.AllocateForApp("piclu", listeners)
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	fw := newBlockingLifecycleFirewall(mgr)
	mgr.SetFirewallManager(fw)

	restoreDone := make(chan error, 1)
	go func() {
		_, restoreErr := mgr.RestoreFromPodmanContext(context.Background(), "piclu", listeners, map[string]int{"8080/tcp": eps[0].HostBind})
		restoreDone <- restoreErr
	}()
	fw.waitForOpen(t)
	if !fw.lifecycleHeld() {
		t.Fatal("RestoreFromPodman released publication lifecycle authority before replacement activation")
	}

	suspendStarted := make(chan struct{})
	suspendDone := make(chan struct{})
	go func() {
		close(suspendStarted)
		mgr.SuspendAppPublication("piclu")
		close(suspendDone)
	}()
	<-suspendStarted
	select {
	case <-suspendDone:
		t.Fatal("suspend completed before in-flight route replacement released lifecycle authority")
	case <-time.After(20 * time.Millisecond):
	}
	fw.releaseOpen()
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore from podman: %v", err)
	}
	<-suspendDone
	assertAppPublicationSuspended(t, mgr, "piclu")
}

func TestSuspendWaitsForRestorePreparedRouteReplacement(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := 18081
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	restored := eps[0]
	mgr.RemoveApp("piclu")
	fw := newBlockingLifecycleFirewall(mgr)
	mgr.SetFirewallManager(fw)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.RestorePreparedPublicationContext(context.Background(), "piclu", []ServiceEndpoint{restored})
	}()
	fw.waitForOpen(t)
	if !fw.lifecycleHeld() {
		t.Fatal("RestorePreparedPublication replaced routes outside publication lifecycle authority")
	}

	suspendStarted := make(chan struct{})
	suspendDone := make(chan struct{})
	go func() {
		close(suspendStarted)
		mgr.SuspendAppPublication("piclu")
		close(suspendDone)
	}()
	<-suspendStarted
	select {
	case <-suspendDone:
		t.Fatal("suspend completed before prepared route replacement released lifecycle authority")
	case <-time.After(20 * time.Millisecond):
	}
	fw.releaseOpen()
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore prepared publication: %v", err)
	}
	<-suspendDone
	assertAppPublicationSuspended(t, mgr, "piclu")
}

func TestSuspensionAuthorityGatesDelayedActivators(t *testing.T) {
	listeners := []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}}

	t.Run("restore from podman", func(t *testing.T) {
		mgr := NewServiceManager()
		mgr.UseInMemoryNetworkForTest()
		eps, err := mgr.AllocateForApp("piclu", listeners)
		if err != nil {
			t.Fatalf("allocate app: %v", err)
		}
		token := mgr.SuspendAppPublication("piclu")
		inactive := mgr.deactivated["piclu"]

		_, err = mgr.RestoreFromPodmanContext(context.Background(), "piclu", listeners, map[string]int{"8080/tcp": eps[0].HostBind})
		if !errors.Is(err, ErrPublicationSuspended) {
			t.Fatalf("passive restore error = %v, want publication suspended", err)
		}
		if mgr.deactivated["piclu"] != inactive || mgr.AppPublicationActive("piclu") {
			t.Fatal("denied restore changed the suspended publication")
		}

		if _, err := mgr.RestoreFromPodmanWithResumeTokenContext(context.Background(), token, "piclu", listeners, map[string]int{"8080/tcp": eps[0].HostBind}); err != nil {
			t.Fatalf("authorized restore: %v", err)
		}
		if !mgr.AppPublicationActive("piclu") {
			t.Fatal("authorized restore did not reactivate publication")
		}
	})

	t.Run("restore prepared", func(t *testing.T) {
		mgr := NewServiceManager()
		mgr.UseInMemoryNetworkForTest()
		eps, err := mgr.AllocateForApp("piclu", listeners)
		if err != nil {
			t.Fatalf("allocate app: %v", err)
		}
		token := mgr.SuspendAppPublication("piclu")
		inactive := mgr.deactivated["piclu"]

		err = mgr.RestorePreparedPublicationContext(context.Background(), "piclu", eps)
		if !errors.Is(err, ErrPublicationSuspended) {
			t.Fatalf("passive prepared restore error = %v, want publication suspended", err)
		}
		if mgr.deactivated["piclu"] != inactive || mgr.AppPublicationActive("piclu") {
			t.Fatal("denied prepared restore changed the suspended publication")
		}

		if err := mgr.RestorePreparedPublicationWithResumeTokenContext(context.Background(), token, "piclu", eps); err != nil {
			t.Fatalf("authorized prepared restore: %v", err)
		}
		if !mgr.AppPublicationActive("piclu") {
			t.Fatal("authorized prepared restore did not reactivate publication")
		}
	})

	t.Run("prepared publish", func(t *testing.T) {
		mgr := NewServiceManager()
		mgr.UseInMemoryNetworkForTest()
		if _, err := mgr.AllocateForApp("piclu", listeners); err != nil {
			t.Fatalf("allocate app: %v", err)
		}
		prepared, err := mgr.PrepareReconcile("piclu", listeners)
		if err != nil {
			t.Fatalf("prepare reconcile: %v", err)
		}
		defer prepared.Release()
		token := mgr.SuspendAppPublication("piclu")
		inactive := mgr.deactivated["piclu"]

		if _, _, err := prepared.PublishContext(context.Background()); !errors.Is(err, ErrPublicationSuspended) {
			t.Fatalf("passive prepared publish error = %v, want publication suspended", err)
		}
		if prepared.published || mgr.deactivated["piclu"] != inactive || mgr.AppPublicationActive("piclu") {
			t.Fatal("denied prepared publish changed the suspended publication")
		}

		if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), token); err != nil {
			t.Fatalf("authorized prepared publish: %v", err)
		}
		if !mgr.AppPublicationActive("piclu") {
			t.Fatal("authorized prepared publish did not reactivate publication")
		}
	})
}

func TestDeniedPreparedPublishReleaseFreesCandidateReservations(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	endpoints := prepared.Endpoints()
	if len(endpoints) != 1 {
		t.Fatalf("prepared endpoints = %+v, want one", endpoints)
	}
	token := mgr.SuspendAppPublication("piclu")
	if _, _, err := prepared.PublishContext(context.Background()); !errors.Is(err, ErrPublicationSuspended) {
		t.Fatalf("passive publish error = %v, want publication suspended", err)
	}
	prepared.Release()
	if _, used := mgr.allocator.usedHost[endpoints[0].HostBind]; used {
		t.Fatalf("denied prepared host reservation %d survived release", endpoints[0].HostBind)
	}
	if _, used := mgr.allocator.usedPublic[endpointPublicAllocationKey(endpoints[0])]; used {
		t.Fatalf("denied prepared public reservation %s survived release", endpointPublicAllocationKey(endpoints[0]))
	}
	if mgr.deactivated["piclu"] != token.record {
		t.Fatal("release invalidated the owning suspension")
	}
}

func TestRepeatedSuspensionInvalidatesOlderResumeToken(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	if _, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}}); err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	oldToken := mgr.SuspendAppPublication("piclu")
	currentToken := mgr.SuspendAppPublication("piclu")
	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), oldToken, "piclu"); !errors.Is(err, ErrPublicationSuspended) {
		t.Fatalf("stale resume error = %v, want publication suspended", err)
	}
	if mgr.deactivated["piclu"] != currentToken.record || mgr.AppPublicationActive("piclu") {
		t.Fatal("stale token changed the current suspension")
	}
	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), currentToken, "piclu"); err != nil {
		t.Fatalf("current token resume: %v", err)
	}
}

func TestStoppedPublicationAllowsPassiveRestore(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	listeners := []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}}
	eps, err := mgr.AllocateForApp("piclu", listeners)
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	mgr.DeactivateApp("piclu")
	if inactive := mgr.deactivated["piclu"]; inactive == nil || inactive.kind != publicationInactiveStopped {
		t.Fatalf("inactive record = %+v, want ordinary stopped publication", inactive)
	}
	if _, err := mgr.RestoreFromPodmanContext(context.Background(), "piclu", listeners, map[string]int{"8080/tcp": eps[0].HostBind}); err != nil {
		t.Fatalf("passive stopped restore: %v", err)
	}
	if !mgr.AppPublicationActive("piclu") {
		t.Fatal("passive stopped restore did not reactivate publication")
	}
}

func TestSuspendedPublicationRejectsPassiveAllocateAndResume(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	listeners := []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
	}}
	if _, err := mgr.AllocateForApp("piclu", listeners); err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	token := mgr.SuspendAppPublication("piclu")
	if _, err := mgr.AllocateForApp("piclu", listeners); !errors.Is(err, ErrPublicationSuspended) {
		t.Fatalf("passive allocate error = %v, want publication suspended", err)
	}
	if err := mgr.ResumeAppPublicationCheckedContext(context.Background(), "piclu"); !errors.Is(err, ErrPublicationSuspended) {
		t.Fatalf("passive resume error = %v, want publication suspended", err)
	}
	if mgr.deactivated["piclu"] != token.record || mgr.AppPublicationActive("piclu") {
		t.Fatal("passive activation changed the suspension")
	}
	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), token, "piclu"); err != nil {
		t.Fatalf("authorized resume: %v", err)
	}
}

func TestClaimedPortResumeCarriesWarningContinuationAndRetainsPendingOnFailure(t *testing.T) {
	pressure.DefaultAdmission.ResetForTest()
	t.Cleanup(pressure.DefaultAdmission.ResetForTest)

	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	claim := 18082
	if _, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}}); err != nil {
		t.Fatalf("allocate app: %v", err)
	}
	fw := &admissionCheckingFirewall{}
	mgr.SetFirewallManager(fw)
	resumeToken := mgr.SuspendAppPublication("piclu")
	pressure.DefaultAdmission.Fence()
	closedBeforeFailedResume := len(fw.closed)

	if err := mgr.ResumeAppPublicationWithResumeTokenContext(context.Background(), resumeToken, "piclu"); !pressure.IsAdmissionError(err) {
		t.Fatalf("resume without continuation err = %v, want task-pressure admission error", err)
	}
	assertAppPublicationSuspended(t, mgr, "piclu")
	if got := len(fw.opened); got != 0 {
		t.Fatalf("failed resume opened %d firewall rules, want none", got)
	}
	if got := len(fw.closed); got != closedBeforeFailedResume+1 {
		t.Fatalf("failed resume cleanup closes = %d, want %d", got, closedBeforeFailedResume+1)
	}

	continuationCtx := pressure.WithTransitionContinuation(context.Background())
	if err := mgr.ResumeAppPublicationWithResumeTokenContext(continuationCtx, resumeToken, "piclu"); err != nil {
		t.Fatalf("resume with durable transition continuation: %v", err)
	}
	if got := len(fw.opened); got != 1 {
		t.Fatalf("continued resume opened %d firewall rules, want one", got)
	}
	if !mgr.AppPublicationActive("piclu") {
		t.Fatal("continued resume did not commit active publication")
	}
}

func TestRestorePreparedPublicationKeepsPreparedReservationsAfterPublishFailure(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("allocate udp listener: %v", err)
	}
	restored := eps[0]
	mgr.RemoveApp("piclu")
	failNextUDPStart := true
	mgr.proxyManager.listenUDP = func(network string, laddr *net.UDPAddr) (udpPacketConn, error) {
		if failNextUDPStart {
			failNextUDPStart = false
			return nil, errors.New("forced udp bind failure")
		}
		return inMemoryTestUDPConn{}, nil
	}

	err = mgr.RestorePreparedPublication("piclu", []ServiceEndpoint{restored})
	if err == nil {
		t.Fatalf("restore prepared publication err = nil, want bind failure")
	}
	if _, ok := mgr.allocator.usedHost[restored.HostBind]; !ok {
		t.Fatalf("prepared host bind %d was not kept reserved after restore failure", restored.HostBind)
	}
	if _, ok := mgr.allocator.usedPublic[publicKey(restored.PublicPort, "tcp")]; !ok {
		t.Fatalf("prepared auto udp public port %d was not kept reserved under tcp key after restore failure", restored.PublicPort)
	}
	if _, ok := mgr.GetAppListener("piclu", "dns"); ok {
		t.Fatalf("failed restore should not publish registry endpoint")
	}
	other := restored
	other.App = "other"
	other.Name = "dns"
	other.HostBind++
	if err := mgr.RestorePreparedPublication("other", []ServiceEndpoint{other}); err == nil {
		t.Fatalf("conflicting restore err = nil, want prepared reservation conflict")
	}
	if _, ok := mgr.GetAppListener("other", "dns"); ok {
		t.Fatalf("conflicting restore should not publish other endpoint")
	}
	if err := mgr.RestorePreparedPublication("piclu", []ServiceEndpoint{restored}); err != nil {
		t.Fatalf("retry restore prepared publication: %v", err)
	}
	if _, ok := mgr.GetAppListener("piclu", "dns"); !ok {
		t.Fatalf("retry restore did not publish endpoint")
	}
	mgr.RemoveApp("piclu")
	if len(mgr.allocator.usedHost) != 0 || len(mgr.allocator.usedPublic) != 0 {
		t.Fatalf("allocations after remove = host:%v public:%v, want none", mgr.allocator.usedHost, mgr.allocator.usedPublic)
	}
}

func TestAllocateForAppRejectsUDPClaimAgainstSuspendedAutoUDPReservation(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("allocate auto udp listener: %v", err)
	}
	claimed := eps[0].PublicPort
	mgr.SuspendAppPublication("piclu")

	_, err = mgr.AllocateForApp("other", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claimed,
	}})
	if err == nil {
		t.Fatalf("other app claimed suspended auto udp public port %d", claimed)
	}
	if !strings.Contains(err.Error(), "already owned by piclu/dns") {
		t.Fatalf("claim error = %v, want piclu/dns ownership", err)
	}
}

func TestAllocateForAppRejectsUDPClaimAgainstPreparedAutoUDPReservation(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	eps, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("allocate auto udp listener: %v", err)
	}
	restored := eps[0]
	mgr.RemoveApp("piclu")
	failNextUDPStart := true
	mgr.proxyManager.listenUDP = func(network string, laddr *net.UDPAddr) (udpPacketConn, error) {
		if failNextUDPStart {
			failNextUDPStart = false
			return nil, errors.New("forced udp bind failure")
		}
		return inMemoryTestUDPConn{}, nil
	}
	if err := mgr.RestorePreparedPublication("piclu", []ServiceEndpoint{restored}); err == nil {
		t.Fatalf("restore prepared publication err = nil, want bind failure")
	}

	claimed := restored.PublicPort
	_, err = mgr.AllocateForApp("other", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
		PortClaim: &claimed,
	}})
	if err == nil {
		t.Fatalf("other app claimed prepared auto udp public port %d", claimed)
	}
	if !strings.Contains(err.Error(), "already owned by piclu/dns") {
		t.Fatalf("claim error = %v, want piclu/dns ownership", err)
	}
}

func TestRestorePreparedPublicationRejectsConflictingRestoredPublicPort(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	otherEndpoints, err := mgr.AllocateForApp("other", []api.AppListener{{
		Name:      "dns",
		GuestPort: 5353,
		Flow:      api.FlowUDP,
		Protocol:  api.ListenerProtocolRaw,
	}})
	if err != nil {
		t.Fatalf("allocate other udp listener: %v", err)
	}
	other := otherEndpoints[0]
	restored := other
	restored.App = "piclu"
	restored.HostBind++

	err = mgr.RestorePreparedPublication("piclu", []ServiceEndpoint{restored})
	if err == nil {
		t.Fatalf("restore prepared publication err = nil, want public port conflict")
	}
	if !strings.Contains(err.Error(), "already owned by other/dns") {
		t.Fatalf("restore prepared publication err = %v, want other ownership conflict", err)
	}
	if _, ok := mgr.GetAppListener("piclu", "dns"); ok {
		t.Fatalf("conflicting restore should not publish piclu endpoint")
	}
	if _, ok := mgr.GetAppListener("other", "dns"); !ok {
		t.Fatalf("conflicting restore removed existing owner endpoint")
	}
}

func TestPrepareReconcileRestartsProxyOnMiddlewareParamChange(t *testing.T) {
	mgr := NewServiceManager()
	mgr.UseInMemoryNetworkForTest()
	_, err := mgr.AllocateForApp("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		Middleware: []api.AppProtocolMiddleware{{
			Name:   "ip_rate_limit",
			Params: map[string]any{"per_second": float64(10), "burst": float64(20)},
		}},
	}})
	if err != nil {
		t.Fatalf("allocate app: %v", err)
	}

	prepared, err := mgr.PrepareReconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 8080,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		Middleware: []api.AppProtocolMiddleware{{
			Name:   "ip_rate_limit",
			Params: map[string]any{"per_second": float64(5), "burst": float64(20)},
		}},
	}})
	if err != nil {
		t.Fatalf("prepare reconcile: %v", err)
	}
	defer prepared.Release()
	result := prepared.Result()
	if len(result.ProxyOnlyChanged) != 1 {
		t.Fatalf("proxy-only changes = %+v, want middleware param restart", result.ProxyOnlyChanged)
	}
}

func TestReconcileNetworkPublicationsReopensRegisteredPortClaims(t *testing.T) {
	mgr := NewServiceManager()
	useFakeProxyListeners(mgr)
	fw := &recordingFirewall{}
	mgr.SetFirewallManager(fw)

	claim := 8080
	if _, _, err := mgr.Reconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 80,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fw.opened = nil

	if got := mgr.ReconcileNetworkPublications(); got != 1 {
		t.Fatalf("reconciled endpoints = %d, want 1", got)
	}
	if len(fw.opened) != 1 || fw.opened[0].Port != 8080 || fw.opened[0].Protocol != "tcp" {
		t.Fatalf("opened = %+v, want 8080/tcp", fw.opened)
	}
}

func TestCloseNetworkPublicationsClosesRegisteredPortClaims(t *testing.T) {
	mgr := NewServiceManager()
	useFakeProxyListeners(mgr)
	fw := &recordingFirewall{}
	mgr.SetFirewallManager(fw)

	claim := 8080
	if _, _, err := mgr.Reconcile("piclu", []api.AppListener{{
		Name:      "web",
		GuestPort: 80,
		Flow:      api.FlowTCP,
		Protocol:  api.ListenerProtocolHTTP,
		PortClaim: &claim,
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fw.closed = nil

	if got := mgr.CloseNetworkPublications(); got != 1 {
		t.Fatalf("closed endpoints = %d, want 1", got)
	}
	if len(fw.closed) != 1 || fw.closed[0].Port != 8080 || fw.closed[0].Protocol != "tcp" {
		t.Fatalf("closed = %+v, want 8080/tcp", fw.closed)
	}
}

func useFakeProxyListeners(mgr *ServiceManager) {
	mgr.UseInMemoryNetworkForTest()
}

type recordingPublication struct {
	published   []int
	unpublished []int
}

func (r *recordingPublication) Publish(port int) {
	r.published = append(r.published, port)
}

func (r *recordingPublication) Unpublish(port int) {
	r.unpublished = append(r.unpublished, port)
}

type recordingFirewall struct {
	opened []firewall.Rule
	closed []firewall.Rule
}

func (r *recordingFirewall) OpenPort(_ context.Context, rule firewall.Rule) error {
	r.opened = append(r.opened, rule)
	return nil
}

func (r *recordingFirewall) ClosePort(_ context.Context, rule firewall.Rule) error {
	r.closed = append(r.closed, rule)
	return nil
}

type blockingLifecycleFirewall struct {
	manager       *ServiceManager
	openStarted   chan struct{}
	openRelease   chan struct{}
	openOnce      sync.Once
	releaseOnce   sync.Once
	heldLifecycle bool
}

func newBlockingLifecycleFirewall(manager *ServiceManager) *blockingLifecycleFirewall {
	return &blockingLifecycleFirewall{
		manager:     manager,
		openStarted: make(chan struct{}),
		openRelease: make(chan struct{}),
	}
}

func (f *blockingLifecycleFirewall) OpenPort(_ context.Context, _ firewall.Rule) error {
	f.openOnce.Do(func() {
		if f.manager.publicationLifecycleMu.TryLock() {
			f.manager.publicationLifecycleMu.Unlock()
			f.heldLifecycle = false
		} else {
			f.heldLifecycle = true
		}
		close(f.openStarted)
		<-f.openRelease
	})
	return nil
}

func (f *blockingLifecycleFirewall) ClosePort(_ context.Context, _ firewall.Rule) error {
	return nil
}

func (f *blockingLifecycleFirewall) waitForOpen(t *testing.T) {
	t.Helper()
	select {
	case <-f.openStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for firewall publication")
	}
}

func (f *blockingLifecycleFirewall) lifecycleHeld() bool {
	return f.heldLifecycle
}

func (f *blockingLifecycleFirewall) releaseOpen() {
	f.releaseOnce.Do(func() { close(f.openRelease) })
}

type admissionCheckingFirewall struct {
	opened []firewall.Rule
	closed []firewall.Rule
}

func (f *admissionCheckingFirewall) OpenPort(ctx context.Context, rule firewall.Rule) error {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkNetworkProbe); err != nil {
		return err
	}
	f.opened = append(f.opened, rule)
	return nil
}

func (f *admissionCheckingFirewall) ClosePort(ctx context.Context, rule firewall.Rule) error {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkNetworkProbe); err != nil {
		return err
	}
	f.closed = append(f.closed, rule)
	return nil
}

func assertAppPublicationSuspended(t *testing.T, mgr *ServiceManager, appName string) {
	t.Helper()
	if _, inactive := mgr.deactivated[appName]; !inactive {
		t.Fatalf("app %s has no suspended publication marker", appName)
	}
	if mgr.AppPublicationActive(appName) {
		t.Fatalf("app %s publication is active, want suspended", appName)
	}
}
