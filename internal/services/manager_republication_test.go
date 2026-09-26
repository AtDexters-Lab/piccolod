package services

import (
	"context"
	"errors"
	"net"
	"testing"

	"piccolod/internal/api"
	"piccolod/internal/events"
)

func TestPreparedReconcileReadvertisesSuspendedPublication(t *testing.T) {
	for _, name := range []string{"unchanged", "claim_changed", "removed_listener"} {
		t.Run(name, func(t *testing.T) {
			mgr := NewServiceManager()
			useFakeProxyListeners(mgr)
			listeners := []api.AppListener{{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}}
			if name == "removed_listener" {
				listeners = append(listeners, api.AppListener{Name: "extra", GuestPort: 8081, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP})
			}
			endpoints, err := mgr.AllocateForApp("landing", listeners)
			if err != nil {
				t.Fatal(err)
			}
			hostLabel := ""
			for _, ep := range endpoints {
				if ep.Name == "web" {
					hostLabel = ep.DerivedHostLabel
				}
			}
			bus := events.NewBus()
			mgr.eventBus = bus
			ch, cancel := bus.SubscribeWithCancel(events.TopicServiceEndpointsChanged, 8)
			defer cancel()
			token := mgr.SuspendAppPublication("landing")
			<-ch // The withdrawal event precedes reactivation.
			if name == "claim_changed" {
				// Make the existing allocation an explicit claim. This creates
				// a configuration Updated delta without changing the route.
				listeners[0].PortClaim = &endpoints[0].PublicPort
			} else if name == "removed_listener" {
				listeners = listeners[:1]
			}
			advertised := 0
			mgr.SetRuntimePublicationCallbacks(nil, func() {
				advertised++
				// Production callbacks resolve routes under mu. This also proves
				// notification occurs after the active projection is committed.
				if _, ok := mgr.ResolveByHostLabelAnyPort(hostLabel); !ok {
					t.Error("advertisement cannot resolve the restored route")
				}
			})
			prepared, err := mgr.PrepareReconcile("landing", listeners)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := prepared.PublishWithResumeTokenContext(context.Background(), token)
			if err != nil {
				t.Fatal(err)
			}
			if advertised != 1 {
				t.Fatalf("runtime advertisements = %d, want 1", advertised)
			}
			select {
			case evt := <-ch:
				payload := evt.Payload.(events.ServiceEndpointsChanged)
				if len(payload.Added) != 1 || payload.Added[0].Name != "web" || len(payload.Updated) != 0 {
					t.Fatalf("restored discovery event = %+v, want the complete route set without duplicate labels", payload)
				}
				if name == "removed_listener" && (len(payload.Removed) != 1 || payload.Removed[0].Name != "extra") {
					t.Fatalf("restoration lost removed discovery labels: %+v", payload)
				}
			default:
				t.Fatal("missing discovery restoration event")
			}
			if name == "claim_changed" && len(result.Updated) != 1 {
				t.Fatalf("configuration result lost its update: %+v", result)
			}
			if name == "removed_listener" && len(result.Removed) != 1 {
				t.Fatalf("configuration result lost its removal: %+v", result)
			}
			if name == "unchanged" && (len(result.Added) != 0 || len(result.Updated) != 0 || len(result.Removed) != 0) {
				t.Fatalf("reactivation changed the configuration result: %+v", result)
			}
			if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), token); err != nil {
				t.Fatal(err)
			}
			if _, _, err := mgr.Reconcile("landing", listeners); err != nil {
				t.Fatal(err)
			}
			if advertised != 1 {
				t.Fatal("repeated publication or passive reconcile advertised again")
			}
			select {
			case evt := <-ch:
				t.Fatalf("unexpected repeated discovery event: %+v", evt)
			default:
			}
		})
	}
}

func TestPreparedReconcileFailedReactivationDoesNotAdvertise(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "bind_failure"
		if stale {
			name = "stale_token"
		}
		t.Run(name, func(t *testing.T) {
			mgr := NewServiceManager()
			useFakeProxyListeners(mgr)
			listeners := []api.AppListener{{Name: "web", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP}}
			if _, err := mgr.AllocateForApp("landing", listeners); err != nil {
				t.Fatal(err)
			}
			token := mgr.SuspendAppPublication("landing")
			if stale {
				mgr.SuspendAppPublication("landing")
			} else {
				mgr.proxyManager.listenTCP = func(string, string) (net.Listener, error) {
					return nil, errors.New("forced bind failure")
				}
			}
			bus := events.NewBus()
			mgr.eventBus = bus
			ch, cancel := bus.SubscribeWithCancel(events.TopicServiceEndpointsChanged, 8)
			defer cancel()
			advertised := false
			mgr.SetRuntimePublicationCallbacks(nil, func() { advertised = true })
			prepared, err := mgr.PrepareReconcile("landing", listeners)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Release()
			if _, _, err := prepared.PublishWithResumeTokenContext(context.Background(), token); err == nil {
				t.Fatal("expected reactivation failure")
			}
			if advertised || mgr.AppPublicationActive("landing") {
				t.Fatal("failed reactivation advertised or enabled routes")
			}
			select {
			case evt := <-ch:
				t.Fatalf("failed reactivation published discovery: %+v", evt)
			default:
			}
		})
	}
}
