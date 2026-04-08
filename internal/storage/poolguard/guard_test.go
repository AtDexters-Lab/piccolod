package poolguard

import (
	"context"
	"sync"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/app"
	"piccolod/internal/events"
	"piccolod/internal/storage/lvm"
)

type mockPool struct {
	mu    sync.Mutex
	stats lvm.PoolStats
	err   error
}

func (m *mockPool) PoolStatus(_ context.Context) (lvm.PoolStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats, m.err
}

func (m *mockPool) set(dataPercent float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = lvm.PoolStats{DataPercent: dataPercent}
}

type mockApps struct {
	mu      sync.Mutex
	apps    []*app.AppInstance
	stopped []string
	stopErr map[string]error
}

func (m *mockApps) List(_ context.Context) ([]*app.AppInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apps, nil
}

func (m *mockApps) Stop(_ context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, instanceID)
	if m.stopErr != nil {
		return m.stopErr[instanceID]
	}
	return nil
}

func (m *mockApps) getStopped() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.stopped))
	copy(out, m.stopped)
	return out
}

func workspaceApp(id, status string) *app.AppInstance {
	return &app.AppInstance{
		InstanceID: id,
		Status:     status,
		Definition: &api.AppDefinition{
			Extensions: map[string]interface{}{
				"mode": "workspace",
			},
		},
	}
}

func serviceApp(id, status string) *app.AppInstance {
	return &app.AppInstance{
		InstanceID: id,
		Status:     status,
		Definition: &api.AppDefinition{},
	}
}

func collectAlerts(bus *events.Bus) (chan events.StorageAlertEvent, func()) {
	ch, unsub := bus.SubscribeWithCancel(events.TopicStorageAlert, 16)
	alerts := make(chan events.StorageAlertEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(alerts)
		for {
			select {
			case evt, ok := <-ch:
				if !ok {
					return
				}
				if a, ok := evt.Payload.(events.StorageAlertEvent); ok {
					alerts <- a
				}
			case <-done:
				return
			}
		}
	}()
	return alerts, func() { close(done); unsub() }
}

func TestGuard_UrgentStopsWorkspaces(t *testing.T) {
	pool := &mockPool{}
	pool.set(96.0)

	apps := &mockApps{apps: []*app.AppInstance{
		workspaceApp("ws1", app.StatusRunning),
		serviceApp("svc1", app.StatusRunning),
		workspaceApp("ws2", app.StatusRunning),
	}}

	bus := events.NewBus()
	alerts, cleanup := collectAlerts(bus)
	defer cleanup()

	g := New(pool, apps, bus)
	g.CheckNow(context.Background())

	stopped := apps.getStopped()
	if len(stopped) != 2 {
		t.Fatalf("expected 2 workspaces stopped, got %d: %v", len(stopped), stopped)
	}
	for _, id := range stopped {
		if id != "ws1" && id != "ws2" {
			t.Errorf("unexpected stopped app: %s", id)
		}
	}

	// On first crossing into 95%, two alerts fire: critical (warning) then urgent (action).
	// Drain to find the urgent one.
	var foundUrgent bool
	timeout := time.After(time.Second)
	for !foundUrgent {
		select {
		case alert := <-alerts:
			if alert.Severity == events.AlertSeverityUrgent {
				foundUrgent = true
				if alert.Action != events.AlertActionWorkspacesStopped {
					t.Errorf("expected action workspaces_stopped, got %s", alert.Action)
				}
				if len(alert.AffectedApps) != 2 {
					t.Errorf("expected 2 affected apps, got %d", len(alert.AffectedApps))
				}
			}
		case <-timeout:
			t.Fatal("timed out waiting for urgent alert")
		}
	}
}

func TestGuard_CriticalWarnsOnly(t *testing.T) {
	pool := &mockPool{}
	pool.set(91.0)

	apps := &mockApps{apps: []*app.AppInstance{
		workspaceApp("ws1", app.StatusRunning),
	}}

	bus := events.NewBus()
	alerts, cleanup := collectAlerts(bus)
	defer cleanup()

	g := New(pool, apps, bus)
	g.CheckNow(context.Background())

	stopped := apps.getStopped()
	if len(stopped) != 0 {
		t.Fatalf("expected no apps stopped at 91%%, got %d", len(stopped))
	}

	select {
	case alert := <-alerts:
		if alert.Severity != events.AlertSeverityCritical {
			t.Errorf("expected severity critical, got %s", alert.Severity)
		}
		if alert.Action != events.AlertActionNone {
			t.Errorf("expected action none, got %s", alert.Action)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for warning alert")
	}
}

func TestGuard_BelowThresholdNoop(t *testing.T) {
	pool := &mockPool{}
	pool.set(75.0)

	apps := &mockApps{apps: []*app.AppInstance{
		workspaceApp("ws1", app.StatusRunning),
	}}

	bus := events.NewBus()
	g := New(pool, apps, bus)
	g.CheckNow(context.Background())

	stopped := apps.getStopped()
	if len(stopped) != 0 {
		t.Fatalf("expected no apps stopped at 75%%, got %d", len(stopped))
	}
}

func TestGuard_CooldownPreventsRapidStops(t *testing.T) {
	pool := &mockPool{}
	pool.set(96.0)

	apps := &mockApps{apps: []*app.AppInstance{
		workspaceApp("ws1", app.StatusRunning),
	}}

	bus := events.NewBus()
	g := New(pool, apps, bus)
	g.SetIntervalsForTest(time.Millisecond, 5*time.Second)

	// First check triggers stop.
	g.CheckNow(context.Background())
	if len(apps.getStopped()) != 1 {
		t.Fatal("first check should have stopped workspace")
	}

	// Reset level to allow re-triggering (simulate recovery + re-fill).
	g.mu.Lock()
	g.lastLevel = 0
	g.mu.Unlock()

	// Second check within cooldown should not stop again.
	apps.mu.Lock()
	apps.stopped = nil
	apps.mu.Unlock()

	g.CheckNow(context.Background())
	if len(apps.getStopped()) != 0 {
		t.Fatal("second check within cooldown should not stop")
	}
}

func TestGuard_RecoveryResetsLevel(t *testing.T) {
	pool := &mockPool{}
	apps := &mockApps{apps: []*app.AppInstance{
		workspaceApp("ws1", app.StatusRunning),
	}}

	bus := events.NewBus()
	g := New(pool, apps, bus)

	// First: pool at 96% → urgent triggers.
	pool.set(96.0)
	g.CheckNow(context.Background())
	if len(apps.getStopped()) != 1 {
		t.Fatal("first urgent check should stop workspace")
	}

	// Pool recovers to 85%.
	pool.set(85.0)
	g.CheckNow(context.Background())

	// Pool rises back to 96% → should re-trigger.
	// Clear cooldown so the second urgent isn't suppressed.
	g.mu.Lock()
	g.lastActionTime = time.Time{}
	g.mu.Unlock()

	apps.mu.Lock()
	apps.stopped = nil
	apps.apps = []*app.AppInstance{workspaceApp("ws1", app.StatusRunning)}
	apps.mu.Unlock()

	pool.set(96.0)
	g.CheckNow(context.Background())
	if len(apps.getStopped()) != 1 {
		t.Fatal("second urgent after recovery should stop workspace again")
	}
}

func TestGuard_AlreadyStoppedNoop(t *testing.T) {
	pool := &mockPool{}
	pool.set(96.0)

	apps := &mockApps{apps: []*app.AppInstance{
		workspaceApp("ws1", app.StatusStopped),
	}}

	bus := events.NewBus()
	g := New(pool, apps, bus)
	g.CheckNow(context.Background())

	if len(apps.getStopped()) != 0 {
		t.Fatal("already-stopped workspace should not be stopped again")
	}
}

func TestGuard_StopErrorContinues(t *testing.T) {
	pool := &mockPool{}
	pool.set(96.0)

	apps := &mockApps{
		apps: []*app.AppInstance{
			workspaceApp("ws1", app.StatusRunning),
			workspaceApp("ws2", app.StatusRunning),
		},
		stopErr: map[string]error{
			"ws1": context.DeadlineExceeded,
		},
	}

	bus := events.NewBus()
	g := New(pool, apps, bus)
	g.CheckNow(context.Background())

	stopped := apps.getStopped()
	// Both should be attempted even though ws1 fails.
	if len(stopped) != 2 {
		t.Fatalf("expected 2 stop attempts, got %d: %v", len(stopped), stopped)
	}
}
