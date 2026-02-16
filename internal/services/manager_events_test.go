package services

import (
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
	"piccolod/internal/events"
)

type stubServiceLockReader struct {
	mu     sync.RWMutex
	locked bool
}

func (s *stubServiceLockReader) ControlLocked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.locked
}

func (s *stubServiceLockReader) set(locked bool) {
	s.mu.Lock()
	s.locked = locked
	s.mu.Unlock()
}

func TestServiceManagerLeadershipTracking(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.ObserveRuntimeEvents(bus)
	defer mgr.StopRuntimeEvents()

	bus.Publish(events.Event{Topic: events.TopicLeadershipRoleChanged, Payload: events.LeadershipChanged{Resource: "control", Role: cluster.RoleLeader}})

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mgr.LastObservedRole("control") == cluster.RoleLeader {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected control role to become leader, got %s", mgr.LastObservedRole("control"))
}

func TestServiceManagerLockTracking(t *testing.T) {
	mgr := NewServiceManager()
	reader := &stubServiceLockReader{}
	mgr.SetLockReader(reader)
	reader.set(true)
	if !mgr.Locked() {
		t.Fatalf("expected Locked() to reflect reader locked state")
	}

	reader.set(false)
	if mgr.Locked() {
		t.Fatalf("expected Locked() to reflect reader unlocked state")
	}
}

func TestServiceManagerPublishesEndpointEventsOnAllocate(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	ch := bus.Subscribe(events.TopicServiceEndpointsChanged, 4)

	listeners := []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}
	_, err := mgr.AllocateForApp("testapp", listeners)
	if err != nil {
		t.Fatalf("AllocateForApp failed: %v", err)
	}

	select {
	case evt := <-ch:
		payload, ok := evt.Payload.(events.ServiceEndpointsChanged)
		if !ok {
			t.Fatalf("expected ServiceEndpointsChanged payload, got %T", evt.Payload)
		}
		if payload.App != "testapp" {
			t.Errorf("expected app=testapp, got %s", payload.App)
		}
		if len(payload.Added) != 1 {
			t.Errorf("expected 1 added endpoint, got %d", len(payload.Added))
		}
		if len(payload.Removed) != 0 {
			t.Errorf("expected 0 removed endpoints, got %d", len(payload.Removed))
		}
		if payload.Added[0].Name != "web" {
			t.Errorf("expected listener name=web, got %s", payload.Added[0].Name)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for ServiceEndpointsChanged event")
	}

	mgr.RemoveApp("testapp")
}

func TestServiceManagerPublishesEndpointEventsOnRemove(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	listeners := []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}
	_, err := mgr.AllocateForApp("testapp", listeners)
	if err != nil {
		t.Fatalf("AllocateForApp failed: %v", err)
	}

	// Subscribe after allocate to only get the remove event
	ch := bus.Subscribe(events.TopicServiceEndpointsChanged, 4)

	mgr.RemoveApp("testapp")

	select {
	case evt := <-ch:
		payload, ok := evt.Payload.(events.ServiceEndpointsChanged)
		if !ok {
			t.Fatalf("expected ServiceEndpointsChanged payload, got %T", evt.Payload)
		}
		if payload.App != "testapp" {
			t.Errorf("expected app=testapp, got %s", payload.App)
		}
		if len(payload.Added) != 0 {
			t.Errorf("expected 0 added endpoints, got %d", len(payload.Added))
		}
		if len(payload.Removed) != 1 {
			t.Errorf("expected 1 removed endpoint, got %d", len(payload.Removed))
		}
		if payload.Removed[0].Name != "web" {
			t.Errorf("expected listener name=web, got %s", payload.Removed[0].Name)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for ServiceEndpointsChanged event")
	}
}

func TestCheckBackends_SkipsTransientApps(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	// Allocate endpoints for an app
	listeners := []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}
	_, err := mgr.AllocateForApp("myapp", listeners)
	if err != nil {
		t.Fatalf("AllocateForApp failed: %v", err)
	}

	// Mark app as transient (starting)
	mgr.appTransientMu.Lock()
	mgr.appTransient["myapp"] = time.Now()
	mgr.appTransientMu.Unlock()

	// Run checkBackends — should skip myapp, so no health state recorded
	mgr.checkBackends()

	endpointKey := "myapp/web"
	isHealthy, _ := mgr.backendHealth.GetHealthState(endpointKey)
	if !isHealthy {
		t.Error("transient app should not have unhealthy state recorded")
	}

	mgr.RemoveApp("myapp")
}

func TestAppStatusTracker_TransitionToRunning(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	// Publish starting event
	bus.Publish(events.Event{
		Topic: events.TopicAppStatusChanged,
		Payload: events.AppStatusChangedEvent{
			App: "myapp", Status: "starting",
		},
	})
	time.Sleep(50 * time.Millisecond)

	mgr.appTransientMu.RLock()
	_, isTransient := mgr.appTransient["myapp"]
	mgr.appTransientMu.RUnlock()
	if !isTransient {
		t.Fatal("expected myapp to be transient after starting")
	}

	// Publish running event
	bus.Publish(events.Event{
		Topic: events.TopicAppStatusChanged,
		Payload: events.AppStatusChangedEvent{
			App: "myapp", Status: "running",
		},
	})
	time.Sleep(50 * time.Millisecond)

	mgr.appTransientMu.RLock()
	_, isTransient = mgr.appTransient["myapp"]
	mgr.appTransientMu.RUnlock()
	if isTransient {
		t.Fatal("expected myapp to no longer be transient after running")
	}
}

func TestAppStatusTracker_TransitionToError(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	bus.Publish(events.Event{
		Topic: events.TopicAppStatusChanged,
		Payload: events.AppStatusChangedEvent{
			App: "myapp", Status: "starting",
		},
	})
	time.Sleep(50 * time.Millisecond)

	mgr.appTransientMu.RLock()
	_, transient := mgr.appTransient["myapp"]
	mgr.appTransientMu.RUnlock()
	if !transient {
		t.Fatal("expected myapp to be transient after starting")
	}

	bus.Publish(events.Event{
		Topic: events.TopicAppStatusChanged,
		Payload: events.AppStatusChangedEvent{
			App: "myapp", Status: "error",
		},
	})
	time.Sleep(50 * time.Millisecond)

	mgr.appTransientMu.RLock()
	_, transient = mgr.appTransient["myapp"]
	mgr.appTransientMu.RUnlock()
	if transient {
		t.Fatal("expected myapp to no longer be transient after error")
	}
}

func TestCheckBackends_ResumesAfterTransient(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	listeners := []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}
	_, err := mgr.AllocateForApp("myapp", listeners)
	if err != nil {
		t.Fatalf("AllocateForApp failed: %v", err)
	}

	// Mark transient, check backends (should skip)
	mgr.appTransientMu.Lock()
	mgr.appTransient["myapp"] = time.Now()
	mgr.appTransientMu.Unlock()

	mgr.checkBackends()

	// Clear transient, check backends (should resume — backend will be unreachable)
	mgr.appTransientMu.Lock()
	delete(mgr.appTransient, "myapp")
	mgr.appTransientMu.Unlock()

	mgr.checkBackends()

	// After resuming, the health state should have been checked
	// (it will be unhealthy since nothing is listening, but we only need 1 check recorded)
	endpointKey := "myapp/web"
	mgr.backendHealth.mu.RLock()
	_, seen := mgr.backendHealth.firstSeen[endpointKey]
	mgr.backendHealth.mu.RUnlock()
	if !seen {
		t.Error("expected health check to have been recorded after transient cleared")
	}

	mgr.RemoveApp("myapp")
}

func TestServiceManagerPublishesEndpointEventsOnLabelChange(t *testing.T) {
	mgr := NewServiceManager()
	bus := events.NewBus()
	mgr.SetEventBus(bus)

	// Allocate with "web" as primary
	listeners := []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		{Name: "api", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
	}
	_, err := mgr.AllocateForApp("testapp", listeners)
	if err != nil {
		t.Fatalf("AllocateForApp failed: %v", err)
	}

	// Subscribe after allocate to only get the reconcile event
	ch := bus.Subscribe(events.TopicServiceEndpointsChanged, 4)

	// Reconcile with "backend" as primary (simulates primary listener change)
	// RFC 20260130: "api" is reserved, use "backend" instead
	listenersNewPrimary := []api.AppListener{
		{Name: "web", GuestPort: 80, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP},
		{Name: "backend", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
	}
	_, _, err = mgr.Reconcile("testapp", listenersNewPrimary)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	select {
	case evt := <-ch:
		payload, ok := evt.Payload.(events.ServiceEndpointsChanged)
		if !ok {
			t.Fatalf("expected ServiceEndpointsChanged payload, got %T", evt.Payload)
		}
		if payload.App != "testapp" {
			t.Errorf("expected app=testapp, got %s", payload.App)
		}
		// Both listeners should have label changes (primary changed)
		if len(payload.Added) != 2 {
			t.Errorf("expected 2 added endpoints (label changes), got %d", len(payload.Added))
		}
		if len(payload.Removed) != 2 {
			t.Errorf("expected 2 removed endpoints (old labels), got %d", len(payload.Removed))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for ServiceEndpointsChanged event")
	}

	mgr.RemoveApp("testapp")
}
